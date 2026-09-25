package wire

import (
	"bytes"
	"errors"
)

// errWriter is an io.Writer that always fails, counting how many times
// Write was called.
type errWriter struct {
	err   error
	calls int
}

func (w *errWriter) Write(p []byte) (int, error) {
	w.calls++
	return 0, w.err
}

// errWriterSentinel is the error errWriter returns, wrapped by the
// production code under test.
var errWriterSentinel = errors.New("wire_test: writer error")

// countingWriter is an io.Writer that always succeeds, counting how many
// times Write was called. WriteFrame and WriteMessage each build their
// whole output (length prefix and body) in one buffer and issue a single
// Write; etcp relies on that, since a partial write on a real connection
// could otherwise interleave with another goroutine's frame.
type countingWriter struct {
	buf   bytes.Buffer
	calls int
}

// lenWriter counts the bytes written to it without keeping them, for tests
// that write messages too large to buffer twice.
type lenWriter struct {
	n int
}

func (w *lenWriter) Write(p []byte) (int, error) {
	w.n += len(p)
	return len(p), nil
}

func (w *countingWriter) Write(p []byte) (int, error) {
	w.calls++
	return w.buf.Write(p)
}
