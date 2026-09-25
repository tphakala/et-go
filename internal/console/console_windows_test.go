package console

import (
	"errors"
	"os"
	"slices"
	"strings"
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

// consoleFreeWait bounds waits in console-free tests.
const consoleFreeWait = 5 * time.Second

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
	case <-time.After(consoleFreeWait):
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
	case <-time.After(consoleFreeWait):
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

// TestCloseWaitsForInFlightWrite needs no console: a Write in progress when
// Close starts must finish before Close restores the modes and returns.
func TestCloseWaitsForInFlightWrite(t *testing.T) {
	c := newConsole(0, 0)
	entered := make(chan struct{})
	release := make(chan struct{})
	c.writeFn = func(_ windows.Handle, _ *uint16, n uint32, written *uint32, _ *byte) error {
		close(entered)
		<-release
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
	case <-time.After(consoleFreeWait):
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
}

// openConsole returns the real console, or skips when the test binary has
// none (for example when its output is piped).
func openConsole(t *testing.T) *Console {
	t.Helper()
	c, err := Open()
	if errors.Is(err, ErrNotTerminal) {
		t.Skip("no console attached; run the test binary under ssh -tt or in a console window")
	}
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
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
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not unblock a pending Read")
	}

	start := time.Now()
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v, want nil", err)
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("second Close took %v, want an immediate return", d)
	}
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
			case <-time.After(5 * time.Second):
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

func TestRestoreReturnsToOpenBaseline(t *testing.T) {
	c := openConsole(t)
	t.Cleanup(func() { _ = windows.SetConsoleMode(c.in, c.inBase) })
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
}

// echoOff turns echo off on c's input, as ssh.exe interrupted at a password
// prompt can leave it, and restores the Open baseline when the test ends.
func echoOff(t *testing.T, c *Console) uint32 {
	t.Helper()
	if c.inBase&windows.ENABLE_ECHO_INPUT == 0 {
		t.Skipf("Open-time input mode %#x has echo off already", c.inBase)
	}
	t.Cleanup(func() { _ = windows.SetConsoleMode(c.in, c.inBase) })
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

func TestWriteLarge(t *testing.T) {
	c := openConsole(t)
	// Record every chunk Write hands to WriteConsoleW, and still write it to
	// the real console.
	var chunks [][]uint16
	orig := writeConsole
	t.Cleanup(func() { writeConsole = orig })
	writeConsole = func(h windows.Handle, buf *uint16, n uint32, written *uint32, reserved *byte) error {
		chunks = append(chunks, slices.Clone(unsafe.Slice(buf, n)))
		return orig(h, buf, n, written, reserved)
	}

	// Several times writeUnits, with an emoji straddling the first chunk edge.
	big := strings.Repeat("x", writeUnits-1) + "😀" + strings.Repeat("y", 3*writeUnits) + "\r\n"
	if n, err := c.Write([]byte(big)); err != nil || n != len(big) {
		t.Fatalf("Write(%d bytes) = %d, %v", len(big), n, err)
	}

	want := utf16.Encode([]rune(big))
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
	deadline := time.After(5 * time.Second)
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
	c := openConsole(t)
	euro := []byte("€\r\n")
	// c.buf holds the UTF-16 units the last Write sent to the console.
	wantUnits := [][]uint16{nil, nil, {0x20AC, '\r', '\n'}}
	for i, part := range [][]byte{euro[:1], euro[1:2], euro[2:]} {
		if n, err := c.Write(part); err != nil || n != len(part) {
			t.Fatalf("Write(%q) = %d, %v", part, n, err)
		}
		if !slices.Equal(c.buf, wantUnits[i]) {
			t.Fatalf("Write %d (%q) sent %#x, want %#x", i+1, part, c.buf, wantUnits[i])
		}
	}
	if c.enc.n != 0 {
		t.Fatalf("encoder still carries %d bytes after a complete sequence", c.enc.n)
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
