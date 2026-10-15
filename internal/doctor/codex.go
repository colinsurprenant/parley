package doctor

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// codexRemedy and ergoRemedy are what a person does about a Codex or an ergo
// version that is not the one verified: the live check exercises both, and
// going back to the verified one is the way out of a table or a build that
// misbehaves.
const (
	codexRemedy = "To try it, run parley doctor --live; if a table or a build misbehaves, install the Codex CLI " + CodexVerified + " (" + CodexInstallURL + ")."
	ergoRemedy  = "To try it, run parley doctor --live; if a table or a build misbehaves, install ergo " + ErgoVerified + " (" + ergoVerifiedURL + ")."
)

// A surface is one codex subcommand parley depends on and the flags it uses
// there. A flag is spelled as parley spells it on the command line: "--json",
// or "-C/--cd" when parley uses a short form that has a long one, in which case
// both must be there.
type surface struct {
	cmd   []string
	flags []string
}

// codexSurfaces are the flags of the documented Codex commands parley uses or
// tells a human to use, read from the 0.161.0 help. exec is the live check's
// turn and, with resume, the form a session is started in; queue is how the
// codex front delivers; delete is how a throwaway or abandoned session is
// removed without a prompt.
var codexSurfaces = []surface{
	{[]string{"exec"}, []string{"--json", "-C/--cd", "-s/--sandbox", "-m/--model", "-c/--config", "--skip-git-repo-check"}},
	{[]string{"resume"}, []string{"-a/--ask-for-approval", "-C/--cd"}},
	{[]string{"queue"}, []string{"--thread", "--message"}},
	{[]string{"delete"}, []string{"--force"}},
}

// checkCodex is the static half of the Codex checks: the binary, its version,
// and the flags of the commands parley uses. The bool is whether the binary was
// found, which is what the live check needs.
func checkCodex(ctx context.Context, bin string) ([]Result, bool) {
	path, err := Resolve(bin)
	if err != nil {
		return []Result{{Fail, "codex", fmt.Sprintf("%v: install the Codex CLI (%s), or name it with --codex-bin", err, CodexInstallURL)}}, false
	}
	rs := []Result{
		{OK, "codex", "found " + path},
		checkVersion(ctx, "codex", path, CodexVerified, codexRemedy),
	}
	for _, s := range codexSurfaces {
		rs = append(rs, checkSurface(ctx, path, s))
	}
	return rs, true
}

func checkSurface(ctx context.Context, bin string, s surface) Result {
	name := "codex " + strings.Join(s.cmd, " ")
	help := append(append([]string(nil), s.cmd...), "--help")
	stdout, stderr, err := probe(ctx, bin, help...)
	if err != nil {
		return Result{Fail, name, fmt.Sprintf("%s --help: %s", name, describe(err, stdout, stderr))}
	}
	have := helpFlags(stdout)
	var missing []string
	for _, f := range s.flags {
		if !have.has(f) {
			missing = append(missing, f)
		}
	}
	if len(missing) > 0 {
		return Result{Fail, name, fmt.Sprintf("%s --help does not list %s", name, strings.Join(missing, ", "))}
	}
	return Result{OK, name, strings.Join(s.flags, " ")}
}

// optionLineRE is the head of an option's definition line in clap's help:
// indented, an optional short spelling, an optional long one. Prose that merely
// mentions a flag is not at the head of a line, so it is not taken for the flag.
var optionLineRE = regexp.MustCompile(`^\s+(-[A-Za-z0-9])?(?:, )?(--[A-Za-z0-9][A-Za-z0-9-]*)?(?:[\s=<\[]|$)`)

// flagSet is the option definitions in a help text, one entry per definition
// line, each holding the spellings that line gives.
type flagSet []map[string]bool

func helpFlags(help string) flagSet {
	var set flagSet
	for _, line := range strings.Split(help, "\n") {
		m := optionLineRE.FindStringSubmatch(line)
		if m == nil || (m[1] == "" && m[2] == "") {
			continue
		}
		def := map[string]bool{}
		for _, s := range m[1:] {
			if s != "" {
				def[s] = true
			}
		}
		set = append(set, def)
	}
	return set
}

// has reports whether one option definition gives every spelling of flag, which
// is "--json" or "-C/--cd".
func (set flagSet) has(flag string) bool {
	spellings := strings.Split(flag, "/")
	for _, def := range set {
		all := true
		for _, s := range spellings {
			if !def[s] {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}
