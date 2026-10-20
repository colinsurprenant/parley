package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/colinsurprenant/parley/internal/state"
	"github.com/colinsurprenant/parley/internal/stream/file"
)

var binPath string

// childDeadline bounds every wait on a child process that is expected to do
// something and end on its own. It is generous: it is here so that a wedged
// child fails its own test with its own log, rather than hanging until the
// package timeout kills the whole run with no message of its own.
const childDeadline = 20 * time.Second

// TestMain builds the real binary once into a temp directory; every test below
// drives it through os/exec, so exit codes and stdout are the ones a harness
// would see.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "parley-bin-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "build parley: %v\n", err)
		os.Exit(1)
	}
	binPath = filepath.Join(dir, "parley")
	if out, err := exec.Command("go", "build", "-o", binPath, ".").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build parley: %v\n%s", err, out)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func binary(t *testing.T) string {
	t.Helper()
	return binPath
}

type result struct {
	stdout string
	stderr string
	code   int
}

func runParley(t *testing.T, stdin string, args ...string) result {
	t.Helper()
	return runParleyIn(t, "", stdin, args...)
}

// runParleyIn runs the binary with dir as its working directory, so a test can
// hand it a relative --stream path and mean a particular file.
func runParleyIn(t *testing.T, dir, stdin string, args ...string) result {
	t.Helper()
	cmd := exec.Command(binary(t), args...)
	cmd.Dir = dir
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	res := result{stdout: stdout.String(), stderr: stderr.String()}
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("run parley %v: %v", args, err)
		}
		res.code = exit.ExitCode()
	}
	return res
}

// runParleyWithin runs the binary like runParley and gives the run its own
// deadline: an invocation expected to return on its own, which a regression
// leaves blocked instead, fails here with what it printed rather than at the
// package timeout with nothing.
func runParleyWithin(t *testing.T, d time.Duration, args ...string) result {
	t.Helper()
	cmd := exec.Command(binary(t), args...)
	// A concurrency-safe sink either way: on the deadline the child is killed
	// while os/exec may still be copying into it, and the failure quotes it.
	stdout, stderr := &syncBuffer{}, &syncBuffer{}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start parley %v: %v", args, err)
	}
	done := joinCmd(cmd)
	select {
	case err := <-done:
		res := result{stdout: stdout.String(), stderr: stderr.String()}
		if err != nil {
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatalf("run parley %v: %v", args, err)
			}
			res.code = exit.ExitCode()
		}
		return res
	case <-time.After(d):
		_ = cmd.Process.Kill()
		// Best effort: a Wait still blocked after the kill is a child holding
		// the pipes, and this test is failing regardless.
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
		t.Fatalf("parley %v did not exit within %s\nstdout: %s\nstderr: %s", args, d, stdout.String(), stderr.String())
		return result{}
	}
}

// joinCmd reaps an already-started child on a goroutine and returns the channel
// carrying what Wait returned, so a caller can put a deadline on the join. Wait
// must be called once and once only, so the caller owns the child from here.
func joinCmd(cmd *exec.Cmd) <-chan error {
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	return done
}

func mustRun(t *testing.T, stdin string, args ...string) result {
	t.Helper()
	return mustRunIn(t, "", stdin, args...)
}

func mustRunIn(t *testing.T, dir, stdin string, args ...string) result {
	t.Helper()
	res := runParleyIn(t, dir, stdin, args...)
	if res.code != 0 {
		t.Fatalf("parley %v in %q exited %d\nstdout: %s\nstderr: %s", args, dir, res.code, res.stdout, res.stderr)
	}
	return res
}

// resolvedTempDir is a temp directory with its symlinks resolved. The canonical
// spec is built with filepath.Abs, which does not resolve them, and a child
// process started in /var/... on macOS reports its working directory as
// /private/var/...: a test comparing a relative spelling against an absolute one
// has to name the directory the way the child will.
func resolvedTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	return dir
}

// facts are the three launch-time facts every channel and primer invocation
// carries, whatever its skill: the human's registered nick, the target repo and
// the session's worktree of it. The two directories are real, and short, so the
// text they land in stays under the client's cap.
type facts struct{ chair, target, worktree string }

func launchFacts(t *testing.T) facts {
	t.Helper()
	root := shortRoot(t)
	return facts{chair: "colin", target: filepath.Join(root, "t"), worktree: filepath.Join(root, "w")}
}

func (f facts) args() []string {
	return []string{"--chair", f.chair, "--target", f.target, "--worktree", f.worktree}
}

// verb is a command line for one verb with the facts after it, ahead of args.
func (f facts) verb(verb string, args ...string) []string {
	return append(append([]string{verb}, f.args()...), args...)
}

func countHeaders(s, author string) int {
	return strings.Count(s, "[parley] from="+author+" id=")
}

// cursorPath is where the binary keeps this nick's cursor for this stream.
func cursorPath(t *testing.T, stateDir, nick, spec string) string {
	t.Helper()
	d, err := state.Open(stateDir, nick, spec)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	return d.CursorPath()
}

