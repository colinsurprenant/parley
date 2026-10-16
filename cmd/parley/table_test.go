package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/colinsurprenant/parley/internal/doctor"
	"github.com/colinsurprenant/parley/internal/state"
	"github.com/colinsurprenant/parley/internal/stream/irc"
)

// fakeThread is the thread id the fake codex gives its nth call.
func fakeThread(n int) string { return fmt.Sprintf("0199c0de-0000-7000-8000-%012d", n) }

// tableBench is a machine for `parley table new` to set a table up on: a real git
// repo to be the target, a fake codex that answers like 0.161.0 and records every
// call, a fake claude, a home with the skill links and a sheet, a state base that
// does not exist yet, and a TMPDIR of its own, so what a run leaves is visible.
type tableBench struct {
	db    doctorBench
	root  string
	repo  string
	base  string
	tmp   string
	fake  string
	path  string
	codex string
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// initRepo makes dir a git repo with one commit holding files (path to content).
func initRepo(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	gitIn(t, dir, "init", "-q")
	if len(files) == 0 {
		files = map[string]string{"README.md": "target\n"}
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "-c", "commit.gpgsign=false", "commit", "-q", "-m", "init")
}

// fakeCodexScript is a codex that answers --version and every --help like the
// 0.161.0 it was copied from, and records the argv of every other call in
// $d/call-<n>. A record is written under a temporary name and renamed into place
// before the count is published, so a reader that sees the count at n finds call-n
// whole. exec prints the JSONL of a turn whose thread id is fakeThread(n) unless
// $d/mode-<n> says otherwise; delete and queue succeed.
const fakeCodexScript = `#!/bin/sh
d=%[1]q
case "$1" in
--version) echo "codex-cli 0.161.0"; exit 0 ;;
esac
case "$2" in
--help) cat %[2]q/codex-0.161.0-"$1".help; exit 0 ;;
esac
n=$(cat "$d/count" 2>/dev/null || echo 0)
n=$((n+1))
{ for a in "$@"; do printf '%%s\0' "$a"; done; } > "$d/.call-$n.tmp"
mv "$d/.call-$n.tmp" "$d/call-$n"
echo "$n" > "$d/.count.tmp"
mv "$d/.count.tmp" "$d/count"
mode=ok
[ -f "$d/mode-$n" ] && mode=$(cat "$d/mode-$n")
id=$(printf '0199c0de-0000-7000-8000-%%012d' "$n")
case "$1" in
exec)
	cat > /dev/null
	case "$mode" in
	fail) echo "boom: not logged in" >&2; exit 3 ;;
	nothread)
		echo '{"type":"turn.started"}'
		echo '{"type":"turn.completed"}'
		exit 0 ;;
	failafter)
		printf '{"type":"thread.started","thread_id":"%%s"}\n' "$id"
		echo '{"type":"error","message":"the model went away"}'
		echo "bad turn" >&2
		exit 1 ;;
	failzero)
		printf '{"type":"thread.started","thread_id":"%%s"}\n' "$id"
		echo '{"type":"turn.started"}'
		echo '{"type":"turn.failed","error":{"message":"quota exceeded"}}'
		exit 0 ;;
	errorzero)
		printf '{"type":"thread.started","thread_id":"%%s"}\n' "$id"
		echo '{"type":"error","message":"stream broke"}'
		echo '{"type":"turn.completed"}'
		exit 0 ;;
	ctrlfail)
		printf '{"type":"thread.started","thread_id":"%%s"}\n' "$id"
		printf '%%s\n' '{"type":"turn.failed","error":{"message":"quota\u001b[31m exceeded\u0007"}}'
		printf 'bad\033[31m stderr\n' >&2
		exit 0 ;;
	nocomplete)
		printf '{"type":"thread.started","thread_id":"%%s"}\n' "$id"
		echo '{"type":"turn.started"}'
		echo '{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"ready, waiting for the TABLE line"}}'
		exit 0 ;;
	overflow)
		printf '{"type":"thread.started","thread_id":"%%s"}\n' "$id"
		printf '{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"'
		head -c 1200000 /dev/zero | tr '\0' x
		printf '"}}\n'
		echo '{"type":"turn.completed"}'
		exit 0 ;;
	overflowafter)
		printf '{"type":"thread.started","thread_id":"%%s"}\n' "$id"
		echo '{"type":"turn.completed"}'
		printf '{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"'
		head -c 1200000 /dev/zero | tr '\0' x
		printf '"}}\n'
		exit 0 ;;
	ctrlstderr) printf 'boom\033[31m: not logged in\007\n' >&2; exit 3 ;;
	ctrlevents)
		printf '%%s\n' '{"type":"odd\u001b[2Jevent"}'
		printf '%%s\n' '{"type":"turn.completed"}'
		exit 0 ;;
	badid)
		echo '{"type":"thread.started","thread_id":"not-a-uuid"}'
		echo '{"type":"turn.completed"}'
		exit 0 ;;
	esac
	printf '{"type":"thread.started","thread_id":"%%s"}\n' "$id"
	echo '{"type":"turn.started"}'
	if [ -f "$d/items" ]; then cat "$d/items"; else echo '{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"ready, waiting for the TABLE line"}}'; fi
	echo '{"type":"turn.completed","usage":{"input_tokens":1}}'
	exit 0 ;;
delete)
	if [ "$mode" = faildelete ]; then echo "cannot delete" >&2; exit 5; fi
	exit 0 ;;
esac
exit 0
`

func newTableBench(t *testing.T) *tableBench {
	t.Helper()
	root := shortRoot(t)
	b := &tableBench{
		db:   bench(t, "0.161.0", nil, doctor.ClaudeVerified),
		root: root,
		repo: filepath.Join(root, "t"),
		base: filepath.Join(root, "st"),
		tmp:  filepath.Join(root, "m"),
		fake: filepath.Join(root, "f"),
	}
	for _, d := range []string{b.tmp, b.fake} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	initRepo(t, b.repo, nil)

	testdata, err := filepath.Abs(filepath.Join("..", "..", "internal", "doctor", "testdata"))
	if err != nil {
		t.Fatal(err)
	}
	b.codex = b.db.codex
	if err := os.WriteFile(b.codex, []byte(fmt.Sprintf(fakeCodexScript, b.fake, testdata)), 0o755); err != nil {
		t.Fatal(err)
	}
	b.path = filepath.Dir(b.db.claude)

	// The sheet template, where the skill links lead.
	link := filepath.Join(b.db.home, ".claude", "skills", "parley-table")
	skill, err := os.Readlink(link)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skill, "sheet.md"), []byte("SHEET TEMPLATE\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return b
}

func (b *tableBench) env() []string {
	return []string{
		"HOME=" + b.db.home,
		// An empty $CODEX_HOME is unset, so the user's own never leaks into a run.
		"CODEX_HOME=",
		"TMPDIR=" + b.tmp,
		"XDG_STATE_HOME=" + filepath.Join(b.root, "xdg"),
		"PATH=" + b.path + string(os.PathListSeparator) + os.Getenv("PATH"),
	}
}

func (b *tableBench) run(t *testing.T, args ...string) result {
	t.Helper()
	return runParleyEnvIn(t, "", b.env(), args...)
}

// newArgs is a valid `table new` for id, with the seats named (a codex seat
// codex-a and a claude seat claude-a by default) and extra flags after, which
// override the defaults of the single-valued ones.
func (b *tableBench) newArgs(id string, seats []string, extra ...string) []string {
	if seats == nil {
		seats = []string{"codex-a=codex", "claude-a=claude"}
	}
	args := []string{"table", "new", id, "--target", b.repo, "--chair", "colin", "--rounds", "3",
		"--state-dir", b.base, "--server", b.db.server}
	for _, s := range seats {
		args = append(args, "--seat", s)
	}
	return append(args, extra...)
}

func (b *tableBench) endArgs(id string, extra ...string) []string {
	return append([]string{"table", "end", id, "--state-dir", b.base}, extra...)
}

func (b *tableBench) head(t *testing.T) string { return gitIn(t, b.repo, "rev-parse", "HEAD") }

// calls reads the argv of the fake codex's call n.
func (b *tableBench) call(t *testing.T, n int) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(b.fake, fmt.Sprintf("call-%d", n)))
	if err != nil {
		t.Fatalf("the fake codex was not called %d times: %v", n, err)
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
}

func (b *tableBench) callCount() int {
	raw, err := os.ReadFile(filepath.Join(b.fake, "count"))
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	return n
}

