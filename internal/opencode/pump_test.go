package opencode

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/colinsurprenant/parley/internal/stream"
)

// spy wraps a Client to count what the pump asks of it and to make a history
// read fail the way a real one can, which a fake over a real listener cannot be
// made to do on demand.
type spy struct {
	Client

	mu     sync.Mutex
	health int
	limits []int
	// messages, if set, is asked before each history read with the limit and the
	// number of the read, counting from one; a non-nil answer stands for the read.
	messages func(limit, call int) error
}

func (s *spy) Health(ctx context.Context) (string, error) {
	s.mu.Lock()
	s.health++
	s.mu.Unlock()
	return s.Client.Health(ctx)
}

func (s *spy) Messages(ctx context.Context, session string, limit int) ([]Message, error) {
	s.mu.Lock()
	s.limits = append(s.limits, limit)
	n, hook := len(s.limits), s.messages
	s.mu.Unlock()
	if hook != nil {
		if err := hook(limit, n); err != nil {
			return nil, err
		}
	}
	return s.Client.Messages(ctx, session, limit)
}

func (s *spy) seenHealth() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.health
}

func (s *spy) seenLimits() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int(nil), s.limits...)
}

func tooLong() error { return &TooLongError{Op: "GET /session/x/message", Limit: maxMessageBody} }

// waitStatusReads blocks until the pump has asked the status n times, which for
// a session held busy is how a test knows the pump is in its wait for the turn.
func waitStatusReads(t *testing.T, f *fakeOpenCode, h *harness, n int) {
	t.Helper()
	deadline := time.Now().Add(testDeadline)
	for len(f.statusDirsSeen()) < n {
		if !time.Now().Before(deadline) {
			t.Fatalf("the pump never polled the status %d times; logs:\n%s", n, h.logs.String())
		}
		time.Sleep(pollInterval)
	}
}

// ---- the size of what one delivery hands over ----

func TestAnOversizedMessageGoesOverAsAStubTheReadBackConfirms(t *testing.T) {
	f := newFake(t)
	h := newHarness(t, f, nil)
	h.start()

	h.peerSend(peerNick, strings.Repeat("y", 200<<10))
	posts := f.waitPosts(1)
	got := postedText(t, posts[0].Body)
	if len(got) > 2000 {
		t.Fatalf("a message over its bound went over as %d bytes", len(got))
	}
	for _, want := range []string{"[parley] from=claude-b id=", "\n  [parley opencode] body withheld: this message renders to ", "the full text stays in the stream at id="} {
		if !strings.Contains(got, want) {
			t.Fatalf("the stub does not hold %q:\n%s", want, got)
		}
	}
	// The read-back compared the session's message with what was posted, the stub,
	// and the cursor moved past the message that the stub stands for.
	if users := f.userTexts(); len(users) != 1 || users[0] != got {
		t.Fatalf("the session holds %d bytes of user text, want the stub once", len(strings.Join(users, "")))
	}
	h.cursorPast("")
	if !strings.Contains(h.logs.String(), "go over as stubs") {
		t.Fatalf("the stub was not logged:\n%s", h.logs.String())
	}

	// The next message is a message again, not a stub.
	h.peerSend(peerNick, "small")
	posts = f.waitPosts(2)
	if next := postedText(t, posts[1].Body); !strings.Contains(next, "\n  small\n") || strings.Contains(next, "withheld") {
		t.Fatalf("the message after the stub is\n%q", next)
	}
}

