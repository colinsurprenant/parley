package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/colinsurprenant/parley/internal/state"
)

const (
	// launchFilesShown bounds how many commits touching a launch file are named
	// for a peer, and ignoredShown how many ignored names are quoted for one.
	launchFilesShown = 5
	ignoredShown     = 3
	// subjectMax bounds the part of a commit subject printed: it is a peer's text.
	subjectMax = 80
)

// buildLaunchFiles are the files build new writes into a peer's worktree to
// launch its session, and the paths a peer must not commit.
var buildLaunchFiles = []string{buildWorktreeMarker, ".mcp.json", ".codex/config.toml"}

// cmdBuildEnd takes a build down: its peers' cursor state and thread records,
// its worktrees, its reply directory and its directory. It removes only what it
// can show is the build's: the record it reads is parley's own, but a path in it
// is a claim, and what is in a worktree is a peer's. It never touches a branch or
// a Codex session; it prints how, for the human to run if the history is not
// wanted.
func cmdBuildEnd(args []string) error {
	fs := flag.NewFlagSet("parley build end", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), buildUsageEnd)
		fs.PrintDefaults()
	}
	force := fs.Bool("force", false, "remove the worktrees even though they hold tracked changes or untracked files beyond the files build new wrote (.parley-build, .mcp.json for a Claude peer, .codex/config.toml for a Codex one); a peer that is still running, and anything not shown to be the build's own, are refused or left either way. Ignored files never block. It also clears the record of an earlier dispute (builds/<id>/disputed): the peers' state a marker that disagreed with build.json kept is then removed, unless a marker disagrees now")
	stateDir := fs.String("state-dir", "", "the state directory the build was set up under (default $XDG_STATE_HOME/parley)")
	id, rest, hasID := splitTableID(args)
	if err := parseFlags(fs, rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return &configError{err: err}
	}
	switch {
	case !hasID && fs.NArg() == 1:
		id = fs.Arg(0)
	case !hasID:
		return cfgErrf("the build id is required, first: parley build end <id> [--force] [--state-dir <dir>]")
	case fs.NArg() > 0:
		return cfgErrf("unexpected argument %q", fs.Arg(0))
	}
	if err := checkBuildID(id); err != nil {
		return err
	}
	base, err := state.ResolveBase(*stateDir)
	if err != nil {
		return cfgErrf("--state-dir: %v", err)
	}
	// A build that is not there is the invocation's mistake and takes no lock.
	if !buildDirIsThere(base, id) {
		return noBuildErr(base, id)
	}
	ctx, stop := signalContext()
	defer stop()

	// The id's lock comes first, before the record is read, and is held to the
	// end: a build new or another build end or table command for this id is
	// refused while this one runs, so the record read below is the one that is
	// taken down, and no other command can be making or unmaking it.
	releaseID, err := lockID(base, id)
	if err != nil {
		return err
	}
	defer releaseID()
	rec, err := readBuildRecord(base, id)
	if err != nil {
		return err
	}

	// Taken before anything else is judged and held until the teardown is done: a
	// peer that is running holds its nick's lock, its pump or its channel would be
	// left talking to nothing, and one that starts now is refused rather than
	// raced.
	nicks := make([]string, len(rec.Peers))
	for i, p := range rec.Peers {
		nicks[i] = p.Nick
	}
	release, err := lockNicks(base, nicks, func(nick, dir string) error {
		return fmt.Errorf("peer %q is still running: its lock under %s is held; stop its pump or its Claude Code session first", nick, dir)
	})
	if err != nil {
		return err
	}
	defer release()

	end := &buildEnd{rec: rec, base: base, buildDir: buildDirOf(base, id)}
	end.claim(ctx)
	if err := end.inspect(ctx, *force); err != nil {
		return err
	}
	end.launchFileReport(ctx)
	// After the last reason to refuse and before the first removal: what this end
	// found is written down while the evidence (the markers of worktrees it is about
	// to remove) still exists.
	if err := end.holdState(*force); err != nil {
		return err
	}
	if err := end.takeDown(ctx); err != nil {
		return err
	}
	return end.print(os.Stdout)
}

