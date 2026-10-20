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
// script. Its help texts are the real 2.0.32 ones in testdata, optionally stripped
// of a flag or a subcommand, and are printed on stdout.
type fakeOpenCode struct {
	version string
	// strip lists flags (as openCodeSurfaces spells them) and subcommands, as
	// "delete", the help of the named command must not list; the TUI's is "tui".
	strip map[string][]string
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
	for _, name := range []string{"tui", "run", "session"} {
		raw, err := os.ReadFile(filepath.Join("testdata", "opencode-"+OpenCodeVerified+"-"+name+".help"))
		if err != nil {
			t.Fatal(err)
		}
		text := stripFlags(string(raw), f.strip[name])
		for _, sub := range f.strip[name] {
			if !strings.HasPrefix(sub, "-") {
				text = stripLines(text, sub+" ")
			}
		}
		if err := os.WriteFile(filepath.Join(helpDir, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %[1]q
case "$1 $2" in
"--version "*) echo "%[2]s"; exit %[3]d ;;
"--help ") cat %[4]q/tui; exit 0 ;;
"run --help" | "session --help") cat %[4]q/"$1"; exit 0 ;;
esac
echo "fake opencode: unexpected arguments: $*" >&2
exit 64
`, log, f.version, f.versionExit, helpDir)
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
	bin, log := fakeOpenCode{version: OpenCodeVerified}.install(t, t.TempDir())
	rs := Check(context.Background(), Options{OpenCode: true, OpenCodeBin: bin})
	if len(rs) != 5 || len(lines(rs, OK)) != 5 {
		t.Fatalf("want five ok lines:\n%s", render(rs))
	}
	if r := find(t, rs, "opencode version"); !strings.Contains(r.Detail, "the version last verified") || !strings.Contains(r.Detail, OpenCodeVerified) {
		t.Errorf("the version line is %q", r)
	}
	// What each line says it checked is what parley uses.
	for surface, want := range map[string]string{
		"opencode tui":     "-s/--session",
		"opencode run":     "--format -m/--model --title -s/--session",
		"opencode session": "subcommand delete subcommand list",
	} {
		if got := find(t, rs, surface).Detail; got != want {
			t.Errorf("%s says %q, want %q", surface, got, want)
		}
	}
	got, _ := os.ReadFile(log)
	for _, want := range []string{"--version\n", "--help\n", "run --help\n", "session --help\n"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("opencode was never run as %q:\n%s", strings.TrimSpace(want), got)
		}
	}
}

func TestOpenCodeMissingAFlagOrSubcommandIsAFAILNamingIt(t *testing.T) {
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

// TestOpenCodeHelpIsReadInItsForm: the real 2.0.32 help lists a flag as
// "--session, -s string", and a subcommand under SUBCOMMANDS as "delete" with its
// aliases after a comma. Neither is mistaken for the other thing: a flag only in
// prose, a subcommand only mentioned in a sentence or in another section, or a
// longer name that begins with it.
func TestOpenCodeHelpIsReadInItsForm(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "opencode-"+OpenCodeVerified+"-run.help"))
	if err != nil {
		t.Fatal(err)
	}
	have := helpFlags(string(raw))
	for _, f := range []string{"-s/--session", "--session", "-m/--model", "--format", "--title"} {
		if !have.has(f) {
			t.Errorf("the run help does not give %s", f)
		}
	}
	if have.has("-x/--session") || have.has("--port") {
		t.Error("a flag that is not there was found")
	}
	sub, err := os.ReadFile(filepath.Join("testdata", "opencode-"+OpenCodeVerified+"-session.help"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"list", "delete", "export", "import"} {
		if !listsSubcommand(string(sub), s) {
			t.Errorf("the session help does not list %s", s)
		}
	}
	if listsSubcommand(string(sub), "destroy") {
		t.Error("a subcommand that is not there was found")
	}
	for _, help := range []string{
		"SUBCOMMANDS\n  list    list sessions\n\nrun opencode session delete <id> to remove one\n",
		"SUBCOMMANDS\n  deleted    not the one\n",
		"FLAGS\n  --delete    not a subcommand\n\nSUBCOMMANDS\n  list    list sessions\n",
		"SUBCOMMANDS\n  list    list sessions\n\nGLOBAL FLAGS\n  delete    not under the subcommands\n",
	} {
		if listsSubcommand(help, "delete") {
			t.Errorf("counted as listing the command:\n%s", help)
		}
	}
	if !listsSubcommand("SUBCOMMANDS\n  upgrade, update    upgrade it\n", "update") {
		t.Error("an alias in the SUBCOMMANDS section was not found")
	}
}

func TestOpenCodeMissingBinaryIsOneFAILNamingHowToInstallIt(t *testing.T) {
	rs := Check(context.Background(), Options{OpenCode: true, OpenCodeBin: filepath.Join(t.TempDir(), "no-opencode")})
	if len(rs) != 1 || rs[0].Level != Fail || rs[0].Surface != "opencode" {
		t.Fatalf("want one opencode FAIL:\n%s", render(rs))
	}
	for _, want := range []string{"install OpenCode 2 (" + OpenCodeInstallURL + ")", "--opencode-bin"} {
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

// TestOpenCodeOtherThanTheVerifiedOneWarns: a different 2.x is a warning with the
// way out, the way out being the 2.x that was verified.
func TestOpenCodeOtherThanTheVerifiedOneWarns(t *testing.T) {
	for _, tc := range []struct{ out, want string }{
		{"2.1.1", "newer than " + OpenCodeVerified},
		{"opencode v2.9.0", "newer than " + OpenCodeVerified},
		{"2.0.3", "older than " + OpenCodeVerified},
		{OpenCodeVerified + "-rc.1", "different than " + OpenCodeVerified},
	} {
		bin, _ := fakeOpenCode{version: tc.out}.install(t, t.TempDir())
		rs := Check(context.Background(), Options{OpenCode: true, OpenCodeBin: bin})
		r := find(t, rs, "opencode version")
		if r.Level != Warn || !strings.Contains(r.Detail, tc.want) {
			t.Errorf("%q: got %q, want a warning with %q", tc.out, r, tc.want)
		}
		for _, want := range []string{"It may work as it is", "Parley was " + OpenCodeVerifiedNote, "a newer 2.x is expected to work", "simulation mode"} {
			if !strings.Contains(r.Detail, want) {
				t.Errorf("%q: %q does not say %q", tc.out, r, want)
			}
		}
		// A different 2.x is not a reason to install an exact other version.
		if strings.Contains(r.Detail, "install OpenCode "+OpenCodeVerified) {
			t.Errorf("%q: the remedy tells the person to install an exact version: %q", tc.out, r)
		}
		if Failed(rs) {
			t.Errorf("%q: a 2.x other than the verified one failed the check:\n%s", tc.out, render(rs))
		}
		find(t, rs, "opencode session")
	}
}

// TestOpenCodeOtherThanTwoIsAFAILWithTheWayOut: the pump refuses a server that is
// not a 2, so the report says so on the version line, and does not read the CLI. A
// 1.x is told where OpenCode 2 comes from.
func TestOpenCodeOtherThanTwoIsAFAILWithTheWayOut(t *testing.T) {
	for _, tc := range []struct{ version, want string }{
		{"1.18.30", "the legacy 1.x"},
		{"opencode v1.19.3", "the legacy 1.x"},
		{"3.1.4", "not a major version parley speaks to"},
		{"opencode v3.0.0", "not a major version parley speaks to"},
		{"10.0.0", "not a major version parley speaks to"},
		{"0.9.0", "not a major version parley speaks to"},
	} {
		bin, log := fakeOpenCode{version: tc.version}.install(t, t.TempDir())
		rs := Check(context.Background(), Options{OpenCode: true, OpenCodeBin: bin})
		if len(rs) != 2 || rs[0].Level != OK || rs[1].Level != Fail || rs[1].Surface != "opencode version" {
			t.Fatalf("%s: want found, then one version FAIL:\n%s", tc.version, render(rs))
		}
		for _, want := range []string{tc.want, "brew install opencode", "@opencode/cli"} {
			if !strings.Contains(rs[1].Detail, want) {
				t.Errorf("%s: the FAIL does not say %q: %q", tc.version, want, rs[1])
			}
		}
		if strings.Contains(rs[1].Detail, "\n") {
			t.Errorf("%s: the FAIL is more than a line: %q", tc.version, rs[1])
		}
		if !Failed(rs) {
			t.Errorf("%s: Failed is false", tc.version)
		}
		if got, _ := os.ReadFile(log); strings.Contains(string(got), "--help") {
			t.Errorf("%s: the CLI's help was read though the version was refused:\n%s", tc.version, got)
		}
	}
}

// TestOpenCodeVersionIsReadFromEitherPrintedForm: OpenCode 2 prints "opencode
// v2.0.32" (as of the 2.0.32 binary), and a bare version is read as the version
// it is.
func TestOpenCodeVersionIsReadFromEitherPrintedForm(t *testing.T) {
	for _, tc := range []struct {
		out   string
		level Level
		want  string
	}{
		{"opencode v" + OpenCodeVerified, OK, "the version last verified"},
		{"v" + OpenCodeVerified, OK, "the version last verified"},
		{OpenCodeVerified, OK, "the version last verified"},
		{"opencode v2.9.0", Warn, "newer than " + OpenCodeVerified},
		{"opencode", Warn, "holds no version"},
	} {
		bin, _ := fakeOpenCode{version: tc.out}.install(t, t.TempDir())
		rs := Check(context.Background(), Options{OpenCode: true, OpenCodeBin: bin})
		r := find(t, rs, "opencode version")
		if r.Level != tc.level || !strings.Contains(r.Detail, tc.want) {
			t.Errorf("%q: got %q, want level %v with %q", tc.out, r, tc.level, tc.want)
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
