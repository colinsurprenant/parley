package stream

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestPollWaitIsBoundedByTimeoutPlusDebounce(t *testing.T) {
	const (
		timeout  = 200 * time.Millisecond
		debounce = 100 * time.Millisecond
	)
	f := &fake{nick: "claude"}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// A stream that never falls quiet: every arrival would extend the
		// debounce window if the hard cap did not win.
		end := time.After(3 * time.Second)
		for {
			select {
			case <-stop:
				return
			case <-end:
				return
			case <-time.After(debounce / 2):
				f.add("codex", "chatter")
			}
		}
	}()
	defer func() {
		close(stop)
		wg.Wait()
	}()

	start := time.Now()
	msgs, _, err := PollWait(context.Background(), f, "", timeout, debounce, 5*time.Millisecond)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("PollWait: %v", err)
	}
	if len(msgs) == 0 {
		t.Fatal("returned nothing from a stream that never stopped talking")
	}
	if elapsed < timeout {
		t.Fatalf("returned after %v, want at least the timeout %v", elapsed, timeout)
	}
	if elapsed > timeout+debounce+time.Second {
		t.Fatalf("returned after %v, want no later than timeout+debounce (%v)", elapsed, timeout+debounce)
	}
}

// endlessReader is an adapter whose backlog never runs out: one Read keeps
// parsing messages until its context is done, then hands back everything it has
// with the context's error, as the file adapter does over a file that grows as
// fast as it is read. Without a bound on the Read itself, one call would run
// forever.
type endlessReader struct {
	mu   sync.Mutex
	next int
}

func (r *endlessReader) Read(ctx context.Context, c Cursor) ([]Message, Cursor, error) {
	var msgs []Message
	for {
		if err := ctx.Err(); err != nil {
			return msgs, c, err
		}
		r.mu.Lock()
		r.next++
		id := r.next
		r.mu.Unlock()
		msgs = append(msgs, Message{ID: strconv.Itoa(id), Author: "codex", Body: "chatter"})
		c = Cursor(strconv.Itoa(id))
		time.Sleep(time.Millisecond)
	}
}

func TestPollWaitBoundsASingleRead(t *testing.T) {
	const (
		timeout  = 150 * time.Millisecond
		debounce = 100 * time.Millisecond
	)
	r := &endlessReader{}
	start := time.Now()
	msgs, cursor, err := PollWait(context.Background(), r, "", timeout, debounce, 5*time.Millisecond)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("PollWait: %v", err)
	}
	if len(msgs) == 0 {
		t.Fatal("returned nothing from a reader that never stopped producing")
	}
	if cursor != Cursor(strconv.Itoa(len(msgs))) {
		t.Fatalf("cursor %q, want %q: the position the cut-short read reported", cursor, strconv.Itoa(len(msgs)))
	}
	if elapsed > timeout+debounce+2*time.Second {
		t.Fatalf("returned after %v, want no later than timeout+debounce (%v)", elapsed, timeout+debounce)
	}
}

func TestPollWaitPropagatesTheCallersCancel(t *testing.T) {
	r := &endlessReader{}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	// The bound is far away, so only the caller's cancel can end this: it is an
	// error the caller must see, not the cap quietly returning early.
	msgs, _, err := PollWait(ctx, r, "", time.Minute, time.Minute, 5*time.Millisecond)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v, want context.Canceled", err)
	}
	if len(msgs) == 0 {
		t.Fatal("the canceled read discarded what it had collected")
	}
}

// errReader hands back a message and an error in the same read, the way an
// adapter does when it reaches a gap partway through a batch.
type errReader struct {
	msgs []Message
	err  error
}

func (r *errReader) Read(context.Context, Cursor) ([]Message, Cursor, error) {
	return r.msgs, Cursor("1"), r.err
}

func TestPollWaitKeepsMessagesReturnedWithAnError(t *testing.T) {
	r := &errReader{
		msgs: []Message{{ID: "0", Author: "codex", Body: "read before the failure"}},
		err:  errors.New("transport failed"),
	}
	msgs, cursor, err := PollWait(context.Background(), r, "", time.Second, 0, time.Millisecond)
	if !errors.Is(err, r.err) {
		t.Fatalf("err %v, want the reader's error", err)
	}
	if !equal(bodies(msgs), []string{"read before the failure"}) {
		t.Fatalf("batch %v, want the message the failing read returned", bodies(msgs))
	}
	if cursor != "1" {
		t.Fatalf("cursor %q, want the position the reader reported", cursor)
	}
}

// laterFailReader succeeds once, reporting a position, then fails without one.
type laterFailReader struct {
	calls int
	err   error
}

func (r *laterFailReader) Read(ctx context.Context, cursor Cursor) ([]Message, Cursor, error) {
	r.calls++
	if r.calls == 1 {
		return []Message{{ID: "0", Author: "codex", Body: "first poll"}}, "1", nil
	}
	return nil, "", r.err
}

func TestPollWaitKeepsTheLastGoodCursorWhenALaterPollFails(t *testing.T) {
	r := &laterFailReader{err: errors.New("transport failed")}
	msgs, cursor, err := PollWait(context.Background(), r, "", time.Second, 50*time.Millisecond, time.Millisecond)
	if !errors.Is(err, r.err) {
		t.Fatalf("err %v, want the reader's error", err)
	}
	if !equal(bodies(msgs), []string{"first poll"}) {
		t.Fatalf("batch %v, want the message from the successful poll", bodies(msgs))
	}
	if cursor != "1" {
		t.Fatalf("cursor %q, want the position the successful poll established", cursor)
	}
}
