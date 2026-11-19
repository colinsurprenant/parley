package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/colinsurprenant/parley/internal/primer"
	"github.com/colinsurprenant/parley/internal/state"
)

// cmdLineSet changes the breaker of a line that is running: its cap, its window,
// or both. It rewrites the launch files the record lists and the record's numbers,
// and prints, for each session, what is left for the human, because the breaker
// lives in each session's front process and reads its numbers only when that
// process starts. It touches no server and dials nothing, and it never posts: a
// line's channel is the chair's and the sessions'. A launch file in a working
// directory is rewritten only on the proof `line drop` removes one on (its bytes
// still hash to the recorded digest), atomically and never through a link, and
// every file is judged before the first is written.
func cmdLineSet(args []string) error {
	fs := flag.NewFlagSet("parley line set", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), lineUsageSet)
		fs.PrintDefaults()
	}
	capN := fs.Int("cap", 0, "the breaker's new cap, as for line new: the most posts by anyone but you that may arrive within --window before every front holds the next until you post; 0 turns the breaker off; at least one of --cap and --window is required, and the one left out keeps its recorded value")
	window := fs.Duration("window", 0, "the breaker's new window, a Go duration above zero such as 10m or 90s")
	stateDir := fs.String("state-dir", "", "the state directory the line was set up under (default $XDG_STATE_HOME/parley)")
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return &configError{err: err}
	}
	switch {
	case len(pos) == 0:
		return cfgErrf("the line id is required: parley line set <id> --cap <n> [--window <duration>]")
	case len(pos) > 1:
		return cfgErrf("unexpected argument %q", pos[1])
	}
	id := pos[0]
	if err := checkLineID(id); err != nil {
		return err
	}
	capGiven, windowGiven := given(fs, "cap"), given(fs, "window")
	if !capGiven && !windowGiven {
		return cfgErrf("--cap or --window is required: the breaker's new cap, its new window, or both")
	}
	// Both are held to what `line new` holds them to.
	if capGiven && *capN < 0 {
		return cfgErrf("--cap must not be negative")
	}
	if windowGiven {
		if err := checkWindow(fs, primer.SkillLine, *window); err != nil {
			return err
		}
	}
	base, err := state.ResolveBase(*stateDir)
	if err != nil {
		return cfgErrf("--state-dir: %v", err)
	}
	// A line that is not there is the invocation's mistake and takes no lock.
	if !lineDirIsThere(base, id) {
		return noLineErr(base, id)
	}

	ctx, stop := signalContext()
	defer stop()
	// The id's lock comes before the record is read, and is held to the end: the
	// record read below is the one that is changed, and no other verb for the id
	// runs meanwhile.
	unlock, err := lockID(base, id)
	if err != nil {
		return err
	}
	defer unlock()
	rec, _, err := readLineRecord(base, id)
	if err != nil {
		return err
	}
	newCap, newWindow := rec.Cap, rec.Window
	if capGiven {
		newCap = *capN
	}
	if windowGiven {
		newWindow = lineWindowFlag(*window)
	}
	if newCap == rec.Cap && newWindow == rec.Window {
		_, err := fmt.Fprintf(os.Stdout, "Line %s already has cap %d and window %s: nothing was changed.\n", id, rec.Cap, rec.Window)
		return err
	}

	self := selfPath()
	plan, err := planLineSet(rec, base, self, newCap, newWindow)
	if err != nil {
		return err
	}
	dir := lineDirOf(base, id)
	signal.Ignore(syscall.SIGPIPE)
	if _, err := io.WriteString(os.Stdout, plan.notes()); err != nil {
		return fmt.Errorf("could not write to standard output (%v); nothing was changed", err)
	}
	if err := plan.apply(ctx, dir); err != nil {
		return err
	}
	if _, err := io.WriteString(os.Stdout, plan.report(filepath.Join(dir, lineRecordName))); err != nil {
		return fmt.Errorf("line %s is changed (cap %d, window %s) but what is left for you could not be written: %v", id, newCap, newWindow, err)
	}
	return nil
}

// setAction is what `line set` does with one launch file.
type setAction int

