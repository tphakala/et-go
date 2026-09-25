package etcp_test

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tphakala/et-go/internal/etcp"
	"github.com/tphakala/et-go/internal/etservertest"
	"github.com/tphakala/et-go/internal/protocol"
	"github.com/tphakala/et-go/internal/wire"
	"golang.org/x/crypto/nacl/secretbox"
)

// The largest payload whose sealed frame fits wire.MaxFrameSize is sent
// intact; one byte more is refused before it is queued.
func TestWritePacketPayloadBoundary(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, etcp.Dialer{})
		defer h.close()

		largest := wire.MaxFrameSize - 2 - secretbox.Overhead
		over := protocol.Packet{Header: protocol.HeaderTerminalBuffer, Payload: make([]byte, largest+1)}
		if err := h.conn.WritePacket(t.Context(), over); !errors.Is(err, wire.ErrTooLarge) {
			t.Fatalf("WritePacket(%d bytes) = %v, want wire.ErrTooLarge", largest+1, err)
		}
		fits := protocol.Packet{Header: protocol.HeaderTerminalBuffer, Payload: make([]byte, largest)}
		fits.Payload[largest-1] = 0x5a
		if err := h.conn.WritePacket(t.Context(), fits); err != nil {
			t.Fatalf("WritePacket(%d bytes): %v", largest, err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		p, err := h.srv.Recv(ctx)
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if len(p.Payload) != largest || p.Payload[largest-1] != 0x5a {
			t.Fatalf("server got %d bytes, want %d intact", len(p.Payload), largest)
		}
	})
}

// A RETURNING_CLIENT answer to the first connect (a first attempt that died
// after the server registered it) runs the recover exchange with empty
// state, in upstream's order, and the session then works.
func TestDialFirstReturningClient(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		got := make(chan int, 1)
		s := &scripted{handle: func(i int, c *rawServer) {
			if i != 0 {
				return
			}
			if c.respond(protocol.ConnectStatus_RETURNING_CLIENT) != nil {
				return
			}
			// Upstream writes each message before reading the peer's
			// (src/base/Connection.cpp:105-143 at et-v7.0.0).
			if wire.WriteMessage(c.conn, &protocol.SequenceHeader{}) != nil {
				return
			}
			var theirSeq protocol.SequenceHeader
			if wire.ReadMessage(c.br, &theirSeq) != nil || theirSeq.GetSequenceNumber() != 0 {
				return
			}
			if wire.WriteMessage(c.conn, &protocol.CatchupBuffer{}) != nil {
				return
			}
			var theirs protocol.CatchupBuffer
			if wire.ReadMessage(c.br, &theirs) != nil || len(theirs.GetBuffer()) != 0 {
				return
			}
			frame, err := wire.ReadFrame(c.br, nil)
			if err != nil {
				return
			}
			if p, err := c.open(frame); err == nil {
				if n, err := number(p); err == nil {
					got <- n
				}
			}
			c.drain()
		}}
		d := etcp.Dialer{NetDialer: s}
		conn, err := d.Dial(t.Context(), testAddr, testID, testKey)
		if err != nil {
			t.Fatalf("Dial after RETURNING_CLIENT: %v", err)
		}
		defer func() {
			_ = conn.Close()
			s.wg.Wait()
		}()
		if err := conn.WritePacket(t.Context(), numbered(0, 10)); err != nil {
			t.Fatalf("WritePacket: %v", err)
		}
		select {
		case n := <-got:
			if n != 0 {
				t.Fatalf("server got packet %d, want 0", n)
			}
		case <-time.After(time.Minute):
			t.Fatal("no packet after a first-connect recover exchange")
		}
	})
}

