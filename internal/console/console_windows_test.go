package console

import (
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestInputRecordLayout(t *testing.T) {
	if got := unsafe.Sizeof(keyEventRecord{}); got != 16 {
		t.Fatalf("sizeof(KEY_EVENT_RECORD) = %d, want 16", got)
	}
	if got := unsafe.Sizeof(inputRecord{}); got != 20 {
		t.Fatalf("sizeof(INPUT_RECORD) = %d, want 20", got)
	}
	if got := unsafe.Offsetof(inputRecord{}.Event); got != 4 {
		t.Fatalf("offsetof(INPUT_RECORD.Event) = %d, want 4", got)
	}
	// KEY_EVENT_RECORD member offsets and sizes (Microsoft docs,
	// KEY_EVENT_RECORD: BOOL, WORD, WORD, WORD, union uChar, DWORD).
	var k keyEventRecord
	for _, f := range []struct {
		name              string
		off, size         uintptr
		wantOff, wantSize uintptr
	}{
		{"bKeyDown", unsafe.Offsetof(k.KeyDown), unsafe.Sizeof(k.KeyDown), 0, 4},
		{"wRepeatCount", unsafe.Offsetof(k.RepeatCount), unsafe.Sizeof(k.RepeatCount), 4, 2},
		{"wVirtualKeyCode", unsafe.Offsetof(k.VirtualKeyCode), unsafe.Sizeof(k.VirtualKeyCode), 6, 2},
		{"wVirtualScanCode", unsafe.Offsetof(k.VirtualScanCode), unsafe.Sizeof(k.VirtualScanCode), 8, 2},
		{"uChar", unsafe.Offsetof(k.UnicodeChar), unsafe.Sizeof(k.UnicodeChar), 10, 2},
		{"dwControlKeyState", unsafe.Offsetof(k.ControlKeyState), unsafe.Sizeof(k.ControlKeyState), 12, 4},
	} {
		if f.off != f.wantOff || f.size != f.wantSize {
			t.Errorf("KEY_EVENT_RECORD.%s at offset %d, size %d; want offset %d, size %d",
				f.name, f.off, f.size, f.wantOff, f.wantSize)
		}
	}
}

func TestCtrlHandler(t *testing.T) {
	t.Cleanup(func() { OnBreak(nil) })

	called := 0
	OnBreak(func() { called++ })
	if got := ctrlHandler(windows.CTRL_BREAK_EVENT); got != 1 || called != 1 {
		t.Fatalf("Ctrl+Break: handler returned %d and called f %d times, want 1 and 1", got, called)
	}
	if got := ctrlHandler(windows.CTRL_C_EVENT); got != 0 || called != 1 {
		t.Fatalf("Ctrl+C: handler returned %d and called f %d times, want 0 and 1", got, called)
	}

	OnBreak(nil)
	if got := ctrlHandler(windows.CTRL_BREAK_EVENT); got != 0 || called != 1 {
		t.Fatalf("after OnBreak(nil): handler returned %d and called f %d times, want 0 and 1", got, called)
	}
}

// TestReadAfterCloseIgnoresPending needs no console: a closed Console must
// not hand out bytes buffered from an earlier console read.
func TestReadAfterCloseIgnoresPending(t *testing.T) {
	c := newConsole(0, 0)
	c.pending = []byte("left over")
	c.closing = true
	n, err := c.Read(make([]byte, 16))
	if n != 0 || !errors.Is(err, os.ErrClosed) {
		t.Fatalf("Read after Close = %d, %v; want 0, os.ErrClosed", n, err)
	}
}

// The tests below that need no console build it on zero handles: the
// console calls Close makes on them (flush, mode read) fail, so the first
// Close returns an error, which these tests do not inspect unless they say
// so.

// waitTimeout bounds how long a test waits for a call or goroutine that
// must finish, with or without a console.
const waitTimeout = 5 * time.Second

// stillRunning is how long a console-free test watches a call that must
// stay blocked. When the code under test is broken such a call makes only
// failing console calls and returns almost at once, so the bound only has
// to exceed scheduling noise.
const stillRunning = 200 * time.Millisecond

// closeAsync runs c.Close on its own goroutine.
func closeAsync(c *Console) <-chan error {
	ch := make(chan error, 1)
	go func() { ch <- c.Close() }()
	return ch
}

func recv(t *testing.T, ch <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(waitTimeout):
		t.Fatalf("%s never returned", what)
		return nil
	}
}

