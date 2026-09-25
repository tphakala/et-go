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
)

var (
	errClosed   = net.ErrClosed
	errLinkDead = errors.New("etcp: no traffic from server")
)

// link is the per-TCP-connection state shared by its goroutines.
type link struct {
	alive      chan struct{} // cap 1: a frame arrived
	delivering atomic.Bool   // the reader is blocked handing a packet to ReadPacket
}

// runLink runs a reader, a writer and a liveness watcher on nc until one of
// them fails, then joins all three and returns the cause.
func (c *Conn) runLink(nc net.Conn, catchup [][]byte) error {
	ctx, cancel := context.WithCancelCause(c.ctx)
	defer cancel(nil)
	stop := context.AfterFunc(ctx, func() { _ = nc.Close() })
	defer stop()

	l := &link{alive: make(chan struct{}, 1)}
	var wg sync.WaitGroup
	wg.Go(func() { cancel(c.readLoop(l, nc, catchup)) })
	wg.Go(func() { cancel(c.writeLoop(ctx, nc)) })
	wg.Go(func() { cancel(c.watch(ctx, l)) })
	wg.Wait()
	_ = nc.Close()
	return context.Cause(ctx)
}

// readLoop delivers the peer's catchup first, then frames from the link.
func (c *Conn) readLoop(l *link, r io.Reader, catchup [][]byte) error {
	for _, b := range catchup {
		if err := c.deliver(l, b); err != nil {
			return err
		}
	}
	br := bufio.NewReaderSize(r, readBufSize)
	var buf []byte
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
		if err := c.deliver(l, frame); err != nil {
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
func (c *Conn) deliver(l *link, b []byte) error {
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
	l.delivering.Store(true)
	defer l.delivering.Store(false)
	// Wait on the Conn, not the link: the packet is already counted in
	// recvSeq, so the server will not resend it and it must not be dropped.
	select {
	case c.inbox <- protocol.Packet{Header: h, Payload: plain}:
		return nil
	case <-c.ctx.Done():
		return context.Cause(c.ctx)
	}
}

// writeLoop sends ring entries from flushed onwards, trimming the ring and
// releasing blocked writers as the backlog drains. It frames a batch of
// entries (at least one, then up to writeBufSize bytes) into one reused
// buffer and sends it with a single Write, so the steady state allocates
// nothing per packet. Entries are immutable once sealed, so they are framed
// outside the lock.
func (c *Conn) writeLoop(ctx context.Context, w io.Writer) error {
	var (
		batch [][]byte
		buf   []byte
	)
	for {
		c.mu.Lock()
		batch = c.ring.appendRange(batch[:0], c.flushed, min(c.ring.next(), c.flushed+maxBatchEntries))
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
		for _, f := range batch {
			if sent > 0 && len(buf)+4+len(f) > writeBufSize {
				break
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
		// A short count without an error breaks the io.Writer contract;
		// counting the whole batch as sent would drop its tail from replay.
		if m, err := w.Write(buf); err != nil || m != len(buf) {
			if err == nil {
				err = io.ErrShortWrite
			}
			return fmt.Errorf("etcp: write: %w", err)
		}
		c.mu.Lock()
		c.flushed += int64(sent)
		c.unsent -= n
		c.ring.trim(c.flushed, c.unsent)
		room := c.unsent <= c.limit
		c.mu.Unlock()
		if room {
			signal(c.space)
		}
	}
}

// watch declares the link dead after two quiet keepAlive periods, sending the
// probe after the first. Time the reader spends blocked on a slow ReadPacket
// caller does not count as silence, and neither does the time before the
// caller's first packet, when no probe may be sent.
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
				c.enqueue(c.probe)
				probed = true
			}
		}
		t.Reset(c.keepAlive)
	}
}
