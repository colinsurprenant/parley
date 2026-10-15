// Package codex delivers peer messages into a live Codex CLI thread. It is the
// Codex front: one long-running pump that blocks on the stream exactly as
// `parley wait` does and hands each delivery to the running session with a
// single `codex queue --thread <thread> --message <text>` call, or with one
// such call per message when the delivery is over the argument bound below.
//
// Codex has no channel of its own. A queued message is appended to the thread
// as a plain user turn, and nothing about it tells the session that the text
// was not typed by the human at the other end of that session. The peer header
// stream.Delivery.Render() stamps above every message is therefore the only
// mark this front has, and it travels untouched: the pump never rewrites,
// re-prefixes, wraps or truncates what Render produced, so the framing and
// every message inside the bound below are byte for byte what `parley wait`
// would have printed. The one exception is a message over that bound, whose
// body is replaced by the stub notice described below, under the message's own
// header. The framing is Render's throughout — headers at column zero, every
// body line indented by two spaces, and no control character left in a body
// that could draw over that indent — which is what keeps a header-shaped line
// inside a peer's prose from reading as a header of its own.
//
// The message is an argument and never a shell word — the runner execs the
// binary directly with an argv, so no quote, backtick or line inside a peer's
// prose is parsed by anything. What an argv cannot carry is decided from the
// rendered text before the call rather than discovered from the call's error: a
// text longer than maxArgBytes below is one no exec could ever accept however
// often it were retried. What is over that bound is almost always the batch
// rather than a message in it, though — the debounce window joins a burst into
// one delivery, and a burst that does not fit is usually several messages that
// each would — so a delivery over the bound is not refused as a unit. It is
// delivered one `codex queue` call per message instead, in stream order, the
// gap marker going first in a call of its own rather than riding the first
// message's: the order is the one the single call would have had, and every
// message is measured against the bound alone, with none of the marker's bytes
// charged to it. Only a message over the bound on its own is withheld, and it is
// withheld with a stub in its place: the header Render stamps on every message,
// and in body position one indented line naming what the message renders to and
// the id whose full text is still in the stream, where the human and every other
// participant can still read it. A participant is told what it is not seeing
// rather than left to infer it from a silence.
//
// A NUL is the other thing an argv cannot carry, but Render has already
// replaced a body's with a '?' before this front sees the text, so a body
// holding one is delivered sanitized rather than withheld, and the NUL check in
// queue stands as a guard over the rest of the rendered text — the header
// parley writes and the transport's own id among it — which today can hold
// none. It is the one shape a stub cannot stand in for: a stub carries the
// message's own header, so a NUL that came from there would ride in the stub
// too, which is why a message whose rendering holds one is logged and skipped
// and its text left in the stream. Every failure the call itself
// reports is a failure of this front and stops it, E2BIG included: with the
// message already inside the bound, a kernel refusing the argv is refusing the
// environment this process inherited, which is a fault to report and not a
// message to drop.
package codex

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/colinsurprenant/parley/internal/stream"
)

// DefaultBin is the codex CLI as it is spelled when nobody says otherwise; it
// is resolved on PATH like any other command name.
const DefaultBin = "codex"

// defaultTimeout is how long one delivery wait blocks before it loops. It is
// the channel front's value for the same reason: the loop has nothing to do
// between deliveries, but a wait that never returns would never notice a
// context an adapter ignored.
const defaultTimeout = time.Hour

// callTimeout bounds one `codex queue` call. Queueing is a local, near-instant
// append to a session's input; a call still running after this is a codex that
// is not coming back, and the pump must not be held by it — the nick's lock is
// held for as long as this process lives.
const callTimeout = 30 * time.Second

// maxArgBytes is the longest rendered text this front will hand to an exec, be
// it a whole delivery or one message split out of one, and it is a property of
// the kernels rather than of codex. Linux caps one argv
// string at MAX_ARG_STRLEN — `#define MAX_ARG_STRLEN (PAGE_SIZE * 32)` in
// include/uapi/linux/binfmts.h, so 131072 bytes on a 4 KiB page — counting the
// terminating NUL, and refuses the exec with E2BIG above it whatever the rest of
// the argv costs. macOS has no per-string limit but caps argv and environment
// together at a 1 MiB ARG_MAX. The smaller of the two rules here, one byte below
// it for the terminator, so the same text is carried or withheld on both, and
// so what is left of macOS's 1 MiB is room for the environment rather than
// something this front spends on one message.
const maxArgBytes = 128<<10 - 1

