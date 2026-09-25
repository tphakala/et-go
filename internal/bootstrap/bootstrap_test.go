package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
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
	fakeSSHModeEnv = "ET_FAKE_SSH_MODE" // which behaviour the fake shows
	fakeSSHArgvEnv = "ET_FAKE_SSH_ARGV" // file the fake writes its argv to, as JSON
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
	case "hang":
		time.Sleep(time.Hour)
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

	got, err := Run(t.Context(), cfg)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if want := (Credentials{ID: testID, Passkey: testPasskey}); got != want {
		t.Fatalf("Run() = %#v, want %#v", got, want)
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
	cfg, _ := useFakeSSH(t, "echo-sent")
	var logs bytes.Buffer
	cfg.Logger = slog.New(slog.NewJSONHandler(&logs, nil))

	got, err := Run(t.Context(), cfg)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !strings.HasPrefix(got.ID, regeneratePrefix) {
		t.Fatalf("Run() id = %q, want the echoed placeholder", got.ID)
	}
	if !strings.Contains(logs.String(), "did not regenerate") {
		t.Fatalf("no regeneration warning logged; logs: %s", logs.String())
	}
	if strings.Contains(logs.String(), got.Passkey) {
		t.Fatalf("warning leaks the passkey: %s", logs.String())
	}
}

func TestRunFailures(t *testing.T) {
	tests := []struct {
		mode     string
		contains []string
	}{
		// Review Focus 1: etterminal missing on the server.
		{"notfound", []string{"etterminal", "status 127", "--terminal-path"}},
		{"authfail", []string{"status 255", "ssh could not connect or authenticate"}},
		{"noise", []string{"status 0", `"Welcome to the server"`}},
		{"malformed", []string{"malformed id", "status 0"}},
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
	long := bytes.Repeat([]byte("x"), 2*excerptLen)
	if got := excerpt(long); len(got) != excerptLen+len("...") || !strings.HasPrefix(got, "...") {
		t.Fatalf("excerpt() of long output has length %d, want %d with a ... prefix", len(got), excerptLen+3)
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
