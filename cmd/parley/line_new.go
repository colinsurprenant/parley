package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/colinsurprenant/parley/internal/codex"
	"github.com/colinsurprenant/parley/internal/doctor"
	"github.com/colinsurprenant/parley/internal/primer"
	"github.com/colinsurprenant/parley/internal/stream"
)

// cmdLineNew makes the line: its directory and record under the state base, with
// no sessions. It prints what the human does next. Everything it made is taken
// down again if a step fails or the run is interrupted.
func cmdLineNew(args []string) error {
	fs := flag.NewFlagSet("parley line new", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), lineUsageNew)
		fs.PrintDefaults()
	}
	chair := fs.String("chair", "", "required: your registered nick on the server, the human whose posts release the breaker")
	capN := fs.Int("cap", 0, "required: the breaker's cap, the most posts by anyone but you that may arrive within --window before every front holds the next until you post; 0 turns the breaker off")
	window := fs.Duration("window", defaultLineWindow, "the breaker's window, a Go duration above zero such as 10m or 90s")
	server := fs.String("server", defaultListen, "the loopback IP:port of the IRC server the line uses (a host name is refused)")
	stateDir := fs.String("state-dir", "", "state directory holding <nick>/ and lines/ (default $XDG_STATE_HOME/parley)")
	codexBin := fs.String("codex-bin", codex.DefaultBin, "the codex CLI the line's Codex sessions run, resolved on PATH unless it holds a separator; recorded, so line add and the lines it prints use it")
	openCodeBin := fs.String("opencode-bin", doctor.DefaultOpenCodeBin, "the opencode CLI the line's OpenCode sessions run, resolved on PATH unless it holds a separator; recorded, so line add, line drop and the lines they print use it")
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return &configError{err: err}
	}
	switch {
	case len(pos) == 0:
		return cfgErrf("the line id is required: parley line new <id> --chair <nick> --cap <n>")
	case len(pos) > 1:
		return cfgErrf("unexpected argument %q", pos[1])
	}
	id := pos[0]
	if err := checkLineID(id); err != nil {
		return err
	}
	if !given(fs, "chair") {
		return cfgErrf("--chair is required: your registered nick on the server")
	}
	if err := stream.ValidNick(*chair); err != nil {
		return cfgErrf("--chair %q %v", *chair, err)
	}
	if err := checkCap(fs, primer.SkillLine, *capN); err != nil {
		return err
	}
	if err := checkWindow(fs, primer.SkillLine, *window); err != nil {
		return err
	}
	env, err := resolvePlanEnv(id, *server, *stateDir)
	if err != nil {
		return err
	}
	if err := checkUTF8(pathField{"the state directory (--state-dir, or $XDG_STATE_HOME, or $HOME)", env.base},
		pathField{"this binary's path", env.self}); err != nil {
		return err
	}
	rec := lineRecord{
		ID: id, Stream: env.stream, Server: env.server, StateDir: env.base,
		Chair: *chair, Cap: *capN, Window: lineWindowFlag(*window),
		Created:  time.Now().UTC().Format(time.RFC3339),
		Sessions: []lineSession{},
	}
	if given(fs, "codex-bin") {
		if *codexBin == "" {
			return cfgErrf("--codex-bin must not be empty")
		}
		if err := checkUTF8(pathField{"--codex-bin", *codexBin}); err != nil {
			return err
		}
		if rec.CodexBin, err = doctor.Resolve(*codexBin); err != nil {
			return cfgErrf("--codex-bin %q: %v", *codexBin, err)
		}
		if err := checkUTF8(pathField{"--codex-bin, resolved,", rec.CodexBin}); err != nil {
			return err
		}
	}
	if given(fs, "opencode-bin") {
		if *openCodeBin == "" {
			return cfgErrf("--opencode-bin must not be empty")
		}
		if err := checkUTF8(pathField{"--opencode-bin", *openCodeBin}); err != nil {
			return err
		}
		if rec.OpenCodeBin, err = doctor.Resolve(*openCodeBin); err != nil {
			return cfgErrf("--opencode-bin %q: %v", *openCodeBin, err)
		}
		if err := checkUTF8(pathField{"--opencode-bin, resolved,", rec.OpenCodeBin}); err != nil {
			return err
		}
	}

	ctx, stop := signalContext()
	defer stop()
	fmt.Fprintf(os.Stderr, "parley line new: checking the machine (parley doctor's static checks)\n")
	serverDown, err := checkMachineWith(ctx, os.Stderr, "parley line new", doctor.Options{Server: env.server})
	if err != nil {
		return err
	}

	// From here the run looks at the base, under the id's lock, held for the
	// whole run, rollback and the runbook included.
	unlock, err := lockID(env.base, id)
	if err != nil {
		return err
	}
	defer unlock()
	if err := checkLineIDFree(env.base, id); err != nil {
		return err
	}

	run := &lineRun{log: os.Stderr, verb: "line new"}
	err = run.makeLine(rec)
	if err == nil && ctx.Err() != nil {
		// An interrupt that lands after the last step that watches ctx is still
		// an interrupt: roll back rather than report a setup the user stopped.
		err = ctx.Err()
	}
	if err != nil {
		if ctx.Err() != nil {
			err = fmt.Errorf("interrupted: %w", err)
		}
		run.rollback()
		return err
	}
	signal.Ignore(syscall.SIGPIPE)
	if _, err := io.WriteString(os.Stdout, lineRunbook(rec, serverDown, env.self)); err != nil {
		return fmt.Errorf("line %s is set up but its instructions could not be written (%v): parley line end %s takes it down", id, err, id)
	}
	return nil
}

