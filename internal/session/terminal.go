package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/tphakala/et-go/internal/console"
	"github.com/tphakala/et-go/internal/protocol"
)

const (
	// maxPending caps keyboard input waiting to be sent (spec 5.7).
	maxPending = 1 << 20
	// pendingGrace is how long the reader waits for the sender to make room
	// before dropping input. While connected the sender frees space within
	// microseconds, so only a stalled connection ever reaches the drop.
	pendingGrace = 250 * time.Millisecond
	// readBufSize is the size of one console read.
	readBufSize = 32 << 10
)

// defaultSize replaces a window size with zero rows or columns. A pty whose
// size was never set can report 0x0, and a 0x0 TERMINAL_INFO breaks remote
// full-screen programs.
var defaultSize = console.Size{Rows: 24, Cols: 80}

// terminalService is the interactive terminal: remote output to the local
// terminal, keyboard input (through the escape filter) to the server, and
// window sizes to the server.
type terminalService struct {
	term    Terminal
	log     *slog.Logger
	pending *pendingInput
	size    *latestSize
	// reader tracks the keyboard reader apart from the rest of the group:
	// on Windows a failed Close can leave it blocked in ReadConsoleW, and Run
	// then must not wait for it (see Run).
	reader sync.WaitGroup
}

func newTerminalService(term Terminal, log *slog.Logger) *terminalService {
	return &terminalService{
		term: term,
		log:  log,
		pending: &pendingInput{
			max:   maxPending,
			grace: pendingGrace,
			ready: make(chan struct{}, 1),
			space: make(chan struct{}, 1),
			log:   log,
		},
		size: &latestSize{changed: make(chan struct{}, 1)},
	}
}

func (s *terminalService) headers() []protocol.Header {
	return []protocol.Header{protocol.HeaderTerminalBuffer}
}

func (s *terminalService) start(ctx context.Context, t Transport, in iter.Seq[protocol.Packet], g *group) {
	// Resizes takes its change baseline when it is called (console contract,
	// plan 04), so call it before reading the initial size: a resize between
	// the two is then reported rather than lost.
	resizes := s.term.Resizes(ctx)
	if sz, err := s.term.Size(); err == nil {
		s.size.set(sz) // the sender sends this first TERMINAL_INFO
	} else {
		s.log.Debug("session: read terminal size", "err", err)
	}
	g.Go(func() error { return s.output(ctx, in) })
	g.goIn(&s.reader, func() error { return s.read(ctx) })
	g.Go(func() error { return s.send(ctx, t) })
	g.Go(func() error {
		for sz := range resizes {
			s.size.set(sz)
		}
		return nil
	})
}

// output writes remote output to the terminal.
func (s *terminalService) output(ctx context.Context, in iter.Seq[protocol.Packet]) error {
	for p := range in {
		tb := &protocol.TerminalBuffer{}
		if err := proto.Unmarshal(p.Payload, tb); err != nil {
			return fmt.Errorf("session: decode terminal buffer: %w", err)
		}
		if _, err := s.term.Write(tb.GetBuffer()); err != nil {
			if ctx.Err() != nil {
				return context.Cause(ctx)
			}
			return fmt.Errorf("session: write terminal: %w", err)
		}
	}
	return nil
}

// read moves keyboard input through the escape filter into the pending
// buffer. It never waits on the network, so the escape is seen even while
// the connection is stalled.
func (s *terminalService) read(ctx context.Context) error {
	buf := make([]byte, readBufSize)
	var esc escapeFilter
	var out []byte
	for {
		n, err := s.term.Read(buf)
		if n > 0 {
			var detach bool
			out, detach = esc.filter(out[:0], buf[:n])
			s.pending.push(ctx, out)
			if detach {
				return ErrDetached
			}
		}
		if err != nil {
			if ctx.Err() != nil {
				return context.Cause(ctx) // Run closed the terminal to stop us
			}
			if errors.Is(err, io.EOF) {
				return fmt.Errorf("session: terminal input closed: %w", err)
			}
			return fmt.Errorf("session: read terminal: %w", err)
		}
	}
}

