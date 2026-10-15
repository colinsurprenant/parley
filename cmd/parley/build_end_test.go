package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/colinsurprenant/parley/internal/state"
)

func writeFileT(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// commitFile writes and commits one file on a peer's branch from its worktree.
func commitFile(t *testing.T, wt, name, content, msg string) {
	t.Helper()
	writeFileT(t, filepath.Join(wt, name), content)
	gitIn(t, wt, "add", "-f", "--", name)
	gitIn(t, wt, "-c", "commit.gpgsign=false", "commit", "-q", "-m", msg)
}

func wantGone(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if _, err := os.Lstat(p); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s is still there (%v)", p, err)
		}
	}
}

func mustBuildEnd(t *testing.T, b *tableBench, id string, extra ...string) result {
	t.Helper()
	res := b.run(t, b.buildEndArgs(id, extra...)...)
	if res.code != 0 {
		t.Fatalf("parley build end exited %d\nstdout: %s\nstderr: %s", res.code, res.stdout, res.stderr)
	}
	return res
}

// TestBuildEndTakesTheBuildDown: the worktrees, the reply directory, the peers'
// cursors and thread record and the build directory go; both branches stay and
// are printed with the line that deletes one; the Codex delete line is printed
// and not run; the lock files stay, released.
func TestBuildEndTakesTheBuildDown(t *testing.T) {
	b := newTableBench(t)
	rec, _ := buildUp(t, b, "b1", nil)
	stream := b.buildStream("b1")
	head := b.head(t)
	for _, nick := range []string{"codex-a", "claude-a"} {
		d, err := state.Open(b.base, nick, stream)
		if err != nil {
			t.Fatal(err)
		}
		if err := d.SaveCursor("c-" + nick); err != nil {
			t.Fatal(err)
		}
	}
	// A peer's commit: the branch keeps it.
	commitFile(t, rec.peer(t, "claude-a").Worktree, "work.txt", "work\n", "claude-a's work")
	tip := gitIn(t, b.repo, "rev-parse", "parley/b1/claude-a")
	calls := b.callCount()

	res := mustBuildEnd(t, b, "b1")
	t.Logf("stdout:\n%s", res.stdout)

	if got := b.callCount(); got != calls {
		t.Errorf("build end called codex (%d calls, was %d): it prints the delete lines, not runs them", got, calls)
	}
	if want := "codex delete '" + fakeThread(1) + "'"; !strings.Contains(res.stdout, want) || strings.Count(res.stdout, "codex delete") != 1 {
		t.Errorf("stdout does not have exactly the one line %q:\n%s", want, res.stdout)
	}
	wantGone(t, rec.Peers[0].Worktree, rec.Peers[1].Worktree, rec.ReplyDir, filepath.Join(b.base, "builds", "b1"))
	for _, nick := range []string{"codex-a", "claude-a"} {
		dir, _ := state.StreamDir(b.base, nick, stream)
		wantGone(t, dir)
		if _, err := os.Stat(filepath.Join(b.base, nick, "lock")); err != nil {
			t.Errorf("%s's lock file is gone: %v", nick, err)
		}
		l, err := state.LockNick(b.base, nick)
		if err != nil {
			t.Errorf("%s's lock is held after build end: %v", nick, err)
			continue
		}
		l.Release()
	}
	if list := gitIn(t, b.repo, "worktree", "list", "--porcelain"); strings.Count(list, "worktree ") != 1 {
		t.Errorf("git worktree list still shows a peer's worktree:\n%s", list)
	}
	// Both branches stay, one with its commit, and the report says how to delete one.
	if got := gitIn(t, b.repo, "rev-parse", "parley/b1/codex-a"); got != head {
		t.Errorf("codex-a's branch is at %s, want %s", got, head)
	}
	if got := gitIn(t, b.repo, "rev-parse", "parley/b1/claude-a"); got != tip {
		t.Errorf("claude-a's branch is at %s, want %s", got, tip)
	}
	for _, want := range []string{"git -C '" + b.repo + "' branch -D 'parley/b1/codex-a'", "git -C '" + b.repo + "' branch -D 'parley/b1/claude-a'", "never deletes or moves"} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, res.stdout)
		}
	}
	if got := gitIn(t, b.repo, "status", "--porcelain"); got != "" {
		t.Errorf("the target is not clean: %s", got)
	}
	// The markers build new sealed match build.json, the reply directory's too: a
	// clean end has nothing to warn of.
	if strings.Contains(res.stdout, "Warning") || strings.Contains(res.stdout, "Left in place") {
		t.Errorf("a clean end warns or leaves something:\n%s", res.stdout)
	}
	// Ending it again is the invocation's mistake.
	if again := b.run(t, b.buildEndArgs("b1")...); again.code != 2 || !strings.Contains(again.stderr, "no build") {
		t.Errorf("ending a build twice exited %d: %s", again.code, again.stderr)
	}
	// And the id is free: a new build of it can start (its branches are the old
	// build's, so it needs them deleted first, which build end left to the human).
	gitIn(t, b.repo, "branch", "-D", "parley/b1/codex-a", "parley/b1/claude-a")
	buildUp(t, b, "b1", nil)
}