// peerEnd is what build end knows of one peer as it goes: whether the worktree is
// the build's to remove, and what it found there.
type peerEnd struct {
	peerRecord
	claim worktreeClaim
	// claimed is whether the worktree is the build's to remove; why not, if not.
	claimed bool
	why     string
	// ignored are the ignored entries the collapsed --ignored status listed, and
	// ignoredMore whether it was cut off. They are collected whether or not the
	// end is forced: they are a report, never a reason to refuse.
	ignored     []string
	ignoredMore bool
	// launch are the commits on the peer's branch that touch a launch file.
	launch     []string
	launchMore bool
	launchErr  error
}

// buildEnd is one `build end` in progress.
type buildEnd struct {
	rec      buildRecord
	base     string
	buildDir string
	peers    []*peerEnd
	// replyDir is the reply directory the claim showed to be the build's, or ""
	// when there is none to remove.
	replyDir string
	// notes are what was found and does not stop the end (a marker that is gone or
	// changed); left are the items it could not claim, and so did not touch.
	notes []string
	left  []string
	// changed holds what a marker that was there said against build.json: a marker
	// that is present and does not match the record parley sealed is evidence the
	// record changed since build new, so the stream state and the thread records
	// its stream and nicks name are not shown to be the build's and are not
	// removed. Every peer's marker is read, claimed worktree or not. A marker that
	// is missing says nothing (a peer's git clean removes it) and is not in here.
	changed []string
	// disputed is what an earlier end left in the build directory, if it did (see
	// disputedFile); hold is whether this end keeps the peers' state, because a
	// marker disagrees now or the file is there and was not cleared with --force;
	// clearDispute is whether this end removes the state and the file, because the
	// file is all that held it and --force was given.
	disputed     disputedFile
	hold         bool
	clearDispute bool
	// removed is what it took down, in order.
	removed []string
}

// claim shows what is the build's to remove. A worktree is claimed when it is
// the recorded path under the build's directory and the target lists it as one
// of its linked worktrees (or, when the target is gone and git has no repository
// to ask, when it is under the build's own directory); the reply directory is
// claimed as table end claims its. An item that cannot be claimed is reported and
// left, and the rest goes on. The markers it reads on the way are also the
// evidence on the record: see buildEnd.changed.
func (e *buildEnd) claim(ctx context.Context) {
	rec := e.rec
	dir, err := claimReplyDirBy(replyClaim{
		dir: rec.ReplyDir, prefix: buildReplyPrefix(rec.ID), marker: buildReplyMarker,
		endCmd: "build end", newCmd: "build new",
		judge: func(path, content string) string {
			problem, _ := judgeMarkerWith(path, content, rec.marker(), buildWords)
			return problem
		},
		onMismatch: func(problem string) { e.changed = append(e.changed, problem) },
	})
	if err != nil {
		e.left = append(e.left, fmt.Sprintf("reply directory %s: %v", rec.ReplyDir, err))
	}
	e.replyDir = dir

	for _, p := range rec.Peers {
		pe := &peerEnd{peerRecord: p}
		pe.claim, pe.why = e.claimWorktree(ctx, p)
		pe.claimed = pe.why == ""
		if !pe.claimed {
			e.left = append(e.left, fmt.Sprintf("worktree %s of peer %s: %s", p.Worktree, p.Nick, pe.why))
		}
		e.readPeerMarker(pe)
		e.peers = append(e.peers, pe)
	}
}

