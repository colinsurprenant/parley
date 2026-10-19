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
	"strings"
	"time"

	"github.com/colinsurprenant/parley/internal/state"
	"github.com/colinsurprenant/parley/internal/stream"
)

// cmdLineDrop takes one session off a line: its thread record, its reply
// directory and, when no other session of its checkout and harness still uses it,
// the launch file the line wrote there and the exclude line it added for it. It
// deletes only what it can show is the line's: the record it reads is parley's
// state, but a checkout is the user's, and a file there is the line's only if its
// bytes still hash to the digest the record holds. It never touches a Codex
// session; it prints the command that would, for the human to run if the history
// is not wanted.
func cmdLineDrop(args []string) error {
	fs := flag.NewFlagSet("parley line drop", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), lineUsageDrop)
		fs.PrintDefaults()
	}
	force := fs.Bool("force", false, lineForceHelp)
	stateDir := fs.String("state-dir", "", "the state directory the line was set up under (default $XDG_STATE_HOME/parley)")
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return &configError{err: err}
	}
	switch {
	case len(pos) < 2:
		return cfgErrf("the line id and the session's nick are required: parley line drop <id> <nick>")
	case len(pos) > 2:
		return cfgErrf("unexpected argument %q", pos[2])
	}
	if err := stream.ValidNick(pos[1]); err != nil {
		return cfgErrf("nick %q %v", pos[1], err)
	}
	return takeLineDown(pos[0], pos[1], *stateDir, *force)
}

// cmdLineEnd takes the whole line down: every session as `line drop` does, all
// their locks taken first, and then the line's directory. A failure partway leaves
// the record describing what is still there, so running it again finishes.
func cmdLineEnd(args []string) error {
	fs := flag.NewFlagSet("parley line end", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), lineUsageEnd)
		fs.PrintDefaults()
	}
	force := fs.Bool("force", false, lineForceHelp)
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
		return cfgErrf("the line id is required: parley line end <id>")
	case len(pos) > 1:
		return cfgErrf("unexpected argument %q", pos[1])
	}
	return takeLineDown(pos[0], "", *stateDir, *force)
}

const lineForceHelp = "finish even though a launch file the line wrote is not the file it wrote any more (edited, replaced, or its directory became a link): the file and its exclude line are left in place and said so, never deleted; a session that is still running, and anything else not shown to be the line's, are refused either way"

// takeLineDown is the one teardown behind drop (nick set) and end (nick empty).
func takeLineDown(id, nick, stateDir string, force bool) error {
	if err := checkLineID(id); err != nil {
		return err
	}
	base, err := state.ResolveBase(stateDir)
	if err != nil {
		return cfgErrf("--state-dir: %v", err)
	}
	// A line that is not there is the invocation's mistake and takes no lock.
	if !lineDirIsThere(base, id) {
		return noLineErr(base, id)
	}
	// The id's lock comes first, before the record is read, and is held to the
	// end: a line verb for this id, or a table or build one, is refused while this
	// one runs, so the record read below is the one that is taken down.
	releaseID, err := lockID(base, id)
	if err != nil {
		return err
	}
	defer releaseID()
	// A line new that was killed between making the directory and writing the record
	// leaves a directory with no record in it, and nothing for the verbs to read;
	// `line end` takes that directory away, and only that: a directory that holds
	// anything else is not shown to be a line's.
	if nick == "" {
		dir := lineDirOf(base, id)
		if empty, err := emptyLineDir(dir); err != nil {
			return fmt.Errorf("look at %s: %w", dir, err)
		} else if empty {
			if err := os.RemoveAll(dir); err != nil {
				return fmt.Errorf("remove the line directory %s: %w", dir, err)
			}
			_, err := fmt.Fprintf(os.Stdout, "Line %s had a directory and no record, a line new that did not finish. Removed:\n  line directory %s\n", id, dir)
			return err
		}
	}
	rec, _, err := readLineRecord(base, id)
	if err != nil {
		return err
	}

	var targets []int
	if nick == "" {
		for i := range rec.Sessions {
			targets = append(targets, i)
		}
	} else {
		i, ok := rec.session(nick)
		if !ok {
			return cfgErrf("line %q has no session %q%s", id, nick, sessionList(rec))
		}
		targets = []int{i}
	}

	// Taken before anything else is judged and held until the teardown is done: a
	// session that is running holds its nick's lock, its pump or its channel would
	// be left talking to nothing, and one that starts now is refused rather than
	// raced.
	nicks := make([]string, len(targets))
	for i, ti := range targets {
		nicks[i] = rec.Sessions[ti].Nick
	}
	release, err := lockNicks(base, nicks, func(nick, dir string) error {
		return fmt.Errorf("session %q is still running: its lock under %s is held; stop its pump or its Claude Code session first", nick, dir)
	})
	if err != nil {
		return err
	}
	defer release()

	// Everything is judged before anything is removed, so that a refusal changes
	// nothing.
	claims := make([]sessionClaim, len(targets))
	for i, ti := range targets {
		var remaining []lineSession
		if nick == "" {
			// A whole line goes in record order: what a later session of the same
			// checkout and harness still uses is kept until it is the last one.
			remaining = rec.Sessions[ti+1:]
		} else {
			remaining = append(append([]lineSession(nil), rec.Sessions[:ti]...), rec.Sessions[ti+1:]...)
		}
		if claims[i], err = claimSession(base, rec, rec.Sessions[ti], remaining, force); err != nil {
			return err
		}
	}

	td := &teardown{base: base, rec: rec, whole: nick == "", nick: nick}
	for _, c := range claims {
		if err := td.drop(c); err != nil {
			return td.failed(err)
		}
	}
	if nick == "" {
		dir := lineDirOf(base, id)
		if err := os.RemoveAll(dir); err != nil {
			return td.failed(fmt.Errorf("remove the line directory %s: %w", dir, err))
		}
		td.removed = append(td.removed, "line directory "+dir)
	}
	return td.report(os.Stdout, nick == "")
}