// TestACoalescedDeliveryIsBoundedAndTheMessagesPastTheBoundAreStubs: the backlog
// of a long turn goes over as one delivery, and one delivery is not unbounded:
// the read-back that confirms it reads the history under a bound of its own, and
// a delivery that passed it could never be confirmed, on any restart.
func TestACoalescedDeliveryIsBoundedAndTheMessagesPastTheBoundAreStubs(t *testing.T) {
	f := newFake(t)
	f.set(func() { f.manualBusy = true })
	h := newHarness(t, f, nil)
	h.start()

	h.peerSend(peerNick, "first, small")
	waitStatusReads(t, f, h, 3)
	body := strings.Repeat("z", 100<<10)
	for i := 0; i < 6; i++ {
		h.peerSend(peerNick, fmt.Sprintf("%d %s", i, body))
	}
	f.set(func() { f.manualBusy = false })

	posts := f.waitPosts(1)
	got := postedText(t, posts[0].Body)
	if len(got) > maxDeliveryBytes {
		t.Fatalf("the delivery is %d bytes, over its bound %d", len(got), maxDeliveryBytes)
	}
	if n := strings.Count(got, "[parley] from=claude-b id="); n != 7 {
		t.Fatalf("%d messages in the delivery, want all 7 (one of them a stub)", n)
	}
	if n := strings.Count(got, "body withheld"); n != 1 {
		t.Fatalf("%d stubs, want the one past the bound:\n%s", n, got[len(got)-700:])
	}
	if !strings.Contains(got, fmt.Sprintf("this delivery is over its %d byte bound", maxDeliveryBytes)) {
		t.Fatalf("the stub does not say the delivery was over its bound")
	}
	// Everything the stream had was consumed: the stub stands for the message.
	if got, want := h.cursorPast(""), string(h.read().Cursor); got != want {
		t.Fatalf("the saved cursor is %q, want the end of the backlog %q", got, want)
	}
	if users := f.userTexts(); len(users) != 1 || users[0] != got {
		t.Fatalf("the session does not hold the delivery once, byte for byte")
	}
}

// ---- the history window ----

func TestAHistoryOverTheBoundIsReadAgainNarrowerAndTheReadBackKeepsTheNarrowWindow(t *testing.T) {
	f := newFake(t)
	sp := &spy{Client: f.client()}
	sp.messages = func(limit, _ int) error {
		if limit == historyWindow {
			return tooLong()
		}
		return nil
	}
	h := newHarness(t, f, func(c *Config) { c.Client = sp })
	h.start()

	h.peerSend(peerNick, "heavy history")
	f.waitPosts(1)
	h.cursorPast("")
	limits := sp.seenLimits()
	if len(limits) < 3 || limits[0] != historyWindow || limits[1] != narrowWindow {
		t.Fatalf("the history reads asked for %v, want %d, then %d", limits, historyWindow, narrowWindow)
	}
	for _, l := range limits[1:] {
		if l != narrowWindow {
			t.Fatalf("the history reads asked for %v: after narrowing, every read of the delivery asks for %d", limits, narrowWindow)
		}
	}
	if !strings.Contains(h.logs.String(), fmt.Sprintf("reading the newest %d instead", narrowWindow)) {
		t.Fatalf("the narrowing was not logged:\n%s", h.logs.String())
	}
	// The next delivery starts from the wide window again.
	h.peerSend(peerNick, "and again")
	f.waitPosts(2)
	h.cursorPast("")
	var wide int
	for _, l := range sp.seenLimits() {
		if l == historyWindow {
			wide++
		}
	}
	if wide != 2 {
		t.Fatalf("%d snapshots asked for the wide window, want one per delivery: %v", wide, sp.seenLimits())
	}
}

func TestAHistoryStillOverTheBoundAtTheNarrowWindowStopsThePumpBeforeSendingAnything(t *testing.T) {
	f := newFake(t)
	sp := &spy{Client: f.client(), messages: func(int, int) error { return tooLong() }}
	h := newHarness(t, f, func(c *Config) { c.Client = sp })
	h.start()

	h.peerSend(peerNick, "too heavy")
	err := h.join()
	if !IsTooLong(err) {
		t.Fatalf("Run returned %v, want the over-the-bound error", err)
	}
	for _, want := range []string{fmt.Sprintf("newest %d messages", narrowWindow), fmt.Sprintf("narrowing the read from %d messages", historyWindow), fmt.Sprint(maxMessageBody), "nothing was sent"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the error does not say %q: %v", want, err)
		}
	}
	if n := len(f.postBodies()); n != 0 {
		t.Fatalf("%d prompt(s) posted", n)
	}
	if c := h.savedCursor(); c != "" {
		t.Fatalf("a cursor %q was saved", c)
	}
}

