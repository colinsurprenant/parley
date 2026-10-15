package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/colinsurprenant/parley/internal/state"
)

// What `table end` and the rollback of `table new` delete is proved to be the
// table's first. The record `table end` reads is parley's own state, but a bad
// disk, a stale run or a table new that never finished can leave it wrong, and a
// path in it is only a claim; these are the proofs. They guard against damage,
// not against a record crafted by hand: whoever can write the state base can
// delete those files directly (docs/table.md, "What parley trusts").

// tableReplyMarker is the file table new writes into a reply directory with the
// table's marker in it (see tableMarker), which is what `table end` checks before
// it removes one.
const tableReplyMarker = ".parley-table"

// tableWorktreeMarker is the same for a worktree: a file at its root, holding the
// table's marker, that table new writes right after git has made the worktree. A
// path in a record is a claim, and git listing a worktree only shows it is some
// table's; the marker is what shows it is this one's.
const tableWorktreeMarker = ".parley-table"

// tableMarkerMax bounds what is read of a marker: the id, a roster of seat nicks
// and a digest, far less than the record's own 64 KiB.
const tableMarkerMax = 64 << 10

// tableMarker is what table new writes into both of a table's markers, the
// worktree's and the reply directory's, each line ending in a newline:
//
//   - the table's id;
//   - the nicks of the seats it was made with, exactly as table new was given
//     them, sorted bytewise and separated by spaces. Case is kept: a nick is a
//     directory name under the state base, and "Zed" and "zed" are two
//     directories on a case-sensitive filesystem, so a record edited from one to
//     the other would lock and remove another seat's;
//   - the digest of table.json, "sha256:" and the hex of a hash of its exact bytes.
//
// table.json is a file a person can edit, and everything in it is a claim: which
// nicks' locks `table end` takes, which thread records it removes, and which
// stream they are under. The markers are what binds it to the table table new
// made. The first two lines are written when the markers are made, before any
// Codex turn; the third is added, and each marker replaced, right after
// table.json is written, so a marker with two lines is a table new that never
// finished. One type renders the marker and parses it, so that what is written
// and what is checked cannot differ.
type tableMarker struct {
	id    string
	seats string
	// digest is the third line, or "" for the two lines written first.
	digest string
}

// newTableMarker is the marker of table id made with seats of these nicks, as
// written before table.json is: without the digest.
func newTableMarker(id string, nicks []string) tableMarker {
	sorted := slices.Clone(nicks)
	sort.Strings(sorted)
	return tableMarker{id: id, seats: strings.Join(sorted, " ")}
}

// sealed is the marker bound to the table.json whose digest this is.
func (m tableMarker) sealed(digest string) tableMarker {
	m.digest = digest
	return m
}

// String renders the marker as the content of the file.
func (m tableMarker) String() string {
	s := m.id + "\n" + m.seats + "\n"
	if m.digest != "" {
		s += m.digest + "\n"
	}
	return s
}

// digestPrefix begins the marker's third line.
const digestPrefix = "sha256:"

// parseTableMarker reads a marker's content back; ok is false unless it is two
// or three lines, each ending in a newline, the third beginning with digestPrefix.
func parseTableMarker(content string) (m tableMarker, ok bool) {
	body, found := strings.CutSuffix(content, "\n")
	if !found {
		return tableMarker{}, false
	}
	lines := strings.Split(body, "\n")
	switch len(lines) {
	case 2:
		return tableMarker{id: lines[0], seats: lines[1]}, true
	case 3:
		if !strings.HasPrefix(lines[2], digestPrefix) {
			return tableMarker{}, false
		}
		return tableMarker{id: lines[0], seats: lines[1], digest: lines[2]}, true
	}
	return tableMarker{}, false
}

// recordMarker is the marker table new sealed for rec's table, as far as the
// record says: its id and its seats, bound to the bytes it was read from.
func recordMarker(rec tableRecord) tableMarker {
	nicks := make([]string, len(rec.Seats))
	for i, s := range rec.Seats {
		nicks[i] = s.Nick
	}
	return newTableMarker(rec.ID, nicks).sealed(rec.digest)
}

// readMarker reads the marker at path: a regular file, not a link, through
// tableMarkerMax bytes. When it cannot, problem says why, in words that complete
// "it ..." in a refusal.
func readMarker(path string) (content, problem string) {
	return readMarkerBy(path, "table new")
}