// sessionList names the line's sessions for a refusal.
func sessionList(rec lineRecord) string {
	if len(rec.Sessions) == 0 {
		return ": it has none"
	}
	nicks := make([]string, len(rec.Sessions))
	for i, s := range rec.Sessions {
		nicks[i] = s.Nick
	}
	return " (its sessions: " + strings.Join(nicks, ", ") + ")"
}

type fileAction int

const (
	// fileKept: another session of the checkout and harness still uses the file.
	fileKept fileAction = iota
	// fileRemove: the file is there and is shown to be the line's.
	fileRemove
	// fileGone: there is nothing at the path.
	fileGone
	// fileLeft: the file is there and is not shown to be the line's, and --force
	// said to finish without it.
	fileLeft
)

type threadAction int

const (
	threadNone   threadAction = iota // a Claude session has no thread
	threadRemove                     // the record holds the session's thread
	threadGone                       // there is no record
	threadLeft                       // the record is not shown to be the session's
)

// sessionClaim is what a session's teardown will do, judged before any of it is
// done.
type sessionClaim struct {
	s lineSession

	threadRec *state.ThreadRecord
	thread    threadAction
	threadWhy string

	// replyDir is the reply directory shown to be the session's, or "" when there
	// is none any more.
	replyDir string

	file    lineFile
	fileAct fileAction
	// fileWhy is why the file is kept (the nick that still uses it) or left.
	fileWhy string
}

// claimSession judges each thing the session made. remaining are the sessions
// that will still be on the line once this one is gone; a launch file one of them
// uses is theirs to take down. A thing the session cannot be shown to own is a
// refusal, with --force letting a launch file that is not the line's stay.
func claimSession(base string, rec lineRecord, s lineSession, remaining []lineSession, force bool) (sessionClaim, error) {
	c := sessionClaim{s: s, file: s.Files[0]}

	if s.Thread != "" {
		tr, err := state.OpenThreadRecord(base, s.Nick, rec.Stream)
		if err != nil {
			return c, err
		}
		c.threadRec = tr
		got, rerr := tr.Read()
		switch {
		case rerr != nil:
			c.thread, c.threadWhy = threadLeft, rerr.Error()
		case got == "":
			c.thread = threadGone
		case strings.EqualFold(got, s.Thread):
			c.thread = threadRemove
		default:
			c.thread, c.threadWhy = threadLeft, fmt.Sprintf("it holds thread %s, not session %s's %s, so it is another session's or no one's", got, s.Nick, s.Thread)
		}
	}

	if s.ReplyDir != "" {
		want := lineReplyMarkerContent(rec.ID, s.Nick)
		dir, err := claimReplyDirBy(replyClaim{
			dir: s.ReplyDir, prefix: lineReplyPrefix(rec.ID), marker: lineReplyMarker,
			endCmd: "line drop", newCmd: "line add",
			judge: func(path, content string) string {
				if content == want {
					return ""
				}
				return fmt.Sprintf("its marker %s holds %s, not the line id %q and the nick %q that line add wrote", path, clipQuoted(content), rec.ID, s.Nick)
			},
		})
		if err != nil {
			return c, err
		}
		c.replyDir = dir
	}

	if others := sharingOf(remaining, s); len(others) > 0 {
		c.fileAct, c.fileWhy = fileKept, others[0].Nick
		return c, nil
	}
	act, problem, err := claimLaunchFile(s, c.file)
	if err != nil {
		return c, err
	}
	if problem != "" {
		if !force {
			return c, fmt.Errorf("will not remove %s: %s; it is not shown to be the file line add wrote for session %s, so look at it, then --force to leave it in place and finish", c.file.Path, problem, s.Nick)
		}
		act, c.fileWhy = fileLeft, problem
	}
	c.fileAct = act
	return c, nil
}

