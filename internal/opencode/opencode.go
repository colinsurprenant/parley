// Package opencode delivers peer messages into a live OpenCode session. It is
// the OpenCode front: one long-running pump that blocks on the stream exactly as
// `parley wait` does and hands each delivery to the session the human is
// running in the OpenCode TUI, as one user message, over the HTTP API of the
// OpenCode server the session runs on: the TUI's, which serves it when it is
// started with --port, on OpenCode 1, and the background service's on OpenCode 2.
//
// OpenCode has no channel of its own. A prompt posted to a session is a user
// message, and nothing about it tells the model that the text was not typed by
// the human at the other end of that session. The peer header
// stream.Delivery.Render() stamps above every message is therefore the only mark
// this front has, and it travels untouched. The invariant, which nothing in this
// package may weaken: the only text the pump ever POSTs is Render()'s output,
// verbatim, as the one text part of the request, with no system field and no
// other part. It never rewrites, re-prefixes, wraps, truncates or splits what
// Render produced, and the delivery is not counted as made until the session's
// own history shows a user message holding exactly those bytes. The one thing
// that is done to a delivery before Render is bounding it (bound.go): a message
// that renders to more than 128 KiB, and every message once the delivery has 512
// KiB, is replaced by a stub, a copy of the message whose body is a notice, so
// what is posted is still the rendering of a Delivery, the stub under the
// message's own header. The read-back reads the history under a bound of its own,
// and a delivery that passed it could never be confirmed.
//
// Threat model. The human, parley's own state records and the OpenCode server at
// 127.0.0.1:<port> are trusted: the server is the human's own TUI, its JSON is
// read through bounded reads and strict shapes, and it is not treated as
// adversarial. Stream content is not trusted: a peer's message reaches OpenCode
// only as Render output, and this package never interprets it. Out of scope, and
// stated so that nobody reads this front as covering it: another local process
// that listens on the port or drives the server (that is OpenCode's own exposure
// once the human passes --port, and setting OPENCODE_SERVER_PASSWORD closes it,
// which this client then sends as HTTP basic credentials, to loopback only); what
// the session does with its own permissions once it has a message, since parley
// is not a sandbox and OpenCode's bash tool is allowed by default; and hand
// edits of parley's state.
//
// This is the delivery for OpenCode 1. OpenCode 2 is a second protocol under the
// same loop, in queue.go: it queues the prompt under an id of its own and reads it
// back by that id, and everything above (the header, the bound, the cursor, the
// threat model) holds for it unchanged. Where the doc below says "the history", on
// OpenCode 2 read "the message with the id".
//
// Delivery is a protocol with five steps, because OpenCode offers no queue of
// its own: a prompt posted while a turn is running is merged into that turn at
// its next step boundary, and the model answers it in place of the answer it was
// writing. So the pump delivers only into an idle session. After Wait returns a
// delivery it waits for the session to be idle; if it had to wait, everything
// that arrived meanwhile is still in the stream, and it reads again from the
// cursor it started at, so the backlog goes over as one delivery rather than
// one turn each. It then notes which messages the session's recent history
// holds, posts, and reads the history back until a user message that was not
// among them holds the text. The server answers 204 before the message exists,
// so the post alone proves nothing, and checking the bytes also proves the
// header arrived. The note is a set of ids and not the newest id, because the
// server assigns ids as it creates messages, not in the order they were posted
// (two prompts posted one after the other were seen to swap), so a message
// created next to a human's own can sort below it. Last it
// waits a bounded time to see the turn begin, because the session's status can
// read idle for a moment after the 204, and only then is the delivery committed
// and the cursor saved. A human submitting a prompt in the TUI between the idle
// check and the post is merged into the human's own turn: with no queue on the
// server there is no closing that window, and it is the human's own action.
//
// Whether the session is idle is read under the session's own directory, which
// the pump gets from the session when it first attaches and again each time it
// has to reattach after the server was away. OpenCode keeps a session's status
// under the directory the session belongs to, and the server may be running in
// another (a TUI started in one checkout on a session of another): read there,
// or under the server's, the status of a turn in progress is not listed at all
// and every session reads idle. See HTTPClient.Busy.
package opencode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/colinsurprenant/parley/internal/stream"
)

// defaultTimeout is how long one delivery wait blocks before it loops, for the
// reason the Codex and channel fronts give for the same value.
const defaultTimeout = time.Hour

