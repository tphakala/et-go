package wire

import (
	"encoding/binary"
	"fmt"
	"io"

	"google.golang.org/protobuf/proto"
)

// WriteMessage writes m as an 8-byte little-endian length followed by its
// protobuf encoding, in a single Write call.
func WriteMessage(w io.Writer, m proto.Message) error {
	body, err := proto.Marshal(m)
	if err != nil {
		return fmt.Errorf("wire: marshal %T: %w", m, err)
	}
	if len(body) > MaxMessageSize {
		return fmt.Errorf("wire: write %T of %d bytes: %w", m, len(body), ErrTooLarge)
	}
	buf := make([]byte, 8, 8+len(body))
	binary.LittleEndian.PutUint64(buf, uint64(len(body)))
	buf = append(buf, body...)
	if _, err := w.Write(buf); err != nil {
		return fmt.Errorf("wire: write %T: %w", m, err)
	}
	return nil
}

// ReadMessage reads one length-prefixed message into m. A zero length yields
// the message's zero value, as upstream does. A length above MaxMessageSize
// (or negative as a signed int64) returns ErrTooLarge without reading the body.
// A stream that ends before the first length byte yields a wrapped io.EOF;
// one that ends anywhere later, including right after a complete length,
// yields a wrapped io.ErrUnexpectedEOF.
func ReadMessage(r io.Reader, m proto.Message) error {
	var hdr [8]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return fmt.Errorf("wire: read %T length: %w", m, err)
	}
	n := int64(binary.LittleEndian.Uint64(hdr[:]))
	if n < 0 || n > MaxMessageSize {
		return fmt.Errorf("wire: read %T length %d: %w", m, n, ErrTooLarge)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return fmt.Errorf("wire: read %T body: %w", m, bodyErr(err))
	}
	if err := proto.Unmarshal(body, m); err != nil {
		return fmt.Errorf("wire: unmarshal %T: %w", m, err)
	}
	return nil
}