// On a redial, NEW_CLIENT means the server lost the session and
// MISMATCHED_PROTOCOL a server upgrade; neither can be recovered, so the
// Conn ends instead of redialing.
func TestRedialStatusIsFatal(t *testing.T) {
	tests := []struct {
		name   string
		status protocol.ConnectStatus
		want   error
	}{
		{name: "new client", status: protocol.ConnectStatus_NEW_CLIENT, want: etcp.ErrRejected},
		{name: "protocol mismatch", status: protocol.ConnectStatus_MISMATCHED_PROTOCOL, want: etcp.ErrVersion},
		{name: "unknown status", status: protocol.ConnectStatus(99), want: etcp.ErrRejected},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := &scripted{handle: func(i int, c *rawServer) {
					if i == 0 {
						// Accept, then drop the link after the test's first
						// packet: closing at once would race Dial's own
						// SetDeadline on the pipe.
						c.acceptFrames(1)
						return
					}
					_ = c.respond(tt.status)
				}}
				d := etcp.Dialer{NetDialer: s}
				conn, err := d.Dial(t.Context(), testAddr, testID, testKey)
				if err != nil {
					t.Fatalf("Dial: %v", err)
				}
				defer func() {
					_ = conn.Close()
					s.wg.Wait()
				}()
				if err := conn.WritePacket(t.Context(), numbered(0, 10)); err != nil {
					t.Fatalf("WritePacket: %v", err)
				}
				ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
				defer cancel()
				if _, err := conn.ReadPacket(ctx); !errors.Is(err, tt.want) {
					t.Fatalf("ReadPacket = %v, want %v", err, tt.want)
				}
				if got := s.dials.Load(); got != 2 {
					t.Fatalf("dials = %d, want 2: the redial status must not be retried", got)
				}
			})
		})
	}
}

// The zero-value Dialer dials real TCP with net.Dialer. A loopback listener
// served by the fake server exercises that path end to end.
func TestDefaultNetDialer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	srv := etservertest.NewServer(testID, testKey)
	// Real sockets and real time: a failure waits out this bound.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	served := make(chan struct{})
	go func() {
		defer close(served)
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_ = srv.Serve(ctx, c)
	}()

	var d etcp.Dialer
	conn, err := d.Dial(ctx, ln.Addr().String(), testID, testKey)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() {
		_ = conn.Close()
		cancel()
		<-served
	}()
	if err := conn.WritePacket(ctx, numbered(0, 10)); err != nil {
		t.Fatalf("WritePacket: %v", err)
	}
	if err := expectNumbered(ctx, 1, srv.Recv); err != nil {
		t.Fatalf("server side: %v", err)
	}
}

// tcpStyleConn reports use of its own closed end as net.ErrClosed, as a TCP
// conn does, instead of net.Pipe's io.ErrClosedPipe. With reset set, a failed
// Write reports a connection reset instead, as a TCP write can when the peer
// that sent a bad message also dropped the connection.
type tcpStyleConn struct {
	net.Conn
	closed atomic.Bool
	reset  bool
}

func (c *tcpStyleConn) Close() error {
	c.closed.Store(true)
	return c.Conn.Close()
}

func (c *tcpStyleConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if err != nil && c.closed.Load() {
		err = fmt.Errorf("tcp-style read: %w", net.ErrClosed)
	}
	return n, err
}

func (c *tcpStyleConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	switch {
	case err != nil && c.reset:
		err = fmt.Errorf("tcp-style write: %w", syscall.ECONNRESET)
	case err != nil && c.closed.Load():
		err = fmt.Errorf("tcp-style write: %w", net.ErrClosed)
	}
	return n, err
}

type tcpStyleDialer struct {
	inner *scripted
	reset bool
}

func (d tcpStyleDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	c, err := d.inner.DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	return &tcpStyleConn{Conn: c, reset: d.reset}, nil
}

