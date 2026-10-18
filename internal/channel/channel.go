// Package channel serves one parley stream to a live Claude Code session as an
// MCP channel server: it answers the MCP handshake on a stdio pair and pushes
// every peer delivery into the session as a notifications/claude/channel event.
//
// The transport is the MCP stdio one: newline-delimited JSON-RPC 2.0, one
// object per line, requests and notifications in on the reader, responses and
// notifications out on the writer. The writer carries JSON-RPC and nothing
// else — a stray log line on it desynchronizes the client — so every diagnostic
// goes to the separate log writer, which is stderr in the binary.
//
// The channel is one-way by design. No tools are declared: a reply goes out
// over `parley send` from the model's own shell, which is the one outbound path
// every harness shares, and which leaves the peer header this package renders
// as the only thing parley itself puts on the wire.
package channel

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/colinsurprenant/parley/internal/primer"
	"github.com/colinsurprenant/parley/internal/stream"
)

// jsonRPCVersion is the only version this server speaks, on every message in
// both directions.
const jsonRPCVersion = "2.0"

// channelMethod is the Claude Code notification that carries a channel message
// into the session.
const channelMethod = "notifications/claude/channel"

// serverName names this server to Claude Code. It becomes the source attribute
// of the <channel> tag the session sees, so it is also what the instructions
// below tell the model to look for.
const serverName = "parley"

// serverVersion is the channel front's own version, not the stream's.
const serverVersion = "0.0.1"

// The JSON-RPC error codes this server answers with: a line that is not JSON,
// a line that is not a well-formed request, and a request naming a method this
// server does not implement.
const (
	parseError     = -32700
	invalidRequest = -32600
	methodNotFound = -32601
)

// latestProtocolVersion is what an initialize carrying a version this server
// does not know negotiates to, and supportedVersions is what it echoes back
// unchanged. Both started as the MCP TypeScript SDK's own lists
// (LATEST_PROTOCOL_VERSION and SUPPORTED_PROTOCOL_VERSIONS in
// @modelcontextprotocol/sdk 1.27.1), and they are deliberately not kept in step
// with it: do not bump them to the SDK's newer list. Claude Code 2.1.292 refuses
// to register a channel server that negotiates "2026-07-28", which a newer list
// would do for a client that asks for it, and this server implements nothing
// version-specific that a newer version would be needed for. A client that asks
// for a version not listed here is answered with latestProtocolVersion, which
// is what a client that offers "2026-07-28" gets.
const latestProtocolVersion = "2025-11-25"

var supportedVersions = map[string]bool{
	"2024-10-07": true,
	"2024-11-05": true,
	"2025-03-26": true,
	"2025-06-18": true,
	"2025-11-25": true,
}

// defaultTimeout is how long one delivery wait blocks before it loops. It is
// long because the loop has nothing to do between deliveries; it is not
// infinite so that a cancel is noticed even by an adapter that ignores its
// context.
const defaultTimeout = time.Hour

// shutdownGrace bounds how long Serve waits, in total, for the goroutines that
// write once the session is ending. Either of them can be blocked in a write to
// a client that has stopped draining its end, and that write never comes back
// on its own; the caller holds the nick's lock until Serve returns, so the lock
// must not be hostage to them.
const shutdownGrace = 2 * time.Second

// dispatchQueue is how many complete lines may wait between the reader and the
// dispatcher. It exists so a burst of requests does not stall the reader behind
// a reply in flight; it is bounded so a client that floods the channel cannot
// grow the queue without limit.
const dispatchQueue = 64

// maxLine caps one inbound JSON-RPC line. The client is the parent process, but
// a line without an end must not become an unbounded buffer; an over-long line
// is skipped like a malformed one.
const maxLine = 1 << 20

// readBuf is the inbound scan buffer; longer lines are accumulated across
// refills up to maxLine.
const readBuf = 64 << 10

var errLineTooLong = errors.New("channel: line exceeds the maximum size")

// errInputQueueFull ends the session when the dispatcher is stuck in a write
// the client is not draining and the client keeps sending anyway. Dropping the
// line instead would leave the reader in a loop nothing can end — the client is
// neither reading nor stopping — with the nick's lock held for as long as it
// lasts, so the session ends and the failure is reported as one.
var errInputQueueFull = fmt.Errorf("channel: input queue full (%d lines) while a reply was blocked", dispatchQueue)

