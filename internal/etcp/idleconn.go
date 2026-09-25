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
// upstream's SocketHandler (src/base/SocketHandler.cpp:6,14,39,68 at
// et-v7.0.0). Every Read gets a fresh deadline, and Write sends in idleChunk
// pieces with a fresh deadline before each: net.Conn.Write transfers the whole
// slice in one call, so a single deadline set before it would limit the entire
// transfer. Write progress is seen per chunk, so a peer that drains fewer than
// idleChunk bytes per timeout still times out.
//
// Progress in either direction also pushes the other direction's deadline. In
// the recover exchange upstream writes its whole catchup before it reads ours
// (src/base/Connection.cpp:125-135), so our catchup write cannot progress
// while the server's catchup is still arriving; that write must not time out
// while those bytes flow. The connection is idle only when neither direction
// moves.
type idleConn struct {
	net.Conn
	timeout time.Duration
}

func (c idleConn) Read(p []byte) (int, error) {
	if err := c.SetReadDeadline(time.Now().Add(c.timeout)); err != nil {
		return 0, err
	}
	n, err := c.Conn.Read(p)
	if n > 0 {
		// An error here means the conn is closed; the next operation reports it.
		_ = c.SetWriteDeadline(time.Now().Add(c.timeout))
	}
	return n, err
}

func (c idleConn) Write(p []byte) (int, error) {
	var n int
	for len(p) > 0 {
		if err := c.SetWriteDeadline(time.Now().Add(c.timeout)); err != nil {
			return n, err
		}
		m, err := c.Conn.Write(p[:min(len(p), idleChunk)])
		n += m
		if m > 0 {
			// As in Read: a closed conn surfaces on the next operation.
			_ = c.SetReadDeadline(time.Now().Add(c.timeout))
		}
		if err != nil {
			return n, err
		}
		p = p[m:]
	}
	return n, nil
}