// As TestBadHandshakeMessageIsFatal below shows for the ConnectResponse, a
// bad SequenceHeader or CatchupBuffer in the recover exchange ends the Conn,
// even when our own write is still blocked because the server has not read
// it: in these rows the server never reads, so the stuck write is our
// SequenceHeader (upstream reads it before writing its catchup,
// src/base/Connection.cpp:116-131 at et-v7.0.0, but the reader must not
// depend on that).
func TestBadRecoverMessageIsFatal(t *testing.T) {
	bad := append(binary.LittleEndian.AppendUint64(nil, 3), 0xff, 0xff, 0xff) // does not decode
	emptySeq := binary.LittleEndian.AppendUint64(nil, 0)
	tests := []struct {
		name string
		good [][]byte // valid messages sent before the bad one
		tcp  bool     // report a local close as net.ErrClosed, as a TCP conn does
		rst  bool     // report our failed write as a connection reset instead
	}{
		{name: "sequence header"},
		{name: "catchup buffer", good: [][]byte{emptySeq}},
		{name: "sequence header, TCP-style close", tcp: true},
		{name: "catchup buffer, TCP-style close", good: [][]byte{emptySeq}, tcp: true},
		{name: "catchup buffer, write reset", good: [][]byte{emptySeq}, tcp: true, rst: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				quit := make(chan struct{})
				s := &scripted{handle: func(i int, c *rawServer) {
					if i == 0 {
						c.acceptFrames(1)
						return
					}
					if c.respond(protocol.ConnectStatus_RETURNING_CLIENT) != nil {
						return
					}
					for _, m := range append(tt.good, bad) {
						if _, err := c.conn.Write(m); err != nil {
							return
						}
					}
					<-quit // never read the client's messages
				}}
				d := etcp.Dialer{NetDialer: s}
				if tt.tcp {
					d.NetDialer = tcpStyleDialer{inner: s, reset: tt.rst}
				}
				conn, err := d.Dial(t.Context(), testAddr, testID, testKey)
				if err != nil {
					t.Fatalf("Dial: %v", err)
				}
				defer func() {
					_ = conn.Close()
					close(quit)
					s.wg.Wait()
				}()
				start := time.Now()
				if err := conn.WritePacket(t.Context(), numbered(0, 10)); err != nil {
					t.Fatalf("WritePacket: %v", err)
				}
				if _, err := readPacket(t, conn); !errors.Is(err, etcp.ErrIntegrity) {
					t.Fatalf("ReadPacket = %v, want ErrIntegrity", err)
				}
				if got := s.dials.Load(); got != 2 {
					t.Fatalf("dials = %d, want 2: a bad recover message must not be retried", got)
				}
				// The reader's failure unblocks our stuck write at once (no
				// fake time passes), not after the handshake idle timeout.
				if elapsed := time.Since(start); elapsed >= time.Second {
					t.Fatalf("recover failed after %v, want at once, not after the idle timeout", elapsed)
				}
			})
		})
	}
}

// A redial whose ConnectResponse is oversized or does not decode can only
// come from a broken or hostile server; as on the stream, the Conn ends
// with ErrIntegrity instead of redialing forever.
func TestBadHandshakeMessageIsFatal(t *testing.T) {
	tests := []struct {
		name string
		msg  []byte
	}{
		{name: "length above the limit", msg: binary.LittleEndian.AppendUint64(nil, wire.MaxMessageSize+1)},
		{name: "body that does not decode", msg: append(binary.LittleEndian.AppendUint64(nil, 3), 0xff, 0xff, 0xff)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := &scripted{handle: func(i int, c *rawServer) {
					if i == 0 {
						c.acceptFrames(1) // drop the link after the test's first packet
						return
					}
					var req protocol.ConnectRequest
					if wire.ReadMessage(c.br, &req) != nil {
						return
					}
					if _, err := c.conn.Write(tt.msg); err == nil {
						c.drain()
					}
				}}
				d := etcp.Dialer{NetDialer: s}
				conn, err := d.Dial(t.Context(), testAddr, testID, testKey)
				if err != nil {
					t.Fatalf("Dial: %v", err)
				}
				defer func() {
					_ = conn.Close()
					s.wg.Wait()
				}()
				if err := conn.WritePacket(t.Context(), numbered(0, 10)); err != nil {
					t.Fatalf("WritePacket: %v", err)
				}
				if _, err := readPacket(t, conn); !errors.Is(err, etcp.ErrIntegrity) {
					t.Fatalf("ReadPacket = %v, want ErrIntegrity", err)
				}
				if got := s.dials.Load(); got != 2 {
					t.Fatalf("dials = %d, want 2: a bad handshake message must not be retried", got)
				}
			})
		})
	}
}
