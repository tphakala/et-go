package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
)

// newLogger returns the client's logger. Without -v or --log-file it
// discards everything; nothing is ever logged to the terminal, which is in
// raw mode while the session runs. -v alone logs at debug level to the
// default path; --log-file alone logs at info level.
func newLogger(verbose bool, path string) (*slog.Logger, func() error, error) {
	if !verbose && path == "" {
		return slog.New(slog.DiscardHandler), func() error { return nil }, nil
	}
	if path == "" {
		p, err := defaultLogPath()
		if err != nil {
			return nil, nil, err
		}
		path = p
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, nil, fmt.Errorf("create log directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("open log file: %w", err)
	}
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{Level: level})), f.Close, nil
}

// defaultLogPath is %LOCALAPPDATA%\et-go\et.log on Windows and
// $XDG_STATE_HOME/et-go/et.log (default ~/.local/state) elsewhere.
func defaultLogPath() (string, error) {
	if runtime.GOOS == "windows" {
		dir := os.Getenv("LOCALAPPDATA")
		if dir == "" {
			return "", errors.New("LOCALAPPDATA is not set; use --log-file")
		}
		return filepath.Join(dir, "et-go", "et.log"), nil
	}
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "et-go", "et.log"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find home directory: %w", err)
	}
	return filepath.Join(home, ".local", "state", "et-go", "et.log"), nil
}
