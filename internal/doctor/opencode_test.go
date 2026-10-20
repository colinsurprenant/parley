package doctor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeOpenCode describes a stand-in for the opencode CLI, written as an sh
// script. Its help texts are the real 1.18.30 ones in testdata (the TUI's with
// its banner cut off), optionally stripped of a flag or the delete subcommand,
// and are printed on stderr, where yargs prints them; or, with cli set to
// "2.0.32", the real 2.0.32 ones, which are printed on stdout.
type fakeOpenCode struct {
	version string
	// cli is the version whose help texts the stand-in prints: 1.18.30 if empty.
	cli string
	// strip lists flags (as openCodeSurfaces spells them) and subcommands, as
	// "delete", the help of the named command must not list; the TUI's is "tui"
	// and session list's is "session-list".
	strip map[string][]string
	// helpOnStdout prints the help on stdout instead.
	helpOnStdout bool
	// versionExit is the status of --version.
	versionExit int
}

// install writes the script as <dir>/opencode and returns its path and the file
// every invocation's arguments are appended to, one line each.
func (f fakeOpenCode) install(t *testing.T, dir string) (bin, log string) {
	t.Helper()
	log = filepath.Join(dir, "opencode.log")
	helpDir := filepath.Join(dir, "help")
	if err := os.MkdirAll(helpDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cli, listing := f.cli, "opencode "
	if cli == "" {
		cli = "1.18.30"
	}
	for _, name := range []string{"tui", "run", "session", "session-list"} {
		raw, err := os.ReadFile(filepath.Join("testdata", "opencode-"+cli+"-"+name+".help"))
		if err != nil {
			t.Fatal(err)
		}
		text := stripFlags(string(raw), f.strip[name])
		for _, sub := range f.strip[name] {
			if !strings.HasPrefix(sub, "-") {
				// OpenCode 1 lists "opencode session delete <sessionID>", and 2 "delete".
				if cli != "1.18.30" {
					text = stripLines(text, sub+" ")
				} else {
					text = stripLines(text, listing+name+" "+sub)
				}
			}
		}
		if err := os.WriteFile(filepath.Join(helpDir, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	redirect := ">&2"
	if f.helpOnStdout || cli != "1.18.30" {
		redirect = ""
	}
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %[1]q
case "$1 $2" in
"--version "*) echo "%[2]s"; exit %[3]d ;;
"--help ") cat %[4]q/tui %[5]s; exit 0 ;;
"run --help" | "session --help") cat %[4]q/"$1" %[5]s; exit 0 ;;
"session list") if [ "$3" = --help ]; then cat %[4]q/session-list %[5]s; exit 0; fi ;;
esac
echo "fake opencode: unexpected arguments: $*" >&2
exit 64
`, log, f.version, f.versionExit, helpDir, redirect)
	bin = filepath.Join(dir, "opencode")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, log
}

// stripLines drops the lines that begin, after their indent, with prefix.
func stripLines(text, prefix string) string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), prefix) {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

func TestOpenCodeWithEveryFlagIsAllOK(t *testing.T) {
	bin, _ := fakeOpenCode{version: OpenCodeVerified}.install(t, t.TempDir())
	rs := Check(context.Background(), Options{OpenCode: true, OpenCodeBin: bin})
	if len(rs) != 6 || len(lines(rs, OK)) != 6 {
		t.Fatalf("want six ok lines:\n%s", render(rs))
	}
	for _, surface := range []string{"opencode", "opencode version", "opencode tui", "opencode run", "opencode session", "opencode session list"} {
		find(t, rs, surface)
	}
	// What each line says it checked is what parley uses.
	for surface, want := range map[string]string{
		"opencode tui":          "--port -s/--session",
		"opencode run":          "--format -m/--model --title",
		"opencode session":      "subcommand delete subcommand list",
		"opencode session list": "--format -n/--max-count",
	} {
		if got := find(t, rs, surface).Detail; got != want {
			t.Errorf("%s says %q, want %q", surface, got, want)
		}
	}
}

// TestOpenCodeHelpOnEitherStreamIsRead: yargs prints a help text on stderr, clap
// on stdout, and a surface is read from whichever the binary used.
func TestOpenCodeHelpOnEitherStreamIsRead(t *testing.T) {
	bin, _ := fakeOpenCode{version: OpenCodeVerified, helpOnStdout: true}.install(t, t.TempDir())
	if rs := Check(context.Background(), Options{OpenCode: true, OpenCodeBin: bin}); len(lines(rs, OK)) != len(rs) {
		t.Fatalf("a help on stdout was not read:\n%s", render(rs))
	}
}

func TestOpenCodeMissingAFlagOrSubcommandIsAFAILNamingIt(t *testing.T) {
	for _, tc := range []struct {
		name, surface string
		strip         map[string][]string
		want          string
	}{
		// The TUI's --fork line mentions --session in prose; only the definition
		// line of -s/--session lists it.
		{"tui -s/--session", "opencode tui", map[string][]string{"tui": {"-s/--session"}}, "-s/--session"},
		{"tui --port", "opencode tui", map[string][]string{"tui": {"--port"}}, "--port"},
		{"run --format", "opencode run", map[string][]string{"run": {"--format"}}, "--format"},
		{"run -m/--model", "opencode run", map[string][]string{"run": {"-m/--model"}}, "-m/--model"},
		{"run --title", "opencode run", map[string][]string{"run": {"--title"}}, "--title"},
		{"session delete", "opencode session", map[string][]string{"session": {"delete"}}, "the delete subcommand"},
		{"session list", "opencode session", map[string][]string{"session": {"list"}}, "the list subcommand"},
		// A stopped run's stray session is found with list --format json -n.
		{"session list --format", "opencode session list", map[string][]string{"session-list": {"--format"}}, "--format"},
		{"session list -n/--max-count", "opencode session list", map[string][]string{"session-list": {"-n/--max-count"}}, "-n/--max-count"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin, _ := fakeOpenCode{version: OpenCodeVerified, strip: tc.strip}.install(t, t.TempDir())
			rs := Check(context.Background(), Options{OpenCode: true, OpenCodeBin: bin})
			fails := lines(rs, Fail)
			if len(fails) != 1 {
				t.Fatalf("want exactly one FAIL:\n%s", render(rs))
			}
			if fails[0].Surface != tc.surface || !strings.Contains(fails[0].Detail, tc.want) || !strings.Contains(fails[0].Detail, "does not list") {
				t.Errorf("the FAIL is %q, want it to name %s on %s", fails[0], tc.want, tc.surface)
			}
			if !Failed(rs) {
				t.Error("Failed is false")
			}
		})
	}
}

// TestAnOpenCodeSubcommandInProseIsNotTheListing: the delete subcommand is the
// head of an indented line in the Commands list, and a sentence that mentions
// it, or a longer command that begins with it, is not that.
func TestAnOpenCodeSubcommandInProseIsNotTheListing(t *testing.T) {
	for _, help := range []string{
		"Commands:\n  opencode session list    list sessions\n\nrun opencode session delete <id> to remove one\n",
		"Commands:\n  opencode session deleted  not the one\n",
		"Commands:\n  opencode sessions delete <id>  not the one\n",
	} {
		if listsCommand(help, []string{"opencode", "session", "delete"}) {
			t.Errorf("counted as listing the command:\n%s", help)
		}
	}
	if !listsCommand("Commands:\n  opencode session delete <sessionID>  delete a session\n", []string{"opencode", "session", "delete"}) {
		t.Error("the real listing line was not found")
	}
}

func TestOpenCodeMissingBinaryIsOneFAILNamingHowToInstallIt(t *testing.T) {
	rs := Check(context.Background(), Options{OpenCode: true, OpenCodeBin: filepath.Join(t.TempDir(), "no-opencode")})
	if len(rs) != 1 || rs[0].Level != Fail || rs[0].Surface != "opencode" {
		t.Fatalf("want one opencode FAIL:\n%s", render(rs))
	}
	for _, want := range []string{"install OpenCode (" + OpenCodeInstallURL + ")", "--opencode-bin"} {
		if !strings.Contains(rs[0].Detail, want) {
			t.Errorf("a missing opencode does not say %q: %q", want, rs[0])
		}
	}
}

func TestOpenCodeHelpThatFailsIsAFAIL(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "opencode")
	script := "#!/bin/sh\ncase \"$1\" in --version) echo " + OpenCodeVerified + "; exit 0 ;; esac\necho 'unknown argument' >&2\nexit 2\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	rs := Check(context.Background(), Options{OpenCode: true, OpenCodeBin: bin})
	if got := len(lines(rs, Fail)); got != len(openCodeSurfaces) {
		t.Fatalf("want a FAIL per surface, got %d:\n%s", got, render(rs))
	}
	r := find(t, rs, "opencode session")
	for _, want := range []string{"opencode session --help", "unknown argument"} {
		if !strings.Contains(r.Detail, want) {
			t.Errorf("the FAIL does not say %q: %q", want, r)
		}
	}
}

func TestOpenCodeVersionsAgainstTheVerifiedOne(t *testing.T) {
	for _, tc := range []struct {
		name, out string
		level     Level
		want      string
	}{
		{"equal", OpenCodeVerified, OK, "the version last verified"},
		{"newer", "1.19.3", Warn, "newer than " + OpenCodeVerified},
		{"older", "1.18.2", Warn, "older than " + OpenCodeVerified},
		{"an older major", "0.9.0", Warn, "older than " + OpenCodeVerified},
		{"unreadable", "opencode", Warn, "holds no version"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin, _ := fakeOpenCode{version: tc.out}.install(t, t.TempDir())
			rs := Check(context.Background(), Options{OpenCode: true, OpenCodeBin: bin})
			r := find(t, rs, "opencode version")
			if r.Level != tc.level || !strings.Contains(r.Detail, tc.want) {
				t.Errorf("got %q, want level %v with %q", r, tc.level, tc.want)
			}
			if tc.level == Warn {
				for _, want := range []string{"It may work as it is", "If an OpenCode session misbehaves, install OpenCode " + OpenCodeVerified + " (" + OpenCodeInstallURL + ")",
					"npm i -g opencode-ai@" + OpenCodeVerified} {
					if !strings.Contains(r.Detail, want) {
						t.Errorf("%q does not say %q", r, want)
					}
				}
			}
			if Failed(rs) {
				t.Errorf("a version other than the verified one failed the check:\n%s", render(rs))
			}
			// Its surfaces are checked all the same.
			find(t, rs, "opencode session")
		})
	}
}

// TestOpenCodeTwoIsSupportedAndItsOwnSurfacesAreChecked: a 2.x is a different CLI
// from a 1.x (no --port, the short spelling after the long in its help, its
// subcommands in a SUBCOMMANDS section), and parley speaks to it, so its version
// is read against the 2.x verified and its help against what parley uses there.
func TestOpenCodeTwoIsSupportedAndItsOwnSurfacesAreChecked(t *testing.T) {
	bin, log := fakeOpenCode{version: OpenCodeVerified2, cli: "2.0.32"}.install(t, t.TempDir())
	rs := Check(context.Background(), Options{OpenCode: true, OpenCodeBin: bin})
	if len(rs) != 5 || len(lines(rs, OK)) != 5 {
		t.Fatalf("want five ok lines:\n%s", render(rs))
	}
	if r := find(t, rs, "opencode version"); !strings.Contains(r.Detail, "the version last verified") || !strings.Contains(r.Detail, OpenCodeVerified2) {
		t.Errorf("the version line is %q", r)
	}
	for surface, want := range map[string]string{
		"opencode tui":     "-s/--session",
		"opencode run":     "--format -m/--model --title -s/--session",
		"opencode session": "subcommand delete subcommand list",
	} {
		if got := find(t, rs, surface).Detail; got != want {
			t.Errorf("%s says %q, want %q", surface, got, want)
		}
	}
	// 2.x has no session list --format -n: parley looks for no stray session there.
	for _, r := range rs {
		if r.Surface == "opencode session list" {
			t.Errorf("OpenCode 2's session list was checked: %q", r)
		}
	}
	got, _ := os.ReadFile(log)
	for _, want := range []string{"--version\n", "--help\n", "run --help\n", "session --help\n"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("opencode was never run as %q:\n%s", strings.TrimSpace(want), got)
		}
	}
}

// TestOpenCodeTwoOtherThanTheVerifiedOneWarns: as for a 1.x, a different version
// is a warning with the way out, the way out here being the 2.x that was verified.
func TestOpenCodeTwoOtherThanTheVerifiedOneWarns(t *testing.T) {
	for _, tc := range []struct{ out, want string }{
		{"2.0.40", "newer than " + OpenCodeVerified2},
		{"opencode v2.1.0", "newer than " + OpenCodeVerified2},
		{"2.0.3", "older than " + OpenCodeVerified2},
		{"2.0.32-rc.1", "different than " + OpenCodeVerified2},
	} {
		bin, _ := fakeOpenCode{version: tc.out, cli: "2.0.32"}.install(t, t.TempDir())
		rs := Check(context.Background(), Options{OpenCode: true, OpenCodeBin: bin})
		r := find(t, rs, "opencode version")
		if r.Level != Warn || !strings.Contains(r.Detail, tc.want) {
			t.Errorf("%q: got %q, want a warning with %q", tc.out, r, tc.want)
		}
		for _, want := range []string{"It may work as it is", "If an OpenCode session misbehaves, install OpenCode " + OpenCodeVerified2 + " (" + OpenCodeInstallURL + ")", "simulation mode"} {
			if !strings.Contains(r.Detail, want) {
				t.Errorf("%q: %q does not say %q", tc.out, r, want)
			}
		}
		if strings.Contains(r.Detail, "opencode-ai") {
			t.Errorf("%q: the remedy is OpenCode 1's: %q", tc.out, r)
		}
		if Failed(rs) {
			t.Errorf("%q: a 2.x other than the verified one failed the check:\n%s", tc.out, render(rs))
		}
		find(t, rs, "opencode session")
	}
}

func TestOpenCodeTwoMissingWhatParleyUsesIsAFAILNamingIt(t *testing.T) {
	for _, tc := range []struct {
		name, surface string
		strip         map[string][]string
		want          string
	}{
		{"tui -s/--session", "opencode tui", map[string][]string{"tui": {"-s/--session"}}, "-s/--session"},
		{"run --format", "opencode run", map[string][]string{"run": {"--format"}}, "--format"},
		{"run -m/--model", "opencode run", map[string][]string{"run": {"-m/--model"}}, "-m/--model"},
		{"run --title", "opencode run", map[string][]string{"run": {"--title"}}, "--title"},
		{"run -s/--session", "opencode run", map[string][]string{"run": {"-s/--session"}}, "-s/--session"},
		{"session delete", "opencode session", map[string][]string{"session": {"delete"}}, "the delete subcommand"},
		{"session list", "opencode session", map[string][]string{"session": {"list"}}, "the list subcommand"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin, _ := fakeOpenCode{version: OpenCodeVerified2, cli: "2.0.32", strip: tc.strip}.install(t, t.TempDir())
			rs := Check(context.Background(), Options{OpenCode: true, OpenCodeBin: bin})
			fails := lines(rs, Fail)
			if len(fails) != 1 {
				t.Fatalf("want exactly one FAIL:\n%s", render(rs))
			}
			if fails[0].Surface != tc.surface || !strings.Contains(fails[0].Detail, tc.want) || !strings.Contains(fails[0].Detail, "does not list") {
				t.Errorf("the FAIL is %q, want it to name %s on %s", fails[0], tc.want, tc.surface)
			}
		})
	}
}

// TestOpenCodeTwoHelpIsReadInItsOwnForm: the real 2.0.32 help lists a flag as
// "--session, -s string", and a subcommand under SUBCOMMANDS as "delete" with its
// aliases after a comma. Neither is mistaken for the other thing: a flag only in
// prose, a subcommand only mentioned in a sentence or in another section, or a
// longer name that begins with it.
func TestOpenCodeTwoHelpIsReadInItsOwnForm(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "opencode-2.0.32-run.help"))
	if err != nil {
		t.Fatal(err)
	}
	have := helpFlags(string(raw))
	for _, f := range []string{"-s/--session", "--session", "-m/--model", "--format", "--title"} {
		if !have.has(f) {
			t.Errorf("the 2.0.32 run help does not give %s", f)
		}
	}
	if have.has("-x/--session") || have.has("--port") {
		t.Error("a flag that is not there was found")
	}
	sub, err := os.ReadFile(filepath.Join("testdata", "opencode-2.0.32-session.help"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := func(sub string) []string { return []string{"opencode", "session", sub} }
	for _, s := range []string{"list", "delete", "export", "import"} {
		if !listsCommand(string(sub), cmd(s)) {
			t.Errorf("the 2.0.32 session help does not list %s", s)
		}
	}
	if listsCommand(string(sub), cmd("destroy")) {
		t.Error("a subcommand that is not there was found")
	}
	for _, help := range []string{
		"SUBCOMMANDS\n  list    list sessions\n\nrun opencode session delete <id> to remove one\n",
		"SUBCOMMANDS\n  deleted    not the one\n",
		"FLAGS\n  --delete    not a subcommand\n\nSUBCOMMANDS\n  list    list sessions\n",
		"SUBCOMMANDS\n  list    list sessions\n\nGLOBAL FLAGS\n  delete    not under the subcommands\n",
	} {
		if listsCommand(help, cmd("delete")) {
			t.Errorf("counted as listing the command:\n%s", help)
		}
	}
	if !listsCommand("SUBCOMMANDS\n  upgrade, update    upgrade it\n", []string{"opencode", "update"}) {
		t.Error("an alias in the SUBCOMMANDS section was not found")
	}
}

// TestOpenCodeThreeIsAFAIL: the pump refuses a server that is neither 1.x nor 2.x,
// so the report says so on the version line, and does not read the CLI.
func TestOpenCodeThreeIsAFAIL(t *testing.T) {
	for _, version := range []string{"3.1.4", "opencode v3.0.0", "10.0.0"} {
		bin, log := fakeOpenCode{version: version}.install(t, t.TempDir())
		rs := Check(context.Background(), Options{OpenCode: true, OpenCodeBin: bin})
		if len(rs) != 2 || rs[0].Level != OK || rs[1].Level != Fail || rs[1].Surface != "opencode version" {
			t.Fatalf("%s: want found, then one version FAIL:\n%s", version, render(rs))
		}
		if !strings.Contains(rs[1].Detail, "later major version") || !strings.Contains(rs[1].Detail, OpenCodeInstallURL) {
			t.Errorf("%s: the FAIL is %q", version, rs[1])
		}
		if strings.Contains(rs[1].Detail, "not supported yet") || strings.Contains(rs[1].Detail, "\n") {
			t.Errorf("%s: the FAIL is the old refusal, or more than a line: %q", version, rs[1])
		}
		if !Failed(rs) {
			t.Errorf("%s: Failed is false", version)
		}
		if got, _ := os.ReadFile(log); strings.Contains(string(got), "--help") {
			t.Errorf("%s: the CLI's help was read though the version was refused:\n%s", version, got)
		}
	}
}

// TestOpenCodeVersionIsReadFromEitherPrintedForm: OpenCode 1 prints a bare
// version, and OpenCode 2 prints "opencode v2.0.32" (as of the 2.0.32 binary), so
// a 2.x install is read as the version it is, against the 2.x verified, and a 1.x
// in the same form as a 1.x.
func TestOpenCodeVersionIsReadFromEitherPrintedForm(t *testing.T) {
	for _, tc := range []struct {
		out, cli string
		level    Level
		want     string
	}{
		{"opencode v2.0.32", "2.0.32", OK, "the version last verified"},
		{"v2.0.32", "2.0.32", OK, "the version last verified"},
		{"opencode v2.1.0", "2.0.32", Warn, "newer than " + OpenCodeVerified2},
		{"opencode v1.18.30", "", OK, "the version last verified"},
		{"v1.19.3", "", Warn, "newer than " + OpenCodeVerified},
	} {
		bin, _ := fakeOpenCode{version: tc.out, cli: tc.cli}.install(t, t.TempDir())
		rs := Check(context.Background(), Options{OpenCode: true, OpenCodeBin: bin})
		r := find(t, rs, "opencode version")
		if r.Level != tc.level || !strings.Contains(r.Detail, tc.want) {
			t.Errorf("%q: got %q, want level %v with %q", tc.out, r, tc.level, tc.want)
		}
		if strings.Contains(r.Detail, "holds no version") {
			t.Errorf("%q was taken for an unreadable version: %q", tc.out, r)
		}
	}
	if got := OpenCodeVersion("opencode v2.0.32\n"); got != "2.0.32" {
		t.Errorf("OpenCodeVersion = %q", got)
	}
	if got := OpenCodeVersion("dev"); got != "" {
		t.Errorf("OpenCodeVersion of dev = %q", got)
	}
}

func TestOpenCodeVersionThatFailsIsAFAIL(t *testing.T) {
	bin, _ := fakeOpenCode{version: "boom", versionExit: 1}.install(t, t.TempDir())
	rs := Check(context.Background(), Options{OpenCode: true, OpenCodeBin: bin})
	if r := find(t, rs, "opencode version"); r.Level != Fail || !strings.Contains(r.Detail, "boom") {
		t.Errorf("an opencode whose --version fails: %q", r)
	}
}

func TestOpenCodeIsCheckedOnlyWhenChosen(t *testing.T) {
	bin, log := fakeOpenCode{version: OpenCodeVerified}.install(t, t.TempDir())
	claude := fakeVersioned(t, t.TempDir(), "claude", ClaudeVerified+" (Claude Code)", 0)
	for _, r := range Check(context.Background(), Options{Claude: true, ClaudeBin: claude, OpenCodeBin: bin}) {
		if strings.HasPrefix(r.Surface, "opencode") {
			t.Errorf("opencode was checked though it was not chosen: %q", r)
		}
	}
	if got, _ := os.ReadFile(log); len(got) != 0 {
		t.Errorf("opencode was run though it was not chosen:\n%s", got)
	}
}

// TestOpenCodeProbesEveryHelpItReads keeps the surfaces to commands the fake,
// and so the real 1.18.30 CLI, answers a --help for.
func TestOpenCodeProbesEveryHelpItReads(t *testing.T) {
	bin, log := fakeOpenCode{version: OpenCodeVerified}.install(t, t.TempDir())
	Check(context.Background(), Options{OpenCode: true, OpenCodeBin: bin})
	got, _ := os.ReadFile(log)
	for _, want := range []string{"--version\n", "--help\n", "run --help\n", "session --help\n", "session list --help\n"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("opencode was never run as %q:\n%s", strings.TrimSpace(want), got)
		}
	}
}
