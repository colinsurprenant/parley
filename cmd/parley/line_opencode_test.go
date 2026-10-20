package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/colinsurprenant/parley/internal/state"
)

// fakeOpenCodeScript is an opencode that answers --version and the three --help
// texts like the 1.18.30 they were copied from, and records the argv of every
// other call in $d/call-<n>, with the directory it ran in (cwd-<n>), three
// variables of its environment (env-<n>) and, for run, its stdin (stdin-<n>). A
// record is written under a temporary name and renamed into place before the
// count is published, so a reader that sees the count at n finds call-n whole.
// run prints the events of a turn whose session is ocID(n) (or $d/id) unless
// $d/mode-<n> says otherwise; session delete succeeds.
const fakeOpenCodeScript = `#!/bin/sh
d=@D@
t=@T@
ver=1.18.30
[ -f "$d/version" ] && ver=$(cat "$d/version")
case "$1 $2" in
"--version "*) echo "$ver"; exit 0 ;;
"--help ") cat "$t/opencode-1.18.30-tui.help" >&2; exit 0 ;;
"run --help") cat "$t/opencode-1.18.30-run.help" >&2; exit 0 ;;
"session --help") cat "$t/opencode-1.18.30-session.help" >&2; exit 0 ;;
"session list") if [ "$3" = --help ]; then cat "$t/opencode-1.18.30-session-list.help" >&2; exit 0; fi ;;
esac
n=$(cat "$d/count" 2>/dev/null || echo 0)
n=$((n+1))
{ for a in "$@"; do printf '%s\0' "$a"; done; } > "$d/.call-$n.tmp"
mv "$d/.call-$n.tmp" "$d/call-$n"
pwd > "$d/cwd-$n"
printf 'OPENCODE=%s\nOPENCODE_PID=%s\nAGENT=%s\n' "${OPENCODE-unset}" "${OPENCODE_PID-unset}" "${AGENT-unset}" > "$d/env-$n"
echo "$n" > "$d/.count.tmp"
mv "$d/.count.tmp" "$d/count"
mode=ok
[ -f "$d/mode-$n" ] && mode=$(cat "$d/mode-$n")
id=ses_$(printf '%026d' "$n")
[ -f "$d/id" ] && id=$(cat "$d/id")
start() { printf '{"type":"step_start","timestamp":1,"sessionID":"%s","part":{"id":"prt_1","type":"step-start"}}\n' "$1"; }
text() { printf '{"type":"text","timestamp":2,"sessionID":"%s","part":{"id":"prt_2","type":"text","text":"%s"}}\n' "$1" "$2"; }
finish() { printf '{"type":"step_finish","timestamp":3,"sessionID":"%s","part":{"id":"prt_3","type":"step-finish","reason":"%s"}}\n' "$1" "$2"; }
err() { printf '{"type":"error","timestamp":4,"sessionID":"%s","error":{"name":"UnknownError","data":{"message":"%s"}}}\n' "$1" "$2"; }
case "$1" in
run)
	cat > "$d/stdin-$n"
	case "$mode" in
	fail) echo "boom: not logged in" >&2; exit 3 ;;
	stray)
		title=; prev=
		for a in "$@"; do [ "$prev" = --title ] && title=$a; prev=$a; done
		now=$(( $(date +%s) * 1000 + 999 ))
		here=$(pwd)
		# The session the run made, and look-alikes with its exact title that are not
		# it: another directory of the project, made before the run, and ids that
		# are not session ids.
		{
			printf '{"id":"ses_00000000000000000000stray1","title":"%s","directory":"%s","projectId":"%s","created":%s}\n' "$title" "$here" "$here" "$now"
			printf '{"id":"ses_00000000000000000000elsewh","title":"%s","directory":"/elsewhere/co","projectId":"%s","created":%s}\n' "$title" "$here" "$now"
			printf '{"id":"ses_00000000000000000000older1","title":"%s","directory":"%s","projectId":"%s","created":1000}\n' "$title" "$here" "$here"
			printf '{"id":"ses_short","title":"%s","directory":"%s","projectId":"%s","created":%s}\n' "$title" "$here" "$here" "$now"
			printf '{"id":"--force","title":"%s","directory":"%s","projectId":"%s","created":%s}\n' "$title" "$here" "$here" "$now"
		} > "$d/stray"
		exit 3 ;;
	nosession)
		echo '{"type":"step_start","timestamp":1,"part":{"type":"step-start"}}'
		echo '{"type":"step_finish","timestamp":3,"part":{"reason":"stop"}}'
		exit 0 ;;
	badid) id=not-an-id ;;
	shortid) id=ses_abc ;;
	dashid) id=--force ;;
	mismatch)
		start "$id"; text "$id" ready
		finish "ses_$(printf '%026d' 99)" stop
		exit 0 ;;
	unfinished)
		start "$id"; text "$id" ready; finish "$id" stop
		start "$id"
		exit 0 ;;
	orphan)
		# The launcher ends the turn and exits at once, leaving a child that holds
		# the pipes.
		sleep 300 &
		echo $! > "$d/child-pid"
		start "$id"; text "$id" ready; finish "$id" stop
		exit 0 ;;
	mismatchfail)
		start "$id"; text "$id" ready
		finish "ses_$(printf '%026d' 99)" stop
		echo "bad turn" >&2; exit 1 ;;
	mixednoid)
		start "$id"; text "$id" ready
		echo '{"type":"step_finish","timestamp":3,"part":{"reason":"stop"}}'
		exit 0 ;;
	noidfirst)
		echo '{"type":"step_start","timestamp":1,"part":{"type":"step-start"}}'
		text "$id" ready; finish "$id" stop
		exit 0 ;;
	failafter) start "$id"; err "$id" "the model went away"; echo "bad turn" >&2; exit 1 ;;
	errorzero) start "$id"; err "$id" "stream broke"; finish "$id" stop; exit 0 ;;
	ctrlerror)
		start "$id"
		printf '%s\n' '{"type":"error","sessionID":"'"$id"'","error":{"name":"UnknownError","data":{"message":"quota\u001b[31m exceeded\u0007"}}}'
		echo "bad" >&2; exit 1 ;;
	nostop) start "$id"; text "$id" ready; finish "$id" tool-calls; exit 0 ;;
	nostep) start "$id"; text "$id" ready; exit 0 ;;
	multistep)
		start "$id"; text "$id" "let me look"; finish "$id" tool-calls
		start "$id"; text "$id" "ready, waiting"; finish "$id" stop
		exit 0 ;;
	overflow)
		start "$id"
		printf '{"type":"text","sessionID":"%s","part":{"type":"text","text":"' "$id"
		head -c 1200000 /dev/zero | tr '\0' x
		printf '"}}\n'
		finish "$id" stop
		exit 0 ;;
	hold)
		sleep 300 &
		echo $! > "$d/child-pid"
		start "$id"
		: > "$d/announced"
		wait
		exit 0 ;;
	esac
	start "$id"
	if [ -f "$d/answer" ]; then text "$id" "$(cat "$d/answer")"; else text "$id" "ready, waiting for the first delivery"; fi
	finish "$id" stop
	exit 0 ;;
session)
	if [ "$mode" = holdcmd ]; then
		sleep 300 &
		echo $! > "$d/child-pid-$n"
		wait
		exit 0
	fi
	case "$2" in
	delete)
		if [ "$mode" = faildelete ]; then echo "cannot delete" >&2; exit 5; fi
		exit 0 ;;
	list)
		if [ "$mode" = faillist ]; then echo "db locked" >&2; exit 4; fi
		# The sessions of the project of the directory it runs in (a test's entries
		# name their project), newest first (the files are in that order, one JSON
		# object a line), at most -n of them.
		n=20; prev=
		for a in "$@"; do [ "$prev" = -n ] && n=$a; prev=$a; done
		out=$({ cat "$d/newer" "$d/stray" "$d/decoys"; } 2>/dev/null | grep -F "\"projectId\":\"$(pwd)\"" | head -n "$n" | paste -s -d ',' -)
		printf '[%s]\n' "$out"
		exit 0 ;;
	esac ;;
esac
exit 0
`

// ocID is the session id the fake opencode gives call n.
func ocID(n int) string { return fmt.Sprintf("ses_%026d", n) }

// ocLineBench is a lineBench whose opencode is the recording fake.
type ocLineBench struct {
	*lineBench
	oc string
}

