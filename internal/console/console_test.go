package console

import (
	"context"
	"errors"
	"os"
	"testing"
)

// TestResizeCheck pins the shared step of both platforms' Resizes loops for
// the size results it can meet (unchanged, changed, closed, another error),
// a changed or unchanged size after ctx ended, and a loop body that stops,
// without a terminal.
func TestResizeCheck(t *testing.T) {
	base := Size{Rows: 24, Cols: 80}
	changed := Size{Rows: 40, Cols: 120}
	live := t.Context()
	ended, cancel := context.WithCancel(t.Context())
	cancel()

	tests := []struct {
		name      string
		ctx       context.Context
		size      Size
		sizeErr   error
		yieldMore bool
		want      bool
		wantYield bool
		wantLast  Size
	}{
		{name: "unchanged", ctx: live, size: base, yieldMore: true, want: true, wantLast: base},
		{name: "changed", ctx: live, size: changed, yieldMore: true, want: true, wantYield: true, wantLast: changed},
		{name: "changed and the body stops", ctx: live, size: changed, want: false, wantYield: true, wantLast: changed},
		{name: "closed", ctx: live, sizeErr: os.ErrClosed, yieldMore: true, want: false, wantLast: base},
		{name: "other size error", ctx: live, sizeErr: errors.New("transient"), yieldMore: true, want: true, wantLast: base},
		{name: "changed after ctx ended", ctx: ended, size: changed, yieldMore: true, want: false, wantLast: changed},
		{name: "unchanged after ctx ended", ctx: ended, size: base, yieldMore: true, want: true, wantLast: base},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			last := base
			yielded := false
			got := resizeCheck(tt.ctx,
				func() (Size, error) { return tt.size, tt.sizeErr },
				&last,
				func(sz Size) bool {
					if sz != tt.size {
						t.Errorf("yielded %+v, want %+v", sz, tt.size)
					}
					yielded = true
					return tt.yieldMore
				})
			if got != tt.want || yielded != tt.wantYield || last != tt.wantLast {
				t.Fatalf("resizeCheck = %v, yielded %v, last %+v; want %v, %v, %+v",
					got, yielded, last, tt.want, tt.wantYield, tt.wantLast)
			}
		})
	}
}
