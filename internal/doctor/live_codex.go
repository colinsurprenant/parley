package doctor

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/colinsurprenant/parley/internal/codex"
	"github.com/colinsurprenant/parley/internal/state"
)

// livePrompt is the one model turn the live check spends. It asks for the
// thread id the way a seat's shell would see it, so the answer is the proof
// that the id Codex announces and the id its sessions run under are one.
const livePrompt = "Run the shell command `printenv CODEX_THREAD_ID`, then answer with its output only."

const (
	liveSurface     = "codex live"
	liveTurnTimeout = 3 * time.Minute
	deleteTimeout   = time.Minute
	// maxLiveOutput bounds what is kept of the turn's JSONL: a one-command turn
	// is a few kilobytes, and a runaway one is already a failure.
	maxLiveOutput = 1 << 20
)

// notExercised is said in a live report whose turn passed: two Codex surfaces
// parley uses cannot be tried by a turn of their own. A turn that failed says
// nothing of the surfaces it was to be a sample of, so it carries no such line.
const notExercised = "not exercised live: codex exec resume (parley never uses it) and codex queue (it needs a running TUI); their flags are checked against --help only"

// liveCodex spends exactly one model turn: a fresh Codex thread, in a temp
// directory, that runs `printenv CODEX_THREAD_ID`. It checks that the
// thread.started event carries a thread id and that the model's command output
// or answer carries the same one, then deletes the throwaway thread so the turn
// leaves no session behind.
func liveCodex(ctx context.Context, bin string) []Result {
	path, err := Resolve(bin)
	if err != nil {
		return []Result{{Fail, liveSurface, err.Error()}}
	}
	// The turn makes a thread that only a delete that does not prompt can clean
	// up, so that delete is shown to exist before the turn is spent.
	if stdout, stderr, err := probe(ctx, path, "delete", "--help"); err != nil {
		return []Result{{Fail, liveSurface, fmt.Sprintf("codex delete --help: %s; the live turn is not run, since its thread could not be deleted", describe(err, stdout, stderr))}}
	} else if !helpFlags(stdout).has("--force") {
		return []Result{{Fail, liveSurface, "codex delete has no --force, so it would prompt; the live turn is not run, since its thread could not be deleted"}}
	}
	tmp, err := os.MkdirTemp("", "parley-doctor-codex-")
	if err != nil {
		return []Result{{Fail, liveSurface, fmt.Sprintf("make a directory for the turn: %v", err)}}
	}
	defer os.RemoveAll(tmp)

	turn, runErr := runTurn(ctx, path, tmp)
	rs := judgeTurn(turn, runErr)
	passed := !Failed(rs)
	// Deleted whenever the turn got as far as announcing a thread, whatever
	// else went wrong with it, and with the caller's cancellation set aside: an
	// interrupted check is the one most likely to have left a thread behind. Only
	// a well-formed id is handed to codex delete: anything else could be read as
	// one of its options (--all) rather than as the throwaway session.
	if state.ValidThreadID(turn.threadID) {
		rs = append(rs, deleteThread(context.WithoutCancel(ctx), path, turn.threadID))
	}
	if passed {
		rs = append(rs, Result{OK, liveSurface, notExercised})
	}
	return rs
}

// turnResult is what the turn's JSONL said.
type turnResult struct {
	threadID string
	// said holds the command outputs and the answers of the turn's completed
	// items, the places the thread id is looked for.
	said []string
	// events counts the lines by event type, for a report of a turn that did not
	// look like the one expected.
	events []string
	// failure is the type and message of an error or a failed turn, if the
	// stream had one, made safe to print.
	failure string
	// completed is whether a turn.completed event was seen; truncated, whether the
	// output was cut off at maxLiveOutput, so that what follows the cut is unseen.
	completed, truncated bool
	// stderr is the last of what codex wrote to stderr.
	stderr string
}

func runTurn(ctx context.Context, bin, dir string) (turnResult, error) {
	ctx, cancel := context.WithTimeout(ctx, liveTurnTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "exec", "--json", "--skip-git-repo-check", "-C", dir, "-s", "read-only", livePrompt)
	cmd.Dir = dir
	// Closed, so a turn that waits for input ends instead of waiting for a person.
	cmd.Stdin = nil
	// The id the model reads must be the one codex gave this thread, so one
	// inherited from a Codex session that is running this check is not passed on.
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "CODEX_THREAD_ID=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	stdout, stderr := &limitBuffer{limit: maxLiveOutput}, &limitBuffer{limit: 4096}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	turn := parseTurn(stdout.String())
	turn.stderr = strings.TrimSpace(stderr.String())
	turn.truncated = stdout.truncated
	if err != nil && ctx.Err() == context.DeadlineExceeded {
		err = fmt.Errorf("no end to the turn within %s: %w", liveTurnTimeout, err)
	}
	return turn, err
}

