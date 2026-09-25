package bootstrap

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

const (
	defaultTerm         = "xterm-256color"
	defaultTerminalPath = "etterminal"
)

// termPattern and terminalPathPattern bound the two configurable values
// interpolated into the remote shell command. Neither admits a quote, space,
// glob, '$' or command separator; terminalPathPattern admits '~', which the
// remote shell expands on purpose so "~/bin/etterminal" works.
//
// termPattern also excludes '_': etterminal splits its stdin line on '_' and
// aborts unless there are exactly two tokens, so a TERM containing '_' kills
// the session start (upstream src/terminal/TerminalMain.cpp:111-124; MEASURED
// against etserver 7.0.0: "Invalid number of tokens: 3", exit 134).
var (
	termPattern         = regexp.MustCompile(`^[A-Za-z0-9.+-]+$`)
	terminalPathPattern = regexp.MustCompile(`^[A-Za-z0-9._/~-]+$`)
)

// applyDefaults fills in empty optional fields. Run calls it on its own copy
// of the caller's Config.
func (cfg *Config) applyDefaults() {
	if cfg.Term == "" {
		cfg.Term = defaultTerm
	}
	if cfg.TerminalPath == "" {
		cfg.TerminalPath = defaultTerminalPath
	}
}

// validate reports the first problem with cfg, wrapped in ErrInvalidConfig.
// It expects applyDefaults to have been called.
func (cfg *Config) validate() error {
	switch {
	case cfg.Destination == "":
		return fmt.Errorf("%w: destination is empty", ErrInvalidConfig)
	case strings.HasPrefix(cfg.Destination, "-"):
		// ssh would parse it as an option.
		return fmt.Errorf("%w: destination %q starts with '-'", ErrInvalidConfig, cfg.Destination)
	case hasSpaceOrControl(cfg.Destination):
		return fmt.Errorf("%w: destination %q contains whitespace or control characters", ErrInvalidConfig, cfg.Destination)
	case strings.ContainsRune(cfg.User, '@'), strings.HasPrefix(cfg.User, "-"), hasSpaceOrControl(cfg.User):
		return fmt.Errorf("%w: user %q is not a valid user name", ErrInvalidConfig, cfg.User)
	case cfg.User != "" && strings.ContainsRune(cfg.Destination, '@'):
		// ssh would read "user@alice@host" as user "user@alice", not what either value meant.
		return fmt.Errorf("%w: user %q given but destination %q already names a user", ErrInvalidConfig, cfg.User, cfg.Destination)
	case !termPattern.MatchString(cfg.Term):
		return fmt.Errorf("%w: TERM %q must match %s", ErrInvalidConfig, cfg.Term, termPattern)
	case !terminalPathPattern.MatchString(cfg.TerminalPath):
		return fmt.Errorf("%w: terminal path %q must match %s", ErrInvalidConfig, cfg.TerminalPath, terminalPathPattern)
	}
	for _, opt := range cfg.SSHOptions {
		if opt == "" || hasControl(opt) {
			return fmt.Errorf("%w: ssh option %q is empty or contains control characters", ErrInvalidConfig, opt)
		}
	}
	return nil
}

// remoteCommand is the command ssh runs on the server. It mirrors upstream's
// genCommand (src/terminal/SshSetupHandler.cpp): etterminal reads
// "<id>/<passkey>_<TERM>" on stdin.
// Every interpolated value is validated or generated from [A-Za-z0-9].
func remoteCommand(id, passkey, term, terminalPath string) string {
	return "echo '" + id + "/" + passkey + "_" + term + "' | " + terminalPath + " --verbose=0"
}

// sshArgs builds ssh's argument list: every option as its own "-o<opt>"
// argument, then [user@]destination, then the remote command as one argument.
func sshArgs(cfg *Config, remote string) []string {
	dest := cfg.Destination
	if cfg.User != "" {
		dest = cfg.User + "@" + dest
	}
	args := make([]string, 0, len(cfg.SSHOptions)+2)
	for _, opt := range cfg.SSHOptions {
		args = append(args, "-o"+opt)
	}
	return append(args, dest, remote)
}

func hasSpaceOrControl(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) })
}

func hasControl(s string) bool {
	return strings.ContainsFunc(s, unicode.IsControl)
}
