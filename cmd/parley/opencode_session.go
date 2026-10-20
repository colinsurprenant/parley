package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/colinsurprenant/parley/internal/codex"
	"github.com/colinsurprenant/parley/internal/doctor"
	"github.com/colinsurprenant/parley/internal/state"
)

// Starting an OpenCode session on its primer, for `line add`: opencode run with
// the primer as the first message, its JSON events read, and the session
// deleted again if the run that made it is rolled back. startCodexSession is the
// model; what differs is what an OpenCode run says and what ends it.
//
// What parley trusts here: opencode run is the human's own binary, but its output
// is read as untrusted. The session id it announces is acted on twice, recorded
// for the pump and handed to `opencode session delete`, so it is checked to have
// the shape of one (state.ValidSessionID) and to be the id every event of the
// run carries; a turn counts as done only on its own events, never on an exit
// status alone.

const (
	// openCodeTurnTimeout bounds the primer turn, as seatTurnTimeout bounds a
	// Codex seat's: a primer is one short turn.
	openCodeTurnTimeout = seatTurnTimeout
	// openCodeDeleteTimeout bounds one `opencode session delete`.
	openCodeDeleteTimeout = time.Minute
	// maxOpenCodeEvents bounds the event types a refusal lists.
	maxOpenCodeEvents = 12
	// openCodeVersionTimeout bounds one `opencode --version`.
	openCodeVersionTimeout = time.Minute
	// openCodeListMax is how many of the newest sessions are asked for when
	// looking for one a stopped run left: a page wide enough that the sessions
	// made in the project since the run began do not push it off.
	openCodeListMax = 200
)