func newOCLineBench(t *testing.T) *ocLineBench {
	t.Helper()
	b := &ocLineBench{lineBench: newLineBench(t)}
	b.oc = filepath.Join(b.root, "oc")
	if err := os.Mkdir(b.oc, 0o755); err != nil {
		t.Fatal(err)
	}
	testdata, err := filepath.Abs(filepath.Join("..", "..", "internal", "doctor", "testdata"))
	if err != nil {
		t.Fatal(err)
	}
	script := strings.NewReplacer("@D@", strconv.Quote(b.oc), "@T@", strconv.Quote(testdata)).Replace(fakeOpenCodeScript)
	// The bench's PATH already holds an opencode, the doctor bench's; this one
	// answers the same questions and records the rest.
	if err := os.WriteFile(b.db.opencode, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return b
}

func (b *ocLineBench) setMode(t *testing.T, n int, mode string) {
	t.Helper()
	b.write(t, fmt.Sprintf("mode-%d", n), mode)
}

func (b *ocLineBench) write(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(b.oc, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (b *ocLineBench) callCount() int {
	raw, err := os.ReadFile(filepath.Join(b.oc, "count"))
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	return n
}

func (b *ocLineBench) call(t *testing.T, n int) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(b.oc, fmt.Sprintf("call-%d", n)))
	if err != nil {
		t.Fatalf("the fake opencode was not called %d times: %v", n, err)
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
}

func (b *ocLineBench) file(t *testing.T, name string) string {
	t.Helper()
	return string(mustRead(t, filepath.Join(b.oc, name)))
}

// calls is every call the fake opencode took, as one line each.
func (b *ocLineBench) calls(t *testing.T) []string {
	t.Helper()
	var out []string
	for n := 1; n <= b.callCount(); n++ {
		out = append(out, strings.Join(b.call(t, n), " "))
	}
	return out
}

// snap is the lineBench snapshot, less the count of codex calls, which an
// OpenCode test has no use for.
func (b *ocLineBench) snap(t *testing.T, dirs ...string) string {
	t.Helper()
	return withoutCalls(b.lsnap(t, dirs...))
}

func (b *ocLineBench) wantNoOpenCodeCalls(t *testing.T) {
	t.Helper()
	if n := b.callCount(); n != 0 {
		t.Errorf("opencode was called %d times (%v), want 0", n, b.calls(t))
	}
}

// listCalls is how many times the fake was asked to list sessions.
func (b *ocLineBench) listCalls(t *testing.T) int {
	t.Helper()
	n := 0
	for _, c := range b.calls(t) {
		if strings.HasPrefix(c, "session list ") {
			n++
		}
	}
	return n
}

// wantDeletes fails unless the calls after the runs are exactly these session
// deletes, the lists of sessions a failed run is followed by left out.
func (b *ocLineBench) wantDeletes(t *testing.T, runs int, ids ...string) {
	t.Helper()
	var got []string
	for _, c := range b.calls(t) {
		if !strings.HasPrefix(c, "session list ") {
			got = append(got, c)
		}
	}
	if len(got) != runs+len(ids) {
		t.Fatalf("opencode was called %d times (%q), want %d run(s) then %d delete(s)", len(got), got, runs, len(ids))
	}
	for i, id := range ids {
		if want := "session delete " + id; got[runs+i] != want {
			t.Errorf("call %d is %q, want %q", runs+i+1, got[runs+i], want)
		}
	}
	for i := 0; i < runs; i++ {
		if !strings.HasPrefix(got[i], "run ") {
			t.Errorf("call %d is %q, want a run", i+1, got[i])
		}
	}
}

// ocPortRE finds the port the runbook's TUI line names.
var ocPortRE = regexp.MustCompile(` --port (\d+) --session `)

func ocPortOf(t *testing.T, runbook string) int {
	t.Helper()
	m := ocPortRE.FindStringSubmatch(lineWith(t, runbook, " --port ", " --session ", "ses_"))
	if m == nil {
		t.Fatalf("no TUI line in the runbook:\n%s", runbook)
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// TestLineAddOpenCodeSession is the acceptance run for an OpenCode session on the
// fake opencode: nothing written in the checkout, the reply directory and marker,
// the session started the way the design says, the records and what is printed.
func TestLineAddOpenCodeSession(t *testing.T) {
	b := newOCLineBench(t)
	lineUp(t, b.lineBench, "demo")
	co := b.gitCheckout(t, "co-build")
	treeBefore := treeListing(t, co)
	env := append(b.env(), "OPENCODE=1", "OPENCODE_PID=4242", "AGENT=1")
	res := runParleyEnvIn(t, "", env, b.ladd("demo", "build", "opencode", co, "--model", "anthropic/claude-sonnet-4-5")...)
	if res.code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", res.code, res.stdout, res.stderr)
	}
	t.Logf("stdout:\n%s", res.stdout)
	t.Logf("stderr:\n%s", res.stderr)
	stream := b.stream("demo")

	// Nothing is written in the checkout: no launch file, no .codex, no exclude
	// line, and a git status that is clean.
	if after := treeListing(t, co); after != treeBefore {
		t.Errorf("the checkout changed.\nbefore:\n%s\nafter:\n%s", treeBefore, after)
	}
	for _, name := range []string{".mcp.json", ".codex"} {
		if _, err := os.Lstat(filepath.Join(co, name)); err == nil {
			t.Errorf("an OpenCode session got a %s", name)
		}
	}
	if got := gitIn(t, co, "status", "--porcelain"); got != "" {
		t.Errorf("the checkout is not clean: %s", got)
	}

	rec := b.record(t, "demo")
	s := sessionByNick(t, rec, "build")
	if s.Harness != "opencode" || s.Model != "anthropic/claude-sonnet-4-5" || s.Checkout != co || s.Thread != ocID(1) || len(s.Files) != 0 {
		t.Errorf("session %+v", s)
	}
	raw := string(mustRead(t, filepath.Join(b.base, "lines", "demo", "line.json")))
	if !strings.Contains(raw, `"files": []`) || strings.Contains(raw, "opencode_bin") {
		t.Errorf("line.json holds the empty file list as an empty list, and no opencode_bin the line was not given:\n%s", raw)
	}
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

	// The session record is where --session auto looks.
	sr, err := state.OpenSessionRecord(b.base, "build", stream)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := sr.Read(); err != nil || got != ocID(1) {
		t.Errorf("session record = %q, %v; want %s", got, err, ocID(1))
	}
	if fi, err := os.Stat(sr.Path()); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("session record mode %v (%v), want 0600", fi.Mode(), err)
	}
	if tr, err := state.OpenThreadRecord(b.base, "build", stream); err != nil {
		t.Fatal(err)
	} else if _, err := os.Lstat(tr.Path()); err == nil {
		t.Error("an OpenCode session got a Codex thread record")
	}

	// opencode was asked once, to run, in the checkout, with the primer on its
	// stdin and not on its command line, and with the caller's OPENCODE variables
	// left as they were.
	b.wantDeletes(t, 1)
	argv := b.call(t, 1)
	// The title carries the token of the reply directory, which is this run's own.
	token := strings.TrimPrefix(filepath.Base(s.ReplyDir), "parley-line-demo-")
	if token == "" || token == filepath.Base(s.ReplyDir) {
		t.Fatalf("no token in the reply directory %s", s.ReplyDir)
	}
	if got, want := strings.Join(argv, "\x00"), strings.Join([]string{"run", "--format", "json", "--title", "parley line demo build " + token, "-m", "anthropic/claude-sonnet-4-5"}, "\x00"); got != want {
		t.Errorf("opencode argv %q, want %q", argv, strings.Split(want, "\x00"))
	}
	if got := strings.TrimSpace(b.file(t, "cwd-1")); got != co {
		t.Errorf("opencode ran in %q, want the checkout %q", got, co)
	}
	if got := b.file(t, "env-1"); got != "OPENCODE=1\nOPENCODE_PID=4242\nAGENT=1\n" {
		t.Errorf("opencode saw %q of the caller's OPENCODE variables, want them passed on as they were", got)
	}
	prompt := b.file(t, "stdin-1")
	for _, want := range []string{"colin", stream, filepath.Join(s.ReplyDir, "build.reply"), "the parley-line skill, which you load with the skill tool", binPath, b.base,
		"--nick build", "more than 8 posts within 10m", "external_directory"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the primer does not hold %q:\n%s", want, prompt)
		}
	}
	if !strings.HasPrefix(prompt, "This primer is your first message") || !strings.HasSuffix(prompt, "\n") {
		t.Errorf("the primer on stdin is not the rendered text:\n%q", prompt)
	}
	for _, a := range argv {
		if strings.Contains(a, "This primer") {
			t.Errorf("the primer is on the command line: %q", a)
		}
	}

	port := ocPortOf(t, res.stdout)
	pump := fmt.Sprintf("'%s' opencode --stream '%s' --nick build --session auto --port %d --skill line --chair colin --cap 8 --window 10m --state-dir '%s'", binPath, stream, port, b.base)
	tui := fmt.Sprintf("opencode --port %d --session '%s'", port, ocID(1))
	wantInOrder(t, res.stdout, []string{
		"Session build (opencode) is added to line demo, checkout " + co + ".",
		"Wrote nothing in the checkout.",
		"OpenCode session " + ocID(1) + ".",
		"What is left is yours, in this order:",
		"cd '" + co + "'",
		tui,
		pump,
		"Notes:",
		fmt.Sprintf("The reply file, '%s', is outside the session's checkout, so each write of it raises OpenCode's external_directory ask in the TUI:", filepath.Join(s.ReplyDir, "build.reply")),
		`answer "Allow always" for the reply directory, since "Allow once" asks again on every reply; "always" asks once more after the TUI restarts.`,
		"The session was made by opencode run, which denies it OpenCode's question tool and plan mode for that session (a known limit of creating it that way).",
		"OPENCODE_SERVER_PASSWORD, if you use one, must be set for both commands, and OPENCODE_SERVER_USERNAME too if it is not opencode.",
		fmt.Sprintf("Session build's answer to its primer (opencode run, session %s):", ocID(1)),
		"ready, waiting for the first delivery",
	})
	for _, unwanted := range []string{"Network:", "codex", "/mcp", "$parley-line", "Wrote /"} {
		if strings.Contains(res.stdout, unwanted) {
			t.Errorf("the runbook mentions %q, which is not an OpenCode session's:\n%s", unwanted, res.stdout)
		}
	}
	if !strings.Contains(res.stderr, "starting OpenCode session build") || !strings.Contains(res.stderr, "session build is OpenCode session "+ocID(1)) {
		t.Errorf("stderr:\n%s", res.stderr)
	}
}

// TestLineAddOpenCodeAnswersWithTheLastStep: the answer printed is the text of the
// turn's last step, not of the steps before it.
func TestLineAddOpenCodeAnswersWithTheLastStep(t *testing.T) {
	b := newOCLineBench(t)
	lineUp(t, b.lineBench, "demo")
	b.setMode(t, 1, "multistep")
	res := mustLine(t, b.lineBench, b.ladd("demo", "build", "opencode", b.gitCheckout(t, "co"))...)
	if !strings.Contains(res.stdout, "ready, waiting") || strings.Contains(res.stdout, "let me look") {
		t.Errorf("stdout:\n%s", res.stdout)
	}
}

// TestLineAddOpenCodeWorksInACheckoutThatIsNoGitCheckout: a plain directory is
// fine, and nothing is written in it.
func TestLineAddOpenCodeWorksInACheckoutThatIsNoGitCheckout(t *testing.T) {
	b := newOCLineBench(t)
	lineUp(t, b.lineBench, "demo")
	co := b.plainCheckout(t, "co")
	mustLine(t, b.lineBench, b.ladd("demo", "build", "opencode", co)...)
	if ents, err := os.ReadDir(co); err != nil || len(ents) != 0 {
		t.Errorf("the checkout holds %v (%v), want it empty", ents, err)
	}
}

// TestLineAddOpenCodeBinIsTheLinesOwn: a line made with --opencode-bin runs that
// opencode, and says so in the TUI line, the record and the delete line a drop
// prints.
func TestLineAddOpenCodeBinIsTheLinesOwn(t *testing.T) {
	b := newOCLineBench(t)
	custom := filepath.Join(b.root, "bin")
	if err := os.Mkdir(custom, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(custom, "oc-custom")
	if err := os.WriteFile(bin, mustRead(t, b.db.opencode), 0o755); err != nil {
		t.Fatal(err)
	}
	res := mustLine(t, b.lineBench, b.lnew("demo", "--opencode-bin", bin)...)
	t.Logf("%s", res.stdout)
	if got := b.record(t, "demo").OpenCodeBin; got != bin {
		t.Fatalf("opencode_bin = %q, want %q", got, bin)
	}
	// The default opencode is out of reach: the line's own is the only one used.
	env := b.envWithPath(b.pathWith(t, "claude", "codex", "ergo", "git", "cat", "mv"))
	co := b.gitCheckout(t, "co")
	add := runParleyEnvIn(t, "", env, b.ladd("demo", "build", "opencode", co)...)
	if add.code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", add.code, add.stdout, add.stderr)
	}
	lineWith(t, add.stdout, "'"+bin+"' --port ", " --session '"+ocID(1)+"'")
	drop := runParleyEnvIn(t, "", env, b.ldrop("demo", "build")...)
	if drop.code != 0 {
		t.Fatalf("drop exit %d\n%s%s", drop.code, drop.stdout, drop.stderr)
	}
	lineWith(t, drop.stdout, "'"+bin+"' session delete '"+ocID(1)+"'", "# session build")
}

// TestLineNewOpenCodeBin: --opencode-bin is resolved and recorded as an absolute
// path, or refused.
func TestLineNewOpenCodeBin(t *testing.T) {
	b := newOCLineBench(t)
	res := mustLine(t, b.lineBench, b.lnew("demo", "--opencode-bin", "opencode")...)
	if got := b.record(t, "demo").OpenCodeBin; got != b.db.opencode {
		t.Errorf("opencode_bin = %q, want the resolved %q", got, b.db.opencode)
	}
	lineWith(t, res.stdout, "--harness <claude|codex|opencode>")
	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{"empty", []string{"--opencode-bin", ""}, "must not be empty"},
		{"missing", []string{"--opencode-bin", filepath.Join(b.root, "nope")}, "--opencode-bin"},
		{"not UTF-8", []string{"--opencode-bin", "bad\xffname"}, "not valid UTF-8"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := b.run(t, b.lnew("other", c.args...)...)
			if r.code != 2 || !strings.Contains(r.stderr, c.want) {
				t.Errorf("exit %d, want 2 saying %q\n%s", r.code, c.want, r.stderr)
			}
			if _, err := os.Lstat(filepath.Join(b.base, "lines", "other")); err == nil {
				t.Error("a refused line new left the line behind")
			}
		})
	}
}

