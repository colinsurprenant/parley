package doctor

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// skillHome builds a home directory with every skill link install.sh makes,
// pointing at skill directories that hold a SKILL.md, and sets $HOME to it.
func skillHome(t *testing.T) (home, skills string) {
	t.Helper()
	home = t.TempDir()
	skills = filepath.Join(t.TempDir(), "skills")
	for _, name := range skillNames {
		dir := filepath.Join(skills, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: "+name+"\n---\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, root := range skillRoots {
			link := filepath.Join(home, filepath.Join(root...), name)
			if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(dir, link); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Setenv("HOME", home)
	return home, skills
}

func TestSkillLinksThatResolveAreOK(t *testing.T) {
	skillHome(t)
	rs := Check(context.Background(), Options{Skills: true})
	if len(rs) != 4 || len(lines(rs, OK)) != 4 {
		t.Fatalf("want four ok lines:\n%s", render(rs))
	}
}

// TestSkillsChecksBothSkillsUnderTheRootOfEachChosenHarness: the doctor of a
// machine with one harness is not failed by the other's missing links.
func TestSkillsChecksBothSkillsUnderTheRootOfEachChosenHarness(t *testing.T) {
	home, _ := skillHome(t)
	claudeRoot := filepath.Join(home, ".claude", "skills")
	codexRoot := filepath.Join(home, ".agents", "skills")
	if err := os.RemoveAll(codexRoot); err != nil {
		t.Fatal(err)
	}
	bin := fakeVersioned(t, t.TempDir(), "claude", ClaudeVerified+" (Claude Code)", 0)
	rs := Check(context.Background(), Options{Claude: true, ClaudeBin: bin, Skills: true})
	skills := 0
	for _, r := range rs {
		if r.Surface != "skill" {
			continue
		}
		skills++
		if r.Level != OK || !strings.Contains(r.Detail, claudeRoot) {
			t.Errorf("a skill line for a Claude-only machine: %q", r)
		}
	}
	if skills != 2 {
		t.Errorf("%d skill lines, want both skills under the Claude root only:\n%s", skills, render(rs))
	}
	// The Codex root is checked, and fails, only when Codex is chosen.
	codexBin, _ := fakeCodex{version: CodexVerified}.install(t, t.TempDir())
	rs = Check(context.Background(), Options{Codex: true, CodexBin: codexBin, Skills: true})
	fails := lines(rs, Fail)
	if len(fails) != 2 {
		t.Fatalf("want a FAIL for each of the two missing Codex-root links:\n%s", render(rs))
	}
	for _, r := range fails {
		if r.Surface != "skill" || !strings.Contains(r.Detail, codexRoot) {
			t.Errorf("unexpected FAIL for a Codex-only machine: %q", r)
		}
	}
	// With neither chosen, both roots, as before.
	if all := Check(context.Background(), Options{Skills: true}); len(lines(all, Fail)) != 2 || len(all) != 4 {
		t.Errorf("Skills alone checks both roots:\n%s", render(Check(context.Background(), Options{Skills: true})))
	}
}

// TestTableSkillChecksOnlyTheLinkEachChosenHarnessReads: a table of Claude seats
// needs ~/.claude/skills/parley-table, one of Codex seats ~/.agents/skills/
// parley-table, and neither needs the parley-build skill or the other root.
func TestTableSkillChecksOnlyTheLinkEachChosenHarnessReads(t *testing.T) {
	home, _ := skillHome(t)
	claudeTable := filepath.Join(home, ".claude", "skills", "parley-table")
	codexTable := filepath.Join(home, ".agents", "skills", "parley-table")
	// Everything the table does not read is gone.
	for _, gone := range []string{
		filepath.Join(home, ".claude", "skills", "parley-build"),
		filepath.Join(home, ".agents", "skills", "parley-build"),
	} {
		if err := os.Remove(gone); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name          string
		claude, codex bool
		wantOK        []string
	}{
		{"claude seats", true, false, []string{claudeTable}},
		{"codex seats", false, true, []string{codexTable}},
		{"both", true, true, []string{claudeTable, codexTable}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rs := Check(context.Background(), Options{Claude: tc.claude, Codex: tc.codex, CodexBin: "no-such-codex", ClaudeBin: "no-such-claude", TableSkill: true})
			var skills []Result
			for _, r := range rs {
				if r.Surface == "skill" {
					skills = append(skills, r)
				}
			}
			if len(skills) != len(tc.wantOK) {
				t.Fatalf("want %d skill lines, got:\n%s", len(tc.wantOK), render(skills))
			}
			for i, want := range tc.wantOK {
				if skills[i].Level != OK || !strings.Contains(skills[i].Detail, want) {
					t.Errorf("skill line %d: %q, want ok for %s", i, skills[i], want)
				}
			}
		})
	}

	// A missing link under a root a chosen harness reads is a FAIL naming it, and
	// one under the other root is not.
	if err := os.Remove(codexTable); err != nil {
		t.Fatal(err)
	}
	claudeOnly := Check(context.Background(), Options{Claude: true, ClaudeBin: "no-such-claude", TableSkill: true})
	if got := lines(claudeOnly, Fail); len(got) != 1 || got[0].Surface != "claude" {
		t.Errorf("a Claude-only table failed on the Codex link:\n%s", render(claudeOnly))
	}
	both := Check(context.Background(), Options{Claude: true, Codex: true, ClaudeBin: "no-such-claude", CodexBin: "no-such-codex", TableSkill: true})
	var named bool
	for _, r := range lines(both, Fail) {
		named = named || (r.Surface == "skill" && strings.Contains(r.Detail, codexTable) && strings.Contains(r.Detail, "install.sh"))
	}
	if !named {
		t.Errorf("a table with a Codex seat does not FAIL on the missing %s:\n%s", codexTable, render(both))
	}
	// No harness chosen, no skill to check; Skills still checks all four.
	if rs := Check(context.Background(), Options{TableSkill: true}); len(rs) != 0 {
		t.Errorf("no harness chosen, yet:\n%s", render(rs))
	}
	if all := Check(context.Background(), Options{Skills: true}); len(all) != 4 {
		t.Errorf("Skills checks %d links, want all four:\n%s", len(all), render(all))
	}
}

// TestBuildSkillChecksOnlyTheBuildLinkEachChosenHarnessReads: a build of Claude
// peers needs ~/.claude/skills/parley-build, one with a Codex peer also
// ~/.agents/skills/parley-build, and neither needs the parley-table skill.
func TestBuildSkillChecksOnlyTheBuildLinkEachChosenHarnessReads(t *testing.T) {
	home, _ := skillHome(t)
	claudeBuild := filepath.Join(home, ".claude", "skills", "parley-build")
	codexBuild := filepath.Join(home, ".agents", "skills", "parley-build")
	for _, gone := range []string{
		filepath.Join(home, ".claude", "skills", "parley-table"),
		filepath.Join(home, ".agents", "skills", "parley-table"),
	} {
		if err := os.Remove(gone); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name          string
		claude, codex bool
		wantOK        []string
	}{
		{"claude peers", true, false, []string{claudeBuild}},
		{"codex peers", false, true, []string{codexBuild}},
		{"both", true, true, []string{claudeBuild, codexBuild}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rs := Check(context.Background(), Options{Claude: tc.claude, Codex: tc.codex, CodexBin: "no-such-codex", ClaudeBin: "no-such-claude", BuildSkill: true})
			var skills []Result
			for _, r := range rs {
				if r.Surface == "skill" {
					skills = append(skills, r)
				}
			}
			if len(skills) != len(tc.wantOK) {
				t.Fatalf("want %d skill lines, got:\n%s", len(tc.wantOK), render(skills))
			}
			for i, want := range tc.wantOK {
				if skills[i].Level != OK || !strings.Contains(skills[i].Detail, want) {
					t.Errorf("skill line %d: %q, want ok for %s", i, skills[i], want)
				}
			}
		})
	}
	if err := os.Remove(codexBuild); err != nil {
		t.Fatal(err)
	}
	claudeOnly := Check(context.Background(), Options{Claude: true, ClaudeBin: "no-such-claude", BuildSkill: true})
	if got := lines(claudeOnly, Fail); len(got) != 1 || got[0].Surface != "claude" {
		t.Errorf("a Claude-only build failed on the Codex link:\n%s", render(claudeOnly))
	}
	both := Check(context.Background(), Options{Claude: true, Codex: true, ClaudeBin: "no-such-claude", CodexBin: "no-such-codex", BuildSkill: true})
	var named bool
	for _, r := range lines(both, Fail) {
		named = named || (r.Surface == "skill" && strings.Contains(r.Detail, codexBuild) && strings.Contains(r.Detail, "install.sh"))
	}
	if !named {
		t.Errorf("a build with a Codex peer does not FAIL on the missing %s:\n%s", codexBuild, render(both))
	}
	if rs := Check(context.Background(), Options{BuildSkill: true}); len(rs) != 0 {
		t.Errorf("no harness chosen, yet:\n%s", render(rs))
	}
}

func TestAMissingSkillLinkIsAFAILNamingIt(t *testing.T) {
	home, _ := skillHome(t)
	gone := filepath.Join(home, ".agents", "skills", "parley-table")
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	rs := Check(context.Background(), Options{Skills: true})
	fails := lines(rs, Fail)
	if len(fails) != 1 || !strings.Contains(fails[0].Detail, gone) || !strings.Contains(fails[0].Detail, "install.sh") {
		t.Fatalf("want one FAIL naming %s and install.sh:\n%s", gone, render(rs))
	}
}

func TestADanglingSkillLinkIsAFAIL(t *testing.T) {
	home, skills := skillHome(t)
	if err := os.RemoveAll(filepath.Join(skills, "parley-build")); err != nil {
		t.Fatal(err)
	}
	rs := Check(context.Background(), Options{Skills: true})
	fails := lines(rs, Fail)
	if len(fails) != 2 {
		t.Fatalf("want a FAIL for each root's dangling parley-build link:\n%s", render(rs))
	}
	for _, f := range fails {
		if !strings.Contains(f.Detail, "dangling") || !strings.Contains(f.Detail, filepath.Join(home, ".claude")) && !strings.Contains(f.Detail, filepath.Join(home, ".agents")) {
			t.Errorf("a dangling link reported as %q", f)
		}
	}
}

func TestALinkToSomethingThatIsNotASkillIsAFAIL(t *testing.T) {
	_, skills := skillHome(t)
	if err := os.Remove(filepath.Join(skills, "parley-build", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	fails := lines(Check(context.Background(), Options{Skills: true}), Fail)
	if len(fails) != 2 || !strings.Contains(fails[0].Detail, "no SKILL.md") {
		t.Fatalf("got %v", fails)
	}
}

func TestASKILLmdThatIsNotAFileIsAFAIL(t *testing.T) {
	_, skills := skillHome(t)
	path := filepath.Join(skills, "parley-build", "SKILL.md")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	fails := lines(Check(context.Background(), Options{Skills: true}), Fail)
	if len(fails) != 2 || !strings.Contains(fails[0].Detail, "no SKILL.md file") {
		t.Fatalf("a directory named SKILL.md passed as a skill file: %v", fails)
	}
}

func TestASkillLinkTargetReachesTheReportPlain(t *testing.T) {
	home, skills := skillHome(t)
	odd := filepath.Join(skills, "par\x1b[2Jley\nok   forged")
	if err := os.Rename(filepath.Join(skills, "parley-build"), odd); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{".claude", ".agents"} {
		link := filepath.Join(home, root, "skills", "parley-build")
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(odd, link); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range Check(context.Background(), Options{Skills: true}) {
		if strings.ContainsAny(r.Detail, "\x1b\n") {
			t.Errorf("a link target reached the report raw: %q", r)
		}
	}
}

func TestADirectoryInPlaceOfALinkIsFineIfItIsASkill(t *testing.T) {
	home, _ := skillHome(t)
	path := filepath.Join(home, ".claude", "skills", "parley-build")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := lines(Check(context.Background(), Options{Skills: true}), Fail); len(got) != 1 {
		t.Fatalf("an empty directory where a skill belongs should fail once, got %v", got)
	}
	if err := os.WriteFile(filepath.Join(path, "SKILL.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := lines(Check(context.Background(), Options{Skills: true}), Fail); len(got) != 0 {
		t.Fatalf("a directory holding a SKILL.md failed: %v", got)
	}
}

// TestSkillLinksAreTheOnesInstallMakes keeps the links this package checks in
// step with the script that makes them.
func TestSkillLinksAreTheOnesInstallMakes(t *testing.T) {
	script, err := os.ReadFile(filepath.Join("..", "..", "skills", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(script)
	names := regexp.MustCompile(`(?m)^skill_names="([^"]*)"$`).FindStringSubmatch(src)
	if names == nil {
		t.Fatal("install.sh no longer sets skill_names on one line")
	}
	if got, want := strings.Join(skillNames, " "), names[1]; got != want {
		t.Errorf("doctor checks the skills %q, install.sh links %q", got, want)
	}
	for _, root := range skillRoots {
		want := "$HOME/" + strings.Join(root, "/")
		if !strings.Contains(src, "="+want+"\n") {
			t.Errorf("install.sh no longer sets a skills root %s", want)
		}
	}
	if got := len(regexp.MustCompile(`(?m)^\w+_skills=\$HOME/`).FindAllString(src, -1)); got != len(skillRoots) {
		t.Errorf("install.sh has %d skills roots, doctor checks %d", got, len(skillRoots))
	}
}
