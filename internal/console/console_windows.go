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

	// mu guards the flags below and is held across every console mode
	// change, so MakeRaw, restore and Close never interleave their
	// SetConsoleMode calls.
	mu      sync.Mutex
	closing bool // guarded by mu: Close has started
	reading bool // guarded by mu: a ReadConsoleW call is in flight

	// wmu is held by Write for its whole call and by Close before it
	// restores the modes, so a Write in flight when Close starts finishes
	// under the modes it started with.
	wmu sync.Mutex

	exited chan struct{} // receives once when a reader returns after Close
	done   chan struct{} // closed when the first Close has finished

	// injectFn appends input records (WriteConsoleInputW), readFn reads
	// UTF-16 units (ReadConsoleW) and writeFn writes them (WriteConsoleW).
	// nil means the real call; tests set fakes to run without a console.
	injectFn func(h windows.Handle, recs []inputRecord) error
	readFn   func(h windows.Handle, buf *uint16, toread uint32, read *uint32, inputControl *byte) error
	writeFn  func(h windows.Handle, buf *uint16, n uint32, written *uint32, reserved *byte) error

	// getModeFn and setModeFn read and set a console mode when restoring
	// (GetConsoleMode, SetConsoleMode). nil means the real call.
	getModeFn func(h windows.Handle, mode *uint32) error
	setModeFn func(h windows.Handle, mode uint32) error

	// sizeFn reads the screen buffer info for Size
	// (GetConsoleScreenBufferInfo). nil means the real call.
	sizeFn func(h windows.Handle, info *windows.ConsoleScreenBufferInfo) error

	// Reader-owned state. pending holds decoded bytes not yet returned,
	// from pendingOff on; it is reset to empty, keeping its capacity, once
	// Read has returned all of it.
	dec        utf16Decoder
	units      []uint16
	unitsRead  uint32 // units the last console read returned
	pending    []byte
	pendingOff int

	// Writer-owned state.
	enc          utf8Encoder
	buf          []uint16
	unitsWritten uint32 // units the last console write reported
}

// Open returns the console attached to stdin and stdout. It returns an error
// satisfying errors.Is(err, ErrNotTerminal) if either is not a console or
// its standard handle cannot be read; the error also wraps the cause.
//
// Open records the console modes as the baseline that restore returns to.
// The intended caller opens the console before running anything that may
// leave it in a changed mode, such as ssh.exe between Open and MakeRaw,
// where an interrupted password prompt can leave echo off.
func Open() (*Console, error) {
	in, err := windows.GetStdHandle(windows.STD_INPUT_HANDLE)
	if err != nil {
		return nil, fmt.Errorf("%w: stdin handle: %w", ErrNotTerminal, err)
	}
	out, err := windows.GetStdHandle(windows.STD_OUTPUT_HANDLE)
	if err != nil {
		return nil, fmt.Errorf("%w: stdout handle: %w", ErrNotTerminal, err)
	}
	c := newConsole(in, out)
	if err := windows.GetConsoleMode(in, &c.inBase); err != nil {
		return nil, fmt.Errorf("%w: stdin console mode: %w", ErrNotTerminal, err)
	}
	if err := windows.GetConsoleMode(out, &c.outBase); err != nil {
		return nil, fmt.Errorf("%w: stdout console mode: %w", ErrNotTerminal, err)
	}
	return c, nil
}

// newConsole returns a Console on the given handles with its channels and
// read buffer set up. Open records the baseline modes; tests that need no
// console pass zero handles.
func newConsole(in, out windows.Handle) *Console {
	return &Console{
		in:     in,
		out:    out,
		exited: make(chan struct{}, 1),
		done:   make(chan struct{}),
		units:  make([]uint16, readUnits),
	}
}

// MakeRaw switches the console to raw VT input and VT output processing,
// starting from the modes recorded by Open. Code pages are not touched: Read
// and Write use the UTF-16 console APIs, and a console code page applies
// only to the 8-bit form of those calls (Microsoft docs, ReadConsole and
// WriteConsole remarks: "uses either Unicode characters or 8-bit
// characters from the console's current code page"). It may be
// called again after restore; Close still returns the console to the Open
// baseline. Once Close has started it returns an error satisfying
// errors.Is(err, os.ErrClosed) and leaves the console alone.
//
// The returned restore func returns the console to the modes recorded by
// Open, not to whatever mode was current when MakeRaw ran. Every restore
// func does this whenever it runs, including one kept from an earlier
// MakeRaw, so running a stale one during a later raw session leaves raw
// mode. It reads the current modes first and sets the baseline only where
// they differ, so it is repeatable and leaves a console already at the
// baseline untouched. It is safe to call from any goroutine. Once Close has
// started it returns nil without touching the console, which Close
// restores.
func (c *Console) MakeRaw() (restore func() error, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closing {
		return nil, fmt.Errorf("console: make raw: %w", os.ErrClosed)
	}
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
			rollback := windows.SetConsoleMode(c.in, c.inBase)
			if rollback != nil {
				rollback = fmt.Errorf("console: roll back input mode: %w", rollback)
			}
			return nil, errors.Join(fmt.Errorf("console: set output mode: %w", err), rollback)
		}
	}

	return c.restore, nil
}