// TestLineAddOpenCodeRefusesBeforeAnyChange: each refusal that is an OpenCode
// session's own leaves the base, the checkout and $TMPDIR as they were, with
// opencode never run.
func TestLineAddOpenCodeRefusesBeforeAnyChange(t *testing.T) {
	cases := []struct {
		name  string
		code  int
		want  string
		setup func(t *testing.T, b *ocLineBench, co string) (args, env []string)
	}{
		{"a temp directory inside the checkout", 2, "inside the checkout", func(t *testing.T, b *ocLineBench, co string) ([]string, []string) {
			tmp := filepath.Join(co, "tmp")
			if err := os.Mkdir(tmp, 0o755); err != nil {
				t.Fatal(err)
			}
			return b.ladd("demo", "build", "opencode", co), []string{"TMPDIR=" + tmp}
		}},
		{"a state directory inside the checkout", 2, "inside the checkout", func(t *testing.T, b *ocLineBench, co string) ([]string, []string) {
			base := filepath.Join(co, "state")
			mustLine(t, b.lineBench, "line", "new", "demo2", "--chair", "colin", "--cap", "8", "--state-dir", base, "--server", b.db.server)
			return []string{"line", "add", "demo2", "--nick", "build", "--harness", "opencode", "--checkout", co, "--state-dir", base}, nil
		}},
		{"a nick with a session record of its own", 2, "already has an OpenCode session record", func(t *testing.T, b *ocLineBench, co string) ([]string, []string) {
			sr, err := state.OpenSessionRecord(b.base, "build", b.stream("demo"))
			if err != nil {
				t.Fatal(err)
			}
			if err := sr.Write(ocID(9)); err != nil {
				t.Fatal(err)
			}
			return b.ladd("demo", "build", "opencode", co), nil
		}},
		{"a nick with a Codex thread record, for an OpenCode session", 2, "already has a Codex thread record", func(t *testing.T, b *ocLineBench, co string) ([]string, []string) {
			tr, err := state.OpenThreadRecord(b.base, "build", b.stream("demo"))
			if err != nil {
				t.Fatal(err)
			}
			if err := tr.Write(fakeThread(9)); err != nil {
				t.Fatal(err)
			}
			return b.ladd("demo", "build", "opencode", co), nil
		}},
		{"a nick that is already in the line", 2, "already session", func(t *testing.T, b *ocLineBench, co string) ([]string, []string) {
			mustLine(t, b.lineBench, b.ladd("demo", "build", "opencode", b.gitCheckout(t, "co-other"))...)
			return b.ladd("demo", "BUILD", "opencode", co), nil
		}},
		{"a model that is not provider/model", 2, "provider/model", func(t *testing.T, b *ocLineBench, co string) ([]string, []string) {
			return b.ladd("demo", "build", "opencode", co, "--model", "sonnet"), nil
		}},
		{"a checkout that is not UTF-8", 2, "not valid UTF-8", func(t *testing.T, b *ocLineBench, co string) ([]string, []string) {
			return b.ladd("demo", "build", "opencode", co+"/bad\xffname"), nil
		}},
		{"no opencode on the machine", 2, "opencode-ai", func(t *testing.T, b *ocLineBench, co string) ([]string, []string) {
			return b.ladd("demo", "build", "opencode", co), []string{"PATH=" + b.pathWith(t, "claude", "codex", "ergo", "git", "cat", "mv")}
		}},
		{"an OpenCode 2", 1, "OpenCode 2 is not supported yet", func(t *testing.T, b *ocLineBench, co string) ([]string, []string) {
			b.write(t, "version", "opencode v2.0.32")
			return b.ladd("demo", "build", "opencode", co), nil
		}},
		{"no parley-line skill under either root", 1, "parley-line", func(t *testing.T, b *ocLineBench, co string) ([]string, []string) {
			for _, root := range []string{".claude", ".agents"} {
				if err := os.Remove(filepath.Join(b.db.home, root, "skills", "parley-line")); err != nil {
					t.Fatal(err)
				}
			}
			return b.ladd("demo", "build", "opencode", co), nil
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := newOCLineBench(t)
			lineUp(t, b.lineBench, "demo")
			co := b.gitCheckout(t, "co")
			args, extra := c.setup(t, b, co)
			before := b.snap(t, b.checkoutDirs(t)...)
			calls := b.callCount()
			res := runParleyEnvIn(t, "", append(b.env(), extra...), args...)
			if res.code != c.code {
				t.Fatalf("exit %d, want %d\nstdout: %s\nstderr: %s", res.code, c.code, res.stdout, res.stderr)
			}
			if !strings.Contains(res.stderr, c.want) {
				t.Errorf("stderr does not say %q:\n%s", c.want, res.stderr)
			}
			if res.stdout != "" {
				t.Errorf("a refused run printed on stdout: %q", res.stdout)
			}
			if got := b.snap(t, b.checkoutDirs(t)...); got != before {
				t.Fatalf("the run left a trace.\nbefore:\n%s\nafter:\n%s", before, got)
			}
			if b.callCount() != calls {
				t.Errorf("opencode was run: %v", b.calls(t))
			}
		})
	}
}

