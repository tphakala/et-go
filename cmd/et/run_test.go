package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/tphakala/et-go/internal/bootstrap"
	"github.com/tphakala/et-go/internal/console"
	"github.com/tphakala/et-go/internal/etcp"
	"github.com/tphakala/et-go/internal/protocol"
	"github.com/tphakala/et-go/internal/session"
)

// events records the order of side effects across the fakes.
type events struct {
	mu  sync.Mutex
	log []string
}

func (e *events) add(s string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.log = append(e.log, s)
}

func (e *events) list() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.log)
}

// fakeConn answers INITIAL_PAYLOAD with the scripted response, then plays
// the scripted packets and ends the session with io.EOF, as *etcp.Conn does
// through etcp.ErrSessionEnded.
type fakeConn struct {
	ev       *events
	startErr string
	script   []protocol.Packet
	in       chan protocol.Packet
	keepOpen bool // true: do not end the session after the script
}

func newFakeConn(ev *events, startErr string, script ...protocol.Packet) *fakeConn {
	return &fakeConn{ev: ev, startErr: startErr, script: script, in: make(chan protocol.Packet, len(script)+1)}
}

func (c *fakeConn) WritePacket(ctx context.Context, p protocol.Packet) error {
	if p.Header == protocol.HeaderInitialPayload {
		c.ev.add("start")
		resp := &protocol.InitialResponse{}
		if c.startErr != "" {
			resp.SetError(c.startErr)
		}
		b, _ := proto.Marshal(resp)
		c.in <- protocol.Packet{Header: protocol.HeaderInitialResponse, Payload: b}
		if c.startErr == "" {
			for _, s := range c.script {
				c.in <- s
			}
			if !c.keepOpen {
				close(c.in)
			}
		}
	}
	return nil
}

func (c *fakeConn) ReadPacket(ctx context.Context) (protocol.Packet, error) {
	select {
	case p, ok := <-c.in:
		if !ok {
			return protocol.Packet{}, io.EOF
		}
		return p, nil
	case <-ctx.Done():
		return protocol.Packet{}, ctx.Err()
	}
}

func (c *fakeConn) Close() error { c.ev.add("conn closed"); return nil }

// fakeConsole blocks reads until closed and records raw mode changes.
type fakeConsole struct {
	ev     *events
	closed chan struct{}
	once   sync.Once
	out    bytes.Buffer

	// makeRawErr, when set, makes MakeRaw fail instead of succeeding.
	makeRawErr error
	// sizePanic, when set, makes Size panic with that value instead of
	// returning normally. Size is called synchronously from session.Run's
	// caller goroutine (terminalService.start, before any of its goroutines
	// are spawned), so this exercises connect's panic-then-restore defer.
	sizePanic any
}

func newFakeConsole(ev *events) *fakeConsole {
	return &fakeConsole{ev: ev, closed: make(chan struct{})}
}

func (c *fakeConsole) Read(p []byte) (int, error) {
	<-c.closed
	return 0, errors.New("closed")
}

func (c *fakeConsole) Write(p []byte) (int, error) { return c.out.Write(p) }

func (c *fakeConsole) Close() error {
	c.once.Do(func() { c.ev.add("console closed"); close(c.closed) })
	return nil
}

func (c *fakeConsole) Size() (console.Size, error) {
	if c.sizePanic != nil {
		panic(c.sizePanic)
	}
	return console.Size{Rows: 24, Cols: 80}, nil
}

func (c *fakeConsole) Resizes(ctx context.Context) iter.Seq[console.Size] {
	return func(func(console.Size) bool) { <-ctx.Done() }
}

func (c *fakeConsole) MakeRaw() (func() error, error) {
	if c.makeRawErr != nil {
		return nil, c.makeRawErr
	}
	c.ev.add("raw")
	return func() error { c.ev.add("restored"); return nil }, nil
}

func exitPacket(code int32) protocol.Packet {
	st := &protocol.TerminalExitStatus{}
	st.SetExitcode(code)
	b, _ := proto.Marshal(st)
	return protocol.Packet{Header: protocol.HeaderTerminalExitStatus, Payload: b}
}

