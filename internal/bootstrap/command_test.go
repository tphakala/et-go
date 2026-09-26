package bootstrap

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	valid := Config{Destination: "example.test"}
	valid.applyDefaults()

	type validateCase struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}
	tests := []validateCase{
		{"defaults", func(*Config) {}, false},
		{"alias with dots and dashes", func(c *Config) { c.Destination = "my-host.lan" }, false},
		{"user", func(c *Config) { c.User = "alice" }, false},
		{"term with plus", func(c *Config) { c.Term = "rxvt-unicode+256color" }, false},
		{"absolute terminal path", func(c *Config) { c.TerminalPath = "/usr/local/bin/etterminal" }, false},
		{"home terminal path", func(c *Config) { c.TerminalPath = "~/bin/etterminal" }, false},
		{"ssh options", func(c *Config) { c.SSHOptions = []string{"BatchMode=yes", "Port 2222"} }, false},
		{"empty destination", func(c *Config) { c.Destination = "" }, true},
		{"destination is an option", func(c *Config) { c.Destination = "-oProxyCommand=evil" }, true},
		{"destination with space", func(c *Config) { c.Destination = "a b" }, true},
		{"destination with control", func(c *Config) { c.Destination = "host\x01" }, true},
		{"IPv6", func(c *Config) { c.Destination = "::1" }, false},
		{"IPv6 with zone", func(c *Config) { c.Destination = "fe80::1%eth0" }, false},
		{"ssh URI with port", func(c *Config) { c.Destination = "ssh://host:2222" }, false},
		{"dotted alias", func(c *Config) { c.Destination = "my-host.example" }, false},
		{"underscore host", func(c *Config) { c.Destination = "host_1" }, false},
		{"user with at", func(c *Config) { c.User = "alice@corp.example" }, false},
		{"user with dot dash underscore", func(c *Config) { c.User = "a.b-c_d" }, false},
		{"user with space", func(c *Config) { c.User = "a b" }, true},
		{"user with control", func(c *Config) { c.User = "a\x01" }, true},
		{"user is an option", func(c *Config) { c.User = "-x" }, true},
		{"destination with user and no User", func(c *Config) { c.Destination = "alice@host" }, false},
		{"User and a user in destination", func(c *Config) { c.User = "bob"; c.Destination = "alice@host" }, true},
		{"term with underscore", func(c *Config) { c.Term = "xterm_256color" }, true},
		{"term with quote", func(c *Config) { c.Term = "xterm'; rm -rf ~; '" }, true},
		{"term with space", func(c *Config) { c.Term = "xterm 256" }, true},
		{"term with dollar", func(c *Config) { c.Term = "$(id)" }, true},
		{"terminal path with semicolon", func(c *Config) { c.TerminalPath = "etterminal;id" }, true},
		{"terminal path with space", func(c *Config) { c.TerminalPath = "/opt/my et/etterminal" }, true},
		{"empty ssh option", func(c *Config) { c.SSHOptions = []string{""} }, true},
		{"ssh option with newline", func(c *Config) { c.SSHOptions = []string{"A=b\nc"} }, true},
	}
	// Spelled out rather than taken from shellMeta and userMeta, so dropping
	// a character from a production set turns a row red.
	const (
		wantDestMeta = "'`\"$\\;&<>|(){}*?[]"
		wantUserMeta = "'`\";&<>|(){}"
	)
	metaCases := make([]validateCase, 0, len(wantDestMeta)+len(wantUserMeta))
	for _, r := range wantDestMeta {
		metaCases = append(metaCases,
			validateCase{"destination with " + string(r), func(c *Config) { c.Destination = "h" + string(r) + "x.example" }, true})
	}
	for _, r := range wantUserMeta {
		metaCases = append(metaCases,
			validateCase{"user with " + string(r), func(c *Config) { c.User = "a" + string(r) + "b" }, true})
	}
	// User names OpenSSH accepts and main accepted: a winbind DOMAIN\user and
	// a Samba machine account. A trailing backslash is refused, as OpenSSH
	// refuses it.
	metaCases = append(metaCases,
		validateCase{"winbind user", func(c *Config) { c.User = `CORP\alice` }, false},
		validateCase{"machine account user", func(c *Config) { c.User = "host$" }, false},
		validateCase{"user with dollar inside", func(c *Config) { c.User = "a$b" }, false},
		validateCase{"user ending in backslash", func(c *Config) { c.User = `alice\` }, true},
	)
	for i, tt := range slices.Concat(tests, metaCases) {
		isMeta := i >= len(tests)
		t.Run(tt.name, func(t *testing.T) {
			cfg := valid
			cfg.SSHOptions = slices.Clone(valid.SSHOptions)
			tt.mutate(&cfg)
			err := cfg.validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("validate() = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("validate() = %v, want it to wrap ErrInvalidConfig", err)
			}
			// A metacharacter row must fail for the metacharacter, not for
			// some other rule that happens to reject the same value.
			if isMeta && tt.wantErr &&
				!strings.Contains(err.Error(), "shell metacharacter") && !strings.Contains(err.Error(), "backslash") {
				t.Fatalf("validate() = %v, want the metacharacter reason", err)
			}
		})
	}
}

func TestApplyDefaults(t *testing.T) {
	got := Config{Destination: "h"}
	got.applyDefaults()
	if got.Term != "xterm-256color" || got.TerminalPath != "etterminal" {
		t.Fatalf("applyDefaults() gave Term %q TerminalPath %q", got.Term, got.TerminalPath)
	}
	kept := Config{Destination: "h", Term: "screen", TerminalPath: "/x/etterminal"}
	kept.applyDefaults()
	if kept.Term != "screen" || kept.TerminalPath != "/x/etterminal" {
		t.Fatalf("applyDefaults() overwrote set fields: Term %q TerminalPath %q", kept.Term, kept.TerminalPath)
	}
}

func TestRemoteCommand(t *testing.T) {
	got := remoteCommand("XXXabcdefghijklm", "0123456789abcdef0123456789abcdef", "xterm-256color", "etterminal")
	want := "echo 'XXXabcdefghijklm/0123456789abcdef0123456789abcdef_xterm-256color' | etterminal --verbose=0"
	if got != want {
		t.Fatalf("remoteCommand() =\n%s\nwant\n%s", got, want)
	}
}

func TestSSHArgs(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want []string
	}{
		{
			name: "destination only",
			cfg:  Config{Destination: "host"},
			want: []string{"--", "host", "REMOTE"},
		},
		{
			name: "user and options",
			cfg:  Config{Destination: "host", User: "alice", SSHOptions: []string{"BatchMode=yes", "Port=2222"}},
			want: []string{"-oBatchMode=yes", "-oPort=2222", "-l", "alice", "--", "host", "REMOTE"},
		},
		{
			name: "user with at",
			cfg:  Config{Destination: "host", User: "alice@corp.example"},
			want: []string{"-l", "alice@corp.example", "--", "host", "REMOTE"},
		},
		{
			name: "user and ssh URI",
			cfg:  Config{Destination: "ssh://host:2222", User: "bob"},
			want: []string{"-l", "bob", "--", "ssh://host:2222", "REMOTE"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sshArgs(&tt.cfg, "REMOTE"); !slices.Equal(got, tt.want) {
				t.Fatalf("sshArgs() = %q, want %q", got, tt.want)
			}
		})
	}
}
