package main

import (
	"os"
	"path/filepath"
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
