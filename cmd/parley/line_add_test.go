package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/colinsurprenant/parley/internal/state"
)

// checkoutDirs are the checkouts a test of the add made, for the snapshot.
func (b *lineBench) checkoutDirs(t *testing.T) []string {
	t.Helper()
	ents, err := os.ReadDir(b.root)
	if err != nil {
		t.Fatal(err)
	}
	var dirs []string
	for _, e := range ents {
		if e.IsDir() && strings.HasPrefix(e.Name(), "co") {
			dirs = append(dirs, filepath.Join(b.root, e.Name()))
		}
	}
	return dirs
}

// TestLineAddRefusesBeforeAnyChange: each refusal is a configuration error, exit
// 2, and the base, the checkouts, $TMPDIR and the record are as they were.
func TestLineAddRefusesBeforeAnyChange(t *testing.T) {
	longBase := func(b *lineBench) string {
		return filepath.Join(b.root, strings.Repeat("a", 200), strings.Repeat("b", 200))
	}
	type env struct {
		args []string
		env  []string
	}
	cases := []struct {
		name  string
		want  string
		setup func(t *testing.T, b *lineBench, co string) env
	}{
		{"no nick", "--nick is required", func(t *testing.T, b *lineBench, co string) env {
			return env{args: []string{"line", "add", "demo", "--harness", "claude", "--checkout", co, "--state-dir", b.base}}
		}},
		{"a nick with a dot", "nick", func(t *testing.T, b *lineBench, co string) env {
			return env{args: b.ladd("demo", "a.b", "claude", co)}
		}},
		{"tables as the nick", "tables", func(t *testing.T, b *lineBench, co string) env {
			return env{args: b.ladd("demo", "Tables", "claude", co)}
		}},
		{"builds as the nick", "builds", func(t *testing.T, b *lineBench, co string) env {
			return env{args: b.ladd("demo", "builds", "claude", co)}
		}},
		{"lines as the nick", "lines", func(t *testing.T, b *lineBench, co string) env {
			return env{args: b.ladd("demo", "LINES", "claude", co)}
		}},
		{"the chair as the nick", "is the chair's", func(t *testing.T, b *lineBench, co string) env {
			return env{args: b.ladd("demo", "Colin", "claude", co)}
		}},
		{"a nick that is already in the line", "already session", func(t *testing.T, b *lineBench, co string) env {
			mustLine(t, b, b.ladd("demo", "plan", "claude", b.gitCheckout(t, "co-plan"))...)
			return env{args: b.ladd("demo", "PLAN", "claude", co)}
		}},
		{"no harness", "--harness is required", func(t *testing.T, b *lineBench, co string) env {
			return env{args: []string{"line", "add", "demo", "--nick", "plan", "--checkout", co, "--state-dir", b.base}}
		}},
		{"an unknown harness", "the harnesses are claude, codex and opencode", func(t *testing.T, b *lineBench, co string) env {
			return env{args: b.ladd("demo", "plan", "gemini", co)}
		}},
		{"an OpenCode model that is not provider/model", "provider/model", func(t *testing.T, b *lineBench, co string) env {
			return env{args: b.ladd("demo", "plan", "opencode", co, "--model", "gpt-5")}
		}},
		{"an OpenCode model with an empty provider", "provider/model", func(t *testing.T, b *lineBench, co string) env {
			return env{args: b.ladd("demo", "plan", "opencode", co, "--model", "/gpt-5")}
		}},
		{"an OpenCode model with an empty name", "provider/model", func(t *testing.T, b *lineBench, co string) env {
			return env{args: b.ladd("demo", "plan", "opencode", co, "--model", "openai/")}
		}},
		{"an empty model", "--model is empty", func(t *testing.T, b *lineBench, co string) env {
			return env{args: b.ladd("demo", "plan", "claude", co, "--model", "")}
		}},
		{"a model with a control character", "control", func(t *testing.T, b *lineBench, co string) env {
			return env{args: b.ladd("demo", "plan", "claude", co, "--model", "x\ny")}
		}},
		{"a model that is a flag", "flag", func(t *testing.T, b *lineBench, co string) env {
			return env{args: b.ladd("demo", "plan", "codex", co, "--model", "--dangerously-bypass-approvals-and-sandbox")}
		}},
		{"a model that is too long", "over the 128 bytes", func(t *testing.T, b *lineBench, co string) env {
			return env{args: b.ladd("demo", "plan", "claude", co, "--model", strings.Repeat("m", 129))}
		}},
		{"a model with a character no id has", "which no model id", func(t *testing.T, b *lineBench, co string) env {
			return env{args: b.ladd("demo", "plan", "claude", co, "--model", "opus;rm")}
		}},
		{"no checkout", "--checkout is required", func(t *testing.T, b *lineBench, co string) env {
			return env{args: []string{"line", "add", "demo", "--nick", "plan", "--harness", "claude", "--state-dir", b.base}}
		}},
		{"a relative checkout", "absolute", func(t *testing.T, b *lineBench, co string) env {
			return env{args: b.ladd("demo", "plan", "claude", "rel/co")}
		}},
		{"a checkout that is not there", "no such file", func(t *testing.T, b *lineBench, co string) env {
			return env{args: b.ladd("demo", "plan", "claude", filepath.Join(b.root, "nope"))}
		}},
		{"a checkout that is a file", "not a directory", func(t *testing.T, b *lineBench, co string) env {
			f := filepath.Join(b.root, "a-file")
			if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			return env{args: b.ladd("demo", "plan", "claude", f)}
		}},
		{"a $ in the checkout", "'$'", func(t *testing.T, b *lineBench, co string) env {
			return env{args: b.ladd("demo", "plan", "claude", b.gitCheckout(t, "co$x"))}
		}},
		{"a control character in the checkout", "control character", func(t *testing.T, b *lineBench, co string) env {
			return env{args: b.ladd("demo", "plan", "claude", filepath.Join(b.root, "co\tx"))}
		}},
		{"a checkout that is not UTF-8", "not valid UTF-8", func(t *testing.T, b *lineBench, co string) env {
			return env{args: b.ladd("demo", "plan", "claude", co+"/bad\xffname")}
		}},
		{"a state directory that is not UTF-8", "not valid UTF-8", func(t *testing.T, b *lineBench, co string) env {
			return env{args: b.ladd("demo", "plan", "claude", co, "--state-dir", b.base+"/bad\xffname")}
		}},
		{"a temp directory that is not UTF-8", "not valid UTF-8", func(t *testing.T, b *lineBench, co string) env {
			return env{args: b.ladd("demo", "plan", "claude", co), env: []string{"TMPDIR=" + b.tmp + "/bad\xffname"}}
		}},
		{"a $ in the state directory of a Claude session", "'$'", func(t *testing.T, b *lineBench, co string) env {
			base := filepath.Join(b.root, "s$x")
			if err := os.Mkdir(base, 0o755); err != nil {
				t.Fatal(err)
			}
			mustLine(t, b, "line", "new", "demo2", "--chair", "colin", "--cap", "8", "--state-dir", base, "--server", b.db.server)
			return env{args: []string{"line", "add", "demo2", "--nick", "plan", "--harness", "claude", "--checkout", co, "--state-dir", base}}
		}},
		{"a state directory inside the checkout", "inside the checkout", func(t *testing.T, b *lineBench, co string) env {
			base := filepath.Join(co, "state")
			mustLine(t, b, "line", "new", "demo2", "--chair", "colin", "--cap", "8", "--state-dir", base, "--server", b.db.server)
			return env{args: []string{"line", "add", "demo2", "--nick", "plan", "--harness", "claude", "--checkout", co, "--state-dir", base}}
		}},
		{"a temp directory inside the checkout of a Codex session", "inside the checkout", func(t *testing.T, b *lineBench, co string) env {
			tmp := filepath.Join(co, "tmp")
			if err := os.Mkdir(tmp, 0o755); err != nil {
				t.Fatal(err)
			}
			return env{args: b.ladd("demo", "build", "codex", co), env: []string{"TMPDIR=" + tmp}}
		}},
		{"an .mcp.json that is not the line's", "is not one this line wrote", func(t *testing.T, b *lineBench, co string) env {
			if err := os.WriteFile(filepath.Join(co, ".mcp.json"), []byte(`{"mcpServers":{}}`), 0o644); err != nil {
				t.Fatal(err)
			}
			return env{args: b.ladd("demo", "plan", "claude", co)}
		}},
		{"an .mcp.json that is a link", "is not one this line wrote", func(t *testing.T, b *lineBench, co string) env {
			if err := os.Symlink(filepath.Join(b.root, "nowhere"), filepath.Join(co, ".mcp.json")); err != nil {
				t.Fatal(err)
			}
			return env{args: b.ladd("demo", "plan", "claude", co)}
		}},
		{"an .mcp.json the line wrote and someone edited", "is not the file this line wrote", func(t *testing.T, b *lineBench, co string) env {
			mustLine(t, b, b.ladd("demo", "plan", "claude", co)...)
			f, err := os.OpenFile(filepath.Join(co, ".mcp.json"), os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			f.WriteString("\n")
			f.Close()
			return env{args: b.ladd("demo", "review", "claude", co)}
		}},
		{"an .mcp.json the line wrote and someone replaced with a link", "is not a regular file", func(t *testing.T, b *lineBench, co string) env {
			mustLine(t, b, b.ladd("demo", "plan", "claude", co)...)
			other := filepath.Join(b.root, "elsewhere.json")
			if err := os.Rename(filepath.Join(co, ".mcp.json"), other); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(other, filepath.Join(co, ".mcp.json")); err != nil {
				t.Fatal(err)
			}
			return env{args: b.ladd("demo", "review", "claude", co)}
		}},
		{"a config.toml that is not the line's", "is not one this line wrote", func(t *testing.T, b *lineBench, co string) env {
			if err := os.Mkdir(filepath.Join(co, ".codex"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(co, ".codex", "config.toml"), []byte("model = \"x\"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			return env{args: b.ladd("demo", "build", "codex", co)}
		}},
		{"a .codex that is a link", "a symlink", func(t *testing.T, b *lineBench, co string) env {
			elsewhere := filepath.Join(b.root, "elsewhere")
			if err := os.Mkdir(elsewhere, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(elsewhere, filepath.Join(co, ".codex")); err != nil {
				t.Fatal(err)
			}
			return env{args: b.ladd("demo", "build", "codex", co)}
		}},
		{"a .codex that is a file", "not a directory", func(t *testing.T, b *lineBench, co string) env {
			if err := os.WriteFile(filepath.Join(co, ".codex"), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			return env{args: b.ladd("demo", "build", "codex", co)}
		}},
		{"a config.toml that is a link", "is not one this line wrote", func(t *testing.T, b *lineBench, co string) env {
			if err := os.Mkdir(filepath.Join(co, ".codex"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(b.root, "nowhere"), filepath.Join(co, ".codex", "config.toml")); err != nil {
				t.Fatal(err)
			}
			return env{args: b.ladd("demo", "build", "codex", co)}
		}},
		{"a nick with a thread record of its own", "already has a Codex thread record", func(t *testing.T, b *lineBench, co string) env {
			tr, err := state.OpenThreadRecord(b.base, "build", b.stream("demo"))
			if err != nil {
				t.Fatal(err)
			}
			if err := tr.Write(fakeThread(9)); err != nil {
				t.Fatal(err)
			}
			return env{args: b.ladd("demo", "build", "codex", co)}
		}},
		// A leftover record of the other harness's kind is refused the same way.
		{"a nick with an OpenCode session record, for a Codex session", "already has an OpenCode session record", func(t *testing.T, b *lineBench, co string) env {
			sr, err := state.OpenSessionRecord(b.base, "build", b.stream("demo"))
			if err != nil {
				t.Fatal(err)
			}
			if err := sr.Write("ses_00000000000000000000000009"); err != nil {
				t.Fatal(err)
			}
			return env{args: b.ladd("demo", "build", "codex", co)}
		}},
		{"no parley-line skill under the Claude root", "parley-line", func(t *testing.T, b *lineBench, co string) env {
			if err := os.Remove(filepath.Join(b.db.home, ".claude", "skills", "parley-line")); err != nil {
				t.Fatal(err)
			}
			return env{args: b.ladd("demo", "plan", "claude", co)}
		}},
		{"no parley-line skill under the Codex root", "parley-line", func(t *testing.T, b *lineBench, co string) env {
			if err := os.Remove(filepath.Join(b.db.home, ".agents", "skills", "parley-line")); err != nil {
				t.Fatal(err)
			}
			return env{args: b.ladd("demo", "build", "codex", co)}
		}},
		{"no codex", "codex:", func(t *testing.T, b *lineBench, co string) env {
			return env{args: b.ladd("demo", "build", "codex", co), env: []string{"PATH=" + b.pathWith(t, "claude", "ergo", "git", "cat", "mv")}}
		}},
		{"a Claude launch over Claude Code's cap", "over Claude Code's cap", func(t *testing.T, b *lineBench, co string) env {
			base := longBase(b)
			mustLine(t, b, "line", "new", "demo2", "--chair", "colin", "--cap", "8", "--state-dir", base, "--server", b.db.server)
			return env{args: []string{"line", "add", "demo2", "--nick", "plan", "--harness", "claude", "--checkout", co, "--state-dir", base}}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := newLineBench(t)
			lineUp(t, b, "demo")
			co := b.gitCheckout(t, "co")
			e := c.setup(t, b, co)
			before := b.lsnap(t, b.checkoutDirs(t)...)
			beforeLong := ""
			if strings.Contains(c.name, "cap") {
				beforeLong = treeListing(t, filepath.Join(b.root, strings.Repeat("a", 200)))
			}
			res := runParleyEnvIn(t, "", append(b.env(), e.env...), e.args...)
			// A missing skill link is doctor's FAIL, which fails the run with exit
			// 1, as table new's does; every other case is a refusal, exit 2.
			code := 2
			if strings.HasPrefix(c.name, "no parley-line skill") {
				code = 1
			}
			if res.code != code {
				t.Fatalf("exit %d, want %d\nstdout: %s\nstderr: %s", res.code, code, res.stdout, res.stderr)
			}
			if !strings.Contains(res.stderr, c.want) {
				t.Errorf("stderr does not say %q:\n%s", c.want, res.stderr)
			}
			if res.stdout != "" {
				t.Errorf("a refused run printed on stdout: %q", res.stdout)
			}
			b.wantLineUnchanged(t, before, b.checkoutDirs(t)...)
			if beforeLong != "" {
				if after := treeListing(t, filepath.Join(b.root, strings.Repeat("a", 200))); after != beforeLong {
					t.Errorf("the run changed the long state directory.\nbefore:\n%s\nafter:\n%s", beforeLong, after)
				}
			}
			b.wantNoCodexCalls(t)
		})
	}
}

// TestLineAddRefusesANickWhoseLockIsHeld: a running session or pump holds the
// nick's lock, and the add refuses at once, exit 1, with nothing changed.
func TestLineAddRefusesANickWhoseLockIsHeld(t *testing.T) {
	for _, harness := range []string{"claude", "codex"} {
		t.Run(harness, func(t *testing.T) {
			b := newLineBench(t)
			lineUp(t, b, "demo")
			co := b.gitCheckout(t, "co")
			l, err := state.LockNick(b.base, "plan")
			if err != nil {
				t.Fatal(err)
			}
			before := b.lsnap(t, co)
			res := b.run(t, b.ladd("demo", "plan", harness, co)...)
			if res.code != 1 || !strings.Contains(res.stderr, `"plan"`) || !strings.Contains(res.stderr, "running") {
				t.Fatalf("exit %d, want 1 naming plan\n%s", res.code, res.stderr)
			}
			b.wantLineUnchanged(t, before, co)
			b.wantNoCodexCalls(t)
			if err := l.Release(); err != nil {
				t.Fatal(err)
			}
			mustLine(t, b, b.ladd("demo", "plan", harness, co)...)
		})
	}
}

// TestLineAddWithNoCodexNeededDoesNotReadTheOtherRoot: a Claude session needs the
// line skill under the Claude root only, and a Codex one under the Codex root
// only; the parley-table and parley-build links are nobody's business.
func TestLineAddReadsOnlyTheRootItsHarnessReads(t *testing.T) {
	b := newLineBench(t)
	lineUp(t, b, "demo")
	if err := os.RemoveAll(filepath.Join(b.db.home, ".agents")); err != nil {
		t.Fatal(err)
	}
	mustLine(t, b, b.ladd("demo", "plan", "claude", b.gitCheckout(t, "co1"))...)
	b2 := newLineBench(t)
	lineUp(t, b2, "demo")
	if err := os.RemoveAll(filepath.Join(b2.db.home, ".claude")); err != nil {
		t.Fatal(err)
	}
	mustLine(t, b2, b2.ladd("demo", "build", "codex", b2.gitCheckout(t, "co1"))...)
	b3 := newLineBench(t)
	lineUp(t, b3, "demo")
	for _, root := range []string{".claude", ".agents"} {
		for _, name := range []string{"parley-table", "parley-build"} {
			if err := os.Remove(filepath.Join(b3.db.home, root, "skills", name)); err != nil {
				t.Fatal(err)
			}
		}
	}
	mustLine(t, b3, b3.ladd("demo", "plan", "claude", b3.gitCheckout(t, "co1"))...)
}

// TestLineAddDoctorFailRefusesBeforeAnyChange: a machine that cannot host the
// session fails with exit 1, as table new does on doctor's FAIL, with nothing
// changed.
func TestLineAddDoctorFailRefusesBeforeAnyChange(t *testing.T) {
	t.Run("no claude", func(t *testing.T) {
		b := newLineBench(t)
		lineUp(t, b, "demo")
		co := b.gitCheckout(t, "co")
		before := b.lsnap(t, co)
		res := runParleyEnvIn(t, "", b.envWithPath(b.pathWith(t, "codex", "ergo", "git", "cat", "mv")), b.ladd("demo", "plan", "claude", co)...)
		if res.code != 1 || !strings.Contains(res.stderr, "FAIL") || !strings.Contains(res.stderr, "claude") {
			t.Fatalf("exit %d, want 1 with doctor's claude FAIL\n%s", res.code, res.stderr)
		}
		b.wantLineUnchanged(t, before, co)
	})
	t.Run("a codex flag gone", func(t *testing.T) {
		b := newLineBench(t)
		lineUp(t, b, "demo")
		co := b.gitCheckout(t, "co")
		script := mustRead(t, b.codex)
		patched := strings.Replace(string(script), `--help) cat`, `--help) if [ "$1" = delete ]; then echo "      --nothing"; exit 0; fi; cat`, 1)
		if err := os.WriteFile(b.codex, []byte(patched), 0o755); err != nil {
			t.Fatal(err)
		}
		before := b.lsnap(t, co)
		res := b.run(t, b.ladd("demo", "build", "codex", co)...)
		if res.code != 1 || !strings.Contains(res.stderr, "FAIL") || !strings.Contains(res.stderr, "--force") {
			t.Fatalf("exit %d, want 1 with doctor's delete FAIL\n%s", res.code, res.stderr)
		}
		b.wantLineUnchanged(t, before, co)
	})
}

// TestLineAddFailureRollsEverythingBack: a codex that fails, announces no thread,
// or ends without a completed turn is exit 1 and a machine as it was, the
// checkout, its exclude file and the record included, with every thread it did
// start deleted.
func TestLineAddFailureRollsEverythingBack(t *testing.T) {
	cases := []struct {
		name string
		mode string
		want string
		// deleted is whether a delete of the thread of exec call 1 is made, as call 2.
		deleted bool
	}{
		{"codex exits nonzero", "fail", "boom: not logged in", false},
		{"no thread.started", "nothread", "no thread", false},
		{"a thread that is not a UUID", "badid", "not a UUID", false},
		{"thread announced, then the turn fails", "failafter", "the model went away", true},
		{"codex exits 0 but the turn failed", "failzero", "quota exceeded", true},
		{"codex exits 0 with no turn.completed", "nocomplete", "no turn.completed event", true},
		{"stdout past the cap", "overflow", "cut off", true},
		{"codex exits 0 but the turn carried an error", "errorzero", "stream broke", true},
	}
	for _, shape := range []struct {
		name  string
		setup func(t *testing.T, b *lineBench, co string)
	}{
		{"a fresh checkout", func(t *testing.T, b *lineBench, co string) {}},
		{"a checkout with a .codex directory and an exclude file of its own", func(t *testing.T, b *lineBench, co string) {
			if err := os.Mkdir(filepath.Join(co, ".codex"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(co, ".git", "info", "exclude"), []byte("# mine\n*.swp"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		for _, c := range cases {
			t.Run(shape.name+"/"+c.name, func(t *testing.T) {
				b := newLineBench(t)
				lineUp(t, b, "demo")
				co := b.gitCheckout(t, "co")
				shape.setup(t, b, co)
				b.setMode(t, 1, c.mode)
				before := b.lsnap(t, co)
				res := b.run(t, b.ladd("demo", "build", "codex", co)...)
				if res.code != 1 {
					t.Fatalf("exit %d, want 1\nstdout: %s\nstderr: %s", res.code, res.stdout, res.stderr)
				}
				if !strings.Contains(res.stderr, c.want) {
					t.Errorf("stderr does not say %q:\n%s", c.want, res.stderr)
				}
				if !strings.Contains(res.stderr, "rolled back") || !strings.Contains(res.stderr, "launch file") {
					t.Errorf("stderr does not say what was rolled back:\n%s", res.stderr)
				}
				if res.stdout != "" {
					t.Errorf("a failed run printed on stdout: %q", res.stdout)
				}
				deletes := 0
				for n := 1; n <= b.callCount(); n++ {
					argv := b.call(t, n)
					if argv[0] != "delete" {
						continue
					}
					deletes++
					if strings.Join(argv, " ") != "delete --force "+fakeThread(1) || n != 2 {
						t.Errorf("call %d is %q, an unexpected delete", n, argv)
					}
				}
				if want := map[bool]int{true: 1, false: 0}[c.deleted]; deletes != want {
					t.Errorf("%d delete calls, want %d", deletes, want)
				}
				after := b.lsnap(t, co)
				if got, want := withoutCalls(after), withoutCalls(before); got != want {
					t.Fatalf("the run left a trace.\nbefore:\n%s\nafter:\n%s", want, got)
				}
			})
		}
	}
}

// TestLineAddRollsBackAClaudeSessionWhenTheRecordCannotBeWritten: the record is the
// last step, and a failure there takes the launch file and the exclude line down
// again.
func TestLineAddRollsBackAClaudeSessionWhenTheRecordCannotBeWritten(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root is not stopped by a directory's mode")
	}
	b := newLineBench(t)
	lineUp(t, b, "demo")
	co := b.gitCheckout(t, "co")
	dir := filepath.Join(b.base, "lines", "demo")
	before := b.lsnap(t, co)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	res := b.run(t, b.ladd("demo", "plan", "claude", co)...)
	if res.code != 1 || !strings.Contains(res.stderr, "rolled back") || !strings.Contains(res.stderr, "launch file") {
		t.Fatalf("exit %d, want 1 with the rollback told\n%s", res.code, res.stderr)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	b.wantLineUnchanged(t, before, co)
}

// TestLineAddInterruptedRollsEverythingBack: a signal while the Codex turn runs
// stops the add, deletes the thread the turn had announced and leaves the
// machine as it was.
func TestLineAddInterruptedRollsEverythingBack(t *testing.T) {
	b := newLineBench(t)
	lineUp(t, b, "demo")
	co := b.gitCheckout(t, "co")
	script := string(mustRead(t, b.codex))
	hold := "\techo '{\"type\":\"turn.started\"}'\n\tif [ -f \"$d/items\" ]"
	// The fake marks "announced" once it has printed thread.started, so the
	// interrupt below lands only after the thread exists: otherwise a slow runner
	// can signal before the announcement, and there is no thread to delete.
	patched := strings.Replace(script, hold, "\tif [ -f \"$d/hold\" ]; then : > \"$d/announced\"; sleep 3; fi\n"+hold, 1)
	if patched == script {
		t.Fatal("could not patch the fake codex")
	}
	if err := os.WriteFile(b.codex, []byte(patched), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.fake, "hold"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	before := b.lsnap(t, co)

	cmd := exec.Command(binPath, b.ladd("demo", "build", "codex", co)...)
	cmd.Env = append(os.Environ(), b.env()...)
	stderr := &syncBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := joinCmd(cmd)
	b.waitCall(t, 1, stderr)
	waitForStderr(t, stderr, "starting Codex session build")
	announced := filepath.Join(b.fake, "announced")
	for deadline := time.Now().Add(childDeadline); ; {
		if _, err := os.Stat(announced); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the fake codex never announced its thread:\n%s", stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(childDeadline):
		_ = cmd.Process.Kill()
		t.Fatalf("the add did not end after SIGINT:\n%s", stderr.String())
	}
	if code := cmd.ProcessState.ExitCode(); code != 1 {
		t.Errorf("exit %d, want 1\n%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "interrupted") || !strings.Contains(stderr.String(), "rolled back") {
		t.Errorf("stderr does not say it was interrupted and rolled back:\n%s", stderr.String())
	}
	// The thread the turn announced was deleted.
	deleted := false
	for n := 1; n <= b.callCount(); n++ {
		if argv := b.call(t, n); argv[0] == "delete" && strings.Join(argv, " ") == "delete --force "+fakeThread(1) {
			deleted = true
		}
	}
	if !deleted {
		t.Errorf("the thread was not deleted; calls: %d", b.callCount())
	}
	if got, want := withoutCalls(b.lsnap(t, co)), withoutCalls(before); got != want {
		t.Fatalf("the interrupted run left a trace.\nbefore:\n%s\nafter:\n%s", want, got)
	}
}