func TestAHistoryThatGrowsOverTheBoundDuringTheReadBackIsNarrowedThere(t *testing.T) {
	f := newFake(t)
	sp := &spy{Client: f.client()}
	// The snapshot (the first read) is fine; the reads after the post find the
	// newest ten over the bound, as a turn that began and printed a great deal
	// would leave them.
	sp.messages = func(limit, call int) error {
		if call > 1 && limit == historyWindow {
			return tooLong()
		}
		return nil
	}
	h := newHarness(t, f, func(c *Config) { c.Client = sp })
	h.start()

	h.peerSend(peerNick, "history grew")
	f.waitPosts(1)
	h.cursorPast("")
	limits := sp.seenLimits()
	if limits[0] != historyWindow || limits[1] != historyWindow || limits[2] != narrowWindow {
		t.Fatalf("the history reads asked for %v, want the snapshot at %d, the read-back at %d and then %d", limits, historyWindow, historyWindow, narrowWindow)
	}
}

func TestAHistoryOverTheBoundAtTheNarrowWindowDuringTheReadBackSaysTheDeliveryLanded(t *testing.T) {
	f := newFake(t)
	sp := &spy{Client: f.client()}
	sp.messages = func(_, call int) error {
		if call > 1 {
			return tooLong()
		}
		return nil
	}
	h := newHarness(t, f, func(c *Config) { c.Client = sp })
	h.start()

	h.peerSend(peerNick, "cannot be confirmed")
	err := h.join()
	if !IsTooLong(err) || !strings.Contains(err.Error(), "was posted to session") || !strings.Contains(err.Error(), "delivered again at the next start") {
		t.Fatalf("Run returned %v, want the posted-but-unconfirmable error", err)
	}
	if c := h.savedCursor(); c != "" {
		t.Fatalf("a cursor %q was saved", c)
	}
}

// ---- a check of the server before every delivery ----

func TestEveryDeliveryIsPrecededByACheckOfTheServer(t *testing.T) {
	f := newFake(t)
	h := newHarness(t, f, nil)
	h.start()

	h.peerSend(peerNick, "first")
	f.waitPosts(1)
	first := h.cursorPast("")

	// The TUI was restarted on an OpenCode 2 install while the pump sat in its
	// wait for the next message: the server answers, and is not the one it was.
	f.set(func() { f.version = "2.0.32" })
	h.peerSend(peerNick, "second")
	err := h.join()
	if err == nil || !strings.Contains(err.Error(), "OpenCode 2 is not supported yet") {
		t.Fatalf("Run returned %v, want the OpenCode 2 refusal", err)
	}
	if n := len(f.postBodies()); n != 1 {
		t.Fatalf("%d prompts, want only the first", n)
	}
	if c := h.savedCursor(); c != first {
		t.Fatalf("the cursor is %q, want it left at %q", c, first)
	}
}

func TestTheDirectoryIsReadBeforeEveryDelivery(t *testing.T) {
	f := newFake(t)
	h := newHarness(t, f, nil)
	h.start()

	h.peerSend(peerNick, "first")
	f.waitPosts(1)
	first := h.cursorPast("")
	f.set(func() { f.dir = "/work/the-session-moved" })
	h.peerSend(peerNick, "second")
	f.waitPosts(2)
	h.cursorPast(first)
	dirs := f.statusDirsSeen()
	if last := dirs[len(dirs)-1]; last != "/work/the-session-moved" {
		t.Fatalf("the last status read was under %q, want the directory the session reports now: %q", last, dirs)
	}
}

