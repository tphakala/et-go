package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestSSHConfigHostname(t *testing.T) {
	out := []byte("user me\nhostname 10.0.0.5\nport 22\n")
	if got := sshConfigHostname(out); got != "10.0.0.5" {
		t.Fatalf("got %q", got)
	}
	if got := sshConfigHostname([]byte("user me\n")); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

// fakeSSH writes a shell script standing in for ssh and returns its path.
func fakeSSH(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake ssh is a POSIX shell script")
	}
	path := filepath.Join(t.TempDir(), "ssh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestResolveHostPassesOptions pins the exact ssh -G argument list, which has
// the shape bootstrap gives the ssh that starts etterminal: each option as
// its own "-o<opt>" word, the user as "-l <user>", then "--" before the host.
// The fake prints its arguments joined by "|" as the hostname, so an option
// containing a space stays one word.
func TestResolveHostPassesOptions(t *testing.T) {
	ssh := fakeSSH(t, `printf 'user x\nhostname '; printf '%s|' "$@"; printf '\n'`)
	tests := []struct {
		name string
		user string
		opts []string
		want string
	}{
		{name: "no options", want: "-G|--|box|"},
		{name: "user", user: "me", want: "-l|me|-G|--|box|"},
		{name: "user with at", user: "me@corp.example", want: "-l|me@corp.example|-G|--|box|"},
		{
			name: "options",
			user: "me",
			opts: []string{"HostName=10.0.0.5", "ProxyCommand=nc a b"},
			want: "-oHostName=10.0.0.5|-oProxyCommand=nc a b|-l|me|-G|--|box|",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveHost(t.Context(), ssh, tt.user, "box", tt.opts); got != tt.want {
				t.Fatalf("resolveHost = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestResolveHostFallback pins that a failing ssh leaves the typed host.
func TestResolveHostFallback(t *testing.T) {
	failing := fakeSSH(t, "exit 1\n")
	missing := filepath.Join(t.TempDir(), "no-such-ssh")
	for _, ssh := range []string{failing, missing} {
		if got := resolveHost(t.Context(), ssh, "", "box", []string{"HostName=10.0.0.5"}); got != "box" {
			t.Errorf("resolveHost with ssh %q = %q, want the host unchanged", ssh, got)
		}
	}
}

// TestResolveHostDescendantHoldsStdout pins resolveWaitDelay: when ssh exits
// but a child it started keeps stdout open, the query still returns promptly
// and uses what ssh printed.
func TestResolveHostDescendantHoldsStdout(t *testing.T) {
	ssh := fakeSSH(t, "printf 'hostname 10.0.0.9\\n'\nsleep 4 &\nexit 0\n")
	begin := time.Now()
	got := resolveHost(t.Context(), ssh, "", "box", nil)
	if elapsed := time.Since(begin); elapsed > 3*time.Second {
		t.Fatalf("resolveHost took %v with a descendant holding stdout, want about resolveWaitDelay (%v)", elapsed, resolveWaitDelay)
	}
	if got != "10.0.0.9" {
		t.Fatalf("resolveHost = %q, want the hostname ssh printed", got)
	}
}
