package bootstrap

import (
	"regexp"
	"strings"
	"testing"
)

// base32Text matches crypto/rand.Text output: the RFC 4648 base32 alphabet,
// which is alphanumeric, so it is safe inside the remote shell command.
var base32Text = regexp.MustCompile(`^[A-Z2-7]+$`)

func TestPlaceholder(t *testing.T) {
	id, passkey := placeholder()

	if len(id) != idLen {
		t.Fatalf("len(id) = %d, want %d (id %q)", len(id), idLen, id)
	}
	if !strings.HasPrefix(id, regeneratePrefix) {
		t.Fatalf("id %q does not start with %q", id, regeneratePrefix)
	}
	if len(passkey) != passkeyLen {
		t.Fatalf("len(passkey) = %d, want %d", len(passkey), passkeyLen)
	}
	for _, s := range []string{id, passkey} {
		if !base32Text.MatchString(s) {
			t.Fatalf("%q is not base32 text", s)
		}
	}

	id2, passkey2 := placeholder()
	if id2 == id || passkey2 == passkey {
		t.Fatalf("two placeholders are equal: %q/%q and %q/%q", id, passkey, id2, passkey2)
	}
}