// partialLineError reports bytes that arrived before the reader ended without
// the newline that would have made them a line. It wraps the underlying read
// error — io.EOF for a client that closed mid-write — so the caller can tell a
// clean end from a broken one while still naming what was dropped.
type partialLineError struct {
	n   int
	err error
}

func (e *partialLineError) Error() string {
	return fmt.Sprintf("channel: %d bytes with no newline before the reader ended: %v", e.n, e.err)
}

func (e *partialLineError) Unwrap() error { return e.err }

// CursorStore persists the stream position between runs. *state.Dir implements
// it; the tests use it to watch the cursor advance.
type CursorStore interface {
	LoadCursor() (string, error)
	SaveCursor(cursor string) error
}

// Config is everything a Server needs. Deliverer, Cursors, In and Out are
// required; Spec is the canonical stream spec, which the instructions hand back
// to the model as the argument of the reply command.
type Config struct {
	Deliverer *stream.Deliverer
	Cursors   CursorStore
	Spec      string

	// Command is the parley binary as the model should invoke it, absolute so
	// the reply command works from whatever directory the model's shell is in.
	// Defaults to "parley".
	Command string

	// StateDir is the resolved state directory this session runs against,
	// absolute. It goes into the reply command the model is given: the reply is
	// a separate parley process, and a transport that keeps a credential for
	// this participant — the IRC adapter's account password — keeps it there.
	// A reply pointed at a different state directory would not be the same
	// participant. Empty leaves the flag out, for a session that has no state
	// directory of its own to name.
	StateDir string

	In  io.Reader
	Out io.Writer
	// Log receives every diagnostic. It must not be Out. Defaults to discarding.
	Log io.Writer

	// Timeout bounds one delivery wait; Debounce is the quiet window that joins
	// a burst into one delivery, as for `parley wait`.
	Timeout  time.Duration
	Debounce time.Duration

	// Skill is the protocol the instructions name: the zero value is the build
	// skill, the two-session mode. primer.SkillTable makes them the table form,
	// primer.SkillLine the line form. The forms are facts and not blanks, so
	// Chair is required for all three: the human's registered nick (the chair,
	// at a table), printed as spelled. The build and table forms also need
	// Target and Worktree, the target repo and this session's worktree of it,
	// printed as spelled; a line has neither, and they are not required there.
	// The caller has validated them; this package only refuses to render a form
	// with one missing. The gate is the Deliverer's, installed by the caller iff
	// the skill is table (the seal and the breaker) or line (the breaker). The
	// line's breaker cap reaches the text through Cap.
	Skill    primer.Skill
	Chair    string
	Target   string
	Worktree string
	// Cap is the line breaker's cap, a fact the line form states. The same value
	// goes to the Deliverer's gate, which the caller builds; this package does
	// not read it back from there. Other skills ignore it.
	Cap int
}

// Server is one MCP channel session.
type Server struct {
	deliverer *stream.Deliverer
	cursors   CursorStore
	cursor    stream.Cursor
	spec      string
	command   string
	stateDir  string

	// skill, chair, target, worktree and cap are Config's, for the instructions.
	skill    primer.Skill
	chair    string
	target   string
	worktree string
	cap      int

	in  io.Reader
	log io.Writer

	// replyDir is the private directory the reply recipe points the model at.
	// The server makes it and removes it when Serve returns; there is no way to
	// hand it one, so the instructions' privacy promise is always true.
	replyDir string

	timeout  time.Duration
	debounce time.Duration

	// wmu serializes the writer: the request handler and the delivery pump both
	// write to it, and a JSON-RPC line interleaved with another is a line the
	// client cannot parse.
	wmu sync.Mutex
	enc *json.Encoder

	// initialized is closed when a completed handshake releases the pump: a
	// notification sent before the client finished the handshake is a
	// notification it may drop. handshook records that an initialize was
	// answered, which is the half of the handshake this server owns; it is read
	// and written only by the dispatch goroutine, the only one that runs handle.
	initialized     chan struct{}
	initializedOnce sync.Once
	handshook       bool
}

