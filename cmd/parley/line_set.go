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
	"strconv"
	"strings"
	"syscall"

	"github.com/colinsurprenant/parley/internal/primer"
	"github.com/colinsurprenant/parley/internal/state"
)

// cmdLineSet changes the breaker of a line that is running: its cap, its window,
// or both. It rewrites the launch files that do not already carry the numbers
// asked for and the record's numbers, and prints, for each session, what is left
// for the human, because the breaker lives in each session's front process and
// reads its numbers only when that process starts. It touches no server and dials
// nothing, and it never posts: a line's channel is the chair's and the sessions'.
// A launch file in a working directory is rewritten only on the proof `line drop`
// removes one on (its bytes still hash to the recorded digest), atomically and
// never through a link, and every file, and every record this run will write, is
// judged before the first write. Every run goes the same way, a run that has
// nothing to change included: it ends in the numbers asked for, from whatever
// state an earlier run left (see lineSetPlan.apply).
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

	// There is no early return for numbers the line already has: the files are
	// judged against the numbers asked for, whatever the record says, and a run with
	// nothing to write goes through the same plan and the same report, so that what
	// is gone, and the way back for it, is said.
	self := selfPath()
	plan, err := planLineSet(rec, base, self, newCap, newWindow)
	if err != nil {
		return err
	}
	signal.Ignore(syscall.SIGPIPE)
	dir := lineDirOf(base, id)
	if _, err := io.WriteString(os.Stdout, plan.notes()); err != nil {
		return fmt.Errorf("could not write to standard output (%v); nothing was changed", err)
	}
	if err := plan.apply(ctx, dir); err != nil {
		return err
	}
	if _, err := io.WriteString(os.Stdout, plan.report(filepath.Join(dir, lineRecordName))); err != nil {
		if !plan.changes() {
			return fmt.Errorf("could not write to standard output (%v); nothing was changed", err)
		}
		return fmt.Errorf("line %s is changed (cap %d, window %s) but what is left for you could not be written: %v", id, newCap, newWindow, err)
	}
	return nil
}

// setAction is what `line set` does with one launch file.
type setAction int

const (
	// setRewrite: the file is the line's by the recorded digest and does not carry
	// the numbers asked for (or its numbers cannot be read), so it is written again.
	setRewrite setAction = iota
	// setCurrent: the file is the line's by the recorded digest and carries the
	// numbers asked for, read from the bytes the digest proved. Nothing is written,
	// whatever numbers the record holds.
	setCurrent
	// setHolds: the file already holds the bytes that would be written, which a run
	// cut short after it replaced the file and before it recorded the file's digest
	// leaves, under the digest the record still has. Nothing is written; the digest
	// is recorded, once the bytes are proved again.
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
	// digest proved are the line's, for a Claude file that is there and is not
	// held; "" when it could not be read.
	command string
}

// lineSetPlan is everything `line set` has judged before it changes anything.
type lineSetPlan struct {
	base, self string
	old        lineRecord
	// next is the record the run ends with: old with the new numbers and, for each
	// file the run writes or adopts, the file's new digest. Its size, and that of the
	// record every step before the last writes, are checked while planning.
	next  lineRecord
	files []*lineSetFile
}

// cloneLineRecord is rec with a copy of every session and of its files, so that a
// digest set in the copy is not set in rec.
func cloneLineRecord(rec lineRecord) lineRecord {
	out := rec
	out.Sessions = make([]lineSession, len(rec.Sessions))
	for i, s := range rec.Sessions {
		s.Files = slices.Clone(s.Files)
		out.Sessions[i] = s
	}
	return out
}

