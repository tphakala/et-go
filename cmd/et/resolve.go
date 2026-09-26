package main

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"time"
)

const (
	// resolveTimeout bounds the local "ssh -G" query.
	resolveTimeout = 5 * time.Second
	// resolveWaitDelay bounds how long the query waits for ssh's stdout to
	// close after ssh exits or is killed, in case a descendant (a Match exec
	// helper, say) inherited it.
	resolveWaitDelay = time.Second
)

// resolveHost returns the host name OpenSSH would connect to for this
// destination, so an ssh_config alias ("Host box / HostName 10.0.0.5") works
// for the etserver TCP connection too. "ssh -G" prints the effective client
// configuration without connecting. The arguments have the shape bootstrap
// gives the ssh that starts etterminal: each of opts as "-o<opt>", the user
// as "-l <user>", and "--" before the host, so an option such as HostName
// and a user name holding '@' resolve as they do there.
// On any failure it returns host unchanged.
func resolveHost(ctx context.Context, sshPath, user, host string, opts []string) string {
	args := make([]string, 0, len(opts)+5)
	for _, opt := range opts {
		args = append(args, "-o"+opt)
	}
	if user != "" {
		args = append(args, "-l", user)
	}
	args = append(args, "-G", "--", host)
	ctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, sshPath, args...)
	cmd.WaitDelay = resolveWaitDelay
	out, err := cmd.Output()
	// ErrWaitDelay means ssh finished but a descendant held stdout open; the
	// output ssh printed is complete, as in bootstrap.Run.
	if err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		return host
	}
	if h := sshConfigHostname(out); h != "" {
		return h
	}
	return host
}

// sshConfigHostname extracts the "hostname" line from "ssh -G" output.
func sshConfigHostname(out []byte) string {
	for line := range bytes.Lines(out) {
		key, val, ok := bytes.Cut(bytes.TrimSpace(line), []byte(" "))
		if ok && string(key) == "hostname" {
			return string(bytes.TrimSpace(val))
		}
	}
	return ""
}