// TestCloseIsIdempotent needs no console: once the first Close has
// finished, later calls return nil at once, without its error.
func TestCloseIsIdempotent(t *testing.T) {
	c := newConsole(0, 0)
	_ = recv(t, closeAsync(c), "first Close")
	for i := range 2 {
		if err := recv(t, closeAsync(c), "Close after Close"); err != nil {
			t.Fatalf("Close %d after the first = %v, want nil", i+1, err)
		}
	}
}

// TestConcurrentCloseWaits needs no console: a Close that starts while the
// first Close is still waking the reader must wait for it, then return nil.
func TestConcurrentCloseWaits(t *testing.T) {
	c := newConsole(0, 0)
	c.reading = true // the first Close takes the wake path
	entered := make(chan struct{})
	release := make(chan struct{})
	c.injectFn = func(windows.Handle, []inputRecord) error {
		select {
		case <-entered:
		default:
			close(entered)
			<-release
		}
		// The reader returns. A later tick may inject again before Close
		// consumes this, so do not block on a full channel.
		select {
		case c.exited <- struct{}{}:
		default:
		}
		return nil
	}

	first := closeAsync(c)
	select {
	case <-entered:
	case <-time.After(waitTimeout):
		t.Fatal("the first Close never tried to wake the reader")
	}
	second := closeAsync(c)
	select {
	case err := <-second:
		close(release)
		t.Fatalf("a second Close returned (%v) while the first was still in progress", err)
	case <-time.After(stillRunning):
	}
	close(release)
	_ = recv(t, first, "first Close")
	if err := recv(t, second, "second Close"); err != nil {
		t.Fatalf("second Close = %v, want nil", err)
	}
}

// TestWakeRetriesAfterInjectError needs no console: a failed wake injection
// must not end Close's wait for the reader; the next tick injects again.
func TestWakeRetriesAfterInjectError(t *testing.T) {
	c := newConsole(0, 0)
	c.reading = true
	errInject := errors.New("injected failure")
	calls := 0
	c.injectFn = func(windows.Handle, []inputRecord) error {
		calls++
		if calls == 1 {
			return errInject
		}
		select {
		case c.exited <- struct{}{}:
		default:
		}
		return nil
	}
	err := c.Close()
	if calls < 2 {
		t.Fatalf("Close injected %d wake records, want a retry after the failed one", calls)
	}
	if errors.Is(err, errInject) {
		t.Fatalf("Close = %v, want no wake error once the reader returned", err)
	}
}

func TestWriteAfterClose(t *testing.T) {
	c := newConsole(0, 0)
	c.writeFn = func(windows.Handle, *uint16, uint32, *uint32, *byte) error {
		t.Error("Write after Close reached the console")
		return nil
	}
	_ = c.Close()
	if n, err := c.Write([]byte("x")); n != 0 || !errors.Is(err, os.ErrClosed) {
		t.Fatalf("Write after Close = %d, %v; want 0, os.ErrClosed", n, err)
	}
}

func TestMakeRawAfterClose(t *testing.T) {
	c := newConsole(0, 0)
	_ = c.Close()
	restore, err := c.MakeRaw()
	if restore != nil || !errors.Is(err, os.ErrClosed) {
		t.Fatalf("MakeRaw after Close = (restore set: %t), %v; want nil, os.ErrClosed", restore != nil, err)
	}
}

func TestSizeAfterClose(t *testing.T) {
	c := newConsole(0, 0)
	_ = c.Close()
	if _, err := c.Size(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("Size after Close = %v, want an error wrapping os.ErrClosed", err)
	}
}

