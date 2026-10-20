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

// A surface is one subcommand of a harness's CLI that parley depends on, the
// flags it uses there and the subcommands it runs under it. A flag is spelled as
// parley spells it on the command line: "--json", or "-C/--cd" when parley uses
// a short form that has a long one, in which case both must be there. A
// subcommand is listed in the yargs help of OpenCode's CLI as an indented line
// that begins with the whole command, "opencode session delete <sessionID>".
type surface struct {
	// label, when it is not empty, is the surface's name in the report;
	// otherwise it is the tool and the subcommand, "codex exec". The default
	// command of a CLI has no subcommand to name it by.
	label string
	cmd   []string
	flags []string
	subs  []string
}

// codexSurfaces are the flags of the documented Codex commands parley uses or
// tells a human to use, read from the 0.161.0 help. exec is the live check's
// turn and, with resume, the form a session is started in; queue is how the
// codex front delivers; delete is how a throwaway or abandoned session is
// removed without a prompt.
var codexSurfaces = []surface{
	{cmd: []string{"exec"}, flags: []string{"--json", "-C/--cd", "-s/--sandbox", "-m/--model", "-c/--config", "--skip-git-repo-check"}},
	{cmd: []string{"resume"}, flags: []string{"-a/--ask-for-approval", "-C/--cd"}},
	{cmd: []string{"queue"}, flags: []string{"--thread", "--message"}},
	{cmd: []string{"delete"}, flags: []string{"--force"}},
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
		rs = append(rs, checkSurface(ctx, "codex", path, s))
	}
	return rs, true
}

// checkSurface reads bin's help for the surface and checks that it lists what
// parley uses there. A help text is read from both streams: clap prints it on
// stdout and OpenCode's yargs on stderr.
func checkSurface(ctx context.Context, tool, bin string, s surface) Result {
	help := append(append([]string(nil), s.cmd...), "--help")
	invoked := strings.Join(append([]string{tool}, help...), " ")
	name := s.label
	if name == "" {
		name = tool + " " + strings.Join(s.cmd, " ")
	}
	stdout, stderr, err := probe(ctx, bin, help...)
	if err != nil {
		return Result{Fail, name, fmt.Sprintf("%s: %s", invoked, describe(err, stdout, stderr))}
	}
	text := stdout + "\n" + stderr
	have := helpFlags(text)
	var missing []string
	for _, f := range s.flags {
		if !have.has(f) {
			missing = append(missing, f)
		}
	}
	for _, sub := range s.subs {
		if !listsCommand(text, append(append([]string{tool}, s.cmd...), sub)) {
			missing = append(missing, "the "+sub+" subcommand")
		}
	}
	if len(missing) > 0 {
		return Result{Fail, name, fmt.Sprintf("%s does not list %s", invoked, strings.Join(missing, ", "))}
	}
	listed := append([]string(nil), s.flags...)
	for _, sub := range s.subs {
		listed = append(listed, "subcommand "+sub)
	}
	return Result{OK, name, strings.Join(listed, " ")}
}

// listsCommand reports whether a yargs help text lists the command, spelled out
// whole, at the head of an indented line: the prose that mentions it is not
// the listing.
func listsCommand(help string, command []string) bool {
	re := regexp.MustCompile(`(?m)^\s+` + regexp.QuoteMeta(strings.Join(command, " ")) + `(?:\s|$)`)
	return re.MatchString(help) || listsSubcommand2(help, command[len(command)-1])
}

// listsSubcommand2 reports whether an OpenCode 2 help text lists sub in its
// SUBCOMMANDS section: an indented line whose first column, before two spaces or
// more, is the name, with aliases after a comma ("upgrade, update").
func listsSubcommand2(help, sub string) bool {
	in := false
	for _, line := range strings.Split(help, "\n") {
		switch {
		case strings.TrimSpace(line) == "SUBCOMMANDS":
			in = true
		case in && (line == "" || line[0] != ' '):
			in = false
		case in:
			names, _, _ := strings.Cut(strings.TrimSpace(line), "  ")
			for _, n := range strings.Split(names, ", ") {
				if n == sub {
					return true
				}
			}
		}
	}
	return false
}

// optionLineRE is the head of an option's definition line in clap's help:
// indented, an optional short spelling, an optional long one. Prose that merely
// mentions a flag is not at the head of a line, so it is not taken for the flag.
//
// OpenCode 2's help puts the short spelling after the long one, "--session, -s
// string", and the pattern takes either order.
var optionLineRE = regexp.MustCompile(`^\s+(-[A-Za-z0-9])?(?:, )?(--[A-Za-z0-9][A-Za-z0-9-]*)?(?:, (-[A-Za-z0-9]))?(?:[\s=<\[]|$)`)

// flagSet is the option definitions in a help text, one entry per definition
// line, each holding the spellings that line gives.
type flagSet []map[string]bool

func helpFlags(help string) flagSet {
	var set flagSet
	for _, line := range strings.Split(help, "\n") {
		m := optionLineRE.FindStringSubmatch(line)
		if m == nil || (m[1] == "" && m[2] == "" && m[3] == "") {
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