// TestLineAddOpenCodeReadsEitherSkillRoot: OpenCode reads both skill roots, so
// the line skill under one of them is enough.
func TestLineAddOpenCodeReadsEitherSkillRoot(t *testing.T) {
	for _, root := range []string{".claude", ".agents"} {
		t.Run("only "+root, func(t *testing.T) {
			b := newOCLineBench(t)
			lineUp(t, b.lineBench, "demo")
			gone := ".agents"
			if root == ".agents" {
				gone = ".claude"
			}
			if err := os.RemoveAll(filepath.Join(b.db.home, gone)); err != nil {
				t.Fatal(err)
			}
			mustLine(t, b.lineBench, b.ladd("demo", "build", "opencode", b.gitCheckout(t, "co"))...)
		})
	}
}

// TestLineAddOpenCodeFailureRollsEverythingBack: an opencode that fails, announces
// no session or a bad one, or ends without a completed turn is exit 1 and a
// machine as it was, the checkout, the record and $TMPDIR included, with the
// session it did make deleted.
func TestLineAddOpenCodeFailureRollsEverythingBack(t *testing.T) {
	cases := []struct {
		name string
		mode string
		want string
		// deleted is whether a delete of the session of run call 1 is made, as call 2.
		deleted bool
		// named is whether the run's events named a session, which is the only case
		// in which the list of sessions is not asked for one the run may have made.
		named bool
	}{
		{"opencode exits nonzero", "fail", "boom: not logged in", false, false},
		{"no sessionID in any event", "nosession", "no event carries a sessionID", false, false},
		{"a session id that is not one", "badid", "is not an OpenCode session id", false, true},
		{"a session id that is too short", "shortid", "is not an OpenCode session id", false, true},
		{"a session id that begins with a dash", "dashid", "is not an OpenCode session id", false, true},
		// Output that names two sessions does not say which the run made: none is
		// deleted, and the error names both.
		{"events that carry two session ids", "mismatch", "more than one session id", false, true},
		{"events that carry two session ids, and the run fails", "mismatchfail", "more than one session id", false, true},
		// A step_finish that names no session certifies nothing, but the id the other
		// events announced is the run's and is deleted.
		{"a step_finish that names no session, after others did", "mixednoid", "event(s) of type step_finish carry no sessionID", true, true},
		{"a step_start that names no session, before others did", "noidfirst", "event(s) of type step_start carry no sessionID", true, true},
		{"the run fails after the session exists", "failafter", "the model went away", true, true},
		{"exit 0 but an error event", "errorzero", "stream broke", true, true},
		// A step_finish(stop) followed by a step that began and never finished: the
		// last step is not done.
		{"a last step that never finished", "unfinished", "never finished", true, true},
		{"a last step that is not a stop", "nostop", `reason "tool-calls"`, true, true},
		{"no step_finish at all", "nostep", "no step_finish event", true, true},
		{"stdout past the cap", "overflow", "cut off", true, true},
		{"control characters in the error", "ctrlerror", "UnknownError: quota[31m exceeded", true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := newOCLineBench(t)
			lineUp(t, b.lineBench, "demo")
			co := b.gitCheckout(t, "co")
			b.setMode(t, 1, c.mode)
			before := b.snap(t, co)
			res := b.run(t, b.ladd("demo", "build", "opencode", co)...)
			if res.code != 1 {
				t.Fatalf("exit %d, want 1\nstdout: %s\nstderr: %s", res.code, res.stdout, res.stderr)
			}
			if !strings.Contains(res.stderr, c.want) {
				t.Errorf("stderr does not say %q:\n%s", c.want, res.stderr)
			}
			if strings.ContainsAny(res.stderr, "\x1b\a") {
				t.Errorf("stderr carries a control character: %q", res.stderr)
			}
			if !strings.Contains(res.stderr, "rolled back") || !strings.Contains(res.stderr, "reply directory") {
				t.Errorf("stderr does not say what was rolled back:\n%s", res.stderr)
			}
			if res.stdout != "" {
				t.Errorf("a failed run printed on stdout: %q", res.stdout)
			}
			if c.deleted {
				b.wantDeletes(t, 1, ocID(1))
				if !strings.Contains(res.stderr, "OpenCode session "+ocID(1)+" (session build)") {
					t.Errorf("the rollback does not name the session it deleted:\n%s", res.stderr)
				}
				if got := strings.TrimSpace(b.file(t, "cwd-2")); got != co {
					t.Errorf("the delete ran in %q, want the checkout %q", got, co)
				}
			} else {
				b.wantDeletes(t, 1)
			}
			// A run that named no session is looked for in the list, in case it made one.
			wantListed := 1
			if c.named {
				wantListed = 0
			}
			if n := b.listCalls(t); n != wantListed {
				t.Errorf("sessions were listed %d times, want %d", n, wantListed)
			}
			// A list that ran and found nothing is logged, so the silence is not mistaken
			// for a list that did not run.
			if !c.named {
				if want := "holds none titled"; !strings.Contains(res.stderr, want) || !strings.Contains(res.stderr, "so there is nothing to delete") {
					t.Errorf("stderr does not say the list found nothing (%q):\n%s", want, res.stderr)
				}
				if got := strings.TrimSpace(b.file(t, "cwd-2")); got != co {
					t.Errorf("the list ran in %q, want the checkout %q", got, co)
				}
			}
			if got := b.snap(t, co); got != before {
				t.Fatalf("the run left a trace.\nbefore:\n%s\nafter:\n%s", before, got)
			}
		})
	}
}

// TestLineAddOpenCodeDeletesNothingOnConflictingIds: events that name two sessions
// do not say which one the run made, and nothing is deleted on a guess. The error
// names both ids and where to look.
func TestLineAddOpenCodeDeletesNothingOnConflictingIds(t *testing.T) {
	// The run exits 0 with the conflicting ids, and exits 1 with them: the error
	// names the ids, the title and where to look either way, and the second says the
	// run also failed.
	for _, mode := range []string{"mismatch", "mismatchfail"} {
		t.Run(mode, func(t *testing.T) {
			b := newOCLineBench(t)
			lineUp(t, b.lineBench, "demo")
			co := b.gitCheckout(t, "co")
			b.setMode(t, 1, mode)
			res := b.run(t, b.ladd("demo", "build", "opencode", co)...)
			if res.code != 1 {
				t.Fatalf("exit %d, want 1\n%s", res.code, res.stderr)
			}
			want := []string{ocID(1), ocID(99), "none is deleted", "opencode session list, run in " + co, `for the title "parley line demo build `}
			if mode == "mismatchfail" {
				want = append(want, "the run also failed: exit status 1: bad turn")
			}
			for _, w := range want {
				if !strings.Contains(res.stderr, w) {
					t.Errorf("stderr does not say %q:\n%s", w, res.stderr)
				}
			}
			b.wantDeletes(t, 1)
			if n := b.listCalls(t); n != 0 {
				t.Errorf("sessions were listed %d times on output that named sessions", n)
			}
			if strings.Contains(res.stderr, "OpenCode session "+ocID(1)) {
				t.Errorf("the rollback names a session it deleted:\n%s", res.stderr)
			}
		})
	}
}