const (
	// defaultIdlePoll is how often a busy session is asked whether its turn is
	// over, and the first step of the backoff for a server that is away.
	defaultIdlePoll = 500 * time.Millisecond

	// defaultReadBackPoll is how often the history is read for the message just
	// posted, and for the turn that follows it.
	defaultReadBackPoll = 100 * time.Millisecond

	// defaultReadBackTimeout bounds the wait for the posted message to appear in
	// the history. It is accepted before it exists, but the gap is milliseconds on
	// a healthy server; thirty seconds is a server that has lost it.
	defaultReadBackTimeout = 30 * time.Second

	// defaultSettleTimeout bounds the wait to see the turn begin. A prompt that
	// starts no turn is possible (a model that errors out at once, a session that
	// was deleted), and it is not worth holding the stream for.
	defaultSettleTimeout = 10 * time.Second

	// defaultMaxBackoff caps the wait between attempts at a server that is away.
	defaultMaxBackoff = 5 * time.Second

	// historyWindow is how much of the history is read for the note and the
	// read-back: the newest this many messages by id. The message being looked
	// for is among the newest of them, and at most a few assistant ones follow it
	// in the seconds in which it is looked for. The same window serves both, which
	// is what makes the set of ids noted before the post comparable with what the
	// read-back sees: messages only ever join the window at its newest end, so one
	// that was outside it when the note was taken cannot be inside it later. Every
	// message comes with all of its parts, tool output among them, so the window is
	// kept small: ten of them is a lot to ask of a session that has been running
	// tools, and is more than the read-back needs.
	historyWindow = 10

	// narrowWindow replaces historyWindow, for the delivery in hand, when a read
	// of the newest historyWindow comes back over the bound a reply is read under.
	narrowWindow = 3
)

// CursorStore persists the stream position between runs. *state.Dir implements
// it; it is declared here rather than imported so this package depends on
// nothing but the stream it reads.
type CursorStore interface {
	LoadCursor() (string, error)
	SaveCursor(cursor string) error
}

// Config is everything a Pump needs. Deliverer, Cursors and Session are
// required, and exactly one of Client and Client2.
type Config struct {
	Deliverer *stream.Deliverer
	Cursors   CursorStore

	// Session is the OpenCode session id to deliver into, passed to the server
	// as it is.
	Session string

	// Client is the HTTP seam for an OpenCode 1 server; see Client. Client2 is
	// the one for an OpenCode 2 server.
	Client  Client
	Client2 Client2

	// Timeout bounds one delivery wait; Debounce is the quiet window that joins
	// a burst into one delivery, as for `parley wait`.
	Timeout  time.Duration
	Debounce time.Duration

	// Log receives every diagnostic. Defaults to discarding.
	Log io.Writer

	// The remaining durations default to the constants above; a test shortens
	// them, and nothing else has a reason to.
	IdlePoll        time.Duration
	ReadBackPoll    time.Duration
	ReadBackTimeout time.Duration
	SettleTimeout   time.Duration
	MaxBackoff      time.Duration
}

// Pump is one OpenCode front session: a stream on one side, a live OpenCode
// session on the other.
type Pump struct {
	deliverer *stream.Deliverer
	cursors   CursorStore
	session   string
	client    Client
	client2   Client2
	// directory is the session's directory, read from the server by attach and
	// "" until then.
	directory string

	timeout  time.Duration
	debounce time.Duration

	idlePoll        time.Duration
	readBackPoll    time.Duration
	readBackTimeout time.Duration
	settleTimeout   time.Duration
	maxBackoff      time.Duration

	log io.Writer
}

