package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// linkedRoot makes a symlink in the bench's root to the root itself, so that
// <link>/co and <root>/co are two spellings of one directory.
func (b *lineBench) linkedRoot(t *testing.T) string {
	t.Helper()
	link := filepath.Join(b.root, "lnk")
	if err := os.Symlink(b.root, link); err != nil {
		t.Fatal(err)
	}
	return link
}

// TestLineAddTwoSpellingsOfOneCheckoutShareTheFileAndKeepTheRecordWhole: the
// checkout is recorded as the directory it resolves to, so a session added
// through a symlinked parent shares the first one's file, the record still holds,
// and drop and end both finish.
func TestLineAddTwoSpellingsOfOneCheckoutShareTheFileAndKeepTheRecordWhole(t *testing.T) {
	for _, harness := range []string{"claude", "codex"} {
		for _, first := range []string{"real", "link"} {
			for _, verb := range []string{"drop", "end"} {
				t.Run(harness+"/first via "+first+"/"+verb, func(t *testing.T) {
					b := newLineBench(t)
					lineUp(t, b, "demo")
					real := b.gitCheckout(t, "co")
					viaLink := filepath.Join(b.linkedRoot(t), "co")
					spellings := []string{real, viaLink}
					if first == "link" {
						spellings = []string{viaLink, real}
					}
					b.mustAdd(t, "demo", "plan", harness, spellings[0])
					res := b.mustAdd(t, "demo", "review", harness, spellings[1])
					if !strings.Contains(res.stdout, "Shares ") {
						t.Errorf("the second session did not share the file:\n%s", res.stdout)
					}
					if _, _, err := readLineRecord(b.base, "demo"); err != nil {
						t.Fatalf("the record is not valid after the second add: %v", err)
					}
					rec := b.record(t, "demo")
					for _, s := range rec.Sessions {
						if s.Checkout != real {
							t.Errorf("session %s is recorded at %s, want the resolved %s", s.Nick, s.Checkout, real)
						}
					}
					if verb == "drop" {
						mustLine(t, b, b.ldrop("demo", "plan")...)
						mustLine(t, b, b.ldrop("demo", "review")...)
					} else {
						mustLine(t, b, b.lend("demo")...)
					}
					for _, gone := range []string{filepath.Join(real, ".mcp.json"), filepath.Join(real, ".codex")} {
						if _, err := os.Lstat(gone); err == nil {
							t.Errorf("%s is still there", gone)
						}
					}
				})
			}
		}
	}
}