// TestBuildEndRefusesChangesWithoutForce: a tracked change and an untracked file
// that build new did not write each refuse, exit 1, naming the file and --force,
// with nothing removed, an untracked .director/repo-key in a target that does not
// ignore it included; --force removes the worktree.
func TestBuildEndRefusesChangesWithoutForce(t *testing.T) {
	cases := []struct {
		name  string
		nick  string
		touch func(t *testing.T, wt string)
		want  string
	}{
		{"a tracked file edited", "claude-a", func(t *testing.T, wt string) { writeFileT(t, filepath.Join(wt, "README.md"), "changed\n") }, "README.md"},
		{"a tracked file deleted", "codex-a", func(t *testing.T, wt string) { os.Remove(filepath.Join(wt, "README.md")) }, "README.md"},
		{"a new file staged", "claude-a", func(t *testing.T, wt string) {
			writeFileT(t, filepath.Join(wt, "staged.txt"), "s\n")
			gitIn(t, wt, "add", "staged.txt")
		}, "staged.txt"},
		{"an untracked file", "codex-a", func(t *testing.T, wt string) { writeFileT(t, filepath.Join(wt, "scratch.txt"), "x\n") }, "scratch.txt"},
		{"an untracked file in a new directory", "claude-a", func(t *testing.T, wt string) { writeFileT(t, filepath.Join(wt, "d", "e", "f.txt"), "x\n") }, "d/e/f.txt"},
		{"an untracked .director/repo-key in a target that does not ignore it", "codex-a", func(t *testing.T, wt string) {
			writeFileT(t, filepath.Join(wt, ".director", "repo-key"), "k\n")
		}, ".director/repo-key"},
		{"another file in .codex/", "codex-a", func(t *testing.T, wt string) { writeFileT(t, filepath.Join(wt, ".codex", "other.toml"), "x\n") }, ".codex/other.toml"},
		{"a .codex/config.toml in a Claude peer's worktree", "claude-a", func(t *testing.T, wt string) {
			writeFileT(t, filepath.Join(wt, ".codex", "config.toml"), "x\n")
		}, ".codex/config.toml"},
		{"a .mcp.json in a Codex peer's worktree", "codex-a", func(t *testing.T, wt string) { writeFileT(t, filepath.Join(wt, ".mcp.json"), "{}\n") }, ".mcp.json"},
		{"a .parley-build below the root", "claude-a", func(t *testing.T, wt string) { writeFileT(t, filepath.Join(wt, "sub", ".parley-build"), "x\n") }, "sub/.parley-build"},
		{"a launch file edited after it was committed", "claude-a", func(t *testing.T, wt string) {
			gitIn(t, wt, "add", ".mcp.json")
			gitIn(t, wt, "-c", "commit.gpgsign=false", "commit", "-q", "-m", "mcp")
			writeFileT(t, filepath.Join(wt, ".mcp.json"), "{\"edited\":1}\n")
		}, ".mcp.json"},
		{"a file whose name holds an escape sequence", "codex-a", func(t *testing.T, wt string) { writeFileT(t, filepath.Join(wt, "evil\x1b[31m.txt"), "x\n") }, `evil\x1b[31m.txt`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := newTableBench(t)
			rec, _ := buildUp(t, b, "b1", nil)
			wt := rec.peer(t, c.nick).Worktree
			c.touch(t, wt)
			res := b.run(t, b.buildEndArgs("b1")...)
			if res.code != 1 || !strings.Contains(res.stderr, c.want) || !strings.Contains(res.stderr, "--force") || !strings.Contains(res.stderr, c.nick) {
				t.Fatalf("exit %d, want 1 naming peer %s, %q and --force\n%s", res.code, c.nick, c.want, res.stderr)
			}
			if strings.Contains(res.stderr, "\x1b") {
				t.Errorf("a raw escape sequence reached the terminal: %q", res.stderr)
			}
			wantExist(t, rec.Peers[0].Worktree, rec.Peers[1].Worktree, rec.ReplyDir, filepath.Join(b.base, "builds", "b1", "build.json"))
			tr, _ := state.OpenThreadRecord(b.base, "codex-a", rec.Stream)
			if got, err := tr.Read(); err != nil || got != fakeThread(1) {
				t.Errorf("a refused end removed the thread record: %q, %v", got, err)
			}
			mustBuildEnd(t, b, "b1", "--force")
			wantGone(t, rec.Peers[0].Worktree, rec.Peers[1].Worktree, rec.ReplyDir)
		})
	}
}