// TestLineAddOpenCodeDoesNotTakeAFullPageForAll: a list as long as the page asked
// for may be only the newest part of the project's sessions, so a run that named no
// session and finds none in it says an older one could exist and where to look; a
// short page is all of them, and says there is nothing to delete.
func TestLineAddOpenCodeDoesNotTakeAFullPageForAll(t *testing.T) {
	entries := func(co string, n int) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, `{"id":"ses_%026d","title":"other work %d","directory":%q,"projectId":%q,"created":4102444800000}`+"\n", 2000+i, i, co, co)
		}
		return b.String()
	}
	for _, c := range []struct {
		name     string
		sessions int
		full     bool
	}{{"a page as long as the one asked for", 200, true}, {"a page one short of it", 199, false}, {"no sessions at all", 0, false}} {
		t.Run(c.name, func(t *testing.T) {
			b := newOCLineBench(t)
			lineUp(t, b.lineBench, "demo")
			co := b.gitCheckout(t, "co")
			b.write(t, "newer", entries(co, c.sessions))
			b.setMode(t, 1, "fail")
			res := b.run(t, b.ladd("demo", "build", "opencode", co)...)
			if res.code != 1 {
				t.Fatalf("exit %d, want 1\n%s", res.code, res.stderr)
			}
			b.wantDeletes(t, 1)
			if c.full {
				for _, want := range []string{"the newest 200 sessions of opencode session list in " + co + " hold none titled", "an older one could exist", "run in " + co} {
					if !strings.Contains(res.stderr, want) {
						t.Errorf("stderr does not say %q:\n%s", want, res.stderr)
					}
				}
				if strings.Contains(res.stderr, "nothing to delete") {
					t.Errorf("a full page was taken for all the sessions:\n%s", res.stderr)
				}
			} else if !strings.Contains(res.stderr, "so there is nothing to delete") || strings.Contains(res.stderr, "an older one could exist") {
				t.Errorf("a short page was not taken for all the sessions:\n%s", res.stderr)
			}
		})
	}
}

// TestLineAddOpenCodeDeletesASessionAStoppedRunLeft: opencode makes the session
// before its first event, so a run stopped before that has made one it never
// named. It is found in the list by its exact title (which carries the run's
// token), directory and creation time, the list is asked for from the checkout
// (1.18.30 lists only the project of the directory it runs in) and wide enough
// that the project's newer sessions do not push it off, and what is not that is
// left alone.
func TestLineAddOpenCodeDeletesASessionAStoppedRunLeft(t *testing.T) {
	b := newOCLineBench(t)
	lineUp(t, b.lineBench, "demo")
	co := b.gitCheckout(t, "co")
	entry := func(id, title string, created int64) string {
		return fmt.Sprintf(`{"id":%q,"title":%q,"directory":%q,"projectId":%q,"created":%d}`+"\n", id, title, co, co, created)
	}
	// Sixty sessions newer than the run's, so that the page has to be wider than
	// the twenty the fake would give by default; and look-alikes of its title
	// that are other runs' (no token, and another token), in the same directory.
	var newer strings.Builder
	for i := 0; i < 60; i++ {
		newer.WriteString(entry(fmt.Sprintf("ses_%026d", 1000+i), fmt.Sprintf("other work %d", i), 4102444800000))
	}
	b.write(t, "newer", newer.String())
	b.write(t, "decoys", entry("ses_00000000000000000notoken", "parley line demo build", 4102444800000)+
		entry("ses_00000000000000000othertk", "parley line demo build 1", 4102444800000))
	b.setMode(t, 1, "stray")
	before := b.snap(t, co)
	res := b.run(t, b.ladd("demo", "build", "opencode", co)...)
	if res.code != 1 {
		t.Fatalf("exit %d, want 1\nstdout: %s\nstderr: %s", res.code, res.stdout, res.stderr)
	}
	b.wantDeletes(t, 1, "ses_00000000000000000000stray1")
	if n := b.listCalls(t); n != 1 {
		t.Errorf("sessions were listed %d times, want 1", n)
	}
	argv := b.call(t, 2)
	if got, want := strings.Join(argv, " "), "session list --format json -n 200"; got != want {
		t.Errorf("the list was asked for as %q, want %q", got, want)
	}
	// The list and the delete are run from the checkout.
	for _, n := range []int{2, 3} {
		if got := strings.TrimSpace(b.file(t, fmt.Sprintf("cwd-%d", n))); got != co {
			t.Errorf("call %d (%v) ran in %q, want the checkout %q", n, b.call(t, n)[:2], got, co)
		}
	}
	if !strings.Contains(res.stderr, "ses_00000000000000000000stray1 (session build, which the run made without announcing it)") {
		t.Errorf("stderr does not say it deleted the stray:\n%s", res.stderr)
	}
	if got := b.snap(t, co); got != before {
		t.Fatalf("the run left a trace.\nbefore:\n%s\nafter:\n%s", before, got)
	}
}

// TestLineAddOpenCodeSaysWhenItCannotLookForAStray: a list that fails is told,
// with the title to look for, and the rest of the rollback goes on.
func TestLineAddOpenCodeSaysWhenItCannotLookForAStray(t *testing.T) {
	b := newOCLineBench(t)
	lineUp(t, b.lineBench, "demo")
	co := b.gitCheckout(t, "co")
	b.setMode(t, 1, "fail")
	b.setMode(t, 2, "faillist")
	before := b.snap(t, co)
	res := b.run(t, b.ladd("demo", "build", "opencode", co)...)
	if res.code != 1 || !strings.Contains(res.stderr, "boom: not logged in") {
		t.Fatalf("exit %d, want 1 with the run's own failure\n%s", res.code, res.stderr)
	}
	for _, want := range []string{"could not look for a session the run may have left", "db locked", `look for the title "parley line demo build `, "in opencode session list, run in", "rolled back"} {
		if !strings.Contains(res.stderr, want) {
			t.Errorf("stderr does not say %q:\n%s", want, res.stderr)
		}
	}
	if got := b.snap(t, co); got != before {
		t.Fatalf("the run left a trace.\nbefore:\n%s\nafter:\n%s", before, got)
	}
}

// TestLineAddOpenCodeRollbackSaysWhenItCannotDelete: a delete that fails is told,
// with the command to run by hand.
func TestLineAddOpenCodeRollbackSaysWhenItCannotDelete(t *testing.T) {
	b := newOCLineBench(t)
	lineUp(t, b.lineBench, "demo")
	co := b.gitCheckout(t, "co")
	b.setMode(t, 1, "failafter")
	b.setMode(t, 2, "faildelete")
	res := b.run(t, b.ladd("demo", "build", "opencode", co)...)
	if res.code != 1 {
		t.Fatalf("exit %d, want 1\n%s", res.code, res.stderr)
	}
	for _, want := range []string{"could NOT roll back OpenCode session " + ocID(1), "cannot delete", "remove it yourself with: opencode session delete " + ocID(1)} {
		if !strings.Contains(res.stderr, want) {
			t.Errorf("stderr does not say %q:\n%s", want, res.stderr)
		}
	}
}

// TestLineAddOpenCodeRollsBackWhenTheRecordCannotBeWritten: the record is the last
// step, and a failure there takes the session, its record and the reply directory
// down.
func TestLineAddOpenCodeRollsBackWhenTheRecordCannotBeWritten(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root is not stopped by a directory's mode")
	}
	b := newOCLineBench(t)
	lineUp(t, b.lineBench, "demo")
	co := b.gitCheckout(t, "co")
	dir := filepath.Join(b.base, "lines", "demo")
	before := b.snap(t, co)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	res := b.run(t, b.ladd("demo", "build", "opencode", co)...)
	if res.code != 1 || !strings.Contains(res.stderr, "rolled back") {
		t.Fatalf("exit %d, want 1 with the rollback told\n%s", res.code, res.stderr)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	b.wantDeletes(t, 1, ocID(1))
	if got := b.snap(t, co); got != before {
		t.Fatalf("the run left a trace.\nbefore:\n%s\nafter:\n%s", before, got)
	}
}