// waitCall waits for the fake codex's call n and returns its argv. The fake
// renames a record into place before it publishes the count, so a count of n is
// a call-n that is whole.
func (b *tableBench) waitCall(t *testing.T, n int, stderr *syncBuffer) []string {
	t.Helper()
	deadline := time.Now().Add(childDeadline)
	for b.callCount() < n {
		if time.Now().After(deadline) {
			t.Fatalf("the fake codex was not called %d times in time (%d calls); stderr:\n%s", n, b.callCount(), stderr.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	return b.call(t, n)
}

// pathWith is a PATH of a directory of its own that holds only the named
// executables: the fakes in the bench's directory first, then whatever the
// machine has by that name. A test that needs a machine without a binary leaves
// it out, rather than putting /usr/bin or /bin on the PATH and hoping.
func (b *tableBench) pathWith(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range names {
		src := filepath.Join(b.path, name)
		if _, err := os.Stat(src); err != nil {
			real, err := exec.LookPath(name)
			if err != nil {
				t.Fatalf("no %s on this machine to link into the isolated PATH: %v", name, err)
			}
			src = real
		}
		if err := os.Symlink(src, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// envWithPath is the bench's environment with PATH replaced.
func (b *tableBench) envWithPath(path string) []string {
	return append(b.env(), "PATH="+path)
}

func (b *tableBench) setMode(t *testing.T, n int, mode string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(b.fake, fmt.Sprintf("mode-%d", n)), []byte(mode), 0o644); err != nil {
		t.Fatal(err)
	}
}

// snapshot is everything a table new could leave behind: the target's worktrees
// and status, the state base tree with modes (less the nick lock files, see
// baseListing), the parley-table directories in TMPDIR, and how many times codex
// was called (not counting --help and --version).
func (b *tableBench) snapshot(t *testing.T) string {
	t.Helper()
	var s strings.Builder
	fmt.Fprintf(&s, "worktrees:\n%s\nstatus:\n%s\n", gitIn(t, b.repo, "worktree", "list", "--porcelain"), gitIn(t, b.repo, "status", "--porcelain"))
	if _, err := os.Stat(filepath.Join(b.repo, ".git", "worktrees")); err == nil {
		ents, _ := os.ReadDir(filepath.Join(b.repo, ".git", "worktrees"))
		for _, e := range ents {
			fmt.Fprintf(&s, "admin: %s\n", e.Name())
		}
	}
	s.WriteString("base:\n")
	s.WriteString(baseListing(b.base))
	s.WriteString("tmp:\n")
	ents, _ := os.ReadDir(b.tmp)
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	s.WriteString(strings.Join(names, "\n") + "\n")
	fmt.Fprintf(&s, "codex calls: %d\n", b.callCount())
	return s.String()
}

// baseListing lists the state base tree with modes, as the snapshot's view of
// it. No parley code unlinks a lock file, so a table new that fails or is
// refused leaves <base>/<nick>/lock behind, as a pump does, and the listing
// leaves out exactly that and nothing else:
//
//   - a file named lock, directly in a directory that is directly in the base,
//     when it is a regular file and empty, which is what taking a lock makes;
//   - that directory, when the lock file is all it holds;
//   - the base itself, when that leaves it empty and a lock file was left out,
//     so that a base only lock files hold lists as a base that is not there.
//
// A lock file with content, a link or a directory called lock, a lock file
// deeper or higher in the tree, anything else in a nick's directory, an empty
// directory of any name and the tables directory are all listed, so that none
// of them can hide a leftover. TestBaseListingHidesOnlyLockFiles holds this.
//
// The id locks are left out the same way: an empty regular <id>.lock directly
// in <base>/.ids, which every table and build command leaves behind, and .ids
// itself when only such files are in it.
func baseListing(base string) string {
	if _, err := os.Lstat(base); err != nil {
		return "(absent)\n"
	}
	hidden := map[string]bool{}
	top, _ := os.ReadDir(base)
	for _, e := range top {
		if !e.IsDir() || e.Name() == tablesDirName {
			continue
		}
		ents, err := os.ReadDir(filepath.Join(base, e.Name()))
		if err != nil {
			continue
		}
		for _, c := range ents {
			if c.Name() != "lock" {
				continue
			}
			if fi, err := c.Info(); err == nil && fi.Mode().IsRegular() && fi.Size() == 0 {
				hidden[e.Name()+string(filepath.Separator)+"lock"] = true
				if len(ents) == 1 {
					hidden[e.Name()] = true
				}
			}
		}
	}
	// The id locks: <id>.lock files, empty and regular, directly in <base>/.ids,
	// which is hidden too when they are all it holds.
	idsDir := filepath.Join(base, ".ids")
	if ents, err := os.ReadDir(idsDir); err == nil && len(ents) > 0 {
		allLocks := true
		for _, c := range ents {
			if fi, err := c.Info(); err == nil && strings.HasSuffix(c.Name(), ".lock") && fi.Mode().IsRegular() && fi.Size() == 0 {
				hidden[filepath.Join(".ids", c.Name())] = true
			} else {
				allLocks = false
			}
		}
		if allLocks {
			hidden[".ids"] = true
		}
	}
	var out strings.Builder
	listed := 0
	_ = filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(base, p)
		if hidden[rel] {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		info, _ := d.Info()
		fmt.Fprintf(&out, "%s %v\n", rel, info.Mode())
		if rel != "." {
			listed++
		}
		return nil
	})
	if listed == 0 && len(hidden) > 0 {
		return "(absent)\n"
	}
	return out.String()
}

// wantUnchanged fails the test if the machine is not as it was before.
func (b *tableBench) wantUnchanged(t *testing.T, before string) {
	t.Helper()
	if after := b.snapshot(t); after != before {
		t.Fatalf("the run left a trace.\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// shellWords splits a line the way a POSIX shell would for the quoting
// primer.ShellQuote produces: single-quoted spans, backslash escapes outside
// them, and blanks between words.
func shellWords(t *testing.T, line string) []string {
	t.Helper()
	var words []string
	var cur strings.Builder
	inWord, inQuote := false, false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case inQuote:
			if c == '\'' {
				inQuote = false
			} else {
				cur.WriteByte(c)
			}
		case c == '\'':
			inQuote, inWord = true, true
		case c == '\\' && i+1 < len(line):
			i++
			cur.WriteByte(line[i])
			inWord = true
		case c == ' ' || c == '\t':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	if inQuote {
		t.Fatalf("unterminated quote in %q", line)
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words
}

// lineWith returns the one stdout line that holds all of the substrings.
func lineWith(t *testing.T, out string, subs ...string) string {
	t.Helper()
	var found []string
lines:
	for _, l := range strings.Split(out, "\n") {
		for _, s := range subs {
			if !strings.Contains(l, s) {
				continue lines
			}
		}
		found = append(found, strings.TrimSpace(l))
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one line with %q, found %d:\n%s", subs, len(found), out)
	}
	return found[0]
}

// stepRE is the number that begins a step's first line in the runbook.
var stepRE = regexp.MustCompile(`^\d+\. `)

// wantInOrder fails unless each line is in out as a whole trimmed line, in order.
// A step's first line is also taken without its number.
func wantInOrder(t *testing.T, out string, lines []string) {
	t.Helper()
	have := strings.Split(out, "\n")
	at := 0
	for _, want := range lines {
		found := false
		for ; at < len(have); at++ {
			if line := strings.TrimSpace(have[at]); line == want || stepRE.ReplaceAllString(line, "") == want {
				found = true
				at++
				break
			}
		}
		if !found {
			t.Fatalf("the output has no line %q after the previous one, in order:\n%s", want, out)
		}
	}
}

func mustTable(t *testing.T, b *tableBench, args ...string) result {
	t.Helper()
	res := b.run(t, args...)
	if res.code != 0 {
		t.Fatalf("parley %v exited %d\nstdout: %s\nstderr: %s", args, res.code, res.stdout, res.stderr)
	}
	return res
}

// TestTableNewSetsTheTableUp is the acceptance run: one claude seat and one
// fake-codex seat on a temp repo. It checks what is on disk, what codex was
// asked, and what is printed.
func TestTableNewSetsTheTableUp(t *testing.T) {
	b := newTableBench(t)
	res := mustTable(t, b, b.newArgs("t1", nil)...)
	t.Logf("stdout:\n%s", res.stdout)
	t.Logf("stderr:\n%s", res.stderr)

	tableDir := filepath.Join(b.base, "tables", "t1")
	wt := filepath.Join(tableDir, "worktree")
	stream := "irc://" + b.db.server + "/#t1"

	// The worktree is detached at the target's HEAD.
	if got := gitIn(t, wt, "rev-parse", "HEAD"); got != b.head(t) {
		t.Errorf("worktree HEAD %s, want the target's %s", got, b.head(t))
	}
	if out, err := exec.Command("git", "-C", wt, "symbolic-ref", "-q", "HEAD").Output(); err == nil {
		t.Errorf("the worktree is on a branch (%s), want it detached", out)
	}
	if got := gitIn(t, b.repo, "status", "--porcelain"); got != "" {
		t.Errorf("the target checkout is not clean: %s", got)
	}

	// The .mcp.json: its args exactly.
	raw, err := os.ReadFile(filepath.Join(wt, ".mcp.json"))
	if err != nil {
		t.Fatalf("no .mcp.json: %v", err)
	}
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
	wantArgs := []string{"channel", "--stream", stream, "--nick", "${PARLEY_NICK}", "--state-dir", b.base,
		"--skill", "table", "--chair", "colin", "--cap", "6", "--target", b.repo, "--worktree", wt}
	if strings.Join(srv.Args, "\x00") != strings.Join(wantArgs, "\x00") {
		t.Errorf(".mcp.json args\n%q\nwant\n%q", srv.Args, wantArgs)
	}
	// And they are the launch flags of the channel: --check on them passes.
	check := append([]string(nil), srv.Args...)
	for i, a := range check {
		if a == "${PARLEY_NICK}" {
			check[i] = "claude-a"
		}
	}
	if c := runCheck(t, b.tmp, check...); c.code != 0 {
		t.Errorf("channel --check on the .mcp.json args exited %d\n%s%s", c.code, c.stdout, c.stderr)
	}

	// The Codex config.
	cfg, err := os.ReadFile(filepath.Join(wt, ".codex", "config.toml"))
	if err != nil {
		t.Fatalf("no .codex/config.toml: %v", err)
	}
	wantCfg := "sandbox_mode = \"workspace-write\"\napproval_policy = \"on-request\"\n\n[sandbox_workspace_write]\nnetwork_access = true\n"
	if string(cfg) != wantCfg {
		t.Errorf(".codex/config.toml\n%s\nwant\n%s", cfg, wantCfg)
	}

	// The reply directory: 0700 under TMPDIR, named for the table.
	var rec tableRecord
	recRaw, err := os.ReadFile(filepath.Join(tableDir, "table.json"))
	if err != nil {
		t.Fatalf("no table record: %v", err)
	}
	if err := json.Unmarshal(recRaw, &rec); err != nil {
		t.Fatalf("table.json: %v", err)
	}
	fi, err := os.Stat(rec.ReplyDir)
	if err != nil {
		t.Fatalf("reply directory: %v", err)
	}
	if fi.Mode().Perm() != 0o700 || !fi.IsDir() {
		t.Errorf("reply directory mode %v, want a directory at 0700", fi.Mode())
	}
	if filepath.Dir(rec.ReplyDir) != b.tmp || !strings.HasPrefix(filepath.Base(rec.ReplyDir), "parley-table-t1-") {
		t.Errorf("reply directory %s, want parley-table-t1-* under %s", rec.ReplyDir, b.tmp)
	}
	// And holds the marker table end checks before it removes it: the table id,
	// then the seats' nicks sorted, then the digest of the record's bytes.
	wantMarker := "t1\nclaude-a codex-a\n" + digestOf(recRaw) + "\n"
	if marker, err := os.ReadFile(filepath.Join(rec.ReplyDir, ".parley-table")); err != nil || string(marker) != wantMarker {
		t.Errorf("the reply directory's marker = %q, %v; want %q: the table id, the seats and the record's digest", marker, err, wantMarker)
	}

	// The record.
	if rec.ID != "t1" || rec.Stream != stream || rec.Target != b.repo || rec.Worktree != wt || rec.Chair != "colin" || rec.Rounds != 3 || rec.Cap != 6 {
		t.Errorf("record %+v", rec)
	}
	if _, err := time.Parse(time.RFC3339, rec.Created); err != nil {
		t.Errorf("created %q: %v", rec.Created, err)
	}
	wantSeats := []seatRecord{
		{Nick: "codex-a", Harness: "codex", Thread: fakeThread(1), ReplyFile: filepath.Join(rec.ReplyDir, "codex-a.reply")},
		{Nick: "claude-a", Harness: "claude"},
	}
	if fmt.Sprint(rec.Seats) != fmt.Sprint(wantSeats) {
		t.Errorf("record seats %+v, want %+v", rec.Seats, wantSeats)
	}
	if st, err := os.Stat(filepath.Join(tableDir, "table.json")); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("table.json mode %v (%v), want 0600", st.Mode(), err)
	}
	var generic map[string]any
	if err := json.Unmarshal(recRaw, &generic); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range generic {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if got, want := strings.Join(keys, ","), "cap,chair,created,id,reply_dir,rounds,seats,stream,target,worktree"; got != want {
		t.Errorf("table.json fields %s, want %s: the record names things and holds no stream content", got, want)
	}

	// The thread record is where --thread auto looks.
	tr, err := state.OpenThreadRecord(b.base, "codex-a", stream)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := tr.Read(); err != nil || got != fakeThread(1) {
		t.Errorf("thread record = %q, %v; want %s", got, err, fakeThread(1))
	}
	if st, err := os.Stat(tr.Path()); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("thread record mode %v (%v), want 0600", st.Mode(), err)
	}

	// The sheet was copied from the skill install.
	if got, err := os.ReadFile(filepath.Join(tableDir, "sheet.md")); err != nil || string(got) != "SHEET TEMPLATE\n" {
		t.Errorf("sheet.md = %q, %v", got, err)
	}

	// codex was asked once, to exec, with network off for the turn and no
	// network-on override.
	if n := b.callCount(); n != 1 {
		t.Fatalf("codex was called %d times, want 1 (the one exec)", n)
	}
	argv := b.call(t, 1)
	if len(argv) != 9 || argv[0] != "exec" {
		t.Fatalf("codex argv %q", argv)
	}
	if strings.Join(argv[1:8], " ") != "--json -C "+wt+" -s workspace-write -c sandbox_workspace_write.network_access=false" {
		t.Errorf("codex exec flags %q", argv[1:8])
	}
	for _, a := range argv[:8] {
		if strings.Contains(a, "network_access=true") {
			t.Errorf("codex was given a network-on override: %q", a)
		}
	}
	prompt := argv[8]
	for _, want := range []string{"colin", b.repo, wt, stream, filepath.Join(rec.ReplyDir, "codex-a.reply"), "$parley-table", binPath, b.base, "--nick codex-a"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the primer does not hold %q:\n%s", want, prompt)
		}
	}

	// The stdout, in order, every command quoted.
	pump := fmt.Sprintf("'%s' codex --stream '%s' --nick codex-a --state-dir '%s' --thread auto --skill table --chair colin --cap 6", binPath, stream, b.base)
	resume := fmt.Sprintf("codex resume '%s' -C '%s' -a on-request", fakeThread(1), wt)
	claude := fmt.Sprintf("cd '%s' && PARLEY_NICK=claude-a claude --dangerously-load-development-channels server:parley", wt)
	wantInOrder(t, res.stdout, []string{
		fmt.Sprintf("'%s' server --listen '%s'", binPath, b.db.server),
		"/join #t1",
		"/msg NickServ INFO colin",
		"/msg NickServ REGISTER <password>",
		pump,
		resume,
		fmt.Sprintf("Seat codex-a's answer to its primer (codex exec, thread %s):", fakeThread(1)),
		"ready, waiting for the TABLE line",
		claude,
		"The gate. Do not post TABLE until every item holds:",
		"- every Claude seat's /mcp shows parley connected, and the notice below its startup banner reads",
		"\"Channels (experimental) messages from server:parley inject directly in this session\"; /mcp alone also passes on a launch",
		"without --dangerously-load-development-channels, which never hears the channel;",
		"- every Codex pump is running;",
		"- each Codex seat's /status shows workspace-write and network on.",
		"TABLE t1 " + filepath.Join(tableDir, "sheet.md"),
	})
	for _, want := range []string{"$parley-table", "/parley-table", "/mcp", "workspace-write", "network on"} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("stdout lacks %q", want)
		}
	}
	// doctor's warn about the server that is not up went to stderr.
	if !strings.Contains(res.stderr, "doctor: warn") || !strings.Contains(res.stderr, "server") {
		t.Errorf("stderr does not carry doctor's server warning:\n%s", res.stderr)
	}
	if strings.Contains(res.stdout, "doctor:") {
		t.Errorf("doctor's lines are on stdout:\n%s", res.stdout)
	}
}

// TestTableNewPrintsTheModelsAndLeavesTheServerOutWhenItAnswers: a model on a
// seat is -m on its exec and --model on its launch line; a server that answers
// has no line of its own.
func TestTableNewPrintsTheModelsAndLeavesTheServerOutWhenItAnswers(t *testing.T) {
	b := newTableBench(t)
	startFakeIRC(t, b.db.server)
	res := mustTable(t, b, b.newArgs("t2", []string{"codex-a=codex:gpt-5.5", "claude-a=claude:opus"})...)
	if strings.Contains(res.stdout, "' server") {
		t.Errorf("the server answers, yet stdout has a server line:\n%s", res.stdout)
	}
	argv := b.call(t, 1)
	if len(argv) != 11 || argv[8] != "-m" || argv[9] != "gpt-5.5" {
		t.Errorf("codex argv %q, want -m gpt-5.5 before the primer", argv[:min(len(argv), 10)])
	}
	wt := filepath.Join(b.base, "tables", "t2", "worktree")
	wantInOrder(t, res.stdout, []string{
		fmt.Sprintf("cd '%s' && PARLEY_NICK=claude-a claude --dangerously-load-development-channels server:parley --model 'opus'", wt),
	})
	if !strings.Contains(res.stdout, "gpt-5.5") || !strings.Contains(res.stdout, "unverified") {
		t.Errorf("the gate does not carry the Codex model to check:\n%s", res.stdout)
	}
}

// startFakeIRC answers CAP LS on addr with every capability the adapter
// requires, which is all doctor's server check asks for.
func startFakeIRC(t *testing.T, addr string) {
	t.Helper()
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				r := bufio.NewReader(c)
				if line, err := r.ReadString('\n'); err == nil && strings.HasPrefix(line, "CAP LS") {
					fmt.Fprintf(c, ":srv CAP * LS :%s\r\n", strings.Join(irc.RequiredCaps(), " "))
				}
			}()
		}
	}()
}

// TestTableNewStartsCodexWithStdinClosed: a codex exec that is left a live stdin
// waits on it. The fake reads its stdin to the end; parley is run with a pipe it
// never closes.
func TestTableNewStartsCodexWithStdinClosed(t *testing.T) {
	b := newTableBench(t)
	cmd := exec.Command(binary(t), b.newArgs("t3", []string{"codex-a=codex"})...)
	cmd.Env = append(os.Environ(), b.env()...)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	var stderr syncBuffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-joinCmd(cmd):
		if err != nil {
			t.Fatalf("table new: %v\n%s", err, stderr.String())
		}
	case <-time.After(childDeadline):
		_ = cmd.Process.Kill()
		t.Fatalf("table new did not finish with a stdin that is never closed: codex was left holding it\n%s", stderr.String())
	}
}

