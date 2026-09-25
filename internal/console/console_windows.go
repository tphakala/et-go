package console

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/windows"
)

const (
	// readUnits is the UTF-16 buffer size for one ReadConsoleW call.
	readUnits = 4096
	// writeUnits caps one WriteConsoleW call: a write larger than the
	// available heap fails with ERROR_NOT_ENOUGH_MEMORY (Microsoft docs,
	// WriteConsole, nNumberOfCharsToWrite).
	writeUnits = 8192
	// resizePoll is how often Resizes checks the window size.
	resizePoll = 200 * time.Millisecond
	// wakeEvery is how often Close re-injects the wake record while the
	// reader has not returned yet.
	wakeEvery = 100 * time.Millisecond
	// closeWait bounds how long Close waits for the reader to return.
	closeWait = time.Second
)

// Console is the Windows console attached to the process.
//
// Read and Write each have one owner goroutine: Read is not safe to call
// concurrently with itself, nor Write with itself; Read and Write may run
// concurrently with each other and with Close.
type Console struct {
	in, out         windows.Handle
	inBase, outBase uint32 // console modes when the console was opened

	mu      sync.Mutex
	restore func() error // set by MakeRaw, run by Close
	closing bool         // guarded by mu
	reading bool         // guarded by mu: a ReadConsoleW call is in flight

	exited chan struct{} // receives once when a reader returns after Close

	// Reader-owned state.
	dec     utf16Decoder
	units   []uint16
	pending []byte

	// Writer-owned state.
	enc utf8Encoder
	buf []uint16
}

// Open returns the console attached to stdin and stdout, or ErrNotTerminal
// if either is not a console.
//
// Open records the console modes as the baseline that restore returns to.
// Call it before running anything that may leave the console in a changed
// mode (cmd/et runs ssh.exe between Open and MakeRaw; an interrupted
// password prompt can leave echo off).
func Open() (*Console, error) {
	in, err := windows.GetStdHandle(windows.STD_INPUT_HANDLE)
	if err != nil {
		return nil, fmt.Errorf("console: stdin handle: %w", err)
	}
	out, err := windows.GetStdHandle(windows.STD_OUTPUT_HANDLE)
	if err != nil {
		return nil, fmt.Errorf("console: stdout handle: %w", err)
	}
	c := &Console{
		in:     in,
		out:    out,
		exited: make(chan struct{}, 1),
		units:  make([]uint16, readUnits),
	}
	if windows.GetConsoleMode(in, &c.inBase) != nil || windows.GetConsoleMode(out, &c.outBase) != nil {
		return nil, ErrNotTerminal
	}
	return c, nil
}

// MakeRaw switches the console to raw VT input and VT output processing,
// starting from the modes recorded by Open. Code pages are not touched: Read
// and Write use the UTF-16 console APIs, which bypass them. The returned
// restore func returns to the Open baseline, not to whatever mode was
// current when MakeRaw ran. It is idempotent and safe to call from any
// goroutine; Close also calls it.
func (c *Console) MakeRaw() (restore func() error, err error) {
	// &^ clears only the listed flags, so ENABLE_EXTENDED_FLAGS and
	// ENABLE_QUICK_EDIT_MODE keep whatever GetConsoleMode reported and Quick
	// Edit stays as the user set it (MEASURED on win11-qa ConPTY: input mode
	// 0x1f7 became 0x3f0, both flags still set).
	rawIn := c.inBase&^(windows.ENABLE_PROCESSED_INPUT|windows.ENABLE_LINE_INPUT|
		windows.ENABLE_ECHO_INPUT|windows.ENABLE_WINDOW_INPUT) |
		windows.ENABLE_VIRTUAL_TERMINAL_INPUT
	if err := windows.SetConsoleMode(c.in, rawIn); err != nil {
		return nil, fmt.Errorf("console: set input mode: %w", err)
	}

	rawOut := c.outBase | windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING |
		windows.ENABLE_PROCESSED_OUTPUT | windows.ENABLE_WRAP_AT_EOL_OUTPUT
	if err := windows.SetConsoleMode(c.out, rawOut|windows.DISABLE_NEWLINE_AUTO_RETURN); err != nil {
		// If the host rejects DISABLE_NEWLINE_AUTO_RETURN, run without it.
		if err := windows.SetConsoleMode(c.out, rawOut); err != nil {
			_ = windows.SetConsoleMode(c.in, c.inBase)
			return nil, fmt.Errorf("console: set output mode: %w", err)
		}
	}

	restore = sync.OnceValue(func() error {
		err := errors.Join(
			windows.SetConsoleMode(c.in, c.inBase),
			windows.SetConsoleMode(c.out, c.outBase),
		)
		if err != nil {
			return fmt.Errorf("console: restore: %w", err)
		}
		return nil
	})
	c.mu.Lock()
	c.restore = restore
	c.mu.Unlock()
	return restore, nil
}

