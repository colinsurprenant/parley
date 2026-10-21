package opencode

import (
	"context"
	"fmt"
	"strconv"
	"time"
)

const (
	// recentWindow is how many messages of a session's history the front asks for in
	// a page. A turn ends in an idle marker, written last, with the assistant message
	// of its final step just before it, so the first page usually decides.
	recentWindow = 4
	// maxHistoryPages bounds how far back the front reads to meet what it looks for:
	// recentWindow messages a page, so recentWindow * maxHistoryPages in all. A
	// history of other entries (a skill, a shell, a compaction) can be longer than
	// one page, and one page that holds neither a user message nor an idle marker
	// says nothing; but there is a bound, and a history that passes it unanswered is
	// inconclusive.
	maxHistoryPages = 10
)

// readHistory reads the newest messages of the session's history, newest first, a
// page at a time, until done says what has been read is enough, the history ends, or
// maxHistoryPages pages have been read. It returns everything read and whether the
// history ended (no older message is left to read). A page that decided is the last
// one read, so the common case is one request.
func readHistory(ctx context.Context, c Client, session string, done func([]HistoryMessage) bool) (msgs []HistoryMessage, ended bool, err error) {
	cursor := ""
	for page := 0; page < maxHistoryPages; page++ {
		got, next, err := c.Recent(ctx, session, recentWindow, cursor)
		if err != nil {
			return msgs, false, err
		}
		msgs = append(msgs, got...)
		if done(msgs) {
			return msgs, false, nil
		}
		if next == "" || len(got) == 0 {
			return msgs, true, nil
		}
		cursor = next
	}
	return msgs, false, nil
}

// holdState is what the history says about whether the human stopped the session.
type holdState int

const (
	// holdNone: nothing says the human stopped it that they have not undone.
	holdNone holdState = iota
	// holdStopped: the newest idle marker has the outcome interrupted, and no user
	// message is newer: the human stopped the session and has sent it nothing since.
	holdStopped
	// holdUndecided: neither a user message nor an idle marker has been met yet.
	holdUndecided
)

// judgeStopped is what msgs, newest first, say about whether the human stopped the
// session. The first user message or idle marker in them decides: a user message is
// the human (or a delivery) starting the session, newer than any marker behind it;
// an idle marker is a turn that ended, and it decides by its outcome. Every other kind
// (a skill, a shell, an assistant message) is read past, however many there are.
func judgeStopped(msgs []HistoryMessage) holdState {
	for _, m := range msgs {
		switch m.Kind {
		case KindUser:
			return holdNone
		case KindIdle:
			if m.Outcome == OutcomeInterrupted {
				return holdStopped
			}
			return holdNone
		}
	}
	return holdUndecided
}

// checkStopped reads back through the history until a user message or an idle marker
// decides whether the human stopped the session. A history that ends with neither has
// nothing in it to say so, and is not stopped; one that is longer than the bound
// without either is inconclusive, and returns holdUndecided, which the caller holds for
// as it does for a stop, because releasing it could start a session the human
// stopped.
func checkStopped(ctx context.Context, c Client, session string) (holdState, error) {
	msgs, ended, err := readHistory(ctx, c, session, func(m []HistoryMessage) bool { return judgeStopped(m) != holdUndecided })
	if err != nil {
		return holdNone, err
	}
	st := judgeStopped(msgs)
	if st == holdUndecided && ended {
		st = holdNone
	}
	return st, nil
}

// verdict is where a turn stands in the history read so far.
type verdict int

const (
	// needMore: the messages so far do not decide; older ones are needed.
	needMore verdict = iota
	// notOver: the turn is not over: a user or assistant message is newer than the
	// newest idle marker, or there is no idle marker.
	notOver
	// over: the turn ended, and the error says whether it went through.
	over
)

// judgeTurn is the verdict on a session's history, newest first, as the proof that
// its turn completed: the newest idle marker, with nothing but other kinds (a skill, a
// shell) newer, must have the outcome succeeded, and the assistant message before it
// must have finished with "stop". A user or assistant message newer than the marker is
// another turn, begun and not over. final says no older messages will come: what would
// need more is then a turn that is not over (no idle marker), or, once the marker is
// found, an error that no assistant message is before it.
func judgeTurn(msgs []HistoryMessage, final bool) (verdict, error) {
	i := 0
	for ; i < len(msgs); i++ {
		if k := msgs[i].Kind; k == KindUser || k == KindAssistant {
			return notOver, nil
		} else if k == KindIdle {
			break
		}
	}
	if i == len(msgs) {
		if final {
			return notOver, nil
		}
		return needMore, nil
	}
	if o := msgs[i].Outcome; o != OutcomeSucceeded {
		return over, fmt.Errorf("the session went idle with the outcome %s and not %s", strconv.Quote(o), strconv.Quote(OutcomeSucceeded))
	}
	for _, m := range msgs[i+1:] {
		switch m.Kind {
		case KindAssistant:
			if m.Finish != "stop" {
				return over, fmt.Errorf("the session went idle with the outcome %s, but the assistant message before it finished with %s and not \"stop\"", strconv.Quote(OutcomeSucceeded), strconv.Quote(m.Finish))
			}
			return over, nil
		case KindIdle:
			return over, fmt.Errorf("the session went idle with the outcome %s, but no assistant message lies between that marker and the one before it", strconv.Quote(OutcomeSucceeded))
		case KindUser:
			return over, fmt.Errorf("the session went idle with the outcome %s, but no assistant message answers the prompt before it", strconv.Quote(OutcomeSucceeded))
		}
	}
	if final {
		return over, fmt.Errorf("the session went idle with the outcome %s, but no assistant message is among the %d messages before it that were read", strconv.Quote(OutcomeSucceeded), len(msgs)-i-1)
	}
	return needMore, nil
}

// CertifyTurn asks the server, and not the output of the run that made the turn,
// whether the session's turn completed: its newest idle marker must have the outcome
// succeeded, and the assistant message before it must have finished with "stop". The
// run's own output cannot say so: `opencode run --format json` can end before its
// last step_finish is printed, with the turn finished on the server. The history is
// read back a page at a time until that is decided (judgeTurn), and a history that
// does not yet show the turn over is read again every poll until within is up;
// anything else, and a server that does not answer, is the error. Each read is
// bounded by the client's.
func CertifyTurn(ctx context.Context, c Client, session string, within, poll time.Duration) error {
	deadline := time.Now().Add(within)
	for {
		msgs, _, err := readHistory(ctx, c, session, func(m []HistoryMessage) bool {
			v, _ := judgeTurn(m, false)
			return v != needMore
		})
		if err != nil {
			return fmt.Errorf("read the history of session %s: %w", session, err)
		}
		v, err := judgeTurn(msgs, true)
		if v == over {
			return err
		}
		if !time.Now().Before(deadline) {
			newest := "an empty history"
			if len(msgs) > 0 {
				newest = "a newest message of the kind " + strconv.Quote(msgs[0].Kind)
			}
			return fmt.Errorf("the history of session %s does not show the turn over within %s (it has %s), so the turn is not shown to have completed", session, within, newest)
		}
		t := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}
