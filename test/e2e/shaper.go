package e2e

import (
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Shaper is a UDP proxy to a server that limits the client-to-server
// direction to a bit rate, like a slow uplink: packets queue up to a byte
// limit and are dropped beyond it. The other direction is not limited.
type Shaper struct {
	ln       *net.UDPConn
	upstream *net.UDPAddr
	rate     atomic.Int64 // bits per second; 0 = unlimited
	queueMax int

	mu      sync.Mutex
	clients map[string]*shapedClient
	closed  bool
}

type shapedClient struct {
	addr   *net.UDPAddr
	conn   *net.UDPConn // to the server
	queue  chan []byte
	queued atomic.Int64 // bytes
}

// NewShaper listens on 127.0.0.1 and forwards to server.
func NewShaper(server string, bitsPerSecond int64, queueBytes int) (*Shaper, error) {
	up, err := net.ResolveUDPAddr("udp", server)
	if err != nil {
		return nil, err
	}
	ln, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return nil, err
	}
	s := &Shaper{ln: ln, upstream: up, queueMax: queueBytes, clients: map[string]*shapedClient{}}
	s.rate.Store(bitsPerSecond)
	go s.run()
	return s, nil
}

// Addr is the address clients connect to.
func (s *Shaper) Addr() *net.UDPAddr { return s.ln.LocalAddr().(*net.UDPAddr) }

// SetRate changes the uplink limit; 0 removes it.
func (s *Shaper) SetRate(bitsPerSecond int64) { s.rate.Store(bitsPerSecond) }

func (s *Shaper) Close() {
	s.mu.Lock()
	s.closed = true
	for _, c := range s.clients {
		c.conn.Close()
		close(c.queue)
	}
	s.mu.Unlock()
	s.ln.Close()
}

func (s *Shaper) run() {
	buf := make([]byte, 65536)
	for {
		n, addr, err := s.ln.ReadFromUDP(buf)
		if err != nil {
			return
		}
		c := s.client(addr)
		if c == nil {
			return
		}
		if c.queued.Load()+int64(n) > int64(s.queueMax) {
			continue // drop-tail
		}
		c.queued.Add(int64(n))
		select {
		case c.queue <- append([]byte(nil), buf[:n]...):
		default:
			c.queued.Add(-int64(n))
		}
	}
}

func (s *Shaper) client(addr *net.UDPAddr) *shapedClient {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	if c := s.clients[addr.String()]; c != nil {
		return c
	}
	conn, err := net.DialUDP("udp", nil, s.upstream)
	if err != nil {
		return nil
	}
	c := &shapedClient{addr: addr, conn: conn, queue: make(chan []byte, 1<<14)}
	s.clients[addr.String()] = c
	go s.send(c)
	go func() { // server to client, unlimited
		buf := make([]byte, 65536)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			s.ln.WriteToUDP(buf[:n], addr)
		}
	}()
	return c
}

// send paces one client's packets at the rate.
func (s *Shaper) send(c *shapedClient) {
	next := time.Now()
	for p := range c.queue {
		if rate := s.rate.Load(); rate > 0 {
			if d := time.Until(next); d > 0 {
				time.Sleep(d)
			}
			now := time.Now()
			if next.Before(now) {
				next = now
			}
			next = next.Add(time.Duration(int64(len(p)) * 8 * int64(time.Second) / rate))
		}
		c.queued.Add(-int64(len(p)))
		c.conn.Write(p)
	}
}
