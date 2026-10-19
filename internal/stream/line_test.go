package stream

import (
	"context"
	"strconv"
	"testing"
	"time"
)

// The line tests drive a Deliverer built with WithLine over the in-memory fake
// from deliver_test.go, with the helpers table_test.go defines. A line has no
// seal, so every post by another session is delivered at once, before this one
// has posted, and its breaker is a rate: more than cap non-chair posts within
// the window. Time is pinned through the messages' own TS. f.post stamps every
// post with one instant, so the tests that use it alone see the cap count posts
// since the chair spoke; the tests that spread posts in time say so, with
// postOffset, and a post with no TS is timed by the clock the test injects.

const (
	lineChair  = tableChair
	lineSelf   = "plan"
	lineWindow = 10 * time.Minute
)

// newLine is a line session's Deliverer and the cursor its front would keep.
func newLine(t *testing.T, f *fake, cap int) *table {
	t.Helper()
	return newLineWindow(t, f, cap, lineWindow)
}

func newLineWindow(t *testing.T, f *fake, cap int, window time.Duration) *table {
	t.Helper()
	d := NewDeliverer(f, lineSelf, WithLine(lineChair, cap, window))
	d.gate.created = tableStart
	// The clock a post with no time of its own is timed by: the instant f.post
	// stamps, so nothing here depends on the wall clock.
	d.gate.now = func() time.Time { return tableStart.Add(time.Second) }
	return &table{t: t, d: d}
}