// New validates cfg, loads the stored cursor and creates the session's private
// reply directory. Doing all three here rather than in the pump keeps a broken
// state directory a startup failure, before the client has been told the server
// is up, and puts the reply directory in place before the handshake that names
// a path inside it. Serve removes a directory New created, so a New that is
// never served leaves one behind unless Close is called.
func New(cfg Config) (*Server, error) {
	if cfg.Deliverer == nil {
		return nil, errors.New("channel: nil deliverer")
	}
	if cfg.Cursors == nil {
		return nil, errors.New("channel: nil cursor store")
	}
	if cfg.In == nil || cfg.Out == nil {
		return nil, errors.New("channel: nil transport")
	}
	if cfg.Spec == "" {
		return nil, errors.New("channel: empty stream spec")
	}
	switch cfg.Skill {
	case "", primer.SkillBuild, primer.SkillTable:
		if cfg.Chair == "" || cfg.Target == "" || cfg.Worktree == "" {
			return nil, errors.New("channel: the instructions need a chair, a target and a worktree")
		}
	case primer.SkillLine:
		if cfg.Chair == "" {
			return nil, errors.New("channel: the instructions need a chair")
		}
		if cfg.Cap < 0 {
			return nil, errors.New("channel: negative cap")
		}
	default:
		return nil, fmt.Errorf("channel: unknown skill %q", cfg.Skill)
	}
	// A negative duration is refused here as well as at the CLI: a debounce
	// below zero past the timeout puts the wait deadline in the past, and the
	// pump would then spin on an idle stream.
	if cfg.Timeout < 0 || cfg.Debounce < 0 {
		return nil, errors.New("channel: negative timeout or debounce")
	}
	s := &Server{
		deliverer:   cfg.Deliverer,
		cursors:     cfg.Cursors,
		spec:        cfg.Spec,
		command:     cfg.Command,
		stateDir:    cfg.StateDir,
		skill:       cfg.Skill,
		chair:       cfg.Chair,
		target:      cfg.Target,
		worktree:    cfg.Worktree,
		cap:         cfg.Cap,
		in:          cfg.In,
		log:         cfg.Log,
		timeout:     cfg.Timeout,
		debounce:    cfg.Debounce,
		enc:         json.NewEncoder(cfg.Out),
		initialized: make(chan struct{}),
	}
	// The content this server carries is a peer's prose, which routinely holds
	// the characters Go escapes by default; escaping them would still decode,
	// but it makes every line a human reads on the wire unreadable.
	s.enc.SetEscapeHTML(false)
	if s.command == "" {
		s.command = "parley"
	}
	if s.log == nil {
		s.log = io.Discard
	}
	if s.timeout <= 0 {
		s.timeout = defaultTimeout
	}
	cursor, err := s.cursors.LoadCursor()
	if err != nil {
		return nil, err
	}
	s.cursor = stream.Cursor(cursor)
	// The reply body is a peer message in plaintext, and it must not sit at a
	// name anything else can guess inside a world-writable directory: another
	// local process could pre-create a symlink there before the model writes,
	// or read what the model wrote afterwards. So the recipe names a file in a
	// directory of this session's own — 0700, created before the first
	// handshake can name a path inside it, and removed when Serve returns.
	// This is the last thing New does, so nothing after it can fail and leave
	// the directory behind.
	dir, err := os.MkdirTemp("", replyDirPrefix(cfg.Deliverer.Nick()))
	if err != nil {
		return nil, fmt.Errorf("channel: create the reply directory: %w", err)
	}
	s.replyDir = dir
	return s, nil
}

// replyDirPrefix is the name of the reply directory up to the random string
// os.MkdirTemp adds.
func replyDirPrefix(nick string) string { return "parley-reply-" + nick + "-" }

// maxTempSuffix is the longest random string os.MkdirTemp can add to a name: the
// decimal form of a uint32, which is what the standard library draws it from. A
// test pins it against the toolchain in use.
const maxTempSuffix = "4294967295"

// replyFile is the file inside the reply directory that the recipe names. It is
// only an example: nothing here reads it, parley reads whatever path is passed,
// and the directory is the model's to fill as it likes.
const replyFile = "reply.txt"

// replyPath is the file the reply recipe names.
func (s *Server) replyPath() string { return filepath.Join(s.replyDir, replyFile) }

// removeReplyDir takes the session's reply directory down with the session, so
// the plaintext of the last reply does not outlive it.
func (s *Server) removeReplyDir() {
	if err := os.RemoveAll(s.replyDir); err != nil {
		s.logf("could not remove the reply directory %s: %v", s.replyDir, err)
	}
}

// Close releases what New made, for a server that is not going to be served: the
// reply directory. It is safe after Serve, which removes the directory itself.
func (s *Server) Close() { s.removeReplyDir() }

