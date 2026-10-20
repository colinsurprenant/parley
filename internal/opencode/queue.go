package opencode

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// The delivery for OpenCode 2, under the loop opencode.go runs for OpenCode 1.
//
// OpenCode 2 has what OpenCode 1 lacks, a queue of its own. A prompt posted with
// delivery "queue" waits in the session's inbox, and the server turns it into a
// message of a turn of its own when the session is next free to take one: it is
// never merged into a running turn, which is the one thing the delivery of
// OpenCode 1 had to be built around. The pump still waits for an idle session
// before it posts, so that what arrived while a turn ran goes over as one
// delivery and not one turn each.
//
// The delivery is made in two phases. In the first the pump chooses the id of the
// message, so it knows it before the server does, and posts; a post that got no
// answer is posted again under the same id, which the server answers with its
// first (see Client2.Prompt). That phase is bounded by the read-back deadline, and
// ends when the server has admitted the prompt and said so, with the text that was
// posted. In the second the pump waits, with no deadline, for the message with
// that id to hold the posted bytes, which is the same proof the history read-back
// gives for OpenCode 1, and only then is the delivery committed.
//
// While the message is not there yet the entry is in the inbox, and the pump
// waits; it never changes it. Two things keep an entry there. The session is busy,
// on a turn that is not this delivery's: the entry goes over when that turn ends,
// which is the wait for an idle session that the pump made before the post, made
// after it. Or the session is idle and the entry is parked: after an interrupt a
// turn that begins is steer-scoped (2.0.32, session_execution.ts), so at its end
// the entries queued during it wait, and so do the entries an interrupt with resume
// leaves, until the session takes them up. A failed turn alone does not park the
// queue (seen on 2.0.32: the next entry is taken up within milliseconds of the
// failure). A parked entry is what the human parked by interrupting, and the
// delivery waits for them to resume: an entry the pump had changed to steer would be
// promoted at the next step of whatever turn runs, with no idle precondition (so
// into a turn the human starts at that instant), and the change wakes execution,
// which releases the prompts the human held back. So there is no wake, and the one
// state the pump says nothing about to the session is "waiting".
//
// The third thing that happens to an entry is that it is not in the inbox and the
// message is not there. The input was cancelled, and only the human does that, on
// purpose. It is logged and counted as handed over, and never sent again: resending
// what the human took back would undo it. The entry leaves the inbox and the message
// appears in one step on the server (the inbox row is deleted in the transaction that
// inserts the message), so an entry that is gone has either become a message or been
// cancelled, and a second read of the message tells which.
//
// There is no settle step. A message exists once the turn that took it up has
// begun, and a post that lands behind a turn that has not been seen to begin is
// a queued one, which waits for its turn and cannot be merged into the one
// before.
//
// What this does not close: a post whose answer is lost is posted again under the
// same id, and if the human deleted the admitted entry in that instant, the second
// post admits it once more, since OpenCode keeps no tombstone of a cancelled id
// (inbox.ts, reconcile and cancel). The delivery then goes over although the human
// took it back, once. That is the at-least-once bargain of the other fronts, in a
// narrower place.

// parkedPolls is how many reads of an idle session with the delivery still in
// the inbox, spaced by the idle poll, it takes to say the delivery is parked.
const parkedPolls = 2

// notSentError is a delivery that provably did not reach the server: every
// attempt to post it failed to connect. The loop goes back to waiting for the
// server, as it does for OpenCode 1.
type notSentError struct{ err error }

func (e *notSentError) Error() string { return e.err.Error() }
func (e *notSentError) Unwrap() error { return e.err }

// busy reads whether the session is in a turn, from whichever server it speaks to.
func (p *Pump) busy(ctx context.Context) (bool, error) {
	if p.client2 != nil {
		return p.client2.Busy(ctx, p.session)
	}
	return p.client.Busy(ctx, p.session, p.directory)
}