// withheldNotice is the body of the stub that stands in for a message no argv
// can carry. It is a body and nothing else: Render stamps the message's own
// header above it and indents this line as it indents every body line, so it
// can never be read as a line parley wrote at column zero. The id it names is
// the one the header above it already carries, said a second time because the
// sentence has to hold on its own — quoted out of the thread, it is the whole
// of what a participant needs to go and read what was withheld. The arguments
// are the message's rendered byte count — what was measured against the bound,
// header and indent included, rather than the body's own length — the bound,
// and the transport id.
const withheldNotice = "[parley codex] body withheld: this message renders to %d bytes, over the %d byte bound of one exec argument on this front; the full text stays in the stream at id=%s"

// maxOutput caps what is kept of one call's stdout and stderr. The output is
// only ever quoted into an error or a log line, and a codex that floods either
// stream must not be buffered whole to say so.
const maxOutput = 64 << 10

// waitDelay bounds how long the wait for one call lingers once the child is
// gone or its context is done. Killing codex closes nothing it left behind: a
// descendant that inherited the stdout or stderr pipe holds it open, and with
// no delay set os/exec reads those pipes to EOF — which is to say until that
// descendant exits, however long that is. The pump would then hang inside a
// call it has already given up on, and the nick's lock is held for as long as
// this process lives. After this delay the pipes are closed and the wait
// returns, so both the deadline above and a Ctrl-C are bounded whatever codex
// spawned. It is short because nothing useful arrives on those pipes after the
// process they belong to has exited or been killed.
const waitDelay = 2 * time.Second

// CursorStore persists the stream position between runs. *state.Dir implements
// it; it is declared here rather than imported so this package depends on
// nothing but the stream it reads.
type CursorStore interface {
	LoadCursor() (string, error)
	SaveCursor(cursor string) error
}

// Runner runs one codex invocation with the given argv and returns nil once it
// has exited successfully. It is the seam the tests inject at; ExecRunner is
// the real one.
type Runner func(ctx context.Context, argv []string) error

// Config is everything a Pump needs. Deliverer, Cursors and Thread are
// required.
type Config struct {
	Deliverer *stream.Deliverer
	Cursors   CursorStore

	// Thread is the live Codex session to queue into — a session UUID or an
	// exact session name — passed to codex verbatim.
	Thread string

	// Timeout bounds one delivery wait; Debounce is the quiet window that joins
	// a burst into one delivery, as for `parley wait`.
	Timeout  time.Duration
	Debounce time.Duration

	// Log receives every diagnostic. Defaults to discarding.
	Log io.Writer

	// Run is how one `codex queue` call is made. Defaults to ExecRunner over
	// DefaultBin, which resolves `codex` on PATH.
	Run Runner
}

// Pump is one Codex front session: a stream on one side, a live Codex thread on
// the other.
type Pump struct {
	deliverer *stream.Deliverer
	cursors   CursorStore
	thread    string

	timeout  time.Duration
	debounce time.Duration

	log io.Writer
	run Runner
}

// New validates cfg and returns the pump. Nothing is read and nothing is run
// until Run is called.
func New(cfg Config) (*Pump, error) {
	if cfg.Deliverer == nil {
		return nil, errors.New("codex: nil deliverer")
	}
	if cfg.Cursors == nil {
		return nil, errors.New("codex: nil cursor store")
	}
	if strings.TrimSpace(cfg.Thread) == "" {
		return nil, errors.New("codex: empty thread")
	}
	// A negative duration is refused here as well as at the CLI: a debounce
	// below zero past the timeout puts the wait deadline in the past, and the
	// pump would then spin on an idle stream.
	if cfg.Timeout < 0 || cfg.Debounce < 0 {
		return nil, errors.New("codex: negative timeout or debounce")
	}
	p := &Pump{
		deliverer: cfg.Deliverer,
		cursors:   cfg.Cursors,
		thread:    cfg.Thread,
		timeout:   cfg.Timeout,
		debounce:  cfg.Debounce,
		log:       cfg.Log,
		run:       cfg.Run,
	}
	if p.timeout <= 0 {
		p.timeout = defaultTimeout
	}
	if p.log == nil {
		p.log = io.Discard
	}
	if p.run == nil {
		p.run = ExecRunner(DefaultBin)
	}
	return p, nil
}