// Read reads raw VT input as UTF-8. Once Close has run, Read returns
// os.ErrClosed, including a Read that was blocked and any bytes still
// buffered from an earlier console read.
func (c *Console) Read(p []byte) (int, error) {
	for {
		c.mu.Lock()
		if c.closing {
			c.mu.Unlock()
			return 0, os.ErrClosed
		}
		if len(c.pending) > 0 {
			c.mu.Unlock()
			break
		}
		c.reading = true
		c.mu.Unlock()

		var n uint32
		err := windows.ReadConsole(c.in, &c.units[0], uint32(len(c.units)), &n, nil)

		c.mu.Lock()
		c.reading = false
		closing := c.closing
		c.mu.Unlock()
		if closing {
			select {
			case c.exited <- struct{}{}:
			default:
			}
			return 0, os.ErrClosed
		}
		if err != nil {
			return 0, fmt.Errorf("console: read: %w", err)
		}
		c.pending = c.dec.append(c.pending[:0], c.units[:n])
	}
	n := copy(p, c.pending)
	c.pending = c.pending[n:]
	return n, nil
}

// Write writes remote output, at most writeUnits UTF-16 units per console
// call and never splitting a surrogate pair between calls. An incomplete
// UTF-8 sequence at the end of p is held until the next Write completes it.
//
// On a console error Write reports 0 bytes written even if earlier chunks
// reached the screen: the UTF-16 units are not mapped back to input bytes.
// io.Writer allows any n < len(p) with a non-nil error, and callers treat a
// console write error as fatal.
func (c *Console) Write(p []byte) (int, error) {
	c.buf = c.enc.append(c.buf[:0], p)
	for units := c.buf; len(units) > 0; {
		chunk := units[:chunkLen(units, writeUnits)]
		var n uint32
		if err := writeConsole(c.out, &chunk[0], uint32(len(chunk)), &n, nil); err != nil {
			return 0, fmt.Errorf("console: write: %w", err)
		}
		if n == 0 {
			return 0, io.ErrShortWrite
		}
		units = units[n:]
	}
	return len(p), nil
}

// Size returns the visible window size in character cells. Windows does not
// report pixel sizes, so Width and Height are 0.
func (c *Console) Size() (Size, error) {
	var info windows.ConsoleScreenBufferInfo
	if err := windows.GetConsoleScreenBufferInfo(c.out, &info); err != nil {
		return Size{}, fmt.Errorf("console: size: %w", err)
	}
	w := info.Window
	return Size{Rows: int(w.Bottom-w.Top) + 1, Cols: int(w.Right-w.Left) + 1}, nil
}

