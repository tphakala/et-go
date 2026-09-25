//go:build e2e

// Package e2e drives the real et binary against the etserver 7.0.0 running
// on this host (systemd unit "et", port 2022), through a pseudo-terminal and
// a TCP proxy that can cut connections. Run with:
//
//	go test -tags e2e -count=1 -v ./test/e2e/
package e2e

import (
	"bytes"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
)

const etserverAddr = "127.0.0.1:2022"

// buildET compiles cmd/et into a temp dir once per test.
func buildET(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "et")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/tphakala/et-go/cmd/et")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build et: %v\n%s", err, out)
	}
	return bin
}

// requireServer skips unless etserver and ssh localhost are usable.
func requireServer(t *testing.T) {
	t.Helper()
	c, err := net.DialTimeout("tcp", etserverAddr, 2*time.Second)
	if err != nil {
		t.Skipf("etserver not reachable at %s: %v", etserverAddr, err)
	}
	_ = c.Close()
	if out, err := exec.Command("ssh", "-o", "BatchMode=yes", "localhost", "true").CombinedOutput(); err != nil {
		t.Skipf("ssh localhost with key auth not available: %v %s", err, out)
	}
}

// proxy forwards TCP connections to etserver and can cut all of them.
type proxy struct {
	ln       net.Listener
	mu       sync.Mutex
	conns    []net.Conn
	accepted int // connections forwarded so far; each reconnect adds one
	wg       sync.WaitGroup
}

// connections returns how many connections the proxy has forwarded.
func (p *proxy) connections() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.accepted
}

// waitConnections waits until the proxy has forwarded at least n connections.
func (p *proxy) waitConnections(t *testing.T, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for p.connections() < n {
		if time.Now().After(deadline) {
			t.Fatalf("proxy forwarded %d connection(s) after %v, want %d: et did not reconnect", p.connections(), timeout, n)
		}
		time.Sleep(50 * time.Millisecond) // real network timing: this is not a synctest test
	}
}

func startProxy(t *testing.T) *proxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &proxy{ln: ln}
	p.wg.Go(p.accept)
	t.Cleanup(func() {
		_ = ln.Close()
		p.cutAll()
		p.wg.Wait()
	})
	return p
}

func (p *proxy) port() int { return p.ln.Addr().(*net.TCPAddr).Port }

func (p *proxy) accept() {
	for {
		down, err := p.ln.Accept()
		if err != nil {
			return
		}
		up, err := net.Dial("tcp", etserverAddr)
		if err != nil {
			_ = down.Close()
			continue
		}
		p.mu.Lock()
		p.conns = append(p.conns, down, up)
		p.accepted++
		p.mu.Unlock()
		p.wg.Go(func() { _, _ = io.Copy(up, down); _ = up.Close() })
		p.wg.Go(func() { _, _ = io.Copy(down, up); _ = down.Close() })
	}
}

// cutAll closes every proxied connection, like a network drop.
func (p *proxy) cutAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		_ = c.Close()
	}
	p.conns = nil
}

// term is et running in a pty, with all output captured.
type term struct {
	t      *testing.T
	f      *os.File
	cmd    *exec.Cmd
	mu     sync.Mutex
	out    bytes.Buffer
	notify chan struct{}
	done   chan error
	wg     sync.WaitGroup
}

func startET(t *testing.T, bin string, args ...string) *term {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 40, Cols: 120})
	if err != nil {
		t.Fatal(err)
	}
	tm := &term{t: t, f: f, cmd: cmd, notify: make(chan struct{}, 1), done: make(chan error, 1)}
	transcript, err := os.Create(filepath.Join(t.ArtifactDir(), "transcript.log"))
	if err != nil {
		t.Fatal(err)
	}
	tm.wg.Go(func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := f.Read(buf)
			if n > 0 {
				_, _ = transcript.Write(buf[:n])
				tm.mu.Lock()
				tm.out.Write(buf[:n])
				tm.mu.Unlock()
				select {
				case tm.notify <- struct{}{}:
				default:
				}
			}
			if err != nil {
				_ = transcript.Close()
				return
			}
		}
	})
	tm.wg.Go(func() { tm.done <- cmd.Wait() })
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = f.Close()
		tm.wg.Wait()
	})
	return tm
}

func (tm *term) send(s string) {
	tm.t.Helper()
	if _, err := tm.f.WriteString(s); err != nil {
		tm.t.Fatalf("write to pty: %v", err)
	}
}

// oscSeq matches OSC escape sequences (ESC ] ... BEL or ESC ] ... ESC \),
// which shells emit to set the window title, for example fish right before
// running a command. They carry no command output.
var oscSeq = regexp.MustCompile("\x1b\\][^\x07\x1b]*(?:\x07|\x1b\\\\)")

// output returns the captured output with CR and OSC sequences removed.
func (tm *term) output() string {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return oscSeq.ReplaceAllString(strings.ReplaceAll(tm.out.String(), "\r", ""), "")
}

// waitFor waits until the cleaned output contains s.
func (tm *term) waitFor(s string, timeout time.Duration) string {
	tm.t.Helper()
	deadline := time.After(timeout)
	for {
		if out := tm.output(); strings.Contains(out, s) {
			return out
		}
		select {
		case <-tm.notify:
		case <-deadline:
			// The artifact dir is removed after the test unless go test runs
			// with -artifacts, so show the end of the output here as well.
			out := tm.output()
			tm.t.Fatalf("timed out after %v waiting for %q; output ends with:\n%s\n(full transcript.log is kept in the artifact dir with go test -artifacts)",
				timeout, s, out[max(0, len(out)-2000):])
		}
	}
}