// readPeerMarker reads a peer's marker at its recorded path whether or not the
// worktree could be claimed: a worktree git does not list, or whose registration
// does not name it, is exactly where a record that was changed would show, and a
// marker that is there and disagrees with the record is evidence on the record
// whatever becomes of the worktree. The read is bounded, starts with an lstat, and
// takes a regular file only, never through a link (readMarkerBy). The worktree
// itself must be a real directory: through a link the marker read would be
// somewhere else's, and a path that is not there has none; claimWorktree has
// already said what is wrong with either. A marker that is missing, or that cannot
// be read, is reported for a worktree that is removed and is no evidence (a peer's
// git clean removes it); one that is there and disagrees is added to changed.
func (e *buildEnd) readPeerMarker(pe *peerEnd) {
	if fi, err := os.Lstat(pe.Worktree); err != nil || !fi.IsDir() {
		return
	}
	path := filepath.Join(pe.Worktree, buildWorktreeMarker)
	content, problem := readMarkerBy(path, "build new")
	mismatch := false
	if problem == "" {
		if problem, _ = judgeMarkerWith(path, content, e.rec.marker(), buildWords); problem != "" {
			e.changed = append(e.changed, problem)
			mismatch = true
		}
	}
	switch {
	case problem == "":
	case pe.claimed:
		e.notes = append(e.notes, fmt.Sprintf("peer %s's worktree %s is removed all the same, but %s", pe.Nick, pe.Worktree, problem))
	case mismatch:
		e.notes = append(e.notes, fmt.Sprintf("peer %s's worktree %s is left in place, and %s", pe.Nick, pe.Worktree, problem))
	}
}

// claimWorktree decides whether p's worktree is the build's to remove; why is
// empty when it is.
func (e *buildEnd) claimWorktree(ctx context.Context, p peerRecord) (worktreeClaim, string) {
	rec, wt := e.rec, p.Worktree
	fi, err := os.Lstat(wt)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return claimAbsent, ""
	case err != nil:
		return 0, fmt.Sprintf("cannot look at it: %v", err)
	case !fi.IsDir():
		// A link would be unlinked and a file removed, and neither is what build new
		// made.
		return 0, "it is not a directory, so it is not what build new made"
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	listed, err := worktreeListed(ctx, rec.Target, wt)
	switch {
	case err == nil && listed:
		if !adminOwns(p.AdminGitDir, wt) {
			return 0, fmt.Sprintf("git lists it, but the recorded admin directory %s does not name it", p.AdminGitDir)
		}
		return claimListed, ""
	case err == nil:
		return 0, fmt.Sprintf("git does not list it as a worktree of %s, so it is not shown to be the build's; if it is, remove it yourself", rec.Target)
	}
	// git could not answer. Only a target that is gone lets the path being under the
	// build's directory stand in for its answer; any other failure is refused.
	return claimWithoutGit(rec.Target, wt, e.buildDir, "build", err)
}

