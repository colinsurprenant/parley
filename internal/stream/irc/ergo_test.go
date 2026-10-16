package irc

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"text/template"
	"time"

	"github.com/colinsurprenant/parley/internal/ergoconf"
)

// The tests in this package talk to a real ergo. There is no fake server:
// everything this adapter does — msgids, server-time, CHATHISTORY paging, the
// gap the history ring produces, what a multiline batch looks like to a client
// that never heard of it — is the server's behaviour, and a fake would only
// prove that the adapter agrees with what its author assumed.
//
// ergoConfig is the same configuration as the one parley embeds
// (internal/ergoconf/ircd.yaml, which `parley server` runs; the lock file lands
// in the working directory each test starts ergo in), with the three
// things a test needs to vary: the port, a datastore under the test's own
// temporary directory, and the size of the retained history ring, which is how
// a test makes a cursor expire. TestErgoConfigMatchesEmbedded keeps the two in
// step.
const ergoConfig = `network:
    name: parley

server:
    name: ergo.test
    listeners:
        "{{.Listener}}": {}
    max-sendq: 96k

accounts:
    authentication-enabled: true
    registration:
        enabled: true
        allow-before-connect: true
        email-verification:
            enabled: false
        bcrypt-cost: 4
    require-sasl:
        enabled: false
    nick-reservation:
        enabled: true
        method: strict
    multiclient:
        enabled: true
        allowed-by-default: true
        always-on: "opt-out"
        auto-away: "opt-in"

datastore:
    path: {{.Datastore}}

lock-file: "ircd.lock"

limits:
    nicklen: 64
    channellen: 64
    awaylen: 390
    kicklen: 390
    topiclen: 390
    multiline:
        max-bytes: 65536
        max-lines: 1000

fakelag:
    enabled: false

history:
    enabled: true
    channel-length: {{.ChannelLength}}
    client-length: 256
    autoresize-window: 3d
    autoreplay-on-join: 0
    chathistory-maxmessages: {{.ChatHistoryMax}}
    restrictions:
        query-cutoff: 'none'
    persistent:
        enabled: false

logging:
    -
        method: stderr
        type: "* -userinput -useroutput"
        level: info
`

// embeddedConfig is the embedded ircd.yaml, rendered from ergoConfig.
var embeddedConfig = ergoSettings{Listener: "127.0.0.1:6667", Datastore: "ergo.db", ChannelLength: 2048, ChatHistoryMax: 1000}

type ergoSettings struct {
	Listener      string
	Datastore     string
	ChannelLength int
	// ChatHistoryMax is the CHATHISTORY page size the server advertises, which
	// is the page size the adapter asks for. A test forces it low to reach the
	// paging the adapter does for a history it cannot read in one request.
	ChatHistoryMax int
}

// ergoServer is a live ergo listening on loopback, torn down with the test.
type ergoServer struct {
	Addr string
	// State is the --state-dir every adapter in this test runs with. It is per
	// server, so two adapters for one nick share the account password exactly
	// as two parley processes on one machine would, and two tests never do.
	State string

	cmd *exec.Cmd
	mu  sync.Mutex
	log bytes.Buffer
}

// startErgo starts an ergo with the given history ring size and waits until it
// accepts connections. A machine without ergo skips the test rather than
// failing it: the adapter's live behaviour cannot be checked without a server,
// and pretending otherwise is what a fake server would be for.
func startErgo(t *testing.T, channelLength int) *ergoServer {
	t.Helper()
	return startErgoWith(t, channelLength, embeddedConfig.ChatHistoryMax)
}

// startErgoWith is startErgo with the history page size the server advertises.
func startErgoWith(t *testing.T, channelLength, chatHistoryMax int) *ergoServer {
	t.Helper()
	bin := ergoBinary(t)
	dir := t.TempDir()

	set := ergoSettings{
		Listener:       "127.0.0.1:" + freePort(t),
		Datastore:      filepath.Join(dir, "ergo.db"),
		ChannelLength:  channelLength,
		ChatHistoryMax: chatHistoryMax,
	}
	conf := filepath.Join(dir, "ircd.yaml")
	if err := os.WriteFile(conf, []byte(renderErgoConfig(t, set)), 0o600); err != nil {
		t.Fatalf("write ergo config: %v", err)
	}

	srv := &ergoServer{Addr: set.Listener, State: filepath.Join(dir, "state")}
	srv.cmd = exec.Command(bin, "run", "--conf", conf)
	srv.cmd.Dir = dir
	srv.cmd.Stdout = &syncWriter{srv: srv}
	srv.cmd.Stderr = &syncWriter{srv: srv}
	if err := srv.cmd.Start(); err != nil {
		t.Fatalf("start ergo: %v", err)
	}
	t.Cleanup(func() {
		_ = srv.cmd.Process.Kill()
		_ = srv.cmd.Wait()
		if t.Failed() {
			srv.mu.Lock()
			t.Logf("ergo log:\n%s", srv.log.String())
			srv.mu.Unlock()
		}
	})

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", srv.Addr, 200*time.Millisecond)
		if err == nil {
			c.Close()
			return srv
		}
		time.Sleep(20 * time.Millisecond)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	t.Fatalf("ergo did not listen on %s in time; log:\n%s", srv.Addr, srv.log.String())
	return nil
}