// checkLineIDFree refuses, under the id's lock, an id that is live as a table, a
// build or a line: the id is one namespace for the three.
func checkLineIDFree(base, id string) error {
	for _, kind := range []struct{ name, dir, end string }{
		{"line", lineDirOf(base, id), "line end"},
		{"table", tableDirOf(base, id), "table end"},
		{"build", buildDirOf(base, id), "build end"},
	} {
		if _, err := os.Lstat(kind.dir); err == nil {
			if kind.name == "line" {
				return lineExistsErr(base, id)
			}
			return cfgErrf("%q is already a %s under %s: parley %s %s takes it down, and a line takes another id", id, kind.name, base, kind.end, id)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("look for %s: %w", kind.dir, err)
		}
	}
	return nil
}

// lineExistsErr is the refusal for an id that has a line directory already. One
// without a record is a `line new` that did not finish, which `line end` clears.
func lineExistsErr(base, id string) error {
	if _, err := os.Lstat(filepath.Join(lineDirOf(base, id), lineRecordName)); errors.Is(err, fs.ErrNotExist) {
		return cfgErrf("line %q has a directory under %s and no record, a line new that did not finish: parley line end %s removes it", id, base, id)
	}
	return cfgErrf("line %q already exists under %s: parley line end %s takes it down", id, base, id)
}

// lineRun is one `line new` or `line add` in progress: what it has made, so a
// failure can take exactly that down again.
type lineRun struct {
	log  io.Writer
	verb string
	undo []undoStep
	// unlock drops the nick lock lockNick took; nil until it has.
	unlock func()
}

func (r *lineRun) logf(format string, a ...any) {
	fmt.Fprintf(r.log, "parley "+r.verb+": "+format+"\n", a...)
}

func (r *lineRun) push(what string, do func() error) {
	r.undo = append(r.undo, undoStep{what: what, do: do})
}

func (r *lineRun) mkdirAll(dir string) error { return mkdirAllWith(dir, r.push) }

// rollback takes down what the run made, newest first, and says what it did and
// what it could not do.
func (r *lineRun) rollback() {
	if len(r.undo) == 0 {
		return
	}
	rollbackSteps(r.logf, r.undo)
	r.undo = nil
}

// release drops the nick lock. The caller calls it once any rollback is done.
func (r *lineRun) release() {
	if r.unlock != nil {
		r.unlock()
		r.unlock = nil
	}
}

// makeLine makes <base>/lines/<id>/ and the record in it.
func (r *lineRun) makeLine(rec lineRecord) error {
	dir := lineDirOf(rec.StateDir, rec.ID)
	if err := r.mkdirAll(filepath.Dir(dir)); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return lineExistsErr(rec.StateDir, rec.ID)
		}
		return fmt.Errorf("create %s: %w", dir, err)
	}
	r.push("line directory "+dir, func() error { return os.RemoveAll(dir) })
	if _, err := writeLineRecord(dir, rec); err != nil {
		return err
	}
	return nil
}

// lineRunbook is what `line new` prints to stdout: what the human does next, in
// order. Shell commands have every path and id quoted; the lines meant for the IRC
// client are not, because a quote there would be part of the line.
func lineRunbook(rec lineRecord, serverDown bool, self string) string {
	var b strings.Builder
	step := 0
	next := func(format string, a ...any) {
		step++
		fmt.Fprintf(&b, "\n%d. "+format+"\n\n", append([]any{step}, a...)...)
	}
	cmd := func(line string) { fmt.Fprintf(&b, "    %s\n", line) }

	breaker := fmt.Sprintf("breaker: more than %d posts within %s", rec.Cap, rec.Window)
	if rec.Cap == 0 {
		breaker = "breaker off"
	}
	fmt.Fprintf(&b, "Line %s is set up: chair %s, %s, no sessions yet. What is left is yours, in this order;\n", rec.ID, rec.Chair, breaker)
	fmt.Fprintf(&b, "indented lines starting with / go into your IRC client, the others into a shell.\n")

	if serverDown {
		server := q(self) + " server"
		if rec.Server != defaultListen {
			server += " --listen " + q(rec.Server)
		}
		next("Nothing answers at %s: start the server, in a terminal of its own.", rec.Server)
		cmd(server)
	}

	next("Join as the chair on %s, and check that your nick is registered, before any session or pump connects.", rec.Server)
	cmd("/join #" + rec.ID)
	cmd("/msg NickServ INFO " + rec.Chair)
	b.WriteString("\n   If INFO says it is not registered, register it, and identify on every later connect:\n\n")
	cmd("/msg NickServ REGISTER <password>")
	b.WriteString("\n   An unregistered chair nick could be taken by a session's process, and the breaker would then\n   release on that session's posts as if they were yours.\n")

	next("Add each session, one `parley line add` per session, then launch it as that command prints:")
	cmd(fmt.Sprintf("%s line add %s --nick <nick> --harness <claude|codex|opencode> [--model <model>] [--workdir <absolute dir>] --state-dir %s", q(self), rec.ID, q(rec.StateDir)))

	fmt.Fprintf(&b, "\nWhen it is over: %s line end %s --state-dir %s\n", q(self), rec.ID, q(rec.StateDir))
	return b.String()
}
