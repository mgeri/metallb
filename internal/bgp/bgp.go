package bgp

import (
	"container/list"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"reflect"
	"sync"
	"time"

	"github.com/golang/glog"
)

const (
	backoff = 2 * time.Second
)

var errClosed = errors.New("session closed")

type Session struct {
	asn      uint32
	routerID net.IP
	addr     string
	peerASN  uint32
	holdTime time.Duration

	newHoldTime chan bool

	mu             sync.Mutex
	cond           *sync.Cond
	closed         bool
	conn           net.Conn
	actualHoldTime time.Duration
	advertised     map[string]*Advertisement
	new            map[string]*Advertisement
	pending        *list.List
}

func (s *Session) run() {
	defer stats.DeleteSession(s.addr)
	for {
		if err := s.connect(); err != nil {
			glog.Error(err)
			time.Sleep(backoff)
			continue
		}
		stats.SessionUp(s.addr)

		glog.Infof("BGP session to %q established", s.addr)

		if err := s.sendUpdates(); err != nil {
			if err == errClosed {
				return
			}
			glog.Error(err)
		}
		stats.SessionDown(s.addr)
		glog.Infof("BGP session to %q down", s.addr)
	}
}

func (s *Session) sendUpdates() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	asn := s.asn
	if s.peerASN == s.asn {
		asn = 0
	}

	if s.new != nil {
		s.advertised, s.new = s.new, nil
	}

	for c, adv := range s.advertised {
		if err := sendUpdate(s.conn, asn, adv); err != nil {
			s.abort()
			return fmt.Errorf("sending update of %q to %q: %s", c, s.addr, err)
		}
		stats.UpdateSent(s.addr)
	}
	stats.AdvertisedPrefixes(s.addr, len(s.advertised))

	for {
		for s.new == nil && s.conn != nil {
			s.cond.Wait()
		}

		if s.closed {
			return errClosed
		}
		if s.conn == nil {
			return nil
		}
		if s.new == nil {
			// nil is "no pending updates", contrast to a non-nil
			// empty map which means "withdraw all".
			continue
		}

		for c, adv := range s.new {
			if adv2, ok := s.advertised[c]; ok && adv2.NextHop.Equal(adv.NextHop) && reflect.DeepEqual(adv2.Communities, adv.Communities) {
				// Peer already has correct state for this
				// advertisement, nothing to do.
				continue
			}

			if err := sendUpdate(s.conn, asn, adv); err != nil {
				s.abort()
				return fmt.Errorf("sending update of %q to %q: %s", c, s.addr, err)
			}
			stats.UpdateSent(s.addr)
		}

		wdr := []*net.IPNet{}
		for c, adv := range s.advertised {
			if s.new[c] == nil {
				wdr = append(wdr, adv.Prefix)
			}
		}
		if len(wdr) > 0 {
			if err := sendWithdraw(s.conn, wdr); err != nil {
				s.abort()
				return fmt.Errorf("sending withdraw of %q to %q: %s", wdr, s.addr, err)
			}
			stats.UpdateSent(s.addr)
		}
		s.advertised, s.new = s.new, nil
		stats.AdvertisedPrefixes(s.addr, len(s.advertised))
	}
}

func (s *Session) connect() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	conn, err := net.Dial("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("dial %q: %s", s.addr, err)
	}

	if err := sendOpen(conn, s.asn, s.routerID, s.holdTime); err != nil {
		conn.Close()
		return fmt.Errorf("send OPEN to %q: %s", s.addr, err)
	}

	asn, requestedHold, err := readOpen(conn)
	if err != nil {
		conn.Close()
		return fmt.Errorf("read OPEN from %q: %s", s.addr, err)
	}
	if asn != s.peerASN {
		conn.Close()
		return fmt.Errorf("unexpected peer ASN %d, want %d", asn, s.peerASN)
	}

	// Consume BGP messages until the connection closes.
	go s.consumeBGP(conn)

	// Send one keepalive to say that yes, we accept the OPEN.
	if err := sendKeepalive(conn); err != nil {
		conn.Close()
		return fmt.Errorf("accepting peer OPEN from %q: %s", s.addr, err)
	}

	// Set up regular keepalives from now on.
	s.actualHoldTime = s.holdTime
	if requestedHold < s.actualHoldTime {
		s.actualHoldTime = requestedHold
	}
	select {
	case s.newHoldTime <- true:
	default:
	}

	s.conn = conn
	return nil
}

