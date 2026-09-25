package session

import (
	"context"
	"iter"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/tphakala/et-go/internal/protocol"
)

// recordingService records the packets routed to it. When slow is set it
// takes the first packet and then stops reading until ctx ends.
type recordingService struct {
	hdrs []protocol.Header
	slow bool

	mu  sync.Mutex
	got []protocol.Packet
}

func (s *recordingService) headers() []protocol.Header { return s.hdrs }

func (s *recordingService) start(ctx context.Context, t Transport, in iter.Seq[protocol.Packet], g *group) {
	g.Go(func() error {
		for p := range in {
			s.mu.Lock()
			s.got = append(s.got, p)
			s.mu.Unlock()
			if s.slow {
				<-ctx.Done()
				return nil
			}
		}
		return nil
	})
}

func (s *recordingService) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.got)
}

func TestNewRouterRejectsDuplicateHeader(t *testing.T) {
	a := &recordingService{hdrs: []protocol.Header{protocol.HeaderTerminalBuffer}}
	b := &recordingService{hdrs: []protocol.Header{protocol.HeaderTerminalBuffer}}
	_, err := newRouter([]service{a, b}, slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "registered twice") {
		t.Fatalf("newRouter = %v, want duplicate header error", err)
	}
}

func TestNewRouterRejectsSessionHeaders(t *testing.T) {
	for _, h := range []protocol.Header{protocol.HeaderKeepAlive, protocol.HeaderTerminalExitStatus, protocol.HeaderTerminalClose} {
		s := &recordingService{hdrs: []protocol.Header{h}}
		if _, err := newRouter([]service{s}, slog.New(slog.DiscardHandler)); err == nil {
			t.Errorf("newRouter accepted session-level header %v", h)
		}
	}
}

// A service that stops reading must not delay packets for another service,
// as long as its queue has room (queueLen).
func TestRouterSlowServiceDoesNotDelayOther(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const other protocol.Header = 40
		slow := &recordingService{hdrs: []protocol.Header{other}, slow: true}
		fast := &recordingService{hdrs: []protocol.Header{protocol.HeaderTerminalBuffer}}
		r, err := newRouter([]service{slow, fast}, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatal(err)
		}
		tr := newFakeTransport()
		ctx, cancel := context.WithCancelCause(t.Context())
		g := &group{cancel: cancel}
		g.Go(func() error { return r.run(ctx, tr) })
		slow.start(ctx, tr, r.packets(ctx, slow), g)
		fast.start(ctx, tr, r.packets(ctx, fast), g)

		for range 10 {
			tr.in <- protocol.Packet{Header: other}
		}
		for range 5 {
			tr.in <- serverOutput(t, "x")
		}
		synctest.Wait()
		if got := fast.count(); got != 5 {
			t.Fatalf("fast service got %d packets, want 5", got)
		}
		cancel(nil)
		g.wg.Wait()
	})
}
