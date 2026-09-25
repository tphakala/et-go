package bootstrap

import "crypto/rand"

const (
	// idLen and passkeyLen are the lengths etterminal expects and returns
	// (upstream src/terminal/SshSetupHandler.cpp: genRandomAlphaNum(16) and (32)).
	idLen      = 16
	passkeyLen = 32

	// regeneratePrefix makes etterminal discard the id and passkey it is sent
	// and generate fresh ones (upstream src/terminal/TerminalMain.cpp:114-120).
	regeneratePrefix = "XXX"
)

// placeholder returns the throwaway id and passkey sent to etterminal. They
// are not secrets: with the regeneratePrefix the server replaces both and
// returns the real ones on stdout.
//
// rand.Text returns at least 26 base32 characters ([A-Z2-7]), all
// alphanumeric, so slicing to 13 and to 32 (from two calls) is always in range.
func placeholder() (id, passkey string) {
	id = regeneratePrefix + rand.Text()[:idLen-len(regeneratePrefix)]
	passkey = (rand.Text() + rand.Text())[:passkeyLen]
	return id, passkey
}
