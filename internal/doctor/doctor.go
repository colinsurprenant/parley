// Package doctor checks that every external surface parley depends on still
// works with this machine's current versions: the Codex CLI and the Claude Code
// CLI parley drives, the ergo server it talks to, and the skills that tell a
// session how to behave. Codex moves fast between releases, so the point
// is to notice a surface drifting before a table finds out mid-run.
//
// Check is the whole API. A caller picks the surfaces: `parley doctor` asks for
// all of them, and `parley table new` and `parley build new` ask for the
// harnesses their seats or peers run, the server (and ergo, when the server is not
// up) and their skill under the roots those harnesses read, with Live off. Without Live a check runs no model
// turn and writes nothing outside the operating system's temporary directory;
// with it, one Codex turn is spent and a throwaway ergo is run.
package doctor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/colinsurprenant/parley/internal/codex"
)

// The versions parley was last verified against, live, by a person: the Codex
// CLI and ergo by `parley doctor --live`, Claude Code at a table (a live table
// ran end to end on 2.1.296). A different version is not a failure: the report
// says it may work and what to do if a table or a build misbehaves; to the
// maintainer it is the cue to verify again, and then to move the constant.
// ErgoVerified is the ergo the embedded configuration
// (internal/ergoconf/ircd.yaml) was written for.
const (
	CodexVerified  = "0.161.0"
	ClaudeVerified = "2.1.296"
	ErgoVerified   = "2.19.1"
)

// Where a person installs each of them, each the project's own install page
// (ergo's is the one its README sends people to). A report that finds a binary
// missing, or at a version other than the verified one, links the page.
const (
	ClaudeInstallURL = "https://code.claude.com/docs/en/setup"
	CodexInstallURL  = "https://learn.chatgpt.com/docs/codex/cli"
	ErgoInstallURL   = "https://github.com/ergochat/ergo/releases/latest"
)

// ergoVerifiedURL is the release page of the ergo that was verified.
const ergoVerifiedURL = "https://github.com/ergochat/ergo/releases/tag/v" + ErgoVerified

// ErgoInstallHint is what a person does about an ergo that is not installed. It
// is the one wording of it: `parley doctor` and `parley server` both say it.
const ErgoInstallHint = "install ergo from " + ErgoInstallURL + " (contrib/ergo/README.md in a parley checkout has the steps), or name it with --ergo-bin"

// The default names the harnesses are resolved by, on PATH.
const (
	DefaultCodexBin  = "codex"
	DefaultClaudeBin = "claude"
	DefaultErgoBin   = "ergo"
)

// Level is how a check came out.
type Level int

const (
	// OK means the surface is as parley needs it.
	OK Level = iota
	// Warn means parley may still work but a person should look: a version that
	// is not the one verified, a server that is not up.
	Warn
	// Fail means something parley depends on is not there or does not work.
	Fail
)

func (l Level) String() string {
	switch l {
	case OK:
		return "ok"
	case Warn:
		return "warn"
	}
	return "FAIL"
}

// Result is one line of the report: how a check came out, the surface it is
// about, and the detail a person needs to act on it.
type Result struct {
	Level   Level
	Surface string
	Detail  string
}

// String is the report line: the level, the surface, the detail.
func (r Result) String() string {
	return fmt.Sprintf("%-4s %-14s %s", r.Level, r.Surface, r.Detail)
}

// Failed reports whether any check failed, which is what makes the exit status
// of `parley doctor` 1.
func Failed(rs []Result) bool {
	for _, r := range rs {
		if r.Level == Fail {
			return true
		}
	}
	return false
}

// Options picks the surfaces to check.
type Options struct {
	// Codex and Claude say which harnesses to check; each is its binary, its
	// version against the verified one and, for Codex, every flag parley uses.
	Codex, Claude bool
	// CodexBin and ClaudeBin are the binaries as they are spelled and resolved
	// on PATH; empty means the default name.
	CodexBin, ClaudeBin string
	// ErgoBin, when not empty, adds the check that ergo is installed.
	ErgoBin string
	// Server, when not empty, is the loopback host:port to ask for its IRC
	// capabilities. A server that is not listening is a warning: it may simply
	// not be started yet.
	Server string
	// Skills adds the check that the skill links install.sh makes exist and
	// resolve: all three skills under the root each harness chosen with Claude
	// and Codex reads, and under both roots when neither is chosen.
	Skills bool
	// TableSkill adds the narrower check a table needs: the parley-table skill
	// under the root each harness chosen with Claude and Codex reads (Claude Code
	// ~/.claude/skills, Codex ~/.agents/skills), and nothing else. Skills, when it
	// is set too, covers all of it.
	TableSkill bool
	// BuildSkill is TableSkill for a build: the parley-build skill under the root
	// each harness chosen with Claude and Codex reads, and nothing else. Skills,
	// when it is set too, covers all of it.
	BuildSkill bool
	// LineSkill is TableSkill for a line: the parley-line skill under the root
	// each harness chosen with Claude and Codex reads, and nothing else. Skills,
	// when it is set too, covers all of it.
	LineSkill bool
	// Live adds what costs something: for Codex one model turn, for ergo (when
	// ErgoBin is set) a throwaway server and a message through it.
	Live bool
}