// New validates cfg and returns the pump. Nothing is read and no request is made
// until Run is called.
func New(cfg Config) (*Pump, error) {
	if cfg.Deliverer == nil {
		return nil, errors.New("opencode: nil deliverer")
	}
	if cfg.Cursors == nil {
		return nil, errors.New("opencode: nil cursor store")
	}
	if cfg.Client == nil && cfg.Client2 == nil {
		return nil, errors.New("opencode: nil client")
	}
	if cfg.Client != nil && cfg.Client2 != nil {
		return nil, errors.New("opencode: a client for OpenCode 1 and one for OpenCode 2")
	}
	if strings.TrimSpace(cfg.Session) == "" {
		return nil, errors.New("opencode: empty session")
	}
	// A negative duration is refused as the Codex front refuses it: a debounce
	// below zero past the timeout puts the wait deadline in the past, and the
	// pump would spin on an idle stream.
	for _, d := range []time.Duration{cfg.Timeout, cfg.Debounce, cfg.IdlePoll, cfg.ReadBackPoll, cfg.ReadBackTimeout, cfg.SettleTimeout, cfg.MaxBackoff} {
		if d < 0 {
			return nil, errors.New("opencode: negative duration")
		}
	}
	p := &Pump{
		deliverer:       cfg.Deliverer,
		cursors:         cfg.Cursors,
		session:         cfg.Session,
		client:          cfg.Client,
		client2:         cfg.Client2,
		timeout:         orDefault(cfg.Timeout, defaultTimeout),
		debounce:        cfg.Debounce,
		idlePoll:        orDefault(cfg.IdlePoll, defaultIdlePoll),
		readBackPoll:    orDefault(cfg.ReadBackPoll, defaultReadBackPoll),
		readBackTimeout: orDefault(cfg.ReadBackTimeout, defaultReadBackTimeout),
		settleTimeout:   orDefault(cfg.SettleTimeout, defaultSettleTimeout),
		maxBackoff:      orDefault(cfg.MaxBackoff, defaultMaxBackoff),
		log:             cfg.Log,
	}
	if p.log == nil {
		p.log = io.Discard
	}
	return p, nil
}

func orDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
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
			delivery, err = p.deliver(ctx, cursor, delivery)
			if err != nil {
				// A step cut short by the shutdown is the shutdown, not a
				// failure: nothing is committed, and the next run re-delivers.
				if ctx.Err() != nil {
					return p.stopped()
				}
				return err
			}
		}
		// Only now: a delivery the session did not take is read again from the
		// cursor this loop already held, with the table's gates as they were
		// before it (see Deliverer.Commit), so the cursor moves with the
		// hand-off and not ahead of it.
		cursor = delivery.Cursor
		// Committed and saved only once the session's history shows the
		// delivery, so a crash in between re-delivers rather than drops:
		// delivery is at-least-once, the same bargain `parley wait` makes.
		//
		// Reached for an empty delivery too, and with no test on whether the
		// cursor moved: what was read and dropped was this participant's own or
		// a duplicate, it will never be delivered, and leaving the cursor behind
		// it would re-read it forever.
		p.deliverer.Commit(delivery)
		if err := p.cursors.SaveCursor(string(cursor)); err != nil {
			return err
		}
	}
}

// deliver hands d, which Wait collected from the cursor from, to the session,
// and returns the delivery that was handed over: d itself, or the one that
// replaced it when the wait for an idle session let more arrive. It returns once
// the history shows the delivery and the turn it started has been seen to begin,
// or has not within the bound; an error means nothing is known to have landed
// that the stream will not deliver again.
//
// Every attempt starts with awaitIdle, which attaches to the server first: each
// delivery is preceded by a check that the server is the OpenCode it was and by
// a fresh read of the session's directory, however long the pump sat in Wait
// since, and an attempt that failed because the server went away starts over the
// same way.
//
// A context canceled before the history shows the delivery is an error here and
// the caller turns it into the shutdown. One canceled after is not: the message
// is in the session, and reporting it as undelivered would send it a second time
// at the next start.
func (p *Pump) deliver(ctx context.Context, from stream.Cursor, d *stream.Delivery) (*stream.Delivery, error) {
	again := false
	for {
		waited, err := p.awaitIdle(ctx)
		if err != nil {
			return d, err
		}
		// What arrived while the turn ran is still in the stream, past the cursor
		// d began at. Read, not Wait: the stream already has it, and nothing was
		// committed, which is what lets a second collection replace the first (see
		// Deliverer.Wait). An attempt made again after a failed post is the same:
		// more may have come in the meantime.
		if waited || again {
			d, err = p.deliverer.Read(ctx, from)
			if err != nil {
				return d, fmt.Errorf("read: %w", err)
			}
			if d.Empty() {
				return d, nil
			}
		}
		// The one text this front sends, and the only place it is made: the
		// rendering of a Delivery, which is d's own where it fits the bound of a
		// delivery and otherwise d with stubs for the messages that do not.
		posted, withheld := boundedDelivery(d)
		text := posted.Render()
		if text == "" {
			return d, errors.New("a delivery with messages rendered to no text")
		}
		if withheld > 0 {
			p.logf("%d of %d message(s) go over as stubs, past the %d byte bound of a message or the %d byte bound of a delivery; the text stays in the stream", withheld, len(d.Messages), maxMessageBytes, maxDeliveryBytes)
		}
		p.logf("delivering %d message(s), %d bytes, to session %s", len(d.Messages), len(text), p.session)

		if p.client2 != nil {
			var unsent *notSentError
			switch err := p.deliverQueued(ctx, text); {
			case err == nil:
				return d, nil
			case errors.As(err, &unsent):
				p.logf("%v: nothing was sent, waiting for the server again", unsent.err)
				again = true
				if !p.sleep(ctx, p.idlePoll) {
					return d, ctx.Err()
				}
				continue
			default:
				return d, err
			}
		}

		known, window, err := p.recentIDs(ctx)
		if err != nil {
			if IsTransport(err) {
				// The server went away between the idle check and here; nothing was
				// sent, so go back to waiting for it.
				p.logf("%v: nothing was sent, waiting for the server again", err)
				again = true
				if !p.sleep(ctx, p.idlePoll) {
					return d, ctx.Err()
				}
				continue
			}
			return d, err
		}
		if err := p.client.Prompt(ctx, p.session, text); err != nil {
			if IsDial(err) {
				// Never connected, so nothing landed: the same as the server being
				// away at the idle check.
				p.logf("%v: nothing was sent, waiting for the server again", err)
				again = true
				if !p.sleep(ctx, p.idlePoll) {
					return d, ctx.Err()
				}
				continue
			}
			// Anything else may or may not have landed. Stopping with nothing
			// committed re-delivers it at the next start, and at-least-once is the
			// bargain.
			return d, fmt.Errorf("prompt session %s: %w", p.session, err)
		}
		userID, window, err := p.readBack(ctx, known, window, text)
		if err != nil {
			return d, err
		}
		p.settle(ctx, userID, window)
		return d, nil
	}
}

