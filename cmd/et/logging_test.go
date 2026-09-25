package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/tphakala/et-go/internal/bootstrap"
)

func TestDefaultLogPath(t *testing.T) {
	dir := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("LOCALAPPDATA", dir)
	} else {
		t.Setenv("XDG_STATE_HOME", dir)
	}
	got, err := defaultLogPath()
	if want := filepath.Join(dir, "et-go", "et.log"); err != nil || got != want {
		t.Fatalf("defaultLogPath() = %q, %v; want %q", got, err, want)
	}
}

func TestNewLoggerWritesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "et.log")
	log, closeLog, err := newLogger(false, path)
	if err != nil {
		t.Fatal(err)
	}
	log.Info("hello", "credentials", bootstrap.NewCredentials("id", "topsecretpasskey"))
	if err := closeLog(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte("hello")) || bytes.Contains(b, []byte("topsecretpasskey")) {
		t.Fatalf("log = %q", b)
	}
}

// TestNewLoggerVerboseUsesDefaultPath pins that -v alone (no --log-file)
// resolves the path through defaultLogPath and logs at debug level.
func TestNewLoggerVerboseUsesDefaultPath(t *testing.T) {
	dir := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("LOCALAPPDATA", dir)
	} else {
		t.Setenv("XDG_STATE_HOME", dir)
	}
	log, closeLog, err := newLogger(true, "")
	if err != nil {
		t.Fatal(err)
	}
	log.Debug("debug marker")
	if err := closeLog(); err != nil {
		t.Fatal(err)
	}
	want, err := defaultLogPath()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(want)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte("debug marker")) {
		t.Fatalf("verbose logger dropped a debug record: log = %q", b)
	}
}

// TestNewLoggerDefaultPathFailure pins that a defaultLogPath failure surfaces
// through newLogger instead of being swallowed.
func TestNewLoggerDefaultPathFailure(t *testing.T) {
	var want string
	if runtime.GOOS == "windows" {
		t.Setenv("LOCALAPPDATA", "")
		want = "LOCALAPPDATA is not set"
	} else {
		t.Setenv("XDG_STATE_HOME", "")
		t.Setenv("HOME", "")
		want = "find home directory"
	}
	_, _, err := newLogger(true, "")
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("newLogger with an unresolvable default path: err = %v, want it to contain %q", err, want)
	}
}

// TestNewLoggerMkdirAllFailure pins that a log directory that cannot be
// created surfaces as an error naming the failure.
func TestNewLoggerMkdirAllFailure(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(blocker, "sub", "et.log")
	if _, _, err := newLogger(false, path); err == nil || !strings.Contains(err.Error(), "create log directory") {
		t.Fatalf("newLogger(%q) error = %v, want a create-log-directory error", path, err)
	}
}

// TestNewLoggerOpenFileFailure pins that a log path that cannot be opened as
// a file surfaces as an error naming the failure.
func TestNewLoggerOpenFileFailure(t *testing.T) {
	path := t.TempDir() // a directory, not a file
	if _, _, err := newLogger(false, path); err == nil || !strings.Contains(err.Error(), "open log file") {
		t.Fatalf("newLogger(%q) error = %v, want an open-log-file error", path, err)
	}
}

// TestDefaultLogPathHomeFallback pins the $HOME fallback used when
// XDG_STATE_HOME is unset (spec: defaultLogPath doc comment).
func TestDefaultLogPathHomeFallback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("LOCALAPPDATA governs the path on windows; see TestDefaultLogPath")
	}
	home := t.TempDir()
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", home)
	got, err := defaultLogPath()
	if want := filepath.Join(home, ".local", "state", "et-go", "et.log"); err != nil || got != want {
		t.Fatalf("defaultLogPath() = %q, %v; want %q", got, err, want)
	}
}

// TestDefaultLogPathNoHome pins that defaultLogPath reports a clear error
// when neither XDG_STATE_HOME nor HOME is set.
func TestDefaultLogPathNoHome(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("LOCALAPPDATA governs the path on windows; see TestDefaultLogPath")
	}
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "")
	if _, err := defaultLogPath(); err == nil {
		t.Fatal("defaultLogPath with no XDG_STATE_HOME and no HOME: want error, got nil")
	}
}
