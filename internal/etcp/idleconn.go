package etcp

import (
	"net"
	"time"
)

const (
	handshakeIdle = 30 * time.Second
	idleChunk     = 64 << 10
)

// idleConn gives handshake I/O an idle timeout that resets on progress, like
// upstream's SocketHandler (src/base/SocketHandler.cpp:6,14,39,68). Every Read
// gets a fresh deadline, and Write sends in idleChunk pieces with a fresh
// deadline before each: net.Conn.Write transfers the whole slice in one call,
// so a single deadline set before it would limit the entire transfer.
type idleConn struct {
	net.Conn
	timeout time.Duration
}

func (c idleConn) Read(p []byte) (int, error) {
	if err := c.SetReadDeadline(time.Now().Add(c.timeout)); err != nil {
		return 0, err
	}
	return c.Conn.Read(p)
}

func (c idleConn) Write(p []byte) (int, error) {
	var n int
	for len(p) > 0 {
		if err := c.SetWriteDeadline(time.Now().Add(c.timeout)); err != nil {
			return n, err
		}
		m, err := c.Conn.Write(p[:min(len(p), idleChunk)])
		n += m
		if err != nil {
			return n, err
		}
		p = p[m:]
	}
	return n, nil
}
