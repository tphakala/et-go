package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// The test binary doubles as a fake ssh. When fakeSSHModeEnv is set, TestMain
// runs runFakeSSH instead of the tests, so Config.SSH = os.Args[0] makes Run
// execute this same binary as "ssh". It needs no shell scripts, so the same
// tests run in the Windows CI job.
const (
	fakeSSHModeEnv     = "ET_FAKE_SSH_MODE"     // which behaviour the fake shows
	fakeSSHArgvEnv     = "ET_FAKE_SSH_ARGV"     // file the fake writes its argv to, as JSON
	fakeSSHReadyEnv    = "ET_FAKE_SSH_READY"    // "trap-int": FIFO opened once the handler is installed
	fakeSSHSignaledEnv = "ET_FAKE_SSH_SIGNALED" // "trap-int": file created when os.Interrupt arrives
)

func TestMain(m *testing.M) {
	if mode := os.Getenv(fakeSSHModeEnv); mode != "" {
		os.Exit(runFakeSSH(mode, os.Args[1:]))
	}
	os.Exit(m.Run())
}

// runFakeSSH records its arguments and then behaves like ssh running
// etterminal in the given mode. It returns the exit status.
func runFakeSSH(mode string, args []string) int {
	if path := os.Getenv(fakeSSHArgvEnv); path != "" {
		data, err := json.Marshal(args)
		if err != nil {
			return 90
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			return 91
		}
	}
	idpasskey := "IDPASSKEY:" + testID + "/" + testPasskey + "\n"
	switch mode {
	case "ok":
		fmt.Print("Last login: Thu Sep 24 21:00:00 2026\nWelcome to the server\n" + idpasskey)
		return 0
	case "ok-exit1":
		// etterminal printed its credentials but ssh itself exited non-zero.
		fmt.Print(idpasskey)
		return 1
	case "echo-sent":
		// A server that does not regenerate: it echoes back what it was sent.
		m := regexp.MustCompile(`echo '([A-Za-z0-9]{16})/([A-Za-z0-9]{32})_`).FindStringSubmatch(args[len(args)-1])
		if m == nil {
			return 92
		}
		fmt.Print("IDPASSKEY:" + m[1] + "/" + m[2] + "\n")
		return 0
	case "notfound":
		// What bash prints when etterminal is not installed (it goes to stderr).
		fmt.Fprintln(os.Stderr, "bash: line 1: etterminal: command not found")
		return 127
	case "authfail":
		fmt.Fprintln(os.Stderr, "alice@example.test: Permission denied (publickey).")
		return 255
	case "noise":
		fmt.Print("Welcome to the server\n")
		return 0
	case "malformed":
		fmt.Print("IDPASSKEY:abcd\n")
		return 0
	case "exit1":
		// A remote failure with some other status and no output.
		return 1
	case "linger":
		// ssh exits 0 without credentials while a descendant still holds its
		// stdout, so Run's WaitDelay has to close the pipe.
		hold := exec.Command(os.Args[0])
		hold.Env = append(os.Environ(), fakeSSHModeEnv+"=hold", fakeSSHArgvEnv+"=")
		hold.Stdout = os.Stdout
		if err := hold.Start(); err != nil {
			return 96
		}
		return 0
	case "hold":
		// Keeps writing to the inherited stdout and exits on the first failed
		// write, which comes as soon as the reader closes the pipe; the loop
		// bound stops it even if nobody does.
		for range 50 {
			if _, err := os.Stdout.WriteString("."); err != nil {
				return 0
			}
			time.Sleep(100 * time.Millisecond)
		}
		return 0
	case "hang":
		time.Sleep(time.Hour)
		return 0
	case "trap-int":
		// Survives os.Interrupt and records that it arrived, so only the
		// WaitDelay kill can end it. Opening the FIFO tells the test that the
		// handler is installed.
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt)
		ready, err := os.OpenFile(os.Getenv(fakeSSHReadyEnv), os.O_WRONLY, 0)
		if err != nil {
			return 94
		}
		_ = ready.Close()
		<-sig
		if err := os.WriteFile(os.Getenv(fakeSSHSignaledEnv), nil, 0o600); err != nil {
			return 95
		}
		// Longer than the test's 15s bound, so only the WaitDelay kill ends it
		// in time, but short enough that a regression which never kills it
		// does not hold go test's output pipe open for long.
		time.Sleep(30 * time.Second)
		return 0
	}
	return 93
}

