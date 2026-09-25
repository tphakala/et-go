package etcp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/tphakala/et-go/internal/protocol"
	"github.com/tphakala/et-go/internal/wire"
)

const (
	readBufSize  = 64 << 10
	writeBufSize = 64 << 10
)

var errClosed = net.ErrClosed

// runLink runs a reader and a writer on nc until one of them fails, then
// joins both and returns the cause.
func (c *Conn) runLink(nc net.Conn, catchup [][]byte) error {
	ctx, cancel := context.WithCancelCause(c.ctx)
	defer cancel(nil)
	stop := context.AfterFunc(ctx, func() { _ = nc.Close() })
	defer stop()

	var wg sync.WaitGroup
	wg.Go(func() { cancel(c.readLoop(nc, catchup)) })
	wg.Go(func() { cancel(c.writeLoop(ctx, nc)) })
	wg.Wait()
	_ = nc.Close()
	return context.Cause(ctx)
}

// readLoop delivers the peer's catchup first, then frames from the link.
func (c *Conn) readLoop(r io.Reader, catchup [][]byte) error {
	for _, b := range catchup {
		if err := c.deliver(b); err != nil {
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
		if err := c.deliver(frame); err != nil {
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
func (c *Conn) deliver(b []byte) error {
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
		batch = c.ring.appendRange(batch[:0], c.flushed, c.ring.next())
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
		if _, err := w.Write(buf); err != nil {
			return fmt.Errorf("etcp: write: %w", err)
		}
		c.mu.Lock()
		c.flushed += int64(sent)
		c.unsent -= n
		c.ring.trim(c.flushed)
		room := c.unsent <= c.limit
		c.mu.Unlock()
		if room {
			signal(c.space)
		}
	}
}