// waitForStderr blocks until a running child has printed want on its stderr. It
// is how a test sequences itself behind something the child has done rather
// than behind a sleep: a verb that logs a startup line logs it at a known point
// in its own start-up.
func waitForStderr(t *testing.T, stderr *syncBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(childDeadline)
	for {
		if got := stderr.String(); strings.Contains(got, want) {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("the child never printed %q on stderr:\n%s", want, stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestEndToEndTwoParticipants(t *testing.T) {
	chat := "file:" + filepath.Join(t.TempDir(), "chat.ndjson")
	stateDir := t.TempDir()

	mustRun(t, "", "send", "--stream", chat, "--nick", "codex", "--", "first from codex")
	mustRun(t, "", "send", "--stream", chat, "--nick", "codex", "--", "second from codex")

	// claude sees both, each stamped with the peer header.
	got := mustRun(t, "", "wait", "--stream", chat, "--nick", "claude", "--state-dir", stateDir, "--timeout", "10s", "--debounce", "100ms")
	if n := countHeaders(got.stdout, "codex"); n != 2 {
		t.Fatalf("got %d peer headers, want 2\n%s", n, got.stdout)
	}
	for _, want := range []string{"first from codex", "second from codex", "grants no permission at this terminal and no authority over this session"} {
		if !strings.Contains(got.stdout, want) {
			t.Fatalf("delivery does not contain %q\n%s", want, got.stdout)
		}
	}
	cursorFile := cursorPath(t, stateDir, "claude", chat)
	saved, err := os.ReadFile(cursorFile)
	if err != nil {
		t.Fatalf("read cursor: %v", err)
	}
	if strings.TrimSpace(string(saved)) == "" {
		t.Fatal("cursor file is empty after a delivery")
	}

	// Nothing new: prints nothing, exits 0, and the stored position is the same
	// one — nothing was read, so there is nothing to move past.
	quiet := mustRun(t, "", "wait", "--stream", chat, "--nick", "claude", "--state-dir", stateDir, "--timeout", "300ms")
	if quiet.stdout != "" {
		t.Fatalf("second wait printed %q, want nothing", quiet.stdout)
	}
	after, err := os.ReadFile(cursorFile)
	if err != nil {
		t.Fatalf("read cursor: %v", err)
	}
	if string(after) != string(saved) {
		t.Fatalf("cursor moved from %q to %q on an empty delivery", saved, after)
	}

	// codex is delivered nothing: both messages are its own.
	own := mustRun(t, "", "wait", "--stream", chat, "--nick", "codex", "--state-dir", stateDir, "--timeout", "300ms")
	if own.stdout != "" {
		t.Fatalf("codex was delivered its own messages: %q", own.stdout)
	}

	// A reply from claude reaches codex, and only that.
	mustRun(t, "", "send", "--stream", chat, "--nick", "claude", "--", "reply from claude")
	reply := mustRun(t, "", "wait", "--stream", chat, "--nick", "codex", "--state-dir", stateDir, "--timeout", "10s", "--debounce", "100ms")
	if n := countHeaders(reply.stdout, "claude"); n != 1 {
		t.Fatalf("got %d headers from claude, want 1\n%s", n, reply.stdout)
	}
	if countHeaders(reply.stdout, "codex") != 0 {
		t.Fatalf("codex was delivered its own messages:\n%s", reply.stdout)
	}
	if !strings.Contains(reply.stdout, "reply from claude") {
		t.Fatalf("delivery does not contain the reply\n%s", reply.stdout)
	}
}

func TestSendReadsBodyFromStdin(t *testing.T) {
	chat := "file:" + filepath.Join(t.TempDir(), "chat.ndjson")
	stateDir := t.TempDir()

	mustRun(t, "from stdin\nwith two lines\n", "send", "--stream", chat, "--nick", "codex")
	got := mustRun(t, "", "wait", "--stream", chat, "--nick", "claude", "--state-dir", stateDir, "--timeout", "10s", "--debounce", "100ms")
	if !strings.Contains(got.stdout, "  from stdin\n  with two lines\n\n") {
		t.Fatalf("body was not delivered verbatim with its trailing newline trimmed:\n%q", got.stdout)
	}
}

// TestSendReadsBodyFromAFile covers the path the channel front's reply recipe
// uses: the body reaches parley as a file, so nothing in it is ever a shell
// word — not a quote, not a substitution, not a line that used to end a
// heredoc — and what lands on the stream is what was written, byte for byte.
func TestSendReadsBodyFromAFile(t *testing.T) {
	chat := "file:" + filepath.Join(t.TempDir(), "chat.ndjson")
	stateDir := t.TempDir()

	body := "he said \"hi\" and 'bye' & then $(echo substituted) `echo backticked`\n" +
		"PARLEY_EOF\n" +
		"still one message, with ${HOME} and a backslash \\ and a lone $"
	path := filepath.Join(t.TempDir(), "parley-reply.txt")
	if err := os.WriteFile(path, []byte(body+"\n"), 0o600); err != nil {
		t.Fatalf("write the body file: %v", err)
	}

	mustRun(t, "", "send", "--stream", chat, "--nick", "codex", "--body-file", path)
	got := mustRun(t, "", "wait", "--stream", chat, "--nick", "claude", "--state-dir", stateDir, "--timeout", "10s", "--debounce", "100ms")
	// The delivery indents every body line by two spaces and then adds a blank
	// line, so the indented body plus "\n\n" is the whole of it: one trailing
	// newline dropped, nothing else touched, and the internal newlines still
	// there.
	want := "  " + strings.ReplaceAll(body, "\n", "\n  ") + "\n\n"
	if !strings.Contains(got.stdout, want) {
		t.Fatalf("the file body was not delivered verbatim under the indent:\n got: %q\nwant: %q", got.stdout, want)
	}
}

func TestSendRefusesABodyFileAndAnArgumentTogether(t *testing.T) {
	chatPath := filepath.Join(t.TempDir(), "chat.ndjson")
	path := filepath.Join(t.TempDir(), "parley-reply.txt")
	if err := os.WriteFile(path, []byte("from the file\n"), 0o600); err != nil {
		t.Fatalf("write the body file: %v", err)
	}

	res := runParley(t, "", "send", "--stream", "file:"+chatPath, "--nick", "codex", "--body-file", path, "--", "from the argument")
	if res.code != 2 {
		t.Fatalf("send exited %d, want 2\nstdout: %s\nstderr: %s", res.code, res.stdout, res.stderr)
	}
	if !strings.Contains(res.stderr, "--body-file") {
		t.Fatalf("the error does not name the flag: %s", res.stderr)
	}
	// Refused before the stream is opened: an invocation naming two bodies
	// leaves nothing behind, not even the file it would have appended to.
	if _, err := os.Stat(chatPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the refused invocation touched the stream (stat err %v)", err)
	}
}

func TestSendRefusesAMissingBodyFile(t *testing.T) {
	chat := "file:" + filepath.Join(t.TempDir(), "chat.ndjson")
	missing := filepath.Join(t.TempDir(), "nope.txt")

	res := runParley(t, "", "send", "--stream", chat, "--nick", "codex", "--body-file", missing)
	if res.code != 1 {
		t.Fatalf("send exited %d, want 1\nstdout: %s\nstderr: %s", res.code, res.stdout, res.stderr)
	}
	if !strings.Contains(res.stderr, missing) {
		t.Fatalf("the error does not name the file: %s", res.stderr)
	}
}

// TestSendRefusesASymlinkedBodyFile covers the reply file's threat model: it
// lives where the model can write, so the model can replace it with a link to
// any file the user can read, and an allowed command runs outside the sandbox.
// send opens without following the link, so the target is never posted.
func TestSendRefusesASymlinkedBodyFile(t *testing.T) {
	chatPath := filepath.Join(t.TempDir(), "chat.ndjson")
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(target, []byte("the target's contents\n"), 0o600); err != nil {
		t.Fatalf("write the link target: %v", err)
	}
	link := filepath.Join(dir, "reply.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	res := runParley(t, "", "send", "--stream", "file:"+chatPath, "--nick", "codex", "--body-file", link)
	if res.code != 1 {
		t.Fatalf("send exited %d, want 1\nstdout: %s\nstderr: %s", res.code, res.stdout, res.stderr)
	}
	if !strings.Contains(res.stderr, "symbolic link") || !strings.Contains(res.stderr, link) {
		t.Fatalf("the error does not name the file and the link: %s", res.stderr)
	}
	// The whole point: nothing the link pointed at reached the stream.
	b, err := os.ReadFile(chatPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(b) != 0 {
		t.Fatalf("the refused body was appended: %q", b)
	}
}

func TestSendRefusesADirectoryAsTheBodyFile(t *testing.T) {
	chat := "file:" + filepath.Join(t.TempDir(), "chat.ndjson")

	res := runParley(t, "", "send", "--stream", chat, "--nick", "codex", "--body-file", t.TempDir())
	if res.code != 1 {
		t.Fatalf("send exited %d, want 1\nstdout: %s\nstderr: %s", res.code, res.stdout, res.stderr)
	}
	if !strings.Contains(res.stderr, "not a regular file") {
		t.Fatalf("the error does not say the file is not a regular one: %s", res.stderr)
	}
}

// TestSendRefusesAFifoAsTheBodyFile is the other half of the non-regular case,
// and the one that has to be refused rather than waited on: a read-only open of
// a FIFO blocks until a writer appears, so a fifo planted at the reply path
// would hang the wrapper instead of failing it.
func TestSendRefusesAFifoAsTheBodyFile(t *testing.T) {
	chat := "file:" + filepath.Join(t.TempDir(), "chat.ndjson")
	fifo := filepath.Join(t.TempDir(), "reply.txt")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	res := runParleyWithin(t, childDeadline, "send", "--stream", chat, "--nick", "codex", "--body-file", fifo)
	if res.code != 1 {
		t.Fatalf("send exited %d, want 1\nstdout: %s\nstderr: %s", res.code, res.stdout, res.stderr)
	}
	if !strings.Contains(res.stderr, "not a regular file") {
		t.Fatalf("the error does not say the file is not a regular one: %s", res.stderr)
	}
}

func TestSendRefusesAWhitespaceOnlyBodyFile(t *testing.T) {
	chat := "file:" + filepath.Join(t.TempDir(), "chat.ndjson")
	path := filepath.Join(t.TempDir(), "parley-reply.txt")
	if err := os.WriteFile(path, []byte(" \t\n\n"), 0o600); err != nil {
		t.Fatalf("write the body file: %v", err)
	}

	// The same refusal a whitespace-only argument or stdin body gets: the body
	// is bounded and trimmed identically whichever way it arrived.
	res := runParley(t, "", "send", "--stream", chat, "--nick", "codex", "--body-file", path)
	if res.code != 2 {
		t.Fatalf("send exited %d, want 2\nstdout: %s\nstderr: %s", res.code, res.stdout, res.stderr)
	}
}

func TestSendRefusesAnOverLongBodyFile(t *testing.T) {
	chatPath := filepath.Join(t.TempDir(), "chat.ndjson")
	path := filepath.Join(t.TempDir(), "parley-reply.txt")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", file.MaxLine+1)), 0o600); err != nil {
		t.Fatalf("write the body file: %v", err)
	}

	res := runParley(t, "", "send", "--stream", "file:"+chatPath, "--nick", "codex", "--body-file", path)
	if res.code != 1 {
		t.Fatalf("send exited %d, want 1\nstdout: %s\nstderr: %s", res.code, res.stdout, res.stderr)
	}
	// The same bound as the stdin path, named the same way, against the file
	// the caller pointed at.
	if !strings.Contains(res.stderr, path) || !strings.Contains(res.stderr, strconv.Itoa(file.MaxLine)) {
		t.Fatalf("the error does not name the file and the %d-byte limit: %s", file.MaxLine, res.stderr)
	}
	fi, err := os.Stat(chatPath)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if fi.Size() != 0 {
		t.Fatalf("the refused body was appended: the chat file holds %d bytes", fi.Size())
	}
}

func TestSendRefusesAnOverLongStdinBody(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chat.ndjson")
	chat := "file:" + path

	res := runParley(t, strings.Repeat("x", file.MaxLine+1), "send", "--stream", chat, "--nick", "codex")
	if res.code != 1 {
		t.Fatalf("send exited %d, want 1\nstdout: %s\nstderr: %s", res.code, res.stdout, res.stderr)
	}
	// The body is refused where it is read, not deep in the adapter after a
	// megabyte has been buffered and encoded: the error says stdin and names the
	// limit it broke.
	if !strings.Contains(res.stderr, "stdin") || !strings.Contains(res.stderr, strconv.Itoa(file.MaxLine)) {
		t.Fatalf("the error does not name stdin and the %d-byte limit: %s", file.MaxLine, res.stderr)
	}
	// The stream is opened before stdin is read, so the file exists; nothing may
	// have been written to it.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if fi.Size() != 0 {
		t.Fatalf("the refused body was appended: the chat file holds %d bytes", fi.Size())
	}
}

// TestOneNickAdmitsOneWaitAtATime starts two waits for one nick at once and
// asserts the guard from both sides: one is refused with the lock error, and
// the other is a live wait that goes on to take a delivery. Which one wins is
// the OS's to decide, so the test claims nothing about the order — it joins
// whichever exits first, and on an empty stream with a 30s timeout that can
// only be the refusal. The holder is then ended by a message rather than by its
// own timeout, so the two lifetimes overlap unless a child stalls for the whole
// 30s. The test never takes the lock itself, so it cannot slip between a
// child's OpenFile and that child's own non-blocking flock and steal the lock
// out from under it.
func TestOneNickAdmitsOneWaitAtATime(t *testing.T) {
	chat := "file:" + filepath.Join(t.TempDir(), "chat.ndjson")
	stateDir := t.TempDir()
	lock := filepath.Join(stateDir, "claude", "lock")

	// Long enough that the holder's own timeout is never what ends it: it is
	// still on the stream well after the refusal has come and gone, and the
	// delivery below is what brings it back.
	const hold = 30 * time.Second

	type run struct {
		name           string
		cmd            *exec.Cmd
		stdout, stderr *syncBuffer
		done           <-chan error
	}
	start := func(name string) *run {
		t.Helper()
		cmd := exec.Command(binary(t), "wait", "--stream", chat, "--nick", "claude",
			"--state-dir", stateDir, "--timeout", hold.String(), "--debounce", "50ms")
		r := &run{name: name, cmd: cmd, stdout: &syncBuffer{}, stderr: &syncBuffer{}}
		cmd.Stdout, cmd.Stderr = r.stdout, r.stderr
		if err := cmd.Start(); err != nil {
			t.Fatalf("start wait: %v", err)
		}
		r.done = joinCmd(cmd)
		// A failure below would otherwise leave a child sitting out its 30s
		// timeout; the kill is a no-op once it has exited on its own.
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		return r
	}
	// collect reads what a child left behind, for a Wait that has already
	// returned err.
	collect := func(r *run, err error) result {
		t.Helper()
		res := result{stdout: r.stdout.String(), stderr: r.stderr.String()}
		if err != nil {
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatalf("join the %s wait: %v", r.name, err)
			}
			res.code = exit.ExitCode()
		}
		return res
	}
	join := func(r *run) result {
		t.Helper()
		select {
		case err := <-r.done:
			return collect(r, err)
		case <-time.After(childDeadline):
			t.Fatalf("the %s wait did not exit within %s\nstderr: %s", r.name, childDeadline, r.stderr.String())
			return result{}
		}
	}

	// Both are started before either is joined, so the two lifetimes overlap
	// whichever of them reaches the lock first.
	first, second := start("first"), start("second")
	var holder *run
	var refusal result
	select {
	case err := <-first.done:
		holder, refusal = second, collect(first, err)
	case err := <-second.done:
		holder, refusal = first, collect(second, err)
	case <-time.After(childDeadline):
		t.Fatalf("neither wait exited within %s\nfirst stderr: %s\nsecond stderr: %s",
			childDeadline, first.stderr.String(), second.stderr.String())
	}

	// The stream is empty and the timeout is 30s away, so a wait holding the
	// lock cannot be the first to come back: whichever did is the one that was
	// refused. The refusal has to be actionable on its own — it names the
	// participant that is doubled up and the file it could not take.
	if refusal.code != 2 {
		t.Fatalf("the first wait to exit exited %d, want 2 and the lock error\nstdout: %s\nstderr: %s",
			refusal.code, refusal.stdout, refusal.stderr)
	}
	if !strings.Contains(refusal.stderr, `"claude"`) {
		t.Fatalf("the lock error does not name the nick: %s", refusal.stderr)
	}
	if !strings.Contains(refusal.stderr, lock) {
		t.Fatalf("the lock error does not name the lock path: %s", refusal.stderr)
	}

	// And the other one is not merely still running: it is a working wait on
	// the stream, so a peer message ends it with that message delivered.
	const body = "the holder was still waiting"
	mustRun(t, "", "send", "--stream", chat, "--nick", "codex", "--", body)
	held := join(holder)
	if held.code != 0 {
		t.Fatalf("the wait that held the lock exited %d, want 0\nstdout: %s\nstderr: %s",
			held.code, held.stdout, held.stderr)
	}
	if n := countHeaders(held.stdout, "codex"); n != 1 {
		t.Fatalf("the wait that held the lock printed %d peer headers, want 1\n%s", n, held.stdout)
	}
	if !strings.Contains(held.stdout, body) {
		t.Fatalf("the delivery does not carry the message body:\n%s", held.stdout)
	}
	if strings.Contains(held.stderr, lock) {
		t.Fatalf("the wait that held the lock still reported the lock error: %s", held.stderr)
	}
}

func TestUnknownStreamScheme(t *testing.T) {
	stateDir := t.TempDir()
	for _, args := range [][]string{
		{"send", "--stream", "irc:foo", "--nick", "claude", "--", "hello"},
		{"wait", "--stream", "irc:foo", "--nick", "claude", "--state-dir", stateDir, "--timeout", "1s"},
	} {
		res := runParley(t, "", args...)
		if res.code != 2 {
			t.Fatalf("parley %v exited %d, want 2\nstderr: %s", args, res.code, res.stderr)
		}
		if !strings.Contains(res.stderr, "irc") {
			t.Fatalf("error does not name the scheme: %s", res.stderr)
		}
	}
}

func TestConfigurationErrorsExitTwo(t *testing.T) {
	stateDir := t.TempDir()
	chat := "file:" + filepath.Join(t.TempDir(), "chat.ndjson")
	// The channel cases carry the facts every channel launch requires, so each
	// is refused for the reason it names and not for the missing flags.
	f := launchFacts(t)
	cases := [][]string{
		{"send", "--stream", chat, "--", "no nick"},
		{"wait", "--stream", chat, "--state-dir", stateDir},
		{"send", "--nick", "claude", "--", "no stream"},
		{"send", "--stream", "nocolon", "--nick", "claude", "--", "bad spec"},
		{"send", "--stream", "file:", "--nick", "claude", "--", "no path"},
		{"send", "--stream", chat, "--nick", "claude", "--", "   "},
		{"wait", "--stream", chat, "--nick", "claude", "--state-dir", stateDir, "extra"},
		{"send", "--stream", chat, "--nick", "two words", "--", "spaced nick"},
		{"send", "--stream", chat, "--nick", "a/b", "--", "nick with a separator"},
		{"send", "--stream", chat, "--nick", "tab\there", "--", "nick with a tab"},
		// Outside the nick grammar: the header can only render what the grammar
		// admits, so a nick it would have to rewrite is refused here instead.
		{"send", "--stream", chat, "--nick", "a+b", "--", "nick with a plus"},
		{"send", "--stream", chat, "--nick", "a@b", "--", "nick with an at sign"},
		{"send", "--stream", chat, "--nick", "codex😀", "--", "nick with an emoji"},
		f.verb("channel", "--stream", chat, "--state-dir", stateDir),
		f.verb("channel", "--nick", "claude", "--state-dir", stateDir),
		f.verb("channel", "--stream", chat, "--nick", "claude", "--state-dir", stateDir, "extra"),
		f.verb("channel", "--stream", "irc:foo", "--nick", "claude", "--state-dir", stateDir),
		// The codex front takes one flag more than the others, and it has no
		// default: a pump with no thread would queue into nothing. /bin/echo
		// stands in for an installed codex wherever the case is about something
		// else, so nothing here depends on a codex being on this machine.
		{"codex", "--stream", chat, "--nick", "claude", "--state-dir", stateDir, "--codex-bin", "/bin/echo"},
		{"codex", "--stream", chat, "--nick", "claude", "--state-dir", stateDir, "--thread", "   ", "--codex-bin", "/bin/echo"},
		{"codex", "--stream", chat, "--state-dir", stateDir, "--thread", "t", "--codex-bin", "/bin/echo"},
		{"codex", "--nick", "claude", "--state-dir", stateDir, "--thread", "t", "--codex-bin", "/bin/echo"},
		{"codex", "--stream", "irc:foo", "--nick", "claude", "--state-dir", stateDir, "--thread", "t", "--codex-bin", "/bin/echo"},
		{"codex", "--stream", chat, "--nick", "claude", "--state-dir", stateDir, "--thread", "t", "--codex-bin", "/bin/echo", "extra"},
		{"codex", "--stream", chat, "--nick", "claude", "--state-dir", stateDir, "--thread", "t", "--debounce", "-1s", "--codex-bin", "/bin/echo"},
		{"codex", "--stream", chat, "--nick", "claude", "--state-dir", stateDir, "--thread", "t", "--codex-bin", "/nonexistent/codex"},
		{"wait", "--stream", chat, "--nick", "..", "--state-dir", stateDir},
		{"wait", "--stream", chat, "--nick", strings.Repeat("n", 65), "--state-dir", stateDir},
		{"bogus"},
		{},
	}
	for _, args := range cases {
		res := runParley(t, "", args...)
		if res.code != 2 {
			t.Fatalf("parley %v exited %d, want 2\nstdout: %s\nstderr: %s", args, res.code, res.stdout, res.stderr)
		}
	}
}

func TestCursorIsPerStream(t *testing.T) {
	dir := t.TempDir()
	stateDir := t.TempDir()
	a := "file:" + filepath.Join(dir, "a.ndjson")
	b := "file:" + filepath.Join(dir, "b.ndjson")

	mustRun(t, "", "send", "--stream", a, "--nick", "codex", "--", "first on A")
	mustRun(t, "", "send", "--stream", b, "--nick", "codex", "--", "first on B")

	got := mustRun(t, "", "wait", "--stream", a, "--nick", "claude", "--state-dir", stateDir, "--timeout", "10s", "--debounce", "100ms")
	if !strings.Contains(got.stdout, "first on A") {
		t.Fatalf("A was not delivered:\n%s", got.stdout)
	}
	if _, err := os.Stat(cursorPath(t, stateDir, "claude", a)); err != nil {
		t.Fatalf("no cursor stored for A: %v", err)
	}

	// The same nick on another stream starts at that stream's beginning: A's
	// position means nothing in B.
	other := mustRun(t, "", "wait", "--stream", b, "--nick", "claude", "--state-dir", stateDir, "--timeout", "10s", "--debounce", "100ms")
	if !strings.Contains(other.stdout, "first on B") {
		t.Fatalf("B's first message was skipped by A's cursor:\n%s", other.stdout)
	}
	if strings.Contains(other.stdout, "gap:") {
		t.Fatalf("B reported a gap from A's cursor:\n%s", other.stdout)
	}
}

func TestOneFileKeepsOneCursorHoweverItIsSpelled(t *testing.T) {
	dir := resolvedTempDir(t)
	stateDir := t.TempDir()
	abs := "file:" + filepath.Join(dir, "chat.ndjson")

	mustRun(t, "", "send", "--stream", abs, "--nick", "codex", "--", "delivered once")

	// Named relatively, from the directory holding it.
	got := mustRunIn(t, dir, "", "wait", "--stream", "file:./chat.ndjson", "--nick", "claude", "--state-dir", stateDir, "--timeout", "10s", "--debounce", "100ms")
	if !strings.Contains(got.stdout, "delivered once") {
		t.Fatalf("the relative spec delivered nothing:\n%s", got.stdout)
	}

	// The absolute spelling of the same file resumes where the relative one
	// left off; a key hashed from the spec as typed would start it over.
	again := mustRun(t, "", "wait", "--stream", abs, "--nick", "claude", "--state-dir", stateDir, "--timeout", "300ms")
	if again.stdout != "" {
		t.Fatalf("the absolute spelling re-delivered what the relative one already saw:\n%s", again.stdout)
	}
	// One cursor, not two: the state tree holds a single stream directory.
	entries, err := os.ReadDir(filepath.Join(stateDir, "claude"))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var streams []string
	for _, e := range entries {
		if e.IsDir() {
			streams = append(streams, e.Name())
		}
	}
	if len(streams) != 1 || streams[0] != state.StreamKey(abs) {
		t.Fatalf("state holds stream directories %v, want only %s", streams, state.StreamKey(abs))
	}
}

func TestTwoFilesWithTheSameRelativeSpellingDoNotShareACursor(t *testing.T) {
	a, b := resolvedTempDir(t), resolvedTempDir(t)
	stateDir := t.TempDir()
	const spec = "file:./chat.ndjson"

	mustRunIn(t, a, "", "send", "--stream", spec, "--nick", "codex", "--", "written in A")
	mustRunIn(t, b, "", "send", "--stream", spec, "--nick", "codex", "--", "written in B")

	gotA := mustRunIn(t, a, "", "wait", "--stream", spec, "--nick", "claude", "--state-dir", stateDir, "--timeout", "10s", "--debounce", "100ms")
	if !strings.Contains(gotA.stdout, "written in A") {
		t.Fatalf("A was not delivered:\n%s", gotA.stdout)
	}
	// B is a different file that happens to be spelled the same way. Sharing A's
	// cursor would skip B's first message or report a gap on it.
	gotB := mustRunIn(t, b, "", "wait", "--stream", spec, "--nick", "claude", "--state-dir", stateDir, "--timeout", "10s", "--debounce", "100ms")
	if !strings.Contains(gotB.stdout, "written in B") {
		t.Fatalf("B's message was skipped by A's cursor:\n%s", gotB.stdout)
	}
	if strings.Contains(gotB.stdout, "gap:") {
		t.Fatalf("B reported a gap from A's cursor:\n%s", gotB.stdout)
	}

	keyA := state.StreamKey("file:" + filepath.Join(a, "chat.ndjson"))
	keyB := state.StreamKey("file:" + filepath.Join(b, "chat.ndjson"))
	if keyA == keyB {
		t.Fatalf("both files hash to the state key %s", keyA)
	}
	for _, key := range []string{keyA, keyB} {
		if _, err := os.Stat(filepath.Join(stateDir, "claude", key, "cursor")); err != nil {
			t.Fatalf("no cursor stored under %s: %v", key, err)
		}
	}
}

func TestFailedDeliveryWriteLeavesTheCursorUnsaved(t *testing.T) {
	chat := "file:" + filepath.Join(t.TempDir(), "chat.ndjson")
	stateDir := t.TempDir()
	mustRun(t, "", "send", "--stream", chat, "--nick", "codex", "--", "for claude")

	cmd := exec.Command(binary(t), "wait", "--stream", chat, "--nick", "claude", "--state-dir", stateDir, "--timeout", "10s", "--debounce", "100ms")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	// The read end is gone before the process can write, so the delivery write
	// fails however quickly it runs.
	if err := stdout.Close(); err != nil {
		t.Fatalf("close the read end: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start wait: %v", err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("wait exited 0 with nowhere to write the delivery")
	}

	if _, err := os.Stat(cursorPath(t, stateDir, "claude", chat)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("cursor exists after a failed write (stat err %v)", err)
	}
	again := mustRun(t, "", "wait", "--stream", chat, "--nick", "claude", "--state-dir", stateDir, "--timeout", "10s", "--debounce", "100ms")
	if !strings.Contains(again.stdout, "for claude") {
		t.Fatalf("the undelivered message was not handed over again:\n%s", again.stdout)
	}
}

func TestConcurrentSendProcessesDoNotInterleave(t *testing.T) {
	const writers, perWriter = 8, 5
	path := filepath.Join(t.TempDir(), "chat.ndjson")
	chat := "file:" + path

	want := map[string]int{}
	body := func(w, i int) string {
		return fmt.Sprintf("agent%d message %d with padding %s", w, i, strings.Repeat("x", (w*perWriter+i)*13))
	}
	for w := 0; w < writers; w++ {
		for i := 0; i < perWriter; i++ {
			want[fmt.Sprintf("agent%d\x00%s", w, body(w, i))]++
		}
	}

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				cmd := exec.Command(binary(t), "send", "--stream", chat, "--nick", fmt.Sprintf("agent%d", w), "--", body(w, i))
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Errorf("send from agent%d: %v\n%s", w, err, out)
					return
				}
			}
		}()
	}
	wg.Wait()

	s, err := file.Open(path, "reader")
	if err != nil {
		t.Fatalf("open the stream: %v", err)
	}
	msgs, _, err := s.Read(context.Background(), "")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	got := map[string]int{}
	for _, m := range msgs {
		got[m.Author+"\x00"+m.Body]++
	}
	if len(got) != len(want) {
		t.Fatalf("read %d distinct messages, want %d", len(got), len(want))
	}
	for k, n := range want {
		if got[k] != n {
			author, body, _ := strings.Cut(k, "\x00")
			t.Fatalf("message from %s appeared %d times, want %d: %q", author, got[k], n, body)
		}
	}
}

