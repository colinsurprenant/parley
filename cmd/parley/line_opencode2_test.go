package main

import (
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

// oc2IDRE is the session id parley makes for an OpenCode 2 session: ses_, twelve
// hex digits of time, fourteen of letters and digits.
var oc2IDRE = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)

// ocBench2 is the OpenCode line bench with an opencode that says it is 2.0.32.
func newOC2LineBench(t *testing.T) *ocLineBench {
	t.Helper()
	b := newOCLineBench(t)
	b.write(t, "version", "opencode v2.0.32")
	return b
}

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

// TestLineAddOpenCode2Session is the acceptance run for an OpenCode 2 session on
// the fake opencode: the session id is parley's own and goes to the run with
// --session, it is the id recorded, printed and checked in the events, nothing is
// written in the checkout, and the runbook has no port.
func TestLineAddOpenCode2Session(t *testing.T) {
	b := newOC2LineBench(t)
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
	if n := b.listCalls(t); n != 0 {
		t.Errorf("sessions were listed %d times on OpenCode 2, where the id is known", n)
	}
	id := idOfRun(t, b)
	if !oc2IDRE.MatchString(id) {
		t.Fatalf("the session id %q is not one OpenCode makes", id)
	}
	token := strings.TrimPrefix(filepath.Base(sessionByNick(t, b.record(t, "demo"), "build").ReplyDir), "parley-line-demo-")
	want := []string{"run", "--format", "json", "--title", "parley line demo build " + token, "-m", "anthropic/claude-sonnet-4-5", "--session", id}
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
		"OpenCode 2 session " + id + ".",
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
		"OpenCode 2 allows external_directory by default, so writing it asks nothing.",
		`{ "permissions": [ { "action": "external_directory", "resource": "*", "effect": "ask" } ] }`,
		"opencode run --session, which adds no permission rules of its own to the session",
		"it rejected any permission ask and cancelled any question",
		"The service's password is in its service file (mode 600)",
		"sends it to the service's loopback address only; it is never printed",
		"verified against 2.0.32 in OpenCode's simulation mode, which blocks all model traffic, and not yet on a live model",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("the runbook does not say %q:\n%s", want, res.stdout)
		}
	}
	for _, unwanted := range []string{"--port", "Allow always", "denies it OpenCode's question tool", "OPENCODE_SERVER_PASSWORD, if you use one"} {
		if strings.Contains(res.stdout, unwanted) && unwanted != "--port" {
			t.Errorf("the runbook mentions %q, which is OpenCode 1's:\n%s", unwanted, res.stdout)
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

// TestLineAddOpenCode2ARunThatFailsLikeASimulatedTurnRollsBackTheChosenID: the turn
// of a 2.0.32 server in simulation mode (it is the one turn that can be run
// without a model) retries and ends in an error event and status 1. The add fails
// with the error's own words, deletes exactly the id it chose, and looks for
// nothing.
func TestLineAddOpenCode2ARunThatFailsLikeASimulatedTurnRollsBackTheChosenID(t *testing.T) {
	b := newOC2LineBench(t)
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
	if n := b.listCalls(t); n != 0 {
		t.Errorf("sessions were listed %d times; the id is known", n)
	}
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

// TestLineAddOpenCode2ARunThatMadeNoSessionIsNotAFailureToRollBack: a run that fails
// before it makes the session leaves nothing to delete, and the delete of an id
// the server does not know ("Session not found", status 1) is not told as one.
func TestLineAddOpenCode2ARunThatMadeNoSessionIsNotAFailureToRollBack(t *testing.T) {
	b := newOC2LineBench(t)
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

// TestLineAddOpenCode2ADeleteThatFailsIsToldWithTheCommand: a delete that fails for
// another reason is a failure, with the command to run by hand.
func TestLineAddOpenCode2ADeleteThatFailsIsToldWithTheCommand(t *testing.T) {
	b := newOC2LineBench(t)
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

// TestLineAddOpenCode2EventsNamingAnotherSessionFailTheTurn: the id was parley's
// choice, so every event must carry it; one that names another session does not
// show the turn to be the session's, and the session parley made is still the one
// deleted.
func TestLineAddOpenCode2EventsNamingAnotherSessionFailTheTurn(t *testing.T) {
	b := newOC2LineBench(t)
	lineUp(t, b.lineBench, "demo")
	co := b.gitCheckout(t, "co")
	b.write(t, "id", "ses_00000000000000000000000042")
	res := b.run(t, b.ladd("demo", "build", "opencode", co)...)
	if res.code != 1 {
		t.Fatalf("exit %d, want 1\n%s", res.code, res.stderr)
	}
	id := idOfRun(t, b)
	for _, want := range []string{"its events name session \"ses_00000000000000000000000042\" and not " + id, "the id this run chose"} {
		if !strings.Contains(res.stderr, want) {
			t.Errorf("stderr does not say %q:\n%s", want, res.stderr)
		}
	}
	b.wantDeletes(t, 1, id)
}

// TestLineAddOpenCode2InterruptedDeletesTheChosenID: an interrupt, which kills the
// run before it may have named anything, still deletes the one id, and looks for
// nothing; the child the opencode started goes with it.
func TestLineAddOpenCode2InterruptedDeletesTheChosenID(t *testing.T) {
	b := newOC2LineBench(t)
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
	if n := b.listCalls(t); n != 0 {
		t.Errorf("sessions were listed %d times; the id is known", n)
	}
}

// TestParseOpenCodeTurnReadsOpenCode2sEvents: the events of a failed 2.0.32 turn
// are captured from a server in simulation mode (the first, a step_start, and the
// last, an error); the events of a turn that completed are shaped from the parts
// 2.0.32's non-interactive run writes (packages/cli/src/run/noninteractive.ts), as
// no model turn could be run. Both are read by the parser OpenCode 1's events are.
func TestParseOpenCodeTurnReadsOpenCode2sEvents(t *testing.T) {
	const id = "ses_ea83ab198ffeRIhi6BZUsPDq5i"
	failed := `{"type":"step_start","timestamp":1792473911119,"sessionID":"` + id + `","part":{"id":"prt_157c54f4f001c3XPVNJjxsFi2f","sessionID":"` + id + `","messageID":"msg_157c54f2a001RFnoHjhjZMb1a1","type":"step-start","snapshot":"4b825dc642cb6eb9a060e54bf8d69288fbee4904"}}
{"type":"step_start","timestamp":1792473913165,"sessionID":"` + id + `","part":{"id":"prt_157c5574d0016VGpP5sOzWlFZ9","sessionID":"` + id + `","messageID":"msg_157c54f2a001RFnoHjhjZMb1a1","type":"step-start","snapshot":"4b825dc642cb6eb9a060e54bf8d69288fbee4904"}}
{"type":"error","timestamp":1792473995534,"sessionID":"` + id + `","error":{"type":"provider.transport","message":"Transport: Simulation denied unregistered network destination: POST https://opencode.ai/inference/openai/v1/chat/completions (POST https://opencode.ai/inference/openai/v1/chat/completions)"}}
`
	got := parseOpenCodeTurn(failed)
	if got.SessionID != id || len(got.Others) != 0 || len(got.NoID) != 0 || got.Steps != 0 || !strings.HasPrefix(got.Failure, "Transport: Simulation denied unregistered network destination") {
		t.Errorf("a failed 2.x turn parsed as %+v", got)
	}

	done := `{"type":"step_start","timestamp":1,"sessionID":"` + id + `","part":{"id":"prt_1","sessionID":"` + id + `","messageID":"msg_1","type":"step-start","snapshot":"4b82"}}
{"type":"text","timestamp":2,"sessionID":"` + id + `","part":{"id":"prt_2","sessionID":"` + id + `","messageID":"msg_1","type":"text","text":"ready, waiting","time":{"start":1,"end":2}}}
{"type":"step_finish","timestamp":3,"sessionID":"` + id + `","part":{"id":"prt_3","sessionID":"` + id + `","messageID":"msg_1","type":"step-finish","reason":"stop","snapshot":"4b82","cost":0,"tokens":{"input":1,"output":1}}}
`
	got = parseOpenCodeTurn(done)
	if got.SessionID != id || got.Steps != 1 || got.LastReason != "stop" || got.Answer != "ready, waiting" || got.Failure != "" || len(got.NoID) != 0 {
		t.Errorf("a completed 2.x turn parsed as %+v", got)
	}
}