// readMarkerBy is readMarker for a marker written by the command maker.
func readMarkerBy(path, maker string) (content, problem string) {
	li, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", fmt.Sprintf("it holds no marker file %s written by %s", path, maker)
	case err != nil:
		return "", fmt.Sprintf("look for its marker file %s: %v", path, err)
	case !li.Mode().IsRegular():
		return "", fmt.Sprintf("its marker %s is not a regular file", path)
	}
	f, err := state.OpenRegular(path)
	if err != nil {
		return "", fmt.Sprintf("read its marker %s: %v", path, err)
	}
	defer f.Close()
	if oi, err := f.Stat(); err != nil || !os.SameFile(li, oi) {
		return "", fmt.Sprintf("its marker %s changed while it was read", path)
	}
	b, err := io.ReadAll(io.LimitReader(f, tableMarkerMax+1))
	if err != nil {
		return "", fmt.Sprintf("read its marker %s: %v", path, err)
	}
	if len(b) > tableMarkerMax {
		return "", fmt.Sprintf("its marker %s holds more than %d bytes", path, tableMarkerMax)
	}
	return string(b), ""
}

// markerWords are the nouns a marker's refusals are written in, so that a build's
// speak of a build, its record and its peers and a table's of a table, its record
// and its seats, in one set of sentences.
type markerWords struct {
	kind    string // "table"
	members string // "seats"
	record  string // "table.json"
	maker   string // "table new"
}

var tableWords = markerWords{kind: "table", members: "seats", record: tableRecordName, maker: "table new"}

// judgeMarker compares the content of the marker at path with the marker table
// new sealed for rec's table. It returns "" when they are exactly the same, and
// otherwise what is wrong, with edited true when the marker is this table's own
// and it is the record that differs from it: its seats, or its bytes.
func judgeMarker(path, content string, rec tableRecord) (problem string, edited bool) {
	return judgeMarkerWith(path, content, recordMarker(rec), tableWords)
}

// judgeMarkerWith is judgeMarker for the marker want, in the words w.
func judgeMarkerWith(path, content string, want tableMarker, w markerWords) (problem string, edited bool) {
	got, ok := parseTableMarker(content)
	switch {
	case !ok:
		return fmt.Sprintf("its marker %s holds %s, not the %s id, the %s and the digest of %s that %s wrote", path, clipQuoted(content), w.kind, w.members, w.record, w.maker), false
	case got.id != want.id:
		return fmt.Sprintf("its marker %s holds %s, not the %s id %q", path, clipQuoted(got.id), w.kind, want.id), false
	case got.seats != want.seats:
		return fmt.Sprintf("the record's %s differ from the ones the %s was made with: its marker %s lists %s and %s lists %s", w.members, w.kind, path, clipQuoted(got.seats), w.record, clipQuoted(want.seats)), true
	case got.digest == "":
		return fmt.Sprintf("its marker %s has the %s id and the %s but no digest of %s: the %s that wrote it did not finish", path, w.kind, w.members, w.record, w.maker), false
	case got.digest != want.digest:
		return fmt.Sprintf("the record is not the one the %s was made with: its marker %s binds %s to %s and the file read has the digest %s", w.kind, path, w.record, clipQuoted(got.digest), clipQuoted(want.digest)), true
	}
	return "", false
}

// clipQuoted quotes s, cut short when it is long: a marker is read from a file
// that may hold anything.
func clipQuoted(s string) string {
	const max = 120
	if len(s) <= max {
		return strconv.Quote(s)
	}
	return strconv.Quote(s[:max]) + "..."
}

