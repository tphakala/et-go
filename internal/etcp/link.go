package etcp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tphakala/et-go/internal/protocol"
	"github.com/tphakala/et-go/internal/wire"
	"golang.org/x/crypto/nacl/secretbox"
)

const (
	readBufSize  = 64 << 10
	writeBufSize = 64 << 10

	// maxBatchEntries bounds how many ring entries one writer pass takes: a
	// framed entry is at least 4+2+secretbox.Overhead bytes, so no pass can
	// frame more than this many into writeBufSize. Taking the whole backlog
	// instead would copy it under c.mu on every pass, quadratic in its length.
	maxBatchEntries = writeBufSize/(4+2+secretbox.Overhead) + 1

	// progressChunk is the piece size the writer sends a batch in, so that
	// a slow uplink shows progress well within one keepAlive period.
	progressChunk = 4 << 10
)

var (
	errClosed   = net.ErrClosed
	errLinkDead = errors.New("etcp: no traffic from server")
)

// link is the per-TCP-connection state shared by its goroutines.
type link struct {
	alive      chan struct{} // cap 1: a frame arrived, or a write progressed ahead of a waiting probe
	delivering atomic.Bool   // the reader is blocked handing a packet to ReadPacket
}

// newLink returns the shared state for one TCP connection's goroutines.
func newLink() *link { return &link{alive: make(chan struct{}, 1)} }

// runLink runs a reader, a writer and a liveness watcher on nc until one of
// them fails, then joins all three and returns the cause.
func (c *Conn) runLink(nc net.Conn, catchup [][]byte) error {
	ctx, cancel := context.WithCancelCause(c.ctx)
	defer cancel(nil)
	stop := context.AfterFunc(ctx, func() { _ = nc.Close() })
	defer stop()

	l := newLink()
	var wg sync.WaitGroup
	wg.Go(func() { cancel(c.readLoop(ctx, l, nc, catchup)) })
	wg.Go(func() { cancel(c.writeLoop(ctx, l, nc)) })
	wg.Go(func() { cancel(c.watch(ctx, l)) })
	wg.Wait()
	_ = nc.Close()
	return context.Cause(ctx)
}

// readLoop hands over packets an earlier link left pending, then delivers the
// peer's catchup, then frames from the link. The first frame ends the hold
// recover placed on our catchup (see releaseHold).
func (c *Conn) readLoop(ctx context.Context, l *link, r io.Reader, catchup [][]byte) error {
	for len(c.pending) > 0 {
		if err := c.handOver(ctx, l, c.pending[0]); err != nil {
			return err // pending stays as it is for the next link
		}
		c.pending[0] = protocol.Packet{}
		c.pending = c.pending[1:]
	}
	c.pending = nil
	for _, b := range catchup {
		if err := c.deliver(ctx, l, b); err != nil {
			return err
		}
	}
	br := bufio.NewReaderSize(r, readBufSize)
	var (
		buf      []byte
		released bool
	)
	for {
		frame, err := wire.ReadFrame(br, buf)
		if err != nil {
			if errors.Is(err, wire.ErrTooLarge) {
				return c.fail(fmt.Errorf("%w: %w", ErrIntegrity, err))
			}
			return fmt.Errorf("etcp: read: %w", err)
		}
		buf = frame
		signal(l.alive)
		if !released {
			c.releaseHold()
			released = true
		}
		if err := c.deliver(ctx, l, frame); err != nil {
			return err
		}
	}
}

// deliver opens one sealed packet and hands it to ReadPacket. A malformed or
// unauthenticated packet ends the Conn: the streams are out of sync or tampered
// with, and reconnecting would replay into the same state. After a failed
// Open the inbound nonce has advanced, so the stream cannot be resumed;
// upstream treats a failed decrypt as fatal too
// (src/base/CryptoHandler.cpp:40-42 at et-v7.0.0).
func (c *Conn) deliver(ctx context.Context, l *link, b []byte) error {
	// Live frames are bounded by wire.ReadFrame; catchup entries arrive in
	// one handshake message, so bound them here to the same limit.
	if len(b) > wire.MaxFrameSize {
		return c.fail(fmt.Errorf("%w: packet of %d bytes: %w", ErrIntegrity, len(b), wire.ErrTooLarge))
	}
	encrypted, h, payload, err := wire.ParsePacket(b)
	if err != nil {
		return c.fail(fmt.Errorf("%w: %w", ErrIntegrity, err))
	}
	if !encrypted {
		return c.fail(fmt.Errorf("%w: unencrypted packet", ErrIntegrity))
	}
	plain, err := c.in.Open(nil, payload)
	if err != nil {
		return c.fail(fmt.Errorf("%w: %w", ErrIntegrity, err))
	}
	c.recvSeq++
	p := protocol.Packet{Header: h, Payload: plain}
	if err := c.handOver(ctx, l, p); err != nil {
		// Counted in recvSeq, so the server will not resend it: keep it
		// for the next link reader rather than drop it.
		c.pending = append(c.pending, p)
		return err
	}
	return nil
}