func TestSendValidatesFlagsBeforeReadingStdin(t *testing.T) {
	const body = "this body must survive a rejected invocation\n"
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe: %v", err)
	}
	defer r.Close()
	if _, err := w.WriteString(body); err != nil {
		t.Fatalf("write to the pipe: %v", err)
	}
	// Closing the only write end makes the read below terminate at EOF.
	if err := w.Close(); err != nil {
		t.Fatalf("close the pipe: %v", err)
	}

	cmd := exec.Command(binary(t), "send", "--stream", "irc:nope", "--nick", "claude")
	cmd.Stdin = r
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err = cmd.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 2 {
		t.Fatalf("send exited %v, want 2\nstderr: %s", err, stderr.String())
	}
	var buf strings.Builder
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("read what is left on stdin: %v", err)
	}
	if buf.String() != body {
		t.Fatalf("stdin holds %q, want the body untouched: a rejected invocation must not consume it", buf.String())
	}
}

// lockPath is where the binary keeps this nick's single-process lock.
func lockPath(t *testing.T, stateDir, nick, spec string) string {
	t.Helper()
	d, err := state.Open(stateDir, nick, spec)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	return d.LockPath()
}

// TestChannelServesOverStdioAndReleasesTheLock drives the real binary the way
// Claude Code drives an MCP stdio server: JSON-RPC lines in on stdin, JSON-RPC
// lines out on stdout, diagnostics on stderr, and the session ending when the
// client closes stdin.
func TestChannelServesOverStdioAndReleasesTheLock(t *testing.T) {
	// Short paths: the stream spec and the state directory both land in the
	// instructions, and t.TempDir's names carry the test's long name.
	root := shortRoot(t)
	spec := "file:" + filepath.Join(root, "chat.ndjson")
	stateDir := filepath.Join(root, "s")

	// No --skill and no --cap: a build installs no gate, so the peer's
	// message below is delivered with no line from the human in the channel.
	cmd := exec.Command(binary(t), launchFacts(t).verb("channel", "--stream", spec, "--nick", "claude-a", "--state-dir", stateDir)...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start channel: %v", err)
	}
	t.Cleanup(func() {
		stdin.Close()
		_ = cmd.Wait()
	})

	lines := make(chan string, 8)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	next := func() map[string]any {
		t.Helper()
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatalf("the server closed stdout; stderr: %s", stderr.String())
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				t.Fatalf("stdout line is not JSON: %v\n%s", err, line)
			}
			return m
		case <-time.After(20 * time.Second):
			t.Fatalf("nothing on stdout; stderr: %s", stderr.String())
			return nil
		}
	}

	if _, err := io.WriteString(stdin, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`+"\n"); err != nil {
		t.Fatalf("write initialize: %v", err)
	}
	if resp := next(); resp["id"] != float64(1) {
		t.Fatalf("initialize answered %v", resp)
	}
	if _, err := io.WriteString(stdin, `{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n"); err != nil {
		t.Fatalf("write initialized: %v", err)
	}

	// A second process for this nick is refused while the session holds the
	// lock, whichever verb it is. The initialize response read above is the
	// proof that the session already holds it: cmdChannel takes the lock before
	// channel.New, so nothing is answered on stdout until the flock is held.
	if res := runParley(t, "", "wait", "--stream", spec, "--nick", "claude-a", "--state-dir", stateDir, "--timeout", "1s"); res.code != 2 {
		t.Fatalf("a second process for the nick exited %d, want 2\nstderr: %s", res.code, res.stderr)
	}

	mustRun(t, "", "send", "--stream", spec, "--nick", "codex-b", "--", "hello over stdio")

	msg := next()
	if msg["method"] != "notifications/claude/channel" {
		t.Fatalf("stdout carried %v, want a channel notification", msg)
	}
	params, ok := msg["params"].(map[string]any)
	if !ok {
		t.Fatalf("notification has no params: %v", msg)
	}
	content, _ := params["content"].(string)
	if !strings.HasPrefix(content, "[parley] from=codex-b id=0 ts=") || !strings.Contains(content, "hello over stdio") {
		t.Fatalf("content is not the peer message under the parley header:\n%s", content)
	}
	meta, ok := params["meta"].(map[string]any)
	if !ok || meta["nick"] != "claude-a" || meta["count"] != "1" {
		t.Fatalf("meta %v, want nick claude-a and count 1", params["meta"])
	}

	// Closing stdin is how the client ends the session: the server exits 0 and
	// the lock goes with it.
	if err := stdin.Close(); err != nil {
		t.Fatalf("close stdin: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("channel exited %v, want 0\nstderr: %s", err, stderr.String())
	}
	f, err := os.OpenFile(lockPath(t, stateDir, "claude-a", spec), os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open lock: %v", err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("the lock was not released on exit: %v", err)
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

// syncBuffer collects a child's stderr while the test is still reading it. A
// non-file writer handed to os/exec is filled by a goroutine of its own, so an
// assertion that quotes the stderr of a process still running would otherwise
// race it.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// fakeCodex writes a stand-in for the codex CLI and returns the directory to
// put first on PATH and the file the stand-in records its argv into. The record
// is NUL-separated and written through a rename, so a test that finds the file
// finds a whole argv rather than half of one.
func fakeCodex(t *testing.T) (binDir, record string) {
	t.Helper()
	binDir = t.TempDir()
	record = filepath.Join(t.TempDir(), "argv")
	script := "#!/bin/sh\n" +
		"{ for a in \"$@\"; do printf '%s\\0' \"$a\"; done; } > \"$PARLEY_TEST_RECORD.tmp\"\n" +
		"mv \"$PARLEY_TEST_RECORD.tmp\" \"$PARLEY_TEST_RECORD\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "codex"), []byte(script), 0o700); err != nil {
		t.Fatalf("write the fake codex: %v", err)
	}
	return binDir, record
}

// readArgv waits for the fake codex to record one invocation and returns its
// arguments.
func readArgv(t *testing.T, record string, stderr *syncBuffer) []string {
	t.Helper()
	deadline := time.Now().Add(childDeadline)
	for {
		b, err := os.ReadFile(record)
		if err == nil {
			return strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00")
		}
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("read the recorded argv: %v", err)
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("codex was never run; parley stderr:\n%s", stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestCodexQueuesTheDeliveryIntoTheThread drives the real binary the way the
// human runs it beside a live Codex session: a foreground pump, a fake codex
// first on PATH, and one `codex queue` call carrying exactly what `parley wait`
// would have printed — the peer header included, since on this front the header
// is the only thing that says the text is not the human's.
func TestCodexQueuesTheDeliveryIntoTheThread(t *testing.T) {
	dir := resolvedTempDir(t)
	spec := "file:" + filepath.Join(dir, "chat.ndjson")
	stateDir := t.TempDir()
	binDir, record := fakeCodex(t)
	const thread = "0199c0de-c0de-7c0d-8c0d-ec0dec0dec0d"

	cmd := exec.Command(binary(t), "codex", "--stream", spec, "--nick", "codex-a",
		"--state-dir", stateDir, "--thread", thread, "--debounce", "100ms")
	// No --codex-bin: the default name is what gets resolved, on the PATH this
	// child is given.
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"PARLEY_TEST_RECORD="+record)
	stderr := &syncBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start codex: %v", err)
	}
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			_ = cmd.Process.Kill()
			select {
			case <-joinCmd(cmd):
			case <-time.After(childDeadline):
			}
		}
	})

	// A second process for this nick is refused while the pump holds the lock,
	// whichever verb it is. /bin/echo stands in for a codex that exists, so the
	// refusal is the lock's and not the binary's.
	//
	// Sequenced behind the pump's startup line, which cmdCodex prints only once
	// dir.Lock has returned: the second pump below cannot land between the
	// first's own OpenFile and its non-blocking flock and take the lock out from
	// under it.
	waitForStderr(t, stderr, "into Codex thread "+thread)
	second := runParleyWithin(t, childDeadline, "codex", "--stream", spec, "--nick", "codex-a",
		"--state-dir", stateDir, "--thread", thread, "--codex-bin", "/bin/echo")
	if second.code != 2 {
		t.Fatalf("a second codex pump for the nick exited %d, want 2\nstderr: %s", second.code, second.stderr)
	}
	if !strings.Contains(second.stderr, `"codex-a"`) {
		t.Fatalf("the lock error does not name the nick: %s", second.stderr)
	}

	mustRun(t, "", "send", "--stream", spec, "--nick", "claude-b", "--", "hello from the peer")
	argv := readArgv(t, record, stderr)

	// What a third participant is handed for the same message is what codex got:
	// a different nick and a state directory of its own, so its cursor starts
	// where the pump's did.
	want := mustRun(t, "", "wait", "--stream", spec, "--nick", "watcher-c",
		"--state-dir", t.TempDir(), "--timeout", "10s", "--debounce", "100ms").stdout
	if want == "" {
		t.Fatal("the reference wait printed nothing")
	}
	got := []string{"queue", "--thread", thread, "--message", want}
	if len(argv) != len(got) {
		t.Fatalf("codex was run with %d arguments, want %d: %q\nstderr: %s", len(argv), len(got), argv, stderr.String())
	}
	for i := range got {
		if argv[i] != got[i] {
			t.Fatalf("argument %d is\n%q\nwant\n%q", i, argv[i], got[i])
		}
	}
	if !strings.HasPrefix(argv[4], "[parley] from=claude-b id=") {
		t.Fatalf("the queued message does not begin with the peer header:\n%q", argv[4])
	}

	// A signal is how the human ends the pump, and it is not a failure. The join
	// has a deadline of its own: a pump that does not come back from a SIGTERM
	// is this test's failure, with its log, and not a package-wide timeout.
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal the pump: %v", err)
	}
	stopped = true
	done := joinCmd(cmd)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("codex exited %v on SIGTERM, want 0\nstderr: %s", err, stderr.String())
		}
	case <-time.After(childDeadline):
		_ = cmd.Process.Kill()
		t.Fatalf("the pump did not exit within %s of the SIGTERM\nstderr: %s", childDeadline, stderr.String())
	}
	if !strings.Contains(stderr.String(), "shutting down") {
		t.Fatalf("the shutdown was not logged:\n%s", stderr.String())
	}
}

