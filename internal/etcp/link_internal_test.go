package etcp

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tphakala/et-go/internal/protocol"
	"github.com/tphakala/et-go/internal/seal"
	"github.com/tphakala/et-go/internal/wire"
)

// shortWriter reports one byte less than it was given and no error, which
// breaks the io.Writer contract.
type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return max(len(p)-1, 0), nil }

// A writer that reports a short count without an error must fail the link
// rather than count the whole batch as sent: the unsent tail would otherwise
// be lost from replay.
func TestWriteLoopShortWrite(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var d Dialer
		c := d.newConn("et.example:2022", "XXXtestclient001", strings.Repeat("k", 32))
		defer c.cancel(nil)
		c.ring.push(make([]byte, 18))
		c.unsent += 18

		// Bounded: a writeLoop that accepted the short write would wait for
		// more data forever.
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		err := c.writeLoop(ctx, newLink(), shortWriter{})
		if !errors.Is(err, io.ErrShortWrite) {
			t.Fatalf("writeLoop = %v, want io.ErrShortWrite", err)
		}
		if c.flushed != 0 {
			t.Fatalf("flushed = %d after a short write, want 0", c.flushed)
		}
	})
}

// A packet larger than any frame the stream allows ends the Conn with
// ErrIntegrity even when it authenticates: catchup entries arrive inside one
// handshake message, so they must not bypass the frame limit that
// wire.ReadFrame applies to live frames. (Called directly: pushing a 16 MiB
// catchup through the fake network takes minutes under the race detector.)
func TestDeliverRefusesOversizedPacket(t *testing.T) {
	var d Dialer
	c := d.newConn("et.example:2022", "XXXtestclient001", strings.Repeat("k", 32))
	defer c.cancel(nil)
	var key [32]byte
	copy(key[:], strings.Repeat("k", 32))
	sealed := seal.New(&key, seal.ServerToClient).Seal(nil, make([]byte, wire.MaxFrameSize))
	b := wire.AppendPacket(nil, true, protocol.HeaderTerminalBuffer, sealed)

	err := c.deliver(t.Context(), &link{alive: make(chan struct{}, 1)}, b)
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("deliver(%d-byte packet) = %v, want ErrIntegrity", len(b), err)
	}
}

// backlogWriter accepts its first Write, during which the caller fills the
// unsent backlog as a racing WritePacket would, then blocks until ctx ends.
type backlogWriter struct {
	ctx    context.Context
	c      *Conn
	writes int
}

func (w *backlogWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes > 1 {
		<-w.ctx.Done()
		return 0, w.ctx.Err()
	}
	w.c.mu.Lock()
	for range 5 {
		w.c.ring.push(make([]byte, 20))
		w.c.unsent += 20
	}
	w.c.mu.Unlock()
	return len(p), nil
}

// The replay limit applies to written packets and to the unsent backlog
// separately: a full backlog must not trim the replay copies of packets just
// written, which may still be in flight and are needed after a cut.
func TestWriteLoopKeepsWrittenWhileBacklogFull(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d := Dialer{ReplayLimit: 100}
		c := d.newConn("et.example:2022", "XXXtestclient001", strings.Repeat("k", 32))
		defer c.cancel(nil)
		for range 5 { // 100 bytes to write, exactly the limit
			c.ring.push(make([]byte, 20))
			c.unsent += 20
		}

		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- c.writeLoop(ctx, newLink(), &backlogWriter{ctx: ctx, c: c}) }()
		synctest.Wait() // the first batch is written, the second is stuck

		c.mu.Lock()
		first, flushed, unsent := c.ring.first, c.flushed, c.unsent
		c.mu.Unlock()
		if flushed != 5 || unsent != 100 {
			t.Fatalf("flushed, unsent = %d, %d; want 5, 100", flushed, unsent)
		}
		if first != 0 {
			t.Fatalf("ring.first = %d, want 0: the 100 written bytes are within the limit", first)
		}
		cancel()
		<-done
	})
}
