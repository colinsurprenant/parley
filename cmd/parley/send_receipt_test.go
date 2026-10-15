package main

import (
	"bytes"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/colinsurprenant/parley/internal/ergoconf"
)

var headerRE = regexp.MustCompile(`(?m)^\[parley\] from=(\S+) id=(\S+) ts=(\S+) — `)

// receiptFields parses what `parley send` printed: exactly one line, and the id
// and ts on it.
func receiptFields(t *testing.T, res result) (id, ts string) {
	t.Helper()
	m := regexp.MustCompile(`^\[parley\] sent id=(\S+) ts=(\S+)\n$`).FindStringSubmatch(res.stdout)
	if m == nil {
		t.Fatalf("send printed %q, want exactly one line of the form %q", res.stdout, "[parley] sent id=<id> ts=<ts>")
	}
	return m[1], m[2]
}

// headerFields is the id and ts of the one header a wait printed for author.
func headerFields(t *testing.T, out, author string) (id, ts string) {
	t.Helper()
	var found [][]string
	for _, m := range headerRE.FindAllStringSubmatch(out, -1) {
		if m[1] == author {
			found = append(found, m)
		}
	}
	if len(found) != 1 {
		t.Fatalf("wait printed %d headers from %s, want 1:\n%s", len(found), author, out)
	}
	return found[0][2], found[0][3]
}

// TestSendPrintsTheIDAndTSAnotherNicksWaitPrints: the receipt is how a seat
// confirms its own post, so it must name the post the way the other seats'
// headers will.
func TestSendPrintsTheIDAndTSAnotherNicksWaitPrints(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chat.ndjson")
	chat := "file:" + path
	stateDir := t.TempDir()

	for i, body := range []string{"first", "second\nover two lines"} {
		sent := mustRun(t, "", "send", "--stream", chat, "--nick", "codex", "--", body)
		id, ts := receiptFields(t, sent)
		if i == 0 && id != "0" {
			t.Fatalf("the first line of a new file starts at byte 0, send printed id=%s", id)
		}
		got := mustRun(t, "", "wait", "--stream", chat, "--nick", "claude", "--state-dir", stateDir, "--timeout", "10s", "--debounce", "100ms")
		wid, wts := headerFields(t, got.stdout, "codex")
		if id != wid || ts != wts {
			t.Fatalf("send printed id=%s ts=%s, wait printed id=%s ts=%s", id, ts, wid, wts)
		}
	}
}

// TestSendAfterATornTailPrintsTheIDWaitPrints: the byte that closes a torn line
// is not part of the message, so the offset printed is the one after it.
func TestSendAfterATornTailPrintsTheIDWaitPrints(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chat.ndjson")
	chat := "file:" + path
	stateDir := t.TempDir()
	if err := os.WriteFile(path, []byte(`{"author":"someone","ts":"2026-09-12T15:04:05Z","body":"whole"}`+"\n"+`{"author":"someone","ts":"2026-09-12T15:0`), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	id, ts := receiptFields(t, mustRun(t, "", "send", "--stream", chat, "--nick", "codex", "--", "after the tear"))
	got := mustRun(t, "", "wait", "--stream", chat, "--nick", "claude", "--state-dir", stateDir, "--timeout", "10s", "--debounce", "100ms")
	wid, wts := headerFields(t, got.stdout, "codex")
	if id != wid || ts != wts {
		t.Fatalf("send printed id=%s ts=%s, wait printed id=%s ts=%s", id, ts, wid, wts)
	}
}

// TestSendPrintsTheIDAndTSAnotherNicksWaitPrintsOnErgo is the same check against
// the real server, where the id and the time are the server's: the echo of the
// post carries them, and the history another seat reads carries the same.
func TestSendPrintsTheIDAndTSAnotherNicksWaitPrintsOnErgo(t *testing.T) {
	addr := startErgo(t)
	chat := "irc://" + addr + "/receipt"
	stateDir := t.TempDir()

	for _, body := range []string{"one line", "two\nlines"} {
		id, ts := receiptFields(t, mustRun(t, "", "send", "--stream", chat, "--nick", "alpha", "--state-dir", stateDir, "--", body))
		got := mustRun(t, "", "wait", "--stream", chat, "--nick", "beta", "--state-dir", stateDir, "--timeout", "10s", "--debounce", "100ms")
		wid, wts := headerFields(t, got.stdout, "alpha")
		if id != wid || ts != wts {
			t.Fatalf("send printed id=%s ts=%s, wait printed id=%s ts=%s", id, ts, wid, wts)
		}
	}
}

// startErgo runs a real ergo on a free loopback port from the configuration the
// binary embeds, with its datastore in the test's own directory, and returns
// the address. A machine without ergo skips the test.
func startErgo(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("PARLEY_ERGO")
	if bin == "" {
		var err error
		if bin, err = exec.LookPath("ergo"); err != nil {
			t.Skip("no ergo: set $PARLEY_ERGO or put ergo on PATH (see contrib/ergo/README.md)")
		}
	}
	addr := freeAddr(t)
	dir := t.TempDir()
	confPath, err := ergoconf.Install(dir, addr)
	if err != nil {
		t.Fatalf("write the ergo config: %v", err)
	}
	cmd := exec.Command(bin, "run", "--conf", confPath)
	cmd.Dir = dir
	var log bytes.Buffer
	cmd.Stdout, cmd.Stderr = &log, &log
	if err := cmd.Start(); err != nil {
		t.Fatalf("start ergo: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
			c.Close()
			return addr
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("ergo did not listen on %s in time", addr)
	return ""
}

// TestSendSurvivesAClosedStdout: the post is made before the receipt is written,
// and a reader that has gone away must not turn the one into a failure — Go ends
// a process that writes to a broken stdout pipe unless SIGPIPE is handled — or
// the caller sends it again.
func TestSendSurvivesAClosedStdout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chat.ndjson")
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("close the read end: %v", err)
	}
	defer w.Close()

	cmd := exec.Command(binary(t), "send", "--stream", "file:"+path, "--nick", "codex", "--", "posted before the pipe broke")
	cmd.Stdout = w
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("send with a closed stdout: %v, want exit 0\nstderr: %s", err, stderr.String())
	}
	got, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(got), "posted before the pipe broke") {
		t.Fatalf("the post is not in the stream (err %v):\n%s", err, got)
	}
}

// freeAddr takes a loopback address whose port the kernel says is free and gives
// it straight back, which is as close to reserving one as a test can get.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe for a free port: %v", err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}