// checkMarkers refuses, before `table end` takes a lock or removes anything, a
// record that is not the one the table was made with: its id, its seats and the
// digest of the bytes read from table.json are held to every marker it can read.
// Those are the worktree's, when the worktree is there and git lists it as the
// target's, and the reply directory's, when that is a directory. A marker it
// cannot read is left to the claims that follow, which refuse it. It is not lifted
// by --force.
//
// vouched is whether at least one marker was read and matched the record exactly.
// Only then is table.json shown to be the one table new sealed, and the thread
// records its stream and seats name shown to be the table's: nothing else holds a
// copy of the digest. With neither marker to read the end still takes down what
// the other claims show is the table's, and no thread record.
func checkMarkers(ctx context.Context, rec tableRecord) (vouched bool, err error) {
	if _, err := os.Lstat(rec.Worktree); err == nil {
		ctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		if listed, err := worktreeListed(ctx, rec.Target, rec.Worktree); err == nil && listed {
			path := filepath.Join(rec.Worktree, tableWorktreeMarker)
			if content, problem := readMarker(path); problem == "" {
				if problem, _ := judgeMarker(path, content, rec); problem != "" {
					return false, fmt.Errorf("will not remove %s: %s", rec.Worktree, problem)
				}
				vouched = true
			}
		}
	}
	dir := filepath.Clean(rec.ReplyDir)
	if fi, err := os.Lstat(dir); err == nil && fi.IsDir() {
		path := filepath.Join(dir, tableReplyMarker)
		if content, problem := readMarker(path); problem == "" {
			if problem, _ := judgeMarker(path, content, rec); problem != "" {
				return false, fmt.Errorf("will not remove the reply directory %s: %s", dir, problem)
			}
			vouched = true
		}
	}
	return vouched, nil
}

// sealMarker replaces the marker at path, a regular file table new wrote under
// root (what names it in a refusal), with the content m, through a temp file and
// a rename, so that a reader finds the whole old marker or the whole new one, and
// a link along the way cannot send the write outside root. The marker keeps the
// mode it had.
func sealMarker(root, what, path string, m tableMarker) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("look for the marker %s: %w", path, err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("the marker %s is not a regular file", path)
	}
	if err := checkParentIn(root, what, path); err != nil {
		return err
	}
	return writeFileAtomic(path, []byte(m.String()), fi.Mode().Perm())
}

// replyDirPrefix is the name of a table's reply directory up to the random
// string os.MkdirTemp adds.
func replyDirPrefix(id string) string { return "parley-table-" + id + "-" }

// strictlyWithin reports whether path, as resolved, is somewhere under dir and
// is not dir itself.
func strictlyWithin(dir, path string) bool {
	return within(dir, path) && resolveExisting(dir) != resolveExisting(path)
}

// worktreeClaim is what shows a worktree is the table's to remove.
type worktreeClaim int

const (
	// claimAbsent: nothing is at the path, so there is nothing to delete.
	claimAbsent worktreeClaim = iota
	// claimListed: git lists the path as a worktree of the target.
	claimListed
	// claimUnderTable: git cannot be asked because the target is gone (it is not at
	// its path any more), and the path is under the owner's own directory, the
	// table's or the build's.
	claimUnderTable
)

// claimWorktree proves rec's worktree is the table's: git lists it among the
// target's linked worktrees and it holds the marker table new sealed for this
// table, or, when git cannot say because the target is gone, it lies under
// tableDir, the directory holding the table's record. Anything else is refused
// with the reason, because whatever is there, it is not shown to be what table
// new made.
func claimWorktree(ctx context.Context, rec tableRecord, tableDir string) (worktreeClaim, error) {
	target, wt := rec.Target, rec.Worktree
	if _, err := os.Lstat(wt); errors.Is(err, fs.ErrNotExist) {
		return claimAbsent, nil
	} else if err != nil {
		return 0, fmt.Errorf("look for the worktree %s: %w", wt, err)
	}
	listed, err := worktreeListed(ctx, target, wt)
	switch {
	case err == nil && listed:
		if err := checkWorktreeMarker(rec); err != nil {
			return 0, err
		}
		return claimListed, nil
	case err == nil:
		return 0, fmt.Errorf("will not remove %s: git does not list it as a worktree of %s, so it is not shown to be the table's; if it is, remove it yourself and run this again", wt, target)
	}
	claim, why := claimWithoutGit(target, wt, tableDir, "table", err)
	if why != "" {
		return 0, fmt.Errorf("will not remove %s: %s; if it is the table's, remove it yourself and run this again", wt, why)
	}
	return claim, nil
}

