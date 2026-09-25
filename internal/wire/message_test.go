package wire

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"github.com/tphakala/et-go/internal/protocol"
	"google.golang.org/protobuf/proto"
)

func TestMessageRoundTrip(t *testing.T) {
	req := &protocol.ConnectRequest{}
	req.SetClientId("XXXabcdefghijklm")
	req.SetVersion(protocol.Version)

	var buf bytes.Buffer
	if err := WriteMessage(&buf, req); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	var got protocol.ConnectRequest
	if err := ReadMessage(&buf, &got); err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if got.GetClientId() != "XXXabcdefghijklm" || got.GetVersion() != protocol.Version {
		t.Errorf("round trip = (%q, %d)", got.GetClientId(), got.GetVersion())
	}
	if buf.Len() != 0 {
		t.Errorf("%d bytes left over after one message", buf.Len())
	}
}

// TestMessageLengthIsLittleEndian pins this package's assumption that the
// server is little-endian (see the wire package doc): the 8-byte length
// comes first, least significant byte first.
func TestMessageLengthIsLittleEndian(t *testing.T) {
	sh := &protocol.SequenceHeader{}
	sh.SetSequenceNumber(300)

	var buf bytes.Buffer
	if err := WriteMessage(&buf, sh); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	b := buf.Bytes()
	// SequenceHeader{sequenceNumber: 300} is tag 0x08 plus varint 0xac 0x02.
	want := []byte{3, 0, 0, 0, 0, 0, 0, 0, 0x08, 0xac, 0x02}
	if !bytes.Equal(b, want) {
		t.Fatalf("encoded = % x, want % x", b, want)
	}
}

// TestReadMessageZeroLength matches upstream: a zero length is a valid,
// empty message (for example an empty CatchupBuffer).
func TestReadMessageZeroLength(t *testing.T) {
	r := bytes.NewReader(make([]byte, 8))
	// Prefill, so the test fails if ReadMessage skips decoding for length 0
	// and leaves a reused message's old contents in place.
	cb := &protocol.CatchupBuffer{}
	cb.SetBuffer([][]byte{[]byte("stale")})
	if err := ReadMessage(r, cb); err != nil {
		t.Fatalf("ReadMessage(zero length) = %v", err)
	}
	if n := len(cb.GetBuffer()); n != 0 {
		t.Fatalf("zero-length message left %d stale buffers, want 0", n)
	}
}

func TestReadMessageLimits(t *testing.T) {
	tests := []struct {
		name   string
		length uint64
	}{
		{"above limit", MaxMessageSize + 1},
		{"negative as int64", 1 << 63},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var hdr [8]byte
			binary.LittleEndian.PutUint64(hdr[:], tt.length)
			var sh protocol.SequenceHeader
			err := ReadMessage(bytes.NewReader(hdr[:]), &sh)
			if !errors.Is(err, ErrTooLarge) {
				t.Fatalf("ReadMessage = %v, want ErrTooLarge", err)
			}
		})
	}
}

// TestReadMessageTruncated pins which cut yields which error: only a stream
// that ends before the first length byte is a clean io.EOF; any later cut,
// including one right after a complete length, is io.ErrUnexpectedEOF.
func TestReadMessageTruncated(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
		want error
	}{
		{"empty", nil, io.EOF},
		{"short length", []byte{1, 0, 0}, io.ErrUnexpectedEOF},
		{"zero body", []byte{5, 0, 0, 0, 0, 0, 0, 0}, io.ErrUnexpectedEOF},
		{"short body", []byte{5, 0, 0, 0, 0, 0, 0, 0, 0x08}, io.ErrUnexpectedEOF},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var sh protocol.SequenceHeader
			err := ReadMessage(bytes.NewReader(tt.in), &sh)
			if !errors.Is(err, tt.want) {
				t.Fatalf("ReadMessage = %v, want %v", err, tt.want)
			}
		})
	}
}

