//go:build ruleguard

package gorules

import "github.com/quasilyte/go-ruleguard/dsl"

// PasskeyRendering rejects the text renderings of a protocol.TerminalUserInfo
// that bypass its redacting Format and LogValue methods: the generated String
// method, and the prototext and protojson encoders. Each prints the session
// passkey, which must never reach logs, errors or fmt output. The generated
// code cannot be changed to redact it (protoc 3.21.12 has no debug_redact),
// so the calls are forbidden here instead. Binary encoding (proto.Marshal),
// which the wire needs, is not affected. A lint rule sees call sites only: a
// method value (f := u.String) or a call through a fmt.Stringer interface is
// not caught.
//
// Rejected:
//
//	log.Print(u.String())
//	b, _ := prototext.Marshal(u)
//	b, _ := protojson.MarshalOptions{}.Marshal(u)
//
// Use fmt or slog on the message itself, which go through the redacting
// methods:
//
//	slog.Info("user", "u", u)
func PasskeyRendering(m dsl.Matcher) {
	m.Import("github.com/tphakala/et-go/internal/protocol")
	m.Import("google.golang.org/protobuf/encoding/prototext")
	m.Import("google.golang.org/protobuf/encoding/protojson")

	m.Match(`$x.String()`).
		Where(m["x"].Type.Is(`*protocol.TerminalUserInfo`) || m["x"].Type.Is(`protocol.TerminalUserInfo`)).
		Report("TerminalUserInfo.String prints the session passkey; format the message with fmt or slog, which redact it")

	m.Match(
		`prototext.Format($x)`,
		`prototext.Marshal($x)`,
		`protojson.Format($x)`,
		`protojson.Marshal($x)`,
	).
		Where(m["x"].Type.Is(`*protocol.TerminalUserInfo`) || m["x"].Type.Is(`protocol.TerminalUserInfo`)).
		Report("prototext and protojson print the TerminalUserInfo session passkey; format the message with fmt or slog, which redact it")

	m.Match(
		`$opts.Format($x)`,
		`$opts.Marshal($x)`,
		`$opts.MarshalAppend($_, $x)`,
	).
		Where((m["x"].Type.Is(`*protocol.TerminalUserInfo`) || m["x"].Type.Is(`protocol.TerminalUserInfo`)) &&
			(m["opts"].Type.Is(`prototext.MarshalOptions`) || m["opts"].Type.Is(`*prototext.MarshalOptions`) ||
				m["opts"].Type.Is(`protojson.MarshalOptions`) || m["opts"].Type.Is(`*protojson.MarshalOptions`))).
		Report("prototext and protojson print the TerminalUserInfo session passkey; format the message with fmt or slog, which redact it")
}