// awaitIdle attaches to the server, then blocks until the session is idle, and
// says whether it had to wait, which tells the caller that its delivery may be
// stale. It attaches on every call and not only the first: it asks the server its
// version and the session's directory, which the status is read under, because
// the server that answers now may not be the one that did when the pump last
// looked, and the human can restart the TUI, perhaps on another install, at any
// time, the long waits for a delivery included. A server that is away is waited
// for with a backoff that doubles up to the cap, with one line logged when it
// goes and one when it is back; the stream keeps everything meanwhile. Any
// failure of the server other than being away stops the pump.
func (p *Pump) awaitIdle(ctx context.Context) (waited bool, err error) {
	backoff := p.idlePoll
	away, outage := true, false
	for {
		if away {
			if err := p.attach(ctx); err != nil {
				if ctx.Err() != nil {
					return waited, ctx.Err()
				}
				if IsTransport(err) {
					if !outage {
						p.logf("OpenCode is not answering (%v): holding deliveries in the stream and retrying", err)
						outage = true
					}
					waited = true
					if !p.sleep(ctx, backoff) {
						return waited, ctx.Err()
					}
					backoff = min(backoff*2, p.maxBackoff)
					continue
				}
				return waited, err
			}
			if outage {
				p.logf("OpenCode is back")
				waited, outage = true, false
			}
			away, backoff = false, p.idlePoll
		}
		busy, err := p.busy(ctx)
		switch {
		case err == nil:
			if !busy {
				return waited, nil
			}
			waited = true
			if !p.sleep(ctx, p.idlePoll) {
				return waited, ctx.Err()
			}
		case ctx.Err() != nil:
			return waited, ctx.Err()
		case IsTransport(err):
			p.logf("OpenCode is not answering (%v): holding deliveries in the stream and retrying", err)
			waited, away, outage = true, true, true
		default:
			return waited, fmt.Errorf("session status: %w", err)
		}
	}
}

// attach asks the server whether it is the one this front can speak to — healthy
// and of a version it supports — and for the session, whose directory it keeps
// for the status reads. A transport error means the server is still away.
func (p *Pump) attach(ctx context.Context) error {
	if p.client2 != nil {
		return p.attach2(ctx)
	}
	version, err := p.client.Health(ctx)
	if err != nil {
		return err
	}
	if err := CheckVersion(version); err != nil {
		return err
	}
	info, err := p.client.Session(ctx, p.session)
	if err != nil {
		return err
	}
	p.directory = info.Directory
	return nil
}