// openCodeSession is one OpenCode session to start on its primer.
type openCodeSession struct {
	nick, model string
	dir         string // the checkout: where opencode runs, and so the session's project
	bin, name   string // the resolved opencode, and how the human spells it
	// title is the session's title: "parley line <id> <nick> <token>", the token
	// being the run's own, so that no other add's session has this title.
	title        string
	prompt       string // the primer
	base, stream string // where the session is recorded
	// timeout is the turn's bound; zero means openCodeTurnTimeout.
	timeout time.Duration
	// sid is the session id parley chose, on OpenCode 2, where opencode run
	// --session creates the session under the id it is given; it is empty on
	// OpenCode 1, whose run makes up an id and announces it.
	sid string
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
	// last of them.
	Steps      int
	LastReason string
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
		case "text":
			if ev.Part.Text != "" {
				answer = append(answer, ev.Part.Text)
			}
		case "step_finish":
			t.Steps++
			t.LastReason = ev.Part.Reason
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
// reads the session id the run announces, and records it for the nick under the
// state base. The session is registered for deletion as soon as its id is known,
// whatever becomes of the turn: opencode creates the session before the model
// answers, so a run that failed, or was stopped, has left one behind. Output
// that names more than one session registers none: nothing is deleted on a
// guess. It returns the session id and the session's answer to the primer.
//
// The primer goes in on stdin and not as the argument: opencode run joins its
// message arguments into one string, wraps it in double quotes when it holds a
// space and escapes each quote in it with a backslash (1.18.30), so an argument
// would reach the model as a quoted copy of the primer. Read from a pipe, the
// text is the message as it is, which the first message of a session should be.
// The environment is the caller's as it is: OPENCODE, AGENT and OPENCODE_PID are
// set by every opencode start and read by nothing in it (1.18.30), so a run from
// inside an OpenCode session's bash tool needs nothing taken out, as a Codex
// thread's variable is.
func startOpenCodeSession(ctx context.Context, env setupEnv, c openCodeSession) (id, answer string, err error) {
	timeout := c.timeout
	if timeout == 0 {
		timeout = openCodeTurnTimeout
	}
	args := []string{"run", "--format", "json", "--title", c.title}
	if c.model != "" {
		args = append(args, "-m", c.model)
	}
	if c.sid != "" {
		args = append(args, "--session", c.sid)
	}

	env.logf("starting OpenCode session %s (opencode run with its primer, up to %s)", c.nick, timeout)
	started := time.Now()
	turn, stderr, runErr := runOpenCodeTurn(ctx, c.bin, c.dir, args, c.prompt, timeout)

	switch {
	case c.sid != "":
		// OpenCode 2: the id is this run's own choice, so the session is known to
		// delete whatever the events say or do not, and nothing is searched for. It
		// may not exist, if the run failed before it made it, and deleting it then
		// is not an error (openCodeDeleteChosen).
		env.push(fmt.Sprintf("OpenCode session %s (session %s)", c.sid, c.nick), func() error { return openCodeDeleteChosen(c.bin, c.name, c.sid, c.dir) })
	case len(turn.Others) > 0:
		// Output that names two sessions does not say which one this run made, and
		// nothing is deleted on a guess: the error names them for the human.
	case turn.SessionID == "":
		// opencode makes the session before its first event, so a run that was
		// stopped, or that failed, before the event has left one it never named.
		// The list is asked for it, from the checkout (1.18.30 lists the project of
		// the directory it runs in) and by the title this run gave it, which no
		// other run's has, and the directory and the time it was made.
		ids, full, err := openCodeStrays(c.bin, c.title, c.dir, started)
		switch {
		case err != nil:
			env.logf("session %s: could not look for a session the run may have left (%v): look for the title %s in opencode session list, run in %s", c.nick, err, clipQuoted(c.title), c.dir)
		case len(ids) == 0 && full:
			// A full page is only the newest part: not finding it there is not
			// finding that there is none.
			env.logf("session %s: the run named no session, and the newest %d sessions of opencode session list in %s hold none titled %s made since it began; an older one could exist: look for the title in opencode session list, run in %s, and delete what is yours", c.nick, openCodeListMax, c.dir, clipQuoted(c.title), c.dir)
		case len(ids) == 0:
			env.logf("session %s: the run named no session, and opencode session list in %s holds none titled %s made since it began, so there is nothing to delete", c.nick, c.dir, clipQuoted(c.title))
		}
		for _, sid := range ids {
			env.push(fmt.Sprintf("OpenCode session %s (session %s, which the run made without announcing it)", sid, c.nick), func() error { return openCodeDelete(c.bin, c.name, sid, c.dir) })
		}
	case state.ValidSessionID(turn.SessionID):
		sid := turn.SessionID
		env.push(fmt.Sprintf("OpenCode session %s (session %s)", sid, c.nick), func() error { return openCodeDelete(c.bin, c.name, sid, c.dir) })
	default:
		env.logf("session %s announced session id %s, which is not an OpenCode session id, so it is not deleted: look for it with opencode session list", c.nick, clipQuoted(turn.SessionID))
	}

	// A run that failed, or whose events carry an error, failed, whatever it
	// exited with: the session would be started on a turn that never answered.
	var runFailed string
	if runErr != nil || turn.Failure != "" {
		switch {
		case runErr != nil && turn.Failure != "":
			runFailed = runErr.Error() + ": " + turn.Failure
		case runErr != nil && stderr != "":
			runFailed = runErr.Error() + ": " + codex.PlainLine(firstLine(stderr))
		case runErr != nil:
			runFailed = runErr.Error()
		default:
			runFailed = "it exited 0, but the run reported an error: " + turn.Failure
		}
	}
	// Events that name two sessions are reported whatever the run exited with: the
	// human has to look in the checkout, and has to be told which ids and which title.
	if len(turn.Others) > 0 {
		seen := []string{clipQuoted(turn.SessionID)}
		for _, o := range turn.Others {
			seen = append(seen, clipQuoted(o))
		}
		also := ""
		if runFailed != "" {
			also = " (the run also failed: " + runFailed + ")"
		}
		// On OpenCode 2 the session the run made is the id parley gave it, and that one
		// is deleted; on 1.x it is not known which, and none is.
		deleted := "none is deleted"
		if c.sid != "" {
			deleted = "only " + c.sid + ", the id this run chose, is deleted"
		}
		return "", "", fmt.Errorf("opencode run for session %s: its events carry more than one session id (%s), so it is not known which the run made, and %s: look with opencode session list, run in %s, for the title %s, and delete what is yours%s",
			c.nick, strings.Join(seen, ", "), deleted, c.dir, clipQuoted(c.title), also)
	}
	if runFailed != "" {
		return "", "", fmt.Errorf("opencode run for session %s: %s", c.nick, runFailed)
	}
	if turn.SessionID == "" {
		return "", "", fmt.Errorf("opencode run for session %s: no event carries a sessionID, so the session is not known; the events were: %s", c.nick, eventList(turn.Events))
	}
	if !state.ValidSessionID(turn.SessionID) {
		return "", "", fmt.Errorf("opencode run for session %s: the session id %s is not an OpenCode session id (ses_ and 26 letters or digits)", c.nick, clipQuoted(turn.SessionID))
	}
	if c.sid != "" && turn.SessionID != c.sid {
		return "", "", fmt.Errorf("opencode run for session %s: its events name session %s and not %s, the id this run chose for the session it made, so the turn is not shown to be that session's", c.nick, clipQuoted(turn.SessionID), c.sid)
	}
	// An event that names no session is not shown to be this run's. The id the
	// others announced is kept: it is registered above, and the rollback deletes it.
	if len(turn.NoID) > 0 {
		return "", "", fmt.Errorf("opencode run for session %s: event(s) of type %s carry no sessionID, so the turn is not shown to be session %s's and not shown to have completed",
			c.nick, strings.Join(turn.NoID, ", "), turn.SessionID)
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
	if err := rec.Write(turn.SessionID); err != nil {
		return "", "", err
	}
	env.push("session record for session "+c.nick, rec.Remove)
	env.logf("session %s is OpenCode session %s", c.nick, turn.SessionID)
	return turn.SessionID, turn.Answer, nil
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
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
	stdout, stderr := &limitBuffer{limit: maxExecOutput}, &limitBuffer{limit: 4096}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	// A descendant that inherited the pipes must not hold the run hostage.
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	turn := parseOpenCodeTurn(stdout.String())
	turn.Truncated = stdout.truncated
	if err != nil && ctx.Err() == context.DeadlineExceeded {
		err = fmt.Errorf("the turn did not end within %s, so it was stopped: %w", timeout, err)
	}
	return turn, strings.TrimSpace(stderr.String()), err
}

// openCodeStrays are the ids of the sessions opencode lists as made by a run that
// named none: exactly the title that run gave (which carries a token of the run's
// own), its directory, and a creation time at or after started (opencode stamps
// it in milliseconds). opencode 1.18.30 lists the sessions of the project of the
// directory it runs in, so it runs in dir, the checkout the run was made in. The
// newest openCodeListMax sessions are asked for, and full says the page came back
// that long, so that it is not taken for all of them; an id that does not have the
// shape of one is never returned, for it goes to a command line.
func openCodeStrays(bin, title, dir string, started time.Time) (ids []string, full bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), openCodeDeleteTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "session", "list", "--format", "json", "-n", strconv.Itoa(openCodeListMax))
	cmd.Dir = existingDir(dir)
	cmd.Stdin = nil
	out, errs := &limitBuffer{limit: maxExecOutput}, &limitBuffer{limit: 4096}
	cmd.Stdout, cmd.Stderr = out, errs
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil {
		return nil, false, fmt.Errorf("opencode session list: %w: %s", err, codex.PlainLine(firstLine(strings.TrimSpace(errs.String()))))
	}
	if out.truncated {
		return nil, false, fmt.Errorf("opencode session list: its output passed %d bytes", maxExecOutput)
	}
	var list []struct {
		ID        string `json:"id"`
		Title     string `json:"title"`
		Directory string `json:"directory"`
		Created   int64  `json:"created"`
	}
	if err := json.Unmarshal([]byte(out.String()), &list); err != nil {
		return nil, false, fmt.Errorf("opencode session list: not the JSON list of sessions: %w", err)
	}
	for _, s := range list {
		if s.Title == title && s.Directory == dir && s.Created >= started.UnixMilli() && state.ValidSessionID(s.ID) {
			ids = append(ids, s.ID)
		}
	}
	// A page as long as the one asked for may be only the newest part of the
	// project's sessions: finding nothing in it does not show there is nothing.
	return ids, len(list) >= openCodeListMax, nil
}