// Resizes yields the window size each time it changes, until ctx ends or
// the loop body stops. Changes are measured against the size when Resizes
// is called, which is not yielded itself; callers read it with Size. The
// window is polled every 200 ms: window resize events are always filtered
// by ReadConsole (Microsoft docs, SetConsoleMode remarks). Range over the
// result once.
func (c *Console) Resizes(ctx context.Context) iter.Seq[Size] {
	last, _ := c.Size()
	return func(yield func(Size) bool) {
		tick := time.NewTicker(resizePoll)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
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

// Close restores the console mode if MakeRaw was called and unblocks a
// pending Read, which then returns os.ErrClosed. The handles are the
// process's standard handles and stay open. Close is idempotent: later
// calls return nil at once. A second Close that runs while the first is
// still in progress also returns nil at once, possibly before the first
// has flushed the input and restored the mode.
//
// A blocked ReadConsoleW is woken by injecting an Enter key-down record;
// the reader sees the closing flag and discards what it read. Enter is
// used because in line mode ReadConsole "returns only when a carriage
// return character is read" (Microsoft docs, SetConsoleMode,
// ENABLE_LINE_INPUT), which a Read meets after restore ran before Close or
// when MakeRaw was never called. MEASURED on win11-qa under ConPTY (ssh
// -tt), 2026-09-25: the record wakes a Read in raw mode and in line mode,
// and no input events remain after Close; a space record left a line-mode
// Read blocked. In line mode with echo on, the console echoes the carriage
// return as a line break on screen.
//
// The record is re-injected every 100 ms until the reader returns (or 1 s
// passes) because the console's single input buffer can be shared by any
// number of processes (Microsoft docs, Consoles), so another process
// attached to the console may consume a record before this reader does.
// The input buffer is flushed only after that, so no wake record or unread
// typeahead reaches the parent shell.
func (c *Console) Close() error {
	c.mu.Lock()
	if c.closing {
		c.mu.Unlock()
		return nil
	}
	c.closing = true
	reading := c.reading
	restore := c.restore
	c.mu.Unlock()

	var errs []error
	if reading {
		errs = append(errs, c.wakeAndWait())
	}
	if err := windows.FlushConsoleInputBuffer(c.in); err != nil {
		errs = append(errs, fmt.Errorf("console: flush input: %w", err))
	}
	if restore != nil {
		errs = append(errs, restore())
	}
	return errors.Join(errs...)
}

// wakeAndWait injects wake records until the reader reports that it
// returned, or closeWait passes.
func (c *Console) wakeAndWait() error {
	tick := time.NewTicker(wakeEvery)
	defer tick.Stop()
	deadline := time.After(closeWait)
	for {
		if err := c.wakeReader(); err != nil {
			return err
		}
		select {
		case <-c.exited:
			return nil
		case <-tick.C:
		case <-deadline:
			return errors.New("console: reader did not return after close")
		}
	}
}

// wakeReader injects an Enter key-down so a pending ReadConsoleW returns. A
// carriage return completes the read in raw mode and in line mode alike.
func (c *Console) wakeReader() error {
	rec := inputRecord{
		EventType: windows.KEY_EVENT,
		Event: keyEventRecord{
			KeyDown:        1,
			RepeatCount:    1,
			VirtualKeyCode: 0x0D, // VK_RETURN
			// MapVirtualKeyW(VK_RETURN, MAPVK_VK_TO_VSC) (MEASURED on
			// win11-qa, 2026-09-25).
			VirtualScanCode: 0x1C,
			UnicodeChar:     '\r',
		},
	}
	if err := writeConsoleInput(c.in, []inputRecord{rec}); err != nil {
		return fmt.Errorf("console: wake reader: %w", err)
	}
	return nil
}

var (
	breakFunc   atomic.Pointer[func()]
	handlerOnce sync.Once
)

// OnBreak registers f to be called when the user presses Ctrl+Break, which
// Windows still raises as a control event while processed input is off
// (Ctrl+C arrives as a byte instead; Microsoft docs, CTRL+C and CTRL+BREAK
// Signals: "CTRL+BREAK is always treated as a signal"). Only the most
// recent f is kept; OnBreak(nil) unregisters it, and Ctrl+Break then has
// its default effect of ending the process. If the handler cannot be
// installed, Ctrl+Break also keeps its default effect.
func OnBreak(f func()) {
	if f == nil {
		breakFunc.Store(nil)
		return
	}
	breakFunc.Store(&f)
	handlerOnce.Do(func() {
		_ = setConsoleCtrlHandler(windows.NewCallback(ctrlHandler), true)
	})
}

// ctrlHandler handles CTRL_BREAK_EVENT when a function is registered, and
// leaves every other event (Ctrl+C, close, logoff, shutdown) to the next
// handler. It runs on a thread Windows creates.
func ctrlHandler(ctrlType uint32) uintptr {
	if ctrlType != windows.CTRL_BREAK_EVENT {
		return 0
	}
	f := breakFunc.Load()
	if f == nil {
		return 0
	}
	(*f)()
	return 1
}
