package etcp_test

import (
	"context"
	"errors"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tphakala/et-go/internal/etcp"
)

// A writer blocked on a full backlog is released by its own context, with
// that context's cause, and by Close, with net.ErrClosed. An already
// cancelled context is refused without queueing.
func TestBackpressureWaitEnds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, etcp.Dialer{ReplayLimit: 1 << 10})
		defer h.close()

		synctest.Wait()
		h.net.SetRefuse(true)
		h.net.CutAll()
		synctest.Wait()
		errCause := errors.New("caller gave up")
		ctx, cancel := context.WithCancelCause(t.Context())
		cancel(errCause)
		if err := h.conn.WritePacket(ctx, numbered(0, 10)); !errors.Is(err, errCause) {
			t.Fatalf("write with a cancelled context = %v, want %v", err, errCause)
		}
		if err := h.conn.WritePacket(t.Context(), numbered(0, 2048)); err != nil {
			t.Fatalf("first write: %v", err)
		}

		ctx, cancel = context.WithCancelCause(t.Context())
		done := make(chan error, 1)
		go func() { done <- h.conn.WritePacket(ctx, numbered(1, 10)) }()
		synctest.Wait()
		cancel(errCause)
		if err := within(t, done); !errors.Is(err, errCause) {
			t.Fatalf("blocked write after cancel = %v, want %v", err, errCause)
		}

		go func() { done <- h.conn.WritePacket(t.Context(), numbered(1, 10)) }()
		synctest.Wait()
		if err := h.conn.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if err := within(t, done); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("blocked write after Close = %v, want net.ErrClosed", err)
		}
	})
}

func TestBackpressureWhileDisconnected(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, etcp.Dialer{ReplayLimit: 4 << 10})
		defer h.close()

		synctest.Wait()
		h.net.SetRefuse(true)
		h.net.CutAll()
		synctest.Wait() // the dead link is noticed and the supervisor is backing off

		// Each packet seals to 1024+2+16 = 1042 bytes. Writes are admitted
		// while the unsent backlog is at most 4 KiB, so the fourth takes it
		// past the limit and the fifth must wait.
		for i := range 4 {
			if err := h.conn.WritePacket(t.Context(), numbered(i, 1024)); err != nil {
				t.Fatalf("WritePacket %d: %v", i, err)
			}
		}
		done := make(chan error, 1)
		go func() { done <- h.conn.WritePacket(t.Context(), numbered(4, 1024)) }()
		synctest.Sleep(time.Minute)
		select {
		case err := <-done:
			t.Fatalf("fifth write returned %v while the backlog was full", err)
		default:
		}

		h.net.SetRefuse(false)
		if err := within(t, done); err != nil {
			t.Fatalf("fifth write after reconnect: %v", err)
		}
		if err := expectNumbered(t.Context(), 5, h.srv.Recv); err != nil {
			t.Fatal(err)
		}
	})
}
