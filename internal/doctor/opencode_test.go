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
// and are printed on stderr, where yargs prints them.
type fakeOpenCode struct {
	version string
	// strip lists flags (as openCodeSurfaces spells them) and subcommands, as
	// "delete", the help of the named command must not list; the TUI's is "tui".
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
	for _, name := range []string{"tui", "run", "session"} {
		raw, err := os.ReadFile(filepath.Join("testdata", "opencode-1.18.30-"+name+".help"))
		if err != nil {
			t.Fatal(err)
		}
		text := stripFlags(string(raw), f.strip[name])
		for _, sub := range f.strip[name] {
			if !strings.HasPrefix(sub, "-") {
				text = stripLines(text, "opencode "+name+" "+sub)
			}
		}
		if err := os.WriteFile(filepath.Join(helpDir, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	redirect := ">&2"
	if f.helpOnStdout {
		redirect = ""
	}
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %[1]q
case "$1 $2" in
"--version "*) echo "%[2]s"; exit %[3]d ;;
"--help ") cat %[4]q/tui %[5]s; exit 0 ;;
"run --help" | "session --help") cat %[4]q/"$1" %[5]s; exit 0 ;;
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
	if len(rs) != 5 || len(lines(rs, OK)) != 5 {
		t.Fatalf("want five ok lines:\n%s", render(rs))
	}
	for _, surface := range []string{"opencode", "opencode version", "opencode tui", "opencode run", "opencode session"} {
		find(t, rs, surface)
	}
	// What each line says it checked is what parley uses.
	for surface, want := range map[string]string{
		"opencode tui":     "--port -s/--session",
		"opencode run":     "--format -m/--model --title",
		"opencode session": "subcommand delete",
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

// TestOpenCodeTwoIsAFAILWithTheWayOut: the front speaks OpenCode 1's HTTP API,
// so a 2.x is refused, on its version line alone: its CLI is not read, since a
// different one would only add noise to the one remedy.
func TestOpenCodeTwoIsAFAILWithTheWayOut(t *testing.T) {
	for _, version := range []string{"2.0.32", "2.0.0", "3.1.4", "10.0.0"} {
		bin, log := fakeOpenCode{version: version}.install(t, t.TempDir())
		rs := Check(context.Background(), Options{OpenCode: true, OpenCodeBin: bin})
		if len(rs) != 2 || rs[0].Level != OK || rs[1].Level != Fail || rs[1].Surface != "opencode version" {
			t.Fatalf("%s: want found, then one version FAIL:\n%s", version, render(rs))
		}
		const want = "OpenCode 2 is not supported yet: parley's front speaks OpenCode 1's HTTP API (prompt_async); install opencode 1.x, e.g. npm i -g opencode-ai@1"
		if !strings.Contains(rs[1].Detail, want) || !strings.Contains(rs[1].Detail, version) {
			t.Errorf("%s: the FAIL does not say %q and name the version: %q", version, want, rs[1])
		}
		if !Failed(rs) {
			t.Errorf("%s: Failed is false", version)
		}
		if got, _ := os.ReadFile(log); strings.Contains(string(got), "--help") {
			t.Errorf("%s: the CLI's help was read though the version was refused:\n%s", version, got)
		}
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
	for _, want := range []string{"--version\n", "--help\n", "run --help\n", "session --help\n"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("opencode was never run as %q:\n%s", strings.TrimSpace(want), got)
		}
	}
}