const (
	// setRewrite: the file is the line's by the recorded digest and carries the
	// numbers, so it is written again.
	setRewrite setAction = iota
	// setHolds: the file already holds the bytes that would be written, which a run
	// cut short after it wrote the file and before it wrote the record leaves, under
	// the digest the record still has. Nothing is written; the digest is recorded.
	setHolds
	// setUntouched: the file is the line's and carries neither number: a Codex
	// config is a constant body behind a nonce.
	setUntouched
	// setGone: nothing is at the path.
	setGone
)

// lineSetFile is one launch file of the line as `line set` judges it. Sessions of
// one working directory and harness share a file, and are one entry.
type lineSetFile struct {
	path string
	// owners are the indexes, in the record, of the sessions that list the file.
	owners []int
	act    setAction
	// data and digest are what the file is written as: set for a Claude session's
	// .mcp.json, empty for a file that carries neither number.
	data   []byte
	digest string
	// command is the parley binary the file runs now, read from the bytes the
	// digest proved are the line's, for a file that is written again; "" when it
	// could not be read.
	command string
}

// lineSetPlan is everything `line set` has judged before it changes anything.
type lineSetPlan struct {
	base, self string
	old        lineRecord
	// next is old with the new numbers; apply gives it the new digests too.
	next  lineRecord
	files []*lineSetFile
}

// planLineSet judges, writing nothing, every launch file the record lists: each
// must be the line's by the proof `line drop` uses (claimLaunchFile), or the
// whole run is refused, naming the file. A file that is not there is left that way.
// The Claude launch files are regenerated for the new numbers, by the function
// `line add` writes them with, from the record and from this binary's path.
func planLineSet(rec lineRecord, base, self string, newCap int, newWindow string) (*lineSetPlan, error) {
	p := &lineSetPlan{base: base, self: self, old: rec, next: rec}
	p.next.Cap, p.next.Window = newCap, newWindow
	p.next.Sessions = make([]lineSession, len(rec.Sessions))
	for i, s := range rec.Sessions {
		s.Files = slices.Clone(s.Files)
		p.next.Sessions[i] = s
	}

	claude := false
	byPath := map[string]*lineSetFile{}
	for i, s := range rec.Sessions {
		claude = claude || s.Harness == harnessClaude
		if !hasLaunchFile(s.Harness) {
			continue
		}
		f := s.Files[0]
		lf, ok := byPath[f.Path]
		if !ok {
			lf = &lineSetFile{path: f.Path}
			byPath[f.Path] = lf
			p.files = append(p.files, lf)
		} else if first := rec.Sessions[lf.owners[0]]; first.Files[0].SHA256 != f.SHA256 {
			return nil, fmt.Errorf("will not rewrite %s: the record holds one digest for it under session %s and another under session %s, though they share the file, so it is not shown to be the file line add wrote; nothing was changed", f.Path, first.Nick, s.Nick)
		}
		lf.owners = append(lf.owners, i)
	}

	// What is regenerated and printed names this binary and the state directory;
	// they are held to what `line add` holds them to.
	if len(rec.Sessions) > 0 {
		if !filepath.IsAbs(self) {
			return nil, cfgErrf("cannot find this binary's absolute path (got %q): the .mcp.json and every command printed name it", self)
		}
		if err := checkUTF8(pathField{"the state directory (--state-dir, or $XDG_STATE_HOME, or $HOME)", base}, pathField{"this binary's path", self}); err != nil {
			return nil, err
		}
		if err := checkPrintedPaths(claude, base, self); err != nil {
			return nil, err
		}
	}
	for _, s := range rec.Sessions {
		if s.Harness == harnessClaude {
			if err := measureLineClaude(p.next, s.Nick, base, "--cap, --window, the state directory or the path of the parley binary"); err != nil {
				return nil, err
			}
		}
	}

	for _, lf := range p.files {
		owner := rec.Sessions[lf.owners[0]]
		f := owner.Files[0]
		if owner.Harness == harnessClaude {
			data, err := lineMCPJSON(self, p.next, base)
			if err != nil {
				return nil, err
			}
			lf.data, lf.digest = data, tableDigest(data)
		}
		act, problem, err := claimLaunchFile(owner, f)
		if err != nil {
			return nil, err
		}
		holds := false
		if problem != "" && lf.data != nil {
			// A run of this verb that was cut short after it wrote the file leaves the
			// bytes it would write now, under the digest the record still holds. They
			// are what is wanted, so there is nothing to lose by writing them again.
			a2, p2, err2 := claimLaunchFile(owner, lineFile{Path: f.Path, SHA256: lf.digest})
			if err2 == nil && p2 == "" && a2 == fileRemove {
				holds, problem = true, ""
			}
		}
		if problem != "" {
			return nil, fmt.Errorf("will not rewrite %s: %s; it is not shown to be the file line add wrote for session %s, so look at it, then run this again; nothing was changed", f.Path, problem, owner.Nick)
		}
		switch {
		case act == fileGone:
			lf.act = setGone
		case holds:
			lf.act = setHolds
		case lf.data == nil:
			lf.act = setUntouched
		default:
			lf.act = setRewrite
			lf.command = launchCommand(f)
		}
	}
	return p, nil
}