// recentIDs returns the ids of the messages in the session's recent history, the
// newest historyWindow of it, and the window it read them with. It is the note
// the read-back compares against: a user message with the delivery's text that is
// in this set was there before the post, however recently, and proves nothing
// about it. The set and not the newest id, because ids are assigned by the server
// as it creates each message, which is not the order the prompts were posted in: a
// delivery created next to a human's own submit can get the lower of the two ids.
//
// A history read carries every part of every message it returns, tool output
// included, and a session whose recent turns are heavy can pass the bound this
// front reads a reply under. The read is then made again for the newest
// narrowWindow, and that is the window the read-back and the wait for the turn use
// for this delivery. Heavy enough at that, the pump stops with the reason, since
// nothing it could show of the history would confirm a delivery.
func (p *Pump) recentIDs(ctx context.Context) (known map[string]struct{}, window int, err error) {
	window = historyWindow
	msgs, err := p.client.Messages(ctx, p.session, window)
	if IsTooLong(err) {
		p.logf("the newest %d messages of session %s are over the bound this front reads a reply under: reading the newest %d instead", window, p.session, narrowWindow)
		window = narrowWindow
		msgs, err = p.client.Messages(ctx, p.session, window)
		if IsTooLong(err) {
			return nil, 0, fmt.Errorf("the newest %d messages of session %s are still over the %d bytes this front reads a history reply under, after narrowing the read from %d messages: nothing was sent, and no delivery into the session could be confirmed until its recent history is smaller (compact the session in the TUI): %w", narrowWindow, p.session, tooLongLimit(err), historyWindow, err)
		}
	}
	if err != nil {
		return nil, 0, fmt.Errorf("read the recent messages of session %s: %w", p.session, err)
	}
	known = make(map[string]struct{}, len(msgs))
	for _, m := range msgs {
		known[m.ID] = struct{}{}
	}
	return known, window, nil
}

// readBack polls the history until a user message that is not in known holds a
// text part byte for byte equal to text, and returns its id and the window it
// read with. known is the set recentIDs took before the post, over the newest
// window messages, and messages only ever join that window at its newest end: one
// outside it then cannot be inside it now, so a message that is not in known is
// one created since. Nothing is assumed of the order of ids. A delivery that is
// already in the history, as the previous run's would be after a crash, is in
// known and is told apart from the one just posted. A server that is away while
// this runs is not a failure of it: the post was accepted, and the message may
// yet show.
//
// The whole of it is bounded by readBackTimeout, by a deadline on the context of
// every call and not by a look at the clock between them: a request that stalls
// is cut at the deadline of the phase, not at its own, longer, one. A history that
// grows past the bound a reply is read under while this waits is read again for
// the newest narrowWindow, and past that it is an error, which with the message
// already posted is the one that sends it twice at the next start; it is said so.
func (p *Pump) readBack(ctx context.Context, known map[string]struct{}, window int, text string) (string, int, error) {
	phase, cancel := context.WithTimeout(ctx, p.readBackTimeout)
	defer cancel()
	var last error
	for {
		msgs, err := p.client.Messages(phase, p.session, window)
		switch {
		case err == nil:
			last = nil
			for _, m := range msgs {
				if _, old := known[m.ID]; !old && m.Role == "user" && hasText(m, text) {
					return m.ID, window, nil
				}
			}
		case ctx.Err() != nil:
			return "", window, ctx.Err()
		case phase.Err() != nil:
			// The deadline of the phase cut the call short; the timeout below says so.
		case IsTooLong(err) && window > narrowWindow:
			p.logf("the newest %d messages of session %s grew over the bound this front reads a reply under: reading the newest %d instead", window, p.session, narrowWindow)
			window = narrowWindow
			continue
		case IsTooLong(err):
			return "", window, fmt.Errorf("the delivery was posted to session %s, but the newest %d messages of it are over the %d bytes this front reads a history reply under, so it cannot be confirmed; nothing was committed, so it is delivered again at the next start: %w", p.session, window, tooLongLimit(err), err)
		case IsTransport(err):
			last = err
		default:
			return "", window, fmt.Errorf("read back the delivery from session %s: %w", p.session, err)
		}
		if phase.Err() != nil || !p.sleep(phase, p.readBackPoll) {
			if ctx.Err() != nil {
				return "", window, ctx.Err()
			}
			detail := ""
			if last != nil {
				detail = " (the last read failed: " + last.Error() + ")"
			}
			return "", window, fmt.Errorf("the server accepted the delivery but no user message holding it appeared in session %s within %s%s; nothing was committed, so it is delivered again at the next start", p.session, p.readBackTimeout, detail)
		}
	}
}

