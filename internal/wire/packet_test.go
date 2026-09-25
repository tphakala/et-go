package wire

import (
	"bytes"
	"errors"
	"testing"

	"github.com/tphakala/et-go/internal/protocol"
)

func TestAppendPacket(t *testing.T) {
	tests := []struct {
		name      string
		encrypted bool
		h         protocol.Header
		payload   []byte
		want      []byte
	}{
		{"plain keepalive", false, protocol.HeaderKeepAlive, nil, []byte{0, 0}},
		{"encrypted buffer", true, protocol.HeaderTerminalBuffer, []byte("box"), []byte{1, 1, 'b', 'o', 'x'}},
		{"initial payload", true, protocol.HeaderInitialPayload, []byte{9}, []byte{1, 253, 9}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AppendPacket([]byte("pre"), tt.encrypted, tt.h, tt.payload)
			if !bytes.Equal(got, append([]byte("pre"), tt.want...)) {
				t.Fatalf("AppendPacket = % x, want pre + % x", got, tt.want)
			}
		})
	}
}

// TestAppendPacketOverlap pins that a payload sharing memory with b comes
// out intact wherever it sits, including the two bytes the prefix is written
// to, and that the in-place layout (payload already at b[len(b)+2:]) reuses
// b's backing array.
func TestAppendPacketOverlap(t *testing.T) {
	const payload = "sealedbox"
	tests := []struct {
		name    string
		offset  int // where the payload starts, relative to len(b) == 0
		inPlace bool
	}{
		{"at b[len(b)]", 0, false},
		{"at b[len(b)+1]", 1, false},
		{"at b[len(b)+2], in place", 2, true},
		{"disjoint, further in", 20, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf := make([]byte, 64)
			p := buf[tt.offset : tt.offset+len(payload)]
			copy(p, payload)

			got := AppendPacket(buf[:0], true, protocol.HeaderTerminalBuffer, p)

			want := append([]byte{1, byte(protocol.HeaderTerminalBuffer)}, payload...)
			if !bytes.Equal(got, want) {
				t.Fatalf("AppendPacket = %q, want %q", got, want)
			}
			if tt.inPlace && &got[0] != &buf[0] {
				t.Error("in-place layout allocated a new array; want buf reused")
			}
		})
	}
}

func TestParsePacket(t *testing.T) {
	tests := []struct {
		name      string
		in        []byte
		encrypted bool
		h         protocol.Header
		payload   []byte
	}{
		{"header only", []byte{0, 0}, false, protocol.HeaderKeepAlive, []byte{}},
		{"encrypted", []byte{1, 1, 'x'}, true, protocol.HeaderTerminalBuffer, []byte("x")},
		{"non-zero flag is encrypted", []byte{7, 2, 'y'}, true, protocol.HeaderTerminalInfo, []byte("y")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			enc, h, payload, err := ParsePacket(tt.in)
			if err != nil {
				t.Fatalf("ParsePacket: %v", err)
			}
			if enc != tt.encrypted || h != tt.h || !bytes.Equal(payload, tt.payload) {
				t.Fatalf("ParsePacket = (%v, %v, %q), want (%v, %v, %q)", enc, h, payload, tt.encrypted, tt.h, tt.payload)
			}
		})
	}
}

func TestParsePacketShort(t *testing.T) {
	for _, in := range [][]byte{nil, {1}} {
		if _, _, _, err := ParsePacket(in); !errors.Is(err, ErrShortPacket) {
			t.Errorf("ParsePacket(% x) = %v, want ErrShortPacket", in, err)
		}
	}
}

func TestParsePacketAliases(t *testing.T) {
	in := []byte{1, 1, 'a', 'b'}
	_, _, payload, err := ParsePacket(in)
	if err != nil {
		t.Fatalf("ParsePacket: %v", err)
	}
	in[2] = 'z'
	if payload[0] != 'z' {
		t.Error("ParsePacket copied the payload; it is documented to alias the input")
	}
}

func FuzzParsePacket(f *testing.F) {
	f.Add([]byte{1, 1, 'x'})
	f.Add([]byte{0})
	f.Fuzz(func(t *testing.T, in []byte) {
		enc, h, payload, err := ParsePacket(in)
		if err != nil {
			return
		}
		if got := AppendPacket(nil, enc, h, payload); len(got) != len(in) || !bytes.Equal(got[1:], in[1:]) {
			t.Fatalf("re-encoding % x gave % x", in, got)
		}
	})
}
