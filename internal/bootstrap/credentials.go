package bootstrap

import "log/slog"

// redacted replaces the passkey wherever Credentials are formatted or logged.
const redacted = "[redacted]"

// Credentials are the session id and passkey returned by etterminal. String,
// GoString and LogValue redact the passkey, so it cannot leak through fmt or
// slog by accident. The id is not secret and stays visible for debugging.
type Credentials struct {
	ID      string
	Passkey string
}

// String implements fmt.Stringer, used by %v, %+v and %s.
func (c Credentials) String() string {
	return "{ID:" + c.ID + " Passkey:" + redacted + "}"
}

// GoString implements fmt.GoStringer, used by %#v.
func (c Credentials) GoString() string {
	return `bootstrap.Credentials{ID:"` + c.ID + `", Passkey:"` + redacted + `"}`
}

// LogValue implements slog.LogValuer.
func (c Credentials) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("id", c.ID),
		slog.String("passkey", redacted),
	)
}
