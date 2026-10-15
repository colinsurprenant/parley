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

	"github.com/colinsurprenant/parley/internal/state"
)

// cmdTableEnd takes a table down: its seats' thread records, its worktree, its
// reply directory and its directory. It deletes only what it can show is the
// table's: the record it reads is a file, and a path in it is a claim. It never
// touches a Codex session; it prints the commands that would, for the human to
// run if the history is not wanted.
func cmdTableEnd(args []string) error {
	fs := flag.NewFlagSet("parley table end", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), tableUsageEnd)
		fs.PrintDefaults()
	}
	force := fs.Bool("force", false, "remove the worktree even though it holds changes beyond the files table new wrote (.parley-table, .mcp.json for the Claude seats, .codex/config.toml for the Codex ones); a seat that is still running, and anything not shown to be the table's own, are refused either way")
	stateDir := fs.String("state-dir", "", "the state directory the table was set up under (default $XDG_STATE_HOME/parley)")
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
		return cfgErrf("the table id is required, first: parley table end <id> [--force] [--state-dir <dir>]")
	case fs.NArg() > 0:
		return cfgErrf("unexpected argument %q", fs.Arg(0))
	}
	if err := checkTableID(id); err != nil {
		return err
	}
	base, err := state.ResolveBase(*stateDir)
	if err != nil {
		return cfgErrf("--state-dir: %v", err)
	}
	// A table that is not there is the invocation's mistake and takes no lock.
	if !tableDirIsThere(base, id) {
		return noTableErr(base, id)
	}
	ctx, stop := signalContext()
	defer stop()
	// The id's lock comes first, before the record is read, and is held to the end:
	// a table new or another table end or build command for this id is refused
	// while this one runs, so the record read below is the one that is taken down,
	// and no other command can be making or unmaking it.
	releaseID, err := lockID(base, id)
	if err != nil {
		return err
	}
	defer releaseID()
	rec, err := readTableRecord(base, id)
	if err != nil {
		return err
	}
	// Everything in the record is a claim: whose locks are taken, whose thread
	// records are removed and under which stream, what is deleted. The markers
	// table new sealed hold the table's id, its seats and the digest of the
	// table.json it wrote. A record that is not the one they were sealed with,
	// down to a byte, is refused before a lock is taken or anything is removed,
	// force or not.
	vouched, err := checkMarkers(ctx, rec)
	if err != nil {
		return err
	}

	// Taken before anything else is judged and held until the teardown is done: a
	// seat that is running holds its nick's lock, its pump or its channel would be
	// left talking to nothing, and one that starts now is refused rather than
	// raced.
	release, err := lockSeats(base, rec)
	if err != nil {
		return err
	}
	defer release()

	// Refused before anything is removed, force or not.
	tableDir := tableDirOf(base, rec.ID)
	replyDir, err := claimReplyDir(rec)
	if err != nil {
		return err
	}
	if _, err := claimWorktree(ctx, rec, tableDir); err != nil {
		return err
	}
	threads, err := claimThreadRecords(base, rec, vouched)
	if err != nil {
		return err
	}
	if !*force {
		dirty, more, err := dirtyWorktree(ctx, rec)
		if err != nil {
			return err
		}
		if len(dirty) > 0 || more {
			return dirtyRefusal(rec.Worktree, dirty, more)
		}
	}

	removed, left, err := takeDown(base, rec, replyDir, threads, vouched)
	if err != nil {
		return err
	}
	return printEnded(os.Stdout, rec, removed, left)
}

// dirtyRefusal is the error for a worktree that holds changes table new did not
// make. When every one of them is an ignored file, and git's status was not cut
// off so that there may be others, it says so: an ignored file is what a tool's
// session hook leaves behind unseen, and the person is told to look at what it
// is before forcing the worktree away. The refusal is the same either way; only
// the words differ.
func dirtyRefusal(worktree string, dirty []statusEntry, more bool) error {
	paths := make([]string, len(dirty))
	ignoredOnly := !more && len(dirty) > 0
	for i, e := range dirty {
		paths[i] = e.path
		ignoredOnly = ignoredOnly && e.code == "!!"
	}
	list := listSome(quoteAll(paths), 5)
	if ignoredOnly {
		what := fmt.Sprintf("all %d are ignored files", len(dirty))
		if len(dirty) == 1 {
			what = "it is an ignored file"
		}
		return fmt.Errorf("the worktree %s has changes beyond the files table new wrote, and %s (%s): a tool's session hook may have written them; look at them, then --force to remove it anyway", worktree, what, list)
	}
	if more {
		list += fmt.Sprintf(", and more past %d bytes of git status", gitOutputMax)
	}
	return fmt.Errorf("the worktree %s has changes beyond the files table new wrote (%s); look at them, or --force to remove it anyway", worktree, list)
}

