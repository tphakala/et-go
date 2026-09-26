package console

import (
	"bytes"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"
)

func TestUTF16DecoderWhole(t *testing.T) {
	tests := []struct {
		name  string
		units []uint16
		want  string
	}{
		{"ascii", []uint16{'h', 'i'}, "hi"},
		{"bmp", utf16.Encode([]rune("äö€")), "äö€"},
		{"surrogate pair", utf16.Encode([]rune("😀")), "😀"},
		{"vt sequence", utf16.Encode([]rune("\x1b[A")), "\x1b[A"},
		{"lone low", []uint16{0xDC00, 'a'}, "�a"},
		{"high then ascii", []uint16{0xD83D, 'a'}, "�a"},
		{"high then high", []uint16{0xD83D, 0xD83D, 0xDE00}, "�😀"},
		{"empty", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var d utf16Decoder
			got := string(d.append(nil, tt.units))
			if got != tt.want {
				t.Fatalf("append(%U) = %q, want %q", tt.units, got, tt.want)
			}
		})
	}
}

func TestUTF16DecoderCarriesHighSurrogate(t *testing.T) {
	units := utf16.Encode([]rune("a😀b"))
	var d utf16Decoder
	first := d.append(nil, units[:2]) // 'a' plus the high surrogate
	if string(first) != "a" {
		t.Fatalf("first call = %q, want %q (high surrogate must be held back)", first, "a")
	}
	second := d.append(nil, units[2:])
	if string(second) != "😀b" {
		t.Fatalf("second call = %q, want %q", second, "😀b")
	}
}

func TestUTF8EncoderWhole(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []uint16
	}{
		{"ascii", "hi", []uint16{'h', 'i'}},
		{"multibyte", "äö€", utf16.Encode([]rune("äö€"))},
		{"astral", "😀", []uint16{0xD83D, 0xDE00}},
		{"invalid byte", "a\xffb", []uint16{'a', 0xFFFD, 'b'}},
		{"truncated inside", "\xe2\x82a", []uint16{0xFFFD, 0xFFFD, 'a'}},
		{"empty", "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var e utf8Encoder
			got := e.append(nil, []byte(tt.in))
			if !slices.Equal(got, tt.want) {
				t.Fatalf("append(%q) = %U, want %U", tt.in, got, tt.want)
			}
		})
	}
}

func TestUTF8EncoderCarriesSplitSequence(t *testing.T) {
	in := []byte("x€y") // € is 3 bytes: e2 82 ac
	for split := range len(in) + 1 {
		var e utf8Encoder
		got := e.append(nil, in[:split])
		got = e.append(got, in[split:])
		want := utf16.Encode([]rune("x€y"))
		if !slices.Equal(got, want) {
			t.Fatalf("split at %d: got %U, want %U", split, got, want)
		}
	}
}

func TestUTF8EncoderHoldsIncompleteTail(t *testing.T) {
	var e utf8Encoder
	got := e.append(nil, []byte("a\xf0\x9f")) // first half of 😀
	if !slices.Equal(got, []uint16{'a'}) {
		t.Fatalf("got %U, want only 'a' while the tail is incomplete", got)
	}
	got = e.append(nil, []byte{0x98})
	if len(got) != 0 {
		t.Fatalf("got %U, want nothing: 3 of 4 bytes still incomplete", got)
	}
	got = e.append(nil, []byte{0x80})
	if !slices.Equal(got, []uint16{0xD83D, 0xDE00}) {
		t.Fatalf("got %U, want the surrogate pair for U+1F600", got)
	}
}

func TestChunkLen(t *testing.T) {
	pair := utf16.Encode([]rune("😀")) // high, low
	tests := []struct {
		name  string
		units []uint16
		limit int
		want  int
	}{
		{"fits", []uint16{'a', 'b'}, 4, 2},
		{"exact", []uint16{'a', 'b', 'c', 'd'}, 4, 4},
		{"split plain", []uint16{'a', 'b', 'c', 'd', 'e'}, 4, 4},
		{"pair at boundary", append([]uint16{'a', 'b', 'c'}, pair...), 4, 3},
		{"pair before boundary", append([]uint16{'a', 'b'}, append(pair, 'c')...), 4, 4},
		{"lone high at boundary", []uint16{'a', 'b', 'c', 0xD83D, 'd'}, 4, 4},
		{"empty", nil, 4, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := chunkLen(tt.units, tt.limit); got != tt.want {
				t.Fatalf("chunkLen(%U, %d) = %d, want %d", tt.units, tt.limit, got, tt.want)
			}
		})
	}
}

// TestChunkLenRejectsSmallLimit checks the precondition: with a limit below
// 2 a pair at the boundary would leave a chunk of zero units, so chunkLen
// panics even for input short enough to take the fast path.
func TestChunkLenRejectsSmallLimit(t *testing.T) {
	for _, limit := range []int{1, 0, -1} {
		for _, units := range [][]uint16{nil, {'a'}, {'a', 'b', 'c'}} {
			func() {
				defer func() {
					r := recover()
					msg, ok := r.(string)
					if !ok || !strings.Contains(msg, "limit") {
						t.Errorf("chunkLen(%U, %d) panic = %v, want a string naming the limit", units, limit, r)
					}
				}()
				chunkLen(units, limit)
			}()
		}
	}
}

