package doctor

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/colinsurprenant/parley/internal/codex"
)

// The links skills/install.sh makes, one per skill under each root: ~/.claude
// for Claude Code, ~/.agents for the agentskills.io convention Codex reads.
// OpenCode 2 loads a SKILL.md from both. TestSkillLinksAreTheOnesInstallMakes
// keeps these in step with the script.
var (
	claudeSkillRoot = []string{".claude", "skills"}
	codexSkillRoot  = []string{".agents", "skills"}
	skillRoots      = [][]string{claudeSkillRoot, codexSkillRoot}
	skillNames      = []string{"parley-build", "parley-table", "parley-line"}
)

// A skillReader is how a harness finds a skill: it finds one when a link under
// any of the roots it reads holds a skill. Claude Code and Codex read one root
// each; OpenCode reads both, so a machine installed for either of the others
// serves it.
type skillReader struct {
	harness string
	roots   [][]string
}

var (
	claudeSkills   = skillReader{"Claude Code", [][]string{claudeSkillRoot}}
	codexSkills    = skillReader{"Codex", [][]string{codexSkillRoot}}
	openCodeSkills = skillReader{"OpenCode", skillRoots}
)

// skillReaders are the readers of the harnesses o chooses, in the order the
// report lists them.
func (o Options) skillReaders() []skillReader {
	var rs []skillReader
	if o.Claude {
		rs = append(rs, claudeSkills)
	}
	if o.Codex {
		rs = append(rs, codexSkills)
	}
	if o.OpenCode {
		rs = append(rs, openCodeSkills)
	}
	return rs
}

// tableSkillName, buildSkillName and lineSkillName are the one skill a table's
// seats, a build's peers, or a line's sessions are started on.
const (
	tableSkillName = "parley-table"
	buildSkillName = "parley-build"
	lineSkillName  = "parley-line"
)

// checkSkills checks each link install.sh makes: it exists, it resolves, and
// what it resolves to is a skill. A missing or dangling link is a failure — the
// seat that needs it would start without its protocol and nothing would say so.
// The links are the ones each harness chosen reads, all three skills, and every
// link under both roots when none is chosen.
func checkSkills(readers []skillReader) []Result {
	if len(readers) == 0 {
		readers = []skillReader{claudeSkills, codexSkills}
	}
	return checkReaderSkills(readers, skillNames)
}

// checkHarnessSkill checks the one skill a table, a build or a line needs under
// the root each harness chosen reads: a table of Claude seats does not depend on
// the links Codex reads, nor one of Codex seats on Claude Code's, and no table
// depends on the parley-build skill. With no harness chosen there is nothing to
// check.
func checkHarnessSkill(name string, readers []skillReader) []Result {
	return checkReaderSkills(readers, []string{name})
}

// checkReaderSkills checks each name for each reader, in that order, and says
// nothing twice: a link two harnesses read is one line.
func checkReaderSkills(readers []skillReader, names []string) []Result {
	if len(readers) == 0 {
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return []Result{{Fail, "skill", fmt.Sprintf("cannot locate the home directory the skills are linked under: %v", err)}}
	}
	var rs []Result
	seen := map[Result]bool{}
	for _, r := range readers {
		for _, name := range names {
			if res := r.check(home, name); !seen[res] {
				seen[res] = true
				rs = append(rs, res)
			}
		}
	}
	return rs
}

// check is the reader's verdict on one skill: the first root whose link holds
// it. A reader of one root reports that root's link as it is; one of several
// that finds none fails once, naming what is wrong under each root and saying
// it needs only one, so that a link missing under one root does not read as one
// that must be there.
func (r skillReader) check(home, name string) Result {
	var problems []string
	for _, root := range r.roots {
		res := checkSkillLink(filepath.Join(home, filepath.Join(root...), name))
		if res.Level == OK || len(r.roots) == 1 {
			return res
		}
		problems = append(problems, strings.TrimSuffix(res.Detail, ": "+skillFix))
	}
	return Result{Fail, "skill", fmt.Sprintf("%s reads skills from either root, and %s is under neither: %s: %s",
		r.harness, name, strings.Join(problems, "; "), skillFix)}
}

// skillFix is what a person does about a link that is not there.
const skillFix = "run sh skills/install.sh from a parley checkout"

func checkSkillLink(path string) Result {
	const surface = "skill"
	fix := skillFix
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