// send is the one goroutine that writes terminal traffic, so it is the one
// that absorbs etcp backpressure. It sends the latest window size when it
// changed, then all pending input in chunks of at most MaxInputPacket.
func (s *terminalService) send(ctx context.Context, t Transport) error {
	for {
		select {
		case <-s.pending.ready:
		case <-s.size.changed:
		case <-ctx.Done():
			return nil
		}
		if sz, ok := s.size.take(); ok {
			if err := write(ctx, t, protocol.HeaderTerminalInfo, terminalInfo(sz)); err != nil {
				return err
			}
		}
		for {
			chunk := s.pending.take(MaxInputPacket)
			if len(chunk) == 0 {
				break
			}
			tb := &protocol.TerminalBuffer{}
			tb.SetBuffer(chunk)
			if err := write(ctx, t, protocol.HeaderTerminalBuffer, tb); err != nil {
				return err
			}
		}
	}
}

// terminalInfo builds TERMINAL_INFO for sz. Zero rows or columns become
// defaultSize; the pixel dimensions are sent as reported.
func terminalInfo(sz console.Size) *protocol.TerminalInfo {
	if sz.Rows == 0 || sz.Cols == 0 {
		sz.Rows, sz.Cols = defaultSize.Rows, defaultSize.Cols
	}
	ti := &protocol.TerminalInfo{}
	ti.SetRow(int32(sz.Rows))
	ti.SetColumn(int32(sz.Cols))
	ti.SetWidth(int32(sz.Width))
	ti.SetHeight(int32(sz.Height))
	return ti
}

// write encodes m and sends it. A send that fails because ctx ended returns
// the context's cause, which leaves the session's first cause in place.
func write(ctx context.Context, t Transport, h protocol.Header, m proto.Message) error {
	b, err := proto.Marshal(m)
	if err != nil {
		return fmt.Errorf("session: encode %v: %w", h, err)
	}
	if err := t.WritePacket(ctx, protocol.Packet{Header: h, Payload: b}); err != nil {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		return fmt.Errorf("session: send %v: %w", h, err)
	}
	return nil
}

// signal does a non-blocking send on a 1-buffered notification channel.
func signal(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

// pendingInput is keyboard input waiting for the sender. The mutex guards
// short critical sections only; all waiting happens on channels (index
// Global Constraints), so synctest can drive it.
type pendingInput struct {
	mu      sync.Mutex
	buf     []byte
	stalled bool
	max     int
	grace   time.Duration
	ready   chan struct{} // signalled when input is added
	space   chan struct{} // signalled when the sender takes input
	log     *slog.Logger
}

// push appends b. When the buffer is full it waits up to grace for the
// sender to make room, then drops what does not fit and logs it. Once a
// wait has timed out the buffer counts as stalled, and later pushes drop
// at once until the sender takes input again, so the reader keeps up with
// the keyboard and still sees the escape.
func (p *pendingInput) push(ctx context.Context, b []byte) {
	for len(b) > 0 {
		p.mu.Lock()
		n := min(len(b), p.max-len(p.buf))
		p.buf = append(p.buf, b[:n]...)
		stalled := p.stalled
		p.mu.Unlock()
		if n > 0 {
			signal(p.ready)
			b = b[n:]
			continue
		}
		if stalled {
			p.log.Debug("session: input dropped while stalled", "bytes", len(b))
			return
		}
		timer := time.NewTimer(p.grace)
		select {
		case <-p.space:
			timer.Stop()
		case <-timer.C:
			p.mu.Lock()
			p.stalled = true
			p.mu.Unlock()
			p.log.Warn("session: connection stalled and input buffer full; dropping input", "bytes", len(b))
			return
		case <-ctx.Done():
			timer.Stop()
			return
		}
	}
}

// take removes and returns up to limit bytes, or nil when nothing is pending.
func (p *pendingInput) take(limit int) []byte {
	p.mu.Lock()
	n := min(limit, len(p.buf))
	if n == 0 {
		p.mu.Unlock()
		return nil
	}
	out := make([]byte, n)
	copy(out, p.buf)
	p.buf = append(p.buf[:0], p.buf[n:]...)
	p.stalled = false
	p.mu.Unlock()
	signal(p.space)
	return out
}

// latestSize holds the newest window size not yet sent. Sizes that arrive
// while the sender is blocked replace each other, so after an outage only
// the latest one is sent.
type latestSize struct {
	mu      sync.Mutex
	sz      console.Size
	dirty   bool
	changed chan struct{}
}

func (l *latestSize) set(sz console.Size) {
	l.mu.Lock()
	l.sz, l.dirty = sz, true
	l.mu.Unlock()
	signal(l.changed)
}

func (l *latestSize) take() (console.Size, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.dirty {
		return console.Size{}, false
	}
	l.dirty = false
	return l.sz, true
}