// attach2 is attach for OpenCode 2: the server answers, is a version this front
// speaks to, and knows the session. In discovery mode it first finds the service
// again from its registration file, which the service rewrites when it starts
// (another address, password and process id): a pump that reads the file once would
// hold for a service that is not there any more. Asking the server who it is comes
// before asking for the session (Discovery.Find), so a stale file reads as stale.
//
// A service that is not there (no file, or one that does not describe the server
// that answers), or is starting (a 5xx), is a server that is away, and is returned
// as a transport failure, which the loop holds for with its backoff. A 401 after the
// file was read again is not: the password in the file is the service's own, and it
// is refused. With a port nothing is found again, and the errors are the server's
// own, as for OpenCode 1.
func (p *Pump) attach2(ctx context.Context) error {
	if p.discovery != nil {
		_, c, _, err := p.discovery.Find(ctx)
		if err != nil {
			return holdForService(err)
		}
		p.client2 = c
	} else {
		info, err := p.client2.Info(ctx)
		if err != nil {
			return err
		}
		if err := CheckVersion2(info.Version); err != nil {
			return err
		}
	}
	s, err := p.client2.Session(ctx, p.session)
	if err != nil {
		if p.discovery != nil {
			return holdForService(err)
		}
		return err
	}
	p.directory = s.Directory
	return nil
}

// holdForService turns the failures that mean "the service is not up yet" into a
// transport failure, which is what the loop holds for, and leaves every other
// failure as it is.
func holdForService(err error) error {
	var stale *StaleServiceError
	var se *StatusError
	switch {
	case IsTransport(err):
		return err
	case errors.Is(err, ErrNoService), errors.As(err, &stale):
		return &TransportError{Op: "find the OpenCode 2 service", Err: err}
	case errors.As(err, &se) && se.Code >= http.StatusInternalServerError:
		return &TransportError{Op: "ask the OpenCode 2 service", Err: err}
	}
	return err
}

// awaitServer2 holds, after a request to the server failed to get an answer, until
// the server answers again, with the backoff the idle wait uses, and says so once
// when it goes and once when it is back. Whatever else fails is the end of the wait.
// The entry the pump is waiting on is in the server's database and survives its
// restart.
func (p *Pump) awaitServer2(ctx context.Context, cause error) error {
	p.logf("OpenCode is not answering (%v): the delivery is queued there, holding for the server", cause)
	backoff := p.idlePoll
	for {
		if !p.sleep(ctx, backoff) {
			return ctx.Err()
		}
		backoff = min(backoff*2, p.maxBackoff)
		err := p.attach(ctx)
		switch {
		case err == nil:
			p.logf("OpenCode is back")
			return nil
		case ctx.Err() != nil:
			return ctx.Err()
		case IsTransport(err):
		default:
			return err
		}
	}
}

// CheckVersion2 refuses a server this OpenCode 2 client cannot speak to: one whose
// major version is not 2, or whose version cannot be read. 2.0.32 is the version
// the client was checked against, in OpenCode's simulation mode.
func CheckVersion2(version string) error {
	major, ok := majorVersion(version)
	if !ok {
		return fmt.Errorf("cannot read an OpenCode version from %s: this client speaks OpenCode 2's HTTP API and is not run against anything it cannot recognise as OpenCode 2; %s", quoteBounded(version), Install2Hint)
	}
	if major != 2 {
		return fmt.Errorf("the server reports OpenCode version %s, not OpenCode 2: this client speaks OpenCode 2's HTTP API; %s", quoteBounded(version), InstallHint)
	}
	return nil
}

// Probe2 is Probe for OpenCode 2: the server answers, speaks a version this front
// supports, and knows the session. It returns what the server says of itself.
func Probe2(ctx context.Context, c Client2, session string) (ServerInfo, error) {
	info, err := c.Info(ctx)
	if err != nil {
		return ServerInfo{}, err
	}
	if err := CheckVersion2(info.Version); err != nil {
		return ServerInfo{}, err
	}
	if _, err := c.Session(ctx, session); err != nil {
		return ServerInfo{}, err
	}
	return info, nil
}

