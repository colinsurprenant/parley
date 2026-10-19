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
		return "", true, fmt.Errorf("%s is not a regular file (a link, most likely): it is not followed", path)
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
		return "", true, fmt.Errorf("%s is larger than %d bytes", path, excludeMax)
	}
	return string(b), true, nil
}

// appendExcludeLine appends line to the exclude file at path unless an identical
// line is already there, ending the file's last line first when it has no
// newline. added says whether it wrote. A file it cannot append to safely (a link,
// a directory missing, too large) is a note and no error: the line is a
// convenience, and the launch file is as good without it.
func appendExcludeLine(path, line string) (added, sep bool, note string, err error) {
	content, exists, err := readExclude(path)
	if err != nil {
		return false, false, fmt.Sprintf("no exclude line added: %v", err), nil
	}
	if exists {
		for _, l := range strings.Split(content, "\n") {
			if l == line {
				return false, false, "", nil
			}
		}
	} else if fi, serr := os.Stat(filepath.Dir(path)); serr != nil || !fi.IsDir() {
		return false, false, fmt.Sprintf("no exclude line added: %s is not a directory", filepath.Dir(path)), nil
	}
	text := line + "\n"
	if content != "" && !strings.HasSuffix(content, "\n") {
		text, sep = "\n"+text, true
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE|syscall.O_NOFOLLOW, 0o644)
	if err != nil {
		return false, false, "", fmt.Errorf("open %s to append %s: %w", path, line, err)
	}
	if _, err := f.WriteString(text); err != nil {
		f.Close()
		return false, false, "", fmt.Errorf("append to %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return false, false, "", fmt.Errorf("close %s: %w", path, err)
	}
	return true, sep, "", nil
}

// removeExcludeLine takes the first line of the exclude file at path that is
// exactly line out of it, and its newline, and nothing else: the rest of the file
// stays as it was, byte for byte. sep says that appendExcludeLine had to end the
// file's last line to add this one: when the line is still the last, that newline
// goes with it, and the file is as it was before the append. The file is replaced
// through a temp file and a rename, with the mode it had. removed says whether the
// line was there.
func removeExcludeLine(path, line string, sep bool) (removed bool, err error) {
	content, exists, err := readExclude(path)
	if err != nil {
		return false, err
	}
	if !exists {
		return false, nil
	}
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
			fi, err := os.Lstat(path)
			if err != nil {
				return false, err
			}
			head := content[:start]
			if sep && next == len(content) && strings.HasSuffix(head, "\n") {
				head = head[:len(head)-1]
			}
			if err := writeFileAtomic(path, []byte(head+content[next:]), fi.Mode().Perm()); err != nil {
				return false, err
			}
			return true, nil
		}
		if end < 0 {
			break
		}
		start = next
	}
	return false, nil
}