// Run blocks until the context is canceled or a delivery fails, which is the
// whole life of the front: there is no other way out. A canceled context is a
// clean shutdown and returns nil; anything else is the failure that made the
// front stop, and the caller exits non-zero on it.
func (p *Pump) Run(ctx context.Context) error {
	stored, err := p.cursors.LoadCursor()
	if err != nil {
		return err
	}
	cursor := stream.Cursor(stored)
	for {
		if ctx.Err() != nil {
			return p.stopped()
		}
		delivery, err := p.deliverer.Wait(ctx, cursor, p.timeout, p.debounce)
		if err != nil {
			// A cancel is the shutdown path, not a failure. Whatever this
			// delivery collected was never handed over and was never committed,
			// so the next run delivers it again.
			if ctx.Err() != nil {
				return p.stopped()
			}
			return fmt.Errorf("wait: %w", err)
		}
		if !delivery.Empty() {
			if err := p.queue(ctx, delivery); err != nil {
				// A call cut short by the shutdown is the shutdown, not a
				// failure: nothing is committed, and the next run re-delivers.
				if ctx.Err() != nil {
					return p.stopped()
				}
				return err
			}
		}
		// Only now: a delivery codex did not take is read again from the
		// cursor this loop already held, with the table's gates as they were
		// before it (see Deliverer.Commit), so the cursor moves with the
		// hand-off and not ahead of it.
		cursor = delivery.Cursor
		// Committed and saved only once codex has taken the delivery, so a
		// crash in between re-delivers rather than drops: delivery is
		// at-least-once, the same bargain `parley wait` makes.
		//
		// Reached for an empty delivery too, and with no test on whether the
		// cursor moved: what was read and dropped was this participant's own or
		// a duplicate, it will never be delivered, and leaving the cursor behind
		// it would re-read it forever. `parley wait` writes on every return for
		// exactly this reason, and one file write per idle hour is nothing.
		p.deliverer.Commit(delivery)
		if err := p.cursors.SaveCursor(string(cursor)); err != nil {
			return err
		}
	}
}

// queue hands one delivery to the live thread, as a single codex call wherever
// one can carry it. The message is Delivery.Render() verbatim — the peer header
// is the only mark this front has, and rewriting the text here would be
// rewriting the mark, the two-space body indent that puts a forged header out
// of column zero included. The one text anywhere on this path that is not the
// delivery's own is the stub queueEach puts in place of a message no argv can
// carry, and that stub is Render's output too: this front still assembles no
// line of its own.
//
// Whether the text can be an argv string at all is settled here, before the
// runner, and from the text alone: one that cannot would fail identically on
// every retry and on every restart, so it must never be left to wedge the front
// and every message behind it. A NUL byte and sheer length are the two ways a
// rendered text could be unqueueable, and only length reaches this check with
// anything to refuse, because Render has already replaced a body's NUL with a
// '?'. Neither refuses the delivery as a unit, though: what is over the bound is
// almost always a burst the debounce window joined rather than any one message
// in it, and dropping the batch drops the messages that would have fitted along
// with the one that would not. queueEach takes it a message at a time instead,
// and withholds only what cannot be carried alone.
//
// The calls a split delivery makes are not one transaction and nothing here
// pretends otherwise: a runner error on the third of five returns like any
// other, so the pump stops with nothing committed and the restart re-delivers
// the whole batch — the two calls the thread already took among it. Delivery is
// at-least-once, the same bargain `parley wait` makes, and a duplicate in the
// thread is the shape that bargain takes here. Run commits and saves the cursor
// only once queue has returned, never between the calls of a split. The
// rejected alternative is a commit per call, which would buy the duplicate off
// at the price of losing whatever was in flight when the pump died, and parley
// makes that trade nowhere else.
func (p *Pump) queue(ctx context.Context, d *stream.Delivery) error {
	text := d.Render()
	p.logf("delivering %d message(s), %d bytes, to thread %s", len(d.Messages), len(text), p.thread)
	// A NUL is what terminates an argv string, so os/exec refuses the argument
	// outright and the exec is never attempted. A body may hold one legally —
	// `parley send --body-file` preserves whatever the file contains, and the
	// stream's JSON carries it as the escape \u0000 and decodes it back —
	// but Render replaces a body's control characters with a '?' before this
	// front ever sees them, so what is left to guard is the rest of the rendered
	// text, the transport's own id among it: a byte scan against an exec that
	// would otherwise never run and be retried forever.
	//
	// Length is measured against the portable bound rather than left to the
	// kernel: an E2BIG says the whole argv and environment did not fit, which is
	// not the same claim as this text being unqueueable.
	switch {
	case strings.ContainsRune(text, 0):
		p.logf("this delivery contains a NUL byte, which no exec argument can carry: delivering it one message per call, so that only what carries the NUL is withheld")
	case len(text) > maxArgBytes:
		p.logf("this delivery is longer than the %d bytes one exec argument can carry: delivering it one message per call, so that only a message over the bound on its own is withheld", maxArgBytes)
	default:
		return p.call(ctx, text)
	}
	return p.queueEach(ctx, d)
}

