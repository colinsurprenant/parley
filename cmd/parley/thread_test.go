package main

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/colinsurprenant/parley/internal/state"
)

const (
	recordedThreadID = "0199c0de-c0de-7c0d-8c0d-ec0dec0dec0d"
	otherThreadID    = "0199c0de-beef-7c0d-8c0d-ec0dec0dec0d"
)

// recordThread writes the thread record the way the next verb will, through the
// state API, for the canonical form of a file: stream.
func recordThread(t *testing.T, stateDir, nick, spec, id string) *state.ThreadRecord {
	t.Helper()
	r, err := state.OpenThreadRecord(stateDir, nick, spec)
	if err != nil {
		t.Fatalf("OpenThreadRecord: %v", err)
	}
	if err := r.Write(id); err != nil {
		t.Fatalf("write the thread record: %v", err)
	}
	return r
}

// envWithout is the test process's environment less one variable, plus extra
// assignments: a child whose behaviour turns on $CODEX_THREAD_ID must not
// inherit the one a developer's own Codex shell has set.
func envWithout(name string, extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, name+"=") {
			env = append(env, kv)
		}
	}
	return append(env, extra...)
}

// runParleyEnv runs the binary with exactly the given environment.
func runParleyEnv(t *testing.T, env []string, stdin string, args ...string) result {
	t.Helper()
	cmd := exec.Command(binary(t), args...)
	cmd.Env = env
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	res := result{}
	err := cmd.Run()
	res.stdout, res.stderr = stdout.String(), stderr.String()
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("run parley %v: %v", args, err)
		}
		res.code = exit.ExitCode()
	}
	return res
}

// startAutoPump starts `parley codex --thread auto` with the fake codex first
// on PATH and returns its stderr and the file the fake records argv into. The
// pump is stopped when the test ends.
func startAutoPump(t *testing.T, env []string, spec, nick string, args ...string) (stderr *syncBuffer, record string) {
	t.Helper()
	binDir, record := fakeCodex(t)
	argv := append([]string{"codex", "--stream", spec, "--nick", nick, "--thread", "auto", "--debounce", "100ms"}, args...)
	cmd := exec.Command(binary(t), argv...)
	cmd.Env = append(env,
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"PARLEY_TEST_RECORD="+record)
	stderr = &syncBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start codex: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-joinCmd(cmd):
		case <-time.After(childDeadline):
			_ = cmd.Process.Kill()
		}
	})
	return stderr, record
}

func wantQueuedInto(t *testing.T, argv []string, thread string, stderr *syncBuffer) {
	t.Helper()
	if len(argv) != 5 || argv[0] != "queue" || argv[1] != "--thread" || argv[2] != thread || argv[3] != "--message" {
		t.Fatalf("codex was run with %q, want queue --thread %s --message <text>\nstderr: %s", argv, thread, stderr.String())
	}
	if !strings.HasPrefix(argv[4], "[parley] from=claude-b id=") {
		t.Fatalf("the queued message does not begin with the peer header:\n%q", argv[4])
	}
}

// TestCodexThreadAutoDeliversIntoTheRecordedThread: with a record written
// through the state API, --thread auto is that thread, and the start line says
// which it resolved to.
func TestCodexThreadAutoDeliversIntoTheRecordedThread(t *testing.T) {
	dir := resolvedTempDir(t)
	spec := "file:" + filepath.Join(dir, "chat.ndjson")
	stateDir := t.TempDir()
	rec := recordThread(t, stateDir, "codex-a", spec, recordedThreadID)

	stderr, record := startAutoPump(t, os.Environ(), spec, "codex-a", "--state-dir", stateDir)
	waitForStderr(t, stderr, "into Codex thread "+recordedThreadID)
	if !strings.Contains(stderr.String(), rec.Path()) {
		t.Fatalf("the start line does not say where the thread came from (%s):\n%s", rec.Path(), stderr.String())
	}

	mustRun(t, "", "send", "--stream", spec, "--nick", "claude-b", "--", "hello from the peer")
	wantQueuedInto(t, readArgv(t, record, stderr), recordedThreadID, stderr)
}

// TestCodexThreadAutoReadsTheRecordFromTheStateDirectory: the record is read
// from the base --state-dir resolves to, not from the default one.
func TestCodexThreadAutoReadsTheRecordFromTheStateDirectory(t *testing.T) {
	dir := resolvedTempDir(t)
	spec := "file:" + filepath.Join(dir, "chat.ndjson")
	stateDir := t.TempDir()
	defaultBase := t.TempDir()
	recordThread(t, stateDir, "codex-a", spec, recordedThreadID)
	// A record for the same nick and stream under the default base, which a
	// pump that ignored --state-dir would find instead.
	recordThread(t, filepath.Join(defaultBase, "parley"), "codex-a", spec, otherThreadID)

	stderr, record := startAutoPump(t, envWithout("XDG_STATE_HOME", "XDG_STATE_HOME="+defaultBase), spec, "codex-a", "--state-dir", stateDir)
	waitForStderr(t, stderr, "into Codex thread "+recordedThreadID)
	mustRun(t, "", "send", "--stream", spec, "--nick", "claude-b", "--state-dir", t.TempDir(), "--", "hello")
	wantQueuedInto(t, readArgv(t, record, stderr), recordedThreadID, stderr)
}

