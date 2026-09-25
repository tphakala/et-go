package wire

import "github.com/tphakala/et-go/internal/protocol"

// AppendPacket appends the serialized packet (encrypted flag byte, header
// byte, payload) to b. The payload is copied as is: when encrypted is true it
// must already be the sealed box.
func AppendPacket(b []byte, encrypted bool, h protocol.Header, payload []byte) []byte {
	flag := byte(0)
	if encrypted {
		flag = 1
	}
	b = append(b, flag, byte(h))
	return append(b, payload...)
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