// launchCommand is the command of the parley server in the Claude launch file f,
// read from its bytes only if they hash to the recorded digest, so that what is
// parsed is what `line add` wrote; "" when they do not, or cannot be read.
func launchCommand(f lineFile) string {
	b, err := readLaunchFile(f.Path)
	if err != nil || tableDigest(b) != f.SHA256 {
		return ""
	}
	var doc struct {
		MCPServers map[string]struct {
			Command string `json:"command"`
		} `json:"mcpServers"`
	}
	if json.Unmarshal(b, &doc) != nil {
		return ""
	}
	return doc.MCPServers["parley"].Command
}

// notes are the lines printed before anything is written, so that they are said
// even when the run fails part way: a launch file that runs one parley binary and
// is about to run another, the one running `line set`, which a dev build or a
// moved binary would otherwise make a session's front without a word. The run goes on.
func (p *lineSetPlan) notes() string {
	var b strings.Builder
	for _, lf := range p.files {
		switch {
		case lf.act != setRewrite || lf.command == p.self:
		case lf.command == "":
			fmt.Fprintf(&b, "Note: the parley command in %s could not be read; this run writes %s, the binary running line set.\n", lf.path, p.self)
		default:
			fmt.Fprintf(&b, "Note: the parley command in %s is %s; this run writes %s, the binary running line set.\n", lf.path, lf.command, p.self)
		}
	}
	return b.String()
}

// apply writes what the plan judged: each launch file, one at a time, judged again
// just before it is written and replaced through a temp file and a rename, and
// then, last, the record with the new numbers and the new digests, so that `line
// drop` still recognizes every file. A failure or an interrupt part way is
// reported with what was and was not written, and nothing is rolled back; the
// record still holds the old numbers, and running the verb again finishes.
func (p *lineSetPlan) apply(ctx context.Context, dir string) error {
	var wrote, pending []string
	for _, lf := range p.files {
		if lf.act == setRewrite {
			pending = append(pending, lf.path)
		}
	}
	for _, lf := range p.files {
		if lf.act != setRewrite {
			continue
		}
		if err := ctx.Err(); err != nil {
			return p.failed(fmt.Errorf("interrupted: %w", err), wrote, pending)
		}
		if err := p.rewrite(lf); err != nil {
			return p.failed(err, wrote, pending)
		}
		wrote, pending = append(wrote, lf.path), pending[1:]
	}
	for _, lf := range p.files {
		if lf.act == setRewrite || lf.act == setHolds {
			for _, i := range lf.owners {
				p.next.Sessions[i].Files[0].SHA256 = lf.digest
			}
		}
	}
	if _, err := writeLineRecord(dir, p.next); err != nil {
		return p.failed(err, wrote, nil)
	}
	return nil
}

// rewrite replaces one launch file, after judging it again: it is the line's only
// while the working directory is where it was recorded and the bytes are still the
// recorded ones. The rename replaces a link at the path, never follows one.
func (p *lineSetPlan) rewrite(lf *lineSetFile) error {
	owner := p.old.Sessions[lf.owners[0]]
	act, problem, err := claimLaunchFile(owner, owner.Files[0])
	switch {
	case err != nil:
		return err
	case problem != "":
		return fmt.Errorf("will not rewrite %s: %s; it changed since it was checked", lf.path, problem)
	case act != fileRemove:
		return fmt.Errorf("will not rewrite %s: it is not there any more", lf.path)
	}
	fi, err := os.Lstat(lf.path)
	if err != nil {
		return fmt.Errorf("look at %s: %w", lf.path, err)
	}
	return writeFileAtomic(lf.path, lf.data, fi.Mode().Perm())
}