// TestBuildEndNamesEveryPeerThatBlocksIt: two dirty worktrees are two names in
// one refusal.
func TestBuildEndNamesEveryPeerThatBlocksIt(t *testing.T) {
	b := newTableBench(t)
	rec, _ := buildUp(t, b, "b1", nil)
	writeFileT(t, filepath.Join(rec.Peers[0].Worktree, "one.txt"), "1\n")
	writeFileT(t, filepath.Join(rec.Peers[1].Worktree, "two.txt"), "2\n")
	res := b.run(t, b.buildEndArgs("b1")...)
	for _, want := range []string{"peer codex-a", "one.txt", "peer claude-a", "two.txt"} {
		if res.code != 1 || !strings.Contains(res.stderr, want) {
			t.Fatalf("exit %d, want 1 naming %q\n%s", res.code, want, res.stderr)
		}
	}
}

// TestBuildEndIgnoredFilesDoNotBlockAndAreCounted: what the target's ignore rules
// call disposable is removed with the worktree, counted and a few names quoted;
// the launch files an ignore rule hides are not counted. This differs from table
// end on purpose: no seat writes a table's worktree, so table end counts every
// ignored file as a change, but a build's peers write theirs.
func TestBuildEndIgnoredFilesDoNotBlockAndAreCounted(t *testing.T) {
	b := newTableBench(t)
	if err := os.RemoveAll(b.repo); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(b.repo, 0o755); err != nil {
		t.Fatal(err)
	}
	initRepo(t, b.repo, map[string]string{"README.md": "target\n", ".gitignore": "*.log\nbuild/\n/.codex/*\n"})
	rec, _ := buildUp(t, b, "b1", nil)
	cw := rec.peer(t, "codex-a").Worktree
	for _, f := range []string{"a.log", "b.log", "c.log", "build/x/y.o", "build/z.o"} {
		writeFileT(t, filepath.Join(cw, f), "x\n")
	}
	writeFileT(t, filepath.Join(rec.peer(t, "claude-a").Worktree, "only.log"), "x\n")

	res := mustBuildEnd(t, b, "b1")
	t.Logf("stdout:\n%s", res.stdout)
	// Four entries for codex-a: three logs and the collapsed build/ (the .codex/
	// config build new wrote is not counted); one for claude-a.
	for _, want := range []string{"Peer codex-a's worktree held 4 ignored paths, removed with it", `"a.log", "b.log", "build/", and 1 more`,
		"Peer claude-a's worktree held 1 ignored path, removed with it", `"only.log"`} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, res.stdout)
		}
	}
	if strings.Contains(res.stdout, ".codex/") || strings.Contains(res.stdout, "0 ignored") {
		t.Errorf("stdout counts the config build new wrote:\n%s", res.stdout)
	}
	wantGone(t, rec.Peers[0].Worktree, rec.Peers[1].Worktree)
}