// TestCodexRefusesABinaryItCannotFind keeps the resolution ahead of the lock: a
// codex that is not installed is a bad invocation, exit 2, and the nick's lock
// is never taken to say so.
func TestCodexRefusesABinaryItCannotFind(t *testing.T) {
	spec := "file:" + filepath.Join(t.TempDir(), "chat.ndjson")
	stateDir := t.TempDir()
	missing := filepath.Join(t.TempDir(), "not-codex")

	res := runParley(t, "", "codex", "--stream", spec, "--nick", "codex-a",
		"--state-dir", stateDir, "--thread", "t", "--codex-bin", missing)
	if res.code != 2 {
		t.Fatalf("codex with a missing binary exited %d, want 2\nstderr: %s", res.code, res.stderr)
	}
	if !strings.Contains(res.stderr, missing) {
		t.Fatalf("the error does not name the binary: %s", res.stderr)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "codex-a", "lock")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the refused invocation took the nick's lock (stat err %v)", err)
	}
}

func TestUsage(t *testing.T) {
	res := mustRun(t, "", "-h")
	for _, want := range []string{"parley send", "parley wait", "parley channel", "parley codex", "parley primer",
		"--thread", "file:<path>", "irc://host:port/channel", "--state-dir", "--body-file", "irc/<host>_<port>/password",
		"--front claude|codex", "--reply-file", "--reply-command", "It opens nothing", "the canonical spec",
		"--thread <thread>|auto", "auto is reserved", "codex-thread", "CODEX_THREAD_ID",
		"parley opencode", "--session <ses_id>|auto", "opencode-session"} {
		if !strings.Contains(res.stdout, want) {
			t.Fatalf("usage does not mention %q:\n%s", want, res.stdout)
		}
	}
	none := runParley(t, "")
	if none.code != 2 || !strings.Contains(none.stderr, "parley wait") {
		t.Fatalf("bare parley exited %d with stderr %q", none.code, none.stderr)
	}
}