// TestATransportFailureBeforeThePostAttachesAgain: a read that fails before the
// post sends the pump back to waiting for an idle session, and that starts with
// the check of the server, as every attempt does.
func TestATransportFailureBeforeThePostAttachesAgain(t *testing.T) {
	f := newFake(t)
	sp := &spy{Client: f.client()}
	sp.messages = func(_, call int) error {
		if call == 1 {
			return &TransportError{Op: "GET /session/x/message", Err: errors.New("connection reset by peer")}
		}
		return nil
	}
	h := newHarness(t, f, func(c *Config) { c.Client = sp })
	h.start()

	h.peerSend(peerNick, "after a reset")
	f.waitPosts(1)
	h.cursorPast("")
	if n := sp.seenHealth(); n != 2 {
		t.Fatalf("the server was checked %d time(s) for one delivery that took two attempts, want 2", n)
	}
	if n := len(f.postBodies()); n != 1 {
		t.Fatalf("%d prompts, want 1", n)
	}
}

// ---- every call of a phase is cut at the phase's deadline ----

// TestAStalledReadDoesNotStretchTheReadBack: the server accepts the post and then
// stops answering the history. The read-back has 150 ms; the client's own timeout
// for one request is two seconds, which is how long a bound checked only between
// calls would take to be noticed.
func TestAStalledReadDoesNotStretchTheReadBack(t *testing.T) {
	f := newFake(t)
	f.set(func() { f.stallMessages = true })
	h := newHarness(t, f, func(c *Config) { c.ReadBackTimeout = 150 * time.Millisecond })
	h.start()

	h.peerSend(peerNick, "then silence")
	f.waitPosts(1)
	posted := time.Now()
	err := h.join()
	elapsed := time.Since(posted)
	if err == nil || !strings.Contains(err.Error(), "no user message holding it appeared") {
		t.Fatalf("Run returned %v, want the read-back timeout", err)
	}
	if elapsed > 1200*time.Millisecond {
		t.Fatalf("the read-back took %s against a bound of 150ms and a request timeout of 2s", elapsed)
	}
	if c := h.savedCursor(); c != "" {
		t.Fatalf("a cursor %q was saved", c)
	}
}

// TestAStalledStatusDoesNotStretchTheSettle: the delivery is confirmed and the
// status then stops answering. The wait to see the turn begin has 100 ms; the
// delivery is committed when it is up, not two seconds later.
func TestAStalledStatusDoesNotStretchTheSettle(t *testing.T) {
	f := newFake(t)
	f.set(func() { f.stallStatus = true })
	h := newHarness(t, f, func(c *Config) { c.SettleTimeout = 100 * time.Millisecond })
	h.start()

	h.peerSend(peerNick, "then the status goes quiet")
	f.waitPosts(1)
	posted := time.Now()
	h.cursorPast("")
	if elapsed := time.Since(posted); elapsed > 1200*time.Millisecond {
		t.Fatalf("the delivery was committed %s after the post, against a settle bound of 100ms and a request timeout of 2s", elapsed)
	}
	h.waitLog("did not go busy")
}

// ---- the gates, across the re-read after a busy wait ----

