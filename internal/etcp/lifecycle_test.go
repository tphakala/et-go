package etcp_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tphakala/et-go/internal/etcp"
	"github.com/tphakala/et-go/internal/etservertest"
	"github.com/tphakala/et-go/internal/protocol"
	"github.com/tphakala/et-go/internal/wire"
)

// A link that lived past the reset period restarts the backoff schedule, so
// the first redial after it drops is immediate even when an earlier outage
// had pushed the delay to its cap.
func TestBackoffResetsAfterLongLink(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv := etservertest.NewServer(testID, testKey)
		nw := etservertest.NewNetwork(srv)
		defer nw.Close()
		clock := &dialClock{inner: nw}
		d := etcp.Dialer{NetDialer: clock}
		conn, err := d.Dial(t.Context(), testAddr, testID, testKey)
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		defer func() { _ = conn.Close() }()
		if err := conn.WritePacket(t.Context(), numbered(0, 10)); err != nil {
			t.Fatalf("WritePacket: %v", err)
		}

		synctest.Wait()
		nw.SetRefuse(true)
		nw.CutAll()
		synctest.Sleep(time.Minute) // the backoff reaches its 5 s cap
		nw.SetRefuse(false)
		synctest.Sleep(10 * time.Second) // one capped delay: reconnected
		synctest.Sleep(31 * time.Second) // the new link outlives the 30 s reset period

		clock.mu.Lock()
		before := len(clock.times)
		clock.mu.Unlock()
		nw.CutAll()
		// No fake time passes in synctest.Wait, so a redial seen here
		// happened at the moment of the cut, with no backoff delay.
		synctest.Wait()

		clock.mu.Lock()
		defer clock.mu.Unlock()
		if len(clock.times) <= before {
			t.Fatal("no immediate redial after the long-lived link was cut: the backoff was not reset")
		}
	})
}

// Close returns even while the reader is blocked handing a packet to a
// caller that has stopped reading.
func TestCloseWithFullInbox(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, etcp.Dialer{})
		defer h.close()
		for i := range 200 { // more than the Conn buffers, so its reader blocks
			if err := h.srv.Send(t.Context(), numbered(i, 10)); err != nil {
				t.Fatalf("Send: %v", err)
			}
		}
		synctest.Wait()

		done := make(chan error, 1)
		go func() { done <- h.conn.Close() }()
		if err := within(t, done); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
}

// A blocked ReadPacket returns its context's cause when the caller gives up.
func TestReadPacketContextCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, etcp.Dialer{})
		defer h.close()

		errCause := errors.New("caller gave up")
		ctx, cancel := context.WithCancelCause(t.Context())
		done := make(chan error, 1)
		go func() {
			_, err := h.conn.ReadPacket(ctx)
			done <- err
		}()
		synctest.Wait()
		cancel(errCause)
		if err := within(t, done); !errors.Is(err, errCause) {
			t.Fatalf("ReadPacket = %v, want %v", err, errCause)
		}
	})
}

// A frame length above wire.MaxFrameSize can only come from a broken or
// hostile peer; the Conn ends with ErrIntegrity instead of redialing.
func TestOversizedServerFrameIsFatal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &scripted{handle: func(_ int, c *rawServer) {
			if c.respond(protocol.ConnectStatus_NEW_CLIENT) != nil {
				return
			}
			_, _ = c.conn.Write(binary.BigEndian.AppendUint32(nil, wire.MaxFrameSize+1))
			c.drain()
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
		if _, err := readPacket(t, conn); !errors.Is(err, etcp.ErrIntegrity) {
			t.Fatalf("ReadPacket = %v, want ErrIntegrity", err)
		}
		synctest.Sleep(time.Minute)
		if got := s.dials.Load(); got != 1 {
			t.Fatalf("dials = %d, want 1: an oversized frame must not be retried", got)
		}
	})
}