// TestBuildEndCountsWhatAPeerLeftInAnIgnoredCodexDirectory: a target that ignores
// .codex/ collapses the config build new wrote and a peer's own files there into
// one entry; the report still counts the peer's files and never the config.
func TestBuildEndCountsWhatAPeerLeftInAnIgnoredCodexDirectory(t *testing.T) {
	for _, extra := range []bool{false, true} {
		b := newTableBench(t)
		if err := os.RemoveAll(b.repo); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(b.repo, 0o755); err != nil {
			t.Fatal(err)
		}
		initRepo(t, b.repo, map[string]string{"README.md": "target\n", ".gitignore": "/.codex/\n"})
		rec, _ := buildUp(t, b, "b1", nil)
		if extra {
			writeFileT(t, filepath.Join(rec.peer(t, "codex-a").Worktree, ".codex", "session.log"), "x\n")
		}
		res := mustBuildEnd(t, b, "b1")
		if strings.Contains(res.stdout, "config.toml") {
			t.Errorf("extra=%v: stdout counts the config build new wrote:\n%s", extra, res.stdout)
		}
		counted := strings.Contains(res.stdout, "Peer codex-a's worktree held 1 ignored path") && strings.Contains(res.stdout, `".codex/session.log"`)
		if counted != extra {
			t.Errorf("extra=%v: the peer's .codex/session.log counted: %v\n%s", extra, counted, res.stdout)
		}
	}
}

// TestBuildEndAnIgnoredDirectorKeyDoesNotBlock: the same .director/repo-key that
// refuses in a target that does not ignore it is only counted in one that does.
func TestBuildEndAnIgnoredDirectorKeyDoesNotBlock(t *testing.T) {
	b := newTableBench(t)
	if err := os.WriteFile(filepath.Join(b.repo, ".git", "info", "exclude"), []byte("/.director/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec, _ := buildUp(t, b, "b1", nil)
	writeFileT(t, filepath.Join(rec.peer(t, "claude-a").Worktree, ".director", "repo-key"), "k\n")
	res := mustBuildEnd(t, b, "b1")
	if !strings.Contains(res.stdout, `held 1 ignored path`) || !strings.Contains(res.stdout, `".director/"`) {
		t.Errorf("stdout does not count the ignored .director/:\n%s", res.stdout)
	}
}

// TestBuildEndReportsACommitOnALaunchFileAndDoesNotRefuse: a peer's commit that
// touches .mcp.json, .codex/config.toml or .parley-build is a warning with the
// short hash and the quoted subject, capped; it never refuses.
func TestBuildEndReportsACommitOnALaunchFileAndDoesNotRefuse(t *testing.T) {
	b := newTableBench(t)
	rec, _ := buildUp(t, b, "b1", nil)
	cw, lw := rec.peer(t, "codex-a").Worktree, rec.peer(t, "claude-a").Worktree

	commitFile(t, lw, ".mcp.json", "{\"a\":1}\n", "touch the mcp")
	mcpHash := gitIn(t, lw, "rev-parse", "--short", "HEAD")
	commitFile(t, lw, "innocent.txt", "i\n", "an innocent commit")
	innocent := gitIn(t, lw, "rev-parse", "--short", "HEAD")
	commitFile(t, cw, ".codex/config.toml", "sandbox_mode = \"danger-full-access\"\n", "widen the sandbox \x1b[31m"+strings.Repeat("x", 200))
	cfgHash := gitIn(t, cw, "rev-parse", "--short", "HEAD")
	commitFile(t, cw, ".parley-build", "forged\n", "forge the marker")
	markHash := gitIn(t, cw, "rev-parse", "--short", "HEAD")

	res := mustBuildEnd(t, b, "b1")
	t.Logf("stdout:\n%s", res.stdout)
	for _, want := range []string{mcpHash + ` "touch the mcp"`, cfgHash + ` "widen the sandbox \x1b[31mxxx`, markHash + ` "forge the marker"`,
		"peer claude-a's branch parley/b1/claude-a has commits that touch a launch file", "peer codex-a's branch parley/b1/codex-a has commits that touch a launch file"} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, res.stdout)
		}
	}
	if strings.Contains(res.stdout, innocent) {
		t.Errorf("stdout names the commit %s that touched no launch file:\n%s", innocent, res.stdout)
	}
	if strings.Contains(res.stdout, "\x1b") || strings.Contains(res.stdout, strings.Repeat("x", 100)) {
		t.Errorf("a subject was not quoted and cut short:\n%q", res.stdout)
	}
	wantGone(t, rec.Peers[0].Worktree, rec.Peers[1].Worktree)
	for _, br := range []string{"parley/b1/codex-a", "parley/b1/claude-a"} {
		if out := gitIn(t, b.repo, "for-each-ref", "refs/heads/"+br); out == "" {
			t.Errorf("branch %s was removed", br)
		}
	}
}