// TestTableNewPrintsTheSeatsAnswerSafely: control characters in a seat's answer
// do not reach the terminal, every line is indented, and it is labelled.
func TestTableNewPrintsTheSeatsAnswerSafely(t *testing.T) {
	b := newTableBench(t)
	items := `{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"first\u001b[31m line\tend\r\nsecond line\u0007\n\nfourth"}}` + "\n"
	if err := os.WriteFile(filepath.Join(b.fake, "items"), []byte(items), 0o644); err != nil {
		t.Fatal(err)
	}
	res := mustTable(t, b, b.newArgs("t4", []string{"codex-a=codex"})...)
	if strings.ContainsAny(res.stdout, "\x1b\x07\r\t") {
		t.Errorf("a control character reached stdout: %q", res.stdout)
	}
	if !strings.Contains(res.stdout, "Seat codex-a's answer to its primer") {
		t.Errorf("the answer is not labelled as the seat's:\n%s", res.stdout)
	}
	for _, want := range []string{"        first[31m line end", "        second line", "        fourth"} {
		if !strings.Contains(res.stdout, want+"\n") {
			t.Errorf("stdout lacks the indented answer line %q:\n%s", want, res.stdout)
		}
	}
}

// TestTableNewCodexBinCarriesToThePrintedLines: a codex named with --codex-bin is
// the codex the pump and the resume line must use too.
func TestTableNewCodexBinCarriesToThePrintedLines(t *testing.T) {
	b := newTableBench(t)
	res := mustTable(t, b, b.newArgs("t5", []string{"codex-a=codex"}, "--codex-bin", b.codex)...)
	pump := lineWith(t, res.stdout, " codex --stream ")
	if !strings.HasSuffix(pump, " --codex-bin '"+b.codex+"'") {
		t.Errorf("the pump line does not carry --codex-bin: %s", pump)
	}
	lineWith(t, res.stdout, "'"+b.codex+"' resume '"+fakeThread(1)+"'")
}

// TestTableNewSecondRunWithTheSameIDExits2AndChangesNothing.
func TestTableNewSecondRunWithTheSameIDExits2AndChangesNothing(t *testing.T) {
	b := newTableBench(t)
	mustTable(t, b, b.newArgs("t1", nil)...)
	before := b.snapshot(t)
	res := b.run(t, b.newArgs("t1", nil)...)
	if res.code != 2 || !strings.Contains(res.stderr, "already exists") {
		t.Fatalf("second run exited %d, want 2 saying the table exists\n%s", res.code, res.stderr)
	}
	b.wantUnchanged(t, before)
}