// planLineSet judges, writing nothing, every launch file the record lists: each
// must be the line's by the proof `line drop` uses (claimLaunchFile), or the
// whole run is refused, naming the file. A file that is not there is left that way.
// The Claude launch files are regenerated for the numbers asked for, by the
// function `line add` writes them with, from the record and from this binary's
// path; a file that already says those numbers is current and is not written,
// whatever the record's numbers are, so that a file restored from an older run, or
// one a run left behind, is judged by what it says and never by what the record
// does. The bytes of a file are read, for anything but the comparison with what
// this run would write, only once they hash to the recorded digest
// (readLaunchSettings). Every check a write of this run could fail on without a
// fault of the machine is made here, the size of each record it will write
// included (checkRecords), so that a run that is refused has written nothing.
func planLineSet(rec lineRecord, base, self string, newCap int, newWindow string) (*lineSetPlan, error) {
	p := &lineSetPlan{base: base, self: self, old: rec, next: cloneLineRecord(rec)}
	p.next.Cap, p.next.Window = newCap, newWindow

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
			// A run of this verb that was cut short after it replaced the file leaves the
			// bytes it would write now, under the digest the record still holds. They
			// are what is wanted, so there is nothing to lose by taking them as they are.
			// The bytes are only compared with what this run would write, never parsed.
			a2, p2, err2 := claimLaunchFile(owner, lineFile{Path: f.Path, SHA256: lf.digest})
			if err2 == nil && p2 == "" && a2 == fileRemove {
				holds, problem = true, ""
			}
		}
		if problem != "" {
			return nil, p.refuse(lf, problem)
		}
		switch {
		case act == fileGone:
			lf.act = setGone
		case holds:
			lf.act = setHolds
		case lf.data == nil:
			lf.act = setUntouched
		default:
			// The digest proved the bytes are the line's, so their numbers are read, and
			// the file is current when they are the ones asked for.
			set := readLaunchSettings(f)
			lf.command = set.command
			lf.act = setRewrite
			if set.numbers && set.capN == newCap && set.window == newWindow {
				lf.act = setCurrent
			}
		}
	}
	for _, lf := range p.files {
		if lf.act == setRewrite || lf.act == setHolds {
			for _, i := range lf.owners {
				p.next.Sessions[i].Files[0].SHA256 = lf.digest
			}
		}
	}
	if err := p.checkRecords(); err != nil {
		return nil, err
	}
	return p, nil
}

// checkRecords asks, before anything is written, whether every record the run will
// write can be: the one each file's step writes, which has the old numbers, and the
// last, which has the new. Digests are all of one length, so these two are the
// largest of the records the run writes, and one the record's reader would refuse
// (a size past lineRecordMax, say) is a refusal here, with every file as it was,
// and not a failure after the files were replaced.
func (p *lineSetPlan) checkRecords() error {
	if !p.changes() {
		return nil
	}
	steps := p.next
	steps.Cap, steps.Window = p.old.Cap, p.old.Window
	for _, rec := range []lineRecord{steps, p.next} {
		if _, err := encodeLineRecord(rec); err != nil {
			return fmt.Errorf("will not change line %s: %w; nothing was changed", p.old.ID, err)
		}
	}
	return nil
}

// numbersChange is whether the run changes the cap or the window the record holds.
func (p *lineSetPlan) numbersChange() bool {
	return p.next.Cap != p.old.Cap || p.next.Window != p.old.Window
}

// changes is whether the run writes anything: a number, or a file, or the digest of
// one that holds the bytes this run would write.
func (p *lineSetPlan) changes() bool {
	if p.numbersChange() {
		return true
	}
	for _, lf := range p.files {
		if lf.act == setRewrite || lf.act == setHolds {
			return true
		}
	}
	return false
}

// setCommand is the line that runs this verb for the line with these numbers, as
// the binary self: runnable as it stands, every value in it quoted. Every value is
// this run's own or the record's, never read from a launch file.
func (p *lineSetPlan) setCommand(self string, capN int, window string) string {
	return fmt.Sprintf("%s line set %s --cap %d --window %s --state-dir %s", q(self), q(p.old.ID), capN, q(window), q(p.base))
}

