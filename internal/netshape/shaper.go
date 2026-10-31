package netshape

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/pion/transport/v5"
)

var ErrInvalidRate = errors.New("UDP test rate must be between 0 and 100 Mbps")

const maxRateBPS = 100_000_000

// Shaper applies a process-local outbound UDP rate cap. It exists only to
// exercise the real WebRTC feedback path without changing system network rules.
type Shaper struct {
	mu      sync.Mutex
	rateBPS int
	next    time.Time
	changed chan struct{}
}

func NewShaper() *Shaper {
	return &Shaper{changed: make(chan struct{})}
}

func (s *Shaper) SetRateBPS(rate int) error {
	if rate < 0 || rate > maxRateBPS {
		return ErrInvalidRate
	}
	s.mu.Lock()
	s.rateBPS = rate
	s.next = time.Now()
	close(s.changed)
	s.changed = make(chan struct{})
	s.mu.Unlock()
	return nil
}

func (s *Shaper) RateBPS() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rateBPS
}

func (s *Shaper) wait(ctx context.Context, packetBytes int) error {
	if packetBytes <= 0 {
		return nil
	}
	for {
		s.mu.Lock()
		if s.rateBPS == 0 {
			s.mu.Unlock()
			return nil
		}
		now := time.Now()
		start := s.next
		if start.Before(now) {
			start = now
		}
		bits := int64(packetBytes) * 8
		rate := int64(s.rateBPS)
		nanoseconds := (bits*int64(time.Second) + rate - 1) / rate
		due := start.Add(time.Duration(nanoseconds))
		s.next = due
		changed := s.changed
		s.mu.Unlock()

		wait := time.Until(due)
		if wait <= 0 {
			return nil
		}
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
			return nil
		case <-changed:
			stopTimer(timer)
		case <-ctx.Done():
			stopTimer(timer)
			return ctx.Err()
		}
	}
}

func stopTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

// Wrap returns a Pion network whose outbound datagrams share the supplied cap.
// Inbound traffic and TCP signaling are left untouched.
func Wrap(base transport.Net, shaper *Shaper) transport.Net {
	if shaper == nil {
		return base
	}
	return &shapedNet{Net: base, shaper: shaper}
}

type shapedNet struct {
	transport.Net
	shaper *Shaper
}

func (n *shapedNet) ListenPacket(network, address string) (net.PacketConn, error) {
	conn, err := n.Net.ListenPacket(network, address)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &shapedPacketConn{PacketConn: conn, shaper: n.shaper, ctx: ctx, cancel: cancel}, nil
}

func (n *shapedNet) ListenUDP(network string, address *net.UDPAddr) (transport.UDPConn, error) {
	conn, err := n.Net.ListenUDP(network, address)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &shapedUDPConn{UDPConn: conn, shaper: n.shaper, ctx: ctx, cancel: cancel}, nil
}

func (n *shapedNet) DialUDP(network string, local, remote *net.UDPAddr) (transport.UDPConn, error) {
	conn, err := n.Net.DialUDP(network, local, remote)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &shapedUDPConn{UDPConn: conn, shaper: n.shaper, ctx: ctx, cancel: cancel}, nil
}

func (n *shapedNet) Dial(network, address string) (net.Conn, error) {
	conn, err := n.Net.Dial(network, address)
	if err != nil || !strings.HasPrefix(network, "udp") {
		return conn, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &shapedConn{Conn: conn, shaper: n.shaper, ctx: ctx, cancel: cancel}, nil
}

type shapedPacketConn struct {
	net.PacketConn
	shaper *Shaper
	ctx    context.Context
	cancel context.CancelFunc
}

func (c *shapedPacketConn) WriteTo(packet []byte, address net.Addr) (int, error) {
	if err := c.shaper.wait(c.ctx, len(packet)); err != nil {
		return 0, err
	}
	return c.PacketConn.WriteTo(packet, address)
}

func (c *shapedPacketConn) Close() error {
	c.cancel()
	return c.PacketConn.Close()
}

type shapedUDPConn struct {
	transport.UDPConn
	shaper *Shaper
	ctx    context.Context
	cancel context.CancelFunc
}

func (c *shapedUDPConn) wait(size int) error { return c.shaper.wait(c.ctx, size) }

func (c *shapedUDPConn) Write(packet []byte) (int, error) {
	if err := c.wait(len(packet)); err != nil {
		return 0, err
	}
	return c.UDPConn.Write(packet)
}

func (c *shapedUDPConn) WriteTo(packet []byte, address net.Addr) (int, error) {
	if err := c.wait(len(packet)); err != nil {
		return 0, err
	}
	return c.UDPConn.WriteTo(packet, address)
}

func (c *shapedUDPConn) WriteToUDP(packet []byte, address *net.UDPAddr) (int, error) {
	if err := c.wait(len(packet)); err != nil {
		return 0, err
	}
	return c.UDPConn.WriteToUDP(packet, address)
}

func (c *shapedUDPConn) WriteMsgUDP(packet, oob []byte, address *net.UDPAddr) (int, int, error) {
	if err := c.wait(len(packet) + len(oob)); err != nil {
		return 0, 0, err
	}
	return c.UDPConn.WriteMsgUDP(packet, oob, address)
}

func (c *shapedUDPConn) Close() error {
	c.cancel()
	return c.UDPConn.Close()
}

type shapedConn struct {
	net.Conn
	shaper *Shaper
	ctx    context.Context
	cancel context.CancelFunc
}

func (c *shapedConn) Write(packet []byte) (int, error) {
	if err := c.shaper.wait(c.ctx, len(packet)); err != nil {
		return 0, err
	}
	return c.Conn.Write(packet)
}

func (c *shapedConn) Close() error {
	c.cancel()
	return c.Conn.Close()
}
