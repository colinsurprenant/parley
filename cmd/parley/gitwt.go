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
	"time"

	"github.com/colinsurprenant/parley/internal/state"
)

// Running git inside a worktree a peer or a seat has had its hands in.
//
// Git decides what repository it is in, and so what config it reads and what
// commands that config names, from files it finds: the `.git` file at the root of
// a linked worktree says "gitdir: <path>", and git then reads <path>/config, in
// which `core.fsmonitor`, `core.hooksPath` and the like name programs that run
// during an ordinary `git status`. A peer that can write its worktree can rewrite
// that one file to point git at a directory it made, and the next `git status`
// parley runs there runs the peer's program as the user, outside every sandbox.
// The same goes for a gitlink in the index with a directory of the peer's making
// behind it: a status that looks inside it finds a repository there by the same
// kind of file and runs a clean filter that repository's own config names, which
// neither the environment below nor `core.fsmonitor=false` stops.
//
// So parley never lets a file in the worktree choose what git runs against. The
// three places git needs are named from the target's side, in the environment of
// every command (GIT_DIR, GIT_COMMON_DIR, GIT_WORK_TREE), `core.fsmonitor` and
// `core.hooksPath` are switched off on the command line, and a status passes
// --ignore-submodules=dirty (statusIgnoreSubmodules): a gitlink alone is enough
// to make an unrestricted status run a filter planted behind it. The target's
// own git dir and config are trusted: they are the user's. Commands that run in
// the target itself (`worktree add|remove|list|prune`, `log`) keep running there,
// with git found by -C as before.

// wtDirs is the git dir, the common dir and the work tree of one linked worktree,
// all named from the target's side.
type wtDirs struct {
	gitDir    string // <common>/worktrees/<name>, the worktree's admin dir
	commonDir string // the target's common git dir
	workTree  string // the worktree itself
}

// wtGitFlags switch off, on the command line, the two settings that name a
// program the user's config could otherwise make a status run.
var wtGitFlags = []string{"-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null"}

// env is the environment a git command in the worktree runs in: the caller's,
// less the variables gitEnv drops, plus the three that name the repository.
func (d wtDirs) env() []string {
	return append(gitEnv(), "GIT_DIR="+d.gitDir, "GIT_COMMON_DIR="+d.commonDir, "GIT_WORK_TREE="+d.workTree)
}

// capped runs `git <flags> <args>` in the worktree and returns what it wrote to
// stdout, cut at gitOutputMax with truncated saying whether it was. The working
// directory is the work tree, where git, told where everything is, looks at
// nothing of it but the files it was asked about.
func (d wtDirs) capped(ctx context.Context, args ...string) (out string, truncated bool, err error) {
	return runGit(ctx, d.workTree, d.env(), append(append([]string{}, wtGitFlags...), args...), args)
}

// adminGitDirMax bounds what is read of the gitdir file in an admin directory,
// a path and a newline.
const adminGitDirMax = 8 << 10

// adminDirsMax bounds how many entries of <common>/worktrees are looked at.
const adminDirsMax = 1 << 14

// commonGitDir asks the target for its common git dir, absolute: the directory
// the objects, the config and every linked worktree's admin dir are under, which
// is the target's own git dir unless the target is itself a linked worktree.
func commonGitDir(ctx context.Context, target string) (string, error) {
	out, err := git(ctx, "-C", target, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", err
	}
	dir := strings.TrimSuffix(out, "\n")
	if dir == "" || hasControl(dir) {
		return "", fmt.Errorf("git rev-parse --git-common-dir in %s printed %q, which is not a directory name", target, dir)
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(target, dir)
	}
	return filepath.Clean(dir), nil
}

// adminOwns reports whether the admin directory's gitdir file names wt's .git:
// the file git wrote there when it made the worktree, in the target's own git
// dir, which is not a file of the worktree's. The paths are compared as the
// filesystem resolves them, since git writes the physical one. With
// worktree.useRelativePaths git writes the path relative to the admin directory,
// so a relative one is taken from there and never from parley's working
// directory.
func adminOwns(admin, wt string) bool {
	f, err := state.OpenRegular(filepath.Join(admin, "gitdir"))
	if err != nil {
		return false
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, adminGitDirMax+1))
	if err != nil || len(b) > adminGitDirMax {
		return false
	}
	line, ok := strings.CutSuffix(string(b), "\n")
	if !ok {
		return false
	}
	dir, ok := strings.CutSuffix(line, "/.git")
	if !ok || dir == "" {
		return false
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(resolveExisting(admin), dir)
	}
	return resolveExisting(dir) == resolveExisting(wt)
}

// worktreeAdminDir finds wt's admin directory among <common>/worktrees/*/gitdir
// by an exact match of the path the file names, the way `git worktree list`
// finds it. Nothing in the worktree is read. No match, or two, is an error: the
// caller then has no way to run git there that parley can vouch for.
func worktreeAdminDir(common, wt string) (string, error) {
	dir := filepath.Join(common, "worktrees")
	ents, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("%s has no linked worktrees, so none is %s", common, wt)
		}
		return "", fmt.Errorf("look in %s: %w", dir, err)
	}
	if len(ents) > adminDirsMax {
		return "", fmt.Errorf("%s holds more than %d entries", dir, adminDirsMax)
	}
	var found []string
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		admin := filepath.Join(dir, e.Name())
		if adminOwns(admin, wt) {
			found = append(found, admin)
		}
	}
	switch len(found) {
	case 0:
		return "", fmt.Errorf("none of the linked worktrees under %s is %s", dir, wt)
	case 1:
		return found[0], nil
	}
	return "", fmt.Errorf("%d linked worktrees under %s claim to be %s", len(found), dir, wt)
}

// tableWorktreeDirs is the three places git needs for a table's worktree, from
// the target the record names: the common dir by asking the target, the admin dir
// by finding it among the target's own registrations. A table.json holds neither,
// and nothing is read from the worktree.
func tableWorktreeDirs(ctx context.Context, rec tableRecord) (wtDirs, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	common, err := commonGitDir(ctx, rec.Target)
	if err != nil {
		return wtDirs{}, err
	}
	admin, err := worktreeAdminDir(common, rec.Worktree)
	if err != nil {
		return wtDirs{}, err
	}
	return wtDirs{gitDir: admin, commonDir: common, workTree: rec.Worktree}, nil
}