func testEnv(ev *events, con *fakeConsole, conn *fakeConn, bootErr error) env {
	return env{
		bootstrap: func(ctx context.Context, cfg bootstrap.Config) (bootstrap.Credentials, error) {
			ev.add("bootstrap " + cfg.User + "@" + cfg.Destination)
			return bootstrap.NewCredentials("XXXid", "secret"), bootErr
		},
		resolveHost: func(ctx context.Context, user, host string) string { return host },
		dial: func(ctx context.Context, d *etcp.Dialer, addr string, creds bootstrap.Credentials) (sessionConn, error) {
			ev.add("dial " + addr)
			if d.Probe.Header != protocol.HeaderKeepAlive {
				return nil, errors.New("probe is not KEEP_ALIVE")
			}
			return conn, nil
		},
		openConsole: func() (localConsole, error) { return con, nil },
		getenv:      func(string) string { return "xterm-256color" },
		onBreak: func(f func()) {
			if f != nil {
				ev.add("break on")
			} else {
				ev.add("break off")
			}
		},
	}
}

func TestRunExitStatusFlow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ev := &events{}
		con := newFakeConsole(ev)
		conn := newFakeConn(ev, "", exitPacket(7))
		var stderr bytes.Buffer
		code := runWith(t.Context(), []string{"me@box"}, testEnv(ev, con, conn, nil), io.Discard, &stderr)
		if code != 7 {
			t.Fatalf("exit code %d, want 7 (stderr %q)", code, stderr.String())
		}
		want := []string{"bootstrap me@box", "dial box:2022", "start", "break on", "raw", "console closed", "restored", "break off", "conn closed"}
		if got := ev.list(); !slices.Equal(got, want) {
			t.Fatalf("events %q\nwant   %q", got, want)
		}
	})
}

func TestRunNormalEndIsZero(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ev := &events{}
		code := runWith(t.Context(), []string{"box"}, testEnv(ev, newFakeConsole(ev), newFakeConn(ev, ""), nil), io.Discard, io.Discard)
		if code != 0 {
			t.Fatalf("exit code %d, want 0", code)
		}
	})
}

func TestRunBootstrapFailureNeverGoesRaw(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ev := &events{}
		var stderr bytes.Buffer
		code := runWith(t.Context(), []string{"box"}, testEnv(ev, newFakeConsole(ev), newFakeConn(ev, ""), errors.New("etterminal: command not found")), io.Discard, &stderr)
		if code != 255 || !strings.Contains(stderr.String(), "etterminal") {
			t.Fatalf("exit code %d, stderr %q", code, stderr.String())
		}
		if slices.Contains(ev.list(), "raw") {
			t.Fatal("console went raw after a bootstrap failure")
		}
		if slices.Contains(ev.list(), "break on") {
			t.Fatal("Ctrl+Break handler registered before a session existed")
		}
		if !slices.Contains(ev.list(), "console closed") {
			t.Fatal("console not closed")
		}
	})
}

func TestRunStartRejectedNeverGoesRaw(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ev := &events{}
		var stderr bytes.Buffer
		code := runWith(t.Context(), []string{"box"}, testEnv(ev, newFakeConsole(ev), newFakeConn(ev, "no terminal"), nil), io.Discard, &stderr)
		if code != 255 || !strings.Contains(stderr.String(), "no terminal") {
			t.Fatalf("exit code %d, stderr %q", code, stderr.String())
		}
		if slices.Contains(ev.list(), "raw") {
			t.Fatal("console went raw after a rejected start")
		}
		if slices.Contains(ev.list(), "break on") {
			t.Fatal("Ctrl+Break handler registered after a rejected start")
		}
	})
}

// TestRunBreakDuringSessionDetaches pins what the registered Ctrl+Break
// handler does: while the session runs it ends et as a detach, exit status
// 0 with the detach message.
func TestRunBreakDuringSessionDetaches(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ev := &events{}
		conn := newFakeConn(ev, "")
		conn.keepOpen = true
		e := testEnv(ev, newFakeConsole(ev), conn, nil)
		var handler func()
		e.onBreak = func(f func()) {
			if f != nil {
				handler = f
			}
		}
		var stderr bytes.Buffer
		done := make(chan int, 1)
		go func() { done <- runWith(t.Context(), []string{"box"}, e, io.Discard, &stderr) }()
		synctest.Wait()
		if handler == nil {
			t.Fatal("no Ctrl+Break handler registered while the session runs")
		}
		handler()
		if code := <-done; code != 0 || !strings.Contains(stderr.String(), "Detached") {
			t.Fatalf("exit code %d, stderr %q; want 0 and the detach message", code, stderr.String())
		}
	})
}

