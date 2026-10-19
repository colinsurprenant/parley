package stream

import (
	"context"
	"math"
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

// TestLinePostsSpreadBeyondTheWindowAreNotTrippedByTheRate: a line runs for days,
// and a count of posts since the human spoke alone would trip on ordinary
// pokes. The rate does not: with every post farther from the last than the
// window can hold cap of them, none is held for that, up to the five times cap
// the backstop allows since the human last posted.
func TestLinePostsSpreadBeyondTheWindowAreNotTrippedByTheRate(t *testing.T) {
	f := &fake{nick: lineSelf}
	tb := newLine(t, f, 20)

	var want []string
	for i := range 100 { // five times the cap: at the backstop, not past it
		body := "poke " + strconv.Itoa(i)
		// Each pair is 11 minutes apart, so no 21 of them fit in ten.
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
// way reads the history the server retains as its first delivery, and posts
// hours apart are not a loop: none trips the rate, and the count backstop, five
// times the cap since the chair last posted, is what a long enough backlog
// without the human reaches.
func TestLineABacklogSpreadOverDaysDoesNotTrip(t *testing.T) {
	f := &fake{nick: lineSelf}
	var want []string
	for i := range 40 { // five times the cap of 8: at the backstop, not past it
		body := "poke " + strconv.Itoa(i)
		f.postOffset([]string{"build", "review"}[i%2], body, -time.Duration(40-i)*5*time.Hour)
		want = append(want, body)
	}
	tb := newLine(t, f, 8)
	tb.want(want...)

	// A backlog of the same kind with a post of the chair's in it counts from there.
	f = &fake{nick: lineSelf}
	want = nil
	for i := range 70 {
		body := "poke " + strconv.Itoa(i)
		if i == 30 {
			f.postOffset(lineChair, "chair", -time.Duration(70-i)*5*time.Hour)
			want = append(want, "chair")
		}
		f.postOffset([]string{"build", "review"}[i%2], body, -time.Duration(70-i)*5*time.Hour)
		want = append(want, body)
	}
	tb = newLine(t, f, 8)
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

// TestLineTimesOutOfOrderCannotHideABurst: the span of the cap+1 posts measured
// is the latest time less the earliest of them, wherever they fall in arrival
// order, and not the last less the first, which is negative or short when they
// arrive out of order. The times here are one clock's, reported out of order.
func TestLineTimesOutOfOrderCannotHideABurst(t *testing.T) {
	for name, tc := range map[string]struct {
		cap   int
		times []time.Duration
		want  []string // delivered, in order; the rest held
	}{
		"two posts five minutes apart, the wrong way round": {1, []time.Duration{30 * time.Minute, 25 * time.Minute}, []string{"p0"}},
		// The earliest of the three is not the first to arrive: 0 to 9 minutes is a
		// burst, though the last less the first is one minute.
		"the earliest is not the first": {2, []time.Duration{8 * time.Minute, 0, 9 * time.Minute}, []string{"p0", "p1"}},
		// The earliest is not the first and the latest is not the last: 5 to 30
		// minutes is no burst, though the last less the first is negative.
		"the earliest is not the first and the span is wide": {2, []time.Duration{30 * time.Minute, 5 * time.Minute, 25 * time.Minute}, []string{"p0", "p1", "p2"}},
		// The latest is not the last: 0 to 25 minutes is no burst, though the last
		// less the first is nine minutes.
		"the latest is not the last": {2, []time.Duration{0, 25 * time.Minute, 9 * time.Minute}, []string{"p0", "p1", "p2"}},
		// An outlier between the posts of a burst: no three posts in a row by
		// arrival fit the window, but 0, 1 and 2 minutes do, so the fourth trips.
		"a burst interleaved with an outlier": {2, []time.Duration{0, 30 * time.Minute, time.Minute, 2 * time.Minute}, []string{"p0", "p1", "p2"}},
		// The burst arrives after a later post and ends before it in time.
		"a burst that arrives after a later post": {2, []time.Duration{40 * time.Minute, 0, 30 * time.Minute, time.Minute, 2 * time.Minute}, []string{"p0", "p1", "p2", "p3"}},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fake{nick: lineSelf}
			tb := newLine(t, f, tc.cap)
			for i, off := range tc.times {
				f.postOffset([]string{"build", "review"}[i%2], "p"+strconv.Itoa(i), off)
			}
			tb.want(tc.want...)
		})
	}
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

// slowPost makes post i of a loop that runs at one post every two minutes,
// alternating between two sessions: never nine within ten minutes, so the rate
// of a cap of 8 over ten minutes never sees it.
func slowPost(f *fake, i int) string {
	body := "slow " + strconv.Itoa(i+1)
	f.postOffset([]string{"build", "review"}[i%2], body, time.Duration(i)*2*time.Minute)
	return body
}

// TestLineASlowLoopTripsTheBackstopAndTheChairReleasesIt: two sessions that
// answer each other every two minutes never make a burst for a cap of 8 over ten
// minutes, so the count since the human last posted is what stops them, at more
// than five times the cap: the 41st post. Nothing releases it but the chair, who
// releases what was held in order, ahead of itself.
func TestLineASlowLoopTripsTheBackstopAndTheChairReleasesIt(t *testing.T) {
	f := &fake{nick: lineSelf}
	tb := newLine(t, f, 8)

	var delivered, held []string
	for i := range 45 {
		body := slowPost(f, i)
		if i < 40 {
			delivered = append(delivered, body)
		} else {
			held = append(held, body)
		}
	}
	tb.want(delivered...)
	tb.want()

	// Hours later, still held.
	slowPost(f, 720)
	held = append(held, "slow 721")
	tb.want()

	f.postOffset(lineChair, "chair one", 25*time.Hour)
	tb.want(append(held, "chair one")...)
}

// TestLineTheBackstopCountsOwnPosts: the session's own posts are in the count
// since the human last posted, as they are in the rate.
func TestLineTheBackstopCountsOwnPosts(t *testing.T) {
	f := &fake{nick: lineSelf}
	tb := newLine(t, f, 2) // the backstop is 10

	var want []string
	for i := range 10 {
		author := []string{"build", lineSelf}[i%2]
		body := "slow " + strconv.Itoa(i+1)
		f.postOffset(author, body, time.Duration(i)*20*time.Minute)
		if author != lineSelf {
			want = append(want, body)
		}
	}
	tb.want(want...)
	f.postOffset("review", "the eleventh", 10*20*time.Minute) // the 11th: trips
	tb.want()
	f.postOffset(lineChair, "chair one", 11*20*time.Minute)
	tb.want("the eleventh", "chair one")
}

// TestLineTheRateStillTripsAFastBurstFirst: the backstop is under the rate, not
// instead of it, and a burst is held at the ninth post, long before the 41st.
func TestLineTheRateStillTripsAFastBurstFirst(t *testing.T) {
	f := &fake{nick: lineSelf}
	tb := newLine(t, f, 8)

	var want []string
	for i := range 8 {
		body := "fast " + strconv.Itoa(i+1)
		f.postOffset([]string{"build", "review"}[i%2], body, time.Duration(i)*10*time.Second)
		want = append(want, body)
	}
	f.postOffset("build", "fast 9", 80*time.Second) // the ninth within ten minutes: trips
	tb.want(want...)
	f.postOffset(lineChair, "chair one", 5*time.Minute)
	tb.want("fast 9", "chair one")
}

// TestLineTheChairResetsTheRateAndTheBackstop: after the chair speaks both start
// from nothing, so a slow loop gets a full 40 more, and a burst a full cap more,
// and the posts it released are in neither.
func TestLineTheChairResetsTheRateAndTheBackstop(t *testing.T) {
	f := &fake{nick: lineSelf}
	tb := newLine(t, f, 8)

	for i := range 30 {
		slowPost(f, i)
	}
	tb.read()
	f.postOffset(lineChair, "chair one", 61*time.Minute)
	tb.want("chair one")

	// Forty more, from the chair on: the 30 before it are not in the count.
	var want []string
	for i := range 40 {
		body := "after " + strconv.Itoa(i+1)
		f.postOffset([]string{"build", "review"}[i%2], body, 2*time.Hour+time.Duration(i)*2*time.Minute)
		want = append(want, body)
	}
	tb.want(want...)
	f.postOffset("build", "after 41", 2*time.Hour+80*2*time.Minute) // the 41st since the chair: trips
	tb.want()
	f.postOffset(lineChair, "chair two", 5*time.Hour)
	tb.want("after 41", "chair two")

	// And a burst after a backstop trip needs its own cap+1 again: the times that
	// the trip recorded are gone with the chair's post.
	for i := range 8 {
		f.postOffset("review", "burst "+strconv.Itoa(i+1), 6*time.Hour+time.Duration(i)*time.Second)
	}
	if got := tb.read(); len(got) != 8 {
		t.Fatalf("delivered %d posts of a burst of the cap after the chair, want all 8", len(got))
	}
	f.postOffset("review", "burst 9", 6*time.Hour+9*time.Second)
	tb.want()
}

// TestLineCapZeroHoldsNothingHoweverManyAndHoweverSlow: cap 0 turns both rules
// off.
func TestLineCapZeroHoldsNothingHoweverManyAndHoweverSlow(t *testing.T) {
	f := &fake{nick: lineSelf}
	tb := newLine(t, f, 0)

	var want []string
	for i := range 500 {
		want = append(want, slowPost(f, i))
	}
	tb.want(want...)
}

// TestLineAGapKeepsTheBackstopCount: the count since the chair last posted
// survives a gap as the history of times does, and only the chair resets it.
func TestLineAGapKeepsTheBackstopCount(t *testing.T) {
	f := &fake{nick: lineSelf}
	tb := newLine(t, f, 1) // the backstop is 5

	for i := range 5 { // an hour apart: no two within the window of ten minutes
		f.postOffset([]string{"build", "review"}[i%2], "slow "+strconv.Itoa(i+1), time.Duration(i)*time.Hour)
	}
	if got := tb.read(); len(got) != 5 {
		t.Fatalf("delivered %d of 5 slow posts, want all", len(got))
	}
	f.mu.Lock()
	f.msgs = nil
	f.gapPending = true
	f.mu.Unlock()
	f.postOffset("build", "after the gap", 10*time.Hour) // the 6th since the chair
	tb.want()
	f.postOffset(lineChair, "chair one", 3*time.Hour)
	tb.want("after the gap", "chair one")
}

// TestLineACapNearTheLargestIntNeverTripsOnAHandfulOfPosts: cap+1 and five times
// cap both overflow an int there, and either would make the first post trip.
func TestLineACapNearTheLargestIntNeverTripsOnAHandfulOfPosts(t *testing.T) {
	for _, capN := range []int{math.MaxInt, math.MaxInt - 1, math.MaxInt / 5, math.MaxInt/5 + 1, math.MaxInt / 2} {
		f := &fake{nick: lineSelf}
		tb := newLine(t, f, capN)
		var want []string
		for i := range 12 {
			body := "poke " + strconv.Itoa(i)
			f.postOffset([]string{"build", "review", lineSelf}[i%3], body, time.Duration(i)*time.Second)
			if i%3 != 2 {
				want = append(want, body)
			}
		}
		if got := tb.read(); !equal(got, want) {
			t.Fatalf("cap %d: delivered %q, want %q", capN, got, want)
		}
		f.postOffset(lineChair, "chair one", time.Minute)
		tb.want("chair one")
	}
}

// TestLineANegativeCapIsNoBreakerThroughTheAPI: the CLI refuses a negative cap,
// but WithLine's contract is that a cap not above zero turns both rules off, and
// five times a large negative cap must not wrap into a positive backstop.
func TestLineANegativeCapIsNoBreakerThroughTheAPI(t *testing.T) {
	for _, capN := range []int{-1, -2*(math.MaxInt/5) - 1, math.MinInt} {
		f := &fake{nick: lineSelf}
		tb := newLine(t, f, capN)
		var want []string
		for i := range 6 {
			body := "poke " + strconv.Itoa(i)
			f.postOffset([]string{"build", "review"}[i%2], body, time.Duration(i)*time.Second)
			want = append(want, body)
		}
		if got := tb.read(); !equal(got, want) {
			t.Fatalf("cap %d: delivered %q, want %q", capN, got, want)
		}
	}
}
