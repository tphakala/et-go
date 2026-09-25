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
	c := &Console{pending: []byte("left over"), closing: true}
	n, err := c.Read(make([]byte, 16))
	if n != 0 || !errors.Is(err, os.ErrClosed) {
		t.Fatalf("Read after Close = %d, %v; want 0, os.ErrClosed", n, err)
	}
}

// TestCloseIsIdempotent needs no console: a Close that finds another Close
// already in progress (closing set, reader still in flight) must return nil
// at once instead of waiting for the reader and reporting an error.
func TestCloseIsIdempotent(t *testing.T) {
	c := &Console{closing: true, reading: true, exited: make(chan struct{}, 1)}
	start := time.Now()
	if err := c.Close(); err != nil {
		t.Fatalf("Close during Close = %v, want nil", err)
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("Close during Close took %v, want an immediate return", d)
	}
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