// TestLineAddOpenCodeInterruptedRollsEverythingBack: a signal while the turn runs
// stops the run and everything it started, including a child of the opencode
// that would otherwise go on, deletes the session the turn had announced and
// leaves the machine as it was.
func TestLineAddOpenCodeInterruptedRollsEverythingBack(t *testing.T) {
	b := newOCLineBench(t)
	lineUp(t, b.lineBench, "demo")
	co := b.gitCheckout(t, "co")
	b.setMode(t, 1, "hold")
	before := b.snap(t, co)

	cmd := exec.Command(binPath, b.ladd("demo", "build", "opencode", co)...)
	cmd.Env = append(os.Environ(), b.env()...)
	stderr := &syncBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := joinCmd(cmd)
	announced := filepath.Join(b.oc, "announced")
	for deadline := time.Now().Add(childDeadline); ; {
		if _, err := os.Stat(announced); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatalf("the fake opencode never announced its session:\n%s", stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(b.file(t, "child-pid")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
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
	b.wantDeletes(t, 1, ocID(1))
	// The child the opencode started is gone with it.
	waitGone(t, pid, "the opencode's child")
	if got := b.snap(t, co); got != before {
		t.Fatalf("the run left a trace.\nbefore:\n%s\nafter:\n%s", before, got)
	}
}

// processGone is whether the process is not there: no such process, or a zombie,
// which is what a killed orphan is until init reaps it (and for good in a
// container with no init), and which kill 0 still finds.
func processGone(pid int) bool {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return true
	}
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	return err != nil || strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}

// waitGone fails unless the process is gone within the deadline: a kill is
// delivered at once, but the process takes a moment to be gone.
func waitGone(t *testing.T, pid int, what string) {
	t.Helper()
	for deadline := time.Now().Add(childDeadline); !processGone(pid); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%s (process %d) is still there", what, pid)
		}
	}
}

// TestOpenCodeDeleteAndListStopTheWholeProcessGroup: the delete and the list are
// run as the turn is, each a process group of its own that a stop takes whole, so
// a launcher's child does not outlive the command that is stopped at its bound.
func TestOpenCodeDeleteAndListStopTheWholeProcessGroup(t *testing.T) {
	old := openCodeDeleteTimeout
	openCodeDeleteTimeout = time.Second
	t.Cleanup(func() { openCodeDeleteTimeout = old })
	for _, c := range []struct {
		name string
		run  func(b *ocLineBench, co string) error
	}{
		{"session delete", func(b *ocLineBench, co string) error {
			return openCodeDelete(b.db.opencode, "opencode", ocID(1), co)
		}},
		{"session list", func(b *ocLineBench, co string) error {
			_, _, err := openCodeStrays(b.db.opencode, "t", co, time.Now())
			return err
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := newOCLineBench(t)
			b.setMode(t, 1, "holdcmd")
			co := b.plainCheckout(t, "co")
			if err := c.run(b, co); err == nil {
				t.Fatal("a command that never ended was taken for a success")
			}
			pid, err := strconv.Atoi(strings.TrimSpace(b.file(t, "child-pid-1")))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
			waitGone(t, pid, "the child of the stopped "+c.name)
		})
	}
}

// TestLineAddOpenCodeKillsAChildTheLauncherLeft: a launcher that ends the turn and
// exits at once while a child still holds the pipes makes Run return at the wait
// delay without the cancellation having run. The child is killed before line add
// returns to its rollback, so it cannot go on with tools on a session about to be
// deleted, and the session is deleted.
func TestLineAddOpenCodeKillsAChildTheLauncherLeft(t *testing.T) {
	b := newOCLineBench(t)
	lineUp(t, b.lineBench, "demo")
	co := b.gitCheckout(t, "co")
	b.setMode(t, 1, "orphan")
	before := b.snap(t, co)
	res := b.run(t, b.ladd("demo", "build", "opencode", co)...)
	if res.code != 1 || !strings.Contains(res.stderr, "WaitDelay") {
		t.Fatalf("exit %d, want 1 naming the wait delay\nstdout: %s\nstderr: %s", res.code, res.stdout, res.stderr)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(b.file(t, "child-pid")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	waitGone(t, pid, "the child the launcher left")
	b.wantDeletes(t, 1, ocID(1))
	if got := b.snap(t, co); got != before {
		t.Fatalf("the run left a trace.\nbefore:\n%s\nafter:\n%s", before, got)
	}
}

// ocEnvRecorder is a setupEnv that keeps what a run registers.
type ocEnvRecorder struct {
	logs  []string
	undos []string
	dirs  []string
	do    []func() error
}

func (e *ocEnvRecorder) logf(format string, a ...any) {
	e.logs = append(e.logs, fmt.Sprintf(format, a...))
}
func (e *ocEnvRecorder) push(what string, do func() error) {
	e.undos = append(e.undos, what)
	e.do = append(e.do, do)
}
func (e *ocEnvRecorder) mkdirAll(dir string) error {
	e.dirs = append(e.dirs, dir)
	return os.MkdirAll(dir, 0o700)
}

// TestStartOpenCodeSessionEndsAHungTurnAndRegistersItsSession: a turn that never
// ends is stopped at the bound, the error says why, and the session it had
// announced is registered for deletion, as is not the record, which was never
// written.
func TestStartOpenCodeSessionEndsAHungTurnAndRegistersItsSession(t *testing.T) {
	b := newOCLineBench(t)
	b.setMode(t, 1, "hold")
	co := b.plainCheckout(t, "co")
	env := &ocEnvRecorder{}
	start := time.Now()
	_, _, err := startOpenCodeSession(context.Background(), env, openCodeSession{
		nick: "build", dir: co, bin: b.db.opencode, name: "opencode", title: "t", prompt: "hello\n",
		base: b.base, stream: b.stream("demo"), timeout: 2 * time.Second,
	})
	if err == nil {
		t.Fatal("a turn that hung was taken for a success")
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("the hung turn was stopped after %s, want about its 2s bound", took)
	}
	for _, want := range []string{"the turn did not end within 2s, so it was stopped", "opencode run for session build"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not say %q: %v", want, err)
		}
	}
	// 1.18.30's run refuses an ask by itself, so the wording says no more than that
	// the turn did not end.
	if strings.Contains(err.Error(), "ask") || strings.Contains(err.Error(), "by itself") {
		t.Errorf("the error says why the turn hung, which it does not know: %v", err)
	}
	if len(env.undos) != 1 || !strings.Contains(env.undos[0], ocID(1)) {
		t.Fatalf("registered %v, want the one session %s", env.undos, ocID(1))
	}
	if err := env.do[0](); err != nil {
		t.Fatalf("the registered delete failed: %v", err)
	}
	b.wantDeletes(t, 1, ocID(1))
	pid, _ := strconv.Atoi(strings.TrimSpace(b.file(t, "child-pid")))
	waitGone(t, pid, "the child of the stopped run")
	if len(env.dirs) != 0 {
		t.Errorf("a record directory was made for a turn that did not complete: %v", env.dirs)
	}
}

// TestParseOpenCodeTurn: the verdict rests on the events, and a line that is no
// event is passed over.
func TestParseOpenCodeTurn(t *testing.T) {
	ev := func(typ, id, part string) string {
		return fmt.Sprintf(`{"type":%q,"timestamp":1,"sessionID":%q,"part":%s}`, typ, id, part)
	}
	id := ocID(1)
	out := strings.Join([]string{
		"permission requested: external_directory (/tmp/x/*); auto-rejecting",
		ev("step_start", id, `{}`),
		ev("text", id, `{"text":"one"}`),
		ev("step_finish", id, `{"reason":"tool-calls"}`),
		"",
		"{not json",
		`[1,2]`,
		`{"timestamp":1}`,
		ev("step_start", id, `{}`),
		ev("text", id, `{"text":"two"}`),
		ev("text", id, `{"text":"three"}`),
		ev("step_finish", id, `{"reason":"stop"}`),
	}, "\n")
	got := parseOpenCodeTurn(out)
	if got.SessionID != id || len(got.Others) != 0 || got.Steps != 2 || got.LastReason != "stop" || got.Answer != "two\nthree" || got.Failure != "" {
		t.Errorf("parsed %+v", got)
	}
	if want := "step_start,text,step_finish"; strings.Join(got.Events, ",") != want {
		t.Errorf("events %v, want %s", got.Events, want)
	}

	got = parseOpenCodeTurn(ev("step_start", id, `{}`) + "\n" + ev("step_finish", ocID(2), `{"reason":"stop"}`) + "\n" + ev("text", ocID(2), `{"text":"x"}`))
	if got.SessionID != id || len(got.Others) != 1 || got.Others[0] != ocID(2) {
		t.Errorf("two ids parsed as %+v", got)
	}

	got = parseOpenCodeTurn(`{"type":"error","sessionID":"` + id + `","error":{"name":"UnknownError","data":{"message":"quota\u001b[31m exceeded"}}}`)
	if got.Failure != "UnknownError: quota[31m exceeded" {
		t.Errorf("failure = %q", got.Failure)
	}
	got = parseOpenCodeTurn(`{"type":"error","sessionID":"` + id + `","error":{"x":1}}`)
	if want := `an error event: "{\"x\":1}"`; got.Failure != want {
		t.Errorf("failure = %q, want %s", got.Failure, want)
	}
	// A step that began after the last finish is open, and has no reason of its own.
	got = parseOpenCodeTurn(strings.Join([]string{
		ev("step_start", id, `{}`), ev("step_finish", id, `{"reason":"stop"}`), ev("step_start", id, `{}`),
	}, "\n"))
	if !got.Open || got.LastReason != "" || got.Steps != 1 {
		t.Errorf("a step_finish(stop) then an unfinished step_start parsed as %+v, want an open step with no reason", got)
	}
	got = parseOpenCodeTurn(strings.Join([]string{ev("step_start", id, `{}`), ev("step_finish", id, `{"reason":"stop"}`)}, "\n"))
	if got.Open || got.LastReason != "stop" {
		t.Errorf("a finished step parsed as %+v", got)
	}
	// An event that names no session certifies nothing: it is not a step, an
	// answer or a reason, and its type is kept for the error.
	got = parseOpenCodeTurn(strings.Join([]string{
		ev("step_start", id, `{}`),
		ev("text", id, `{"text":"x"}`),
		`{"type":"step_finish","timestamp":3,"part":{"reason":"stop"}}`,
		`{"type":"text","part":{"text":"unnamed"}}`,
	}, "\n"))
	if got.SessionID != id || got.Steps != 0 || got.LastReason != "" || got.Answer != "x" || strings.Join(got.NoID, ",") != "step_finish,text" {
		t.Errorf("an event with no session parsed as %+v", got)
	}
	got = parseOpenCodeTurn(`{"type":"step_finish","sessionID":"` + id + `","part":"not an object"}`)
	if got.Steps != 0 {
		t.Errorf("a step_finish whose part is no object counted: %+v", got)
	}
}

// TestLineSessionCheckForOpenCode: the record of an OpenCode session is held to
// what line add writes: a session id as its thread, no launch file, a reply
// directory of the line.
func TestLineSessionCheckForOpenCode(t *testing.T) {
	base := t.TempDir()
	good := func() lineRecord {
		rec := goodLineRecord(base)
		rec.Sessions = append([]lineSession(nil), rec.Sessions...)
		rec.Sessions = append(rec.Sessions, lineSession{Nick: "oc", Harness: "opencode", Model: "openai/gpt-5", Checkout: "/work/co",
			Thread: ocID(1), ReplyDir: "/tmp/parley-line-demo-456", Files: []lineFile{}})
		return rec
	}
	if err := good().check("demo", base); err != nil {
		t.Fatalf("a good OpenCode session is refused: %v", err)
	}
	rec := good()
	rec.Sessions[2].Files = nil
	rec.OpenCodeBin = "/opt/oc/bin/opencode"
	if err := rec.check("demo", base); err != nil {
		t.Fatalf("a nil file list is refused: %v", err)
	}
	for _, c := range []struct {
		name   string
		mutate func(r *lineRecord)
		want   string
	}{
		{"no session id", func(r *lineRecord) { r.Sessions[2].Thread = "" }, "no session id"},
		{"a Codex thread as the id", func(r *lineRecord) { r.Sessions[2].Thread = fakeThread(1) }, "OpenCode session's id"},
		{"an id that is not one", func(r *lineRecord) { r.Sessions[2].Thread = "ses_abc" }, "OpenCode session's id"},
		{"a session id on a Codex session", func(r *lineRecord) { r.Sessions[1].Thread = ocID(1) }, "UUID"},
		{"a session id on a Claude session", func(r *lineRecord) { r.Sessions[0].Thread = ocID(1) }, "a Claude session holds no thread"},
		{"a launch file", func(r *lineRecord) {
			r.Sessions[2].Files = []lineFile{{Path: "/work/co/.mcp.json", SHA256: "sha256:" + strings.Repeat("a", 64)}}
		}, "has none"},
		{"a reply directory of another line", func(r *lineRecord) { r.Sessions[2].ReplyDir = "/tmp/parley-line-other-456" }, "reply directory"},
		{"a relative reply directory", func(r *lineRecord) { r.Sessions[2].ReplyDir = "parley-line-demo-456" }, "reply directory"},
		{"a model with a flag in it", func(r *lineRecord) { r.Sessions[2].Model = "-x" }, "model"},
		{"a relative opencode_bin", func(r *lineRecord) { r.OpenCodeBin = "opencode" }, "opencode_bin"},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := good()
			rec.Sessions = append([]lineSession(nil), rec.Sessions...)
			c.mutate(&rec)
			if err := rec.check("demo", base); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("check = %v, want an error saying %q", err, c.want)
			}
		})
	}
}