// checkoutMoved says, in words that complete "it ...", what a recorded checkout
// now resolves to when that is not the path recorded, which was resolved when the
// session was added; "" when it still is, and when there is nothing at the path
// any more, since nothing is then there to remove.
func checkoutMoved(checkout string) string {
	now, err := filepath.EvalSymlinks(checkout)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return ""
	case err != nil:
		return fmt.Sprintf("its checkout %s cannot be resolved (%v)", checkout, err)
	case now != filepath.Clean(checkout):
		return fmt.Sprintf("its checkout %s now resolves to %s: a directory on the way, or the checkout, was replaced by a link or moved, so what is behind the recorded path is not shown to be the line's", checkout, now)
	}
	return ""
}

// sharingOf are the sessions among of that use the launch file s does.
func sharingOf(of []lineSession, s lineSession) []lineSession {
	var out []lineSession
	for _, o := range of {
		if o.Harness == s.Harness && sameDir(o.Checkout, s.Checkout) {
			out = append(out, o)
		}
	}
	return out
}

// claimLaunchFile decides what is at the launch file f: fileGone when nothing
// is, fileRemove when it is the regular file whose bytes hash to the recorded
// digest in a directory that is a real one, and otherwise a problem, in words
// that complete "it ...", for the caller to refuse or to leave the file for.
func claimLaunchFile(s lineSession, f lineFile) (act fileAction, problem string, err error) {
	// The record holds the checkout resolved, so a path above it that has since
	// become a link, or a checkout that was moved and replaced, is a different
	// directory behind the same name: nothing at the recorded path is shown to be
	// the line's.
	if moved := checkoutMoved(s.Checkout); moved != "" {
		return 0, moved, nil
	}
	dir := filepath.Dir(f.Path)
	if s.Harness == harnessCodex {
		switch fi, lerr := os.Lstat(dir); {
		case errors.Is(lerr, fs.ErrNotExist):
			return fileGone, "", nil
		case lerr != nil:
			return 0, "", fmt.Errorf("look for %s: %w", dir, lerr)
		case !fi.IsDir():
			kind := "is not a directory"
			if fi.Mode()&fs.ModeSymlink != 0 {
				kind = "is a symlink now"
			}
			return 0, fmt.Sprintf("its directory %s %s, so what is behind it is not shown to be the line's", dir, kind), nil
		}
	}
	li, lerr := os.Lstat(f.Path)
	switch {
	case errors.Is(lerr, fs.ErrNotExist):
		return fileGone, "", nil
	case lerr != nil:
		return 0, "", fmt.Errorf("look for %s: %w", f.Path, lerr)
	case !li.Mode().IsRegular():
		kind := "is not a regular file"
		if li.Mode()&fs.ModeSymlink != 0 {
			kind = "is a symlink"
		}
		return 0, "it " + kind, nil
	}
	got, derr := fileDigest(f.Path)
	if derr != nil {
		return 0, fmt.Sprintf("it cannot be read to be checked (%v)", derr), nil
	}
	if got != f.SHA256 {
		return 0, fmt.Sprintf("it holds %s and the record has %s: it was edited or replaced", got, f.SHA256), nil
	}
	return fileRemove, "", nil
}

// teardown is the doing: it removes what the claims showed was the line's, a
// session at a time, and keeps line.json describing what is left.
type teardown struct {
	base string
	rec  lineRecord
	// whole is a `line end`: a launch file another session still uses is not
	// reported as kept, because that session goes next.
	whole bool
	// nick is the session a drop takes off, as the human gave it.
	nick string

	removed, kept, left []string
	codexDeletes        []string
}

