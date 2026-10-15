package stream

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"
)

// The table tests drive a Deliverer built with WithTable over the in-memory fake
// from deliver_test.go. Time is pinned: tableStart stands in for the Deliverer's
// creation, a post made "now" is stamped after it and a post from "before" is
// stamped before it, so what opens the seal never depends on the wall clock.
var tableStart = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

const (
	tableChair = "colin"
	tableSelf  = "claude"
)

// post appends a message stamped after the Deliverer's creation.
func (f *fake) post(author, body string) {
	f.postAt(author, body, tableStart.Add(time.Second))
}

// postBefore appends a message stamped before the Deliverer's creation: history
// older than this process.
func (f *fake) postBefore(author, body string) {
	f.postAt(author, body, tableStart.Add(-time.Hour))
}

func (f *fake) postAt(author, body string, ts time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.msgs = append(f.msgs, Message{ID: strconv.Itoa(len(f.msgs)), Author: author, TS: ts, Body: body})
}

// table is a seat's Deliverer and the cursor its front would keep.
type table struct {
	t      *testing.T
	d      *Deliverer
	cursor Cursor
}

func newTable(t *testing.T, f *fake, cap int) *table {
	t.Helper()
	d := NewDeliverer(f, tableSelf, WithTable(tableChair, cap))
	d.gate.created = tableStart
	return &table{t: t, d: d}
}

// read hands the front one delivery and commits it, as the pump does, and
// returns the bodies it carried.
func (tb *table) read() []string {
	tb.t.Helper()
	del, err := tb.d.Read(context.Background(), tb.cursor)
	if err != nil {
		tb.t.Fatalf("Read: %v", err)
	}
	tb.cursor = del.Cursor
	tb.d.Commit(del)
	return bodies(del.Messages)
}

func (tb *table) want(want ...string) {
	tb.t.Helper()
	got := tb.read()
	if !equal(got, want) {
		tb.t.Fatalf("delivered %q, want %q", got, want)
	}
}

func TestSealedSeatHoldsSeatPostsAndPassesChairPosts(t *testing.T) {
	f := &fake{nick: tableSelf}
	tb := newTable(t, f, 0)

	f.post("codex", "seat one")
	f.post(tableChair, "chair one")
	f.post("gemini", "seat two")
	tb.want("chair one")

	// Held, not dropped and not repeated: nothing new, nothing delivered.
	tb.want()
	f.post(tableChair, "chair two")
	tb.want("chair two")
}

func TestOwnPostOpensTheSealAndReleasesHeldPostsInOrder(t *testing.T) {
	f := &fake{nick: tableSelf}
	tb := newTable(t, f, 0)

	f.post("codex", "seat one")
	f.post("gemini", "seat two")
	tb.want()

	// The own post opens the seal in the middle of a batch: what was held goes
	// out first, then the seat posts that arrive after the answer, in stream
	// order.
	f.post(tableSelf, "my answer")
	f.post("codex", "seat three")
	tb.want("seat one", "seat two", "seat three")

	// Open for good: seat posts now pass as they arrive.
	f.post("gemini", "seat four")
	tb.want("seat four")
}

func TestOwnPostFromBeforeTheDelivererDoesNotOpenTheSeal(t *testing.T) {
	f := &fake{nick: tableSelf}
	f.postBefore(tableSelf, "last run's answer")
	f.post("codex", "seat one")
	tb := newTable(t, f, 0)

	tb.want()
	f.post("codex", "seat two")
	tb.want()

	f.post(tableSelf, "this run's answer")
	tb.want("seat one", "seat two")
}

func TestOwnPostWithNoTimeOpensTheSeal(t *testing.T) {
	f := &fake{nick: tableSelf}
	tb := newTable(t, f, 0)

	f.post("codex", "seat one")
	tb.want()

	f.postAt(tableSelf, "my answer, echoed without a time tag", time.Time{})
	tb.want("seat one")
}

func TestSealedChairPostsPassAheadOfHeldSeatPosts(t *testing.T) {
	f := &fake{nick: tableSelf}
	tb := newTable(t, f, 0)

	f.post("codex", "seat one")
	f.post(tableChair, "chair one")
	tb.want("chair one")

	f.post(tableSelf, "my answer")
	tb.want("seat one")
}

