// Package bootstrap starts etterminal on the server over the system ssh and
// returns the session id and passkey it prints.
package bootstrap

import (
	"errors"
	"log/slog"
)

var (
	// ErrNoCredentials means ssh finished without printing a valid IDPASSKEY line.
	ErrNoCredentials = errors.New("bootstrap: no IDPASSKEY in ssh output")
	// ErrInvalidConfig means a Config field failed validation before ssh ran.
	ErrInvalidConfig = errors.New("bootstrap: invalid configuration")
)

// Config describes how to start etterminal over ssh. The zero value is not
// usable: Destination is required.
type Config struct {
	Destination  string       // ssh destination as typed; ssh_config aliases work
	User         string       // optional; becomes user@destination
	TerminalPath string       // default "etterminal"; must match [A-Za-z0-9._/~-]+
	Term         string       // default "xterm-256color"; must match [A-Za-z0-9.+-]+ (no underscore)
	SSHOptions   []string     // each passed as its own "-o<opt>" argument
	SSH          string       // ssh executable; default found with exec.LookPath("ssh")
	Logger       *slog.Logger // nil discards; receives the no-regeneration warning
}