// claimWithoutGit decides a worktree's claim when `git worktree list` failed
// with gitErr. The path being under the owner's own directory (ownerDir, of the
// kind owner, "table" or "build") stands in for git's answer only when the
// target is no longer at its path: git then has no repository to ask, and nothing
// else could be listing the worktree. Any other failure (a target that is there
// but whose repository git cannot read, a timeout, a git that is missing) says
// nothing of whether the worktree is the owner's, and is refused with git's words,
// quoted: they are git's output. why is empty when the claim is made.
func claimWithoutGit(target, wt, ownerDir, owner string, gitErr error) (claim worktreeClaim, why string) {
	switch _, err := os.Lstat(target); {
	case errors.Is(err, fs.ErrNotExist):
		if strictlyWithin(ownerDir, wt) {
			return claimUnderTable, ""
		}
		return 0, fmt.Sprintf("git cannot say whether it is a worktree of %s (the target is gone), and it is not under the %s's directory %s", target, owner, ownerDir)
	case err != nil:
		return 0, fmt.Sprintf("git cannot say whether it is a worktree of %s (%s), and the target cannot be looked at either (%v)", target, clipQuoted(gitErr.Error()), err)
	}
	return 0, fmt.Sprintf("git cannot say whether it is a worktree of %s: %s; the target is there, so being under the %s's directory is not enough to show it is the %s's", target, clipQuoted(gitErr.Error()), owner, owner)
}

// checkWorktreeMarker accepts rec's worktree only if its marker is a regular
// file, not a link, whose content is exactly the marker table new sealed for the
// table: its id, its seats and the digest of table.json. Git lists every worktree of the target, the other
// tables' included, so a record edited to name one of them is told apart from the
// table's own only here.
func checkWorktreeMarker(rec tableRecord) error {
	wt, id := rec.Worktree, rec.ID
	path := filepath.Join(wt, tableWorktreeMarker)
	content, problem := readMarker(path)
	if problem == "" {
		var edited bool
		if problem, edited = judgeMarker(path, content, rec); problem == "" {
			return nil
		} else if edited {
			return fmt.Errorf("will not remove %s: %s; table.json was changed, so put it back and run this again", wt, problem)
		}
	}
	return fmt.Errorf("will not remove %s: %s; it is not shown to be table %q's, so if it is, remove it yourself and run this again", wt, problem, id)
}

// worktreeListed asks git whether wt is one of target's linked worktrees. The
// first one listed is the checkout itself, which is never a table's.
func worktreeListed(ctx context.Context, target, wt string) (bool, error) {
	out, err := git(ctx, "-C", target, "worktree", "list", "--porcelain")
	if err != nil {
		return false, err
	}
	want := resolveExisting(wt)
	main := true
	for _, line := range strings.Split(out, "\n") {
		path, ok := strings.CutPrefix(line, "worktree ")
		if !ok {
			continue
		}
		if main {
			main = false
			continue
		}
		if resolveExisting(path) == want {
			return true, nil
		}
	}
	return false, nil
}

// removeWorktree takes a worktree that is shown to be the table's (claimWorktree)
// out of the target's registry and off the disk, and returns the claim it was
// made on. One that is already gone has its registration removed, if git still
// holds one. One git cannot remove, as after a checkout that was killed, is
// deleted by hand and then deregistered, and only if it lies under tableDir: that
// fallback is for the table's own directory and for nowhere else. A worktree
// claimed as claimUnderTable, because the target is gone, is deleted by hand with
// no registration to drop: the caller says the registration is left.
func removeWorktree(rec tableRecord, tableDir string) (worktreeClaim, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	claim, err := claimWorktree(ctx, rec, tableDir)
	if err != nil {
		return 0, err
	}
	return claim, removeClaimed(ctx, rec.Target, rec.Worktree, tableDir, "table", claim)
}

// removeClaimed is what removeWorktree does once the claim is made, for a
// worktree under ownerDir, the directory of the owner kind ("table" or "build").
func removeClaimed(ctx context.Context, target, wt, ownerDir, owner string, claim worktreeClaim) error {
	switch claim {
	case claimAbsent:
		return deregisterWorktree(ctx, target, wt)
	case claimUnderTable:
		return os.RemoveAll(wt)
	}
	if _, err := git(ctx, "-C", target, "worktree", "remove", "--force", wt); err != nil {
		if !strictlyWithin(ownerDir, wt) {
			return fmt.Errorf("%w (%s is not under the %s's directory %s, so it is not deleted by hand)", err, wt, owner, ownerDir)
		}
		if rmErr := os.RemoveAll(wt); rmErr != nil {
			return err
		}
		return deregisterWorktree(ctx, target, wt)
	}
	return nil
}