func (s *Session) sendKeepalives() {
	var (
		t  *time.Ticker
		ch <-chan time.Time
	)

	for {
		select {
		case <-s.newHoldTime:
			s.mu.Lock()
			ht := s.actualHoldTime
			s.mu.Unlock()
			if t != nil {
				t.Stop()
				t = nil
				ch = nil
			}
			if ht != 0 {
				t = time.NewTicker(ht / 3)
				ch = t.C
			}

		case <-ch:
			if err := s.sendKeepalive(); err != nil {
				if err == errClosed {
					// Session has been closed by package caller, we're
					// done here.
					return
				}
				glog.Error(err)
			}
		}
	}
}

func (s *Session) sendKeepalive() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errClosed
	}
	if s.conn == nil {
		// No connection established, othing to do.
		return nil
	}
	if err := sendKeepalive(s.conn); err != nil {
		s.abort()
		return fmt.Errorf("sending keepalive to %q: %s", s.addr, err)
	}
	return nil
}

func New(addr string, asn uint32, routerID net.IP, peerASN uint32, holdTime time.Duration) (*Session, error) {
	ret := &Session{
		addr:        addr,
		asn:         asn,
		routerID:    routerID.To4(),
		peerASN:     peerASN,
		holdTime:    holdTime,
		newHoldTime: make(chan bool, 1),
		advertised:  map[string]*Advertisement{},
	}
	if ret.routerID == nil {
		return nil, fmt.Errorf("invalid routerID %q, must be IPv4", routerID)
	}
	ret.cond = sync.NewCond(&ret.mu)
	go ret.sendKeepalives()
	go ret.run()

	stats.sessionUp.WithLabelValues(ret.addr).Set(0)
	stats.prefixes.WithLabelValues(ret.addr).Set(0)

	return ret, nil
}

func (s *Session) consumeBGP(conn net.Conn) {
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.conn == conn {
			s.abort()
		} else {
			conn.Close()
		}
	}()

	for {
		hdr := struct {
			Marker1, Marker2 uint64
			Len              uint16
			Type             uint8
		}{}
		if err := binary.Read(conn, binary.BigEndian, &hdr); err != nil {
			// TODO: log, or propagate the error somehow.
			return
		}
		if hdr.Marker1 != 0xffffffffffffffff || hdr.Marker2 != 0xffffffffffffffff {
			// TODO: propagate
			return
		}
		if _, err := io.Copy(ioutil.Discard, io.LimitReader(conn, int64(hdr.Len)-19)); err != nil {
			// TODO: propagate
			return
		}
	}
}

func (s *Session) Set(advs ...*Advertisement) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	newAdvs := map[string]*Advertisement{}
	for _, adv := range advs {
		if adv.Prefix.IP.To4() == nil {
			return fmt.Errorf("cannot advertise non-v4 prefix %q", adv.Prefix)
		}

		if adv.NextHop.To4() == nil {
			return fmt.Errorf("next-hop must be IPv4, got %q", adv.NextHop)
		}
		if len(adv.Communities) > 63 {
			return fmt.Errorf("max supported communities is 63, got %d", len(adv.Communities))
		}
		newAdvs[adv.Prefix.String()] = adv
	}

	s.new = newAdvs
	stats.PendingPrefixes(s.addr, len(s.new))
	s.cond.Broadcast()
	return nil
}

func (s *Session) abort() {
	if s.conn != nil {
		s.conn.Close()
		s.conn = nil
		stats.SessionDown(s.addr)
	}
	// Next time we retry the connection, we can just skip straight to
	// the desired end state.
	if s.new != nil {
		s.advertised, s.new = s.new, nil
		stats.PendingPrefixes(s.addr, len(s.advertised))
	}
	s.cond.Broadcast()
}

func (s *Session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.abort()
	return nil
}

type Advertisement struct {
	Prefix      *net.IPNet
	NextHop     net.IP
	LocalPref   uint32
	Communities []uint32
}