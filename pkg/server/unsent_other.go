//go:build !linux

package server

import "net"

func limitUnsent(*net.TCPConn) {}
