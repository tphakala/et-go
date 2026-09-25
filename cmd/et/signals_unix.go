//go:build unix

package main

import (
	"os"
	"syscall"
)

// shutdownSignals end the client. In raw mode the terminal sends Ctrl+C as a
// byte, so os.Interrupt only matters before and after the session runs.
var shutdownSignals = []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP}
