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
// render of the primer package's lineSample(), on every front. The line form has
// no target or worktree, so nothing here is substituted.
func TestPrimerLineFormIsByteForByteTheGolden(t *testing.T) {
	for _, front := range []string{"claude", "codex", "opencode"} {
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
	for front, skill := range map[string]string{"claude": "invoked as /parley-line.", "codex": "invoked as $parley-line.",
		"opencode": "the parley-line skill, which you load with the skill tool."} {
		out := mustRun(t, "", linePrimerArgs(front)...).stdout
		for _, want := range []string{"your human partner's registered nick colin; breaker: more than 8 posts within 10m, or five times that since colin last posted, hold deliveries until colin posts.", skill, "other sessions", "the one named above"} {
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
		// Codex keeps a working directory's git directory read-only in its sandbox, so only
		// the codex front is told to ask for escalation for git's writes.
		if has := strings.Contains(out, "ask for escalation for each of them directly"); has != (front == "codex") {
			t.Fatalf("the %s line primer carries the git escalation paragraph = %v:\n%s", front, has, out)
		}
	}
	// The cap is printed as spelled, 0 included, and says there is no breaker.
	args := append([]string(nil), linePrimerArgs("claude")...)
	for i := range args {
		if args[i] == "8" {
			args[i] = "0"
		}
	}
	if out := mustRun(t, "", args...).stdout; !strings.Contains(out, "breaker off (cap 0).") || strings.Contains(out, "within") {
		t.Fatalf("the primer for --cap 0 does not say so:\n%s", out)
	}
	// The window is printed as the person writes it, not as Go does.
	for window, want := range map[string]string{"90s": "within 1m30s, or", "2h": "within 2h, or", "10m": "within 10m, or", "45s": "within 45s, or"} {
		if out := mustRun(t, "", append(append([]string(nil), linePrimerArgs("claude")...), "--window", window)...).stdout; !strings.Contains(out, want) {
			t.Fatalf("the primer for --window %s does not say %q:\n%s", window, want, out)
		}
	}
}

// TestChannelSkillLineHandsOverTheLineForm: the instructions the real binary
// answers initialize with are the line form, with the values it was launched
// with and no directory among them, and they fit the client's cap.
func TestChannelSkillLineHandsOverTheLineForm(t *testing.T) {
	root := shortRoot(t)
	instructions, stderr := channelInitialize(t, nil, lineChannelArgs(root)...)
	for _, want := range []string{"your human partner's registered nick colin; breaker: more than 8 posts within 10m, or five times that since colin last posted, hold deliveries until colin posts.", "invoked as /parley-line.",
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
			{verb + ": a zero window", plus(base, "--chair", "colin", "--cap", "3", "--window", "0s"), "--window must be above zero"},
			{verb + ": a bare zero window", plus(base, "--chair", "colin", "--cap", "3", "--window", "0"), "--window must be above zero"},
			{verb + ": a negative window", plus(base, "--chair", "colin", "--cap", "3", "--window", "-5m"), "--window must be above zero"},
			{verb + ": a zero window with cap 0", plus(base, "--chair", "colin", "--cap", "0", "--window", "0s"), "--window must be above zero"},
			{verb + ": a window that is not a duration", plus(base, "--chair", "colin", "--cap", "3", "--window", "soon"), "window"},
			{verb + ": a window with no unit", plus(base, "--chair", "colin", "--cap", "3", "--window", "10"), "window"},
			{verb + ": an empty window", plus(base, "--chair", "colin", "--cap", "3", "--window", ""), "window"},
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
	// an installed one's, so the margin asked for here is the modest one; the
	// primer package holds the form to 150 with a sample path.
	if margin := primer.ClaudeInstructionsCap - sizes["line"]; margin < 100 {
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
	// The line form is the shortest, and how far under the cap it starts depends
	// on the machine's temp paths, so a fixed padding crosses the cap on one OS
	// and not another. Measure it first, then lengthen the state directory, which
	// the form names, by what it takes to cross with room to spare, in components
	// short enough for any file system.
	fits := runCheck(t, t.TempDir(), args...)
	if fits.code != 0 {
		t.Fatalf("the unpadded line form exited %d under --check, want 0\nstderr: %s", fits.code, fits.stderr)
	}
	parts := []string{root, "s"}
	for need := primer.ClaudeInstructionsCap - checkedLength(t, fits) + 64; need > 0; need -= 201 {
		parts = append(parts, strings.Repeat("d", min(need, 200)))
	}
	for i := range args {
		if args[i] == filepath.Join(root, "s") {
			args[i] = filepath.Join(parts...)
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
	for _, want := range []string{"parley channel --stream <spec> --nick <nick> --skill line --chair <nick> --cap <n> [--window 10m]",
		"parley primer --front claude|codex|opencode --stream <spec> --nick <nick> --skill line --chair <nick> --cap <n> [--window 10m]",
		"[--skill build|table|line --chair <nick> --cap <n> [--window 10m]]", "lines:", "parley-line skill",
		"there is no first hold", "--target and --worktree are refused", "--cap is for --skill table and --skill line only",
		"--window for --skill line only", "Nothing is released by time passing", "more than <n> of them within --window"} {
		if !strings.Contains(flat, want) {
			t.Fatalf("usage does not mention %q:\n%s", want, top)
		}
	}
	for _, verb := range []string{"channel", "primer", "codex", "opencode"} {
		help := runParley(t, "", verb, "-h").stdout
		for _, want := range []string{"--skill line", "-cap", "-window"} {
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

// TestCodexLineDeliversPeerPostsAtOnceAndTheBreakerIsARate drives the real binary
// with the line flags: a peer's post is delivered before the pump has posted (a
// line has no seal); a second post a window later is delivered too, though a
// count of posts since the chair spoke would have held it; the post that makes
// more than the cap within the window is held, and the chair's post releases it
// ahead of itself. It is what proves the flags reach the Deliverer, which the
// unit tests of the Deliverer cannot.
func TestCodexLineDeliversPeerPostsAtOnceAndTheBreakerIsARate(t *testing.T) {
	dir := resolvedTempDir(t)
	spec := "file:" + filepath.Join(dir, "chat.ndjson")
	stateDir := t.TempDir()
	binDir, record := fakeCodex(t)
	const thread = "0199c0de-c0de-7c0d-8c0d-ec0dec0dec0d"
	const window = 4 * time.Second

	cmd := exec.Command(binary(t), "codex", "--stream", spec, "--nick", "codex-a",
		"--state-dir", stateDir, "--thread", thread, "--debounce", "100ms",
		"--skill", "line", "--chair", "colin", "--cap", "1", "--window", window.String())
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

	// A post more than a window after the last is the second post since the
	// chair's, and it is delivered: the cap is a rate, not a count.
	if err := os.Remove(record); err != nil {
		t.Fatalf("clear the record: %v", err)
	}
	time.Sleep(window + window/4)
	mustRun(t, "", "send", "--stream", spec, "--nick", "claude-b", "--", "phase two done")
	argv = readArgv(t, record, stderr)
	if msg := argv[len(argv)-1]; !strings.Contains(msg, "phase two done") {
		t.Fatalf("the line pump delivered\n%s\nwant the post a window later at once", msg)
	}

	// The next post comes within the window of the last: more than the cap of 1,
	// so it is held.
	if err := os.Remove(record); err != nil {
		t.Fatalf("clear the record: %v", err)
	}
	mustRun(t, "", "send", "--stream", spec, "--nick", "claude-b", "--", "phase three done")
	time.Sleep(600 * time.Millisecond)
	if _, err := os.Stat(record); err == nil {
		t.Fatal("the tripped breaker delivered a post")
	}

	mustRun(t, "", "send", "--stream", spec, "--nick", "colin", "--", "chair talk")
	argv = readArgv(t, record, stderr)
	msg := argv[len(argv)-1]
	held, chair := strings.Index(msg, "phase three done"), strings.Index(msg, "chair talk")
	if held < 0 || chair < 0 || held > chair {
		t.Fatalf("the chair's post delivered\n%s\nwant the held post ahead of the chair's", msg)
	}
}

// TestWindowIsALinesAlone: --window is refused by name, with exit 2, under the
// table and build skills, on each verb that takes the flag, and is accepted
// with a line. A table's breaker counts posts since the chair last posted, and a
// build has none.
func TestWindowIsALinesAlone(t *testing.T) {
	root := shortRoot(t)
	stateDir := filepath.Join(root, "s")
	chat := "file:" + filepath.Join(root, "c")
	target, worktree := filepath.Join(root, "t"), filepath.Join(root, "w")
	plus := func(args []string, extra ...string) []string { return append(append([]string(nil), args...), extra...) }
	facts := []string{"--chair", "colin", "--target", target, "--worktree", worktree}
	for name, args := range map[string][]string{
		"channel build": plus([]string{"channel", "--stream", chat, "--nick", "plan", "--state-dir", stateDir}, plus(facts, "--window", "10m")...),
		"channel table": plus([]string{"channel", "--stream", chat, "--nick", "plan", "--state-dir", stateDir, "--skill", "table", "--cap", "3"}, plus(facts, "--window", "10m")...),
		"primer build":  plus([]string{"primer", "--front", "claude", "--stream", chat, "--nick", "plan", "--reply-file", "/tmp/r"}, plus(facts, "--window", "10m")...),
		"primer table":  plus([]string{"primer", "--front", "claude", "--stream", chat, "--nick", "plan", "--reply-file", "/tmp/r", "--skill", "table"}, plus(facts, "--window", "10m")...),
		"codex build":   {"codex", "--stream", chat, "--nick", "plan", "--state-dir", stateDir, "--thread", "t", "--codex-bin", "/bin/echo", "--window", "10m"},
		"codex table":   {"codex", "--stream", chat, "--nick", "plan", "--state-dir", stateDir, "--thread", "t", "--codex-bin", "/bin/echo", "--skill", "table", "--chair", "colin", "--cap", "3", "--window", "10m"},
	} {
		res := runParley(t, "", args...)
		if res.code != 2 || res.stdout != "" || !strings.Contains(res.stderr, "--window applies to --skill line only") {
			t.Fatalf("%s: parley %v exited %d with stdout %q and stderr %q, want 2 and the refusal", name, args, res.code, res.stdout, res.stderr)
		}
		// Even the window the flag defaults to is refused when it is spelled.
		if name == "channel table" {
			check := runCheck(t, t.TempDir(), args...)
			if check.code != 2 || !strings.Contains(check.stderr, "--window applies to --skill line only") {
				t.Fatalf("channel --check: exit %d, stderr %q", check.code, check.stderr)
			}
		}
	}
	// Left alone, the flag is not an error under a table or a build.
	if res := runParley(t, "", plus([]string{"primer", "--front", "claude", "--stream", chat, "--nick", "plan", "--reply-file", "/tmp/r"}, facts...)...); res.code != 0 {
		t.Fatalf("a build primer without --window exited %d: %s", res.code, res.stderr)
	}
	// And a line takes it on channel and on primer; the pump's is the end-to-end
	// test above, which runs it with --window 4s.
	line := []string{"--skill", "line", "--chair", "colin", "--cap", "3", "--window", "90s"}
	for name, args := range map[string][]string{
		"channel": plus([]string{"channel", "--stream", chat, "--nick", "plan", "--state-dir", stateDir}, line...),
		"primer":  plus([]string{"primer", "--front", "claude", "--stream", chat, "--nick", "plan", "--reply-file", "/tmp/r"}, line...),
	} {
		var res result
		if name == "channel" {
			res = runCheck(t, t.TempDir(), args...)
		} else {
			res = runParley(t, "", args...)
		}
		if res.code != 0 {
			t.Fatalf("%s with a line's --window exited %d: %s", name, res.code, res.stderr)
		}
	}
}

// TestLineWindowDefaultsToTenMinutes: no --window is the 10m the help says, in
// the text each front hands over and in the usage.
func TestLineWindowDefaultsToTenMinutes(t *testing.T) {
	root := shortRoot(t)
	instructions, _ := channelInitialize(t, nil, lineChannelArgs(root)...)
	if want := "breaker: more than 8 posts within 10m, or five times that since colin last posted, hold deliveries until colin posts."; !strings.Contains(instructions, want) {
		t.Fatalf("the instructions for a launch with no --window do not say %q:\n%s", want, instructions)
	}
	for _, verb := range []string{"channel", "codex", "primer"} {
		help := runParley(t, "", verb, "-h").stdout
		if !strings.Contains(help, "-window duration") || !strings.Contains(help, "(default 10m0s)") || !strings.Contains(help, "--window 10m") {
			t.Fatalf("parley %s -h does not document --window and its default of 10m:\n%s", verb, help)
		}
	}
}

// TestCodexThreadAutoUnderALineNamesLineAdd: parley line add makes the thread
// record --thread auto reads, so the error under --skill line for a nick with none
// sends its user to it, and not to the verbs of a table or a build; a pump for a
// session started by hand still takes --thread <id>. A missing --thread says the
// same, before anything is opened.
func TestCodexThreadAutoUnderALineNamesLineAdd(t *testing.T) {
	spec := "file:" + filepath.Join(t.TempDir(), "chat.ndjson")
	stateDir := t.TempDir()
	base := []string{"codex", "--stream", spec, "--nick", "codex-a", "--state-dir", stateDir,
		"--codex-bin", "/bin/echo", "--skill", "line", "--chair", "colin", "--cap", "3"}
	for name, thread := range map[string][]string{"auto": {"--thread", "auto"}, "no thread": nil} {
		res := runParley(t, "", append(append([]string(nil), base...), thread...)...)
		if res.code != 2 {
			t.Fatalf("%s: codex exited %d, want 2\nstderr: %s", name, res.code, res.stderr)
		}
		for _, want := range []string{"parley line add", "--thread"} {
			if !strings.Contains(res.stderr, want) {
				t.Fatalf("%s: the error does not say %q: %s", name, want, res.stderr)
			}
		}
		if name == "auto" && !strings.Contains(res.stderr, "--thread <id>") {
			t.Fatalf("auto: the error does not offer --thread <id>: %s", res.stderr)
		}
		for _, unwanted := range []string{"parley table new", "parley build new", "a line has no thread record"} {
			if strings.Contains(res.stderr, unwanted) {
				t.Fatalf("%s: the error says %q: %s", name, unwanted, res.stderr)
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
