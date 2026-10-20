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
	if len(rs) != 6 || len(lines(rs, OK)) != 6 {
		t.Fatalf("want six ok lines, three skills under each of two roots:\n%s", render(rs))
	}
	for _, name := range []string{"parley-build", "parley-table", "parley-line"} {
		n := 0
		for _, r := range rs {
			if strings.Contains(r.Detail, "/"+name+" ->") {
				n++
			}
		}
		if n != 2 {
			t.Errorf("%d ok lines for %s, want one under each root:\n%s", n, name, render(rs))
		}
	}
}

// TestSkillsFailsOnAMissingLineLink: the generic check covers parley-line like
// the other two, so a machine installed before the line skill existed is told
// to run install.sh again.
func TestSkillsFailsOnAMissingLineLink(t *testing.T) {
	home, _ := skillHome(t)
	gone := filepath.Join(home, ".claude", "skills", "parley-line")
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	fails := lines(Check(context.Background(), Options{Skills: true}), Fail)
	if len(fails) != 1 || fails[0].Surface != "skill" || !strings.Contains(fails[0].Detail, gone) || !strings.Contains(fails[0].Detail, "install.sh") {
		t.Fatalf("want one FAIL naming %s and install.sh, got:\n%s", gone, render(fails))
	}
}

// TestSkillsChecksEverySkillUnderTheRootOfEachChosenHarness: the doctor of a
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
	if skills != 3 {
		t.Errorf("%d skill lines, want all three skills under the Claude root only:\n%s", skills, render(rs))
	}
	// The Codex root is checked, and fails, only when Codex is chosen.
	codexBin, _ := fakeCodex{version: CodexVerified}.install(t, t.TempDir())
	rs = Check(context.Background(), Options{Codex: true, CodexBin: codexBin, Skills: true})
	fails := lines(rs, Fail)
	if len(fails) != 3 {
		t.Fatalf("want a FAIL for each of the three missing Codex-root links:\n%s", render(rs))
	}
	for _, r := range fails {
		if r.Surface != "skill" || !strings.Contains(r.Detail, codexRoot) {
			t.Errorf("unexpected FAIL for a Codex-only machine: %q", r)
		}
	}
	// With neither chosen, both roots, as before.
	if all := Check(context.Background(), Options{Skills: true}); len(lines(all, Fail)) != 3 || len(all) != 6 {
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
	// No harness chosen, no skill to check; Skills still checks all six.
	if rs := Check(context.Background(), Options{TableSkill: true}); len(rs) != 0 {
		t.Errorf("no harness chosen, yet:\n%s", render(rs))
	}
	if all := Check(context.Background(), Options{Skills: true}); len(all) != 6 {
		t.Errorf("Skills checks %d links, want all six:\n%s", len(all), render(all))
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

// TestLineSkillChecksOnlyTheLineLinkEachChosenHarnessReads: a line session of
// Claude Code needs ~/.claude/skills/parley-line, one of Codex
// ~/.agents/skills/parley-line, and neither needs the other two skills.
func TestLineSkillChecksOnlyTheLineLinkEachChosenHarnessReads(t *testing.T) {
	home, _ := skillHome(t)
	claudeLine := filepath.Join(home, ".claude", "skills", "parley-line")
	codexLine := filepath.Join(home, ".agents", "skills", "parley-line")
	for _, gone := range []string{
		filepath.Join(home, ".claude", "skills", "parley-table"),
		filepath.Join(home, ".agents", "skills", "parley-table"),
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
		{"claude sessions", true, false, []string{claudeLine}},
		{"codex sessions", false, true, []string{codexLine}},
		{"both", true, true, []string{claudeLine, codexLine}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rs := Check(context.Background(), Options{Claude: tc.claude, Codex: tc.codex, CodexBin: "no-such-codex", ClaudeBin: "no-such-claude", LineSkill: true})
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
	if err := os.Remove(codexLine); err != nil {
		t.Fatal(err)
	}
	claudeOnly := Check(context.Background(), Options{Claude: true, ClaudeBin: "no-such-claude", LineSkill: true})
	if got := lines(claudeOnly, Fail); len(got) != 1 || got[0].Surface != "claude" {
		t.Errorf("a Claude-only line failed on the Codex link:\n%s", render(claudeOnly))
	}
	both := Check(context.Background(), Options{Claude: true, Codex: true, ClaudeBin: "no-such-claude", CodexBin: "no-such-codex", LineSkill: true})
	var named bool
	for _, r := range lines(both, Fail) {
		named = named || (r.Surface == "skill" && strings.Contains(r.Detail, codexLine) && strings.Contains(r.Detail, "install.sh"))
	}
	if !named {
		t.Errorf("a line with a Codex session does not FAIL on the missing %s:\n%s", codexLine, render(both))
	}
	if rs := Check(context.Background(), Options{LineSkill: true}); len(rs) != 0 {
		t.Errorf("no harness chosen, yet:\n%s", render(rs))
	}
}

// skillLines are the skill lines of a report.
func skillLines(rs []Result) []Result {
	var out []Result
	for _, r := range rs {
		if r.Surface == "skill" {
			out = append(out, r)
		}
	}
	return out
}

// TestOpenCodeFindsASkillUnderEitherRoot: OpenCode 2 loads a SKILL.md
// from ~/.claude/skills and from ~/.agents/skills, so one valid link of the two
// is a skill it has, and a machine installed for one of the other harnesses is
// installed for it.
func TestOpenCodeFindsASkillUnderEitherRoot(t *testing.T) {
	home, skills := skillHome(t)
	opencode := Options{OpenCode: true, OpenCodeBin: "no-such-opencode", Skills: true}

	// Both roots are there: each skill is one line, the first root's.
	rs := skillLines(Check(context.Background(), opencode))
	if len(rs) != 3 || len(lines(rs, OK)) != 3 {
		t.Fatalf("want one ok line per skill:\n%s", render(rs))
	}
	for _, r := range rs {
		if !strings.Contains(r.Detail, filepath.Join(home, ".claude", "skills")) {
			t.Errorf("a skill line that is not the first root's: %q", r)
		}
	}

	// Only the Codex root, then only the Claude one: still three ok lines.
	for _, gone := range []string{".claude", ".agents"} {
		kept := map[string]string{".claude": ".agents", ".agents": ".claude"}[gone]
		bare := skillHomeWithout(t, skills, gone)
		rs := skillLines(Check(context.Background(), opencode))
		if len(rs) != 3 || len(lines(rs, OK)) != 3 {
			t.Fatalf("with only %s:\n%s", kept, render(rs))
		}
		for _, r := range rs {
			if !strings.Contains(r.Detail, filepath.Join(bare, kept, "skills")) {
				t.Errorf("with only %s, a skill line elsewhere: %q", kept, r)
			}
		}
	}
}

// skillHomeWithout is a home with every link under the other root and none
// under root, set as $HOME.
func skillHomeWithout(t *testing.T, skills, root string) string {
	t.Helper()
	home := t.TempDir()
	for _, r := range skillRoots {
		if r[0] == root {
			continue
		}
		for _, name := range skillNames {
			link := filepath.Join(home, filepath.Join(r...), name)
			if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(skills, name), link); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Setenv("HOME", home)
	return home
}

// TestOpenCodeSkillIsAFAILOnlyWhenNeitherRootHasIt: a link that is gone, dangling
// or not a skill under one root is no failure while the other root's is sound,
// and when neither is, the one FAIL names both and says OpenCode needs only one.
func TestOpenCodeSkillIsAFAILOnlyWhenNeitherRootHasIt(t *testing.T) {
	home, skills := skillHome(t)
	opencode := Options{OpenCode: true, OpenCodeBin: "no-such-opencode", Skills: true}
	claudeBuild := filepath.Join(home, ".claude", "skills", "parley-build")
	codexBuild := filepath.Join(home, ".agents", "skills", "parley-build")

	// Missing under the first root.
	if err := os.Remove(claudeBuild); err != nil {
		t.Fatal(err)
	}
	rs := skillLines(Check(context.Background(), opencode))
	if len(rs) != 3 || len(lines(rs, OK)) != 3 {
		t.Fatalf("a link missing under one root only:\n%s", render(rs))
	}
	var viaCodexRoot bool
	for _, r := range rs {
		viaCodexRoot = viaCodexRoot || strings.Contains(r.Detail, codexBuild)
	}
	if !viaCodexRoot {
		t.Errorf("parley-build was not found under the Codex root:\n%s", render(rs))
	}

	// Dangling under the second, missing under the first: neither.
	if err := os.RemoveAll(filepath.Join(skills, "parley-build")); err != nil {
		t.Fatal(err)
	}
	rs = skillLines(Check(context.Background(), opencode))
	fails := lines(rs, Fail)
	if len(rs) != 3 || len(fails) != 1 || len(lines(rs, OK)) != 2 {
		t.Fatalf("want two ok lines and one FAIL:\n%s", render(rs))
	}
	for _, want := range []string{"OpenCode reads skills from either root", "parley-build is under neither", claudeBuild + " is missing",
		codexBuild + " ->", "is dangling", "run sh skills/install.sh from a parley checkout"} {
		if !strings.Contains(fails[0].Detail, want) {
			t.Errorf("the FAIL does not say %q: %q", want, fails[0])
		}
	}
	// The fix is said once, at the end.
	if n := strings.Count(fails[0].Detail, "install.sh"); n != 1 {
		t.Errorf("the FAIL says to run install.sh %d times: %q", n, fails[0])
	}
}

// TestOpenCodeSkillChecksOnlyTheSkillAnEntryPointNeeds: a line of OpenCode
// sessions needs the parley-line skill and nothing else, as every other harness
// does.
func TestOpenCodeSkillChecksOnlyTheSkillAnEntryPointNeeds(t *testing.T) {
	home, _ := skillHome(t)
	for _, name := range []string{"parley-table", "parley-build"} {
		for _, root := range []string{".claude", ".agents"} {
			if err := os.Remove(filepath.Join(home, root, "skills", name)); err != nil {
				t.Fatal(err)
			}
		}
	}
	for field, opts := range map[string]Options{
		"LineSkill":  {OpenCode: true, OpenCodeBin: "no-such-opencode", LineSkill: true},
		"Skills":     {OpenCode: true, OpenCodeBin: "no-such-opencode", Skills: true},
		"TableSkill": {OpenCode: true, OpenCodeBin: "no-such-opencode", TableSkill: true},
	} {
		rs := skillLines(Check(context.Background(), opts))
		wantFails := map[string]int{"LineSkill": 0, "Skills": 2, "TableSkill": 1}[field]
		if got := len(lines(rs, Fail)); got != wantFails {
			t.Errorf("%s: %d FAIL lines, want %d:\n%s", field, got, wantFails, render(rs))
		}
	}
}

// TestSkillLinesTwoHarnessesShareAreOneLine: Claude Code and OpenCode both read
// ~/.claude/skills, and a link both read is not reported twice; a harness's own
// root still FAILs for it when its link is gone.
func TestSkillLinesTwoHarnessesShareAreOneLine(t *testing.T) {
	home, _ := skillHome(t)
	all := Options{Claude: true, Codex: true, OpenCode: true, ClaudeBin: "no-such-claude", CodexBin: "no-such-codex", OpenCodeBin: "no-such-opencode", Skills: true}
	if rs := skillLines(Check(context.Background(), all)); len(rs) != 6 || len(lines(rs, OK)) != 6 {
		t.Fatalf("want six ok lines for three skills under two roots, whoever reads them:\n%s", render(rs))
	}

	// Claude Code's link is gone: it FAILs, and OpenCode is served by the other.
	gone := filepath.Join(home, ".claude", "skills", "parley-line")
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	rs := skillLines(Check(context.Background(), all))
	fails := lines(rs, Fail)
	if len(fails) != 1 || !strings.Contains(fails[0].Detail, gone) || strings.Contains(fails[0].Detail, "OpenCode") {
		t.Fatalf("want one FAIL, Claude Code's, naming %s:\n%s", gone, render(rs))
	}
	if len(rs) != 6 {
		t.Errorf("%d skill lines, want six (five ok, one FAIL):\n%s", len(rs), render(rs))
	}

	// OpenCode alone does not fail for it.
	if fails := lines(skillLines(Check(context.Background(), Options{OpenCode: true, OpenCodeBin: "no-such-opencode", Skills: true})), Fail); len(fails) != 0 {
		t.Errorf("OpenCode failed with the Codex root intact:\n%s", render(fails))
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