// deliverQueued hands text to the session and returns once the session holds it
// as a message, or once the human is seen to have cancelled it. A *notSentError
// is a delivery that did not reach the server at all; any other error leaves it
// not known to have landed that the stream will not deliver again.
func (p *Pump) deliverQueued(ctx context.Context, text string) error {
	id, err := newMessageID()
	if err != nil {
		return err
	}
	if err := p.handOff(ctx, id, text); err != nil {
		return err
	}
	return p.landed(ctx, id, text)
}

// handOff posts the delivery under id. A post that got no answer may have been
// admitted, and is posted again under the same id until the server answers or
// the read-back bound passes; one that never connected has sent nothing, and is
// the caller's to start over. Anything else the server says stops the pump. In
// discovery mode the service is found again between the tries, since a service
// that restarted is at another address.
func (p *Pump) handOff(ctx context.Context, id, text string) error {
	phase, cancel := context.WithTimeout(ctx, p.readBackTimeout)
	defer cancel()
	// reached is whether any attempt may have reached the server. Until one has,
	// a failure to connect sent nothing.
	reached := false
	var last error
	for {
		err := p.client2.Prompt(phase, p.session, id, text)
		switch {
		case err == nil:
			return nil
		case ctx.Err() != nil:
			return ctx.Err()
		case phase.Err() != nil:
			// The bound of the phase cut the request short, which may have been
			// the one that reached the server.
			reached = true
		case IsTransport(err):
			if !IsDial(err) {
				reached = true
			} else if !reached {
				return &notSentError{err: err}
			}
			last = err
			p.logf("%v: posting the delivery again under message id %s", err, id)
			if p.discovery != nil {
				if aerr := p.attach(phase); aerr != nil && !IsTransport(aerr) && phase.Err() == nil {
					return aerr
				}
			}
		default:
			return fmt.Errorf("prompt session %s: %w", p.session, err)
		}
		if phase.Err() != nil || !p.sleep(phase, p.readBackPoll) {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			detail := ""
			if last != nil {
				detail = " (the last attempt failed: " + last.Error() + ")"
			}
			return fmt.Errorf("the server did not answer the delivery posted to session %s as message %s within %s%s; it may have landed, and nothing was committed, so it is delivered again at the next start", p.session, id, p.readBackTimeout, detail)
		}
	}
}

// landing is where the wait for an admitted delivery stands: what has been said
// of it, so that each thing is said once, and the reads of an idle session.
type landing struct {
	// waiting, parked and tooLong are whether the wait behind a busy turn, the
	// parked entry and the inbox that is too large to read have been logged.
	waiting, parked, tooLong bool
	// idle is how many reads of an idle session with the delivery in the inbox, and
	// idleAt when the last was counted.
	idle   int
	idleAt time.Time
	// queued is whether the delivery was last seen in the inbox, which slows the
	// polls.
	queued bool
}

// landed waits for the delivery under id, which the server has admitted, to become
// a message holding text, for the outcomes the comment at the top of the file lays
// out. It has no deadline of its own: the entry is in the server's inbox, and
// giving up on it would send it twice at the next start. It ends when the message
// holds the bytes, when the entry was cancelled, when the context ends, and on an
// error that waiting does not mend (the session gone, an answer it cannot read, a
// message that holds other text).
func (p *Pump) landed(ctx context.Context, id, text string) error {
	var l landing
	for {
		done, err := p.landedStep(ctx, id, text, &l)
		switch {
		case err != nil:
			return err
		case done:
			return nil
		case ctx.Err() != nil:
			return ctx.Err()
		}
		poll := p.readBackPoll
		if l.queued {
			poll = p.idlePoll
		}
		if !p.sleep(ctx, poll) {
			return ctx.Err()
		}
	}
}

