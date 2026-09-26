//go:build unix

package bootstrap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestRunReportsKillingSignal: an ssh that dies by a signal has no exit
// status, so the error names the signal instead of "exited with status -1".
func TestRunReportsKillingSignal(t *testing.T) {
	cfg, _ := useFakeSSH(t, "sigterm")
	_, err := Run(t.Context(), cfg)
	if !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("Run() error = %v, want ErrNoCredentials", err)
	}
	want := "ssh was killed by signal " + strconv.Itoa(int(syscall.SIGTERM)) + " (" + syscall.SIGTERM.String() + ")"
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("Run() error = %q, want it to contain %q", err, want)
	}
	if strings.Contains(err.Error(), "status -1") {
		t.Fatalf("Run() error = %q reports a status for a signalled ssh", err)
	}
}

// TestRunCancelInterruptsThenKills pins both halves of Run's cancellation: ssh
// is sent os.Interrupt first (so it can restore the terminal), and a child that
// survives the interrupt is still killed once waitDelay has passed. Windows
// cannot deliver os.Interrupt to another process, so this test is Unix only.
func TestRunCancelInterruptsThenKills(t *testing.T) {
	cfg, _ := useFakeSSH(t, "trap-int")
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	signaled := filepath.Join(dir, "signaled")
	if err := syscall.Mkfifo(ready, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	t.Setenv(fakeSSHReadyEnv, ready)
	t.Setenv(fakeSSHSignaledEnv, signaled)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	type result struct{ err error }
	done := make(chan result, 1)
	go func() {
		_, err := Run(ctx, cfg)
		done <- result{err}
	}()

	// Opening the FIFO for reading blocks until the fake opens it for
	// writing, which it does only after installing its interrupt handler.
	opened := make(chan error, 1)
	go func() {
		f, err := os.Open(ready)
		if err == nil {
			_ = f.Close()
		}
		opened <- err
	}()
	bound := time.NewTimer(15 * time.Second)
	defer bound.Stop()
	select {
	case err := <-opened:
		if err != nil {
			t.Fatalf("open ready FIFO: %v", err)
		}
	case r := <-done:
		t.Fatalf("Run() returned %v before the fake was ready", r.err)
	case <-bound.C:
		t.Fatal("the fake never opened the ready FIFO")
	}

	cancel()
	select {
	case r := <-done:
		if !errors.Is(r.err, context.Canceled) {
			t.Fatalf("Run() error = %v, want context.Canceled", r.err)
		}
	case <-bound.C:
		t.Fatal("Run() did not return after cancel; a child that ignores the interrupt must be killed after waitDelay")
	}
	if _, err := os.Stat(signaled); err != nil {
		t.Fatalf("the fake never received os.Interrupt: %v", err)
	}
}