// limitBuffer keeps the first limit bytes written to it. truncated says whether
// anything was discarded.
type limitBuffer struct {
	limit     int
	buf       bytes.Buffer
	truncated bool
}

func (b *limitBuffer) Write(p []byte) (int, error) {
	room := max(b.limit-b.buf.Len(), 0)
	b.buf.Write(p[:min(len(p), room)])
	if len(p) > room {
		b.truncated = true
	}
	return len(p), nil
}

func (b *limitBuffer) String() string { return b.buf.String() }

// parseTurn reads the run's JSONL with the parse `parley table new` and `parley
// build new` share
// (codex.ParseTurn, which has the shapes codex 0.161.0 printed for this very
// turn) into the fields the check judges.
func parseTurn(out string) turnResult {
	t := codex.ParseTurn(out)
	return turnResult{threadID: t.ThreadID, said: t.Said, events: t.Events, failure: t.Failure, completed: t.Completed}
}

// judgeTurn turns what the turn did into report lines. A turn that carries an
// error or a failed turn failed, whatever codex exited with.
func judgeTurn(t turnResult, runErr error) []Result {
	if runErr != nil {
		detail := "codex exec: " + runErr.Error()
		if t.failure != "" {
			detail += ": " + t.failure
		} else if t.stderr != "" {
			detail += ": " + codex.PlainLine(firstLine(t.stderr))
		}
		return []Result{{Fail, liveSurface, detail}}
	}
	if t.failure != "" {
		return []Result{{Fail, liveSurface, "codex exec exited 0, but the turn failed: " + t.failure}}
	}
	if t.threadID == "" {
		return []Result{{Fail, liveSurface, fmt.Sprintf("no thread.started event carried a thread id; the turn's events were: %s", eventList(t.events))}}
	}
	if !state.ValidThreadID(t.threadID) {
		return []Result{{Fail, liveSurface, fmt.Sprintf("thread.started carried %q, which is not a thread id, so no codex delete is run for it", t.threadID)}}
	}
	// An exit 0 says codex ended, not that the turn did.
	if t.truncated {
		return []Result{{Fail, liveSurface, fmt.Sprintf("codex exec exited 0, but its output passed %d bytes and was cut off, so the turn is not shown to have completed", maxLiveOutput)}}
	}
	if !t.completed {
		return []Result{{Fail, liveSurface, fmt.Sprintf("codex exec exited 0, but its output has no turn.completed event, so the turn is not shown to have completed; the turn's events were: %s", eventList(t.events))}}
	}
	for _, s := range t.said {
		if strings.Contains(s, t.threadID) {
			return []Result{{OK, liveSurface, fmt.Sprintf("thread.started carried thread %s, and the model's command output or answer carried the same id", t.threadID)}}
		}
	}
	return []Result{{Fail, liveSurface, fmt.Sprintf("thread.started carried thread %s, but no command output or answer carried that id (they said %s); the turn's events were: %s", t.threadID, quoted(t.said), eventList(t.events))}}
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

func eventList(types []string) string {
	if len(types) == 0 {
		return "none"
	}
	return strings.Join(types, ", ")
}

// quoted lists the non-empty strings in s, trimmed, for a report line.
func quoted(s []string) string {
	var parts []string
	for _, x := range s {
		if x = strings.TrimSpace(x); x != "" {
			parts = append(parts, fmt.Sprintf("%q", x))
		}
	}
	if len(parts) == 0 {
		return "nothing"
	}
	return strings.Join(parts, ", ")
}

// deleteThread removes the throwaway session. --force is what keeps codex from
// asking, and the call has no stdin to answer with, so it cannot wait on a
// prompt; a codex that no longer has --force is reported rather than guessed at.
func deleteThread(ctx context.Context, bin, id string) Result {
	left := fmt.Sprintf("the throwaway thread %s is still there: remove it with codex delete %s", id, id)
	stdout, stderr, err := probe(ctx, bin, "delete", "--help")
	if err != nil {
		return Result{Fail, liveSurface, fmt.Sprintf("codex delete --help: %s; %s", describe(err, stdout, stderr), left)}
	}
	if !helpFlags(stdout).has("--force") {
		return Result{Fail, liveSurface, "codex delete has no --force, so it would prompt; " + left}
	}
	stdout, stderr, err = probeFor(ctx, deleteTimeout, bin, "delete", "--force", id)
	if err != nil {
		return Result{Fail, liveSurface, fmt.Sprintf("codex delete --force %s: %s; %s", id, describe(err, stdout, stderr), left)}
	}
	return Result{OK, liveSurface, "deleted the throwaway thread " + id}
}
