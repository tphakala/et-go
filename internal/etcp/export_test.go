package etcp

// SetMaxCatchupSize lowers the catchup size limit for a test, so the limit
// can be reached without writing 128 MiB, and returns a function that
// restores it.
func SetMaxCatchupSize(n int) (restore func()) {
	old := maxCatchupSize
	maxCatchupSize = n
	return func() { maxCatchupSize = old }
}

// RingBytes reports how many bytes of sealed packets c retains for replay.
func RingBytes(c *Conn) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ring.bytes
}

// Unsent reports the bytes of sealed packets c has not yet written to a link.
func Unsent(c *Conn) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.unsent
}
