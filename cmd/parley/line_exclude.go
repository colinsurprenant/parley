package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/colinsurprenant/parley/internal/state"
)

// What `line add` and `line drop` do to a checkout's info/exclude: the exclude
// file every checkout of a repository shares, which keeps the launch file out of
// `git status` without touching a tracked file. It is the user's file, not
// parley's state, so it is only ever appended to and then, when the line added
// the line itself, that one line is taken out again; a link in its place is
// never followed.

// excludeMax bounds what is read of an exclude file: a few patterns, not a
// place for a megabyte. A larger file is left alone, with a note.
const excludeMax = 4 << 20

// errExcludeUnsafe marks an exclude file that is not read or written because it
// is not a regular file, or is too large: the line is a convenience, so a caller
// leaves such a file alone and says so, rather than fail.
var errExcludeUnsafe = errors.New("not touched")

// excludeFileOf asks git, from the checkout, where its info/exclude is. It
// returns "" and a note for the human when there is none to use: the checkout is
// not a git checkout, or it is a directory inside one (the anchored line would
// not match the launch file there).
func excludeFileOf(ctx context.Context, checkout string) (path, note string) {
	args := append(append([]string(nil), wtGitFlags...), "rev-parse", "--show-toplevel", "--git-path", "info/exclude")
	out, _, err := runGit(ctx, checkout, gitEnv(), args, args)
	if err != nil {
		return "", fmt.Sprintf("no exclude line added: %s is not a git checkout (%s)", checkout, firstLine(err.Error()))
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 || lines[0] == "" || lines[1] == "" {
		return "", fmt.Sprintf("no exclude line added: git said something else than a top directory and an exclude file for %s", checkout)
	}
	if resolveExisting(lines[0]) != resolveExisting(checkout) {
		return "", fmt.Sprintf("no exclude line added: %s is inside the repository at %s and is not its root, where the anchored line would not match", checkout, lines[0])
	}
	path = lines[1]
	if !filepath.IsAbs(path) {
		path = filepath.Join(checkout, path)
	}
	return filepath.Clean(path), ""
}

// readExclude reads the exclude file at path, a regular file and not a link. A
// file that is not there is "" and exists false.
func readExclude(path string) (content string, exists bool, err error) {
	li, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", false, nil
	case err != nil:
		return "", false, err
	case !li.Mode().IsRegular():
		return "", true, fmt.Errorf("%s is not a regular file (a link, most likely), so it is %w and not followed", path, errExcludeUnsafe)
	}
	f, err := state.OpenRegular(path)
	if err != nil {
		return "", true, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, excludeMax+1))
	if err != nil {
		return "", true, err
	}
	if len(b) > excludeMax {
		return "", true, fmt.Errorf("%s is larger than %d bytes, so it is %w", path, excludeMax, errExcludeUnsafe)
	}
	return string(b), true, nil
}

// excludeAdd says what appendExcludeLine did to the file.
type excludeAdd struct {
	// added is whether it wrote the line (it was not there).
	added bool
	// sep is whether it had to end the file's last line first, with a newline.
	sep bool
	// made is whether the file was not there and it created it.
	made bool
}

// appendExcludeLine appends line to the exclude file at path unless an identical
// line is already there, ending the file's last line first when it has no
// newline, and creating the file when it is not there. A file it cannot append to
// safely (a link, a directory missing, too large) is a note and no error: the line
// is a convenience, and the launch file is as good without it.
func appendExcludeLine(path, line string) (res excludeAdd, note string, err error) {
	content, exists, err := readExclude(path)
	if err != nil {
		return res, fmt.Sprintf("no exclude line added: %v", err), nil
	}
	if exists {
		for _, l := range strings.Split(content, "\n") {
			if l == line {
				return res, "", nil
			}
		}
	} else if fi, serr := os.Stat(filepath.Dir(path)); serr != nil || !fi.IsDir() {
		return res, fmt.Sprintf("no exclude line added: %s is not a directory", filepath.Dir(path)), nil
	}
	text := line + "\n"
	if content != "" && !strings.HasSuffix(content, "\n") {
		text, res.sep = "\n"+text, true
	}
	// O_EXCL is not asked for: the file may be made by someone between the look and
	// the open, and then the line is appended to theirs; but the run only claims to
	// have made a file it saw was not there.
	res.made = !exists
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE|syscall.O_NOFOLLOW, 0o644)
	if err != nil {
		return excludeAdd{}, "", fmt.Errorf("open %s to append %s: %w", path, line, err)
	}
	if err := appendText(f, path, text, res.made); err != nil {
		return excludeAdd{}, "", err
	}
	res.added = true
	return res, "", nil
}

// appendFile is the open exclude file appendText writes to; a test supplies one
// whose write or close fails.
type appendFile interface {
	io.WriteCloser
	Stat() (fs.FileInfo, error)
}

