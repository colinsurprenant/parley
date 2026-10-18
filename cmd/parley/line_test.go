package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/colinsurprenant/parley/internal/primer"
)

// The line skill's command-line tests: the flags each verb takes under it, the
// text it hands over, and the gate it installs.

// lineChannelArgs are the arguments of a line session's channel launch: a chair
// and a cap, and no target or worktree.
func lineChannelArgs(root string) []string {
	return []string{"channel", "--stream", "file:" + filepath.Join(root, "c"), "--nick", "plan",
		"--state-dir", filepath.Join(root, "s"), "--skill", "line", "--chair", "colin", "--cap", "8"}
}

// TestPrimerLineFormIsByteForByteTheGolden: --skill line prints the golden
// render of the primer package's lineSample(), on both fronts. The line form has
// no target or worktree, so nothing here is substituted.
func TestPrimerLineFormIsByteForByteTheGolden(t *testing.T) {
	for _, front := range []string{"claude", "codex"} {
		want, err := os.ReadFile(filepath.Join("..", "..", "internal", "primer", "testdata", "line-"+front+".golden"))
		if err != nil {
			t.Fatalf("read the golden render: %v", err)
		}
		res := mustRun(t, "", "primer", "--front", front, "--skill", "line", "--stream", "irc://127.0.0.1:6667/#parley", "--nick", "claude-a",
			"--command", "/home/someone/projects/target/bin/parley",
			"--state-dir", goldenStateDir,
			"--reply-file", "/var/folders/ab/cdefghijklmnopqrstuvwxyz0123456/T/parley-reply-claude-a-1234567890/reply.txt",
			"--chair", "colin-the-chair1", "--cap", "12")
		if res.stdout != string(want) {
			t.Fatalf("primer --front %s --skill line printed\n%s\nwant the golden render\n%s", front, res.stdout, want)
		}
	}
}

func linePrimerArgs(front string) []string {
	return []string{"primer", "--front", front, "--skill", "line", "--stream", "irc://127.0.0.1:6667/line", "--nick", "plan",
		"--chair", "colin", "--cap", "8",
		"--state-dir", "./state", "--command", "/opt/parley/bin/parley", "--reply-file", "/tmp/reply.txt"}
}

// TestPrimerLineFormCarriesTheFactsOnBothFronts: the nick and the cap are values
// the human launched the session with, so nothing is left to fill in, nothing of
// a build or a table is said, and the skill is invoked the way the front does.
func TestPrimerLineFormCarriesTheFactsOnBothFronts(t *testing.T) {
	for front, skill := range map[string]string{"claude": "invoked as /parley-line.", "codex": "invoked as $parley-line."} {
		out := mustRun(t, "", linePrimerArgs(front)...).stdout
		for _, want := range []string{"your human partner's registered nick colin; breaker cap 8 posts", skill, "other sessions", "the one named above"} {
			if !strings.Contains(out, want) {
				t.Fatalf("the %s line primer does not carry %q:\n%s", front, want, out)
			}
		}
		for _, unwanted := range []string{"<human nick>", "<reply file path>", "target repo", "worktree", "other seats", "the peer",
			"/parley-build", "$parley-build", "/parley-table", "$parley-table", "rule 8", "TABLE"} {
			if strings.Contains(out, unwanted) {
				t.Fatalf("the %s line primer still carries %q:\n%s", front, unwanted, out)
			}
		}
		if got, want := replyCommandLine(t, out), "'/opt/parley/bin/parley' send --stream 'irc://127.0.0.1:6667/line' --nick plan --state-dir './state' --body-file '/tmp/reply.txt'"; got != want {
			t.Fatalf("the reply command is\n%q\nwant\n%q", got, want)
		}
		if !strings.HasSuffix(out, "\n") || strings.HasSuffix(out, "\n\n") {
			t.Fatalf("the %s line primer does not end in exactly one newline", front)
		}
		// Codex keeps a checkout's git directory read-only in its sandbox, so only
		// the codex front is told to ask for escalation for git's writes.
		if has := strings.Contains(out, "ask for escalation for each of them directly"); has != (front == "codex") {
			t.Fatalf("the %s line primer carries the git escalation paragraph = %v:\n%s", front, has, out)
		}
	}
	// The cap is printed as spelled, 0 included.
	args := append([]string(nil), linePrimerArgs("claude")...)
	for i := range args {
		if args[i] == "8" {
			args[i] = "0"
		}
	}
	if out := mustRun(t, "", args...).stdout; !strings.Contains(out, "breaker cap 0 posts") || !strings.Contains(out, "(0 is no breaker)") {
		t.Fatalf("the primer for --cap 0 does not say so:\n%s", out)
	}
}

