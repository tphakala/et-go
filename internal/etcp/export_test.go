package etcp

// SetMaxCatchupSize lowers the catchup size limit for a test, so the limit
// can be reached without writing 128 MiB, and returns a function that
// restores it.
func SetMaxCatchupSize(n int) (restore func()) {
	old := maxCatchupSize
	maxCatchupSize = n
	return func() { maxCatchupSize = old }
}
