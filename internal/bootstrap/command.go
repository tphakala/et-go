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

// shellMeta holds the characters a Destination may not contain, and userMeta
// those a User may not contain, so neither value carries POSIX shell syntax
// into a ProxyCommand or Match exec that ssh expands %h or %r into (MEASURED
// against OpenSSH_10.0p2 on 2026-09-26: both commands received the host and
// the -l user substituted for %h and %r). OpenSSH 9.6 added its own hostname
// and user checks with the same aim (the CVE-2023-51385 fix). MEASURED
// against OpenSSH_10.0p2 on 2026-09-26 with
// ssh -G: a host with '$' and a user with ';', '(' or '"' or ending in '\'
// are refused, while the users CORP\alice, host$ and a$b are accepted.
// userMeta therefore leaves out '$' and '\', which winbind DOMAIN\user names
// and Samba machine accounts use, and validate refuses a trailing '\'
// instead. MEASURED against OpenSSH_for_Windows_9.5p2 on 2026-09-26: ssh -G
// accepts the user a;b and the host h$x, so on that client these sets are
// the only check. They model a POSIX shell; which interpreter the Windows
// client runs a ProxyCommand with is not measured.
//
// shellMeta also holds the glob characters, which a shell would expand
// against local file names: no host name contains them, and ssh keeps the
// brackets of a bracketed address as part of the host name (MEASURED against
// OpenSSH_10.0p2 on 2026-09-26: "ssh -G -- [::1]" gives hostname "[::1]"), so
// an IPv6 destination is passed bare. userMeta leaves them out, as OpenSSH
// accepts them in a user name.
const (
	shellMeta = "'`\"$\\;&<>|(){}*?[]"
	userMeta  = "'`\";&<>|(){}"
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
		// sshArgs puts "--" before the destination, so ssh no longer reads it
		// as an option, but ssh still substitutes it for %h in a ProxyCommand
		// or Match exec (measured, see shellMeta), where a command such as
		// "nc %h %p" would take it as an option.
		return fmt.Errorf("%w: destination %q starts with '-'", ErrInvalidConfig, cfg.Destination)
	case hasSpaceOrControl(cfg.Destination):
		return fmt.Errorf("%w: destination %q contains whitespace or control characters", ErrInvalidConfig, cfg.Destination)
	case strings.ContainsAny(cfg.Destination, shellMeta):
		return fmt.Errorf("%w: destination %q contains a shell metacharacter", ErrInvalidConfig, cfg.Destination)
	case strings.HasPrefix(cfg.User, "-"), hasSpaceOrControl(cfg.User):
		// '@' is allowed: the user goes to ssh as its own -l argument, so
		// "alice@corp.example" stays one user name.
		return fmt.Errorf("%w: user %q is not a valid user name", ErrInvalidConfig, cfg.User)
	case strings.ContainsAny(cfg.User, userMeta):
		return fmt.Errorf("%w: user %q contains a shell metacharacter", ErrInvalidConfig, cfg.User)
	case strings.HasSuffix(cfg.User, `\`):
		return fmt.Errorf("%w: user %q ends in a backslash", ErrInvalidConfig, cfg.User)
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