// inspect looks at every claimed worktree before anything is removed. Unless the
// end is forced it refuses a worktree that holds tracked changes or untracked
// files beyond the launch files build new wrote, and it collects what is wrong
// with every peer before it refuses, so that the refusal lists them all: a
// peer's dirty paths, or the error git gave when it could not be asked. That
// check deliberately differs from table end's: no seat should write a table's
// worktree, so table end counts ignored files as changes, but a build's peers
// write theirs, and the target's ignore rules are what say which of the files
// they leave are disposable. Ignored files do not block. They are collected apart
// from the blocking decision, with the ignored entries collapsed to a directory,
// and counted for the report whether or not the end is forced.
func (e *buildEnd) inspect(ctx context.Context, force bool) error {
	var problems []string
	for _, pe := range e.peers {
		if !pe.claimed || pe.claim == claimAbsent {
			continue
		}
		if pe.claim != claimListed {
			// The target is gone: there is no repository to ask. Only --force removes it,
			// unasked.
			if !force {
				problems = append(problems, fmt.Sprintf("peer %s's worktree %s cannot be asked about its changes (the target is not there to ask git for its repository)", pe.Nick, pe.Worktree))
			}
			continue
		}
		if !force {
			dirty, more, err := e.dirtyFiles(ctx, pe)
			if err != nil {
				problems = append(problems, fmt.Sprintf("peer %s's worktree %s: git could not say what it holds (%s)", pe.Nick, pe.Worktree, clipQuoted(err.Error())))
				continue
			}
			if len(dirty) > 0 || more {
				list := listSome(quoteAll(dirty), 5)
				if more {
					list += fmt.Sprintf(", and more past %d bytes of git status", gitOutputMax)
				}
				problems = append(problems, fmt.Sprintf("peer %s's worktree %s has changes beyond the files build new wrote (%s)", pe.Nick, pe.Worktree, list))
			}
		}
		if err := e.collectIgnored(ctx, pe); err != nil {
			msg := fmt.Sprintf("peer %s's worktree %s: git could not list its ignored files (%s)", pe.Nick, pe.Worktree, clipQuoted(err.Error()))
			if force {
				e.notes = append(e.notes, msg+", so they are removed uncounted")
			} else {
				problems = append(problems, msg)
			}
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("%s; look at them, or --force to remove the worktrees anyway", strings.Join(problems, "; "))
	}
	return nil
}

// peerDirs are the places git needs for a peer's claimed worktree: the ones
// build new recorded from the target's side, the admin directory shown to name
// this worktree. Nothing is read from the worktree.
func (e *buildEnd) peerDirs(pe *peerEnd) wtDirs {
	return wtDirs{gitDir: pe.AdminGitDir, commonDir: e.rec.CommonGit, workTree: pe.Worktree}
}

// launchWritten is the set of untracked files build new wrote into a peer's
// worktree: the marker, and the launch file its harness reads.
func (pe *peerEnd) launchWritten() map[string]bool {
	written := map[string]bool{buildWorktreeMarker: true}
	switch pe.Harness {
	case harnessClaude:
		written[".mcp.json"] = true
	case harnessCodex:
		written[".codex/config.toml"] = true
	}
	return written
}

// dirtyFiles lists a worktree's tracked changes and untracked files that build
// new did not write; ignored files are not asked.
func (e *buildEnd) dirtyFiles(ctx context.Context, pe *peerEnd) (dirty []string, more bool, err error) {
	dirs := e.peerDirs(pe)
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	written := pe.launchWritten()
	out, more, err := dirs.capped(ctx, "status", "--porcelain=v1", "-z", "--untracked-files=all", statusIgnoreSubmodules)
	if err != nil {
		return nil, false, err
	}
	for _, en := range parseStatusZ(out, more) {
		if en.code == "??" && written[en.path] {
			continue
		}
		dirty = append(dirty, en.path)
	}
	return dirty, more, nil
}

// collectIgnored records the ignored entries of a worktree for the report, apart
// from any decision to refuse: a second status with the ignored entries collapsed
// to a directory where the whole directory is ignored, so that a node_modules is
// one entry, not tens of thousands.
func (e *buildEnd) collectIgnored(ctx context.Context, pe *peerEnd) error {
	dirs := e.peerDirs(pe)
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	written := pe.launchWritten()
	out, cut, err := dirs.capped(ctx, "status", "--porcelain=v1", "-z", "--ignored", statusIgnoreSubmodules)
	if err != nil {
		return err
	}
	for _, en := range parseStatusZ(out, cut) {
		if en.code != "!!" || written[en.path] {
			continue
		}
		if pe.Harness == harnessCodex && en.path == ".codex/" {
			// A target that ignores .codex/ collapses the config build new wrote
			// and anything a peer left beside it into this one entry: list the
			// directory's own entries and leave out only the config.
			inner, innerCut, err := dirs.capped(ctx, "status", "--porcelain=v1", "-z", "--ignored", "--untracked-files=all", statusIgnoreSubmodules, "--", ".codex/")
			if err != nil {
				return err
			}
			for _, in := range parseStatusZ(inner, innerCut) {
				if in.code == "!!" && !written[in.path] {
					pe.ignored = append(pe.ignored, in.path)
				}
			}
			cut = cut || innerCut
			continue
		}
		pe.ignored = append(pe.ignored, en.path)
	}
	pe.ignoredMore = cut
	return nil
}

// launchFileReport names, for each peer, the commits on its branch that touch a
// launch file, which the skill forbids a peer to change. It is a report and never
// a refusal. It runs in the target: the branch is a ref of the target's own
// repository, and the commits' text is the peer's, so each subject is quoted and
// cut short.
func (e *buildEnd) launchFileReport(ctx context.Context) {
	for _, pe := range e.peers {
		ctx, cancel := context.WithTimeout(ctx, time.Minute)
		args := []string{"-C", e.rec.Target, "log", "--no-show-signature", "--no-color", "--format=tformat:%h %s",
			"--max-count=" + strconv.Itoa(launchFilesShown+1), e.rec.BaseCommit + ".." + pe.Branch, "--"}
		out, err := git(ctx, append(args, buildLaunchFiles...)...)
		cancel()
		if err != nil {
			pe.launchErr = err
			continue
		}
		lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
		if len(lines) == 1 && lines[0] == "" {
			continue
		}
		if len(lines) > launchFilesShown {
			lines, pe.launchMore = lines[:launchFilesShown], true
		}
		for _, l := range lines {
			hash, subject, _ := strings.Cut(l, " ")
			pe.launch = append(pe.launch, hash+" "+quoteSubject(subject))
		}
	}
}

// quoteSubject quotes a commit subject, cut short.
func quoteSubject(s string) string {
	if len(s) > subjectMax {
		cut := subjectMax
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		return strconv.Quote(s[:cut]) + "..."
	}
	return strconv.Quote(s)
}

// takeDown removes what build new made, and only what was claimed: the peers'
// cursor state and thread records first, then the worktrees, the reply directory
// and last the build directory, which holds the record, so that an end that stops
// partway can be run again. The build directory stays when a worktree was left in
// it. No branch is touched. When a marker that is there disagrees with build.json
// (buildEnd.changed), or an earlier end recorded that one did (disputedFile) and
// --force has not said the state was looked at, the stream state and the thread
// records are listed and not removed: the stream and the nicks they are found by
// are the record's claims, and nothing shows them to be the build's (holdState).
func (e *buildEnd) takeDown(ctx context.Context) error {
	rec := e.rec
	for _, pe := range e.peers {
		dir, err := state.StreamDir(e.base, pe.Nick, rec.Stream)
		if err != nil {
			return err
		}
		_, statErr := os.Lstat(dir)
		gone := errors.Is(statErr, fs.ErrNotExist)
		if e.hold {
			if !gone {
				e.left = append(e.left, fmt.Sprintf("cursor state and thread record of peer %s: %s (%s)", pe.Nick, dir, e.heldWhy()))
			}
			continue
		}
		what := "cursor state and thread record of peer " + pe.Nick
		if gone {
			what += " (already gone)"
		}
		if err := state.RemoveStreamDir(e.base, pe.Nick, rec.Stream); err != nil {
			return err
		}
		e.removed = append(e.removed, what)
	}
	if e.clearDispute {
		// The state is gone, so the file that held it has nothing left to hold; it is
		// removed now, and not left for the build directory's removal, because a worktree
		// that was left keeps the directory and the next end must not find it.
		path := filepath.Join(e.buildDir, buildDisputedName)
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", path, err)
		}
		e.removed = append(e.removed, "the record of the dispute "+path)
	}

	for _, pe := range e.peers {
		if !pe.claimed {
			continue
		}
		wctx, cancel := context.WithTimeout(ctx, time.Minute)
		err := removeClaimed(wctx, rec.Target, pe.Worktree, e.buildDir, "build", pe.claim)
		cancel()
		if err != nil {
			return fmt.Errorf("remove the worktree %s: %w", pe.Worktree, err)
		}
		switch pe.claim {
		case claimAbsent:
			e.removed = append(e.removed, "worktree "+pe.Worktree+" (already gone)")
		case claimUnderTable:
			// Claimed without git, because the target is gone: nothing was asked to
			// deregister the worktree.
			e.removed = append(e.removed, "worktree "+pe.Worktree+" (deleted by hand: the target is gone; "+e.dropRegistration(pe)+")")
		default:
			e.removed = append(e.removed, "worktree "+pe.Worktree)
		}
	}

	if e.replyDir == "" {
		if !e.replyLeft() {
			e.removed = append(e.removed, "reply directory "+rec.ReplyDir+" (already gone)")
		}
	} else {
		if err := os.RemoveAll(e.replyDir); err != nil {
			return fmt.Errorf("remove the reply directory %s: %w", e.replyDir, err)
		}
		e.removed = append(e.removed, "reply directory "+rec.ReplyDir)
	}

	// The build directory holds the worktrees, so one that was left keeps it.
	for _, pe := range e.peers {
		if !pe.claimed {
			e.left = append(e.left, "build directory "+e.buildDir+" (it holds the worktree left above, and the record build end needs to be run again)")
			return nil
		}
	}
	if err := os.RemoveAll(e.buildDir); err != nil {
		return fmt.Errorf("remove the build directory %s: %w", e.buildDir, err)
	}
	e.removed = append(e.removed, "build directory "+e.buildDir)
	return nil
}