// lockSeats takes every seat's lock without blocking, and returns what releases
// them. A lock that is held is a seat that is running, and is named.
func lockSeats(base string, rec tableRecord) (release func(), err error) {
	nicks := make([]string, len(rec.Seats))
	for i, s := range rec.Seats {
		nicks[i] = s.Nick
	}
	return lockNicks(base, nicks, func(nick, dir string) error {
		return fmt.Errorf("seat %q is still running: its lock under %s is held; stop its pump or its Claude Code session first", nick, dir)
	})
}

// lockNicks takes each nick's lock without blocking and returns what releases
// them all. When another process holds one, the ones already taken are released
// and the error is what held makes of it, given the nick and the directory its
// lock is in.
func lockNicks(base string, nicks []string, held func(nick, dir string) error) (release func(), err error) {
	var taken []*state.Lock
	release = func() {
		for _, l := range taken {
			_ = l.Release()
		}
	}
	for _, nick := range nicks {
		l, err := state.LockNick(base, nick)
		if err != nil {
			release()
			var locked *state.LockedError
			if errors.As(err, &locked) {
				return nil, held(nick, filepath.Dir(locked.Path))
			}
			return nil, err
		}
		taken = append(taken, l)
	}
	return release, nil
}

// claimedThread is a Codex seat's thread record that claimThreadRecords showed
// to be the table's: it holds the seat's thread, or it is not there. A record
// that is there but unvouched (see checkMarkers) is not shown to be anyone's and
// is only listed, never removed.
type claimedThread struct {
	nick    string
	record  *state.ThreadRecord
	present bool
	// unvouched is set when no marker was left to show table.json is the record
	// table new made, so that the stream and the thread in it are only claims.
	unvouched bool
}

// claimThreadRecords reads each Codex seat's thread record and returns the ones
// takeDown may remove. The record's path comes from the stream and the nicks in
// table.json, which are claims like the rest of it, so a record that holds
// another thread than the seat's is another table's, and one that cannot be read
// is not shown to be anyone's: either refuses the end, force or not. A record
// that is not there is fine; there is nothing to remove.
//
// vouched says whether checkMarkers found a marker that binds table.json. Without
// one, the thread in the file is a claim that nothing but the file backs, and a
// record that holds it may be another table's that an edit pointed at: nothing is
// removed, and a record that is there is returned unvouched, to be listed as left
// in place. Its contents are not read; it is not touched.
func claimThreadRecords(base string, rec tableRecord, vouched bool) ([]claimedThread, error) {
	var claimed []claimedThread
	for _, s := range rec.Seats {
		if s.Thread == "" {
			continue
		}
		tr, err := state.OpenThreadRecord(base, s.Nick, rec.Stream)
		if err != nil {
			return nil, err
		}
		if !vouched {
			if _, err := os.Lstat(tr.Path()); errors.Is(err, fs.ErrNotExist) {
				continue
			}
			claimed = append(claimed, claimedThread{nick: s.Nick, record: tr, present: true, unvouched: true})
			continue
		}
		got, err := tr.Read()
		if err != nil {
			return nil, fmt.Errorf("will not remove the thread record for seat %s: %w", s.Nick, err)
		}
		if got != "" && got != s.Thread {
			return nil, fmt.Errorf("will not remove the thread record %s for seat %s: it holds thread %s, and the table's seat is thread %s; table.json or the record was changed, and the record is another table's or no one's", tr.Path(), s.Nick, got, s.Thread)
		}
		claimed = append(claimed, claimedThread{nick: s.Nick, record: tr, present: got != ""})
	}
	return claimed, nil
}