// TestChannelSkillLineHandsOverTheLineForm: the instructions the real binary
// answers initialize with are the line form, with the values it was launched
// with and no directory among them, and they fit the client's cap.
func TestChannelSkillLineHandsOverTheLineForm(t *testing.T) {
	root := shortRoot(t)
	instructions, stderr := channelInitialize(t, nil, lineChannelArgs(root)...)
	for _, want := range []string{"your human partner's registered nick colin; breaker cap 8 posts", "invoked as /parley-line.",
		"other sessions", "the one named above", "steers the line"} {
		if !strings.Contains(instructions, want) {
			t.Fatalf("the instructions do not carry %q:\n%s", want, instructions)
		}
	}
	for _, unwanted := range []string{"<human nick>", "the peer", "other seats", "target repo", "worktree", "rule 8", "parley-build", "parley-table"} {
		if strings.Contains(instructions, unwanted) {
			t.Fatalf("the instructions still carry %q:\n%s", unwanted, instructions)
		}
	}
	n := primer.UTF16Len(instructions)
	if n > primer.ClaudeInstructionsCap {
		t.Fatalf("the real instructions are %d UTF-16 code units, over the cap of %d:\n%s", n, primer.ClaudeInstructionsCap, instructions)
	}
	t.Logf("the real line instructions are %d UTF-16 code units of the %d cap", n, primer.ClaudeInstructionsCap)
	if stderr != "" {
		t.Fatalf("the server logged on a clean session:\n%s", stderr)
	}
}