// appendText appends text to f, the exclude file at path just opened for appending.
// If the write or the close fails it undoes exactly what it did, so that the file is
// as it was and nothing else of it is touched: a file it created (made) is removed,
// and any other is cut back to the size it had when it was opened.
func appendText(f appendFile, path, text string, made bool) error {
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("look at %s: %w", path, err)
	}
	before := fi.Size()
	fail := func(cause error) error {
		var uerr error
		if made {
			if uerr = os.Remove(path); errors.Is(uerr, fs.ErrNotExist) {
				uerr = nil
			}
		} else {
			uerr = os.Truncate(path, before)
		}
		if uerr != nil {
			return fmt.Errorf("%w (and the partial change to %s could not be undone: %v)", cause, path, uerr)
		}
		return cause
	}
	if _, err := f.Write([]byte(text)); err != nil {
		f.Close()
		return fail(fmt.Errorf("append to %s: %w", path, err))
	}
	if err := f.Close(); err != nil {
		return fail(fmt.Errorf("close %s: %w", path, err))
	}
	return nil
}

// removeExcludeLine takes the first line of the exclude file at path that is
// exactly line out of it, and its newline, and nothing else: the rest of the file
// stays as it was, byte for byte. The one exception is the newline that
// appendExcludeLine had to supply to end the file's last line (sep): it goes with
// the line while the line is still the last, which leaves the file as it was
// before the append, and stays when another line was appended after it, since it
// then ends a line that is no longer the last. made says that the line created
// the file; if nothing is left in it, it is removed. The file is replaced through a
// temp file and a rename, with the mode it had. removed says whether the line was
// there.
func removeExcludeLine(path, line string, sep, made bool) (removed bool, err error) {
	content, exists, err := readExclude(path)
	if err != nil {
		return false, err
	}
	if !exists {
		return false, nil
	}
	after, removed := content, false
	start := 0
	for start <= len(content) {
		end := strings.IndexByte(content[start:], '\n')
		next := len(content)
		text := content[start:]
		if end >= 0 {
			text = content[start : start+end]
			next = start + end + 1
		}
		if text == line {
			head := content[:start]
			if sep && next == len(content) && strings.HasSuffix(head, "\n") {
				head = head[:len(head)-1]
			}
			after, removed = head+content[next:], true
			break
		}
		if end < 0 {
			break
		}
		start = next
	}
	if made && after == "" {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return false, err
		}
		return removed, nil
	}
	if !removed {
		return false, nil
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	if err := writeFileAtomic(path, []byte(after), fi.Mode().Perm()); err != nil {
		return false, err
	}
	return true, nil
}

// worktreeRoots are the roots of the worktrees of the repository the checkout is
// in, as git lists them, the main one and the linked ones. It asks git from the
// checkout when that is there and otherwise from the common git directory the
// exclude file sits in (<common>/info/exclude). ok is false when git cannot say,
// or names a path it quotes: nothing is then known of the other worktrees.
func worktreeRoots(ctx context.Context, checkout, excludeFile string) (roots []string, ok bool) {
	// The worktrees are listed through the git directory that owns the recorded
	// exclude file, <common>/info/exclude, never through the checkout: a checkout
	// path reused by another repository would list that repository's worktrees
	// while the line edits the original's exclude file. A listing cut short at
	// gitOutputMax is not taken as the whole, since a missing worktree could be
	// the one that still needs the line.
	common := filepath.Dir(filepath.Dir(excludeFile))
	a := append(append([]string(nil), wtGitFlags...), "--git-dir", common, "worktree", "list", "--porcelain")
	out, truncated, err := runGit(ctx, "", gitEnv(), a, a)
	if err != nil || truncated {
		return nil, false
	}
	for _, l := range strings.Split(out, "\n") {
		path, found := strings.CutPrefix(l, "worktree ")
		if !found {
			continue
		}
		if strings.HasPrefix(path, `"`) {
			return nil, false
		}
		roots = append(roots, path)
	}
	return roots, len(roots) > 0
}

// excludeLineStillNeeded reports whether the exclude line should stay: a file
// exists at the pattern's path, the line without its leading slash, at the root
// of any worktree of the repository. The exclude file is shared by all of them, so
// the line a session of this line added hides the launch files of the others, this
// line's or another line's or the user's own. why says what keeps it: the file, a
// worktree that could not be looked at (only a not-exist error shows a worktree has
// no such file; any other leaves the line kept, naming the error), or git, which
// could not list the worktrees.
func excludeLineStillNeeded(ctx context.Context, checkout, excludeFile, line string) (needed bool, why string) {
	roots, ok := worktreeRoots(ctx, checkout, excludeFile)
	if !ok {
		return true, "git cannot list the worktrees of the repository, so nothing shows the line is not needed"
	}
	rel := strings.TrimPrefix(line, "/")
	for _, root := range roots {
		path := filepath.Join(root, filepath.FromSlash(rel))
		switch _, err := os.Lstat(path); {
		case err == nil:
			return true, "a file at " + path + " is kept out of git status by it"
		case !errors.Is(err, fs.ErrNotExist):
			return true, fmt.Sprintf("the worktree %s cannot be looked at (%v), so nothing shows the line is not needed", root, err)
		}
	}
	return false, ""
}