// TestReadMessageSizeBoundary checks that a header declaring exactly
// MaxMessageSize, with no body following, reads as far as it can and
// returns io.ErrUnexpectedEOF, not ErrTooLarge: the length itself is
// within bounds. This pins the limit check at "greater than," not "greater
// than or equal": sabotaging ReadMessage's check to >= would reject this
// length outright and return ErrTooLarge instead. The missing body is
// deliberate: the limit check runs before any body byte is read, so the
// header alone decides between the two errors, and a complete valid message
// at this size would cost hundreds of MiB per run for no extra coverage.
func TestReadMessageSizeBoundary(t *testing.T) {
	var hdr [8]byte
	binary.LittleEndian.PutUint64(hdr[:], uint64(MaxMessageSize))
	var sh protocol.SequenceHeader
	err := ReadMessage(bytes.NewReader(hdr[:]), &sh)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("ReadMessage(length=MaxMessageSize, no body) = %v, want io.ErrUnexpectedEOF", err)
	}
}

// TestWriteMessageWriterError checks that a failing io.Writer's error
// reaches the caller through errors.Is. The single-Write contract is pinned
// by TestWriteMessageSingleWrite: a writer that fails on its first call
// cannot tell one Write from two.
func TestWriteMessageWriterError(t *testing.T) {
	sh := &protocol.SequenceHeader{}
	sh.SetSequenceNumber(1)

	w := &errWriter{err: errWriterSentinel}
	err := WriteMessage(w, sh)
	if !errors.Is(err, errWriterSentinel) {
		t.Fatalf("WriteMessage = %v, want wrapping %v", err, errWriterSentinel)
	}
	if w.calls != 1 {
		t.Fatalf("Write called %d times, want 1", w.calls)
	}
}

// TestWriteMessageSingleWrite pins the single-Write contract on the success
// path: WriteMessage must build the length prefix and marshaled body in one
// buffer before writing, not write the header and body separately. etcp
// relies on this to keep a message from interleaving with another
// goroutine's write on the same connection.
func TestWriteMessageSingleWrite(t *testing.T) {
	sh := &protocol.SequenceHeader{}
	sh.SetSequenceNumber(1)

	w := &countingWriter{}
	if err := WriteMessage(w, sh); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	if w.calls != 1 {
		t.Fatalf("Write called %d times, want 1", w.calls)
	}
}

func TestReadMessageGarbage(t *testing.T) {
	// Length 2, then bytes that are not a valid protobuf (field 0 is illegal).
	in := []byte{2, 0, 0, 0, 0, 0, 0, 0, 0x00, 0x00}
	var sh protocol.SequenceHeader
	if err := ReadMessage(bytes.NewReader(in), &sh); err == nil {
		t.Fatal("ReadMessage(garbage) = nil, want an unmarshal error")
	}
}

func FuzzReadMessage(f *testing.F) {
	f.Add([]byte{3, 0, 0, 0, 0, 0, 0, 0, 0x08, 0xac, 0x02})
	f.Add(make([]byte, 8))
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	// A CatchupBuffer holding one entry "x": field 1, length 1.
	f.Add([]byte{3, 0, 0, 0, 0, 0, 0, 0, 0x0a, 0x01, 'x'})
	f.Fuzz(func(t *testing.T, in []byte) {
		var cb protocol.CatchupBuffer
		if err := ReadMessage(bytes.NewReader(in), &cb); err != nil {
			return
		}
		// Success means in holds an 8-byte length and at least that many
		// body bytes, and the result must equal decoding that body
		// directly.
		n := binary.LittleEndian.Uint64(in[:8])
		var want protocol.CatchupBuffer
		if err := proto.Unmarshal(in[8:8+n], &want); err != nil {
			t.Fatalf("ReadMessage accepted a body proto.Unmarshal rejects: %v", err)
		}
		if !proto.Equal(&cb, &want) {
			t.Fatalf("ReadMessage decoded %v, direct decode of the body gives %v", &cb, &want)
		}
	})
}