// failed is err with what the run had done when it stopped: the launch files
// written, those not, and that the record was not, which holds for every failure,
// since it is written last.
func (p *lineSetPlan) failed(err error, wrote, pending []string) error {
	list := func(paths []string) string {
		if len(paths) == 0 {
			return "none"
		}
		return strings.Join(paths, ", ")
	}
	return fmt.Errorf("%w; rewritten: %s; not rewritten: %s; line.json was not written and still holds cap %d, window %s: run this again to finish, a file already rewritten is recognized by its bytes",
		err, list(wrote), list(pending), p.old.Cap, p.old.Window)
}

// report is what goes to stdout: what was changed, and for each session what is
// left for the human. The breaker lives in each front process and reads its cap
// and window once, at launch, so nothing running changes until it is started again.
func (p *lineSetPlan) report(recordPath string) string {
	var b strings.Builder
	old, next := p.old, p.next
	numbers := func(label string, was, now any) string {
		if was == now {
			return fmt.Sprintf("%s %v (unchanged)", label, now)
		}
		return fmt.Sprintf("%s %v (was %v)", label, now, was)
	}
	fmt.Fprintf(&b, "Line %s now has %s and %s.\n", next.ID, numbers("cap", old.Cap, next.Cap), numbers("window", old.Window, next.Window))
	for _, lf := range p.files {
		nicks := make([]string, len(lf.owners))
		for i, o := range lf.owners {
			nicks[i] = old.Sessions[o].Nick
		}
		users := "session " + strings.Join(nicks, ", ")
		if len(nicks) > 1 {
			users = "sessions " + strings.Join(nicks, ", ")
		}
		switch lf.act {
		case setRewrite:
			fmt.Fprintf(&b, "Rewrote %s (%s).\n", lf.path, users)
		case setHolds:
			fmt.Fprintf(&b, "%s already held the new numbers (%s), as a run cut short leaves it: its digest is recorded.\n", lf.path, users)
		case setUntouched:
			fmt.Fprintf(&b, "Left %s as it is (%s): it carries neither number.\n", lf.path, users)
		case setGone:
			fmt.Fprintf(&b, "%s is not there (%s), so nothing was written for it.\n", lf.path, users)
		}
	}
	fmt.Fprintf(&b, "Updated %s.\n", recordPath)

	if len(old.Sessions) == 0 {
		b.WriteString("No session is on the line yet: line add passes the new numbers on.\n")
		return b.String()
	}
	b.WriteString("\nNothing that is running changes yet: each session's front process reads its cap and window once, when it starts.\nWhat is left is yours, for each session, in its own terminal:\n")
	for _, s := range old.Sessions {
		switch s.Harness {
		case harnessClaude:
			fmt.Fprintf(&b, "\n%s (Claude Code): restart it, resuming its conversation:\n\n", s.Nick)
			fmt.Fprintf(&b, "    cd %s\n    %s --resume <session-id>\n", q(s.Workdir), claudeLaunch(s.Nick, s.Model))
		case harnessCodex:
			fmt.Fprintf(&b, "\n%s (Codex): stop its pump, which holds the old numbers, and start this one in its place; the Codex session itself keeps running:\n\n", s.Nick)
			fmt.Fprintf(&b, "    %s\n", codexPump(p.self, next, s.Nick, p.base, old.CodexBin))
		case harnessOpenCode:
			fmt.Fprintf(&b, "\n%s (OpenCode): stop its pump, which holds the old numbers, and start this one in its place; the OpenCode session itself keeps running:\n\n", s.Nick)
			fmt.Fprintf(&b, "    %s\n", openCodePump(p.self, next, s.Nick, p.base))
		}
	}
	b.WriteString("\nEach session's primer still states the old numbers, a Claude Code session's channel instructions too, and a session started outside line add is not in the record, so it was not touched.\n")
	return b.String()
}
