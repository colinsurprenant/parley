package stream

import (
	"math"
	"strconv"
	"testing"
	"time"
)

// The backstop tests pin the line breaker's second rule: more than five times cap
// non-chair posts within backstopWindows times the window, timed by TS, with the
// chair's post the only thing that releases it. They use a cap of 2 over the
// test window of ten minutes, so the backstop is 11 posts within six hours.

// TestLineBackstopTripsWhenFiveTimesCapPlusOneFitInTheSpan: 11 posts spaced so
// that no 3 of them fit in ten minutes — the rate never sees them — but all 11
// fit in the six hours trip the backstop, at the 11th, and name it as the leg.
func TestLineBackstopTripsWhenFiveTimesCapPlusOneFitInTheSpan(t *testing.T) {
	f := &fake{nick: lineSelf}
	tb := newLine(t, f, 2)

	const gap = 5*time.Minute + 30*time.Second // three span 11 minutes; 11 span 55, well inside six hours
	var want []string
	for i := range 10 {
		body := "p" + strconv.Itoa(i+1)
		f.postOffset([]string{"build", "review"}[i%2], body, time.Duration(i)*gap)
		want = append(want, body)
	}
	tb.want(want...)
	if tb.d.gate.state.tripped {
		t.Fatal("the breaker tripped on 10 posts, which is the backstop and not past it")
	}
	f.postOffset("build", "p11", 10*gap)
	tb.want()
	if st := tb.d.gate.state; !st.tripped || st.leg != legBackstop {
		t.Fatalf("after the 11th post tripped=%v leg=%v, want the backstop (%v)", st.tripped, st.leg, legBackstop)
	}
}

// TestLineBackstopSpanIncludesItsEdgeAndNotAnInstantMore: the 11 posts that span
// exactly 36 windows trip, and one nanosecond more does not. The spacing is
// 36 minutes, so no three posts fit in ten and the rate has no say.
func TestLineBackstopSpanIncludesItsEdgeAndNotAnInstantMore(t *testing.T) {
	for name, tc := range map[string]struct {
		gap     time.Duration
		tripped bool
	}{
		"exactly the span":   {backstopWindows * lineWindow / 10, true},
		"just over the span": {backstopWindows*lineWindow/10 + time.Nanosecond, false},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fake{nick: lineSelf}
			tb := newLine(t, f, 2)
			for i := range 11 {
				f.postOffset([]string{"build", "review"}[i%2], "p"+strconv.Itoa(i+1), time.Duration(i)*tc.gap)
			}
			tb.read()
			st := tb.d.gate.state
			if st.tripped != tc.tripped {
				t.Fatalf("tripped=%v, want %v (11 posts %v apart)", st.tripped, tc.tripped, tc.gap)
			}
			if tc.tripped && st.leg != legBackstop {
				t.Fatalf("leg=%v, want the backstop (%v)", st.leg, legBackstop)
			}
		})
	}
}

// TestLineTheSlowestLoopTheBackstopCatchesAtTheDefaults: at a cap of 20 over ten
// minutes the backstop is more than 100 posts within six hours, so the slowest
// loop it stops is one post every 3.6 minutes — 216 seconds, which puts 101 posts
// exactly six hours apart end to end — and a loop a second slower is not stopped.
// 21 posts at that pace span 72 minutes, so the rate has no say in either.
func TestLineTheSlowestLoopTheBackstopCatchesAtTheDefaults(t *testing.T) {
	for name, tc := range map[string]struct {
		gap     time.Duration
		tripped bool
	}{
		"every 3.6 minutes":         {216 * time.Second, true},
		"a second slower than that": {217 * time.Second, false},
		"every three minutes":       {3 * time.Minute, true},
		"every five minutes":        {5 * time.Minute, false},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fake{nick: lineSelf}
			tb := newLine(t, f, 20)
			for i := range 101 {
				f.postOffset([]string{"build", "review"}[i%2], "p"+strconv.Itoa(i+1), time.Duration(i)*tc.gap)
			}
			tb.read()
			st := tb.d.gate.state
			if st.tripped != tc.tripped {
				t.Fatalf("tripped=%v after 101 posts %v apart, want %v", st.tripped, tc.gap, tc.tripped)
			}
			if tc.tripped && st.leg != legBackstop {
				t.Fatalf("leg=%v, want the backstop", st.leg)
			}
		})
	}
}

