package opencode

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// The delivery, under the loop opencode.go runs.
//
// OpenCode has a queue of its own. A prompt posted with delivery "queue" waits in
// the session's inbox, and the server turns it into a message of a turn of its own
// when the session is next free to take one: it is never merged into a running
// turn. The pump still waits for an idle session before it posts, so that what
// arrived while a turn ran goes over as one delivery and not one turn each.
//
// The delivery is made in two phases. In the first the pump chooses the id of the
// message, so it knows it before the server does, and posts; a post that got no
// answer is posted again under the same id, which the server answers with its
// first (see Client.Prompt). That phase is bounded by the read-back deadline, and
// ends when the server has admitted the prompt and said so, with the text that was
// posted. In the second the pump waits, with no deadline, for the message with
// that id to hold the posted bytes, and only then is the delivery committed.
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
// server.
type notSentError struct{ err error }

func (e *notSentError) Error() string { return e.err.Error() }
func (e *notSentError) Unwrap() error { return e.err }

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
// the caller's to start over. Anything else the server says stops the pump.
//
// The repeat is made only after the pump has attached to the server again, within
// the bound of the phase and with the backoff of the idle wait: a post that got no
// answer is the sign that the server may be another one now (in discovery mode, a
// service that restarted at another address), and a client whose attach failed is
// not one to post through. A service that stays away to the end of the bound is
// the post that got no answer, and ends the phase the same way.
func (p *Pump) handOff(ctx context.Context, id, text string) error {
	phase, cancel := context.WithTimeout(ctx, p.postTimeout)
	defer cancel()
	// reached is whether any attempt may have reached the server. Until one has,
	// a failure to connect sent nothing.
	reached := false
	reauths := 0
	var last error
	for {
		err := p.client.Prompt(phase, p.session, id, text)
		switch {
		case err == nil:
			return nil
		case ctx.Err() != nil:
			return ctx.Err()
		case phase.Err() != nil:
			// The bound of the phase cut the request short, which may have been
			// the one that reached the server.
			reached = true
		case IsTransport(err) || IsServerError(err) || p.staleCredentials(err):
			if IsTransport(err) {
				if !IsDial(err) {
					reached = true
				} else if !reached {
					return &notSentError{err: err}
				}
			} else if IsServerError(err) {
				// A service in trouble may have admitted the entry before it failed:
				// the same id makes the repeat safe, as for an answer that was lost.
				reached = true
			} else if reauths++; reauths > maxReauths {
				// A 401 is a post that was refused before anything was admitted, so
				// it is posted again under the same id once the service is found
				// again; but not for ever.
				return p.refusedAgain(err)
			}
			last = err
			p.logf("%v: posting the delivery again under message id %s", err, id)
			if !p.sleep(phase, p.readBackPoll) {
				break
			}
			if aerr := p.attachWithin(phase, p.idlePoll); aerr != nil {
				if phase.Err() == nil {
					return aerr
				}
				last = aerr
			}
		default:
			return fmt.Errorf("prompt session %s: %w", p.session, err)
		}
		if phase.Err() != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			detail := ""
			if last != nil {
				detail = " (the last attempt failed: " + last.Error() + ")"
			}
			return fmt.Errorf("the server did not answer the delivery posted to session %s as message %s within %s%s; it may have landed, and nothing was committed, so it is delivered again at the next start", p.session, id, p.postTimeout, detail)
		}
	}
}

// landing is where the wait for an admitted delivery stands: what has been said
// of it, so that each thing is said once, and the reads of an idle session.
type landing struct {
	// waiting, parked and tooLong are whether the wait behind a busy turn, the
	// parked entry and the inbox that is too large to read have been logged.
	waiting, parked, tooLong bool
	// outage is whether the server is held for: it was said not to answer, and no
	// look at the delivery has been made in full since. held is whether the look in
	// hand has had to hold for it.
	outage, held bool
	// idle is how many reads of an idle session with the delivery in the inbox, and
	// idleAt when the last was counted.
	idle   int
	idleAt time.Time
	// queued is whether the delivery was last seen in the inbox, which slows the
	// polls.
	queued bool
	// inboxAt is when the inbox is next read, and inboxBackoff how long the last wait
	// for it was: an inbox that is too large to read is asked for again on a
	// schedule of its own, and not at every poll for the message.
	inboxAt      time.Time
	inboxBackoff time.Duration
	// reauths is how many 401s in a row were answered by finding the service again.
	reauths int
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
// that got no answer, or a 5xx, holds for the server and is neither. A look that
// the server answered in every read it needed ends an outage.
func (p *Pump) landedStep(ctx context.Context, id, text string, l *landing) (done bool, err error) {
	l.held = false
	defer func() {
		if err == nil && !l.held {
			p.answered(l)
		}
	}()
	msg, found, err := p.client.Message(ctx, p.session, id)
	if err != nil {
		return false, p.readFailed(ctx, err, l)
	}
	if found {
		return true, p.confirm(msg, text)
	}
	if time.Now().Before(l.inboxAt) {
		// The inbox was too large to read a moment ago; the message is asked for at
		// the normal poll and the inbox when its turn comes.
		return false, nil
	}
	inbox, err := p.client.Inbox(ctx, p.session)
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
		l.inboxBackoff = nextBackoff(l.inboxBackoff, p.idlePoll, p.maxBackoff)
		l.inboxAt = time.Now().Add(l.inboxBackoff)
		return false, nil
	}
	if err != nil {
		return false, p.readFailed(ctx, err, l)
	}
	l.inboxBackoff, l.inboxAt = 0, time.Time{}
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
		msg, found, err = p.client.Message(ctx, p.session, id)
		if err != nil {
			return false, p.readFailed(ctx, err, l)
		}
		if found {
			return true, p.confirm(msg, text)
		}
		p.logf("delivery %s is not in session %s's inbox and is not a message of it: it was cancelled there, which only the human does, so it counts as handed over and is not sent again", id, p.session)
		return true, nil
	}

	busy, err := p.client.Busy(ctx, p.session)
	if err != nil {
		return false, p.readFailed(ctx, err, l)
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

// nextBackoff doubles prev from first up to limit.
func nextBackoff(prev, first, limit time.Duration) time.Duration {
	if prev <= 0 {
		return first
	}
	return min(prev*2, limit)
}

// readFailed decides what a failed read means. A shutdown ends the wait. A server
// that is away, or answers a 5xx (starting, restarting, in trouble), is waited for,
// since the delivery was admitted and the message may yet show, and stopping would
// have the next start post it again under an id of its own: the pump holds until
// the server answers (finding it again in discovery mode) and the look is made
// again. A session that is gone, any other status, or an answer this front cannot
// read, ends it.
func (p *Pump) readFailed(ctx context.Context, err error, l *landing) error {
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case IsTransport(err), IsServerError(err):
		return p.awaitServer(ctx, err, l)
	case p.staleCredentials(err):
		// The service restarted at the same address with a new password: it is found
		// again from its file before another read, as for a server that was away.
		if l.reauths++; l.reauths > maxReauths {
			return p.refusedAgain(err)
		}
		return p.awaitServer(ctx, err, l)
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
