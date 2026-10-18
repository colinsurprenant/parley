package main

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/colinsurprenant/parley/internal/primer"
	"github.com/colinsurprenant/parley/internal/state"
)

var checkLineRE = regexp.MustCompile(`^instructions: (\d+) of 2048 UTF-16 units\n$`)

// tableChannelArgs are the arguments of a table seat's channel launch.
func tableChannelArgs(root, target string) []string {
	return []string{"channel", "--stream", "file:" + filepath.Join(root, "c"), "--nick", "fable",
		"--state-dir", filepath.Join(root, "s"), "--chair", "colin", "--cap", "6",
		"--skill", "table", "--target", target, "--worktree", filepath.Join(root, "w")}
}

// buildChannelArgs are the arguments of a build's channel launch, which
// carries the same three facts and no cap.
func buildChannelArgs(root, target string) []string {
	return []string{"channel", "--stream", "file:" + filepath.Join(root, "c"), "--nick", "fable",
		"--state-dir", filepath.Join(root, "s"), "--chair", "colin",
		"--target", target, "--worktree", filepath.Join(root, "w")}
}

// skillChannelArgs is the launch of the skill named.
func skillChannelArgs(skill, root, target string) []string {
	if skill == "table" {
		return tableChannelArgs(root, target)
	}
	return buildChannelArgs(root, target)
}

// runCheck runs `parley channel <args> --check` with TMPDIR at tmp, so the reply
// directory the check makes is the only thing that can appear there.
func runCheck(t *testing.T, tmp string, args ...string) result {
	t.Helper()
	cmd := exec.Command(binary(t), append(append([]string(nil), args...), "--check")...)
	cmd.Env = append(os.Environ(), "TMPDIR="+tmp)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	res := result{}
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("run parley %v: %v", args, err)
		}
		res.code = exit.ExitCode()
	}
	res.stdout, res.stderr = stdout.String(), stderr.String()
	return res
}

func checkedLength(t *testing.T, res result) int {
	t.Helper()
	m := checkLineRE.FindStringSubmatch(res.stdout)
	if m == nil {
		t.Fatalf("--check printed %q, want one line of the form %q\nstderr: %s", res.stdout, "instructions: <n> of 2048 UTF-16 units", res.stderr)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("length %q: %v", m[1], err)
	}
	return n
}

// TestChannelCheckMeasuresWhatTheChannelSends: --check builds the text through
// the code a session uses, so its length is the real handshake's, except in the
// one thing that is a draw: the digits of the random name of the reply
// directory, which the text holds some number of times. It measures at the
// longest name there can be, so it is never under what a launch sends, whatever
// that launch's draw.
func TestChannelCheckMeasuresWhatTheChannelSends(t *testing.T) {
	for _, skill := range []string{"build", "table"} {
		t.Run(skill, func(t *testing.T) { checkMeasuresWhatTheChannelSends(t, skill) })
	}
}

func checkMeasuresWhatTheChannelSends(t *testing.T, skill string) {
	root := shortRoot(t)
	args := skillChannelArgs(skill, root, filepath.Join(root, "t"))
	tmp := t.TempDir()

	// Several checks and several launches, so each is held against more than one
	// draw of the name: a measure at one random name is under a launch that draws
	// a longer one a fair part of the time, and the shortest check is the one that
	// must still cover the longest launch.
	n := -1
	for range 8 {
		res := runCheck(t, tmp, args...)
		if res.code != 0 || res.stderr != "" {
			t.Fatalf("--check exited %d with stderr %q, want 0 and nothing", res.code, res.stderr)
		}
		if got := checkedLength(t, res); n < 0 || got < n {
			n = got
		}
	}

	var m int
	for range 8 {
		real, _ := channelInitialize(t, []string{"TMPDIR=" + tmp}, args...)
		m = primer.UTF16Len(real)
		if n < m {
			t.Fatalf("--check measured %d, under the %d the channel sends for the same arguments: a pass would not guarantee the launch fits", n, m)
		}
		dir := regexp.MustCompile(`parley-reply-fable-(\d+)`).FindStringSubmatch(real)
		if dir == nil {
			t.Fatalf("the real instructions name no reply directory:\n%s", real)
		}
		// Not wasteful either: the most it can be over is the digits the name has
		// to spare, in each place the text writes it.
		times := strings.Count(real, "parley-reply-fable-"+dir[1])
		if over := n - m; over > times*(len("4294967295")-len(dir[1])) {
			t.Fatalf("--check measured %d, the channel sends %d (%d digits in the name, written %d times): more over than the digits of the name explain", n, m, len(dir[1]), times)
		}
	}
	// Not the primer's measure of the same flags, which is shorter: it lacks the
	// channel's resolved state directory, reply directory and canonical spec.
	primerArgs := []string{"primer", "--front", "claude", "--skill", skill, "--stream", "file:" + filepath.Join(root, "c"), "--nick", "fable",
		"--chair", "colin", "--target", filepath.Join(root, "t"), "--worktree", filepath.Join(root, "w"),
		"--state-dir", filepath.Join(root, "s"), "--reply-file", filepath.Join(tmp, "reply.txt")}
	if got := primer.UTF16Len(mustRun(t, "", primerArgs...).stdout); got >= m {
		t.Fatalf("the primer measures %d, not under the channel's %d: the case is not showing what --check is for", got, m)
	}
}

