package opencode

import (
	"context"
	"fmt"
	"strconv"
	"time"
)

// recentWindow is how many of the newest messages of a session's history the front
// reads to judge where the session stands. A turn ends in an idle marker, written
// last, with the assistant message of its final step just before it, so a few are
// enough; a window that does not reach what it looks for finds nothing and says so.
const recentWindow = 4

// humanStopped is whether the human stopped the session and has sent it nothing
// since: the newest idle marker in recent, which is newest first, has the outcome
// interrupted, and no user message is newer than it. A user message newer than the
// marker is the human (or a delivery) starting the session again, and a newer idle
// marker that is not interrupted is a turn that ran after it.
func humanStopped(recent []HistoryMessage) bool {
	for _, m := range recent {
		switch m.Kind {
		case KindUser:
			return false
		case KindIdle:
			return m.Outcome == OutcomeInterrupted
		}
	}
	return false
}

// judgeTurn is the verdict on a session's newest messages as the proof that its
// turn completed. settled is false while the history does not yet end in an idle
// marker, which is a turn that is not over (or a marker not yet written). Once it
// does, the turn completed if the marker's outcome is succeeded and the assistant
// message just before it finished with "stop"; anything else is the error.
func judgeTurn(recent []HistoryMessage) (settled bool, err error) {
	if len(recent) == 0 || recent[0].Kind != KindIdle {
		return false, nil
	}
	if o := recent[0].Outcome; o != OutcomeSucceeded {
		return true, fmt.Errorf("the session went idle with the outcome %s and not %s", strconv.Quote(o), strconv.Quote(OutcomeSucceeded))
	}
	for _, m := range recent[1:] {
		switch m.Kind {
		case KindAssistant:
			if m.Finish != "stop" {
				return true, fmt.Errorf("the session went idle with the outcome %s, but the assistant message before it finished with %s and not \"stop\"", strconv.Quote(OutcomeSucceeded), strconv.Quote(m.Finish))
			}
			return true, nil
		case KindIdle:
			return true, fmt.Errorf("the session went idle with the outcome %s, but no assistant message lies between that marker and the one before it", strconv.Quote(OutcomeSucceeded))
		}
	}
	return true, fmt.Errorf("the session went idle with the outcome %s, but no assistant message is among the %d newest messages before it", strconv.Quote(OutcomeSucceeded), recentWindow-1)
}

// CertifyTurn asks the server, and not the output of the run that made the turn,
// whether the session's turn completed: its newest message must be an idle marker
// with the outcome succeeded, and the assistant message before it must have
// finished with "stop". The run's own output cannot say so: `opencode run
// --format json` can end before its last step_finish is printed, with the turn
// finished on the server. A history that does not yet end in an idle marker is read
// again every poll until within is up; anything else, and a server that does not
// answer, is the error. Each read is bounded by the client's.
func CertifyTurn(ctx context.Context, c Client, session string, within, poll time.Duration) error {
	deadline := time.Now().Add(within)
	for {
		recent, err := c.Recent(ctx, session, recentWindow)
		if err != nil {
			return fmt.Errorf("read the history of session %s: %w", session, err)
		}
		settled, err := judgeTurn(recent)
		if err != nil {
			return err
		}
		if settled {
			return nil
		}
		if !time.Now().Before(deadline) {
			newest := "an empty history"
			if len(recent) > 0 {
				newest = "a newest message of the kind " + strconv.Quote(recent[0].Kind)
			}
			return fmt.Errorf("the history of session %s does not end in an idle marker within %s (it has %s), so the turn is not shown to have completed", session, within, newest)
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