// failed ends the run with err, after saying what was already taken down: the
// record lists the rest, and running the verb again finishes.
func (t *teardown) failed(err error) error {
	var b strings.Builder
	if len(t.removed) > 0 {
		b.WriteString("; already removed: " + strings.Join(t.removed, ", "))
	}
	return fmt.Errorf("%w%s (line.json lists what is left: run this again to finish)", err, b.String())
}

// drop takes one session's things down, in order: the thread record, the reply
// directory, the launch file and its directory, the exclude line, and last the
// session's place in the record. Every step is safe to repeat, so a session that
// is still in the record after a failure is taken down from where it stopped.
func (t *teardown) drop(c sessionClaim) error {
	s := c.s

	switch c.thread {
	case threadRemove:
		if err := c.threadRec.Remove(); err != nil {
			return err
		}
		// Its stream directory goes with it when nothing else is in it, as it does
		// for a session whose pump never committed a cursor.
		_ = os.Remove(filepath.Dir(c.threadRec.Path()))
		t.removed = append(t.removed, "thread record for session "+s.Nick)
	case threadGone:
		t.removed = append(t.removed, "thread record for session "+s.Nick+" (already gone)")
	case threadLeft:
		t.left = append(t.left, fmt.Sprintf("thread record %s: %s", c.threadRec.Path(), c.threadWhy))
	}

	if s.ReplyDir != "" {
		if c.replyDir == "" {
			t.removed = append(t.removed, "reply directory "+s.ReplyDir+" (already gone)")
		} else {
			if err := removeDirMarkerLast(c.replyDir, lineReplyMarker); err != nil {
				return fmt.Errorf("remove the reply directory %s: %w", c.replyDir, err)
			}
			t.removed = append(t.removed, "reply directory "+s.ReplyDir)
		}
	}

	f := c.file
	switch c.fileAct {
	case fileKept:
		if !t.whole {
			t.kept = append(t.kept, fmt.Sprintf("%s: session %s of the checkout still uses it", f.Path, c.fileWhy))
		}
	case fileLeft:
		t.left = append(t.left, fmt.Sprintf("%s: %s; it is not deleted, and neither is its exclude line", f.Path, c.fileWhy))
	case fileRemove, fileGone:
		if err := t.removeLaunchFile(s, f, c.fileAct); err != nil {
			return err
		}
	}

	return t.forget(s)
}

// removeLaunchFile removes the launch file, if it is there, and what the line
// made around it: the .codex directory it made, once empty, and the exclude line
// it added.
func (t *teardown) removeLaunchFile(s lineSession, f lineFile, act fileAction) error {
	if act == fileRemove {
		// Looked at again, as close to the removal as can be: the file is only the
		// line's if the checkout is still where it was recorded and the file still
		// hashes to the digest.
		if moved := checkoutMoved(s.Checkout); moved != "" {
			return fmt.Errorf("will not remove %s: %s", f.Path, moved)
		}
		if got, err := fileDigest(f.Path); errors.Is(err, fs.ErrNotExist) {
			act = fileGone
		} else if err != nil || got != f.SHA256 {
			return fmt.Errorf("will not remove %s: it changed since it was checked", f.Path)
		}
	}
	if act == fileRemove {
		if err := os.Remove(f.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", f.Path, err)
		}
		t.removed = append(t.removed, f.Path)
	} else {
		t.removed = append(t.removed, f.Path+" (already gone)")
	}

	if f.MadeDir {
		dir := filepath.Dir(f.Path)
		if li, err := os.Lstat(dir); err == nil && li.IsDir() {
			// Only an empty directory goes: os.Remove refuses any other.
			if err := removeEmptyDir(dir); err == nil {
				if _, serr := os.Lstat(dir); errors.Is(serr, fs.ErrNotExist) {
					t.removed = append(t.removed, "directory "+dir)
				}
			}
		}
	}

	if f.ExcludeFile != "" {
		_, line := launchFileOf(s.Harness, s.Checkout)
		// The exclude file is the repository's, shared by all its worktrees: the
		// line hides the launch files of the others, and goes only when no worktree
		// has a file at its path any more, this line's, another line's or the user's.
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		needed, why := excludeLineStillNeeded(ctx, s.Checkout, f.ExcludeFile, line)
		cancel()
		if needed {
			// A whole line takes its other sessions next, and the last of them to
			// go is the one to take the line out; nothing is left to say then.
			if !(t.whole && t.otherOwner(s, f.ExcludeFile, line)) {
				t.left = append(t.left, fmt.Sprintf("the line %s in %s: %s, so it stays", line, f.ExcludeFile, why))
			}
			return nil
		}
		removed, err := removeExcludeLine(f.ExcludeFile, line, f.ExcludeSep, f.ExcludeMade)
		switch {
		case errors.Is(err, errExcludeUnsafe):
			// A link, or a file too large to read, is not written through or read
			// whole; the line is a convenience, so the drop goes on without it.
			t.left = append(t.left, fmt.Sprintf("the line %s in %s: %v", line, f.ExcludeFile, err))
		case err != nil:
			return fmt.Errorf("remove the line %s from %s: %w", line, f.ExcludeFile, err)
		case removed:
			t.removed = append(t.removed, fmt.Sprintf("the line %s in %s", line, f.ExcludeFile))
		default:
			t.removed = append(t.removed, fmt.Sprintf("the line %s in %s (already gone)", line, f.ExcludeFile))
		}
	}
	return nil
}

