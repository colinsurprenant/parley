package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colinsurprenant/parley/internal/state"
)

func (b *lineBench) mustAdd(t *testing.T, id, nick, harness, co string, extra ...string) result {
	t.Helper()
	return mustLine(t, b, b.ladd(id, nick, harness, co, extra...)...)
}

// TestLineDropClaudeKeepsTheSharedFileUntilTheLastSessionGoes: two Claude sessions
// share one .mcp.json and one exclude line; the first to go leaves both, the last
// takes both, and the checkout is as it was.
func TestLineDropClaudeKeepsTheSharedFileUntilTheLastSessionGoes(t *testing.T) {
	b := newLineBench(t)
	lineUp(t, b, "demo")
	co := b.gitCheckout(t, "co")
	pristine := treeListing(t, co)
	b.mustAdd(t, "demo", "plan", "claude", co)
	b.mustAdd(t, "demo", "review", "claude", co)
	path := filepath.Join(co, ".mcp.json")

	res := mustLine(t, b, b.ldrop("demo", "plan")...)
	t.Logf("stdout:\n%s", res.stdout)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the first drop removed the shared file: %v", err)
	}
	if !strings.Contains(readExcludeForTest(t, co), "/.mcp.json\n") {
		t.Error("the first drop removed the shared exclude line")
	}
	if !strings.Contains(res.stdout, "Kept, because the line still needs them") || !strings.Contains(res.stdout, "session review of the checkout still uses it") {
		t.Errorf("stdout does not say the file is kept:\n%s", res.stdout)
	}
	rec := b.record(t, "demo")
	if len(rec.Sessions) != 1 || rec.Sessions[0].Nick != "review" || rec.Sessions[0].Files[0].ExcludeFile == "" {
		t.Fatalf("record after the first drop: %+v", rec.Sessions)
	}

	res = mustLine(t, b, b.ldrop("demo", "review")...)
	t.Logf("stdout:\n%s", res.stdout)
	if _, err := os.Stat(path); err == nil {
		t.Error("the last drop left the file")
	}
	if got := treeListing(t, co); got != pristine {
		t.Errorf("the checkout is not as it was.\nbefore:\n%s\nafter:\n%s", pristine, got)
	}
	for _, want := range []string{path, "the line /.mcp.json in " + filepath.Join(co, ".git", "info", "exclude")} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("stdout does not list %q as removed:\n%s", want, res.stdout)
		}
	}
	if strings.Contains(res.stdout, "codex delete") {
		t.Errorf("a Claude session has no Codex session to delete:\n%s", res.stdout)
	}
	if rec := b.record(t, "demo"); len(rec.Sessions) != 0 {
		t.Errorf("sessions left: %+v", rec.Sessions)
	}
}

// TestLineDropCodexSessionRemovesWhatAddMade: the config, the .codex directory it
// made, the exclude line, the thread record, the reply directory; the record is
// what it was before the add, and the codex delete line is printed and not run.
func TestLineDropCodexSessionRemovesWhatAddMade(t *testing.T) {
	b := newLineBench(t)
	lineUp(t, b, "demo")
	co := b.gitCheckout(t, "co")
	before := b.lsnap(t, co)
	b.mustAdd(t, "demo", "build", "codex", co)
	rec := b.record(t, "demo")
	s := rec.Sessions[0]
	tr, err := state.OpenThreadRecord(b.base, "build", rec.Stream)
	if err != nil {
		t.Fatal(err)
	}

	res := mustLine(t, b, b.ldrop("demo", "build")...)
	t.Logf("stdout:\n%s", res.stdout)
	for _, gone := range []string{s.Files[0].Path, filepath.Join(co, ".codex"), s.ReplyDir, tr.Path(), filepath.Dir(tr.Path())} {
		if _, err := os.Lstat(gone); err == nil {
			t.Errorf("%s is still there", gone)
		}
	}
	if got, want := withoutCalls(b.lsnap(t, co)), withoutCalls(before); got != want {
		t.Fatalf("the machine is not as it was before the add.\nbefore:\n%s\nafter:\n%s", want, got)
	}
	wantLines := []string{
		"thread record for session build",
		"reply directory " + s.ReplyDir,
		s.Files[0].Path,
		"directory " + filepath.Join(co, ".codex"),
		"the line /.codex/config.toml in " + filepath.Join(co, ".git", "info", "exclude"),
		"Codex still has each Codex session. Keep that history, or delete it:",
		"codex delete '" + fakeThread(1) + "'    # session build",
	}
	wantInOrder(t, res.stdout, wantLines)
	// codex delete is printed and never run.
	for n := 1; n <= b.callCount(); n++ {
		if argv := b.call(t, n); argv[0] == "delete" {
			t.Errorf("drop ran %q", argv)
		}
	}
}