// queueEach delivers d one call at a time, in stream order, for the delivery
// the bound or a NUL refused whole: the gap marker first where there is one,
// then one message per call. Each call carries Render's output for a delivery
// of that one thing, so the header, the body indent and the sanitizing are the
// same code that produced the batch's text and this front still assembles no
// line of its own.
//
// The marker belongs to the delivery rather than to any message in it, and it
// rides no message's call. Riding the first message's would measure that
// message against the bound with bytes that are not its own, so a message that
// fits alone would be stubbed for the marker's hundred-odd bytes — which is
// exactly what "only a message over the bound on its own is withheld" says does
// not happen. It goes in a call of its own instead, ahead of every message,
// which is the order the single call would have had — marker, then messages —
// and leaves each message rendered and measured alone. A delivery carrying
// nothing but a marker is that case with no messages behind it: the marker call
// is the whole of the split.
func (p *Pump) queueEach(ctx context.Context, d *stream.Delivery) error {
	if d.Gap {
		marker := stream.Delivery{Gap: true, GapCursor: d.GapCursor}
		text := marker.Render()
		// One short line and the cursor the transport gave it, so neither check
		// can refuse it with what Render writes today. Checked rather than
		// assumed, as the stub below is: the cost of being wrong is an exec
		// refused on every restart forever, and the messages behind the marker
		// are deliverable whatever the marker is.
		if strings.ContainsRune(text, 0) || len(text) > maxArgBytes {
			p.logf("the gap marker for cursor %s, %d bytes, cannot be an exec argument: it is skipped and the gap stays in the stream; the messages behind it follow", d.GapCursor, len(text))
		} else if err := p.call(ctx, text); err != nil {
			return err
		}
	}
	for _, m := range d.Messages {
		one := stream.Delivery{Messages: []stream.Message{m}}
		text := one.Render()
		// The one shape with no stub to fall back on. A stub is built on this
		// message's own header, so a NUL from there — never from the body, which
		// Render has already cleaned — would ride in the stub too, and there is
		// nothing left to do but leave the message in the stream and say so.
		if strings.ContainsRune(text, 0) {
			p.logf("skipping message id=%s, %d bytes: its rendering contains a NUL byte, which no exec argument can carry and which a stub built on the same header would carry too; the text stays in the stream", m.ID, len(text))
			continue
		}
		if len(text) > maxArgBytes {
			// Over the bound on its own, so no further splitting could carry it.
			// What goes in its place is its own header — Render's, not a line
			// assembled here — over a one-line body naming what the message
			// renders to, which is what was measured here, and the id it is still
			// readable under.
			one.Messages = []stream.Message{withheld(m, len(text))}
			stub := one.Render()
			// Which is unqueueable itself only if the header above it is, and it
			// cannot be with what Render writes today. It is checked rather than
			// assumed because the cost of being wrong is an exec that is refused
			// on every restart forever, and what is left when the stub cannot be
			// carried is what was left before stubs.
			if strings.ContainsRune(stub, 0) || len(stub) > maxArgBytes {
				p.logf("skipping message id=%s, %d bytes: it is over the bound and the %d byte stub for it is unqueueable in its turn; the text stays in the stream", m.ID, len(text), len(stub))
				continue
			}
			p.logf("delivering message id=%s as a stub of %d bytes: its own rendering is %d bytes, over the %d bytes one exec argument can carry; the text stays in the stream", m.ID, len(stub), len(text), maxArgBytes)
			text = stub
		}
		if err := p.call(ctx, text); err != nil {
			return err
		}
	}
	return nil
}

// withheld returns m with its body replaced by the one-line notice that stands
// in for it. Everything else is the message's own — the author, the id and the
// timestamp — so Render stamps the header a participant would have read above
// the real body, and only what could not be carried is missing from it. rendered
// is what m's own rendering measured, the length the bound was compared against:
// a body inside the bound can still render past it, and the notice names the
// number that was actually over.
func withheld(m stream.Message, rendered int) stream.Message {
	m.Body = fmt.Sprintf(withheldNotice, rendered, maxArgBytes, m.ID)
	return m
}

