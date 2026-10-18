package stream

import (
	"context"
	"strconv"
	"testing"
	"time"
)

// The line tests drive a Deliverer built with WithLine over the in-memory fake
// from deliver_test.go, with the helpers table_test.go defines. A line is a
// table without the seal: the breaker behaves as it does there, and every post
// by another session is delivered at once, before this one has posted.

const (
	lineChair = tableChair
	lineSelf  = "plan"
)

// newLine is a line session's Deliverer and the cursor its front would keep.
func newLine(t *testing.T, f *fake, cap int) *table {
	t.Helper()
	d := NewDeliverer(f, lineSelf, WithLine(lineChair, cap))
	d.gate.created = tableStart
	return &table{t: t, d: d}
}

func TestLineHasNoSealAFreshSessionGetsPeerPostsBeforePosting(t *testing.T) {
	f := &fake{nick: lineSelf}
	tb := newLine(t, f, 0)

	f.post("build", "phase one done")
	f.post(lineChair, "chair one")
	f.post("review", "a question")
	tb.want("phase one done", "chair one", "a question")

	// Not once, but as they arrive, and still without a post of its own.
	f.post("build", "phase two done")
	tb.want("phase two done")
}

func TestLineHistoryOlderThanTheSessionIsDeliveredToo(t *testing.T) {
	// No seal means no own post to wait for, whatever the history holds: a post
	// of this session's own from a previous run is its own and never delivered.
	f := &fake{nick: lineSelf}
	f.postBefore(lineSelf, "last run's post")
	f.postBefore("build", "older peer post")
	tb := newLine(t, f, 0)

	tb.want("older peer post")
}

func TestLineBreakerTripsPastTheCapAndTheChairReleasesAheadOfItself(t *testing.T) {
	f := &fake{nick: lineSelf}
	tb := newLine(t, f, 2)

	f.post("build", "one")   // count 1
	f.post("review", "two")  // count 2: at the cap, delivered
	f.post("build", "three") // count 3: the message that trips it, held
	f.post("review", "four") // held behind it
	tb.want("one", "two")
	tb.want()

	// The chair's message releases the queue ahead of itself and resets the
	// count to zero: the two released messages do not count toward it.
	f.post(lineChair, "chair one")
	tb.want("three", "four", "chair one")

	f.post("build", "five") // count 1
	f.post("build", "six")  // count 2: still at the cap
	tb.want("five", "six")
	f.post("review", "seven") // count 3: trips again
	tb.want()
	f.post(lineChair, "chair two")
	tb.want("seven", "chair two")
}

func TestLineBreakerCountsOwnPosts(t *testing.T) {
	f := &fake{nick: lineSelf}
	tb := newLine(t, f, 2)

	f.post(lineSelf, "mine one") // count 1
	f.post(lineSelf, "mine two") // count 2
	f.post("build", "one")       // count 3: trips
	tb.want()

	f.post(lineChair, "chair one")
	tb.want("one", "chair one")
}

func TestLineChairPostsAlwaysPass(t *testing.T) {
	f := &fake{nick: lineSelf}
	tb := newLine(t, f, 1)

	f.post("build", "one")
	f.post("build", "two") // trips
	tb.want("one")

	// Tripped or not, a chair line is delivered, and each one resets the count.
	f.post(lineChair, "chair one")
	tb.want("two", "chair one")
	f.post(lineChair, "chair two")
	tb.want("chair two")
	f.post("build", "three")
	tb.want("three")
}

func TestLineCapZeroMeansNoBreaker(t *testing.T) {
	f := &fake{nick: lineSelf}
	tb := newLine(t, f, 0)

	var want []string
	for i := range 50 {
		body := "peer " + strconv.Itoa(i)
		f.post("build", body)
		f.post(lineSelf, "mine "+strconv.Itoa(i))
		want = append(want, body)
	}
	tb.want(want...)

	// A negative cap is no breaker either, as it is at a table.
	f2 := &fake{nick: lineSelf}
	tb = newLine(t, f2, -1)
	for i := range 20 {
		f2.post("build", "peer "+strconv.Itoa(i))
	}
	if got := tb.read(); len(got) != 20 {
		t.Fatalf("delivered %d posts under a negative cap, want 20", len(got))
	}
}

// TestLineAGapDiscardsTheHeldQueue is the table's gap rule: transport IDs may
// restart across a gap, so a held message goes with the history it belonged to.
// If it were released it would come out ahead of the chair's message, and the
// chair's, with the same ID, would be dropped as its repeat.
func TestLineAGapDiscardsTheHeldQueue(t *testing.T) {
	f := &fake{nick: lineSelf}
	tb := newLine(t, f, 1)

	f.post("build", "old one") // ID 0, count 1: delivered
	f.post("build", "old two") // ID 1, count 2: trips, held
	tb.want("old one")

	f.mu.Lock()
	f.msgs = nil
	f.gapPending = true
	f.mu.Unlock()
	f.post(lineChair, "new chair") // ID 0 again
	tb.want("new chair")

	// Nothing from before the gap comes back later, and the IDs the new history
	// reuses are delivered as the new messages they are.
	f.post("build", "new one") // ID 1 again: the held message's ID
	tb.want("new one")
	f.post("build", "new two") // count 2 after the chair: trips
	tb.want()
	f.post(lineChair, "chair two")
	tb.want("new two", "chair two")
}

