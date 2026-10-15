package doctor

import (
	"bufio"
	"context"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/colinsurprenant/parley/internal/ergoconf"
	"github.com/colinsurprenant/parley/internal/stream/irc"
)

// fakeIRC listens on a loopback port, answers the first line it reads with
// reply (which may be empty, for a server that says nothing), and records what
// it was sent.
func fakeIRC(t *testing.T, reply string) (addr string, got <-chan string) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	first := make(chan string, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		line, _ := bufio.NewReader(c).ReadString('\n')
		first <- strings.TrimRight(line, "\r\n")
		if reply != "" {
			_, _ = c.Write([]byte(reply))
		}
	}()
	return l.Addr().String(), first
}

func capsLine(caps ...string) string {
	return ":ergo.test CAP * LS :" + strings.Join(caps, " ") + "\r\n"
}

func TestServerThatOffersEveryRequiredCapabilityIsOK(t *testing.T) {
	// Values on a capability are not part of its name, and a capability the
	// adapter does not require changes nothing.
	caps := []string{"account-tag", "draft/multiline=max-bytes=65536,max-lines=1000", "sasl=PLAIN,EXTERNAL"}
	for _, c := range irc.RequiredCaps() {
		if c != "draft/multiline" && c != "sasl" {
			caps = append(caps, c)
		}
	}
	addr, got := fakeIRC(t, capsLine(caps...))
	r := checkServer(context.Background(), addr)
	if r.Level != OK || r.Surface != "server" {
		t.Fatalf("got %q", r)
	}
	if line := <-got; line != "CAP LS 302" {
		t.Errorf("the server was sent %q, want CAP LS 302 and nothing before it", line)
	}
}

func TestServerWithAMultilineCapabilityListIsRead(t *testing.T) {
	req := irc.RequiredCaps()
	reply := ":ergo.test CAP * LS * :" + strings.Join(req[:4], " ") + "\r\n" +
		":ergo.test CAP * LS :" + strings.Join(req[4:], " ") + "\r\n"
	addr, _ := fakeIRC(t, reply)
	if r := checkServer(context.Background(), addr); r.Level != OK {
		t.Fatalf("got %q", r)
	}
}

func TestServerMissingACapabilityIsAFAILNamingIt(t *testing.T) {
	var caps []string
	for _, c := range irc.RequiredCaps() {
		if c != "draft/chathistory" {
			caps = append(caps, c)
		}
	}
	addr, _ := fakeIRC(t, capsLine(caps...))
	r := checkServer(context.Background(), addr)
	if r.Level != Fail || !strings.Contains(r.Detail, "draft/chathistory") || strings.Contains(r.Detail, "draft/multiline") {
		t.Fatalf("got %q, want a FAIL naming only draft/chathistory", r)
	}
}

// requiredCapsWith is the required capabilities as a CAP LS line holds them,
// with sasl spelled as given.
func requiredCapsWith(sasl string) []string {
	var caps []string
	for _, c := range irc.RequiredCaps() {
		if c == "sasl" {
			c = sasl
		}
		caps = append(caps, c)
	}
	return caps
}

// TestServerSaslThatListsMechanismsMustListPLAIN: parley authenticates with
// AUTHENTICATE PLAIN, so a sasl without it is a server a table cannot register
// on. A sasl with no mechanisms listed says nothing against it.
func TestServerSaslThatListsMechanismsMustListPLAIN(t *testing.T) {
	for _, tc := range []struct {
		name, sasl string
		level      Level
	}{
		{"PLAIN among others", "sasl=EXTERNAL,PLAIN,SCRAM-SHA-256", OK},
		{"PLAIN alone", "sasl=PLAIN", OK},
		{"no mechanisms listed", "sasl", OK},
		{"PLAIN absent", "sasl=EXTERNAL,SCRAM-SHA-256", Fail},
		{"only EXTERNAL", "sasl=EXTERNAL", Fail},
		{"a control sequence in the list", "sasl=EXTERNAL,\x1b[2J", Fail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr, _ := fakeIRC(t, capsLine(requiredCapsWith(tc.sasl)...))
			r := checkServer(context.Background(), addr)
			if r.Level != tc.level {
				t.Fatalf("got %q, want %v", r, tc.level)
			}
			if tc.level == Fail && (!strings.Contains(r.Detail, "PLAIN") || !strings.Contains(r.Detail, "EXTERNAL")) {
				t.Errorf("the FAIL does not name PLAIN and what the server lists: %q", r)
			}
			if strings.ContainsRune(r.Detail, 0x1b) {
				t.Errorf("the server's text reached the report raw: %q", r)
			}
		})
	}
}

