package main

import (
	"errors"
	"flag"
	"io"
	"testing"
	"time"
)

// Review Focus 4: every destination form, and -p / -u precedence.
func TestParseArgsDestination(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    destination
		wantErr bool
	}{
		{name: "host", args: []string{"box"}, want: destination{Host: "box", Port: 2022}},
		{name: "user@host", args: []string{"me@box"}, want: destination{User: "me", Host: "box", Port: 2022}},
		{name: "host:port", args: []string{"box:2200"}, want: destination{Host: "box", Port: 2200}},
		{name: "user@host:port", args: []string{"me@box:2200"}, want: destination{User: "me", Host: "box", Port: 2200}},
		{name: "fqdn", args: []string{"box.example.org"}, want: destination{Host: "box.example.org", Port: 2022}},
		{name: "ipv4 with port", args: []string{"10.0.0.5:2022"}, want: destination{Host: "10.0.0.5", Port: 2022}},
		{name: "bracketed ipv6", args: []string{"[::1]"}, want: destination{Host: "::1", Port: 2022}},
		{name: "bracketed ipv6 zone and port", args: []string{"[fe80::1%eth0]:2200"}, want: destination{Host: "fe80::1%eth0", Port: 2200}},
		{name: "bare ipv6 has no port", args: []string{"fe80::1:2200"}, want: destination{Host: "fe80::1:2200", Port: 2022}},
		{name: "user@bracketed ipv6", args: []string{"me@[::1]:2200"}, want: destination{User: "me", Host: "::1", Port: 2200}},
		{name: "-p sets the port", args: []string{"-p", "2200", "box"}, want: destination{Host: "box", Port: 2200}},
		{name: "--port sets the port", args: []string{"--port", "2200", "box"}, want: destination{Host: "box", Port: 2200}},
		{name: "explicit :port wins over -p default", args: []string{"box:2300"}, want: destination{Host: "box", Port: 2300}},
		{name: "-p equal to :port is fine", args: []string{"-p", "2300", "box:2300"}, want: destination{Host: "box", Port: 2300}},
		{name: "-p conflicting with :port", args: []string{"-p", "2200", "box:2300"}, wantErr: true},
		{name: "-u sets the user", args: []string{"-u", "me", "box"}, want: destination{User: "me", Host: "box", Port: 2022}},
		{name: "-u conflicting with user@", args: []string{"-u", "you", "me@box"}, wantErr: true},
		{name: "empty user", args: []string{"@box"}, wantErr: true},
		{name: "empty host", args: []string{"me@"}, wantErr: true},
		{name: "bad port", args: []string{"box:http"}, wantErr: true},
		{name: "port out of range", args: []string{"box:70000"}, wantErr: true},
		{name: "unclosed bracket", args: []string{"[::1"}, wantErr: true},
		{name: "junk after bracket", args: []string{"[::1]x"}, wantErr: true},
		{name: "no destination", args: []string{}, wantErr: true},
		{name: "two destinations", args: []string{"a", "b"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o, err := parseArgs(tt.args, io.Discard)
			if tt.wantErr {
				if !errors.Is(err, errUsage) {
					t.Fatalf("parseArgs(%q) error = %v, want errUsage", tt.args, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseArgs(%q): %v", tt.args, err)
			}
			if o.dest != tt.want {
				t.Fatalf("parseArgs(%q) dest = %+v, want %+v", tt.args, o.dest, tt.want)
			}
		})
	}
}

func TestParseArgsOptions(t *testing.T) {
	o, err := parseArgs([]string{
		"-k", "2", "--terminal-path", "/opt/et/etterminal",
		"--ssh-option", "BatchMode=yes", "--ssh-option", "ConnectTimeout=5",
		"-v", "--log-file", "/tmp/et.log", "box",
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if o.keepAlive != 2*time.Second || o.terminalPath != "/opt/et/etterminal" ||
		len(o.sshOptions) != 2 || o.sshOptions[1] != "ConnectTimeout=5" ||
		!o.verbose || o.logFile != "/tmp/et.log" {
		t.Fatalf("options = %+v", o)
	}
}

func TestParseArgsKeepAliveRange(t *testing.T) {
	for _, k := range []string{"0", "6"} {
		if _, err := parseArgs([]string{"-k", k, "box"}, io.Discard); !errors.Is(err, errUsage) {
			t.Errorf("-k %s: error = %v, want errUsage", k, err)
		}
	}
}

func TestParseArgsHelpAndVersion(t *testing.T) {
	if _, err := parseArgs([]string{"-h"}, io.Discard); !errors.Is(err, flag.ErrHelp) {
		t.Errorf("-h: error = %v, want flag.ErrHelp", err)
	}
	o, err := parseArgs([]string{"--version"}, io.Discard)
	if err != nil || !o.version {
		t.Errorf("--version: %+v, %v", o, err)
	}
}