// probeCmd prints READY without the word appearing in the typed line.
const probeCmd = "printf 'RE%sY\\n' AD\r"

// waitReady waits until the remote shell runs commands. A login shell may
// discard input typed while it starts (fish does), and prompts differ between
// shells, so instead of looking for a prompt it re-sends an idempotent probe
// every 500 ms until the probe's output appears.
func (tm *term) waitReady() {
	tm.t.Helper()
	deadline := time.After(20 * time.Second)
	for {
		tm.send(probeCmd)
		resend := time.After(500 * time.Millisecond)
		for waiting := true; waiting; {
			if strings.Contains(tm.output(), "READY") {
				return
			}
			select {
			case <-tm.notify:
			case <-resend:
				waiting = false
			case <-deadline:
				tm.t.Fatal("remote shell not ready after 20 s; see transcript.log in the artifact dir")
			}
		}
	}
}

// markers print BEGIN and END without either word appearing in the typed
// command line, so the echoed command cannot be mistaken for output.
const (
	beginCmd = `printf 'B%sN\n' EGI`
	endCmd   = `printf 'E%sD\n' N`
)

// checkSeq asserts that the lines between BEGIN and END are exactly 1..n.
// It cuts on "BEGIN\n" rather than "\nBEGIN\n": the shell may emit a title
// sequence right before the output, so the marker need not follow a newline.
func checkSeq(t *testing.T, out string, n int) {
	t.Helper()
	_, afterBegin, ok := strings.Cut(out, "BEGIN\n")
	if !ok {
		t.Fatal("BEGIN marker not found")
	}
	body, _, ok := strings.Cut(afterBegin, "END\n")
	if !ok {
		t.Fatal("END marker not found")
	}
	lines := strings.Split(strings.TrimSuffix(body, "\n"), "\n")
	if len(lines) != n {
		t.Fatalf("got %d lines between markers, want %d", len(lines), n)
	}
	for i, l := range lines {
		if l != strconv.Itoa(i+1) {
			t.Fatalf("line %d is %q, want %d", i+1, l, i+1)
		}
	}
}

func etArgs(p *proxy) []string {
	return []string{"-p", strconv.Itoa(p.port()), "--ssh-option", "BatchMode=yes", "localhost"}
}

func TestEchoAndExit(t *testing.T) {
	requireServer(t)
	bin := buildET(t)
	p := startProxy(t)
	tm := startET(t, bin, etArgs(p)...)
	tm.waitReady()

	tm.send("printf 'ET%sOK\\n' _E2E_\r")
	tm.waitFor("ET_E2E_OK", 30*time.Second)

	tm.send("exit\r")
	select {
	case err := <-tm.done:
		if err != nil {
			t.Fatalf("et exited with %v, want status 0", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("et did not exit within 10 s after the shell exited")
	}
}

func TestSeqByteExact(t *testing.T) {
	requireServer(t)
	bin := buildET(t)
	p := startProxy(t)
	tm := startET(t, bin, etArgs(p)...)
	tm.waitReady()

	tm.send(beginCmd + "; seq 1 200000; " + endCmd + "\r")
	out := tm.waitFor("END\n", 2*time.Minute)
	checkSeq(t, out, 200000)
	tm.send("exit\r")
}

func TestReconnectUnderLoad(t *testing.T) {
	requireServer(t)
	bin := buildET(t)
	p := startProxy(t)
	tm := startET(t, bin, etArgs(p)...)
	tm.waitReady()

	// Pace the output (50 ms pause every 1000 lines, about 10 s in total) so
	// the cuts at 1, 2 and 3 s land mid-stream; unpaced, seq finishes in well
	// under a second (MEASURED on this host). The loop runs under sh -c so it
	// works whatever the login shell is (fish on this host).
	const paced = `sh -c 'seq 1 200000 | while read l; do echo "$l"; case $l in *000) sleep 0.05;; esac; done'`
	tm.send(beginCmd + "; " + paced + "; " + endCmd + "\r")
	for i := range 3 {
		time.Sleep(time.Second) // real network timing: this is not a synctest test
		if strings.Contains(tm.output(), "END\n") {
			t.Fatalf("output finished before cut %d; the test no longer cuts mid-stream", i+1)
		}
		before := p.connections()
		p.cutAll()
		p.waitConnections(t, before+1, 30*time.Second) // et reconnected after this cut
	}
	out := tm.waitFor("END\n", 5*time.Minute)
	checkSeq(t, out, 200000)
	if n := p.connections(); n < 4 {
		t.Fatalf("proxy forwarded %d connection(s), want at least 4: the first and one per cut", n)
	}
	t.Logf("proxy forwarded %d connections for 3 cuts", p.connections())

	// Typed input still flows after the reconnects.
	tm.send("printf 'AFT%sR\\n' E\r")
	tm.waitFor("AFTER", 30*time.Second)
	tm.send("exit\r")
	select {
	case err := <-tm.done:
		if err != nil {
			t.Fatalf("et exited with %v after reconnects, want status 0", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("et did not exit within 10 s after the shell exited")
	}
}