// TestServerCapabilityNamesAreKeptVerbatim: the irc adapter keeps a CAP LS name
// as the server wrote it, so a server listing `-sasl` and no `sasl` offers
// nothing parley can request.
func TestServerCapabilityNamesAreKeptVerbatim(t *testing.T) {
	var caps []string
	for _, c := range irc.RequiredCaps() {
		if c == "sasl" {
			c = "-sasl"
		}
		caps = append(caps, c)
	}
	addr, _ := fakeIRC(t, capsLine(caps...))
	r := checkServer(context.Background(), addr)
	if r.Level != Fail || !strings.Contains(r.Detail, "sasl") || strings.Contains(r.Detail, "draft/multiline") {
		t.Fatalf("got %q, want a FAIL naming sasl as missing", r)
	}
	got, err := readCaps(bufio.NewReader(strings.NewReader(":s CAP * LS :-sasl batch\r\n")))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["sasl"]; ok {
		t.Errorf("-sasl was read as sasl: %v", got)
	}
	if _, ok := got["-sasl"]; !ok {
		t.Errorf("-sasl was not kept verbatim: %v", got)
	}
}

func TestReadCapsStopsAtALineLongerThanIRCAllows(t *testing.T) {
	endless := strings.NewReader(strings.Repeat("x", 1<<20))
	_, err := readCaps(bufio.NewReaderSize(endless, maxCapsLine))
	if err == nil || !strings.Contains(err.Error(), "longer than") {
		t.Fatalf("got %v, want a refusal of the overlong line", err)
	}
	if read := 1<<20 - endless.Len(); read > maxCapsLine {
		t.Errorf("read %d bytes of the endless line, more than the %d-byte bound", read, maxCapsLine)
	}
}

func TestReadCapsKeepsTheValues(t *testing.T) {
	got, err := readCaps(bufio.NewReader(strings.NewReader(":s CAP * LS * :sasl=PLAIN,EXTERNAL batch\r\n:s CAP * LS :draft/multiline=max-bytes=4096,max-lines=100\r\n")))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"sasl": "PLAIN,EXTERNAL", "batch": "", "draft/multiline": "max-bytes=4096,max-lines=100"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if g, ok := got[k]; !ok || g != v {
			t.Errorf("%s = %q (listed %v), want %q", k, g, ok, v)
		}
	}
}

func TestServerThatIsNotIRCIsAFAIL(t *testing.T) {
	addr, _ := fakeIRC(t, "")
	r := checkServer(context.Background(), addr)
	if r.Level != Fail || !strings.Contains(r.Detail, "CAP LS 302") {
		t.Fatalf("got %q", r)
	}
}

func TestServerNobodyListensOnIsAWarningNamingParleyServer(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	r := checkServer(context.Background(), addr)
	if r.Level != Warn || !strings.Contains(r.Detail, "parley server") {
		t.Fatalf("got %q", r)
	}
	if Failed([]Result{r}) {
		t.Error("an unreachable server failed the check; it is a warning")
	}
}

func TestServerThatIsNotLoopbackIsRefusedWithoutConnecting(t *testing.T) {
	r := checkServer(context.Background(), "192.0.2.1:6667")
	if r.Level != Fail || !strings.Contains(r.Detail, "loopback") {
		t.Fatalf("got %q", r)
	}
}

func TestServerCheckAgainstTheRealThing(t *testing.T) {
	bin := realErgo(t)
	dir := t.TempDir()
	addr, err := freeLoopbackAddr()
	if err != nil {
		t.Fatal(err)
	}
	conf, err := ergoconf.Install(dir, addr)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := startErgo(bin, conf, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.stop()
	if err := srv.waitListening(context.Background(), addr); err != nil {
		t.Fatal(err)
	}
	r := checkServer(context.Background(), addr)
	if r.Level != OK {
		t.Fatalf("the embedded configuration's ergo fails the server check: %q", r)
	}
}

func TestLiveErgoRoundTripLeavesNothingBehind(t *testing.T) {
	bin := realErgo(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	rs := Check(context.Background(), Options{ErgoBin: bin, Live: true})
	live := find(t, rs, ergoSurface)
	if live.Level != OK {
		t.Fatalf("got %q", live)
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Errorf("the live check left %d entries in the temp directory", len(left))
	}
	// The port it used is closed again: the server is stopped.
	m := regexpAddr.FindString(live.Detail)
	if m == "" {
		t.Fatalf("no address in %q", live.Detail)
	}
	if c, err := net.DialTimeout("tcp", m, time.Second); err == nil {
		c.Close()
		t.Errorf("%s still accepts connections after the live check", m)
	}
}

func TestLiveErgoThatCannotStartIsAFAILAndLeavesNothing(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "ergo")
	script := "#!/bin/sh\ncase \"$1\" in --version) echo ergo-fake; exit 0 ;; esac\necho 'cannot read the config' >&2\nexit 3\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	rs := Check(context.Background(), Options{ErgoBin: bin, Live: true})
	live := find(t, rs, ergoSurface)
	if live.Level != Fail || !strings.Contains(live.Detail, "cannot read the config") {
		t.Fatalf("got %q", live)
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Errorf("a failed live check left %d entries in the temp directory", len(left))
	}
}

func TestLiveErgoIsNotRunWithoutLive(t *testing.T) {
	bin := realErgo(t)
	for _, r := range Check(context.Background(), Options{ErgoBin: bin}) {
		if r.Surface == ergoSurface {
			t.Errorf("a static check ran the live ergo: %q", r)
		}
	}
}

var regexpAddr = regexp.MustCompile(`127\.0\.0\.1:\d+`)
