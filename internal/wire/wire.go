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

// growChunk is the first read step for a body that does not fit the
// caller's buffer; the read steps then double as bytes arrive.
const growChunk = 64 << 10

// bodyErr classifies a failed body read as headerErr does after one byte: a
// body read always follows a complete length prefix, so any end of stream
// there is io.ErrUnexpectedEOF.
func bodyErr(err error) error {
	return headerErr(1, err)
}

// headerErr classifies a failed read that got n bytes, of a length prefix
// or (through bodyErr) of a body. Only a read that got no byte at all is a
// clean end, reported as a plain io.EOF.
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
// reusing buf's capacity when it is large enough. Otherwise it reads in
// steps, the first of min(n, growChunk) bytes and each later one doubling
// what has arrived, and grows the buffer only when a step does not fit: to
// twice its old capacity or the step, whichever is larger, but never past
// max(n, growChunk). A peer that declares a large length and sends little
// therefore costs memory in proportion to what it sent, not to what it
// declared.
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
		if cap(buf) < next {
			// A reused buffer at least doubles, so a reader that passes
			// its previous frame back grows it a few times rather than
			// once per larger frame. The capacity is capped at n above
			// growChunk, where the body ends exactly at n rather than at
			// append's rounded size; a reused buffer growing past
			// growChunk still reallocates for each larger body, which
			// frames carrying one pty read (16 KiB, see MaxFrameSize, plus
			// packet overhead) never reach.
			nb := make([]byte, len(buf), min(max(next, 2*cap(buf)), max(n, growChunk)))
			copy(nb, buf)
			buf = nb
		}
		m, err := io.ReadFull(r, buf[len(buf):next])
		buf = buf[:len(buf)+m]
		if err != nil {
			return nil, bodyErr(err)
		}
	}
	return buf, nil
}
