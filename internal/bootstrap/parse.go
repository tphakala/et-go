package bootstrap

import (
	"bytes"
	"fmt"
)

// marker precedes the credentials in etterminal's output
// (upstream src/terminal/TerminalMain.cpp:185).
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
	id, n := alnumRun(rest)
	if n != idLen || len(rest) == n || rest[n] != '/' {
		return Credentials{}, fmt.Errorf("%w: malformed id after %q", ErrNoCredentials, marker)
	}
	passkey, m := alnumRun(rest[n+1:])
	if m != passkeyLen {
		// Do not echo the value: a malformed passkey may still be a real one.
		return Credentials{}, fmt.Errorf("%w: malformed passkey after %q", ErrNoCredentials, marker)
	}
	return NewCredentials(string(id), string(passkey)), nil
}

// alnumRun returns the leading run of ASCII letters and digits in b and its length.
func alnumRun(b []byte) (run []byte, n int) {
	for n < len(b) && isAlnum(b[n]) {
		n++
	}
	return b[:n], n
}

func isAlnum(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}