// TestBuildEndCapsTheLaunchFileReport: more commits than the cap are five and a
// line saying there are more.
func TestBuildEndCapsTheLaunchFileReport(t *testing.T) {
	b := newTableBench(t)
	rec, _ := buildUp(t, b, "b1", []string{"claude-a=claude", "claude-b=claude"})
	wt := rec.peer(t, "claude-a").Worktree
	for i := 0; i < 8; i++ {
		commitFile(t, wt, ".mcp.json", "{\"n\":"+strconv.Itoa(i)+"}\n", "touch "+strconv.Itoa(i))
	}
	res := mustBuildEnd(t, b, "b1")
	if n := strings.Count(res.stdout, ` "touch `); n != launchFilesShown {
		t.Errorf("%d commits named, want %d:\n%s", n, launchFilesShown, res.stdout)
	}
	if !strings.Contains(res.stdout, "and more: git -C '"+b.repo+"' log") {
		t.Errorf("stdout does not say there are more, and how to see them:\n%s", res.stdout)
	}
}

// TestBuildEndFinishesWhenAMarkerIsGone: a peer's git clean can remove the marker,
// and a changed one is no better; the worktree is still the recorded path that
// the target lists, so it is removed, with a note. A reply directory whose marker
// is gone is reported and left.
func TestBuildEndFinishesWhenAMarkerIsGone(t *testing.T) {
	t.Run("worktree markers", func(t *testing.T) {
		b := newTableBench(t)
		rec, _ := buildUp(t, b, "b1", nil)
		if err := os.Remove(filepath.Join(rec.peer(t, "codex-a").Worktree, ".parley-build")); err != nil {
			t.Fatal(err)
		}
		// Tracked or not, a marker holding another build's text is a note.
		writeFileT(t, filepath.Join(rec.peer(t, "claude-a").Worktree, ".parley-build"), "b9\nclaude-a codex-a\n")
		res := mustBuildEnd(t, b, "b1")
		t.Logf("stdout:\n%s", res.stdout)
		for _, want := range []string{"peer codex-a's worktree " + rec.peer(t, "codex-a").Worktree + " is removed all the same, but it holds no marker file",
			"peer claude-a's worktree " + rec.peer(t, "claude-a").Worktree + " is removed all the same, but its marker", "not the build id \"b1\""} {
			if !strings.Contains(res.stdout, want) {
				t.Errorf("stdout lacks %q:\n%s", want, res.stdout)
			}
		}
		wantGone(t, rec.Peers[0].Worktree, rec.Peers[1].Worktree, rec.ReplyDir, filepath.Join(b.base, "builds", "b1"))
	})
	t.Run("the reply directory's marker", func(t *testing.T) {
		b := newTableBench(t)
		rec, _ := buildUp(t, b, "b1", nil)
		if err := os.Remove(filepath.Join(rec.ReplyDir, ".parley-build")); err != nil {
			t.Fatal(err)
		}
		res := mustBuildEnd(t, b, "b1")
		t.Logf("stdout:\n%s", res.stdout)
		if !strings.Contains(res.stdout, "Left in place") || !strings.Contains(res.stdout, "reply directory "+rec.ReplyDir) || !strings.Contains(res.stdout, "holds no marker file") {
			t.Errorf("stdout does not say the reply directory was left and why:\n%s", res.stdout)
		}
		wantExist(t, rec.ReplyDir)
		wantGone(t, rec.Peers[0].Worktree, rec.Peers[1].Worktree, filepath.Join(b.base, "builds", "b1"))
		if strings.Contains(res.stdout, "reply directory "+rec.ReplyDir+" (already gone)") {
			t.Errorf("stdout calls the reply directory gone:\n%s", res.stdout)
		}
	})
	t.Run("the reply directory's marker names another build", func(t *testing.T) {
		b := newTableBench(t)
		rec, _ := buildUp(t, b, "b1", nil)
		writeFileT(t, filepath.Join(rec.ReplyDir, ".parley-build"), "b9\nclaude-a codex-a\nsha256:00\n")
		res := mustBuildEnd(t, b, "b1")
		if !strings.Contains(res.stdout, "Left in place") || !strings.Contains(res.stdout, "not the build id") {
			t.Errorf("stdout does not say the reply directory was left and why:\n%s", res.stdout)
		}
		wantExist(t, rec.ReplyDir)
	})
	t.Run("a reply directory that is a link", func(t *testing.T) {
		b := newTableBench(t)
		rec, _ := buildUp(t, b, "b1", nil)
		precious := filepath.Join(b.root, "precious")
		writeFileT(t, filepath.Join(precious, "keep.txt"), "keep\n")
		if err := os.RemoveAll(rec.ReplyDir); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(precious, rec.ReplyDir); err != nil {
			t.Fatal(err)
		}
		res := mustBuildEnd(t, b, "b1")
		if !strings.Contains(res.stdout, "it is not a directory") {
			t.Errorf("stdout does not say the link was left:\n%s", res.stdout)
		}
		wantExist(t, filepath.Join(precious, "keep.txt"), rec.ReplyDir)
	})
}