// replyCommandLine returns the one line of a primer that is the reply command.
// The model is told to run that line as it stands, so a primer that carried two
// of them, or none, would be a primer with no unambiguous way to answer.
func replyCommandLine(t *testing.T, out string) string {
	t.Helper()
	var found string
	for _, l := range strings.Split(out, "\n") {
		if !strings.Contains(l, " send --stream ") {
			continue
		}
		if found != "" {
			t.Fatalf("two lines carry the reply command:\n%s", out)
		}
		found = l
	}
	if found == "" {
		t.Fatalf("no reply command in the primer:\n%s", out)
	}
	return found
}

// TestPrimerPrintsTheCodexForm: the text that is a Codex session's first
// message, carrying the three facts as values. Every value is printed as it was
// spelled — the spec keeps the spelling that was passed, and the relative
// command and state directory are left relative.
func TestPrimerPrintsTheCodexForm(t *testing.T) {
	f := launchFacts(t)
	res := mustRun(t, "", f.verb("primer", "--front", "codex", "--stream", "irc://127.0.0.1:6667/parley",
		"--nick", "codex-b", "--state-dir", "./state", "--command", "../../bin/parley")...)
	for _, want := range []string{"your human partner's registered nick " + f.chair + ";", "target repo " + f.target + ";",
		"your worktree " + f.worktree + ". The protocol is the parley-build skill, invoked as $parley-build."} {
		if !strings.Contains(res.stdout, want) {
			t.Fatalf("the codex primer does not carry %q:\n%s", want, res.stdout)
		}
	}
	for _, unwanted := range []string{"<human nick>", "<target repo root>", "<your worktree of it>", "pasted"} {
		if strings.Contains(res.stdout, unwanted) {
			t.Fatalf("the codex primer still carries %q:\n%s", unwanted, res.stdout)
		}
	}
	if !strings.HasPrefix(res.stdout, "This primer is your first message because") || !strings.HasSuffix(res.stdout, "to read.\n") {
		t.Fatalf("the codex primer does not open with its opening and end with its front paragraph:\n%s", res.stdout)
	}
	want := "'../../bin/parley' send --stream 'irc://127.0.0.1:6667/parley' --nick codex-b --state-dir './state' --body-file '<reply file path>'"
	if got := replyCommandLine(t, res.stdout); got != want {
		t.Fatalf("the reply command is\n%q\nwant\n%q", got, want)
	}
	// The reply file is never defaulted into the state directory: the lock, the
	// cursors and the IRC account password live there.
	if strings.Contains(res.stdout, "state/reply") {
		t.Fatalf("the codex primer puts the reply file in the state directory:\n%s", res.stdout)
	}
	if !strings.Contains(res.stdout, "invoked as $parley-build") {
		t.Fatalf("the codex primer does not invoke the skill as Codex does:\n%s", res.stdout)
	}
}