// Instructions is the text the handshake hands the client, built from the same
// values it will be built from then. It is exported so the caller can measure it
// against the client's cap before it commits to a session: the text holds paths
// that are the user's.
func (s *Server) Instructions() string { return s.instructionsFor(s.replyPath()) }

// LongestInstructions is Instructions with the reply directory named as long as
// New can name one: the random string os.MkdirTemp adds is not always the same
// length, and the text holds the reply path several times. A caller that has to
// know the text will fit, before a launch whose reply directory is a different
// draw, measures this one; it is never shorter than what Instructions or any
// other session's handshake sends for the same Config.
func (s *Server) LongestInstructions() string {
	dir := filepath.Join(filepath.Dir(s.replyDir), replyDirPrefix(s.deliverer.Nick())+maxTempSuffix)
	return s.instructionsFor(filepath.Join(dir, replyFile))
}

func (s *Server) instructionsFor(replyPath string) string {
	return instructions(s.skill, s.command, s.spec, s.deliverer.Nick(), s.stateDir, replyPath, s.chair, s.target, s.worktree, s.cap)
}

// Serve runs the session until the client closes the reader, the context is
// canceled, or the delivery pump fails. A closed reader and a canceled context
// are both clean ends; a closed reader is also the one end whose requests are
// finished rather than dropped, since a client may send and close in the same
// breath. Only a failure the session cannot continue past comes back as an
// error, and that is what makes the server show as failed in /mcp.
// A client that floods the input queue while refusing to drain its own end is
// such a failure: the session ends with errInputQueueFull, which reaches the
// process as a non-zero exit, because a client that is neither reading nor
// stopping is not a session that ended cleanly.
func (s *Server) Serve(ctx context.Context) error {
	// Every path out of here ends the session, and the reply directory ends
	// with it: a failure, a signal and a clean EOF all leave nothing of the
	// last reply behind under the system temp directory.
	defer s.removeReplyDir()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Reading and answering are separate goroutines on purpose. A reply the
	// client is not draining blocks its writer for as long as the client stays
	// away; if that write happened on the reading goroutine, the reader would
	// never reach the EOF that says the client is gone, and this function would
	// never return — with the nick's lock still held by the caller.
	lines := make(chan []byte, dispatchQueue)
	pumpDone := make(chan error, 1)
	inputDone := make(chan error, 1)
	dispatchDone := make(chan error, 1)
	go func() { pumpDone <- s.pump(ctx) }()
	go func() { dispatchDone <- s.dispatch(ctx, lines) }()
	// The input loop is deliberately not joined on the way out: a read blocked
	// in the kernel on stdin cannot be interrupted by canceling a context, and
	// every path out of here is the process ending.
	go func() { inputDone <- s.readInput(ctx, lines) }()

	var err error
	pumped, dispatched := false, false
	drain := false
	select {
	case err = <-inputDone:
		// A clean end of input is the one path with requests possibly still in
		// flight: the client sent them and closed its end without waiting for
		// the answers.
		drain = err == nil
	case err = <-dispatchDone:
		dispatched = true
	case err = <-pumpDone:
		pumped = true
	case <-ctx.Done():
	}
	// Both writers get joined, under one deadline between them: each can be
	// stuck in a write to a client that has stopped reading, a cancel does not
	// interrupt such a write, and a session that ends must still hand the
	// nick's lock back. The pump is joined second because it owns the cursor
	// and the de-duplication state, and should finish the delivery it is in.
	deadline := time.Now().Add(shutdownGrace)
	if drain {
		// The reader closed the queue behind it, so the dispatcher ends on its
		// own once it has answered everything still queued — but only if it is
		// joined before the cancel, since its own ctx.Done arm would otherwise
		// race the queue and drop what the client had already sent. A
		// dispatcher stuck in a write the client is not draining is still
		// abandoned at the deadline, and the cancel below is what the pump is
		// waiting for, so the whole shutdown stays inside one grace.
		if derr := s.join(dispatchDone, deadline, "request dispatcher"); err == nil {
			err = derr
		}
		dispatched = true
	}
	cancel()
	if !dispatched {
		if derr := s.join(dispatchDone, deadline, "request dispatcher"); err == nil {
			err = derr
		}
	}
	if !pumped {
		if perr := s.join(pumpDone, deadline, "delivery pump"); err == nil {
			err = perr
		}
	}
	return err
}