// handOver gives p to ReadPacket, or fails when the link ends first (ctx is
// the link's context). Waiting on the Conn alone would keep a dead link
// alive, and with it every write, until the caller read again.
func (c *Conn) handOver(ctx context.Context, l *link, p protocol.Packet) error {
	l.delivering.Store(true)
	defer l.delivering.Store(false)
	select {
	case c.inbox <- p:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

// releaseHold ends the hold recover placed on our catchup and trims the ring
// back to ReplayLimit. It is called on a link's first frame: upstream writes
// on a recovered socket only after it has decoded our whole catchup, since
// BackedWriter::write waits on the recover mutex that Connection::recover
// holds until then (src/base/BackedWriter.cpp:17-18 and
// src/base/Connection.cpp:109,134-142 at et-v7.0.0).
func (c *Conn) releaseHold() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ring.held {
		c.ring.held = false
		c.ring.trim(c.flushed, c.unsent)
	}
}

// writeLoop sends ring entries from flushed onwards, trimming the ring and
// releasing blocked writers as the backlog drains. It frames a batch of
// entries (at least one, then up to writeBufSize bytes) into one reused
// buffer and sends it in progressChunk pieces, so the steady state allocates
// nothing per packet. Entries are immutable once sealed, so they are framed
// outside the lock.
//
// A piece written while a probe still waits behind it signals l.alive. The
// probe's echo cannot come before the probe is sent, and once the socket's
// send buffer is full a write completes only as the peer acknowledges data,
// so a backlog draining ahead of the probe proves the path is alive while
// the server sends nothing. Other writes prove nothing (a dead link's send
// buffer still takes them), so data written with no probe behind it, the
// probe itself included, never counts.
func (c *Conn) writeLoop(ctx context.Context, l *link, w io.Writer) error {
	var (
		batch [][]byte
		buf   []byte
	)
	for {
		c.mu.Lock()
		base, probe := c.flushed, c.lastProbe
		batch = c.ring.appendRange(batch[:0], base, min(c.ring.next(), base+maxBatchEntries))
		c.mu.Unlock()
		if len(batch) == 0 {
			select {
			case <-c.wake:
				continue
			case <-ctx.Done():
				return context.Cause(ctx)
			}
		}
		buf = buf[:0]
		sent, n := 0, 0
		probeAt := -1 // offset in buf of the probe's frame, if it is in this batch
		for _, f := range batch {
			if sent > 0 && len(buf)+4+len(f) > writeBufSize {
				break
			}
			if base+int64(sent) == probe {
				probeAt = len(buf)
			}
			var err error
			// WritePacket refuses packets above wire.MaxFrameSize, so an
			// error here means the ring is corrupt and no link can help.
			if buf, err = wire.AppendFrame(buf, f); err != nil {
				return c.fail(fmt.Errorf("%w: %w", ErrIntegrity, err))
			}
			sent++
			n += len(f)
		}
		clear(batch) // drop references so trimmed entries can be collected
		for written := 0; written < len(buf); {
			piece := buf[written:min(len(buf), written+progressChunk)]
			// A short count without an error breaks the io.Writer contract;
			// counting the whole batch as sent would drop its tail from replay.
			if m, err := w.Write(piece); err != nil || m != len(piece) {
				if err == nil {
					err = io.ErrShortWrite
				}
				return fmt.Errorf("etcp: write: %w", err)
			}
			written += len(piece)
			if c.probeBehind(base+int64(sent), probeAt, written) {
				signal(l.alive)
			}
		}
		c.mu.Lock()
		c.flushed += int64(sent)
		c.unsent -= n
		room := c.releaseLocked()
		c.mu.Unlock()
		if room {
			signal(c.space)
		}
	}
}

// probeBehind reports whether a probe still waits behind the first written
// bytes of the current batch: in the batch past them (probeAt is its frame's
// offset, or -1), or queued after the batch, whose entries end before
// sequence end. Only the watcher queues probes, so one queued after the batch
// was framed is always behind it.
func (c *Conn) probeBehind(end int64, probeAt, written int) bool {
	if probeAt >= 0 {
		return written <= probeAt
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastProbe >= end
}

// watch declares the link dead after two quiet keepAlive periods, sending the
// probe after the first. A frame from the server, or writer progress ahead
// of a waiting probe (see writeLoop), breaks the quiet. Time the reader spends blocked on
// a slow ReadPacket caller does not count as silence, and neither does the
// time before the caller's first packet, when no probe may be sent.
func (c *Conn) watch(ctx context.Context, l *link) error {
	t := time.NewTimer(c.keepAlive)
	defer t.Stop()
	probed := false
	for {
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-l.alive:
			probed = false
		case <-t.C:
			switch {
			case l.delivering.Load():
				probed = false
			case c.sealedNone():
				// No probe before the caller's first packet: etserver 7.0.0
				// aborts when a session's first packet is not INITIAL_PAYLOAD
				// (src/terminal/TerminalServer.cpp:429-439 at et-v7.0.0).
				// Until then a dead link is found only by a read error or
				// by TCP keepalive when the NetDialer enables it.
				probed = false
			case probed:
				return errLinkDead
			default:
				c.probeOnce()
				probed = true
			}
		}
		t.Reset(c.keepAlive)
	}
}
