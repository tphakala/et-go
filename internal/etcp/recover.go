package etcp

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/tphakala/et-go/internal/protocol"
	"github.com/tphakala/et-go/internal/wire"
	"google.golang.org/protobuf/proto"
)

// supervise owns reconnects: it runs a link until it dies, then redials with
// backoff and runs the next one, until the Conn ends.
func (c *Conn) supervise(nc net.Conn, catchup [][]byte) {
	var b backoff
	for {
		started := time.Now()
		cause := c.runLink(nc, catchup)
		if c.ctx.Err() != nil {
			return
		}
		c.logger.Info("etcp: link lost", "cause", cause)
		if time.Since(started) >= backoffReset {
			b.reset()
		}
		var err error
		nc, catchup, err = c.reconnect(&b)
		if err != nil {
			c.cancel(err)
			return
		}
		c.logger.Info("etcp: link restored")
	}
}

// reconnect dials until a link is recovered, the error is fatal, or the Conn ends.
func (c *Conn) reconnect(b *backoff) (net.Conn, [][]byte, error) {
	for {
		if d := b.next(); d > 0 {
			t := time.NewTimer(d)
			select {
			case <-t.C:
			case <-c.ctx.Done():
				t.Stop()
				return nil, nil, context.Cause(c.ctx)
			}
		}
		nc, catchup, err := c.connect(c.ctx, false)
		if err == nil {
			return nc, catchup, nil
		}
		if isFatal(err) || c.ctx.Err() != nil {
			return nil, nil, err
		}
		c.logger.Debug("etcp: reconnect attempt failed", "err", err)
	}
}

// recover runs the client side of the recover exchange and returns the peer's
// catchup entries. Upstream has each side write its SequenceHeader, read the
// peer's, write its CatchupBuffer, then read the peer's
// (src/base/Connection.cpp:105-143). Because both sides write before they
// read at every step, this client reads the peer's two messages concurrently
// with writing its own: done in sequence, the exchange deadlocks as soon as
// the connection cannot buffer what both sides write (at once over net.Pipe,
// and over TCP once both catchups exceed the socket buffers).
func (c *Conn) recover(conn net.Conn) ([][]byte, error) {
	var (
		peer   protocol.SequenceHeader
		theirs protocol.CatchupBuffer
		gotSeq = make(chan error, 1)
		gotAll = make(chan error, 1)
		wg     sync.WaitGroup
	)
	wg.Go(func() {
		err := wire.ReadMessage(conn, &peer)
		gotSeq <- err
		if err == nil {
			err = wire.ReadMessage(conn, &theirs)
		}
		gotAll <- err
	})

	snap, err := c.writeRecover(conn, gotSeq, &peer)
	if err != nil {
		_ = conn.Close() // unblock the reader
	}
	rerr := <-gotAll
	wg.Wait()
	if err != nil {
		return nil, err
	}
	if rerr != nil {
		return nil, fmt.Errorf("etcp: read catchup: %w", rerr)
	}

	c.mu.Lock()
	c.unsent -= c.ring.bytesBetween(c.flushed, snap)
	c.flushed = snap
	room := c.unsent <= c.limit
	c.mu.Unlock()
	if room {
		signal(c.space)
	}
	return theirs.GetBuffer(), nil
}

// maxCatchupSize is the largest CatchupBuffer we send: etserver refuses
// handshake messages above 128 MiB (src/base/SocketHandler.hpp:60 at
// et-v7.0.0), the same bound as wire.MaxMessageSize. It is a variable only
// so a test can lower it.
var maxCatchupSize = wire.MaxMessageSize

// writeRecover writes our half of the recover exchange and returns the
// sequence number the new link starts sending from. It fails with
// ErrReplayExceeded when the peer's position is outside the retained window
// or our catchup is too large for one message.
func (c *Conn) writeRecover(conn net.Conn, gotSeq <-chan error, peer *protocol.SequenceHeader) (int64, error) {
	mine := &protocol.SequenceHeader{}
	mine.SetSequenceNumber(int32(c.recvSeq))
	if err := wire.WriteMessage(conn, mine); err != nil {
		return 0, fmt.Errorf("etcp: write sequence: %w", err)
	}
	if err := <-gotSeq; err != nil {
		return 0, fmt.Errorf("etcp: read sequence: %w", err)
	}

	// Read sendSeq exactly once. Packets written after this snapshot are not
	// in our catchup; the new link sends them because it starts at snap.
	c.mu.Lock()
	snap := c.ring.next()
	ours, ok := c.ring.since(int64(peer.GetSequenceNumber()), snap)
	first := c.ring.first
	c.mu.Unlock()
	if !ok {
		return 0, fmt.Errorf("%w: peer is at %d, retained window is [%d, %d]",
			ErrReplayExceeded, peer.GetSequenceNumber(), first, snap)
	}
	cb := &protocol.CatchupBuffer{}
	cb.SetBuffer(ours)
	// A catchup the server cannot accept would fail the same way on every
	// redial, so it ends the Conn instead.
	if size := proto.Size(cb); size > maxCatchupSize {
		return 0, fmt.Errorf("%w: catchup of %d bytes exceeds the %d-byte message limit",
			ErrReplayExceeded, size, maxCatchupSize)
	}
	if err := wire.WriteMessage(conn, cb); err != nil {
		return 0, fmt.Errorf("etcp: write catchup: %w", err)
	}
	return snap, nil
}