// restore is the func MakeRaw returns; see MakeRaw.
func (c *Console) restore() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closing {
		return nil
	}
	return c.restoreLocked()
}

// restoreLocked sets each handle back to its Open baseline mode where the
// current mode differs from it. c.mu must be held.
func (c *Console) restoreLocked() error {
	err := errors.Join(
		c.setModeIfChanged(c.in, c.inBase),
		c.setModeIfChanged(c.out, c.outBase),
	)
	if err != nil {
		return fmt.Errorf("console: restore: %w", err)
	}
	return nil
}

func (c *Console) setModeIfChanged(h windows.Handle, mode uint32) error {
	get, set := c.getModeFn, c.setModeFn
	if get == nil {
		get = windows.GetConsoleMode
	}
	if set == nil {
		set = windows.SetConsoleMode
	}
	var cur uint32
	if err := get(h, &cur); err != nil {
		return err
	}
	if cur == mode {
		return nil
	}
	return set(h, mode)
}

// Read reads raw VT input as UTF-8. Once Close has started, Read returns
// os.ErrClosed, including a Read that was blocked and any bytes still
// buffered from an earlier console read. A console read that returns no
// units is retried. A Read with an empty p returns 0, nil at once without
// reading the console.
//
// A generated Ctrl+Break does not end a pending read. MEASURED on win11-qa
// under ConPTY (ssh -tt), 2026-09-25: with a handler that consumes
// CTRL_BREAK_EVENT (as OnBreak installs), GenerateConsoleCtrlEvent
// (CTRL_BREAK_EVENT, 0) ran the handler and left ReadConsoleW pending in
// raw and in line mode, so Read has no handling for it. A physical
// Ctrl+Break key press is unmeasured.
func (c *Console) Read(p []byte) (int, error) {
	read := c.readFn
	if read == nil {
		read = windows.ReadConsole
	}
	for {
		c.mu.Lock()
		if c.closing {
			c.mu.Unlock()
			return 0, os.ErrClosed
		}
		if len(p) == 0 {
			// Nothing to read into: do not block in the console.
			c.mu.Unlock()
			return 0, nil
		}
		if len(c.pending) > 0 {
			c.mu.Unlock()
			break
		}
		c.reading = true
		c.mu.Unlock()

		// The count goes into a field: a local whose address is passed to
		// the read func value is moved to the heap, one allocation per
		// console read (go build -gcflags=-m, go1.27).
		c.unitsRead = 0
		err := read(c.in, &c.units[0], uint32(len(c.units)), &c.unitsRead, nil)

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
		c.pending = c.dec.append(c.pending[:0], c.units[:c.unitsRead])
	}
	n := copy(p, c.pending[c.pendingOff:])
	c.pendingOff += n
	if c.pendingOff == len(c.pending) {
		// Drained: keep the whole capacity for the next decode.
		c.pending, c.pendingOff = c.pending[:0], 0
	}
	return n, nil
}

// Write writes remote output, at most writeUnits UTF-16 units per console
// call, and never ends the chunk it hands to a call on a high surrogate.
// An incomplete UTF-8 sequence at the end of p is held until the next
// Write completes it.
// When the console reports a partial write, Write continues from the first
// unit not written, so a pair can be split between calls only if the
// console itself reports writing half of it. Whether any console host does
// that is unmeasured; Write does not back off to a pair boundary.
//
// On a console error Write reports 0 bytes written even if earlier chunks
// reached the screen: the UTF-16 units are not mapped back to input bytes.
// io.Writer allows any n < len(p) with a non-nil error, and callers treat a
// console write error as fatal.
//
// Once Close has started Write returns an error satisfying
// errors.Is(err, os.ErrClosed) and writes nothing. A Write already in
// progress when Close starts finishes first: Close restores the console
// modes only after it returns. This differs from Unix, where a Write that
// runs while Close is in progress can still reach the terminal until Close
// closes the descriptor.
func (c *Console) Write(p []byte) (int, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.mu.Lock()
	closing := c.closing
	c.mu.Unlock()
	if closing {
		return 0, os.ErrClosed
	}
	write := c.writeFn
	if write == nil {
		write = windows.WriteConsole
	}
	c.buf = c.enc.append(c.buf[:0], p)
	for units := c.buf; len(units) > 0; {
		chunk := units[:chunkLen(units, writeUnits)]
		// The count goes into a field, as in Read: a local whose address
		// is passed to the write func value is moved to the heap, one
		// allocation per console write (go build -gcflags=-m, go1.27).
		c.unitsWritten = 0
		if err := write(c.out, &chunk[0], uint32(len(chunk)), &c.unitsWritten, nil); err != nil {
			return 0, fmt.Errorf("console: write: %w", err)
		}
		n := c.unitsWritten
		if n == 0 {
			return 0, io.ErrShortWrite
		}
		if int(n) > len(chunk) {
			return 0, fmt.Errorf("console: write: console reported %d units written of %d", n, len(chunk))
		}
		units = units[n:]
	}
	return len(p), nil
}

