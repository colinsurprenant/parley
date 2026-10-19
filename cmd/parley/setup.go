package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/colinsurprenant/parley/internal/codex"
	"github.com/colinsurprenant/parley/internal/state"
	"github.com/colinsurprenant/parley/internal/stream/irc"
)

// What `table new` and `build new` share: a run that makes things one step at a
// time and takes exactly those down again if a step fails, the nick locks it
// holds while it does, and starting a Codex session on its primer.

// planEnv is what table new and build new resolve the same way before anything
// else: the loopback server and the stream on it for the id, this binary's path,
// and the state base.
type planEnv struct {
	server, stream, self, base string
}

func resolvePlanEnv(id, server, stateDir string) (planEnv, error) {
	var e planEnv
	addr, err := irc.LoopbackAddr(server)
	if err != nil {
		return e, cfgErrf("--server %q: %v", server, err)
	}
	e.server = addr
	if e.stream, _, err = resolveSpec("irc://" + addr + "/#" + id); err != nil {
		return e, err
	}
	e.self = selfPath()
	if !filepath.IsAbs(e.self) {
		return e, cfgErrf("cannot find this binary's absolute path (got %q): the .mcp.json and every command printed name it", e.self)
	}
	if e.base, err = state.ResolveBase(stateDir); err != nil {
		return e, cfgErrf("--state-dir: %v", err)
	}
	return e, nil
}

// pathField is a path that goes into a record or a launch file, and what names it
// in a refusal: the flag it came from, or what it is.
type pathField struct{ name, path string }

// checkUTF8 refuses, naming it, the first of fields that is not valid UTF-8. The
// records (table.json, build.json), the .mcp.json and the Codex config are JSON or
// TOML, and encoding/json replaces each invalid byte with U+FFFD: a record would
// then name a path other than the one the run used, and the end that reads it
// would act on that one. It is checked at plan time, before any change, for every
// path a run serializes.
func checkUTF8(fields ...pathField) error {
	for _, f := range fields {
		if !utf8.ValidString(f.path) {
			return cfgErrf("%s %q is not valid UTF-8: it is written into a JSON record or a launch file, where encoding/json would replace each bad byte with U+FFFD and the file would name another path than the one used", f.name, f.path)
		}
	}
	return nil
}

// setupEnv is what startCodexSession needs of the run it is a step of: a log
// line, an undo step for what it made, and a directory made with the rollback
// told of it. Both tableRun and buildRun are one.
type setupEnv interface {
	logf(format string, a ...any)
	push(what string, do func() error)
	mkdirAll(dir string) error
}

// errNothingToUndo is what an undo returns when the thing it undoes was never
// made (an undo registered ahead of the call that makes it, and the call made
// nothing): there is nothing to report, neither a rollback nor a failure.
var errNothingToUndo = errors.New("nothing to undo")

// leftAlone is what an undo returns, with the reason, when the thing it undoes is
// there but is not shown to be this run's: it is left as it is and said so, which
// is not a failure to roll back.
type leftAlone string

func (l leftAlone) Error() string { return string(l) }

// rollbackSteps takes down what a run made, newest first, and says what it did,
// what it left alone and what it could not do.
func rollbackSteps(logf func(format string, a ...any), steps []undoStep) {
	var done []string
	for i := len(steps) - 1; i >= 0; i-- {
		u := steps[i]
		err := u.do()
		var left leftAlone
		switch {
		case err == nil:
			done = append(done, u.what)
		case errors.Is(err, errNothingToUndo):
		case errors.As(err, &left):
			logf("left %s as it is: %s", u.what, string(left))
		default:
			logf("could NOT roll back %s: %v", u.what, err)
		}
	}
	if len(done) > 0 {
		logf("rolled back: %s", strings.Join(done, "; "))
	}
}

// mkdirAllWith makes dir and whatever is missing above it at 0700, and hands push
// the removal of each directory it made, so that a rollback removes the ones that
// are still empty. A directory that turns up between the look and the make is
// another run's, and is left alone.
func mkdirAllWith(dir string, push func(what string, do func() error)) error {
	var missing []string
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Lstat(d); err == nil {
			break
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("look for %s: %w", d, err)
		}
		missing = append(missing, d)
		if filepath.Dir(d) == d {
			break
		}
	}
	for i := len(missing) - 1; i >= 0; i-- {
		d := missing[i]
		if err := os.Mkdir(d, 0o700); err != nil {
			if errors.Is(err, fs.ErrExist) {
				continue
			}
			return fmt.Errorf("create %s: %w", d, err)
		}
		push("directory "+d, func() error { return removeEmptyDir(d) })
	}
	return nil
}

// removeEmptyDir removes a directory this run made if it is still empty; one
// that something else has since put files in is not this run's to remove.
func removeEmptyDir(d string) error {
	if err := os.Remove(d); err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTEMPTY) {
		return err
	}
	return nil
}

// lockNicksMaking makes each nick's directory under base (with mkdirAll, which
// registers it for the rollback) and then takes every nick's lock without
// blocking, returning what releases them. A lock another process holds is what
// held makes of it, given the nick and the directory its lock is in.
func lockNicksMaking(base string, nicks []string, mkdirAll func(string) error, held func(nick, dir string) error) (release func(), err error) {
	for _, nick := range nicks {
		lockPath, err := state.NickLockPath(base, nick)
		if err != nil {
			return nil, err
		}
		if err := mkdirAll(filepath.Dir(lockPath)); err != nil {
			return nil, err
		}
	}
	return lockNicks(base, nicks, held)
}