// TestCloseWaitsForInFlightWrite needs no console: when Close starts while
// a Write is inside its console call, Close must not set a console mode
// until that call has returned, and must not return before it either. The
// fake mode calls report a mode that differs from the baseline, so Close
// really sets modes and the order can be observed.
func TestCloseWaitsForInFlightWrite(t *testing.T) {
	c := newConsole(0, 0)
	c.inBase, c.outBase = 0x1f7, 0x7
	var writing atomic.Bool // set while the fake console write is in progress
	var sets atomic.Int32
	c.getModeFn = func(_ windows.Handle, mode *uint32) error {
		*mode = 0 // differs from both baselines, so Close sets each
		return nil
	}
	c.setModeFn = func(windows.Handle, uint32) error {
		sets.Add(1)
		if writing.Load() {
			t.Error("Close set a console mode while a Write was still in its console call")
		}
		return nil
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	c.writeFn = func(_ windows.Handle, _ *uint16, n uint32, written *uint32, _ *byte) error {
		writing.Store(true)
		close(entered)
		<-release
		writing.Store(false)
		*written = n
		return nil
	}
	wrote := make(chan error, 1)
	go func() {
		_, err := c.Write([]byte("x"))
		wrote <- err
	}()
	select {
	case <-entered:
	case <-time.After(waitTimeout):
		t.Fatal("Write never reached the console")
	}

	closed := closeAsync(c)
	select {
	case <-closed:
		close(release)
		t.Fatal("Close returned while a Write was still in progress")
	case <-time.After(stillRunning):
	}
	close(release)
	if err := recv(t, wrote, "Write"); err != nil {
		t.Fatalf("in-flight Write = %v, want nil", err)
	}
	_ = recv(t, closed, "Close")
	if sets.Load() == 0 {
		t.Fatal("Close set no console mode, so the test observed no ordering")
	}
}

// TestOpenWithoutConsoleKeepsCause runs where stdin is not a console (the
// test binary run with its standard handles redirected): Open must report
// ErrNotTerminal and keep the Windows error that caused it.
func TestOpenWithoutConsoleKeepsCause(t *testing.T) {
	c, err := Open()
	if err == nil {
		if cerr := c.Close(); cerr != nil {
			t.Errorf("Close: %v", cerr)
		}
		t.Skip("a console is attached; run the test binary with redirected standard handles")
	}
	if !errors.Is(err, ErrNotTerminal) {
		t.Fatalf("Open error = %v, want ErrNotTerminal", err)
	}
	if _, ok := errors.AsType[windows.Errno](err); !ok {
		t.Fatalf("Open error = %v, want it to wrap the Windows error that caused it", err)
	}
}

// rangeResizes ranges over c.Resizes(ctx) on its own goroutine and closes
// the returned channel when the range statement finishes. Any size yielded
// fails the test: these Consoles have no window to resize.
func rangeResizes(t *testing.T, c *Console, ctx context.Context) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	resizes := c.Resizes(ctx)
	go func() {
		defer close(done)
		for sz := range resizes {
			t.Errorf("Resizes yielded %+v, want nothing", sz)
		}
	}()
	return done
}

// TestResizesEndsAfterClose needs no console: once the Console is closed,
// the next poll ends the range while ctx is still live.
func TestResizesEndsAfterClose(t *testing.T) {
	c := newConsole(0, 0)
	_ = c.Close()
	done := rangeResizes(t, c, t.Context())
	select {
	case <-done:
	case <-time.After(waitTimeout):
		t.Fatal("the range over Resizes did not end after Close")
	}
}

// TestResizesKeepsPollingOnOtherErrors needs no console: a Size error other
// than a closed Console (here the zero handles are invalid) does not end
// the range; only ctx does.
func TestResizesKeepsPollingOnOtherErrors(t *testing.T) {
	c := newConsole(0, 0)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := rangeResizes(t, c, ctx)
	select {
	case <-done:
		t.Fatal("the range over Resizes ended on a Size error that is not a Close")
	case <-time.After(3 * resizePoll):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(waitTimeout):
		t.Fatal("the range over Resizes did not end after ctx was cancelled")
	}
}

// openConsole returns the real console, or skips when the test binary has
// none (for example when its output is piped). The console is closed when
// the test ends, which returns it to the Open baseline for the next test;
// a Close the test made itself leaves that one returning nil.
func openConsole(t *testing.T) *Console {
	t.Helper()
	c, err := Open()
	if errors.Is(err, ErrNotTerminal) {
		t.Skip("no console attached; run the test binary under ssh -tt or in a console window")
	}
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Errorf("Close at test end: %v", err)
		}
	})
	return c
}

func TestCloseUnblocksRead(t *testing.T) {
	c := openConsole(t)
	if _, err := c.MakeRaw(); err != nil {
		t.Fatalf("MakeRaw: %v", err)
	}
	readErr := make(chan error, 1)
	go func() {
		_, err := c.Read(make([]byte, 16))
		readErr <- err
	}()
	// Wait until the reader is inside ReadConsoleW, so Close takes the
	// wake-up path rather than the not-yet-reading path.
	waitReading(t, c)

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-readErr:
		if !errors.Is(err, os.ErrClosed) {
			t.Fatalf("pending Read returned %v, want os.ErrClosed", err)
		}
	case <-time.After(waitTimeout):
		t.Fatal("Close did not unblock a pending Read")
	}

	if err := recv(t, closeAsync(c), "second Close"); err != nil {
		t.Fatalf("second Close: %v, want nil", err)
	}
}

