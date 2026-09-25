package protocol

import (
	"fmt"
	"log/slog"
)

// TerminalUserInfo is the one generated message that carries the session
// passkey. Its generated String method prints every field, so fmt and slog
// would write the passkey to any log that formats the message, and the
// passkey must never reach logs or fmt output. Format and LogValue print it
// as REDACTED instead. The generated String, prototext and protojson still
// print it and cannot be changed here, so the PasskeyRendering ruleguard rule
// (rules/secrets.go) rejects those calls at lint time.
// A TerminalUserInfo value never reaches Format, but fmt then prints only
// field pointers, and go vet rejects the copy (the message holds a mutex).

// Format implements fmt.Formatter for every verb.
func (x *TerminalUserInfo) Format(f fmt.State, _ rune) {
	if x == nil {
		_, _ = fmt.Fprint(f, "<nil>")
		return
	}
	_, _ = fmt.Fprintf(f, "TerminalUserInfo{id:%q passkey:REDACTED uid:%d gid:%d fd:%d}",
		x.GetId(), x.GetUid(), x.GetGid(), x.GetFd())
}

// LogValue implements slog.LogValuer so handlers that do not go through fmt
// (the JSON handler, for one) never see the passkey either. The generated
// getters are nil-safe, so a nil message logs empty fields.
func (x *TerminalUserInfo) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("id", x.GetId()),
		slog.String("passkey", "REDACTED"),
		slog.Int64("uid", x.GetUid()),
		slog.Int64("gid", x.GetGid()),
		slog.Int64("fd", x.GetFd()),
	)
}
