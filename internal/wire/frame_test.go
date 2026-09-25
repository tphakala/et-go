package wire

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	frames := [][]byte{{}, []byte("a"), bytes.Repeat([]byte("x"), 70000)}

	var buf bytes.Buffer
	for _, f := range frames {
		if err := WriteFrame(&buf, f); err != nil {
			t.Fatalf("WriteFrame(%d bytes): %v", len(f), err)
		}
	}
	for i, want := range frames {
		got, err := ReadFrame(&buf, nil)
		if err != nil {
			t.Fatalf("ReadFrame %d: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("frame %d: got %d bytes, want %d", i, len(got), len(want))
		}
	}
	if _, err := ReadFrame(&buf, nil); !errors.Is(err, io.EOF) {
		t.Fatalf("ReadFrame at end = %v, want io.EOF", err)
	}
}

// TestFrameLengthIsBigEndian pins upstream's htonl framing.
func TestFrameLengthIsBigEndian(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, []byte{0xaa, 0xbb, 0xcc}); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	want := []byte{0, 0, 0, 3, 0xaa, 0xbb, 0xcc}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("encoded = % x, want % x", buf.Bytes(), want)
	}
}

func TestReadFrameReusesBuffer(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, []byte("hello")); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	scratch := make([]byte, 0, 64)
	got, err := ReadFrame(&buf, scratch)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if &got[:1][0] != &scratch[:1][0] {
		t.Error("ReadFrame allocated although buf had enough capacity")
	}
	if string(got) != "hello" {
		t.Errorf("ReadFrame = %q, want %q", got, "hello")
	}
}

func TestReadFrameLimit(t *testing.T) {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], MaxFrameSize+1)
	if _, err := ReadFrame(bytes.NewReader(hdr[:]), nil); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("ReadFrame = %v, want ErrTooLarge", err)
	}
}

// TestFrameSizeBoundary checks that a frame of exactly MaxFrameSize is
// accepted and round-trips, pinning the limit checks at "greater than," not
// "greater than or equal": sabotaging either WriteFrame's or ReadFrame's
// check to >= rejects this frame as too large.
func TestFrameSizeBoundary(t *testing.T) {
	frame := bytes.Repeat([]byte{0xab}, MaxFrameSize)

	var buf bytes.Buffer
	if err := WriteFrame(&buf, frame); err != nil {
		t.Fatalf("WriteFrame(MaxFrameSize bytes): %v", err)
	}
	got, err := ReadFrame(&buf, nil)
	if err != nil {
		t.Fatalf("ReadFrame(MaxFrameSize bytes): %v", err)
	}
	if !bytes.Equal(got, frame) {
		t.Fatalf("ReadFrame returned %d bytes, want %d", len(got), len(frame))
	}
}

// TestWriteFrameWriterError checks that a failing io.Writer's error reaches
// the caller through errors.Is, and that WriteFrame issues exactly one
// Write call: it builds the length prefix and body in one buffer first,
// matching the contract etcp relies on.
func TestWriteFrameWriterError(t *testing.T) {
	w := &errWriter{err: errWriterSentinel}
	err := WriteFrame(w, []byte("hello"))
	if !errors.Is(err, errWriterSentinel) {
		t.Fatalf("WriteFrame = %v, want wrapping %v", err, errWriterSentinel)
	}
	if w.calls != 1 {
		t.Fatalf("Write called %d times, want 1", w.calls)
	}
}

// TestWriteFrameSingleWrite pins the single-Write contract on the success
// path: WriteFrame must build the length prefix and body in one buffer
// before writing, not write the header and body separately. etcp relies on
// this to keep a frame from interleaving with another goroutine's write on
// the same connection.
func TestWriteFrameSingleWrite(t *testing.T) {
	w := &countingWriter{}
	if err := WriteFrame(w, []byte("hello")); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	if w.calls != 1 {
		t.Fatalf("Write called %d times, want 1", w.calls)
	}
}

func TestWriteFrameLimit(t *testing.T) {
	var buf bytes.Buffer
	err := WriteFrame(&buf, make([]byte, MaxFrameSize+1))
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("WriteFrame = %v, want ErrTooLarge", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("WriteFrame wrote %d bytes of an oversized frame", buf.Len())
	}
}

// TestReadFrameTruncated pins which cut yields which error: io.EOF means the
// link closed cleanly between frames, and io.ErrUnexpectedEOF means it was
// cut mid-frame. The distinction is for diagnostics and logging only; both
// are link failures to etcp, which reconnects on any read error. A session
// end is signalled only by the server, as INVALID_KEY on redial (upstream
// turns a 0-byte header read into EPIPE, BackedReader.cpp:48-53 at
// et-v7.0.0: "the server needs to explicitly tell the client that the
// session is over").
func TestReadFrameTruncated(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
		want error
	}{
		{"empty", nil, io.EOF},
		{"short length", []byte{0, 0}, io.ErrUnexpectedEOF},
		{"zero body", []byte{0, 0, 0, 5}, io.ErrUnexpectedEOF},
		{"short body", []byte{0, 0, 0, 5, 'a'}, io.ErrUnexpectedEOF},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ReadFrame(bytes.NewReader(tt.in), nil)
			if !errors.Is(err, tt.want) {
				t.Fatalf("ReadFrame = %v, want %v", err, tt.want)
			}
		})
	}
}

func FuzzReadFrame(f *testing.F) {
	f.Add([]byte{0, 0, 0, 3, 1, 2, 3})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, in []byte) {
		got, err := ReadFrame(bytes.NewReader(in), nil)
		if err == nil && len(got) > MaxFrameSize {
			t.Fatalf("ReadFrame returned %d bytes, above MaxFrameSize", len(got))
		}
	})
}

func BenchmarkFrameRoundTrip(b *testing.B) {
	payload := make([]byte, 1024)
	var buf bytes.Buffer
	scratch := make([]byte, 0, 2048)
	b.SetBytes(int64(len(payload)))
	for b.Loop() {
		buf.Reset()
		if err := WriteFrame(&buf, payload); err != nil {
			b.Fatal(err)
		}
		var err error
		if scratch, err = ReadFrame(&buf, scratch); err != nil {
			b.Fatal(err)
		}
	}
}
