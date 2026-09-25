package etcp

import (
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"testing/synctest"
	"time"
)

// A slow but steady peer must never trip the idle timeout, however long the
// whole transfer takes. Before chunking, one deadline covered the entire
// Write and this failed after handshakeIdle.
func TestIdleConnSlowSteadyPeer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, server := net.Pipe()
		defer func() { _ = client.Close() }()
		defer func() { _ = server.Close() }()

		const total = 1 << 20 // 1 MiB at 8 KiB/s takes 128 s, far past the 30 s timeout
		done := make(chan error, 1)
		go func() {
			buf := make([]byte, 8<<10)
			for read := 0; read < total; {
				n, err := server.Read(buf)
				if err != nil {
					done <- err
					return
				}
				read += n
				time.Sleep(time.Second)
			}
			done <- nil
		}()

		start := time.Now()
		ic := idleConn{Conn: client, timeout: handshakeIdle}
		if _, err := ic.Write(make([]byte, total)); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if err := <-done; err != nil {
			t.Fatalf("reader: %v", err)
		}
		if elapsed := time.Since(start); elapsed <= handshakeIdle {
			t.Fatalf("transfer took %v; the test needs it to outlast the %v timeout", elapsed, handshakeIdle)
		}
	})
}

func TestIdleConnStalledPeer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, server := net.Pipe()
		defer func() { _ = client.Close() }()
		defer func() { _ = server.Close() }()

		ic := idleConn{Conn: client, timeout: handshakeIdle}
		start := time.Now()
		_, err := ic.Write([]byte("nobody reads this"))
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("Write error = %v, want os.ErrDeadlineExceeded", err)
		}
		if elapsed := time.Since(start); elapsed != handshakeIdle {
			t.Fatalf("timed out after %v, want %v", elapsed, handshakeIdle)
		}

		start = time.Now()
		_, err = io.ReadFull(ic, make([]byte, 1))
		if !errors.Is(err, os.ErrDeadlineExceeded) || time.Since(start) != handshakeIdle {
			t.Fatalf("Read error = %v after %v, want os.ErrDeadlineExceeded after %v", err, time.Since(start), handshakeIdle)
		}
	})
}