// refuse is the refusal for a launch file that is not shown to be the line's:
// problem, which completes "it ...", says why, and the rest says what unblocks it.
// Nothing in it comes from the bytes of the file: they do not hash to the recorded
// digest, so they are not shown to be parley's, and a command, a window or anything
// else in them could be anyone's. It says that a `line set` cut short between
// replacing this file and recording it is finished by running it again, with the
// same numbers from the same binary, which recognizes the file by its bytes; and
// that any file can be restored, or its sessions taken off with --force (which
// leaves the file), the path cleared and the sessions added again, which `line add`
// then writes with the record's numbers.
func (p *lineSetPlan) refuse(lf *lineSetFile, problem string) error {
	owner := p.old.Sessions[lf.owners[0]]
	f := owner.Files[0]
	var b strings.Builder
	fmt.Fprintf(&b, "will not rewrite %s: %s; it is not shown to be the file line add wrote for session %s", lf.path, problem, owner.Nick)
	if owner.Harness == harnessClaude {
		b.WriteString("; if an earlier line set on this line was cut short between replacing this file and recording its digest (it was killed there, or the record could not be written), running that same line set again, with the same numbers and from the same binary, finishes it (a run that failed printed that command), then run this again")
	}
	var drops, adds []string
	codex := false
	for _, i := range lf.owners {
		s := p.old.Sessions[i]
		codex = codex || s.Harness == harnessCodex
		drops = append(drops, fmt.Sprintf("%s line drop %s %s --force --state-dir %s", q(p.self), p.old.ID, s.Nick, q(p.base)))
		add := fmt.Sprintf("%s line add %s --nick %s --harness %s --workdir %s", q(p.self), p.old.ID, s.Nick, s.Harness, q(s.Workdir))
		if s.Model != "" {
			add += " --model " + q(s.Model)
		}
		adds = append(adds, add+" --state-dir "+q(p.base))
	}
	fmt.Fprintf(&b, "; to go on, restore the file to the bytes the record has a digest of (%s), or take its sessions off the line (stop each first), move what is at %s aside, and add them again: %s; then %s",
		f.SHA256, lf.path, strings.Join(drops, "; "), strings.Join(adds, "; "))
	if codex {
		b.WriteString(" (a Codex session then starts a new thread)")
	}
	// `line drop --force` leaves the exclude line, and `line add` finds it with no
	// session of the line that records it, so records none: no later drop takes it out.
	if f.ExcludeFile != "" {
		_, line := launchFileOf(owner.Harness, owner.Workdir)
		fmt.Fprintf(&b, "; the exclude line %s in %s stays through that, and unless another session of the line records it, `line add` records none, so no later `line drop` takes it out: remove it from that file by hand once the line has ended", line, f.ExcludeFile)
	}
	b.WriteString("; nothing was changed")
	return errors.New(b.String())
}

// launchSettings is what a Claude launch file says: the parley binary it runs and
// the breaker's numbers, as they are in the args `line add` and `line set` write.
type launchSettings struct {
	command string
	// numbers is whether the args name a --cap that is a number and a --window; capN
	// and window are the last of each, as the flag package would take them.
	numbers bool
	capN    int
	window  string
}

// readLaunchSettings reads the settings of the Claude launch file f from its bytes,
// only if they hash to the recorded digest, so that what is parsed is what `line
// add` or `line set` wrote; the zero value when they do not, or cannot be read or
// parsed.
func readLaunchSettings(f lineFile) launchSettings {
	b, err := readLaunchFile(f.Path)
	if err != nil || tableDigest(b) != f.SHA256 {
		return launchSettings{}
	}
	var doc struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if json.Unmarshal(b, &doc) != nil {
		return launchSettings{}
	}
	srv := doc.MCPServers["parley"]
	set := launchSettings{command: srv.Command}
	var capSeen, windowSeen bool
	for i := 0; i+1 < len(srv.Args); i++ {
		switch srv.Args[i] {
		case "--cap":
			n, err := strconv.Atoi(srv.Args[i+1])
			set.capN, capSeen = n, err == nil
			i++
		case "--window":
			set.window, windowSeen = srv.Args[i+1], true
			i++
		}
	}
	set.numbers = capSeen && windowSeen
	return set
}