// takeDown removes what table new made: the seats' thread records first, then the
// worktree, the reply directory, and the table directory, which holds the record,
// last. The worktree and the reply directory hold the markers that show
// table.json is the table's, and the thread records are removed on that
// authority, so an end that stops partway still has its markers when it is run
// again. replyDir is the reply directory claimReplyDir showed to be the table's,
// or "" when there is none; threads are the records claimThreadRecords showed to
// be its seats', and the only ones removed. A record it returns unvouched is not
// removed, and its line is in left, with the reason.
func takeDown(base string, rec tableRecord, replyDir string, threads []claimedThread, vouched bool) (removed, left []string, err error) {
	for _, th := range threads {
		if th.unvouched {
			left = append(left, fmt.Sprintf("thread record for seat %s: %s", th.nick, th.record.Path()))
			continue
		}
		if !th.present {
			removed = append(removed, "thread record for seat "+th.nick+" (already gone)")
			continue
		}
		if err := th.record.Remove(); err != nil {
			return removed, left, err
		}
		// Its stream directory goes with it when nothing else is in it, as it
		// does for a seat whose pump never committed a cursor.
		_ = os.Remove(filepath.Dir(th.record.Path()))
		removed = append(removed, "thread record for seat "+th.nick)
	}

	_, statErr := os.Lstat(rec.Worktree)
	absent := errors.Is(statErr, fs.ErrNotExist)
	byHand := false
	switch {
	case absent && !vouched:
		// Nothing setup-owned vouches for the record, so a git registration at
		// its worktree path is not shown to be this table's: it may be another
		// table's checkout that is missing for now. It is listed, not touched.
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		listed, lerr := worktreeListed(ctx, rec.Target, rec.Worktree)
		cancel()
		if lerr == nil && listed {
			left = append(left, fmt.Sprintf("git's registration of the worktree %s; to drop it: git -C %s worktree remove --force --force %s", rec.Worktree, q(rec.Target), q(rec.Worktree)))
		}
	default:
		claim, err := removeWorktree(rec, tableDirOf(base, rec.ID))
		if err != nil {
			return removed, left, fmt.Errorf("remove the worktree %s: %w", rec.Worktree, err)
		}
		byHand = claim == claimUnderTable
	}
	switch {
	case absent:
		removed = append(removed, "worktree "+rec.Worktree+" (already gone)")
	case byHand:
		// The claim was made without git, because the target is gone, so there was
		// no registry to ask or to prune.
		removed = append(removed, fmt.Sprintf("worktree %s (deleted by hand: the target %s is gone, so git's registration of it is left wherever the repository's git directory is; git worktree prune, run in a checkout of that repository, drops it)", rec.Worktree, rec.Target))
	default:
		removed = append(removed, "worktree "+rec.Worktree)
	}

	if replyDir == "" {
		removed = append(removed, "reply directory "+rec.ReplyDir+" (already gone)")
	} else {
		if err := os.RemoveAll(replyDir); err != nil {
			return removed, left, fmt.Errorf("remove the reply directory %s: %w", replyDir, err)
		}
		removed = append(removed, "reply directory "+rec.ReplyDir)
	}

	dir := tableDirOf(base, rec.ID)
	if err := os.RemoveAll(dir); err != nil {
		return removed, left, fmt.Errorf("remove the table directory %s: %w", dir, err)
	}
	removed = append(removed, "table directory "+dir)
	return removed, left, nil
}

// printEnded says what was removed and what was left in place, and prints, never
// runs, the commands that delete the seats' Codex sessions.
func printEnded(w io.Writer, rec tableRecord, removed, left []string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "Table %s is down. Removed:\n", rec.ID)
	for _, r := range removed {
		fmt.Fprintf(&b, "  %s\n", r)
	}
	if len(left) > 0 {
		b.WriteString("\nLeft in place: no marker remained to show the record is the one table new made, so nothing shows these are this table's:\n")
		for _, l := range left {
			fmt.Fprintf(&b, "  %s\n", l)
		}
		b.WriteString("If one is this table's, remove it yourself.\n")
	}
	var threads []string
	for _, s := range rec.Seats {
		if s.Thread != "" {
			threads = append(threads, fmt.Sprintf("  codex delete %s    # seat %s", q(s.Thread), s.Nick))
		}
	}
	if len(threads) > 0 {
		b.WriteString("\nCodex still has each Codex seat's session. Keep that history, or delete it:\n")
		b.WriteString(strings.Join(threads, "\n") + "\n")
	}
	b.WriteString("\nThe channel's history stays in the server until it stops; close the seats' terminals, and Ctrl-C the server if no other table needs it.\n")
	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("write the report: %w", err)
	}
	return nil
}