// TestBuildEndRefusesWhileAPeerIsRunning: a held nick lock is a running pump or
// Claude Code session, named, and nothing is removed, --force or not.
func TestBuildEndRefusesWhileAPeerIsRunning(t *testing.T) {
	b := newTableBench(t)
	rec, _ := buildUp(t, b, "b1", nil)
	held, err := state.LockNick(b.base, "claude-a")
	if err != nil {
		t.Fatal(err)
	}
	for _, extra := range [][]string{nil, {"--force"}} {
		res := b.run(t, b.buildEndArgs("b1", extra...)...)
		if res.code != 1 || !strings.Contains(res.stderr, `"claude-a"`) || !strings.Contains(res.stderr, "is still running") {
			t.Fatalf("exit %d, want 1 naming claude-a\n%s", res.code, res.stderr)
		}
		wantExist(t, rec.Peers[0].Worktree, rec.Peers[1].Worktree, rec.ReplyDir, filepath.Join(b.base, "builds", "b1", "build.json"))
	}
	// The lock it did take on codex-a was released.
	l, err := state.LockNick(b.base, "codex-a")
	if err != nil {
		t.Fatalf("the refused end left codex-a's lock held: %v", err)
	}
	l.Release()
	held.Release()
	mustBuildEnd(t, b, "b1")
}

// TestBuildEndTakesBothLocksAndHoldsThem: a peer that starts during the teardown
// is refused (the locks are held to the end), which build end shows by being run
// against a held lock above; here the locks are free again once it returns.
func TestBuildEndLeavesLockFilesReleased(t *testing.T) {
	b := newTableBench(t)
	buildUp(t, b, "b1", nil)
	mustBuildEnd(t, b, "b1")
	for _, nick := range []string{"codex-a", "claude-a"} {
		l, err := state.LockNick(b.base, nick)
		if err != nil {
			t.Fatalf("%s: %v", nick, err)
		}
		l.Release()
	}
}

