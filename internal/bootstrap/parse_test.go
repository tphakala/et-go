package bootstrap

import (
	"errors"
	"strings"
	"testing"
)

func TestParseCredentials(t *testing.T) {
	tests := []struct {
		name    string
		out     string
		wantErr string // "" means success with testID and testPasskey; otherwise a substring the error must contain
	}{
		{"plain", "IDPASSKEY:" + testID + "/" + testPasskey + "\n", ""},
		{"crlf", "IDPASSKEY:" + testID + "/" + testPasskey + "\r\n", ""},
		{"no trailing newline", "IDPASSKEY:" + testID + "/" + testPasskey, ""},
		{"noise before", "Last login: Thu\nWelcome!\nIDPASSKEY:" + testID + "/" + testPasskey + "\n", ""},
		{"first marker wins", "IDPASSKEY:" + testID + "/" + testPasskey + "\nIDPASSKEY:zzzzzzzzzzzzzzzz/" + testPasskey, ""},
		{"no marker", "Welcome!\n", "no IDPASSKEY"},
		{"empty", "", "no IDPASSKEY"},
		{"truncated id", "IDPASSKEY:abcd", "malformed id"},
		{"id at end of output", "IDPASSKEY:" + testID, "malformed id"},
		{"colon instead of slash", "IDPASSKEY:" + testID + ":" + testPasskey, "malformed id"},
		{"truncated passkey", "IDPASSKEY:" + testID + "/0123", "malformed passkey"},
		{"id too long", "IDPASSKEY:" + testID + "X/" + testPasskey, "malformed id"},
		{"id too short", "IDPASSKEY:" + testID[1:] + "/" + testPasskey, "malformed id"},
		{"passkey too long", "IDPASSKEY:" + testID + "/" + testPasskey + "X\n", "malformed passkey"},
		{"missing slash", "IDPASSKEY:" + testID + testPasskey, "malformed id"},
		{"non alphanumeric in id", "IDPASSKEY:abcdEFGH1234567-/" + testPasskey, "malformed id"},
		{"non alphanumeric in passkey", "IDPASSKEY:" + testID + "/0123456789abcdef_BCDEF0123456789", "malformed passkey"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseCredentials([]byte(tt.out))
			if tt.wantErr == "" {
				// Compared with the constants, so a broken constructor or
				// accessor cannot make both sides agree.
				if err != nil || got.ID != testID || got.Passkey() != testPasskey {
					t.Fatalf("parseCredentials() = %v with passkey match %v, %v; want id %s, the test passkey, nil",
						got, got.Passkey() == testPasskey, err, testID)
				}
				return
			}
			if !errors.Is(err, ErrNoCredentials) {
				t.Fatalf("parseCredentials() error = %v, want ErrNoCredentials", err)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("parseCredentials() error = %q, want it to mention %q", err, tt.wantErr)
			}
			// Every row's passkey material starts with "0123"; no error may quote it.
			if strings.Contains(err.Error(), testPasskey[:4]) {
				t.Fatalf("error %q quotes passkey material", err)
			}
		})
	}
}
