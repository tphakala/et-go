package wire

import (
	"slices"

	"github.com/tphakala/et-go/internal/protocol"
)

// AppendPacket appends the serialized packet (encrypted flag byte, header
// byte, payload) to b. The payload is copied as is: when encrypted is true it
// must already be the sealed box.
//
// payload may share memory with b, including b's spare capacity: the payload
// is copied before the two prefix bytes are written, so the returned packet
// always holds the original payload bytes. When payload lies in
// b[len(b):len(b)+2+len(payload)] anywhere other than exactly at
// b[len(b)+2:], the caller's payload slice is overwritten and must not be
// used afterwards. A payload already at b[len(b)+2:], for example one sealed
// into buf[2:2], is copied onto itself, with no allocation when b has
// capacity for the packet.
func AppendPacket(b []byte, encrypted bool, h protocol.Header, payload []byte) []byte {
	flag := byte(0)
	if encrypted {
		flag = 1
	}
	n := len(b)
	b = slices.Grow(b, 2+len(payload))[:n+2+len(payload)]
	copy(b[n+2:], payload)
	b[n] = flag
	b[n+1] = byte(h)
	return b
}

// ParsePacket splits a serialized packet. The returned payload aliases b.
// Any non-zero flag byte means encrypted, matching upstream's bool
// conversion (src/base/Packet.hpp:30 at et-v7.0.0).
func ParsePacket(b []byte) (encrypted bool, h protocol.Header, payload []byte, err error) {
	if len(b) < 2 {
		return false, 0, nil, ErrShortPacket
	}
	return b[0] != 0, protocol.Header(b[1]), b[2:], nil
}
