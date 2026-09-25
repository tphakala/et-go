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
// not secret and stays visible for debugging. The passkey sits behind an
// unexported pointer, read with Passkey: renderings that reach it by reflection
// (fmt of a Credentials held in an unexported field, %p, encoding/gob,
// encoding/json of the raw fields) see only an address or nothing. Format,
// MarshalJSON, MarshalText and LogValue add a readable form with the passkey
// replaced by REDACTED.
//
// Hold Credentials in a named field, not embedded: embedding promotes Format
// and the marshalers, so the outer struct would print and encode as the
// credentials alone. Credentials hold a pointer, so == compares identity;
// compare ID and Passkey() instead.
type Credentials struct {
	ID      string
	passkey *string
}

// NewCredentials returns Credentials holding id and passkey.
func NewCredentials(id, passkey string) Credentials {
	return Credentials{ID: id, passkey: &passkey}
}

// Passkey returns the session passkey, or "" for the zero value.
func (c Credentials) Passkey() string {
	if c.passkey == nil {
		return ""
	}
	return *c.passkey
}

// String returns the redacted form, for callers that want it as a string.
func (c Credentials) String() string {
	return "{ID:" + c.ID + " Passkey:" + redacted + "}"
}

// Format implements fmt.Formatter so every verb prints the redacted form; %#v
// prints it as a Go literal.
func (c Credentials) Format(f fmt.State, verb rune) {
	if verb == 'v' && f.Flag('#') {
		_, _ = fmt.Fprintf(f, "bootstrap.Credentials{ID:%q, Passkey:%q}", c.ID, redacted)
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
