package console

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// waitTimeout bounds every wait on a real file descriptor; synctest cannot
// drive real terminal I/O.
const waitTimeout = 5 * time.Second

func mustConsole(t *testing.T, tty *os.File) *Console {
	t.Helper()
	c, err := newConsole(tty)
	if err != nil {
		t.Fatalf("newConsole: %v", err)
	}
	return c
}

func TestOpenRejectsNonTerminal(t *testing.T) {
	_, slave := openPTY(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	t.Cleanup(func() { _ = r.Close(); _ = w.Close() })

	tests := []struct {
		name          string
		stdin, stdout *os.File
	}{
		{"both pipes", r, w},
		{"stdin pipe", r, slave},
		{"stdout pipe", slave, w},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := open(slave.Name(), tt.stdin, tt.stdout)
			if !errors.Is(err, ErrNotTerminal) {
				if c != nil {
					_ = c.Close()
				}
				t.Fatalf("open error = %v, want ErrNotTerminal", err)
			}
		})
	}
}

func TestOpenWithoutControllingTerminal(t *testing.T) {
	_, slave := openPTY(t)
	_, err := open("/nonexistent/et-go-tty", slave, slave)
	if !errors.Is(err, ErrNotTerminal) {
		t.Fatalf("open error = %v, want ErrNotTerminal", err)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("open error = %v, want the underlying open error kept", err)
	}
}