// join waits for one of the session's goroutines to report, no later than
// deadline. Abandoning it is not an error — the session is ending either way —
// but it is never silent.
func (s *Server) join(done <-chan error, deadline time.Time, what string) error {
	select {
	case err := <-done:
		return err
	default:
	}
	select {
	case err := <-done:
		return err
	case <-time.After(time.Until(deadline)):
		s.logf("the %s did not stop within %s, probably blocked writing to the client; ending the session anyway", what, shutdownGrace)
		return nil
	}
}

// pump is the delivery side: it blocks on the stream and turns every non-empty
// delivery into one channel notification.
func (s *Server) pump(ctx context.Context) error {
	select {
	case <-s.initialized:
	case <-ctx.Done():
		return nil
	}
	for {
		if ctx.Err() != nil {
			return nil
		}
		delivery, err := s.deliverer.Wait(ctx, s.cursor, s.timeout, s.debounce)
		if err != nil {
			// A cancel is the shutdown path, not a failure. Whatever this
			// delivery collected was never handed over and was never committed,
			// so the next run delivers it again.
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("wait: %w", err)
		}
		if !delivery.Empty() {
			if err := s.notify(delivery); err != nil {
				return err
			}
		}
		// Only now: a delivery that did not reach the client is read again from
		// the cursor this loop already held, with the table's gates as they were
		// before it (see Deliverer.Commit), so the cursor moves with the hand-off
		// and not ahead of it.
		s.cursor = delivery.Cursor
		// Committed and saved only once the notification is on the wire, so a
		// crash in between re-delivers rather than drops: delivery is
		// at-least-once, the same bargain `parley wait` makes.
		//
		// Reached for an empty delivery too, and with no test on whether the
		// cursor moved: what was read and dropped was this participant's own or
		// a duplicate, it will never be delivered, and leaving the cursor
		// behind it would re-read it forever. Whether two spellings of an
		// opaque cursor mean the same position is the adapter's business and
		// not this loop's, so every return of Wait is written. `parley wait`
		// writes on every return for exactly this reason, and one file write
		// per idle hour is nothing.
		s.deliverer.Commit(delivery)
		if err := s.cursors.SaveCursor(string(s.cursor)); err != nil {
			return err
		}
	}
}

// notify writes one delivery as a channel notification. The content is
// Delivery.Render() verbatim — the peer header parley stamps on every message
// is the invariant this whole front exists to preserve, and so is the framing
// that carries it: headers at column zero, every body line indented by two
// spaces, and no control character left in a body that could draw over that
// indent, so nothing a peer wrote can read as a header.
func (s *Server) notify(d *stream.Delivery) error {
	meta := map[string]string{
		"nick":  s.deliverer.Nick(),
		"count": strconv.Itoa(len(d.Messages)),
	}
	if ids := d.IDs(); len(ids) > 0 {
		meta["first_id"] = ids[0]
		meta["last_id"] = ids[len(ids)-1]
	}
	if d.Gap {
		meta["gap"] = "true"
	}
	return s.write(notification{
		JSONRPC: jsonRPCVersion,
		Method:  channelMethod,
		Params:  channelParams{Content: d.Render(), Meta: meta},
	})
}

// readInput is the reading half of the protocol side: one JSON-RPC message per
// line, handed to the dispatcher, until the client closes the reader. It never
// writes, so nothing a stalled client does can keep it from noticing EOF.
func (s *Server) readInput(ctx context.Context, lines chan<- []byte) error {
	defer close(lines)
	r := bufio.NewReaderSize(s.in, readBuf)
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		line, err := readLine(r)
		if err != nil {
			if errors.Is(err, errLineTooLong) {
				s.logf("dropping an inbound line over %d bytes", maxLine)
				continue
			}
			var partial *partialLineError
			if errors.As(err, &partial) {
				s.logf("dropping %d bytes of an unterminated final line", partial.n)
			}
			// The client closed its end, or the pipe broke under it: either way
			// the session is over and there is nobody left to deliver to. It is
			// not a failure of this server, so it is not reported as one —
			// anything other than a clean EOF is logged on the way out.
			if !errors.Is(err, io.EOF) {
				s.logf("reading from the client ended: %v", err)
			}
			return nil
		}
		// A full queue must not pin the reader either: the dispatcher may be
		// blocked in a write that never comes back, and this is the goroutine
		// that has to stay free to see the client leave. So the enqueue is
		// non-blocking, and a queue that is full with the dispatcher stuck
		// ends the session rather than parking the reader behind it.
		select {
		case lines <- line:
		case <-ctx.Done():
			return nil
		default:
			s.logf("input queue full (%d lines) while a reply is blocked; ending the session", dispatchQueue)
			return errInputQueueFull
		}
	}
}

