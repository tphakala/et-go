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
		want    Credentials
		wantErr string // "" means success; otherwise a substring the error must contain
	}{
		{"plain", "IDPASSKEY:" + testID + "/" + testPasskey + "\n", Credentials{testID, testPasskey}, ""},
		{"crlf", "IDPASSKEY:" + testID + "/" + testPasskey + "\r\n", Credentials{testID, testPasskey}, ""},
		{"no trailing newline", "IDPASSKEY:" + testID + "/" + testPasskey, Credentials{testID, testPasskey}, ""},
		{"noise before", "Last login: Thu\nWelcome!\nIDPASSKEY:" + testID + "/" + testPasskey + "\n", Credentials{testID, testPasskey}, ""},
		{"first marker wins", "IDPASSKEY:" + testID + "/" + testPasskey + "\nIDPASSKEY:zzzzzzzzzzzzzzzz/" + testPasskey, Credentials{testID, testPasskey}, ""},
		{"no marker", "Welcome!\n", Credentials{}, "no IDPASSKEY"},
		{"empty", "", Credentials{}, "no IDPASSKEY"},
		{"truncated id", "IDPASSKEY:abcd", Credentials{}, "malformed id"},
		{"id at end of output", "IDPASSKEY:" + testID, Credentials{}, "malformed id"},
		{"colon instead of slash", "IDPASSKEY:" + testID + ":" + testPasskey, Credentials{}, "malformed id"},
		{"truncated passkey", "IDPASSKEY:" + testID + "/0123", Credentials{}, "malformed passkey"},
		{"id too long", "IDPASSKEY:" + testID + "X/" + testPasskey, Credentials{}, "malformed id"},
		{"id too short", "IDPASSKEY:" + testID[1:] + "/" + testPasskey, Credentials{}, "malformed id"},
		{"passkey too long", "IDPASSKEY:" + testID + "/" + testPasskey + "X\n", Credentials{}, "malformed passkey"},
		{"missing slash", "IDPASSKEY:" + testID + testPasskey, Credentials{}, "malformed id"},
		{"non alphanumeric in id", "IDPASSKEY:abcdEFGH1234567-/" + testPasskey, Credentials{}, "malformed id"},
		{"non alphanumeric in passkey", "IDPASSKEY:" + testID + "/0123456789abcdef_BCDEF0123456789", Credentials{}, "malformed passkey"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseCredentials([]byte(tt.out))
			if tt.wantErr == "" {
				if err != nil || got != tt.want {
					t.Fatalf("parseCredentials() = %v, %v; want %v, nil", got, err, tt.want)
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
