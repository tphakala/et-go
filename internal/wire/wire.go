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

// ErrShortPacket reports a serialized packet without its 2-byte header.
var ErrShortPacket = errors.New("wire: packet shorter than its 2-byte header")

// bodyErr maps io.EOF from a body read to io.ErrUnexpectedEOF. io.ReadFull
// returns io.EOF when no byte at all was read, which for a body means the
// stream was cut after a complete length prefix: a broken link, not a clean
// end of stream.
func bodyErr(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}
