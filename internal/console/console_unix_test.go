//go:build unix

package console

import (
	"io/fs"
	"testing"

	"golang.org/x/sys/unix"
)

// TestNoTerminalToOpen pins which open failures mean there is no terminal:
// ENXIO, what opening /dev/tty gives a process without a controlling
// terminal, which the package tests cannot reach from a process that has
// one, and ENOENT; the rest are plain failures.
func TestNoTerminalToOpen(t *testing.T) {
	tests := []struct {
		errno unix.Errno
		want  bool
	}{
		{unix.ENXIO, true},
		{unix.ENOENT, true},
		{unix.EACCES, false},
		{unix.EISDIR, false},
		{unix.EMFILE, false},
	}
	for _, tt := range tests {
		t.Run(tt.errno.Error(), func(t *testing.T) {
			err := &fs.PathError{Op: "open", Path: "/dev/tty", Err: tt.errno}
			if got := noTerminalToOpen(err); got != tt.want {
				t.Fatalf("noTerminalToOpen(%v) = %v, want %v", err, got, tt.want)
			}
		})
	}
}
