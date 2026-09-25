// Package protocol holds the Eternal Terminal wire messages, generated from
// upstream's .proto files, and the packet types the client exchanges with
// etserver.
package protocol

// Header identifies a packet's type. Values come from the EtPacketType and
// TerminalPacketType enums, which share one byte on the wire.
type Header uint8

// Packet types used by the client. The values are fixed by upstream; a test
// checks each one against the generated enums.
const (
	HeaderKeepAlive          Header = 0
	HeaderTerminalBuffer     Header = 1
	HeaderTerminalInfo       Header = 2
	HeaderTerminalClose      Header = 11
	HeaderTerminalExitStatus Header = 12
	HeaderInitialResponse    Header = 252
	HeaderInitialPayload     Header = 253
)

// Version is the protocol version the client speaks (upstream
// PROTOCOL_VERSION, src/base/Headers.hpp:169 at et-v7.0.0).
const Version = 6

// Packet is one plaintext application packet.
type Packet struct {
	Header  Header
	Payload []byte
}