// disputedFile is what `build end` leaves in the build directory when a marker
// disagreed with build.json, so that the gate outlives the end that tripped it. The
// evidence is in files an end removes: the first end of a build whose peer A's
// marker disagrees removes A's worktree, and with it the marker, and a second end,
// run once B's worktree has been dealt with, would find nothing to disagree and
// remove the state the first kept. The file is in the build directory, which the
// end that finishes the job removes, since there is no later end then.
type disputedFile struct {
	// present is whether anything is at the path. A file that cannot be looked at, or
	// read, still counts: the gate fails closed.
	present bool
	// first is the first thing the file says disagreed, quoted and clipped, "" when
	// it could not be read.
	first string
}

// buildDisputedName is the file's name in <base>/builds/<id>/.
const buildDisputedName = "disputed"

// disputedMax bounds what is read of the file and what is written to it.
const disputedMax = 8 << 10

// readDisputed looks for the file in dir. Anything at the path counts as the file;
// what it says is read through disputedMax bytes, from a regular file, never
// through a link, and shown quoted.
func readDisputed(dir string) disputedFile {
	path := filepath.Join(dir, buildDisputedName)
	if _, err := os.Lstat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return disputedFile{}
		}
		return disputedFile{present: true}
	}
	f, err := state.OpenRegular(path)
	if err != nil {
		return disputedFile{present: true}
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, disputedMax))
	if err != nil {
		return disputedFile{present: true}
	}
	// The first line says what the file is; the second is the first thing that
	// disagreed.
	lines := strings.SplitN(string(b), "\n", 3)
	if len(lines) >= 2 {
		return disputedFile{present: true, first: clipQuoted(lines[1])}
	}
	return disputedFile{present: true}
}