// TestLineAddRefusesToWriteARecordItsReadersWouldRefuse is the guard behind it
// all: an add that would leave a record nothing can read fails and rolls back.
// A checkout whose resolved path holds a '$' is refused before anything is made.
func TestLineAddRefusesACheckoutThatResolvesToAPathWithADollar(t *testing.T) {
	b := newLineBench(t)
	lineUp(t, b, "demo")
	target := b.gitCheckout(t, "co$x")
	link := filepath.Join(b.root, "co-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	before := b.lsnap(t, target)
	res := b.run(t, b.ladd("demo", "plan", "claude", link)...)
	if res.code != 2 || !strings.Contains(res.stderr, "resolves to") || !strings.Contains(res.stderr, "'$'") {
		t.Fatalf("exit %d, want 2\n%s", res.code, res.stderr)
	}
	b.wantLineUnchanged(t, before, target)
}

// worktreeOf makes a linked worktree of repo named name in the bench's root.
func (b *lineBench) worktreeOf(t *testing.T, repo, name string) string {
	t.Helper()
	wt := filepath.Join(b.root, name)
	gitIn(t, repo, "worktree", "add", "-q", "--detach", wt, "HEAD")
	return wt
}

// TestLineDropKeepsTheExcludeLineWhileAnotherWorktreeNeedsIt: the exclude file is
// the repository's, shared by every worktree. Of two sessions in two worktrees,
// the one that found the line there inherits the duty to take it out, and the line
// goes with whichever is dropped last, in either order.
func TestLineDropKeepsTheExcludeLineWhileAnotherWorktreeNeedsIt(t *testing.T) {
	for _, order := range [][]string{{"plan", "review"}, {"review", "plan"}} {
		t.Run(strings.Join(order, " then "), func(t *testing.T) {
			b := newLineBench(t)
			lineUp(t, b, "demo")
			co := b.gitCheckout(t, "co")
			wt2 := b.worktreeOf(t, co, "co-wt2")
			pristine := readExcludeForTest(t, co)
			b.mustAdd(t, "demo", "plan", "claude", co)
			b.mustAdd(t, "demo", "review", "claude", wt2)
			rec := b.record(t, "demo")
			plan, review := sessionByNick(t, rec, "plan"), sessionByNick(t, rec, "review")
			if plan.Files[0].ExcludeFile == "" || review.Files[0].ExcludeFile != plan.Files[0].ExcludeFile {
				t.Fatalf("exclude files recorded: %q and %q, want the one the first session added to on both", plan.Files[0].ExcludeFile, review.Files[0].ExcludeFile)
			}
			if got := gitIn(t, wt2, "status", "--porcelain"); got != "" {
				t.Fatalf("the second worktree is not clean: %s", got)
			}

			first, last := order[0], order[1]
			res := mustLine(t, b, b.ldrop("demo", first)...)
			t.Logf("stdout:\n%s", res.stdout)
			if !strings.Contains(readExcludeForTest(t, co), "/.mcp.json\n") {
				t.Fatal("the first drop took out a line the other worktree needs")
			}
			left := map[string]string{"plan": wt2, "review": co}[first]
			if !strings.Contains(res.stdout, "Left in place:") || !strings.Contains(res.stdout, "a file at "+filepath.Join(left, ".mcp.json")+" is kept out of git status by it") {
				t.Errorf("stdout does not say the line stays and why:\n%s", res.stdout)
			}
			if got := gitIn(t, left, "status", "--porcelain"); got != "" {
				t.Errorf("the other worktree shows its file in git status: %s", got)
			}

			res = mustLine(t, b, b.ldrop("demo", last)...)
			if got := readExcludeForTest(t, co); got != pristine {
				t.Errorf("info/exclude = %q, want it as it was: %q", got, pristine)
			}
			if !strings.Contains(res.stdout, "the line /.mcp.json in ") || strings.Contains(res.stdout, "Left in place") {
				t.Errorf("stdout:\n%s", res.stdout)
			}
		})
	}
}

// TestLineEndTakesTheSharedExcludeLineOutOnceAndSaysNothingOfIt: a whole line goes
// session by session; the line stays while a later session's file needs it and
// goes with the last, without a note about the interim.
func TestLineEndTakesTheSharedExcludeLineOutOnceAndSaysNothingOfIt(t *testing.T) {
	b := newLineBench(t)
	lineUp(t, b, "demo")
	co := b.gitCheckout(t, "co")
	wt2 := b.worktreeOf(t, co, "co-wt2")
	pristine := readExcludeForTest(t, co)
	b.mustAdd(t, "demo", "plan", "claude", co)
	b.mustAdd(t, "demo", "review", "claude", wt2)
	res := mustLine(t, b, b.lend("demo")...)
	if got := readExcludeForTest(t, co); got != pristine {
		t.Errorf("info/exclude = %q, want it as it was: %q", got, pristine)
	}
	if strings.Contains(res.stdout, "Left in place") {
		t.Errorf("stdout speaks of the interim:\n%s", res.stdout)
	}
	if strings.Count(res.stdout, "the line /.mcp.json in ") != 1 {
		t.Errorf("stdout does not list the line as removed once:\n%s", res.stdout)
	}
}

// TestLineDropLeavesTheExcludeLineForAFileAnotherLineOrTheUserOwns: any file at
// the pattern's path in any worktree keeps the line, whoever put it there.
func TestLineDropLeavesTheExcludeLineForAFileAnotherLineOrTheUserOwns(t *testing.T) {
	b := newLineBench(t)
	lineUp(t, b, "demo")
	co := b.gitCheckout(t, "co")
	wt2 := b.worktreeOf(t, co, "co-wt2")
	b.mustAdd(t, "demo", "plan", "claude", co)
	mine := filepath.Join(wt2, ".mcp.json")
	if err := os.WriteFile(mine, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := mustLine(t, b, b.ldrop("demo", "plan")...)
	if !strings.Contains(readExcludeForTest(t, co), "/.mcp.json\n") || !strings.Contains(res.stdout, mine) {
		t.Errorf("the line was taken out from under a file of the user's:\n%s", res.stdout)
	}
	if err := os.Remove(mine); err != nil {
		t.Fatal(err)
	}
}

// TestLineDropFindsTheOtherWorktreesWhenItsOwnCheckoutIsGone: the session's
// checkout may have been removed; git is then asked from the common directory the
// exclude file sits in, and a worktree that is gone needs nothing.
func TestLineDropFindsTheOtherWorktreesWhenItsOwnCheckoutIsGone(t *testing.T) {
	for _, otherHasFile := range []bool{false, true} {
		name := "nothing else needs the line"
		if otherHasFile {
			name = "another worktree needs the line"
		}
		t.Run(name, func(t *testing.T) {
			b := newLineBench(t)
			lineUp(t, b, "demo")
			co := b.gitCheckout(t, "co")
			wt2 := b.worktreeOf(t, co, "co-wt2")
			pristine := readExcludeForTest(t, co)
			b.mustAdd(t, "demo", "review", "claude", wt2)
			if otherHasFile {
				if err := os.WriteFile(filepath.Join(co, ".mcp.json"), []byte("{}"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.RemoveAll(wt2); err != nil {
				t.Fatal(err)
			}
			res := mustLine(t, b, b.ldrop("demo", "review")...)
			got := readExcludeForTest(t, co)
			if otherHasFile {
				if !strings.Contains(got, "/.mcp.json\n") || !strings.Contains(res.stdout, filepath.Join(co, ".mcp.json")) {
					t.Errorf("info/exclude = %q\n%s", got, res.stdout)
				}
			} else if got != pristine {
				t.Errorf("info/exclude = %q, want it as it was: %q", got, pristine)
			}
		})
	}
}

// TestLineAddCreatesAMissingExcludeFileAndDropRemovesIt: a repository with no
// info/exclude gets one for the line, and loses it with the line unless something
// else was written to it meanwhile.
func TestLineAddCreatesAMissingExcludeFileAndDropRemovesIt(t *testing.T) {
	for _, extra := range []string{"", "mine\n"} {
		t.Run("extra "+extra, func(t *testing.T) {
			b := newLineBench(t)
			lineUp(t, b, "demo")
			co := b.gitCheckout(t, "co")
			exclude := filepath.Join(co, ".git", "info", "exclude")
			if err := os.Remove(exclude); err != nil {
				t.Fatal(err)
			}
			b.mustAdd(t, "demo", "plan", "claude", co)
			if got := readExcludeForTest(t, co); got != "/.mcp.json\n" {
				t.Fatalf("info/exclude = %q", got)
			}
			if f := b.record(t, "demo").Sessions[0].Files[0]; !f.ExcludeMade || f.ExcludeFile != exclude {
				t.Errorf("file entry %+v, want the made exclude file recorded", f)
			}
			if extra != "" {
				f, err := os.OpenFile(exclude, os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				f.WriteString(extra)
				f.Close()
			}
			mustLine(t, b, b.ldrop("demo", "plan")...)
			_, err := os.Lstat(exclude)
			if extra == "" && err == nil {
				t.Error("the exclude file the line made is still there")
			}
			if extra != "" {
				if got := readExcludeForTest(t, co); got != extra {
					t.Errorf("info/exclude = %q, want %q", got, extra)
				}
			}
		})
	}
}

// TestLineAddRollbackRemovesAnExcludeFileItMade.
func TestLineAddRollbackRemovesAnExcludeFileItMade(t *testing.T) {
	b := newLineBench(t)
	lineUp(t, b, "demo")
	co := b.gitCheckout(t, "co")
	if err := os.Remove(filepath.Join(co, ".git", "info", "exclude")); err != nil {
		t.Fatal(err)
	}
	b.setMode(t, 1, "fail")
	before := b.lsnap(t, co)
	res := b.run(t, b.ladd("demo", "build", "codex", co)...)
	if res.code != 1 {
		t.Fatalf("exit %d, want 1\n%s", res.code, res.stderr)
	}
	if got, want := withoutCalls(b.lsnap(t, co)), withoutCalls(before); got != want {
		t.Fatalf("the run left a trace.\nbefore:\n%s\nafter:\n%s", want, got)
	}
}

// TestLineAddWritesTheCodexConfigWithANonceAndDropRefusesALookalike: the config's
// body is a constant, so the line writes it behind a comment that names the line
// and carries a nonce of its own, and the digest of the whole is what the record
// holds. A file with the constant body and no nonce is not the line's.
func TestLineAddWritesTheCodexConfigWithANonceAndDropRefusesALookalike(t *testing.T) {
	b := newLineBench(t)
	lineUp(t, b, "demo")
	co1, co2 := b.gitCheckout(t, "co1"), b.gitCheckout(t, "co2")
	b.mustAdd(t, "demo", "build", "codex", co1)
	b.mustAdd(t, "demo", "ops", "codex", co2)
	n1 := wantCodexConfig(t, string(mustRead(t, filepath.Join(co1, ".codex", "config.toml"))), "demo")
	n2 := wantCodexConfig(t, string(mustRead(t, filepath.Join(co2, ".codex", "config.toml"))), "demo")
	if n1 == n2 {
		t.Errorf("two configs carry the same nonce, %s", n1)
	}

	// The same bytes without the comment: a copy made by anyone but the line.
	cfg := filepath.Join(co1, ".codex", "config.toml")
	if err := os.WriteFile(cfg, []byte(codexConfigWant), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{b.ldrop("demo", "build"), b.lend("demo")} {
		before := b.lsnap(t, b.checkoutDirs(t)...)
		res := b.run(t, args...)
		if res.code != 1 || !strings.Contains(res.stderr, cfg) || !strings.Contains(res.stderr, "edited or replaced") {
			t.Fatalf("%v: exit %d, want 1 refusing the lookalike\n%s", args[:2], res.code, res.stderr)
		}
		b.wantLineUnchanged(t, before, b.checkoutDirs(t)...)
	}
}

// TestLineEndClearsALineDirectoryThatHasNoRecord: a line new killed between making
// the directory and writing the record leaves a directory nothing can read. line
// new says to run line end, and line end removes it, and only when nothing else is
// in it.
func TestLineEndClearsALineDirectoryThatHasNoRecord(t *testing.T) {
	for _, c := range []struct {
		name  string
		files map[string]string
		clear bool
	}{
		{"empty", nil, true},
		{"the temp file of a record's write", map[string]string{".tmp-line.json-123": "{"}, true},
		{"something else", map[string]string{"notes.txt": "mine"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := newLineBench(t)
			dir := filepath.Join(b.base, "lines", "demo")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			for name, content := range c.files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			res := b.run(t, b.lnew("demo")...)
			if res.code != 2 || !strings.Contains(res.stderr, "parley line end demo") {
				t.Fatalf("line new: exit %d, want 2 saying to run line end\n%s", res.code, res.stderr)
			}
			co := b.gitCheckout(t, "co")
			res = b.run(t, b.ladd("demo", "plan", "claude", co)...)
			if res.code != 2 || !strings.Contains(res.stderr, "parley line end demo") {
				t.Fatalf("line add: exit %d, want 2 pointing to line end\n%s", res.code, res.stderr)
			}
			res = b.run(t, b.lend("demo")...)
			if !c.clear {
				if res.code != 2 {
					t.Fatalf("line end: exit %d, want 2\n%s", res.code, res.stderr)
				}
				for name := range c.files {
					if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
						t.Errorf("line end removed %s: %v", name, err)
					}
				}
				return
			}
			if res.code != 0 || !strings.Contains(res.stdout, "no record") || !strings.Contains(res.stdout, dir) {
				t.Fatalf("line end: exit %d, want 0 saying what it removed\nstdout: %s\nstderr: %s", res.code, res.stdout, res.stderr)
			}
			if _, err := os.Lstat(dir); err == nil {
				t.Error("the directory is still there")
			}
			lineUp(t, b, "demo")
		})
	}
}

// TestLineDropRefusesACheckoutThatWasMovedAndReplacedByALink: the record holds the
// checkout resolved, so a checkout renamed and replaced by a link to the moved
// directory (or a link put in a directory above it) is another directory behind
// the same name. drop and end refuse, naming the checkout and what it resolves to,
// before anything is removed; --force leaves the session's files where they are.
func TestLineDropRefusesACheckoutThatWasMovedAndReplacedByALink(t *testing.T) {
	for _, harness := range []string{"claude", "codex"} {
		for _, verb := range []string{"drop", "end"} {
			t.Run(harness+"/"+verb, func(t *testing.T) {
				b := newLineBench(t)
				lineUp(t, b, "demo")
				co := b.gitCheckout(t, "co")
				b.mustAdd(t, "demo", "worker", harness, co)
				file := filepath.Join(co, ".mcp.json")
				if harness == "codex" {
					file = filepath.Join(co, ".codex", "config.toml")
				}
				moved := co + "-moved"
				if err := os.Rename(co, moved); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(moved, co); err != nil {
					t.Fatal(err)
				}
				movedFile := filepath.Join(moved, strings.TrimPrefix(file, co))
				before := b.lsnap(t, moved)
				args := b.ldrop("demo", "worker")
				if verb == "end" {
					args = b.lend("demo")
				}
				res := b.run(t, args...)
				if res.code != 1 || !strings.Contains(res.stderr, co) || !strings.Contains(res.stderr, "now resolves to "+moved) || !strings.Contains(res.stderr, "--force") {
					t.Fatalf("exit %d, want 1 naming the checkout and where it leads, and --force\n%s", res.code, res.stderr)
				}
				b.wantLineUnchanged(t, before, moved)

				force := append(args, "--force")
				res = mustLine(t, b, force...)
				if !strings.Contains(res.stdout, "Left in place:") || !strings.Contains(res.stdout, "now resolves to "+moved) {
					t.Errorf("stdout does not say the files are left:\n%s", res.stdout)
				}
				if _, err := os.Lstat(movedFile); err != nil {
					t.Errorf("--force removed %s: %v", movedFile, err)
				}
				if verb == "drop" {
					if rec := b.record(t, "demo"); len(rec.Sessions) != 0 {
						t.Errorf("sessions left: %+v", rec.Sessions)
					}
				}
			})
		}
	}
}

// TestLineDropOfACheckoutThatIsGoneIsNotARefusal: a checkout that is simply gone has no
// file to remove, and is not a refusal.
func TestLineDropOfACheckoutThatIsGoneIsNotARefusal(t *testing.T) {
	b := newLineBench(t)
	lineUp(t, b, "demo")
	co := b.plainCheckout(t, "co")
	b.mustAdd(t, "demo", "worker", "claude", co)
	if err := os.RemoveAll(co); err != nil {
		t.Fatal(err)
	}
	mustLine(t, b, b.ldrop("demo", "worker")...)
}

// TestLineAddRollbackLeavesAFileAloneWhenTheCheckoutWasMoved is the same guard in
// the add's rollback.
func TestLineAddRollbackLeavesAFileAloneWhenTheCheckoutWasMoved(t *testing.T) {
	b := newLineBench(t)
	co := b.plainCheckout(t, "co")
	file := filepath.Join(co, ".mcp.json")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	digest := tableDigest([]byte("x"))
	if err := removeIfDigest(file, digest, co); err != nil {
		t.Fatalf("a file that is the run's, in place: %v", err)
	}
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	moved := co + "-moved"
	if err := os.Rename(co, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, co); err != nil {
		t.Fatal(err)
	}
	var left leftAlone
	if err := removeIfDigest(file, digest, co); !errors.As(err, &left) || !strings.Contains(string(left), "now resolves to "+moved) {
		t.Fatalf("removeIfDigest = %v, want it left alone, naming where the checkout leads", err)
	}
	if _, err := os.Lstat(filepath.Join(moved, ".mcp.json")); err != nil {
		t.Errorf("the file was removed: %v", err)
	}
}

// TestLineAddRollbackLeavesAConfigAloneWhenItsDirectoryBecameALink: a .codex
// moved out of the checkout and replaced by a link while the add ran leaves the
// checkout's own path unchanged, so only the parent check keeps rollback from
// removing the config through the link.
func TestLineAddRollbackLeavesAConfigAloneWhenItsDirectoryBecameALink(t *testing.T) {
	b := newLineBench(t)
	co := b.plainCheckout(t, "co")
	dir := filepath.Join(co, ".codex")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	digest := tableDigest([]byte("x"))
	outside := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.Rename(dir, outside); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, dir); err != nil {
		t.Fatal(err)
	}
	var left leftAlone
	if err := removeIfDigest(file, digest, co); !errors.As(err, &left) || !strings.Contains(string(left), "is a symlink now") {
		t.Fatalf("removeIfDigest = %v, want it left alone, naming the linked directory", err)
	}
	if _, err := os.Lstat(filepath.Join(outside, "config.toml")); err != nil {
		t.Errorf("the config outside the checkout was removed: %v", err)
	}
}

// TestLineDropKeepsTheExcludeLineForAWorktreeItCannotLookAt: only a not-exist
// error shows a worktree has no file at the pattern's path; any other leaves the
// line, and says which worktree and why.
func TestLineDropKeepsTheExcludeLineForAWorktreeItCannotLookAt(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root is not stopped by a directory's mode")
	}
	b := newLineBench(t)
	lineUp(t, b, "demo")
	co := b.gitCheckout(t, "co")
	wt2 := b.worktreeOf(t, co, "co-wt2")
	b.mustAdd(t, "demo", "plan", "claude", co)
	if err := os.Chmod(wt2, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(wt2, 0o755) })
	res := mustLine(t, b, b.ldrop("demo", "plan")...)
	if !strings.Contains(readExcludeForTest(t, co), "/.mcp.json\n") {
		t.Error("the line was taken out although a worktree could not be looked at")
	}
	if !strings.Contains(res.stdout, "the worktree "+wt2+" cannot be looked at") || !strings.Contains(res.stdout, "permission denied") {
		t.Errorf("stdout does not name the worktree and the error:\n%s", res.stdout)
	}
}

// TestLineAddCodexInANonGitCheckoutSkipsCodexsGitCheck: codex exec refuses a
// directory that is no git checkout unless told not to look, and a line accepts
// one. Only a line session's turn carries the flag.
func TestLineAddCodexInANonGitCheckoutSkipsCodexsGitCheck(t *testing.T) {
	b := newLineBench(t)
	lineUp(t, b, "demo")
	co := b.plainCheckout(t, "plain")
	mustLine(t, b, b.ladd("demo", "build", "codex", co)...)
	argv := b.call(t, 1)
	if len(argv) < 4 || argv[0] != "exec" || argv[1] != "--json" || argv[2] != "--skip-git-repo-check" || argv[3] != "-C" || argv[4] != co {
		t.Fatalf("codex argv %q, want exec --json --skip-git-repo-check -C %s ...", argv, co)
	}
	// A table's seat keeps its argv.
	tb := newTableBench(t)
	mustTable(t, tb, tb.newArgs("t1", []string{"codex-a=codex"})...)
	for _, a := range tb.call(t, 1) {
		if a == "--skip-git-repo-check" {
			t.Errorf("table new passes --skip-git-repo-check: %q", tb.call(t, 1))
		}
	}
}

// TestWorktreeRootsListsTheExcludeFilesRepository: a linked worktree removed and
// another repository made at its path must not stand in for the original's
// worktrees, since the exclude file the line edits is the original's.
func TestWorktreeRootsListsTheExcludeFilesRepository(t *testing.T) {
	b := newLineBench(t)
	repo := b.gitCheckout(t, "repo")
	wt := b.worktreeOf(t, repo, "wt")
	exclude, _ := excludeFileOf(context.Background(), wt)
	if exclude == "" {
		t.Fatal("no exclude file for the worktree")
	}
	gitIn(t, repo, "worktree", "remove", "--force", wt)
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, wt, "init", "-q")
	roots, ok := worktreeRoots(context.Background(), wt, exclude)
	if !ok {
		t.Fatal("worktreeRoots could not list the original repository's worktrees")
	}
	realRepo, _ := filepath.EvalSymlinks(repo)
	realWt, _ := filepath.EvalSymlinks(wt)
	if !slices.Contains(roots, realRepo) || slices.Contains(roots, realWt) {
		t.Fatalf("roots %q: want the original repository %s and not the new one at %s", roots, realRepo, realWt)
	}
}

// TestWorktreeRootsTakesACutListingAsUnknown: a listing cut at gitOutputMax could
// omit the worktree that still needs the line, so the line is kept.
func TestWorktreeRootsTakesACutListingAsUnknown(t *testing.T) {
	b := newLineBench(t)
	repo := b.gitCheckout(t, "repo")
	exclude, _ := excludeFileOf(context.Background(), repo)
	defer func(old int) { gitOutputMax = old }(gitOutputMax)
	gitOutputMax = 10
	if roots, ok := worktreeRoots(context.Background(), repo, exclude); ok {
		t.Fatalf("a cut listing was taken as whole: %q", roots)
	}
	if needed, why := excludeLineStillNeeded(context.Background(), repo, exclude, "/.mcp.json"); !needed || !strings.Contains(why, "cannot list") {
		t.Fatalf("excludeLineStillNeeded = %v, %q; want the line kept because git could not list the worktrees", needed, why)
	}
}

// TestLineAddRefusesALaunchPathGitStillTracks: a tracked launch file the user
// moved aside is absent from the checkout, but a file written there would be a
// change to a tracked file, which no exclude line hides.
func TestLineAddRefusesALaunchPathGitStillTracks(t *testing.T) {
	for _, harness := range []string{"claude", "codex"} {
		t.Run(harness, func(t *testing.T) {
			b := newLineBench(t)
			lineUp(t, b, "demo")
			co := b.gitCheckout(t, "co")
			rel := ".mcp.json"
			if harness == "codex" {
				rel = filepath.Join(".codex", "config.toml")
			}
			file := filepath.Join(co, rel)
			if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file, []byte("tracked\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitIn(t, co, "add", "--", rel)
			gitIn(t, co, "commit", "-q", "-m", "track it")
			if err := os.Remove(file); err != nil {
				t.Fatal(err)
			}
			before := b.lsnap(t, co)
			res := b.run(t, b.ladd("demo", "plan", harness, co)...)
			if res.code != 2 || !strings.Contains(res.stderr, "is tracked by git") {
				t.Fatalf("exit %d, want 2 naming the tracked path\n%s", res.code, res.stderr)
			}
			b.wantLineUnchanged(t, before, co)
		})
	}
}

// TestRemoveDirMarkerLastKeepsTheMarkerWhenItStopsPartway: a reply directory whose
// removal fails on an entry it may not delete keeps its marker, so the next end
// still recognises it and finishes once the cause is fixed.
func TestRemoveDirMarkerLastKeepsTheMarkerWhenItStopsPartway(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root deletes regardless of permissions")
	}
	dir := filepath.Join(t.TempDir(), "reply")
	locked := filepath.Join(dir, "locked")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{filepath.Join(dir, ".marker"), filepath.Join(dir, "a.reply"), filepath.Join(locked, "stuck")} {
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	if err := removeDirMarkerLast(dir, ".marker"); err == nil {
		t.Fatal("removal through a directory it may not write succeeded")
	}
	if _, err := os.Lstat(filepath.Join(dir, ".marker")); err != nil {
		t.Fatalf("the marker went with a removal that failed: %v", err)
	}
	if err := os.Chmod(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := removeDirMarkerLast(dir, ".marker"); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the directory is still there after the retry: %v", err)
	}
}

// TestLineTeardownRemovesAnExcludeFileTheLineMadeWhateverTheOrder: with no
// info/exclude at first, a Claude session's add creates it and a Codex session's
// add appends to it. Either session may go last, and line end drops Claude first,
// so the duty to remove the file once empty is the line's, shared by both.
func TestLineTeardownRemovesAnExcludeFileTheLineMadeWhateverTheOrder(t *testing.T) {
	for _, order := range [][]string{{"plan", "build"}, {"build", "plan"}, nil} {
		name := "end"
		if order != nil {
			name = "drop " + strings.Join(order, " then ")
		}
		t.Run(name, func(t *testing.T) {
			b := newLineBench(t)
			lineUp(t, b, "demo")
			co := b.gitCheckout(t, "co")
			exclude := filepath.Join(co, ".git", "info", "exclude")
			if err := os.Remove(exclude); err != nil {
				t.Fatal(err)
			}
			mustLine(t, b, b.ladd("demo", "plan", "claude", co)...)
			mustLine(t, b, b.ladd("demo", "build", "codex", co)...)
			if order == nil {
				mustLine(t, b, b.lend("demo")...)
			} else {
				for _, nick := range order {
					mustLine(t, b, b.ldrop("demo", nick)...)
				}
			}
			if _, err := os.Lstat(exclude); !errors.Is(err, os.ErrNotExist) {
				got, _ := os.ReadFile(exclude)
				t.Fatalf("the exclude file the line made is still there (%v): %q", err, got)
			}
		})
	}
}

// TestTreeListingSeesTheExcludeFileAndALinkedWorktreesLaunchFile: the snapshot
// the "nothing changed" checks compare must change when the exclude file does, in
// a main checkout or through a linked worktree, and when a launch file appears in
// a linked worktree, whose .git is a file.
func TestTreeListingSeesTheExcludeFileAndALinkedWorktreesLaunchFile(t *testing.T) {
	b := newLineBench(t)
	repo := b.gitCheckout(t, "repo")
	wt := b.worktreeOf(t, repo, "wt")
	exclude := filepath.Join(repo, ".git", "info", "exclude")
	for _, dir := range []string{repo, wt} {
		before := treeListing(t, dir)
		f, err := os.OpenFile(exclude, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString("/" + filepath.Base(dir) + "\n"); err != nil {
			t.Fatal(err)
		}
		f.Close()
		if treeListing(t, dir) == before {
			t.Errorf("%s: a change to the exclude file did not change the listing", dir)
		}
	}
	before := treeListing(t, wt)
	if err := os.WriteFile(filepath.Join(wt, ".mcp.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if treeListing(t, wt) == before {
		t.Error("a launch file in a linked worktree did not change the listing")
	}
}

// TestRemoveDirMarkerLastPutsTheMarkerBackWhenTheDirectoryStays: with the parent
// not writable, the entries and the marker go but the directory cannot; the
// marker comes back, so the next end recognises the directory and finishes.
func TestRemoveDirMarkerLastPutsTheMarkerBackWhenTheDirectoryStays(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root deletes regardless of permissions")
	}
	parent := filepath.Join(t.TempDir(), "tmp")
	dir := filepath.Join(parent, "reply")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, ".marker")
	if err := os.WriteFile(marker, []byte("demo\nplan\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plan.reply"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })
	if err := removeDirMarkerLast(dir, ".marker"); err == nil {
		t.Fatal("removing a directory whose parent is not writable succeeded")
	}
	got, err := os.ReadFile(marker)
	if err != nil || string(got) != "demo\nplan\n" {
		t.Fatalf("the marker was not put back as it was: %q, %v", got, err)
	}
	if fi, err := os.Lstat(marker); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the marker's mode was not kept: %v, %v", fi, err)
	}
	if err := os.Chmod(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := removeDirMarkerLast(dir, ".marker"); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the directory is still there after the retry: %v", err)
	}
}
