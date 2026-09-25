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

// Write progress also keeps a blocked read alive: in the recover exchange
// the reader may wait for the peer's catchup while our own catchup is still
// flowing out. A read that outlasts the idle timeout must not fail while
// writes progress, and must fail once they stop.
func TestIdleConnWriteProgressKeepsReadAlive(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, server := net.Pipe()
		defer func() { _ = client.Close() }()
		defer func() { _ = server.Close() }()

		ic := idleConn{Conn: client, timeout: handshakeIdle}
		readErr := make(chan error, 1)
		go func() {
			_, err := io.ReadFull(ic, make([]byte, 1)) // the peer never writes
			readErr <- err
		}()
		go func() { // the peer drains 8 KiB/s
			buf := make([]byte, 8<<10)
			for {
				if _, err := server.Read(buf); err != nil {
					return
				}
				time.Sleep(time.Second)
			}
		}()

		const total = 512 << 10 // 64 s at 8 KiB/s, past the 30 s timeout
		if _, err := ic.Write(make([]byte, total)); err != nil {
			t.Fatalf("Write: %v", err)
		}
		select {
		case err := <-readErr:
			t.Fatalf("read failed with %v while writes were still progressing", err)
		default:
		}
		select {
		case err := <-readErr:
			if !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("read error = %v, want os.ErrDeadlineExceeded once writes stop", err)
			}
		case <-time.After(time.Minute):
			t.Fatal("read never timed out after writes stopped")
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
