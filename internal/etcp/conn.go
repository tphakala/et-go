package etcp

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/tphakala/et-go/internal/protocol"
	"github.com/tphakala/et-go/internal/seal"
	"github.com/tphakala/et-go/internal/wire"
	"golang.org/x/crypto/nacl/secretbox"
)

const inboxSize = 64

// maxPayload is the largest packet payload whose sealed, serialized packet
// still fits in one wire frame.
const maxPayload = wire.MaxFrameSize - 2 - secretbox.Overhead

// Conn is a reliable, ordered, encrypted packet connection that survives
// reconnects. Its methods are safe for concurrent use.
//
// Waiting happens only on channels and timers, never on a held mutex, so
// testing/synctest can drive every blocking path.
type Conn struct {
	netDialer netDialer
	addr      string
	id        string
	keepAlive time.Duration
	probe     protocol.Packet
	limit     int
	logger    *slog.Logger

	ctx    context.Context // lifetime of the Conn; its cause is what callers see
	cancel context.CancelCauseFunc
	wg     sync.WaitGroup

	mu      sync.Mutex   // guards the outbound state below
	out     *seal.Stream // client to server
	ring    ring
	flushed int64 // next sequence number the current link writer sends
	unsent  int   // bytes of entries at or after flushed

	// in and recvSeq belong to whichever goroutine reads: a link reader, or
	// the supervisor during recovery, never both at once.
	in      *seal.Stream
	recvSeq int64

	inbox chan protocol.Packet
	wake  chan struct{} // cap 1: new outbound data for the link writer
	space chan struct{} // cap 1: the unsent backlog fell to limit or below
}

// WritePacket seals p and queues it for delivery. It returns once p is queued,
// not when it is sent. It blocks only when the unsent backlog exceeds
// ReplayLimit, until a link drains it, ctx ends or the Conn fails. A payload
// too large for one sealed frame (just under wire.MaxFrameSize) is refused
// with an error wrapping wire.ErrTooLarge. A ctx already done on entry is
// refused; one that ends while the write is blocked returns its cause, unless
// the write was woken for room at the same moment, in which case it may still
// be queued, as a net.Conn write racing its deadline may complete.
func (c *Conn) WritePacket(ctx context.Context, p protocol.Packet) error {
	if len(p.Payload) > maxPayload {
		return fmt.Errorf("etcp: packet payload of %d bytes: %w", len(p.Payload), wire.ErrTooLarge)
	}
	// ctx is checked here and in the select below, never between waking on
	// c.space and the room check: a writer that took the wakeup and then
	// returned would leave the next blocked writer parked with room available.
	if err := ctx.Err(); err != nil {
		return context.Cause(ctx)
	}
	for {
		if c.ctx.Err() != nil {
			return context.Cause(c.ctx)
		}
		c.mu.Lock()
		if c.unsent <= c.limit {
			c.enqueueLocked(p)
			room := c.unsent <= c.limit
			c.mu.Unlock()
			signal(c.wake)
			if room {
				signal(c.space) // pass the turn to another blocked writer
			}
			return nil
		}
		c.mu.Unlock()
		select {
		case <-c.space:
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-c.ctx.Done():
			return context.Cause(c.ctx)
		}
	}
}

// ReadPacket returns the next packet from the server, blocking until one
// arrives, ctx ends or the Conn fails. Packets are delivered exactly once, in
// order. Packets that arrived before the Conn failed are still returned
// first, so a shell's last output is not lost when the session ends.
func (c *Conn) ReadPacket(ctx context.Context) (protocol.Packet, error) {
	select {
	case p := <-c.inbox:
		return p, nil
	case <-ctx.Done():
		return protocol.Packet{}, context.Cause(ctx)
	case <-c.ctx.Done():
		select {
		case p := <-c.inbox:
			return p, nil
		default:
			return protocol.Packet{}, context.Cause(c.ctx)
		}
	}
}

// Close shuts the connection down and waits for its goroutines. The server
// keeps the session; a later client cannot resume it (the replay state is gone).
func (c *Conn) Close() error {
	c.cancel(errClosed)
	c.wg.Wait()
	return nil
}

// sealedNone reports whether no packet has been sealed yet, probes included.
func (c *Conn) sealedNone() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ring.next() == 0
}

// enqueue queues p regardless of the backlog limit; used for liveness probes.
func (c *Conn) enqueue(p protocol.Packet) {
	c.mu.Lock()
	c.enqueueLocked(p)
	c.mu.Unlock()
	signal(c.wake)
}

// enqueueLocked seals p exactly once, so nonce order equals sequence order and
// a replay resends identical bytes.
func (c *Conn) enqueueLocked(p protocol.Packet) {
	buf := make([]byte, 0, 2+secretbox.Overhead+len(p.Payload))
	data := c.out.Seal(wire.AppendPacket(buf, true, p.Header, nil), p.Payload)
	c.ring.push(data)
	c.unsent += len(data)
}

// fail ends the Conn with err and returns it.
func (c *Conn) fail(err error) error {
	c.cancel(err)
	return err
}

func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}
