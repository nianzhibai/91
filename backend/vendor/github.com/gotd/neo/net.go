package neo

import (
	"errors"
	"net"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// Net is virtual "net" package, implements mesh of peers.
type Net struct {
	peers map[string]*PacketConn
}

type packet struct {
	buf  []byte
	addr net.Addr
}

// PacketConn simulates mesh peer of Net.
type PacketConn struct {
	packets chan packet
	addr    net.Addr
	net     *Net

	closedMux sync.Mutex
	closed    bool

	mux           sync.Mutex
	deadline      notifier
	readDeadline  notifier
	writeDeadline notifier
}

func addrKey(a net.Addr) string {
	if u, ok := a.(*net.UDPAddr); ok {
		return "udp/" + u.String()
	}
	return a.Network() + "/" + a.String()
}

func (c *PacketConn) ok() bool {
	if c == nil {
		return false
	}
	c.closedMux.Lock()
	defer c.closedMux.Unlock()
	return !c.closed
}

var ErrDeadline = errors.New("deadline")

// ReadFrom reads a packet from the connection,
// copying the payload into p.
func (c *PacketConn) ReadFrom(p []byte) (n int, addr net.Addr, err error) {
	if !c.ok() {
		return 0, nil, syscall.EINVAL
	}

	c.mux.Lock()
	deadline := c.deadline
	readDeadline := c.readDeadline
	c.mux.Unlock()

	select {
	case pp := <-c.packets:
		return copy(p, pp.buf), pp.addr, nil
	case <-readDeadline:
		return 0, nil, ErrDeadline
	case <-deadline:
		return 0, nil, ErrDeadline
	}
}

// WriteTo writes a packet with payload p to addr.
func (c *PacketConn) WriteTo(p []byte, a net.Addr) (n int, err error) {
	if !c.ok() {
		return 0, syscall.EINVAL
	}

	c.mux.Lock()
	deadline := c.deadline
	writeDeadline := c.writeDeadline
	c.mux.Unlock()

	select {
	case c.net.peers[addrKey(a)].packets <- packet{
		addr: c.addr,
		buf:  append([]byte{}, p...),
	}:
		return len(p), nil
	case <-writeDeadline:
		return 0, ErrDeadline
	case <-deadline:
		return 0, ErrDeadline
	}
}

func (c *PacketConn) LocalAddr() net.Addr { return c.addr }

// Close closes the connection.
func (c *PacketConn) Close() error {
	if !c.ok() {
		return syscall.EINVAL
	}
	c.closedMux.Lock()
	defer c.closedMux.Unlock()
	if c.closed {
		return syscall.EINVAL
	}
	c.closed = true
	close(c.packets)
	return nil
}

type notifier chan struct{}

func simpleDeadline(t time.Time) notifier {
	deadline := make(notifier)
	go func() {
		now := time.Now()
		<-time.After(t.Sub(now))
		close(deadline)
	}()

	return deadline
}

func (c *PacketConn) SetDeadline(t time.Time) error {
	if !c.ok() {
		return syscall.EINVAL
	}
	c.mux.Lock()
	c.deadline = simpleDeadline(t)
	c.mux.Unlock()
	return nil
}

func (c *PacketConn) SetReadDeadline(t time.Time) error {
	if !c.ok() {
		return syscall.EINVAL
	}
	c.mux.Lock()
	c.readDeadline = simpleDeadline(t)
	c.mux.Unlock()
	return nil
}

func (c *PacketConn) SetWriteDeadline(t time.Time) error {
	if !c.ok() {
		return syscall.EINVAL
	}
	c.mux.Lock()
	c.writeDeadline = simpleDeadline(t)
	c.mux.Unlock()
	return nil
}

type NetAddr struct {
	Net     string
	Address string
}

func (n NetAddr) Network() string { return n.Net }
func (n NetAddr) String() string  { return n.Address }

// ResolveUDPAddr returns an address of UDP end point.
func (n *Net) ResolveUDPAddr(network, address string) (*net.UDPAddr, error) {
	a := &net.UDPAddr{
		Port: 0,
		IP:   net.IPv4(127, 0, 0, 1),
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if a.IP = net.ParseIP(host); a.IP == nil {
		// Probably we should use virtual DNS here.
		return nil, errors.New("bad IP")
	}
	if a.Port, err = strconv.Atoi(port); err != nil {
		return nil, err
	}
	return a, nil
}

// ListenPacket announces on the local network address.
func (n *Net) ListenPacket(network, address string) (net.PacketConn, error) {
	if network != "udp4" && network != "udp" && network != "udp6" {
		return nil, errors.New("bad net")
	}
	a, err := n.ResolveUDPAddr(network, address)
	if err != nil {
		return nil, err
	}
	pc := &PacketConn{
		net:     n,
		addr:    a,
		packets: make(chan packet, 10),
	}
	n.peers[addrKey(a)] = pc
	return pc, nil
}

// NAT implements facility for Network Address Translation simulation.
//
// Basic example:
// 	[ A ] <-----> [ NAT1 ] <-----> [ NAT2 ] <-----> [ B ]
//      IPa              IPa'     IPb'             IPb
//
// 	1) A sends packet P with dst = IPb'
//  2) NAT1 receives packet P and changes it's src to IPa',
//   sending it to NAT2 from IPa'.
//  3) NAT2 receives packet P from IPa' to IPb', does a lookup to
//   NAT translation table and finds association IPb' <-> IPb.
//   Then it sends packet P to B.
//  4) B receives packet P from NAT2, observing that it has src = IPa'.
//
//  Now B can repeat steps 1-4 and send packet back.
//
//  IPa  = 10.5.0.1:30000
//  IPa' = 83.30.100.1:23100
//  IPb' = 91.10.100.1:13000
//  IPb  = 10.1.0.1:20000
type NAT struct {
	// TODO(ar): implement
}