// TestPrimerPrintsAWrapperAsTheWholeReplyCommand: an approval rule matches one
// command and everything appended to it, so the command a rule can safely name
// is a script that holds the run's arguments and discards its own. With
// --reply-command the primer tells the model to run exactly that, and the
// flags it stands in for are gone from the text: a flag named in prose is one
// a model can be talked into typing.
//
// It does not go through replyCommandLine, which looks for the send line this
// form is here to replace.
func TestPrimerPrintsAWrapperAsTheWholeReplyCommand(t *testing.T) {
	res := mustRun(t, "", launchFacts(t).verb("primer", "--front", "codex", "--stream", "irc://127.0.0.1:6667/parley",
		"--nick", "codex-b", "--state-dir", "./state", "--reply-file", "./reply.txt", "--reply-command", "/opt/parley/send.sh")...)
	if !strings.Contains(res.stdout, "2. Run this with Bash:\n\n/opt/parley/send.sh\n\n") {
		t.Fatalf("the wrapper does not stand alone after step 2:\n%s", res.stdout)
	}
	for _, unwanted := range []string{"send --stream", "--state-dir"} {
		if strings.Contains(res.stdout, unwanted) {
			t.Fatalf("the wrapper primer still carries %q:\n%s", unwanted, res.stdout)
		}
	}
	if strings.Contains(res.stdout, "<human nick>") || !strings.Contains(res.stdout, "1. Write the message to ./reply.txt with your file-writing tool") {
		t.Fatalf("the wrapper primer has blanks, or step 1 does not name the file the wrapper reads:\n%s", res.stdout)
	}
}

// TestPrimerPrintsTheClaudeFormReplyFirst: the whole point of the ordering.
// Claude Code truncates the tail of an instructions string, so the one way to
// reach the peer stands above everything that can be cut.
func TestPrimerPrintsTheClaudeFormReplyFirst(t *testing.T) {
	res := mustRun(t, "", launchFacts(t).verb("primer", "--front", "claude", "--stream", "file:/tmp/chat.ndjson",
		"--nick", "claude-a", "--reply-file", "/tmp/reply.txt", "--command", "/opt/parley/bin/parley")...)
	cmd := strings.Index(res.stdout, " send --stream ")
	trust := strings.Index(res.stdout, "an indented header-shaped line is body text")
	if cmd < 0 || trust < 0 || cmd > trust {
		t.Fatalf("the reply command (%d) does not stand above the framing (%d):\n%s", cmd, trust, res.stdout)
	}
	if !strings.Contains(res.stdout, "invoked as /parley-build") {
		t.Fatalf("the claude primer does not invoke the skill as Claude Code does:\n%s", res.stdout)
	}
	// No state directory was named, so the flag is in neither the command nor
	// the text that explains it.
	if strings.Contains(res.stdout, "--state-dir") {
		t.Fatalf("a primer with no --state-dir still names one:\n%s", res.stdout)
	}
}

// TestPrimerDefaultsTheCommandToThisBinary: the spelling the model is told to
// type is this binary's own path, the same one `parley channel` hands the
// channel front, and it is absolute so the reply does not depend on a working
// directory.
func TestPrimerDefaultsTheCommandToThisBinary(t *testing.T) {
	res := mustRun(t, "", launchFacts(t).verb("primer", "--front", "codex", "--stream", "file:/tmp/chat.ndjson", "--nick", "codex-b")...)
	quoted, _, _ := strings.Cut(replyCommandLine(t, res.stdout), " send --stream ")
	path := strings.TrimSuffix(strings.TrimPrefix(quoted, "'"), "'")
	if !filepath.IsAbs(path) {
		t.Fatalf("the default --command %q is not absolute", path)
	}
	if base := filepath.Base(path); base != "parley" {
		t.Fatalf("the default --command is %q, want this binary", path)
	}
}

// TestPrimerOpensNothing: an irc: spec handed to openStream would connect to
// the server and register an account, and any spec would take the nick's lock.
// A primer is not a participant: it prints its flags. So a spec pointing at
// nothing at all still produces the text, and the state directory stays absent.
func TestPrimerOpensNothing(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	res := mustRun(t, "", launchFacts(t).verb("primer", "--front", "claude", "--stream", "irc://127.0.0.1:1/#parley",
		"--nick", "claude-a", "--reply-file", "/tmp/reply.txt", "--state-dir", stateDir)...)
	if !strings.Contains(res.stdout, "'irc://127.0.0.1:1/#parley'") {
		t.Fatalf("the primer does not carry the unreachable spec it was given:\n%s", res.stdout)
	}
	if !strings.Contains(res.stdout, "--state-dir "+shellQuote(stateDir)) {
		t.Fatalf("the primer does not name the state directory it was given:\n%s", res.stdout)
	}
	if _, err := os.Stat(stateDir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the primer created the state directory (stat err %v): it took a lock or a cursor", err)
	}
}