// TestTableNewRefusesBeforeAnyChange: each of these is the invocation's mistake,
// exit 2, and the machine is as it was.
func TestTableNewRefusesBeforeAnyChange(t *testing.T) {
	cases := []struct {
		name  string
		want  string
		setup func(t *testing.T, b *tableBench) []string
	}{
		{"tracked .mcp.json", ".mcp.json", func(t *testing.T, b *tableBench) []string {
			if err := os.WriteFile(filepath.Join(b.repo, ".mcp.json"), []byte("{}"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitIn(t, b.repo, "add", "-A")
			gitIn(t, b.repo, "-c", "commit.gpgsign=false", "commit", "-q", "-m", "mcp")
			return b.newArgs("t1", nil)
		}},
		{"tracked .codex/config.toml", ".codex/config.toml", func(t *testing.T, b *tableBench) []string {
			if err := os.MkdirAll(filepath.Join(b.repo, ".codex"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(b.repo, ".codex", "config.toml"), []byte("x = 1\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitIn(t, b.repo, "add", "-A")
			gitIn(t, b.repo, "-c", "commit.gpgsign=false", "commit", "-q", "-m", "codex")
			return b.newArgs("t1", nil)
		}},
		{"tracked .parley-table", ".parley-table", func(t *testing.T, b *tableBench) []string {
			if err := os.WriteFile(filepath.Join(b.repo, ".parley-table"), []byte("t1"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitIn(t, b.repo, "add", "-A")
			gitIn(t, b.repo, "-c", "commit.gpgsign=false", "commit", "-q", "-m", "marker")
			return b.newArgs("t1", nil)
		}},
		{"bad id, upper case", "table id", func(t *testing.T, b *tableBench) []string { return b.newArgs("T1", nil) }},
		{"bad id, underscore", "table id", func(t *testing.T, b *tableBench) []string { return b.newArgs("a_b", nil) }},
		{"bad id, slash", "table id", func(t *testing.T, b *tableBench) []string { return b.newArgs("a/b", nil) }},
		{"bad id, empty", "table id", func(t *testing.T, b *tableBench) []string { return b.newArgs("", nil) }},
		{"bad id, too long", "table id", func(t *testing.T, b *tableBench) []string { return b.newArgs(strings.Repeat("a", 33), nil) }},
		{"duplicate seat nick", "also seat", func(t *testing.T, b *tableBench) []string {
			return b.newArgs("t1", []string{"a=codex", "a=claude"})
		}},
		{"duplicate seat nick, other case", "also seat", func(t *testing.T, b *tableBench) []string {
			return b.newArgs("t1", []string{"Alpha=codex", "alpha=claude"})
		}},
		{"the chair as a seat", "chair", func(t *testing.T, b *tableBench) []string {
			return b.newArgs("t1", []string{"Colin=codex"})
		}},
		{"tables as a seat nick", "tables", func(t *testing.T, b *tableBench) []string {
			return b.newArgs("t1", []string{"Tables=codex"})
		}},
		{"tables as the chair", "tables", func(t *testing.T, b *tableBench) []string {
			return b.newArgs("t1", nil, "--chair", "tables")
		}},
		{"opencode", "not supported yet", func(t *testing.T, b *tableBench) []string { return b.newArgs("t1", []string{"o=opencode"}) }},
		{"unknown harness", "unknown harness", func(t *testing.T, b *tableBench) []string { return b.newArgs("t1", []string{"o=gemini"}) }},
		{"no equals", "<nick>=", func(t *testing.T, b *tableBench) []string { return b.newArgs("t1", []string{"codex"}) }},
		{"bad seat nick", "nick", func(t *testing.T, b *tableBench) []string { return b.newArgs("t1", []string{"a.b=codex"}) }},
		{"empty model", "model", func(t *testing.T, b *tableBench) []string { return b.newArgs("t1", []string{"a=codex:"}) }},
		{"model with a control character", "control", func(t *testing.T, b *tableBench) []string {
			return b.newArgs("t1", []string{"a=codex:x\ny"})
		}},
		{"model that is a flag", "flag", func(t *testing.T, b *tableBench) []string {
			return b.newArgs("t1", []string{"a=codex:--dangerously-bypass-approvals-and-sandbox"})
		}},
		{"no seat", "--seat", func(t *testing.T, b *tableBench) []string { return b.newArgs("t1", []string{}) }},
		{"zero rounds", "--rounds", func(t *testing.T, b *tableBench) []string { return b.newArgs("t1", nil, "--rounds", "0") }},
		{"no chair", "--chair", func(t *testing.T, b *tableBench) []string {
			return []string{"table", "new", "t1", "--target", b.repo, "--rounds", "3", "--seat", "a=codex", "--state-dir", b.base}
		}},
		{"bad chair", "--chair", func(t *testing.T, b *tableBench) []string { return b.newArgs("t1", nil, "--chair", "co lin") }},
		{"an existing --worktree", "already exists", func(t *testing.T, b *tableBench) []string {
			return b.newArgs("t1", nil, "--worktree", b.root)
		}},
		{"a relative --worktree", "absolute", func(t *testing.T, b *tableBench) []string {
			return b.newArgs("t1", nil, "--worktree", "rel/wt")
		}},
		{"a --worktree whose parent is missing", "parent", func(t *testing.T, b *tableBench) []string {
			return b.newArgs("t1", nil, "--worktree", filepath.Join(b.root, "nope", "wt"))
		}},
		{"a --worktree inside the target", "inside the target", func(t *testing.T, b *tableBench) []string {
			return b.newArgs("t1", nil, "--worktree", filepath.Join(b.repo, "wt"))
		}},
		{"a $ in the --worktree", "'$'", func(t *testing.T, b *tableBench) []string {
			return b.newArgs("t1", nil, "--worktree", filepath.Join(b.root, "w$x"))
		}},
		{"a $ in the target path", "'$'", func(t *testing.T, b *tableBench) []string {
			dir := filepath.Join(b.root, "t$x")
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			initRepo(t, dir, nil)
			return b.newArgs("t1", nil, "--target", dir)
		}},
		{"a $ in the state directory of a claude seat", "state directory", func(t *testing.T, b *tableBench) []string {
			return b.newArgs("t1", nil, "--state-dir", filepath.Join(b.root, "s$x"), "--worktree", filepath.Join(b.root, "wt"))
		}},
		{"a target that is not a repo", "not a git checkout", func(t *testing.T, b *tableBench) []string {
			return b.newArgs("t1", nil, "--target", filepath.Join(b.root, "s"))
		}},
		{"a target below the root of its repo", "not the root", func(t *testing.T, b *tableBench) []string {
			sub := filepath.Join(b.repo, "sub")
			if err := os.Mkdir(sub, 0o755); err != nil {
				t.Fatal(err)
			}
			return b.newArgs("t1", nil, "--target", sub)
		}},
		{"a missing target", "--target", func(t *testing.T, b *tableBench) []string {
			return b.newArgs("t1", nil, "--target", filepath.Join(b.root, "nope"))
		}},
		{"a repo with no commit", "HEAD", func(t *testing.T, b *tableBench) []string {
			dir := filepath.Join(b.root, "empty")
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			gitIn(t, dir, "init", "-q")
			return b.newArgs("t1", nil, "--target", dir)
		}},
		{"a server that is not loopback", "--server", func(t *testing.T, b *tableBench) []string {
			return b.newArgs("t1", nil, "--server", "192.0.2.1:6667")
		}},
		{"a codex that is not there", "--codex-bin", func(t *testing.T, b *tableBench) []string {
			return b.newArgs("t1", nil, "--codex-bin", filepath.Join(b.root, "no-codex"))
		}},
		{"a .codex tracked as a symlink", ".codex", func(t *testing.T, b *tableBench) []string {
			if err := os.Symlink("elsewhere", filepath.Join(b.repo, ".codex")); err != nil {
				t.Fatal(err)
			}
			gitIn(t, b.repo, "add", "-A")
			gitIn(t, b.repo, "-c", "commit.gpgsign=false", "commit", "-q", "-m", "link")
			return b.newArgs("t1", nil)
		}},
		{"a .codex tracked as a file", "not as a directory", func(t *testing.T, b *tableBench) []string {
			if err := os.WriteFile(filepath.Join(b.repo, ".codex"), []byte("x\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitIn(t, b.repo, "add", "-A")
			gitIn(t, b.repo, "-c", "commit.gpgsign=false", "commit", "-q", "-m", "file")
			return b.newArgs("t1", nil)
		}},
		{"a .mcp.json tracked as a symlink", ".mcp.json", func(t *testing.T, b *tableBench) []string {
			if err := os.Symlink("README.md", filepath.Join(b.repo, ".mcp.json")); err != nil {
				t.Fatal(err)
			}
			gitIn(t, b.repo, "add", "-A")
			gitIn(t, b.repo, "-c", "commit.gpgsign=false", "commit", "-q", "-m", "link")
			return b.newArgs("t1", nil)
		}},
		{"a .mcp.json tracked as a directory", ".mcp.json", func(t *testing.T, b *tableBench) []string {
			if err := os.MkdirAll(filepath.Join(b.repo, ".mcp.json"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(b.repo, ".mcp.json", "x"), []byte("x\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitIn(t, b.repo, "add", "-A")
			gitIn(t, b.repo, "-c", "commit.gpgsign=false", "commit", "-q", "-m", "dir")
			return b.newArgs("t1", nil)
		}},
		{"a state directory inside the target", "inside the target checkout", func(t *testing.T, b *tableBench) []string {
			return b.newArgs("t1", nil, "--state-dir", filepath.Join(b.repo, "st"), "--worktree", filepath.Join(b.root, "wt"))
		}},
		{"a state directory inside the worktree", "inside the worktree", func(t *testing.T, b *tableBench) []string {
			return b.newArgs("t1", nil, "--state-dir", filepath.Join(b.root, "wt", "st"), "--worktree", filepath.Join(b.root, "wt"))
		}},
		{"whitespace in the state directory", "whitespace", func(t *testing.T, b *tableBench) []string {
			return b.newArgs("t1", nil, "--state-dir", filepath.Join(b.root, "s p"))
		}},
		{"a claude seat whose worktree puts the instructions over the cap", "UTF-16 code units", func(t *testing.T, b *tableBench) []string {
			long := b.root
			for i := 0; i < 3; i++ {
				long = filepath.Join(long, strings.Repeat("w", 230))
			}
			if err := os.MkdirAll(long, 0o755); err != nil {
				t.Fatal(err)
			}
			return b.newArgs("t1", []string{"claude-a=claude"}, "--worktree", filepath.Join(long, "wt"))
		}},
		{"a claude seat whose instructions are over the cap", "UTF-16 code units", func(t *testing.T, b *tableBench) []string {
			long := b.root
			for i := 0; i < 3; i++ {
				long = filepath.Join(long, strings.Repeat("d", 230))
			}
			return b.newArgs("t1", []string{"claude-a=claude"}, "--state-dir", filepath.Join(long, "st"))
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := newTableBench(t)
			args := c.setup(t, b)
			before := b.snapshot(t)
			res := b.run(t, args...)
			if res.code != 2 {
				t.Fatalf("exit %d, want 2\nstdout: %s\nstderr: %s", res.code, res.stdout, res.stderr)
			}
			t.Logf("stderr: %s", strings.TrimSpace(res.stderr))
			if !strings.Contains(res.stderr, c.want) {
				t.Errorf("stderr does not say %q:\n%s", c.want, res.stderr)
			}
			if res.stdout != "" {
				t.Errorf("a refused run printed on stdout: %q", res.stdout)
			}
			b.wantUnchanged(t, before)
		})
	}
}

// TestTableNewRefusesABinaryWithADollarInItsPath: Claude Code expands ${...} in
// the .mcp.json's command, so the binary may not live under such a path.
func TestTableNewRefusesABinaryWithADollarInItsPath(t *testing.T) {
	b := newTableBench(t)
	dir := filepath.Join(b.root, "b$x")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "parley")
	if err := os.WriteFile(bin, raw, 0o755); err != nil {
		t.Fatal(err)
	}
	before := b.snapshot(t)
	cmd := exec.Command(bin, b.newArgs("t1", nil)...)
	cmd.Env = append(os.Environ(), b.env()...)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 2 || !strings.Contains(string(out), "binary") {
		t.Fatalf("run from a $ path: %v\n%s", err, out)
	}
	b.wantUnchanged(t, before)
}

// TestTableNewRefusesATempDirectoryInsideTheTargetOrTheWorktree: the reply
// directory is made under $TMPDIR, and the target checkout and the worktree are
// not where it goes. Exit 2, nothing changed.
func TestTableNewRefusesATempDirectoryInsideTheTargetOrTheWorktree(t *testing.T) {
	for _, tc := range []struct {
		name string
		tmp  func(b *tableBench) string
		args func(b *tableBench) []string
		want string
	}{
		{"the target", func(b *tableBench) string { return filepath.Join(b.repo, "tmp") },
			func(b *tableBench) []string { return b.newArgs("t1", nil) }, "inside the target checkout"},
		{"the target itself", func(b *tableBench) string { return b.repo },
			func(b *tableBench) []string { return b.newArgs("t1", nil) }, "inside the target checkout"},
		{"the worktree", func(b *tableBench) string { return filepath.Join(b.root, "wt", "tmp") },
			func(b *tableBench) []string { return b.newArgs("t1", nil, "--worktree", filepath.Join(b.root, "wt")) }, "inside the worktree"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newTableBench(t)
			before := b.snapshot(t)
			env := append(b.env(), "TMPDIR="+tc.tmp(b))
			res := runParleyEnvIn(t, "", env, tc.args(b)...)
			if res.code != 2 || !strings.Contains(res.stderr, tc.want) || !strings.Contains(res.stderr, "TMPDIR") {
				t.Fatalf("exit %d, want 2 saying the TMPDIR is %s\n%s", res.code, tc.want, res.stderr)
			}
			b.wantUnchanged(t, before)
		})
	}
}

// TestTableNewDoctorFailAbortsBeforeAnyChange: a machine without claude, or
// without the skill links, is a FAIL, and a FAIL changes nothing.
func TestTableNewDoctorFailAbortsBeforeAnyChange(t *testing.T) {
	t.Run("no claude", func(t *testing.T) {
		b := newTableBench(t)
		before := b.snapshot(t)
		// A PATH of codex, ergo and the tools they and git need, and no claude.
		res := runParleyEnvIn(t, "", b.envWithPath(b.pathWith(t, "codex", "ergo", "git", "cat", "mv")), b.newArgs("t1", nil)...)
		if res.code != 1 || !strings.Contains(res.stderr, "FAIL") || !strings.Contains(res.stderr, "claude") {
			t.Fatalf("exit %d, want 1 with doctor's claude FAIL\n%s", res.code, res.stderr)
		}
		b.wantUnchanged(t, before)
	})
	t.Run("no ergo, and nothing answering at the server", func(t *testing.T) {
		b := newTableBench(t)
		before := b.snapshot(t)
		// The first step of the runbook is parley server, which runs ergo.
		res := runParleyEnvIn(t, "", b.envWithPath(b.pathWith(t, "codex", "claude", "git", "cat", "mv")), b.newArgs("t1", nil)...)
		if res.code != 1 || !strings.Contains(res.stderr, "FAIL ergo") || !strings.Contains(res.stderr, "contrib/ergo/README.md") {
			t.Fatalf("exit %d, want 1 with doctor's ergo FAIL\n%s", res.code, res.stderr)
		}
		b.wantUnchanged(t, before)
	})
	t.Run("no ergo, and a server that answers", func(t *testing.T) {
		b := newTableBench(t)
		startFakeIRC(t, b.db.server)
		res := runParleyEnvIn(t, "", b.envWithPath(b.pathWith(t, "codex", "claude", "git", "cat", "mv")), b.newArgs("t1", nil)...)
		if res.code != 0 || strings.Contains(res.stderr, "doctor:") {
			t.Fatalf("exit %d, want 0 and no doctor line: the server is up, so nothing needs ergo\n%s", res.code, res.stderr)
		}
	})
	t.Run("no skill links", func(t *testing.T) {
		b := newTableBench(t)
		if err := os.RemoveAll(filepath.Join(b.db.home, ".agents")); err != nil {
			t.Fatal(err)
		}
		before := b.snapshot(t)
		res := b.run(t, b.newArgs("t1", nil)...)
		if res.code != 1 || !strings.Contains(res.stderr, "FAIL") || !strings.Contains(res.stderr, "skill") {
			t.Fatalf("exit %d, want 1 with doctor's skill FAIL\n%s", res.code, res.stderr)
		}
		b.wantUnchanged(t, before)
	})
	// The check covers the parley-table link under the root each seat's harness
	// reads, and no other.
	t.Run("the parley-build skill is not a table's business", func(t *testing.T) {
		b := newTableBench(t)
		for _, root := range []string{".claude", ".agents"} {
			if err := os.Remove(filepath.Join(b.db.home, root, "skills", "parley-build")); err != nil {
				t.Fatal(err)
			}
		}
		mustTable(t, b, b.newArgs("t1", nil)...)
	})
	t.Run("a Claude-only table does not read the Codex root", func(t *testing.T) {
		b := newTableBench(t)
		if err := os.RemoveAll(filepath.Join(b.db.home, ".agents")); err != nil {
			t.Fatal(err)
		}
		mustTable(t, b, b.newArgs("t1", []string{"claude-a=claude"})...)
	})
	t.Run("a Codex-only table does not read the Claude root", func(t *testing.T) {
		b := newTableBench(t)
		if err := os.RemoveAll(filepath.Join(b.db.home, ".claude")); err != nil {
			t.Fatal(err)
		}
		mustTable(t, b, b.newArgs("t1", []string{"codex-a=codex"})...)
	})
	t.Run("a Claude seat needs the table skill under the Claude root", func(t *testing.T) {
		b := newTableBench(t)
		if err := os.Remove(filepath.Join(b.db.home, ".claude", "skills", "parley-table")); err != nil {
			t.Fatal(err)
		}
		before := b.snapshot(t)
		res := b.run(t, b.newArgs("t1", []string{"claude-a=claude"})...)
		if res.code != 1 || !strings.Contains(res.stderr, "FAIL") || !strings.Contains(res.stderr, filepath.Join(".claude", "skills", "parley-table")) {
			t.Fatalf("exit %d, want 1 with doctor's skill FAIL\n%s", res.code, res.stderr)
		}
		b.wantUnchanged(t, before)
	})
	t.Run("a codex flag gone", func(t *testing.T) {
		b := newTableBench(t)
		before := b.snapshot(t)
		// The same fake with the --force line of delete's help removed.
		script, err := os.ReadFile(b.codex)
		if err != nil {
			t.Fatal(err)
		}
		patched := strings.Replace(string(script), `--help) cat`, `--help) if [ "$1" = delete ]; then echo "      --nothing"; exit 0; fi; cat`, 1)
		if patched == string(script) {
			t.Fatal("could not patch the fake codex")
		}
		if err := os.WriteFile(b.codex, []byte(patched), 0o755); err != nil {
			t.Fatal(err)
		}
		res := b.run(t, b.newArgs("t1", nil)...)
		if res.code != 1 || !strings.Contains(res.stderr, "FAIL") || !strings.Contains(res.stderr, "--force") {
			t.Fatalf("exit %d, want 1 with doctor's delete FAIL\n%s", res.code, res.stderr)
		}
		b.wantUnchanged(t, before)
	})
}

// TestTableNewFailureRollsEverythingBack: a codex that fails, announces no
// thread, or ends without a completed turn (no turn.completed, or an output cut
// off at the cap), is exit 1 and a machine as it was, with every thread it did make
// deleted.
func TestTableNewFailureRollsEverythingBack(t *testing.T) {
	cases := []struct {
		name  string
		seats []string
		modes map[int]string // fake call number -> mode
		want  string
		// deleted maps the number of a delete call to the number of the exec call
		// whose thread it must delete.
		deleted map[int]int
	}{
		{"codex exits nonzero", []string{"codex-a=codex", "claude-a=claude"}, map[int]string{1: "fail"}, "boom: not logged in", nil},
		{"no thread.started", []string{"codex-a=codex"}, map[int]string{1: "nothread"}, "no thread", nil},
		{"a thread that is not a UUID", []string{"codex-a=codex"}, map[int]string{1: "badid"}, "not a UUID", nil},
		{"thread announced, then the turn fails", []string{"codex-a=codex"}, map[int]string{1: "failafter"}, "the model went away", map[int]int{2: 1}},
		{"the second seat fails after the first succeeded", []string{"codex-a=codex", "codex-b=codex", "claude-a=claude"}, map[int]string{2: "fail"}, "boom", map[int]int{3: 1}},
		{"codex exits 0 but the turn failed", []string{"codex-a=codex"}, map[int]string{1: "failzero"}, "quota exceeded", map[int]int{2: 1}},
		{"codex exits 0 with no turn.completed", []string{"codex-a=codex"}, map[int]string{1: "nocomplete"}, "no turn.completed event", map[int]int{2: 1}},
		{"stdout past the cap, turn.completed beyond it", []string{"codex-a=codex"}, map[int]string{1: "overflow"}, "cut off", map[int]int{2: 1}},
		{"stdout past the cap, turn.completed inside it", []string{"codex-a=codex"}, map[int]string{1: "overflowafter"}, "cut off", map[int]int{2: 1}},
		{"codex exits 0 but the turn carried an error", []string{"codex-a=codex", "claude-a=claude"}, map[int]string{1: "errorzero"}, "stream broke", map[int]int{2: 1}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := newTableBench(t)
			for n, m := range c.modes {
				b.setMode(t, n, m)
			}
			before := b.snapshot(t)
			res := b.run(t, b.newArgs("t1", c.seats)...)
			if res.code != 1 {
				t.Fatalf("exit %d, want 1\nstdout: %s\nstderr: %s", res.code, res.stdout, res.stderr)
			}
			if !strings.Contains(res.stderr, c.want) {
				t.Errorf("stderr does not say %q:\n%s", c.want, res.stderr)
			}
			if !strings.Contains(res.stderr, "rolled back") || !strings.Contains(res.stderr, "worktree") {
				t.Errorf("stderr does not say what was rolled back:\n%s", res.stderr)
			}
			if res.stdout != "" {
				t.Errorf("a failed run printed on stdout: %q", res.stdout)
			}
			// Every thread made is deleted, without a prompt (--force), and nothing
			// is deleted that was not made.
			deletes := 0
			for n := 1; n <= b.callCount(); n++ {
				argv := b.call(t, n)
				if argv[0] != "delete" {
					continue
				}
				deletes++
				exec, ok := c.deleted[n]
				if !ok {
					t.Errorf("call %d is %q, an unexpected delete", n, argv)
					continue
				}
				if strings.Join(argv, " ") != "delete --force "+fakeThread(exec) {
					t.Errorf("call %d is %q, want delete --force %s", n, argv, fakeThread(exec))
				}
			}
			if deletes != len(c.deleted) {
				t.Errorf("%d delete calls, want %d", deletes, len(c.deleted))
			}
			// The machine, but for the fake's own call log, is as it was.
			after := strings.Split(b.snapshot(t), "codex calls:")[0]
			if want := strings.Split(before, "codex calls:")[0]; after != want {
				t.Fatalf("the run left a trace.\nbefore:\n%s\nafter:\n%s", want, after)
			}
		})
	}
}

// TestLimitBufferSaysWhetherItDroppedAnything: a run whose output ended exactly
// at the cap lost nothing; one byte more and the end of it is unseen.
func TestLimitBufferSaysWhetherItDroppedAnything(t *testing.T) {
	b := &limitBuffer{limit: 8}
	if _, err := b.Write([]byte("12345678")); err != nil {
		t.Fatal(err)
	}
	if b.truncated {
		t.Error("a write that filled the buffer exactly was reported as truncated")
	}
	if n, err := b.Write([]byte("9")); err != nil || n != 1 {
		t.Fatalf("write returned %d, %v; a discarded write still reports its length", n, err)
	}
	if !b.truncated || b.String() != "12345678" {
		t.Errorf("truncated %v, kept %q; want true and the first 8 bytes", b.truncated, b.String())
	}
}

// TestTableNewStripsControlCharactersFromWhatCodexSaid: an event type, a failure
// message and stderr are codex's text, and they are printed on the terminal of
// whoever ran the command.
func TestTableNewStripsControlCharactersFromWhatCodexSaid(t *testing.T) {
	for _, tc := range []struct{ name, mode, want string }{
		{"a failure message", "ctrlfail", "quota[31m exceeded"},
		{"stderr", "ctrlstderr", "boom[31m: not logged in"},
		{"event types", "ctrlevents", "odd[2Jevent, turn.completed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newTableBench(t)
			b.setMode(t, 1, tc.mode)
			res := b.run(t, b.newArgs("t1", []string{"codex-a=codex"})...)
			if res.code != 1 || !strings.Contains(res.stderr, tc.want) {
				t.Fatalf("exit %d, want 1 with %q on stderr\n%s", res.code, tc.want, res.stderr)
			}
			if strings.ContainsAny(res.stderr+res.stdout, "\x1b\x07") {
				t.Errorf("a control character reached the terminal: %q", res.stderr)
			}
		})
	}
}

// TestTableNewDeleteThatFailsIsReportedNotHidden: the rollback says which thread
// is left and how to remove it, and the exit stays 1.
func TestTableNewDeleteThatFailsIsReportedNotHidden(t *testing.T) {
	b := newTableBench(t)
	b.setMode(t, 1, "failafter")
	b.setMode(t, 2, "faildelete")
	res := b.run(t, b.newArgs("t1", []string{"codex-a=codex"})...)
	if res.code != 1 {
		t.Fatalf("exit %d, want 1\n%s", res.code, res.stderr)
	}
	if !strings.Contains(res.stderr, "could NOT roll back") || !strings.Contains(res.stderr, "codex delete "+fakeThread(1)) {
		t.Errorf("stderr does not name the thread that is left:\n%s", res.stderr)
	}
	// With no --codex-bin the codex that ran is the one on the PATH, spelled plain.
	for _, want := range []string{"codex delete --force " + fakeThread(1), "remove it yourself with: codex delete " + fakeThread(1)} {
		if !strings.Contains(res.stderr, want) {
			t.Errorf("stderr does not say %q:\n%s", want, res.stderr)
		}
	}
}

// TestTableNewDeleteThatFailsNamesTheSelectedCodex: with --codex-bin the run used
// that binary, so the command the rollback says it ran, and the one it tells the
// human to run by hand, name it, shell-quoted, rather than a plain codex that may
// be another program or none.
func TestTableNewDeleteThatFailsNamesTheSelectedCodex(t *testing.T) {
	b := newTableBench(t)
	// A copy of the fake at a path that needs quoting.
	dir := filepath.Join(b.root, "other bin")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "codex")
	if err := os.WriteFile(bin, mustRead(t, b.codex), 0o755); err != nil {
		t.Fatal(err)
	}
	b.setMode(t, 1, "failafter")
	b.setMode(t, 2, "faildelete")
	res := b.run(t, b.newArgs("t1", []string{"codex-a=codex"}, "--codex-bin", bin)...)
	if res.code != 1 {
		t.Fatalf("exit %d, want 1\n%s", res.code, res.stderr)
	}
	quoted := "'" + bin + "'"
	for _, want := range []string{
		"could NOT roll back",
		quoted + " delete --force " + fakeThread(1),
		"remove it yourself with: " + quoted + " delete " + fakeThread(1),
	} {
		if !strings.Contains(res.stderr, want) {
			t.Errorf("stderr does not say %q:\n%s", want, res.stderr)
		}
	}
	if strings.Contains(res.stderr, "with: codex delete") || strings.Contains(res.stderr, ": codex delete --force") {
		t.Errorf("stderr names a plain codex though --codex-bin was given:\n%s", res.stderr)
	}
}

// TestTableNewRollsBackAClaudeOnlyTableOnALateFailure: the last step, the sheet
// copy, fails; the worktree, the reply directory and the table directory are all
// taken down, and there is no codex to delete from.
func TestTableNewRollsBackAClaudeOnlyTableOnALateFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads any file")
	}
	b := newTableBench(t)
	sheet := filepath.Join(b.db.home, ".claude", "skills", "parley-table", "sheet.md")
	skill, err := os.Readlink(filepath.Dir(sheet))
	if err != nil {
		t.Fatal(err)
	}
	unreadable := filepath.Join(skill, "sheet.md")
	if err := os.Chmod(unreadable, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(unreadable, 0o644) })
	before := b.snapshot(t)
	res := b.run(t, b.newArgs("t1", []string{"claude-a=claude"})...)
	if res.code != 1 || !strings.Contains(res.stderr, "sheet template") || !strings.Contains(res.stderr, "rolled back") {
		t.Fatalf("exit %d, want 1 after a sheet failure, rolled back\n%s", res.code, res.stderr)
	}
	b.wantUnchanged(t, before)
}

// TestTableNewWithoutASheetTemplateSaysWhereToGetIt: not a failure.
func TestTableNewWithoutASheetTemplateSaysWhereToGetIt(t *testing.T) {
	b := newTableBench(t)
	skill, err := os.Readlink(filepath.Join(b.db.home, ".claude", "skills", "parley-table"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(skill, "sheet.md")); err != nil {
		t.Fatal(err)
	}
	res := mustTable(t, b, b.newArgs("t1", []string{"claude-a=claude"})...)
	sheet := filepath.Join(b.base, "tables", "t1", "sheet.md")
	if _, err := os.Stat(sheet); err == nil {
		t.Error("a sheet was made with no template to copy")
	}
	for _, want := range []string{"No sheet template was found", "skills/parley-table/sheet.md", "TABLE t1 " + sheet} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, res.stdout)
		}
	}
}

// TestTableNewPumpLineDeliversIntoTheRecordedThread is the whole path on a real
// ergo: the pump line table new printed, run as printed, finds the thread table
// new recorded and queues the chair's post into it.
func TestTableNewPumpLineDeliversIntoTheRecordedThread(t *testing.T) {
	addr := startErgo(t)
	b := newTableBench(t)
	b.db.server = addr
	res := mustTable(t, b, b.newArgs("t6", []string{"codex-a=codex", "claude-a=claude"})...)
	if strings.Contains(res.stdout, "' server") {
		t.Errorf("a real server answers, yet stdout has a server line:\n%s", res.stdout)
	}

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

	// The chair posts; the gates let the chair's line through.
	stream := "irc://" + addr + "/#t6"
	mustRun(t, "", "send", "--stream", stream, "--nick", "colin", "--state-dir", t.TempDir(), "--", "welcome to the table")
	argv := b.waitCall(t, 2, stderr)
	if len(argv) != 5 || argv[0] != "queue" || argv[1] != "--thread" || argv[2] != fakeThread(1) || argv[3] != "--message" {
		t.Fatalf("codex was run with %q, want queue --thread %s --message <text>", argv, fakeThread(1))
	}
	if !strings.HasPrefix(argv[4], "[parley] from=colin id=") || !strings.Contains(argv[4], "welcome to the table") {
		t.Fatalf("the queued message is not the chair's post under the peer header:\n%q", argv[4])
	}
}

// tableUp sets a two-seat table up and returns its pieces.
func tableUp(t *testing.T, b *tableBench, id string) (wt string, rec tableRecord) {
	t.Helper()
	mustTable(t, b, b.newArgs(id, nil)...)
	raw, err := os.ReadFile(filepath.Join(b.base, "tables", id, "table.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	return rec.Worktree, rec
}

// TestTableEndTakesTheTableDown: the worktree, the reply directory, the thread
// record and the table directory go, the machine is as before table new, and the
// codex delete line is printed and not run.
func TestTableEndTakesTheTableDown(t *testing.T) {
	b := newTableBench(t)
	_, rec := tableUp(t, b, "t1")
	calls := b.callCount()

	res := mustTable(t, b, b.endArgs("t1")...)
	t.Logf("stdout:\n%s", res.stdout)
	if got := b.callCount(); got != calls {
		t.Errorf("table end called codex (%d calls, was %d): it must print the delete lines, not run them", got, calls)
	}
	want := "codex delete '" + fakeThread(1) + "'"
	if !strings.Contains(res.stdout, want) {
		t.Errorf("stdout lacks %q:\n%s", want, res.stdout)
	}
	if strings.Count(res.stdout, "codex delete") != 1 {
		t.Errorf("one Codex seat, yet the delete lines are:\n%s", res.stdout)
	}
	for _, gone := range []string{rec.Worktree, rec.ReplyDir, filepath.Join(b.base, "tables", "t1")} {
		if _, err := os.Lstat(gone); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s is still there (%v)", gone, err)
		}
	}
	tr, _ := state.OpenThreadRecord(b.base, "codex-a", rec.Stream)
	if got, err := tr.Read(); err != nil || got != "" {
		t.Errorf("the thread record is still there: %q, %v", got, err)
	}
	if list := gitIn(t, b.repo, "worktree", "list", "--porcelain"); strings.Count(list, "worktree ") != 1 {
		t.Errorf("git worktree list still shows the table's worktree:\n%s", list)
	}
	// The thread record's directory went with it, and the target and the codex
	// calls are as table new left them.
	if _, err := os.Stat(filepath.Dir(tr.Path())); err == nil {
		t.Errorf("the seat's stream directory %s remains", filepath.Dir(tr.Path()))
	}
	if got := gitIn(t, b.repo, "status", "--porcelain"); got != "" {
		t.Errorf("the target is not clean: %s", got)
	}
	// Ending it again is the invocation's mistake.
	if again := b.run(t, b.endArgs("t1")...); again.code != 2 || !strings.Contains(again.stderr, "no table") {
		t.Errorf("ending a table twice exited %d: %s", again.code, again.stderr)
	}
}

// TestTableEndRefusesADirtyWorktreeWithoutForce: a change that table new did not
// make is the human's to look at first.
func TestTableEndRefusesADirtyWorktreeWithoutForce(t *testing.T) {
	b := newTableBench(t)
	wt, rec := tableUp(t, b, "t1")
	if err := os.WriteFile(filepath.Join(wt, "scratch.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := b.run(t, b.endArgs("t1")...)
	if res.code != 1 || !strings.Contains(res.stderr, "scratch.txt") || !strings.Contains(res.stderr, "--force") {
		t.Fatalf("exit %d, want 1 naming the change and --force\n%s", res.code, res.stderr)
	}
	for _, still := range []string{wt, rec.ReplyDir, filepath.Join(b.base, "tables", "t1", "table.json")} {
		if _, err := os.Stat(still); err != nil {
			t.Errorf("a refused end removed %s: %v", still, err)
		}
	}
	forced := mustTable(t, b, b.endArgs("t1", "--force")...)
	if _, err := os.Stat(wt); err == nil {
		t.Errorf("--force left the worktree\n%s", forced.stdout)
	}
}

// TestTableEndDirtyIsWhatTableNewDidNotWrite: the files table new wrote are not
// changes: the .parley-table marker at the root, whatever the seats, and the ones
// its record's seats call for, .mcp.json for Claude seats and .codex/config.toml
// for Codex ones. Every other file, in .codex/ or not, is, a .parley-table below
// the root included, and so is an edit to a tracked file under .codex/.
func TestTableEndDirtyIsWhatTableNewDidNotWrite(t *testing.T) {
	write := func(t *testing.T, path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	both := []string{"codex-a=codex", "claude-a=claude"}
	claudeOnly, codexOnly := []string{"claude-a=claude"}, []string{"codex-a=codex"}
	cases := []struct {
		name   string
		seats  []string
		change func(t *testing.T, wt string)
		dirty  string // the file the refusal names, or empty for a clean worktree
	}{
		{"nothing, both harnesses", both, func(t *testing.T, wt string) {}, ""},
		{"nothing, Claude seats only", claudeOnly, func(t *testing.T, wt string) {}, ""},
		{"nothing, Codex seats only", codexOnly, func(t *testing.T, wt string) {}, ""},
		{"a modified tracked file", both, func(t *testing.T, wt string) { write(t, filepath.Join(wt, "README.md"), "changed\n") }, "README.md"},
		{"a tracked file under .codex/", both, func(t *testing.T, wt string) { write(t, filepath.Join(wt, ".codex", "rules.txt"), "changed\n") }, ".codex/rules.txt"},
		{"an untracked file", both, func(t *testing.T, wt string) { write(t, filepath.Join(wt, "scratch.txt"), "x") }, "scratch.txt"},
		{"a .parley-table below the root", both, func(t *testing.T, wt string) { write(t, filepath.Join(wt, "sub", ".parley-table"), "t1") }, "sub/.parley-table"},
		{"an untracked file beside the .parley-table", both, func(t *testing.T, wt string) { write(t, filepath.Join(wt, ".parley-table.bak"), "t1") }, ".parley-table.bak"},
		{"an untracked file beside the .mcp.json", both, func(t *testing.T, wt string) { write(t, filepath.Join(wt, ".mcp.json.bak"), "x") }, ".mcp.json.bak"},
		{"another untracked file in .codex/", both, func(t *testing.T, wt string) { write(t, filepath.Join(wt, ".codex", "other.toml"), "x") }, ".codex/other.toml"},
		{"another untracked file in .codex/, Codex seats only", codexOnly, func(t *testing.T, wt string) { write(t, filepath.Join(wt, ".codex", "other.toml"), "x") }, ".codex/other.toml"},
		{"a .codex/config.toml in a table with no Codex seat", claudeOnly, func(t *testing.T, wt string) { write(t, filepath.Join(wt, ".codex", "config.toml"), "x") }, ".codex/config.toml"},
		{"a .mcp.json in a table with no Claude seat", codexOnly, func(t *testing.T, wt string) { write(t, filepath.Join(wt, ".mcp.json"), "{}") }, ".mcp.json"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := newTableBench(t)
			// A target that tracks a file under .codex/, so that directory is
			// not wholly new.
			write(t, filepath.Join(b.repo, ".codex", "rules.txt"), "rules\n")
			gitIn(t, b.repo, "add", "-A")
			gitIn(t, b.repo, "-c", "commit.gpgsign=false", "commit", "-q", "-m", "rules")
			mustTable(t, b, b.newArgs("t1", c.seats)...)
			wt := filepath.Join(b.base, "tables", "t1", "worktree")
			c.change(t, wt)
			res := b.run(t, b.endArgs("t1")...)
			if c.dirty != "" && (res.code != 1 || !strings.Contains(res.stderr, "changes") || !strings.Contains(res.stderr, c.dirty)) {
				t.Fatalf("exit %d, want 1 for a dirty worktree naming %s\n%s", res.code, c.dirty, res.stderr)
			}
			if c.dirty == "" && res.code != 0 {
				t.Fatalf("exit %d, want 0 for a worktree holding only what table new wrote\n%s", res.code, res.stderr)
			}
		})
	}
}

// TestTableEndDirtyCountsIgnoredFilesAndQuotesNames: a file the target's
// .gitignore matches is still one table new did not write, the files it did
// write stay exempt when the target ignores them, and a name a seat made is
// quoted in the refusal, never printed raw.
func TestTableEndDirtyCountsIgnoredFilesAndQuotesNames(t *testing.T) {
	cases := []struct {
		name, gitignore, file, want string
	}{
		{"an ignored build artifact", "build/\n", "build/out.bin", "build/out.bin"},
		{"the files table new wrote, all ignored by the target", ".mcp.json\n.codex/config.toml\n.parley-table\n", "", ""},
		{"a name holding an escape", "", "bad\x1b[2Jname", `bad\x1b[2Jname`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := newTableBench(t)
			if c.gitignore != "" {
				if err := os.WriteFile(filepath.Join(b.repo, ".gitignore"), []byte(c.gitignore), 0o644); err != nil {
					t.Fatal(err)
				}
				gitIn(t, b.repo, "add", "-A")
				gitIn(t, b.repo, "-c", "commit.gpgsign=false", "commit", "-q", "-m", "ignore")
			}
			mustTable(t, b, b.newArgs("t1", []string{"codex-a=codex", "claude-a=claude"})...)
			wt := filepath.Join(b.base, "tables", "t1", "worktree")
			if c.file != "" {
				path := filepath.Join(wt, c.file)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			res := b.run(t, b.endArgs("t1")...)
			if c.want == "" {
				if res.code != 0 {
					t.Fatalf("exit %d, want 0 for a worktree holding only what table new wrote\n%s", res.code, res.stderr)
				}
				return
			}
			if res.code != 1 || !strings.Contains(res.stderr, c.want) {
				t.Fatalf("exit %d, want 1 naming %s\n%s", res.code, c.want, res.stderr)
			}
			if strings.ContainsRune(res.stderr, 0x1b) {
				t.Errorf("a file name reached the refusal raw: %q", res.stderr)
			}
			if _, err := os.Lstat(filepath.Join(wt, c.file)); err != nil {
				t.Errorf("the refused end removed %s: %v", c.file, err)
			}
		})
	}
}

// TestTableEndSaysWhenEveryChangeIsAnIgnoredFile: a worktree whose only changes
// are ignored files is refused as any dirty one is, but the refusal says what
// they are and that a tool's session hook may have written them; one untracked
// file among them, or an untracked .director/, keeps the plain wording. Ignored
// files still count: --force is what removes the worktree.
func TestTableEndSaysWhenEveryChangeIsAnIgnoredFile(t *testing.T) {
	const ignoredOnly = "a tool's session hook may have written them"
	cases := []struct {
		name, gitignore string
		files           []string
		wantIgnoredOnly bool
		want            []string
	}{
		{"every change ignored", "build/\n*.log\n", []string{"build/out.bin", "build/more.bin", "x.log"}, true,
			[]string{"all 3 are ignored files", "a tool's session hook may have written them", "look at them", "--force", `"build/out.bin"`, `"x.log"`}},
		{"an ignored .director/", ".director/\n", []string{".director/session.json"}, true,
			[]string{"it is an ignored file", `".director/session.json"`, "session hook"}},
		{"an ignored file and an untracked one", "build/\n", []string{"build/out.bin", "scratch.txt"}, false,
			[]string{`"scratch.txt"`, "look at them, or --force to remove it anyway"}},
		{"an untracked .director/", "", []string{".director/session.json"}, false,
			[]string{`".director/session.json"`, "look at them, or --force to remove it anyway"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := newTableBench(t)
			if c.gitignore != "" {
				if err := os.WriteFile(filepath.Join(b.repo, ".gitignore"), []byte(c.gitignore), 0o644); err != nil {
					t.Fatal(err)
				}
				gitIn(t, b.repo, "add", "-A")
				gitIn(t, b.repo, "-c", "commit.gpgsign=false", "commit", "-q", "-m", "ignore")
			}
			wt, rec := tableUp(t, b, "t1")
			for _, f := range c.files {
				path := filepath.Join(wt, f)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			res := b.run(t, b.endArgs("t1")...)
			if res.code != 1 {
				t.Fatalf("exit %d, want 1\n%s", res.code, res.stderr)
			}
			if got := strings.Contains(res.stderr, ignoredOnly); got != c.wantIgnoredOnly {
				t.Errorf("the refusal says the changes are all ignored files: %v, want %v\n%s", got, c.wantIgnoredOnly, res.stderr)
			}
			for _, want := range c.want {
				if !strings.Contains(res.stderr, want) {
					t.Errorf("the refusal does not say %q:\n%s", want, res.stderr)
				}
			}
			if !c.wantIgnoredOnly && strings.Contains(res.stderr, "session hook") {
				t.Errorf("the plain refusal blames a session hook:\n%s", res.stderr)
			}
			for _, still := range []string{wt, rec.ReplyDir} {
				if _, err := os.Stat(still); err != nil {
					t.Errorf("a refused end removed %s: %v", still, err)
				}
			}
			// The semantics are unchanged: --force removes it.
			mustTable(t, b, b.endArgs("t1", "--force")...)
			if _, err := os.Stat(wt); err == nil {
				t.Errorf("--force left the worktree")
			}
		})
	}
}

// TestDirtyRefusalWordsByWhatGitSaw: the ignored-only wording needs every listed
// change ignored and git's status not cut off, since a cut-off status may hold
// others that are not.
func TestDirtyRefusalWordsByWhatGitSaw(t *testing.T) {
	ign := statusEntry{"!!", "build/out.bin"}
	for _, tc := range []struct {
		name        string
		dirty       []statusEntry
		more        bool
		wantIgnored bool
	}{
		{"all ignored", []statusEntry{ign, {"!!", "x.log"}}, false, true},
		{"all ignored but the list was cut off", []statusEntry{ign}, true, false},
		{"one untracked", []statusEntry{ign, {"??", "scratch.txt"}}, false, false},
		{"one modified", []statusEntry{ign, {" M", "README.md"}}, false, false},
		{"only an untracked file", []statusEntry{{"??", "scratch.txt"}}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := dirtyRefusal("/wt", tc.dirty, tc.more).Error()
			if got := strings.Contains(msg, "ignored files"); got != tc.wantIgnored {
				t.Errorf("says ignored files: %v, want %v: %s", got, tc.wantIgnored, msg)
			}
			if !strings.Contains(msg, "--force") || !strings.Contains(msg, "/wt") {
				t.Errorf("a refusal that does not name the worktree and --force: %s", msg)
			}
			if tc.more && !strings.Contains(msg, "and more past") {
				t.Errorf("a cut-off status is not said to be: %s", msg)
			}
		})
	}
}

// TestTableEndRefusesWhileASeatIsRunning: a held lock, a pump's or a Claude
// seat's channel's, is a seat still running, and it is named. Nothing is removed.
func TestTableEndRefusesWhileASeatIsRunning(t *testing.T) {
	for _, nick := range []string{"codex-a", "claude-a"} {
		t.Run(nick, func(t *testing.T) {
			b := newTableBench(t)
			wt, rec := tableUp(t, b, "t1")
			d, err := state.Open(b.base, nick, rec.Stream)
			if err != nil {
				t.Fatal(err)
			}
			lock, err := d.Lock()
			if err != nil {
				t.Fatal(err)
			}
			res := b.run(t, b.endArgs("t1", "--force")...)
			if res.code != 1 || !strings.Contains(res.stderr, `"`+nick+`"`) || !strings.Contains(res.stderr, "running") {
				t.Fatalf("exit %d, want 1 naming %s\n%s", res.code, nick, res.stderr)
			}
			for _, still := range []string{wt, rec.ReplyDir, filepath.Join(b.base, "tables", "t1", "table.json")} {
				if _, err := os.Stat(still); err != nil {
					t.Errorf("a refused end removed %s: %v", still, err)
				}
			}
			// A refusal holds nothing: every other seat's lock is free again.
			for _, other := range rec.Seats {
				if other.Nick == nick {
					continue
				}
				free, err := state.LockNick(b.base, other.Nick)
				if err != nil {
					t.Fatalf("the refused end left seat %s's lock held: %v", other.Nick, err)
				}
				free.Release()
			}
			if err := lock.Release(); err != nil {
				t.Fatal(err)
			}
			mustTable(t, b, b.endArgs("t1")...)
			// And a finished end holds nothing either.
			for _, s := range rec.Seats {
				free, err := state.LockNick(b.base, s.Nick)
				if err != nil {
					t.Fatalf("the finished end left seat %s's lock held: %v", s.Nick, err)
				}
				free.Release()
			}
		})
	}
}

// TestTableEndWhenTheWorktreeIsAlreadyGone: the rest is still taken down and the
// target's registry is pruned.
func TestTableEndWhenTheWorktreeIsAlreadyGone(t *testing.T) {
	b := newTableBench(t)
	wt, rec := tableUp(t, b, "t1")
	if err := os.RemoveAll(wt); err != nil {
		t.Fatal(err)
	}
	// Another worktree of the target, unavailable for now: its registration is
	// not the table's to drop, so the end must not prune it away.
	other := filepath.Join(b.root, "other-wt")
	gitIn(t, b.repo, "worktree", "add", "--detach", other)
	if err := os.RemoveAll(other); err != nil {
		t.Fatal(err)
	}
	res := mustTable(t, b, b.endArgs("t1")...)
	if !strings.Contains(res.stdout, "worktree "+wt+" (already gone)") {
		t.Errorf("the worktree was not there to remove, yet the report does not say so:\n%s", res.stdout)
	}
	list := gitIn(t, b.repo, "worktree", "list", "--porcelain")
	if strings.Contains(list, "worktree "+wt+"\n") || strings.Count(list, "worktree ") != 2 {
		t.Errorf("want the target's checkout and the other worktree registered, the table's gone:\n%s", list)
	}
	if !strings.Contains(list, "other-wt") {
		t.Errorf("the end dropped another worktree's registration:\n%s", list)
	}
	if _, err := os.Stat(rec.ReplyDir); err == nil {
		t.Error("reply directory remains")
	}
}

// TestTableEndSaysWhichReplyDirectoryItRemovedAndWhichWasAlreadyGone: the report
// names only what this end removed. A reply directory that was there is listed
// as removed; one that was already gone is said to be, and the rest of the table
// is still taken down.
func TestTableEndSaysWhichReplyDirectoryItRemovedAndWhichWasAlreadyGone(t *testing.T) {
	t.Run("removed", func(t *testing.T) {
		b := newTableBench(t)
		_, rec := tableUp(t, b, "t1")
		res := mustTable(t, b, b.endArgs("t1")...)
		line := lineWith(t, res.stdout, "reply directory")
		if line != "reply directory "+rec.ReplyDir {
			t.Errorf("the reply directory was removed, yet the line is %q", line)
		}
		if _, err := os.Lstat(rec.ReplyDir); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s is still there (%v)", rec.ReplyDir, err)
		}
	})
	t.Run("already gone", func(t *testing.T) {
		b := newTableBench(t)
		_, rec := tableUp(t, b, "t1")
		if err := os.RemoveAll(rec.ReplyDir); err != nil {
			t.Fatal(err)
		}
		res := mustTable(t, b, b.endArgs("t1")...)
		line := lineWith(t, res.stdout, "reply directory")
		if line != "reply directory "+rec.ReplyDir+" (already gone)" {
			t.Errorf("the reply directory was not there to remove, yet the line is %q", line)
		}
		if _, err := os.Lstat(filepath.Join(b.base, "tables", "t1")); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the table directory is still there (%v): the rest was not taken down", err)
		}
	})
}

// TestTableEndRefusesARecordItDoesNotTrust: it deletes the paths the record
// names, so a record pointing anywhere else is not acted on.
func TestTableEndRefusesARecordItDoesNotTrust(t *testing.T) {
	b := newTableBench(t)
	_, rec := tableUp(t, b, "t1")
	victim := filepath.Join(b.root, "precious")
	if err := os.Mkdir(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	rec.ReplyDir = victim
	raw, _ := json.Marshal(rec)
	if err := os.WriteFile(filepath.Join(b.base, "tables", "t1", "table.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	res := b.run(t, b.endArgs("t1", "--force")...)
	if res.code == 0 || !strings.Contains(res.stderr, "not valid") {
		t.Fatalf("exit %d, want a refusal of the record\n%s", res.code, res.stderr)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Errorf("the directory the record named was removed: %v", err)
	}
}

func TestTableEndConfigurationErrors(t *testing.T) {
	b := newTableBench(t)
	for name, args := range map[string][]string{
		"no id":      {"table", "end"},
		"a bad id":   {"table", "end", "T1", "--state-dir", b.base},
		"no such":    {"table", "end", "nope", "--state-dir", b.base},
		"an extra":   {"table", "end", "t1", "extra", "--state-dir", b.base},
		"no verb":    {"table"},
		"bad verb":   {"table", "start"},
		"flags only": {"table", "new", "--rounds", "2"},
	} {
		if res := b.run(t, args...); res.code != 2 {
			t.Errorf("%s: exit %d, want 2\n%s", name, res.code, res.stderr)
		}
	}
}

// TestUsageDocumentsTheTableVerbs.
func TestUsageDocumentsTheTableVerbs(t *testing.T) {
	top := mustRun(t, "", "-h").stdout
	for _, want := range []string{"parley table   new <id>", "--seat <nick>=<claude|codex>[:<model>]", "parley table   end <id> [--force]",
		"table    set a table up", "codex delete line for each Codex seat and never runs one", "no opencode seats yet", "tables/<id>/table.json", "Tables live in <dir>/tables/<id>/",
		"no whitespace in the state directory", "no .codex that is not a", "the parley-table skill under", "holds the marker new wrote",
		"before it takes a lock or removes anything", "the digest of the record's", "so those lines say plain codex", "takes every seat's lock and holds them to the end", "a hangup (the terminal", "PLAIN among the"} {
		if !strings.Contains(top, want) {
			t.Errorf("usage does not mention %q", want)
		}
	}
	for _, verb := range []string{"new", "end"} {
		help := runParley(t, "", "table", verb, "-h")
		if help.code != 0 {
			t.Errorf("table %s -h exited %d", verb, help.code)
		}
		if !strings.Contains(help.stdout, "parley table "+verb) || help.stderr != "" {
			t.Errorf("table %s -h prints %q on stdout, %q on stderr", verb, help.stdout, help.stderr)
		}
	}
	if help := mustRun(t, "", "table", "-h"); !strings.Contains(help.stdout, "parley table new") || !strings.Contains(help.stdout, "parley table end") {
		t.Errorf("table -h prints %q", help.stdout)
	}
}

// TestTableNewWritesOnlyTheConfigsItsSeatsNeed: a table of one harness has the
// other's config nowhere, and a Claude-only table never calls codex.
func TestTableNewWritesOnlyTheConfigsItsSeatsNeed(t *testing.T) {
	t.Run("claude only", func(t *testing.T) {
		b := newTableBench(t)
		res := mustTable(t, b, b.newArgs("t1", []string{"claude-a=claude", "claude-b=claude"})...)
		wt := filepath.Join(b.base, "tables", "t1", "worktree")
		if _, err := os.Stat(filepath.Join(wt, ".mcp.json")); err != nil {
			t.Errorf("no .mcp.json: %v", err)
		}
		if _, err := os.Stat(filepath.Join(wt, ".codex")); err == nil {
			t.Error("a Claude-only table has a .codex directory")
		}
		if n := b.callCount(); n != 0 {
			t.Errorf("a Claude-only table called codex %d times", n)
		}
		if strings.Contains(res.stdout, "Codex") || strings.Contains(res.stdout, "codex resume") {
			t.Errorf("a Claude-only table prints Codex steps:\n%s", res.stdout)
		}
		// Two seats, three rounds.
		if !strings.Contains(string(mustRead(t, filepath.Join(wt, ".mcp.json"))), `"6"`) {
			t.Errorf(".mcp.json does not carry cap 6")
		}
		lineWith(t, res.stdout, "PARLEY_NICK=claude-a claude")
		lineWith(t, res.stdout, "PARLEY_NICK=claude-b claude")
	})
	t.Run("codex only", func(t *testing.T) {
		b := newTableBench(t)
		// No claude on the machine at all is fine for a table that has none.
		res := runParleyEnvIn(t, "", b.envWithPath(b.pathWith(t, "codex", "ergo", "git", "cat", "mv")), b.newArgs("t1", []string{"codex-a=codex"})...)
		if res.code != 0 {
			t.Fatalf("exit %d\n%s", res.code, res.stderr)
		}
		wt := filepath.Join(b.base, "tables", "t1", "worktree")
		if _, err := os.Stat(filepath.Join(wt, ".mcp.json")); err == nil {
			t.Error("a Codex-only table has a .mcp.json")
		}
		if _, err := os.Stat(filepath.Join(wt, ".codex", "config.toml")); err != nil {
			t.Errorf("no .codex/config.toml: %v", err)
		}
		if strings.Contains(res.stdout, "Claude seats") {
			t.Errorf("a Codex-only table prints Claude steps:\n%s", res.stdout)
		}
	})
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestTableNewMakesTheWorktreeWhereItIsToldAndEndRemovesIt.
func TestTableNewMakesTheWorktreeWhereItIsToldAndEndRemovesIt(t *testing.T) {
	b := newTableBench(t)
	wt := filepath.Join(b.root, "shared")
	res := mustTable(t, b, b.newArgs("t1", nil, "--worktree", wt)...)
	if _, err := os.Stat(filepath.Join(wt, ".mcp.json")); err != nil {
		t.Fatalf("no .mcp.json in the worktree asked for: %v", err)
	}
	if !strings.Contains(string(mustRead(t, filepath.Join(wt, ".mcp.json"))), wt) {
		t.Error(".mcp.json does not name the worktree asked for")
	}
	lineWith(t, res.stdout, "cd '"+wt+"' && PARLEY_NICK=claude-a")
	if _, err := os.Stat(filepath.Join(b.base, "tables", "t1", "worktree")); err == nil {
		t.Error("a worktree was also made at the default place")
	}
	mustTable(t, b, b.endArgs("t1")...)
	if _, err := os.Stat(wt); err == nil {
		t.Error("table end left the worktree it was told of")
	}
}

// TestTableEndWhenTheTargetIsGone: with the repo deleted there is no registry to
// leave, so the rest is still taken down.
func TestTableEndWhenTheTargetIsGone(t *testing.T) {
	b := newTableBench(t)
	wt, rec := tableUp(t, b, "t1")
	if err := os.RemoveAll(b.repo); err != nil {
		t.Fatal(err)
	}
	// Without --force the worktree cannot even be looked at: its repo is gone.
	if res := b.run(t, b.endArgs("t1")...); res.code != 1 {
		t.Fatalf("exit %d, want 1 for a worktree that cannot be inspected\n%s", res.code, res.stderr)
	}
	mustTable(t, b, b.endArgs("t1", "--force")...)
	for _, gone := range []string{wt, rec.ReplyDir, filepath.Join(b.base, "tables", "t1")} {
		if _, err := os.Lstat(gone); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s is still there (%v)", gone, err)
		}
	}
}