// FuzzChunkLen checks that chunking any input covers it exactly, never
// exceeds the limit, and never separates a surrogate pair.
func FuzzChunkLen(f *testing.F) {
	f.Add([]byte("h\x00i\x00=\xd8\x00\xde"), uint8(2))
	f.Add([]byte("\x30\x30\x30\x30\x30\xd8\x30\xd8"), uint8(2)) // two lone high surrogates, limit 4
	f.Fuzz(func(t *testing.T, raw []byte, lim uint8) {
		limit := int(lim)%16 + 2
		units := make([]uint16, len(raw)/2)
		for i := range units {
			units[i] = uint16(raw[2*i]) | uint16(raw[2*i+1])<<8
		}
		var joined []uint16
		for rest := units; len(rest) > 0; {
			n := chunkLen(rest, limit)
			if n < 1 || n > limit {
				t.Fatalf("chunkLen = %d with limit %d", n, limit)
			}
			if n < len(rest) && isHighSurrogate(rest[n-1]) && isLowSurrogate(rest[n]) {
				t.Fatalf("chunk separates a surrogate pair: %U | %U", rest[:n], rest[n:n+1])
			}
			joined = append(joined, rest[:n]...)
			rest = rest[n:]
		}
		if !slices.Equal(joined, units) {
			t.Fatalf("chunks %U do not rejoin to %U", joined, units)
		}
	})
}

// FuzzUTF8EncoderSplit checks that splitting the input anywhere never changes
// the output, and that any input, valid or not, matches the standard
// library's encoding of the Go []rune conversion once the carried tail is
// flushed.
func FuzzUTF8EncoderSplit(f *testing.F) {
	f.Add([]byte("hello, 世界 😀"), uint8(3))
	f.Add([]byte("\xe2\x82"), uint8(1))
	f.Add([]byte("a\xffb\xf0\x9f\x98\x80"), uint8(5))
	f.Add([]byte("\xf0\x9f\x98a\xed\xa0\x80"), uint8(2)) // truncated 4-byte, UTF-8 encoded surrogate
	f.Fuzz(func(t *testing.T, in []byte, at uint8) {
		var whole utf8Encoder
		want := whole.append(nil, in)

		split := int(at) % (len(in) + 1)
		var parts utf8Encoder
		got := parts.append(nil, in[:split])
		got = parts.append(got, in[split:])

		if !slices.Equal(got, want) {
			t.Fatalf("split %d of %q: got %U, want %U", split, in, got, want)
		}
		if !slices.Equal(parts.carry[:parts.n], whole.carry[:whole.n]) {
			t.Fatalf("split %d of %q: carry state differs", split, in)
		}
		// The carry only ever holds one incomplete sequence, and the Go
		// []rune conversion turns each byte of that into its own U+FFFD, so
		// flushing it appends one U+FFFD per carried byte.
		flushed := got
		for range parts.n {
			flushed = append(flushed, utf8.RuneError)
		}
		if ref := utf16.Encode([]rune(string(in))); !slices.Equal(flushed, ref) {
			t.Fatalf("split %d of %q, flushed: got %U, want utf16.Encode %U", split, in, flushed, ref)
		}
	})
}

// FuzzUTF16DecoderSplit is the decoder counterpart: any split gives the same
// output, and any input, valid or not, matches the standard library's
// decoding (one U+FFFD per unpaired surrogate) once a carried high
// surrogate is flushed.
func FuzzUTF16DecoderSplit(f *testing.F) {
	f.Add([]byte("h\x00i\x00=\xd8\x00\xde"), uint8(2))
	f.Add([]byte("\x00\xdc"), uint8(0))
	f.Add([]byte("=\xd8a\x00=\xd8=\xd8\x00\xde=\xd8"), uint8(1)) // high then ASCII, high then pair, trailing high
	f.Fuzz(func(t *testing.T, raw []byte, at uint8) {
		units := make([]uint16, len(raw)/2)
		for i := range units {
			units[i] = uint16(raw[2*i]) | uint16(raw[2*i+1])<<8
		}
		var whole utf16Decoder
		want := whole.append(nil, units)

		split := int(at) % (len(units) + 1)
		var parts utf16Decoder
		got := parts.append(nil, units[:split])
		got = parts.append(got, units[split:])

		if !bytes.Equal(got, want) || parts.high != whole.high {
			t.Fatalf("split %d of %U: got %q, want %q", split, units, got, want)
		}
		// A high surrogate still carried at the end is unpaired: flushing
		// it appends one U+FFFD.
		flushed := got
		if parts.high != 0 {
			flushed = utf8.AppendRune(flushed, utf8.RuneError)
		}
		if ref := string(utf16.Decode(units)); string(flushed) != ref {
			t.Fatalf("split %d of %U, flushed: got %q, want utf16.Decode %q", split, units, flushed, ref)
		}
	})
}