// useFakeSSH points the fake at mode and returns a Config that runs it and the
// path its argv will be written to.
func useFakeSSH(t *testing.T, mode string) (cfg Config, argvPath string) {
	t.Helper()
	argvPath = filepath.Join(t.TempDir(), "argv.json")
	t.Setenv(fakeSSHModeEnv, mode)
	t.Setenv(fakeSSHArgvEnv, argvPath)
	// Under -race the fake child otherwise pauses for the race runtime's
	// default atexit_sleep_ms (1s) on every exit with status 0.
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	cfg = Config{
		Destination: "example.test",
		User:        "alice",
		SSHOptions:  []string{"BatchMode=yes"},
		SSH:         os.Args[0],
	}
	return cfg, argvPath
}

func readArgv(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("fake ssh did not record its argv: %v", err)
	}
	var args []string
	if err := json.Unmarshal(data, &args); err != nil {
		t.Fatalf("decode argv: %v", err)
	}
	return args
}

func TestRunSuccess(t *testing.T) {
	cfg, argvPath := useFakeSSH(t, "ok")
	var logs bytes.Buffer
	cfg.Logger = slog.New(slog.NewJSONHandler(&logs, nil))

	got, err := Run(t.Context(), cfg)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got.ID != testID || got.Passkey() != testPasskey {
		t.Fatalf("Run() = %v with passkey match %v, want id %s and the fake's passkey", got, got.Passkey() == testPasskey, testID)
	}
	// The server regenerated the credentials, so there is nothing to warn about.
	if logs.Len() != 0 {
		t.Fatalf("Run() logged on a normal start: %s", logs.String())
	}

	args := readArgv(t, argvPath)
	if want := []string{"-oBatchMode=yes", "alice@example.test"}; len(args) != 3 || !slices.Equal(args[:2], want) {
		t.Fatalf("ssh argv = %q, want %q followed by the remote command", args, want)
	}
	remote := regexp.MustCompile(`^echo 'XXX[A-Z2-7]{13}/[A-Z2-7]{32}_xterm-256color' \| etterminal --verbose=0$`)
	if !remote.MatchString(args[2]) {
		t.Fatalf("remote command = %q, want it to match %s", args[2], remote)
	}
}

func TestRunIgnoresSSHExitStatusWhenCredentialsArrive(t *testing.T) {
	cfg, _ := useFakeSSH(t, "ok-exit1")
	if _, err := Run(t.Context(), cfg); err != nil {
		t.Fatalf("Run() error = %v, want success despite ssh exit status 1", err)
	}
}

func TestRunWarnsWhenServerDoesNotRegenerate(t *testing.T) {
	cfg, argvPath := useFakeSSH(t, "echo-sent")
	var logs bytes.Buffer
	cfg.Logger = slog.New(slog.NewJSONHandler(&logs, nil))

	got, err := Run(t.Context(), cfg)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	sent := regexp.MustCompile(`^echo '([A-Z2-7]{16})/`).FindStringSubmatch(readArgv(t, argvPath)[2])
	if len(sent) != 2 || got.ID != sent[1] {
		t.Fatalf("Run() id = %q, want the placeholder sent in the remote command (%q)", got.ID, sent)
	}
	if !strings.Contains(logs.String(), "did not regenerate") {
		t.Fatalf("no regeneration warning logged; logs: %s", logs.String())
	}
	if strings.Contains(logs.String(), got.Passkey()) {
		t.Fatalf("warning leaks the passkey: %s", logs.String())
	}
}

// TestRunNilLoggerOnWarningPath pins the documented nil-Logger default on the
// only path that logs.
func TestRunNilLoggerOnWarningPath(t *testing.T) {
	cfg, _ := useFakeSSH(t, "echo-sent")
	cfg.Logger = nil
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Run() with a nil Logger panicked on the no-regeneration warning: %v", r)
		}
	}()
	if _, err := Run(t.Context(), cfg); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
}

