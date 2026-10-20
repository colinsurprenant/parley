package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/colinsurprenant/parley/internal/doctor"
)

// runParleyEnvIn runs the binary in dir with extra environment entries, which
// override the inherited ones of the same name.
func runParleyEnvIn(t *testing.T, dir string, env []string, args ...string) result {
	t.Helper()
	cmd := exec.Command(binary(t), args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
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

// fakeOpenCodeBin writes a stand-in for the opencode CLI in dir: --version
// prints version and the help texts are the real ones of the version last
// verified, printed on stderr as yargs does. The fixtures are named by absolute path, since the
// script runs from the system temp directory.
func fakeOpenCodeBin(t *testing.T, dir, version string) string {
	t.Helper()
	testdata, err := filepath.Abs(filepath.Join("..", "..", "internal", "doctor", "testdata"))
	if err != nil {
		t.Fatal(err)
	}
	help := func(name string) string {
		return filepath.Join(testdata, "opencode-"+doctor.OpenCodeVerified+"-"+name+".help")
	}
	script := fmt.Sprintf(`#!/bin/sh
case "$1 $2" in
"--version "*) echo %q; exit 0 ;;
"--help ") cat %q; exit 0 ;;
"run --help") cat %q; exit 0 ;;
"session --help") cat %q; exit 0 ;;
esac
exit 64
`, version, help("tui"), help("run"), help("session"))
	bin := filepath.Join(dir, "opencode")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// doctorBench is a machine for `parley doctor` to look at: fake codex, claude,
// opencode and ergo, a home with the skill links, and an address nothing
// listens on.
type doctorBench struct {
	codex, claude, opencode, ergo string
	codexLog                      string
	home                          string
	state                         string
	server                        string
}

// bench builds one. codexVersion and strip shape the fake codex; strip names
// flags to leave out of the help of the subcommand it is keyed by.
func bench(t *testing.T, codexVersion string, strip map[string][]string, claudeVersion string) doctorBench {
	t.Helper()
	dir := t.TempDir()
	b := doctorBench{
		home:     t.TempDir(),
		state:    t.TempDir(),
		codexLog: filepath.Join(dir, "codex.log"),
		server:   freeAddr(t),
	}
	// Help texts: the real 0.161.0 ones, minus the stripped flags' lines.
	helpDir := filepath.Join(dir, "help")
	if err := os.MkdirAll(helpDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{"exec", "resume", "queue", "delete"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "internal", "doctor", "testdata", "codex-0.161.0-"+sub+".help"))
		if err != nil {
			t.Fatal(err)
		}
		text := string(raw)
		for _, flag := range strip[sub] {
			re := regexp.MustCompile(`(?m)^\s+(-\w, )?` + regexp.QuoteMeta(flag) + `\b.*\n`)
			if !re.MatchString(text) {
				t.Fatalf("codex %s help has no %s line to strip", sub, flag)
			}
			text = re.ReplaceAllString(text, "")
		}
		if err := os.WriteFile(filepath.Join(helpDir, sub), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	b.codex = filepath.Join(dir, "codex")
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %[1]q
case "$1 $2" in
"--version "*) echo "codex-cli %[2]s"; exit 0 ;;
"exec --help" | "resume --help" | "queue --help" | "delete --help") cat %[3]q/"$1"; exit 0 ;;
esac
exit 64
`, b.codexLog, codexVersion, helpDir)
	if err := os.WriteFile(b.codex, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	b.claude = filepath.Join(dir, "claude")
	if err := os.WriteFile(b.claude, []byte(fmt.Sprintf("#!/bin/sh\necho %q\n", claudeVersion+" (Claude Code)")), 0o755); err != nil {
		t.Fatal(err)
	}
	b.opencode = fakeOpenCodeBin(t, dir, doctor.OpenCodeVerified)
	b.ergo = filepath.Join(dir, "ergo")
	if err := os.WriteFile(b.ergo, []byte("#!/bin/sh\necho ergo-"+doctor.ErgoVerified+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The skill links, to skill directories that hold a SKILL.md.
	for _, name := range []string{"parley-build", "parley-table", "parley-line"} {
		skill := filepath.Join(dir, "skills", name)
		if err := os.MkdirAll(skill, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, root := range [][]string{{".claude", "skills"}, {".agents", "skills"}} {
			link := filepath.Join(b.home, filepath.Join(root...), name)
			if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(skill, link); err != nil {
				t.Fatal(err)
			}
		}
	}
	return b
}

func (b doctorBench) run(t *testing.T, extra ...string) result {
	t.Helper()
	args := append([]string{"doctor", "--codex-bin", b.codex, "--claude-bin", b.claude, "--opencode-bin", b.opencode, "--ergo-bin", b.ergo, "--server", b.server}, extra...)
	return runParleyEnvIn(t, "", []string{"HOME=" + b.home, "XDG_STATE_HOME=" + b.state}, args...)
}

// reportLines are the lines of a doctor run, each of the form "<level> ...".
func reportLines(t *testing.T, out string) []string {
	t.Helper()
	var ls []string
	for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if !regexp.MustCompile(`^(ok|warn|FAIL) `).MatchString(l) {
			t.Fatalf("a line of the report is not `ok|warn|FAIL <surface> <detail>`: %q", l)
		}
		ls = append(ls, l)
	}
	return ls
}

func withLevel(ls []string, level string) []string {
	var out []string
	for _, l := range ls {
		if strings.HasPrefix(l, level+" ") {
			out = append(out, l)
		}
	}
	return out
}

func TestDoctorWithEverythingPresentExits0AndWarnsOnlyAboutTheServer(t *testing.T) {
	b := bench(t, doctor.CodexVerified, nil, doctor.ClaudeVerified)
	res := b.run(t)
	if res.code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr: %s", res.code, res.stdout, res.stderr)
	}
	ls := reportLines(t, res.stdout)
	if fails := withLevel(ls, "FAIL"); len(fails) != 0 {
		t.Errorf("FAIL lines: %v", fails)
	}
	warns := withLevel(ls, "warn")
	if len(warns) != 1 || !strings.Contains(warns[0], "server") || !strings.Contains(warns[0], "parley server") {
		t.Errorf("want exactly the warning that nothing is listening, naming parley server; got %v", warns)
	}
	if got := len(withLevel(ls, "ok")); got != len(ls)-1 {
		t.Errorf("%d ok lines of %d", got, len(ls))
	}
	// OpenCode is checked only when named, so a machine without it is not failed.
	for _, l := range ls {
		if strings.Contains(l, "opencode") {
			t.Errorf("an opencode line though it was not named: %q", l)
		}
	}
}

func TestDoctorWithAFlagMissingExits1NamingTheFlag(t *testing.T) {
	for sub, flag := range map[string]string{"queue": "--thread", "exec": "--skip-git-repo-check", "resume": "--ask-for-approval", "delete": "--force"} {
		t.Run(sub+" "+flag, func(t *testing.T) {
			b := bench(t, doctor.CodexVerified, map[string][]string{sub: {flag}}, doctor.ClaudeVerified)
			res := b.run(t)
			if res.code != 1 {
				t.Fatalf("exit %d, want 1\n%s", res.code, res.stdout)
			}
			fails := withLevel(reportLines(t, res.stdout), "FAIL")
			if len(fails) != 1 || !strings.Contains(fails[0], flag) || !strings.Contains(fails[0], "codex "+sub) {
				t.Errorf("want one FAIL line naming %s on codex %s; got %v", flag, sub, fails)
			}
		})
	}
}

func TestDoctorADifferentVersionIsAWarningNotAFailure(t *testing.T) {
	b := bench(t, "0.162.0", nil, "2.1.297")
	res := b.run(t)
	if res.code != 0 {
		t.Fatalf("exit %d\n%s", res.code, res.stdout)
	}
	ls := reportLines(t, res.stdout)
	var codexWarn, claudeWarn bool
	for _, l := range withLevel(ls, "warn") {
		codexWarn = codexWarn || strings.Contains(l, "codex version") && strings.Contains(l, "newer than "+doctor.CodexVerified) &&
			strings.Contains(l, "It may work as it is") && strings.Contains(l, "run parley doctor --live") &&
			strings.Contains(l, "install the Codex CLI "+doctor.CodexVerified+" ("+doctor.CodexInstallURL+")")
		claudeWarn = claudeWarn || strings.Contains(l, "claude version") && strings.Contains(l, "newer than "+doctor.ClaudeVerified) &&
			strings.Contains(l, "It may work as it is") && strings.Contains(l, "install Claude Code "+doctor.ClaudeVerified+" ("+doctor.ClaudeInstallURL+")")
	}
	if !codexWarn || !claudeWarn {
		t.Errorf("want a warn line for each of the newer codex and claude versions, saying it may work and what to install if a table or a build misbehaves:\n%s", res.stdout)
	}
	if strings.Contains(res.stdout, "bump the verified constant") {
		t.Errorf("a remedy is the maintainer's step:\n%s", res.stdout)
	}
}

func TestDoctorAMissingBinaryAndADanglingSkillLinkAreFAILs(t *testing.T) {
	b := bench(t, doctor.CodexVerified, nil, doctor.ClaudeVerified)
	if err := os.Remove(b.claude); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(b.home, ".claude", "skills", "parley-build")); err != nil {
		t.Fatal(err)
	}
	// Dangling: the link stays, what it points at goes.
	target := filepath.Join(b.home, ".agents", "skills", "parley-table")
	to, err := os.Readlink(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(to); err != nil {
		t.Fatal(err)
	}
	res := b.run(t)
	if res.code != 1 {
		t.Fatalf("exit %d, want 1\n%s", res.code, res.stdout)
	}
	fails := strings.Join(withLevel(reportLines(t, res.stdout), "FAIL"), "\n")
	for _, want := range []string{"claude ", filepath.Join(b.home, ".claude", "skills", "parley-build") + " is missing", target + " -> " + to + " is dangling"} {
		if !strings.Contains(fails, want) {
			t.Errorf("no FAIL line has %q:\n%s", want, fails)
		}
	}
}

// TestDoctorHarnessNarrowsTheChecksToTheNamedInstallation: a machine with one
// harness is not asked about the other's binary, version, flags or skill links,
// and a missing one is not its FAIL.
func TestDoctorHarnessNarrowsTheChecksToTheNamedInstallation(t *testing.T) {
	b := bench(t, doctor.CodexVerified, nil, doctor.ClaudeVerified)
	// Neither the other harness's binary nor its skill links are there.
	if err := os.Remove(b.claude); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(b.home, ".claude")); err != nil {
		t.Fatal(err)
	}
	res := b.run(t, "--harness", "codex")
	if res.code != 0 {
		t.Fatalf("--harness codex: exit %d\n%s\nstderr: %s", res.code, res.stdout, res.stderr)
	}
	ls := reportLines(t, res.stdout)
	for _, l := range ls {
		if strings.Contains(l, "claude") {
			t.Errorf("a claude line though only codex was named: %q", l)
		}
	}
	var skillLines int
	for _, l := range ls {
		if strings.HasPrefix(l, "ok   skill ") {
			skillLines++
		}
	}
	if !strings.Contains(res.stdout, "codex version") || !strings.Contains(res.stdout, "codex exec") || skillLines != 3 {
		t.Errorf("want codex's checks and its three skill links (%d skill lines):\n%s", skillLines, res.stdout)
	}

	// The other way round: claude alone, no codex line and no ~/.agents link.
	b = bench(t, doctor.CodexVerified, nil, doctor.ClaudeVerified)
	if err := os.Remove(b.codex); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(b.home, ".agents")); err != nil {
		t.Fatal(err)
	}
	res = b.run(t, "--harness", "claude")
	if res.code != 0 {
		t.Fatalf("--harness claude: exit %d\n%s", res.code, res.stdout)
	}
	ls = reportLines(t, res.stdout)
	for _, l := range ls {
		if strings.Contains(l, "codex") || strings.Contains(l, ".agents") {
			t.Errorf("a codex line though only claude was named: %q", l)
		}
	}
	claudeLine := ""
	for _, l := range ls {
		if strings.HasPrefix(l, "ok   claude ") && !strings.Contains(l, "claude version") {
			claudeLine = l
		}
	}
	if !strings.Contains(claudeLine, "--dangerously-load-development-channels") || !strings.Contains(claudeLine, "doctor cannot check") {
		t.Errorf("the claude line does not say the launch needs the channel flag and that doctor cannot check it: %q", claudeLine)
	}

	// Repeated, it is both: today's behaviour, and a missing claude FAILs again.
	b = bench(t, doctor.CodexVerified, nil, doctor.ClaudeVerified)
	if err := os.Remove(b.claude); err != nil {
		t.Fatal(err)
	}
	res = b.run(t, "--harness", "claude", "--harness", "codex")
	if res.code != 1 || !strings.Contains(strings.Join(withLevel(reportLines(t, res.stdout), "FAIL"), "\n"), "claude") {
		t.Errorf("--harness claude --harness codex without claude: exit %d\n%s", res.code, res.stdout)
	}
}

// TestDoctorHarnessLiveSpendsNoCodexTurnForClaudeAlone: --live is narrowed too.
func TestDoctorHarnessLiveSpendsNoCodexTurnForClaudeAlone(t *testing.T) {
	ergo := realErgo(t)
	b := bench(t, doctor.CodexVerified, nil, doctor.ClaudeVerified)
	b.ergo = ergo
	res := b.run(t, "--harness", "claude", "--live")
	if res.code != 0 {
		t.Fatalf("exit %d\n%s\nstderr: %s", res.code, res.stdout, res.stderr)
	}
	if strings.Contains(res.stdout, "codex") {
		t.Errorf("a codex line from --harness claude --live:\n%s", res.stdout)
	}
	if !strings.Contains(res.stdout, "ok   ergo live") {
		t.Errorf("the ergo live check is not harness-bound and should have run:\n%s", res.stdout)
	}
	if log, _ := os.ReadFile(b.codexLog); len(log) != 0 {
		t.Errorf("codex was run though only claude was named:\n%s", log)
	}
}

func TestDoctorRefusesAnUnknownHarness(t *testing.T) {
	for _, args := range [][]string{{"--harness", "gemini"}, {"--harness", ""}, {"--harness", "Claude"}, {"--harness", "OpenCode"}, {"--harness", "claude", "--harness", "gemini"}} {
		res := runParley(t, "", append([]string{"doctor"}, args...)...)
		if res.code != exitConfig || !strings.Contains(res.stderr, "--harness") || !strings.Contains(res.stderr, "claude, codex or opencode") {
			t.Errorf("doctor %v: exit %d, stderr %q; want 2 naming --harness", args, res.code, res.stderr)
		}
	}
}

// TestDoctorHarnessOpenCodeChecksOpenCodeAndItsSkills: named alone, OpenCode is
// the only harness looked at: its binary, version, the flags and subcommand of
// its commands, and the skills under the roots it reads. It is served by either
// root, so a machine with only the Codex-side links passes.
func TestDoctorHarnessOpenCodeChecksOpenCodeAndItsSkills(t *testing.T) {
	b := bench(t, doctor.CodexVerified, nil, doctor.ClaudeVerified)
	if err := os.Remove(b.codex); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(b.claude); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(b.home, ".claude")); err != nil {
		t.Fatal(err)
	}
	res := b.run(t, "--harness", "opencode")
	if res.code != 0 {
		t.Fatalf("--harness opencode: exit %d\n%s\nstderr: %s", res.code, res.stdout, res.stderr)
	}
	ls := reportLines(t, res.stdout)
	skills := 0
	for _, l := range ls {
		if strings.Contains(l, "codex") || strings.Contains(l, "claude") && !strings.Contains(l, ".claude") {
			t.Errorf("a codex or claude line though only opencode was named: %q", l)
		}
		if strings.HasPrefix(l, "ok   skill ") {
			skills++
		}
	}
	for _, surface := range []string{"ok   opencode ", "ok   opencode version", "ok   opencode tui", "ok   opencode run", "ok   opencode session"} {
		found := false
		for _, l := range ls {
			found = found || strings.HasPrefix(l, surface)
		}
		if !found {
			t.Errorf("no %q line:\n%s", surface, res.stdout)
		}
	}
	if skills != 3 || !strings.Contains(res.stdout, filepath.Join(b.home, ".agents", "skills", "parley-line")) {
		t.Errorf("want the three skills, found under the Codex root (%d skill lines):\n%s", skills, res.stdout)
	}
}

// TestDoctorOpenCodeCanBeAddedToTheOthers: --harness is repeatable, and a named
// opencode that is missing is its own FAIL while the others pass.
func TestDoctorOpenCodeCanBeAddedToTheOthers(t *testing.T) {
	b := bench(t, doctor.CodexVerified, nil, doctor.ClaudeVerified)
	res := b.run(t, "--harness", "claude", "--harness", "codex", "--harness", "opencode")
	if res.code != 0 {
		t.Fatalf("exit %d\n%s\nstderr: %s", res.code, res.stdout, res.stderr)
	}
	for _, want := range []string{"ok   codex version", "ok   claude version", "ok   opencode version"} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("no %q line:\n%s", want, res.stdout)
		}
	}
	if err := os.Remove(b.opencode); err != nil {
		t.Fatal(err)
	}
	res = b.run(t, "--harness", "codex", "--harness", "opencode")
	fails := withLevel(reportLines(t, res.stdout), "FAIL")
	if res.code != 1 || len(fails) != 1 || !strings.Contains(fails[0], "opencode") || !strings.Contains(fails[0], doctor.OpenCodeInstallURL) {
		t.Errorf("a missing opencode: exit %d, FAIL lines %v", res.code, fails)
	}
}

// TestDoctorOpenCodeIsSpokenToOnlyAsATwo: the front speaks OpenCode 2's HTTP API
// and the version last verified passes; the legacy 1.x is a FAIL that says where
// OpenCode 2 comes from, and any other major version is a FAIL.
func TestDoctorOpenCodeIsSpokenToOnlyAsATwo(t *testing.T) {
	b := bench(t, doctor.CodexVerified, nil, doctor.ClaudeVerified)
	b.opencode = fakeOpenCodeBin(t, t.TempDir(), "opencode v"+doctor.OpenCodeVerified)
	res := b.run(t, "--harness", "opencode")
	if fails := withLevel(reportLines(t, res.stdout), "FAIL"); res.code != 0 || len(fails) != 0 {
		t.Errorf("an OpenCode 2: exit %d, FAIL lines %v\n%s", res.code, fails, res.stdout)
	}
	if !strings.Contains(res.stdout, "ok   opencode version "+doctor.OpenCodeVerified+", the version last verified") {
		t.Errorf("no ok version line for the verified 2.x:\n%s", res.stdout)
	}

	for version, want := range map[string]string{
		"1.18.30":         "the legacy 1.x",
		"opencode v3.0.0": "not a major version parley speaks to",
	} {
		b.opencode = fakeOpenCodeBin(t, t.TempDir(), version)
		res = b.run(t, "--harness", "opencode")
		fails := withLevel(reportLines(t, res.stdout), "FAIL")
		if res.code != 1 || len(fails) != 1 || !strings.Contains(fails[0], want) || !strings.Contains(fails[0], "brew install opencode") {
			t.Errorf("%s: exit %d, FAIL lines %v\n%s", version, res.code, fails, res.stdout)
		}
	}
}

// TestDoctorOpenCodeADifferentTwoVersionIsAWarning: a 2.x other than the verified
// one warns, and says what parley was last verified against.
func TestDoctorOpenCodeADifferentTwoVersionIsAWarning(t *testing.T) {
	b := bench(t, doctor.CodexVerified, nil, doctor.ClaudeVerified)
	b.opencode = fakeOpenCodeBin(t, t.TempDir(), "2.9.0")
	res := b.run(t, "--harness", "opencode")
	warns := withLevel(reportLines(t, res.stdout), "warn")
	var ok bool
	for _, l := range warns {
		ok = ok || strings.Contains(l, "opencode version") && strings.Contains(l, "newer than "+doctor.OpenCodeVerified) &&
			strings.Contains(l, "It may work as it is") && strings.Contains(l, "Parley was "+doctor.OpenCodeVerifiedNote) &&
			strings.Contains(l, "a newer 2.x is expected to work") && !strings.Contains(l, "install OpenCode "+doctor.OpenCodeVerified)
	}
	if res.code != 0 || !ok {
		t.Errorf("exit %d; want a warn that says it may work and what it was verified against:\n%s", res.code, res.stdout)
	}
}

func TestDoctorRefusesAServerThatIsNotLoopbackAndTakesNoArguments(t *testing.T) {
	if res := runParley(t, "", "doctor", "--server", "0.0.0.0:6667"); res.code != exitConfig {
		t.Errorf("--server 0.0.0.0:6667 exited %d, want 2\n%s", res.code, res.stderr)
	}
	if res := runParley(t, "", "doctor", "extra"); res.code != exitConfig {
		t.Errorf("a stray argument exited %d, want 2", res.code)
	}
	if res := runParley(t, "", "doctor", "--codex-bin", ""); res.code != exitConfig {
		t.Errorf("an empty --codex-bin exited %d, want 2", res.code)
	}
	if res := runParley(t, "", "doctor", "--opencode-bin", ""); res.code != exitConfig {
		t.Errorf("an empty --opencode-bin exited %d, want 2", res.code)
	}
}

// TestDoctorStaticWritesNothingInTheStateDir: the static run touches no state
// directory, and no model turn is spent.
func TestDoctorStaticWritesNothingInTheStateDir(t *testing.T) {
	b := bench(t, doctor.CodexVerified, nil, doctor.ClaudeVerified)
	if res := b.run(t); res.code != 0 {
		t.Fatalf("exit %d\n%s", res.code, res.stdout)
	}
	if left, _ := os.ReadDir(b.state); len(left) != 0 {
		t.Errorf("doctor wrote %d entries into the state directory", len(left))
	}
	log, _ := os.ReadFile(b.codexLog)
	if strings.Contains(string(log), "--json") {
		t.Errorf("a static run spent a model turn:\n%s", log)
	}
}

// TestDoctorLive drives --live end to end with a fake codex whose turn prints
// the given JSONL, against the real ergo.
func TestDoctorLive(t *testing.T) {
	ergo := realErgo(t)
	const id = "0199c0de-c0de-7c0d-8c0d-ec0dec0dec0d"
	turn := func(output string) string {
		return `{"type":"thread.started","thread_id":"` + id + `"}` + "\n" +
			`{"type":"turn.started"}` + "\n" +
			`{"type":"item.completed","item":{"id":"item_0","type":"command_execution","command":"printenv CODEX_THREAD_ID","aggregated_output":"` + output + `\n","exit_code":0,"status":"completed"}}` + "\n" +
			`{"type":"item.completed","item":{"id":"item_1","type":"agent_message","text":"` + output + `"}}` + "\n" +
			`{"type":"turn.completed","usage":{}}` + "\n"
	}
	for _, tc := range []struct {
		name, out string
		code      int
		want      string
	}{
		{"a matching id", turn(id), 0, "ok   codex live"},
		{"a mismatching id", turn("0199c0de-0000-7000-8000-000000000000"), 1, "FAIL codex live"},
		{"no thread.started", strings.Replace(turn(id), `{"type":"thread.started","thread_id":"`+id+`"}`+"\n", "", 1), 1, "FAIL codex live"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := bench(t, doctor.CodexVerified, nil, doctor.ClaudeVerified)
			// The same stand-in, now with a turn and a delete.
			turnFile := filepath.Join(t.TempDir(), "turn")
			if err := os.WriteFile(turnFile, []byte(tc.out), 0o644); err != nil {
				t.Fatal(err)
			}
			script, err := os.ReadFile(b.codex)
			if err != nil {
				t.Fatal(err)
			}
			patched := strings.Replace(string(script), "esac\nexit 64", fmt.Sprintf("\"delete --force\") exit 0 ;;\nesac\ncase \"$1\" in exec) cat %q; exit 0 ;; esac\nexit 64", turnFile), 1)
			if err := os.WriteFile(b.codex, []byte(patched), 0o755); err != nil {
				t.Fatal(err)
			}
			b.ergo = ergo
			res := b.run(t, "--live")
			if res.code != tc.code {
				t.Fatalf("exit %d, want %d\n%s\nstderr: %s", res.code, tc.code, res.stdout, res.stderr)
			}
			if !strings.Contains(res.stdout, tc.want) {
				t.Errorf("no %q line:\n%s", tc.want, res.stdout)
			}
			if !strings.Contains(res.stdout, "ok   ergo live") {
				t.Errorf("no ok ergo live line:\n%s", res.stdout)
			}
			log, _ := os.ReadFile(b.codexLog)
			if tc.name != "no thread.started" && !strings.Contains(string(log), "delete --force "+id+"\n") {
				t.Errorf("the throwaway thread was not deleted:\n%s", log)
			}
			if n := strings.Count(string(log), "exec --json"); n != 1 {
				t.Errorf("%d model turns, want 1:\n%s", n, log)
			}
		})
	}
}