// existingDir is dir if it is a directory now, else "" (the caller's own): a
// checkout that is gone by the time a rollback runs must not stop the rollback.
func existingDir(dir string) string {
	if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
		return dir
	}
	return ""
}

// openCodeDelete removes an OpenCode session this run created, from the checkout
// it was made in. The caller's
// cancellation is not honoured: an interrupted run is the one most likely to
// have a session to delete. name is how the failure spells bin to the human, who
// is told the command to run by hand.
func openCodeDelete(bin, name, id, dir string) error {
	ctx, cancel := context.WithTimeout(context.Background(), openCodeDeleteTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "session", "delete", id)
	cmd.Dir = existingDir(dir)
	cmd.Stdin = nil
	var out limitBuffer
	out.limit = 4096
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s session delete %s: %w: %s (remove it yourself with: %s session delete %s)", name, id, err, codex.PlainLine(firstLine(strings.TrimSpace(out.String()))), name, id)
	}
	return nil
}

// openCodeDeleteChosen is openCodeDelete for a session whose id parley chose, on
// OpenCode 2: a delete that is answered "Session not found" (exit 1) found nothing
// to remove, which is the session never having been made, and not a failure.
func openCodeDeleteChosen(bin, name, id, dir string) error {
	ctx, cancel := context.WithTimeout(context.Background(), openCodeDeleteTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "session", "delete", id)
	cmd.Dir = existingDir(dir)
	cmd.Stdin = nil
	var out limitBuffer
	out.limit = 4096
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	if err == nil {
		return nil
	}
	if text := strings.TrimSpace(out.String()); strings.Contains(strings.ToLower(text), "session not found") {
		return nil
	}
	return fmt.Errorf("%s session delete %s: %w: %s (remove it yourself with: %s session delete %s)", name, id, err, codex.PlainLine(firstLine(strings.TrimSpace(out.String()))), name, id)
}

