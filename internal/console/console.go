// Package console is the local console or terminal that et puts into raw mode:
// raw VT input as UTF-8 bytes, remote output written back, the window size,
// and resize events. Each platform implements the same concrete Console type
// (console_unix.go, console_windows.go).
//
// On Unix, et must run as the foreground job of its controlling terminal.
// A background job that reads or changes the terminal is stopped by job
// control (SIGTTIN, SIGTTOU) until it is brought to the foreground.
package console

import (
	"context"
	"errors"
	"os"
)

// Size is the terminal size in character cells, plus pixels when the
// platform reports them (0 otherwise).
type Size struct {
	Rows, Cols    int
	Width, Height int
}

// ErrNotTerminal is returned by Open when stdin or stdout is not a terminal,
// or when the process has no controlling terminal to open.
var ErrNotTerminal = errors.New("console: stdin or stdout is not a terminal")

// resizeCheck reads the size once and yields it if it changed since *last.
// It reports false when the Console is closed, when ctx has ended with a
// changed size pending, or when yield stops; an unchanged size or any other
// Size error reports true, even after ctx ended, and the caller's select on
// ctx ends the loop. Checking ctx before yielding keeps a change from being
// yielded after cancellation, whether or not a select raced it.
func resizeCheck(ctx context.Context, size func() (Size, error), last *Size, yield func(Size) bool) bool {
	sz, err := size()
	if errors.Is(err, os.ErrClosed) {
		return false
	}
	if err != nil || sz == *last {
		return true
	}
	*last = sz
	return ctx.Err() == nil && yield(sz)
}
