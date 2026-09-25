package bootstrap

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// testID and testPasskey are well-formed credentials shared by this
// package's tests.
const (
	testID      = "abcdEFGH12345678"                 // 16 alphanumeric
	testPasskey = "0123456789abcdefABCDEF0123456789" // 32 alphanumeric
)

func TestCredentialsRedaction(t *testing.T) {
	c := Credentials{ID: testID, Passkey: testPasskey}
	type holder struct{ Creds Credentials }

	mustJSON := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		return string(b)
	}
	mustXML := func(v any) string {
		b, err := xml.Marshal(v)
		if err != nil {
			t.Fatalf("xml.Marshal: %v", err)
		}
		return string(b)
	}
	slogOut := func(h func(*bytes.Buffer) slog.Handler, args ...any) string {
		var buf bytes.Buffer
		slog.New(h(&buf)).Info("got", args...)
		return buf.String()
	}
	jsonHandler := func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) }
	textHandler := func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) }

	// Each rendering carries surrounding text, as real log and error lines do.
	rendered := map[string]string{
		"%v":                fmt.Sprintf("creds %v", c),
		"%+v":               fmt.Sprintf("creds %+v", c),
		"%s":                fmt.Sprintf("creds %s", c),
		"%q":                fmt.Sprintf("creds %q", c),
		"%x":                fmt.Sprintf("creds %x", c),
		"%X":                fmt.Sprintf("creds %X", c),
		"%d":                fmt.Sprintf("creds %d", c),
		"%t":                fmt.Sprintf("creds %t", c),
		"%o":                fmt.Sprintf("creds %o", c),
		"%#v":               fmt.Sprintf("creds %#v", c),
		"pointer %v":        fmt.Sprintf("creds %v", &c),
		"pointer %d":        fmt.Sprintf("creds %d", &c),
		"nested %+v":        fmt.Sprintf("creds %+v", holder{c}),
		"nested %#v":        fmt.Sprintf("creds %#v", holder{c}),
		"slice %v":          fmt.Sprintf("creds %v", []Credentials{c}),
		"map %v":            fmt.Sprintf("creds %v", map[string]Credentials{"k": c}),
		"Sprint":            fmt.Sprint("creds ", c),
		"Errorf %v":         fmt.Errorf("dial with %v", c).Error(),
		"json":              mustJSON(c),
		"json nested":       mustJSON(holder{c}),
		"json slice":        mustJSON([]Credentials{c}),
		"xml nested":        mustXML(holder{c}),
		"slog json":         slogOut(jsonHandler, "credentials", c),
		"slog json nested":  slogOut(jsonHandler, "h", holder{c}),
		"slog json slice":   slogOut(jsonHandler, "s", []Credentials{c}),
		"slog text":         slogOut(textHandler, "credentials", c),
		"slog text nested":  slogOut(textHandler, "h", holder{c}),
		"slog json pointer": slogOut(jsonHandler, "credentials", &c),
	}
	for name, s := range rendered {
		if strings.Contains(s, testPasskey) {
			t.Errorf("%s leaks the passkey: %s", name, s)
		}
		if !strings.Contains(s, testID) || !strings.Contains(s, redacted) {
			t.Errorf("%s = %s, want the id and %q", name, s, redacted)
		}
	}

	if got := rendered["slog json"]; !strings.Contains(got, `"credentials":{"id":"`+testID+`","passkey":"REDACTED"}`) {
		t.Fatalf("slog JSON = %s, want a redacted credentials group", got)
	}
	if got, want := rendered["%#v"], `creds bootstrap.Credentials{ID:"`+testID+`", Passkey:"REDACTED"}`; got != want {
		t.Fatalf("%%#v = %s, want %s", got, want)
	}
	if got, want := rendered["json"], `{"id":"`+testID+`","passkey":"REDACTED"}`; got != want {
		t.Fatalf("json = %s, want %s", got, want)
	}
}
