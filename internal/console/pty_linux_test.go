package console

import (
	"fmt"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

// openPTY returns a pseudo-terminal pair using only x/sys ioctls, so the
// console tests need no extra module and no controlling terminal. Both ends
// are closed when the test ends.
func openPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatalf("open /dev/ptmx: %v", err)
	}
	t.Cleanup(func() { _ = master.Close() })

	var n int
	err = controlFile(master, func(fd int) error {
		if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
			return fmt.Errorf("unlockpt: %w", err)
		}
		var gerr error
		n, gerr = unix.IoctlGetInt(fd, unix.TIOCGPTN)
		return gerr
	})
	if err != nil {
		t.Fatalf("pty setup: %v", err)
	}
	slave, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatalf("open pty slave: %v", err)
	}
	t.Cleanup(func() { _ = slave.Close() })
	return master, slave
}

// termios reads the terminal attributes of f.
func termios(t *testing.T, f *os.File) *unix.Termios {
	t.Helper()
	var tio *unix.Termios
	err := controlFile(f, func(fd int) error {
		var gerr error
		tio, gerr = unix.IoctlGetTermios(fd, unix.TCGETS)
		return gerr
	})
	if err != nil {
		t.Fatalf("tcgets: %v", err)
	}
	return tio
}
