//go:build unix

package console

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"os"
	"os/signal"
	"sync"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// Console is the controlling terminal, opened as /dev/tty.
//
// Opening /dev/tty creates an open file description of its own, so the
// non-blocking mode Go sets on it (which is what lets Close unblock a
// pending Read; MEASURED on Linux against a pty in the package tests, not
// yet measured on darwin) never leaks into the shell's stdin, even if et
// crashes. Fd is never called on the file: per the os.File.Fd docs its
// deadline methods would then stop working. All ioctls go through
// SyscallConn.
//
// Read and Write each have one owner goroutine: Read is not safe to call
// concurrently with itself, nor Write with itself; Read and Write may run
// concurrently with each other and with Close.
type Console struct {
	tty  *os.File
	base *term.State // terminal state when the console was opened

	// setState applies a terminal state. It is term.Restore, held in a
	// field so a test can count the calls restore makes.
	setState func(fd int, s *term.State) error

	// mu serialises MakeRaw, restore, Size and Close. Close holds it for
	// its whole body, so a second Close waits for the first to finish.
	mu     sync.Mutex
	closed bool // guarded by mu
}

// Open checks that stdin and stdout are terminals, then opens the
// controlling terminal (/dev/tty) as the console. It returns an error
// satisfying errors.Is(err, ErrNotTerminal) if either is not a terminal or
// the process has no controlling terminal.
//
// Open records the terminal state as the baseline that restore returns to.
// The intended caller opens the console before running anything that may
// leave it in a changed mode, such as ssh between Open and MakeRaw, where
// an interrupted password prompt can leave echo off.
func Open() (*Console, error) {
	return open("/dev/tty", os.Stdin, os.Stdout)
}

// open is Open with the terminal path and standard files as parameters, so
// tests can supply pipes and a missing path.
func open(ttyPath string, stdin, stdout *os.File) (*Console, error) {
	if !isTerminal(stdin) || !isTerminal(stdout) {
		return nil, ErrNotTerminal
	}
	tty, err := os.OpenFile(ttyPath, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: open %s: %w", ErrNotTerminal, ttyPath, err)
	}
	c, err := newConsole(tty)
	if err != nil {
		_ = tty.Close()
		return nil, err
	}
	return c, nil
}

// newConsole wraps an already open terminal file and records its current
// state as the baseline. Tests pass a pty slave.
func newConsole(tty *os.File) (*Console, error) {
	c := &Console{tty: tty, setState: term.Restore}
	err := c.control(func(fd int) error {
		var gerr error
		c.base, gerr = term.GetState(fd)
		return gerr
	})
	if err != nil {
		return nil, fmt.Errorf("%w: read terminal state: %w", ErrNotTerminal, err)
	}
	return c, nil
}

// MakeRaw switches the terminal to raw mode. It may be called again after
// restore; Close still returns the terminal to the Open baseline. After
// Close it returns an error satisfying errors.Is(err, os.ErrClosed) and
// leaves the terminal alone.
//
// The returned restore func returns the terminal to the state recorded by
// Open, not to whatever mode was current when MakeRaw ran. Every restore
// func does this whenever it runs, including one kept from an earlier
// MakeRaw, so running a stale one during a later raw session leaves raw
// mode. It reads the current state first and sets the baseline only when
// they differ, so it is repeatable and leaves a terminal already at the
// baseline untouched. It is safe to call from any goroutine. After Close it
// returns nil without touching the terminal, which Close already restored.
func (c *Console) MakeRaw() (restore func() error, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, fmt.Errorf("console: make raw: %w", os.ErrClosed)
	}
	err = c.control(func(fd int) error {
		_, rerr := term.MakeRaw(fd)
		return rerr
	})
	if err != nil {
		return nil, fmt.Errorf("console: make raw: %w", err)
	}
	return c.restore, nil
}

// restore is the func MakeRaw returns; see MakeRaw.
func (c *Console) restore() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	return c.restoreLocked()
}

// restoreLocked sets the Open baseline if the current state differs from
// it. Only setting the state can stop a background job (SIGTTOU; reading it
// cannot), so a terminal already at the baseline is left untouched. c.mu
// must be held.
func (c *Console) restoreLocked() error {
	err := c.control(func(fd int) error {
		cur, err := term.GetState(fd)
		if err != nil {
			return err
		}
		if *cur == *c.base {
			return nil
		}
		return c.setState(fd, c.base)
	})
	if err != nil {
		return fmt.Errorf("console: restore: %w", err)
	}
	return nil
}

