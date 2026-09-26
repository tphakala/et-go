package bootstrap

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestOnlyAlnumOr(t *testing.T) {
	tests := []struct {
		s, extra string
		want     bool
	}{
		{"abcXYZ019", "", true},
		{"a.b-c", ".-", true},
		{"", ".-", false},     // empty is not a valid value
		{"a_b", ".-", false},  // a byte outside extra
		{"café", ".-", false}, // non-ASCII letters are not allowed
		{"a b", ".-", false},
	}
	for _, tt := range tests {
		if got := onlyAlnumOr(tt.s, tt.extra); got != tt.want {
			t.Errorf("onlyAlnumOr(%q, %q) = %v, want %v", tt.s, tt.extra, got, tt.want)
		}
	}
}

// TestMarkerIsUpstream pins the marker to what etterminal prints (upstream
// src/terminal/TerminalMain.cpp:185 at et-v7.0.0); the other tests build
// their input from the constant.
func TestMarkerIsUpstream(t *testing.T) {
	if marker != "IDPASSKEY:" {
		t.Fatalf("marker = %q, want %q", marker, "IDPASSKEY:")
	}
}

// FuzzParseCredentials: parseCredentials never panics, and whatever it
// accepts is a well-formed id and passkey that directly follow the first
// marker in the input.
func FuzzParseCredentials(f *testing.F) {
	for _, s := range []string{
		marker + testID + "/" + testPasskey + "\n",
		"banner\n" + marker + testID + "/" + testPasskey,
		marker + testID + "/" + testPasskey + "X",
		marker + "abcd",
		marker,
		"",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, out []byte) {
		c, err := parseCredentials(out)
		if err != nil {
			return
		}
		id, passkey := c.ID, c.Passkey()
		if len(id) != idLen || !onlyAlnumOr(id, "") {
			t.Fatalf("parseCredentials(%q) id = %q, want %d letters and digits", out, id, idLen)
		}
		if len(passkey) != passkeyLen || !onlyAlnumOr(passkey, "") {
			t.Fatalf("parseCredentials(%q) passkey has length %d or a non-alphanumeric byte, want %d letters and digits", out, len(passkey), passkeyLen)
		}
		_, rest, _ := bytes.Cut(out, []byte(marker))
		if !bytes.HasPrefix(rest, []byte(id+"/"+passkey)) {
			t.Fatalf("parseCredentials(%q) returned credentials that do not follow the first marker", out)
		}
	})
}

func TestParseCredentials(t *testing.T) {
	tests := []struct {
		name    string
		out     string
		wantErr string // "" means success with testID and testPasskey; otherwise a substring the error must contain
	}{
		{"plain", marker + testID + "/" + testPasskey + "\n", ""},
		{"crlf", marker + testID + "/" + testPasskey + "\r\n", ""},
		{"no trailing newline", marker + testID + "/" + testPasskey, ""},
		{"noise before", "Last login: Thu\nWelcome!\n" + marker + testID + "/" + testPasskey + "\n", ""},
		{"first marker wins", marker + testID + "/" + testPasskey + "\n" + marker + "zzzzzzzzzzzzzzzz/" + testPasskey, ""},
		{"no marker", "Welcome!\n", "no IDPASSKEY"},
		{"empty", "", "no IDPASSKEY"},
		{"truncated id", marker + "abcd", "malformed id"},
		{"id at end of output", marker + testID, "malformed id"},
		{"colon instead of slash", marker + testID + ":" + testPasskey, "malformed id"},
		{"truncated passkey", marker + testID + "/0123", "malformed passkey"},
		{"id too long", marker + testID + "X/" + testPasskey, "malformed id"},
		{"id too short", marker + testID[1:] + "/" + testPasskey, "malformed id"},
		{"passkey too long", marker + testID + "/" + testPasskey + "X\n", "malformed passkey"},
		{"missing slash", marker + testID + testPasskey, "malformed id"},
		{"non alphanumeric in id", marker + "abcdEFGH1234567-/" + testPasskey, "malformed id"},
		{"non alphanumeric in passkey", marker + testID + "/0123456789abcdef_BCDEF0123456789", "malformed passkey"},
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
