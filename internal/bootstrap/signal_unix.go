//go:build unix

package bootstrap

import (
	"os/exec"
	"strconv"
	"syscall"
)

// killedBy reports the signal that ended the process behind exitErr, as
// "<number> (<description>)", and whether a signal ended it at all.
func killedBy(exitErr *exec.ExitError) (string, bool) {
	ws, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		return "", false
	}
	sig := ws.Signal()
	return strconv.Itoa(int(sig)) + " (" + sig.String() + ")", true
}
