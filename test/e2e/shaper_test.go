package e2e

import (
	"net"
	"testing"
	"time"
)

func TestShaperLimitsUplink(t *testing.T) {
	server, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	s, err := NewShaper(server.LocalAddr().String(), 800_000, 20_000) // 100 kB/s, 20 kB queue
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	client, err := net.DialUDP("udp", nil, s.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	// 200 kB offered at once: the queue holds 20 kB, the rest is dropped.
	pkt := make([]byte, 1000)
	for range 200 {
		client.Write(pkt)
	}
	got, start := 0, time.Now()
	buf := make([]byte, 2000)
	var from *net.UDPAddr
	for {
		server.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, addr, err := server.ReadFromUDP(buf)
		if err != nil {
			break
		}
		got += n
		from = addr
	}
	elapsed := time.Since(start) - 500*time.Millisecond
	if got < 15_000 || got > 22_000 {
		t.Errorf("delivered %d bytes; want about the 20 kB queue", got)
	}
	if rate := float64(got) / elapsed.Seconds(); rate > 130_000 {
		t.Errorf("delivered %.0f B/s; want at most about 100 kB/s", rate)
	}
	// Replies go back unlimited.
	server.WriteToUDP([]byte("pong"), from)
	client.SetReadDeadline(time.Now().Add(time.Second))
	if n, err := client.Read(buf); err != nil || string(buf[:n]) != "pong" {
		t.Errorf("reply %q, %v", buf[:n], err)
	}
}
