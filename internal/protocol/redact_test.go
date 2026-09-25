package protocol

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// TestTerminalUserInfoRedacted checks that no fmt verb and no slog handler
// renders a TerminalUserInfo's passkey, that each rendering says REDACTED,
// and that the id still appears, for two different ids so a hardcoded id
// cannot pass.
func TestTerminalUserInfoRedacted(t *testing.T) {
	const passkey = "SECRETPASSKEY0123456789abcdefXYZ"
	for _, id := range []string{"XXXabcdefghijklm", "YYYnopqrstuvwxyz"} {
		u := &TerminalUserInfo{}
		u.SetId(id)
		u.SetPasskey(passkey)

		check := func(t *testing.T, out string) {
			t.Helper()
			if strings.Contains(out, passkey) {
				t.Errorf("output %q contains the passkey", out)
			}
			if !strings.Contains(out, "REDACTED") {
				t.Errorf("output %q does not contain REDACTED", out)
			}
			if !strings.Contains(out, id) {
				t.Errorf("output %q does not contain the id %q", out, id)
			}
		}
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q"} {
			t.Run(id+" "+verb, func(t *testing.T) { check(t, fmt.Sprintf(verb, u)) })
		}
		t.Run(id+" slog text", func(t *testing.T) {
			var buf bytes.Buffer
			slog.New(slog.NewTextHandler(&buf, nil)).Info("u", slog.Any("u", u))
			check(t, buf.String())
		})
		t.Run(id+" slog json", func(t *testing.T) {
			var buf bytes.Buffer
			slog.New(slog.NewJSONHandler(&buf, nil)).Info("u", slog.Any("u", u))
			check(t, buf.String())
		})
	}

	t.Run("nil", func(t *testing.T) {
		var u *TerminalUserInfo
		// Through fmt, not u.String(): the generated String bypasses Format.
		var sb strings.Builder
		_, _ = fmt.Fprintf(&sb, "%v", u)
		if got := sb.String(); got != "<nil>" {
			t.Errorf("fmt %%v of a nil *TerminalUserInfo = %q, want <nil>", got)
		}
	})
}
