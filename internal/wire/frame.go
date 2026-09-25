package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// WriteFrame writes frame with a 4-byte big-endian length prefix, in a single
// Write call.
func WriteFrame(w io.Writer, frame []byte) error {
	if len(frame) > MaxFrameSize {
		return fmt.Errorf("wire: write frame of %d bytes: %w", len(frame), ErrTooLarge)
	}
	buf := make([]byte, 4, 4+len(frame))
	binary.BigEndian.PutUint32(buf, uint32(len(frame)))
	buf = append(buf, frame...)
	if _, err := w.Write(buf); err != nil {
		return fmt.Errorf("wire: write frame: %w", err)
	}
	return nil
}

// ReadFrame reads one frame and returns its body, reusing buf's capacity when
// it is large enough. The result aliases buf; callers that keep it past the
// next ReadFrame must copy it. A length above MaxFrameSize returns ErrTooLarge
// without reading the body. io.EOF is returned unwrapped only when the stream
// ends cleanly before a frame starts; a stream that ends anywhere after the
// first length byte, including right after a complete length, yields a
// wrapped io.ErrUnexpectedEOF.
func ReadFrame(r io.Reader, buf []byte) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, io.EOF
		}
		return nil, fmt.Errorf("wire: read frame length: %w", err)
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > MaxFrameSize {
		return nil, fmt.Errorf("wire: read frame length %d: %w", n, ErrTooLarge)
	}
	if cap(buf) < int(n) {
		buf = make([]byte, n)
	}
	buf = buf[:n]
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, fmt.Errorf("wire: read frame body: %w", bodyErr(err))
	}
	return buf, nil
}