// TestLineDropOpenCodeSession: the session record and the reply directory go,
// nothing in the checkout is touched, the delete line is printed and never run.
func TestLineDropOpenCodeSession(t *testing.T) {
	b := newOCLineBench(t)
	lineUp(t, b.lineBench, "demo")
	co := b.gitCheckout(t, "co")
	mustLine(t, b.lineBench, b.ladd("demo", "build", "opencode", co)...)
	s := sessionByNick(t, b.record(t, "demo"), "build")
	sr, err := state.OpenSessionRecord(b.base, "build", b.stream("demo"))
	if err != nil {
		t.Fatal(err)
	}
	treeBefore := treeListing(t, co)

	res := mustLine(t, b.lineBench, b.ldrop("demo", "build")...)
	t.Logf("stdout:\n%s", res.stdout)
	for _, gone := range []string{s.ReplyDir, sr.Path(), filepath.Dir(sr.Path())} {
		if _, err := os.Lstat(gone); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s is still there (%v)", gone, err)
		}
	}
	if got := treeListing(t, co); got != treeBefore {
		t.Errorf("the drop touched the checkout.\nbefore:\n%s\nafter:\n%s", treeBefore, got)
	}
	if rec := b.record(t, "demo"); len(rec.Sessions) != 0 {
		t.Errorf("sessions left: %+v", rec.Sessions)
	}
	wantInOrder(t, res.stdout, []string{
		"Session build is dropped from line demo. Removed:",
		"OpenCode session record for session build",
		"reply directory " + s.ReplyDir,
		"OpenCode still has each OpenCode session. Keep that history, or delete it:",
		"opencode session delete '" + ocID(1) + "'    # session build",
	})
	for _, unwanted := range []string{"launch file", "Codex still has", ".mcp.json", ".codex"} {
		if strings.Contains(res.stdout, unwanted) {
			t.Errorf("the report mentions %q:\n%s", unwanted, res.stdout)
		}
	}
	// parley never deletes the harness's session itself.
	b.wantDeletes(t, 1)
}