func TestBreakerTripsPastTheCapAndTheChairReleasesAheadOfItself(t *testing.T) {
	f := &fake{nick: tableSelf}
	tb := newTable(t, f, 2)

	// The own post opens the seal and is the first of the count.
	f.post(tableSelf, "my answer")
	tb.want()

	f.post("codex", "one")   // count 2: at the cap, delivered
	f.post("gemini", "two")  // count 3: the message that trips it, held
	f.post("codex", "three") // held behind it
	tb.want("one")
	tb.want()

	// The chair's message releases the queue ahead of itself and resets the
	// count to zero: the two released messages do not count toward it.
	f.post(tableChair, "chair one")
	tb.want("two", "three", "chair one")

	f.post("codex", "four") // count 1
	f.post("codex", "five") // count 2: still at the cap
	tb.want("four", "five")
	f.post("codex", "six") // count 3: trips again
	tb.want()
	f.post(tableChair, "chair two")
	tb.want("six", "chair two")
}

func TestBreakerCountsOwnPosts(t *testing.T) {
	f := &fake{nick: tableSelf}
	tb := newTable(t, f, 2)

	f.post(tableSelf, "mine one") // count 1, opens the seal
	f.post(tableSelf, "mine two") // count 2
	f.post("codex", "one")        // count 3: trips
	tb.want()

	f.post(tableChair, "chair one")
	tb.want("one", "chair one")
}

func TestCapZeroMeansNoBreaker(t *testing.T) {
	f := &fake{nick: tableSelf}
	tb := newTable(t, f, 0)

	f.post(tableSelf, "my answer")
	var want []string
	for i := range 50 {
		body := "seat " + strconv.Itoa(i)
		f.post("codex", body)
		want = append(want, body)
	}
	tb.want(want...)
}

func TestSealAndBreakerTogether(t *testing.T) {
	f := &fake{nick: tableSelf}
	tb := newTable(t, f, 2)

	// Sealed, and the breaker trips on the third seat post while still sealed.
	f.post("codex", "one")
	f.post("gemini", "two")
	f.post("codex", "three")
	tb.want()

	// Opening the seal is not enough while the breaker is tripped.
	f.post(tableSelf, "my answer")
	tb.want()
	f.post("codex", "four")
	tb.want()

	// The chair clears the breaker, and with the seal open everything held goes
	// out, in stream order, ahead of the chair message.
	f.post(tableChair, "chair one")
	tb.want("one", "two", "three", "four", "chair one")
}

func TestChairClearsTheBreakerWhileTheSealIsStillClosed(t *testing.T) {
	f := &fake{nick: tableSelf}
	tb := newTable(t, f, 3)

	f.post("codex", "one")
	f.post("gemini", "two")
	f.post("codex", "three")
	f.post("gemini", "four") // count 4: trips
	f.post(tableChair, "chair one")
	tb.want("chair one")

	// Un-tripped but still sealed: seat posts are held on the seal's account.
	f.post("codex", "five") // count 1
	tb.want()

	// The own post is count 2, under the cap, so it opens the seal and the whole
	// queue goes out, the posts held before the chair message among them.
	f.post(tableSelf, "my answer")
	tb.want("one", "two", "three", "four", "five")
}

func TestHeldPostsAreNotHeldOrDeliveredTwice(t *testing.T) {
	for name, f := range map[string]*fake{
		"repeats within a read": {nick: tableSelf, twice: true},
		"repeats across reads":  {nick: tableSelf, atLeastOnce: true},
	} {
		t.Run(name, func(t *testing.T) {
			tb := newTable(t, f, 4)

			// Each post is handed over more than once, and counts once: one, two
			// and the own post make 3, and "three" makes 4, which is at the cap.
			// Counted per delivery of it, the breaker would have tripped by now.
			f.post("codex", "one")
			f.post("gemini", "two")
			tb.want()
			f.post(tableSelf, "my answer")
			tb.want("one", "two")

			// Released and committed, they stay delivered once.
			tb.want()
			f.post("codex", "three")
			tb.want("three")
			tb.want()
		})
	}
}

func TestReplayedHistoryDoesNotHoldOrDeliverAnythingTwice(t *testing.T) {
	f := &fake{nick: tableSelf}
	tb := newTable(t, f, 0)

	f.post("codex", "one")
	tb.want()

	// The cursor is lost and the whole history is read again: the held message
	// is not held a second time, so it is delivered once.
	tb.cursor = ""
	tb.want()
	f.post(tableSelf, "my answer")
	tb.want("one")

	tb.cursor = ""
	tb.want()
}