// postOffset appends a message stamped d after tableStart.
func (f *fake) postOffset(author, body string, d time.Duration) {
	f.postAt(author, body, tableStart.Add(d))
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

// TestLineAGapKeepsTheHistory: the gap says history was lost, not that the
// breaker was reset; only a chair message resets it.
func TestLineAGapKeepsTheHistoryOfTimes(t *testing.T) {
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

// TestLineAGapKeepsTheHistoryOfTimesWhenNothingIsTripped: the times before the
// gap still count toward the posts after it, spread in time or not.
func TestLineAGapKeepsTheHistoryOfTimesWhenNothingIsTripped(t *testing.T) {
	f := &fake{nick: lineSelf}
	tb := newLine(t, f, 2)

	f.postOffset("build", "one", 0)
	f.postOffset("review", "two", time.Minute)
	tb.want("one", "two")
	f.mu.Lock()
	f.msgs = nil
	f.gapPending = true
	f.mu.Unlock()
	// Three posts within the window, across the gap.
	f.postOffset("build", "three", 2*time.Minute)
	tb.want()
	f.postOffset(lineChair, "chair one", 3*time.Minute)
	tb.want("three", "chair one")
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
	d := NewDeliverer(f, lineSelf, WithLine("", 1, lineWindow))
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

// TestLineAndTableAgreeOnceTheTableSealIsOpenAndPostsShareAnInstant: posts that
// carry one instant are all within any window, so the line's rate counts them as
// the table's breaker does, and the same stream read by a table seat whose seal
// is open and by a line session delivers the same posts in the same order.
func TestLineAndTableAgreeOnceTheTableSealIsOpenAndPostsShareAnInstant(t *testing.T) {
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
	d := NewDeliverer(fl, tableSelf, WithLine(tableChair, 3, lineWindow))
	d.gate.created = tableStart
	line := &table{t: t, d: d}
	if a, b := seat.read(), line.read(); !equal(a, b) {
		t.Fatalf("the table seat delivered %q, the line session %q", a, b)
	}
}

// TestLinePostsSpreadBeyondTheWindowNeverTripHoweverMany: a line runs for days,
// and a count of posts since the human spoke would trip on ordinary pokes. A
// rate does not: with every post farther from the last than the window can hold
// cap of them, none is held, however many there are.
func TestLinePostsSpreadBeyondTheWindowNeverTripHoweverMany(t *testing.T) {
	f := &fake{nick: lineSelf}
	tb := newLine(t, f, 2)

	var want []string
	for i := range 60 {
		body := "poke " + strconv.Itoa(i)
		// Each pair is 11 minutes apart, so no three of them fit in ten.
		f.postOffset([]string{"build", "review"}[i%2], body, time.Duration(i)*11*time.Minute)
		want = append(want, body)
	}
	tb.want(want...)
}

// TestLineTripsOnCapPlusOnePostsWithinTheWindowAndHoldsTheRestUntilTheChair: the
// post that makes more than cap within the window is held with every later one,
// whatever time passes: nothing releases a tripped breaker but the chair.
func TestLineTripsOnCapPlusOnePostsWithinTheWindowAndHoldsTheRestUntilTheChair(t *testing.T) {
	f := &fake{nick: lineSelf}
	tb := newLine(t, f, 2)

	f.postOffset("build", "one", 0)
	f.postOffset("review", "two", 3*time.Minute)
	f.postOffset("build", "three", 6*time.Minute) // the third within ten minutes: trips, held
	tb.want("one", "two")

	// An hour later, and a day: still held, and so is a post that on its own
	// would have been nowhere near a window of the others.
	f.postOffset("review", "four", time.Hour)
	f.postOffset("build", "five", 30*time.Hour)
	tb.want()
	tb.want()

	f.postOffset(lineChair, "chair one", 31*time.Hour)
	tb.want("three", "four", "five", "chair one")
}

// TestLineTheWindowSlides: it is the cap+1 most recent posts that are measured,
// not the posts since some fixed start.
func TestLineTheWindowSlides(t *testing.T) {
	f := &fake{nick: lineSelf}
	tb := newLine(t, f, 2)

	f.postOffset("build", "one", 0)
	f.postOffset("review", "two", 6*time.Minute)
	f.postOffset("build", "three", 12*time.Minute) // one..three span 12 minutes: no trip
	tb.want("one", "two", "three")
	f.postOffset("review", "four", 14*time.Minute) // two..four span 8 minutes: trips
	tb.want()
	f.postOffset(lineChair, "chair one", 15*time.Minute)
	tb.want("four", "chair one")
}

// TestLineTheWindowIncludesItsEdge: cap+1 posts that span exactly the window trip
// it, and one nanosecond more does not.
func TestLineTheWindowIncludesItsEdge(t *testing.T) {
	for name, tc := range map[string]struct {
		span time.Duration
		want []string
	}{
		"exactly the window":   {lineWindow, []string{"one"}},
		"just over the window": {lineWindow + time.Nanosecond, []string{"one", "two"}},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fake{nick: lineSelf}
			tb := newLine(t, f, 1)
			f.postOffset("build", "one", 0)
			f.postOffset("review", "two", tc.span)
			tb.want(tc.want...)
		})
	}
}

// TestLineTheChairClearsTheHistoryAndReleasedPostsDoNotCount: after the chair
// speaks the count of the window starts empty, and the posts it released, which
// were within the window of the next ones, are not in it.
func TestLineTheChairClearsTheHistoryAndReleasedPostsDoNotCount(t *testing.T) {
	f := &fake{nick: lineSelf}
	tb := newLine(t, f, 2)

	f.postOffset("build", "one", 0)
	f.postOffset("review", "two", time.Minute)
	f.postOffset("build", "three", 2*time.Minute) // trips, held
	f.postOffset("review", "four", 2*time.Minute+time.Second)
	tb.want("one", "two")

	f.postOffset(lineChair, "chair one", 3*time.Minute)
	tb.want("three", "four", "chair one")

	// Cap posts right after the chair's: delivered, though three more of the
	// released ones were within the window of them.
	f.postOffset("build", "five", 4*time.Minute)
	f.postOffset("review", "six", 5*time.Minute)
	tb.want("five", "six")
	f.postOffset("build", "seven", 6*time.Minute) // the third since the chair: trips
	tb.want()
	f.postOffset(lineChair, "chair two", 7*time.Minute)
	tb.want("seven", "chair two")
}

func TestLineCapZeroNeverTripsHoweverFastTheyComeInTime(t *testing.T) {
	f := &fake{nick: lineSelf}
	tb := newLine(t, f, 0)

	var want []string
	for i := range 100 {
		body := "peer " + strconv.Itoa(i)
		f.postOffset("build", body, time.Duration(i)*time.Millisecond)
		want = append(want, body)
	}
	tb.want(want...)
}

// TestLineAPostWithNoTimeIsTimedByTheClock: a transport that reports no TS has
// its messages timed when the Deliverer receives them.
func TestLineAPostWithNoTimeIsTimedByTheClock(t *testing.T) {
	post := func(f *fake) {
		for i := range 3 {
			f.postAt("build", "poke "+strconv.Itoa(i), time.Time{})
		}
	}
	// The clock moves a minute a message: three within the window, and trips.
	f := &fake{nick: lineSelf}
	tb := newLine(t, f, 2)
	now := tableStart
	tb.d.gate.now = func() time.Time { now = now.Add(time.Minute); return now }
	post(f)
	tb.want("poke 0", "poke 1")

	// An hour a message: never within the window of each other.
	f = &fake{nick: lineSelf}
	tb = newLine(t, f, 2)
	now = tableStart
	tb.d.gate.now = func() time.Time { now = now.Add(time.Hour); return now }
	post(f)
	tb.want("poke 0", "poke 1", "poke 2")

	// A post with a time of its own is not timed by the clock, and one without is
	// timed against it in the same window.
	f = &fake{nick: lineSelf}
	tb = newLine(t, f, 1)
	tb.d.gate.now = func() time.Time { return tableStart.Add(time.Minute) }
	f.postOffset("build", "stamped", 0)
	f.postAt("review", "unstamped", time.Time{})
	tb.want("stamped")
}

// TestLineABacklogSpreadOverDaysDoesNotTrip: a session that joins a line under
// way reads the history the server retains as its first delivery, and posts days
// apart are not a loop.
func TestLineABacklogSpreadOverDaysDoesNotTrip(t *testing.T) {
	f := &fake{nick: lineSelf}
	var want []string
	for i := range 200 {
		body := "poke " + strconv.Itoa(i)
		f.postOffset([]string{"build", "review"}[i%2], body, -time.Duration(200-i)*time.Hour)
		want = append(want, body)
	}
	tb := newLine(t, f, 3)
	tb.want(want...)
}

// TestLineABurstInTheBacklogTripsAtTheBurst: the first delivery is gated like any
// other, so a burst in the history holds what follows it.
func TestLineABurstInTheBacklogTripsAtTheBurst(t *testing.T) {
	f := &fake{nick: lineSelf}
	f.postOffset("build", "days ago", -72*time.Hour)
	f.postOffset("build", "burst 1", -time.Hour)
	f.postOffset("review", "burst 2", -time.Hour+time.Second)
	f.postOffset("build", "burst 3", -time.Hour+2*time.Second)
	f.postOffset("review", "later", -time.Minute)
	tb := newLine(t, f, 2)
	// The three burst posts are the third within the window, and it is held, and
	// so is the one after it.
	tb.want("days ago", "burst 1", "burst 2")
	f.postOffset(lineChair, "chair one", time.Minute)
	tb.want("burst 3", "later", "chair one")
}

// TestLineOwnPostsAreTimedAndCounted: the session's own posts are in the rate,
// by the TS the transport gave them, and one from a previous run counts only
// where it falls.
func TestLineOwnPostsAreTimedAndCounted(t *testing.T) {
	f := &fake{nick: lineSelf}
	tb := newLine(t, f, 1)
	f.postOffset(lineSelf, "mine", 0)
	f.postOffset("build", "one", time.Minute) // two within the window: trips
	tb.want()
	f.postOffset(lineChair, "chair one", 2*time.Minute)
	tb.want("one", "chair one")

	f = &fake{nick: lineSelf}
	f.postBefore(lineSelf, "last run's post") // an hour before: out of the window
	tb = newLine(t, f, 1)
	f.postOffset("build", "one", time.Minute)
	tb.want("one")
}

// TestLineTimesOutOfOrderCannotHideABurst: the span of the posts measured is the
// latest time less the earliest, not the last less the first.
func TestLineTimesOutOfOrderCannotHideABurst(t *testing.T) {
	f := &fake{nick: lineSelf}
	tb := newLine(t, f, 1)
	f.postOffset("build", "one", 30*time.Minute)
	f.postOffset("review", "two", 25*time.Minute) // five minutes apart, the wrong way round
	tb.want("one")
}

// TestLineRepeatedHandOversAreNotTimedTwice: the same post handed over twice is
// one post in the window.
func TestLineRepeatedHandOversAreNotTimedTwice(t *testing.T) {
	for name, f := range map[string]*fake{
		"repeats within a read": {nick: lineSelf, twice: true},
		"repeats across reads":  {nick: lineSelf, atLeastOnce: true},
	} {
		t.Run(name, func(t *testing.T) {
			tb := newLine(t, f, 2)
			f.postOffset("build", "one", 0)
			f.postOffset("review", "two", time.Minute)
			tb.want("one", "two")
			tb.want()
			f.postOffset("build", "three", 2*time.Minute) // the third: trips
			tb.want()
			f.postOffset(lineChair, "chair one", 3*time.Minute)
			tb.want("three", "chair one")
		})
	}
}
