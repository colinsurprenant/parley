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

	"github.com/colinsurprenant/parley/internal/doctor"
	"github.com/colinsurprenant/parley/internal/state"
)

// fakeOpenCodeScript is an opencode that answers --version and the three --help
// texts like the @V@ they were copied from, and records the argv of every
// other call in $d/call-<n>, with the directory it ran in (cwd-<n>), three
// variables of its environment (env-<n>) and, for run, its stdin (stdin-<n>). A
// record is written under a temporary name and renamed into place before the
// count is published, so a reader that sees the count at n finds call-n whole.
// run prints the events of a turn whose session is the id it was given with
// --session (or $d/id) unless $d/mode-<n> says otherwise; session delete succeeds.
const fakeOpenCodeScript = `#!/bin/sh
d=@D@
t=@T@
ver="opencode v@V@"
[ -f "$d/version" ] && ver=$(cat "$d/version")
case "$1 $2" in
"--version "*) echo "$ver"; exit 0 ;;
"--help ") cat "$t/opencode-@V@-tui.help"; exit 0 ;;
"run --help") cat "$t/opencode-@V@-run.help"; exit 0 ;;
"session --help") cat "$t/opencode-@V@-session.help"; exit 0 ;;
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
# The run creates the session under the id it is given.
prev=; for a in "$@"; do [ "$prev" = --session ] && id=$a; prev=$a; done
[ -f "$d/id" ] && id=$(cat "$d/id")
start() { printf '{"type":"step_start","timestamp":1,"sessionID":"%s","part":{"id":"prt_1","type":"step-start"}}\n' "$1"; }
text() { printf '{"type":"text","timestamp":2,"sessionID":"%s","part":{"id":"prt_2","type":"text","text":"%s"}}\n' "$1" "$2"; }
finish() { printf '{"type":"step_finish","timestamp":3,"sessionID":"%s","part":{"id":"prt_3","type":"step-finish","reason":"%s"}}\n' "$1" "$2"; }
tool() { printf '{"type":"tool_use","timestamp":2,"sessionID":"%s","part":{"partID":"prt_t","sessionID":"%s","type":"tool","id":"tool_1","tool":"skill","state":{"status":"completed","input":{"id":"parley-line"}}}}\n' "$1" "$1"; }
err() { printf '{"type":"error","timestamp":4,"sessionID":"%s","error":{"name":"UnknownError","data":{"message":"%s"}}}\n' "$1" "$2"; }
case "$1" in
run)
	cat > "$d/stdin-$n"
	case "$mode" in
	fail) echo "boom: not logged in" >&2; exit 3 ;;
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
	dropped)
		# What a real 2.1 primer turn prints when the run loses the race with the
		# session going idle: the last step's finish is never printed.
		start "$id"; tool "$id"; finish "$id" tool-calls
		start "$id"; text "$id" "ready, waiting for the first delivery"
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
	simfail)
		# What a 2.0.32 server in simulation mode does with a turn: it retries the
		# step, every try a step_start, and ends with an error event and status 1.
		start "$id"; start "$id"; start "$id"
		printf '{"type":"error","timestamp":4,"sessionID":"%s","error":{"type":"provider.transport","message":"Transport: Simulation denied unregistered network destination: POST https://opencode.ai/inference/openai/v1/chat/completions"}}\n' "$id"
		exit 1 ;;
	unsaid) echo "boom: could not reach the service" >&2; exit 3 ;;
	errorzero) start "$id"; err "$id" "stream broke"; finish "$id" stop; exit 0 ;;
	ctrlerror)
		start "$id"
		printf '%s\n' '{"type":"error","sessionID":"'"$id"'","error":{"name":"UnknownError","data":{"message":"quota\u001b[31m exceeded\u0007"}}}'
		echo "bad" >&2; exit 1 ;;
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
		if [ "$mode" = gone ]; then echo "Session not found: $3" >&2; exit 1; fi
		exit 0 ;;
	esac ;;
esac
exit 0
`

