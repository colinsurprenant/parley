package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/colinsurprenant/parley/internal/primer"
)

// goldenSample are the flags that make `parley primer` render sample() from the
// primer package's tests, whose render is the golden file the build form is
// held to. The sample's chair is spelled as it is there; its target repo and
// worktree are not real directories, which the verb requires, so the real ones
// are passed and goldenWant puts the golden's spelling of them in their place.
func goldenSample(front, target, worktree string) []string {
	return []string{"primer", "--front", front, "--stream", "irc://127.0.0.1:6667/#parley", "--nick", "claude-a",
		"--command", "/home/someone/projects/target/bin/parley",
		"--state-dir", goldenStateDir,
		"--reply-file", "/var/folders/ab/cdefghijklmnopqrstuvwxyz0123456/T/parley-reply-claude-a-1234567890/reply.txt",
		"--chair", "colin-the-chair1", "--target", target, "--worktree", worktree}
}

const goldenStateDir = "/home/someone/projects/target/run/data/state"

// goldenWant is the golden render for a front with the primer test's target and
// worktree, each as long as the state directory, replaced by the real ones.
func goldenWant(t *testing.T, front, target, worktree string) string {
	t.Helper()
	want, err := os.ReadFile(filepath.Join("..", "..", "internal", "primer", "testdata", "build-"+front+".golden"))
	if err != nil {
		t.Fatalf("read the golden render: %v", err)
	}
	sampleTarget := "/" + strings.Repeat("t", len(goldenStateDir)-1)
	sampleWorktree := "/" + strings.Repeat("w", len(goldenStateDir)-1)
	out := strings.NewReplacer("target repo "+sampleTarget+";", "target repo "+target+";",
		"your worktree "+sampleWorktree+".", "your worktree "+worktree+".").Replace(string(want))
	if out == string(want) {
		t.Fatalf("the golden render carries neither fact:\n%s", want)
	}
	return out
}

// TestPrimerBuildFormIsByteForByteTheGolden: --skill build, named or left to
// its default, prints the golden render, with the facts it was launched with.
func TestPrimerBuildFormIsByteForByteTheGolden(t *testing.T) {
	f := launchFacts(t)
	for _, front := range []string{"claude", "codex"} {
		want := goldenWant(t, front, f.target, f.worktree)
		for _, extra := range [][]string{nil, {"--skill", "build"}} {
			res := mustRun(t, "", append(goldenSample(front, f.target, f.worktree), extra...)...)
			if res.stdout != want {
				t.Fatalf("primer --front %s %v printed\n%s\nwant the golden render\n%s", front, extra, res.stdout, want)
			}
		}
	}
}

// tableDirs are two real directories for the table skill's --target and
// --worktree, spelled as an absolute path the way a human would.
func tableDirs(t *testing.T) (target, worktree string) {
	t.Helper()
	root := resolvedTempDir(t)
	target, worktree = filepath.Join(root, "repo"), filepath.Join(root, "repo", ".claude", "worktrees", "seat a")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("make the directories: %v", err)
	}
	return target, worktree
}

func tablePrimerArgs(front, target, worktree string) []string {
	return []string{"primer", "--front", front, "--skill", "table", "--stream", "irc://127.0.0.1:6667/table", "--nick", "fable",
		"--chair", "colin", "--target", target, "--worktree", worktree,
		"--state-dir", "./state", "--command", "/opt/parley/bin/parley", "--reply-file", "/tmp/reply.txt"}
}

// TestPrimerTableFormCarriesTheFactsOnBothFronts: the facts are values the
// human launched the seat with, so nothing is left to fill in and nothing of a
// build is said.
func TestPrimerTableFormCarriesTheFactsOnBothFronts(t *testing.T) {
	target, worktree := tableDirs(t)
	for front, skill := range map[string]string{"claude": "invoked as /parley-table.", "codex": "invoked as $parley-table."} {
		res := mustRun(t, "", tablePrimerArgs(front, target, worktree)...)
		out := res.stdout
		for _, want := range []string{"chair colin, your human partner's registered nick", "target repo " + target + ";",
			"your worktree " + worktree + " (read-only)", skill, "other seats", "the one named above"} {
			if !strings.Contains(out, want) {
				t.Fatalf("the %s table primer does not carry %q:\n%s", front, want, out)
			}
		}
		for _, unwanted := range []string{"<human nick>", "<target repo root>", "<your worktree of it>", "<reply file path>",
			"the peer", "/parley-build", "$parley-build", "rule 8"} {
			if strings.Contains(out, unwanted) {
				t.Fatalf("the %s table primer still carries %q:\n%s", front, unwanted, out)
			}
		}
		if got, want := replyCommandLine(t, out), "'/opt/parley/bin/parley' send --stream 'irc://127.0.0.1:6667/table' --nick fable --state-dir './state' --body-file '/tmp/reply.txt'"; got != want {
			t.Fatalf("the reply command is\n%q\nwant\n%q", got, want)
		}
		if !strings.HasSuffix(out, "\n") || strings.HasSuffix(out, "\n\n") {
			t.Fatalf("the %s table primer does not end in exactly one newline", front)
		}
	}
}

