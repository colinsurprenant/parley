package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Running git in a worktree a peer or a seat can write (gitwt.go). The attack is
// real: a peer rewrites the worktree's .git file to point at a directory of its
// own whose config names a program as core.fsmonitor, and the next `git status`
// there runs it as the user. These tests plant that, show with a plain git that
// the plant runs, and then show that build end and table end never run it.

// plantedRepo is a git directory a peer could point git at. Its config names two
// programs that run during an ordinary git status, each writing the sentinel
// file: core.fsmonitor, and a clean filter (filter.x.clean, selected for every
// file by the repository's info/attributes) that runs when a file's stat data in
// the index no longer matches and git has to hash it again.
type plantedRepo struct {
	root     string
	gitdir   string
	sentinel string
}

// newPlant makes the planted repository under root, or makes it again from
// nothing: a status that ran in it has refreshed its index, and the filter runs
// only while the index is stale.
func newPlant(t *testing.T, root string) *plantedRepo {
	t.Helper()
	p := &plantedRepo{root: root, gitdir: filepath.Join(root, "plant", ".git"), sentinel: filepath.Join(root, "sentinel")}
	p.rebuild(t)
	return p
}

func (p *plantedRepo) rebuild(t *testing.T) {
	t.Helper()
	plant := filepath.Join(p.root, "plant")
	if err := os.RemoveAll(plant); err != nil {
		t.Fatal(err)
	}
	os.Remove(p.sentinel)
	gitIn(t, p.root, "init", "-q", plant)
	writeFileT(t, filepath.Join(plant, "f"), "content\n")
	gitIn(t, plant, "add", "f")
	gitIn(t, plant, "-c", "commit.gpgsign=false", "commit", "-q", "-m", "plant")
	hook := filepath.Join(p.root, "fsmonitor.sh")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho fsmonitor >> "+p.sentinel+"\nprintf '\\0'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	clean := filepath.Join(p.root, "clean.sh")
	if err := os.WriteFile(clean, []byte("#!/bin/sh\necho filter >> "+p.sentinel+"\ncat\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, plant, "config", "core.fsmonitor", hook)
	gitIn(t, plant, "config", "filter.x.clean", clean)
	writeFileT(t, filepath.Join(p.gitdir, "info", "attributes"), "* filter=x\n")
	// What the commit above ran is not the test's to count.
	os.Remove(p.sentinel)
}

// plantRuns shows that the plant is live: a plain git status in dir runs it. A
// test that skipped this would prove nothing if the plant never ran. The plant
// is made again afterwards, so that the run the test is about meets it fresh.
func plantRuns(t *testing.T, p *plantedRepo, dir string) {
	t.Helper()
	gitIn(t, dir, "status", "--porcelain")
	if _, err := os.Stat(p.sentinel); err != nil {
		t.Fatalf("a plain git status in %s did not run the planted config, so the test proves nothing: %v", dir, err)
	}
	p.rebuild(t)
}

func noSentinel(t *testing.T, p *plantedRepo) {
	t.Helper()
	if raw, err := os.ReadFile(p.sentinel); err == nil {
		t.Fatalf("the planted config ran: %s holds %q", p.sentinel, raw)
	}
}

// repoint rewrites a worktree's .git file to name gitdir, as a peer can.
func repoint(t *testing.T, wt, gitdir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+gitdir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// addPlantedSubmodule puts a gitlink named sub into the index of the worktree wt,
// with a directory sub whose .git file names the planted repository and whose
// file f is stale against that repository's index, as a peer can: a git status
// that goes into the submodule has to hash f, and runs the clean filter.
func addPlantedSubmodule(t *testing.T, wt string, p *plantedRepo) {
	t.Helper()
	head := gitIn(t, filepath.Join(p.root, "plant"), "rev-parse", "HEAD")
	gitIn(t, wt, "update-index", "--add", "--cacheinfo", "160000,"+head+",sub")
	f := filepath.Join(wt, "sub", "f")
	writeFileT(t, f, "content\n")
	old := time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(f, old, old); err != nil {
		t.Fatal(err)
	}
	writeFileT(t, filepath.Join(wt, "sub", ".git"), "gitdir: "+p.gitdir+"\n")
}

// TestBuildEndNeverRunsWhatAPeerPointedGitAt: a peer's .git file names a
// directory of its own whose config runs a program, and a gitlink in its index
// names a repository of its own that does. build end never runs either, and its
// dirty check reads the real admin directory: a worktree that is clean there is
// clean, though the planted repository would call every file untracked.
func TestBuildEndNeverRunsWhatAPeerPointedGitAt(t *testing.T) {
	t.Run("the .git file", func(t *testing.T) {
		b := newTableBench(t)
		rec, _ := buildUp(t, b, "b1", nil)
		p := newPlant(t, b.root)
		for _, peer := range rec.Peers {
			repoint(t, peer.Worktree, p.gitdir)
			plantRuns(t, p, peer.Worktree)
		}
		// A real change in one worktree is still seen: refused, naming it.
		writeFileT(t, filepath.Join(rec.Peers[1].Worktree, "scratch.txt"), "x\n")
		res := b.run(t, b.buildEndArgs("b1")...)
		if res.code != 1 || !strings.Contains(res.stderr, "scratch.txt") || !strings.Contains(res.stderr, rec.Peers[1].Nick) {
			t.Fatalf("exit %d, want 1 naming scratch.txt\n%s", res.code, res.stderr)
		}
		if strings.Contains(res.stderr, "README.md") || strings.Contains(res.stderr, rec.Peers[0].Nick+"'s worktree") {
			t.Errorf("the dirty check read the planted repository, which has no such index: %s", res.stderr)
		}
		noSentinel(t, p)
		if err := os.Remove(filepath.Join(rec.Peers[1].Worktree, "scratch.txt")); err != nil {
			t.Fatal(err)
		}

		// Clean in the real repository, so the end goes through: git's own removal
		// refuses a worktree whose .git does not point back, and parley's fallback
		// takes it down.
		mustBuildEnd(t, b, "b1")
		noSentinel(t, p)
		wantGone(t, rec.Peers[0].Worktree, rec.Peers[1].Worktree, rec.ReplyDir, filepath.Join(b.base, "builds", "b1"))
		if list := gitIn(t, b.repo, "worktree", "list", "--porcelain"); strings.Count(list, "worktree ") != 1 {
			t.Errorf("git worktree list still shows a peer's worktree:\n%s", list)
		}
		if entries, _ := os.ReadDir(filepath.Join(b.repo, ".git", "worktrees")); len(entries) != 0 {
			t.Errorf("admin directories are left: %v", entries)
		}
	})

	t.Run("a submodule", func(t *testing.T) {
		b := newTableBench(t)
		rec, _ := buildUp(t, b, "b1", nil)
		p := newPlant(t, b.root)
		wt := rec.Peers[0].Worktree
		addPlantedSubmodule(t, wt, p)
		plantRuns(t, p, wt)
		addPlantedSubmodule(t, wt, p)

		res := b.run(t, b.buildEndArgs("b1")...)
		if res.code != 1 || !strings.Contains(res.stderr, `"sub"`) {
			t.Fatalf("exit %d, want 1 naming the gitlink sub\n%s", res.code, res.stderr)
		}
		noSentinel(t, p)
		mustBuildEnd(t, b, "b1", "--force")
		noSentinel(t, p)
		wantGone(t, rec.Peers[0].Worktree, rec.Peers[1].Worktree)
	})
}

// TestTableEndNeverRunsWhatASeatPointedGitAt: the same for a table's worktree,
// whose dirty check ran git on the worktree's own .git file.
func TestTableEndNeverRunsWhatASeatPointedGitAt(t *testing.T) {
	t.Run("the .git file", func(t *testing.T) {
		b := newTableBench(t)
		wt, rec := tableUp(t, b, "t1")
		p := newPlant(t, b.root)
		repoint(t, wt, p.gitdir)
		plantRuns(t, p, wt)

		// Clean in the real repository: the end goes through without --force.
		mustTable(t, b, b.endArgs("t1")...)
		noSentinel(t, p)
		wantGone(t, wt, rec.ReplyDir, filepath.Join(b.base, "tables", "t1"))
		if list := gitIn(t, b.repo, "worktree", "list", "--porcelain"); strings.Count(list, "worktree ") != 1 {
			t.Errorf("git worktree list still shows the table's worktree:\n%s", list)
		}
	})
	t.Run("the .git file, and a change that is real", func(t *testing.T) {
		b := newTableBench(t)
		wt, _ := tableUp(t, b, "t1")
		p := newPlant(t, b.root)
		repoint(t, wt, p.gitdir)
		writeFileT(t, filepath.Join(wt, "scratch.txt"), "x\n")
		res := b.run(t, b.endArgs("t1")...)
		if res.code != 1 || !strings.Contains(res.stderr, "scratch.txt") {
			t.Fatalf("exit %d, want 1 naming scratch.txt\n%s", res.code, res.stderr)
		}
		if strings.Contains(res.stderr, "README.md") {
			t.Errorf("the dirty check read the planted repository: %s", res.stderr)
		}
		noSentinel(t, p)
	})
	t.Run("a submodule", func(t *testing.T) {
		b := newTableBench(t)
		wt, _ := tableUp(t, b, "t1")
		p := newPlant(t, b.root)
		addPlantedSubmodule(t, wt, p)
		plantRuns(t, p, wt)
		addPlantedSubmodule(t, wt, p)
		res := b.run(t, b.endArgs("t1")...)
		if res.code != 1 || !strings.Contains(res.stderr, `"sub"`) {
			t.Fatalf("exit %d, want 1 naming the gitlink sub\n%s", res.code, res.stderr)
		}
		noSentinel(t, p)
		mustTable(t, b, b.endArgs("t1", "--force")...)
		noSentinel(t, p)
	})
}

// TestTheWorktreeCommandsSwitchOffTheTargetsOwnFsmonitor: core.fsmonitor and
// core.hooksPath are off on the command line of every git command in a worktree,
// so a config that enables them (a global core.fsmonitor=true is common) does not
// start a monitor, or run a hook, for a worktree about to be removed.
func TestTheWorktreeCommandsSwitchOffTheTargetsOwnFsmonitor(t *testing.T) {
	setup := func(t *testing.T, b *tableBench) *plantedRepo {
		p := newPlant(t, b.root)
		hook := filepath.Join(b.root, "fsmonitor.sh")
		gitIn(t, b.repo, "config", "core.fsmonitor", hook)
		hooks := filepath.Join(b.root, "hooks")
		writeFileT(t, filepath.Join(hooks, "post-index-change"), "#!/bin/sh\necho hook >> "+p.sentinel+"\n")
		if err := os.Chmod(filepath.Join(hooks, "post-index-change"), 0o755); err != nil {
			t.Fatal(err)
		}
		gitIn(t, b.repo, "config", "core.hooksPath", hooks)
		return p
	}
	t.Run("build end", func(t *testing.T) {
		b := newTableBench(t)
		rec, _ := buildUp(t, b, "b1", nil)
		p := setup(t, b)
		// Stale the index so that a status writes it, which runs the hook.
		writeFileT(t, filepath.Join(rec.Peers[0].Worktree, "README.md"), "target\n")
		plantRuns(t, p, rec.Peers[0].Worktree)
		mustBuildEnd(t, b, "b1")
		noSentinel(t, p)
	})
	t.Run("table end", func(t *testing.T) {
		b := newTableBench(t)
		wt, _ := tableUp(t, b, "t1")
		p := setup(t, b)
		writeFileT(t, filepath.Join(wt, "README.md"), "target\n")
		plantRuns(t, p, wt)
		mustTable(t, b, b.endArgs("t1")...)
		noSentinel(t, p)
	})
}

// TestWorktreeAdminDirIsFoundFromTheTargetsSide: the admin directory is the one
// under <common>/worktrees whose gitdir file names the worktree, by exact path;
// a worktree whose own .git file was rewritten to name another admin directory
// does not change the answer.
func TestWorktreeAdminDirIsFoundFromTheTargetsSide(t *testing.T) {
	b := newTableBench(t)
	rec, _ := buildUp(t, b, "b1", []string{"claude-a=claude", "claude-b=claude"})
	a, c := rec.Peers[0], rec.Peers[1]
	common, err := commonGitDir(t.Context(), b.repo)
	if err != nil || common != filepath.Join(b.repo, ".git") {
		t.Fatalf("commonGitDir = %q, %v, want %s", common, err, filepath.Join(b.repo, ".git"))
	}
	repoint(t, a.Worktree, c.AdminGitDir)
	got, err := worktreeAdminDir(common, a.Worktree)
	if err != nil || got != a.AdminGitDir {
		t.Errorf("worktreeAdminDir = %q, %v, want %s: the .git file inside the worktree must not be asked", got, err, a.AdminGitDir)
	}
	if got, err := worktreeAdminDir(common, c.Worktree); err != nil || got != c.AdminGitDir {
		t.Errorf("worktreeAdminDir = %q, %v, want %s", got, err, c.AdminGitDir)
	}
	// A path that merely starts like a worktree's is not it, and neither is one
	// that is not a worktree.
	for _, wt := range []string{a.Worktree + "x", filepath.Dir(a.Worktree), b.repo, filepath.Join(a.Worktree, "sub")} {
		if got, err := worktreeAdminDir(common, wt); err == nil {
			t.Errorf("worktreeAdminDir(%s) = %q, want an error", wt, got)
		}
	}
	if adminOwns(a.AdminGitDir, c.Worktree) || !adminOwns(a.AdminGitDir, a.Worktree) {
		t.Error("adminOwns does not tell the worktrees apart")
	}
}

// TestRelativeWorktreePathsAreResolvedFromTheAdminDir: with
// worktree.useRelativePaths=true git writes the admin directory's gitdir file as a
// path relative to that directory, not an absolute one. parley resolves it against
// the admin directory, as git does, and not against its own working directory, so
// build new and table end, which find a worktree among those files, still do.
func TestRelativeWorktreePathsAreResolvedFromTheAdminDir(t *testing.T) {
	relative := func(t *testing.T, b *tableBench) {
		t.Helper()
		gitIn(t, b.repo, "config", "worktree.useRelativePaths", "true")
	}
	t.Run("build new and build end", func(t *testing.T) {
		b := newTableBench(t)
		relative(t, b)
		rec, _ := buildUp(t, b, "b1", []string{"claude-a=claude", "claude-b=claude"})
		for _, p := range rec.Peers {
			raw := strings.TrimSuffix(string(mustRead(t, filepath.Join(p.AdminGitDir, "gitdir"))), "\n")
			if filepath.IsAbs(raw) {
				t.Fatalf("%s's gitdir file holds %q, an absolute path: git did not write a relative one, so the test proves nothing", p.Nick, raw)
			}
		}
		mustBuildEnd(t, b, "b1")
		for _, p := range rec.Peers {
			wantGone(t, p.Worktree)
		}
		if list := gitIn(t, b.repo, "worktree", "list", "--porcelain"); strings.Count(list, "worktree ") != 1 {
			t.Errorf("git worktree list still shows a build worktree:\n%s", list)
		}
	})
	t.Run("table new and table end", func(t *testing.T) {
		b := newTableBench(t)
		relative(t, b)
		wt, rec := tableUp(t, b, "t1")
		mustTable(t, b, b.endArgs("t1")...)
		wantGone(t, wt, rec.ReplyDir, filepath.Join(b.base, "tables", "t1"))
	})
	t.Run("adminOwns", func(t *testing.T) {
		root := t.TempDir()
		wt := filepath.Join(root, "wt")
		admin := filepath.Join(root, "repo", ".git", "worktrees", "wt")
		writeFileT(t, filepath.Join(wt, ".git"), "x\n")
		writeFileT(t, filepath.Join(admin, "gitdir"), "../../../../wt/.git\n")
		if !adminOwns(admin, wt) {
			t.Error("adminOwns does not resolve a relative gitdir against the admin directory")
		}
		if adminOwns(admin, filepath.Join(root, "other")) {
			t.Error("adminOwns takes a relative gitdir for another worktree's")
		}
		// Not the process's working directory: from there the same text names
		// something else.
		t.Chdir(filepath.Join(root, "repo"))
		if !adminOwns(admin, wt) {
			t.Error("adminOwns depends on the working directory")
		}
	})
}

// TestParseStatusZ: records, a rename's source field skipped, and a cut-off
// record dropped.
func TestParseStatusZ(t *testing.T) {
	out := " M a.txt\x00?? b c.txt\x00R  new.txt\x00old.txt\x00!! d/\x00A  e"
	got := parseStatusZ(out, false)
	want := []statusEntry{{" M", "a.txt"}, {"??", "b c.txt"}, {"R ", "new.txt"}, {"!!", "d/"}, {"A ", "e"}}
	if len(got) != len(want) {
		t.Fatalf("parseStatusZ = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %v, want %v", i, got[i], want[i])
		}
	}
	if got := parseStatusZ(out, true); len(got) != 4 || got[3].path != "d/" {
		t.Errorf("a cut-off status kept %v, want the last, partial record dropped", got)
	}
	if got := parseStatusZ("?? only", true); len(got) != 0 {
		t.Errorf("a status with no complete record kept %v", got)
	}
}
