package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// defaultPort is etserver's default TCP port.
const defaultPort = 2022

// errUsage marks command-line mistakes; run exits with status 2 for them.
var errUsage = errors.New("usage")

// options is the parsed command line.
type options struct {
	dest         destination
	terminalPath string
	sshOptions   []string
	keepAlive    time.Duration
	verbose      bool
	logFile      string
	version      bool
}

// destination is the parsed [user@]host[:port] argument.
type destination struct {
	User string
	Host string
	Port int // 0 when the argument has no explicit port
}

// stringList is a repeatable string flag.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// parseArgs parses the command line. It returns flag.ErrHelp for -h, and an
// error wrapping errUsage for any other command-line mistake.
func parseArgs(args []string, stderr io.Writer) (*options, error) {
	fs := flag.NewFlagSet("et", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		_, _ = fmt.Fprintf(stderr, "Usage: et [flags] [user@]host[:port]\n\nFlags:\n")
		fs.PrintDefaults()
	}

	var (
		user, terminalPath, logFile string
		port, keepAlive             int
		verbose, showVersion        bool
		sshOptions                  stringList
	)
	// Each short name is an alias of the long one: both write the same variable.
	fs.StringVar(&user, "u", "", "remote user (same as --username)")
	fs.StringVar(&user, "username", "", "remote user")
	fs.IntVar(&port, "p", defaultPort, "etserver port (same as --port)")
	fs.IntVar(&port, "port", defaultPort, "etserver port")
	fs.StringVar(&terminalPath, "terminal-path", "", "etterminal path on the server")
	fs.Var(&sshOptions, "ssh-option", "extra ssh -o option (repeatable)")
	fs.IntVar(&keepAlive, "k", 5, "keepalive seconds, 1-5 (same as --keepalive)")
	fs.IntVar(&keepAlive, "keepalive", 5, "keepalive seconds, 1-5")
	fs.BoolVar(&verbose, "v", false, "write a debug log (same as --verbose)")
	fs.BoolVar(&verbose, "verbose", false, "write a debug log (see --log-file)")
	fs.StringVar(&logFile, "log-file", "", "log file path")
	fs.BoolVar(&showVersion, "version", false, "print the version and exit")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %w", errUsage, err)
	}
	if showVersion {
		return &options{version: true}, nil
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return nil, fmt.Errorf("%w: expected exactly one destination, got %d arguments", errUsage, fs.NArg())
	}

	dest, err := parseDestination(fs.Arg(0))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errUsage, err)
	}
	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })

	// An explicit :port in the destination wins over the -p default; both
	// given and different is an error. The same rule applies to the user.
	portSet := explicit["p"] || explicit["port"]
	switch {
	case dest.Port != 0 && portSet && dest.Port != port:
		return nil, fmt.Errorf("%w: port %d in the destination conflicts with -p %d", errUsage, dest.Port, port)
	case dest.Port == 0:
		dest.Port = port
	}
	if dest.Port < 1 || dest.Port > 65535 {
		return nil, fmt.Errorf("%w: port %d out of range", errUsage, dest.Port)
	}
	switch {
	case dest.User != "" && user != "" && dest.User != user:
		return nil, fmt.Errorf("%w: user %q in the destination conflicts with -u %q", errUsage, dest.User, user)
	case dest.User == "":
		dest.User = user
	}
	if keepAlive < 1 || keepAlive > 5 {
		return nil, fmt.Errorf("%w: keepalive must be 1-5 seconds, got %d", errUsage, keepAlive)
	}

	return &options{
		dest:         dest,
		terminalPath: terminalPath,
		sshOptions:   sshOptions,
		keepAlive:    time.Duration(keepAlive) * time.Second,
		verbose:      verbose,
		logFile:      logFile,
	}, nil
}

// parseDestination parses [user@]host[:port]. IPv6 literals need brackets to
// carry a port ("[::1]:2022"); a bare IPv6 literal ("::1") has no port.
func parseDestination(s string) (destination, error) {
	var d destination
	if before, after, found := strings.CutLast(s, "@"); found {
		if before == "" {
			return d, fmt.Errorf("empty user in destination %q", s)
		}
		d.User, s = before, after
	}
	var portText string
	switch {
	case strings.HasPrefix(s, "["):
		host, rest, ok := strings.Cut(s[1:], "]")
		if !ok {
			return d, fmt.Errorf("missing ] in destination %q", s)
		}
		d.Host = host
		if rest != "" {
			p, ok := strings.CutPrefix(rest, ":")
			if !ok {
				return d, fmt.Errorf("unexpected %q after ] in destination", rest)
			}
			portText = p
		}
	case strings.Count(s, ":") > 1:
		d.Host = s // bare IPv6 literal
	default:
		host, p, _ := strings.Cut(s, ":")
		d.Host, portText = host, p
	}
	if d.Host == "" {
		return d, fmt.Errorf("empty host in destination")
	}
	if portText != "" {
		p, err := strconv.Atoi(portText)
		if err != nil || p < 1 || p > 65535 {
			return d, fmt.Errorf("invalid port %q in destination", portText)
		}
		d.Port = p
	}
	return d, nil
}
