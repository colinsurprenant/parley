// Package doctor checks that every external surface parley depends on still
// works with this machine's current versions: the Codex CLI, the Claude Code
// CLI and the OpenCode CLI parley drives, the ergo server it talks to, and the
// skills that tell a session how to behave. Codex moves fast between releases, so the point
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
	"syscall"
	"time"

	"github.com/colinsurprenant/parley/internal/codex"
)

// The versions parley was last verified against, live, by a person: the Codex
// CLI and ergo by `parley doctor --live`, Claude Code at a table (a live table
// ran end to end on 2.1.296), and OpenCode 2 on a live model (delivery, queueing,
// steering, cancelling, a service restart, the reply path and the header byte for
// byte). Each is a floor within its major version, not an exact match: a newer
// version of the same major version is ok, and the report names both. A version
// older than the one verified is a warning that it may lack something parley
// uses, and one of a different major version (0 is a major version like any
// other) a warning that it may not work. So is one equal in number that differs
// only in its suffix, such as a prerelease, and one that cannot be read. A
// warning says what to do if a table, a build or a line misbehaves. Codex's real
// compatibility check is `parley doctor --live`. An OpenCode of a major version other than 2 is not a
// warning but a failure, which parley does not speak to (checkOpenCode, and the
// pump's own refusal), so the different-major warning never meets an OpenCode.
// To the maintainer a different major version, or a release that changes
// something parley uses, is the cue to verify again, and then to move the
// constant (and the help fixtures under testdata, named for the version they
// were read from).
// ErgoVerified is the ergo the embedded configuration (internal/ergoconf/ircd.yaml)
// was written for.
const (
	CodexVerified    = "0.161.0"
	ClaudeVerified   = "2.1.296"
	OpenCodeVerified = "2.1.0"
	ErgoVerified     = "2.19.1"
)

// OpenCodeVerifiedNote is the one phrase that says what OpenCode parley was last
// verified against, for the usage text, the runbook and the doctor's remedy; the
// docs say it in the same words, and a test holds them to OpenCodeVerified.
const OpenCodeVerifiedNote = "last verified against OpenCode " + OpenCodeVerified + " on a live model"

// Where a person installs each of them, each the project's own install page
// (ergo's is the one its README sends people to). A report that finds a binary
// missing, or at a version it warns about, links the page.
const (
	ClaudeInstallURL   = "https://code.claude.com/docs/en/setup"
	CodexInstallURL    = "https://learn.chatgpt.com/docs/codex/cli"
	OpenCodeInstallURL = "https://opencode.ai/docs/"
	ErgoInstallURL     = "https://github.com/ergochat/ergo/releases/latest"
)

// ergoVerifiedURL is the release page of the ergo that was verified.
const ergoVerifiedURL = "https://github.com/ergochat/ergo/releases/tag/v" + ErgoVerified

// ErgoInstallHint is what a person does about an ergo that is not installed. It
// is the one wording of it: `parley doctor` and `parley server` both say it.
const ErgoInstallHint = "install ergo from " + ErgoInstallURL + " (contrib/ergo/README.md in a parley checkout has the steps), or name it with --ergo-bin"

// The default names the harnesses are resolved by, on PATH.
const (
	DefaultCodexBin    = "codex"
	DefaultClaudeBin   = "claude"
	DefaultOpenCodeBin = "opencode"
	DefaultErgoBin     = "ergo"
)

// Level is how a check came out.
type Level int

const (
	// OK means the surface is as parley needs it.
	OK Level = iota
	// Warn means parley may still work but a person should look: a version older
	// than the one verified or of another major version, a server that is not up.
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
	return fmt.Sprintf("%-4s %-16s %s", r.Level, r.Surface, r.Detail)
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
	// Codex, Claude and OpenCode say which harnesses to check; each is its
	// binary, its version against the verified one and, for Codex and OpenCode,
	// every flag and subcommand parley uses.
	Codex, Claude, OpenCode bool
	// CodexBin, ClaudeBin and OpenCodeBin are the binaries as they are spelled
	// and resolved on PATH; empty means the default name.
	CodexBin, ClaudeBin, OpenCodeBin string
	// ErgoBin, when not empty, adds the check that ergo is installed.
	ErgoBin string
	// Server, when not empty, is the loopback host:port to ask for its IRC
	// capabilities. A server that is not listening is a warning: it may simply
	// not be started yet.
	Server string
	// Skills adds the check that the skill links install.sh makes exist and
	// resolve: all three skills under the root each harness chosen with Claude,
	// Codex and OpenCode reads (OpenCode reads both, and one valid link of the
	// two is enough), and under both roots when none is chosen.
	Skills bool
	// TableSkill adds the narrower check a table needs: the parley-table skill
	// under the root each harness chosen with Claude, Codex and OpenCode reads
	// (Claude Code ~/.claude/skills, Codex ~/.agents/skills, OpenCode either),
	// and nothing else. Skills, when it is set too, covers all of it.
	TableSkill bool
	// BuildSkill is TableSkill for a build: the parley-build skill under the root
	// each harness chosen reads, and nothing else. Skills, when it is set too,
	// covers all of it.
	BuildSkill bool
	// LineSkill is TableSkill for a line: the parley-line skill under the root
	// each harness chosen reads, and nothing else. Skills, when it is set too,
	// covers all of it.
	LineSkill bool
	// Live adds what costs something: for Codex one model turn, for ergo (when
	// ErgoBin is set) a throwaway server and a message through it.
	Live bool
}

