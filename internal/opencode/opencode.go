// Package opencode delivers peer messages into a live OpenCode session. It is
// the OpenCode front: one long-running pump that blocks on the stream exactly as
// `parley wait` does and hands each delivery to the session the human is
// running in the OpenCode TUI, as one user message, over the HTTP API of the
// OpenCode 2 server the session runs on: the background service that
// `opencode -s <session>` starts, found from the file it writes, or a server
// started with `opencode serve --port`.
//
// OpenCode has no channel of its own. A prompt posted to a session is a user
// message, and nothing about it tells the model that the text was not typed by
// the human at the other end of that session. The peer header
// stream.Delivery.Render() stamps above every message is therefore the only mark
// this front has, and it travels untouched. The invariant, which nothing in this
// package may weaken: the only text the pump ever POSTs is Render()'s output,
// verbatim, as the text of the request, with no system field and no other part.
// It never rewrites, re-prefixes, wraps, truncates or splits what Render
// produced, and the delivery is not counted as made until the session's own
// message with the id the pump posted holds exactly those bytes. The one thing
// that is done to a delivery before Render is bounding it (bound.go): a message
// that renders to more than 128 KiB, and every message once the delivery has 512
// KiB, is replaced by a stub, a copy of the message whose body is a notice, so
// what is posted is still the rendering of a Delivery, the stub under the
// message's own header.
//
// Threat model. The human, parley's own state records and the OpenCode server at
// a loopback address are trusted: the server is the human's own, its JSON is read
// through bounded reads and strict shapes, and it is not treated as adversarial.
// The registration file the service writes is a credential written for
// OpenCode's own clients, and is trusted as far as the user's own `opencode`
// trusts it and no further (service.go). Stream content is not trusted: a peer's
// message reaches OpenCode only as Render output, and this package never
// interprets it. Out of scope, and stated so that nobody reads this front as
// covering it: another local process that drives a server the human started
// without a password (a server always wants one, and this client sends it as HTTP
// basic credentials, to loopback only); what the session does with its own
// permissions once it has a message, since parley is not a sandbox and OpenCode's
// bash tool is allowed by default; and hand edits of parley's state.
//
// Delivery waits for an idle session, then posts into its queue. After Wait
// returns a delivery the pump waits for the session to be idle; if it had to wait,
// everything that arrived meanwhile is still in the stream, and it reads again
// from the cursor it started at, so the backlog goes over as one delivery rather
// than one turn each. The server queues the post and makes a turn of its own of it,
// which is what makes a post that lands in a turn nobody expected safe. How a
// delivery is made, and when it is counted as made, is the comment at the top of
// queue.go.
//
// Before every delivery, whatever the pump did in between, it asks the server who
// it is and for the session (attach), and when the service was found from its
// registration file it reads the file again first: the service can stop and start
// with another address, password and process id while a pump runs.
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
	// over, how often the inbox is looked at while a delivery waits there, and the
	// first step of the backoff for a server that is away.
	defaultIdlePoll = 500 * time.Millisecond

	// defaultReadBackPoll is how often the message just posted is asked for.
	defaultReadBackPoll = 100 * time.Millisecond

	// defaultPostTimeout bounds the post: the retries of a post that got no answer,
	// which end when the server has said it admitted the prompt. Thirty seconds is a
	// server that has lost it. The wait for the message after that has no bound.
	defaultPostTimeout = 30 * time.Second

	// defaultMaxBackoff caps the wait between attempts at a server that is away.
	defaultMaxBackoff = 5 * time.Second
)

// CursorStore persists the stream position between runs. *state.Dir implements
// it; it is declared here rather than imported so this package depends on
// nothing but the stream it reads.
type CursorStore interface {
	LoadCursor() (string, error)
	SaveCursor(cursor string) error
}

// Config is everything a Pump needs. Deliverer, Cursors, Session and Client are
// required.
type Config struct {
	Deliverer *stream.Deliverer
	Cursors   CursorStore

	// Session is the OpenCode session id to deliver into, passed to the server
	// as it is.
	Session string

	// Client is the HTTP seam; see Client.
	Client Client
	// Discovery, if set, makes the pump find OpenCode's service again from its
	// registration file whenever it attaches (every delivery, and after every
	// outage), instead of keeping the client it started with: Client is then the
	// first of them. Without it the server is the one Client is for, wherever it is.
	Discovery *Discovery

	// Timeout bounds one delivery wait; Debounce is the quiet window that joins
	// a burst into one delivery, as for `parley wait`.
	Timeout  time.Duration
	Debounce time.Duration

	// Log receives every diagnostic. Defaults to discarding.
	Log io.Writer

	// The remaining durations default to the constants above; a test shortens
	// them, and nothing else has a reason to.
	IdlePoll     time.Duration
	ReadBackPoll time.Duration
	PostTimeout  time.Duration
	MaxBackoff   time.Duration
}

