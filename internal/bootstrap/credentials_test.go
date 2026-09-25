package bootstrap

import (
	"bytes"
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

	// Each format carries surrounding text, as real log and error lines do.
	formatted := map[string]string{
		"%v":         fmt.Sprintf("creds %v", c),
		"%+v":        fmt.Sprintf("creds %+v", c),
		"%s":         fmt.Sprintf("creds %s", c),
		"%#v":        fmt.Sprintf("creds %#v", c),
		"pointer %v": fmt.Sprintf("creds %v", &c),
		"nested %+v": fmt.Sprintf("creds %+v", holder{c}),
		"nested %#v": fmt.Sprintf("creds %#v", holder{c}),
		"Sprint":     fmt.Sprint("creds ", c),
		"Errorf %v":  fmt.Errorf("dial with %v", c).Error(),
	}
	for verb, s := range formatted {
		if strings.Contains(s, testPasskey) {
			t.Errorf("%s leaks the passkey: %s", verb, s)
		}
		if !strings.Contains(s, testID) || !strings.Contains(s, redacted) {
			t.Errorf("%s = %s, want the id and %q", verb, s, redacted)
		}
	}

	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("got", "credentials", c)
	if strings.Contains(buf.String(), testPasskey) {
		t.Fatalf("slog JSON leaks the passkey: %s", buf.String())
	}
	if !strings.Contains(buf.String(), `"credentials":{"id":"`+testID+`","passkey":"[redacted]"}`) {
		t.Fatalf("slog JSON = %s, want a redacted credentials group", buf.String())
	}
}