// TestLineDropOpenCodeJudgesItsRecordBeforeRemovingIt: a record that is gone is
// said so, one that holds another session is left in place and said, and a
// reply directory that is not the session's is refused.
func TestLineDropOpenCodeJudgesItsRecordBeforeRemovingIt(t *testing.T) {
	setup := func(t *testing.T) (*ocLineBench, lineSession, *state.SessionRecord) {
		b := newOCLineBench(t)
		lineUp(t, b.lineBench, "demo")
		mustLine(t, b.lineBench, b.ladd("demo", "build", "opencode", b.gitCheckout(t, "co"))...)
		sr, err := state.OpenSessionRecord(b.base, "build", b.stream("demo"))
		if err != nil {
			t.Fatal(err)
		}
		return b, sessionByNick(t, b.record(t, "demo"), "build"), sr
	}
	t.Run("a record that is gone", func(t *testing.T) {
		b, _, sr := setup(t)
		if err := sr.Remove(); err != nil {
			t.Fatal(err)
		}
		res := mustLine(t, b.lineBench, b.ldrop("demo", "build")...)
		if !strings.Contains(res.stdout, "OpenCode session record for session build (already gone)") {
			t.Errorf("stdout:\n%s", res.stdout)
		}
	})
	t.Run("a record that holds another session", func(t *testing.T) {
		b, _, sr := setup(t)
		const other = "ses_AbCdEfGhIjKlMnOpQrStUvWxYz"
		if err := sr.Write(other); err != nil {
			t.Fatal(err)
		}
		res := mustLine(t, b.lineBench, b.ldrop("demo", "build")...)
		wantInOrder(t, res.stdout, []string{
			"Left in place:",
			fmt.Sprintf("OpenCode session record %s: it holds session %s, not session build's %s, so it is another session's or no one's", sr.Path(), other, ocID(1)),
		})
		if _, err := os.Stat(sr.Path()); err != nil {
			t.Errorf("the record that is not the session's was removed: %v", err)
		}
	})
	t.Run("a record that differs only in case is another session", func(t *testing.T) {
		b, _, sr := setup(t)
		// Session ids are case-sensitive, unlike a Codex thread's hex UUID.
		if err := sr.Write("ses_ABCDEFGHIJKLMNOPQRSTUVWXYZ"); err != nil {
			t.Fatal(err)
		}
		rec := b.record(t, "demo")
		rec.Sessions[0].Thread = "ses_abcdefghijklmnopqrstuvwxyz"
		if _, err := writeLineRecord(filepath.Join(b.base, "lines", "demo"), rec); err != nil {
			t.Fatal(err)
		}
		res := mustLine(t, b.lineBench, b.ldrop("demo", "build")...)
		if !strings.Contains(res.stdout, "Left in place:") {
			t.Errorf("a record in another case was taken for the session's:\n%s", res.stdout)
		}
	})
	t.Run("a reply directory that is not the session's", func(t *testing.T) {
		b, s, _ := setup(t)
		if err := os.WriteFile(filepath.Join(s.ReplyDir, ".parley-line"), []byte("other\nbuild\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		before := b.snap(t)
		res := b.run(t, b.ldrop("demo", "build")...)
		if res.code != 1 || !strings.Contains(res.stderr, "marker") {
			t.Fatalf("exit %d, want 1 naming the marker\n%s", res.code, res.stderr)
		}
		if got := b.snap(t); got != before {
			t.Errorf("a refused drop changed the machine.\nbefore:\n%s\nafter:\n%s", before, got)
		}
	})
}

// TestLineEndTakesDownEverySession: a Claude, a Codex and two OpenCode sessions,
// two of them in one checkout. The OpenCode sessions share no launch file with
// anyone, so the Claude session's .mcp.json goes with the Claude session whichever
// order they leave in, and an OpenCode session never keeps it.
func TestLineEndTakesDownEverySession(t *testing.T) {
	b := newOCLineBench(t)
	lineUp(t, b.lineBench, "demo")
	shared := b.gitCheckout(t, "co-shared")
	other := b.gitCheckout(t, "co-other")
	mustLine(t, b.lineBench, b.ladd("demo", "oc-a", "opencode", shared)...)
	mustLine(t, b.lineBench, b.ladd("demo", "plan", "claude", shared)...)
	mustLine(t, b.lineBench, b.ladd("demo", "oc-b", "opencode", shared)...)
	mustLine(t, b.lineBench, b.ladd("demo", "build", "codex", other)...)
	mustLine(t, b.lineBench, b.ladd("demo", "oc-c", "opencode", other)...)
	if _, err := os.Stat(filepath.Join(shared, ".mcp.json")); err != nil {
		t.Fatal(err)
	}

	// Dropping an OpenCode session leaves the Claude session's file.
	res := mustLine(t, b.lineBench, b.ldrop("demo", "oc-a")...)
	if strings.Contains(res.stdout, ".mcp.json") {
		t.Errorf("an OpenCode drop reports the Claude session's file:\n%s", res.stdout)
	}
	if _, err := os.Stat(filepath.Join(shared, ".mcp.json")); err != nil {
		t.Errorf("an OpenCode drop removed the Claude session's file: %v", err)
	}

	res = mustLine(t, b.lineBench, b.lend("demo")...)
	t.Logf("stdout:\n%s", res.stdout)
	for _, gone := range []string{filepath.Join(shared, ".mcp.json"), filepath.Join(other, ".codex"), filepath.Join(b.base, "lines", "demo")} {
		if _, err := os.Lstat(gone); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s is still there (%v)", gone, err)
		}
	}
	for _, nick := range []string{"oc-a", "oc-b", "oc-c", "plan", "build"} {
		for _, rec := range []string{"opencode-session", "codex-thread"} {
			if m, _ := filepath.Glob(filepath.Join(b.base, nick, "*", rec)); len(m) != 0 {
				t.Errorf("%s is still there: %v", rec, m)
			}
		}
	}
	wantInOrder(t, res.stdout, []string{
		"Codex still has each Codex session. Keep that history, or delete it:",
		"codex delete '" + fakeThread(1) + "'    # session build",
		"OpenCode still has each OpenCode session. Keep that history, or delete it:",
	})
	got := 0
	for _, l := range strings.Split(res.stdout, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "opencode session delete ") {
			got++
		}
	}
	if got != 2 {
		t.Errorf("%d opencode session delete lines, want 2 (oc-b and oc-c; oc-a was dropped before)\n%s", got, res.stdout)
	}
	if got := readExcludeForTest(t, shared); strings.Contains(got, ".mcp.json") {
		t.Errorf("the exclude line was left: %q", got)
	}
	if got := readExcludeForTest(t, other); strings.Contains(got, ".codex") {
		t.Errorf("the exclude line was left: %q", got)
	}
	if status := gitIn(t, shared, "status", "--porcelain"); status != "" {
		t.Errorf("the shared checkout is not clean: %s", status)
	}
}

// TestLineAddOpenCodePumpFindsTheRecordedSession: parley opencode --session auto,
// as the runbook prints it, finds the session the add recorded and posts the
// chair's message into it as one prompt, whose text is what parley wait prints.
// The port is the one the runbook names for a TUI that is not there; the fake
// server stands in for it on that line's port by the test pointing the pump at
// the fake's.
func TestLineAddOpenCodePumpFindsTheRecordedSession(t *testing.T) {
	addr := startErgo(t)
	b := newOCLineBench(t)
	b.db.server = addr
	lineUp(t, b.lineBench, "demo")
	co := b.gitCheckout(t, "co")
	b.write(t, "id", ocSession)
	res := mustLine(t, b.lineBench, b.ladd("demo", "build", "opencode", co)...)
	if got := sessionByNick(t, b.record(t, "demo"), "build").Thread; got != ocSession {
		t.Fatalf("recorded session %q, want %q", got, ocSession)
	}

	srv := newOCServer(t)
	words := shellWords(t, lineWith(t, res.stdout, " opencode --stream "))
	if words[0] != binPath || words[1] != "opencode" {
		t.Fatalf("the pump line starts with %q, want this binary and opencode", words[:2])
	}
	args := append([]string(nil), words[2:]...)
	swapped := false
	for i, a := range args {
		if a == "--port" {
			args[i+1], swapped = strconv.Itoa(srv.port()), true
		}
	}
	if !swapped {
		t.Fatalf("no --port in %q", args)
	}
	_, _, stderr := startOpencode(t, append(ocEnv(), b.env()...), append(args, "--debounce", "100ms")...)
	waitForStderr(t, stderr, "into OpenCode session "+ocSession)

	mustRun(t, "", "send", "--stream", b.stream("demo"), "--nick", "colin", "--state-dir", t.TempDir(), "--", "welcome to the line")
	posts := srv.waitPosts(1, stderr)
	got := ocPostedText(t, posts[0])
	want := mustRun(t, "", "wait", "--stream", b.stream("demo"), "--nick", "watcher", "--state-dir", t.TempDir(), "--timeout", "10s", "--debounce", "100ms").stdout
	if want == "" || got != want {
		t.Fatalf("the posted text is\n%q\nwant what parley wait prints\n%q", got, want)
	}
	if !strings.HasPrefix(got, "[parley] from=colin id=") || !strings.Contains(got, "welcome to the line") {
		t.Errorf("the posted text:\n%q", got)
	}
	if n := len(srv.postBodies()); n != 1 {
		t.Errorf("%d prompts, want 1", n)
	}
}

// TestTableAndBuildRefuseOpenCodeAndPointToLines: a table or a build takes no
// OpenCode seat or peer yet, and the refusal says a line does.
func TestTableAndBuildRefuseOpenCodeAndPointToLines(t *testing.T) {
	b := newTableBench(t)
	for _, args := range [][]string{b.newArgs("t1", []string{"o=opencode"}), b.buildArgs("b1", []string{"x=opencode", "claude-a=claude"})} {
		res := b.run(t, args...)
		if res.code != 2 || !strings.Contains(res.stderr, "OpenCode") || !strings.Contains(res.stderr, "not supported yet") || !strings.Contains(res.stderr, "parley line add --harness opencode") {
			t.Errorf("%v: exit %d\n%s", args[:2], res.code, res.stderr)
		}
	}
}

// TestLineUsageNamesOpenCode: the usage a human reads lists the harness and the
// binary flag.
func TestLineUsageNamesOpenCode(t *testing.T) {
	help := runParley(t, "", "line", "-h").stdout
	for _, want := range []string{"--harness claude|codex|opencode", "[--opencode-bin opencode]"} {
		if !strings.Contains(help, want) {
			t.Errorf("parley line -h does not say %q:\n%s", want, help)
		}
	}
	top := strings.Join(strings.Fields(mustRun(t, "", "-h").stdout), " ")
	for _, want := range []string{"parley line add <id> --nick <nick> --harness claude|codex|opencode", "[--opencode-bin opencode]", "opencode session delete"} {
		if !strings.Contains(top, want) {
			t.Errorf("parley -h does not say %q", want)
		}
	}
}