// TestSkillConfigurationErrorsExitTwo: every way of asking for either skill
// wrongly is refused before anything is printed or opened. The chair, the target
// and the worktree are required on both skills; the cap is the table's alone.
func TestSkillConfigurationErrorsExitTwo(t *testing.T) {
	target, worktree := tableDirs(t)
	file := filepath.Join(target, "a file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Another spelling of the target: a link to it.
	link := filepath.Join(filepath.Dir(target), "link to repo")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	stateDir := t.TempDir()
	chat := "file:" + filepath.Join(t.TempDir(), "chat.ndjson")
	primerArgs := func(extra ...string) []string {
		return append([]string{"primer", "--front", "claude", "--stream", chat, "--nick", "fable", "--reply-file", "/tmp/reply.txt"}, extra...)
	}
	channelArgs := func(extra ...string) []string {
		return append([]string{"channel", "--stream", chat, "--nick", "fable", "--state-dir", stateDir}, extra...)
	}
	facts := []string{"--chair", "colin", "--target", target, "--worktree", worktree}
	// The same facts under each skill: the table's is named, the build's is the
	// default and then named too.
	good := append([]string{"--skill", "table"}, facts...)
	buildGood := append([]string{"--skill", "build"}, facts...)
	// without drops the flag named from an invocation, with its value.
	without := func(args []string, name string) []string {
		var out []string
		for i := 0; i < len(args); i++ {
			if args[i] == name {
				i++
				continue
			}
			out = append(out, args[i])
		}
		return out
	}
	withCap := func(args []string) []string { return append(args, "--cap", "3") }
	// plus is args and extra in a slice of their own: the invocations below are
	// built side by side, and an append onto a shared backing array would change
	// the one before it.
	plus := func(args []string, extra ...string) []string { return append(append([]string(nil), args...), extra...) }
	// Each fact, refused the same way under either skill, which is the point.
	type tc struct {
		name string
		args []string
		want string
	}
	var cases []tc
	for _, skill := range []struct {
		name  string
		args  []string
		cap   func([]string) []string
		short string
	}{
		{"table", good, withCap, "--skill table"},
		{"build", buildGood, func(a []string) []string { return a }, "--skill build"},
	} {
		n := skill.name
		cases = append(cases, []tc{
			{"primer " + n + ": no chair", primerArgs(without(skill.args, "--chair")...), "--chair is required with " + skill.short},
			{"primer " + n + ": empty chair", primerArgs(append(without(skill.args, "--chair"), "--chair", "")...), "--chair"},
			{"primer " + n + ": chair with a space", primerArgs(append(without(skill.args, "--chair"), "--chair", "two words")...), "--chair"},
			{"primer " + n + ": chair is the nick", primerArgs(append(without(skill.args, "--chair"), "--chair", "fable")...), "own --nick"},
			{"primer " + n + ": chair is the nick in another case", primerArgs(append(without(skill.args, "--chair"), "--chair", "Fable")...), "own --nick"},
			{"primer " + n + ": no target", primerArgs(without(skill.args, "--target")...), "--target is required with " + skill.short},
			{"primer " + n + ": no worktree", primerArgs(without(skill.args, "--worktree")...), "--worktree is required with " + skill.short},
			{"primer " + n + ": relative target", primerArgs(append(without(skill.args, "--target"), "--target", "repo")...), "--target"},
			{"primer " + n + ": relative worktree", primerArgs(append(without(skill.args, "--worktree"), "--worktree", "./wt")...), "--worktree"},
			{"primer " + n + ": missing target", primerArgs(append(without(skill.args, "--target"), "--target", filepath.Join(target, "nope"))...), "--target"},
			{"primer " + n + ": worktree is a file", primerArgs(append(without(skill.args, "--worktree"), "--worktree", file)...), "is not a directory"},
			{"primer " + n + ": worktree is the target", primerArgs(append(without(skill.args, "--worktree"), "--worktree", target)...), "the same directory"},
			{"primer " + n + ": worktree is the target spelled with a dot", primerArgs(append(without(skill.args, "--worktree"), "--worktree", target+"/.")...), "the same directory"},
			{"primer " + n + ": worktree is a link to the target", primerArgs(append(without(skill.args, "--worktree"), "--worktree", link)...), "the same directory"},
			{"primer " + n + ": target is a link to the worktree", primerArgs(append(without(without(skill.args, "--target"), "--worktree"), "--target", link, "--worktree", target)...), "the same directory"},
			{"primer " + n + ": control character in target", primerArgs(append(without(skill.args, "--target"), "--target", "/a\nb")...), "control character"},
			{"channel " + n + ": no chair", channelArgs(skill.cap(without(skill.args, "--chair"))...), "--chair is required with " + skill.short},
			{"channel " + n + ": no target", channelArgs(skill.cap(without(skill.args, "--target"))...), "--target is required with " + skill.short},
			{"channel " + n + ": no worktree", channelArgs(skill.cap(without(skill.args, "--worktree"))...), "--worktree is required with " + skill.short},
			{"channel " + n + ": relative target", channelArgs(skill.cap(append(without(skill.args, "--target"), "--target", "repo"))...), "--target"},
			{"channel " + n + ": missing worktree", channelArgs(skill.cap(append(without(skill.args, "--worktree"), "--worktree", filepath.Join(worktree, "nope")))...), "--worktree"},
			{"channel " + n + ": worktree is the target", channelArgs(skill.cap(append(without(skill.args, "--worktree"), "--worktree", target))...), "the same directory"},
			{"channel " + n + ": worktree is a link to the target", channelArgs(skill.cap(append(without(skill.args, "--worktree"), "--worktree", link))...), "the same directory"},
			{"channel " + n + ": chair is the nick", channelArgs(skill.cap(append(without(skill.args, "--chair"), "--chair", "fable"))...), "own --nick"},
			{"channel " + n + ": chair is the nick in another case", channelArgs(skill.cap(append(without(skill.args, "--chair"), "--chair", "FABLE"))...), "own --nick"},
		}...)
	}
	cases = append(cases, []tc{
		{"primer: unknown skill", primerArgs(append([]string{"--skill", "other"}, facts...)...), `unknown skill "other"`},
		{"primer: empty skill", primerArgs(append([]string{"--skill", ""}, facts...)...), "unknown skill"},
		{"primer: the old name of build", primerArgs(append([]string{"--skill", "parley"}, facts...)...), `unknown skill "parley": expected "build" or "table"`},
		{"primer: no reply file", plus([]string{"primer", "--front", "codex", "--stream", chat, "--nick", "fable"}, good...), "--reply-file is required with --skill table"},
		{"primer: no facts at all", primerArgs(), "--chair is required with --skill build"},
		{"primer: a cap", primerArgs(plus(good, "--cap", "3")...), "flag provided but not defined"},
		{"channel: unknown skill", channelArgs(append([]string{"--skill", "other"}, facts...)...), `unknown skill "other"`},
		{"channel: the old name of build", channelArgs(append([]string{"--skill", "parley"}, facts...)...), `unknown skill "parley": expected "build" or "table"`},
		{"channel: no facts at all", channelArgs(), "--chair is required with --skill build"},
		{"channel: table with no cap", channelArgs(good...), "--cap is required with --skill table"},
		{"channel: table with a negative cap", channelArgs(plus(good, "--cap", "-1")...), "--cap must not be negative"},
		{"channel: build with a cap", channelArgs(plus(facts, "--cap", "3")...), "--cap applies to --skill table only"},
		{"channel: build with a cap of zero", channelArgs(plus(buildGood, "--cap", "0")...), "--cap applies to --skill table only"},
	}...)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := runParley(t, "", tc.args...)
			if res.code != 2 {
				t.Fatalf("parley %v exited %d, want 2\nstdout: %s\nstderr: %s", tc.args, res.code, res.stdout, res.stderr)
			}
			if !strings.Contains(res.stderr, tc.want) {
				t.Fatalf("the error does not say %q: %s", tc.want, res.stderr)
			}
			if res.stdout != "" {
				t.Fatalf("a refused invocation still printed:\n%s", res.stdout)
			}
		})
	}
	// None of the channel refusals got as far as the nick's state.
	if _, err := os.Stat(filepath.Join(stateDir, "fable", "lock")); err == nil {
		t.Fatal("a refused invocation took the nick's lock")
	}
}

