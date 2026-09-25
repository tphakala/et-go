package wire

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"runtime"
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

// shortWriter reports writing one byte less than it was given and no error,
// which breaks the io.Writer contract.
type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) {
	return max(len(p)-1, 0), nil
}

// wrappedEOFReader returns all of data in one Read together with an io.EOF
// wrapped in another error, as some io.Reader implementations do.
type wrappedEOFReader struct {
	data []byte
}

func (r *wrappedEOFReader) Read(p []byte) (int, error) {
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, fmt.Errorf("wrappedEOFReader: %w", io.EOF)
}

// allocatedBytes returns the heap bytes f allocates, measured as the
// difference in runtime.MemStats.TotalAlloc.
func allocatedBytes(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}
