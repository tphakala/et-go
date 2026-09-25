// Package wire implements Eternal Terminal's two framings and its packet
// layout.
//
// Handshake messages (ConnectRequest, SequenceHeader, CatchupBuffer and so on)
// are an 8-byte length followed by the serialized protobuf. Upstream writes the
// length in host byte order (src/base/SocketHandler.hpp at et-v7.0.0). This
// package assumes a little-endian server, which covers x86-64 and arm64; a
// big-endian etserver would not interoperate with any little-endian client
// either, so the assumption is not a compatibility risk in practice.
//
// Stream frames on an established connection are a 4-byte big-endian length
// (upstream htonl in BackedWriter.cpp:49) followed by one serialized packet:
// an encrypted flag byte, a header byte, then the payload.
package wire

import (
	"errors"
	"io"
	"slices"
)

// Size limits for lengths read from the network.
const (
	// MaxMessageSize is upstream's bound for handshake messages.
	MaxMessageSize = 128 << 20
	// MaxFrameSize bounds stream frames. Upstream reads the pty in chunks of
	// at most 16 KiB (src/terminal/TerminalServer.cpp:6, BUF_SIZE), so this
	// bound leaves wide headroom while keeping a broken or hostile peer from
	// forcing a large allocation.
	MaxFrameSize = 16 << 20
)

// ErrTooLarge reports a length prefix above the applicable limit, or a
// negative handshake length.
var ErrTooLarge = errors.New("wire: length exceeds limit")

// ErrMalformed reports a handshake message whose complete body does not
// decode: the peer sent garbage, since a cut stream yields
// io.ErrUnexpectedEOF before decoding is attempted.
var ErrMalformed = errors.New("wire: malformed message")

// ErrShortPacket reports a serialized packet without its 2-byte header.
var ErrShortPacket = errors.New("wire: packet shorter than its 2-byte header")

// growChunk is the first allocation for a body that does not fit the
// caller's buffer; the buffer then doubles as bytes arrive.
const growChunk = 64 << 10

// bodyErr maps an io.EOF (possibly wrapped) from a body read to
// io.ErrUnexpectedEOF. A body read happens only after a complete length
// prefix, so an end of stream there, whether before the first body byte or
// between two growth chunks, is a broken link, not a clean end of stream.
func bodyErr(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// headerErr classifies a failed length-prefix read that got n bytes. Only a
// read that got no byte at all is a clean end, reported as a plain io.EOF.
// io.ReadFull maps only an unwrapped io.EOF after a partial read to
// io.ErrUnexpectedEOF, so a reader that returns bytes together with a
// wrapped io.EOF is mapped here.
func headerErr(n int, err error) error {
	if !errors.Is(err, io.EOF) {
		return err
	}
	if n == 0 {
		return io.EOF
	}
	return io.ErrUnexpectedEOF
}

// readBody reads exactly n bytes into buf's storage and returns buf[:n],
// reusing buf's capacity when it is large enough. Otherwise the buffer grows
// as bytes arrive, starting at growChunk and doubling, so a peer that
// declares a large length and sends little costs memory in proportion to
// what it sent, not to what it declared.
func readBody(r io.Reader, buf []byte, n int) ([]byte, error) {
	if cap(buf) >= n {
		buf = buf[:n]
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, bodyErr(err)
		}
		return buf, nil
	}
	buf = buf[:0]
	for len(buf) < n {
		next := min(n, max(2*len(buf), growChunk))
		buf = slices.Grow(buf, next-len(buf))
		m, err := io.ReadFull(r, buf[len(buf):next])
		buf = buf[:len(buf)+m]
		if err != nil {
			return nil, bodyErr(err)
		}
	}
	return buf, nil
}