// TestChannelCheckLeavesNoReplyDirectory: the directory the check makes to find
// the length is removed whether the text fits or not.
func TestChannelCheckLeavesNoReplyDirectory(t *testing.T) {
	root := shortRoot(t)
	long := filepath.Join(root, "t", strings.Repeat("d", 250), strings.Repeat("e", 250))
	if err := os.MkdirAll(long, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, skill := range []string{"build", "table"} {
		for name, target := range map[string]string{"within": filepath.Join(root, "t"), "over": long} {
			tmp := t.TempDir()
			runCheck(t, tmp, skillChannelArgs(skill, root, target)...)
			if left, _ := filepath.Glob(filepath.Join(tmp, "parley-reply-*")); len(left) != 0 {
				t.Fatalf("a check of a %s launch that is %s the cap left reply directories behind: %v", skill, name, left)
			}
		}
	}
}

// TestChannelCheckSurvivesAClosedStdout: the check makes a reply directory to
// measure with and removes it on the way out. A reader that has gone away must
// not end the process before that — Go ends one that writes to a broken stdout
// pipe unless SIGPIPE is handled — or the directory is left in the temp
// directory, once for every such run.
func TestChannelCheckSurvivesAClosedStdout(t *testing.T) {
	root := shortRoot(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("close the read end: %v", err)
	}
	defer w.Close()

	tmp := t.TempDir()
	cmd := exec.Command(binary(t), append(tableChannelArgs(root, filepath.Join(root, "t")), "--check")...)
	cmd.Env = append(os.Environ(), "TMPDIR="+tmp)
	cmd.Stdout = w
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err = cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() < 0 {
		t.Fatalf("--check with a closed stdout was ended by a signal (%v), want it to handle SIGPIPE\nstderr: %s", exit, stderr.String())
	}
	if left, _ := filepath.Glob(filepath.Join(tmp, "parley-reply-*")); len(left) != 0 {
		t.Fatalf("--check with a closed stdout left reply directories behind: %v", left)
	}
}

// TestChannelCheckExitsZeroWithinTheCapAndTwoOverIt: the line on stdout is the
// measurement either way; over the cap the refusal, which names the length and
// the cap, is on stderr and the exit is 2.
func TestChannelCheckExitsZeroWithinTheCapAndTwoOverIt(t *testing.T) {
	root := shortRoot(t)
	long := filepath.Join(root, "t", strings.Repeat("d", 250), strings.Repeat("e", 250))
	if err := os.MkdirAll(long, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, skill := range []string{"build", "table"} {
		within := runCheck(t, t.TempDir(), skillChannelArgs(skill, root, filepath.Join(root, "t"))...)
		if within.code != 0 {
			t.Fatalf("a %s launch within the cap exited %d\nstderr: %s", skill, within.code, within.stderr)
		}
		if n := checkedLength(t, within); n > primer.ClaudeInstructionsCap {
			t.Fatalf("measured %d, over the cap, with exit 0", n)
		}

		over := runCheck(t, t.TempDir(), skillChannelArgs(skill, root, long)...)
		if over.code != 2 {
			t.Fatalf("a %s launch over the cap exited %d, want 2\nstdout: %s\nstderr: %s", skill, over.code, over.stdout, over.stderr)
		}
		n := checkedLength(t, over)
		if n <= primer.ClaudeInstructionsCap {
			t.Fatalf("measured %d, within the cap, with exit 2", n)
		}
		if strings.Count(over.stderr, "\n") != 1 || !strings.Contains(over.stderr, strconv.Itoa(n)+" UTF-16 code units") || !strings.Contains(over.stderr, "cap of 2048") || !strings.Contains(over.stderr, "--target") {
			t.Fatalf("stderr is not one line naming the length %d, the cap and what to shorten: %q", n, over.stderr)
		}
	}
}

// TestChannelCheckReportsAnOverlongBuildForm: the check says so before the
// launch, and the launch, which refuses the same text, says the same.
func TestChannelCheckReportsAnOverlongBuildForm(t *testing.T) {
	root := shortRoot(t)
	args := buildChannelArgs(root, filepath.Join(root, "t"))
	for i := range args {
		if args[i] == filepath.Join(root, "s") {
			args[i] = filepath.Join(root, "s", strings.Repeat("d", 150), strings.Repeat("e", 150))
		}
	}
	res := runCheck(t, t.TempDir(), args...)
	if res.code != 2 {
		t.Fatalf("an over-cap build form exited %d, want 2\nstderr: %s", res.code, res.stderr)
	}
	if n := checkedLength(t, res); n <= primer.ClaudeInstructionsCap {
		t.Fatalf("measured %d, within the cap, with exit 2", n)
	}
	if !strings.Contains(res.stderr, "cap of 2048") || strings.Count(res.stderr, "\n") != 1 {
		t.Fatalf("stderr is not one line naming the cap: %q", res.stderr)
	}
	// The same flags without --check are refused the same way, and serve nothing.
	if launch := runParley(t, "", args...); launch.code != 2 || launch.stdout != "" || !strings.Contains(launch.stderr, "cap of 2048") {
		t.Fatalf("the launch exited %d with stdout %q and stderr %q, want the same refusal", launch.code, launch.stdout, launch.stderr)
	}
}

// TestChannelCheckOpensNoStreamTakesNoLockAndMakesNoState: the check serves
// nothing. A file: stream in a directory that does not exist could not be
// opened, and an irc: server at a port nothing listens on could not be reached;
// neither stops it, and neither the state directory nor the lock is created.
func TestChannelCheckOpensNoStreamTakesNoLockAndMakesNoState(t *testing.T) {
	root := shortRoot(t)
	missing := filepath.Join(root, "no", "such", "dir", "chat.ndjson")
	for name, spec := range map[string]string{
		"file": "file:" + missing,
		"irc":  "irc://127.0.0.1:1/table",
	} {
		stateDir := filepath.Join(root, "state-"+name)
		res := runCheck(t, shortRoot(t), launchFacts(t).verb("channel", "--stream", spec, "--nick", "fable", "--state-dir", stateDir)...)
		if res.code != 0 {
			t.Fatalf("%s: --check exited %d\nstderr: %s", name, res.code, res.stderr)
		}
		checkedLength(t, res)
		if _, err := os.Stat(stateDir); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s: --check made the state directory (stat err %v)", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "no")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("--check made the stream's directory (stat err %v)", err)
	}

	// And a nick whose lock is held elsewhere is still measured: the check never
	// asks for it.
	held := shortRoot(t)
	chat := filepath.Join(held, "c")
	stateDir := filepath.Join(held, "s")
	dir, err := state.Open(stateDir, "fable", "file:"+chat)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	lock, err := dir.Lock()
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	defer lock.Release()
	if res := runCheck(t, t.TempDir(), launchFacts(t).verb("channel", "--stream", "file:"+chat, "--nick", "fable", "--state-dir", stateDir)...); res.code != 0 {
		t.Fatalf("--check beside a held lock exited %d\nstderr: %s", res.code, res.stderr)
	}
}

// TestChannelCheckValidatesTheFlags: a check of a launch that would be refused is
// refused the same way, with the same exit, and measures nothing.
func TestChannelCheckValidatesTheFlags(t *testing.T) {
	root := shortRoot(t)
	good := tableChannelArgs(root, filepath.Join(root, "t"))
	with := func(extra ...string) []string { return append(append([]string(nil), good...), extra...) }
	f := launchFacts(t)
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no nick", f.verb("channel", "--stream", "file:"+filepath.Join(root, "c")), "--nick is required"},
		{"no stream", f.verb("channel", "--nick", "fable"), "--stream is required"},
		{"bad scheme", f.verb("channel", "--stream", "ftp:x", "--nick", "fable"), "unknown stream scheme"},
		{"no chair", []string{"channel", "--stream", "file:" + filepath.Join(root, "c"), "--nick", "fable", "--target", f.target, "--worktree", f.worktree}, "--chair is required with --skill build"},
		{"cap with the build skill", append(f.verb("channel", "--stream", "file:"+filepath.Join(root, "c"), "--nick", "fable"), "--cap", "3"), "--cap applies to --skill table or line only"},
		{"chair is the nick", with("--chair", "FABLE"), "own --nick"},
		{"nick tables", with("--nick", "Tables"), "cannot be a nick"},
		{"chair builds", with("--chair", "BUILDS"), "cannot be a nick"},
		{"missing target", append(append([]string(nil), good[:len(good)-4]...), "--target", filepath.Join(root, "nope"), "--worktree", filepath.Join(root, "w")), "--target"},
		{"stray argument", with("stray"), "unexpected argument"},
	} {
		res := runCheck(t, t.TempDir(), tc.args...)
		if res.code != 2 || res.stdout != "" || !strings.Contains(res.stderr, tc.want) {
			t.Fatalf("%s: exit %d, stdout %q, stderr %q; want exit 2, nothing on stdout, and %q", tc.name, res.code, res.stdout, res.stderr, tc.want)
		}
	}
}
