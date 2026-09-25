package etcp_test

import (
	"context"
	"errors"
	"net"
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