// dispatch is the answering half: it runs every line the reader hands over, in
// arrival order, and owns every write a request draws. One goroutine is what
// keeps replies ordered and the handshake state single-threaded.
func (s *Server) dispatch(ctx context.Context, lines <-chan []byte) error {
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				return nil
			}
			if err := s.handle(line); err != nil {
				return err
			}
		case <-ctx.Done():
			return nil
		}
	}
}

// handle dispatches one inbound line. Only a failed write comes back as an
// error: anything the client got wrong is answered or logged and the session
// goes on, since a channel server that exits on a message it did not expect
// takes the session's peer delivery down with it.
func (s *Server) handle(line []byte) error {
	if len(strings.TrimSpace(string(line))) == 0 {
		return nil
	}
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		if !json.Valid(line) {
			// Not JSON at all, which is the one thing -32700 is for. Nothing
			// about this line is trustworthy, its id included, so the answer
			// carries the null id the spec reserves for exactly this case.
			s.logf("answering a malformed JSON line with a parse error: %v", err)
			return s.fail(nil, parseError, "parse error")
		}
		// Well-formed JSON that is not a request object: a top-level array or
		// scalar, or an object whose jsonrpc is not a string. The bytes parsed,
		// so this is -32600 and not -32700. A struct decode records the first
		// type error and keeps going, so an id that did decode is still here to
		// echo; a top-level mismatch leaves none, and the answer carries null.
		s.logf("answering JSON that is not a JSON-RPC request with an invalid request: %v", err)
		return s.fail(echoID(req.ID), invalidRequest, "invalid request: not a JSON-RPC request object")
	}
	// A top-level null is the one scalar encoding/json reads into a struct
	// without complaining: it leaves the struct as it was and reports nothing,
	// so it has to be named here to be answered like the other scalars above.
	if isNull(line) {
		s.logf("answering a top-level null with an invalid request")
		return s.fail(nil, invalidRequest, "invalid request: not a JSON-RPC request object")
	}
	if req.JSONRPC != jsonRPCVersion {
		s.logf("refusing a line whose jsonrpc is %q", req.JSONRPC)
		return s.fail(echoID(req.ID), invalidRequest, "invalid request: jsonrpc must be "+jsonRPCVersion)
	}
	if len(bytes.TrimSpace(req.Method)) == 0 {
		// An object with no method but with a result or an error is a response
		// to a request this server never sends, and a response must never be
		// answered: an error back would itself be a line the client cannot
		// match to anything. An object with none of the three is neither a
		// request nor a response, and the client is waiting on its id, so it is
		// told what is wrong instead of being left to time out. Only an absent
		// method is read either way, which is why the field is kept raw: a
		// method present as null, or as anything but a string, is a line that
		// meant to be a request and got it wrong, and it is refused below.
		if len(req.Result) > 0 || len(req.Error) > 0 {
			s.logf("dropping a JSON-RPC response, which this server never asked for")
			return nil
		}
		s.logf("refusing a JSON-RPC line with no method, result or error")
		return s.fail(echoID(req.ID), invalidRequest, "invalid request: no method")
	}
	var method string
	// null is named here rather than left to the decoder: encoding/json reads
	// null into a string as "leave it as it was" and reports no error, so a
	// null method would otherwise pass through as the empty one.
	if isNull(req.Method) || json.Unmarshal(req.Method, &method) != nil {
		s.logf("refusing a line whose method is not a string: %s", req.Method)
		return s.fail(echoID(req.ID), invalidRequest, "invalid request: method must be a string")
	}
	// JSON-RPC tells a request from a notification by the id: only a request
	// gets a reply, and replying to a notification would be a line the client
	// cannot match to anything. An id sent explicitly as null is neither — MCP
	// forbids it on a request — so it is refused rather than guessed at.
	if isNull(req.ID) {
		s.logf("refusing %q: a request id must not be null", method)
		return s.fail(nil, invalidRequest, "invalid request: a request id must not be null")
	}
	// JSON-RPC allows a string or a number and nothing else, so an object, an
	// array or a boolean is refused here rather than carried into the method
	// below, which would echo it back verbatim as the id of a response. echoID
	// folds exactly those shapes to null, and null is what an answer the client
	// cannot match its request to has to carry.
	if id := echoID(req.ID); len(req.ID) > 0 && id == nil {
		s.logf("refusing %q: a request id must be a string or a number, not %s", method, req.ID)
		return s.fail(id, invalidRequest, "invalid request: a request id must be a string or a number")
	}
	isRequest := len(req.ID) > 0

	switch method {
	case "initialize":
		if !isRequest {
			s.logf("ignoring initialize sent as a notification")
			return nil
		}
		if err := s.write(response{JSONRPC: jsonRPCVersion, ID: req.ID, Result: s.initializeResult(req.Params)}); err != nil {
			return err
		}
		// Only an initialize that was answered counts: the gate below opens for
		// a client this server has told what it is, not for one whose handshake
		// never completed.
		s.handshook = true
		return nil
	case "notifications/initialized":
		if isRequest {
			s.logf("refusing notifications/initialized sent as a request")
			return s.fail(req.ID, invalidRequest, "invalid request: notifications/initialized is a notification, not a request")
		}
		if !s.handshook {
			// Opening the gate here would push notifications at a client that
			// never asked this server what it was, which is a client with no
			// reason to be listening for them.
			s.logf("ignoring notifications/initialized before a successful initialize")
			return nil
		}
		s.initializedOnce.Do(func() { close(s.initialized) })
		return nil
	case "ping":
		if !isRequest {
			return nil
		}
		return s.write(response{JSONRPC: jsonRPCVersion, ID: req.ID, Result: struct{}{}})
	default:
		if !isRequest {
			s.logf("ignoring notification %q", method)
			return nil
		}
		// A client probes server/discover to learn what a server offers, and
		// answers itself from the -32601: it is a question every session asks,
		// not a method somebody got wrong, so it is answered without a line in
		// the log that every other unknown method keeps.
		if method != "server/discover" {
			s.logf("refusing unimplemented method %q", method)
		}
		return s.fail(req.ID, methodNotFound, "method not found: "+method)
	}
}