// Pump is one OpenCode front session: a stream on one side, a live OpenCode
// session on the other.
type Pump struct {
	deliverer *stream.Deliverer
	cursors   CursorStore
	session   string
	client    Client
	discovery *Discovery

	timeout  time.Duration
	debounce time.Duration

	idlePoll     time.Duration
	readBackPoll time.Duration
	postTimeout  time.Duration
	maxBackoff   time.Duration

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
	if cfg.Client == nil {
		return nil, errors.New("opencode: nil client")
	}
	if strings.TrimSpace(cfg.Session) == "" {
		return nil, errors.New("opencode: empty session")
	}
	// A negative duration is refused as the Codex front refuses it: a debounce
	// below zero past the timeout puts the wait deadline in the past, and the
	// pump would spin on an idle stream.
	for _, d := range []time.Duration{cfg.Timeout, cfg.Debounce, cfg.IdlePoll, cfg.ReadBackPoll, cfg.PostTimeout, cfg.MaxBackoff} {
		if d < 0 {
			return nil, errors.New("opencode: negative duration")
		}
	}
	p := &Pump{
		deliverer:    cfg.Deliverer,
		cursors:      cfg.Cursors,
		session:      cfg.Session,
		client:       cfg.Client,
		discovery:    cfg.Discovery,
		timeout:      orDefault(cfg.Timeout, defaultTimeout),
		debounce:     cfg.Debounce,
		idlePoll:     orDefault(cfg.IdlePoll, defaultIdlePoll),
		readBackPoll: orDefault(cfg.ReadBackPoll, defaultReadBackPoll),
		postTimeout:  orDefault(cfg.PostTimeout, defaultPostTimeout),
		maxBackoff:   orDefault(cfg.MaxBackoff, defaultMaxBackoff),
		log:          cfg.Log,
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
		// Committed and saved only once the session shows the delivery, so a
		// crash in between re-delivers rather than drops: delivery is
		// at-least-once, the same bargain `parley wait` makes.
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
// the session holds the delivery as a message, or the human is seen to have
// cancelled it; an error means nothing is known to have landed that the stream
// will not deliver again.
//
// Every attempt starts with awaitIdle, which attaches to the server first: each
// delivery is preceded by a check that the server is the OpenCode it was, however
// long the pump sat in Wait since, and an attempt that failed because the server
// went away starts over the same way.
//
// A context canceled before the session holds the delivery is an error here and
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

		var unsent *notSentError
		switch err := p.deliverQueued(ctx, text); {
		case err == nil:
			return d, nil
		case errors.As(err, &unsent):
			// Never connected, so nothing landed: the same as the server being away at
			// the idle check.
			p.logf("%v: nothing was sent, waiting for the server again", unsent.err)
			again = true
			if !p.sleep(ctx, p.idlePoll) {
				return d, ctx.Err()
			}
		default:
			return d, err
		}
	}
}

// What the pump says when it holds deliveries because of what the history shows, and
// when it lets go: once each, by the kind of hold. A stop is the human's interrupt.
// The undecided hold is a history that goes back further than the pump reads without a
// user message or an idle marker, where releasing could start a session the human
// stopped.
var (
	heldSaid = map[holdState]string{
		holdStopped:   "the session was interrupted; holding deliveries until you send it something",
		holdUndecided: fmt.Sprintf("the newest %d messages of the session hold neither a user message nor an idle marker, so it cannot be told whether you interrupted it; holding deliveries until it can", recentWindow*maxHistoryPages),
	}
	heldLifted = map[holdState]string{
		holdStopped:   "the session was sent something; delivering again",
		holdUndecided: "the session's history shows a user message or an idle marker again; delivering again",
	}
)