// tooLongLimit is the bound a TooLongError says it was over.
func tooLongLimit(err error) int64 {
	var t *TooLongError
	if errors.As(err, &t) {
		return t.Limit
	}
	return 0
}

// hasText reports whether m carries a text part holding exactly text.
func hasText(m Message, text string) bool {
	for _, part := range m.Parts {
		if part.Type == "text" && part.Text == text {
			return true
		}
	}
	return false
}

// settle waits, bounded, to see the turn the delivery started: the session going
// busy, or an assistant message that answers the delivery's message. Until one of
// them shows, the session can still read idle, and the next idle check would take
// that for a turn that has already ended and post into it. Neither showing is
// logged and passed over: a turn that began and ended inside the polls leaves the
// second, and one that never began is not worth holding the stream for. Errors
// from the server are passed over for the same reason.
//
// settleTimeout bounds the whole of it by a deadline on the context of every call,
// as readBack's is, so a request that stalls does not stretch it by its own
// timeout. A shutdown during it returns at once and says nothing, and the
// delivery stands: it has been confirmed.
func (p *Pump) settle(ctx context.Context, userID string, window int) {
	phase, cancel := context.WithTimeout(ctx, p.settleTimeout)
	defer cancel()
	for {
		if busy, err := p.client.Busy(phase, p.session, p.directory); err == nil && busy {
			return
		}
		if msgs, err := p.client.Messages(phase, p.session, window); err == nil {
			for _, m := range msgs {
				if m.Role == "assistant" && m.ParentID == userID {
					return
				}
			}
		}
		if phase.Err() != nil || !p.sleep(phase, p.readBackPoll) {
			if ctx.Err() == nil {
				p.logf("session %s did not go busy within %s of the delivery; carrying on", p.session, p.settleTimeout)
			}
			return
		}
	}
}

// sleep waits d, and reports false if the context ended first.
func (p *Pump) sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// stopped logs the one shutdown line and reports the clean end.
func (p *Pump) stopped() error {
	p.logf("shutting down; nothing in flight was committed")
	return nil
}

func (p *Pump) logf(format string, a ...any) {
	fmt.Fprintf(p.log, "parley opencode: "+format+"\n", a...)
}

// CheckVersion refuses a server this OpenCode 1 client cannot speak to: one whose
// major version is 2 or more, or whose version cannot be read. The client speaks
// OpenCode 1's HTTP API, whose prompt endpoint is prompt_async; OpenCode 2 has a
// different one (CheckVersion2 and Client2 are for that), and a client that
// guessed at it could deliver into the wrong place or nowhere. The version comes
// from the server, so what is quoted of it is bounded.
func CheckVersion(version string) error {
	major, ok := majorVersion(version)
	if !ok {
		return fmt.Errorf("cannot read an OpenCode version from %s: this client speaks OpenCode 1's HTTP API and is not run against anything it cannot recognise as OpenCode 1; %s", quoteBounded(version), Install1Hint)
	}
	if major >= 2 {
		return fmt.Errorf("the server reports OpenCode version %s, not OpenCode 1: this client speaks OpenCode 1's HTTP API (prompt_async), which OpenCode 2 does not have; start the pump again, which finds an OpenCode 2 server itself", quoteBounded(version))
	}
	return nil
}

// majorVersion is the number before the first dot of version, and false if there
// is none: a sign or an empty head is not a version.
func majorVersion(version string) (int, bool) {
	head, _, _ := strings.Cut(version, ".")
	major, err := strconv.Atoi(head)
	if err != nil || head == "" || strings.HasPrefix(head, "+") || strings.HasPrefix(head, "-") {
		return 0, false
	}
	return major, true
}

func quoteBounded(s string) string {
	const limit = 40
	if r := []rune(s); len(r) > limit {
		return strconv.Quote(string(r[:limit])) + "…"
	}
	return strconv.Quote(s)
}

// Probe is the startup check: the server answers, speaks a version this front
// supports, and knows the session. It returns the version. The caller makes a
// failure a configuration error before it opens anything else, so a pump that
// was started against nothing costs the nick no account, connection or lock.
func Probe(ctx context.Context, c Client, session string) (version string, err error) {
	version, err = c.Health(ctx)
	if err != nil {
		return "", err
	}
	if err := CheckVersion(version); err != nil {
		return "", err
	}
	if _, err := c.Session(ctx, session); err != nil {
		return "", err
	}
	return version, nil
}
