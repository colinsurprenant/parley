package opencode

import (
	"context"
	"errors"
	"fmt"
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
// The delivery is made in three steps. The pump chooses the id of the message,
// so it knows it before the server does; it posts, and a post that got no answer
// is posted again under the same id, which the server answers with its first
// (see Client2.Prompt). It then reads the message back by that id until the
// server shows it, with the bytes that were posted, which is the same proof the
// history read-back gives for OpenCode 1, and only then is the delivery
// committed.
//
// Between the post and the message there is the inbox, and what the pump does
// while the message is not there yet is the part that has no counterpart on
// OpenCode 1:
//
//   - The entry is in the inbox and the session is busy. The session is on a
//     turn that is not this delivery's, and the delivery waits behind it. That
//     is not a failure and has no bound: it is the wait for an idle session that
//     the pump makes before it posts, made after. The bound of the read-back
//     begins again for as long as the session is seen busy.
//   - The entry is in the inbox and the session is idle, on two reads apart. An
//     interrupt leaves the queue parked: a turn that begins after one is
//     steer-scoped (2.0.32, session_execution.ts), so at its end the entries
//     queued during it wait, and nothing takes them up until something changes
//     one. A failed turn alone does not park the queue (seen on 2.0.32: the next
//     entry is taken up within milliseconds of the failure). The pump changes the
//     entry to steer, once, which is what wakes the session, and logs that it did.
//   - The entry is not in the inbox and the message is not there. The input was
//     cancelled, and only the human does that, on purpose. It is logged and
//     counted as handed over, and never sent again: resending what the human
//     took back would undo it.
//
// The entry leaves the inbox and the message appears in one step on the server
// (the inbox row is deleted in the transaction that inserts the message), so an
// entry that is gone has either become a message or been cancelled, and a second
// read of the message tells which.
//
// There is no settle step. A message exists once the turn that took it up has
// begun, and a post that lands behind a turn that has not been seen to begin is
// a queued one, which waits for its turn and cannot be merged into the one
// before.

// parkedPolls is how many reads of an idle session with the delivery still in
// the inbox, spaced by the idle poll, it takes to call the delivery parked.
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
// speaks to, and knows the session.
func (p *Pump) attach2(ctx context.Context) error {
	info, err := p.client2.Info(ctx)
	if err != nil {
		return err
	}
	if err := CheckVersion2(info.Version); err != nil {
		return err
	}
	s, err := p.client2.Session(ctx, p.session)
	if err != nil {
		return err
	}
	p.directory = s.Directory
	return nil
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
// the caller's to start over. Anything else the server says stops the pump.
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

// landing is where the wait for a posted delivery stands.
type landing struct {
	// deadline is when the wait gives up. Seeing the delivery behind a busy
	// session, or waking it, starts the bound again (progress).
	deadline time.Time
	progress bool
	// waiting is whether the wait behind a busy session has been logged.
	waiting bool
	// idle is how many reads of an idle session with the delivery in the inbox,
	// and idleAt when the last was counted.
	idle   int
	idleAt time.Time
	// steered is whether the delivery was changed to steer.
	steered bool
	// queued is whether the delivery was last seen in the inbox, and last the last
	// read that failed to get an answer, for the timeout to say.
	queued bool
	last   error
}

// landed waits for the delivery under id to become a message holding text, for
// the three outcomes the comment at the top of the file lays out. Every call it
// makes carries the deadline of the wait, so a request that stalls is cut at it.
func (p *Pump) landed(ctx context.Context, id, text string) error {
	l := landing{deadline: time.Now().Add(p.readBackTimeout)}
	for {
		if l.progress {
			l.deadline, l.progress = time.Now().Add(p.readBackTimeout), false
		}
		phase, cancel := context.WithDeadline(ctx, l.deadline)
		done, err := p.landedStep(ctx, phase, id, text, &l)
		cut := !l.progress && phase.Err() != nil && !time.Now().Before(l.deadline)
		cancel()
		switch {
		case err != nil:
			return err
		case done:
			return nil
		case ctx.Err() != nil:
			return ctx.Err()
		case cut:
			return l.timeout(p, id)
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

// timeout is the error for a delivery that did not become a message in time.
func (l *landing) timeout(p *Pump, id string) error {
	state := "it was not seen in the session's inbox"
	switch {
	case l.steered:
		state = "it sat in the session's inbox, changed to steer, and the session did not take it up"
	case l.queued:
		state = "it sat in the session's inbox"
	case l.last != nil:
		state = "the last read failed: " + l.last.Error()
	}
	return fmt.Errorf("the server accepted the delivery as message %s but it did not become a message of session %s within %s (%s); nothing was committed, so it is delivered again at the next start, and an entry still in the inbox is then delivered twice", id, p.session, p.readBackTimeout, state)
}

// landedStep is one look at where the delivery is. It returns done if it is a
// message (or was cancelled), and an error for anything that ends the wait; a
// look that got no answer is neither, and is tried again. parent is the caller's
// context and phase the one with the wait's deadline.
func (p *Pump) landedStep(parent, phase context.Context, id, text string, l *landing) (bool, error) {
	msg, found, err := p.client2.Message(phase, p.session, id)
	if err != nil {
		return false, p.readFailed(parent, phase, err, l)
	}
	if found {
		return true, p.confirm(msg, text)
	}
	inbox, err := p.client2.Inbox(phase, p.session)
	if err != nil {
		return false, p.readFailed(parent, phase, err, l)
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
		msg, found, err = p.client2.Message(phase, p.session, id)
		if err != nil {
			return false, p.readFailed(parent, phase, err, l)
		}
		if found {
			return true, p.confirm(msg, text)
		}
		p.logf("delivery %s is not in session %s's inbox and is not a message of it: it was cancelled there, which only the human does, so it counts as handed over and is not sent again", id, p.session)
		return true, nil
	}

	busy, err := p.client2.Busy(phase, p.session)
	if err != nil {
		return false, p.readFailed(parent, phase, err, l)
	}
	if busy {
		if !l.waiting {
			p.logf("delivery %s is queued behind a turn of session %s that is not its own; waiting for it", id, p.session)
			l.waiting = true
		}
		l.progress, l.idle = true, 0
		return false, nil
	}

	// In the inbox and the session idle. Right after the post that is the moment
	// before the session takes it up, so it takes two reads, apart, to call it
	// parked.
	if now := time.Now(); l.idle == 0 || now.Sub(l.idleAt) >= p.idlePoll {
		l.idle++
		l.idleAt = now
	}
	if l.idle < parkedPolls || l.steered || item.Delivery != "queue" {
		return false, nil
	}
	switch err := p.client2.Steer(phase, p.session, id); {
	case err == nil:
		p.logf("delivery %s sat in session %s's inbox while the session was idle, as after an interrupt: changed it to steer, which makes the session take it up", id, p.session)
		l.steered, l.progress = true, true
	case errors.Is(err, ErrConflict):
		// It was promoted, cancelled or already steered since the read; the next
		// look says which.
	default:
		return false, p.readFailed(parent, phase, err, l)
	}
	return false, nil
}

// readFailed decides what a failed read means. A shutdown ends the wait; the
// bound of the wait cutting a call short, or a server that is away, does not,
// because the delivery was accepted and the message may yet show; a session that
// is gone, or an answer this front cannot read, does.
func (p *Pump) readFailed(parent, phase context.Context, err error, l *landing) error {
	switch {
	case parent.Err() != nil:
		return parent.Err()
	case phase.Err() != nil:
		return nil
	case IsTransport(err):
		l.last = err
		return nil
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