// Check runs the checks Options asks for and returns one Result per check, in
// a stable order: codex, claude, ergo, the server, the skills, then what Live
// adds. A check that cannot run because its binary is missing is that
// binary's failure, reported once.
func Check(ctx context.Context, o Options) []Result {
	var rs []Result
	codexOK, ergoOK := false, false
	if o.Codex {
		var cr []Result
		cr, codexOK = checkCodex(ctx, orDefault(o.CodexBin, DefaultCodexBin))
		rs = append(rs, cr...)
	}
	if o.Claude {
		rs = append(rs, checkClaude(ctx, orDefault(o.ClaudeBin, DefaultClaudeBin))...)
	}
	if o.ErgoBin != "" {
		var er []Result
		er, ergoOK = checkErgo(ctx, o.ErgoBin)
		rs = append(rs, er...)
	}
	if o.Server != "" {
		rs = append(rs, checkServer(ctx, o.Server))
	}
	switch {
	case o.Skills:
		rs = append(rs, checkSkills(o.Claude, o.Codex)...)
	case o.TableSkill:
		rs = append(rs, checkTableSkill(o.Claude, o.Codex)...)
	case o.BuildSkill:
		rs = append(rs, checkBuildSkill(o.Claude, o.Codex)...)
	case o.LineSkill:
		rs = append(rs, checkLineSkill(o.Claude, o.Codex)...)
	}
	if o.Live {
		if codexOK {
			rs = append(rs, liveCodex(ctx, orDefault(o.CodexBin, DefaultCodexBin))...)
		}
		if ergoOK {
			rs = append(rs, liveErgo(ctx, o.ErgoBin)...)
		}
	}
	return rs
}

// Resolve finds bin the way a shell would — on PATH unless it holds a separator
// — and returns the absolute path. A probe or a server runs in a directory of
// its own, and a relative path such as ./ergo would be looked for there.
func Resolve(bin string) (string, error) {
	path, err := exec.LookPath(bin)
	if err != nil {
		return "", err
	}
	return filepath.Abs(path)
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// probeTimeout bounds a probe of a binary — its --version, its --help — which
// is a matter of milliseconds when the binary is sound.
const probeTimeout = 20 * time.Second

// probe runs bin with args and returns what it wrote to stdout and to stderr.
func probe(ctx context.Context, bin string, args ...string) (stdout, stderr string, err error) {
	return probeFor(ctx, probeTimeout, bin, args...)
}

// probeFor is probe with its own bound. A probe runs where nothing belongs to
// anyone — the system temp directory — with no stdin, so one that unexpectedly
// asks a question gets end-of-file rather than a person's terminal.
// maxProbeOutput bounds what a version or help probe keeps of each stream: the
// largest help text parley reads (claude --help) is a few tens of KiB.
const maxProbeOutput = 1 << 20

func probeFor(ctx context.Context, d time.Duration, bin string, args ...string) (stdout, stderr string, err error) {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = os.TempDir()
	// A broken binary may talk without end until the timeout; what is kept of
	// it is capped, as on every other child-process path.
	so, se := &limitBuffer{limit: maxProbeOutput}, &limitBuffer{limit: maxProbeOutput}
	cmd.Stdout, cmd.Stderr = so, se
	// A descendant that inherited the pipes must not hold the report hostage.
	cmd.WaitDelay = 2 * time.Second
	err = cmd.Run()
	return so.String(), se.String(), err
}

// describe is how a failed probe reads in a report line: the exit status, and
// the first line the binary said about it.
func describe(err error, stdout, stderr string) string {
	msg := err.Error()
	for _, s := range []string{stderr, stdout} {
		if line, _, _ := strings.Cut(strings.TrimSpace(s), "\n"); line != "" {
			return msg + ": " + codex.PlainLine(line)
		}
	}
	return msg
}

var versionRE = regexp.MustCompile(`\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?`)

// checkVersion is the version check the harnesses and ergo share: bin's
// --version against the one verified. A difference is a warning that says the
// surface may work as it is; remedy is what a person does about it, written as a
// sentence of its own, and is what turns a table or a build that misbehaves
// into something they can act on.
func checkVersion(ctx context.Context, name, bin, verified, remedy string) Result {
	surface := name + " version"
	stdout, stderr, err := probe(ctx, bin, "--version")
	if err != nil {
		return Result{Fail, surface, fmt.Sprintf("%s --version: %s", name, describe(err, stdout, stderr))}
	}
	have := versionRE.FindString(stdout)
	if have == "" {
		return Result{Warn, surface, fmt.Sprintf("%s --version printed %q, which holds no version; the last verified is %s. It may work as it is. %s", name, strings.TrimSpace(stdout), verified, remedy)}
	}
	if have == verified {
		return Result{OK, surface, fmt.Sprintf("%s, the version last verified", have)}
	}
	return Result{Warn, surface, fmt.Sprintf("%s is %s than %s, the version last verified. It may work as it is. %s", have, newerOrOlder(have, verified), verified, remedy)}
}

// newerOrOlder says how have compares with verified: "newer" or "older" by the
// numeric parts, and "different" when those are equal and only a suffix differs.
func newerOrOlder(have, verified string) string {
	h, v := numericParts(have), numericParts(verified)
	for i := range h {
		switch {
		case h[i] > v[i]:
			return "newer"
		case h[i] < v[i]:
			return "older"
		}
	}
	return "different"
}

func numericParts(version string) [3]int {
	core, _, _ := strings.Cut(version, "-")
	var n [3]int
	for i, p := range strings.SplitN(core, ".", 3) {
		n[i], _ = strconv.Atoi(p)
	}
	return n
}
