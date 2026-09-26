package bootstrap

import (
	"fmt"
	"strings"
	"unicode"
)

const (
	defaultTerm         = "xterm-256color"
	defaultTerminalPath = "etterminal"
)

// termExtra and terminalPathExtra are the bytes besides ASCII letters and
// digits allowed in the two configurable values interpolated into the remote
// shell command. Neither admits a quote, space, glob, '$' or command
// separator; terminalPathExtra admits '~', which the remote shell expands on
// purpose so "~/bin/etterminal" works.
//
// termExtra also excludes '_': etterminal splits its stdin line on '_' and
// aborts unless there are exactly two tokens, so a TERM containing '_' kills
// the session start (upstream src/terminal/TerminalMain.cpp:111-124; MEASURED
// against etserver 7.0.0: "Invalid number of tokens: 3", exit 134).
const (
	termExtra         = ".+-"
	terminalPathExtra = "._/~-"
)

// shellMeta holds the characters a Destination or User may not contain, so
// neither value can carry shell syntax into whatever ssh or its configuration
// does with it. OpenSSH 9.6 added its own hostname and user checks with the
// same aim (the CVE-2023-51385 fix): MEASURED against OpenSSH_10.0p2 on 2026-09-26, "ssh -G -l 'a;b' h6.example"
// fails with "remote username contains invalid characters" and
// "ssh -G 'h$x.example'" with "hostname contains invalid characters". The set
// here covers the metacharacters in those measured examples and more, and it
// matters most where ssh has no such check: the in-box Windows OpenSSH
// measured on 2026-09-26 was 9.5p2.
const shellMeta = "'`\"$\\;&<>|(){}"

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
	case strings.ContainsAny(cfg.Destination, shellMeta):
		return fmt.Errorf("%w: destination %q contains a shell metacharacter", ErrInvalidConfig, cfg.Destination)
	case strings.HasPrefix(cfg.User, "-"), hasSpaceOrControl(cfg.User), strings.ContainsAny(cfg.User, shellMeta):
		// '@' is allowed: the user goes to ssh as its own -l argument, so
		// "alice@corp.example" stays one user name.
		return fmt.Errorf("%w: user %q is not a valid user name", ErrInvalidConfig, cfg.User)
	case cfg.User != "" && strings.ContainsRune(cfg.Destination, '@'):
		// ssh would silently let -l win over the user in the destination
		// (MEASURED against OpenSSH_10.0p2 on 2026-09-26: "ssh -G -l alice
		// bob@h2.example" resolves user alice), so one of the two values
		// would be ignored.
		return fmt.Errorf("%w: user %q given but destination %q already names a user", ErrInvalidConfig, cfg.User, cfg.Destination)
	case !onlyAlnumOr(cfg.Term, termExtra):
		return fmt.Errorf("%w: TERM %q must be letters, digits and %s only", ErrInvalidConfig, cfg.Term, termExtra)
	case !onlyAlnumOr(cfg.TerminalPath, terminalPathExtra):
		return fmt.Errorf("%w: terminal path %q must be letters, digits and %s only", ErrInvalidConfig, cfg.TerminalPath, terminalPathExtra)
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
// argument, then "-l <user>" when User is set, then "--", the destination and
// the remote command as one argument.
//
// The user goes through -l rather than as user@destination so a user name
// holding '@' stays whole and a URI destination is passed untouched
// (MEASURED against OpenSSH_10.0p2 on 2026-09-26 with ssh -G:
// "-l alice@corp.example -- h3.example" resolves user alice@corp.example and
// "-l alice ssh://h1.example:2222" user alice, host h1.example, port 2222).
//
// "--" ends option parsing, so nothing from the destination onwards is read
// as an option, while the -o options before it still apply (MEASURED against
// OpenSSH_10.0p2 on 2026-09-26: "ssh -G -oPort=2200 -- h4.example" keeps port
// 2200, and "ssh -G -- h5.example -p 2201" keeps port 22 where
// "ssh -G h5.example -p 2201" gives 2201).
func sshArgs(cfg *Config, remote string) []string {
	args := make([]string, 0, len(cfg.SSHOptions)+5)
	for _, opt := range cfg.SSHOptions {
		args = append(args, "-o"+opt)
	}
	if cfg.User != "" {
		args = append(args, "-l", cfg.User)
	}
	return append(args, "--", cfg.Destination, remote)
}

func hasSpaceOrControl(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) })
}

func hasControl(s string) bool {
	return strings.ContainsFunc(s, unicode.IsControl)
}