// TestCodexThreadAutoUsesTheDefaultStateDirectory: with no --state-dir the
// record is where the default base puts it.
func TestCodexThreadAutoUsesTheDefaultStateDirectory(t *testing.T) {
	dir := resolvedTempDir(t)
	spec := "file:" + filepath.Join(dir, "chat.ndjson")
	defaultBase := t.TempDir()
	recordThread(t, filepath.Join(defaultBase, "parley"), "codex-a", spec, recordedThreadID)

	env := envWithout("XDG_STATE_HOME", "XDG_STATE_HOME="+defaultBase)
	stderr, record := startAutoPump(t, env, spec, "codex-a")
	waitForStderr(t, stderr, "into Codex thread "+recordedThreadID)
	mustRun(t, "", "send", "--stream", spec, "--nick", "claude-b", "--state-dir", t.TempDir(), "--", "hello")
	wantQueuedInto(t, readArgv(t, record, stderr), recordedThreadID, stderr)
}

// TestCodexThreadAutoWithoutARecordExitsTwo: no record is the invocation's
// mistake, said before anything is opened: no lock, and no directory.
func TestCodexThreadAutoWithoutARecordExitsTwo(t *testing.T) {
	spec := "file:" + filepath.Join(t.TempDir(), "chat.ndjson")
	stateDir := t.TempDir()

	res := runParley(t, "", "codex", "--stream", spec, "--nick", "codex-a",
		"--state-dir", stateDir, "--thread", "auto", "--codex-bin", "/bin/echo")
	if res.code != 2 {
		t.Fatalf("codex --thread auto with no record exited %d, want 2\nstderr: %s", res.code, res.stderr)
	}
	for _, want := range []string{"parley table new", "--thread <id>"} {
		if !strings.Contains(res.stderr, want) {
			t.Fatalf("the error does not say %q: %s", want, res.stderr)
		}
	}
	if _, err := os.Stat(filepath.Join(stateDir, "codex-a")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the refused invocation made the nick's state directory (stat err %v)", err)
	}
}

// TestCodexThreadAutoWithAMalformedRecordExitsTwo names the file, and offers the
// manual form.
func TestCodexThreadAutoWithAMalformedRecordExitsTwo(t *testing.T) {
	spec := "file:" + filepath.Join(resolvedTempDir(t), "chat.ndjson")
	stateDir := t.TempDir()
	rec := recordThread(t, stateDir, "codex-a", spec, recordedThreadID)
	if err := os.WriteFile(rec.Path(), []byte("not a thread\n"), 0o600); err != nil {
		t.Fatalf("corrupt the record: %v", err)
	}

	res := runParley(t, "", "codex", "--stream", spec, "--nick", "codex-a",
		"--state-dir", stateDir, "--thread", "auto", "--codex-bin", "/bin/echo")
	if res.code != 2 {
		t.Fatalf("codex --thread auto with a malformed record exited %d, want 2\nstderr: %s", res.code, res.stderr)
	}
	if !strings.Contains(res.stderr, rec.Path()) || !strings.Contains(res.stderr, "--thread <id>") {
		t.Fatalf("the error does not name the record and the manual form: %s", res.stderr)
	}
}

// TestCodexThreadAutoIsReservedButNothingElseIs: auto is the one value that is
// read as a record, and any other is still the thread, verbatim, whatever record
// there is.
func TestCodexThreadAutoIsReservedButNothingElseIs(t *testing.T) {
	dir := resolvedTempDir(t)
	spec := "file:" + filepath.Join(dir, "chat.ndjson")
	stateDir := t.TempDir()
	recordThread(t, stateDir, "codex-a", spec, recordedThreadID)
	binDir, record := fakeCodex(t)

	cmd := exec.Command(binary(t), "codex", "--stream", spec, "--nick", "codex-a",
		"--state-dir", stateDir, "--thread", "autopilot", "--debounce", "100ms")
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"PARLEY_TEST_RECORD="+record)
	stderr := &syncBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start codex: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-joinCmd(cmd):
		case <-time.After(childDeadline):
			_ = cmd.Process.Kill()
		}
	})
	waitForStderr(t, stderr, "into Codex thread autopilot")
	mustRun(t, "", "send", "--stream", spec, "--nick", "claude-b", "--state-dir", t.TempDir(), "--", "hello")
	wantQueuedInto(t, readArgv(t, record, stderr), "autopilot", stderr)
}

