package seal

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
)

var testKey = [32]byte([]byte("0123456789ABCDEFGHIJKLMNOPQRSTUV"))

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex %q: %v", s, err)
	}
	return b
}

// TestNaClVector checks Seal against the NaCl crypto_secretbox test vector,
// which pins the MAC-then-ciphertext layout libsodium uses on the wire.
func TestNaClVector(t *testing.T) {
	key := [32]byte(mustHex(t, "1b27556473e985d462cd51197a9a46c76009549eac6474f206c4ee0844f68389"))
	nonce := mustHex(t, "69696ee955b62b73cd62bda875fc73d68219e0036b7a0b37")
	plaintext := mustHex(t, "be075fc53c81f2d5cf141316ebeb0c7b5228c52a4c62cbd44b66849b64244ffce5ecbaaf33bd751a1ac728d45e6c61296cdc3c01233561f41db66cce314adb310e3be8250c46f06dceea3a7fa1348057e2f6556ad6b1318a024a838f21af1fde048977eb48f59ffd4924ca1c60902e52f0a089bc76897040e082f937763848645e0705")
	want := mustHex(t, "f3ffc7703f9400e52a7dfb4b3d3305d98e993b9f48681273c29650ba32fc76ce48332ea7164d96a4476fb8c531a1186ac0dfc17c98dce87b4da7f011ec48c97271d2c20f9b928fe2270d6fb863d51738b48eeee314a7cc8ab932164548e526ae90224368517acfeabd6bb3732bc0e9da99832b61ca01b6de56244a9e88d5f9b37973f622a43d14a6599b1f654cb45a74e355a5")

	s := New(&key, ClientToServer)
	// Seal increments before use, so start one below the vector's nonce.
	copy(s.nonce[:], nonce)
	s.nonce[0]--

	if got := s.Seal(nil, plaintext); !bytes.Equal(got, want) {
		t.Fatalf("Seal = %x\nwant    %x", got, want)
	}
}

type goldenRow struct {
	dir   Direction
	index int
	nonce []byte
	box   []byte
}

func readGolden(t *testing.T) []goldenRow {
	t.Helper()
	data, err := os.ReadFile("testdata/golden.txt")
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}

	var rows []goldenRow
	for line := range strings.Lines(string(data)) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 4 {
			t.Fatalf("golden line %q: want 4 fields", line)
		}
		dir, err := strconv.Atoi(fields[0])
		if err != nil {
			t.Fatalf("golden line %q: %v", line, err)
		}
		index, err := strconv.Atoi(fields[1])
		if err != nil {
			t.Fatalf("golden line %q: %v", line, err)
		}
		rows = append(rows, goldenRow{
			dir:   Direction(dir),
			index: index,
			nonce: mustHex(t, fields[2]),
			box:   mustHex(t, fields[3]),
		})
	}
	if len(rows) == 0 {
		t.Fatal("golden file has no rows")
	}
	return rows
}

// TestGolden seals the same sequence as testdata/gen/golden.c and compares
// nonce and box at the recorded indices, including the carry from byte 0 to
// byte 1 at operation 256.
func TestGolden(t *testing.T) {
	rows := readGolden(t)
	for _, dir := range []Direction{ClientToServer, ServerToClient} {
		t.Run(fmt.Sprintf("dir%d", dir), func(t *testing.T) {
			want := map[int]goldenRow{}
			for _, r := range rows {
				if r.dir == dir {
					want[r.index] = r
				}
			}
			s := New(&testKey, dir)
			checked := 0
			for i := 1; i <= 257; i++ {
				box := s.Seal(nil, fmt.Appendf(nil, "packet %d", i))
				r, ok := want[i]
				if !ok {
					continue
				}
				checked++
				if !bytes.Equal(s.nonce[:], r.nonce) {
					t.Errorf("op %d: nonce %x, want %x", i, s.nonce, r.nonce)
				}
				if !bytes.Equal(box, r.box) {
					t.Errorf("op %d: box %x, want %x", i, box, r.box)
				}
			}
			if checked != len(want) {
				t.Errorf("checked %d rows, golden has %d", checked, len(want))
			}
		})
	}
}

func TestRoundTrip(t *testing.T) {
	sealer := New(&testKey, ServerToClient)
	opener := New(&testKey, ServerToClient)
	for i := range 300 {
		msg := fmt.Appendf(nil, "message %d", i)
		box := sealer.Seal(nil, msg)
		got, err := opener.Open(nil, box)
		if err != nil {
			t.Fatalf("Open %d: %v", i, err)
		}
		if !bytes.Equal(got, msg) {
			t.Fatalf("Open %d = %q, want %q", i, got, msg)
		}
	}
}

func TestEmptyPlaintext(t *testing.T) {
	box := New(&testKey, ClientToServer).Seal(nil, nil)
	if len(box) != 16 {
		t.Fatalf("empty box length = %d, want 16 (MAC only)", len(box))
	}
	got, err := New(&testKey, ClientToServer).Open(nil, box)
	if err != nil || len(got) != 0 {
		t.Fatalf("Open(empty box) = %q, %v; want empty, nil", got, err)
	}
}

func TestOpenRejects(t *testing.T) {
	box := New(&testKey, ClientToServer).Seal(nil, []byte("hello"))
	tampered := bytes.Clone(box)
	tampered[len(tampered)-1] ^= 1

	tests := []struct {
		name   string
		opener *Stream
		box    []byte
	}{
		{"tampered", New(&testKey, ClientToServer), tampered},
		{"wrong direction", New(&testKey, ServerToClient), box},
		{"too short", New(&testKey, ClientToServer), box[:10]},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := tt.opener.Open(nil, tt.box); !errors.Is(err, ErrOpen) {
				t.Fatalf("Open = %v, want ErrOpen", err)
			}
		})
	}
}

// TestOpenOutOfOrder shows that the stream position matters: opening the
// second box first fails, because each call consumes one nonce.
func TestOpenOutOfOrder(t *testing.T) {
	sealer := New(&testKey, ClientToServer)
	_ = sealer.Seal(nil, []byte("first"))
	second := sealer.Seal(nil, []byte("second"))

	if _, err := New(&testKey, ClientToServer).Open(nil, second); !errors.Is(err, ErrOpen) {
		t.Fatalf("Open(second box at first position) = %v, want ErrOpen", err)
	}
}

func TestSealAppendsToDst(t *testing.T) {
	prefix := []byte("hdr")
	out := New(&testKey, ClientToServer).Seal(prefix, []byte("x"))
	if !bytes.HasPrefix(out, []byte("hdr")) || len(out) != 3+16+1 {
		t.Fatalf("Seal(prefix) = %x, want prefix kept and 17 bytes appended", out)
	}
}

func TestOpenAppendsToDst(t *testing.T) {
	box := New(&testKey, ClientToServer).Seal(nil, []byte("payload"))
	out, err := New(&testKey, ClientToServer).Open([]byte("hdr"), box)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if string(out) != "hdrpayload" {
		t.Fatalf("Open(prefix) = %q, want %q", out, "hdrpayload")
	}
}

func BenchmarkSeal(b *testing.B) {
	s := New(&testKey, ClientToServer)
	msg := make([]byte, 1024)
	buf := make([]byte, 0, len(msg)+16)
	b.SetBytes(int64(len(msg)))
	for b.Loop() {
		buf = s.Seal(buf[:0], msg)
	}
}