// holdState decides, with the evidence in hand and before anything is removed,
// whether this end keeps the peers' state, and writes the dispute down when it is
// new. A marker that disagrees now holds the state (and --force does not lift it:
// the marker is still wrong, so repair it, or remove what it is in); the file of an
// earlier end holds it too, until --force says the state has been looked at, which
// removes the state and the file. Writing the file is the one thing that can fail
// here, and it fails the end before a thing is removed.
func (e *buildEnd) holdState(force bool) error {
	e.disputed = readDisputed(e.buildDir)
	switch {
	case len(e.changed) > 0:
		e.hold = true
		if !e.disputed.present {
			if err := e.writeDisputed(); err != nil {
				return fmt.Errorf("nothing was removed: %w", err)
			}
		}
	case e.disputed.present && !force:
		e.hold = true
	case e.disputed.present:
		e.clearDispute = true
	}
	return nil
}

// writeDisputed writes the file through a temp file and a rename, mode 0600, with
// a line saying what it is and a line for each thing that disagreed, each on one
// line and the whole cut at disputedMax.
func (e *buildEnd) writeDisputed() error {
	var b strings.Builder
	fmt.Fprintf(&b, "parley build end %s: a marker disagreed with build.json, so the peers' cursor state and thread records are kept, now and at every later end, until you look at them and run parley build end %s --force\n", e.rec.ID, e.rec.ID)
	for _, c := range e.changed {
		b.WriteString(strings.NewReplacer("\n", " ", "\r", " ").Replace(c) + "\n")
	}
	data := b.String()
	if len(data) > disputedMax {
		data = data[:disputedMax]
	}
	path := filepath.Join(e.buildDir, buildDisputedName)
	if err := writeFileAtomic(path, []byte(data), 0o600); err != nil {
		return fmt.Errorf("record the dispute in %s: %w", path, err)
	}
	return nil
}