// Check runs the checks Options asks for and returns one Result per check, in
// a stable order: codex, claude, opencode, ergo, the server, the skills, then
// what Live adds. A check that cannot run because its binary is missing is that
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
	if o.OpenCode {
		rs = append(rs, checkOpenCode(ctx, orDefault(o.OpenCodeBin, DefaultOpenCodeBin))...)
	}
	if o.ErgoBin != "" {
		var er []Result
		er, ergoOK = checkErgo(ctx, o.ErgoBin)
		rs = append(rs, er...)
	}
	if o.Server != "" {
		rs = append(rs, checkServer(ctx, o.Server))
	}
	readers := o.skillReaders()
	switch {
	case o.Skills:
		rs = append(rs, checkSkills(readers)...)
	case o.TableSkill:
		rs = append(rs, checkHarnessSkill(tableSkillName, readers)...)
	case o.BuildSkill:
		rs = append(rs, checkHarnessSkill(buildSkillName, readers)...)
	case o.LineSkill:
		rs = append(rs, checkHarnessSkill(lineSkillName, readers)...)
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
	ownGroup(cmd)
	// A descendant that inherited the pipes must not hold the report hostage.
	cmd.WaitDelay = 2 * time.Second
	err = cmd.Run()
	if err != nil {
		killGroup(cmd)
	}
	return so.String(), se.String(), err
}

// ownGroup makes the probe a process group of its own, which a timeout takes
// whole. A harness's CLI may be a launcher that runs the program as a child, as
// an npm install's is, and the default cancellation of CommandContext kills the
// launcher alone and leaves the child that hangs on --version or --help. When the
// wait delay expires Run returns without Cancel having run, which is why a probe
// that failed is followed by killGroup.
func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
}

// killGroup takes whatever is left of the process group of a probe that ended
// badly: the launcher may have exited while a child still held the pipes, and a
// group lives as long as it has a member. It is called after the command has been
// waited for, so the group is the probe's own or empty.
func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
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
// --version against the one verified, by the rule compareVersion states. remedy
// is what a person does about a version that warns, written as a sentence of its
// own, and is what turns a table, a build or a line that misbehaves into
// something they can act on.
func checkVersion(ctx context.Context, name, bin, verified, remedy string) Result {
	surface := name + " version"
	stdout, stderr, err := probe(ctx, bin, "--version")
	if err != nil {
		return Result{Fail, surface, fmt.Sprintf("%s --version: %s", name, describe(err, stdout, stderr))}
	}
	return compareVersion(name, stdout, verified, remedy)
}

// compareVersion is checkVersion's verdict on what --version printed. The
// verified version is a floor within its major version:
//
//   - the same version is ok;
//   - a newer one of the same major version is ok, and the line names both;
//   - an older one of the same major version is a warning that it may lack
//     something parley uses;
//   - one of a different major version, newer or older and 0 like any other, is a
//     warning that it may not work;
//   - one equal in number that differs in its suffix (a prerelease) is a warning;
//   - none found is a warning.
//
// A warning carries remedy.
func compareVersion(name, stdout, verified, remedy string) Result {
	surface := name + " version"
	have := versionRE.FindString(stdout)
	if have == "" {
		return Result{Warn, surface, fmt.Sprintf("%s --version printed %q, which holds no version; the last verified is %s. It may work as it is. %s", name, strings.TrimSpace(stdout), verified, remedy)}
	}
	if have == verified {
		return Result{OK, surface, fmt.Sprintf("%s, the version last verified", have)}
	}
	if numericParts(have)[0] != numericParts(verified)[0] {
		return Result{Warn, surface, fmt.Sprintf("%s is a different major version from %s, the version last verified, and may not work. %s", have, verified, remedy)}
	}
	switch newerOrOlder(have, verified) {
	case "newer":
		return Result{OK, surface, fmt.Sprintf("%s, newer than %s, the version last verified", have, verified)}
	case "older":
		return Result{Warn, surface, fmt.Sprintf("%s is older than %s, the version last verified, and may lack something parley uses. %s", have, verified, remedy)}
	}
	return Result{Warn, surface, fmt.Sprintf("%s is different than %s, the version last verified. It may work as it is. %s", have, verified, remedy)}
}

// orNewer names a verified version and the releases of its major version that
// are expected to work with it: "2.1.296 or a newer 2.x", the way the remedies
// say what to install.
func orNewer(verified string) string {
	return fmt.Sprintf("%s or a newer %d.x", verified, numericParts(verified)[0])
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