// TestLineConfigurationErrorsExitTwo: every way of asking for the line skill
// wrongly is refused with exit 2 before anything is printed or opened, on each
// verb that takes the skill, and channel --check refuses what channel refuses.
// --target and --worktree are refused by name, wherever they are given.
func TestLineConfigurationErrorsExitTwo(t *testing.T) {
	root := shortRoot(t)
	stateDir := filepath.Join(root, "s")
	chat := "file:" + filepath.Join(root, "c")
	target, worktree := filepath.Join(root, "t"), filepath.Join(root, "w")

	channel := []string{"channel", "--stream", chat, "--nick", "plan", "--state-dir", stateDir, "--skill", "line"}
	// /bin/echo stands in for an installed codex, so nothing here depends on one
	// being on this machine.
	codex := []string{"codex", "--stream", chat, "--nick", "plan", "--state-dir", stateDir, "--skill", "line", "--thread", "t", "--codex-bin", "/bin/echo"}
	primerCmd := []string{"primer", "--front", "claude", "--stream", chat, "--nick", "plan", "--skill", "line", "--reply-file", "/tmp/reply.txt"}
	primerCodex := []string{"primer", "--front", "codex", "--stream", chat, "--nick", "plan", "--skill", "line"}
	plus := func(args []string, extra ...string) []string { return append(append([]string(nil), args...), extra...) }
	type tc struct {
		name string
		args []string
		want string
	}
	var cases []tc
	for verb, base := range map[string][]string{"channel": channel, "codex": codex, "primer": primerCmd} {
		cases = append(cases, []tc{
			{verb + ": no chair and no cap", base, "--chair is required with --skill line"},
			{verb + ": cap without chair", plus(base, "--cap", "3"), "--chair is required with --skill line"},
			{verb + ": chair without cap", plus(base, "--chair", "colin"), "--cap is required with --skill line"},
			{verb + ": chair is the nick", plus(base, "--chair", "plan", "--cap", "3"), "own --nick"},
			{verb + ": chair is the nick in another case", plus(base, "--chair", "PLAN", "--cap", "3"), "own --nick"},
			{verb + ": chair with a space", plus(base, "--chair", "two words", "--cap", "3"), "--chair"},
			{verb + ": empty chair", plus(base, "--chair", "", "--cap", "3"), "--chair"},
			{verb + ": negative cap", plus(base, "--chair", "colin", "--cap", "-1"), "--cap must not be negative"},
			{verb + ": cap is not a number", plus(base, "--chair", "colin", "--cap", "many"), "cap"},
		}...)
	}
	good := func(base []string) []string { return plus(base, "--chair", "colin", "--cap", "3") }
	for verb, base := range map[string][]string{"channel": channel, "primer": primerCmd} {
		cases = append(cases, []tc{
			{verb + ": a target", plus(good(base), "--target", target), "--target is not taken with --skill line"},
			{verb + ": a worktree", plus(good(base), "--worktree", worktree), "--worktree is not taken with --skill line"},
			{verb + ": a target and a worktree", plus(good(base), "--target", target, "--worktree", worktree), "--target is not taken with --skill line"},
			{verb + ": an empty target", plus(good(base), "--target", ""), "--target is not taken with --skill line"},
			{verb + ": a target that is not there", plus(good(base), "--target", filepath.Join(root, "nope")), "--target is not taken with --skill line"},
		}...)
	}
	// The pump never took a directory: the flag does not exist there at all.
	cases = append(cases, []tc{
		{"codex: a target", plus(good(codex), "--target", target), "flag provided but not defined: -target"},
		{"codex: a worktree", plus(good(codex), "--worktree", worktree), "flag provided but not defined: -worktree"},
		{"primer: no reply file on the codex front", plus(primerCodex, "--chair", "colin", "--cap", "3"), "--reply-file is required with --skill line"},
		{"channel: unknown skill spelled like it", []string{"channel", "--stream", chat, "--nick", "plan", "--state-dir", stateDir, "--skill", "Line", "--chair", "colin", "--cap", "3"}, `unknown skill "Line"`},
	}...)
	// A build and a table are told what a cap and a chair are for.
	cases = append(cases, []tc{
		{"primer: a cap with the build skill", []string{"primer", "--front", "claude", "--stream", chat, "--nick", "plan", "--reply-file", "/tmp/r", "--chair", "colin", "--target", target, "--worktree", worktree, "--cap", "3"}, "--cap is taken by primer with --skill line only"},
		{"codex: a chair with the build skill", []string{"codex", "--stream", chat, "--nick", "plan", "--state-dir", stateDir, "--thread", "t", "--codex-bin", "/bin/echo", "--chair", "colin"}, "--chair applies to --skill table or line only"},
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
	// channel --check refuses what channel refuses, with the same exit.
	for _, c := range cases {
		if c.args[0] != "channel" {
			continue
		}
		res := runCheck(t, t.TempDir(), c.args...)
		if res.code != 2 || res.stdout != "" || !strings.Contains(res.stderr, c.want) {
			t.Fatalf("%s with --check: exit %d, stdout %q, stderr %q; want exit 2, nothing on stdout, and %q", c.name, res.code, res.stdout, res.stderr, c.want)
		}
	}
	// None of the refusals got as far as the nick's state: the refusal comes
	// before the lock is taken.
	if _, err := os.Stat(filepath.Join(stateDir, "plan")); err == nil {
		t.Fatal("a refused invocation made the nick's state")
	}
}

// TestLineCapZeroIsAccepted: 0 is the spelling of no breaker, and the one value
// that is not an omission.
func TestLineCapZeroIsAccepted(t *testing.T) {
	root := shortRoot(t)
	args := lineChannelArgs(root)
	for i := range args {
		if args[i] == "8" {
			args[i] = "0"
		}
	}
	res := runCheck(t, t.TempDir(), args...)
	if res.code != 0 {
		t.Fatalf("--check with --cap 0 exited %d\nstderr: %s", res.code, res.stderr)
	}
	checkedLength(t, res)
}

// TestChannelCheckMeasuresTheLineForm: --check works for a line like the others,
// and a line, which carries no target or worktree, is the shortest of the three.
// The sizes are logged with a state directory of about 100 characters.
func TestChannelCheckMeasuresTheLineForm(t *testing.T) {
	root := shortRoot(t)
	long := filepath.Join(root, "s", strings.Repeat("d", 100-len(filepath.Join(root, "s"))-1))
	if err := os.MkdirAll(long, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	swapState := func(args []string) []string {
		args = append([]string(nil), args...)
		for i := range args {
			if args[i] == filepath.Join(root, "s") {
				args[i] = long
			}
		}
		return args
	}
	sizes := map[string]int{}
	tmp := t.TempDir()
	for skill, args := range map[string][]string{
		"build": buildChannelArgs(root, filepath.Join(root, "t")),
		"table": tableChannelArgs(root, filepath.Join(root, "t")),
		"line":  lineChannelArgs(root),
	} {
		res := runCheck(t, tmp, swapState(args)...)
		if res.code != 0 || res.stderr != "" {
			t.Fatalf("--check of a %s launch exited %d with stderr %q, want 0 and nothing", skill, res.code, res.stderr)
		}
		sizes[skill] = checkedLength(t, res)
	}
	t.Logf("channel --check with a state directory of %d characters: build %d, table %d, line %d of %d",
		len(long), sizes["build"], sizes["table"], sizes["line"], primer.ClaudeInstructionsCap)
	if sizes["line"] >= sizes["table"] || sizes["line"] >= sizes["build"] {
		t.Fatalf("the line form measures %d, not under the table's %d and the build's %d", sizes["line"], sizes["table"], sizes["build"])
	}
	// The binary here is a test build in a temp directory, with a longer path than
	// an installed one, so the margin asked for is the modest one.
	if margin := primer.ClaudeInstructionsCap - sizes["line"]; margin < 150 {
		t.Fatalf("the line form leaves a margin of %d units under the cap, want a clear one", margin)
	}
	if left, _ := filepath.Glob(filepath.Join(tmp, "parley-reply-*")); len(left) != 0 {
		t.Fatalf("--check left reply directories behind: %v", left)
	}
}

// TestChannelRefusesALineOverTheCapAndNamesWhatToShorten: a line has no target
// and no worktree, so a long state directory is what puts it over, and the
// refusal, which is one line, does not send the human looking for flags the
// launch does not have.
func TestChannelRefusesALineOverTheCapAndNamesWhatToShorten(t *testing.T) {
	root := shortRoot(t)
	args := lineChannelArgs(root)
	for i := range args {
		if args[i] == filepath.Join(root, "s") {
			args[i] = filepath.Join(root, "s", strings.Repeat("d", 150), strings.Repeat("e", 150))
		}
	}
	tmp := t.TempDir()
	check := runCheck(t, tmp, args...)
	if check.code != 2 {
		t.Fatalf("an over-cap line form exited %d under --check, want 2\nstderr: %s", check.code, check.stderr)
	}
	if n := checkedLength(t, check); n <= primer.ClaudeInstructionsCap {
		t.Fatalf("measured %d, within the cap, with exit 2", n)
	}
	cmd := exec.Command(binary(t), args...)
	cmd.Env = append(os.Environ(), "TMPDIR="+tmp)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 2 {
		t.Fatalf("the launch exited %v, want 2\nstderr: %s", err, stderr.String())
	}
	for what, got := range map[string]string{"--check": check.stderr, "the launch": stderr.String()} {
		if strings.Count(got, "\n") != 1 || !strings.Contains(got, "over Claude Code's cap of 2048") || !strings.Contains(got, "--state-dir") {
			t.Fatalf("%s: stderr is not one clear line naming the cap and --state-dir: %q", what, got)
		}
		if strings.Contains(got, "--target") || strings.Contains(got, "--worktree") {
			t.Fatalf("%s: stderr names a flag a line does not take: %q", what, got)
		}
	}
	if stdout.String() != "" {
		t.Fatalf("a refused channel wrote %q on the JSON-RPC stream", stdout.String())
	}
	if left, _ := filepath.Glob(filepath.Join(tmp, "parley-reply-*")); len(left) != 0 {
		t.Fatalf("the refusal left reply directories behind: %v", left)
	}
}

// TestUsageDocumentsTheLineSkill: the help is where a human finds that a line
// takes a chair and a cap and no directory, and what its gate does.
func TestUsageDocumentsTheLineSkill(t *testing.T) {
	top := mustRun(t, "", "-h").stdout
	flat := strings.Join(strings.Fields(top), " ")
	for _, want := range []string{"parley channel --stream <spec> --nick <nick> --skill line --chair <nick> --cap <n>",
		"parley primer --front claude|codex --stream <spec> --nick <nick> --skill line --chair <nick> --cap <n>",
		"[--skill build|table|line --chair <nick> --cap <n>]", "lines:", "parley-line skill",
		"there is no first hold", "--target and --worktree are refused", "--cap is for --skill table and --skill line only"} {
		if !strings.Contains(flat, want) {
			t.Fatalf("usage does not mention %q:\n%s", want, top)
		}
	}
	for _, verb := range []string{"channel", "primer", "codex"} {
		help := runParley(t, "", verb, "-h").stdout
		for _, want := range []string{"--skill line", "-cap"} {
			if !strings.Contains(help, want) {
				t.Fatalf("parley %s -h does not mention %q:\n%s", verb, want, help)
			}
		}
	}
	// The per-verb help of the pump names the skills it takes.
	if help := runParley(t, "", "codex", "-h").stdout; !strings.Contains(help, "build|table|line") {
		t.Fatalf("parley codex -h does not list the three skills:\n%s", help)
	}
}

// TestCodexLineDeliversPeerPostsAtOnceAndTheBreakerHolds drives the real binary
// with the line flags: a peer's post is delivered before the pump has posted
// (a line has no seal), the post that takes the count past the cap is held, and
// the chair's post releases it ahead of itself. It is what proves the flags
// reach the Deliverer, which the unit tests of the Deliverer cannot.
func TestCodexLineDeliversPeerPostsAtOnceAndTheBreakerHolds(t *testing.T) {
	dir := resolvedTempDir(t)
	spec := "file:" + filepath.Join(dir, "chat.ndjson")
	stateDir := t.TempDir()
	binDir, record := fakeCodex(t)
	const thread = "0199c0de-c0de-7c0d-8c0d-ec0dec0dec0d"

	cmd := exec.Command(binary(t), "codex", "--stream", spec, "--nick", "codex-a",
		"--state-dir", stateDir, "--thread", thread, "--debounce", "100ms",
		"--skill", "line", "--chair", "colin", "--cap", "1")
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
	waitForStderr(t, stderr, "into Codex thread "+thread)

	// No seal: a peer's post arrives before the pump has posted anything.
	mustRun(t, "", "send", "--stream", spec, "--nick", "claude-b", "--", "phase one done")
	argv := readArgv(t, record, stderr)
	if msg := argv[len(argv)-1]; !strings.Contains(msg, "from=claude-b") || !strings.Contains(msg, "phase one done") {
		t.Fatalf("the line pump delivered\n%s\nwant the peer's post at once", msg)
	}

	// The second post since the chair's last takes the count past the cap of 1.
	if err := os.Remove(record); err != nil {
		t.Fatalf("clear the record: %v", err)
	}
	mustRun(t, "", "send", "--stream", spec, "--nick", "claude-b", "--", "phase two done")
	time.Sleep(600 * time.Millisecond)
	if _, err := os.Stat(record); err == nil {
		t.Fatal("the tripped breaker delivered a post")
	}

	mustRun(t, "", "send", "--stream", spec, "--nick", "colin", "--", "chair talk")
	argv = readArgv(t, record, stderr)
	msg := argv[len(argv)-1]
	held, chair := strings.Index(msg, "phase two done"), strings.Index(msg, "chair talk")
	if held < 0 || chair < 0 || held > chair {
		t.Fatalf("the chair's post delivered\n%s\nwant the held post ahead of the chair's", msg)
	}
}

// TestCodexThreadAutoUnderALineSaysTheLineHasNoRecord: parley table new and
// parley build new make the thread record --thread auto reads, and a line is set
// up by hand, so the error under --skill line does not send its user to either.
// It says there is no record and that the pump takes the thread id, and a
// missing --thread says the same, before anything is opened.
func TestCodexThreadAutoUnderALineSaysTheLineHasNoRecord(t *testing.T) {
	spec := "file:" + filepath.Join(t.TempDir(), "chat.ndjson")
	stateDir := t.TempDir()
	base := []string{"codex", "--stream", spec, "--nick", "codex-a", "--state-dir", stateDir,
		"--codex-bin", "/bin/echo", "--skill", "line", "--chair", "colin", "--cap", "3"}
	for name, thread := range map[string][]string{"auto": {"--thread", "auto"}, "no thread": nil} {
		res := runParley(t, "", append(append([]string(nil), base...), thread...)...)
		if res.code != 2 {
			t.Fatalf("%s: codex exited %d, want 2\nstderr: %s", name, res.code, res.stderr)
		}
		for _, want := range []string{"a line has no thread record", "--thread"} {
			if !strings.Contains(res.stderr, want) {
				t.Fatalf("%s: the error does not say %q: %s", name, want, res.stderr)
			}
		}
		if name == "auto" && !strings.Contains(res.stderr, "--thread <id>") {
			t.Fatalf("auto: the error does not offer --thread <id>: %s", res.stderr)
		}
		for _, unwanted := range []string{"parley table new", "parley build new"} {
			if strings.Contains(res.stderr, unwanted) {
				t.Fatalf("%s: the error sends a line to %q: %s", name, unwanted, res.stderr)
			}
		}
		if res.stdout != "" {
			t.Fatalf("%s: a refused invocation printed %q", name, res.stdout)
		}
	}
	if _, err := os.Stat(filepath.Join(stateDir, "codex-a")); err == nil {
		t.Fatal("the refused invocations made the nick's state directory")
	}
	// The table's and the build's keep their advice.
	table := []string{"codex", "--stream", spec, "--nick", "codex-a", "--state-dir", stateDir,
		"--codex-bin", "/bin/echo", "--skill", "table", "--chair", "colin", "--cap", "3", "--thread", "auto"}
	if res := runParley(t, "", table...); res.code != 2 || !strings.Contains(res.stderr, "parley table new") {
		t.Fatalf("the table's error exited %d and says %q", res.code, res.stderr)
	}
}
