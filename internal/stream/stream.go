// Package stream defines the chat primitive parley is built on: a
// transport-agnostic Stream a participant posts to and blocks on, plus the
// participant-side delivery layer in deliver.go (echo suppression,
// de-duplication, gap reporting, the peer-header rendering, and, for a table, a
// round-one seal and a cap breaker that hold other seats' messages in memory).
//
// The transport is the sequencing authority. A message's ID and TS are assigned
// by the adapter — a byte offset and the writer's append time for the file
// adapter, an IRCv3 msgid and server-time for IRC — never by this package, so
// that a stock client writing to the same transport produces messages parley
// can order exactly like its own.
//
// Wait is a method on Stream rather than a core helper wrapped around Read,
// because adapters differ in how they learn that a message arrived. Adapters
// with no push channel implement it by embedding PollWait, the generic
// poll-Read-until-quiet loop below: that is what the file adapter does, and
// Wait is the only method the IRC adapter will implement differently, driving
// the same debounce off server push instead of a timer. Keeping Wait on the
// interface means the core never has to know which kind of adapter it holds.
package stream

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// DefaultPollInterval is the change-detection interval used by PollWait when an
// adapter does not supply one.
const DefaultPollInterval = 100 * time.Millisecond

// Message is one post on the stream. ID and TS come from the transport.
type Message struct {
	ID     string
	Author string
	TS     time.Time
	Body   string
}

// missingField is what a header and a receipt both print for an ID or a TS the
// transport did not report.
const missingField = "-"

// idField is the message's ID as a header and a receipt print it: the ID itself,
// or "-" when the transport reported none.
func (m Message) idField() string {
	if m.ID == "" {
		return missingField
	}
	return m.ID
}

// tsField is the message's TS as a header and a receipt print it: RFC 3339 in
// UTC, or "-" for the zero time, which is what a transport that reported none
// leaves and not something a real message's TS ever is.
func (m Message) tsField() string {
	if m.TS.IsZero() {
		return missingField
	}
	return m.TS.UTC().Format(time.RFC3339)
}

// Receipt is the one line `parley send` prints for a message it posted, with the
// ID and TS spelled exactly as a Delivery's header spells them — both come from
// idField and tsField — so a participant can match it against what another
// seat's wait prints for the same message. A field the transport did not report
// is "-" in both, the header included.
func (m Message) Receipt() string {
	return "[parley] sent id=" + m.idField() + " ts=" + m.tsField()
}

// Cursor is an opaque transport position just past the last message returned.
// It is assigned by the adapter and meaningful only to it: it is not a message
// ID, and nothing outside the adapter may parse, compare or order it. The zero
// Cursor means the start of the transport's retained history.
type Cursor string

// Stream is one chat transport. Implementations must be safe for concurrent
// use by multiple goroutines.
type Stream interface {
	// Send posts one message authored by this participant and returns it as the
	// transport recorded it: the ID and TS a reader of the stream sees for that
	// message, which are the transport's to assign, never the caller's. A field
	// the transport did not report comes back as its zero value: the post has
	// happened by then, so that is not an error, and Receipt says so. On an
	// error nothing was posted, or the transport cannot say whether anything was.
	Send(ctx context.Context, body string) (Message, error)

	// Read returns every message after cursor, in stream order, along with the
	// cursor just past the last message returned. It does not block. If cursor
	// predates the transport's retained history, Read returns a
	// *HistoryGapError and the cursor to restart from.
	Read(ctx context.Context, cursor Cursor) ([]Message, Cursor, error)

	// Wait blocks until at least one message lands after cursor or timeout
	// expires, then returns everything queued plus whatever arrives within
	// debounce of the last arrival, so a burst becomes one delivery. It returns
	// no later than timeout+debounce after it was called, however busy the
	// stream is: the quiet window can delay the return, but only once. A
	// timeout with nothing to deliver returns no messages, the unchanged cursor
	// and a nil error.
	Wait(ctx context.Context, cursor Cursor, timeout, debounce time.Duration) ([]Message, Cursor, error)
}

// Reader is the part of Stream that PollWait drives.
type Reader interface {
	Read(ctx context.Context, cursor Cursor) ([]Message, Cursor, error)
}

// ErrHistoryGap matches every history-gap error via errors.Is; use
// *HistoryGapError with errors.As to recover the cursors.
var ErrHistoryGap = errors.New("cursor predates retained history")

// HistoryGapError reports that the transport no longer retains the history
// after Cursor. Restart is the earliest position still retained. The
// participant is told about the gap (see Delivery.Render) rather than having
// messages silently skipped.
type HistoryGapError struct {
	Cursor  Cursor
	Restart Cursor
}

func (e *HistoryGapError) Error() string {
	return fmt.Sprintf("history after cursor %q is no longer retained (restart at %q)", string(e.Cursor), string(e.Restart))
}

func (e *HistoryGapError) Unwrap() error { return ErrHistoryGap }

// PollWait implements Stream.Wait for adapters whose only change signal is
// re-reading: it polls Read every interval until a message lands or timeout
// expires, then keeps polling until debounce elapses with nothing new, so a
// burst of messages is returned as one delivery. It returns by timeout+debounce
// at the latest, a bound it also imposes on each Read it makes. Any messages
// already collected are returned alongside an error, so a gap detected
// mid-burst does not discard what was read before it.
func PollWait(ctx context.Context, r Reader, cursor Cursor, timeout, debounce, interval time.Duration) ([]Message, Cursor, error) {
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	var batch []Message
	deadline := time.Now().Add(timeout)
	// A stream busier than the quiet window would otherwise extend the wait for
	// as long as it keeps talking; the caller asked for a bounded call, so the
	// hard cap wins over the debounce.
	hard := deadline.Add(debounce)
	// The cap binds each Read as well as the loop around them. A backlog larger
	// than one Read can drain, or a file still growing under it, would otherwise
	// spend the whole bound inside a single call and return past it.
	readCtx, cancel := context.WithDeadline(ctx, hard)
	defer cancel()
	for {
		msgs, next, err := r.Read(readCtx, cursor)
		if len(msgs) > 0 {
			batch = append(batch, msgs...)
		}
		if err != nil {
			// The cap firing is the bound working, not a failure: the Read
			// returned what it had read, which is what the caller asked for. The
			// caller's own cancellation still propagates, so the two are told
			// apart by whose context is done.
			if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
				if next != "" {
					cursor = next
				}
				return batch, cursor, nil
			}
			// A failing Read that reports no position must not discard the
			// position earlier successful polls established; a caller that
			// persists the returned cursor would otherwise restart from zero.
			if next != "" {
				cursor = next
			}
			return batch, cursor, err
		}
		cursor = next
		if len(msgs) > 0 {
			// Every arrival restarts the quiet window, so an agent writing
			// three lines in a row is delivered once rather than three times.
			deadline = time.Now().Add(debounce)
			if deadline.After(hard) {
				deadline = hard
			}
		}
		wait := time.Until(deadline)
		if wait <= 0 {
			return batch, cursor, nil
		}
		if err := sleep(ctx, min(interval, wait)); err != nil {
			return batch, cursor, err
		}
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