// TestBuildEndLeavesAWorktreeItCannotClaim: a path that is the recorded one but
// that the target does not list, or that is a link, or that is not a directory,
// is reported and left; the rest goes on, and the build directory stays for a
// second run.
func TestBuildEndLeavesAWorktreeItCannotClaim(t *testing.T) {
	t.Run("not listed by the target", func(t *testing.T) {
		b := newTableBench(t)
		rec, _ := buildUp(t, b, "b1", nil)
		wt := rec.peer(t, "claude-a").Worktree
		gitIn(t, b.repo, "worktree", "remove", "--force", wt)
		writeFileT(t, filepath.Join(wt, "mine.txt"), "mine\n")

		res := mustBuildEnd(t, b, "b1")
		t.Logf("stdout:\n%s", res.stdout)
		if !strings.Contains(res.stdout, "Left in place") || !strings.Contains(res.stdout, "worktree "+wt+" of peer claude-a: git does not list it") {
			t.Errorf("stdout does not say the worktree was left and why:\n%s", res.stdout)
		}
		wantExist(t, filepath.Join(wt, "mine.txt"), filepath.Join(b.base, "builds", "b1", "build.json"))
		wantGone(t, rec.peer(t, "codex-a").Worktree, rec.ReplyDir)
		// Once the human has dealt with it, the same command finishes.
		if err := os.RemoveAll(wt); err != nil {
			t.Fatal(err)
		}
		mustBuildEnd(t, b, "b1")
		wantGone(t, filepath.Join(b.base, "builds", "b1"))
	})
	t.Run("a link", func(t *testing.T) {
		b := newTableBench(t)
		rec, _ := buildUp(t, b, "b1", nil)
		wt := rec.peer(t, "codex-a").Worktree
		precious := filepath.Join(b.root, "precious")
		writeFileT(t, filepath.Join(precious, "keep.txt"), "keep\n")
		if err := os.RemoveAll(wt); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(precious, wt); err != nil {
			t.Fatal(err)
		}
		res := mustBuildEnd(t, b, "b1")
		if !strings.Contains(res.stdout, "it is not a directory") {
			t.Errorf("stdout does not say the link was left:\n%s", res.stdout)
		}
		wantExist(t, filepath.Join(precious, "keep.txt"), wt)
		wantGone(t, rec.peer(t, "claude-a").Worktree)
	})
	t.Run("another build's worktree", func(t *testing.T) {
		// A record that names a worktree git lists but that is the target's other
		// worktree, not under this build's directory, is not valid at all.
		b := newTableBench(t)
		rec, _ := buildUp(t, b, "b1", nil)
		other := filepath.Join(b.root, "other-wt")
		gitIn(t, b.repo, "worktree", "add", "-q", "--detach", other)
		tamperBuildRecord(t, b, "b1", func(r *buildRecord) { r.Peers[0].Worktree = other })
		res := b.run(t, b.buildEndArgs("b1", "--force")...)
		if res.code != 1 || !strings.Contains(res.stderr, "is not valid") {
			t.Fatalf("exit %d, want 1: a record naming a worktree outside the build is not valid\n%s", res.code, res.stderr)
		}
		wantExist(t, other, rec.Peers[1].Worktree, rec.ReplyDir)
	})
}