// fail answers one bad line with a JSON-RPC error. The id is echoed when the
// client can match a response to it and null when it cannot, which is what the
// spec asks for on a line whose id could not be read.
func (s *Server) fail(id json.RawMessage, code int, message string) error {
	return s.write(response{JSONRPC: jsonRPCVersion, ID: id, Error: &rpcError{Code: code, Message: message}})
}

// echoID returns the id an error response may carry. JSON-RPC allows a string
// or a number; anything else — an object, an array, an absent or null id — is
// nothing the client could match a response to, and null is how that is said.
func echoID(raw json.RawMessage) json.RawMessage {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 {
		return nil
	}
	if t[0] == '"' || t[0] == '-' || (t[0] >= '0' && t[0] <= '9') {
		return t
	}
	return nil
}

// isNull reports an id sent explicitly as null, which JSON-RPC treats as no id
// at all and MCP forbids on a request.
func isNull(raw json.RawMessage) bool {
	return string(bytes.TrimSpace(raw)) == "null"
}

// initializeResult answers the handshake. No tools capability is declared: this
// is a one-way channel, and the outbound path is `parley send`, which the
// instructions spell out in full.
func (s *Server) initializeResult(params json.RawMessage) initializeResult {
	var p initializeParams
	if len(params) > 0 {
		// A client that sends no protocolVersion, or an unparseable params, is
		// answered with this server's latest rather than refused: the handshake
		// is the only chance to tell it anything at all.
		if err := json.Unmarshal(params, &p); err != nil {
			s.logf("initialize params did not parse: %v", err)
		}
	}
	version := p.ProtocolVersion
	if !supportedVersions[version] {
		if version != "" {
			s.logf("client asked for protocol version %q; answering with %q", version, latestProtocolVersion)
		}
		version = latestProtocolVersion
	}
	// The text is written to sit under the client's cap, but the paths in it are
	// a user's and could be arbitrarily long. A client that truncates drops the
	// tail, which is the trust rule; nothing is cut here, since a short text
	// missing the rule is no better than a long one. The caller measures
	// Instructions against the cap before it serves, and refuses a session whose
	// text is over it.
	text := s.Instructions()
	return initializeResult{
		ProtocolVersion: version,
		Capabilities: capabilities{
			Experimental: map[string]any{"claude/channel": struct{}{}},
		},
		ServerInfo:   serverInfo{Name: serverName, Version: serverVersion},
		Instructions: text,
	}
}

