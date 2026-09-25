package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"

	"github.com/tphakala/et-go/internal/bootstrap"
	"github.com/tphakala/et-go/internal/console"
	"github.com/tphakala/et-go/internal/etcp"
	"github.com/tphakala/et-go/internal/protocol"
	"github.com/tphakala/et-go/internal/session"
)

// version is set at release time with -ldflags "-X main.version=...".
var version = "dev"

// detachedMessage is printed after the local escape or Ctrl+Break.
const detachedMessage = "Detached; the session stays on the server until it times out."

// sessionConn is what run needs from the etserver connection.
type sessionConn interface {
	session.Transport
	Close() error
}

// localConsole is what run needs from the local console.
type localConsole interface {
	session.Terminal
	MakeRaw() (restore func() error, err error)
}

// env holds run's side-effecting dependencies, so tests can replace them.
type env struct {
	bootstrap   func(ctx context.Context, cfg bootstrap.Config) (bootstrap.Credentials, error)
	resolveHost func(ctx context.Context, user, host string) string
	dial        func(ctx context.Context, d *etcp.Dialer, addr string, creds bootstrap.Credentials) (sessionConn, error)
	openConsole func() (localConsole, error)
	getenv      func(string) string
}

func defaultEnv() env {
	return env{
		bootstrap: bootstrap.Run,
		resolveHost: func(ctx context.Context, user, host string) string {
			return resolveHost(ctx, "ssh", user, host)
		},
		dial: func(ctx context.Context, d *etcp.Dialer, addr string, creds bootstrap.Credentials) (sessionConn, error) {
			c, err := d.Dial(ctx, addr, creds.ID, creds.Passkey())
			if err != nil {
				return nil, err
			}
			return c, nil
		},
		openConsole: func() (localConsole, error) {
			c, err := console.Open()
			if err != nil {
				return nil, err
			}
			return c, nil
		},
		getenv: os.Getenv,
	}
}

// *etcp.Conn is passed to session as is. session.Run treats a ReadPacket
// error wrapping io.EOF as the normal end of a session, and etcp defines
// ErrSessionEnded (INVALID_KEY on redial) to wrap io.EOF; TestEtcpConnIsTransport
// pins that contract.
var _ sessionConn = (*etcp.Conn)(nil)

// run is the whole client: it returns the process exit status.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runWith(ctx, args, defaultEnv(), stdout, stderr)
}

func runWith(ctx context.Context, args []string, e env, stdout, stderr io.Writer) int {
	o, err := parseArgs(args, stderr)
	switch {
	case errors.Is(err, flag.ErrHelp):
		return 0
	case err != nil:
		_, _ = fmt.Fprintf(stderr, "et: %v\n", err)
		return 2
	case o.version:
		_, _ = fmt.Fprintf(stdout, "et %s\n", version)
		return 0
	}

	log, closeLog, err := newLogger(o.verbose, o.logFile)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "et: %v\n", err)
		return 255
	}
	defer func() { _ = closeLog() }()

	return exitCode(connect(ctx, o, e, log), stderr)
}

// connect runs one session. The console is restored before it returns, so
// the caller can print normally.
func connect(ctx context.Context, o *options, e env, log *slog.Logger) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	onBreak(func() { cancel(session.ErrDetached) })
	defer onBreak(nil)

	con, err := e.openConsole()
	if err != nil {
		return err
	}
	runStarted := false
	defer func() {
		if !runStarted { // session.Run closes the console itself
			_ = con.Close()
		}
	}()

	creds, err := e.bootstrap(ctx, bootstrap.Config{
		Destination:  o.dest.Host,
		User:         o.dest.User,
		TerminalPath: o.terminalPath,
		Term:         e.getenv("TERM"),
		SSHOptions:   o.sshOptions,
		Logger:       log,
	})
	if err != nil {
		return err
	}
	log.Info("etterminal started", "credentials", creds)

	addr := net.JoinHostPort(e.resolveHost(ctx, o.dest.User, o.dest.Host), strconv.Itoa(o.dest.Port))
	d := &etcp.Dialer{
		KeepAlive: o.keepAlive,
		Probe:     protocol.Packet{Header: protocol.HeaderKeepAlive},
		Logger:    log,
	}
	conn, err := e.dial(ctx, d, addr, creds)
	if err != nil {
		return fmt.Errorf("connect to etserver at %s: %w", addr, err)
	}
	defer func() { _ = conn.Close() }()

	opts := session.Options{Terminal: con, Logger: log}
	if err := session.Start(ctx, conn, opts); err != nil {
		return err
	}

	restore, err := con.MakeRaw()
	if err != nil {
		return err
	}
	// Restore on every return, and on a panic in this goroutine restore
	// first, then re-panic (Go prints "[recovered, repanicked]" with the
	// original stack).
	defer func() {
		_ = restore()
		if r := recover(); r != nil {
			panic(r)
		}
	}()

	runStarted = true
	return session.Run(ctx, conn, opts)
}

// exitCode maps the session outcome to the process exit status (spec 5.8).
func exitCode(err error, stderr io.Writer) int {
	if err == nil {
		return 0
	}
	if ee, ok := errors.AsType[*session.ExitError](err); ok {
		return ee.Code
	}
	if errors.Is(err, session.ErrDetached) {
		_, _ = fmt.Fprintln(stderr, detachedMessage)
		return 0
	}
	_, _ = fmt.Fprintf(stderr, "et: %v\n", err)
	return 255
}