func TestRepeatedChairAndOwnPostsDoNotMoveTheCountTwice(t *testing.T) {
	f := &fake{nick: tableSelf, twice: true}
	tb := newTable(t, f, 2)

	f.post(tableSelf, "my answer") // count 1, once however often it is handed over
	f.post(tableChair, "chair one")
	f.post(tableSelf, "mine two") // count 1 after the chair message
	f.post("codex", "one")        // count 2: at the cap
	tb.want("chair one", "one")
}

// TestAMessageHeldAcrossAGapIsHeldOnceFromTheReplay: a gap discards the held
// queue, so a message that is still in the retained history comes back only
// because the restart reads it again, and it is held, and delivered, once.
func TestAMessageHeldAcrossAGapIsHeldOnceFromTheReplay(t *testing.T) {
	f := &fake{nick: tableSelf}
	tb := newTable(t, f, 0)

	f.post("codex", "one")
	tb.want()

	// The gap restarts from retained history, which still holds the message that
	// was being held, so the transport hands it over again. It is delivered once.
	f.gapPending = true
	del, err := tb.d.Read(context.Background(), tb.cursor)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !del.Gap || len(del.Messages) != 0 {
		t.Fatalf("delivery gap=%v messages=%q, want the gap alone", del.Gap, bodies(del.Messages))
	}
	tb.cursor = del.Cursor
	tb.d.Commit(del)

	f.post(tableSelf, "my answer")
	tb.want("one")
	tb.want()
}

// TestAGapDiscardsTheHeldQueue: the file adapter's IDs are byte offsets and
// restart once the file rotates, so a message held from before the gap and a
// message after it can carry one ID. The held message goes with the history it
// belonged to; if it were released it would be delivered ahead of the chair's
// message and the chair's, with the same ID, would be dropped as its repeat.
func TestAGapDiscardsTheHeldQueue(t *testing.T) {
	f := &fake{nick: tableSelf}
	tb := newTable(t, f, 1)

	f.post("codex", "old peer") // ID 0, held for the seal; count 1
	f.post(tableSelf, "answer") // ID 1: opens the seal, count 2 trips the breaker
	tb.want()

	// Rotation: the history starts over, and the first message after it has the
	// first one's ID again.
	f.mu.Lock()
	f.msgs = nil
	f.gapPending = true
	f.mu.Unlock()
	f.post(tableChair, "new chair")
	tb.want("new chair")

	// Nothing from before the gap comes back later, either.
	f.post(tableSelf, "answer again")
	tb.want()
}

func TestWaitKeepsWaitingWhenEverythingReadWasHeld(t *testing.T) {
	f := &fake{nick: tableSelf}
	f.post("codex", "one")
	tb := newTable(t, f, 0)

	start := time.Now()
	got, err := tb.d.Wait(context.Background(), "", 150*time.Millisecond, 0)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !got.Empty() {
		t.Fatalf("delivered %q, want nothing", bodies(got.Messages))
	}
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Fatalf("returned after %v, want a full 150ms wait", elapsed)
	}
	if got.Cursor != "1" {
		t.Fatalf("cursor %q, want %q: a held message still advances the cursor", got.Cursor, "1")
	}
}

func TestWaitReturnsOnceTheChairReleasesWhatWasHeld(t *testing.T) {
	f := &fake{nick: tableSelf}
	f.post("codex", "one")
	f.post(tableSelf, "my answer")
	tb := newTable(t, f, 1)

	// "one" was held for the seal, and the own post that opens it is the second
	// of the count against a cap of 1: it trips the breaker, so the first wait
	// ends empty at its deadline.
	got, err := tb.d.Wait(context.Background(), "", 100*time.Millisecond, 0)
	if err != nil || !got.Empty() {
		t.Fatalf("Wait = %q, %v; want nothing", bodies(got.Messages), err)
	}
	tb.cursor = got.Cursor
	tb.d.Commit(got)

	go func() {
		time.Sleep(50 * time.Millisecond)
		f.post(tableChair, "chair one")
	}()
	got, err = tb.d.Wait(context.Background(), tb.cursor, 5*time.Second, 0)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !equal(bodies(got.Messages), []string{"one", "chair one"}) {
		t.Fatalf("delivered %q, want the held post ahead of the chair message", bodies(got.Messages))
	}
}