func TestPrimerRejectsABadInvocation(t *testing.T) {
	const spec = "file:/tmp/chat.ndjson"
	// Every case carries the facts the primer requires, so that each is refused
	// for the reason it names; the cases about the facts themselves are in
	// TestSkillConfigurationErrorsExitTwo.
	fa := launchFacts(t).args()
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no front", []string{"primer", "--stream", spec, "--nick", "codex-b"}, "unknown front"},
		{"an unknown front", []string{"primer", "--front", "other", "--stream", spec, "--nick", "codex-b"}, `unknown front "other"`},
		{"no stream", []string{"primer", "--front", "codex", "--nick", "codex-b"}, "--stream is required"},
		{"no nick", []string{"primer", "--front", "codex", "--stream", spec}, "--nick is required"},
		{"a bad nick", []string{"primer", "--front", "codex", "--stream", spec, "--nick", "co dex"}, "--nick"},
		{"claude with no reply file", []string{"primer", "--front", "claude", "--stream", spec, "--nick", "claude-a"}, "--reply-file is required"},
		{"an empty command", []string{"primer", "--front", "codex", "--stream", spec, "--nick", "codex-b", "--command", ""}, "--command must not be empty"},
		{"a stray argument", []string{"primer", "--front", "codex", "--stream", spec, "--nick", "codex-b", "extra"}, `unexpected argument "extra"`},
		{"a reply command on the claude front", []string{"primer", "--front", "claude", "--stream", spec, "--nick", "claude-a",
			"--reply-file", "/tmp/reply.txt", "--reply-command", "/opt/parley/send.sh"}, "--reply-command applies to the codex front only"},
		{"an empty reply command", []string{"primer", "--front", "codex", "--stream", spec, "--nick", "codex-b",
			"--reply-command", ""}, "--reply-command must not be empty"},
		{"a reply command of two lines", []string{"primer", "--front", "codex", "--stream", spec, "--nick", "codex-b",
			"--reply-command", "/opt/parley/send.sh\nrm -rf ./state"}, "--reply-command must be one line"},
		// The wrapper reads a fixed file; the codex placeholder would have step 1
		// name a file the wrapper never sends.
		{"a wrapper with no reply file", []string{"primer", "--front", "codex", "--stream", spec, "--nick", "codex-b",
			"--reply-command", "/opt/parley/send.sh"}, "--reply-file is required with --reply-command"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"primer"}, append(append([]string(nil), fa...), tc.args[1:]...)...)
			res := runParley(t, "", args...)
			if res.code != 2 {
				t.Fatalf("primer with %s exited %d, want 2\nstderr: %s", tc.name, res.code, res.stderr)
			}
			if !strings.Contains(res.stderr, tc.want) {
				t.Fatalf("the error does not say %q: %s", tc.want, res.stderr)
			}
			if res.stdout != "" {
				t.Fatalf("a refused primer still printed text:\n%s", res.stdout)
			}
		})
	}
}

// shellQuote is the primer's quoting, restated here so a test can build the
// command line it expects without importing the renderer.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// TestSendTakesAStateDirectory: send keeps no cursor and takes no lock, but it
// has to be able to read the credential a transport holds for this participant
// — the account password the pump registered — or it could not speak as them.
func TestSendTakesAStateDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chat.ndjson")
	res := runParley(t, "", "send", "--stream", "file:"+path, "--nick", "alpha",
		"--state-dir", t.TempDir(), "--", "a body")
	if res.code != 0 {
		t.Fatalf("send with --state-dir exited %d\nstderr: %s", res.code, res.stderr)
	}
	b, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(b), "a body") {
		t.Fatalf("the message did not reach the stream (%v): %s", err, b)
	}
}

// TestOpenStreamIRCKeysOneChannelOnce: the state key is the canonical spelling
// the adapter returns, so a channel spelled three ways keeps one cursor.
func TestOpenStreamIRCKeysOneChannelOnce(t *testing.T) {
	const want = "irc://127.0.0.1:6667/#parley"
	for _, spec := range []string{
		"irc://127.0.0.1:6667/parley",
		"irc://127.0.0.1:6667/#Parley",
		"irc://127.0.0.1:6667/%23parley",
		"irc://127.0.0.1:6667/PARLEY",
	} {
		// Nothing is listening on that port: opening an irc stream must not
		// touch the network, so that a bad invocation fails on the invocation
		// and a server that is down fails on the verb that needed it.
		s, key, err := openStream(spec, "alpha", t.TempDir())
		if err != nil {
			t.Fatalf("openStream(%q): %v", spec, err)
		}
		closeStream(s)
		if key != want {
			t.Errorf("openStream(%q) keyed the state on %q, want %q", spec, key, want)
		}
	}
}

// TestOpenStreamAcceptsTheSchemeInAnyCase: a URL scheme is case-insensitive and
// the adapters parse it that way, so the factory that dispatches on it has to
// agree — otherwise IRC:// is refused by the one part of parley that never
// looked at the spec.
func TestOpenStreamAcceptsTheSchemeInAnyCase(t *testing.T) {
	for _, tc := range []struct{ spec, key string }{
		{"IRC://127.0.0.1:6667/parley", "irc://127.0.0.1:6667/#parley"},
		{"Irc://127.0.0.1:6667/#Parley", "irc://127.0.0.1:6667/#parley"},
		// A host name is case-insensitive too, so one server keys one cursor
		// and one account password however it was spelled.
		{"irc://LocalHost:6667/parley", "irc://localhost:6667/#parley"},
	} {
		s, key, err := openStream(tc.spec, "alpha", t.TempDir())
		if err != nil {
			t.Errorf("openStream(%q): %v", tc.spec, err)
			continue
		}
		closeStream(s)
		if key != tc.key {
			t.Errorf("openStream(%q) keyed the state on %q, want %q", tc.spec, key, tc.key)
		}
	}
	path := filepath.Join(resolvedTempDir(t), "chat.ndjson")
	s, key, err := openStream("FILE:"+path, "alpha", t.TempDir())
	if err != nil {
		t.Fatalf("openStream with an upper-case file scheme: %v", err)
	}
	closeStream(s)
	if key != "file:"+path {
		t.Errorf("openStream keyed the state on %q, want %q", key, "file:"+path)
	}
}

func TestOpenStreamRefusesBadIRCSpecs(t *testing.T) {
	for _, spec := range []string{
		"irc://127.0.0.1/parley",            // no port
		"irc://127.0.0.1:6667",              // no channel
		"irc://127.0.0.1:6667/",             // empty channel
		"irc://127.0.0.1:6667/a/b",          // a path, not a channel name
		"irc://colin@127.0.0.1:6667/parley", // userinfo
		"irc://127.0.0.1:6667/parley?tls=1", // a query
		"irc://127.0.0.1:6667/parley#m2",    // a fragment
		"ircs://127.0.0.1:6697/parley",      // a scheme parley does not speak
	} {
		s, _, err := openStream(spec, "alpha", t.TempDir())
		if err == nil {
			closeStream(s)
			t.Errorf("openStream(%q) was accepted", spec)
			continue
		}
		var cfg *configError
		if !errors.As(err, &cfg) {
			t.Errorf("openStream(%q) failed with %v, which does not exit 2", spec, err)
		}
	}
}

// TestTableFlagsConfigurationErrorsExitTwo covers every way the table flags can
// be wrong with --skill table on the two fronts that take them. Each case names
// the flag at fault in its error, so a refusal for some other reason cannot pass
// for one of these.
func TestTableFlagsConfigurationErrorsExitTwo(t *testing.T) {
	stateDir := t.TempDir()
	chat := "file:" + filepath.Join(t.TempDir(), "chat.ndjson")
	f := launchFacts(t)
	fronts := map[string][]string{
		// The chair is each case's own, so the base carries the other facts only.
		"channel": {"channel", "--stream", chat, "--nick", "claude", "--state-dir", stateDir,
			"--skill", "table", "--target", f.target, "--worktree", f.worktree},
		// /bin/echo stands in for an installed codex, so nothing here depends on
		// one being on this machine. The pump takes no --target or --worktree.
		"codex": {"codex", "--stream", chat, "--nick", "claude", "--state-dir", stateDir,
			"--skill", "table", "--thread", "t", "--codex-bin", "/bin/echo"},
	}
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no chair and no cap", nil, "--chair is required with --skill table"},
		{"cap without chair", []string{"--cap", "3"}, "--chair is required with --skill table"},
		{"cap zero without chair", []string{"--cap", "0"}, "--chair is required with --skill table"},
		{"chair without cap", []string{"--chair", "colin"}, "--cap is required with --skill table"},
		{"chair is the nick", []string{"--chair", "claude", "--cap", "3"}, "own --nick"},
		// The server folds ASCII case in nicks, so this chair is the participant.
		{"chair is the nick in another case", []string{"--chair", "Claude", "--cap", "3"}, "own --nick"},
		{"chair is the nick in capitals", []string{"--chair", "CLAUDE", "--cap", "3"}, "own --nick"},
		{"chair with a space", []string{"--chair", "two words", "--cap", "3"}, "--chair"},
		{"chair with a separator", []string{"--chair", "a/b", "--cap", "3"}, "--chair"},
		{"chair too long", []string{"--chair", strings.Repeat("n", 65), "--cap", "3"}, "--chair"},
		{"chair dot dot", []string{"--chair", "..", "--cap", "3"}, "--chair"},
		{"empty chair", []string{"--chair", "", "--cap", "3"}, "--chair"},
		{"negative cap", []string{"--chair", "colin", "--cap", "-1"}, "--cap must not be negative"},
		{"cap is not a number", []string{"--chair", "colin", "--cap", "many"}, "cap"},
	}
	for front, base := range fronts {
		for _, c := range cases {
			args := append(append([]string(nil), base...), c.args...)
			res := runParley(t, "", args...)
			if res.code != 2 {
				t.Fatalf("%s: parley %v exited %d, want 2\nstdout: %s\nstderr: %s", c.name, args, res.code, res.stdout, res.stderr)
			}
			if !strings.Contains(res.stderr, c.want) {
				t.Fatalf("%s on %s: stderr does not mention %q:\n%s", c.name, front, c.want, res.stderr)
			}
		}
	}
	// None of those got as far as the nick's state: the refusal comes before the
	// lock is taken.
	if _, err := os.Stat(filepath.Join(stateDir, "claude", "lock")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a refused invocation took the nick's lock (stat err %v)", err)
	}
}

