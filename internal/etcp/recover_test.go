package etcp_test

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tphakala/et-go/internal/etcp"
	"github.com/tphakala/et-go/internal/protocol"
	"github.com/tphakala/et-go/internal/wire"
	"golang.org/x/crypto/nacl/secretbox"
)

func TestSessionEndedIsEOF(t *testing.T) {
	if !errors.Is(etcp.ErrSessionEnded, io.EOF) {
		t.Fatal("ErrSessionEnded does not wrap io.EOF")
	}
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, etcp.Dialer{})
		defer h.close()
		h.srv.EndSession()
		_, err := readPacket(t, h.conn)
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

// Writes keep arriving across repeated cuts and reconnects; none may be
// skipped or sent twice. Recovery over net.Pipe takes no fake time, so the
// cuts rarely land inside a recover exchange; TestWritePacketRacingRecovery
// pins that race deterministically.
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

// A catchup too large for one handshake message (etserver refuses messages
// above wire.MaxMessageSize, src/base/SocketHandler.hpp:60 at et-v7.0.0) can
// never be sent, so the Conn ends with ErrReplayExceeded instead of redialing
// forever. The limit is lowered so a few KiB reach it.
func TestOversizedCatchupIsFatal(t *testing.T) {
	defer etcp.SetMaxCatchupSize(1 << 10)()
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		s := &scripted{handle: func(i int, c *rawServer) {
			if i == 0 {
				// Accept the link but read nothing until released, so the
				// packets below stay unsent and land in the catchup.
				if c.respond(protocol.ConnectStatus_NEW_CLIENT) == nil {
					<-release
				}
				return
			}
			c.claimSequence(0)
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
		var releaseOnce sync.Once
		releaseServer := func() { releaseOnce.Do(func() { close(release) }) }
		defer releaseServer() // runs first, so an early failure cannot strand s.wg.Wait
		for i := range 8 {    // about 8 KiB of catchup against a 1 KiB limit
			if err := conn.WritePacket(t.Context(), numbered(i, 1024)); err != nil {
				t.Fatalf("WritePacket: %v", err)
			}
		}
		synctest.Wait()
		releaseServer()

		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		if _, err := conn.ReadPacket(ctx); !errors.Is(err, etcp.ErrReplayExceeded) {
			t.Fatalf("ReadPacket = %v, want ErrReplayExceeded", err)
		}
		if got := s.dials.Load(); got != 2 {
			t.Fatalf("dials = %d, want 2: an oversized catchup must not be retried", got)
		}
	})
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
		if _, err := readPacket(t, conn); !errors.Is(err, etcp.ErrIntegrity) {
			t.Fatalf("ReadPacket = %v, want ErrIntegrity", err)
		}
		synctest.Sleep(time.Minute)
		if got := s.dials.Load(); got != 1 {
			t.Fatalf("dials = %d, want 1: integrity failures must not be retried", got)
		}
	})
}

// A packet written after the recover snapshot is not in our catchup, so the
// new link must send it. Reading ring.next() a second time at the end of the
// exchange would skip it.
//
// In the second row the packets written during recovery exceed ReplayLimit,
// so recover's trim must stop at the snapshot: trimming past it would drop
// packets no link has sent.
func TestWritePacketRacingRecovery(t *testing.T) {
	tests := []struct {
		name   string
		limit  int
		during int // packets written between the snapshot and resume
	}{
		{name: "one packet", during: 1},
		{name: "more than ReplayLimit", limit: 64, during: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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
						// Later links (the watcher may redial, since drain
						// never echoes probes) end at once; got already holds
						// packet 1's number, so they cannot change the verdict.
					}
				}}
				d := etcp.Dialer{NetDialer: s, ReplayLimit: tt.limit}
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
				for i := 1; i <= tt.during; i++ {
					if err := conn.WritePacket(t.Context(), numbered(i, 10)); err != nil {
						t.Fatalf("WritePacket %d: %v", i, err)
					}
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
				// The new link has sent everything, so recover must have
				// counted the packets written during it exactly once.
				synctest.Wait()
				if got := etcp.Unsent(conn); got != 0 {
					t.Fatalf("unsent = %d bytes after the link drained, want 0", got)
				}
			})
		})
	}
}

// A link that recovers and then dies before writing anything must not let the
// replay ring grow past ReplayLimit: recover counts our catchup as sent, which
// frees WritePacket to admit another ReplayLimit of packets, so recover must
// also trim what it has now written. The server here reports its
// true received count in each SequenceHeader and drops every link right after
// the exchange, while the caller keeps writing.
func TestFlappingLinkKeepsRingBounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const (
			limit   = 1 << 10
			payload = 100
			sealed  = 2 + secretbox.Overhead + payload // one ring entry
		)
		var received, caughtUp, lastCatchup atomic.Int32
		s := &scripted{handle: func(i int, c *rawServer) {
			if i == 0 {
				if c.respond(protocol.ConnectStatus_NEW_CLIENT) != nil {
					return
				}
				if _, err := wire.ReadFrame(c.br, nil); err == nil {
					received.Store(1)
				}
				return
			}
			if c.respond(protocol.ConnectStatus_RETURNING_CLIENT) != nil {
				return
			}
			var mine protocol.SequenceHeader
			if wire.ReadMessage(c.br, &mine) != nil {
				return
			}
			sh := &protocol.SequenceHeader{}
			sh.SetSequenceNumber(received.Load())
			if wire.WriteMessage(c.conn, sh) != nil {
				return
			}
			var theirs protocol.CatchupBuffer
			if wire.ReadMessage(c.br, &theirs) != nil {
				return
			}
			received.Add(int32(len(theirs.GetBuffer())))
			caughtUp.Add(int32(len(theirs.GetBuffer())))
			lastCatchup.Store(int32(len(theirs.GetBuffer())))
			_ = wire.WriteMessage(c.conn, &protocol.CatchupBuffer{})
		}}
		d := etcp.Dialer{NetDialer: s, ReplayLimit: limit}
		conn, err := d.Dial(t.Context(), testAddr, testID, testKey)
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		var wg sync.WaitGroup
		defer func() {
			_ = conn.Close()
			wg.Wait()
			s.wg.Wait()
		}()
		wg.Go(func() {
			for i := 0; ; i++ {
				if conn.WritePacket(t.Context(), numbered(i, payload)) != nil {
					return
				}
			}
		})
		// Cap the wait in fake time: a regression that ends the Conn stops
		// the redials, and an uncapped loop would hang the suite.
		deadline := time.Now().Add(time.Hour)
		for s.dials.Load() < 12 {
			if time.Now().After(deadline) {
				t.Fatalf("only %d dials after an hour", s.dials.Load())
			}
			time.Sleep(time.Second)
		}
		if caughtUp.Load() == 0 {
			t.Fatal("no catchup reached the server; the test exercises nothing")
		}
		// The caller writes between every pair of links, so an empty latest
		// catchup means WritePacket stayed blocked: recover must release it.
		if lastCatchup.Load() == 0 {
			t.Fatal("writer stalled: the last recover carried no catchup")
		}
		// At most ReplayLimit of written entries survive a trim, plus the
		// unsent backlog WritePacket admits: ReplayLimit and one packet.
		if got, bound := etcp.RingBytes(conn), 2*limit+sealed; got > bound {
			t.Fatalf("ring holds %d bytes after %d links, want at most %d", got, s.dials.Load(), bound)
		}
	})
}