// pause stops the server process without closing anything: the sockets stay
// open and every request written to them goes unanswered, which is the one
// thing a client cannot tell from a server that is merely slow.
//
// It returns only once the process has stopped. A SIGSTOP is delivered
// asynchronously: kill returns while ergo's threads may still be running on
// other cores, and a request written in that window is answered (seen on a
// Linux CI runner). Waiting for the stop report closes it; ergo is this
// process's child and nothing else waits on it until cleanup.
func (s *ergoServer) pause(t *testing.T) {
	t.Helper()
	if err := s.cmd.Process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatalf("stop ergo: %v", err)
	}
	var ws syscall.WaitStatus
	for {
		_, err := syscall.Wait4(s.cmd.Process.Pid, &ws, syscall.WUNTRACED, nil)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			t.Fatalf("wait for ergo to stop: %v", err)
		}
		break
	}
	// Not ws.Stopped(): on the BSDs, Go reports a SIGSTOP as Continued.
	if ws.Exited() || ws.Signaled() {
		t.Fatalf("ergo ended instead of stopping: wait status %#x", uint32(ws))
	}
}

func (s *ergoServer) resume(t *testing.T) {
	t.Helper()
	if err := s.cmd.Process.Signal(syscall.SIGCONT); err != nil {
		t.Fatalf("continue ergo: %v", err)
	}
}

type syncWriter struct{ srv *ergoServer }

func (w *syncWriter) Write(p []byte) (int, error) {
	w.srv.mu.Lock()
	defer w.srv.mu.Unlock()
	return w.srv.log.Write(p)
}

// ergoBinary finds the server: $PARLEY_ERGO names it outright, otherwise it is
// whatever `ergo` is on PATH.
func ergoBinary(t *testing.T) string {
	t.Helper()
	if bin := os.Getenv("PARLEY_ERGO"); bin != "" {
		if _, err := os.Stat(bin); err != nil {
			t.Fatalf("$PARLEY_ERGO is set to %q, which is not there: %v", bin, err)
		}
		return bin
	}
	bin, err := exec.LookPath("ergo")
	if err != nil {
		t.Skip("no ergo: set $PARLEY_ERGO or put ergo on PATH (see contrib/ergo/README.md)")
	}
	return bin
}

// freePort takes a loopback port the kernel says is free and gives it straight
// back, which is as close to reserving one as a test can get.
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe for a free port: %v", err)
	}
	defer l.Close()
	_, port, err := net.SplitHostPort(l.Addr().String())
	if err != nil {
		t.Fatalf("probe for a free port: %v", err)
	}
	return port
}

func renderErgoConfig(t *testing.T, set ergoSettings) string {
	t.Helper()
	tmpl, err := template.New("ircd.yaml").Parse(ergoConfig)
	if err != nil {
		t.Fatalf("parse the ergo config template: %v", err)
	}
	var b strings.Builder
	if err := tmpl.Execute(&b, set); err != nil {
		t.Fatalf("render the ergo config template: %v", err)
	}
	return b.String()
}

// TestErgoConfigMatchesEmbedded keeps the configuration the tests run against
// and the one the binary embeds from drifting apart: every setting the template
// makes must be in the embedded ircd.yaml, spelled the same way, so that a green
// test says something about the file `parley server` starts ergo with.
func TestErgoConfigMatchesEmbedded(t *testing.T) {
	have := make(map[string]bool)
	for _, l := range strings.Split(ergoconf.YAML, "\n") {
		have[strings.TrimRight(l, " \t")] = true
	}
	for _, l := range strings.Split(renderErgoConfig(t, embeddedConfig), "\n") {
		l = strings.TrimRight(l, " \t")
		if l == "" || strings.HasPrefix(strings.TrimSpace(l), "#") {
			continue
		}
		if !have[l] {
			t.Errorf("the embedded ircd.yaml is missing the line %q that the test config sets", l)
		}
	}
}

// fmtChannel turns a test's name into a channel name of its own, so tests
// sharing a server never share history.
func fmtChannel(t *testing.T) string {
	t.Helper()
	name := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		}
		return '-'
	}, t.Name())
	return fmt.Sprintf("#%s", name)
}
