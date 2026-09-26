//go:build !race

package wire

import (
	"errors"
	"testing"

	"github.com/tphakala/et-go/internal/protocol"
	"google.golang.org/protobuf/proto"
)

// TestWriteMessageLimit pins WriteMessage's bound from both sides: a body of
// exactly MaxMessageSize is written, one byte more is refused with
// ErrTooLarge and nothing reaches the writer. The at-limit case holds about
// 256 MiB (the fixture and the framed buffer) and the one-over case the
// fixture alone, so it is skipped in -short mode and excluded from -race
// builds, where the race detector's shadow memory pushes it past 1 GiB. CI
// runs it in the Windows leg and in a dedicated non-race step on Ubuntu.
func TestWriteMessageLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates up to about 256 MiB per case")
	}
	// A CatchupBuffer with one entry encodes as tag 0x0a, a 4-byte varint
	// length, then the entry: 5 bytes of overhead at this size.
	const overhead = 5
	tests := []struct {
		name    string
		size    int
		wantErr error
	}{
		{"at limit", MaxMessageSize, nil},
		{"one over", MaxMessageSize + 1, ErrTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cb := &protocol.CatchupBuffer{}
			cb.SetBuffer([][]byte{make([]byte, tt.size-overhead)})
			if got := proto.Size(cb); got != tt.size {
				t.Fatalf("fixture encodes to %d bytes, want %d", got, tt.size)
			}
			w := &lenWriter{}
			err := WriteMessage(w, cb)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("WriteMessage(%d-byte body) = %v, want %v", tt.size, err, tt.wantErr)
			}
			wantWritten := 0
			if tt.wantErr == nil {
				wantWritten = 8 + tt.size
			}
			if w.n != wantWritten {
				t.Errorf("wrote %d bytes, want %d", w.n, wantWritten)
			}
		})
	}
}