func tamperBuildRecord(t *testing.T, b *tableBench, id string, edit func(r *buildRecord)) {
	t.Helper()
	path := filepath.Join(b.base, "builds", id, "build.json")
	var rec buildRecord
	if err := json.Unmarshal(mustRead(t, path), &rec); err != nil {
		t.Fatal(err)
	}
	edit(&rec)
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestBuildEndRefusesARecordItDoesNotTrust: build.json is parley's own, so a bad
// one is damage; it is refused, exit 1, before a lock is taken or anything is
// removed, whatever --force says.
func TestBuildEndRefusesARecordItDoesNotTrust(t *testing.T) {
	for _, c := range []struct {
		name string
		edit func(b *tableBench, r *buildRecord)
	}{
		{"the target's own checkout as a worktree", func(b *tableBench, r *buildRecord) { r.Peers[0].Worktree = b.repo }},
		{"a reply directory that is not a build's", func(b *tableBench, r *buildRecord) { r.ReplyDir = b.root }},
		{"another build's id", func(b *tableBench, r *buildRecord) { r.ID = "b2" }},
		{"one peer", func(b *tableBench, r *buildRecord) { r.Peers = r.Peers[:1] }},
		{"a peer called builds", func(b *tableBench, r *buildRecord) { r.Peers[0].Nick = "builds" }},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := newTableBench(t)
			rec, _ := buildUp(t, b, "b1", nil)
			tamperBuildRecord(t, b, "b1", func(r *buildRecord) { c.edit(b, r) })
			res := b.run(t, b.buildEndArgs("b1", "--force")...)
			if res.code != 1 || !strings.Contains(res.stderr, "is not valid") {
				t.Fatalf("exit %d, want 1 and \"is not valid\"\n%s", res.code, res.stderr)
			}
			wantExist(t, b.repo, filepath.Join(b.repo, "README.md"), rec.Peers[0].Worktree, rec.Peers[1].Worktree, rec.ReplyDir)
		})
	}
	t.Run("a record that is a FIFO", func(t *testing.T) {
		b := newTableBench(t)
		buildUp(t, b, "b1", nil)
		path := filepath.Join(b.base, "builds", "b1", "build.json")
		os.Remove(path)
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			t.Fatal(err)
		}
		res := b.run(t, b.buildEndArgs("b1")...)
		if res.code != 1 || !strings.Contains(res.stderr, "not a regular file") {
			t.Fatalf("exit %d\n%s", res.code, res.stderr)
		}
	})
	t.Run("a record past the bound", func(t *testing.T) {
		b := newTableBench(t)
		buildUp(t, b, "b1", nil)
		path := filepath.Join(b.base, "builds", "b1", "build.json")
		if err := os.WriteFile(path, []byte(strings.Repeat(" ", buildRecordMax+1)), 0o600); err != nil {
			t.Fatal(err)
		}
		res := b.run(t, b.buildEndArgs("b1")...)
		if res.code != 1 || !strings.Contains(res.stderr, "larger than") {
			t.Fatalf("exit %d\n%s", res.code, res.stderr)
		}
	})
}

// TestBuildEndWhenTheTargetIsGone: without the repo git cannot be asked about a
// worktree, so it is refused without --force; with it the rest is taken down by
// hand, under the build's directory only.
func TestBuildEndWhenTheTargetIsGone(t *testing.T) {
	b := newTableBench(t)
	rec, _ := buildUp(t, b, "b1", nil)
	if err := os.RemoveAll(b.repo); err != nil {
		t.Fatal(err)
	}
	if res := b.run(t, b.buildEndArgs("b1")...); res.code != 1 || !strings.Contains(res.stderr, "cannot be asked about its changes") {
		t.Fatalf("exit %d, want 1 for a worktree that cannot be inspected\n%s", res.code, res.stderr)
	}
	wantExist(t, rec.Peers[0].Worktree, rec.Peers[1].Worktree)
	mustBuildEnd(t, b, "b1", "--force")
	wantGone(t, rec.Peers[0].Worktree, rec.Peers[1].Worktree, rec.ReplyDir, filepath.Join(b.base, "builds", "b1"))
}

// TestBuildEndDoesNotRemoveWhatItWasNotAskedTo: a bystander in the state base, in
// another build's directory and in the target survives.
func TestBuildEndDoesNotRemoveWhatItWasNotAskedTo(t *testing.T) {
	b := newTableBench(t)
	buildUp(t, b, "b1", nil)
	rec2, _ := buildUp(t, b, "b2", []string{"codex-b=codex", "claude-b=claude"})
	writeFileT(t, filepath.Join(b.base, "bystander.txt"), "b\n")
	writeFileT(t, filepath.Join(b.base, "claude-a", "irc", "password"), "pw\n")
	mustBuildEnd(t, b, "b1")
	wantExist(t, filepath.Join(b.base, "bystander.txt"), filepath.Join(b.base, "claude-a", "irc", "password"), b.repo, filepath.Join(b.repo, "README.md"),
		rec2.Peers[0].Worktree, rec2.Peers[1].Worktree, rec2.ReplyDir, filepath.Join(b.base, "builds", "b2", "build.json"))
	tr, _ := state.OpenThreadRecord(b.base, "codex-b", rec2.Stream)
	if got, err := tr.Read(); err != nil || got == "" {
		t.Errorf("build b2's thread record was removed by ending b1: %q, %v", got, err)
	}
}
