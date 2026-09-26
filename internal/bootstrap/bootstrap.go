// Package bootstrap starts etterminal on the server over the system ssh and
// returns the session id and passkey it prints.
package bootstrap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	// ErrNoCredentials means ssh finished without printing a valid IDPASSKEY line.
	ErrNoCredentials = errors.New("bootstrap: no IDPASSKEY in ssh output")
	// ErrInvalidConfig means a Config field failed validation before ssh ran.
	ErrInvalidConfig = errors.New("bootstrap: invalid configuration")
)

const (
	// maxOutput caps how much ssh stdout is kept. Shell startup noise before
	// the marker is normally a few lines, far below the cap; a marker that
	// arrives after the cap is dropped and the start fails, with an error
	// that says output was dropped. Keep it a whole number of MiB: that
	// error states it in MiB.
	maxOutput = 1 << 20
	// excerptLen caps the output excerpt quoted in an error.
	excerptLen = 512
	// waitDelay is how long Run waits after cancelling ssh before killing it,
	// and how long it waits for ssh's stdout to close after ssh exits; the
	// exec.ErrWaitDelay branch in Run depends on the latter.
	waitDelay = 2 * time.Second
)

// Config describes how to start etterminal over ssh. The zero value is not
// usable: Destination is required.
type Config struct {
	Destination  string       // ssh destination as typed; ssh_config aliases work
	User         string       // optional; passed to ssh as -l, so it may contain '@'
	TerminalPath string       // default "etterminal"; must match [A-Za-z0-9._/~-]+
	Term         string       // default "xterm-256color"; must match [A-Za-z0-9.+-]+ (no underscore)
	SSHOptions   []string     // each passed as its own "-o<opt>" argument
	SSH          string       // ssh executable; default found with exec.LookPath("ssh")
	Logger       *slog.Logger // nil discards; receives the no-regeneration warning
}

// Run starts etterminal on the server through ssh and returns the session
// credentials it prints. ssh's stdin and stderr are the process's own, so
// password, passphrase and host key prompts work; stdout is captured. Run
// must finish before the local console switches to raw mode.
//
// ssh's exit status does not decide success: etterminal daemonises after
// printing its credentials, so ssh can exit 0 or not regardless
// (upstream src/terminal/TerminalMain.cpp:185-188, MEASURED against etserver 7.0.0).
// Cancellation does: once ctx is done, Run returns its error even if the
// credentials had already been printed.
//
//nolint:gocritic // hugeParam: Config is taken by value on purpose so Run fills defaults on its own copy and never mutates the caller's; bootstrap runs once per session, not on a hot path.
func Run(ctx context.Context, cfg Config) (Credentials, error) {
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return Credentials{}, err
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	sshPath := cfg.SSH
	if sshPath == "" {
		p, err := exec.LookPath("ssh")
		if err != nil {
			return Credentials{}, fmt.Errorf("bootstrap: ssh not found on PATH (install OpenSSH): %w", err)
		}
		sshPath = p
	}

	id, passkey := placeholder()
	cmd, out := newCommand(ctx, sshPath, sshArgs(&cfg, remoteCommand(id, passkey, cfg.Term, cfg.TerminalPath)))
	runErr := cmd.Run()
	creds, parseErr := parseCredentials(out.Bytes())
	// A cancelled ctx wins even over credentials that already arrived: the
	// caller asked to stop, so it must not go on to connect.
	if parseErr == nil && ctx.Err() == nil {
		if creds.ID == id {
			logger.Warn("etterminal did not regenerate the session id; the passkey in use was visible in the ssh command line on both hosts",
				"credentials", creds)
		}
		return creds, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		// A caller can match context.Canceled or DeadlineExceeded as well
		// as a cause it set. A cause that already wraps the context error
		// (or is it) is reported alone, so the text names it once.
		cause := context.Cause(ctx)
		if errors.Is(cause, ctxErr) {
			return Credentials{}, fmt.Errorf("bootstrap: ssh interrupted: %w", cause)
		}
		return Credentials{}, fmt.Errorf("bootstrap: ssh interrupted: %w: %w", ctxErr, cause)
	}
	exitErr, isExit := errors.AsType[*exec.ExitError](runErr)
	// ErrWaitDelay means ssh exited 0 but a descendant kept its stdout open
	// past waitDelay: a finished run without credentials, not a start failure.
	if runErr != nil && !isExit && !errors.Is(runErr, exec.ErrWaitDelay) {
		return Credentials{}, fmt.Errorf("bootstrap: run %s: %w", sshPath, runErr)
	}
	// The remote side may echo the command it ran (shell tracing, a
	// diagnostic) before any marker, and against a server that does not
	// regenerate, the generated passkey is the session passkey. Output that
	// holds any piece of it cannot be scrubbed reliably (it may be split or
	// wrapped), so it is not quoted at all.
	shown := out.Bytes()
	if containsPiece(shown, passkey) {
		shown = []byte(withheldOutput)
	}
	return Credentials{}, describeFailure(exitErr, parseErr, shown, out.dropped > 0)
}