// ocLineBench is a lineBench whose opencode is the recording fake.
type ocLineBench struct {
	*lineBench
	oc string
	// svc is OpenCode's service as line add finds it, from the registration file in
	// the bench's state home: it knows every session and shows the history of a
	// real turn that went through, until a test says otherwise.
	svc *ocServer
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
	script := strings.NewReplacer("@D@", strconv.Quote(b.oc), "@T@", strconv.Quote(testdata), "@V@", doctor.OpenCodeVerified).Replace(fakeOpenCodeScript)
	// The bench's PATH already holds an opencode, the doctor bench's; this one
	// answers the same questions and records the rest.
	if err := os.WriteFile(b.db.opencode, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	b.svc = newOCServiceServer(t)
	b.svc.set(func() {
		b.svc.anySession = true
		b.svc.history = append([]ocHist(nil), ocLiveTurn...)
	})
	writeService(t, filepath.Join(b.root, "xdg"), b.svc.url(), ocPID, 0o600)
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

// wantDeletes fails unless the calls are exactly these runs, each followed by the
// session deletes, and nothing else: no list of sessions is asked for, since the id
// is known.
func (b *ocLineBench) wantDeletes(t *testing.T, runs int, ids ...string) {
	t.Helper()
	got := b.calls(t)
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

// ocIDRE is the session id parley makes for an OpenCode 2 session: ses_, twelve
// hex digits of time, fourteen of letters and digits.
var ocIDRE = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)

// ocID is a session id of the shape OpenCode makes, numbered, for a test that needs a
// few that differ.
func ocID(n int) string { return fmt.Sprintf("ses_%026d", n) }

// idOfRun is the session id the first call, the run, was given with --session.
func idOfRun(t *testing.T, b *ocLineBench) string {
	t.Helper()
	argv := b.call(t, 1)
	for i, a := range argv {
		if a == "--session" && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	t.Fatalf("the run was not given --session: %q", argv)
	return ""
}

// TestLineAddOpenCodeSession is the acceptance run for an OpenCode session on the
// fake opencode: the session id is parley's own and goes to the run with
// --session, it is the id recorded, printed and checked in the events, nothing is
// written in the checkout, and the runbook has no port.
func TestLineAddOpenCodeSession(t *testing.T) {
	b := newOCLineBench(t)
	lineUp(t, b.lineBench, "demo")
	co := b.gitCheckout(t, "co-build")
	treeBefore := treeListing(t, co)
	res := runParleyEnvIn(t, "", b.env(), b.ladd("demo", "build", "opencode", co, "--model", "anthropic/claude-sonnet-4-5")...)
	if res.code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", res.code, res.stdout, res.stderr)
	}
	stream := b.stream("demo")
	if after := treeListing(t, co); after != treeBefore {
		t.Errorf("the checkout changed.\nbefore:\n%s\nafter:\n%s", treeBefore, after)
	}

	// One run, with the id parley chose; nothing was listed or deleted.
	b.wantDeletes(t, 1)
	id := idOfRun(t, b)
	if !ocIDRE.MatchString(id) {
		t.Fatalf("the session id %q is not one OpenCode makes", id)
	}
	want := []string{"run", "--format", "json", "--title", "parley line demo build", "--session", id, "-m", "anthropic/claude-sonnet-4-5"}
	if got := b.call(t, 1); strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("opencode argv %q, want %q", got, want)
	}
	if got := strings.TrimSpace(b.file(t, "cwd-1")); got != co {
		t.Errorf("opencode ran in %q, want the checkout %q", got, co)
	}
	if prompt := b.file(t, "stdin-1"); !strings.HasPrefix(prompt, "This primer is your first message") {
		t.Errorf("the primer is not on stdin:\n%q", prompt)
	}

	s := sessionByNick(t, b.record(t, "demo"), "build")
	if s.Harness != "opencode" || s.Thread != id || len(s.Files) != 0 {
		t.Errorf("session %+v, want thread %s", s, id)
	}
	sr, err := state.OpenSessionRecord(b.base, "build", stream)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := sr.Read(); err != nil || got != id {
		t.Errorf("session record = %q, %v; want %s", got, err, id)
	}

	// The runbook: the TUI on the session with -s, no port anywhere, the pump
	// without --port, and the notes for 2.x.
	pump := "'" + binPath + "' opencode --stream '" + stream + "' --nick build --session auto --skill line --chair colin --cap 8 --window 10m --state-dir '" + b.base + "'"
	wantInOrder(t, res.stdout, []string{
		"Session build (opencode) is added to line demo, checkout " + co + ".",
		"Wrote nothing in the checkout.",
		"OpenCode session " + id + ".",
		"What is left is yours, in this order:",
		"cd '" + co + "'",
		"opencode -s '" + id + "'",
		pump,
		"Notes:",
		"Session build's answer to its primer (opencode run, session " + id + "):",
		"ready, waiting for the first delivery",
	})
	flat := strings.Join(strings.Fields(res.stdout), " ")
	for _, want := range []string{
		"OpenCode allows external_directory by default, so writing it asks nothing.",
		`{ "permissions": [ { "action": "external_directory", "resource": "*", "effect": "ask" } ] }`,
		"opencode run --session, which adds no permission rules of its own to the session",
		"it rejected any permission ask and cancelled any question",
		"The service's password is in its service file (mode 600)",
		"sends it to the service's loopback address only; it is never printed",
		"If you interrupt the session, the pump holds deliveries until you send it something.",
		"Parley was " + doctor.OpenCodeVerifiedNote,
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("the runbook does not say %q:\n%s", want, res.stdout)
		}
	}
	// The one --port in it is the note for an opencode serve of the human's own.
	if n := strings.Count(res.stdout, "--port"); n != 1 || !strings.Contains(res.stdout, "run the pump with --port <n> and") {
		t.Errorf("the runbook names --port %d times; want only the note on an opencode serve of one's own:\n%s", n, res.stdout)
	}
	if !strings.Contains(res.stderr, "session build is OpenCode session "+id) {
		t.Errorf("stderr:\n%s", res.stderr)
	}
}