// buildDirStays is whether this end leaves the build directory in place: it holds
// the worktrees, so one that could not be claimed keeps it.
func (e *buildEnd) buildDirStays() bool {
	for _, pe := range e.peers {
		if !pe.claimed {
			return true
		}
	}
	return false
}

// heldWhy is why the state is listed and not removed, and what the human does. The
// file is only there for a later end when the directory stays.
func (e *buildEnd) heldWhy() string {
	var b strings.Builder
	if len(e.changed) > 0 {
		b.WriteString("a marker does not match build.json, so the stream and the nick it names are not shown to be the build's")
	} else {
		b.WriteString("an earlier end of this build found a marker that did not match build.json")
		if e.disputed.first != "" {
			b.WriteString(" (" + e.disputed.first + ")")
		}
		b.WriteString(" and recorded it in " + filepath.Join(e.buildDir, buildDisputedName) + ", so the stream and the nick it names are not shown to be the build's")
	}
	if e.buildDirStays() {
		fmt.Fprintf(&b, "; look at it, and once no marker disagrees, parley build end %s --force --state-dir %s removes it and that record", e.rec.ID, q(e.base))
	} else {
		b.WriteString("; remove the directory yourself if they are")
	}
	return b.String()
}

// dropRegistration deals with git's registration of a worktree deleted by hand
// because the target is gone, and says what became of it. When the target's
// common git directory is still reachable (the target was itself a linked
// worktree of a repository that is still there), the recorded admin directory is
// removed, and that one only: it is shown to be the worktree's by its gitdir
// file, which names the worktree, and it is a direct child of the common
// directory's worktrees/. This is what `git worktree prune` does for the one
// entry; a prune would also drop the registration of any other worktree of that
// repository that happens to be unavailable. When it is not reachable the
// registration is left, and the report says so.
func (e *buildEnd) dropRegistration(pe *peerEnd) string {
	common := e.rec.CommonGit
	if fi, err := os.Lstat(common); err != nil || !fi.IsDir() {
		return "git's registration of it was in " + common + ", which is not reachable, so it is left; git worktree prune, run in a checkout of that repository, drops it"
	}
	admin := pe.AdminGitDir
	if filepath.Dir(admin) != filepath.Join(common, "worktrees") || !adminOwns(admin, pe.Worktree) {
		return "git's registration of it, " + admin + ", does not name it, so it is left"
	}
	if err := os.RemoveAll(admin); err != nil {
		return "git's registration of it, " + admin + ", could not be removed (" + err.Error() + "), so it is left"
	}
	return "git's registration of it, " + admin + ", is dropped"
}

