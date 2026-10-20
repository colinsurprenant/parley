package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/colinsurprenant/parley/internal/opencode"
	"github.com/colinsurprenant/parley/internal/primer"
	"github.com/colinsurprenant/parley/internal/state"
	"github.com/colinsurprenant/parley/internal/stream"
)

// The variables OpenCode's own CLI reads for the credentials of a server started
// with a password. The front reads the same two, so a human who has them set for
// `opencode` has set them for the pump.
const (
	opencodePasswordEnv = "OPENCODE_SERVER_PASSWORD"
	opencodeUserEnv     = "OPENCODE_SERVER_USERNAME"
	opencodeDefaultUser = "opencode"
)

// sessionAuto is the --session value that asks for the recorded session instead
// of naming one. No OpenCode session id looks like it, so unlike --thread auto
// there is nothing it shadows.
const sessionAuto = "auto"

// cmdOpencode runs the OpenCode front: a foreground pump that blocks on the
// stream and hands every delivery to a live OpenCode session over the HTTP API
// the TUI serves when it is started with --port. It opens the stream, the state
// directory, the lock and the cursor exactly as cmdCodex does, and differs in
// where the delivery goes and in what is checked before anything is opened: the
// server has to be there, be one this front speaks to, and know the session, and
// a pump pointed at nothing is a bad invocation that must cost the nick no IRC
// account, no connection and no lock to say so. Nothing is printed on stdout:
// the deliveries go into the OpenCode session, and everything this process has
// to say goes to stderr, so the terminal running it stays a log and nothing else.
func cmdOpencode(args []string) error {
	fs := flag.NewFlagSet("parley opencode", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: parley opencode --stream <spec> --nick <nick> --session <ses_id>|auto --port <n> [--debounce 250ms] [--state-dir <dir>] [--skill build|table|line --chair <nick> --cap <n> [--window 10m]]")
		fs.PrintDefaults()
	}
	spec := fs.String("stream", "", "stream to read, e.g. file:/path/to/chat.ndjson")
	nick := fs.String("nick", "", "this participant's nick")
	session := fs.String("session", "", "the live OpenCode session to deliver into, an id such as ses_ea8bbe5a9ffePFZ3AOhDdaNgbW, or auto for the session id recorded for this nick and stream, in the opencode-session file beside the cursor")
	port := fs.Int("port", 0, "required: the port the OpenCode TUI serves its HTTP API on, the one it was started with in `opencode --port <n>`; the pump connects to 127.0.0.1 and nowhere else. If the server was started with OPENCODE_SERVER_PASSWORD, set it (and OPENCODE_SERVER_USERNAME, opencode by default) here too")
	debounce := fs.Duration("debounce", 250*time.Millisecond, "quiet window that joins a burst into one delivery")
	stateDir := fs.String("state-dir", "", "state directory holding <nick>/ (default $XDG_STATE_HOME/parley)")
	skillFlag := fs.String("skill", string(primer.SkillBuild), "the skill the session was started with: build (two peers, nothing held back), table (a seat at a table, which needs --chair and --cap) or line (a session on a line, which needs --chair and --cap)")
	chair := fs.String("chair", "", "with --skill table or line, required: the human's nick, matched without regard to ASCII case: its posts always pass and release what the cap held; at a table they also hold the other agents' posts until this participant has posted once")
	capN := addCapFlag(fs)
	window := addWindowFlag(fs, true)
	if err := parseFlags(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return &configError{err: err}
	}
	if err := checkNick(*nick); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return cfgErrf("unexpected argument %q", fs.Arg(0))
	}
	if *debounce < 0 {
		return cfgErrf("--debounce must not be negative")
	}
	// The pump holds what the skill says is held: the gate is installed iff the
	// skill is table or line. It is told the chair for that and nothing else; the
	// facts the session's text carries are the primer's and the channel's.
	skill, err := parseSkill(*skillFlag)
	if err != nil {
		return err
	}
	if skill == primer.SkillTable || skill == primer.SkillLine {
		if err := checkChair(fs, *chair, *nick, skill); err != nil {
			return err
		}
	} else if given(fs, "chair") {
		return cfgErrf("--chair applies to --skill table or line only: a build has no chair to hold the others back for")
	}
	gate, err := gateOptions(fs, skill, *chair, *capN, *window)
	if err != nil {
		return err
	}
	// The spec is checked here, where nothing is opened, so that a bad --stream
	// is refused before the server is asked anything.
	if _, _, err := resolveSpec(*spec); err != nil {
		return err
	}
	if !given(fs, "port") {
		return cfgErrf("--port is required: the port the OpenCode TUI serves its HTTP API on, from `opencode --port <n>`")
	}
	if *port < 1 || *port > 65535 {
		return cfgErrf("--port %d is not a port: want 1 to 65535", *port)
	}
	// There is no default session: a pump pointed at nothing would post every
	// delivery into whichever session the server happens to have.
	if *session == "" {
		return cfgErrf("--session is required: the OpenCode session id to deliver into (`opencode session list` shows them), or auto for the session recorded for this nick and stream")
	}
	// Read before the stream is opened, which for an irc: stream connects and
	// registers: a seat with no record is the invocation's mistake, and it must
	// not cost the nick an account, a connection or its lock to say so.
	sessionID, source := *session, ""
	if *session == sessionAuto {
		if sessionID, source, err = recordedSession(*stateDir, *nick, *spec); err != nil {
			return err
		}
	} else if !state.ValidSessionID(sessionID) {
		return cfgErrf("--session %q is not an OpenCode session id: want ses_ and 26 letters or digits, as `opencode session list` shows them, or auto", sessionID)
	}

	user := os.Getenv(opencodeUserEnv)
	if user == "" {
		user = opencodeDefaultUser
	}
	client, err := opencode.NewHTTPClient(opencode.LoopbackURL(*port), opencode.Auth{User: user, Password: os.Getenv(opencodePasswordEnv)})
	if err != nil {
		return &configError{err: err}
	}
	// Before the stream is opened, for the reason above: a server that is not
	// there, is not one this front speaks to, or does not know the session is a
	// bad invocation, and the TUI command that makes it right is named.
	version, err := opencode.Probe(context.Background(), client, sessionID)
	if err != nil {
		return probeFailure(*port, sessionID, err)
	}

	s, canonical, err := openStream(*spec, *nick, *stateDir)
	if err != nil {
		return err
	}
	defer closeStream(s)
	dir, err := state.Open(*stateDir, *nick, canonical)
	if err != nil {
		return err
	}
	lock, err := dir.Lock()
	if err != nil {
		var held *state.LockedError
		if errors.As(err, &held) {
			return &configError{err: err}
		}
		return err
	}
	defer lock.Release()
	// The record was read before the lock, so that a seat with none costs
	// nothing; a command that removed it in between has released the lock, so it
	// is read again now that this pump holds the nick.
	if *session == sessionAuto {
		again, _, err := recordedSession(*stateDir, *nick, *spec)
		if err != nil {
			return fmt.Errorf("the recorded session is gone since this pump started: %w", err)
		}
		if again != sessionID {
			return fmt.Errorf("the recorded session for %s changed from %s to %s while this pump started: run it again", *nick, sessionID, again)
		}
	}

	pump, err := opencode.New(opencode.Config{
		Deliverer: stream.NewDeliverer(s, *nick, gate...),
		Cursors:   dir,
		Session:   sessionID,
		Client:    client,
		Debounce:  *debounce,
		Log:       os.Stderr,
	})
	if err != nil {
		return err
	}
	ctx, stop := signalContext()
	defer stop()
	fmt.Fprintf(os.Stderr, "parley opencode: %s as %s into OpenCode session %s on 127.0.0.1:%d (OpenCode %s)%s\n", canonical, *nick, sessionID, *port, version, source)
	if err := pump.Run(ctx); err != nil {
		return fmt.Errorf("opencode: %w", err)
	}
	return nil
}