// channelInitialize starts `parley channel`, performs the initialize exchange
// and returns the instructions it answered with, then ends the session. env
// entries are added to the child's environment.
func channelInitialize(t *testing.T, env []string, args ...string) (instructions, stderr string) {
	t.Helper()
	cmd := exec.Command(binary(t), args...)
	cmd.Env = append(os.Environ(), env...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	errs := &syncBuffer{}
	cmd.Stderr = errs
	if err := cmd.Start(); err != nil {
		t.Fatalf("start channel: %v", err)
	}
	t.Cleanup(func() {
		stdin.Close()
		_ = cmd.Wait()
	})
	lines := make(chan string, 4)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	if _, err := io.WriteString(stdin, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`+"\n"); err != nil {
		t.Fatalf("write initialize: %v", err)
	}
	select {
	case line, ok := <-lines:
		if !ok {
			t.Fatalf("the server closed stdout; stderr: %s", errs.String())
		}
		var resp struct {
			Result struct {
				Instructions string `json:"instructions"`
			} `json:"result"`
		}
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			t.Fatalf("stdout line is not JSON: %v\n%s", err, line)
		}
		instructions = resp.Result.Instructions
	case <-time.After(childDeadline):
		t.Fatalf("nothing on stdout; stderr: %s", errs.String())
	}
	stdin.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("channel exited %v, want 0\nstderr: %s", err, errs.String())
	}
	return instructions, errs.String()
}

// shortRoot is a directory with a short path, for a test about the length of
// what the paths in it add to the instructions. The system temp directory of a
// macOS machine is some 50 characters before the name and the instructions hold
// seven paths, so it is /tmp where there is one, and the system temp directory
// where there is not.
func shortRoot(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "p")
	if err != nil {
		root, err = os.MkdirTemp("", "p")
	}
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	for _, d := range []string{"s", "t", "w"} {
		if err := os.Mkdir(filepath.Join(root, d), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	return root
}

// TestChannelSkillTableHandsOverTheTableForm: the instructions the real binary
// answers initialize with are the table form, with the values it was launched
// with, and they fit the client's cap.
func TestChannelSkillTableHandsOverTheTableForm(t *testing.T) {
	root := shortRoot(t)
	target, worktree := filepath.Join(root, "t"), filepath.Join(root, "w")
	instructions, stderr := channelInitialize(t, nil, "channel", "--stream", "file:"+filepath.Join(root, "c"), "--nick", "fable",
		"--state-dir", filepath.Join(root, "s"), "--chair", "colin", "--cap", "6",
		"--skill", "table", "--target", target, "--worktree", worktree)
	for _, want := range []string{"chair colin, your human partner's registered nick", "target repo " + target + ";",
		"your worktree " + worktree + " (read-only)", "invoked as /parley-table.", "other seats", "the one named above"} {
		if !strings.Contains(instructions, want) {
			t.Fatalf("the instructions do not carry %q:\n%s", want, instructions)
		}
	}
	for _, unwanted := range []string{"<human nick>", "the peer", "rule 8", "invoked as /parley-build"} {
		if strings.Contains(instructions, unwanted) {
			t.Fatalf("the instructions still carry %q:\n%s", unwanted, instructions)
		}
	}
	n := primer.UTF16Len(instructions)
	if n > primer.ClaudeInstructionsCap {
		t.Fatalf("the real instructions are %d UTF-16 code units, over the cap of %d:\n%s", n, primer.ClaudeInstructionsCap, instructions)
	}
	t.Logf("the real table instructions are %d UTF-16 code units of the %d cap", n, primer.ClaudeInstructionsCap)
	if stderr != "" {
		t.Fatalf("the server logged on a clean session:\n%s", stderr)
	}
}

// TestChannelSkillBuildHandsOverTheBuildForm: the default skill's instructions
// are the build form, with the three facts the channel was launched with as
// values, and they fit the client's cap, and the session that carries them logs
// nothing. (server/discover is not asked here: internal/channel's
// TestServerDiscoverIsAnsweredWithoutALogLine is the test of that.)
func TestChannelSkillBuildHandsOverTheBuildForm(t *testing.T) {
	root := shortRoot(t)
	target, worktree := filepath.Join(root, "t"), filepath.Join(root, "w")
	instructions, stderr := channelInitialize(t, nil, "channel", "--stream", "file:"+filepath.Join(root, "c"), "--nick", "claude-a",
		"--state-dir", filepath.Join(root, "s"), "--chair", "colin", "--target", target, "--worktree", worktree)
	for _, want := range []string{"your human partner's registered nick colin;", "target repo " + target + ";",
		"your worktree " + worktree + ". The protocol is the parley-build skill, invoked as /parley-build.", "the one named above", "rule 8"} {
		if !strings.Contains(instructions, want) {
			t.Fatalf("the instructions do not carry %q:\n%s", want, instructions)
		}
	}
	for _, unwanted := range []string{"<human nick>", "at your own terminal", "other seats", "(read-only)"} {
		if strings.Contains(instructions, unwanted) {
			t.Fatalf("the instructions still carry %q:\n%s", unwanted, instructions)
		}
	}
	if n := primer.UTF16Len(instructions); n > primer.ClaudeInstructionsCap {
		t.Fatalf("the real instructions are %d UTF-16 code units, over the cap of %d:\n%s", n, primer.ClaudeInstructionsCap, instructions)
	} else {
		t.Logf("the real build instructions are %d UTF-16 code units of the %d cap", n, primer.ClaudeInstructionsCap)
	}
	if stderr != "" {
		t.Fatalf("the server logged on a clean session:\n%s", stderr)
	}
}

// TestChannelRefusesInstructionsOverTheCap: a long enough --target puts either
// skill's text over the client's cap, which would cut the trust rule off the
// end. The session is refused at launch with one line, and the reply directory it
// made to find that out is gone.
func TestChannelRefusesInstructionsOverTheCap(t *testing.T) {
	for _, skill := range []string{"build", "table"} {
		t.Run(skill, func(t *testing.T) {
			root := shortRoot(t)
			long := filepath.Join(root, "t", strings.Repeat("d", 250), strings.Repeat("e", 250))
			if err := os.MkdirAll(long, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			tmp := t.TempDir()
			stateDir := filepath.Join(root, "s")
			spec := "file:" + filepath.Join(root, "c")
			args := func(target string) []string {
				a := []string{"channel", "--stream", spec, "--nick", "fable", "--state-dir", stateDir,
					"--chair", "colin", "--skill", skill, "--target", target, "--worktree", filepath.Join(root, "w")}
				if skill == "table" {
					a = append(a, "--cap", "6")
				}
				return a
			}
			cmd := exec.Command(binary(t), args(long)...)
			cmd.Env = append(os.Environ(), "TMPDIR="+tmp)
			var stdout, stderr strings.Builder
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			exit, ok := err.(*exec.ExitError)
			if !ok || exit.ExitCode() != 2 {
				t.Fatalf("channel exited %v, want 2\nstderr: %s", err, stderr.String())
			}
			if got := stderr.String(); strings.Count(got, "\n") != 1 || !strings.Contains(got, "over Claude Code's cap of 2048") || !strings.Contains(got, "--target") {
				t.Fatalf("stderr is not one clear line naming the cap and the flags to shorten: %q", got)
			}
			if stdout.String() != "" {
				t.Fatalf("a refused channel wrote %q on the JSON-RPC stream", stdout.String())
			}
			if left, _ := filepath.Glob(filepath.Join(tmp, "parley-reply-*")); len(left) != 0 {
				t.Fatalf("the refusal left reply directories behind: %v", left)
			}
			// The lock was released with the refusal: the same nick starts again.
			if instructions, _ := channelInitialize(t, nil, args(filepath.Join(root, "t"))...); instructions == "" {
				t.Fatal("the nick could not start again after the refusal")
			}
		})
	}
}

// TestPrimerClaudeFormRefusesWhatTheClientWouldTruncate: the claude form is
// what the channel front sends, so the primer refuses the text that is over the
// cap, on either skill, as the channel does. It is not the pre-check for a
// launch — it measures a shorter text than the channel sends, and channel
// --check is the pre-check — but what it refuses it refuses. The codex form is
// the session's first message and has no cap.
func TestPrimerClaudeFormRefusesWhatTheClientWouldTruncate(t *testing.T) {
	root := shortRoot(t)
	long := filepath.Join(root, "t", strings.Repeat("d", 200), strings.Repeat("e", 200))
	if err := os.MkdirAll(long, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, skill := range []string{"build", "table"} {
		args := func(front string) []string {
			a := tablePrimerArgs(front, long, filepath.Join(root, "w"))
			for i := range a {
				if a[i] == "table" {
					a[i] = skill
				}
			}
			return a
		}
		res := runParley(t, "", args("claude")...)
		if res.code != 2 || res.stdout != "" {
			t.Fatalf("an over-cap %s primer exited %d and printed %q, want 2 and nothing\nstderr: %s", skill, res.code, res.stdout, res.stderr)
		}
		if got := res.stderr; strings.Count(got, "\n") != 1 || !strings.Contains(got, "over Claude Code's cap of 2048") || !strings.Contains(got, "--target") {
			t.Fatalf("stderr is not one clear line naming the cap and the flags to shorten: %q", got)
		}
		if res := mustRun(t, "", args("codex")...); !strings.Contains(res.stdout, long) {
			t.Fatalf("the %s codex form, which has no cap, did not print:\n%s", skill, res.stdout)
		}
	}
	// A build form that is over the cap on its state directory is refused too.
	f := launchFacts(t)
	build := goldenSample("claude", f.target, f.worktree)
	for i := range build {
		if build[i] == goldenStateDir {
			build[i] = filepath.Join(root, "s", strings.Repeat("d", 150), strings.Repeat("e", 150))
		}
	}
	if res := runParley(t, "", build...); res.code != 2 || res.stdout != "" || !strings.Contains(res.stderr, "--state-dir") {
		t.Fatalf("an over-cap build primer exited %d and printed %q, want 2 and nothing\nstderr: %s", res.code, res.stdout, res.stderr)
	}
}

// TestUsageDocumentsTheReceiptAndTheSkillFlags: the help is where a human finds
// that send prints a line and that channel and primer take a skill and the three
// facts of a launch, and codex only the skill and the gate.
func TestUsageDocumentsTheReceiptAndTheSkillFlags(t *testing.T) {
	top := mustRun(t, "", "-h").stdout
	for _, want := range []string{"[parley] sent id=<id> ts=<ts>", `"-"`, "skills:", "--skill build|table",
		"--target <dir>", "--worktree <dir>", "--chair <nick>", "parley-table skill", "primer\n  also requires --reply-file",
		"2048-unit cap", "[--check]"} {
		if !strings.Contains(top, want) {
			t.Fatalf("usage does not mention %q:\n%s", want, top)
		}
	}
	// The text is wrapped, so a phrase that runs over a line break is looked for
	// with its whitespace folded.
	flat := strings.Join(strings.Fields(top), " ")
	for _, want := range []string{"All three are required on both skills", "--cap is for --skill table only",
		"channel and primer --front claude refuse the text, on either skill, when it is over Claude Code's 2048-unit cap",
		"instructions: <n> of 2048 UTF-16 units", "channel <the .mcp.json args> --check", "match without regard to ASCII case",
		"a human pastes it in, or the session is started with it as its first prompt",
		"not the same one (links followed; a worktree inside the target is fine)"} {
		if !strings.Contains(flat, want) {
			t.Fatalf("usage does not mention %q:\n%s", want, top)
		}
	}
	for _, unwanted := range []string{"three blanks", "are an error with --skill build"} {
		if strings.Contains(flat, unwanted) {
			t.Fatalf("usage still says %q:\n%s", unwanted, top)
		}
	}
	for _, verb := range []string{"channel", "primer"} {
		help := runParley(t, "", verb, "-h").stdout
		for _, want := range []string{"-skill", "-chair", "-target", "-worktree", "[--skill build|table"} {
			if !strings.Contains(help, want) {
				t.Fatalf("parley %s -h does not mention %q:\n%s", verb, want, help)
			}
		}
	}
	if help := runParley(t, "", "channel", "-h").stdout; !strings.Contains(help, "-check") {
		t.Fatalf("parley channel -h does not mention -check:\n%s", help)
	}
	for _, verb := range []string{"send", "wait"} {
		if help := runParley(t, "", verb, "-h").stdout; strings.Contains(help, "-skill") || strings.Contains(help, "-target") || strings.Contains(help, "-worktree") {
			t.Fatalf("parley %s -h mentions the skill flags, which it does not take:\n%s", verb, help)
		}
	}
	// The pump takes the skill and the gate, and none of a launch's directories.
	if help := runParley(t, "", "codex", "-h").stdout; !strings.Contains(help, "-skill") || strings.Contains(help, "-target") || strings.Contains(help, "-worktree") {
		t.Fatalf("parley codex -h mentions the wrong flags:\n%s", help)
	}
}