// TestLineASlowTrickleNeverTripsAndTheHistoryStaysBounded: one post every 37
// minutes is under both rules however long it goes on with the chair silent, and
// the history of times never holds more than five times cap plus one.
func TestLineASlowTrickleNeverTripsAndTheHistoryStaysBounded(t *testing.T) {
	f := &fake{nick: lineSelf}
	tb := newLine(t, f, 2)

	// A minute more than the span of 11 posts, which is ten gaps: 3 span 74 minutes
	// and 11 span 370, past the 360 of 36 windows.
	const gap = backstopWindows*lineWindow/10 + time.Minute
	var want []string
	for i := range 3000 {
		body := "p" + strconv.Itoa(i)
		f.postOffset([]string{"build", "review"}[i%2], body, time.Duration(i)*gap)
		want = append(want, body)
	}
	tb.want(want...)
	st := tb.d.gate.state
	if st.tripped {
		t.Fatal("a trickle with no chair post tripped the breaker")
	}
	if got, bound := len(st.times), 5*2+1; got != bound {
		t.Fatalf("the history holds %d times, want exactly the bound %d", got, bound)
	}
}

// TestLineTheHistoryNeverExceedsItsBoundWhateverTheOrder: posts stamped out of
// order, one older than everything retained among them, leave the history sorted
// and at its bound, and a post older than all of it is still checked against it.
func TestLineTheHistoryNeverExceedsItsBoundWhateverTheOrder(t *testing.T) {
	f := &fake{nick: lineSelf}
	tb := newLine(t, f, 2)
	// 45 minutes apart: no 3 in ten, and no 11 in 360, even with the middle post
	// below added, which makes 11 of them span nine gaps and a half, 405 minutes.
	const gap = backstopWindows * lineWindow / 8
	const base = 4 * time.Hour
	for i := range 11 {
		f.postOffset("build", "p"+strconv.Itoa(i), base+time.Duration(i)*gap)
	}
	tb.read()
	f.postOffset("review", "an old one", time.Minute)  // older than all 11
	f.postOffset("review", "a middle one", base+gap/2) // between the first two
	tb.read()
	st := tb.d.gate.state
	if st.tripped {
		t.Fatal("posts spread past both spans tripped the breaker")
	}
	if len(st.times) != 11 {
		t.Fatalf("the history holds %d times, want its bound of 11", len(st.times))
	}
	for i := 1; i < len(st.times); i++ {
		if st.times[i].Before(st.times[i-1]) {
			t.Fatalf("the history is not in time order at %d: %v", i, st.times)
		}
	}
	if want := tableStart.Add(base + gap/2); !st.times[0].Equal(want) {
		t.Fatalf("the earliest time kept is %v, want %v: the old post is checked and dropped, then the earliest of what remains", st.times[0], want)
	}
}

// TestLineAChairPostReleasesAndResetsBothLegs: after a trip by either leg the
// chair's post releases what was held and the next trip needs a full run again.
func TestLineAChairPostReleasesAndResetsBothLegs(t *testing.T) {
	f := &fake{nick: lineSelf}
	tb := newLine(t, f, 2)

	const gap = 5*time.Minute + 30*time.Second
	for i := range 11 {
		f.postOffset("build", "slow "+strconv.Itoa(i+1), time.Duration(i)*gap)
	}
	tb.read()
	if st := tb.d.gate.state; !st.tripped || st.leg != legBackstop {
		t.Fatalf("setup: tripped=%v leg=%v, want the backstop", st.tripped, st.leg)
	}
	f.postOffset(lineChair, "chair one", 61*time.Minute)
	tb.want("slow 11", "chair one")
	if st := tb.d.gate.state; st.tripped || st.leg != legNone || len(st.times) != 0 {
		t.Fatalf("the chair left tripped=%v leg=%v with %d times", st.tripped, st.leg, len(st.times))
	}

	// Ten more inside the span of the first run's last posts, delivered: the
	// backstop's history is empty, and the rate's too.
	var want []string
	for i := range 10 {
		body := "again " + strconv.Itoa(i+1)
		f.postOffset("build", body, 62*time.Minute+time.Duration(i)*gap)
		want = append(want, body)
	}
	tb.want(want...)
	f.postOffset("review", "again 11", 62*time.Minute+10*gap)
	tb.want()
	if st := tb.d.gate.state; !st.tripped || st.leg != legBackstop {
		t.Fatalf("after the 11th: tripped=%v leg=%v, want the backstop again", st.tripped, st.leg)
	}
	f.postOffset(lineChair, "chair two", 3*time.Hour)
	tb.want("again 11", "chair two")

	// The rate leg resets too, and is named as itself.
	for i := range 3 {
		f.postOffset("build", "burst "+strconv.Itoa(i+1), 4*time.Hour+time.Duration(i)*time.Second)
	}
	tb.read()
	if st := tb.d.gate.state; !st.tripped || st.leg != legRate {
		t.Fatalf("after a burst: tripped=%v leg=%v, want the rate (%v)", st.tripped, st.leg, legRate)
	}
}