// replyLeft reports whether the reply directory is among the items left because
// it could not be claimed.
func (e *buildEnd) replyLeft() bool {
	for _, l := range e.left {
		if strings.HasPrefix(l, "reply directory ") {
			return true
		}
	}
	return false
}

// print says what was removed, what was left in place and why, what to look at,
// the branches kept and how to delete one, and the commands that delete the Codex
// sessions, which it prints and never runs.
func (e *buildEnd) print(w io.Writer) error {
	rec := e.rec
	var b strings.Builder
	fmt.Fprintf(&b, "Build %s is down. Removed:\n", rec.ID)
	for _, r := range e.removed {
		fmt.Fprintf(&b, "  %s\n", r)
	}
	if len(e.left) > 0 {
		b.WriteString("\nLeft in place, because nothing shows it is this build's to remove; remove it yourself if it is:\n")
		for _, l := range e.left {
			fmt.Fprintf(&b, "  %s\n", l)
		}
	}
	var warn []string
	warn = append(warn, e.notes...)
	for _, pe := range e.peers {
		if pe.launchErr != nil {
			warn = append(warn, fmt.Sprintf("could not ask for the commits on peer %s's branch %s that touch a launch file: %v", pe.Nick, pe.Branch, pe.launchErr))
		}
		if len(pe.launch) > 0 {
			warn = append(warn, fmt.Sprintf("peer %s's branch %s has commits that touch a launch file (%s), which the skill forbids a peer to change; read them before you merge it:", pe.Nick, pe.Branch, strings.Join(buildLaunchFiles, ", ")))
			for _, l := range pe.launch {
				warn = append(warn, "    "+l)
			}
			if pe.launchMore {
				warn = append(warn, fmt.Sprintf("    and more: git -C %s log %s..%s -- %s", q(rec.Target), rec.BaseCommit[:12], q(pe.Branch), strings.Join(buildLaunchFiles, " ")))
			}
		}
	}
	if len(warn) > 0 {
		b.WriteString("\nWarning:\n")
		for _, l := range warn {
			if strings.HasPrefix(l, "    ") {
				fmt.Fprintf(&b, "  %s\n", l)
			} else {
				fmt.Fprintf(&b, "  - %s\n", l)
			}
		}
	}
	for _, pe := range e.peers {
		if len(pe.ignored) == 0 {
			continue
		}
		count := strconv.Itoa(len(pe.ignored)) + " ignored path"
		if len(pe.ignored) != 1 {
			count += "s"
		}
		if pe.ignoredMore {
			count = "at least " + count
		}
		fmt.Fprintf(&b, "\nPeer %s's worktree held %s, removed with it (the target's ignore rules call them disposable): %s\n",
			pe.Nick, count, listSome(quoteAll(pe.ignored), ignoredShown))
	}

	b.WriteString("\nBranches are kept; build end never deletes or moves one. Merge what you want, then delete a branch by hand:\n")
	for _, pe := range e.peers {
		fmt.Fprintf(&b, "  git -C %s branch -D %s    # peer %s; -D drops its commits if they are not merged\n", q(rec.Target), q(pe.Branch), pe.Nick)
	}
	var threads []string
	for _, p := range rec.Peers {
		if p.Thread != "" {
			threads = append(threads, fmt.Sprintf("  codex delete %s    # peer %s", q(p.Thread), p.Nick))
		}
	}
	if len(threads) > 0 {
		b.WriteString("\nCodex still has each Codex peer's session. Keep that history, or delete it:\n")
		b.WriteString(strings.Join(threads, "\n") + "\n")
	}
	b.WriteString("\nThe channel's history stays in the server until it stops; close the peers' terminals, and Ctrl-C the server if no other table or build needs it.\n")
	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("write the report: %w", err)
	}
	return nil
}