func TestRunUsageErrors(t *testing.T) {
	var stderr bytes.Buffer
	if code := runWith(t.Context(), []string{}, env{}, io.Discard, &stderr); code != 2 {
		t.Fatalf("no destination: exit code %d, want 2", code)
	}
	if code := runWith(t.Context(), []string{"-h"}, env{}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("-h: exit code %d, want 0", code)
	}
	var stdout bytes.Buffer
	if code := runWith(t.Context(), []string{"--version"}, env{}, &stdout, io.Discard); code != 0 || !strings.HasPrefix(stdout.String(), "et ") {
		t.Fatalf("--version: exit code %d, stdout %q", code, stdout.String())
	}
}

// TestRun pins that run itself (not just runWith) reaches parseArgs, wiring
// in defaultEnv along the way, for a flow that returns before any real I/O.
func TestRun(t *testing.T) {
	var stdout bytes.Buffer
	if code := run(t.Context(), []string{"--version"}, &stdout, io.Discard); code != 0 || !strings.HasPrefix(stdout.String(), "et ") {
		t.Fatalf("run --version: exit code %d, stdout %q", code, stdout.String())
	}
}

// TestRunNewLoggerFailure pins that a log file that cannot be created is
// reported as a startup failure rather than panicking or being ignored.
func TestRunNewLoggerFailure(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(blocker, "sub", "et.log")
	var stderr bytes.Buffer
	code := runWith(t.Context(), []string{"--log-file", logPath, "box"}, env{}, io.Discard, &stderr)
	if code != 255 || !strings.Contains(stderr.String(), "create log directory") {
		t.Fatalf("exit code %d, stderr %q", code, stderr.String())
	}
}

// TestDefaultEnvWiring pins that defaultEnv's closures reach the real
// packages they wrap (bootstrap, resolveHost, etcp, console), without
// requiring a live etserver or a controlling terminal: resolveHost falls
// back to the host unchanged, dial fails against an address nothing listens
// on, and openConsole either fails (no controlling terminal in the test
// environment) or succeeds and is closed.
func TestDefaultEnvWiring(t *testing.T) {
	e := defaultEnv()
	if e.bootstrap == nil || e.resolveHost == nil || e.dial == nil || e.openConsole == nil || e.getenv == nil || e.onBreak == nil {
		t.Fatal("defaultEnv left a dependency nil")
	}

	if got := e.resolveHost(t.Context(), "", "et-go-test-host.invalid"); got != "et-go-test-host.invalid" {
		t.Fatalf("resolveHost(unresolvable host) = %q, want the host unchanged", got)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if _, err := e.dial(ctx, &etcp.Dialer{}, "127.0.0.1:1", bootstrap.NewCredentials("id", "pass")); err == nil {
		t.Fatal("dial to a port nothing listens on: want error, got nil")
	}

	con, err := e.openConsole()
	if err == nil {
		_ = con.Close()
	}
}

// TestConnectOpenConsoleFailure pins that a console that fails to open ends
// the session before bootstrap runs at all.
func TestConnectOpenConsoleFailure(t *testing.T) {
	ev := &events{}
	wantErr := errors.New("not a terminal")
	e := env{
		openConsole: func() (localConsole, error) { return nil, wantErr },
		bootstrap: func(ctx context.Context, cfg bootstrap.Config) (bootstrap.Credentials, error) {
			ev.add("bootstrap")
			return bootstrap.Credentials{}, nil
		},
	}
	o := &options{dest: destination{Host: "box", Port: 2022}, keepAlive: time.Second}
	err := connect(t.Context(), o, e, slog.New(slog.DiscardHandler))
	if !errors.Is(err, wantErr) {
		t.Fatalf("connect error = %v, want %v", err, wantErr)
	}
	if slices.Contains(ev.list(), "bootstrap") {
		t.Fatal("bootstrap ran after openConsole failed")
	}
}

// TestConnectDialFailure pins that a dial failure is wrapped with the
// address it tried, and that the console is closed without ever going raw.
func TestConnectDialFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ev := &events{}
		con := newFakeConsole(ev)
		wantErr := errors.New("connection refused")
		e := testEnv(ev, con, newFakeConn(ev, ""), nil)
		e.dial = func(ctx context.Context, d *etcp.Dialer, addr string, creds bootstrap.Credentials) (sessionConn, error) {
			ev.add("dial " + addr)
			return nil, wantErr
		}
		o := &options{dest: destination{Host: "box", Port: 2022}, keepAlive: time.Second}
		err := connect(t.Context(), o, e, slog.New(slog.DiscardHandler))
		if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "box:2022") {
			t.Fatalf("connect error = %v, want it to wrap %v and name box:2022", err, wantErr)
		}
		if slices.Contains(ev.list(), "raw") {
			t.Fatal("console went raw after a dial failure")
		}
		if !slices.Contains(ev.list(), "console closed") {
			t.Fatal("console not closed after a dial failure")
		}
	})
}

