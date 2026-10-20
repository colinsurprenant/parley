package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/colinsurprenant/parley/internal/codex"
	"github.com/colinsurprenant/parley/internal/state"
)

// Starting an OpenCode session on its primer, for `line add`: opencode run with
// the primer as the first message and a session id parley chose, its JSON events
// read, and the session deleted again if the run that made it is rolled back.
// startCodexSession is the model; what differs is what an OpenCode run says and
// what ends it.
//
// `opencode run --session <id>` continues the session, or creates it if it does not
// exist (2.0.32), so the id is parley's own: known before the run, whatever the run
// does, and the one thing the rollback deletes. Nothing is read back to learn it and
// nothing is searched for.
//
// What parley trusts here: opencode run is the human's own binary, but its output
// is read as untrusted. A turn counts as done only on its own events, every one of
// which must carry the id parley chose, never on an exit status alone.

const (
	// openCodeTurnTimeout bounds the primer turn, as seatTurnTimeout bounds a
	// Codex seat's: a primer is one short turn.
	openCodeTurnTimeout = seatTurnTimeout
	// maxOpenCodeEvents bounds the event types a refusal lists.
	maxOpenCodeEvents = 12
)

// openCodeDeleteTimeout bounds one `opencode session delete`; a variable only so
// that a test can shorten it.
var openCodeDeleteTimeout = time.Minute

// openCodeSession is one OpenCode session to start on its primer.
type openCodeSession struct {
	nick, model string
	dir         string // the checkout: where opencode runs, and so the session's project
	bin, name   string // the resolved opencode, and how the human spells it
	// sid is the session's id, which parley chose: ses_ and 26 letters or digits,
	// as OpenCode makes them (opencode.NewSessionID).
	sid          string
	title        string // the session's title: "parley line <id> <nick>"
	prompt       string // the primer
	base, stream string // where the session is recorded
	// timeout is the turn's bound; zero means openCodeTurnTimeout.
	timeout time.Duration
}

// openCodeTurn is what a run's `--format json` events say. Every event is one
// line, {"type":..,"sessionID":..,"part":{..}}: step_start and step_finish
// bracket each model step (the finish carries a reason, "stop" when the model is
// done and "tool-calls" when it goes on), text carries a message part, and error
// carries an error object.
type openCodeTurn struct {
	// SessionID is the first id an event carried; Others are any different ones.
	// NoID are the types of the events that carried none: such an event certifies
	// nothing, and is not counted below.
	SessionID string
	Others    []string
	NoID      []string
	// Steps is the number of step_finish events and LastReason the reason of the
	// last of them; Open is whether a step has begun since the last one finished,
	// which then has no reason: a turn whose last step never finished is not done.
	Steps      int
	LastReason string
	Open       bool
	// Answer is the text of the last step: what the session said last.
	Answer string
	// Failure is the first error event's message.
	Failure   string
	Events    []string
	Truncated bool
}

// parseOpenCodeTurn reads out, the stdout of opencode run --format json. A line
// that is not a JSON object with a type is not an event (opencode prints a
// warning in text on a refused permission, for one), and is passed over: the
// verdict rests on the events alone.
func parseOpenCodeTurn(out string) openCodeTurn {
	var t openCodeTurn
	var answer []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var ev struct {
			Type      string `json:"type"`
			SessionID string `json:"sessionID"`
			Part      struct {
				Text   string `json:"text"`
				Reason string `json:"reason"`
			} `json:"part"`
			Error json.RawMessage `json:"error"`
		}
		if json.Unmarshal([]byte(line), &ev) != nil || ev.Type == "" {
			continue
		}
		if typ := codex.PlainLine(ev.Type); !slices.Contains(t.Events, typ) && len(t.Events) < maxOpenCodeEvents {
			t.Events = append(t.Events, typ)
		}
		switch {
		case ev.SessionID == "":
			// An event that does not name its session is not shown to be this run's,
			// so it counts for nothing: not as a step, not as an answer.
			if typ := codex.PlainLine(ev.Type); !slices.Contains(t.NoID, typ) && len(t.NoID) < maxOpenCodeEvents {
				t.NoID = append(t.NoID, typ)
			}
			continue
		case t.SessionID == "":
			t.SessionID = ev.SessionID
		case ev.SessionID != t.SessionID && !slices.Contains(t.Others, ev.SessionID) && len(t.Others) < maxOpenCodeEvents:
			t.Others = append(t.Others, ev.SessionID)
		}
		switch ev.Type {
		case "step_start":
			answer = nil
			t.LastReason = ""
			t.Open = true
		case "text":
			if ev.Part.Text != "" {
				answer = append(answer, ev.Part.Text)
			}
		case "step_finish":
			t.Steps++
			t.LastReason = ev.Part.Reason
			t.Open = false
		case "error":
			if t.Failure == "" {
				t.Failure = openCodeErrorText(ev.Error)
			}
		}
	}
	t.Answer = strings.Join(answer, "\n")
	return t
}