// newCommand prepares the ssh command. Its stdin and stderr are the process's
// own, so password, passphrase and host key prompts reach the user; its
// stdout is captured in the returned buffer.
func newCommand(ctx context.Context, sshPath string, args []string) (*exec.Cmd, *cappedBuffer) {
	cmd := exec.CommandContext(ctx, sshPath, args...)
	cmd.Stdin = os.Stdin
	cmd.Stderr = os.Stderr
	out := &cappedBuffer{max: maxOutput}
	cmd.Stdout = out
	cmd.Cancel = func() error {
		// Interrupt first so ssh can restore the terminal; Windows cannot
		// deliver os.Interrupt to another process, so kill there.
		if err := cmd.Process.Signal(os.Interrupt); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
	cmd.WaitDelay = waitDelay
	return cmd, out
}

const (
	// minPiece is the shortest piece of the passkey containsPiece looks for.
	// Only output wrapped at fewer columns than this could slip past it.
	minPiece = 4
	// withheldOutput replaces an excerpt that would quote passkey material.
	withheldOutput = "[withheld: the output contains part of the generated session key]"
)

// containsPiece reports whether out holds any minPiece-byte piece of secret.
func containsPiece(out []byte, secret string) bool {
	for i := 0; i+minPiece <= len(secret); i++ {
		if bytes.Contains(out, []byte(secret[i:i+minPiece])) {
			return true
		}
	}
	return false
}

// describeFailure builds the single error the user sees when ssh finished
// without valid credentials: the parse problem, ssh's exit status or the
// signal that killed it, a hint for the common exit statuses, a note when
// output past maxOutput was dropped, and a trimmed excerpt of stdout.
func describeFailure(exitErr *exec.ExitError, parseErr error, out []byte, overflowed bool) error {
	var b strings.Builder
	b.WriteString("ssh ")
	code, sig, killed := 0, "", false
	if exitErr != nil {
		code = exitErr.ExitCode() // -1 when a signal ended ssh
		sig, killed = killedBy(exitErr)
	}
	if killed {
		fmt.Fprintf(&b, "was killed by signal %s", sig)
	} else {
		fmt.Fprintf(&b, "exited with status %d", code)
	}
	switch code {
	case 127:
		b.WriteString(": etterminal was not found on the server; install Eternal Terminal there or pass --terminal-path")
	case 255:
		b.WriteString(": ssh could not connect or authenticate; check that plain ssh to this host works")
	}
	if overflowed {
		fmt.Fprintf(&b, "; output exceeded %d MiB; the credentials may have been cut off", maxOutput>>20)
	}
	if ex := excerpt(out); ex != "" {
		fmt.Fprintf(&b, "; output: %q", ex)
	}
	return fmt.Errorf("%w: %s", parseErr, b.String())
}

// excerpt returns at most the last excerptLen bytes of the trimmed output
// before the first marker, prefixed with "..." when it was cut, so no part of
// a (possibly malformed) passkey is ever quoted. A cut never splits a UTF-8
// sequence: the excerpt then starts at the next rune.
func excerpt(out []byte) string {
	before, _, _ := bytes.Cut(out, []byte(marker))
	b := bytes.TrimSpace(before)
	if len(b) <= excerptLen {
		return string(b)
	}
	b = b[len(b)-excerptLen:]
	for len(b) > 0 && !utf8.RuneStart(b[0]) {
		b = b[1:]
	}
	return "..." + string(b)
}

// cappedBuffer keeps the first max bytes written to it and drops the rest,
// counting them in dropped, so a chatty login shell cannot grow memory
// without bound. It never returns an error, so ssh never sees a broken pipe.
type cappedBuffer struct {
	buf     bytes.Buffer
	max     int
	dropped int // bytes written past max and discarded
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	kept := min(len(p), max(c.max-c.buf.Len(), 0))
	c.buf.Write(p[:kept])
	c.dropped += len(p) - kept
	return len(p), nil
}

func (c *cappedBuffer) Bytes() []byte { return c.buf.Bytes() }