// TestConnectMakeRawFailure pins that a MakeRaw failure ends the session
// without ever calling session.Run.
func TestConnectMakeRawFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ev := &events{}
		con := newFakeConsole(ev)
		wantErr := errors.New("ioctl failed")
		con.makeRawErr = wantErr
		e := testEnv(ev, con, newFakeConn(ev, ""), nil)
		o := &options{dest: destination{Host: "box", Port: 2022}, keepAlive: time.Second}
		err := connect(t.Context(), o, e, slog.New(slog.DiscardHandler))
		if !errors.Is(err, wantErr) {
			t.Fatalf("connect error = %v, want %v", err, wantErr)
		}
		if !slices.Contains(ev.list(), "console closed") {
			t.Fatal("console not closed after a MakeRaw failure")
		}
	})
}

// TestConnectPanicDuringRunStillRestores pins that a panic reaching connect
// while raw restores the console before the panic is re-raised: the recover
// in connect's last defer exists to guarantee that ordering, not to hide the
// panic (spec: run.go connect doc comment).
func TestConnectPanicDuringRunStillRestores(t *testing.T) {
	ev := &events{}
	con := newFakeConsole(ev)
	con.sizePanic = "boom"
	e := testEnv(ev, con, newFakeConn(ev, ""), nil)
	o := &options{dest: destination{Host: "box", Port: 2022}, keepAlive: time.Second}

	func() {
		defer func() {
			r := recover()
			if r != "boom" {
				t.Fatalf("recovered %v, want %q", r, "boom")
			}
		}()
		_ = connect(t.Context(), o, e, slog.New(slog.DiscardHandler))
		t.Fatal("connect returned normally; want a panic")
	}()

	if !slices.Contains(ev.list(), "restored") {
		t.Fatal("console was not restored before the panic propagated")
	}
}

func TestExitCodeDetached(t *testing.T) {
	var stderr bytes.Buffer
	if code := exitCode(fmt.Errorf("ctrl+break: %w", session.ErrDetached), &stderr); code != 0 {
		t.Fatalf("exit code %d, want 0", code)
	}
	if !strings.Contains(stderr.String(), "Detached") {
		t.Fatalf("stderr %q", stderr.String())
	}
}

// cmd/et passes *etcp.Conn straight to session, which ends normally only on a
// ReadPacket error wrapping io.EOF. This pins the etcp side of that contract;
// the compile-time assertion in run.go pins the method set.
func TestEtcpConnIsTransport(t *testing.T) {
	if !errors.Is(etcp.ErrSessionEnded, io.EOF) {
		t.Fatal("etcp.ErrSessionEnded must wrap io.EOF: session.Run relies on it to end normally")
	}
	for _, err := range []error{etcp.ErrIntegrity, etcp.ErrVersion, etcp.ErrReplayExceeded} {
		if errors.Is(err, io.EOF) {
			t.Errorf("%v wraps io.EOF: a failure would end the session as a normal exit", err)
		}
	}
}
