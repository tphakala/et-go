package etcp_test

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tphakala/et-go/internal/etcp"
	"github.com/tphakala/et-go/internal/protocol"
	"github.com/tphakala/et-go/internal/wire"
)

func TestSessionEndedIsEOF(t *testing.T) {
	if !errors.Is(etcp.ErrSessionEnded, io.EOF) {
		t.Fatal("ErrSessionEnded does not wrap io.EOF")
	}
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, etcp.Dialer{})
		defer h.close()
		h.srv.EndSession()
		_, err := h.conn.ReadPacket(t.Context())
		if !errors.Is(err, etcp.ErrSessionEnded) || !errors.Is(err, io.EOF) {
			t.Fatalf("ReadPacket = %v, want ErrSessionEnded wrapping io.EOF", err)
		}
	})
}

// Both sides have a catchup to replay. The fake server writes its whole
// catchup before reading ours, as upstream does, and net.Pipe buffers nothing,
// so this completes only if the client reads concurrently.
func TestCatchupBothWays(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, etcp.Dialer{})
		defer h.close()

		synctest.Wait()
		h.net.SetRefuse(true)
		h.net.CutAll()
		synctest.Wait()
		for i := range 50 {
			if err := h.conn.WritePacket(t.Context(), numbered(i, 4096)); err != nil {
				t.Fatalf("WritePacket: %v", err)
			}
			if err := h.srv.Send(t.Context(), numbered(i, 4096)); err != nil {
				t.Fatalf("Send: %v", err)
			}
		}
		h.net.SetRefuse(false)

		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		if err := expectNumbered(ctx, 50, h.srv.Recv); err != nil {
			t.Fatalf("server side: %v", err)
		}
		if err := expectNumbered(ctx, 50, h.conn.ReadPacket); err != nil {
			t.Fatalf("client side: %v", err)
		}
	})
}

// Writes keep arriving while recovery runs; none may be skipped or sent twice.
func TestWriteDuringRecovery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, etcp.Dialer{})
		defer h.close()

		const n = 2000
		var wg sync.WaitGroup
		wg.Go(func() {
			for i := range n {
				if err := h.conn.WritePacket(t.Context(), numbered(i, 64)); err != nil {
					t.Errorf("WritePacket %d: %v", i, err)
					return
				}
				if i%100 == 0 {
					time.Sleep(10 * time.Millisecond)
				}
			}
		})
		wg.Go(func() {
			for range 10 {
				time.Sleep(15 * time.Millisecond)
				h.net.CutAll()
			}
		})
		if err := expectNumbered(t.Context(), n, h.srv.Recv); err != nil {
			t.Fatal(err)
		}
		wg.Wait()
	})
}

// The server claims to have received less than we still hold (or more than
// we ever sent): the session cannot be recovered and the Conn ends.
func TestReplayWindowExceeded(t *testing.T) {
	tests := []struct {
		name string
		peer int32
	}{
		{name: "peer behind the window", peer: 0},
		{name: "peer ahead of us", peer: 1_000_000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := &scripted{handle: func(i int, c *rawServer) {
					if i == 0 {
						c.acceptFrames(100) // the client writes and trims its ring
						return
					}
					c.claimSequence(tt.peer)
				}}
				d := etcp.Dialer{NetDialer: s, ReplayLimit: 1 << 10}
				conn, err := d.Dial(t.Context(), testAddr, testID, testKey)
				if err != nil {
					t.Fatalf("Dial: %v", err)
				}
				defer func() {
					_ = conn.Close()
					s.wg.Wait()
				}()
				for i := range 100 { // 100 KiB through a 1 KiB window
					if err := conn.WritePacket(t.Context(), numbered(i, 1024)); err != nil {
						t.Fatalf("WritePacket: %v", err)
					}
				}
				ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
				defer cancel()
				if _, err := conn.ReadPacket(ctx); !errors.Is(err, etcp.ErrReplayExceeded) {
					t.Fatalf("ReadPacket = %v, want ErrReplayExceeded", err)
				}
			})
		})
	}
}

// A packet that fails authentication ends the Conn; it is not retried.
func TestIntegrityFailureIsFatal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &scripted{handle: func(_ int, c *rawServer) {
			if c.respond(protocol.ConnectStatus_NEW_CLIENT) != nil {
				return
			}
			garbage := wire.AppendPacket(nil, true, protocol.HeaderTerminalBuffer, make([]byte, 40))
			_ = wire.WriteFrame(c.conn, garbage)
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
		if _, err := conn.ReadPacket(t.Context()); !errors.Is(err, etcp.ErrIntegrity) {
			t.Fatalf("ReadPacket = %v, want ErrIntegrity", err)
		}
		synctest.Sleep(time.Minute)
		if got := s.dials.Load(); got != 1 {
			t.Fatalf("dials = %d, want 1: integrity failures must not be retried", got)
		}
	})
}

// A packet written after the recover snapshot is not in our catchup, so the
// new link must send it. Reading sendSeq a second time at the end of the
// exchange would skip it.
func TestWritePacketRacingRecovery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		snapped := make(chan struct{})
		resume := make(chan struct{})
		got := make(chan int, 1)
		s := &scripted{handle: func(i int, c *rawServer) {
			switch i {
			case 0:
				c.acceptFrames(1) // take packet 0 off the wire, then drop the link
			case 1:
				c.pausedRecover(snapped, resume, got)
			default:
				// Later links only appear if packet 1 was lost; refuse them.
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
			t.Fatalf("WritePacket 0: %v", err)
		}
		<-snapped
		if err := conn.WritePacket(t.Context(), numbered(1, 10)); err != nil {
			t.Fatalf("WritePacket 1: %v", err)
		}
		close(resume)

		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		select {
		case n := <-got:
			if n != 1 {
				t.Fatalf("new link sent packet %d first, want 1", n)
			}
		case <-ctx.Done():
			t.Fatal("packet written during recovery never arrived")
		}
	})
}