// TestLineAddOpenCodeARunThatFailsLikeASimulatedTurnRollsBackTheChosenID: the turn
// of a 2.0.32 server in simulation mode (it is the one turn that can be run
// without a model) retries and ends in an error event and status 1. The add fails
// with the error's own words, deletes exactly the id it chose, and looks for
// nothing.
func TestLineAddOpenCodeARunThatFailsLikeASimulatedTurnRollsBackTheChosenID(t *testing.T) {
	b := newOCLineBench(t)
	lineUp(t, b.lineBench, "demo")
	co := b.gitCheckout(t, "co")
	b.setMode(t, 1, "simfail")
	before := b.snap(t, co)
	res := b.run(t, b.ladd("demo", "build", "opencode", co)...)
	if res.code != 1 {
		t.Fatalf("exit %d, want 1\nstdout: %s\nstderr: %s", res.code, res.stdout, res.stderr)
	}
	id := idOfRun(t, b)
	for _, want := range []string{"Simulation denied unregistered network destination", "exit status 1", "rolled back", "OpenCode session " + id + " (session build)"} {
		if !strings.Contains(res.stderr, want) {
			t.Errorf("stderr does not say %q:\n%s", want, res.stderr)
		}
	}
	b.wantDeletes(t, 1, id)
	// The delete ran from the checkout.
	if got := strings.TrimSpace(b.file(t, "cwd-2")); got != co {
		t.Errorf("the delete ran in %q, want the checkout %q", got, co)
	}
	if got := b.snap(t, co); got != before {
		t.Fatalf("the run left a trace.\nbefore:\n%s\nafter:\n%s", before, got)
	}
	if _, err := os.Stat(filepath.Join(b.base, "build")); err == nil {
		if sr, err := state.OpenSessionRecord(b.base, "build", b.stream("demo")); err == nil {
			if got, _ := sr.Read(); got != "" {
				t.Errorf("a session record %q was left", got)
			}
		}
	}
}

// TestLineAddOpenCodeARunThatMadeNoSessionIsNotAFailureToRollBack: a run that fails
// before it makes the session leaves nothing to delete, and the delete of an id
// the server does not know ("Session not found", status 1) is not told as one.
func TestLineAddOpenCodeARunThatMadeNoSessionIsNotAFailureToRollBack(t *testing.T) {
	b := newOCLineBench(t)
	lineUp(t, b.lineBench, "demo")
	co := b.gitCheckout(t, "co")
	b.setMode(t, 1, "unsaid")
	b.setMode(t, 2, "gone")
	res := b.run(t, b.ladd("demo", "build", "opencode", co)...)
	if res.code != 1 || !strings.Contains(res.stderr, "boom: could not reach the service") {
		t.Fatalf("exit %d, want 1 with the run's own failure\n%s", res.code, res.stderr)
	}
	id := idOfRun(t, b)
	b.wantDeletes(t, 1, id)
	if !strings.Contains(res.stderr, "rolled back") || strings.Contains(res.stderr, "could NOT roll back") || strings.Contains(res.stderr, "Session not found") {
		t.Errorf("a delete of a session that was never made was told as a failure:\n%s", res.stderr)
	}
}

// TestLineAddOpenCodeADeleteThatFailsIsToldWithTheCommand: a delete that fails for
// another reason is a failure, with the command to run by hand.
func TestLineAddOpenCodeADeleteThatFailsIsToldWithTheCommand(t *testing.T) {
	b := newOCLineBench(t)
	lineUp(t, b.lineBench, "demo")
	co := b.gitCheckout(t, "co")
	b.setMode(t, 1, "failafter")
	b.setMode(t, 2, "faildelete")
	res := b.run(t, b.ladd("demo", "build", "opencode", co)...)
	id := idOfRun(t, b)
	for _, want := range []string{"could NOT roll back OpenCode session " + id, "cannot delete", "remove it yourself with: opencode session delete " + id} {
		if res.code != 1 || !strings.Contains(res.stderr, want) {
			t.Errorf("exit %d; stderr does not say %q:\n%s", res.code, want, res.stderr)
		}
	}
}

// TestLineAddOpenCodeEventsNamingAnotherSessionFailTheTurn: the id was parley's
// choice, so every event must carry it; one that names another session does not
// show the turn to be the session's, and the session parley made is still the one
// deleted.
func TestLineAddOpenCodeEventsNamingAnotherSessionFailTheTurn(t *testing.T) {
	b := newOCLineBench(t)
	lineUp(t, b.lineBench, "demo")
	co := b.gitCheckout(t, "co")
	b.write(t, "id", "ses_00000000000000000000000042")
	res := b.run(t, b.ladd("demo", "build", "opencode", co)...)
	if res.code != 1 {
		t.Fatalf("exit %d, want 1\n%s", res.code, res.stderr)
	}
	id := idOfRun(t, b)
	for _, want := range []string{"its events name \"ses_00000000000000000000000042\" and not only " + id, "the id this run chose"} {
		if !strings.Contains(res.stderr, want) {
			t.Errorf("stderr does not say %q:\n%s", want, res.stderr)
		}
	}
	b.wantDeletes(t, 1, id)
}