// TestCloseFlushesTypeahead checks that input nobody read is discarded by
// Close, so it does not reach the shell et returns to.
func TestCloseFlushesTypeahead(t *testing.T) {
	c := openConsole(t)
	key := inputRecord{
		EventType: windows.KEY_EVENT,
		Event:     keyEventRecord{KeyDown: 1, RepeatCount: 1, VirtualKeyCode: 'A', UnicodeChar: 'a'},
	}
	if err := writeConsoleInput(c.in, []inputRecord{key, key}); err != nil {
		t.Fatalf("inject typeahead: %v", err)
	}
	if n := inputEvents(t, c); n == 0 {
		t.Fatal("injected typeahead is not in the input buffer")
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if n := inputEvents(t, c); n != 0 {
		t.Fatalf("%d input events left after Close, want 0", n)
	}
}

func inputEvents(t *testing.T, c *Console) uint32 {
	t.Helper()
	var n uint32
	if err := windows.GetNumberOfConsoleInputEvents(c.in, &n); err != nil {
		t.Fatalf("GetNumberOfConsoleInputEvents: %v", err)
	}
	return n
}

// TestCloseRestoresOutputMode checks that Close returns the output handle,
// not only the input handle, to its Open baseline.
func TestCloseRestoresOutputMode(t *testing.T) {
	c := openConsole(t)
	if _, err := c.MakeRaw(); err != nil {
		t.Fatalf("MakeRaw: %v", err)
	}
	if mode := outputMode(t, c); mode == c.outBase {
		t.Skipf("MakeRaw left the output mode at the baseline %#x", mode)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if mode := outputMode(t, c); mode != c.outBase {
		t.Fatalf("output mode after Close = %#x, want the Open baseline %#x", mode, c.outBase)
	}
}

func outputMode(t *testing.T, c *Console) uint32 {
	t.Helper()
	var mode uint32
	if err := windows.GetConsoleMode(c.out, &mode); err != nil {
		t.Fatalf("GetConsoleMode(out): %v", err)
	}
	return mode
}

// TestCloseUnblocksLineModeRead covers a Read blocked while the input is in
// line mode, where ReadConsoleW returns only on a carriage return: after
// restore ran before Close, or when MakeRaw was never called.
func TestCloseUnblocksLineModeRead(t *testing.T) {
	for _, tc := range []struct {
		name    string
		restore bool // call MakeRaw and its restore before reading
	}{
		{name: "restored", restore: true},
		{name: "never_raw", restore: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := openConsole(t)
			if c.inBase&windows.ENABLE_LINE_INPUT == 0 {
				t.Skipf("Open-time input mode %#x is not line mode", c.inBase)
			}
			if tc.restore {
				restore, err := c.MakeRaw()
				if err != nil {
					t.Fatalf("MakeRaw: %v", err)
				}
				if err := restore(); err != nil {
					t.Fatalf("restore: %v", err)
				}
			}
			readErr := make(chan error, 1)
			go func() {
				_, err := c.Read(make([]byte, 16))
				readErr <- err
			}()
			waitReading(t, c)

			if err := c.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			select {
			case err := <-readErr:
				if !errors.Is(err, os.ErrClosed) {
					t.Fatalf("pending Read returned %v, want os.ErrClosed", err)
				}
			case <-time.After(waitTimeout):
				t.Fatal("Close did not unblock a Read in line mode")
			}
			var left uint32
			if err := windows.GetNumberOfConsoleInputEvents(c.in, &left); err != nil {
				t.Fatalf("GetNumberOfConsoleInputEvents: %v", err)
			}
			if left != 0 {
				t.Fatalf("%d input events left after Close, want 0", left)
			}
		})
	}
}

// resetInputAtEnd sets c's input back to its Open baseline when the test
// ends, reporting a failure.
func resetInputAtEnd(t *testing.T, c *Console) {
	t.Helper()
	t.Cleanup(func() {
		if err := windows.SetConsoleMode(c.in, c.inBase); err != nil {
			t.Errorf("reset input mode to %#x: %v", c.inBase, err)
		}
	})
}

func TestRestoreReturnsToOpenBaseline(t *testing.T) {
	c := openConsole(t)
	resetInputAtEnd(t, c)
	// Model ssh.exe interrupted at a password prompt: echo left off after Open.
	if err := windows.SetConsoleMode(c.in, c.inBase&^windows.ENABLE_ECHO_INPUT); err != nil {
		t.Fatalf("degrade input mode: %v", err)
	}
	restore, err := c.MakeRaw()
	if err != nil {
		t.Fatalf("MakeRaw: %v", err)
	}
	if err := restore(); err != nil {
		t.Fatalf("restore: %v", err)
	}
	var mode uint32
	if err := windows.GetConsoleMode(c.in, &mode); err != nil {
		t.Fatalf("GetConsoleMode: %v", err)
	}
	if mode != c.inBase {
		t.Fatalf("input mode after restore = %#x, want the Open baseline %#x", mode, c.inBase)
	}
	if mode := outputMode(t, c); mode != c.outBase {
		t.Fatalf("output mode after restore = %#x, want the Open baseline %#x", mode, c.outBase)
	}
}

// fakeModes makes c's restore read the given modes, in call order, and
// records the modes it sets. It needs no console.
func fakeModes(t *testing.T, c *Console, setErr error, reported ...uint32) *[]uint32 {
	t.Helper()
	var set []uint32
	c.getModeFn = func(_ windows.Handle, mode *uint32) error {
		if len(reported) == 0 {
			t.Errorf("restore read more console modes than the script reports")
			return errors.New("script exhausted")
		}
		*mode = reported[0]
		reported = reported[1:]
		return nil
	}
	c.setModeFn = func(_ windows.Handle, mode uint32) error {
		set = append(set, mode)
		return setErr
	}
	return &set
}

// TestRestoreSkipsUnchangedMode needs no console: Close sets a handle's
// baseline mode only when the current mode differs from it.
func TestRestoreSkipsUnchangedMode(t *testing.T) {
	const inBase, outBase = 0x1f7, 0x7
	for _, tc := range []struct {
		name     string
		reported []uint32 // input mode, then output mode
		want     []uint32
	}{
		{name: "at_baseline", reported: []uint32{inBase, outBase}, want: nil},
		{name: "input_changed", reported: []uint32{0x3f0, outBase}, want: []uint32{inBase}},
		{name: "both_changed", reported: []uint32{0x3f0, 0x1f}, want: []uint32{inBase, outBase}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newConsole(0, 0)
			c.inBase, c.outBase = inBase, outBase
			set := fakeModes(t, c, nil, tc.reported...)
			_ = c.Close() // the flush on a zero handle fails; not inspected
			if !slices.Equal(*set, tc.want) {
				t.Fatalf("Close set modes %#x, want %#x", *set, tc.want)
			}
		})
	}

	// A failed set reaches Close's caller.
	t.Run("set_fails", func(t *testing.T) {
		c := newConsole(0, 0)
		c.inBase, c.outBase = inBase, outBase
		errSet := errors.New("set console mode failed")
		set := fakeModes(t, c, errSet, 0x3f0, outBase)
		err := c.Close()
		if len(*set) != 1 {
			t.Fatalf("Close made %d set calls, want 1", len(*set))
		}
		if !errors.Is(err, errSet) {
			t.Fatalf("Close = %v, want an error wrapping the failed set", err)
		}
	})
}

