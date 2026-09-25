package etcp

import (
	"bytes"
	"testing"
)

func filledRing(limit, n, size int) *ring {
	r := &ring{limit: limit}
	for i := range n {
		r.push(bytes.Repeat([]byte{byte(i)}, size))
	}
	return r
}

func TestRingNextCountsPushes(t *testing.T) {
	r := filledRing(1<<20, 5, 10)
	if got := r.next(); got != 5 {
		t.Fatalf("next() = %d, want 5", got)
	}
}

func TestRingTrimKeepsUnsent(t *testing.T) {
	r := filledRing(25, 10, 10) // 100 bytes held, limit 25
	r.trim(4, 60)               // entries 0..3 were written; 4..9 (60 bytes) were not
	if r.first != 2 {
		t.Fatalf("first = %d, want 2 (trim written bytes to the limit, ignoring the backlog)", r.first)
	}
	r.trim(4, 60) // the written 20 bytes are already within the limit
	if r.first != 2 {
		t.Fatalf("first = %d after a second trim, want 2", r.first)
	}
	r.trim(10, 0)
	if r.first != 8 || r.bytes != 20 {
		t.Fatalf("first, bytes = %d, %d; want 8, 20", r.first, r.bytes)
	}
}

func TestRingSince(t *testing.T) {
	r := filledRing(25, 10, 10)
	r.trim(10, 0) // retains 8 and 9
	tests := []struct {
		name string
		from int64
		want int
		ok   bool
	}{
		{name: "whole window", from: 8, want: 2, ok: true},
		{name: "peer up to date", from: 10, want: 0, ok: true},
		{name: "peer behind the window", from: 7, ok: false},
		{name: "peer ahead of us", from: 11, ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := r.since(tt.from, r.next())
			if ok != tt.ok || len(got) != tt.want {
				t.Fatalf("since(%d) = %d entries, %v; want %d, %v", tt.from, len(got), ok, tt.want, tt.ok)
			}
			if ok && tt.want > 0 && got[0][0] != byte(tt.from) {
				t.Fatalf("first entry holds %d, want %d", got[0][0], tt.from)
			}
		})
	}
}

func TestRingBytesBetweenAndRange(t *testing.T) {
	r := filledRing(1<<20, 6, 10)
	if got := r.bytesBetween(2, 5); got != 30 {
		t.Fatalf("bytesBetween(2, 5) = %d, want 30", got)
	}
	got := r.appendRange(nil, 1, 3)
	if len(got) != 2 || got[0][0] != 1 || got[1][0] != 2 {
		t.Fatalf("appendRange(1, 3) = %v", got)
	}
}