// call makes one `codex queue` call with text as the message and says what a
// failure of it means. The text is inside the bound and free of NUL by the time
// it gets here; what is left to fail is codex, or the exec itself.
func (p *Pump) call(ctx context.Context, text string) error {
	err := p.run(ctx, []string{"queue", "--thread", p.thread, "--message", text})
	if err == nil {
		return nil
	}
	// The message is inside the bound above, so a kernel still answering E2BIG
	// is refusing what surrounds it: argv and environment share one ARG_MAX, and
	// an environment near it leaves no room for any message at all. That is the
	// human's to fix and it is said plainly, because passing over it would
	// silently drop every delivery for as long as the environment stays that
	// large.
	if errors.Is(err, syscall.E2BIG) {
		return fmt.Errorf("queue into thread %s: argument list too long: the environment plus the message exceed ARG_MAX; trim the environment: %w", p.thread, err)
	}
	return fmt.Errorf("queue into thread %s: %w", p.thread, err)
}

// stopped logs the one shutdown line and reports the clean end.
func (p *Pump) stopped() error {
	p.logf("shutting down; nothing in flight was committed")
	return nil
}

func (p *Pump) logf(format string, a ...any) {
	fmt.Fprintf(p.log, "parley codex: "+format+"\n", a...)
}

// ExecRunner returns the real Runner: it runs bin with the argv it is given and
// nothing else. There is no shell anywhere on this path, so the peer's text is
// one argument the kernel hands to codex unread — quotes, newlines, `$(...)`
// and backticks included. The child inherits this process's environment and
// working directory, which is what lets `codex` find the same session state the
// human's terminal does, and it gets no stdin: a queue call has nothing to read.
func ExecRunner(bin string) Runner {
	return func(ctx context.Context, argv []string) error {
		callCtx, cancel := context.WithTimeout(ctx, callTimeout)
		defer cancel()
		cmd := exec.CommandContext(callCtx, bin, argv...)
		cmd.Stdin = nil
		// Bounded on both streams: what codex says about a failure is worth
		// quoting, and worth quoting only up to a point.
		stdout := &capped{limit: maxOutput}
		stderr := &capped{limit: maxOutput}
		cmd.Stdout, cmd.Stderr = stdout, stderr
		// Bounded in time as well: see waitDelay.
		cmd.WaitDelay = waitDelay

		err := cmd.Run()
		if err == nil {
			return nil
		}
		// ErrWaitDelay is exec saying the pipes outlived the process, and only
		// that: it is returned when "the process exits with a successful status
		// code but its output pipes are not closed before the command's
		// WaitDelay expires", and never when the context cut the call short.
		// codex took the delivery and exited 0, which is the whole of what this
		// call is for, so this is success — a descendant of codex still holding
		// a pipe it inherited is codex's business and says nothing about the
		// queue. Reporting it as a failure would stop the pump and re-deliver a
		// message the thread already has.
		if errors.Is(err, exec.ErrWaitDelay) {
			return nil
		}
		// Said plainly, because "signal: killed" on its own reads as codex
		// having crashed rather than as this deadline having fired. A cancel
		// from the caller's own context is left to speak for itself.
		if errors.Is(callCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			err = fmt.Errorf("no answer within %s: %w", callTimeout, err)
		}
		// stderr is where a CLI explains itself; stdout is the fallback for one
		// that does not. Either way the exit status is in err.
		detail := stderr.String()
		if detail == "" {
			detail = stdout.String()
		}
		if detail != "" {
			return fmt.Errorf("%s: %w: %s", bin, err, detail)
		}
		return fmt.Errorf("%s: %w", bin, err)
	}
}

// capped keeps the first limit bytes written to it and counts what it dropped,
// so a bounded quote of a child's output still says that it was bounded.
type capped struct {
	limit   int
	buf     bytes.Buffer
	dropped int
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.limit - c.buf.Len(); room > 0 {
		if len(p) <= room {
			c.buf.Write(p)
			return len(p), nil
		}
		c.buf.Write(p[:room])
		c.dropped += len(p) - room
		return len(p), nil
	}
	c.dropped += len(p)
	return len(p), nil
}

func (c *capped) String() string {
	s := strings.TrimSpace(c.buf.String())
	if c.dropped > 0 {
		return fmt.Sprintf("%s … (%d more bytes)", s, c.dropped)
	}
	return s
}