// probeFailure turns a failed startup probe into the configuration error it is,
// and says what the human runs to make it right when that is the TUI: the server
// is only there while the TUI runs, and only for the session it was given.
func probeFailure(port int, session string, err error) error {
	tui := fmt.Sprintf("`opencode --port %d --session %s` in the session's checkout", port, session)
	switch {
	case errors.Is(err, opencode.ErrSessionNotFound):
		return cfgErrf("OpenCode on 127.0.0.1:%d does not know session %s: start the TUI on it with %s: %v", port, session, tui, err)
	case opencode.IsTransport(err):
		// OpenCode 2's TUI has no --port at all, so a machine whose opencode is
		// OpenCode 2 gets here too, and the hint says what this front needs.
		return cfgErrf("cannot reach OpenCode on 127.0.0.1:%d: the TUI serves its API there only while it runs with --port, so start it with %s (%s): %v", port, tui, opencode.Install1Hint, err)
	default:
		return cfgErrf("OpenCode on 127.0.0.1:%d: %v", port, err)
	}
}

// recordedSession reads the session the record beside the cursor names for nick
// on the stream spec, for --session auto. It returns the id and a note for the
// start line saying where it came from. A missing or unreadable record is a
// configuration error: the invocation asked for something that is not there. The
// spec is canonicalized here as openStream will, and nothing is opened or
// created.
func recordedSession(stateDir, nick, spec string) (id, source string, err error) {
	canonical, _, err := resolveSpec(spec)
	if err != nil {
		return "", "", err
	}
	rec, err := state.OpenSessionRecord(stateDir, nick, canonical)
	if err != nil {
		return "", "", &configError{err: err}
	}
	id, err = rec.Read()
	if err != nil {
		return "", "", cfgErrf("--session %s: %v: pass --session <id> instead", sessionAuto, err)
	}
	if id == "" {
		return "", "", cfgErrf("--session %s: no OpenCode session is recorded for nick %q on %s (looked for %s): pass --session <id> for the session the TUI runs (`opencode session list` shows them)", sessionAuto, nick, canonical, rec.Path())
	}
	return id, fmt.Sprintf(" (--session %s, from %s)", sessionAuto, rec.Path()), nil
}
