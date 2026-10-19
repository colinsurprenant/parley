package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/colinsurprenant/parley/internal/state"
)

// lineBench is a tableBench with the extras the line verbs need: checkouts for
// sessions to live in, the line's args, and a snapshot that covers them.
type lineBench struct{ *tableBench }

func newLineBench(t *testing.T) *lineBench { return &lineBench{newTableBench(t)} }

func (b *lineBench) lnew(id string, extra ...string) []string {
	return append([]string{"line", "new", id, "--chair", "colin", "--cap", "8", "--state-dir", b.base, "--server", b.db.server}, extra...)
}

func (b *lineBench) ladd(id, nick, harness, checkout string, extra ...string) []string {
	return append([]string{"line", "add", id, "--nick", nick, "--harness", harness, "--checkout", checkout, "--state-dir", b.base}, extra...)
}

func (b *lineBench) ldrop(id, nick string, extra ...string) []string {
	return append([]string{"line", "drop", id, nick, "--state-dir", b.base}, extra...)
}

func (b *lineBench) lend(id string, extra ...string) []string {
	return append([]string{"line", "end", id, "--state-dir", b.base}, extra...)
}

// gitCheckout is a git checkout of its own for a session.
func (b *lineBench) gitCheckout(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(b.root, name)
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	initRepo(t, dir, nil)
	return dir
}

// plainCheckout is a directory that is no git checkout.
func (b *lineBench) plainCheckout(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(b.root, name)
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func mustLine(t *testing.T, b *lineBench, args ...string) result {
	t.Helper()
	res := b.run(t, args...)
	if res.code != 0 {
		t.Fatalf("parley %v exited %d\nstdout: %s\nstderr: %s", args, res.code, res.stdout, res.stderr)
	}
	return res
}

// lineUp makes line id and returns it.
func lineUp(t *testing.T, b *lineBench, id string, extra ...string) {
	t.Helper()
	mustLine(t, b, b.lnew(id, extra...)...)
}

func (b *lineBench) record(t *testing.T, id string) lineRecord {
	t.Helper()
	raw := mustRead(t, filepath.Join(b.base, "lines", id, "line.json"))
	var rec lineRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("line.json: %v\n%s", err, raw)
	}
	return rec
}

func (b *lineBench) stream(id string) string { return "irc://" + b.db.server + "/#" + id }