// landedStep is one look at where the delivery is. It returns done if it is a
// message (or was cancelled), and an error for anything that ends the wait; a look
// that got no answer holds for the server and is neither.
func (p *Pump) landedStep(ctx context.Context, id, text string, l *landing) (bool, error) {
	msg, found, err := p.client2.Message(ctx, p.session, id)
	if err != nil {
		return false, p.readFailed(ctx, err)
	}
	if found {
		return true, p.confirm(msg, text)
	}
	inbox, err := p.client2.Inbox(ctx, p.session)
	if IsTooLong(err) {
		// The inbox carries every queued entry's payload, attachments included, and the
		// human's own queued prompts can pass the bound it is read under. With it
		// unread, an entry that is gone cannot be told from one that is there, so the
		// pump goes on asking for the message and infers nothing: it neither calls the
		// delivery cancelled nor gives up.
		if !l.tooLong {
			p.logf("session %s's inbox is over the %d bytes this front reads, so it cannot be told whether delivery %s is still queued; the human's queued attachments are the likely cause. Waiting for the message itself", p.session, tooLongLimit(err), id)
			l.tooLong = true
		}
		l.queued = false
		return false, nil
	}
	if err != nil {
		return false, p.readFailed(ctx, err)
	}
	var item *InboxItem
	for i := range inbox {
		if inbox[i].ID == id {
			item = &inbox[i]
			break
		}
	}
	l.queued = item != nil
	if item == nil {
		// Gone from the inbox since the read above: promoted in between, or
		// cancelled. Promotion deletes the entry and creates the message in one
		// step, so the message is there now if that is what it was.
		msg, found, err = p.client2.Message(ctx, p.session, id)
		if err != nil {
			return false, p.readFailed(ctx, err)
		}
		if found {
			return true, p.confirm(msg, text)
		}
		p.logf("delivery %s is not in session %s's inbox and is not a message of it: it was cancelled there, which only the human does, so it counts as handed over and is not sent again", id, p.session)
		return true, nil
	}

	busy, err := p.client2.Busy(ctx, p.session)
	if err != nil {
		return false, p.readFailed(ctx, err)
	}
	if busy {
		if !l.waiting {
			p.logf("delivery %s is queued behind a turn of session %s that is not its own; waiting for it", id, p.session)
			l.waiting = true
		}
		l.idle = 0
		return false, nil
	}

	// In the inbox and the session idle. Right after the post that is the moment
	// before the session takes it up, so it takes two reads, apart, to say it is
	// parked.
	if now := time.Now(); l.idle == 0 || now.Sub(l.idleAt) >= p.idlePoll {
		l.idle++
		l.idleAt = now
	}
	if l.idle >= parkedPolls && !l.parked {
		p.logf("delivery %s sits in session %s's inbox with the session idle: parked after an interrupt; it goes over when the session takes it up", id, p.session)
		l.parked = true
	}
	return false, nil
}

// readFailed decides what a failed read means. A shutdown ends the wait. A server
// that is away is waited for, since the delivery was admitted and the message may
// yet show: the pump holds until the server answers (finding it again in discovery
// mode) and the look is made again. A session that is gone, or an answer this front
// cannot read, ends it.
func (p *Pump) readFailed(ctx context.Context, err error) error {
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case IsTransport(err):
		return p.awaitServer2(ctx, err)
	case errors.Is(err, ErrSessionNotFound):
		return fmt.Errorf("the delivery was posted to session %s, which the server no longer knows, so it cannot be confirmed; nothing was committed: %w", p.session, err)
	case IsTooLong(err):
		return fmt.Errorf("the delivery was posted to session %s, but the server's answer about it is over the %d bytes this front reads, so it cannot be confirmed; nothing was committed, so it is delivered again at the next start: %w", p.session, tooLongLimit(err), err)
	default:
		return fmt.Errorf("read back the delivery from session %s: %w", p.session, err)
	}
}

// confirm is the proof that the delivery landed: the message holds exactly the
// bytes that were posted. The id was this front's own, so anything else under it
// is the server not keeping what it was given, and is not a delivery.
func (p *Pump) confirm(msg UserMessage, text string) error {
	if msg.Text != text {
		return fmt.Errorf("message %s of session %s holds %d bytes of text and not the %d posted: the delivery is not shown to have landed, and nothing was committed", msg.ID, p.session, len(msg.Text), len(text))
	}
	return nil
}
