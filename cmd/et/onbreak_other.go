//go:build !windows

package main

// onBreak is a no-op outside Windows: there is no Ctrl+Break event.
func onBreak(func()) {}
