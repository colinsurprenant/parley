package doctor

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/colinsurprenant/parley/internal/codex"
)

// The links skills/install.sh makes, one per skill under each root: ~/.claude
// for Claude Code, ~/.agents for the agentskills.io convention Codex reads.
// TestSkillLinksAreTheOnesInstallMakes keeps these in step with the script.
var (
	claudeSkillRoot = []string{".claude", "skills"}
	codexSkillRoot  = []string{".agents", "skills"}
	skillRoots      = [][]string{claudeSkillRoot, codexSkillRoot}
	skillNames      = []string{"parley-build", "parley-table"}
)

// tableSkillName and buildSkillName are the one skill a table's seats, or a
// build's peers, are started on.
const (
	tableSkillName = "parley-table"
	buildSkillName = "parley-build"
)

// checkSkills checks each link install.sh makes: it exists, it resolves, and
// what it resolves to is a skill. A missing or dangling link is a failure — the
// seat that needs it would start without its protocol and nothing would say so.
// The links are the ones under the root of each harness chosen, both skills
// under each, and under both roots when neither is chosen.
func checkSkills(claude, codex bool) []Result {
	if !claude && !codex {
		return checkSkillLinks(skillRoots, skillNames)
	}
	var roots [][]string
	if claude {
		roots = append(roots, claudeSkillRoot)
	}
	if codex {
		roots = append(roots, codexSkillRoot)
	}
	return checkSkillLinks(roots, skillNames)
}

// checkTableSkill checks the parley-table link under the root each harness a
// table's seats run on reads: a table of Claude seats does not depend on the
// links Codex reads, nor one of Codex seats on Claude Code's, and no table
// depends on the parley-build skill.
func checkTableSkill(claude, codex bool) []Result {
	return checkHarnessSkill(tableSkillName, claude, codex)
}

// checkBuildSkill is checkTableSkill for a build's peers: the parley-build link
// under the root each harness reads, and not the parley-table one.
func checkBuildSkill(claude, codex bool) []Result {
	return checkHarnessSkill(buildSkillName, claude, codex)
}

func checkHarnessSkill(name string, claude, codex bool) []Result {
	var roots [][]string
	if claude {
		roots = append(roots, claudeSkillRoot)
	}
	if codex {
		roots = append(roots, codexSkillRoot)
	}
	return checkSkillLinks(roots, []string{name})
}

func checkSkillLinks(roots [][]string, names []string) []Result {
	if len(roots) == 0 {
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return []Result{{Fail, "skill", fmt.Sprintf("cannot locate the home directory the skills are linked under: %v", err)}}
	}
	var rs []Result
	for _, root := range roots {
		for _, name := range names {
			rs = append(rs, checkSkillLink(filepath.Join(home, filepath.Join(root...), name)))
		}
	}
	return rs
}

func checkSkillLink(path string) Result {
	const surface = "skill"
	fix := "run sh skills/install.sh from a parley checkout"
	li, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return Result{Fail, surface, fmt.Sprintf("%s is missing: %s", path, fix)}
	case err != nil:
		return Result{Fail, surface, fmt.Sprintf("%s: %v", path, err)}
	}
	target := path
	if li.Mode()&fs.ModeSymlink != 0 {
		raw, err := os.Readlink(path)
		if err != nil {
			return Result{Fail, surface, fmt.Sprintf("%s: %v", path, err)}
		}
		// A link's target is any bytes; none of them reaches the report as a
		// control character or a second line.
		to := codex.PlainLine(raw)
		if _, err := os.Stat(path); err != nil {
			return Result{Fail, surface, fmt.Sprintf("%s -> %s is dangling: %s", path, to, fix)}
		}
		target = fmt.Sprintf("%s -> %s", path, to)
	}
	if fi, err := os.Stat(filepath.Join(path, "SKILL.md")); err != nil || !fi.Mode().IsRegular() {
		return Result{Fail, surface, fmt.Sprintf("%s holds no SKILL.md file: %s", target, fix)}
	}
	return Result{OK, surface, target}
}
