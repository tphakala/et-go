package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"sync"

	"google.golang.org/protobuf/proto"

	"github.com/tphakala/et-go/internal/protocol"
)

// service is one feature multiplexed over the session's packet stream. It is
// unexported while the terminal is its only implementation (spec 4.2 rule 1).
type service interface {
	// headers lists the packet types routed to this service.
	headers() []protocol.Header
	// start launches the service's goroutines through g. Its packets arrive
	// through in, in order; it writes with t. The service must range over in
	// exactly once: on a normal end the router waits until in is drained, so
	// output the server sent just before ending still reaches the service.
	// Every goroutine must return once ctx is done.
	start(ctx context.Context, t Transport, in iter.Seq[protocol.Packet], g *group)
}

// group runs goroutines for Run. The first goroutine to return a non-nil
// error cancels the session with that error as the cause; Run joins them all.
type group struct {
	wg     sync.WaitGroup
	cancel context.CancelCauseFunc
}

// Go runs f in a new goroutine owned by the group.
func (g *group) Go(f func() error) {
	g.wg.Go(func() {
		if err := f(); err != nil {
			g.cancel(err)
		}
	})
}

// queueLen is the per-service packet queue. A service that falls this far
// behind stalls the router, and with it every other service.
const queueLen = 64

// queue carries one service's packets. The router closes ch on a normal end;
// the service's iterator closes drained when it stops ranging.
type queue struct {
	ch      chan protocol.Packet
	drained chan struct{}
	once    sync.Once
}

func (q *queue) markDrained() { q.once.Do(func() { close(q.drained) }) }

// router owns the single ReadPacket loop. Session-level packets are handled
// here; everything else goes to the service that registered its header.
type router struct {
	routes map[protocol.Header]*queue
	queues map[service]*queue
	log    *slog.Logger
}

func newRouter(services []service, log *slog.Logger) (*router, error) {
	r := &router{
		routes: make(map[protocol.Header]*queue),
		queues: make(map[service]*queue),
		log:    log,
	}
	for _, s := range services {
		q := &queue{ch: make(chan protocol.Packet, queueLen), drained: make(chan struct{})}
		r.queues[s] = q
		for _, h := range s.headers() {
			if sessionHeader(h) {
				return nil, fmt.Errorf("session: header %v is handled by the router", h)
			}
			if _, dup := r.routes[h]; dup {
				return nil, fmt.Errorf("session: header %v registered twice", h)
			}
			r.routes[h] = q
		}
	}
	return r, nil
}

// sessionHeader reports whether h concerns the whole session, so the router
// handles it and no service may register it.
func sessionHeader(h protocol.Header) bool {
	return h == protocol.HeaderKeepAlive || h == protocol.HeaderTerminalExitStatus || h == protocol.HeaderTerminalClose
}

// packets returns s's queue as an iterator. It ends when the router closes
// the queue (after yielding everything queued) or when ctx is done.
func (r *router) packets(ctx context.Context, s service) iter.Seq[protocol.Packet] {
	q := r.queues[s]
	return func(yield func(protocol.Packet) bool) {
		defer q.markDrained()
		for {
			select {
			case p, ok := <-q.ch:
				if !ok || !yield(p) {
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}
}

// run reads packets until the session ends. On a normal end it closes every
// service queue, waits until the services have drained them, and returns
// errEnded or *ExitError. On a failure it returns the error at once. When ctx
// ends first it returns the context's cause, which leaves that cause in place
// (a cancel func keeps only the first cause).
func (r *router) run(ctx context.Context, t Transport) error {
	var exit *ExitError
	ended := func() error {
		for _, q := range r.queues {
			close(q.ch)
		}
		for _, q := range r.queues {
			select {
			case <-q.drained:
			case <-ctx.Done():
				return context.Cause(ctx)
			}
		}
		if exit != nil {
			return exit
		}
		return errEnded
	}
	for {
		p, err := t.ReadPacket(ctx)
		switch {
		case err == nil:
		case ctx.Err() != nil:
			return context.Cause(ctx)
		case errors.Is(err, io.EOF):
			return ended()
		default:
			return fmt.Errorf("session: read: %w", err)
		}
		switch p.Header {
		case protocol.HeaderKeepAlive:
			// Liveness is etcp's job.
		case protocol.HeaderTerminalExitStatus:
			st := &protocol.TerminalExitStatus{}
			if err := proto.Unmarshal(p.Payload, st); err != nil {
				return fmt.Errorf("session: decode exit status: %w", err)
			}
			if st.HasExitcode() {
				exit = &ExitError{Code: int(st.GetExitcode())}
			}
		case protocol.HeaderTerminalClose:
			return ended()
		default:
			q, ok := r.routes[p.Header]
			if !ok {
				r.log.Debug("session: skipping packet with unknown header", "header", p.Header)
				continue
			}
			select {
			case q.ch <- p:
			case <-ctx.Done():
				return context.Cause(ctx)
			}
		}
	}
}
