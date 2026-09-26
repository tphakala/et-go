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
// the caller through errors.Is. The single-Write contract is pinned by
// TestWriteFrameSingleWrite: a writer that fails on its first call cannot
// tell one Write from two.
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
// before writing, not write the header and body separately, so a frame is
// never split by another goroutine's write on a connection whose Write is
// safe for concurrent use. etcp's link writer frames with AppendFrame and
// issues its own single Write per batch.
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
	frame := make([]byte, MaxFrameSize+1)
	var err error
	// The size is checked before the output buffer is allocated, so an
	// oversized frame costs no 16 MiB allocation.
	if got := allocatedBytes(func() { err = WriteFrame(&buf, frame) }); got > 1<<20 {
		t.Fatalf("WriteFrame allocated %d bytes to reject an oversized frame", got)
	}
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

// FuzzReadFrame checks ReadFrame against the framing itself: on success the
// body is exactly the bytes after the 4-byte length, and reading the same
// input into a reused buffer, a small one or one that already holds bytes,
// gives the same body or the same error.
func FuzzReadFrame(f *testing.F) {
	f.Add([]byte{0, 0, 0, 3, 1, 2, 3}, 0)
	f.Add([]byte{0xff, 0xff, 0xff, 0xff}, 2)
	f.Add([]byte{}, 64)
	// 96 KiB declared: past the first 64 KiB grow chunk, with a body pattern
	// that shows any bytes written at the wrong offset.
	f.Add(append([]byte{0, 1, 0x80, 0}, bytes.Repeat([]byte{1, 2, 3, 4, 5, 6, 7}, 14100)...), 100)
	f.Fuzz(func(t *testing.T, in []byte, capacity int) {
		got, err := ReadFrame(bytes.NewReader(in), nil)
		if err == nil {
			if len(in) < 4 {
				t.Fatalf("ReadFrame succeeded on %d input bytes", len(in))
			}
			n := int(binary.BigEndian.Uint32(in))
			if n > MaxFrameSize || !bytes.Equal(got, in[4:4+n]) {
				t.Fatalf("ReadFrame body of %d bytes does not match the %d declared", len(got), n)
			}
		}
		scratch := bytes.Repeat([]byte{0xee}, max(capacity, 0)%(1<<17))
		again, err2 := ReadFrame(bytes.NewReader(in), scratch)
		if (err == nil) != (err2 == nil) || !bytes.Equal(got, again) {
			t.Fatalf("reused buffer (cap %d): got %d bytes, %v; fresh: %d bytes, %v", cap(scratch), len(again), err2, len(got), err)
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

func TestAppendFrame(t *testing.T) {
	dst := []byte{0xee}
	got, err := AppendFrame(dst, []byte{0xaa, 0xbb})
	if err != nil {
		t.Fatalf("AppendFrame: %v", err)
	}
	want := []byte{0xee, 0, 0, 0, 2, 0xaa, 0xbb}
	if !bytes.Equal(got, want) {
		t.Fatalf("AppendFrame = % x, want % x", got, want)
	}

	got, err = AppendFrame(dst, make([]byte, MaxFrameSize+1))
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("AppendFrame(oversized) error = %v, want ErrTooLarge", err)
	}
	if !bytes.Equal(got, dst) {
		t.Fatalf("AppendFrame(oversized) = % x, want dst unchanged", got)
	}
}

// A writer that reports a short count without an error must not silently
// truncate the frame.
func TestWriteFrameShortWrite(t *testing.T) {
	if err := WriteFrame(shortWriter{}, []byte("hello")); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("WriteFrame = %v, want io.ErrShortWrite", err)
	}
}

// A reader that returns the first bytes of a length together with a wrapped
// io.EOF has cut the stream mid-header: that is not a clean end. With no
// byte read at all, the same wrapped io.EOF is a clean end, reported as a
// plain io.EOF.
func TestReadFrameWrappedEOF(t *testing.T) {
	tests := []struct {
		name  string
		in    []byte
		clean bool
	}{
		{name: "nothing read", in: nil, clean: true},
		{name: "partial length", in: []byte{0, 0}},
		{name: "partial body", in: []byte{0, 0, 0, 5, 'a'}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ReadFrame(&wrappedEOFReader{data: tt.in}, nil)
			if tt.clean {
				if err != io.EOF { //nolint:errorlint // a clean end must be the plain sentinel
					t.Fatalf("ReadFrame = %v, want plain io.EOF", err)
				}
				return
			}
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("ReadFrame = %v, want io.ErrUnexpectedEOF", err)
			}
		})
	}
}

// A peer that declares a large frame and sends only a few bytes must not
// make ReadFrame allocate the declared length up front.
func TestReadFrameGrowsWithData(t *testing.T) {
	in := []byte("\x01\x00\x00\x00only ten b") // 16 MiB declared, 10 bytes sent
	var err error
	got := allocatedBytes(func() {
		_, err = ReadFrame(bytes.NewReader(in), nil)
	})
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("ReadFrame = %v, want io.ErrUnexpectedEOF", err)
	}
	if got > 1<<20 {
		t.Fatalf("ReadFrame allocated %d bytes for a 10-byte body, want under 1 MiB", got)
	}
}

// A body that outgrows the caller's buffer ends in a buffer of exactly the
// declared length: the last growth step is capped at the length, not rounded
// up by append's growth policy.
func TestReadFrameGrowsExactly(t *testing.T) {
	const n = 300_000
	var enc bytes.Buffer
	if err := WriteFrame(&enc, bytes.Repeat([]byte{'x'}, n)); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	got, err := ReadFrame(&enc, nil)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if len(got) != n || cap(got) != n {
		t.Fatalf("ReadFrame body len %d cap %d, want both %d", len(got), cap(got), n)
	}
	if !bytes.Equal(got, bytes.Repeat([]byte{'x'}, n)) {
		t.Fatal("ReadFrame body differs from the frame written")
	}
}

// ReadFrame into a buffer that is large enough, and AppendFrame into one,
// allocate nothing: etcp's read and write loops run them for every packet.
func TestFrameSteadyStateAllocs(t *testing.T) {
	var enc bytes.Buffer
	if err := WriteFrame(&enc, make([]byte, 1024)); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	data := enc.Bytes()
	r := bytes.NewReader(data)
	buf := make([]byte, 0, 2048)
	if n := testing.AllocsPerRun(100, func() {
		r.Reset(data)
		var err error
		if buf, err = ReadFrame(r, buf); err != nil {
			t.Fatalf("ReadFrame: %v", err)
		}
	}); n != 0 {
		t.Errorf("ReadFrame with a reused buffer: %v allocs, want 0", n)
	}

	frame := make([]byte, 1024)
	out := make([]byte, 0, 2048)
	if n := testing.AllocsPerRun(100, func() {
		var err error
		if out, err = AppendFrame(out[:0], frame); err != nil {
			t.Fatalf("AppendFrame: %v", err)
		}
	}); n != 0 {
		t.Errorf("AppendFrame into a reused buffer: %v allocs, want 0", n)
	}
}
