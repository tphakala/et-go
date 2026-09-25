package main

import "testing"

func TestSSHConfigHostname(t *testing.T) {
	out := []byte("user me\nhostname 10.0.0.5\nport 22\n")
	if got := sshConfigHostname(out); got != "10.0.0.5" {
		t.Fatalf("got %q", got)
	}
	if got := sshConfigHostname([]byte("user me\n")); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}
