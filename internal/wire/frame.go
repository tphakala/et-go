package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"
)

// AppendFrame appends frame to dst with its 4-byte big-endian length prefix
// and returns the extended slice. A frame above MaxFrameSize returns dst
// unchanged and an error wrapping ErrTooLarge. Appending into a reused dst
// lets a writer batch several frames into one Write without allocating per
// frame. frame must not lie in dst's spare capacity, which the prefix
// overwrites.
func AppendFrame(dst, frame []byte) ([]byte, error) {
	if len(frame) > MaxFrameSize {
		return dst, fmt.Errorf("wire: frame of %d bytes: %w", len(frame), ErrTooLarge)
	}
	dst = binary.BigEndian.AppendUint32(dst, uint32(len(frame)))
	return append(dst, frame...), nil
}

// WriteFrame writes frame with a 4-byte big-endian length prefix, in a single
// Write call. A writer that reports fewer bytes than it was given yields
// io.ErrShortWrite.
func WriteFrame(w io.Writer, frame []byte) error {
	buf, err := AppendFrame(make([]byte, 0, 4+min(len(frame), MaxFrameSize)), frame)
	if err != nil {
		return err
	}
	if err := writeAll(w, buf); err != nil {
		return fmt.Errorf("wire: write frame: %w", err)
	}
	return nil
}

// ReadFrame reads one frame and returns its body, reusing buf's capacity when
// it is large enough; the 4-byte length is read into buf's storage too, so a
// reused buf makes ReadFrame allocate nothing. When buf is too small the body
// buffer grows as bytes arrive rather than being allocated at the declared
// length up front. The result aliases buf; callers that keep it past the next
// ReadFrame must copy it. A length above MaxFrameSize returns ErrTooLarge
// without reading the body. io.EOF is returned unwrapped only when the stream
// ends cleanly before a frame starts; a stream that ends anywhere after the
// first length byte, including right after a complete length, yields a
// wrapped io.ErrUnexpectedEOF.
func ReadFrame(r io.Reader, buf []byte) ([]byte, error) {
	// A local array for the length would escape to the heap through the
	// io.Reader call, one allocation per frame.
	buf = slices.Grow(buf[:0], 4)[:4]
	if m, err := io.ReadFull(r, buf); err != nil {
		if err = headerErr(m, err); errors.Is(err, io.EOF) {
			return nil, io.EOF
		}
		return nil, fmt.Errorf("wire: read frame length: %w", err)
	}
	n := binary.BigEndian.Uint32(buf)
	if n > MaxFrameSize {
		return nil, fmt.Errorf("wire: read frame length %d: %w", n, ErrTooLarge)
	}
	buf, err := readBody(r, buf[:0], int(n))
	if err != nil {
		return nil, fmt.Errorf("wire: read frame body: %w", err)
	}
	return buf, nil
}

// writeAll issues one Write of buf and turns a short count without an error
// into io.ErrShortWrite, as io.Copy does.
func writeAll(w io.Writer, buf []byte) error {
	n, err := w.Write(buf)
	if err == nil && n != len(buf) {
		err = io.ErrShortWrite
	}
	return err
}