func fileDigestOf(t *testing.T, path string) string {
	t.Helper()
	sum := sha256.Sum256(mustRead(t, path))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// treeListing lists dir with modes and, for a regular file, a digest; a git
// directory is left out but for the exclude file, whose whole content is shown.
func treeListing(t *testing.T, dir string) string {
	t.Helper()
	var out strings.Builder
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if rel == filepath.Join(".git", "info", "exclude") {
			fmt.Fprintf(&out, "%s %q\n", rel, mustRead(t, p))
			return nil
		}
		if rel == ".git" {
			return fs.SkipDir
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case info.Mode().IsRegular():
			fmt.Fprintf(&out, "%s %v %s\n", rel, info.Mode(), fileDigestOf(t, p))
		case info.Mode()&fs.ModeSymlink != 0:
			to, _ := os.Readlink(p)
			fmt.Fprintf(&out, "%s %v -> %s\n", rel, info.Mode(), to)
		default:
			fmt.Fprintf(&out, "%s %v\n", rel, info.Mode())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// lsnap is snapshot and the content of every line record and the given
// directories: everything a line verb could leave behind.
func (b *lineBench) lsnap(t *testing.T, dirs ...string) string {
	t.Helper()
	var s strings.Builder
	s.WriteString(b.snapshot(t))
	if recs, _ := filepath.Glob(filepath.Join(b.base, "lines", "*", "line.json")); len(recs) > 0 {
		for _, r := range recs {
			fmt.Fprintf(&s, "record %s:\n%s", r, mustRead(t, r))
		}
	}
	for _, d := range dirs {
		fmt.Fprintf(&s, "tree %s:\n%s", d, treeListing(t, d))
	}
	return s.String()
}

func (b *lineBench) wantLineUnchanged(t *testing.T, before string, dirs ...string) {
	t.Helper()
	if after := b.lsnap(t, dirs...); after != before {
		t.Fatalf("the run left a trace.\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// mcpArgs is what a line's Claude session has in its .mcp.json.
func (b *lineBench) mcpArgs(id string) []string {
	return []string{"channel", "--stream", b.stream(id), "--nick", "${PARLEY_NICK}", "--state-dir", b.base,
		"--skill", "line", "--chair", "colin", "--cap", "8", "--window", "10m"}
}

// TestLineNewSetsTheLineUp: the record, with no sessions, and what is printed.
func TestLineNewSetsTheLineUp(t *testing.T) {
	b := newLineBench(t)
	res := mustLine(t, b, b.lnew("demo")...)
	t.Logf("stdout:\n%s", res.stdout)
	t.Logf("stderr:\n%s", res.stderr)

	dir := filepath.Join(b.base, "lines", "demo")
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("line directory: %v, %v; want 0700", fi, err)
	}
	path := filepath.Join(dir, "line.json")
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("line.json: %v, %v; want 0600", fi, err)
	}
	raw := mustRead(t, path)
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range generic {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if got, want := strings.Join(keys, ","), "cap,chair,created,id,server,sessions,state_dir,stream,window"; got != want {
		t.Errorf("line.json fields %s, want %s: the record names things and holds no stream content", got, want)
	}
	rec := b.record(t, "demo")
	if rec.ID != "demo" || rec.Stream != b.stream("demo") || rec.Server != b.db.server || rec.StateDir != b.base ||
		rec.Chair != "colin" || rec.Cap != 8 || rec.Window != "10m" || rec.CodexBin != "" || len(rec.Sessions) != 0 {
		t.Errorf("record %+v", rec)
	}
	if arr, ok := generic["sessions"].([]any); !ok || len(arr) != 0 {
		t.Errorf("sessions = %v, want an empty list", generic["sessions"])
	}
	if _, err := time.Parse(time.RFC3339, rec.Created); err != nil {
		t.Errorf("created %q: %v", rec.Created, err)
	}

	// The doctor warning for a server that is not up went to stderr, and the
	// skills are not checked yet.
	if !strings.Contains(res.stderr, "doctor: warn") || strings.Contains(res.stderr, "parley-line") {
		t.Errorf("stderr:\n%s", res.stderr)
	}
	wantInOrder(t, res.stdout, []string{
		fmt.Sprintf("'%s' server --listen '%s'", binPath, b.db.server),
		"/join #demo",
		"/msg NickServ INFO colin",
		"/msg NickServ REGISTER <password>",
		fmt.Sprintf("'%s' line add demo --nick <nick> --harness <claude|codex> [--model <model>] --checkout <absolute dir> --state-dir '%s'", binPath, b.base),
		fmt.Sprintf("When it is over: '%s' line end demo --state-dir '%s'", binPath, b.base),
	})
	if strings.Contains(res.stdout, "doctor:") {
		t.Errorf("doctor's lines are on stdout:\n%s", res.stdout)
	}
	// Nothing but the line's own directory was made in the base.
	b.wantNoCodexCalls(t)
}

func (b *lineBench) wantNoCodexCalls(t *testing.T) {
	t.Helper()
	if n := b.callCount(); n != 0 {
		t.Errorf("codex was called %d times", n)
	}
}

// TestLineNewTakesTheWindowAndCodexBinAndPrintsNoServerLineWhenItAnswers.
func TestLineNewTakesTheWindowAndCodexBin(t *testing.T) {
	addr := startErgo(t)
	b := newLineBench(t)
	b.db.server = addr
	res := mustLine(t, b, b.lnew("demo", "--window", "1h30m0s", "--codex-bin", b.codex)...)
	if strings.Contains(res.stdout, "' server") {
		t.Errorf("a real server answers, yet stdout has a server line:\n%s", res.stdout)
	}
	rec := b.record(t, "demo")
	if rec.Window != "1h30m" || rec.CodexBin != b.codex {
		t.Errorf("window %q codex_bin %q, want 1h30m and %s", rec.Window, rec.CodexBin, b.codex)
	}
	// --cap 0 is a breaker that is off, and the line says so.
	res = mustLine(t, b, b.lnew("off", "--cap", "0")...)
	if !strings.Contains(res.stdout, "breaker off") {
		t.Errorf("stdout does not say the breaker is off:\n%s", res.stdout)
	}
}

// TestLineNewRefusesBeforeAnyChange: each refusal is a configuration error, exit
// 2, and the machine is as it was.
func TestLineNewRefusesBeforeAnyChange(t *testing.T) {
	cases := []struct {
		name  string
		want  string
		setup func(t *testing.T, b *lineBench) []string
	}{
		{"bad id, upper case", "line id", func(t *testing.T, b *lineBench) []string { return b.lnew("DEMO") }},
		{"bad id, underscore", "line id", func(t *testing.T, b *lineBench) []string { return b.lnew("a_b") }},
		{"bad id, slash", "line id", func(t *testing.T, b *lineBench) []string { return b.lnew("a/b") }},
		{"bad id, too long", "line id", func(t *testing.T, b *lineBench) []string { return b.lnew(strings.Repeat("a", 33)) }},
		{"no id", "line id is required", func(t *testing.T, b *lineBench) []string {
			return []string{"line", "new", "--chair", "colin", "--cap", "8", "--state-dir", b.base}
		}},
		{"two ids", "unexpected argument", func(t *testing.T, b *lineBench) []string { return append(b.lnew("demo"), "other") }},
		{"no chair", "--chair", func(t *testing.T, b *lineBench) []string {
			return []string{"line", "new", "demo", "--cap", "8", "--state-dir", b.base}
		}},
		{"bad chair", "--chair", func(t *testing.T, b *lineBench) []string { return b.lnew("demo", "--chair", "co lin") }},
		{"lines as the chair", "lines", func(t *testing.T, b *lineBench) []string { return b.lnew("demo", "--chair", "Lines") }},
		{"no cap", "--cap is required", func(t *testing.T, b *lineBench) []string {
			return []string{"line", "new", "demo", "--chair", "colin", "--state-dir", b.base}
		}},
		{"negative cap", "--cap", func(t *testing.T, b *lineBench) []string { return b.lnew("demo", "--cap", "-1") }},
		{"zero window", "--window", func(t *testing.T, b *lineBench) []string { return b.lnew("demo", "--window", "0s") }},
		{"negative window", "--window", func(t *testing.T, b *lineBench) []string { return b.lnew("demo", "--window", "-5m") }},
		{"a window that is not a duration", "window", func(t *testing.T, b *lineBench) []string { return b.lnew("demo", "--window", "soon") }},
		{"a server that is a host name", "--server", func(t *testing.T, b *lineBench) []string { return b.lnew("demo", "--server", "example.com:6667") }},
		{"an empty codex-bin", "--codex-bin", func(t *testing.T, b *lineBench) []string { return b.lnew("demo", "--codex-bin", "") }},
		{"a codex-bin that is not there", "--codex-bin", func(t *testing.T, b *lineBench) []string {
			return b.lnew("demo", "--codex-bin", filepath.Join(b.root, "no-such-codex"))
		}},
		{"an unknown flag", "flag provided but not defined", func(t *testing.T, b *lineBench) []string { return b.lnew("demo", "--target", "x") }},
		{"a table with the id", `"demo" is already a table`, func(t *testing.T, b *lineBench) []string {
			if err := os.MkdirAll(filepath.Join(b.base, "tables", "demo"), 0o700); err != nil {
				t.Fatal(err)
			}
			return b.lnew("demo")
		}},
		{"a build with the id", `"demo" is already a build`, func(t *testing.T, b *lineBench) []string {
			if err := os.MkdirAll(filepath.Join(b.base, "builds", "demo"), 0o700); err != nil {
				t.Fatal(err)
			}
			return b.lnew("demo")
		}},
		{"a line with the id", `line "demo" already exists`, func(t *testing.T, b *lineBench) []string {
			lineUp(t, b, "demo")
			return b.lnew("demo")
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := newLineBench(t)
			args := c.setup(t, b)
			before := b.lsnap(t)
			res := b.run(t, args...)
			if res.code != 2 {
				t.Fatalf("exit %d, want 2\nstdout: %s\nstderr: %s", res.code, res.stdout, res.stderr)
			}
			if !strings.Contains(res.stderr, c.want) {
				t.Errorf("stderr does not say %q:\n%s", c.want, res.stderr)
			}
			if res.stdout != "" {
				t.Errorf("a refused run printed on stdout: %q", res.stdout)
			}
			b.wantLineUnchanged(t, before)
		})
	}
}

// TestLineNewRefusesAnIdLiveAsATableOrABuild: the id is one namespace for the
// three, and a table or a build made from the same id is refused the same way.
func TestLineNewRefusesAnIdLiveAsATableOrABuild(t *testing.T) {
	t.Run("a table", func(t *testing.T) {
		b := newLineBench(t)
		mustTable(t, b.tableBench, b.newArgsTable("x1")...)
		before := b.lsnap(t)
		res := b.run(t, b.lnew("x1")...)
		if res.code != 2 || !strings.Contains(res.stderr, `"x1" is already a table`) || !strings.Contains(res.stderr, "parley table end x1") {
			t.Fatalf("exit %d, want 2 naming the table\n%s", res.code, res.stderr)
		}
		b.wantLineUnchanged(t, before)
	})
	t.Run("a build", func(t *testing.T) {
		b := newLineBench(t)
		buildUp(t, b.tableBench, "x1", nil)
		before := b.lsnap(t)
		res := b.run(t, b.lnew("x1")...)
		if res.code != 2 || !strings.Contains(res.stderr, `"x1" is already a build`) || !strings.Contains(res.stderr, "parley build end x1") {
			t.Fatalf("exit %d, want 2 naming the build\n%s", res.code, res.stderr)
		}
		b.wantLineUnchanged(t, before)
	})
	t.Run("a table new for a line's id", func(t *testing.T) {
		b := newLineBench(t)
		lineUp(t, b, "x1")
		before := b.lsnap(t)
		res := b.run(t, b.newArgsTable("x1")...)
		if res.code != 2 || !strings.Contains(res.stderr, `"x1" is already a line`) || !strings.Contains(res.stderr, "parley line end x1") {
			t.Fatalf("exit %d, want 2 naming the line\n%s", res.code, res.stderr)
		}
		b.wantLineUnchanged(t, before)
	})
	t.Run("a build new for a line's id", func(t *testing.T) {
		b := newLineBench(t)
		lineUp(t, b, "x1")
		before := b.lsnap(t)
		res := b.run(t, b.buildArgs("x1", nil)...)
		if res.code != 2 || !strings.Contains(res.stderr, `"x1" is already a line`) || !strings.Contains(res.stderr, "parley line end x1") {
			t.Fatalf("exit %d, want 2 naming the line\n%s", res.code, res.stderr)
		}
		b.wantLineUnchanged(t, before)
	})
}

// newArgsTable is a valid `table new` for id with one Claude seat.
func (b *lineBench) newArgsTable(id string) []string {
	return b.tableBench.newArgs(id, []string{"claude-t=claude"})
}

// TestLineNewFailureRollsBack: a base that cannot hold the line's directory is a
// runtime failure, exit 1, and what the run made is gone.
func TestLineNewFailureRollsBack(t *testing.T) {
	b := newLineBench(t)
	if err := os.MkdirAll(b.base, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.base, "lines"), []byte("a file"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := b.lsnap(t)
	res := b.run(t, b.lnew("demo")...)
	if res.code != 1 {
		t.Fatalf("exit %d, want 1\nstdout: %s\nstderr: %s", res.code, res.stdout, res.stderr)
	}
	if res.stdout != "" {
		t.Errorf("a failed run printed on stdout: %q", res.stdout)
	}
	b.wantLineUnchanged(t, before)
}

// TestLineVerbsHoldTheIDLock: a second command for the id refuses at once, exit 1,
// whether the lock is a table's, a build's or a line's, and says a line command
// can hold it.
func TestLineVerbsHoldTheIDLock(t *testing.T) {
	b := newLineBench(t)
	lineUp(t, b, "demo")
	co := b.gitCheckout(t, "co")
	l, err := state.LockID(b.base, "demo")
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{b.lnew("demo"), b.ladd("demo", "plan", "claude", co), b.ldrop("demo", "plan"), b.lend("demo")} {
		res := b.run(t, args...)
		if res.code != 1 || !strings.Contains(res.stderr, idInUse) || !strings.Contains(res.stderr, "line command") {
			t.Errorf("%v with the id's lock held: exit %d\n%s", args[:2], res.code, res.stderr)
		}
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	mustLine(t, b, b.ladd("demo", "plan", "claude", co)...)
	// And a line verb does not leave the lock held.
	free, err := state.LockID(b.base, "demo")
	if err != nil {
		t.Fatalf("a finished line add left the id's lock held: %v", err)
	}
	free.Release()
}

// TestLineVerbsOnALineThatIsNotThereTakeNoLock: exit 2, and no lock file is made
// for an id that was never a line.
func TestLineVerbsOnALineThatIsNotThereTakeNoLock(t *testing.T) {
	b := newLineBench(t)
	co := b.gitCheckout(t, "co")
	for _, args := range [][]string{b.ladd("nope", "plan", "claude", co), b.ldrop("nope", "plan"), b.lend("nope")} {
		res := b.run(t, args...)
		if res.code != 2 || !strings.Contains(res.stderr, `no line "nope"`) {
			t.Errorf("%v: exit %d, want 2 naming the line\n%s", args[:2], res.code, res.stderr)
		}
	}
	if _, err := os.Stat(filepath.Join(b.base, ".ids")); err == nil {
		t.Error("a verb on a line that is not there made the id lock")
	}
}

// sessionByNick finds a session in a record.
func sessionByNick(t *testing.T, rec lineRecord, nick string) lineSession {
	t.Helper()
	for _, s := range rec.Sessions {
		if s.Nick == nick {
			return s
		}
	}
	t.Fatalf("no session %q in %+v", nick, rec.Sessions)
	return lineSession{}
}

func readExcludeForTest(t *testing.T, co string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(co, ".git", "info", "exclude"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestLineAddClaudeSession is the acceptance run for a Claude session: the
// .mcp.json, the exclude line, the record and what is printed.
func TestLineAddClaudeSession(t *testing.T) {
	b := newLineBench(t)
	lineUp(t, b, "demo")
	co := b.gitCheckout(t, "co-plan")
	excludeBefore := readExcludeForTest(t, co)
	res := mustLine(t, b, b.ladd("demo", "plan", "claude", co, "--model", "opus")...)
	t.Logf("stdout:\n%s", res.stdout)
	t.Logf("stderr:\n%s", res.stderr)

	// The .mcp.json: its args exactly.
	path := filepath.Join(co, ".mcp.json")
	raw := mustRead(t, path)
	var mcp struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &mcp); err != nil {
		t.Fatalf(".mcp.json is not JSON: %v\n%s", err, raw)
	}
	srv, ok := mcp.MCPServers["parley"]
	if !ok || len(mcp.MCPServers) != 1 {
		t.Fatalf(".mcp.json servers %v, want only parley", mcp.MCPServers)
	}
	if srv.Command != binPath {
		t.Errorf(".mcp.json command %q, want this binary %q", srv.Command, binPath)
	}
	if want := b.mcpArgs("demo"); strings.Join(srv.Args, "\x00") != strings.Join(want, "\x00") {
		t.Errorf(".mcp.json args\n%q\nwant\n%q", srv.Args, want)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o644 {
		t.Errorf(".mcp.json mode %v (%v), want 0644", fi.Mode(), err)
	}
	// And they are the launch flags of the channel: --check on them passes.
	check := append([]string(nil), srv.Args...)
	for i, a := range check {
		if a == "${PARLEY_NICK}" {
			check[i] = "plan"
		}
	}
	if c := runCheck(t, b.tmp, check...); c.code != 0 {
		t.Errorf("channel --check on the .mcp.json args exited %d\n%s%s", c.code, c.stdout, c.stderr)
	}

	// The exclude line is appended, nothing else touched, and git status is clean.
	exclude := filepath.Join(co, ".git", "info", "exclude")
	if got := readExcludeForTest(t, co); got != excludeBefore+"/.mcp.json\n" {
		t.Errorf("info/exclude = %q, want %q and /.mcp.json", got, excludeBefore)
	}
	if got := gitIn(t, co, "status", "--porcelain"); got != "" {
		t.Errorf("the checkout is not clean: %s", got)
	}

	// The record.
	rec := b.record(t, "demo")
	if len(rec.Sessions) != 1 {
		t.Fatalf("sessions %+v", rec.Sessions)
	}
	want := lineSession{Nick: "plan", Harness: "claude", Model: "opus", Checkout: co,
		Files: []lineFile{{Path: path, SHA256: fileDigestOf(t, path), ExcludeFile: exclude}}}
	if fmt.Sprint(rec.Sessions[0]) != fmt.Sprint(want) {
		t.Errorf("session %+v, want %+v", rec.Sessions[0], want)
	}
	if fi, err := os.Stat(filepath.Join(b.base, "lines", "demo", "line.json")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("line.json mode %v (%v), want 0600", fi.Mode(), err)
	}
	// Nothing for a Codex session was made: no .codex, no reply directory, no
	// call to codex.
	if _, err := os.Stat(filepath.Join(co, ".codex")); err == nil {
		t.Error("a Claude session got a .codex")
	}
	if ents, _ := os.ReadDir(b.tmp); len(ents) != 0 {
		t.Errorf("a Claude session made %d entries in $TMPDIR", len(ents))
	}
	b.wantNoCodexCalls(t)

	// What is printed, in order, every path quoted.
	launch := "PARLEY_NICK=plan claude --dangerously-load-development-channels server:parley --model 'opus'"
	wantInOrder(t, res.stdout, []string{
		fmt.Sprintf("cd '%s'", co),
		launch,
		launch + " --resume <session-id>",
		"/mcp alone also passes on a launch without --dangerously-load-development-channels, which never hears the channel.",
		"Then type /parley-line.",
	})
	if !strings.Contains(res.stdout, channelNotice) {
		t.Errorf("stdout lacks the Channels notice:\n%s", res.stdout)
	}
	if !strings.Contains(res.stdout, "Wrote "+path) || !strings.Contains(res.stdout, "/.mcp.json to "+exclude) {
		t.Errorf("stdout does not say what was written:\n%s", res.stdout)
	}
	if strings.Contains(res.stdout, "network") {
		t.Errorf("a Claude session's output speaks of network access:\n%s", res.stdout)
	}
	// doctor checked the parley-line skill, and said nothing as it is there.
	if strings.Contains(res.stderr, "FAIL") {
		t.Errorf("stderr:\n%s", res.stderr)
	}
}

// TestLineAddSecondClaudeSessionSharesTheFile: two sessions in one checkout share
// the .mcp.json the first wrote and the one exclude line.
func TestLineAddSecondClaudeSessionSharesTheFile(t *testing.T) {
	b := newLineBench(t)
	lineUp(t, b, "demo")
	co := b.gitCheckout(t, "co")
	mustLine(t, b, b.ladd("demo", "plan", "claude", co)...)
	path := filepath.Join(co, ".mcp.json")
	before := treeListing(t, co)
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	res := mustLine(t, b, b.ladd("demo", "review", "claude", co)...)
	if after := treeListing(t, co); after != before {
		t.Errorf("the second session changed the checkout.\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if st2, err := os.Stat(path); err != nil || !os.SameFile(st, st2) {
		t.Errorf("the .mcp.json was replaced: %v", err)
	}
	if got := strings.Count(readExcludeForTest(t, co), "/.mcp.json\n"); got != 1 {
		t.Errorf("info/exclude holds the line %d times", got)
	}
	if !strings.Contains(res.stdout, "Shares "+path) {
		t.Errorf("stdout does not say the file is shared:\n%s", res.stdout)
	}
	rec := b.record(t, "demo")
	a, c := sessionByNick(t, rec, "plan"), sessionByNick(t, rec, "review")
	if fmt.Sprint(a.Files) != fmt.Sprint(c.Files) || a.Files[0].ExcludeFile == "" {
		t.Errorf("the sessions record %+v and %+v, want the same file entry with its exclude file", a.Files, c.Files)
	}
	// A third in another checkout gets a file of its own.
	other := b.gitCheckout(t, "co2")
	mustLine(t, b, b.ladd("demo", "third", "claude", other)...)
	if _, err := os.Stat(filepath.Join(other, ".mcp.json")); err != nil {
		t.Errorf("no .mcp.json in the other checkout: %v", err)
	}
}

// TestLineAddNonGitCheckoutGetsNoExcludeLine: a directory that is no git
// checkout is allowed, and its file is the line's all the same.
func TestLineAddNonGitCheckoutGetsNoExcludeLine(t *testing.T) {
	b := newLineBench(t)
	lineUp(t, b, "demo")
	co := b.plainCheckout(t, "plain")
	res := mustLine(t, b, b.ladd("demo", "plan", "claude", co)...)
	if _, err := os.Stat(filepath.Join(co, ".mcp.json")); err != nil {
		t.Fatal(err)
	}
	rec := b.record(t, "demo")
	if f := rec.Sessions[0].Files[0]; f.ExcludeFile != "" {
		t.Errorf("a non-git checkout has an exclude file recorded: %+v", f)
	}
	if !strings.Contains(res.stdout, "no exclude line added") {
		t.Errorf("stdout does not say no exclude line was added:\n%s", res.stdout)
	}
	// A directory inside a checkout is not its root: the anchored line would not
	// match there, so none is added either.
	repo := b.gitCheckout(t, "repo")
	sub := filepath.Join(repo, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	excl := readExcludeForTest(t, repo)
	res = mustLine(t, b, b.ladd("demo", "review", "claude", sub)...)
	if got := readExcludeForTest(t, repo); got != excl {
		t.Errorf("the exclude file of the enclosing repository changed: %q", got)
	}
	if !strings.Contains(res.stdout, "is not its root") {
		t.Errorf("stdout does not say why:\n%s", res.stdout)
	}
}

// codexConfigWant is the config a Codex session's checkout gets.
const codexConfigWant = "sandbox_mode = \"workspace-write\"\napproval_policy = \"on-request\"\n\n[sandbox_workspace_write]\nnetwork_access = true\n"

// TestLineAddCodexSession is the acceptance run for a Codex session on the fake
// codex: the config, the reply directory and marker, the thread started the way a
// table seat's is, the record and what is printed.
func TestLineAddCodexSession(t *testing.T) {
	b := newLineBench(t)
	lineUp(t, b, "demo")
	co := b.gitCheckout(t, "co-build")
	res := mustLine(t, b, b.ladd("demo", "build", "codex", co, "--model", "gpt-5")...)
	t.Logf("stdout:\n%s", res.stdout)
	t.Logf("stderr:\n%s", res.stderr)
	stream := b.stream("demo")

	cfgPath := filepath.Join(co, ".codex", "config.toml")
	if got := string(mustRead(t, cfgPath)); got != codexConfigWant {
		t.Errorf(".codex/config.toml\n%s\nwant\n%s", got, codexConfigWant)
	}
	if fi, err := os.Stat(filepath.Join(co, ".codex")); err != nil || !fi.IsDir() {
		t.Errorf(".codex: %v, %v", fi, err)
	}
	if _, err := os.Stat(filepath.Join(co, ".mcp.json")); err == nil {
		t.Error("a Codex session got a .mcp.json")
	}
	if got := readExcludeForTest(t, co); !strings.HasSuffix(got, "/.codex/config.toml\n") || strings.Contains(got, ".mcp.json") {
		t.Errorf("info/exclude = %q, want /.codex/config.toml appended", got)
	}
	if got := gitIn(t, co, "status", "--porcelain"); got != "" {
		t.Errorf("the checkout is not clean: %s", got)
	}

	rec := b.record(t, "demo")
	s := sessionByNick(t, rec, "build")
	if s.Harness != "codex" || s.Model != "gpt-5" || s.Checkout != co || s.Thread != fakeThread(1) {
		t.Errorf("session %+v", s)
	}
	// The reply directory: 0700 under TMPDIR, named for the line, with the marker
	// naming the line and the nick.
	fi, err := os.Stat(s.ReplyDir)
	if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("reply directory %s: %v, %v; want a directory at 0700", s.ReplyDir, fi, err)
	}
	if filepath.Dir(s.ReplyDir) != b.tmp || !strings.HasPrefix(filepath.Base(s.ReplyDir), "parley-line-demo-") {
		t.Errorf("reply directory %s, want parley-line-demo-* under %s", s.ReplyDir, b.tmp)
	}
	if got := string(mustRead(t, filepath.Join(s.ReplyDir, ".parley-line"))); got != "demo\nbuild\n" {
		t.Errorf("marker = %q, want the line id and the nick", got)
	}
	f := s.Files[0]
	if f.Path != cfgPath || f.SHA256 != fileDigestOf(t, cfgPath) || f.ExcludeFile != filepath.Join(co, ".git", "info", "exclude") || !f.MadeDir {
		t.Errorf("file entry %+v", f)
	}
	if want := tableDigest([]byte(codexConfigWant)); f.SHA256 != want {
		t.Errorf("digest %s, want the config's %s", f.SHA256, want)
	}

	// The thread record is where --thread auto looks.
	tr, err := state.OpenThreadRecord(b.base, "build", stream)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := tr.Read(); err != nil || got != fakeThread(1) {
		t.Errorf("thread record = %q, %v; want %s", got, err, fakeThread(1))
	}
	if fi, err := os.Stat(tr.Path()); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("thread record mode %v (%v), want 0600", fi.Mode(), err)
	}

	// codex was asked once, to exec, in the checkout, with network off for the turn.
	if n := b.callCount(); n != 1 {
		t.Fatalf("codex was called %d times, want 1 (the one exec)", n)
	}
	argv := b.call(t, 1)
	if len(argv) != 11 || argv[0] != "exec" {
		t.Fatalf("codex argv %q", argv)
	}
	if got, want := strings.Join(argv[1:10], " "), "--json -C "+co+" -s workspace-write -c sandbox_workspace_write.network_access=false -m gpt-5"; got != want {
		t.Errorf("codex exec flags %q, want %q", got, want)
	}
	prompt := argv[10]
	for _, want := range []string{"colin", stream, filepath.Join(s.ReplyDir, "build.reply"), "$parley-line", binPath, b.base, "--nick build", "more than 8 posts within 10m"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the primer does not hold %q:\n%s", want, prompt)
		}
	}

	pump := fmt.Sprintf("'%s' codex --stream '%s' --nick build --state-dir '%s' --thread auto --skill line --chair colin --cap 8 --window 10m", binPath, stream, b.base)
	resume := fmt.Sprintf("codex resume '%s' -C '%s' -a on-request", fakeThread(1), co)
	wantInOrder(t, res.stdout, []string{
		"Network: network access is ON for Codex in this checkout: the config.toml this line wrote sets network_access = true under workspace-write, so that parley send can reach the loopback IRC server from inside the sandbox. It applies to every Codex session in the checkout, not only this one.",
		pump,
		resume,
		"Check that /status shows workspace-write and network on, and its model (gpt-5); the model check is unverified on a resumed session, so confirm it here.",
		fmt.Sprintf("Session build's answer to its primer (codex exec, thread %s):", fakeThread(1)),
		"ready, waiting for the TABLE line",
	})
	if !strings.Contains(res.stdout, "$parley-line") {
		t.Errorf("stdout lacks $parley-line:\n%s", res.stdout)
	}
}

// TestLineAddCodexBinCarriesToTheLinesItPrints: a line made with --codex-bin runs
// that codex, and says so in the pump and resume lines.
func TestLineAddCodexBinCarriesToTheLinesItPrints(t *testing.T) {
	b := newLineBench(t)
	lineUp(t, b, "demo", "--codex-bin", b.codex)
	co := b.gitCheckout(t, "co")
	res := mustLine(t, b, b.ladd("demo", "build", "codex", co)...)
	lineWith(t, res.stdout, " codex --stream ", "--codex-bin '"+b.codex+"'")
	lineWith(t, res.stdout, "'"+b.codex+"' resume ")
}

// TestLineAddCodexPumpFindsTheRecordedThread: parley codex --thread auto --skill
// line, as printed, finds the thread the add recorded and delivers the chair's
// post into it.
func TestLineAddCodexPumpFindsTheRecordedThread(t *testing.T) {
	addr := startErgo(t)
	b := newLineBench(t)
	b.db.server = addr
	lineUp(t, b, "demo")
	co := b.gitCheckout(t, "co")
	res := mustLine(t, b, b.ladd("demo", "build", "codex", co)...)

	words := shellWords(t, lineWith(t, res.stdout, " codex --stream "))
	if words[0] != binPath {
		t.Fatalf("the pump line starts with %q, want this binary", words[0])
	}
	pump := exec.Command(words[0], append(words[1:], "--debounce", "100ms")...)
	pump.Env = append(os.Environ(), b.env()...)
	stderr := &syncBuffer{}
	pump.Stderr = stderr
	if err := pump.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = pump.Process.Signal(syscall.SIGTERM)
		select {
		case <-joinCmd(pump):
		case <-time.After(childDeadline):
			_ = pump.Process.Kill()
		}
	})
	waitForStderr(t, stderr, "into Codex thread "+fakeThread(1))

	mustRun(t, "", "send", "--stream", b.stream("demo"), "--nick", "colin", "--state-dir", t.TempDir(), "--", "welcome to the line")
	argv := b.waitCall(t, 2, stderr)
	if len(argv) != 5 || argv[0] != "queue" || argv[2] != fakeThread(1) || !strings.Contains(argv[4], "welcome to the line") {
		t.Fatalf("codex was run with %q, want queue --thread %s --message <text>", argv, fakeThread(1))
	}
}

// TestLineAddCodexSessionsShareTheConfig: a second Codex session in the checkout
// shares the config, a Claude one in the same checkout does not share it, and
// each keeps its own reply directory and thread.
func TestLineAddCodexSessionsShareTheConfig(t *testing.T) {
	b := newLineBench(t)
	lineUp(t, b, "demo")
	co := b.gitCheckout(t, "co")
	mustLine(t, b, b.ladd("demo", "build", "codex", co)...)
	cfg := filepath.Join(co, ".codex", "config.toml")
	st, err := os.Stat(cfg)
	if err != nil {
		t.Fatal(err)
	}
	res := mustLine(t, b, b.ladd("demo", "review", "codex", co)...)
	if st2, err := os.Stat(cfg); err != nil || !os.SameFile(st, st2) {
		t.Errorf("the config was replaced: %v", err)
	}
	if got := strings.Count(readExcludeForTest(t, co), "/.codex/config.toml\n"); got != 1 {
		t.Errorf("info/exclude holds the line %d times", got)
	}
	if !strings.Contains(res.stdout, "Shares "+cfg) || !strings.Contains(res.stdout, "network access is ON") {
		t.Errorf("stdout:\n%s", res.stdout)
	}
	rec := b.record(t, "demo")
	a, c := sessionByNick(t, rec, "build"), sessionByNick(t, rec, "review")
	if fmt.Sprint(a.Files) != fmt.Sprint(c.Files) || !c.Files[0].MadeDir {
		t.Errorf("file entries %+v and %+v differ", a.Files, c.Files)
	}
	if a.Thread == c.Thread || a.ReplyDir == c.ReplyDir {
		t.Errorf("the sessions share a thread or a reply directory: %+v %+v", a, c)
	}
	// A Claude session in the same checkout has its own file.
	mustLine(t, b, b.ladd("demo", "plan", "claude", co)...)
	if _, err := os.Stat(filepath.Join(co, ".mcp.json")); err != nil {
		t.Errorf("no .mcp.json for the Claude session: %v", err)
	}
	if got := readExcludeForTest(t, co); !strings.Contains(got, "/.mcp.json\n") || !strings.Contains(got, "/.codex/config.toml\n") {
		t.Errorf("info/exclude = %q", got)
	}
	p := sessionByNick(t, b.record(t, "demo"), "plan")
	if p.Files[0].Path != filepath.Join(co, ".mcp.json") || p.Files[0].MadeDir {
		t.Errorf("file entry %+v", p.Files[0])
	}
}

// TestLineAddKeepsAnExcludeLineThatWasAlreadyThere: a line already in the exclude
// file is not added again, and the record says the line did not add it.
func TestLineAddKeepsAnExcludeLineThatWasAlreadyThere(t *testing.T) {
	b := newLineBench(t)
	lineUp(t, b, "demo")
	co := b.gitCheckout(t, "co")
	exclude := filepath.Join(co, ".git", "info", "exclude")
	pre := "# mine\n/.mcp.json\n*.swp"
	if err := os.WriteFile(exclude, []byte(pre), 0o644); err != nil {
		t.Fatal(err)
	}
	mustLine(t, b, b.ladd("demo", "plan", "claude", co)...)
	if got := readExcludeForTest(t, co); got != pre {
		t.Errorf("info/exclude = %q, want it untouched: %q", got, pre)
	}
	if f := b.record(t, "demo").Sessions[0].Files[0]; f.ExcludeFile != "" {
		t.Errorf("the record says the line added the exclude line: %+v", f)
	}
}

// TestLineAddEndsAnUnterminatedExcludeFileBeforeAppending.
func TestLineAddEndsAnUnterminatedExcludeFileBeforeAppending(t *testing.T) {
	b := newLineBench(t)
	lineUp(t, b, "demo")
	co := b.gitCheckout(t, "co")
	exclude := filepath.Join(co, ".git", "info", "exclude")
	if err := os.WriteFile(exclude, []byte("a\nb"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustLine(t, b, b.ladd("demo", "plan", "claude", co)...)
	if got := readExcludeForTest(t, co); got != "a\nb\n/.mcp.json\n" {
		t.Errorf("info/exclude = %q", got)
	}
}

// TestLineAddNeverWritesThroughALinkedExcludeFile: the exclude file is the
// user's, and a link in its place is not followed; the add goes on without it.
func TestLineAddNeverWritesThroughALinkedExcludeFile(t *testing.T) {
	b := newLineBench(t)
	lineUp(t, b, "demo")
	co := b.gitCheckout(t, "co")
	target := filepath.Join(b.root, "shared-excludes")
	if err := os.WriteFile(target, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	exclude := filepath.Join(co, ".git", "info", "exclude")
	if err := os.Remove(exclude); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, exclude); err != nil {
		t.Fatal(err)
	}
	res := mustLine(t, b, b.ladd("demo", "plan", "claude", co)...)
	if got := string(mustRead(t, target)); got != "x\n" {
		t.Errorf("the link's target was written through: %q", got)
	}
	if !strings.Contains(res.stdout, "no exclude line added") || b.record(t, "demo").Sessions[0].Files[0].ExcludeFile != "" {
		t.Errorf("stdout:\n%s", res.stdout)
	}
}
