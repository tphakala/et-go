package bootstrap

import (
	"errors"
	"strings"
	"testing"
)

func TestParseCredentials(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want Credentials
		ok   bool
	}{
		{"plain", "IDPASSKEY:" + testID + "/" + testPasskey + "\n", Credentials{testID, testPasskey}, true},
		{"crlf", "IDPASSKEY:" + testID + "/" + testPasskey + "\r\n", Credentials{testID, testPasskey}, true},
		{"no trailing newline", "IDPASSKEY:" + testID + "/" + testPasskey, Credentials{testID, testPasskey}, true},
		{"noise before", "Last login: Thu\nWelcome!\nIDPASSKEY:" + testID + "/" + testPasskey + "\n", Credentials{testID, testPasskey}, true},
		{"first marker wins", "IDPASSKEY:" + testID + "/" + testPasskey + "\nIDPASSKEY:zzzzzzzzzzzzzzzz/" + testPasskey, Credentials{testID, testPasskey}, true},
		{"no marker", "Welcome!\n", Credentials{}, false},
		{"empty", "", Credentials{}, false},
		{"truncated id", "IDPASSKEY:abcd", Credentials{}, false},
		{"truncated passkey", "IDPASSKEY:" + testID + "/0123", Credentials{}, false},
		{"id too long", "IDPASSKEY:" + testID + "X/" + testPasskey, Credentials{}, false},
		{"id too short", "IDPASSKEY:" + testID[1:] + "/" + testPasskey, Credentials{}, false},
		{"passkey too long", "IDPASSKEY:" + testID + "/" + testPasskey + "X\n", Credentials{}, false},
		{"missing slash", "IDPASSKEY:" + testID + testPasskey, Credentials{}, false},
		{"non alphanumeric in id", "IDPASSKEY:abcdEFGH1234567-/" + testPasskey, Credentials{}, false},
		{"non alphanumeric in passkey", "IDPASSKEY:" + testID + "/0123456789abcdef_BCDEF0123456789", Credentials{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseCredentials([]byte(tt.out))
			if tt.ok {
				if err != nil || got != tt.want {
					t.Fatalf("parseCredentials() = %v, %v; want %v, nil", got, err, tt.want)
				}
				return
			}
			if !errors.Is(err, ErrNoCredentials) {
				t.Fatalf("parseCredentials() error = %v, want ErrNoCredentials", err)
			}
			if strings.Contains(err.Error(), testPasskey[:8]) {
				t.Fatalf("error %q quotes passkey material", err)
			}
		})
	}
}