func TestNoTableDeliversEveryPeerPost(t *testing.T) {
	// An empty chair is no table, and no table is today's behaviour: every post
	// by another nick is delivered at once.
	f := &fake{nick: tableSelf}
	f.post("codex", "one")
	f.post(tableChair, "chair one")
	f.post(tableSelf, "mine")
	d := NewDeliverer(f, tableSelf, WithTable("", 3))
	if d.gate != nil {
		t.Fatal("an empty chair built a gate")
	}
	got, err := d.Read(context.Background(), "")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !equal(bodies(got.Messages), []string{"one", "chair one"}) {
		t.Fatalf("delivered %q, want every peer post", bodies(got.Messages))
	}
}

// TestAnUncommittedDeliveryLeavesTheGateAsItWas: a delivery that never reaches
// its destination is collected again from the previous cursor and must come out
// the same, so what it released is still held until it is committed.
func TestAnUncommittedDeliveryLeavesTheGateAsItWas(t *testing.T) {
	f := &fake{nick: tableSelf}
	tb := newTable(t, f, 0)

	f.post("codex", "A")
	tb.want()
	f.post(tableSelf, "my answer")

	// The delivery that carries A is collected and lost: the hand-off failed.
	lost, err := tb.d.Read(context.Background(), tb.cursor)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got := bodies(lost.Messages); !equal(got, []string{"A"}) {
		t.Fatalf("first collection delivered %q, want A", got)
	}

	// The retry reads from the cursor the caller kept, and finds the seal shut
	// and A held again, then the own post that opens it and releases A.
	tb.want("A")
	tb.want()
}

// TestRetriedUncommittedReadsDoNotCountTwiceTowardTheCap: the same messages read
// again after a failed hand-off are the same messages, not new ones.
func TestRetriedUncommittedReadsDoNotCountTwiceTowardTheCap(t *testing.T) {
	f := &fake{nick: tableSelf}
	tb := newTable(t, f, 2)

	f.post(tableSelf, "my answer") // count 1, opens the seal
	tb.want()
	f.post("codex", "one") // count 2: at the cap

	// Three collections, none committed: counted twice or thrice, "one" would
	// trip the breaker and be held.
	for range 3 {
		del, err := tb.d.Read(context.Background(), tb.cursor)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if got := bodies(del.Messages); !equal(got, []string{"one"}) {
			t.Fatalf("delivered %q, want one", got)
		}
	}
	tb.want("one")

	// The count stands at 2, so the next post is the one that trips it.
	f.post("gemini", "two")
	tb.want()
	f.post(tableChair, "chair one")
	tb.want("two", "chair one")
}

// TestAnEmptyWaitInstallsItsGateChanges: a delivery with nothing in it is never
// committed to anything and the cursor moves past what it read, so the message
// it held must be in the Deliverer's queue when it returns.
func TestAnEmptyWaitInstallsItsGateChanges(t *testing.T) {
	f := &fake{nick: tableSelf}
	f.post("codex", "one")
	tb := newTable(t, f, 0)

	got, err := tb.d.Wait(context.Background(), "", 10*time.Millisecond, 0)
	if err != nil || !got.Empty() {
		t.Fatalf("Wait = %q, %v; want nothing", bodies(got.Messages), err)
	}
	// Not committed, and the cursor is past the held message.
	tb.cursor = got.Cursor
	f.post(tableSelf, "my answer")
	tb.want("one")
}

// TestAStaleCommitDoesNotReplaceTheGate: two deliveries built on the same state,
// the later committed first, leave the gate as the first commit made it.
func TestAStaleCommitDoesNotReplaceTheGate(t *testing.T) {
	f := &fake{nick: tableSelf}
	tb := newTable(t, f, 0)

	f.post("codex", "A")
	tb.want()
	f.post(tableSelf, "my answer")

	first, err := tb.d.Read(context.Background(), tb.cursor)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	second, err := tb.d.Read(context.Background(), tb.cursor)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	tb.d.Commit(second)
	tb.d.Commit(first)
	tb.cursor = second.Cursor
	tb.want()
	f.post("codex", "B")
	tb.want("B")
}

// TestAStaleCommitRecordsNothing: a delivery collected, then overtaken by another
// Wait that installs gate state (an empty or cancelled read), is abandoned. Its
// Commit must record neither the IDs it carried nor its gate changes, or the
// chair message in it would be seen without having reset the breaker, and the
// replay of it would be dropped as a repeat.
func TestAStaleEmptyInstallLeavesANewerGateAlone(t *testing.T) {
	f := &fake{nick: tableSelf}
	tb := newTable(t, f, 1)

	// An empty delivery begun on the gate as it stands now.
	stale := tb.d.begin(tb.cursor)

	// Meanwhile another delivery changes the gate and installs it: the own post
	// opens the seal and the next seat post trips the breaker.
	f.post(tableSelf, "my answer")
	f.post("codex", "one")
	tb.want()

	// The stale delivery's install must not put the sealed, untripped gate back.
	tb.d.install(stale)

	f.post(tableChair, "chair one")
	tb.want("one", "chair one")
}