// TestLineAddOpenCodeInterruptedDeletesTheChosenID: an interrupt, which kills the
// run before it may have named anything, still deletes the one id, and looks for
// nothing; the child the opencode started goes with it.
func TestLineAddOpenCodeInterruptedDeletesTheChosenID(t *testing.T) {
	b := newOCLineBench(t)
	lineUp(t, b.lineBench, "demo")
	co := b.gitCheckout(t, "co")
	b.setMode(t, 1, "hold")

	cmd := exec.Command(binPath, b.ladd("demo", "build", "opencode", co)...)
	cmd.Env = append(os.Environ(), b.env()...)
	stderr := &syncBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := joinCmd(cmd)
	for deadline := time.Now().Add(childDeadline); ; {
		if _, err := os.Stat(filepath.Join(b.oc, "announced")); err == nil {
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
	b.wantDeletes(t, 1, idOfRun(t, b))
	// The run is a process group of its own and the stop took it whole: the child
	// the opencode started is gone with it.
	for deadline := time.Now().Add(childDeadline); ; {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the opencode's child %d outlived the run", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestParseOpenCodeTurnReadsOpenCodeEvents: the events of a failed 2.0.32 turn
// are captured from a server in simulation mode (the first, a step_start, and the
// last, an error); the events of a turn that completed are shaped from the parts
// 2.0.32's non-interactive run writes (packages/cli/src/run/noninteractive.ts), as
// no model turn could be run. Both are read by the same parser.
func TestParseOpenCodeTurnReadsOpenCodeEvents(t *testing.T) {
	const id = "ses_ea83ab198ffeRIhi6BZUsPDq5i"
	failed := `{"type":"step_start","timestamp":1792473911119,"sessionID":"` + id + `","part":{"id":"prt_157c54f4f001c3XPVNJjxsFi2f","sessionID":"` + id + `","messageID":"msg_157c54f2a001RFnoHjhjZMb1a1","type":"step-start","snapshot":"4b825dc642cb6eb9a060e54bf8d69288fbee4904"}}
{"type":"step_start","timestamp":1792473913165,"sessionID":"` + id + `","part":{"id":"prt_157c5574d0016VGpP5sOzWlFZ9","sessionID":"` + id + `","messageID":"msg_157c54f2a001RFnoHjhjZMb1a1","type":"step-start","snapshot":"4b825dc642cb6eb9a060e54bf8d69288fbee4904"}}
{"type":"error","timestamp":1792473995534,"sessionID":"` + id + `","error":{"type":"provider.transport","message":"Transport: Simulation denied unregistered network destination: POST https://opencode.ai/inference/openai/v1/chat/completions (POST https://opencode.ai/inference/openai/v1/chat/completions)"}}
`
	got := parseOpenCodeTurn(failed)
	if got.SessionID != id || len(got.Others) != 0 || len(got.NoID) != 0 || got.Answer != "" || !strings.HasPrefix(got.Failure, "Transport: Simulation denied unregistered network destination") {
		t.Errorf("a failed 2.x turn parsed as %+v", got)
	}

	done := `{"type":"step_start","timestamp":1,"sessionID":"` + id + `","part":{"id":"prt_1","sessionID":"` + id + `","messageID":"msg_1","type":"step-start","snapshot":"4b82"}}
{"type":"text","timestamp":2,"sessionID":"` + id + `","part":{"id":"prt_2","sessionID":"` + id + `","messageID":"msg_1","type":"text","text":"ready, waiting","time":{"start":1,"end":2}}}
{"type":"step_finish","timestamp":3,"sessionID":"` + id + `","part":{"id":"prt_3","sessionID":"` + id + `","messageID":"msg_1","type":"step-finish","reason":"stop","snapshot":"4b82","cost":0,"tokens":{"input":1,"output":1}}}
`
	got = parseOpenCodeTurn(done)
	if got.SessionID != id || got.Answer != "ready, waiting" || got.Failure != "" || len(got.NoID) != 0 {
		t.Errorf("a completed 2.x turn parsed as %+v", got)
	}

	// What a real primer turn printed on 2.1.0, which lost the race with the session
	// going idle: the last step_finish is not there, and the answer is the text after
	// the last step began.
	dropped := `{"type":"step_start","timestamp":1,"sessionID":"` + id + `","part":{"id":"prt_1","sessionID":"` + id + `","messageID":"msg_1","type":"step-start","snapshot":"29cd"}}
{"type":"tool_use","timestamp":2,"sessionID":"` + id + `","part":{"partID":"prt_2","sessionID":"` + id + `","messageID":"msg_1","type":"tool","id":"tool_1","tool":"skill","state":{"status":"completed","input":{"id":"parley-line"}}}}
{"type":"step_finish","timestamp":3,"sessionID":"` + id + `","part":{"id":"prt_3","sessionID":"` + id + `","messageID":"msg_1","type":"step-finish","reason":"tool-calls","snapshot":"29cd"}}
{"type":"step_start","timestamp":4,"sessionID":"` + id + `","part":{"id":"prt_4","sessionID":"` + id + `","messageID":"msg_2","type":"step-start","snapshot":"29cd"}}
{"type":"text","timestamp":5,"sessionID":"` + id + `","part":{"id":"prt_5","sessionID":"` + id + `","messageID":"msg_2","type":"text","text":"Line protocol loaded.","time":{"start":4,"end":5}}}
`
	got = parseOpenCodeTurn(dropped)
	if got.SessionID != id || got.Answer != "Line protocol loaded." || got.Failure != "" || len(got.NoID) != 0 || strings.Join(got.Events, ",") != "step_start,tool_use,step_finish,text" {
		t.Errorf("a 2.1 turn without its last step_finish parsed as %+v", got)
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
	id := sessionByNick(t, b.record(t, "demo"), "build").Thread
	lineWith(t, add.stdout, "'"+bin+"' -s '"+id+"'")
	drop := runParleyEnvIn(t, "", env, b.ldrop("demo", "build")...)
	if drop.code != 0 {
		t.Fatalf("drop exit %d\n%s%s", drop.code, drop.stdout, drop.stderr)
	}
	lineWith(t, drop.stdout, "'"+bin+"' session delete '"+id+"'", "# session build")
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
		{"a model with a variant and no slash", 2, "an OpenCode model is provider/model", func(t *testing.T, b *ocLineBench, co string) ([]string, []string) {
			return b.ladd("demo", "build", "opencode", co, "--model", "a#b"), nil
		}},
		{"a model with an empty variant", 2, "variant after '#' is empty", func(t *testing.T, b *ocLineBench, co string) ([]string, []string) {
			return b.ladd("demo", "build", "opencode", co, "--model", "a/b#"), nil
		}},
		{"a model with two variants", 2, "at most one '#'", func(t *testing.T, b *ocLineBench, co string) ([]string, []string) {
			return b.ladd("demo", "build", "opencode", co, "--model", "a/b#c#d"), nil
		}},
		{"a model with no provider", 2, "an OpenCode model is provider/model", func(t *testing.T, b *ocLineBench, co string) ([]string, []string) {
			return b.ladd("demo", "build", "opencode", co, "--model", "/b"), nil
		}},
		{"a model with no model", 2, "an OpenCode model is provider/model", func(t *testing.T, b *ocLineBench, co string) ([]string, []string) {
			return b.ladd("demo", "build", "opencode", co, "--model", "a/"), nil
		}},
		{"a model with a variant that holds a character no model has", 2, "holds ' '", func(t *testing.T, b *ocLineBench, co string) ([]string, []string) {
			return b.ladd("demo", "build", "opencode", co, "--model", "a/b#c d"), nil
		}},
		{"a checkout that is not UTF-8", 2, "not valid UTF-8", func(t *testing.T, b *ocLineBench, co string) ([]string, []string) {
			return b.ladd("demo", "build", "opencode", co+"/bad\xffname"), nil
		}},
		{"no opencode on the machine", 2, "install OpenCode 2", func(t *testing.T, b *ocLineBench, co string) ([]string, []string) {
			return b.ladd("demo", "build", "opencode", co), []string{"PATH=" + b.pathWith(t, "claude", "codex", "ergo", "git", "cat", "mv")}
		}},
		{"an OpenCode 1", 1, "the legacy 1.x", func(t *testing.T, b *ocLineBench, co string) ([]string, []string) {
			b.write(t, "version", "1.18.30")
			return b.ladd("demo", "build", "opencode", co), nil
		}},
		{"an OpenCode 3", 1, "not a major version parley speaks to", func(t *testing.T, b *ocLineBench, co string) ([]string, []string) {
			b.write(t, "version", "opencode v3.0.0")
			return b.ladd("demo", "build", "opencode", co), nil
		}},
		{"an OpenCode 0", 1, "not a major version parley speaks to", func(t *testing.T, b *ocLineBench, co string) ([]string, []string) {
			b.write(t, "version", "0.9.0")
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

// TestLineAddOpenCodeModelWithAVariantReachesTheRunVerbatim: OpenCode's `run -m` takes
// provider/model#variant (its help says so), and line add passes it on as it was
// given. The record keeps it, and a verb that reads the record back accepts it.
func TestLineAddOpenCodeModelWithAVariantReachesTheRunVerbatim(t *testing.T) {
	const model = "kimi-code-plan-global/k3#high"
	b := newOCLineBench(t)
	lineUp(t, b.lineBench, "demo")
	co := b.gitCheckout(t, "co")
	mustLine(t, b.lineBench, b.ladd("demo", "build", "opencode", co, "--model", model)...)
	argv := b.call(t, 1)
	found := false
	for i, a := range argv {
		if a == "-m" && i+1 < len(argv) && argv[i+1] == model {
			found = true
		}
	}
	if !found {
		t.Errorf("the run was not given -m %s verbatim: %q", model, argv)
	}
	if got := sessionByNick(t, b.record(t, "demo"), "build").Model; got != model {
		t.Errorf("the record keeps the model %q, want %q", got, model)
	}
	// The record is read and checked again by the verbs that take the session down.
	mustLine(t, b.lineBench, b.ldrop("demo", "build")...)
}

// TestLineAddCodexAndClaudeModelsStillRefuseAHash: only an OpenCode model may carry a
// '#', for its variant; no Claude or Codex model id has one.
func TestLineAddCodexAndClaudeModelsStillRefuseAHash(t *testing.T) {
	b := newOCLineBench(t)
	lineUp(t, b.lineBench, "demo")
	co := b.gitCheckout(t, "co")
	for _, harness := range []string{"claude", "codex"} {
		for _, model := range []string{"opus#high", "gpt-5#high", "a/b#high"} {
			res := runParleyEnvIn(t, "", b.env(), b.ladd("demo", harness, harness, co, "--model", model)...)
			if res.code != 2 || !strings.Contains(res.stderr, `holds '#'`) {
				t.Errorf("%s --model %s: exit %d, stderr:\n%s", harness, model, res.code, res.stderr)
			}
		}
	}
}

// TestLineAddOpenCodeAcceptsATurnWhoseFinalStepWasNeverPrinted: a real 2.1 run can
// end, exit 0, before it has printed the finish of its last step, while the server
// holds the turn as finished. The server, which line add asks, is what says the turn
// completed, and the session is started with the answer the events carried.
func TestLineAddOpenCodeAcceptsATurnWhoseFinalStepWasNeverPrinted(t *testing.T) {
	b := newOCLineBench(t)
	lineUp(t, b.lineBench, "demo")
	b.setMode(t, 1, "dropped")
	res := mustLine(t, b.lineBench, b.ladd("demo", "build", "opencode", b.gitCheckout(t, "co"))...)
	if !strings.Contains(res.stdout, "ready, waiting for the first delivery") {
		t.Errorf("stdout does not carry the answer of the last step:\n%s", res.stdout)
	}
	b.wantDeletes(t, 1)
	b.svc.mu.Lock()
	reads := b.svc.historyReads
	b.svc.mu.Unlock()
	if reads == 0 {
		t.Error("the history of the session was never asked for: the turn was taken on the run's word")
	}
	if strings.Contains(res.stdout+res.stderr, ocPassword) {
		t.Errorf("the service's password is in the output:\n%s\n%s", res.stdout, res.stderr)
	}
}

// TestLineAddOpenCodeCertifiesAcrossPagesOfTheHistory: entries of other kinds lie
// between the newest of the history and the idle marker, more than a page of them,
// and the turn is read back through them to the marker and the assistant message.
func TestLineAddOpenCodeCertifiesAcrossPagesOfTheHistory(t *testing.T) {
	b := newOCLineBench(t)
	lineUp(t, b.lineBench, "demo")
	b.svc.set(func() {
		b.svc.history = append(append([]ocHist(nil), ocLiveTurn...), ocHist{kind: "skill"}, ocHist{kind: "skill"}, ocHist{kind: "skill"}, ocHist{kind: "skill"}, ocHist{kind: "skill"}, ocHist{kind: "skill"})
	})
	mustLine(t, b.lineBench, b.ladd("demo", "build", "opencode", b.gitCheckout(t, "co"))...)
	b.svc.mu.Lock()
	reads := b.svc.historyReads
	b.svc.mu.Unlock()
	if reads < 2 {
		t.Errorf("the history was read %d time(s), want a second page", reads)
	}
}

// TestLineAddOpenCodeTurnTheServiceDoesNotCertifyRollsBack: the run exited 0 and its
// events are clean, but the server does not show the turn to have completed: the
// session was interrupted or failed, its last step went on, or the service cannot be
// asked at all. The add fails and undoes itself, the session deleted.
func TestLineAddOpenCodeTurnTheServiceDoesNotCertifyRollsBack(t *testing.T) {
	cases := []struct {
		name  string
		setup func(b *ocLineBench)
		want  string
	}{
		{"the session was interrupted", func(b *ocLineBench) {
			b.svc.set(func() {
				b.svc.history = []ocHist{{kind: "user"}, {kind: "assistant", finish: "error"}, {kind: "idle", outcome: "interrupted"}}
			})
		}, `outcome "interrupted"`},
		{"the session failed", func(b *ocLineBench) {
			b.svc.set(func() {
				b.svc.history = []ocHist{{kind: "user"}, {kind: "assistant", finish: "error"}, {kind: "idle", outcome: "failed"}}
			})
		}, `outcome "failed"`},
		{"the last step went on", func(b *ocLineBench) {
			b.svc.set(func() {
				b.svc.history = []ocHist{{kind: "user"}, {kind: "assistant", finish: "tool-calls"}, {kind: "idle", outcome: "succeeded"}}
			})
		}, `finished with "tool-calls"`},
		{"the service answers 500", func(b *ocLineBench) { b.svc.set(func() { b.svc.down = true }) }, "500"},
		{"the service is gone", func(b *ocLineBench) { b.svc.srv.Close() }, "could not be found to ask it"},
		{"the service left no file", func(b *ocLineBench) {
			if err := os.Remove(filepath.Join(b.root, "xdg", "opencode", "service.json")); err != nil {
				t.Fatal(err)
			}
		}, "no OpenCode 2 service is registered"},
		{"the service does not know the session", func(b *ocLineBench) { b.svc.set(func() { b.svc.anySession = false }) }, "not found"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := newOCLineBench(t)
			lineUp(t, b.lineBench, "demo")
			co := b.gitCheckout(t, "co")
			c.setup(b)
			before := b.snap(t, co)
			res := b.run(t, b.ladd("demo", "build", "opencode", co)...)
			if res.code != 1 {
				t.Fatalf("exit %d, want 1\nstdout: %s\nstderr: %s", res.code, res.stdout, res.stderr)
			}
			for _, want := range []string{c.want, "the turn is not shown to have completed", "rolled back"} {
				if !strings.Contains(res.stderr, want) {
					t.Errorf("stderr does not say %q:\n%s", want, res.stderr)
				}
			}
			// The nick's directory stays, its lock file being kept on purpose, and the
			// rollback says it left it and does not say it removed it.
			nickDir := filepath.Join(b.base, "build")
			if _, err := os.Stat(nickDir); err != nil {
				t.Errorf("the nick's directory is gone: %v", err)
			}
			for _, line := range strings.Split(res.stderr, "\n") {
				if strings.Contains(line, "rolled back:") && strings.Contains(line, "directory "+nickDir) {
					t.Errorf("the rollback says it removed the nick's directory, which stays:\n%s", line)
				}
			}
			if !strings.Contains(res.stderr, "left directory "+nickDir+" as it is") {
				t.Errorf("the rollback does not say it left the nick's directory:\n%s", res.stderr)
			}
			if strings.Contains(res.stdout+res.stderr, ocPassword) {
				t.Errorf("the service's password is in the output:\n%s\n%s", res.stdout, res.stderr)
			}
			if res.stdout != "" {
				t.Errorf("a failed run printed on stdout: %q", res.stdout)
			}
			b.wantDeletes(t, 1, idOfRun(t, b))
			if got := b.snap(t, co); got != before {
				t.Fatalf("the run left a trace.\nbefore:\n%s\nafter:\n%s", before, got)
			}
		})
	}
}

// TestLineAddOpenCodeFailureRollsEverythingBack: an opencode that fails, names a
// session other than the one it was given or none, or ends without a completed
// turn is exit 1 and a machine as it was, the checkout, the record and $TMPDIR
// included, with the session it was given the id of deleted whatever it did.
func TestLineAddOpenCodeFailureRollsEverythingBack(t *testing.T) {
	cases := []struct {
		name string
		mode string
		want string
	}{
		{"opencode exits nonzero", "fail", "boom: not logged in"},
		{"no sessionID in any event", "nosession", "no event carries a sessionID"},
		{"a session id that is not one", "badid", "and not only"},
		{"a session id that is too short", "shortid", "and not only"},
		{"a session id that begins with a dash", "dashid", "and not only"},
		// Events that name another session do not show the turn to be this session's,
		// whatever the run exited with.
		{"events that carry two session ids", "mismatch", "and not only"},
		{"events that carry two session ids, and the run fails", "mismatchfail", "bad turn"},
		{"a step_finish that names no session, after others did", "mixednoid", "event(s) of type step_finish carry no sessionID"},
		{"a step_start that names no session, before others did", "noidfirst", "event(s) of type step_start carry no sessionID"},
		{"the run fails after the session exists", "failafter", "the model went away"},
		{"exit 0 but an error event", "errorzero", "stream broke"},
		{"stdout past the cap", "overflow", "cut off"},
		{"control characters in the error", "ctrlerror", "UnknownError: quota[31m exceeded"},
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
			id := idOfRun(t, b)
			b.wantDeletes(t, 1, id)
			if !strings.Contains(res.stderr, "OpenCode session "+id+" (session build)") {
				t.Errorf("the rollback does not name the session it deleted:\n%s", res.stderr)
			}
			if got := strings.TrimSpace(b.file(t, "cwd-2")); got != co {
				t.Errorf("the delete ran in %q, want the checkout %q", got, co)
			}
			if got := b.snap(t, co); got != before {
				t.Fatalf("the run left a trace.\nbefore:\n%s\nafter:\n%s", before, got)
			}
		})
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
	b.wantDeletes(t, 1, idOfRun(t, b))
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

// TestOpenCodeDeleteStopsTheWholeProcessGroup: the delete is run as the turn is, a
// process group of its own that a stop takes whole, so a launcher's child does not
// outlive the command that is stopped at its bound.
func TestOpenCodeDeleteStopsTheWholeProcessGroup(t *testing.T) {
	old := openCodeDeleteTimeout
	openCodeDeleteTimeout = time.Second
	t.Cleanup(func() { openCodeDeleteTimeout = old })
	b := newOCLineBench(t)
	b.setMode(t, 1, "holdcmd")
	co := b.plainCheckout(t, "co")
	if err := openCodeDelete(b.db.opencode, "opencode", "ses_000000000000000000000000AA", co); err == nil {
		t.Fatal("a command that never ended was taken for a success")
	}
	pid, err := strconv.Atoi(strings.TrimSpace(b.file(t, "child-pid-1")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	waitGone(t, pid, "the child of the stopped session delete")
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
	b.wantDeletes(t, 1, idOfRun(t, b))
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
// ends is stopped at the bound, the error says why, and the session it was
// given is registered for deletion, as is not the record, which was never
// written.
func TestStartOpenCodeSessionEndsAHungTurnAndRegistersItsSession(t *testing.T) {
	b := newOCLineBench(t)
	b.setMode(t, 1, "hold")
	co := b.plainCheckout(t, "co")
	env := &ocEnvRecorder{}
	start := time.Now()
	_, _, err := startOpenCodeSession(context.Background(), env, openCodeSession{
		nick: "build", dir: co, bin: b.db.opencode, name: "opencode", sid: "ses_000000000000000000000000AA", title: "t", prompt: "hello\n",
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
	// The wording says no more than that the turn did not end.
	if strings.Contains(err.Error(), "ask") || strings.Contains(err.Error(), "by itself") {
		t.Errorf("the error says why the turn hung, which it does not know: %v", err)
	}
	const sid = "ses_000000000000000000000000AA"
	if len(env.undos) != 1 || !strings.Contains(env.undos[0], sid) {
		t.Fatalf("registered %v, want the one session %s", env.undos, sid)
	}
	if err := env.do[0](); err != nil {
		t.Fatalf("the registered delete failed: %v", err)
	}
	b.wantDeletes(t, 1, sid)
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
	if got.SessionID != id || len(got.Others) != 0 || got.Answer != "two\nthree" || got.Failure != "" {
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
	// An event that names no session certifies nothing: it is not an answer, and its
	// type is kept for the error.
	got = parseOpenCodeTurn(strings.Join([]string{
		ev("step_start", id, `{}`),
		ev("text", id, `{"text":"x"}`),
		`{"type":"step_finish","timestamp":3,"part":{"reason":"stop"}}`,
		`{"type":"text","part":{"text":"unnamed"}}`,
	}, "\n"))
	if got.SessionID != id || got.Answer != "x" || strings.Join(got.NoID, ",") != "step_finish,text" {
		t.Errorf("an event with no session parsed as %+v", got)
	}
}

// ocSampleID is a session id of the shape OpenCode makes, for the records a test writes by hand.
const ocSampleID = "ses_00000000000000000000000001"

// TestLineSessionCheckForOpenCode: the record of an OpenCode session is held to
// what line add writes: a session id as its thread, no launch file, a reply
// directory of the line.
func TestLineSessionCheckForOpenCode(t *testing.T) {
	base := t.TempDir()
	good := func() lineRecord {
		rec := goodLineRecord(base)
		rec.Sessions = append([]lineSession(nil), rec.Sessions...)
		rec.Sessions = append(rec.Sessions, lineSession{Nick: "oc", Harness: "opencode", Model: "openai/gpt-5", Checkout: "/work/co",
			Thread: ocSampleID, ReplyDir: "/tmp/parley-line-demo-456", Files: []lineFile{}})
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
		{"a session id on a Codex session", func(r *lineRecord) { r.Sessions[1].Thread = ocSampleID }, "UUID"},
		{"a session id on a Claude session", func(r *lineRecord) { r.Sessions[0].Thread = ocSampleID }, "a Claude session holds no thread"},
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
		"opencode session delete '" + s.Thread + "'    # session build",
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
		b, s, sr := setup(t)
		const other = "ses_AbCdEfGhIjKlMnOpQrStUvWxYz"
		if err := sr.Write(other); err != nil {
			t.Fatal(err)
		}
		res := mustLine(t, b.lineBench, b.ldrop("demo", "build")...)
		wantInOrder(t, res.stdout, []string{
			"Left in place:",
			fmt.Sprintf("OpenCode session record %s: it holds session %s, not session build's %s, so it is another session's or no one's", sr.Path(), other, s.Thread),
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
// chair's message into it as one prompt, whose text is what parley wait prints. The
// runbook's pump line has no port: the pump finds the service from its file, which
// the test writes for the fake server in the state home the runbook's environment
// names.
func TestLineAddOpenCodePumpFindsTheRecordedSession(t *testing.T) {
	addr := startErgo(t)
	b := newOCLineBench(t)
	b.db.server = addr
	lineUp(t, b.lineBench, "demo")
	co := b.gitCheckout(t, "co")
	res := mustLine(t, b.lineBench, b.ladd("demo", "build", "opencode", co)...)
	recorded := sessionByNick(t, b.record(t, "demo"), "build").Thread
	if !ocIDRE.MatchString(recorded) {
		t.Fatalf("recorded session %q is not one OpenCode makes", recorded)
	}

	srv := newOCServiceServer(t)
	srv.set(func() { srv.known = map[string]bool{recorded: true} })
	stateHome := t.TempDir()
	writeService(t, stateHome, srv.url(), ocPID, 0o600)
	words := shellWords(t, lineWith(t, res.stdout, " opencode --stream "))
	if words[0] != binPath || words[1] != "opencode" {
		t.Fatalf("the pump line starts with %q, want this binary and opencode", words[:2])
	}
	for _, w := range words {
		if w == "--port" {
			t.Fatalf("the pump line names a port: %q", words)
		}
	}
	_, _, stderr := startOpencode(t, append(append(ocEnv(), b.env()...), "XDG_STATE_HOME="+stateHome), append(append([]string(nil), words[2:]...), "--debounce", "100ms")...)
	waitForStderr(t, stderr, "into OpenCode session "+recorded)

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
