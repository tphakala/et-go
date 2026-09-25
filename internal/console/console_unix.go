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
type Console struct {
	tty  *os.File
	base *term.State // terminal state when the console was opened

	mu      sync.Mutex
	restore func() error // set by MakeRaw, run by Close
}

// Open returns the console attached to stdin and stdout. It returns an error
// satisfying errors.Is(err, ErrNotTerminal) if either is not a terminal or
// the process has no controlling terminal.
//
// Open records the terminal state as the baseline that restore returns to.
// Call it before running anything that may leave the terminal in a changed
// mode (cmd/et runs ssh between Open and MakeRaw; an interrupted password
// prompt can leave echo off).
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
	c := &Console{tty: tty}
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

// MakeRaw switches the terminal to raw mode. The returned restore func
// returns the terminal to the state recorded by Open, not to whatever mode
// was current when MakeRaw ran. It is idempotent and safe to call from any
// goroutine; Close also calls it.
func (c *Console) MakeRaw() (restore func() error, err error) {
	err = c.control(func(fd int) error {
		_, rerr := term.MakeRaw(fd)
		return rerr
	})
	if err != nil {
		return nil, fmt.Errorf("console: make raw: %w", err)
	}
	restore = sync.OnceValue(func() error {
		if err := c.control(func(fd int) error { return term.Restore(fd, c.base) }); err != nil {
			return fmt.Errorf("console: restore: %w", err)
		}
		return nil
	})
	c.mu.Lock()
	c.restore = restore
	c.mu.Unlock()
	return restore, nil
}

// Read reads raw input bytes. A pending Read returns os.ErrClosed once Close
// is called (MEASURED on Linux against a pty in the package tests, not yet
// measured on darwin).
func (c *Console) Read(p []byte) (int, error) { return c.tty.Read(p) }

// Write writes remote output to the terminal.
func (c *Console) Write(p []byte) (int, error) { return c.tty.Write(p) }

// Size returns the current window size, including pixels when known.
func (c *Console) Size() (Size, error) {
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
// fall in the gap. Range over the result once.
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
			if !yield(sz) {
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
			if !yield(sz) {
				return
			}
		}
	}
}

// Close restores the terminal mode if MakeRaw was called, then closes the
// terminal, which unblocks a pending Read (MEASURED on Linux against a pty
// in the package tests, not yet measured on darwin).
func (c *Console) Close() error {
	c.mu.Lock()
	restore := c.restore
	c.mu.Unlock()
	var rerr error
	if restore != nil {
		rerr = restore()
	}
	return errors.Join(rerr, c.tty.Close())
}

// control runs f with the terminal's file descriptor.
func (c *Console) control(f func(fd int) error) error {
	return controlFile(c.tty, f)
}

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

func isTerminal(f *os.File) bool {
	var ok bool
	err := controlFile(f, func(fd int) error {
		ok = term.IsTerminal(fd)
		return nil
	})
	return err == nil && ok
}