// TestSendWarnsWhenThisSessionIsNotThePumpsThread: $CODEX_THREAD_ID against the
// record. A mismatch is one warning on stderr and the post goes out regardless;
// a match, no variable, no record are silence; a record that cannot be read is a
// warning and not a failure.
func TestSendWarnsWhenThisSessionIsNotThePumpsThread(t *testing.T) {
	const nick = "codex-a"
	cases := []struct {
		name    string
		session string // $CODEX_THREAD_ID; unset when empty
		record  string // the record's content; no record when empty
		warns   bool
		wantIn  []string
	}{
		{name: "a different thread", session: otherThreadID, record: recordedThreadID + "\n", warns: true,
			wantIn: []string{"this session is Codex thread \"" + otherThreadID + "\"", "delivers into thread " + recordedThreadID, "will not reach this session"}},
		{name: "the same thread", session: recordedThreadID, record: recordedThreadID + "\n"},
		{name: "the same thread in another case", session: strings.ToUpper(recordedThreadID), record: recordedThreadID + "\n"},
		{name: "no variable", record: recordedThreadID + "\n"},
		{name: "no record", session: otherThreadID},
		{name: "a variable carrying a control sequence", session: otherThreadID + "\x1b[2J", record: recordedThreadID + "\n", warns: true,
			wantIn: []string{`\x1b[2J`, "will not reach this session"}},
		{name: "a malformed record", session: otherThreadID, record: "garbage\n", warns: true,
			wantIn: []string{"cannot check", "malformed"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(resolvedTempDir(t), "chat.ndjson")
			spec := "file:" + path
			stateDir := t.TempDir()
			if c.record != "" {
				rec := recordThread(t, stateDir, nick, spec, recordedThreadID)
				if err := os.WriteFile(rec.Path(), []byte(c.record), 0o600); err != nil {
					t.Fatalf("write the record: %v", err)
				}
			}
			env := envWithout(codexThreadEnv)
			if c.session != "" {
				env = append(env, codexThreadEnv+"="+c.session)
			}

			res := runParleyEnv(t, env, "", "send", "--stream", spec, "--nick", nick,
				"--state-dir", stateDir, "--", "a body from the seat")
			if res.code != 0 {
				t.Fatalf("send exited %d, want 0: a warning is never a failure\nstderr: %s", res.code, res.stderr)
			}
			if !strings.HasPrefix(res.stdout, "[parley] sent id=") {
				t.Fatalf("no receipt on stdout: %q", res.stdout)
			}
			if b, err := os.ReadFile(path); err != nil || !strings.Contains(string(b), "a body from the seat") {
				t.Fatalf("the post did not reach the stream (%v): %s", err, b)
			}
			if !c.warns {
				if res.stderr != "" {
					t.Fatalf("send was meant to stay silent, stderr: %q", res.stderr)
				}
				return
			}
			if n := strings.Count(res.stderr, "\n"); n != 1 || !strings.HasPrefix(res.stderr, "parley: warning: ") {
				t.Fatalf("want exactly one warning line, stderr: %q", res.stderr)
			}
			for _, want := range c.wantIn {
				if !strings.Contains(res.stderr, want) {
					t.Fatalf("the warning does not say %q: %q", want, res.stderr)
				}
			}
		})
	}
}

// TestSendWarnsBeforeTouchingStdin: the cross-check comes after the flags are
// validated and before the body is read, so a send that then fails on its body
// has already said it.
func TestSendWarnsBeforeTouchingStdin(t *testing.T) {
	spec := "file:" + filepath.Join(resolvedTempDir(t), "chat.ndjson")
	stateDir := t.TempDir()
	recordThread(t, stateDir, "codex-a", spec, recordedThreadID)
	env := envWithout(codexThreadEnv, codexThreadEnv+"="+otherThreadID)

	res := runParleyEnv(t, env, "   \n", "send", "--stream", spec, "--nick", "codex-a", "--state-dir", stateDir)
	if res.code != 2 {
		t.Fatalf("send of a blank body exited %d, want 2\nstderr: %s", res.code, res.stderr)
	}
	if !strings.Contains(res.stderr, "will not reach this session") {
		t.Fatalf("no warning before the body failed: %q", res.stderr)
	}

	// A rejected invocation says nothing about threads: the flags come first.
	res = runParleyEnv(t, env, "", "send", "--stream", spec, "--state-dir", stateDir, "--", "no nick")
	if res.code != 2 || strings.Contains(res.stderr, "will not reach this session") {
		t.Fatalf("send with no nick exited %d with stderr %q, want 2 and no thread warning", res.code, res.stderr)
	}
}

// TestCodexHelpSaysAutoIsReserved: the flag's own help is where someone whose
// session is named auto finds out why it is not found.
func TestCodexHelpSaysAutoIsReserved(t *testing.T) {
	res := runParley(t, "", "codex", "-h")
	if res.code != 0 {
		t.Fatalf("codex -h exited %d\nstderr: %s", res.code, res.stderr)
	}
	for _, want := range []string{"--thread <thread>|auto", "auto is reserved", "by its UUID", "parley table new"} {
		if !strings.Contains(res.stdout, want) {
			t.Fatalf("codex -h does not say %q:\n%s", want, res.stdout)
		}
	}
}