// otherOwner is whether a session of the record other than s recorded having put
// line in the exclude file.
func (t *teardown) otherOwner(s lineSession, excludeFile, line string) bool {
	for _, o := range t.rec.Sessions {
		if stream.NickEqual(o.Nick, s.Nick) || len(o.Files) != 1 || o.Files[0].ExcludeFile == "" {
			continue
		}
		if _, l := launchFileOf(o.Harness, o.Checkout); l == line && resolveExisting(o.Files[0].ExcludeFile) == resolveExisting(excludeFile) {
			return true
		}
	}
	return false
}

// forget takes the session out of the record and writes it.
func (t *teardown) forget(s lineSession) error {
	var sessions []lineSession
	for _, o := range t.rec.Sessions {
		if !stream.NickEqual(o.Nick, s.Nick) {
			sessions = append(sessions, o)
		}
	}
	t.rec.Sessions = sessions
	if _, err := writeLineRecord(lineDirOf(t.base, t.rec.ID), t.rec); err != nil {
		return err
	}
	if s.Harness == harnessCodex && s.Thread != "" {
		name := "codex"
		if t.rec.CodexBin != "" {
			name = q(t.rec.CodexBin)
		}
		t.codexDeletes = append(t.codexDeletes, fmt.Sprintf("  %s delete %s    # session %s", name, q(s.Thread), s.Nick))
	}
	return nil
}

// report says what was removed, kept and left in place, and prints, never runs,
// the commands that delete the Codex sessions.
func (t *teardown) report(w io.Writer, whole bool) error {
	var b strings.Builder
	if whole {
		fmt.Fprintf(&b, "Line %s is down. Removed:\n", t.rec.ID)
	} else {
		fmt.Fprintf(&b, "Session %s is dropped from line %s. Removed:\n", t.nick, t.rec.ID)
	}
	for _, r := range t.removed {
		fmt.Fprintf(&b, "  %s\n", r)
	}
	if len(t.kept) > 0 {
		b.WriteString("\nKept, because the line still needs them:\n")
		for _, k := range t.kept {
			fmt.Fprintf(&b, "  %s\n", k)
		}
	}
	if len(t.left) > 0 {
		b.WriteString("\nLeft in place:\n")
		for _, l := range t.left {
			fmt.Fprintf(&b, "  %s\n", l)
		}
		b.WriteString("parley deletes none of these; remove one yourself if you do not need it.\n")
	}
	if len(t.codexDeletes) > 0 {
		b.WriteString("\nCodex still has each Codex session. Keep that history, or delete it:\n")
		b.WriteString(strings.Join(t.codexDeletes, "\n") + "\n")
	}
	if whole {
		b.WriteString("\nEach session's cursor and IRC password stay under " + filepath.Join(t.base, "<nick>") + "/, so the same nick can come back; remove one by hand once the nick is retired.\n")
		b.WriteString("The channel's history stays in the server until it stops; close the sessions' terminals, and Ctrl-C the server if no other table, build or line needs it.\n")
	} else {
		b.WriteString("\nThe session's cursor and IRC password stay under " + filepath.Join(t.base, "<nick>") + "/, so the same nick can come back. Close its terminal and its pump if they are still open.\n")
	}
	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("write the report: %w", err)
	}
	return nil
}
