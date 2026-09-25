package main

import (
	"bytes"
	"context"
	"os/exec"
	"time"
)

// resolveTimeout bounds the local "ssh -G" query.
//
//nolint:unused // consumed by Task 6, which wires resolveHost into run.
const resolveTimeout = 5 * time.Second

// resolveHost returns the host name OpenSSH would connect to for this
// destination, so an ssh_config alias ("Host box / HostName 10.0.0.5") works
// for the etserver TCP connection too. "ssh -G" prints the effective client
// configuration without connecting. On any failure it returns host unchanged.
//
//nolint:unused // consumed by Task 6, which wires resolveHost into run.
func resolveHost(ctx context.Context, sshPath, user, host string) string {
	target := host
	if user != "" {
		target = user + "@" + host
	}
	ctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, sshPath, "-G", target).Output()
	if err != nil {
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