// TestLineAGapKeepsTheCount: the gap says history was lost, not that the breaker
// was reset; only a chair message resets it.
func TestLineAGapKeepsTheCount(t *testing.T) {
	f := &fake{nick: lineSelf}
	tb := newLine(t, f, 1)

	f.post("build", "one") // count 1
	tb.want("one")
	f.mu.Lock()
	f.msgs = nil
	f.gapPending = true
	f.mu.Unlock()
	f.post("build", "two") // count 2 across the gap: trips, so it is held
	tb.want()
	f.post(lineChair, "chair one")
	tb.want("two", "chair one")
}

func TestLineHeldAndDeliveredPostsAreNotCountedTwice(t *testing.T) {
	for name, f := range map[string]*fake{
		"repeats within a read": {nick: lineSelf, twice: true},
		"repeats across reads":  {nick: lineSelf, atLeastOnce: true},
	} {
		t.Run(name, func(t *testing.T) {
			tb := newLine(t, f, 3)

			// Each post is handed over more than once and counts once: these make 3,
			// the cap. Counted per hand-over, the breaker would have tripped.
			f.post("build", "one")
			f.post(lineSelf, "mine")
			f.post("review", "two")
			tb.want("one", "two")
			tb.want()

			f.post("build", "three") // count 4: trips
			tb.want()
			f.post(lineChair, "chair one")
			tb.want("three", "chair one")
			tb.want()
		})
	}
}

// TestLineAnUncommittedDeliveryLeavesTheGateAsItWas: a delivery that never
// reaches its destination is collected again from the previous cursor and must
// come out the same, with the count where it was.
func TestLineAnUncommittedDeliveryLeavesTheGateAsItWas(t *testing.T) {
	f := &fake{nick: lineSelf}
	tb := newLine(t, f, 2)

	f.post("build", "one")
	f.post("build", "two") // count 2: at the cap
	for range 3 {
		del, err := tb.d.Read(context.Background(), tb.cursor)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if got := bodies(del.Messages); !equal(got, []string{"one", "two"}) {
			t.Fatalf("delivered %q, want one and two", got)
		}
	}
	tb.want("one", "two")

	// The count stands at 2, so the next post is the one that trips it.
	f.post("review", "three")
	tb.want()
	f.post(lineChair, "chair one")
	tb.want("three", "chair one")
}

func TestLineWaitReturnsOnceTheChairReleasesWhatWasHeld(t *testing.T) {
	f := &fake{nick: lineSelf}
	f.post("build", "one")
	f.post("build", "two") // cap 1: trips, held
	tb := newLine(t, f, 1)

	got, err := tb.d.Wait(context.Background(), "", 100*time.Millisecond, 0)
	if err != nil || !equal(bodies(got.Messages), []string{"one"}) {
		t.Fatalf("Wait = %q, %v; want the first post alone", bodies(got.Messages), err)
	}
	tb.cursor = got.Cursor
	tb.d.Commit(got)

	got, err = tb.d.Wait(context.Background(), tb.cursor, 100*time.Millisecond, 0)
	if err != nil || !got.Empty() {
		t.Fatalf("Wait = %q, %v; want nothing while tripped", bodies(got.Messages), err)
	}
	tb.cursor = got.Cursor

	go func() {
		time.Sleep(50 * time.Millisecond)
		f.post(lineChair, "chair one")
	}()
	got, err = tb.d.Wait(context.Background(), tb.cursor, 5*time.Second, 0)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !equal(bodies(got.Messages), []string{"two", "chair one"}) {
		t.Fatalf("delivered %q, want the held post ahead of the chair message", bodies(got.Messages))
	}
}

func TestLineChairMatchesWithoutRegardToASCIICase(t *testing.T) {
	f := &fake{nick: lineSelf}
	tb := newLine(t, f, 1)

	f.post("Build", "one")
	f.post("BUILD", "two") // count 2 against a cap of 1: held
	tb.want("one")
	f.post("Colin", "chair one")
	tb.want("two", "chair one")
	f.post("PLAN", "my post under another case") // own: counts, never delivered
	f.post("build", "three")                     // count 2: held
	tb.want()
}

func TestNoLineDeliversEveryPeerPost(t *testing.T) {
	// An empty chair is no line, and no line is today's behaviour: every post by
	// another nick is delivered at once, whatever the cap says.
	f := &fake{nick: lineSelf}
	for range 5 {
		f.post("build", "one")
	}
	f.post(lineSelf, "mine")
	d := NewDeliverer(f, lineSelf, WithLine("", 1))
	if d.gate != nil {
		t.Fatal("an empty chair built a gate")
	}
	got, err := d.Read(context.Background(), "")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got.Messages) != 5 {
		t.Fatalf("delivered %q, want every peer post", bodies(got.Messages))
	}
}

// TestLineAndTableAgreeOnceTheTableSealIsOpen: the line's breaker is the table's,
// so the same stream read by a table seat whose seal is open and by a line
// session delivers the same posts in the same order.
func TestLineAndTableAgreeOnceTheTableSealIsOpen(t *testing.T) {
	post := func(f *fake) {
		f.post(tableSelf, "answer") // opens the table's seal; counts for both
		for i := range 8 {
			f.post("build", "peer "+strconv.Itoa(i))
			if i == 4 {
				f.post(tableChair, "chair")
			}
		}
		f.post(tableChair, "chair two")
		f.post("build", "after")
	}
	ft, fl := &fake{nick: tableSelf}, &fake{nick: tableSelf}
	post(ft)
	post(fl)
	seat := newTable(t, ft, 3)
	d := NewDeliverer(fl, tableSelf, WithLine(tableChair, 3))
	d.gate.created = tableStart
	line := &table{t: t, d: d}
	if a, b := seat.read(), line.read(); !equal(a, b) {
		t.Fatalf("the table seat delivered %q, the line session %q", a, b)
	}
}
