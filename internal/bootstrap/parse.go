package bootstrap

import (
	"bytes"
	"fmt"
	"strings"
)

// marker precedes the credentials in etterminal's output
// (upstream src/terminal/TerminalMain.cpp:185 at et-v7.0.0).
const marker = "IDPASSKEY:"

// parseCredentials finds the first IDPASSKEY marker in out and returns the
// id and passkey that follow it. It requires exactly idLen alphanumeric
// characters, a '/', then exactly passkeyLen alphanumeric characters, followed
// by the end of output or a non-alphanumeric character. Anything before the
// marker (banners, motd, shell noise) is ignored. Only the first marker counts,
// as in the upstream client (src/terminal/SshSetupHandler.cpp, sshBuffer.find),
// so a banner that itself prints "IDPASSKEY:" makes the start fail.
func parseCredentials(out []byte) (Credentials, error) {
	_, rest, found := bytes.Cut(out, []byte(marker))
	if !found {
		return Credentials{}, ErrNoCredentials
	}
	id := alnumRun(rest)
	n := len(id)
	if n != idLen || len(rest) == n || rest[n] != '/' {
		return Credentials{}, fmt.Errorf("%w: malformed id after %q", ErrNoCredentials, marker)
	}
	passkey := alnumRun(rest[n+1:])
	if len(passkey) != passkeyLen {
		// Do not echo the value: a malformed passkey may still be a real one.
		return Credentials{}, fmt.Errorf("%w: malformed passkey after %q", ErrNoCredentials, marker)
	}
	return NewCredentials(string(id), string(passkey)), nil
}

// alnumRun returns the leading run of ASCII letters and digits in b.
func alnumRun(b []byte) []byte {
	n := 0
	for n < len(b) && isAlnum(b[n]) {
		n++
	}
	return b[:n]
}

// onlyAlnumOr reports whether s is non-empty and every byte of it is an
// ASCII letter, an ASCII digit or one of the bytes in extra.
func onlyAlnumOr(s, extra string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if !isAlnum(s[i]) && strings.IndexByte(extra, s[i]) < 0 {
			return false
		}
	}
	return true
}

func isAlnum(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}
