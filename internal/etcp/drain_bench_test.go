package etcp

import (
	"context"
	"strings"
	"testing"
)

// countingWriter cancels once it has taken want bytes.
type countingWriter struct {
	n, want int
	done    context.CancelFunc
}

func (w *countingWriter) Write(p []byte) (int, error) {
	w.n += len(p)
	if w.n >= w.want {
		w.done()
	}
	return len(p), nil
}

// BenchmarkDrainBacklog drains a backlog of minimum-size entries, the shape a
// long outage with small interactive packets leaves, and fails only if the
// writer stops before the backlog is drained. It measures one backlog size;
// compare runs before and after a writer change to see its cost (taking the
// whole backlog on every writer pass made the drain quadratic).
func BenchmarkDrainBacklog(b *testing.B) {
	const entries = 500_000
	for b.Loop() {
		var d Dialer
		c := d.newConn("et.example:2022", "XXXtestclient001", strings.Repeat("k", 32))
		for range entries {
			c.ring.push(make([]byte, 18))
			c.unsent += 18
		}
		ctx, cancel := context.WithCancel(b.Context())
		w := &countingWriter{want: entries * (4 + 18), done: cancel}
		if err := c.writeLoop(ctx, newLink(), w); err == nil || ctx.Err() == nil {
			b.Fatalf("writeLoop = %v before draining %d bytes (took %d)", err, w.want, w.n)
		}
		c.cancel(nil)
	}
}
