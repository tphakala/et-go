package etcp

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// shortWriter reports one byte less than it was given and no error, which
// breaks the io.Writer contract.
type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return max(len(p)-1, 0), nil }

// A writer that reports a short count without an error must fail the link
// rather than count the whole batch as sent: the unsent tail would otherwise
// be lost from replay.
func TestWriteLoopShortWrite(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var d Dialer
		c := d.newConn("et.example:2022", "XXXtestclient001", strings.Repeat("k", 32))
		defer c.cancel(nil)
		c.ring.push(make([]byte, 18))
		c.unsent += 18

		// Bounded: a writeLoop that accepted the short write would wait for
		// more data forever.
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		err := c.writeLoop(ctx, shortWriter{})
		if !errors.Is(err, io.ErrShortWrite) {
			t.Fatalf("writeLoop = %v, want io.ErrShortWrite", err)
		}
		if c.flushed != 0 {
			t.Fatalf("flushed = %d after a short write, want 0", c.flushed)
		}
	})
}
