package doctor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const threadID = "0199c0de-c0de-7c0d-8c0d-ec0dec0dec0d"

// turnJSONL is the output of a one-command turn in the shape codex 0.161.0
// prints: the thread, the turn, the command with what it printed, the answer.
func turnJSONL(started, commandOutput, answer string) string {
	var b strings.Builder
	if started != "" {
		b.WriteString(`{"type":"thread.started","thread_id":"` + started + `"}` + "\n")
	}
	b.WriteString(`{"type":"turn.started"}` + "\n")
	b.WriteString(`{"type":"item.started","item":{"id":"item_0","type":"command_execution","command":"/bin/bash -lc 'printenv CODEX_THREAD_ID'","aggregated_output":"","exit_code":null,"status":"in_progress"}}` + "\n")
	b.WriteString(`{"type":"item.completed","item":{"id":"item_0","type":"command_execution","command":"/bin/bash -lc 'printenv CODEX_THREAD_ID'","aggregated_output":"` + commandOutput + `\n","exit_code":0,"status":"completed"}}` + "\n")
	b.WriteString(`{"type":"item.completed","item":{"id":"item_1","type":"agent_message","text":"` + answer + `"}}` + "\n")
	b.WriteString(`{"type":"turn.completed","usage":{"input_tokens":35188,"cached_input_tokens":24576,"cache_write_input_tokens":0,"output_tokens":112,"reasoning_output_tokens":49}}` + "\n")
	return b.String()
}

func liveRun(t *testing.T, f fakeCodex) ([]Result, string) {
	t.Helper()
	bin, log := f.install(t, t.TempDir())
	rs := Check(context.Background(), Options{Codex: true, CodexBin: bin, Live: true})
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	return rs, string(raw)
}

func countCalls(log, prefix string) int {
	n := 0
	for _, l := range strings.Split(log, "\n") {
		if strings.HasPrefix(l, prefix) {
			n++
		}
	}
	return n
}

func TestLiveCodexAMatchingIdIsOKAndTheThreadIsDeleted(t *testing.T) {
	rs, log := liveRun(t, fakeCodex{version: CodexVerified, exec: turnJSONL(threadID, threadID, threadID)})
	if Failed(rs) {
		t.Fatalf("a matching id failed:\n%s", render(rs))
	}
	if n := countCalls(log, "exec --json"); n != 1 {
		t.Errorf("%d model turns were spent, want exactly 1:\n%s", n, log)
	}
	if !strings.Contains(log, "delete --force "+threadID+"\n") {
		t.Errorf("the throwaway thread was not deleted with --force:\n%s", log)
	}
	for _, l := range strings.Split(log, "\n") {
		if (strings.HasPrefix(l, "queue") || strings.HasPrefix(l, "resume") || strings.HasPrefix(l, "exec resume")) && !strings.HasSuffix(l, "--help") {
			t.Errorf("a surface that is not exercised live was run: %q", l)
		}
	}
	var said bool
	for _, r := range rs {
		if r.Surface == liveSurface && strings.Contains(r.Detail, "codex exec resume") && strings.Contains(r.Detail, "codex queue") && strings.Contains(r.Detail, "not exercised") {
			said = true
		}
	}
	if !said {
		t.Errorf("the output does not say resume and queue are not exercised live:\n%s", render(rs))
	}
}

