package bootstrap

import (
	"encoding/json"
	"fmt"
	"log/slog"
)

// redacted replaces the passkey wherever Credentials are formatted, encoded or
// logged. It matches the spelling used by protocol.TerminalUserInfo and
// seal.Stream.
const redacted = "REDACTED"

// Credentials are the session id and passkey returned by etterminal. The id is
// not secret and stays visible for debugging; the passkey is replaced by
// REDACTED in every rendering the type controls: fmt with any verb (Format),
// encoding/json (MarshalJSON), text encoders such as encoding/xml
// (MarshalText) and slog (LogValue). Those methods also run when Credentials
// sits behind a pointer or inside an exported field, a slice or a map.
//
// Known renderings that bypass every method and print the passkey: fmt formatting a
// Credentials held in an unexported struct field (fmt cannot call methods on
// it), and %p applied to a Credentials value (fmt handles %p before it looks
// for any method, and go vet does not flag it), and encoding/gob, which encodes
// the exported fields directly. Keep Credentials in exported fields, never
// format it with %p, and never gob-encode it.
type Credentials struct {
	ID      string
	Passkey string
}

// String returns the redacted form, for callers that want it as a string.
func (c Credentials) String() string {
	return "{ID:" + c.ID + " Passkey:" + redacted + "}"
}

// Format implements fmt.Formatter so every verb prints the redacted form; %#v
// prints it as a Go literal.
func (c Credentials) Format(f fmt.State, verb rune) {
	if verb == 'v' && f.Flag('#') {
		_, _ = fmt.Fprint(f, `bootstrap.Credentials{ID:"`+c.ID+`", Passkey:"`+redacted+`"}`)
		return
	}
	_, _ = fmt.Fprint(f, c.String())
}

// MarshalJSON implements json.Marshaler with the passkey redacted.
func (c Credentials) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		ID      string `json:"id"`
		Passkey string `json:"passkey"`
	}{c.ID, redacted})
}

// MarshalText implements encoding.TextMarshaler with the passkey redacted.
func (c Credentials) MarshalText() ([]byte, error) {
	return []byte(c.String()), nil
}

// LogValue implements slog.LogValuer.
func (c Credentials) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("id", c.ID),
		slog.String("passkey", redacted),
	)
}
