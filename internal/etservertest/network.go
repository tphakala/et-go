package etservertest

import (
	"context"
	"errors"
	"io"
	"maps"
	"net"
	"slices"
	"sync"
)

var errRefused = errors.New("connection refused")

// Network is an in-memory network that connects etcp to a Server over
// net.Pipe, with fault injection. It implements etcp.Dialer.NetDialer.
// net.Pipe has no buffering, so every write blocks until the peer reads it.
type Network struct {
	srv    *Server
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu       sync.Mutex
	pairs    map[*pair]struct{}
	refuse   bool
	dials    int
	cutAfter int64 // byte budget for the next connection; negative means none
}

// NewNetwork returns a network whose connections are served by s.
func NewNetwork(s *Server) *Network {
	ctx, cancel := context.WithCancel(context.Background())
	return &Network{
		srv:      s,
		ctx:      ctx,
		cancel:   cancel,
		pairs:    make(map[*pair]struct{}),
		cutAfter: -1,
	}
}

// DialContext connects to the server. It fails with a *net.OpError while
// SetRefuse(true) is in effect and after Close, and with a *net.OpError
// wrapping ctx.Err() when ctx has already ended, as net.Dialer does.
func (n *Network) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.dials++
	if err := ctx.Err(); err != nil {
		return nil, &net.OpError{Op: "dial", Net: network, Err: err}
	}
	if n.refuse || n.ctx.Err() != nil {
		return nil, &net.OpError{Op: "dial", Net: network, Err: errRefused}
	}
	budget := n.cutAfter
	n.cutAfter = -1
	client, server := net.Pipe()
	p := &pair{remaining: budget}
	p.client = &pipeEnd{Conn: client, p: p}
	p.server = &pipeEnd{Conn: server, p: p}
	n.pairs[p] = struct{}{}
	// Started under n.mu, so Close, whose CutAll takes n.mu before its
	// wg.Wait, cannot wait on the group before this goroutine is added.
	n.wg.Go(func() {
		_ = n.srv.Serve(n.ctx, p.server)
		p.close()
		n.mu.Lock()
		delete(n.pairs, p)
		n.mu.Unlock()
	})
	return p.client, nil
}

// CutAll closes every live connection, both ends.
func (n *Network) CutAll() {
	n.mu.Lock()
	pairs := slices.Collect(maps.Keys(n.pairs))
	n.mu.Unlock()
	for _, p := range pairs {
		p.close()
	}
}

// CutAfter makes the next connection die after bytes have crossed it, counted
// over both directions. A cut can land in the middle of a frame or message.
func (n *Network) CutAfter(bytes int64) {
	n.mu.Lock()
	n.cutAfter = bytes
	n.mu.Unlock()
}

// SetRefuse makes dials fail while on is true.
func (n *Network) SetRefuse(on bool) {
	n.mu.Lock()
	n.refuse = on
	n.mu.Unlock()
}

// Dials returns the number of dial attempts so far, refused ones included.
func (n *Network) Dials() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.dials
}

// Close cuts everything, refuses further dials and waits for the server
// goroutines it started.
func (n *Network) Close() {
	n.cancel()
	n.CutAll()
	n.wg.Wait()
}

// pair is the two ends of one connection and its shared byte budget.
type pair struct {
	client, server *pipeEnd
	once           sync.Once
	mu             sync.Mutex
	remaining      int64 // negative means unlimited
}

func (p *pair) close() {
	p.once.Do(func() {
		_ = p.client.Conn.Close()
		_ = p.server.Conn.Close()
	})
}

// take reserves up to n bytes of the budget and reports whether the
// connection must be cut after writing them.
func (p *pair) take(n int) (allowed int, cut bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.remaining < 0 {
		return n, false
	}
	allowed = int(min(int64(n), p.remaining))
	p.remaining -= int64(allowed)
	return allowed, p.remaining == 0
}

type pipeEnd struct {
	net.Conn
	p *pair
}

func (e *pipeEnd) Write(b []byte) (int, error) {
	allowed, cut := e.p.take(len(b))
	n, err := e.Conn.Write(b[:allowed])
	if cut {
		e.p.close()
		if err == nil && n < len(b) {
			err = io.ErrClosedPipe
		}
	}
	return n, err
}

func (e *pipeEnd) Close() error {
	e.p.close()
	return nil
}
