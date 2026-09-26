//go:build !unix

package bootstrap

import "os/exec"

// killedBy reports false: outside Unix a process has no terminating signal
// to name, so describeFailure falls back to the exit status.
func killedBy(*exec.ExitError) (string, bool) { return "", false }