// The Logger receives link events, and nothing logged carries the passkey.
func TestLoggerRecordsLinkEventsWithoutPasskey(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
		h := newHarness(t, etcp.Dialer{Logger: logger})
		defer h.close()
		if err := h.conn.WritePacket(t.Context(), numbered(0, 10)); err != nil {
			t.Fatalf("WritePacket: %v", err)
		}
		synctest.Wait()
		h.net.CutAll()
		if err := expectNumbered(t.Context(), 1, h.srv.Recv); err != nil {
			t.Fatalf("server side: %v", err)
		}
		synctest.Wait()
		// Close joins every goroutine that logs, so buf is safe to read.
		if err := h.conn.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		out := buf.String()
		for _, want := range []string{"etcp: link lost", "etcp: link restored"} {
			if !strings.Contains(out, want) {
				t.Errorf("log lacks %q:\n%s", want, out)
			}
		}
		if strings.Contains(out, testKey) {
			t.Fatalf("log contains the passkey:\n%s", out)
		}
	})
}

// A write whose context was already cancelled is refused and never queued:
// the server's first packet is the next one written.
func TestCancelledWriteNotQueued(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, etcp.Dialer{})
		defer h.close()

		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := h.conn.WritePacket(ctx, numbered(99, 10)); !errors.Is(err, context.Canceled) {
			t.Fatalf("WritePacket with a cancelled context = %v, want context.Canceled", err)
		}
		if err := h.conn.WritePacket(t.Context(), numbered(0, 10)); err != nil {
			t.Fatalf("WritePacket: %v", err)
		}
		if err := expectNumbered(t.Context(), 1, h.srv.Recv); err != nil {
			t.Fatalf("server side: %v", err)
		}
	})
}

// A write is admitted while the unsent backlog is at most ReplayLimit: with
// the backlog exactly at the limit, the next write still goes in.
func TestBackpressureBoundary(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const sealed = 1024 + 2 + 16 // payload, packet header, MAC
		h := newHarness(t, etcp.Dialer{ReplayLimit: 3 * sealed})
		defer h.close()

		synctest.Wait()
		h.net.SetRefuse(true)
		h.net.CutAll()
		synctest.Wait()
		for i := range 3 { // the backlog is now exactly ReplayLimit
			if err := h.conn.WritePacket(t.Context(), numbered(i, 1024)); err != nil {
				t.Fatalf("WritePacket %d: %v", i, err)
			}
		}
		done := make(chan error, 1)
		go func() { done <- h.conn.WritePacket(t.Context(), numbered(3, 1024)) }()
		synctest.Wait()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("write at the limit: %v", err)
			}
		default:
			t.Fatal("a write with the backlog exactly at ReplayLimit blocked")
		}
		h.net.SetRefuse(false)
		if err := expectNumbered(t.Context(), 4, h.srv.Recv); err != nil {
			t.Fatalf("server side: %v", err)
		}
	})
}

// A caller that has stopped reading must not stop the Conn from replacing a
// dead link: the reader, blocked handing a packet to a full inbox, has to
// let the link go so writes reach the server over the next one. The packet
// in its hand is already counted as received, so it must still be
// delivered, once and in order.
func TestReconnectWhileCallerNotReading(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, etcp.Dialer{})
		defer h.close()

		const n = 100 // more than the inbox holds
		if err := h.conn.WritePacket(t.Context(), numbered(0, 10)); err != nil {
			t.Fatalf("WritePacket 0: %v", err)
		}
		for i := range n {
			if err := h.srv.Send(t.Context(), numbered(i, 10)); err != nil {
				t.Fatalf("Send %d: %v", i, err)
			}
		}
		synctest.Wait() // the inbox is full and the reader is blocked
		h.net.CutAll()
		synctest.Wait()
		if err := h.conn.WritePacket(t.Context(), numbered(1, 10)); err != nil {
			t.Fatalf("WritePacket 1: %v", err)
		}
		if err := expectNumbered(t.Context(), 2, h.srv.Recv); err != nil {
			t.Fatalf("server side, caller not reading: %v", err)
		}
		// Cut the replacement link too, while its reader is still trying
		// to hand over the packet the first link left pending.
		synctest.Wait()
		h.net.CutAll()
		synctest.Wait()
		if err := h.conn.WritePacket(t.Context(), numbered(2, 10)); err != nil {
			t.Fatalf("WritePacket 2: %v", err)
		}
		if p, err := h.srv.Recv(t.Context()); err != nil {
			t.Fatalf("server side, second cut: %v", err)
		} else if got, err := number(p); err != nil || got != 2 {
			t.Fatalf("server side, second cut: got packet %d (%v), want 2", got, err)
		}
		if err := expectNumbered(t.Context(), n, h.conn.ReadPacket); err != nil {
			t.Fatalf("client side: %v", err)
		}
	})
}