// notes are the lines printed before anything is written, so that they are said
// even when the run fails part way: a launch file that runs one parley binary and
// is about to run another, the one running `line set`, which a dev build or a
// moved binary would otherwise make a session's front without a word; and one that
// runs another binary and is left as it is, because it already has the numbers
// asked for, so the line goes on with files that run two. The run goes on.
func (p *lineSetPlan) notes() string {
	var b strings.Builder
	for _, lf := range p.files {
		if (lf.act != setRewrite && lf.act != setCurrent) || lf.command == p.self {
			continue
		}
		left := ""
		if lf.act == setCurrent {
			left = "; the file already has the numbers asked for, so this run leaves it as it is"
		}
		switch {
		case lf.command == "" && lf.act == setRewrite:
			fmt.Fprintf(&b, "Note: the parley command in %s could not be read; this run writes %s, the binary running line set.\n", lf.path, p.self)
		case lf.command == "":
			fmt.Fprintf(&b, "Note: the parley command in %s could not be read%s.\n", lf.path, left)
		case lf.act == setRewrite:
			fmt.Fprintf(&b, "Note: the parley command in %s is %s; this run writes %s, the binary running line set.\n", lf.path, lf.command, p.self)
		default:
			fmt.Fprintf(&b, "Note: the parley command in %s is %s, not %s, the binary running line set%s.\n", lf.path, lf.command, p.self, left)
		}
	}
	return b.String()
}

// setProgress is what a run has done so far, which a failure reports.
type setProgress struct {
	// rewritten are the files the run replaced, and pending those it was still to
	// replace.
	rewritten, pending []string
	// recorded are the files, replaced or adopted, whose new digest line.json holds.
	recorded []string
	// unrecorded is a file whose digest the run was about to record and did not: its
	// bytes are in place, and the record holds another digest. adoptable is whether
	// those bytes are the ones this run writes, which running it again recognizes.
	unrecorded string
	adoptable  bool
	// cur is line.json as it was last written.
	cur lineRecord
}