// write puts one JSON-RPC message on the writer, as one line. The encoder
// writes the object and its newline in a single Write, and the mutex keeps the
// pump's notifications from splitting a response in half.
func (s *Server) write(v any) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if err := s.enc.Encode(v); err != nil {
		return fmt.Errorf("write jsonrpc: %w", err)
	}
	return nil
}

func (s *Server) logf(format string, a ...any) {
	fmt.Fprintf(s.log, "parley channel: "+format+"\n", a...)
}

// instructions is what Claude Code hands the model as context when this server
// connects: the reply command, the facts the human launched the session with,
// and then what a delivery is and what it is worth. The text itself lives in
// internal/primer, which renders the same blocks for the fronts whose harness
// has no connect-time channel to hand them over; this front is the claude form
// of it and nothing more.
func instructions(skill primer.Skill, command, spec, nick, stateDir, replyPath, chair, target, worktree string, cap int) string {
	return primer.Render(primer.Params{
		Front:     primer.Claude,
		Command:   command,
		Spec:      spec,
		Nick:      nick,
		StateDir:  stateDir,
		ReplyPath: replyPath,
		Skill:     skill,
		Chair:     chair,
		Target:    target,
		Worktree:  worktree,
		Cap:       cap,
	})
}

// shellQuote renders one POSIX shell word the way the reply command in the
// instructions does, so the tests can rebuild that command line from the values
// that went into it.
func shellQuote(s string) string { return primer.ShellQuote(s) }

// readLine returns the next complete line, newline included. A line over
// maxLine is discarded and reported as errLineTooLong so the caller can go on
// reading the ones behind it; every other error means there is no complete line
// and the caller must stop.
func readLine(r *bufio.Reader) ([]byte, error) {
	var (
		line    []byte
		n       int
		tooLong bool
	)
	for {
		frag, err := r.ReadSlice('\n')
		n += len(frag)
		switch {
		case err == nil:
			if tooLong || n > maxLine {
				return nil, errLineTooLong
			}
			return append(line, frag...), nil
		case errors.Is(err, bufio.ErrBufferFull):
			if tooLong || n > maxLine {
				// Keep consuming to the newline, but stop accumulating: the
				// line is already past what will ever be handed over.
				tooLong, line = true, nil
				continue
			}
			line = append(line, frag...)
		default:
			// A partial line at EOF is a client that died mid-write; there is
			// nothing complete to dispatch, but the bytes it did send must not
			// vanish without a trace, so the count travels with the error.
			if n > 0 {
				return nil, &partialLineError{n: n, err: err}
			}
			return nil, err
		}
	}
}

// request is an inbound JSON-RPC request or notification. ID is kept raw and
// echoed byte for byte: JSON-RPC allows a string or a number, and re-encoding a
// number through float64 would hand back an id the client does not recognize.
// Method is raw for a different reason: a method present as null or as a
// non-string is a malformed request and is refused, while an absent one is read
// by what sits beside it, and only the undecoded bytes tell those apart. Result
// and Error are carried for that reading alone — nothing here decodes them, and
// raw is what says whether the key was there at all.
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  json.RawMessage `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   json.RawMessage `json:"error"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type initializeParams struct {
	ProtocolVersion string `json:"protocolVersion"`
}

type initializeResult struct {
	ProtocolVersion string       `json:"protocolVersion"`
	Capabilities    capabilities `json:"capabilities"`
	ServerInfo      serverInfo   `json:"serverInfo"`
	Instructions    string       `json:"instructions"`
}

type capabilities struct {
	Experimental map[string]any `json:"experimental"`
}

type serverInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type notification struct {
	JSONRPC string        `json:"jsonrpc"`
	Method  string        `json:"method"`
	Params  channelParams `json:"params"`
}

// channelParams is what Claude Code renders into the session: Content becomes
// the body of the <channel> tag and each Meta entry one of its attributes. Meta
// keys must be identifiers — letters, digits and underscores — or the client
// drops them silently.
type channelParams struct {
	Content string            `json:"content"`
	Meta    map[string]string `json:"meta,omitempty"`
}