// TestTheGateFollowsTheSkill: the table gate is the table skill's and nothing
// else's, so --cap is refused with --skill build, which is the default, on both
// fronts that would install it, and the pump takes --chair only with the skill
// that uses it and no directory at all.
func TestTheGateFollowsTheSkill(t *testing.T) {
	stateDir := t.TempDir()
	chat := "file:" + filepath.Join(t.TempDir(), "chat.ndjson")
	f := launchFacts(t)
	channel := f.verb("channel", "--stream", chat, "--nick", "claude", "--state-dir", stateDir)
	codex := []string{"codex", "--stream", chat, "--nick", "claude", "--state-dir", stateDir, "--thread", "t", "--codex-bin", "/bin/echo"}
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"channel: cap with the default skill", append(append([]string(nil), channel...), "--cap", "3"), "--cap applies to --skill table or line only"},
		{"channel: cap zero with the default skill", append(append([]string(nil), channel...), "--cap", "0"), "--cap applies to --skill table or line only"},
		{"channel: cap with the named build skill", append(append([]string(nil), channel...), "--skill", "build", "--cap", "3"), "--cap applies to --skill table or line only"},
		{"codex: cap with the default skill", append(append([]string(nil), codex...), "--cap", "3"), "--cap applies to --skill table or line only"},
		{"codex: cap with the named build skill", append(append([]string(nil), codex...), "--skill", "build", "--cap", "0"), "--cap applies to --skill table or line only"},
		{"codex: chair with the default skill", append(append([]string(nil), codex...), "--chair", "colin"), "--chair applies to --skill table or line only"},
		{"codex: chair and cap with the default skill", append(append([]string(nil), codex...), "--chair", "colin", "--cap", "3"), "applies to --skill table or line only"},
		{"codex: table without a chair", append(append([]string(nil), codex...), "--skill", "table"), "--chair is required with --skill table"},
		{"codex: table with a chair and no cap", append(append([]string(nil), codex...), "--skill", "table", "--chair", "colin"), "--cap is required with --skill table"},
		{"codex: unknown skill", append(append([]string(nil), codex...), "--skill", "other"), `unknown skill "other"`},
		{"codex: the old name of build", append(append([]string(nil), codex...), "--skill", "parley"), `unknown skill "parley": expected "build", "table" or "line"`},
		{"codex: a target", append(append([]string(nil), codex...), "--target", f.target), "flag provided but not defined"},
		{"codex: a worktree", append(append([]string(nil), codex...), "--worktree", f.worktree), "flag provided but not defined"},
	} {
		res := runParley(t, "", tc.args...)
		if res.code != 2 || !strings.Contains(res.stderr, tc.want) {
			t.Fatalf("%s: parley %v exited %d, want 2 saying %q\nstdout: %s\nstderr: %s", tc.name, tc.args, res.code, tc.want, res.stdout, res.stderr)
		}
	}
	if _, err := os.Stat(filepath.Join(stateDir, "claude", "lock")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a refused invocation took the nick's lock (stat err %v)", err)
	}
}

// TestWaitTakesNeitherTableFlag keeps the one-shot verb free of the gates: a
// wait that held anything back would hand its caller an empty delivery for a
// message that was in the stream.
func TestWaitTakesNeitherTableFlag(t *testing.T) {
	stateDir := t.TempDir()
	chat := "file:" + filepath.Join(t.TempDir(), "chat.ndjson")
	for _, extra := range [][]string{
		{"--chair", "x"},
		{"--cap", "3"},
		{"--chair", "x", "--cap", "3"},
	} {
		args := append([]string{"wait", "--stream", chat, "--nick", "claude", "--state-dir", stateDir, "--timeout", "1s"}, extra...)
		res := runParley(t, "", args...)
		if res.code != 2 {
			t.Fatalf("parley %v exited %d, want 2\nstderr: %s", args, res.code, res.stderr)
		}
		if !strings.Contains(res.stderr, "flag provided but not defined") {
			t.Fatalf("parley %v was refused for another reason:\n%s", args, res.stderr)
		}
	}
}

func TestUsageDocumentsTheTableFlags(t *testing.T) {
	top := mustRun(t, "", "-h").stdout
	for _, want := range []string{"--chair <nick> --cap <n>", "tables:", "--cap 0 turns the cap", "in this process's memory only", "wait takes neither flag",
		"[--skill build|table|line --chair <nick> --cap <n> [--window 10m]]", "they take no --target or --worktree"} {
		if !strings.Contains(top, want) {
			t.Fatalf("usage does not mention %q:\n%s", want, top)
		}
	}
	// The per-verb help prints the flags with their own descriptions, on stdout.
	for front, usage := range map[string]string{"channel": "[--skill build|table --cap <n>]", "codex": "[--skill build|table|line --chair <nick> --cap <n> [--window 10m]]", "opencode": "[--skill build|table|line --chair <nick> --cap <n> [--window 10m]]"} {
		help := runParley(t, "", front, "-h").stdout
		for _, want := range []string{"-chair", "-cap", "-skill", usage, "memory only"} {
			if !strings.Contains(help, want) {
				t.Fatalf("parley %s -h does not mention %q:\n%s", front, want, help)
			}
		}
	}
	if help := runParley(t, "", "wait", "-h").stdout; strings.Contains(help, "chair") || strings.Contains(help, "-cap") {
		t.Fatalf("parley wait -h mentions the table flags:\n%s", help)
	}
}

// TestCodexHoldsTheOtherSeatsUntilItHasPosted drives the real binary with the
// table flags through the whole of round one: a seat's post is held, the
// chair's passes, and the pump's own first post opens the seal and releases what
// was held. It is what proves the flags reach the Deliverer, which the unit
// tests of the Deliverer cannot.
func TestCodexHoldsTheOtherSeatsUntilItHasPosted(t *testing.T) {
	dir := resolvedTempDir(t)
	spec := "file:" + filepath.Join(dir, "chat.ndjson")
	stateDir := t.TempDir()
	binDir, record := fakeCodex(t)
	const thread = "0199c0de-c0de-7c0d-8c0d-ec0dec0dec0d"

	cmd := exec.Command(binary(t), "codex", "--stream", spec, "--nick", "codex-a",
		"--state-dir", stateDir, "--thread", thread, "--debounce", "100ms",
		"--skill", "table", "--chair", "colin", "--cap", "0")
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"PARLEY_TEST_RECORD="+record)
	stderr := &syncBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start codex: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		select {
		case <-joinCmd(cmd):
		case <-time.After(childDeadline):
		}
	})
	// The startup line is printed after the Deliverer is built, so the posts
	// below are all made after it, which is what lets the pump's own post open
	// the seal.
	waitForStderr(t, stderr, "into Codex thread "+thread)

	mustRun(t, "", "send", "--stream", spec, "--nick", "claude-b", "--", "seat talk")
	mustRun(t, "", "send", "--stream", spec, "--nick", "colin", "--", "chair talk")
	argv := readArgv(t, record, stderr)
	if msg := argv[len(argv)-1]; !strings.Contains(msg, "from=colin") || strings.Contains(msg, "seat talk") {
		t.Fatalf("the sealed pump delivered\n%s\nwant the chair's post alone", msg)
	}

	if err := os.Remove(record); err != nil {
		t.Fatalf("clear the record: %v", err)
	}
	mustRun(t, "", "send", "--stream", spec, "--nick", "codex-a", "--", "my answer")
	argv = readArgv(t, record, stderr)
	msg := argv[len(argv)-1]
	if !strings.Contains(msg, "from=claude-b") || !strings.Contains(msg, "seat talk") || strings.Contains(msg, "from=colin") || strings.Contains(msg, "my answer") {
		t.Fatalf("the pump's own post delivered\n%s\nwant the held seat post alone", msg)
	}
}