// TestRestoreSerializedWithClose needs no console: a Close that starts
// while a restore func is setting modes must wait for it, so the two never
// set console modes at the same time.
func TestRestoreSerializedWithClose(t *testing.T) {
	c := newConsole(0, 0)
	c.inBase, c.outBase = 0x1f7, 0x7
	c.getModeFn = func(_ windows.Handle, mode *uint32) error {
		*mode = 0 // always differs, so every restore sets
		return nil
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	var inFlight atomic.Int32
	c.setModeFn = func(windows.Handle, uint32) error {
		if inFlight.Add(1) > 1 {
			t.Error("Close set a console mode while restore was setting one")
		}
		defer inFlight.Add(-1)
		select {
		case <-entered:
		default:
			close(entered)
			<-release
		}
		return nil
	}

	restored := make(chan error, 1)
	go func() { restored <- c.restore() }()
	select {
	case <-entered:
	case <-time.After(waitTimeout):
		t.Fatal("restore never set a mode")
	}
	closed := closeAsync(c)
	select {
	case <-closed:
		close(release)
		t.Fatal("Close returned while restore was still setting modes")
	case <-time.After(stillRunning):
	}
	close(release)
	if err := recv(t, restored, "restore"); err != nil {
		t.Fatalf("restore = %v, want nil", err)
	}
	_ = recv(t, closed, "Close")
}

// echoOff turns echo off on c's input, as ssh.exe interrupted at a password
// prompt can leave it, and restores the Open baseline when the test ends.
func echoOff(t *testing.T, c *Console) uint32 {
	t.Helper()
	if c.inBase&windows.ENABLE_ECHO_INPUT == 0 {
		t.Skipf("Open-time input mode %#x has echo off already", c.inBase)
	}
	resetInputAtEnd(t, c)
	degraded := c.inBase &^ windows.ENABLE_ECHO_INPUT
	if err := windows.SetConsoleMode(c.in, degraded); err != nil {
		t.Fatalf("degrade input mode: %v", err)
	}
	return degraded
}

func inputMode(t *testing.T, c *Console) uint32 {
	t.Helper()
	var mode uint32
	if err := windows.GetConsoleMode(c.in, &mode); err != nil {
		t.Fatalf("GetConsoleMode: %v", err)
	}
	return mode
}

// TestCloseRestoresBaselineWithoutMakeRaw covers et closing before MakeRaw
// ran, after a prompt left echo off: Close must still restore the baseline.
func TestCloseRestoresBaselineWithoutMakeRaw(t *testing.T) {
	c := openConsole(t)
	echoOff(t, c)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if mode := inputMode(t, c); mode != c.inBase {
		t.Fatalf("input mode after Close = %#x, want the Open baseline %#x", mode, c.inBase)
	}
}

// TestRestoreAfterClose checks that a restore func run after Close returns
// nil and leaves the console alone: Close already restored it.
func TestRestoreAfterClose(t *testing.T) {
	c := openConsole(t)
	restore, err := c.MakeRaw()
	if err != nil {
		t.Fatalf("MakeRaw: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Another program now owns the console and turned echo off.
	changed := echoOff(t, c)
	if err := restore(); err != nil {
		t.Fatalf("restore after Close = %v, want nil", err)
	}
	if mode := inputMode(t, c); mode != changed {
		t.Fatalf("restore after Close set input mode %#x, want it left at %#x", mode, changed)
	}
}

// writeStep is one scripted WriteConsoleW result: the units it reports
// written (all of the chunk when all is set) or an error.
type writeStep struct {
	n   uint32
	all bool
	err error
}

// scriptWrite sets c.writeFn to a fake that records every chunk and answers
// with steps in order. It fails the test if Write calls it more often than
// the script allows.
func scriptWrite(t *testing.T, c *Console, steps ...writeStep) *[][]uint16 {
	t.Helper()
	var chunks [][]uint16
	c.writeFn = func(_ windows.Handle, buf *uint16, n uint32, written *uint32, _ *byte) error {
		chunks = append(chunks, slices.Clone(unsafe.Slice(buf, n)))
		if len(steps) == 0 {
			t.Errorf("Write made console call %d, beyond its script", len(chunks))
			return errors.New("script exhausted")
		}
		s := steps[0]
		steps = steps[1:]
		*written = s.n
		if s.all {
			*written = n
		}
		return s.err
	}
	return &chunks
}

// fullWrites answers every console call by reporting the whole chunk
// written, for up to n calls.
func fullWrites(n int) []writeStep {
	return slices.Repeat([]writeStep{{all: true}}, n)
}

func TestWriteLarge(t *testing.T) {
	c := newConsole(0, 0)
	chunksp := scriptWrite(t, c, fullWrites(8)...)

	// Several times writeUnits, with an emoji straddling the first chunk edge.
	big := strings.Repeat("x", writeUnits-1) + "😀" + strings.Repeat("y", 3*writeUnits) + "\r\n"
	if n, err := c.Write([]byte(big)); err != nil || n != len(big) {
		t.Fatalf("Write(%d bytes) = %d, %v", len(big), n, err)
	}

	want := utf16.Encode([]rune(big))
	chunks := *chunksp
	if len(chunks) == 0 {
		t.Fatal("Write made no console calls")
	}
	if len(chunks[0]) != writeUnits-1 {
		t.Fatalf("first chunk has %d units, want %d (the pair moved to the next chunk)", len(chunks[0]), writeUnits-1)
	}
	var got []uint16
	for i, ch := range chunks {
		if len(ch) > writeUnits {
			t.Fatalf("chunk %d has %d units, want at most %d", i, len(ch), writeUnits)
		}
		if len(ch) > 0 && isHighSurrogate(ch[len(ch)-1]) {
			t.Fatalf("chunk %d ends on a high surrogate", i)
		}
		got = append(got, ch...)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("chunks carry %d units, want the %d units of the input in order", len(got), len(want))
	}
}

func waitReading(t *testing.T, c *Console) {
	t.Helper()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	deadline := time.After(waitTimeout)
	for {
		c.mu.Lock()
		reading := c.reading
		c.mu.Unlock()
		if reading {
			return
		}
		select {
		case <-tick.C:
		case <-deadline:
			t.Fatal("reader never entered ReadConsoleW")
		}
	}
}

func TestWriteSplitUTF8(t *testing.T) {
	c := newConsole(0, 0)
	chunks := scriptWrite(t, c, fullWrites(1)...)
	euro := []byte("€\r\n")
	// The first two parts hold an incomplete sequence and reach no console
	// call; the third completes it.
	wantCalls := []int{0, 0, 1}
	for i, part := range [][]byte{euro[:1], euro[1:2], euro[2:]} {
		if n, err := c.Write(part); err != nil || n != len(part) {
			t.Fatalf("Write(%q) = %d, %v", part, n, err)
		}
		if len(*chunks) != wantCalls[i] {
			t.Fatalf("after Write %d (%q) the console had %d calls, want %d", i+1, part, len(*chunks), wantCalls[i])
		}
	}
	if want := []uint16{0x20AC, '\r', '\n'}; !slices.Equal((*chunks)[0], want) {
		t.Fatalf("console got %#x, want %#x", (*chunks)[0], want)
	}
}

// TestWriteConsoleResults covers how Write reacts to what WriteConsoleW
// reports: all written, a partial write, nothing written, an error, and an
// impossible count larger than the chunk.
func TestWriteConsoleResults(t *testing.T) {
	errConsole := errors.New("console failed")
	twoChunks := strings.Repeat("z", writeUnits+10) // one full chunk and 10 units
	for _, tc := range []struct {
		name      string
		p         string
		steps     []writeStep
		wantN     int
		wantErr   error // matched with errors.Is; nil means no error
		anyErr    bool  // an error is wanted but no sentinel names it
		wantCalls []int // units handed to each console call
	}{
		{name: "full", p: "hello", steps: fullWrites(1), wantN: 5, wantCalls: []int{5}},
		{name: "partial", p: "hello", steps: []writeStep{{n: 2}, {all: true}}, wantN: 5, wantCalls: []int{5, 3}},
		{name: "nothing_written", p: "hello", steps: []writeStep{{n: 0}}, wantErr: io.ErrShortWrite, wantCalls: []int{5}},
		{name: "error", p: "hello", steps: []writeStep{{err: errConsole}}, wantErr: errConsole, wantCalls: []int{5}},
		{
			name: "count_beyond_chunk", p: twoChunks,
			steps:  []writeStep{{n: writeUnits + 1}, {all: true}},
			anyErr: true, wantCalls: []int{writeUnits},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newConsole(0, 0)
			chunks := scriptWrite(t, c, tc.steps...)
			n, err := c.Write([]byte(tc.p))
			switch {
			case tc.wantErr != nil:
				if n != 0 || !errors.Is(err, tc.wantErr) {
					t.Fatalf("Write = %d, %v; want 0, %v", n, err, tc.wantErr)
				}
			case tc.anyErr:
				if n != 0 || err == nil {
					t.Fatalf("Write = %d, %v; want 0 and an error", n, err)
				}
			default:
				if n != tc.wantN || err != nil {
					t.Fatalf("Write = %d, %v; want %d, nil", n, err, tc.wantN)
				}
			}
			calls := make([]int, 0, len(*chunks))
			for _, ch := range *chunks {
				calls = append(calls, len(ch))
			}
			if !slices.Equal(calls, tc.wantCalls) {
				t.Fatalf("console calls carried %v units, want %v", calls, tc.wantCalls)
			}
		})
	}
}

// scriptRead sets c.readFn to a fake that returns each element of reads
// from one ReadConsoleW call, in order. It fails the test if Read calls it
// more often than the script allows.
func scriptRead(t *testing.T, c *Console, reads ...[]uint16) {
	t.Helper()
	calls := 0
	c.readFn = func(_ windows.Handle, buf *uint16, toread uint32, read *uint32, _ *byte) error {
		calls++
		if len(reads) == 0 {
			t.Errorf("Read made console read %d, beyond its script", calls)
			return errors.New("script exhausted")
		}
		*read = uint32(copy(unsafe.Slice(buf, toread), reads[0]))
		reads = reads[1:]
		return nil
	}
}

func readString(t *testing.T, c *Console, size int) string {
	t.Helper()
	p := make([]byte, size)
	n, err := c.Read(p)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return string(p[:n])
}

// TestReadJoinsSplitSurrogatePair needs no console: a pair whose halves
// arrive from two console reads decodes to one UTF-8 sequence.
func TestReadJoinsSplitSurrogatePair(t *testing.T) {
	c := newConsole(0, 0)
	pair := utf16.Encode([]rune("😀"))
	scriptRead(t, c, pair[:1], pair[1:])
	if got := readString(t, c, 16); got != "😀" {
		t.Fatalf("Read = %q, want %q", got, "😀")
	}
}

// TestReadPassesEscapeSequences needs no console: terminal replies such as
// a device attributes reply and a cursor position report reach the reader
// byte for byte.
func TestReadPassesEscapeSequences(t *testing.T) {
	c := newConsole(0, 0)
	const replies = "\x1b[?1;2c\x1b[12;40R"
	scriptRead(t, c, utf16.Encode([]rune(replies)))
	if got := readString(t, c, 64); got != replies {
		t.Fatalf("Read = %q, want %q", got, replies)
	}
}

// TestReadSmallBufferKeepsRest needs no console: bytes that do not fit p
// are returned by the next Read without another console read.
func TestReadSmallBufferKeepsRest(t *testing.T) {
	c := newConsole(0, 0)
	scriptRead(t, c, utf16.Encode([]rune("abcdef")))
	if got := readString(t, c, 3); got != "abc" {
		t.Fatalf("first Read = %q, want %q", got, "abc")
	}
	if got := readString(t, c, 16); got != "def" {
		t.Fatalf("second Read = %q, want %q", got, "def")
	}
}

// TestReadEmptyBufferReturnsAtOnce needs no console: a Read with an empty p
// returns 0, nil without reading the console, where it could block.
func TestReadEmptyBufferReturnsAtOnce(t *testing.T) {
	c := newConsole(0, 0)
	scriptRead(t, c) // any console read fails the test
	if n, err := c.Read(nil); n != 0 || err != nil {
		t.Fatalf("Read(nil) = %d, %v; want 0, nil", n, err)
	}
	if n, err := c.Read([]byte{}); n != 0 || err != nil {
		t.Fatalf("Read(empty) = %d, %v; want 0, nil", n, err)
	}

	// Once closed, even an empty Read reports the Close.
	_ = c.Close()
	if n, err := c.Read(nil); n != 0 || !errors.Is(err, os.ErrClosed) {
		t.Fatalf("Read(nil) after Close = %d, %v; want 0 and an error wrapping os.ErrClosed", n, err)
	}
}

// TestSizeHoldsLockAgainstClose needs no console: a Size query in progress
// when Close starts finishes before Close sets any mode or returns, and
// Size after Close reports os.ErrClosed.
func TestSizeHoldsLockAgainstClose(t *testing.T) {
	c := newConsole(0, 0)
	c.inBase, c.outBase = 0x1f7, 0x7
	var querying atomic.Bool // set while the fake size query is in progress
	var sets atomic.Int32
	c.getModeFn = func(_ windows.Handle, mode *uint32) error {
		*mode = 0 // differs from both baselines, so Close sets each
		return nil
	}
	c.setModeFn = func(windows.Handle, uint32) error {
		sets.Add(1)
		if querying.Load() {
			t.Error("Close set a console mode while Size was still querying")
		}
		return nil
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	c.sizeFn = func(_ windows.Handle, info *windows.ConsoleScreenBufferInfo) error {
		querying.Store(true)
		close(entered)
		<-release
		querying.Store(false)
		info.Window = windows.SmallRect{Left: 0, Top: 0, Right: 79, Bottom: 23}
		return nil
	}

	type result struct {
		sz  Size
		err error
	}
	sized := make(chan result, 1)
	go func() {
		sz, err := c.Size()
		sized <- result{sz, err}
	}()
	select {
	case <-entered:
	case <-time.After(waitTimeout):
		t.Fatal("Size never queried the console")
	}

	closed := closeAsync(c)
	select {
	case <-closed:
		close(release)
		t.Fatal("Close returned while Size was still querying")
	case <-time.After(stillRunning):
	}
	close(release)
	select {
	case r := <-sized:
		if r.err != nil || r.sz != (Size{Rows: 24, Cols: 80}) {
			t.Fatalf("Size = %+v, %v; want 24x80, nil", r.sz, r.err)
		}
	case <-time.After(waitTimeout):
		t.Fatal("Size never returned")
	}
	_ = recv(t, closed, "Close")
	if sets.Load() == 0 {
		t.Fatal("Close set no console mode, so the test observed no ordering")
	}
	if _, err := c.Size(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("Size after Close = %v, want an error wrapping os.ErrClosed", err)
	}
}

// TestReadRetriesEmptyRead needs no console: a console read that returns no
// units does not end Read with zero bytes; Read reads again.
func TestReadRetriesEmptyRead(t *testing.T) {
	c := newConsole(0, 0)
	scriptRead(t, c, nil, utf16.Encode([]rune("x")))
	if got := readString(t, c, 16); got != "x" {
		t.Fatalf("Read = %q, want %q", got, "x")
	}
}

func TestSizeReportsCells(t *testing.T) {
	c := openConsole(t)
	sz, err := c.Size()
	if err != nil {
		t.Fatalf("Size: %v", err)
	}
	if sz.Rows < 1 || sz.Cols < 1 || sz.Width != 0 || sz.Height != 0 {
		t.Fatalf("Size() = %+v, want positive cells and no pixels", sz)
	}
}