func TestOpenAcceptsTerminal(t *testing.T) {
	_, slave := openPTY(t)
	c, err := open(slave.Name(), slave, slave)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestReadWrite(t *testing.T) {
	master, slave := openPTY(t)
	c := mustConsole(t, slave)
	if _, err := c.MakeRaw(); err != nil {
		t.Fatalf("MakeRaw: %v", err)
	}

	if _, err := master.WriteString("\x1b[A"); err != nil {
		t.Fatalf("write to master: %v", err)
	}
	got := make([]byte, 16)
	n, err := c.Read(got)
	if err != nil || string(got[:n]) != "\x1b[A" {
		t.Fatalf("Read = %q, %v; want the arrow-up sequence unchanged", got[:n], err)
	}

	if _, err := c.Write([]byte("out")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	out := make([]byte, 3)
	if _, err := io.ReadFull(master, out); err != nil || string(out) != "out" {
		t.Fatalf("master read %q, %v; want %q", out, err, "out")
	}
}

func TestMakeRawAndRestore(t *testing.T) {
	_, slave := openPTY(t)
	c := mustConsole(t, slave)
	before := termios(t, slave)
	if before.Lflag&(unix.ICANON|unix.ECHO) == 0 {
		t.Fatal("a fresh pty should start in cooked mode")
	}

	restore, err := c.MakeRaw()
	if err != nil {
		t.Fatalf("MakeRaw: %v", err)
	}
	if raw := termios(t, slave); raw.Lflag&(unix.ICANON|unix.ECHO) != 0 {
		t.Fatalf("after MakeRaw Lflag = %#x, want ICANON and ECHO cleared", raw.Lflag)
	}

	if err := restore(); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if err := restore(); err != nil {
		t.Fatalf("second restore must be a no-op, got %v", err)
	}
	if after := termios(t, slave); after.Lflag != before.Lflag {
		t.Fatalf("after restore Lflag = %#x, want %#x", after.Lflag, before.Lflag)
	}
}

// TestRestoreReturnsToOpenBaseline models ssh being interrupted at a
// password prompt between Open and MakeRaw, leaving echo off: restore must
// return to the state recorded at Open, not to the degraded one.
func TestRestoreReturnsToOpenBaseline(t *testing.T) {
	_, slave := openPTY(t)
	c := mustConsole(t, slave)
	baseline := termios(t, slave)

	degraded := *baseline
	degraded.Lflag &^= unix.ECHO
	if err := controlFile(slave, func(fd int) error {
		return unix.IoctlSetTermios(fd, unix.TCSETS, &degraded)
	}); err != nil {
		t.Fatalf("degrade termios: %v", err)
	}

	restore, err := c.MakeRaw()
	if err != nil {
		t.Fatalf("MakeRaw: %v", err)
	}
	if err := restore(); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if after := termios(t, slave); after.Lflag != baseline.Lflag {
		t.Fatalf("after restore Lflag = %#x, want the Open baseline %#x (echo on)", after.Lflag, baseline.Lflag)
	}
}

func TestCloseRestoresAndUnblocksRead(t *testing.T) {
	_, slave := openPTY(t)
	// A second descriptor on the same terminal to inspect it after Close.
	probe, err := os.OpenFile(slave.Name(), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatalf("reopen slave: %v", err)
	}
	t.Cleanup(func() { _ = probe.Close() })
	before := termios(t, probe)

	c := mustConsole(t, slave)
	if _, err := c.MakeRaw(); err != nil {
		t.Fatalf("MakeRaw: %v", err)
	}
	readErr := make(chan error, 1)
	go func() {
		_, err := c.Read(make([]byte, 8))
		readErr <- err
	}()

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
	if after := termios(t, probe); after.Lflag != before.Lflag {
		t.Fatalf("Close left Lflag = %#x, want restored %#x", after.Lflag, before.Lflag)
	}
}

func TestSize(t *testing.T) {
	_, slave := openPTY(t)
	c := mustConsole(t, slave)
	want := unix.Winsize{Row: 40, Col: 132, Xpixel: 1320, Ypixel: 800}
	setWinsize(t, slave, &want)

	got, err := c.Size()
	if err != nil {
		t.Fatalf("Size: %v", err)
	}
	if got != (Size{Rows: 40, Cols: 132, Width: 1320, Height: 800}) {
		t.Fatalf("Size() = %+v, want 40x132 with 1320x800 pixels", got)
	}
}

func TestResizesYieldsChangesOnly(t *testing.T) {
	_, slave := openPTY(t)
	c := mustConsole(t, slave)
	setWinsize(t, slave, &unix.Winsize{Row: 24, Col: 80})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sizes := make(chan Size, 4)
	done := make(chan struct{})
	resizes := c.Resizes(ctx) // baseline 24x80 is taken here
	go func() {
		defer close(done)
		for sz := range resizes {
			sizes <- sz
		}
	}()

	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	winch := func() {
		t.Helper()
		if err := unix.Kill(os.Getpid(), unix.SIGWINCH); err != nil {
			t.Fatalf("kill: %v", err)
		}
	}

	// Phase 1: SIGWINCH with the size unchanged, repeated for 300 ms so the
	// iterator has certainly registered for the signal. Nothing may be yielded.
	quiet := time.After(300 * time.Millisecond)
unchanged:
	for {
		winch()
		select {
		case sz := <-sizes:
			t.Fatalf("an unchanged size was yielded: %+v", sz)
		case <-tick.C:
		case <-quiet:
			break unchanged
		}
	}

	// Phase 2: change the size and signal until the new size arrives.
	setWinsize(t, slave, &unix.Winsize{Row: 50, Col: 120})
	deadline := time.After(waitTimeout)
	for {
		winch()
		select {
		case sz := <-sizes:
			if sz != (Size{Rows: 50, Cols: 120}) {
				t.Fatalf("Resizes yielded %+v, want 50x120", sz)
			}
			cancel()
			<-done
			if len(sizes) != 0 {
				t.Fatalf("the new size was yielded more than once: %+v", <-sizes)
			}
			return
		case <-tick.C:
		case <-deadline:
			t.Fatal("Resizes never yielded the new size")
		}
	}
}

func setWinsize(t *testing.T, f *os.File, ws *unix.Winsize) {
	t.Helper()
	if err := controlFile(f, func(fd int) error {
		return unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, ws)
	}); err != nil {
		t.Fatalf("set winsize: %v", err)
	}
}