func TestLiveCodexRunsTheDocumentedCommandInATempDirectory(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	_, log := liveRun(t, fakeCodex{version: CodexVerified, exec: turnJSONL(threadID, threadID, threadID)})
	var line string
	for _, l := range strings.Split(log, "\n") {
		if strings.HasPrefix(l, "exec --json") {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("no turn was run:\n%s", log)
	}
	fields := strings.Fields(line)
	if len(fields) < 9 || strings.Join(fields[:3], " ") != "exec --json --skip-git-repo-check" || fields[3] != "-C" || fields[5] != "-s" || fields[6] != "read-only" {
		t.Errorf("the turn is %q, want codex exec --json --skip-git-repo-check -C <dir> -s read-only <prompt>", line)
	}
	if !strings.HasPrefix(fields[4], tmp) && !strings.HasPrefix(fields[4], "/private"+tmp) {
		t.Errorf("the turn ran in %s, not under the temp directory %s", fields[4], tmp)
	}
	if !strings.Contains(line, "printenv CODEX_THREAD_ID") {
		t.Errorf("the prompt does not ask for CODEX_THREAD_ID: %q", line)
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Errorf("the live check left %d entries in the temp directory", len(left))
	}
}

func TestLiveCodexTheAnswerAloneMayCarryTheId(t *testing.T) {
	rs, _ := liveRun(t, fakeCodex{version: CodexVerified, exec: turnJSONL(threadID, "", threadID)})
	if Failed(rs) {
		t.Fatalf("an answer carrying the id failed:\n%s", render(rs))
	}
}

func TestLiveCodexAMismatchingIdIsAFAIL(t *testing.T) {
	other := "0199c0de-0000-7000-8000-000000000000"
	rs, log := liveRun(t, fakeCodex{version: CodexVerified, exec: turnJSONL(threadID, other, other)})
	r := find(t, rs, liveSurface)
	if r.Level != Fail || !strings.Contains(r.Detail, threadID) || !strings.Contains(r.Detail, other) {
		t.Fatalf("got %q, want a FAIL naming both ids", r)
	}
	if !strings.Contains(log, "delete --force "+threadID) {
		t.Errorf("a failed turn left its thread behind:\n%s", log)
	}
}

func TestLiveCodexAnIdThatIsNotAUUIDIsAFAILAndNeverReachesDelete(t *testing.T) {
	for _, bad := range []string{"--all", "not-a-uuid", "0199c0de-c0de-7c0d-8c0d-ec0dec0dec0d\\u001b[2J"} {
		t.Run(bad, func(t *testing.T) {
			rs, log := liveRun(t, fakeCodex{version: CodexVerified, exec: turnJSONL(bad, bad, bad)})
			r := find(t, rs, liveSurface)
			if r.Level != Fail || !strings.Contains(r.Detail, "not a thread id") {
				t.Fatalf("got %q, want a FAIL saying the id is not one", r)
			}
			if strings.ContainsRune(r.Detail, 0x1b) {
				t.Errorf("the announced value reached the report raw: %q", r)
			}
			if n := countCalls(log, "delete --force"); n != 0 {
				t.Errorf("codex delete ran %d times for a value that is not a thread id:\n%s", n, log)
			}
		})
	}
}

func TestLiveCodexNoThreadStartedIsAFAIL(t *testing.T) {
	rs, log := liveRun(t, fakeCodex{version: CodexVerified, exec: turnJSONL("", threadID, threadID)})
	r := find(t, rs, liveSurface)
	if r.Level != Fail || !strings.Contains(r.Detail, "thread.started") {
		t.Fatalf("got %q", r)
	}
	if countCalls(log, "delete --force") != 0 {
		t.Errorf("a delete was attempted with no thread id:\n%s", log)
	}
}

func TestLiveCodexAThreadStartedWithoutAnIdIsAFAIL(t *testing.T) {
	rs, _ := liveRun(t, fakeCodex{version: CodexVerified, exec: `{"type":"thread.started"}` + "\n" + turnJSONL("", threadID, threadID)})
	if r := find(t, rs, liveSurface); r.Level != Fail {
		t.Fatalf("got %q", r)
	}
}

func TestLiveCodexATurnThatFailsIsAFAILAndStillDeletes(t *testing.T) {
	out := `{"type":"thread.started","thread_id":"` + threadID + `"}` + "\n" + `{"type":"turn.failed","error":{"message":"usage limit reached"}}` + "\n"
	rs, log := liveRun(t, fakeCodex{version: CodexVerified, exec: out, execExit: 1})
	r := find(t, rs, liveSurface)
	if r.Level != Fail || !strings.Contains(r.Detail, "usage limit reached") {
		t.Fatalf("got %q", r)
	}
	if !strings.Contains(log, "delete --force "+threadID) {
		t.Errorf("the thread was not deleted:\n%s", log)
	}
}

// TestLiveCodexATurnThatFailsEvenThoughCodexExitedZeroIsAFAIL: an error or a
// turn.failed event is the turn's failure, whatever the exit status says. The
// thread is still deleted, and a failed turn says nothing of the surfaces it was
// to sample, so the not-exercised line is not printed.
func TestLiveCodexATurnThatFailsEvenThoughCodexExitedZeroIsAFAIL(t *testing.T) {
	for name, event := range map[string]string{
		"turn.failed": `{"type":"turn.failed","error":{"message":"usage limit reached"}}`,
		"error":       `{"type":"error","message":"usage limit reached"}`,
	} {
		t.Run(name, func(t *testing.T) {
			rs, log := liveRun(t, fakeCodex{version: CodexVerified, exec: turnJSONL(threadID, threadID, threadID) + event + "\n", execExit: 0})
			r := find(t, rs, liveSurface)
			if r.Level != Fail || !strings.Contains(r.Detail, "usage limit reached") || !strings.Contains(r.Detail, name) {
				t.Fatalf("got %q, want a FAIL naming the %s and its message", r, name)
			}
			if !strings.Contains(log, "delete --force "+threadID) {
				t.Errorf("the thread was not deleted:\n%s", log)
			}
			for _, r := range rs {
				if strings.Contains(r.Detail, "not exercised") {
					t.Errorf("a failed turn printed the not-exercised line: %q", r)
				}
			}
		})
	}
}

// padLine is one JSONL line of an agent message with n bytes of text.
func padLine(n int) string {
	return `{"type":"item.completed","item":{"id":"item_9","type":"agent_message","text":"` + strings.Repeat("x", n) + `"}}` + "\n"
}

// TestLiveCodexATurnThatNeverCompletedIsAFAILAndStillDeletes: codex exiting 0
// says it ended, not that the turn did. Without a turn.completed the thread id
// and the model's answer prove nothing about a finished turn, and the throwaway
// thread is deleted all the same.
func TestLiveCodexATurnThatNeverCompletedIsAFAILAndStillDeletes(t *testing.T) {
	out := `{"type":"thread.started","thread_id":"` + threadID + `"}` + "\n" +
		`{"type":"turn.started"}` + "\n" +
		`{"type":"item.completed","item":{"id":"item_1","type":"agent_message","text":"` + threadID + `"}}` + "\n"
	rs, log := liveRun(t, fakeCodex{version: CodexVerified, exec: out, execExit: 0})
	r := find(t, rs, liveSurface)
	if r.Level != Fail || !strings.Contains(r.Detail, "no turn.completed") || !strings.Contains(r.Detail, "thread.started, turn.started, item.completed") {
		t.Fatalf("got %q, want a FAIL saying there was no turn.completed, with the events", r)
	}
	if !strings.Contains(log, "delete --force "+threadID) {
		t.Errorf("the thread was not deleted:\n%s", log)
	}
	for _, r := range rs {
		if strings.Contains(r.Detail, "not exercised") {
			t.Errorf("a turn that did not complete printed the not-exercised line: %q", r)
		}
	}
}

// TestLiveCodexOutputPastTheCapIsAFAILAndStillDeletes: what is read of the turn
// stops at maxLiveOutput, so a turn.completed beyond it is never seen, and one
// before it does not vouch for what followed. Either way the turn is not shown to
// have completed.
func TestLiveCodexOutputPastTheCapIsAFAILAndStillDeletes(t *testing.T) {
	started := `{"type":"thread.started","thread_id":"` + threadID + `"}` + "\n"
	done := `{"type":"turn.completed"}` + "\n"
	for name, out := range map[string]string{
		"turn.completed beyond the cap": started + padLine(maxLiveOutput+4096) + done,
		"turn.completed inside the cap": started + padLine(10) + done + padLine(maxLiveOutput),
	} {
		t.Run(name, func(t *testing.T) {
			rs, log := liveRun(t, fakeCodex{version: CodexVerified, exec: out, execExit: 0})
			r := find(t, rs, liveSurface)
			if r.Level != Fail || !strings.Contains(r.Detail, "cut off") {
				t.Fatalf("got %q, want a FAIL saying the output was cut off", r)
			}
			if !strings.Contains(log, "delete --force "+threadID) {
				t.Errorf("the thread was not deleted:\n%s", log)
			}
		})
	}
}

// TestLiveCodexOutputExactlyAtTheCapIsNotTruncated: a run that ends exactly at the
// cap lost nothing.
func TestLiveCodexOutputExactlyAtTheCapIsNotTruncated(t *testing.T) {
	b := &limitBuffer{limit: 8}
	if _, err := b.Write([]byte("12345678")); err != nil {
		t.Fatal(err)
	}
	if b.truncated {
		t.Error("a write that filled the buffer exactly was reported as truncated")
	}
	if _, err := b.Write([]byte("9")); err != nil {
		t.Fatal(err)
	}
	if !b.truncated || b.String() != "12345678" {
		t.Errorf("truncated %v, kept %q; want true and the first 8 bytes", b.truncated, b.String())
	}
}

// TestLiveCodexNotExercisedIsSaidOnlyOfATurnThatPassed.
func TestLiveCodexNotExercisedIsSaidOnlyOfATurnThatPassed(t *testing.T) {
	for name, f := range map[string]fakeCodex{
		"a mismatching id":      {version: CodexVerified, exec: turnJSONL(threadID, "0199c0de-0000-7000-8000-000000000000", "x")},
		"no thread":             {version: CodexVerified, exec: turnJSONL("", threadID, threadID)},
		"a nonzero exit":        {version: CodexVerified, exec: turnJSONL(threadID, threadID, threadID), execExit: 1},
		"a failed delete only":  {version: CodexVerified, exec: turnJSONL(threadID, threadID, threadID), deleteExit: 1},
		"a passing turn":        {version: CodexVerified, exec: turnJSONL(threadID, threadID, threadID)},
		"a passing turn, quiet": {version: CodexVerified, exec: turnJSONL(threadID, "", threadID)},
	} {
		t.Run(name, func(t *testing.T) {
			rs, _ := liveRun(t, f)
			said := false
			for _, r := range rs {
				said = said || strings.Contains(r.Detail, "not exercised")
			}
			turnPassed := strings.HasPrefix(name, "a passing") || name == "a failed delete only"
			if said != turnPassed {
				t.Errorf("not-exercised said = %v, want %v:\n%s", said, turnPassed, render(rs))
			}
		})
	}
}

// TestLiveCodexStripsControlCharactersFromWhatItPrints: codex's error text and
// stderr are printed on the human's terminal.
func TestLiveCodexStripsControlCharactersFromWhatItPrints(t *testing.T) {
	out := `{"type":"thread.started","thread_id":"` + threadID + `"}` + "\n" +
		`{"type":"turn.failed","error":{"message":"limit\u001b[31m reached\u0007"}}` + "\n"
	rs, _ := liveRun(t, fakeCodex{version: CodexVerified, exec: out})
	r := find(t, rs, liveSurface)
	if r.Level != Fail || !strings.Contains(r.Detail, "limit[31m reached") {
		t.Fatalf("got %q", r)
	}
	if strings.ContainsAny(render(rs), "\x1b\x07") {
		t.Errorf("a control character reached the report: %q", render(rs))
	}
	// An event type from the run, as the unexpected-events report lists them.
	weird := `{"type":"thread.started","thread_id":"` + threadID + `"}` + "\n" + `{"type":"odd\u001b[2Jevent"}` + "\n"
	rs, _ = liveRun(t, fakeCodex{version: CodexVerified, exec: weird})
	r = find(t, rs, liveSurface)
	if r.Level != Fail || !strings.Contains(r.Detail, "odd[2Jevent") || strings.ContainsAny(render(rs), "\x1b") {
		t.Fatalf("got %q", r)
	}
}

func TestLiveCodexADeleteThatFailsNamesTheThread(t *testing.T) {
	rs, _ := liveRun(t, fakeCodex{version: CodexVerified, exec: turnJSONL(threadID, threadID, threadID), deleteExit: 1})
	var fail Result
	for _, r := range lines(rs, Fail) {
		fail = r
	}
	if fail.Surface != liveSurface || !strings.Contains(fail.Detail, "codex delete "+threadID) {
		t.Fatalf("want a FAIL telling the human how to remove thread %s:\n%s", threadID, render(rs))
	}
}

// TestLiveCodexWithoutForceSpendsNoTurn: a delete that would prompt cannot
// clean up the turn's thread, so the turn is never run and nothing is left.
func TestLiveCodexWithoutForceSpendsNoTurn(t *testing.T) {
	rs, log := liveRun(t, fakeCodex{version: CodexVerified, exec: turnJSONL(threadID, threadID, threadID), noForce: true})
	if n := countCalls(log, "exec --json"); n != 0 {
		t.Errorf("the live turn ran %d times though its thread could not be deleted:\n%s", n, log)
	}
	if countCalls(log, "delete --force") != 0 {
		t.Errorf("delete ran without --force in its help, so it could prompt:\n%s", log)
	}
	var said bool
	for _, r := range lines(rs, Fail) {
		if r.Surface == liveSurface && strings.Contains(r.Detail, "no --force") && strings.Contains(r.Detail, "not run") {
			said = true
		}
	}
	if !said {
		t.Errorf("no FAIL says the live turn was not run for want of --force:\n%s", render(rs))
	}
}

func TestLiveCodexIsNotRunWithoutLive(t *testing.T) {
	bin, log := fakeCodex{version: CodexVerified, exec: turnJSONL(threadID, threadID, threadID)}.install(t, t.TempDir())
	Check(context.Background(), Options{Codex: true, CodexBin: bin})
	raw, _ := os.ReadFile(log)
	if n := countCalls(string(raw), "exec --json"); n != 0 {
		t.Errorf("a static check spent %d turns:\n%s", n, raw)
	}
}

func TestLiveCodexIsNotRunWhenCodexIsNotFound(t *testing.T) {
	rs := Check(context.Background(), Options{Codex: true, CodexBin: filepath.Join(t.TempDir(), "none"), Live: true})
	if len(rs) != 1 || rs[0].Level != Fail {
		t.Errorf("want only the missing-binary FAIL:\n%s", render(rs))
	}
}

func TestLiveCodexInheritedThreadIdIsNotPassedOn(t *testing.T) {
	t.Setenv("CODEX_THREAD_ID", "inherited-from-an-outer-session")
	dir := t.TempDir()
	bin := filepath.Join(dir, "codex")
	seen := filepath.Join(dir, "env")
	script := "#!/bin/sh\ncase \"$1\" in\n--version) echo codex-cli " + CodexVerified + " ;;\nexec) case \"$2\" in --help) exit 0 ;; esac; printenv CODEX_THREAD_ID > " + seen + "; exit 1 ;;\nesac\nexit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	liveCodex(context.Background(), bin)
	if b, _ := os.ReadFile(seen); len(b) != 0 {
		t.Errorf("the turn inherited CODEX_THREAD_ID=%q", strings.TrimSpace(string(b)))
	}
}

func TestParseTurnReadsTheThreadAndWhatTheModelSaid(t *testing.T) {
	turn := parseTurn("not json\n" + turnJSONL(threadID, threadID, "the id is "+threadID))
	if turn.threadID != threadID {
		t.Errorf("thread id %q", turn.threadID)
	}
	if len(turn.said) != 4 {
		t.Errorf("said %q", turn.said)
	}
	if got := eventList(turn.events); !strings.HasPrefix(got, "(not JSON), thread.started, turn.started") {
		t.Errorf("events %q", got)
	}
}

// TestParseTurnReadsARealCodexRun: the run in testdata is what codex 0.161.0
// printed for the live turn on a real machine, so the shapes the check reads are
// the shapes that exist.
func TestParseTurnReadsARealCodexRun(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "codex-0.161.0-exec-turn.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	turn := parseTurn(string(raw))
	if turn.threadID != "01a1218f-e2ce-7173-b121-e4588840f47a" {
		t.Errorf("thread id %q", turn.threadID)
	}
	if !turn.completed || turn.truncated {
		t.Errorf("the real run is completed %v, truncated %v, want true and false", turn.completed, turn.truncated)
	}
	if got := judgeTurn(turn, nil); len(got) != 1 || got[0].Level != OK {
		t.Errorf("the real run is judged %v", got)
	}
	want := "thread.started, turn.started, item.started, item.completed, item.completed, turn.completed"
	if got := eventList(turn.events); got != want {
		t.Errorf("events %q, want %q", got, want)
	}
}
