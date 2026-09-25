package bootstrap

import (
	"errors"
	"slices"
	"testing"
)

func TestValidate(t *testing.T) {
	valid := Config{Destination: "example.test"}
	valid.applyDefaults()

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
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
		{"user with at", func(c *Config) { c.User = "a@b" }, true},
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
	for _, tt := range tests {
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
			want: []string{"host", "REMOTE"},
		},
		{
			name: "user and options",
			cfg:  Config{Destination: "host", User: "alice", SSHOptions: []string{"BatchMode=yes", "Port=2222"}},
			want: []string{"-oBatchMode=yes", "-oPort=2222", "alice@host", "REMOTE"},
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