// Read reads raw input bytes. A pending Read returns os.ErrClosed once Close
// is called (MEASURED on Linux against a pty in the package tests, not yet
// measured on darwin).
func (c *Console) Read(p []byte) (int, error) { return c.tty.Read(p) }

// Write writes remote output to the terminal. After Close it returns an
// error satisfying errors.Is(err, os.ErrClosed), which os.File reports for
// a closed file. A Write that runs while Close is in progress can still
// reach the terminal until Close closes the descriptor, possibly after the
// mode was restored. This differs from Windows, where Write writes nothing
// once Close has started.
func (c *Console) Write(p []byte) (int, error) { return c.tty.Write(p) }

// Size returns the current window size, including pixels when known. After
// Close it returns an error satisfying errors.Is(err, os.ErrClosed).
func (c *Console) Size() (Size, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return Size{}, fmt.Errorf("console: size: %w", os.ErrClosed)
	}
	var ws *unix.Winsize
	err := c.control(func(fd int) error {
		var gerr error
		ws, gerr = unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ)
		return gerr
	})
	if err != nil {
		return Size{}, fmt.Errorf("console: size: %w", err)
	}
	return Size{
		Rows:   int(ws.Row),
		Cols:   int(ws.Col),
		Width:  int(ws.Xpixel),
		Height: int(ws.Ypixel),
	}, nil
}

// Resizes yields the window size each time it changes, until ctx ends or
// the loop body stops. Changes are measured against the size when Resizes
// is called, which is not yielded itself; callers read it with Size. A
// change that happens before ranging starts is still caught: SIGWINCH
// registration and the first comparison against the call-time size both
// happen as soon as the returned sequence starts running, so no resize can
// fall in the gap. Nothing is yielded once ctx has ended, even a change
// made before it ended. Range over the result once.
func (c *Console) Resizes(ctx context.Context) iter.Seq[Size] {
	last, _ := c.Size()
	return func(yield func(Size) bool) {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, unix.SIGWINCH)
		defer signal.Stop(sig)

		// A resize between the Resizes call and Notify registering above
		// would otherwise be lost: SIGWINCH is ignored by default, so a
		// signal that fires in that gap never reaches sig. Check once,
		// right after registering, so such a change is still caught.
		if sz, err := c.Size(); err == nil && sz != last {
			last = sz
			if ctx.Err() != nil || !yield(sz) {
				return
			}
		}

		for {
			select {
			case <-ctx.Done():
				return
			case <-sig:
			}
			sz, err := c.Size()
			if err != nil || sz == last {
				continue
			}
			last = sz
			// select picks at random when a signal and the cancellation
			// are both ready, so check ctx again before yielding.
			if ctx.Err() != nil || !yield(sz) {
				return
			}
		}
	}
}

// Close returns the terminal to the state recorded by Open whether or not
// MakeRaw was called, so a mode an interrupted prompt left behind is undone
// too, then closes the terminal. Like a restore func, it sets the baseline
// only when the current state differs from it. Closing the terminal
// unblocks a pending Read, which returns os.ErrClosed (MEASURED on Linux
// against a pty in the package tests, not yet measured on darwin).
//
// Close is idempotent. A second Close that runs while the first is still in
// progress waits for the first to finish, then returns nil; a Close after
// that returns nil at once. Only the first Close reports errors.
//
// Close does not discard typeahead the reader has not consumed; it stays
// queued for the next reader of the terminal. OpenSSH does the same: its
// leave_raw_mode (sshtty.c) restores with tcsetattr TCSADRAIN, which does
// not flush input. On Windows, Close flushes the input buffer instead.
func (c *Console) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	return errors.Join(c.restoreLocked(), c.tty.Close())
}

// control runs f with the terminal's file descriptor.
func (c *Console) control(f func(fd int) error) error {
	return controlFile(c.tty, f)
}

// controlFile runs f with file's descriptor through SyscallConn, never Fd.
func controlFile(file *os.File, f func(fd int) error) error {
	rc, err := file.SyscallConn()
	if err != nil {
		return err
	}
	var ferr error
	if err := rc.Control(func(fd uintptr) { ferr = f(int(fd)) }); err != nil {
		return err
	}
	return ferr
}

// isTerminal reports whether f is a terminal; an error reading it counts as no.
func isTerminal(f *os.File) bool {
	var ok bool
	err := controlFile(f, func(fd int) error {
		ok = term.IsTerminal(fd)
		return nil
	})
	return err == nil && ok
}