// Size returns the visible window size in character cells. Windows does not
// report pixel sizes, so Width and Height are 0. Once Close has started it
// returns an error satisfying errors.Is(err, os.ErrClosed). Size holds the
// Console lock for the whole query, and Close takes that lock to start, so
// a Size that succeeds finished before Close started.
func (c *Console) Size() (Size, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closing {
		return Size{}, fmt.Errorf("console: size: %w", os.ErrClosed)
	}
	query := c.sizeFn
	if query == nil {
		query = windows.GetConsoleScreenBufferInfo
	}
	var info windows.ConsoleScreenBufferInfo
	if err := query(c.out, &info); err != nil {
		return Size{}, fmt.Errorf("console: size: %w", err)
	}
	w := info.Window
	return Size{Rows: int(w.Bottom-w.Top) + 1, Cols: int(w.Right-w.Left) + 1}, nil
}

// Resizes yields the window size each time it changes, until ctx ends, the
// Console is closed, or the loop body stops. Changes are measured against
// the size when Resizes is called, which is not yielded itself; callers
// read it with Size. The window is polled every 200 ms: window resize
// events are always filtered by ReadConsole (Microsoft docs, SetConsoleMode
// remarks). Nothing is yielded once ctx has ended, even a change made
// before it ended. Range over the result once.
//
// The sequence ends when ctx ends or the Console is closed. It notices a
// Close at the next poll. On Unix the next SIGWINCH notices it instead, or
// the start of ranging if the Console was closed before that.
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
			if !resizeCheck(ctx, c.Size, &last, yield) {
				return
			}
		}
	}
}

// Close returns the console to the modes recorded by Open whether or not
// MakeRaw was called, so a mode an interrupted prompt left behind is undone
// too. Like a restore func, it sets a baseline mode only where the current
// mode differs from it. Close unblocks a pending Read, which then returns
// os.ErrClosed, and flushes the input buffer. A Write in progress when
// Close starts finishes before the modes are restored. Close waits for it
// with no bound: whether WriteConsoleW can stall (for example while a
// classic conhost QuickEdit selection pauses output) is unmeasured. The
// handles are the process's standard handles and stay open.
//
// Close is idempotent. A second Close that runs while the first is still in
// progress waits for the first to finish, then returns nil; a Close after
// that returns nil at once. Only the first Close reports errors. A non-nil
// error can mean the reader is still blocked in ReadConsoleW; it returns
// os.ErrClosed once it wakes.
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
		<-c.done
		return nil
	}
	c.closing = true
	reading := c.reading
	c.mu.Unlock()
	defer close(c.done)

	var errs []error
	if reading {
		errs = append(errs, c.wakeAndWait())
	}
	if err := windows.FlushConsoleInputBuffer(c.in); err != nil {
		errs = append(errs, fmt.Errorf("console: flush input: %w", err))
	}

	// Let a Write in flight finish under the modes it started with; every
	// later Write sees closing and writes nothing.
	c.wmu.Lock()
	c.mu.Lock()
	errs = append(errs, c.restoreLocked())
	c.mu.Unlock()
	c.wmu.Unlock()
	return errors.Join(errs...)
}

// wakeAndWait injects wake records until the reader reports that it
// returned, or closeWait passes. A failed injection does not end the wait:
// the next tick injects again, and if the reader returns anyway (another
// record woke it) the wait succeeds.
func (c *Console) wakeAndWait() error {
	tick := time.NewTicker(wakeEvery)
	defer tick.Stop()
	deadline := time.After(closeWait)
	var injectErr error
	for {
		if err := c.wakeReader(); err != nil {
			injectErr = err
		}
		select {
		case <-c.exited:
			return nil
		case <-tick.C:
		case <-deadline:
			return errors.Join(errors.New("console: reader did not return after close"), injectErr)
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
	inject := c.injectFn
	if inject == nil {
		inject = writeConsoleInput
	}
	if err := inject(c.in, []inputRecord{rec}); err != nil {
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
// recent f is kept. OnBreak(nil) unregisters it, and Ctrl+Break then goes
// to the next handler: the Go runtime's, which delivers it as os.Interrupt
// if the program called signal.Notify for it, and otherwise passes it on to
// the default handler, which ends the process (runtime/os_windows.go,
// ctrlHandler, go1.27.0; Microsoft docs, HandlerRoutine remarks). If the
// handler cannot be installed, Ctrl+Break reaches those handlers directly.
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
// handler. It runs on a thread Windows creates for the call (Microsoft docs,
// HandlerRoutine: "the system creates a new thread in the process to
// execute the function").
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
