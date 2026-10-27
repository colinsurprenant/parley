package doctor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/colinsurprenant/parley/internal/ergoconf"
)

// fakeCodex describes a stand-in for the codex CLI, written as an sh script. Its
// help texts are the real 0.161.0 ones in testdata, optionally stripped of a
// flag, so what the flag check reads is the shape clap really prints.
type fakeCodex struct {
	version string
	// strip lists flags (as codexSurfaces spells them) the help of the named
	// subcommand must not list.
	strip map[string][]string
	// exec is what `codex exec` prints to stdout; execExit is its status.
	exec     string
	execExit int
	// deleteExit is the status of `codex delete --force`.
	deleteExit int
	// noForce takes --force out of `codex delete --help`.
	noForce bool
}

// install writes the script as <dir>/codex and returns its path and the file
// every invocation's arguments are appended to, one line each.
func (f fakeCodex) install(t *testing.T, dir string) (bin, log string) {
	t.Helper()
	log = filepath.Join(dir, "codex.log")
	helpDir := filepath.Join(dir, "help")
	if err := os.MkdirAll(helpDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, s := range codexSurfaces {
		name := s.cmd[0]
		raw, err := os.ReadFile(filepath.Join("testdata", "codex-0.161.0-"+name+".help"))
		if err != nil {
			t.Fatal(err)
		}
		strip := f.strip[name]
		if name == "delete" && f.noForce {
			strip = append(strip, "--force")
		}
		if err := os.WriteFile(filepath.Join(helpDir, name), []byte(stripFlags(string(raw), strip)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	execOut := filepath.Join(dir, "exec.out")
	if err := os.WriteFile(execOut, []byte(f.exec), 0o644); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %[1]q
case "$1 $2" in
"--version "*) echo "codex-cli %[2]s"; exit 0 ;;
"exec --help" | "resume --help" | "queue --help" | "delete --help") cat %[3]q/"$1"; exit 0 ;;
"delete --force") exit %[4]d ;;
esac
case "$1" in
exec) cat %[5]q; exit %[6]d ;;
esac
echo "fake codex: unexpected arguments: $*" >&2
exit 64
`, log, f.version, helpDir, f.deleteExit, execOut, f.execExit)
	bin = filepath.Join(dir, "codex")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, log
}

// stripFlags drops the definition line of each flag from a help text.
func stripFlags(help string, flags []string) string {
	var out []string
	for _, line := range strings.Split(help, "\n") {
		drop := false
		for _, f := range flags {
			if helpFlags(line).has(f) {
				drop = true
			}
		}
		if !drop {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// fakeVersioned writes a stand-in for a binary that answers --version with out.
func fakeVersioned(t *testing.T, dir, name, out string, exit int) string {
	t.Helper()
	bin := filepath.Join(dir, name)
	script := fmt.Sprintf("#!/bin/sh\ncase \"$1\" in --version) echo %q; exit %d ;; esac\nexit 64\n", out, exit)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func lines(rs []Result, level Level) []Result {
	var out []Result
	for _, r := range rs {
		if r.Level == level {
			out = append(out, r)
		}
	}
	return out
}

func find(t *testing.T, rs []Result, surface string) Result {
	t.Helper()
	for _, r := range rs {
		if r.Surface == surface {
			return r
		}
	}
	t.Fatalf("no %q line in:\n%s", surface, render(rs))
	return Result{}
}

func render(rs []Result) string {
	var b strings.Builder
	for _, r := range rs {
		b.WriteString(r.String() + "\n")
	}
	return b.String()
}

func TestResultLineIsLevelSurfaceDetail(t *testing.T) {
	for _, tc := range []struct {
		r    Result
		want string
	}{
		{Result{OK, "codex", "found /bin/codex"}, "ok   codex            found /bin/codex"},
		{Result{Warn, "codex version", "newer"}, "warn codex version    newer"},
		{Result{Fail, "server", "down"}, "FAIL server           down"},
		// The widest surface names, "opencode version" and "opencode session",
		// fill the column exactly.
		{Result{OK, "opencode session", "subcommand delete"}, "ok   opencode session subcommand delete"},
	} {
		if got := tc.r.String(); got != tc.want {
			t.Errorf("String() = %q, want %q", got, tc.want)
		}
	}
	if Failed([]Result{{OK, "a", ""}, {Warn, "b", ""}}) {
		t.Error("Failed is true with only ok and warn")
	}
	if !Failed([]Result{{OK, "a", ""}, {Fail, "b", ""}}) {
		t.Error("Failed is false with a FAIL")
	}
}

func TestCodexWithEveryFlagIsAllOK(t *testing.T) {
	bin, _ := fakeCodex{version: CodexVerified}.install(t, t.TempDir())
	rs := Check(context.Background(), Options{Codex: true, CodexBin: bin})
	if len(lines(rs, OK)) != len(rs) {
		t.Fatalf("want every line ok:\n%s", render(rs))
	}
	for _, surface := range []string{"codex", "codex version", "codex exec", "codex resume", "codex queue", "codex delete"} {
		find(t, rs, surface)
	}
}

func TestCodexMissingAFlagIsAFAILNamingIt(t *testing.T) {
	for _, s := range codexSurfaces {
		for _, flag := range s.flags {
			name := strings.Join(s.cmd, " ")
			t.Run(name+" "+flag, func(t *testing.T) {
				bin, _ := fakeCodex{version: CodexVerified, strip: map[string][]string{name: {flag}}}.install(t, t.TempDir())
				rs := Check(context.Background(), Options{Codex: true, CodexBin: bin})
				fails := lines(rs, Fail)
				if len(fails) != 1 {
					t.Fatalf("want exactly one FAIL:\n%s", render(rs))
				}
				if fails[0].Surface != "codex "+name || !strings.Contains(fails[0].Detail, flag) {
					t.Errorf("the FAIL is %q, want it to name %s on codex %s", fails[0], flag, name)
				}
				if !Failed(rs) {
					t.Error("Failed is false")
				}
			})
		}
	}
}

func TestCodexProseThatMentionsAFlagIsNotTheFlag(t *testing.T) {
	// The queue help's text about --remote says nothing of --thread, but a help
	// that only mentioned a flag in a sentence must not pass for listing it.
	help := "Usage: codex queue\n\nOptions:\n      --remote <ADDR>\n          Pass --thread and --message along; -C is ignored here\n"
	for _, f := range []string{"--thread", "--message", "-C/--cd"} {
		if helpFlags(help).has(f) {
			t.Errorf("a sentence mentioning %s counted as listing it", f)
		}
	}
}

func TestCodexMissingBinaryIsOneFAIL(t *testing.T) {
	rs := Check(context.Background(), Options{Codex: true, CodexBin: filepath.Join(t.TempDir(), "no-codex")})
	if len(rs) != 1 || rs[0].Level != Fail || rs[0].Surface != "codex" {
		t.Fatalf("want one codex FAIL:\n%s", render(rs))
	}
}

func TestCodexHelpThatFailsIsAFAIL(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "codex")
	script := "#!/bin/sh\ncase \"$1\" in --version) echo codex-cli " + CodexVerified + "; exit 0 ;; esac\necho 'unknown subcommand' >&2\nexit 2\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	rs := Check(context.Background(), Options{Codex: true, CodexBin: bin})
	if got := len(lines(rs, Fail)); got != len(codexSurfaces) {
		t.Fatalf("want a FAIL per subcommand, got %d:\n%s", got, render(rs))
	}
	if r := find(t, rs, "codex queue"); !strings.Contains(r.Detail, "unknown subcommand") {
		t.Errorf("the FAIL does not say what codex said: %q", r)
	}
}

func TestAProbeKeepsOnlyABoundedShareOfARunawayBinary(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "talker")
	script := "#!/bin/sh\nhead -c 3145728 /dev/zero | tr '\\000' x\nhead -c 3145728 /dev/zero | tr '\\000' y >&2\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err := probeFor(context.Background(), 20*time.Second, bin)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if len(stdout) != maxProbeOutput || len(stderr) != maxProbeOutput {
		t.Fatalf("kept %d bytes of stdout and %d of stderr, want %d of each", len(stdout), len(stderr), maxProbeOutput)
	}
}

func TestLiveErgoSanitizesWhatAFailingErgoSaid(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "ergo")
	script := "#!/bin/sh\nprintf '\\033[2Jboom\\n' >&2\nexit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	rs := liveErgo(context.Background(), bin)
	fails := lines(rs, Fail)
	if len(fails) == 0 {
		t.Fatalf("a failing ergo passed: %s", render(rs))
	}
	for _, r := range fails {
		if strings.ContainsRune(r.Detail, 0x1b) {
			t.Errorf("ergo's output reached the report raw: %q", r)
		}
	}
	if !strings.Contains(render(rs), "boom") {
		t.Errorf("the report does not carry what ergo said: %s", render(rs))
	}
}

// TestCompareVersionIsAFloorWithinTheMajorVersion: the one rule for every tool.
// The same version, and a newer one of the same major version, are ok (the line
// names both); an older one, or one of a different major version, warns, and so
// does one equal in number with another suffix, and one that cannot be read.
// Major version 0 is a major version like any other.
func TestCompareVersionIsAFloorWithinTheMajorVersion(t *testing.T) {
	const remedy = "Do the remedy."
	for _, tc := range []struct {
		name, tool, verified, out string
		level                     Level
		detail                    string
	}{
		{"equal", "claude", "2.1.296", "2.1.296 (Claude Code)", OK, "2.1.296, the version last verified"},
		{"newer patch", "claude", "2.1.296", "2.1.311 (Claude Code)", OK, "2.1.311, newer than 2.1.296, the version last verified"},
		{"newer minor", "claude", "2.1.296", "2.2.0", OK, "2.2.0, newer than 2.1.296, the version last verified"},
		{"newer by number, not by text", "claude", "2.9.0", "2.10.0", OK, "2.10.0, newer than 2.9.0, the version last verified"},
		{"newer, a suffix on it", "claude", "2.1.296", "2.1.311-rc.1", OK, "2.1.311-rc.1, newer than 2.1.296, the version last verified"},
		{"older patch", "claude", "2.1.296", "2.1.295 (Claude Code)", Warn, "2.1.295 is older than 2.1.296, the version last verified, and may lack something parley uses. " + remedy},
		{"older minor", "claude", "2.1.296", "2.0.999", Warn, "2.0.999 is older than 2.1.296, the version last verified, and may lack something parley uses. " + remedy},
		{"older by number, not by text", "claude", "2.10.0", "2.9.0", Warn, "2.9.0 is older than 2.10.0, the version last verified, and may lack something parley uses. " + remedy},
		{"newer major", "claude", "2.1.296", "3.0.0", Warn, "3.0.0 is a different major version from 2.1.296, the version last verified, and may not work. " + remedy},
		{"older major", "claude", "2.1.296", "1.99.99", Warn, "1.99.99 is a different major version from 2.1.296, the version last verified, and may not work. " + remedy},
		{"same core, a suffix", "claude", "2.1.296", "2.1.296-rc.1", Warn, "2.1.296-rc.1 is different than 2.1.296, the version last verified. It may work as it is. " + remedy},
		{"unreadable", "claude", "2.1.296", "claude", Warn, `claude --version printed "claude", which holds no version; the last verified is 2.1.296. It may work as it is. ` + remedy},
		{"0.x equal", "codex", "0.161.0", "codex-cli 0.161.0", OK, "0.161.0, the version last verified"},
		{"0.x newer", "codex", "0.161.0", "codex-cli 0.162.0", OK, "0.162.0, newer than 0.161.0, the version last verified"},
		{"0.x older", "codex", "0.161.0", "codex-cli 0.160.9", Warn, "0.160.9 is older than 0.161.0, the version last verified, and may lack something parley uses. " + remedy},
		{"0.x to 1.x", "codex", "0.161.0", "codex-cli 1.0.0", Warn, "1.0.0 is a different major version from 0.161.0, the version last verified, and may not work. " + remedy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := compareVersion(tc.tool, tc.out, tc.verified, remedy)
			if r.Level != tc.level || r.Detail != tc.detail || r.Surface != tc.tool+" version" {
				t.Errorf("got %v %q %q, want %v %q", r.Level, r.Surface, r.Detail, tc.level, tc.detail)
			}
		})
	}
}

func TestVersionsAgainstTheVerifiedOnes(t *testing.T) {
	for _, tc := range []struct {
		name, out string
		level     Level
		want      string
	}{
		{"equal", "codex-cli " + CodexVerified, OK, "the version last verified"},
		{"newer", "codex-cli 0.162.0", OK, "newer than " + CodexVerified},
		{"older", "codex-cli 0.160.9", Warn, "older than " + CodexVerified},
		{"newer major", "codex-cli 1.0.0", Warn, "different major version from " + CodexVerified},
		{"same core, a suffix", "codex-cli " + CodexVerified + "-alpha.1", Warn, "different than"},
		{"unreadable", "codex-cli", Warn, "holds no version"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := fakeVersioned(t, t.TempDir(), "codex", tc.out, 0)
			r := checkVersion(context.Background(), "codex", bin, CodexVerified, codexRemedy)
			if r.Level != tc.level || !strings.Contains(r.Detail, tc.want) {
				t.Errorf("got %q, want level %v with %q", r, tc.level, tc.want)
			}
			if tc.level == Warn {
				wantUserRemedy(t, r, "the Codex CLI "+orNewer(CodexVerified), CodexInstallURL)
			} else if strings.Contains(r.Detail, "install") {
				t.Errorf("an ok version line carries a remedy: %q", r)
			}
		})
	}
	bin := fakeVersioned(t, t.TempDir(), "codex", "boom", 1)
	if r := checkVersion(context.Background(), "codex", bin, CodexVerified, codexRemedy); r.Level != Fail {
		t.Errorf("a --version that fails is %v, want FAIL", r.Level)
	}
}

// TestRemediesAskForTheVerifiedVersionOrANewerOfItsMajor: no remedy sends a person
// back to the exact version that was verified.
func TestRemediesAskForTheVerifiedVersionOrANewerOfItsMajor(t *testing.T) {
	for _, tc := range []struct{ remedy, want string }{
		{claudeRemedy, "install Claude Code 2.1.296 or a newer 2.x (" + ClaudeInstallURL + ")"},
		{codexRemedy, "run parley doctor --live; if a table, a build or a line misbehaves, install the Codex CLI 0.161.0 or a newer 0.x (" + CodexInstallURL + ")"},
		{ergoRemedy, "run parley doctor --live; if a table, a build or a line misbehaves, install ergo 2.19.1 or a newer 2.x (" + ergoVerifiedURL + ")"},
	} {
		if !strings.Contains(tc.remedy, tc.want) {
			t.Errorf("%q does not say %q", tc.remedy, tc.want)
		}
	}
	if got := orNewer("10.4.2-rc.1"); got != "10.4.2-rc.1 or a newer 10.x" {
		t.Errorf("orNewer = %q", got)
	}
}

// wantUserRemedy checks that a version warning speaks to the person running the
// machine and not to parley's maintainer: it names what to install if a table, a
// build or a line misbehaves and links where, and says nothing of bumping a
// constant.
func wantUserRemedy(t *testing.T, r Result, install, url string) {
	t.Helper()
	for _, want := range []string{"if a table, a build or a line misbehaves, install " + install, "(" + url + ")"} {
		if !strings.Contains(strings.ToLower(r.Detail), strings.ToLower(want)) {
			t.Errorf("%q does not say %q", r, want)
		}
	}
	for _, maintainer := range []string{"bump", "constant", "next live table"} {
		if strings.Contains(r.Detail, maintainer) {
			t.Errorf("%q is a maintainer's step (%q), not a user's", r, maintainer)
		}
	}
}

func TestAMissingCodexNamesHowToInstallItWithALink(t *testing.T) {
	rs := Check(context.Background(), Options{Codex: true, CodexBin: filepath.Join(t.TempDir(), "none")})
	if len(rs) != 1 || rs[0].Level != Fail || !strings.Contains(rs[0].Detail, "install the Codex CLI ("+CodexInstallURL+")") || !strings.Contains(rs[0].Detail, "--codex-bin") {
		t.Errorf("a missing codex: %s", render(rs))
	}
}

func TestClaudeIsVersionOnlyAndWarnsOnAnOlderOrAnotherMajorOne(t *testing.T) {
	dir := t.TempDir()
	bin := fakeVersioned(t, dir, "claude", ClaudeVerified+" (Claude Code)", 0)
	rs := Check(context.Background(), Options{Claude: true, ClaudeBin: bin})
	if len(rs) != 2 || len(lines(rs, OK)) != 2 {
		t.Fatalf("want found and version, both ok:\n%s", render(rs))
	}
	// The claude line says the launch needs the flag and that doctor cannot
	// check it; the version line is the version and nothing else.
	for _, want := range []string{"found " + bin, "--dangerously-load-development-channels", "doctor cannot check"} {
		if !strings.Contains(find(t, rs, "claude").Detail, want) {
			t.Errorf("the claude line does not say %q: %q", want, find(t, rs, "claude"))
		}
	}
	if strings.Contains(find(t, rs, "claude version").Detail, "--dangerously") {
		t.Errorf("the version line carries the channel note: %q", find(t, rs, "claude version"))
	}

	// A newer 2.x is ok and says which it is; a remedy is for the versions that warn.
	bin = fakeVersioned(t, t.TempDir(), "claude", "2.1.311 (Claude Code)", 0)
	rs = Check(context.Background(), Options{Claude: true, ClaudeBin: bin})
	if r := find(t, rs, "claude version"); r.Level != OK || r.Detail != "2.1.311, newer than "+ClaudeVerified+", the version last verified" {
		t.Errorf("claude 2.1.311: %q", r)
	}
	if len(lines(rs, Warn)) != 0 {
		t.Errorf("a newer claude of the same major version warned:\n%s", render(rs))
	}

	for _, tc := range []struct{ out, want string }{
		{"2.1.295 (Claude Code)", "older than " + ClaudeVerified},
		{"3.0.0 (Claude Code)", "a different major version from " + ClaudeVerified},
	} {
		bin = fakeVersioned(t, t.TempDir(), "claude", tc.out, 0)
		rs = Check(context.Background(), Options{Claude: true, ClaudeBin: bin})
		r := find(t, rs, "claude version")
		if r.Level != Warn || !strings.Contains(r.Detail, tc.want) {
			t.Errorf("claude %s: %q", tc.out, r)
		}
		wantUserRemedy(t, r, "Claude Code "+orNewer(ClaudeVerified), ClaudeInstallURL)
		if Failed(rs) {
			t.Errorf("claude %s failed the check; it is a warning", tc.out)
		}
	}

	rs = Check(context.Background(), Options{Claude: true, ClaudeBin: filepath.Join(dir, "none")})
	if len(rs) != 1 || rs[0].Level != Fail {
		t.Errorf("a missing claude: %s", render(rs))
	} else if !strings.Contains(rs[0].Detail, "install Claude Code ("+ClaudeInstallURL+")") || !strings.Contains(rs[0].Detail, "--claude-bin") {
		t.Errorf("a missing claude does not say how to install it, with a link: %q", rs[0])
	}
}

func TestOnlyTheChosenSurfacesAreChecked(t *testing.T) {
	if rs := Check(context.Background(), Options{}); len(rs) != 0 {
		t.Errorf("no surface chosen, yet:\n%s", render(rs))
	}
	bin := fakeVersioned(t, t.TempDir(), "claude", ClaudeVerified+" (Claude Code)", 0)
	rs := Check(context.Background(), Options{Claude: true, ClaudeBin: bin, CodexBin: "no-such-codex"})
	for _, r := range rs {
		if strings.HasPrefix(r.Surface, "codex") {
			t.Errorf("codex was checked though only claude was chosen: %q", r)
		}
	}
}

func TestErgoFoundAndMissing(t *testing.T) {
	bin := fakeVersioned(t, t.TempDir(), "ergo", "ergo-"+ErgoVerified, 0)
	rs := Check(context.Background(), Options{ErgoBin: bin})
	if len(rs) != 2 || len(lines(rs, OK)) != 2 || !strings.Contains(find(t, rs, "ergo version").Detail, "the version last verified") {
		t.Errorf("ergo found: %s", render(rs))
	}
	rs = Check(context.Background(), Options{ErgoBin: filepath.Join(t.TempDir(), "none")})
	if len(rs) != 1 || rs[0].Level != Fail || !strings.Contains(rs[0].Detail, "install ergo from "+ErgoInstallURL) || !strings.Contains(rs[0].Detail, "--ergo-bin") {
		t.Errorf("ergo missing: %s", render(rs))
	}
}

// TestErgoVersionIsComparedWithTheVerifiedOne: a newer ergo of the verified major
// version is ok; an older one, another major version and one that cannot be read
// warn in the harnesses' own words, never a failure; and one that does not run is
// a FAIL.
func TestErgoVersionIsComparedWithTheVerifiedOne(t *testing.T) {
	for _, tc := range []struct {
		name, out string
		level     Level
		want      string
	}{
		{"newer", "ergo-2.20.0", OK, "2.20.0, newer than " + ErgoVerified + ", the version last verified"},
		{"older", "ergo-2.18.0", Warn, "older than " + ErgoVerified},
		{"another major", "ergo-3.0.0", Warn, "a different major version from " + ErgoVerified},
		{"unreadable", "ergo-fake", Warn, "holds no version"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := fakeVersioned(t, t.TempDir(), "ergo", tc.out, 0)
			rs := Check(context.Background(), Options{ErgoBin: bin})
			r := find(t, rs, "ergo version")
			if r.Level != tc.level || !strings.Contains(r.Detail, tc.want) {
				t.Errorf("got %q, want a %v with %q", r, tc.level, tc.want)
			}
			if tc.level == Warn {
				wantUserRemedy(t, r, "ergo "+orNewer(ErgoVerified), ergoVerifiedURL)
				if !strings.Contains(r.Detail, "run parley doctor --live") {
					t.Errorf("an ergo warning that does not offer the live check: %q", r)
				}
			}
			if Failed(rs) {
				t.Errorf("a different ergo version failed the check:\n%s", render(rs))
			}
		})
	}
	bin := fakeVersioned(t, t.TempDir(), "ergo", "boom", 1)
	rs := Check(context.Background(), Options{ErgoBin: bin, Live: true})
	if r := find(t, rs, "ergo version"); r.Level != Fail {
		t.Errorf("an ergo whose --version fails is %v, want FAIL", r.Level)
	}
	for _, r := range rs {
		if r.Surface == "ergo live" {
			t.Errorf("the live check ran against an ergo that does not run: %q", r)
		}
	}
}

// TestErgoVerifiedIsTheVersionTheEmbeddedConfigurationWasWrittenFor keeps the
// constant in step with the file it describes.
func TestErgoVerifiedIsTheVersionTheEmbeddedConfigurationWasWrittenFor(t *testing.T) {
	if !strings.Contains(ergoconf.YAML, "ergo v"+ErgoVerified+" configuration") {
		t.Errorf("ircd.yaml no longer says it is an ergo v%s configuration; move ErgoVerified or the file together", ErgoVerified)
	}
}

// realErgo is the ergo the live tests run, or a skip.
func realErgo(t *testing.T) string {
	t.Helper()
	if bin := os.Getenv("PARLEY_ERGO"); bin != "" {
		return bin
	}
	bin, err := exec.LookPath("ergo")
	if err != nil {
		t.Skip("no ergo: set $PARLEY_ERGO or put ergo on PATH (see contrib/ergo/README.md)")
	}
	return bin
}

func TestBinariesNamedByARelativePathAreResolvedBeforeAnythingChangesDirectory(t *testing.T) {
	dir := t.TempDir()
	fakeVersioned(t, dir, "claude", ClaudeVerified+" (Claude Code)", 0)
	t.Chdir(dir)
	rs := Check(context.Background(), Options{Claude: true, ClaudeBin: "./claude"})
	if len(rs) != 2 || len(lines(rs, OK)) != 2 {
		t.Fatalf("a relative --claude-bin was not run:\n%s", render(rs))
	}
	if got, _ := Resolve("./claude"); !filepath.IsAbs(got) {
		t.Errorf("Resolve returned %q, want an absolute path", got)
	}
}