// ocVersion is the version an opencode binary reports.
type ocVersion struct {
	major int
	text  string
}

// openCodeVersion asks the binary which OpenCode it is: 1.x prints a bare version
// ("1.18.30") and 2.x "opencode v2.0.32". A binary that prints no version is taken
// for a 1.x, as every binary was before the two were told apart; doctor has
// already warned of it. A major version other than 1 or 2 is refused here, before anything
// is made, as the pump would refuse its server. The output is the binary's own and
// is read as text: it is parsed for a number, and quoted bounded.
func openCodeVersion(ctx context.Context, bin, dir string) (ocVersion, error) {
	ctx, cancel := context.WithTimeout(ctx, openCodeVersionTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--version")
	cmd.Dir = existingDir(dir)
	cmd.Stdin = nil
	out, errs := &limitBuffer{limit: 4096}, &limitBuffer{limit: 4096}
	cmd.Stdout, cmd.Stderr = out, errs
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil {
		return ocVersion{}, fmt.Errorf("opencode --version: %w: %s", err, codex.PlainLine(firstLine(strings.TrimSpace(errs.String()))))
	}
	text := doctor.OpenCodeVersion(out.String())
	head, _, _ := strings.Cut(text, ".")
	major, err := strconv.Atoi(head)
	if err != nil {
		return ocVersion{major: 1}, nil
	}
	if major > 2 || major < 1 {
		return ocVersion{}, fmt.Errorf("opencode %s: parley starts sessions on OpenCode 1.x and 2.x only", text)
	}
	return ocVersion{major: major, text: text}, nil
}