// lockID takes the lock of a table, build or line id under base, without
// blocking, and returns what releases it. It is the first thing `table new`,
// `table end`, `build new`, `build end` and the line verbs take once they know
// the state base, and each holds it for its whole run (before the nick locks, in
// that order everywhere): the id is one namespace for a table, a build and a
// line, and nothing else serializes two commands on it. A command that finds it held refuses and does not wait; the one
// holding it is a command of this kind that is still running, and running this
// one again once it is done is what the message asks for. The lock file is
// <base>/.ids/<id>.lock, outside the table's or the build's own directory, and
// stays, released, as a nick's lock file does.
func lockID(base, id string) (release func(), err error) {
	l, err := state.LockID(base, id)
	if err != nil {
		var held *state.IDLockedError
		if errors.As(err, &held) {
			return nil, fmt.Errorf("%q is in use by another parley table or build command (new or end), or line command (new, add, drop or end): its lock %s is held; wait for that command to finish, then run this again", id, held.Path)
		}
		return nil, err
	}
	return func() { _ = l.Release() }, nil
}

// codexSession is one Codex session to start on its primer: noun names what it
// is for in a message ("seat" or "peer").
type codexSession struct {
	noun         string
	nick, model  string
	dir          string // the worktree: where codex runs, and its -C
	bin, name    string // the resolved codex, and how the human spells it
	prompt       string // the primer
	base, stream string // where the thread is recorded
	// sandbox is the -s of the turn, workspace-write when empty: a table's seat or
	// a build's peer runs in a worktree parley made for it, a line's session in the
	// user's own checkout.
	sandbox string
}

// startCodexSession starts a session with its primer as the first prompt, reads
// the thread id the session announces, and records it for nick under the state
// base. Network is off for this turn: the session must not post, and the chair
// may not be registered yet. The thread is registered for deletion as soon as its
// id is known, whatever becomes of the turn. It returns the thread and the
// session's answer to the primer.
func startCodexSession(ctx context.Context, env setupEnv, c codexSession) (thread, answer string, err error) {
	sandbox := c.sandbox
	if sandbox == "" {
		sandbox = "workspace-write"
	}
	args := []string{"exec", "--json", "-C", c.dir, "-s", sandbox, "-c", "sandbox_workspace_write.network_access=false"}
	if c.model != "" {
		args = append(args, "-m", c.model)
	}
	args = append(args, c.prompt)

	env.logf("starting Codex %s %s (codex exec with its primer, network off for this turn, up to %s)", c.noun, c.nick, seatTurnTimeout)
	turn, stderr, runErr := runCodexExec(ctx, c.bin, c.dir, args)

	switch {
	case turn.ThreadID == "":
	case state.ValidThreadID(turn.ThreadID):
		id := turn.ThreadID
		env.push(fmt.Sprintf("Codex thread %s (%s %s)", id, c.noun, c.nick), func() error { return codexDelete(c.bin, c.name, id) })
	default:
		env.logf("%s %s announced thread id %q, which is not a UUID, so it is not deleted: look for it with codex resume", c.noun, c.nick, turn.ThreadID)
	}

	// A turn that carries an error or a failed turn failed, whatever codex exited
	// with: the session would be started on one that never answered.
	if runErr != nil || turn.Failure != "" {
		var detail string
		switch {
		case runErr != nil && turn.Failure != "":
			detail = runErr.Error() + ": " + turn.Failure
		case runErr != nil && stderr != "":
			detail = runErr.Error() + ": " + codex.PlainLine(firstLine(stderr))
		case runErr != nil:
			detail = runErr.Error()
		default:
			detail = "it exited 0, but the turn failed: " + turn.Failure
		}
		return "", "", fmt.Errorf("codex exec for %s %s: %s", c.noun, c.nick, detail)
	}
	if turn.ThreadID == "" {
		return "", "", fmt.Errorf("codex exec for %s %s announced no thread (no thread.started event); the events were: %s", c.noun, c.nick, eventList(turn.Events))
	}
	if !state.ValidThreadID(turn.ThreadID) {
		return "", "", fmt.Errorf("codex exec for %s %s announced thread id %q, which is not a UUID", c.noun, c.nick, turn.ThreadID)
	}
	// A session is started on one whose first turn demonstrably completed. An
	// exit 0 says codex ended, not that the turn did: the stream may have stopped
	// short, or its end may be past the cap the output was read through.
	if turn.Truncated {
		return "", "", fmt.Errorf("codex exec for %s %s: its output passed %d bytes and was cut off, so the turn is not shown to have completed", c.noun, c.nick, maxExecOutput)
	}
	if !turn.Completed {
		return "", "", fmt.Errorf("codex exec for %s %s: it exited 0, but its output has no turn.completed event, so the turn is not shown to have completed; the events were: %s", c.noun, c.nick, eventList(turn.Events))
	}

	rec, err := state.OpenThreadRecord(c.base, c.nick, c.stream)
	if err != nil {
		return "", "", err
	}
	if err := env.mkdirAll(filepath.Dir(rec.Path())); err != nil {
		return "", "", err
	}
	// Written through a temp file and a rename, so a failure leaves no record; the
	// removal is registered once it is there, and never for one this run did not
	// write.
	if err := rec.Write(turn.ThreadID); err != nil {
		return "", "", err
	}
	env.push(fmt.Sprintf("thread record for %s %s", c.noun, c.nick), rec.Remove)
	env.logf("%s %s is Codex thread %s", c.noun, c.nick, turn.ThreadID)
	return turn.ThreadID, turn.Answer, nil
}