// TestTheReReadAfterABusyWaitDoesNotDoubleTheLineCount: the first collection of
// a delivery counts its messages in gate state that is the delivery's own until
// it is committed, and the re-read after the wait collects them again from the
// state before. If the count carried over, the first message would be counted
// twice and the third would trip a breaker of three.
func TestTheReReadAfterABusyWaitDoesNotDoubleTheLineCount(t *testing.T) {
	f := newFake(t)
	f.set(func() { f.manualBusy = true })
	h := newGatedHarness(t, f, nil, stream.WithLine("colin", 3, time.Hour))
	h.start()

	h.peerSend(peerNick, "one")
	waitStatusReads(t, f, h, 3)
	h.peerSend(peerNick, "two")
	h.peerSend(peerNick, "three")
	f.set(func() { f.manualBusy = false })

	got := postedText(t, f.waitPosts(1)[0].Body)
	for _, want := range []string{"\n  one\n", "\n  two\n", "\n  three\n"} {
		if !strings.Contains(got, want) {
			t.Fatalf("the delivery after the wait lacks %q:\n%s", want, got)
		}
	}
	h.cursorPast("")

	// The count is three, no more: a fourth trips the breaker and is held until
	// the chair posts, then goes ahead of the chair's own post.
	h.peerSend(peerNick, "four")
	time.Sleep(300 * time.Millisecond)
	if n := len(f.postBodies()); n != 1 {
		t.Fatalf("%d prompts, want the fourth message held", n)
	}
	h.peerSend("colin", "chair says go")
	released := postedText(t, f.waitPosts(2)[1].Body)
	four, chair := strings.Index(released, "\n  four\n"), strings.Index(released, "\n  chair says go\n")
	if four < 0 || chair < 0 || four > chair {
		t.Fatalf("the release is\n%s\nwant the held message ahead of the chair's", released)
	}
}

// TestTheReReadAfterABusyWaitKeepsAHeldMessageHeld: a message the breaker held in
// the first collection is held in the second, not delivered by the second's
// seeing it afresh, and is released by the chair's post as it would have been.
func TestTheReReadAfterABusyWaitKeepsAHeldMessageHeld(t *testing.T) {
	f := newFake(t)
	f.set(func() { f.manualBusy = true })
	h := newGatedHarness(t, f, nil, stream.WithLine("colin", 1, time.Hour))
	h.start()

	h.peerSend(peerNick, "one")
	waitStatusReads(t, f, h, 3)
	h.peerSend(peerNick, "two, over the cap of one")
	f.set(func() { f.manualBusy = false })

	got := postedText(t, f.waitPosts(1)[0].Body)
	if !strings.Contains(got, "\n  one\n") || strings.Contains(got, "two, over the cap") {
		t.Fatalf("the delivery after the wait is\n%s\nwant the first message alone", got)
	}
	h.cursorPast("")
	time.Sleep(200 * time.Millisecond)
	if n := len(f.postBodies()); n != 1 {
		t.Fatalf("%d prompts, the held message was delivered without the chair", n)
	}

	h.peerSend("colin", "chair is here")
	released := postedText(t, f.waitPosts(2)[1].Body)
	two, chair := strings.Index(released, "two, over the cap"), strings.Index(released, "chair is here")
	if two < 0 || chair < 0 || two > chair {
		t.Fatalf("the release is\n%s\nwant the held message ahead of the chair's", released)
	}
}

// TestTheReReadAfterABusyWaitKeepsATablesSealClosed: at a table a seat's post is
// held until this seat has posted, and the re-read after the wait finds the seal
// as the first collection did, closed: the seat's post stays held until the
// pump's own post opens it.
func TestTheReReadAfterABusyWaitKeepsATablesSealClosed(t *testing.T) {
	f := newFake(t)
	f.set(func() { f.manualBusy = true })
	h := newGatedHarness(t, f, nil, stream.WithTable("colin", 0))
	h.start()

	h.peerSend("colin", "chair opens")
	waitStatusReads(t, f, h, 3)
	h.peerSend(peerNick, "seat speaks too early")
	f.set(func() { f.manualBusy = false })

	got := postedText(t, f.waitPosts(1)[0].Body)
	if !strings.Contains(got, "chair opens") || strings.Contains(got, "seat speaks too early") {
		t.Fatalf("the delivery after the wait is\n%s\nwant the chair's post alone", got)
	}
	h.cursorPast("")

	h.peerSend(selfNick, "my answer")
	released := postedText(t, f.waitPosts(2)[1].Body)
	if !strings.Contains(released, "seat speaks too early") || strings.Contains(released, "chair opens") || strings.Contains(released, "my answer") {
		t.Fatalf("the release is\n%s\nwant the held seat post alone", released)
	}
}