// TestLineBackstopArithmeticSaturates: five times cap and 36 windows are what the
// breaker, the instructions and the notice state, and none of them wraps.
func TestLineBackstopArithmeticSaturates(t *testing.T) {
	for _, tc := range []struct {
		cap    int
		window time.Duration
		posts  int
		span   time.Duration
	}{
		{20, 10 * time.Minute, 100, backstopWindows * 10 * time.Minute},
		{20, 10 * time.Minute, 100, 6 * time.Hour}, // the span the docs state for the defaults
		{1, 10 * time.Minute, 5, backstopWindows * 10 * time.Minute},
		{8, 90 * time.Second, 40, backstopWindows * 90 * time.Second},
		{0, time.Hour, 0, time.Hour},
		{-1, time.Hour, 0, time.Hour},
		{math.MinInt, time.Hour, 0, time.Hour},
		{math.MaxInt / 5, time.Hour, math.MaxInt / 5 * 5, backstopWindows * time.Hour},
		{math.MaxInt/5 + 1, time.Hour, math.MaxInt, backstopWindows * time.Hour},
		{math.MaxInt, time.Hour, math.MaxInt, backstopWindows * time.Hour},
		{20, math.MaxInt64, 100, math.MaxInt64},
		{20, math.MaxInt64 / backstopWindows, 100, backstopWindows * (math.MaxInt64 / backstopWindows)},
		{20, math.MaxInt64/backstopWindows + 1, 100, math.MaxInt64},
		{20, 0, 100, 0},
		{20, -time.Hour, 100, -time.Hour},
		{20, math.MinInt64, 100, math.MinInt64},
	} {
		posts, span := LineBackstop(tc.cap, tc.window)
		if posts != tc.posts || span != tc.span {
			t.Errorf("LineBackstop(%d, %v) = %d, %v; want %d, %v", tc.cap, tc.window, posts, span, tc.posts, tc.span)
		}
	}
}

// TestLineBackstopSpansAre36Windows: the constant the docs and the notice
// restate. A change to it is a change to every text that states the span, which
// take it from LineBackstop, and to the prose that says 36 and six hours — the
// flag help and usage in cmd/parley/main.go and line_new.go, docs/line.md,
// docs/reference.md and the README — which this test is the reminder of.
func TestLineBackstopSpansAre36Windows(t *testing.T) {
	if backstopWindows != 36 || backstopFactor != 5 {
		t.Fatalf("the backstop is %d times cap within %d windows; the prose says 5 and 36: update it with the constants", backstopFactor, backstopWindows)
	}
}

// TestLineBackstopSpanGuardHoldsAtTheEdge: 36 windows of the longest window that
// does not overflow is exact, and one nanosecond more saturates rather than wraps.
func TestLineBackstopSpanGuardHoldsAtTheEdge(t *testing.T) {
	const edge = time.Duration(math.MaxInt64 / 36)
	if _, span := LineBackstop(20, edge); span != 36*edge || span <= 0 {
		t.Fatalf("at the edge the span is %v, want exactly 36 windows", span)
	}
	if _, span := LineBackstop(20, edge+1); span != time.Duration(math.MaxInt64) {
		t.Fatalf("one past the edge the span is %v, want it saturated", span)
	}
}

// TestLineRuleSpellsTheRuleAsTheTextsDo: the one spelling the instructions and
// the notice share.
func TestLineRuleSpellsTheRuleAsTheTextsDo(t *testing.T) {
	for _, tc := range []struct {
		posts int
		span  time.Duration
		want  string
	}{
		{20, 10 * time.Minute, "more than 20 posts within 10m"},
		{100, time.Hour, "more than 100 posts within 1h"},
		{1, 10 * time.Minute, "more than 1 post within 10m"},
		{5, 90 * time.Minute, "more than 5 posts within 1h30m"},
		{40, 9 * time.Minute, "more than 40 posts within 9m"},
		{12, 500 * time.Millisecond, "more than 12 posts within 500ms"},
	} {
		if got := LineRule(tc.posts, tc.span); got != tc.want {
			t.Errorf("LineRule(%d, %v) = %q, want %q", tc.posts, tc.span, got, tc.want)
		}
	}
}