// deregisterWorktree removes wt's own entry from target's worktree registry when
// git still lists it, which git worktree remove does for a path that is no
// longer on disk. It is called only once wt's files are gone and wt is shown to
// be the table's, so the force is doubled: a lock on the entry no longer guards
// anything. It never prunes: a prune would also drop the registration of any
// other worktree of the target that happens to be unavailable.
func deregisterWorktree(ctx context.Context, target, wt string) error {
	// A target that is gone has nothing to deregister from.
	if fi, err := os.Stat(target); err != nil || !fi.IsDir() {
		return nil
	}
	listed, err := worktreeListed(ctx, target, wt)
	if err != nil || !listed {
		return err
	}
	_, err = git(ctx, "-C", target, "worktree", "remove", "--force", "--force", wt)
	return err
}

// removeMadeWorktree takes down a worktree directory that this run created itself
// (os.Mkdir in table new's setUp) and handed to `git worktree add`, in whatever
// state git left it: not registered, registered and checked out, as when a
// post-checkout hook failed, or registered with nothing checked out, as when git
// was killed. No claim is asked for, because the directory's creation is the
// proof it is this run's. git is asked to remove it when it lists the path; the
// directory is then removed by hand if git did not, and its entry deregistered
// if git did not clear it.
func removeMadeWorktree(target, wt string) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	listed, listErr := worktreeListed(ctx, target, wt)
	stale := listErr != nil
	if listErr == nil && listed {
		if _, err := git(ctx, "-C", target, "worktree", "remove", "--force", wt); err != nil {
			stale = true
		}
	}
	if err := os.RemoveAll(wt); err != nil {
		return err
	}
	if !stale {
		return nil
	}
	return deregisterWorktree(ctx, target, wt)
}

// claimReplyDir proves rec's reply directory is one table new made for this
// table: it is a directory itself, not a link; it sits directly in the temp
// directory; it is named for the table; and it holds the marker table new sealed,
// with the table's id, its seats and the digest of table.json in it. It returns
// the directory to remove, or "" when there is none any more. Anything else is
// refused, naming the path.
func claimReplyDir(rec tableRecord) (string, error) {
	return claimReplyDirBy(replyClaim{
		dir: rec.ReplyDir, prefix: replyDirPrefix(rec.ID), marker: tableReplyMarker,
		endCmd: "table end", newCmd: "table new",
		judge: func(path, content string) string {
			problem, _ := judgeMarker(path, content, rec)
			return problem
		},
	})
}

// replyClaim is what claimReplyDirBy needs to prove a reply directory is one a
// command made: where it is, what its name begins with, the marker file in it,
// how that marker is judged against the record, and the commands (end, new) its
// refusals name.
type replyClaim struct {
	dir, prefix, marker string
	endCmd, newCmd      string
	judge               func(path, content string) (problem string)
	// onMismatch, when set, is called with judge's problem when the marker was read
	// and does not match the record: that is evidence the record changed, which a
	// marker that is not there is not.
	onMismatch func(problem string)
}

func claimReplyDirBy(c replyClaim) (string, error) {
	dir := filepath.Clean(c.dir)
	refuse := func(format string, a ...any) (string, error) {
		return "", fmt.Errorf("will not remove the reply directory %s: %s", dir, fmt.Sprintf(format, a...))
	}
	fi, err := os.Lstat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", nil
	case err != nil:
		return refuse("%v", err)
	case !fi.IsDir():
		return refuse("it is not a directory")
	}
	tmp, err := filepath.Abs(os.TempDir())
	if err != nil {
		return refuse("cannot resolve the temp directory: %v", err)
	}
	if resolveExisting(filepath.Dir(dir)) != resolveExisting(tmp) {
		return refuse("it is not directly in this process's temp directory %s; run %s with the TMPDIR %s ran under, or remove it yourself", tmp, c.endCmd, c.newCmd)
	}
	if !strings.HasPrefix(filepath.Base(dir), c.prefix) {
		return refuse("its name does not begin with %s", c.prefix)
	}
	marker := filepath.Join(dir, c.marker)
	content, problem := readMarkerBy(marker, c.newCmd)
	if problem != "" {
		return refuse("%s", problem)
	}
	if problem := c.judge(marker, content); problem != "" {
		if c.onMismatch != nil {
			c.onMismatch(problem)
		}
		return refuse("%s", problem)
	}
	return dir, nil
}