func TestALaterDeliveryAbandonsTheOneBeforeIt(t *testing.T) {
	f := &fake{nick: tableSelf}
	tb := newTable(t, f, 1)

	f.post(tableSelf, "my answer")  // count 1, opens the seal
	f.post("codex", "one")          // count 2: trips, held
	f.post(tableChair, "chair one") // resets, releases one

	first, err := tb.d.Read(context.Background(), tb.cursor)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	f.post("codex", "two")   // count 1
	f.post("codex", "three") // count 2: trips, held
	second, err := tb.d.Read(context.Background(), tb.cursor)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got := bodies(second.Messages); !equal(got, []string{"one", "chair one", "two"}) {
		t.Fatalf("second collected %q, want [one chair one two]", got)
	}

	// The first delivery was abandoned when the second began: its late commit
	// must not install its gate and leave the second's stale.
	tb.d.Commit(first)
	tb.d.Commit(second)
	tb.cursor = second.Cursor

	f.post(tableChair, "chair two")
	tb.want("three", "chair two")
}

func TestAStaleCommitRecordsNothing(t *testing.T) {
	f := &fake{nick: tableSelf}
	tb := newTable(t, f, 1)

	f.post(tableSelf, "my answer") // count 1, opens the seal
	tb.want()
	f.post("codex", "one") // count 2 passes the cap of 1: trips, held
	tb.want()
	f.post(tableChair, "chair one")

	// The delivery that would reset the breaker and release the held message.
	stale, err := tb.d.Read(context.Background(), tb.cursor)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got := bodies(stale.Messages); !equal(got, []string{"one", "chair one"}) {
		t.Fatalf("collected %q, want [one chair one]", got)
	}

	// Another Wait, cancelled, installs its own gate state before the commit.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	overtaker, err := tb.d.Wait(ctx, tb.cursor, time.Second, 0)
	if !errors.Is(err, context.Canceled) || !overtaker.Empty() {
		t.Fatalf("Wait = %q, %v; want nothing and context.Canceled", bodies(overtaker.Messages), err)
	}

	tb.d.Commit(stale) // abandoned: a no-op

	// The cursor never moved past the abandoned delivery, so the messages come
	// round again, and the chair message resets the breaker this time.
	tb.want("one", "chair one")
	f.post("codex", "two")
	tb.want("two")
}

// TestChairAndOwnNickMatchWithoutRegardToASCIICase: the bundled ergo folds ASCII
// case in nicks, so a chair who posts as "Colin" is the chair a seat was given
// as "colin", and a seat's own post under another case is its own.
func TestChairAndOwnNickMatchWithoutRegardToASCIICase(t *testing.T) {
	f := &fake{nick: tableSelf}
	tb := newTable(t, f, 1)

	// Sealed: only the chair passes, whatever case it posts in.
	f.post("Codex", "seat one")
	f.post("COLIN", "chair one")
	tb.want("chair one")

	// An own post under another case opens the seal and counts against the cap.
	f.post("Claude", "my answer")
	tb.want("seat one")
	f.post("codex", "seat two") // count 2 against a cap of 1: held
	tb.want()
	f.post("Colin", "chair two")
	tb.want("seat two", "chair two")
}

// TestNickEqualityDoesNotFoldBeyondASCII: a nick that differs by a non-ASCII
// case is another nick to the server.
func TestNickEqualityDoesNotFoldBeyondASCII(t *testing.T) {
	f := &fake{nick: "élan"}
	d := NewDeliverer(f, "élan", WithTable("colin", 0))
	d.gate.created = tableStart
	f.postAt("Élan", "someone else", tableStart.Add(time.Second))
	f.postAt("colin", "chair", tableStart.Add(time.Second))
	got, err := d.Read(context.Background(), "")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !equal(bodies(got.Messages), []string{"chair"}) {
		t.Fatalf("delivered %q, want the chair alone: Élan is not élan, so it is held as a seat", bodies(got.Messages))
	}
}