// openCodeErrorText is the message of an error event's error object, {"name":..,
// "data":{"message":..}}, or the name, or the object itself clipped.
func openCodeErrorText(raw json.RawMessage) string {
	var e struct {
		Name    string `json:"name"`
		Message string `json:"message"`
		Data    struct {
			Message string `json:"message"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &e) == nil {
		switch {
		case e.Data.Message != "" && e.Name != "":
			return codex.PlainLine(e.Name + ": " + e.Data.Message)
		case e.Data.Message != "":
			return codex.PlainLine(e.Data.Message)
		case e.Message != "":
			return codex.PlainLine(e.Message)
		case e.Name != "":
			return codex.PlainLine(e.Name)
		}
	}
	return "an error event: " + clipQuoted(codex.PlainLine(string(raw)))
}

// startOpenCodeSession starts a session with its primer as the first message,
// under the id parley chose, and records it for the nick under the state base.
// The session is registered for deletion as soon as the run returns, whatever
// became of the turn: opencode creates the session before the model answers, so a
// run that failed, or was stopped, has left one behind, and it may have been stopped
// before it made it, in which case deleting it finds nothing and is not a failure
// (openCodeDelete). It returns the session id and the session's answer to the
// primer.
//
// The primer goes in on stdin and not as the argument: opencode run joins its
// message arguments into one string, wraps it in double quotes when it holds a
// space and escapes each quote in it with a backslash (2.0.32), so an argument
// would reach the model as a quoted copy of the primer. Read from a pipe, the
// text is the message as it is, which the first message of a session should be.
// The environment is the caller's as it is.
//
// The run starts the user's background service when none runs. The service
// detaches itself (checked on 2.0.32: its own session and process group, stdin and
// stdout on /dev/null), so the group-kill of a run that ended badly leaves it, and
// the TUIs on it, alone, and a run that succeeds is not held up by it.
func startOpenCodeSession(ctx context.Context, env setupEnv, c openCodeSession) (id, answer string, err error) {
	timeout := c.timeout
	if timeout == 0 {
		timeout = openCodeTurnTimeout
	}
	args := []string{"run", "--format", "json", "--title", c.title, "--session", c.sid}
	if c.model != "" {
		args = append(args, "-m", c.model)
	}

	env.logf("starting OpenCode session %s (opencode run with its primer, up to %s)", c.nick, timeout)
	turn, stderr, runErr := runOpenCodeTurn(ctx, c.bin, c.dir, args, c.prompt, timeout)
	env.push(fmt.Sprintf("OpenCode session %s (session %s)", c.sid, c.nick), func() error { return openCodeDelete(c.bin, c.name, c.sid, c.dir) })

	// A run that failed, or whose events carry an error, failed, whatever it
	// exited with: the session would be started on a turn that never answered.
	if runErr != nil || turn.Failure != "" {
		var failed string
		switch {
		case runErr != nil && turn.Failure != "":
			failed = runErr.Error() + ": " + turn.Failure
		case runErr != nil && stderr != "":
			failed = runErr.Error() + ": " + codex.PlainLine(firstLine(stderr))
		case runErr != nil:
			failed = runErr.Error()
		default:
			failed = "it exited 0, but the run reported an error: " + turn.Failure
		}
		return "", "", fmt.Errorf("opencode run for session %s: %s", c.nick, failed)
	}
	if turn.SessionID == "" {
		return "", "", fmt.Errorf("opencode run for session %s: no event carries a sessionID, so the turn is not shown to be the session's; the events were: %s", c.nick, eventList(turn.Events))
	}
	if turn.SessionID != c.sid || len(turn.Others) > 0 {
		seen := []string{clipQuoted(turn.SessionID)}
		for _, o := range turn.Others {
			seen = append(seen, clipQuoted(o))
		}
		return "", "", fmt.Errorf("opencode run for session %s: its events name %s and not only %s, the id this run chose for the session it made, so the turn is not shown to be that session's", c.nick, strings.Join(seen, ", "), c.sid)
	}
	// An event that names no session is not shown to be this run's.
	if len(turn.NoID) > 0 {
		return "", "", fmt.Errorf("opencode run for session %s: event(s) of type %s carry no sessionID, so the turn is not shown to be session %s's and not shown to have completed",
			c.nick, strings.Join(turn.NoID, ", "), c.sid)
	}
	// A session is started on one whose first turn demonstrably completed. An exit
	// 0 says opencode ended, not that the turn did: the events may have stopped
	// short, or their end may be past the cap the output was read through.
	if turn.Truncated {
		return "", "", fmt.Errorf("opencode run for session %s: its output passed %d bytes and was cut off, so the turn is not shown to have completed", c.nick, maxExecOutput)
	}
	if turn.Steps == 0 {
		return "", "", fmt.Errorf("opencode run for session %s: it exited 0, but its output has no step_finish event, so the turn is not shown to have completed; the events were: %s", c.nick, eventList(turn.Events))
	}
	if turn.Open {
		return "", "", fmt.Errorf("opencode run for session %s: it exited 0, but its last step began and never finished (no step_finish follows the last step_start), so the turn is not shown to have completed", c.nick)
	}
	if turn.LastReason != "stop" {
		return "", "", fmt.Errorf("opencode run for session %s: it exited 0, but its last step ended with reason %s and not \"stop\", so the turn is not shown to have completed", c.nick, clipQuoted(turn.LastReason))
	}

	rec, err := state.OpenSessionRecord(c.base, c.nick, c.stream)
	if err != nil {
		return "", "", err
	}
	if err := env.mkdirAll(filepath.Dir(rec.Path())); err != nil {
		return "", "", err
	}
	// Written through a temp file and a rename, so a failure leaves no record; the
	// removal is registered once it is there, and never for one this run did not
	// write.
	if err := rec.Write(c.sid); err != nil {
		return "", "", err
	}
	env.push("session record for session "+c.nick, rec.Remove)
	env.logf("session %s is OpenCode session %s", c.nick, c.sid)
	return c.sid, turn.Answer, nil
}

// ownGroup makes every run of the opencode binary a process group of its own, which
// a stop takes whole. opencode may be a launcher that runs the program as a child,
// as an npm install's is, and killing only the launcher leaves the child running:
// a turn that goes on with its tools on a session about to be deleted. The default
// cancellation of CommandContext kills the launcher alone, so it is replaced.
// WaitDelay keeps a descendant that holds the pipes from holding the run hostage,
// and when it expires Run returns without Cancel having run, which is why a run
// that failed is followed by killGroup.
func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
	cmd.WaitDelay = 5 * time.Second
}

// killGroup takes whatever is left of the process group of a command that ended
// badly: the launcher may have exited while a child still held the pipes, and a
// group lives as long as it has a member. It is called after the command has been
// waited for, so the group is the command's own or empty.
func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// runOpenCodeTurn runs one `opencode run --format json` turn with stdin as its
// message and parses what it said, even when it did not end well: a run that was
// stopped has still announced the session that has to be deleted. A run that
// does not end within timeout is stopped.
//
// opencode may be a launcher that runs the program as a child, as an npm
// install's is, and killing only the launcher would leave a turn running on a
// session about to be deleted. So the run is a process group of its own, and the
// stop takes the group.
func runOpenCodeTurn(ctx context.Context, bin, dir string, args []string, stdin string, timeout time.Duration) (openCodeTurn, string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(stdin)
	ownGroup(cmd)
	stdout, stderr := &limitBuffer{limit: maxExecOutput}, &limitBuffer{limit: 4096}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	if err != nil {
		killGroup(cmd)
	}
	turn := parseOpenCodeTurn(stdout.String())
	turn.Truncated = stdout.truncated
	if err != nil && ctx.Err() == context.DeadlineExceeded {
		err = fmt.Errorf("the turn did not end within %s, so it was stopped: %w", timeout, err)
	}
	return turn, strings.TrimSpace(stderr.String()), err
}

// existingDir is dir if it is a directory now, else "" (the caller's own): a
// checkout that is gone by the time a rollback runs must not stop the rollback.
func existingDir(dir string) string {
	if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
		return dir
	}
	return ""
}

// openCodeDelete removes the OpenCode session this run was given the id of, from
// the checkout it was made in. The caller's cancellation is not honoured: an
// interrupted run is the one most likely to have a session to delete. A delete that
// is answered "Session not found" (exit 1) found nothing to remove, which is the
// session never having been made, and not a failure. name is how the failure
// spells bin to the human, who is told the command to run by hand.
func openCodeDelete(bin, name, id, dir string) error {
	ctx, cancel := context.WithTimeout(context.Background(), openCodeDeleteTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "session", "delete", id)
	cmd.Dir = existingDir(dir)
	cmd.Stdin = nil
	// Its own process group, taken whole on a failure, like every other run of the
	// binary here. A delete may start the service when none runs; it detaches itself
	// (see startOpenCodeSession), so the group-kill does not reach it.
	ownGroup(cmd)
	var out limitBuffer
	out.limit = 4096
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if err != nil {
		killGroup(cmd)
	}
	if err == nil {
		return nil
	}
	if text := strings.TrimSpace(out.String()); strings.Contains(strings.ToLower(text), "session not found") {
		return nil
	}
	return fmt.Errorf("%s session delete %s: %w: %s (remove it yourself with: %s session delete %s)", name, id, err, codex.PlainLine(firstLine(strings.TrimSpace(out.String()))), name, id)
}
