package etcp

import (
	"testing"
	"time"
)

func TestBackoffSchedule(t *testing.T) {
	b := backoff{jitter: func() float64 { return 0.5 }} // factor 1.0: no jitter
	want := []time.Duration{
		0,
		250 * time.Millisecond,
		500 * time.Millisecond,
		time.Second,
		2 * time.Second,
		4 * time.Second,
		5 * time.Second,
		5 * time.Second,
	}
	for i, w := range want {
		if got := b.next(); got != w {
			t.Fatalf("attempt %d: next() = %v, want %v", i, got, w)
		}
	}
	b.reset()
	if got := b.next(); got != 0 {
		t.Fatalf("after reset: next() = %v, want 0", got)
	}
}

func TestBackoffJitterBounds(t *testing.T) {
	tests := []struct {
		name   string
		jitter float64
		second time.Duration // delay before attempt 2 (base 250 ms)
	}{
		{name: "low", jitter: 0, second: 200 * time.Millisecond},
		{name: "high", jitter: 0.999, second: 299_900_000 * time.Nanosecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := backoff{jitter: func() float64 { return tt.jitter }}
			b.next()
			if got := b.next(); got != tt.second {
				t.Fatalf("second delay = %v, want %v", got, tt.second)
			}
			for range 100 { // far past the attempt where an unclamped shift overflows
				if got := b.next(); got <= 0 || got > backoffMax {
					t.Fatalf("delay %v outside (0, %v]", got, backoffMax)
				}
			}
		})
	}
}