func TestRunFailures(t *testing.T) {
	const (
		missingHint = "--terminal-path"
		sshHint     = "ssh could not connect or authenticate"
	)
	tests := []struct {
		mode     string
		contains []string
		absent   []string // hints that belong to other exit statuses
	}{
		// etterminal missing on the server.
		{"notfound", []string{"etterminal", "status 127", missingHint}, []string{sshHint}},
		{"authfail", []string{"status 255", sshHint}, []string{missingHint}},
		{"noise", []string{"status 0", `"Welcome to the server"`}, []string{missingHint, sshHint}},
		{"malformed", []string{"malformed id", "status 0"}, []string{missingHint, sshHint}},
		{"exit1", []string{"status 1"}, []string{missingHint, sshHint, "; output:"}},
		{"linger", []string{"status 0"}, []string{missingHint, sshHint}},
	}
	for _, tt := range tests {
		t.Run(tt.mode, func(t *testing.T) {
			cfg, _ := useFakeSSH(t, tt.mode)
			_, err := Run(t.Context(), cfg)
			if !errors.Is(err, ErrNoCredentials) {
				t.Fatalf("Run() error = %v, want ErrNoCredentials", err)
			}
			for _, s := range tt.contains {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("error %q does not mention %q", err, s)
				}
			}
			for _, s := range tt.absent {
				if strings.Contains(err.Error(), s) {
					t.Errorf("error %q mentions %q, which does not apply", err, s)
				}
			}
			// "status 1" alone would also match "status 127"; exit1 prints no
			// output and gets no hint, so its status ends the message.
			if tt.mode == "exit1" && !strings.HasSuffix(err.Error(), "status 1") {
				t.Errorf("error %q does not end with the exit status", err)
			}
			// The marker ends in ':', so printing it bare before ": ssh exited"
			// would render a confusing "IDPASSKEY::".
			if strings.Contains(err.Error(), "::") {
				t.Errorf("error %q contains a doubled colon", err)
			}
		})
	}
}

func TestRunCancel(t *testing.T) {
	cfg, _ := useFakeSSH(t, "hang")
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := Run(ctx, cfg)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run() error = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("Run() took %v after cancel, want it bounded by waitDelay", elapsed)
	}
}

func TestRunInvalidConfigDoesNotStartSSH(t *testing.T) {
	cfg, argvPath := useFakeSSH(t, "ok")
	cfg.Term = "xterm'; id; '"
	if _, err := Run(t.Context(), cfg); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("Run() error = %v, want ErrInvalidConfig", err)
	}
	if _, err := os.Stat(argvPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ssh ran despite an invalid config (argv file stat: %v)", err)
	}
}

func TestRunMissingSSH(t *testing.T) {
	cfg := Config{Destination: "example.test", SSH: filepath.Join(t.TempDir(), "no-such-ssh")}
	_, err := Run(t.Context(), cfg)
	if err == nil || errors.Is(err, ErrNoCredentials) || !strings.Contains(err.Error(), "no-such-ssh") {
		t.Fatalf("Run() error = %v, want a start error naming the executable", err)
	}
}

func TestRunSSHNotOnPath(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := Run(t.Context(), Config{Destination: "example.test"})
	if err == nil || errors.Is(err, ErrNoCredentials) || !strings.Contains(err.Error(), "ssh not found on PATH") {
		t.Fatalf("Run() error = %v, want an ssh-not-found error", err)
	}
}

func TestExcerptCutsAtMarker(t *testing.T) {
	out := []byte("banner\nIDPASSKEY:" + testID + "/" + testPasskey[:10])
	if got := excerpt(out); got != "banner" {
		t.Fatalf("excerpt() = %q, want %q", got, "banner")
	}
	// The end of the output is what explains a failure, so the tail is kept.
	long := []byte("HEAD" + strings.Repeat("x", 2*excerptLen) + "TAIL")
	got := excerpt(long)
	if len(got) != excerptLen+len("...") || !strings.HasPrefix(got, "...") || !strings.HasSuffix(got, "TAIL") || strings.Contains(got, "HEAD") {
		t.Fatalf("excerpt() of long output = %q (length %d), want the last %d bytes after a ... prefix", got, len(got), excerptLen)
	}
}

func TestRunFindsSSHOnPath(t *testing.T) {
	cfg, _ := useFakeSSH(t, "ok")
	dir := t.TempDir()
	name := "ssh"
	if runtime.GOOS == "windows" {
		name = "ssh.exe"
	}
	self, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), self, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	cfg.SSH = ""

	got, err := Run(t.Context(), cfg)
	if err != nil || got.ID != testID {
		t.Fatalf("Run() = %v, %v; want the fake's credentials through the ssh found on PATH", got, err)
	}
}

func TestCappedBuffer(t *testing.T) {
	c := &cappedBuffer{max: 4}
	for _, s := range []string{"ab", "cdef", "gh"} {
		if n, err := c.Write([]byte(s)); n != len(s) || err != nil {
			t.Fatalf("Write(%q) = %d, %v", s, n, err)
		}
	}
	if got := string(c.Bytes()); got != "abcd" {
		t.Fatalf("Bytes() = %q, want %q", got, "abcd")
	}
}