// statusIgnoreSubmodules keeps a status from going into a submodule's work tree,
// where it would run git against a directory a seat or a peer made (gitwt.go): a
// gitlink whose commit changed is still listed, what is dirty inside one is not
// asked.
const statusIgnoreSubmodules = "--ignore-submodules=dirty"

// dirtyWorktree lists the changes in the worktree that table new did not make:
// anything git sees except the untracked files table new wrote: the table's
// marker, always, and what its record says by its seats' harnesses — .mcp.json
// when a seat is Claude's, .codex/config.toml when one is Codex's. Any other
// file, .codex/'s other contents included, is a change, and so is an edit to a
// tracked file. A worktree that is no longer there has none. Each change comes
// with its status code, so that a caller can tell an ignored file ("!!") from an
// untracked one.
func dirtyWorktree(ctx context.Context, rec tableRecord) (dirty []statusEntry, more bool, err error) {
	wt := rec.Worktree
	if _, err := os.Lstat(wt); errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	written := map[string]bool{tableWorktreeMarker: true}
	for _, s := range rec.Seats {
		switch s.Harness {
		case harnessClaude:
			written[".mcp.json"] = true
		case harnessCodex:
			written[".codex/config.toml"] = true
		}
	}
	// git is told where the worktree's repository is, from the target's side: its
	// own .git file is a seat's to rewrite (gitwt.go).
	dirs, err := tableWorktreeDirs(ctx, rec)
	if err != nil {
		return nil, false, fmt.Errorf("cannot ask git about the worktree %s: %w", wt, err)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	// Ignored files count too: a build artifact a seat left behind is still
	// something table new did not write. (A build's end differs on purpose: its
	// peers write their worktrees, and the target's ignore rules say what of it is
	// disposable.)
	out, more, err := dirs.capped(ctx, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignored", statusIgnoreSubmodules)
	if err != nil {
		return nil, false, err
	}
	for _, e := range parseStatusZ(out, more) {
		if (e.code == "??" || e.code == "!!") && written[e.path] {
			continue
		}
		dirty = append(dirty, e)
	}
	return dirty, more, nil
}

// statusEntry is one record of `git status --porcelain=v1 -z`: its two-letter
// code and the path. A rename or a copy is listed by the path it has now.
type statusEntry struct{ code, path string }

// parseStatusZ reads the records of a `status --porcelain=v1 -z`. When out was
// cut off (truncated), the record after the last NUL is partial and is dropped.
func parseStatusZ(out string, truncated bool) []statusEntry {
	if truncated {
		if i := strings.LastIndexByte(out, 0); i >= 0 {
			out = out[:i]
		} else {
			out = ""
		}
	}
	var entries []statusEntry
	fields := strings.Split(out, "\x00")
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if len(f) < 4 || f[2] != ' ' {
			continue
		}
		code, path := f[:2], f[3:]
		// A rename or a copy lists its source in a field of its own.
		if code[0] == 'R' || code[0] == 'C' {
			i++
		}
		entries = append(entries, statusEntry{code, path})
	}
	return entries
}

// quoteAll quotes each path for a report line: a name a seat made can hold a
// newline or a terminal control sequence.
func quoteAll(paths []string) []string {
	q := make([]string, len(paths))
	for i, p := range paths {
		q[i] = strconv.Quote(p)
	}
	return q
}

func listSome(items []string, n int) string {
	if len(items) <= n {
		return strings.Join(items, ", ")
	}
	return fmt.Sprintf("%s, and %d more", strings.Join(items[:n], ", "), len(items)-n)
}