// awaitIdle attaches to the server, then blocks until the session is idle, and
// says whether it had to wait, which tells the caller that its delivery may be
// stale. It attaches on every call and not only the first: the server that
// answers now may not be the one that did when the pump last looked, and the human
// can restart OpenCode, perhaps on another install, at any time, the long waits for
// a delivery included. A server that is away is waited for with a backoff that
// doubles up to the cap, with one line logged when it goes and one when it is back;
// the stream keeps everything meanwhile. Any failure of the server other than being
// away stops the pump.
//
// A session the human interrupted is not idle for this purpose: if the newest idle
// marker of its history says interrupted and no user message is newer, the human
// stopped it and has sent it nothing since, and a delivery would start it again over
// their stop. The history is read back until a user message or an idle marker decides
// (checkStopped), however many other entries lie between; if it does not within the
// bound, it is held as a stop is, since releasing could start a stopped session. The
// pump holds, says so once, and goes on looking at the idle backoff until the human
// sends the session something.
func (p *Pump) awaitIdle(ctx context.Context) (waited bool, err error) {
	backoff := p.idlePoll
	away, outage, held := true, false, holdNone
	reauths := 0
	for {
		if away {
			if err := p.attach(ctx); err != nil {
				if ctx.Err() != nil {
					return waited, ctx.Err()
				}
				if IsTransport(err) || IsServerError(err) {
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
		busy, err := p.client.Busy(ctx, p.session)
		hold := holdNone
		if err == nil && !busy {
			// Idle is not yet free to post into: the human may have stopped the
			// session, and a delivery now would start it again over their stop.
			hold, err = checkStopped(ctx, p.client, p.session)
		}
		switch {
		case err == nil && !busy && hold == holdNone:
			if held != holdNone {
				p.logf("%s", heldLifted[held])
			}
			return waited, nil
		case err == nil:
			if hold != holdNone && hold != held {
				p.logf("%s", heldSaid[hold])
				held = hold
			}
			waited = true
			if !p.sleep(ctx, p.idlePoll) {
				return waited, ctx.Err()
			}
		case ctx.Err() != nil:
			return waited, ctx.Err()
		case IsTransport(err), IsServerError(err):
			p.logf("OpenCode is not answering (%v): holding deliveries in the stream and retrying", err)
			waited, away, outage = true, true, true
		case p.staleCredentials(err):
			// The service restarted at the address the pump holds, with a new
			// password: attach reads the file again, and a refusal of what it finds
			// there is the end.
			if reauths++; reauths > maxReauths {
				return waited, p.refusedAgain(err)
			}
			p.logf("%v: the service refused the password the pump holds; reading its file again", err)
			waited, away = true, true
		default:
			return waited, fmt.Errorf("session status: %w", err)
		}
	}
}

// attach asks the server whether it is the one this front can speak to — of a
// version it supports, and knowing the session. In discovery mode it first finds
// the service again from its registration file, which the service rewrites when it
// starts (another address, password and process id): a pump that reads the file once
// would hold for a service that is not there any more. Asking the server who it is
// comes before asking for the session (Discovery.Find), so a stale file reads as
// stale.
//
// The client the pump uses is replaced only by an attach that went through whole.
// An attach that failed leaves the pump with a client it has no right to use, and
// every caller that gets an error from here goes back to attaching before it makes
// another request to the session: awaitIdle and awaitServer loop on it, and
// handOff does through attachWithin.
//
// A service that is not there (no file, or one that does not describe the server
// that answers), or is starting (a 5xx), is a server that is away, and is returned
// as a transport failure, which the loop holds for with its backoff. A 401 after the
// file was read again is not: the password in the file is the service's own, and it
// is refused. With a port nothing is found again, and the errors are the server's
// own.
func (p *Pump) attach(ctx context.Context) error {
	client := p.client
	if p.discovery != nil {
		_, c, _, err := p.discovery.Find(ctx)
		if err != nil {
			return holdForService(err)
		}
		client = c
	} else {
		info, err := client.Info(ctx)
		if err != nil {
			return err
		}
		if err := CheckVersion(info.Version); err != nil {
			return err
		}
	}
	if _, err := client.Session(ctx, p.session); err != nil {
		if p.discovery != nil {
			return holdForService(err)
		}
		return err
	}
	p.client = client
	return nil
}

// attachWithin attaches, and again after every failure that is the server being
// away or in trouble (no answer, or a 5xx), sleeping backoff between the tries and
// doubling it up to the pump's cap, until the attach succeeds. It ends with the
// context, and with any failure that waiting does not mend: a 401 from the
// credentials it has just found, a version it refuses, a session the server does not
// know, any other status. No request to the session is made by it or between its
// tries, which is the point of it: the caller posts, or reads, only once it returns
// nil.
func (p *Pump) attachWithin(ctx context.Context, backoff time.Duration) error {
	for {
		err := p.attach(ctx)
		switch {
		case err == nil:
			return nil
		case ctx.Err() != nil:
			return ctx.Err()
		case !IsTransport(err) && !IsServerError(err):
			return err
		}
		if !p.sleep(ctx, backoff) {
			return ctx.Err()
		}
		backoff = min(backoff*2, p.maxBackoff)
	}
}

// holdForService turns the failures that mean "the service is not up yet" into a
// transport failure, which is what the loop holds for, and leaves every other
// failure as it is.
func holdForService(err error) error {
	var stale *StaleServiceError
	switch {
	case IsTransport(err):
		return err
	case errors.Is(err, ErrNoService), errors.As(err, &stale):
		return &TransportError{Op: "find the OpenCode service", Err: err}
	case IsServerError(err):
		return &TransportError{Op: "ask the OpenCode service", Err: err}
	}
	return err
}

// maxReauths is how many times in a row a 401 on a client the pump found earlier is
// answered by finding the service again, before the pump gives up on it: the
// credentials it just found are the ones that were refused, and finding them again
// would not mend that.
const maxReauths = 3

// staleCredentials is whether err is a 401 on the client the pump holds, in discovery
// mode: the service has probably restarted at the same address with a new password,
// which its file now holds, so the thing to do is to read the file again and make a
// client of what it says. The same 401 on a client made from a file just read (an
// attach that fails) is not this: that password is the service's own, and it was
// refused. With a port the password is the human's, set once, and is never this.
func (p *Pump) staleCredentials(err error) bool { return p.discovery != nil && IsUnauthorized(err) }

// refusedAgain is the end of a pump whose service keeps refusing the password its
// file holds.
func (p *Pump) refusedAgain(err error) error {
	return fmt.Errorf("the service refused the password in its file %d times in a row, each time after reading the file again: %w", maxReauths, err)
}

// awaitServer holds, after a request to the server got no answer or a 5xx, until
// the server answers again, with the backoff the idle wait uses, and says so once
// when the outage begins (l is what has been said of the wait). Whatever else fails
// is the end of the wait. The entry the pump is waiting on is in the server's
// database and survives its restart. It returns once the attach has succeeded; the
// outage ends, and is said to, with the next look at the delivery that the server
// answers in every read (answered), so that a service that is well for one read and
// not for the next is one outage and not two.
func (p *Pump) awaitServer(ctx context.Context, cause error, l *landing) error {
	l.held = true
	if !l.outage {
		p.logf("OpenCode is not answering (%v): the delivery is queued there, holding for the server", cause)
		l.outage = true
	}
	if !p.sleep(ctx, p.idlePoll) {
		return ctx.Err()
	}
	return p.attachWithin(ctx, min(p.idlePoll*2, p.maxBackoff))
}

// answered notes that the server answered a whole look at the delivery, which ends
// an outage that awaitServer began.
func (p *Pump) answered(l *landing) {
	l.reauths = 0
	if l.outage {
		p.logf("OpenCode is back")
		l.outage = false
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

// CheckVersion refuses a server this front cannot speak to: one whose major
// version is anything but 2, or whose version cannot be read. A 1 is the legacy
// line, which has no HTTP API of this shape, and a front that guessed at one could
// deliver into the wrong place or nowhere; a 0 or a 3 is a version it was never
// shown, and no more a thing to guess at. Any 2.x passes (doctor.OpenCodeVerified
// is the one it was last verified on). The version comes from the server, so what
// is quoted of it is bounded.
func CheckVersion(version string) error {
	major, ok := majorVersion(version)
	if !ok {
		return fmt.Errorf("cannot read an OpenCode version from %s: this front speaks OpenCode 2's HTTP API and is not run against anything it cannot recognise as OpenCode 2; %s", quoteBounded(version), InstallHint)
	}
	switch {
	case major == 2:
		return nil
	case major == 1:
		return fmt.Errorf("the server reports OpenCode version %s: this front speaks OpenCode 2's HTTP API, which the legacy OpenCode 1 does not have; %s", quoteBounded(version), InstallHint)
	}
	return fmt.Errorf("the server reports OpenCode version %s: this front speaks OpenCode 2's HTTP API and was verified on 2.x, not on a version %d; %s", quoteBounded(version), major, InstallHint)
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
// supports, and knows the session. It returns what the server says of itself. The
// caller makes a failure a configuration error before it opens anything else, so a
// pump that was started against nothing costs the nick no account, connection or
// lock.
func Probe(ctx context.Context, c Client, session string) (ServerInfo, error) {
	info, err := c.Info(ctx)
	if err != nil {
		return ServerInfo{}, err
	}
	if err := CheckVersion(info.Version); err != nil {
		return ServerInfo{}, err
	}
	if _, err := c.Session(ctx, session); err != nil {
		return ServerInfo{}, err
	}
	return info, nil
}