// TestLineDropKeepsAnExcludeDirectoryTheUserMade: a .codex directory the line did
// not make stays, with whatever else is in it; so does a made one that is no
// longer empty.
func TestLineDropKeepsADotCodexDirectoryItDidNotMake(t *testing.T) {
	b := newLineBench(t)
	lineUp(t, b, "demo")
	co := b.gitCheckout(t, "co")
	if err := os.Mkdir(filepath.Join(co, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	b.mustAdd(t, "demo", "build", "codex", co)
	if b.record(t, "demo").Sessions[0].Files[0].MadeDir {
		t.Fatal("the record says the line made a .codex that was there")
	}
	mustLine(t, b, b.ldrop("demo", "build")...)
	if fi, err := os.Stat(filepath.Join(co, ".codex")); err != nil || !fi.IsDir() {
		t.Errorf(".codex: %v, %v; want it kept", fi, err)
	}
	if _, err := os.Stat(filepath.Join(co, ".codex", "config.toml")); err == nil {
		t.Error("the config is still there")
	}

	// One the line made but that holds something else now stays too.
	co2 := b.gitCheckout(t, "co2")
	b.mustAdd(t, "demo", "ops", "codex", co2)
	extra := filepath.Join(co2, ".codex", "notes.txt")
	if err := os.WriteFile(extra, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustLine(t, b, b.ldrop("demo", "ops")...)
	if got := string(mustRead(t, extra)); got != "mine" {
		t.Errorf("notes.txt = %q", got)
	}
}

// TestLineDropKeepsEveryOtherExcludeLineByteForByte: the exclude file after an
// add and a drop is the file before them, whatever it held.
func TestLineDropKeepsEveryOtherExcludeLineByteForByte(t *testing.T) {
	for name, content := range map[string]string{
		"empty":                    "",
		"comments":                 "# git ls-files --others --exclude-from=.git/info/exclude\n# Lines that start with '#' are comments.\n",
		"blank lines and CRLF":     "a\n\n\n# x\r\nfoo/\r\n\n",
		"no trailing newline":      "a\nb",
		"one line, no newline":     "only",
		"lookalikes":               "/.mcp.json.bak\n/x/.mcp.json\n  /.mcp.json\n/.mcp.json \n!/.mcp.jsonx\n",
		"lines on both sides":      "before\n",
		"a pattern with a tab":     "a\tb\n*.log\n",
		"a long line":              strings.Repeat("x", 5000) + "\n",
		"non-UTF-8 bytes":          "caf\xe9\n\xff\xfe\n",
		"the codex line as a near": "/.codex/config.toml.bak\n",
	} {
		t.Run(name, func(t *testing.T) {
			b := newLineBench(t)
			lineUp(t, b, "demo")
			co := b.gitCheckout(t, "co")
			exclude := filepath.Join(co, ".git", "info", "exclude")
			if err := os.WriteFile(exclude, []byte(content), 0o640); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(exclude, 0o640); err != nil {
				t.Fatal(err)
			}
			b.mustAdd(t, "demo", "plan", "claude", co)
			b.mustAdd(t, "demo", "build", "codex", co)
			got := string(mustRead(t, exclude))
			if !strings.HasPrefix(got, content) || !strings.Contains("\n"+got, "\n/.mcp.json\n") {
				t.Fatalf("after the adds: %q", got)
			}
			mustLine(t, b, b.ldrop("demo", "build")...)
			mustLine(t, b, b.ldrop("demo", "plan")...)
			if after := string(mustRead(t, exclude)); after != content {
				t.Errorf("info/exclude = %q, want it as it was: %q", after, content)
			}
			if fi, err := os.Stat(exclude); err != nil || fi.Mode().Perm() != 0o640 {
				t.Errorf("info/exclude mode %v (%v), want it kept at 0640", fi.Mode(), err)
			}
		})
	}
}

// TestLineDropLeavesAnExcludeLineItDidNotAdd.
func TestLineDropLeavesAnExcludeLineItDidNotAdd(t *testing.T) {
	b := newLineBench(t)
	lineUp(t, b, "demo")
	co := b.gitCheckout(t, "co")
	exclude := filepath.Join(co, ".git", "info", "exclude")
	if err := os.WriteFile(exclude, []byte("/.mcp.json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	b.mustAdd(t, "demo", "plan", "claude", co)
	mustLine(t, b, b.ldrop("demo", "plan")...)
	if got := string(mustRead(t, exclude)); got != "/.mcp.json\n" {
		t.Errorf("info/exclude = %q, want the user's line kept", got)
	}
}

// TestLineDropRemovesOnlyTheFirstOfTwoIdenticalExcludeLines: the line the user
// added after the line's own is theirs.
func TestLineDropRemovesOnlyTheLinesOwnExcludeLine(t *testing.T) {
	b := newLineBench(t)
	lineUp(t, b, "demo")
	co := b.gitCheckout(t, "co")
	exclude := filepath.Join(co, ".git", "info", "exclude")
	b.mustAdd(t, "demo", "plan", "claude", co)
	f, err := os.OpenFile(exclude, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("/.mcp.json\nlater\n")
	f.Close()
	mustLine(t, b, b.ldrop("demo", "plan")...)
	if got := readExcludeForTest(t, co); strings.Count(got, "/.mcp.json\n") != 1 || !strings.HasSuffix(got, "later\n") {
		t.Errorf("info/exclude = %q, want one /.mcp.json left, the later one", got)
	}
}

// TestLineDropRefusesWhileTheSessionIsRunning: its nick lock is held by the
// session or its pump; exit 1, nothing removed, and the next drop goes through.
func TestLineDropRefusesWhileTheSessionIsRunning(t *testing.T) {
	for _, harness := range []string{"claude", "codex"} {
		t.Run(harness, func(t *testing.T) {
			b := newLineBench(t)
			lineUp(t, b, "demo")
			co := b.gitCheckout(t, "co")
			b.mustAdd(t, "demo", "other", "claude", b.gitCheckout(t, "co-other"))
			b.mustAdd(t, "demo", "worker", harness, co)
			l, err := state.LockNick(b.base, "worker")
			if err != nil {
				t.Fatal(err)
			}
			before := b.lsnap(t, b.checkoutDirs(t)...)
			res := b.run(t, b.ldrop("demo", "worker", "--force")...)
			if res.code != 1 || !strings.Contains(res.stderr, `"worker"`) || !strings.Contains(res.stderr, "still running") {
				t.Fatalf("exit %d, want 1 naming worker\n%s", res.code, res.stderr)
			}
			b.wantLineUnchanged(t, before, b.checkoutDirs(t)...)
			// A refusal holds nothing: the other session's lock is free again.
			free, err := state.LockNick(b.base, "other")
			if err != nil {
				t.Fatalf("the refused drop left a lock held: %v", err)
			}
			free.Release()
			if err := l.Release(); err != nil {
				t.Fatal(err)
			}
			mustLine(t, b, b.ldrop("demo", "worker")...)
			// And a finished drop holds nothing either.
			free, err = state.LockNick(b.base, "worker")
			if err != nil {
				t.Fatalf("the finished drop left the lock held: %v", err)
			}
			free.Release()
		})
	}
}

// TestLineDropRefusesAFileThatIsNotTheLines: the launch file is the user's the
// moment its bytes stop matching the digest, or it stops being a regular file in
// a real directory: exit 1 naming the file, nothing removed, and --force leaves
// the file and its exclude line where they are.
func TestLineDropRefusesAFileThatIsNotTheLines(t *testing.T) {
	cases := []struct {
		name    string
		harness string
		// tamper changes the checkout and returns the file outside it that the change
		// points at, or "".
		tamper func(t *testing.T, b *lineBench, co string) string
		file   string
		why    string
	}{
		{"an edited .mcp.json", "claude", func(t *testing.T, b *lineBench, co string) string {
			f, err := os.OpenFile(filepath.Join(co, ".mcp.json"), os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			f.WriteString(" ")
			f.Close()
			return ""
		}, ".mcp.json", "edited or replaced"},
		{"a .mcp.json written over with another", "claude", func(t *testing.T, b *lineBench, co string) string {
			if err := os.WriteFile(filepath.Join(co, ".mcp.json"), []byte(`{"mcpServers":{"mine":{}}}`), 0o644); err != nil {
				t.Fatal(err)
			}
			return ""
		}, ".mcp.json", "edited or replaced"},
		{"a .mcp.json that is a link to a copy of itself", "claude", func(t *testing.T, b *lineBench, co string) string {
			copyPath := filepath.Join(b.root, "copy.json")
			if err := os.Rename(filepath.Join(co, ".mcp.json"), copyPath); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(copyPath, filepath.Join(co, ".mcp.json")); err != nil {
				t.Fatal(err)
			}
			return copyPath
		}, ".mcp.json", "is a symlink"},
		{"a .mcp.json that is a directory now", "claude", func(t *testing.T, b *lineBench, co string) string {
			if err := os.Remove(filepath.Join(co, ".mcp.json")); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(co, ".mcp.json"), 0o755); err != nil {
				t.Fatal(err)
			}
			return ""
		}, ".mcp.json", "is not a regular file"},
		{"an edited config.toml", "codex", func(t *testing.T, b *lineBench, co string) string {
			if err := os.WriteFile(filepath.Join(co, ".codex", "config.toml"), []byte("model = \"x\"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			return ""
		}, "config.toml", "edited or replaced"},
		{"a .codex that became a link to a copy of itself", "codex", func(t *testing.T, b *lineBench, co string) string {
			copyDir := filepath.Join(b.root, "copy-codex")
			if err := os.Rename(filepath.Join(co, ".codex"), copyDir); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(copyDir, filepath.Join(co, ".codex")); err != nil {
				t.Fatal(err)
			}
			return filepath.Join(copyDir, "config.toml")
		}, "config.toml", "symlink now"},
		{"a config.toml that is a link", "codex", func(t *testing.T, b *lineBench, co string) string {
			cfg := filepath.Join(co, ".codex", "config.toml")
			copyPath := filepath.Join(b.root, "copy.toml")
			if err := os.Rename(cfg, copyPath); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(copyPath, cfg); err != nil {
				t.Fatal(err)
			}
			return copyPath
		}, "config.toml", "is a symlink"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := newLineBench(t)
			lineUp(t, b, "demo")
			co := b.gitCheckout(t, "co")
			b.mustAdd(t, "demo", "worker", c.harness, co)
			pointedAt := c.tamper(t, b, co)
			before := b.lsnap(t, b.checkoutDirs(t)...)
			// The file outside the checkout that a link points at is never touched.
			var pointedAtBytes []byte
			if pointedAt != "" {
				pointedAtBytes = mustRead(t, pointedAt)
			}

			res := b.run(t, b.ldrop("demo", "worker")...)
			if res.code != 1 || !strings.Contains(res.stderr, c.file) || !strings.Contains(res.stderr, c.why) || !strings.Contains(res.stderr, "--force") {
				t.Fatalf("exit %d, want 1 naming the file and why, and --force\n%s", res.code, res.stderr)
			}
			b.wantLineUnchanged(t, before, b.checkoutDirs(t)...)

			res = mustLine(t, b, b.ldrop("demo", "worker", "--force")...)
			t.Logf("stdout:\n%s", res.stdout)
			if !strings.Contains(res.stdout, "Left in place, because nothing shows they are the line's") || !strings.Contains(res.stdout, c.file) || !strings.Contains(res.stdout, c.why) {
				t.Errorf("stdout does not say the file is left in place:\n%s", res.stdout)
			}
			// The file stays as the user left it, and so does its exclude line.
			wantFile := filepath.Join(co, ".mcp.json")
			wantLine := "/.mcp.json\n"
			if c.harness == "codex" {
				wantFile, wantLine = filepath.Join(co, ".codex", "config.toml"), "/.codex/config.toml\n"
			}
			if _, err := os.Lstat(wantFile); err != nil {
				t.Errorf("--force removed %s: %v", wantFile, err)
			}
			if !strings.Contains(readExcludeForTest(t, co), wantLine) {
				t.Error("--force removed the exclude line of a file it left")
			}
			if pointedAt != "" {
				if got := mustRead(t, pointedAt); string(got) != string(pointedAtBytes) {
					t.Errorf("the file the link points at, %s, was changed", pointedAt)
				}
			}
			if rec := b.record(t, "demo"); len(rec.Sessions) != 0 {
				t.Errorf("sessions left after --force: %+v", rec.Sessions)
			}
			if c.harness == "codex" && !strings.Contains(res.stdout, "reply directory") {
				t.Errorf("the rest of the session was not taken down:\n%s", res.stdout)
			}
		})
	}
}

// TestLineDropOfOneSessionNeedsOnlyItsOwnFileToBeTheLines: a session whose
// checkout-mate still uses a modified file goes without a word about it; the file
// is the last one's to answer for.
func TestLineDropOfASharingSessionLeavesTheFileToTheLastOne(t *testing.T) {
	b := newLineBench(t)
	lineUp(t, b, "demo")
	co := b.gitCheckout(t, "co")
	b.mustAdd(t, "demo", "plan", "claude", co)
	b.mustAdd(t, "demo", "review", "claude", co)
	if err := os.WriteFile(filepath.Join(co, ".mcp.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustLine(t, b, b.ldrop("demo", "plan")...)
	res := b.run(t, b.ldrop("demo", "review")...)
	if res.code != 1 || !strings.Contains(res.stderr, ".mcp.json") {
		t.Fatalf("exit %d, want 1 naming the file\n%s", res.code, res.stderr)
	}
}

// TestLineDropWhenWhatItMadeIsAlreadyGone: the file, the reply directory and the
// thread record may have been removed by hand; the drop says so and finishes.
func TestLineDropWhenWhatItMadeIsAlreadyGone(t *testing.T) {
	b := newLineBench(t)
	lineUp(t, b, "demo")
	co := b.gitCheckout(t, "co")
	b.mustAdd(t, "demo", "build", "codex", co)
	s := b.record(t, "demo").Sessions[0]
	tr, _ := state.OpenThreadRecord(b.base, "build", b.stream("demo"))
	for _, p := range []string{s.Files[0].Path, tr.Path()} {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.RemoveAll(s.ReplyDir); err != nil {
		t.Fatal(err)
	}
	res := mustLine(t, b, b.ldrop("demo", "build")...)
	for _, want := range []string{"thread record for session build (already gone)", "reply directory " + s.ReplyDir + " (already gone)", s.Files[0].Path + " (already gone)", "the line /.codex/config.toml in "} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, res.stdout)
		}
	}
	if rec := b.record(t, "demo"); len(rec.Sessions) != 0 {
		t.Errorf("sessions left: %+v", rec.Sessions)
	}
}

// TestLineDropLeavesAThreadRecordThatIsNotTheSessions: a record that holds another
// thread, or is not a record, is not shown to be the session's; it is left, said,
// and the rest goes.
func TestLineDropLeavesAThreadRecordThatIsNotTheSessions(t *testing.T) {
	for name, content := range map[string]string{"another thread": fakeThread(9) + "\n", "not a record": "garbage"} {
		t.Run(name, func(t *testing.T) {
			b := newLineBench(t)
			lineUp(t, b, "demo")
			co := b.gitCheckout(t, "co")
			b.mustAdd(t, "demo", "build", "codex", co)
			tr, _ := state.OpenThreadRecord(b.base, "build", b.stream("demo"))
			if err := os.WriteFile(tr.Path(), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			res := mustLine(t, b, b.ldrop("demo", "build")...)
			if got := string(mustRead(t, tr.Path())); got != content {
				t.Errorf("the record was changed: %q", got)
			}
			if !strings.Contains(res.stdout, "Left in place") || !strings.Contains(res.stdout, tr.Path()) {
				t.Errorf("stdout does not list the record as left:\n%s", res.stdout)
			}
			if rec := b.record(t, "demo"); len(rec.Sessions) != 0 {
				t.Errorf("sessions left: %+v", rec.Sessions)
			}
		})
	}
}

// TestLineDropRefusesAReplyDirectoryItCannotShowIsTheSessions: the marker names the
// line and the nick, the directory is directly in $TMPDIR and is named for the
// line; anything else is refused, exit 1, nothing removed.
func TestLineDropRefusesAReplyDirectoryItCannotShowIsTheSessions(t *testing.T) {
	cases := map[string]func(t *testing.T, b *lineBench, dir string) []string{
		"another nick's marker": func(t *testing.T, b *lineBench, dir string) []string {
			if err := os.WriteFile(filepath.Join(dir, ".parley-line"), []byte("demo\nother\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			return nil
		},
		"another line's marker": func(t *testing.T, b *lineBench, dir string) []string {
			if err := os.WriteFile(filepath.Join(dir, ".parley-line"), []byte("other\nbuild\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			return nil
		},
		"no marker": func(t *testing.T, b *lineBench, dir string) []string {
			if err := os.Remove(filepath.Join(dir, ".parley-line")); err != nil {
				t.Fatal(err)
			}
			return nil
		},
		"a marker that is a link": func(t *testing.T, b *lineBench, dir string) []string {
			m := filepath.Join(dir, ".parley-line")
			keep := filepath.Join(b.root, "marker")
			if err := os.Rename(m, keep); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(keep, m); err != nil {
				t.Fatal(err)
			}
			return nil
		},
		"a directory that is not directly in TMPDIR": func(t *testing.T, b *lineBench, dir string) []string {
			return []string{"TMPDIR=" + b.root}
		},
	}
	for name, tamper := range cases {
		t.Run(name, func(t *testing.T) {
			b := newLineBench(t)
			lineUp(t, b, "demo")
			co := b.gitCheckout(t, "co")
			b.mustAdd(t, "demo", "build", "codex", co)
			dir := b.record(t, "demo").Sessions[0].ReplyDir
			env := tamper(t, b, dir)
			before := b.lsnap(t, co)
			res := runParleyEnvIn(t, "", append(b.env(), env...), b.ldrop("demo", "build", "--force")...)
			if res.code != 1 || !strings.Contains(res.stderr, "will not remove the reply directory "+dir) {
				t.Fatalf("exit %d, want 1 refusing the reply directory\n%s", res.code, res.stderr)
			}
			b.wantLineUnchanged(t, before, co)
		})
	}
}

// TestLineDropConfigurationErrors: exit 2 for what is the invocation's mistake.
func TestLineDropConfigurationErrors(t *testing.T) {
	b := newLineBench(t)
	lineUp(t, b, "demo")
	b.mustAdd(t, "demo", "plan", "claude", b.gitCheckout(t, "co"))
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"line", "drop", "--state-dir", b.base}, "the line id and the session's nick are required"},
		{[]string{"line", "drop", "demo", "--state-dir", b.base}, "the line id and the session's nick are required"},
		{b.ldrop("demo", "plan", "extra"), "unexpected argument"},
		{b.ldrop("demo", "nobody"), `line "demo" has no session "nobody" (its sessions: plan)`},
		{b.ldrop("demo", "a.b"), "nick"},
		{b.ldrop("DEMO", "plan"), "line id"},
		{b.ldrop("demo", "plan", "--frobnicate"), "flag provided but not defined"},
		{[]string{"line", "end", "--state-dir", b.base}, "the line id is required"},
		{b.lend("demo", "extra"), "unexpected argument"},
		{b.lend("DEMO"), "line id"},
	} {
		res := b.run(t, c.args...)
		if res.code != 2 || !strings.Contains(res.stderr, c.want) {
			t.Errorf("%v: exit %d, want 2 saying %q\n%s", c.args, res.code, c.want, res.stderr)
		}
	}
	// A nick in another case is the same session, and flags may come before or
	// after the positionals.
	mustLine(t, b, "line", "drop", "--state-dir", b.base, "demo", "PLAN")
	if rec := b.record(t, "demo"); len(rec.Sessions) != 0 {
		t.Errorf("sessions left: %+v", rec.Sessions)
	}
}

// TestLineAddAfterDropGivesTheNickBackItsPlace: nothing a drop left is in the
// way of the same nick, in the same checkout, joining again.
func TestLineAddAfterDropGivesTheNickBackItsPlace(t *testing.T) {
	b := newLineBench(t)
	lineUp(t, b, "demo")
	co := b.gitCheckout(t, "co")
	b.mustAdd(t, "demo", "build", "codex", co)
	mustLine(t, b, b.ldrop("demo", "build")...)
	res := b.mustAdd(t, "demo", "build", "codex", co)
	if !strings.Contains(res.stdout, fakeThread(2)) {
		t.Errorf("stdout:\n%s", res.stdout)
	}
	if got := readExcludeForTest(t, co); strings.Count(got, "/.codex/config.toml\n") != 1 {
		t.Errorf("info/exclude = %q", got)
	}
}

// TestLineEndTakesTheLineDown: every session's things and the line's directory go,
// the checkouts are as they were, and each Codex session's delete line is printed.
func TestLineEndTakesTheLineDown(t *testing.T) {
	b := newLineBench(t)
	lineUp(t, b, "demo", "--codex-bin", b.codex)
	co1, co2, co3 := b.gitCheckout(t, "co1"), b.gitCheckout(t, "co2"), b.plainCheckout(t, "co3")
	pristine := treeListing(t, co1) + treeListing(t, co2) + treeListing(t, co3)
	b.mustAdd(t, "demo", "plan", "claude", co1)
	b.mustAdd(t, "demo", "review", "claude", co1)
	b.mustAdd(t, "demo", "build", "codex", co2)
	b.mustAdd(t, "demo", "ops", "codex", co2)
	b.mustAdd(t, "demo", "solo", "claude", co2)
	b.mustAdd(t, "demo", "plain", "claude", co3)
	rec := b.record(t, "demo")

	res := mustLine(t, b, b.lend("demo")...)
	t.Logf("stdout:\n%s", res.stdout)
	if got := treeListing(t, co1) + treeListing(t, co2) + treeListing(t, co3); got != pristine {
		t.Errorf("the checkouts are not as they were.\nbefore:\n%s\nafter:\n%s", pristine, got)
	}
	if _, err := os.Stat(filepath.Join(b.base, "lines", "demo")); err == nil {
		t.Error("the line's directory is still there")
	}
	if ents, _ := os.ReadDir(b.tmp); len(ents) != 0 {
		t.Errorf("$TMPDIR still holds %d entries", len(ents))
	}
	for _, s := range rec.Sessions {
		if s.Thread == "" {
			continue
		}
		tr, _ := state.OpenThreadRecord(b.base, s.Nick, rec.Stream)
		if _, err := os.Lstat(filepath.Dir(tr.Path())); err == nil {
			t.Errorf("session %s's stream directory is still there", s.Nick)
		}
		if !strings.Contains(res.stdout, "'"+b.codex+"' delete '"+s.Thread+"'    # session "+s.Nick) {
			t.Errorf("stdout has no delete line for %s with the line's codex:\n%s", s.Nick, res.stdout)
		}
	}
	if !strings.Contains(res.stdout, "Line demo is down.") || strings.Contains(res.stdout, "Kept") {
		t.Errorf("stdout:\n%s", res.stdout)
	}
	// And the id is free: a table or a line can be made with it.
	lineUp(t, b, "demo")
	// Every nick's lock is free.
	for _, s := range rec.Sessions {
		free, err := state.LockNick(b.base, s.Nick)
		if err != nil {
			t.Fatalf("the finished end left %s's lock held: %v", s.Nick, err)
		}
		free.Release()
	}
}

// TestLineEndOfALineWithNoSessions.
func TestLineEndOfALineWithNoSessions(t *testing.T) {
	b := newLineBench(t)
	lineUp(t, b, "demo")
	res := mustLine(t, b, b.lend("demo")...)
	if !strings.Contains(res.stdout, "line directory "+filepath.Join(b.base, "lines", "demo")) {
		t.Errorf("stdout:\n%s", res.stdout)
	}
	if _, err := os.Stat(filepath.Join(b.base, "lines", "demo")); err == nil {
		t.Error("the line's directory is still there")
	}
}

// TestLineEndRefusesWhileAnySessionIsRunning: all the locks are taken first, so a
// session that is running anywhere in the line stops the end before anything is
// removed.
func TestLineEndRefusesWhileAnySessionIsRunning(t *testing.T) {
	b := newLineBench(t)
	lineUp(t, b, "demo")
	b.mustAdd(t, "demo", "plan", "claude", b.gitCheckout(t, "co1"))
	b.mustAdd(t, "demo", "build", "codex", b.gitCheckout(t, "co2"))
	b.mustAdd(t, "demo", "review", "claude", b.gitCheckout(t, "co3"))
	l, err := state.LockNick(b.base, "review")
	if err != nil {
		t.Fatal(err)
	}
	before := b.lsnap(t, b.checkoutDirs(t)...)
	res := b.run(t, b.lend("demo", "--force")...)
	if res.code != 1 || !strings.Contains(res.stderr, `"review"`) || !strings.Contains(res.stderr, "still running") {
		t.Fatalf("exit %d, want 1 naming review\n%s", res.code, res.stderr)
	}
	b.wantLineUnchanged(t, before, b.checkoutDirs(t)...)
	for _, nick := range []string{"plan", "build"} {
		free, err := state.LockNick(b.base, nick)
		if err != nil {
			t.Fatalf("the refused end left %s's lock held: %v", nick, err)
		}
		free.Release()
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	mustLine(t, b, b.lend("demo")...)
}

// TestLineEndJudgesEverySessionBeforeRemovingAny: a file that is not the line's
// stops the end with nothing removed, whichever session it is; --force finishes,
// leaving that file.
func TestLineEndJudgesEverySessionBeforeRemovingAny(t *testing.T) {
	b := newLineBench(t)
	lineUp(t, b, "demo")
	co1, co2 := b.gitCheckout(t, "co1"), b.gitCheckout(t, "co2")
	b.mustAdd(t, "demo", "plan", "claude", co1)
	b.mustAdd(t, "demo", "build", "codex", co2)
	b.mustAdd(t, "demo", "review", "claude", b.gitCheckout(t, "co3"))
	if err := os.WriteFile(filepath.Join(co2, ".codex", "config.toml"), []byte("mine = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := b.lsnap(t, b.checkoutDirs(t)...)
	res := b.run(t, b.lend("demo")...)
	if res.code != 1 || !strings.Contains(res.stderr, "config.toml") || !strings.Contains(res.stderr, "--force") {
		t.Fatalf("exit %d, want 1 naming the config\n%s", res.code, res.stderr)
	}
	b.wantLineUnchanged(t, before, b.checkoutDirs(t)...)

	res = mustLine(t, b, b.lend("demo", "--force")...)
	if !strings.Contains(res.stdout, "Left in place") || !strings.Contains(res.stdout, filepath.Join(co2, ".codex", "config.toml")) {
		t.Errorf("stdout does not list the config as left:\n%s", res.stdout)
	}
	if got := string(mustRead(t, filepath.Join(co2, ".codex", "config.toml"))); got != "mine = 1\n" {
		t.Errorf("config.toml = %q", got)
	}
	if _, err := os.Stat(filepath.Join(co1, ".mcp.json")); err == nil {
		t.Error("the other sessions' files were not removed")
	}
	if _, err := os.Stat(filepath.Join(b.base, "lines", "demo")); err == nil {
		t.Error("the line's directory is still there")
	}
}

// TestLineEndAfterAPartialFailureFinishesOnTheSecondRun: a session whose checkout
// cannot be written is the failure; the first session's things are gone, the record
// lists the second, and the next run takes it down.
func TestLineEndAfterAPartialFailureFinishesOnTheSecondRun(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root is not stopped by a directory's mode")
	}
	b := newLineBench(t)
	lineUp(t, b, "demo")
	co1, co2 := b.gitCheckout(t, "co1"), b.gitCheckout(t, "co2")
	b.mustAdd(t, "demo", "first", "claude", co1)
	b.mustAdd(t, "demo", "second", "claude", co2)
	b.mustAdd(t, "demo", "third", "codex", b.gitCheckout(t, "co3"))
	if err := os.Chmod(co2, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(co2, 0o755) })

	res := b.run(t, b.lend("demo")...)
	if res.code != 1 || !strings.Contains(res.stderr, filepath.Join(co2, ".mcp.json")) || !strings.Contains(res.stderr, "run this again to finish") {
		t.Fatalf("exit %d, want 1 naming the file and saying to run again\n%s", res.code, res.stderr)
	}
	if !strings.Contains(res.stderr, "already removed") || !strings.Contains(res.stderr, filepath.Join(co1, ".mcp.json")) {
		t.Errorf("stderr does not say what was already removed:\n%s", res.stderr)
	}
	if _, err := os.Stat(filepath.Join(co1, ".mcp.json")); err == nil {
		t.Error("the first session's file is still there")
	}
	rec := b.record(t, "demo")
	var nicks []string
	for _, s := range rec.Sessions {
		nicks = append(nicks, s.Nick)
	}
	if strings.Join(nicks, ",") != "second,third" {
		t.Errorf("the record lists %v, want the sessions still there: second,third", nicks)
	}
	if _, err := os.Stat(filepath.Join(co2, ".mcp.json")); err != nil {
		t.Errorf("the second session's file is gone: %v", err)
	}

	if err := os.Chmod(co2, 0o755); err != nil {
		t.Fatal(err)
	}
	res = mustLine(t, b, b.lend("demo")...)
	t.Logf("stdout:\n%s", res.stdout)
	for _, gone := range []string{filepath.Join(co2, ".mcp.json"), filepath.Join(b.base, "lines", "demo")} {
		if _, err := os.Stat(gone); err == nil {
			t.Errorf("%s is still there", gone)
		}
	}
	if got := readExcludeForTest(t, co2); strings.Contains(got, ".mcp.json") {
		t.Errorf("co2's info/exclude still holds the line: %q", got)
	}
	if ents, _ := os.ReadDir(b.tmp); len(ents) != 0 {
		t.Errorf("$TMPDIR still holds %d entries", len(ents))
	}
}
