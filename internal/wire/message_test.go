package wire

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"github.com/tphakala/et-go/internal/protocol"
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
	f.Fuzz(func(t *testing.T, in []byte) {
		var cb protocol.CatchupBuffer
		_ = ReadMessage(bytes.NewReader(in), &cb)
	})
}