// apply writes what the plan judged, one file at a time, and the record after each:
//
//   - A file is judged again just before it is written, and replaced through a temp
//     file and a rename. It is then proved again, by its new bytes, and the record
//     is written at once with its new digest under every session that shares it and
//     the numbers the record had. A file already holding the bytes this run would
//     write (setHolds) is proved the same way, against the digest of those bytes,
//     before its digest is recorded.
//   - The new cap and window are written last, in a record of their own, and not at
//     all when they are the record's.
//
// So the record is true after every step: each digest it holds is the file's own
// bytes, and each file says its own numbers, which `line drop` and a later `line set`,
// with any numbers, need. The one thing that is not is the moment between a file's
// rename and the write of its record, which a kill leaves behind, and which only the
// same numbers from the same binary go on from (setHolds). Nothing is rolled back.
//
// The context is asked before every file is written, before every adopted digest is
// recorded and before the numbers are, so that an interrupt stops the run between
// two steps; the record of a file just replaced is written whatever the context says,
// since stopping between the two is the one state that is not true. A failure or an
// interrupt is reported with what was and was not done.
func (p *lineSetPlan) apply(ctx context.Context, dir string) error {
	st := &setProgress{cur: cloneLineRecord(p.old)}
	for _, lf := range p.files {
		if lf.act == setRewrite {
			st.pending = append(st.pending, lf.path)
		}
	}
	for _, lf := range p.files {
		if lf.act != setRewrite && lf.act != setHolds {
			continue
		}
		if err := ctx.Err(); err != nil {
			return p.failed(fmt.Errorf("interrupted: %w", err), st)
		}
		if lf.act == setRewrite {
			if err := p.rewrite(lf); err != nil {
				return p.failed(err, st)
			}
			st.rewritten, st.pending = append(st.rewritten, lf.path), st.pending[1:]
		}
		if err := p.recordFile(dir, lf, st); err != nil {
			return p.failed(err, st)
		}
	}
	if !p.numbersChange() {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return p.failed(fmt.Errorf("interrupted: %w", err), st)
	}
	final := cloneLineRecord(st.cur)
	final.Cap, final.Window = p.next.Cap, p.next.Window
	if _, err := writeLineRecord(dir, final); err != nil {
		return p.failed(err, st)
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

// reprove is the proof that the file is still the one whose digest is about to be
// recorded: the regular file, in the working directory and the real directory it
// was recorded in, whose bytes hash to the digest the run generated. A file that
// was changed since it was written, or since it was found to hold what this run
// writes, is not recorded as the line's.
func (p *lineSetPlan) reprove(lf *lineSetFile) error {
	owner := p.old.Sessions[lf.owners[0]]
	act, problem, err := claimLaunchFile(owner, lineFile{Path: lf.path, SHA256: lf.digest})
	switch {
	case err != nil:
		return err
	case problem != "":
		return fmt.Errorf("will not record the digest of %s: %s; it changed while this run was going", lf.path, problem)
	case act != fileRemove:
		return fmt.Errorf("will not record the digest of %s: it is not there any more", lf.path)
	}
	return nil
}

// recordFile writes line.json with the digest of lf, which has just been replaced
// or is held, under every session that shares it, after proving the file again; on
// a failure it says, in st, that the file's digest is not in the record.
func (p *lineSetPlan) recordFile(dir string, lf *lineSetFile, st *setProgress) error {
	if err := p.reprove(lf); err != nil {
		st.unrecorded, st.adoptable = lf.path, false
		return err
	}
	next := cloneLineRecord(st.cur)
	for _, i := range lf.owners {
		next.Sessions[i].Files[0].SHA256 = lf.digest
	}
	if _, err := writeLineRecord(dir, next); err != nil {
		st.unrecorded, st.adoptable = lf.path, true
		return err
	}
	st.cur = next
	st.recorded = append(st.recorded, lf.path)
	return nil
}

// failed is err with what the run had done when it stopped: the launch files
// replaced, those not, what line.json holds, and how to go on. Every state it can
// leave is true (see apply), so going on is running the verb again with the numbers
// wanted; the exception is a file replaced whose record was not written, and that
// takes the same numbers from the same binary.
func (p *lineSetPlan) failed(err error, st *setProgress) error {
	list := func(paths []string) string {
		if len(paths) == 0 {
			return "none"
		}
		return strings.Join(paths, ", ")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "rewritten: %s; not rewritten: %s; ", list(st.rewritten), list(st.pending))
	if len(st.recorded) == 0 && st.unrecorded == "" {
		fmt.Fprintf(&b, "line.json was not changed and holds cap %d, window %s", st.cur.Cap, st.cur.Window)
	} else {
		fmt.Fprintf(&b, "line.json holds the new digest of %s, and cap %d, window %s, which are written last", list(st.recorded), st.cur.Cap, st.cur.Window)
	}
	if st.unrecorded != "" {
		fmt.Fprintf(&b, "; it does not hold the digest of %s", st.unrecorded)
	}
	repeat := p.setCommand(p.self, p.next.Cap, p.next.Window)
	switch {
	case st.unrecorded != "" && st.adoptable:
		fmt.Fprintf(&b, "; %s holds the bytes this run writes: once the cause is cleared, finish with %s, the same numbers from the same binary, which recognizes the file by its bytes and records its digest (other numbers are refused for that file until then)", st.unrecorded, repeat)
	case st.unrecorded != "":
		fmt.Fprintf(&b, "; %s is not shown to be the line's now, and line set will refuse it, saying what unblocks it", st.unrecorded)
	default:
		fmt.Fprintf(&b, "; to go on, once the cause is cleared, run line set again with the numbers you want: every file the record lists is the line's by the digest it holds and says its own numbers, so any numbers go on from here (%s repeats this run)", repeat)
	}
	return fmt.Errorf("%w; %s", err, b.String())
}

// report is what goes to stdout: what was changed, and for each session what is
// left for the human. The breaker lives in each front process and reads its cap
// and window once, at launch, so nothing running changes until it is started again.
// A run that changes nothing and finds nothing gone says so in one line; any other
// says what each file got, and what is left for a session whose file this run
// replaced or that has none, whatever the numbers were.
func (p *lineSetPlan) report(recordPath string) string {
	var b strings.Builder
	old, next := p.old, p.next
	same := !p.numbersChange()
	gone := map[int]*lineSetFile{}
	rewrote := map[int]bool{}
	for _, lf := range p.files {
		for _, i := range lf.owners {
			switch lf.act {
			case setGone:
				gone[i] = lf
			case setRewrite:
				rewrote[i] = true
			}
		}
	}
	if !p.changes() && len(gone) == 0 {
		fmt.Fprintf(&b, "Line %s already has cap %d and window %s, and no launch file needs a change: nothing was changed.\n", next.ID, old.Cap, old.Window)
		return b.String()
	}
	numbers := func(label string, was, now any) string {
		if was == now {
			return fmt.Sprintf("%s %v (unchanged)", label, now)
		}
		return fmt.Sprintf("%s %v (was %v)", label, now, was)
	}
	if p.changes() {
		fmt.Fprintf(&b, "Line %s now has %s and %s.\n", next.ID, numbers("cap", old.Cap, next.Cap), numbers("window", old.Window, next.Window))
	} else {
		// Not "nothing was changed": that is the all-clear of a run that found every
		// file current, and this one found a file gone.
		fmt.Fprintf(&b, "Line %s already has cap %d and window %s; nothing was written.\n", next.ID, old.Cap, old.Window)
	}
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
		case setCurrent:
			fmt.Fprintf(&b, "Left %s as it is (%s): it already has cap %d and window %s.\n", lf.path, users, next.Cap, next.Window)
		case setHolds:
			fmt.Fprintf(&b, "%s already holds what this run writes (%s): its digest is recorded.\n", lf.path, users)
		case setUntouched:
			fmt.Fprintf(&b, "Left %s as it is (%s): it carries neither number.\n", lf.path, users)
		case setGone:
			fmt.Fprintf(&b, "%s is not there (%s), so nothing was written for it.\n", lf.path, users)
		}
	}
	if p.changes() {
		fmt.Fprintf(&b, "Updated %s.\n", recordPath)
	}

	// With the numbers changed every session has something to do. Otherwise a front
	// holds the numbers the line has, but for the Claude sessions whose file this run
	// replaced, and those with no file at all.
	var need []int
	for i, s := range old.Sessions {
		if !same || (s.Harness == harnessClaude && (rewrote[i] || gone[i] != nil)) {
			need = append(need, i)
		}
	}
	switch {
	case !same && len(old.Sessions) == 0:
		b.WriteString("No session is on the line yet: line add passes the new numbers on.\n")
		return b.String()
	case len(need) == 0:
		// A run that only brought the recorded digests up to date changed no number, and
		// nothing running holds old ones.
		if p.changes() {
			b.WriteString("The numbers are the ones the line had, so no session needs a restart: only the digests the record holds were brought up to date.\n")
		}
		return b.String()
	case same:
		b.WriteString("\nA front process reads its cap and window once, when it starts.\nWhat is left is yours, for each session below, in its own terminal:\n")
	default:
		b.WriteString("\nNothing that is running changes yet: each session's front process reads its cap and window once, when it starts.\nWhat is left is yours, for each session, in its own terminal:\n")
	}
	for _, i := range need {
		s := old.Sessions[i]
		switch s.Harness {
		case harnessClaude:
			if lf, ok := gone[i]; ok {
				// A restart would load no parley server at all. A session taken off and
				// added again is given the file, written with the record's numbers, and
				// every session of the working directory shares it.
				add := fmt.Sprintf("%s line add %s --nick %s --harness claude --workdir %s", q(p.self), old.ID, s.Nick, q(s.Workdir))
				if s.Model != "" {
					add += " --model " + q(s.Model)
				}
				which := "the new numbers"
				if same {
					which = "the line's numbers"
				}
				fmt.Fprintf(&b, "\n%s (Claude Code): %s is not there, so restarting it loads no parley server. Stop it, take it off the line and add it again, which writes the file with %s (the other sessions of the working directory share it), then launch it as line add prints:\n\n", s.Nick, lf.path, which)
				fmt.Fprintf(&b, "    %s line drop %s %s --state-dir %s\n    %s --state-dir %s\n", q(p.self), old.ID, s.Nick, q(p.base), add, q(p.base))
				continue
			}
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
	if !same {
		b.WriteString("\nEach session's primer still states the old numbers, a Claude Code session's channel instructions too, and a session started outside line add is not in the record, so it was not touched.\n")
	}
	return b.String()
}
