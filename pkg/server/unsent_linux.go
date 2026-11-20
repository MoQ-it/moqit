package server

import (
	"net"

	"golang.org/x/sys/unix"
)

// limitUnsent caps the data the kernel holds unsent on c, so the rest
// waits in the WebSocket transport, where urgent streams overtake it and
// resets drop it. TCP_NOTSENT_LOWAT leaves the congestion window alone.
func limitUnsent(c *net.TCPConn) {
	raw, err := c.SyscallConn()
	if err != nil {
		return
	}
	raw.Control(func(fd uintptr) {
		unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_NOTSENT_LOWAT, maxUnsent)
	})
}
