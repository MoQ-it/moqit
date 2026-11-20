package server

import (
	"net"
	"testing"

	"golang.org/x/sys/unix"
)

func TestLimitUnsent(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if c, err := net.Dial("tcp", ln.Addr().String()); err == nil {
			defer c.Close()
			c.Read(make([]byte, 1))
		}
	}()
	c, err := unsentListener{ln}.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	raw, _ := c.(*net.TCPConn).SyscallConn()
	var got int
	var gerr error
	raw.Control(func(fd uintptr) { got, gerr = unix.GetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_NOTSENT_LOWAT) })
	if gerr != nil || got != maxUnsent {
		t.Fatalf("TCP_NOTSENT_LOWAT = %d, %v; want %d", got, gerr, maxUnsent)
	}
}
