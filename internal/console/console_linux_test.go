package console

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
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
	closeAtEnd(t, r)
	closeAtEnd(t, w)

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

// openThroughPath builds the Console the way Open does, reopening the
// terminal by path. open does not pass O_NOCTTY, so a session leader
// without a controlling terminal would acquire the test pty as one; the
// helper skips in that case rather than change the process's terminal.
func openThroughPath(t *testing.T, slave *os.File) *Console {
	t.Helper()
	if sid, err := unix.Getsid(0); err == nil && sid == os.Getpid() {
		t.Skip("the test process is a session leader; opening the pty without O_NOCTTY could make it the controlling terminal")
	}
	c, err := open(slave.Name(), slave, slave)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return c
}

// TestOpenRejectsNonTerminalTTYPath covers a tty path that opens but is not
// a terminal: open must report ErrNotTerminal, not a bare ioctl error.
func TestOpenRejectsNonTerminalTTYPath(t *testing.T) {
	_, slave := openPTY(t)
	regular := filepath.Join(t.TempDir(), "not-a-tty")
	if err := os.WriteFile(regular, nil, 0o600); err != nil {
		t.Fatalf("create regular file: %v", err)
	}
	c, err := open(regular, slave, slave)
	if !errors.Is(err, ErrNotTerminal) {
		if c != nil {
			_ = c.Close()
		}
		t.Fatalf("open(regular file) error = %v, want ErrNotTerminal", err)
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

	// restore is repeatable: after the mode changes again, a second call
	// returns to the baseline too.
	setTermios(t, slave, &degraded)
	if err := restore(); err != nil {
		t.Fatalf("second restore: %v", err)
	}
	if after := termios(t, slave); after.Lflag != baseline.Lflag {
		t.Fatalf("after a second restore Lflag = %#x, want the Open baseline %#x", after.Lflag, baseline.Lflag)
	}
}

// probeTTY opens a second descriptor on the terminal behind slave, to
// inspect and change it after the Console closed its own.
func probeTTY(t *testing.T, slave *os.File) *os.File {
	t.Helper()
	probe, err := os.OpenFile(slave.Name(), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatalf("reopen slave: %v", err)
	}
	closeAtEnd(t, probe)
	return probe
}

func setTermios(t *testing.T, f *os.File, tio *unix.Termios) {
	t.Helper()
	if err := controlFile(f, func(fd int) error {
		return unix.IoctlSetTermios(fd, unix.TCSETS, tio)
	}); err != nil {
		t.Fatalf("tcsets: %v", err)
	}
}

// withEchoOff clears ECHO on f and returns the termios it had before.
func withEchoOff(t *testing.T, f *os.File) *unix.Termios {
	t.Helper()
	before := termios(t, f)
	degraded := *before
	degraded.Lflag &^= unix.ECHO
	setTermios(t, f, &degraded)
	return before
}

// TestCloseRestoresBaselineWithoutMakeRaw models ssh interrupted at a
// password prompt with echo off and et closing before MakeRaw ran: Close
// must still return the terminal to the Open baseline.
func TestCloseRestoresBaselineWithoutMakeRaw(t *testing.T) {
	_, slave := openPTY(t)
	probe := probeTTY(t, slave)
	c := mustConsole(t, slave)
	baseline := withEchoOff(t, probe)

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if after := termios(t, probe); after.Lflag != baseline.Lflag {
		t.Fatalf("after Close Lflag = %#x, want the Open baseline %#x (echo on)", after.Lflag, baseline.Lflag)
	}
}

// TestCloseTwice checks that a Close that starts while another is still
// restoring waits for it and returns nil, and that a Close after both
// returns nil at once.
func TestCloseTwice(t *testing.T) {
	_, slave := openPTY(t)
	probe := probeTTY(t, slave)
	c := mustConsole(t, slave)
	baseline := withEchoOff(t, probe) // so the first Close has a mode to set

	entered := make(chan struct{})
	release := make(chan struct{})
	set := c.setState
	c.setState = func(fd int, s *term.State) error {
		close(entered)
		<-release
		return set(fd, s)
	}

	first := make(chan error, 1)
	go func() { first <- c.Close() }()
	select {
	case <-entered:
	case <-time.After(waitTimeout):
		t.Fatal("the first Close never reached the terminal restore")
	}

	second := make(chan error, 1)
	go func() { second <- c.Close() }()
	select {
	case err := <-second:
		close(release)
		t.Fatalf("a second Close returned (%v) while the first was still restoring", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)

	for name, ch := range map[string]chan error{"first": first, "second": second} {
		select {
		case err := <-ch:
			if err != nil {
				t.Fatalf("%s Close = %v, want nil", name, err)
			}
		case <-time.After(waitTimeout):
			t.Fatalf("%s Close never returned", name)
		}
	}
	if after := termios(t, probe); after.Lflag != baseline.Lflag {
		t.Fatalf("after Close Lflag = %#x, want the Open baseline %#x", after.Lflag, baseline.Lflag)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close after Close = %v, want nil", err)
	}
}

func TestWriteAfterClose(t *testing.T) {
	_, slave := openPTY(t)
	c := mustConsole(t, slave)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if n, err := c.Write([]byte("x")); n != 0 || !errors.Is(err, os.ErrClosed) {
		t.Fatalf("Write after Close = %d, %v; want 0, os.ErrClosed", n, err)
	}
}

func TestMakeRawAfterClose(t *testing.T) {
	_, slave := openPTY(t)
	probe := probeTTY(t, slave)
	c := mustConsole(t, slave)
	before := termios(t, probe)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	restore, err := c.MakeRaw()
	if restore != nil || !errors.Is(err, os.ErrClosed) {
		t.Fatalf("MakeRaw after Close = (restore set: %t), %v; want nil, os.ErrClosed", restore != nil, err)
	}
	if after := termios(t, probe); after.Lflag != before.Lflag {
		t.Fatalf("MakeRaw after Close changed Lflag to %#x, want %#x", after.Lflag, before.Lflag)
	}
}

// TestRestoreAfterClose checks that a restore func run after Close returns
// nil and leaves the terminal alone: Close already restored it.
func TestRestoreAfterClose(t *testing.T) {
	_, slave := openPTY(t)
	probe := probeTTY(t, slave)
	c := mustConsole(t, slave)
	restore, err := c.MakeRaw()
	if err != nil {
		t.Fatalf("MakeRaw: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Another program now owns the terminal and turned echo off.
	withEchoOff(t, probe)
	changed := termios(t, probe)

	if err := restore(); err != nil {
		t.Fatalf("restore after Close = %v, want nil", err)
	}
	if after := termios(t, probe); after.Lflag != changed.Lflag {
		t.Fatalf("restore after Close changed Lflag to %#x, want it left at %#x", after.Lflag, changed.Lflag)
	}
}

// TestCloseRestoresAfterSecondMakeRaw covers raw mode entered again after a
// restore: Close must still return to the baseline.
func TestCloseRestoresAfterSecondMakeRaw(t *testing.T) {
	_, slave := openPTY(t)
	probe := probeTTY(t, slave)
	c := mustConsole(t, slave)
	baseline := termios(t, probe)

	restore, err := c.MakeRaw()
	if err != nil {
		t.Fatalf("MakeRaw: %v", err)
	}
	if err := restore(); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if _, err := c.MakeRaw(); err != nil {
		t.Fatalf("second MakeRaw: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if after := termios(t, probe); after.Lflag != baseline.Lflag {
		t.Fatalf("after Close Lflag = %#x, want the Open baseline %#x", after.Lflag, baseline.Lflag)
	}
}

func TestSizeAfterClose(t *testing.T) {
	_, slave := openPTY(t)
	c := mustConsole(t, slave)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := c.Size(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("Size after Close = %v, want an error wrapping os.ErrClosed", err)
	}
}

// TestRestoreSkipsUnchangedMode checks that restore and Close do not set a
// terminal that is already at the baseline. Setting it from a background
// job would stop et with SIGTTOU, but the test pty is opened with O_NOCTTY
// and is not the test's controlling terminal, so job control never applies
// here; the set calls are counted through c.setState instead.
func TestRestoreSkipsUnchangedMode(t *testing.T) {
	_, slave := openPTY(t)
	c := mustConsole(t, slave)
	sets := 0
	set := c.setState
	c.setState = func(fd int, s *term.State) error {
		sets++
		return set(fd, s)
	}

	restore, err := c.MakeRaw()
	if err != nil {
		t.Fatalf("MakeRaw: %v", err)
	}
	if err := restore(); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if sets != 1 {
		t.Fatalf("restore from raw mode made %d set calls, want 1", sets)
	}
	if err := restore(); err != nil {
		t.Fatalf("second restore: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if sets != 1 {
		t.Fatalf("restore and Close at the baseline made %d more set calls, want 0", sets-1)
	}
}

// TestRestoreConcurrentWithClose runs a restore func and Close at the same
// time, many times over so the two land in both orders: restore must run
// under the lock Close holds, so it either restores before Close or sees
// the Console closed and returns nil, never touching a closed descriptor.
func TestRestoreConcurrentWithClose(t *testing.T) {
	for i := range 50 {
		_, slave := openPTY(t)
		c := mustConsole(t, slave)
		restore, err := c.MakeRaw()
		if err != nil {
			t.Fatalf("round %d: MakeRaw: %v", i, err)
		}
		started := make(chan struct{})
		restored := make(chan error, 1)
		go func() {
			close(started)
			restored <- restore()
		}()
		<-started // start Close as restore starts, so the two overlap
		if err := c.Close(); err != nil {
			t.Fatalf("round %d: Close = %v, want nil", i, err)
		}
		select {
		case err := <-restored:
			if err != nil {
				t.Fatalf("round %d: restore concurrent with Close = %v, want nil", i, err)
			}
		case <-time.After(waitTimeout):
			t.Fatalf("round %d: restore never returned", i)
		}
	}
}

func TestCloseRestoresAndUnblocksRead(t *testing.T) {
	_, slave := openPTY(t)
	// A second descriptor on the same terminal to inspect it after Close.
	probe := probeTTY(t, slave)
	before := termios(t, probe)

	c := openThroughPath(t, slave)
	if _, err := c.MakeRaw(); err != nil {
		t.Fatalf("MakeRaw: %v", err)
	}
	// SetReadDeadline succeeds only on a pollable fd (a non-pollable one
	// returns os.ErrNoDeadline). A pollable fd is what lets Close unblock a
	// pending Read: the Read parks in the poller, which Close wakes.
	if err := c.tty.SetReadDeadline(time.Time{}); err != nil {
		t.Fatalf("SetReadDeadline: %v, want the tty fd to be pollable", err)
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
			waitDone(t, done, "Resizes after cancel")
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

// TestResizesYieldsChangeBeforeRangingStarts covers the gap between the
// Resizes call and the start of ranging over its result: SIGWINCH is only
// registered once the returned sequence starts running, so a resize that
// happens earlier must still be caught by the check Resizes makes right
// after registering, not lost until the next signal.
func TestResizesYieldsChangeBeforeRangingStarts(t *testing.T) {
	_, slave := openPTY(t)
	c := mustConsole(t, slave)
	setWinsize(t, slave, &unix.Winsize{Row: 24, Col: 80})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	resizes := c.Resizes(ctx) // baseline 24x80 is taken here

	// Change the size before ranging starts. No SIGWINCH is sent to this
	// process: TIOCSWINSZ on Linux signals a pty's foreground process
	// group, and this test process was never made that group (the pty was
	// opened with O_NOCTTY and never became a controlling terminal), so
	// the kernel does not deliver one here either (confirmed empirically
	// against this behavior: signal.Notify(SIGWINCH) plus a bare
	// TIOCSWINSZ on such a pty times out with nothing received). The only
	// way the new size can reach the consumer below is the immediate
	// post-Notify check inside Resizes.
	setWinsize(t, slave, &unix.Winsize{Row: 50, Col: 120})

	sizes := make(chan Size, 4)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for sz := range resizes {
			sizes <- sz
		}
	}()

	select {
	case sz := <-sizes:
		if sz != (Size{Rows: 50, Cols: 120}) {
			t.Fatalf("Resizes yielded %+v, want 50x120", sz)
		}
	case <-time.After(waitTimeout):
		t.Fatal("Resizes never yielded the size that changed before ranging started")
	}
	cancel()
	waitDone(t, done, "Resizes after cancel")
}

// waitDone waits, bounded, for a consumer goroutine to close done.
func waitDone(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(waitTimeout):
		t.Fatalf("%s: the range over Resizes never finished", what)
	}
}

// TestResizesStopsAfterCancel checks that nothing is yielded once ctx has
// ended, even a change made before it ended: here the size changes and ctx
// is cancelled before ranging starts, so the check Resizes makes right
// after registering for SIGWINCH sees a change it must not yield.
func TestResizesStopsAfterCancel(t *testing.T) {
	_, slave := openPTY(t)
	c := mustConsole(t, slave)
	setWinsize(t, slave, &unix.Winsize{Row: 24, Col: 80})

	ctx, cancel := context.WithCancel(t.Context())
	resizes := c.Resizes(ctx) // baseline 24x80 is taken here
	setWinsize(t, slave, &unix.Winsize{Row: 50, Col: 120})
	cancel()

	sizes := make(chan Size, 4)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for sz := range resizes {
			sizes <- sz
		}
	}()
	waitDone(t, done, "cancelled Resizes")
	if len(sizes) != 0 {
		t.Fatalf("Resizes yielded %+v after its context ended", <-sizes)
	}
}

// TestResizesStopsWhenConsumerBreaks checks that breaking out of the range
// ends it while ctx is still live, at both yield sites: the check right
// after SIGWINCH registration (a change made before ranging starts) and
// the signal loop (a change signalled while ranging).
func TestResizesStopsWhenConsumerBreaks(t *testing.T) {
	winch := func(t *testing.T) {
		t.Helper()
		if err := unix.Kill(os.Getpid(), unix.SIGWINCH); err != nil {
			t.Fatalf("kill: %v", err)
		}
	}
	for _, tc := range []struct {
		name string
		// inLoop changes the size only after ranging has run for a while,
		// so the signal loop yields it rather than the post-registration
		// check.
		inLoop bool
	}{
		{name: "post_registration_check"},
		{name: "signal_loop", inLoop: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, slave := openPTY(t)
			c := mustConsole(t, slave)
			setWinsize(t, slave, &unix.Winsize{Row: 24, Col: 80})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel() // ctx stays live until the test has checked
			resizes := c.Resizes(ctx)
			if !tc.inLoop {
				setWinsize(t, slave, &unix.Winsize{Row: 50, Col: 120})
			}

			got := make(chan Size, 1)
			done := make(chan struct{})
			go func() {
				defer close(done)
				for sz := range resizes {
					got <- sz
					break
				}
			}()

			if tc.inLoop {
				// Signal an unchanged size for 300 ms so the post-registration
				// check has certainly run and found nothing, as in
				// TestResizesYieldsChangesOnly, then change it.
				tick := time.NewTicker(20 * time.Millisecond)
				defer tick.Stop()
				quiet := time.After(300 * time.Millisecond)
			settle:
				for {
					winch(t)
					select {
					case sz := <-got:
						t.Fatalf("an unchanged size was yielded: %+v", sz)
					case <-tick.C:
					case <-quiet:
						break settle
					}
				}
				setWinsize(t, slave, &unix.Winsize{Row: 50, Col: 120})
				deadline := time.After(waitTimeout)
			signal:
				for {
					winch(t)
					select {
					case sz := <-got:
						got <- sz // hand it on to the check below
						break signal
					case <-tick.C:
					case <-deadline:
						t.Fatal("Resizes never yielded the new size")
					}
				}
			}

			select {
			case sz := <-got:
				if sz != (Size{Rows: 50, Cols: 120}) {
					t.Fatalf("Resizes yielded %+v, want 50x120", sz)
				}
			case <-time.After(waitTimeout):
				t.Fatal("Resizes never yielded the new size")
			}
			waitDone(t, done, "break with ctx live")
		})
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
