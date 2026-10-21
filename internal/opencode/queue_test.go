package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/colinsurprenant/parley/internal/state"
)

var msgIDRE = regexp.MustCompile(`^msg_[0-9a-f]{12}[0-9A-Za-z]{14}$`)

// post is what a recorded prompt body holds, parsed strictly: the three fields
// this front sends, and nothing else in it.
func post(t *testing.T, body []byte) (id, text, delivery string) {
	t.Helper()
	var p struct {
		ID       string `json:"id"`
		Text     string `json:"text"`
		Delivery string `json:"delivery"`
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		t.Fatalf("the prompt body %s is not {id, text, delivery}: %v", body, err)
	}
	return p.ID, p.Text, p.Delivery
}

func TestAQueuedDeliveryLandsAndIsCommitted(t *testing.T) {
	f := newFake(t)
	h := newHarness(t, f, nil)
	h.start()

	h.peerSend(peerNick, "hello over OpenCode 2")
	posts := f.waitPosts(1)
	id, text, delivery := post(t, posts[0].Body)
	if !msgIDRE.MatchString(id) {
		t.Errorf("the message id %q is not one OpenCode makes", id)
	}
	if delivery != "queue" {
		t.Errorf("the delivery is %q, want queue", delivery)
	}
	// The text is the rendering of the delivery and nothing else, the header
	// included: it is what the stream says, byte for byte.
	if want := h.render(); text != want {
		t.Fatalf("posted\n%q\nwant\n%q", text, want)
	}
	if !strings.Contains(text, "[parley] from=claude-b id=") || !strings.Contains(text, "hello over OpenCode 2") {
		t.Fatalf("the text is not a rendered peer message:\n%s", text)
	}
	h.cursorPast("")
	if users := f.userTexts(); len(users) != 1 || users[0] != text {
		t.Fatalf("the session holds %v, want the delivery once", users)
	}
	if n := len(f.patched()); n != 0 {
		t.Fatalf("%d change request(s) for a delivery the session took up at once", n)
	}
	if strings.Contains(h.logs.String(), "settle") || strings.Contains(h.logs.String(), "did not go busy") {
		t.Fatalf("OpenCode 2 has no settle step:\n%s", h.logs.String())
	}
}

// TestADeliveryWaitsForAnIdleSessionAndTheBacklogGoesOverAsOne: the pump delivers
// into an idle session, though OpenCode 2 would queue the post behind the turn,
// so that what arrives while a turn runs is one delivery and not one turn each.
func TestADeliveryWaitsForAnIdleSessionAndTheBacklogGoesOverAsOne(t *testing.T) {
	f := newFake(t)
	f.set(func() { f.manualBusy = true })
	h := newHarness(t, f, nil)
	h.start()

	h.peerSend(peerNick, "first")
	f.waitReads("active", 3)
	if n := len(f.postBodies()); n != 0 {
		t.Fatalf("%d prompt(s) posted into a busy session", n)
	}
	h.peerSend(peerNick, "second")
	f.release()

	posts := f.waitPosts(1)
	_, text, _ := post(t, posts[0].Body)
	if !strings.Contains(text, "\n  first\n") || !strings.Contains(text, "\n  second\n") {
		t.Fatalf("the backlog did not go over as one delivery:\n%s", text)
	}
	if posts[0].TurnRunning {
		t.Fatal("the delivery was posted while a turn ran")
	}
	h.cursorPast("")
	if n := len(f.postBodies()); n != 1 {
		t.Fatalf("%d prompts, want one", n)
	}
}

// TestAPostThatGotNoAnswerIsPostedAgainUnderTheSameID: the server admitted the
// entry and the answer was lost. The id is what makes the repeat safe: the session
// ends up with the delivery once.
func TestAPostThatGotNoAnswerIsPostedAgainUnderTheSameID(t *testing.T) {
	f := newFake(t)
	f.set(func() { f.loseResponses = 2 })
	h := newHarness(t, f, nil)
	h.start()

	h.peerSend(peerNick, "sent through a bad connection")
	posts := f.waitPosts(3)
	first, _, _ := post(t, posts[0].Body)
	for i, p := range posts {
		id, _, _ := post(t, p.Body)
		if id != first {
			t.Fatalf("post %d was made under id %s, not the %s of the first", i, id, first)
		}
	}
	h.cursorPast("")
	if users := f.userTexts(); len(users) != 1 {
		t.Fatalf("the session holds %d copies of the delivery: %v", len(users), users)
	}
	if !strings.Contains(h.logs.String(), "posting the delivery again under message id "+first) {
		t.Fatalf("the repeat was not logged:\n%s", h.logs.String())
	}
}

// TestAPostThatNeverGetsAnAnswerStopsThePumpUncommitted: the bound of the read-back
// applies to the post, and what stops the pump says the delivery may have landed.
func TestAPostThatNeverGetsAnAnswerStopsThePumpUncommitted(t *testing.T) {
	f := newFake(t)
	f.set(func() { f.loseResponses = 1 << 20 })
	h := newHarness(t, f, func(c *Config) { c.PostTimeout = 150 * time.Millisecond })
	h.start()

	h.peerSend(peerNick, "never answered")
	err := h.join()
	if err == nil || !strings.Contains(err.Error(), "may have landed") || !strings.Contains(err.Error(), "delivered again at the next start") {
		t.Fatalf("Run returned %v", err)
	}
	if c := h.savedCursor(); c != "" {
		t.Fatalf("a cursor %q was saved", c)
	}
	posts := f.postBodies()
	first, _, _ := post(t, posts[0].Body)
	for _, p := range posts {
		if id, _, _ := post(t, p.Body); id != first {
			t.Fatalf("a repeat was made under id %s, not %s", id, first)
		}
	}
	if users := f.userTexts(); len(users) != 1 {
		t.Fatalf("the session holds %d copies of the delivery", len(users))
	}
}

// TestTheMessageIsReadAgainWhenTheEntryLeftTheInboxBetweenTheReads: the read of the
// message is a 404 and the session takes the entry up before the read of the inbox,
// which no longer lists it. An entry that is gone is a message or a cancel, and the
// message is read once more to tell which: here it is there.
func TestTheMessageIsReadAgainWhenTheEntryLeftTheInboxBetweenTheReads(t *testing.T) {
	f := newFake(t)
	f.set(func() { f.lagReads = 1 })
	h := newHarness(t, f, nil)
	h.start()

	h.peerSend(peerNick, "taken up between the reads")
	f.waitPosts(1)
	h.cursorPast("")
	if n := f.read("message"); n != 2 {
		t.Fatalf("%d read(s) of the message, want the 404 and the one after the inbox", n)
	}
	if users := f.userTexts(); len(users) != 1 {
		t.Fatalf("the session holds %v", users)
	}
	if strings.Contains(h.logs.String(), "cancelled") {
		t.Fatalf("a delivery that was taken up was called cancelled:\n%s", h.logs.String())
	}
	if n := len(f.patched()); n != 0 {
		t.Fatalf("a delivery that was only in the inbox for a moment was changed %d time(s)", n)
	}
}

// TestAnEntryTheSessionTakesUpWithinTheParkedPollsIsNotCalledParked: it takes two reads
// of an idle session, spaced by the idle poll, to call an entry parked, and an entry
// the session takes up before the second is not called so.
func TestAnEntryTheSessionTakesUpWithinTheParkedPollsIsNotCalledParked(t *testing.T) {
	f := newFake(t)
	f.set(func() { f.lagReads = 2 })
	h := newHarness(t, f, func(c *Config) { c.IdlePoll = 300 * time.Millisecond })
	h.start()

	h.peerSend(peerNick, "a session that is slow to start")
	f.waitPosts(1)
	h.cursorPast("")
	if n := len(f.patched()); n != 0 {
		t.Fatalf("an entry the session took up on its own was changed %d time(s)", n)
	}
	if strings.Contains(h.logs.String(), "parked") {
		t.Fatalf("an entry the session took up was called parked:\n%s", h.logs.String())
	}
}

// TestAParkedDeliveryWaitsForTheHumanToResumeAndIsNeverSteered: after an interrupt
// the queue is parked and nothing takes up an entry. The entry is in the inbox with
// the session idle, and the pump says once that it is parked and goes on waiting,
// with no deadline and no request to change it: a steer would be promoted into
// whatever turn runs next, and would wake what the human parked. The entry goes over
// when the human resumes the session.
func TestAParkedDeliveryWaitsForTheHumanToResumeAndIsNeverSteered(t *testing.T) {
	f := newFake(t)
	f.set(func() { f.parked = true })
	h := newHarness(t, f, func(c *Config) { c.PostTimeout = 150 * time.Millisecond })
	h.start()

	h.peerSend(peerNick, "into a parked session")
	f.waitPosts(1)
	h.waitLog("parked after an interrupt; it goes over when the session takes it up")
	// Far past the bound of the post, and past many idle polls.
	time.Sleep(700 * time.Millisecond)
	if c := h.savedCursor(); c != "" {
		t.Fatalf("a cursor %q was saved while the delivery was parked", c)
	}
	select {
	case <-h.done:
		t.Fatalf("the pump stopped with %v while the delivery was parked", h.runErr)
	default:
	}
	if got := f.patched(); len(got) != 0 {
		t.Fatalf("the pump changed an entry (%q); it never steers", got)
	}
	if n := f.read("active"); n < 3 {
		t.Fatalf("%d read(s) of the active list: the pump was not looking", n)
	}
	f.resume()
	h.cursorPast("")
	if users := f.userTexts(); len(users) != 1 {
		t.Fatalf("the session holds %v", users)
	}
	if n := strings.Count(h.logs.String(), "parked after an interrupt"); n != 1 {
		t.Fatalf("the parked state was logged %d times, want once:\n%s", n, h.logs.String())
	}
	if got := f.patched(); len(got) != 0 {
		t.Fatalf("the pump changed an entry (%q); it never steers", got)
	}
}

// TestADeliveryBehindAnotherTurnWaitsPastTheBoundOfTheReadBack: the human started
// a turn in the moment between the idle check and the post. The entry waits in the
// inbox behind it, which is not a failure and has no bound: the read-back bound is
// the bound of the post, and this wait has none.
func TestADeliveryBehindAnotherTurnWaitsPastTheBoundOfTheReadBack(t *testing.T) {
	f := newFake(t)
	f.set(func() { f.busyOnPost = true })
	h := newHarness(t, f, func(c *Config) { c.PostTimeout = 150 * time.Millisecond })
	h.start()

	h.peerSend(peerNick, "behind the human's own turn")
	f.waitPosts(1)
	time.Sleep(600 * time.Millisecond)
	if c := h.savedCursor(); c != "" {
		t.Fatalf("a cursor %q was saved while the delivery was still in the inbox", c)
	}
	if n := len(f.patched()); n != 0 {
		t.Fatalf("an entry behind a running turn was changed %d time(s)", n)
	}
	f.set(func() { f.busyOnPost = false })
	f.release()
	h.cursorPast("")
	if users := f.userTexts(); len(users) != 1 {
		t.Fatalf("the session holds %v", users)
	}
	if !strings.Contains(h.logs.String(), "queued behind a turn of session "+testSession) {
		t.Fatalf("the wait was not logged:\n%s", h.logs.String())
	}
	if n := len(f.postBodies()); n != 1 {
		t.Fatalf("%d prompts, want one", n)
	}
}

// TestADeliveryTheHumanCancelledIsHandedOverAndNotSentAgain: the entry is gone
// from the inbox and is not a message. Only the human does that, on purpose, and
// the pump counts the delivery as made.
func TestADeliveryTheHumanCancelledIsHandedOverAndNotSentAgain(t *testing.T) {
	f := newFake(t)
	f.set(func() { f.cancelOnPost = true })
	h := newHarness(t, f, nil)
	h.start()

	h.peerSend(peerNick, "taken back by the human")
	f.waitPosts(1)
	first := h.cursorPast("")
	if !strings.Contains(h.logs.String(), "it was cancelled there") {
		t.Fatalf("the cancel was not logged:\n%s", h.logs.String())
	}
	// A later message is delivered, and the cancelled one is not delivered again
	// with it.
	f.set(func() { f.cancelOnPost = false })
	h.peerSend(peerNick, "after the cancel")
	posts := f.waitPosts(2)
	h.cursorPast(first)
	_, text, _ := post(t, posts[1].Body)
	if strings.Contains(text, "taken back by the human") || !strings.Contains(text, "after the cancel") {
		t.Fatalf("the second delivery is\n%s", text)
	}
	if n := len(f.postBodies()); n != 2 {
		t.Fatalf("%d prompts, want the two deliveries", n)
	}
}

// TestAMessageHoldingOtherTextIsNotADelivery: the proof of a delivery is the bytes.
func TestAMessageHoldingOtherTextIsNotADelivery(t *testing.T) {
	f := newFake(t)
	f.set(func() { f.corrupt = true })
	h := newHarness(t, f, nil)
	h.start()

	h.peerSend(peerNick, "altered on the way")
	err := h.join()
	if err == nil || !strings.Contains(err.Error(), "not shown to have landed") {
		t.Fatalf("Run returned %v", err)
	}
	if c := h.savedCursor(); c != "" {
		t.Fatalf("a cursor %q was saved", c)
	}
}

// TestADeliveryIntoASessionThatWasDeletedStopsThePump: it cannot be confirmed, and
// is not committed.
func TestADeliveryIntoASessionThatWasDeletedStopsThePump(t *testing.T) {
	f := newFake(t)
	f.set(func() { f.deleteOnPost, f.cancelOnPost = true, true })
	h := newHarness(t, f, nil)
	h.start()

	h.peerSend(peerNick, "into a session about to go")
	err := h.join()
	if !errors.Is(err, ErrSessionNotFound) || !strings.Contains(err.Error(), "no longer knows") {
		t.Fatalf("Run returned %v", err)
	}
	if c := h.savedCursor(); c != "" {
		t.Fatalf("a cursor %q was saved", c)
	}
}

// TestAServerThatAsksForAPasswordStopsThePumpWithoutLeakingIt: a 401 is the
// pump's own configuration to put right, so it stops with the advice, and the
// password that was sent is nowhere in what it says.
func TestAServerThatAsksForAPasswordStopsThePumpWithoutLeakingIt(t *testing.T) {
	f := newFake(t)
	const secret = "s3cr3t-pa55-word"
	f.set(func() { f.password = secret })
	c, err := NewHTTPClient(f.url(), Auth{Password: "wrong-" + secret}, WithRequestTimeout(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "chat.ndjson")
	dir, err := state.Open(t.TempDir(), selfNick, "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	h := newHarnessOn(t, c, path, dir, nil)
	h.start()
	h.peerSend(peerNick, "to a server that wants a password")
	err = h.join()
	if err == nil || !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "OPENCODE_PASSWORD") {
		t.Fatalf("Run returned %v", err)
	}
	for _, out := range []string{err.Error(), h.logs.String()} {
		if strings.Contains(out, secret) {
			t.Fatalf("the password is in\n%s", out)
		}
	}
	if n := len(f.postBodies()); n != 0 {
		t.Fatalf("%d prompt(s) reached a server that refused the credentials", n)
	}
}

// spy wraps a Client to make a post fail the way a real one can, which a fake
// over a real listener cannot be made to do on demand.
type spy struct {
	Client

	mu      sync.Mutex
	prompts int
	// promptErr, if set, is asked before each post with the number of the post; a
	// non-nil answer stands for it.
	promptErr func(call int) error
}

func (s *spy) Prompt(ctx context.Context, session, id, text string) error {
	s.mu.Lock()
	s.prompts++
	n, hook := s.prompts, s.promptErr
	s.mu.Unlock()
	if hook != nil {
		if err := hook(n); err != nil {
			return err
		}
	}
	return s.Client.Prompt(ctx, session, id, text)
}

// TestADeliveryThatNeverConnectedIsStartedOver: every attempt failed to connect, so
// nothing was sent; the pump goes back to waiting for the server, and the delivery
// goes over, under an id of its own, once.
func TestADeliveryThatNeverConnectedIsStartedOver(t *testing.T) {
	f := newFake(t)
	dial := &TransportError{Op: "POST /api/session/x/prompt", Err: errors.New("connection refused"), Dial: true}
	s := &spy{Client: f.client(), promptErr: func(call int) error {
		if call == 1 {
			return dial
		}
		return nil
	}}
	path := filepath.Join(t.TempDir(), "chat.ndjson")
	dir, err := state.Open(t.TempDir(), selfNick, "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	h := newHarnessOn(t, s, path, dir, nil)
	h.start()

	h.peerSend(peerNick, "sent while the server was away")
	f.waitPosts(1)
	h.cursorPast("")
	if !strings.Contains(h.logs.String(), "nothing was sent, waiting for the server again") {
		t.Fatalf("the unsent post was not logged:\n%s", h.logs.String())
	}
	if users := f.userTexts(); len(users) != 1 {
		t.Fatalf("the session holds %v", users)
	}
}

// TestAPostThatConnectedOnceIsNotStartedOverAfterADial: a failure to connect that
// follows an attempt that may have reached the server does not make the delivery
// unsent; it is posted again under the same id.
func TestAPostThatConnectedOnceIsNotStartedOverAfterADial(t *testing.T) {
	f := newFake(t)
	reset := &TransportError{Op: "POST", Err: errors.New("connection reset")}
	dial := &TransportError{Op: "POST", Err: errors.New("connection refused"), Dial: true}
	s := &spy{Client: f.client(), promptErr: func(call int) error {
		switch call {
		case 1:
			return reset
		case 2:
			return dial
		}
		return nil
	}}
	path := filepath.Join(t.TempDir(), "chat.ndjson")
	dir, err := state.Open(t.TempDir(), selfNick, "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	h := newHarnessOn(t, s, path, dir, nil)
	h.start()

	h.peerSend(peerNick, "sent through a flapping server")
	f.waitPosts(1)
	h.cursorPast("")
	if strings.Contains(h.logs.String(), "nothing was sent") {
		t.Fatalf("a delivery that may have landed was called unsent:\n%s", h.logs.String())
	}
	if n := len(f.postBodies()); n != 1 {
		t.Fatalf("%d post(s) reached the server, want the one", n)
	}
}

// TestADeliveryOverTheBoundGoesOverAsAStubTheReadBackConfirms: the bound on a
// delivery is the one the Codex front applies, and what is posted is the rendering
// of the bounded delivery.
func TestADeliveryOverTheBoundGoesOverAsAStubTheReadBackConfirms(t *testing.T) {
	f := newFake(t)
	h := newHarness(t, f, nil)
	h.start()

	h.peerSend(peerNick, strings.Repeat("y", 200<<10))
	posts := f.waitPosts(1)
	_, text, _ := post(t, posts[0].Body)
	if len(text) > 2000 {
		t.Fatalf("a message over its bound went over as %d bytes", len(text))
	}
	for _, want := range []string{"[parley] from=claude-b id=", "body withheld: this message renders to ", "the full text stays in the stream at id="} {
		if !strings.Contains(text, want) {
			t.Fatalf("the stub does not hold %q:\n%s", want, text)
		}
	}
	h.cursorPast("")
	if users := f.userTexts(); len(users) != 1 || users[0] != text {
		t.Fatalf("the session holds %d bytes of user text, want the stub once", len(strings.Join(users, "")))
	}
}

// TestACoalescedDeliveryIsBoundedAndTheMessagesPastTheBoundAreStubs: the backlog
// of a long turn goes over as one delivery, and one delivery is not unbounded.
// Each message here is within its own bound, and together they pass the bound of a
// delivery: the ones that fit go whole, the one past it goes as a stub under its own
// header, the read-back confirms the text that was posted, and the cursor moves past
// every message, the stubbed one included.
func TestACoalescedDeliveryIsBoundedAndTheMessagesPastTheBoundAreStubs(t *testing.T) {
	f := newFake(t)
	f.set(func() { f.manualBusy = true })
	h := newHarness(t, f, nil)
	h.start()

	h.peerSend(peerNick, "first, small")
	waitActiveReads(t, f, h, 3)
	body := strings.Repeat("z", 100<<10)
	for i := 0; i < 6; i++ {
		h.peerSend(peerNick, fmt.Sprintf("%d %s", i, body))
	}
	if n := len(f.postBodies()); n != 0 {
		t.Fatalf("%d prompt(s) posted into a busy session", n)
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
		t.Fatalf("%d stubs, want the one past the bound:\n%s", n, got[max(0, len(got)-700):])
	}
	if !strings.Contains(got, fmt.Sprintf("this delivery is over its %d byte bound", maxDeliveryBytes)) {
		t.Fatalf("the stub does not say the delivery was over its bound:\n%s", got[max(0, len(got)-700):])
	}
	if !strings.Contains(got, "\n  first, small\n") || !strings.Contains(got, "\n  4 zzz") || strings.Contains(got, "\n  5 zzz") {
		t.Fatalf("the delivery does not hold the first five whole and not the sixth: first %v, 4th %v, 5th %v",
			strings.Contains(got, "\n  first, small\n"), strings.Contains(got, "\n  4 zzz"), strings.Contains(got, "\n  5 zzz"))
	}
	// The read-back compared the session's message with what was posted, and
	// the stub stands for the message past the bound: the stream is consumed to its end.
	if got, want := h.cursorPast(""), string(h.read().Cursor); got != want {
		t.Fatalf("the saved cursor is %q, want the end of the backlog %q", got, want)
	}
	if users := f.userTexts(); len(users) != 1 || users[0] != got {
		t.Fatalf("the session does not hold the delivery once, byte for byte")
	}
	if !strings.Contains(h.logs.String(), "1 of 7 message(s) go over as stubs") {
		t.Fatalf("the stub was not logged:\n%s", h.logs.String())
	}
	time.Sleep(100 * time.Millisecond)
	if n := len(f.postBodies()); n != 1 {
		t.Fatalf("%d prompts, want the one delivery", n)
	}
}

// TestTheServerIsCheckedBeforeEveryDelivery: the version and the session are asked
// for again for each delivery, whatever the pump did in between.
func TestTheServerIsCheckedBeforeEveryDelivery(t *testing.T) {
	f := newFake(t)
	h := newHarness(t, f, nil)
	h.start()

	h.peerSend(peerNick, "one")
	f.waitPosts(1)
	c1 := h.cursorPast("")
	infos, sessions := f.read("info"), f.read("session")
	h.peerSend(peerNick, "two")
	f.waitPosts(2)
	h.cursorPast(c1)
	if f.read("info") <= infos || f.read("session") <= sessions {
		t.Fatalf("no check of the server for the second delivery: info %d then %d, session %d then %d", infos, f.read("info"), sessions, f.read("session"))
	}
}

func TestAShutdownDuringTheWaitForTheMessageCommitsNothing(t *testing.T) {
	f := newFake(t)
	f.set(func() { f.busyOnPost = true })
	h := newHarness(t, f, nil)
	cancel := h.start()

	h.peerSend(peerNick, "interrupted in the inbox")
	f.waitPosts(1)
	cancel()
	if err := h.join(); err != nil {
		t.Fatalf("Run returned %v after a shutdown", err)
	}
	if c := h.savedCursor(); c != "" {
		t.Fatalf("a cursor %q was saved", c)
	}
}

// TestAnInboxTooLargeToReadIsWaitedOutAndNeverCommitted: the inbox carries every
// queued entry's payload, attachments included, and the human's large queued prompt
// can pass the bound it is read under. Unread, it cannot say whether the delivery's
// entry is still there, so the pump does not call it cancelled and does not give up:
// it says so once and goes on asking for the message.
func TestAnInboxTooLargeToReadIsWaitedOutAndNeverCommitted(t *testing.T) {
	f := newFake(t)
	// The entry is gone from the inbox, as a cancelled one is; the inbox cannot say.
	f.set(func() { f.inboxTooLong, f.cancelOnPost = true, true })
	h := newHarness(t, f, func(c *Config) { c.PostTimeout = 150 * time.Millisecond })
	h.start()

	h.peerSend(peerNick, "behind an inbox that cannot be read")
	posts := f.waitPosts(1)
	h.waitLog("inbox is over the")
	time.Sleep(600 * time.Millisecond)
	if c := h.savedCursor(); c != "" {
		t.Fatalf("a cursor %q was saved on an inbox that could not be read", c)
	}
	select {
	case <-h.done:
		t.Fatalf("the pump stopped with %v on an inbox that could not be read", h.runErr)
	default:
	}
	if strings.Contains(h.logs.String(), "cancelled") {
		t.Fatalf("the delivery was called cancelled on an inbox that could not be read:\n%s", h.logs.String())
	}
	if n := strings.Count(h.logs.String(), "inbox is over the"); n != 1 {
		t.Fatalf("the oversized inbox was logged %d times, want once:\n%s", n, h.logs.String())
	}
	for _, want := range []string{"cannot be told whether delivery", "attachments are the likely cause"} {
		if !strings.Contains(h.logs.String(), want) {
			t.Fatalf("the log does not say %q:\n%s", want, h.logs.String())
		}
	}
	// The message shows, as the session takes it up: the delivery lands.
	id, text, _ := post(t, posts[0].Body)
	f.set(func() { f.msgs = append(f.msgs, fakeMsg{id: id, text: text}) })
	h.cursorPast("")
}

// TestAPromptTheServerEchoesAsOtherTextIsSaidToBeAdmitted: after admission the entry
// is in the inbox and will probably be taken up as the server holds it. The pump
// stops, commits nothing, and says the entry is there and that a restart sends the
// delivery again under a new id.
func TestAPromptTheServerEchoesAsOtherTextIsSaidToBeAdmitted(t *testing.T) {
	f := newFake(t)
	f.set(func() { f.echoOther = true })
	h := newHarness(t, f, nil)
	h.start()

	h.peerSend(peerNick, "rewritten by a plugin")
	err := h.join()
	for _, want := range []string{"admitted the prompt", "a plugin may have rewritten it", "will probably be taken up as it is", "a restart sends the delivery again under a new id"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("Run returned %v, want it to say %q", err, want)
		}
	}
	if c := h.savedCursor(); c != "" {
		t.Fatalf("a cursor %q was saved", c)
	}
}

// writeFakeService writes the registration file the way the service does: a
// temporary file in the directory, mode 0600, renamed into place.
func writeFakeService(t *testing.T, path string, f *fakeOpenCode, password string) {
	t.Helper()
	f.mu.Lock()
	pid, version, addr := f.pid, f.version, f.addr
	f.mu.Unlock()
	writeServiceJSON(t, path, "http://"+addr, version, pid, password)
}

// writeServiceJSON writes a registration file with the values given, which a test
// can make wrong.
func writeServiceJSON(t *testing.T, path, url, version string, pid int, password string) {
	t.Helper()
	if err := putServiceJSON(path, url, version, pid, password); err != nil {
		t.Fatal(err)
	}
}

// putServiceJSON is writeServiceJSON for a goroutine that is not the test's.
func putServiceJSON(path, url, version string, pid int, password string) error {
	b, err := json.Marshal(map[string]any{"id": "svc", "version": version, "url": url, "pid": pid, "password": password})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// newDiscoveryHarness is a pump that finds the service from the file at path, as
// `parley opencode` does without --port.
func newDiscoveryHarness(t *testing.T, path string) *harness {
	t.Helper()
	d := &Discovery{Path: path, Options: []Option{WithRequestTimeout(2 * time.Second)}}
	_, c, _, err := d.Find(context.Background())
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	chat := filepath.Join(t.TempDir(), "chat.ndjson")
	dir, err := state.Open(t.TempDir(), selfNick, "file:"+chat)
	if err != nil {
		t.Fatal(err)
	}
	return newHarnessOn(t, c, chat, dir, func(cfg *Config) { cfg.Discovery = d })
}

func (f *fakeOpenCode) setPassword(pw string) { f.set(func() { f.password = pw }) }

// TestTheServiceIsFoundAgainAfterItRestartsElsewhere: a restarted service has a new
// address, password and process id. The pump read the file once, at the start, and
// reads it again each time it attaches, so the next delivery goes to the new one.
func TestTheServiceIsFoundAgainAfterItRestartsElsewhere(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode", "service.json")
	a := newFake(t)
	a.setPassword("password-of-a")
	writeFakeService(t, path, a, "password-of-a")
	h := newDiscoveryHarness(t, path)
	h.start()

	h.peerSend(peerNick, "to the first service")
	a.waitPosts(1)
	first := h.cursorPast("")

	a.stop()
	b := newFake(t)
	b.set(func() { b.pid, b.password = 777, "password-of-b" })
	writeFakeService(t, path, b, "password-of-b")
	h.peerSend(peerNick, "to the restarted service")
	b.waitPosts(1)
	h.cursorPast(first)
	if n := len(a.postBodies()); n != 1 {
		t.Fatalf("%d prompts reached the first service, want the one", n)
	}
	if users := b.userTexts(); len(users) != 1 || !strings.Contains(users[0], "to the restarted service") {
		t.Fatalf("the restarted service holds %v", users)
	}
}

// TestADeliveryIsHeldForAServiceThatIsNotThereOrIsStarting: with the file gone, a
// file that describes another process, or a service that answers 503, the pump holds
// with its backoff, says so once, and delivers when the service is up.
func TestADeliveryIsHeldForAServiceThatIsNotThereOrIsStarting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode", "service.json")
	a := newFake(t)
	writeFakeService(t, path, a, "")
	h := newDiscoveryHarness(t, path)
	h.start()

	h.peerSend(peerNick, "one")
	a.waitPosts(1)
	first := h.cursorPast("")

	// The service is gone and its file with it.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	h.peerSend(peerNick, "two")
	h.waitLog("OpenCode is not answering")
	time.Sleep(100 * time.Millisecond)
	if c := h.savedCursor(); c != first {
		t.Fatalf("a delivery was committed with no service: %q", c)
	}
	// The file is back but describes a process that is not the one answering (a
	// file left behind, or written a moment before the service is up): the pump
	// asks and keeps holding.
	a.mu.Lock()
	url := "http://" + a.addr
	a.mu.Unlock()
	writeServiceJSON(t, path, url, "2.0.32", 999, "")
	reads := a.read("info")
	a.waitReads("info", reads+3)
	if c := h.savedCursor(); c != first {
		t.Fatalf("a delivery was committed to a server the file does not describe: %q", c)
	}
	// The file now describes a service that answers 503 for a while (starting).
	b := newFake(t)
	b.set(func() { b.pid, b.info503 = 555, 3 })
	writeFakeService(t, path, b, "")
	b.waitPosts(1)
	h.cursorPast(first)
	if n := strings.Count(h.logs.String(), "OpenCode is not answering"); n < 1 {
		t.Fatalf("the outage was not logged:\n%s", h.logs.String())
	}
	if !strings.Contains(h.logs.String(), "OpenCode is back") {
		t.Fatalf("the return was not logged:\n%s", h.logs.String())
	}
	if n := len(a.postBodies()); n != 1 {
		t.Fatalf("%d prompts reached the first service while it was away, want the one", n)
	}
}

// TestA401AfterTheFileIsReadAgainStopsThePump: the password in the file is the
// service's own; if it is refused after the file was read, there is nothing to wait
// for.
func TestA401AfterTheFileIsReadAgainStopsThePump(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode", "service.json")
	a := newFake(t)
	a.setPassword("right")
	writeFakeService(t, path, a, "right")
	h := newDiscoveryHarness(t, path)
	h.start()

	h.peerSend(peerNick, "one")
	a.waitPosts(1)
	h.cursorPast("")
	writeFakeService(t, path, a, "wrong")
	h.peerSend(peerNick, "two")
	err := h.join()
	if err == nil || !strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), "wrong") {
		t.Fatalf("Run returned %v", err)
	}
	if n := len(a.postBodies()); n != 1 {
		t.Fatalf("%d prompts reached a service that refused the password", n)
	}
}

// TestAStaleServiceFileReadsAsStaleBeforeTheSessionIsAsked: Info and the cross-check
// come first, so a file for another process is not a session the server does not
// know.
func TestAStaleServiceFileReadsAsStaleBeforeTheSessionIsAsked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode", "service.json")
	a := newFake(t)
	a.set(func() { a.exists = false })
	a.set(func() { a.pid = 1 })
	writeFakeService(t, path, a, "")
	a.set(func() { a.pid = 2 })
	d := Discovery{Path: path, Options: []Option{WithRequestTimeout(2 * time.Second)}}
	_, _, _, err := d.Find(context.Background())
	var stale *StaleServiceError
	if !errors.As(err, &stale) || stale.FilePID != 1 || stale.PID != 2 {
		t.Fatalf("Find = %v, want a StaleServiceError for pid 1 against 2", err)
	}
	if n := a.read("session"); n != 0 {
		t.Fatalf("the session was asked for %d times before the file was shown to be stale", n)
	}
	if !strings.Contains(err.Error(), "the file is stale or the address belongs to another server") {
		t.Fatalf("the error: %v", err)
	}
}

// TestAServerThatGoesAwayDuringTheWaitIsWaitedFor: the delivery is in the server's
// inbox, which survives the server; the pump holds with its backoff, says so, and
// goes on asking for the message when the server is back. It has no deadline to
// give up at, and sends nothing twice.
func TestAServerThatGoesAwayDuringTheWaitIsWaitedFor(t *testing.T) {
	f := newFake(t)
	f.set(func() { f.busyOnPost = true })
	h := newHarness(t, f, func(c *Config) { c.PostTimeout = 100 * time.Millisecond })
	h.start()

	h.peerSend(peerNick, "behind a turn, across a restart")
	f.waitPosts(1)
	addr := f.address()
	f.stop()
	h.waitLog("the delivery is queued there, holding for the server")
	time.Sleep(400 * time.Millisecond)
	f.start(addr)
	f.set(func() { f.busyOnPost = false })
	f.release()
	h.cursorPast("")
	h.waitLog("OpenCode is back")
	if n := len(f.postBodies()); n != 1 {
		t.Fatalf("%d prompts, want the one", n)
	}
}

// TestAPostThatGotNoAnswerIsPostedAgainOnlyAfterTheServiceIsFoundAgain: the answer
// to the first post is lost, and the service's file goes stale at that moment (a
// service that restarted, or a file that was written a moment before it was up). A
// client whose attach failed is not one to post through: the pump attaches until
// the file describes the server that answers, within the bound of the post, and only
// then posts the same id again.
func TestAPostThatGotNoAnswerIsPostedAgainOnlyAfterTheServiceIsFoundAgain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode", "service.json")
	a := newFake(t)
	writeFakeService(t, path, a, "")
	a.set(func() {
		a.loseResponses = 1
		a.onLose = func() { _ = putServiceJSON(path, "http://"+a.addr, a.version, 999, "") }
	})
	h := newDiscoveryHarness(t, path)
	h.start()

	base := a.read("info")
	h.peerSend(peerNick, "sent as the service restarted")
	// The attach before the post, then two attaches that find the file stale.
	deadline := time.Now().Add(testDeadline)
	for a.read("info") < base+3 && len(a.postBodies()) < 2 {
		if !time.Now().Before(deadline) {
			t.Fatalf("%d reads of info, %d posts; logs:\n%s", a.read("info"), len(a.postBodies()), h.logs.String())
		}
		time.Sleep(pollInterval)
	}
	if n := len(a.postBodies()); n != 1 {
		t.Fatalf("%d posts reached the server while the file was stale, want only the one that got no answer; logs:\n%s", n, h.logs.String())
	}
	if c := h.savedCursor(); c != "" {
		t.Fatalf("a cursor %q was saved before the delivery was shown", c)
	}

	// The file describes the server again.
	writeFakeService(t, path, a, "")
	posts := a.waitPosts(2)
	first, _, _ := post(t, posts[0].Body)
	if again, _, _ := post(t, posts[1].Body); again != first {
		t.Fatalf("the repeat was made under id %s, not the %s of the first post", again, first)
	}
	h.cursorPast("")
	time.Sleep(100 * time.Millisecond)
	if n := len(a.postBodies()); n != 2 {
		t.Fatalf("%d posts, want the one and its repeat", n)
	}
	if users := a.userTexts(); len(users) != 1 {
		t.Fatalf("the session holds %d copies of the delivery: %v", len(users), users)
	}
}

// TestTheWaitForAnAdmittedDeliveryReadsOnlyThroughAnAttachThatSucceeded: the
// delivery is in the inbox behind a turn, and the server goes away and comes back
// while its file is stale. No read of the message, the inbox or the active list is
// made between the read that failed and the attach that found the service; then the
// wait goes on, and the delivery commits.
func TestTheWaitForAnAdmittedDeliveryReadsOnlyThroughAnAttachThatSucceeded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode", "service.json")
	a := newFake(t)
	a.set(func() { a.busyOnPost = true })
	writeFakeService(t, path, a, "")
	h := newDiscoveryHarness(t, path)
	h.start()

	h.peerSend(peerNick, "behind a turn, across a restart with a stale file")
	a.waitPosts(1)
	a.waitReads("inbox", 2)

	addr := a.address()
	writeServiceJSON(t, path, "http://"+addr, "2.0.32", 999, "")
	a.stop()
	h.waitLog("the delivery is queued there, holding for the server")
	a.start(addr)
	// Two attaches that find the file stale, with the server up to be asked.
	base := a.read("info")
	a.waitReads("info", base+2)
	message, inbox, active := a.read("message"), a.read("inbox"), a.read("active")
	time.Sleep(100 * time.Millisecond)
	if m, i, ac := a.read("message"), a.read("inbox"), a.read("active"); m != message || i != inbox || ac != active {
		t.Fatalf("the wait read through a client whose attach failed: message %d to %d, inbox %d to %d, active %d to %d",
			message, m, inbox, i, active, ac)
	}
	if c := h.savedCursor(); c != "" {
		t.Fatalf("a cursor %q was saved before the delivery was shown", c)
	}

	// The file describes the server again: the reads resume, and the turn ends.
	writeFakeService(t, path, a, "")
	a.waitReads("message", message+1)
	a.set(func() { a.busyOnPost = false })
	a.release()
	h.cursorPast("")
	h.waitLog("OpenCode is back")
	if n := len(a.postBodies()); n != 1 {
		t.Fatalf("%d posts, want the one", n)
	}
	if users := a.userTexts(); len(users) != 1 {
		t.Fatalf("the session holds %d copies of the delivery: %v", len(users), users)
	}
}

// TestA5xxDuringTheWaitForAnAdmittedDeliveryIsHeldForAndNotFatal: the entry is in
// the session's inbox, and the service answers a 503 and then a 500 to a read of
// the wait (the message, the inbox or the list of active sessions) before it is
// well again. A stop here would have the next start post the delivery again under an
// id of its own, so the pump holds as for a server that is away: it says so once,
// attaches again before it reads again, and the delivery commits once.
func TestA5xxDuringTheWaitForAnAdmittedDeliveryIsHeldForAndNotFatal(t *testing.T) {
	for _, route := range []string{"message", "inbox", "active"} {
		t.Run(route, func(t *testing.T) {
			f := newFake(t)
			f.set(func() { f.busyOnPost = true })
			h := newHarness(t, f, nil)
			h.start()

			h.peerSend(peerNick, "behind a turn, with the service in trouble")
			f.waitPosts(1)
			f.waitReads("inbox", 2)
			infos := f.read("info")
			f.set(func() { f.failNext = map[string][]int{route: {503, 500}} })
			pending := func() int {
				f.mu.Lock()
				defer f.mu.Unlock()
				return len(f.failNext[route])
			}
			for deadline := time.Now().Add(testDeadline); pending() > 0; time.Sleep(pollInterval) {
				if !time.Now().Before(deadline) {
					t.Fatalf("the pump never made the reads that were to fail; logs:\n%s", h.logs.String())
				}
			}
			h.waitLog("OpenCode is back")
			select {
			case <-h.done:
				t.Fatalf("the pump stopped on a 5xx: %v\n%s", h.runErr, h.logs.String())
			default:
			}
			// Each of the two failed reads was followed by an attach before the next read.
			if got := f.read("info") - infos; got < 2 {
				t.Fatalf("%d attach(es) after two failed reads, want at least 2", got)
			}

			f.set(func() { f.busyOnPost = false })
			f.release()
			h.cursorPast("")
			logs := h.logs.String()
			if n := strings.Count(logs, "OpenCode is not answering"); n != 1 {
				t.Fatalf("the outage was said %d times, want once:\n%s", n, logs)
			}
			if n := strings.Count(logs, "OpenCode is back"); n != 1 {
				t.Fatalf("the return was said %d times, want once:\n%s", n, logs)
			}
			if !strings.Contains(logs, "503") {
				t.Fatalf("the log does not say what the service answered:\n%s", logs)
			}
			if n := len(f.postBodies()); n != 1 {
				t.Fatalf("%d posts, want the one", n)
			}
			if users := f.userTexts(); len(users) != 1 {
				t.Fatalf("the session holds %d copies of the delivery: %v", len(users), users)
			}
		})
	}
}

// TestA4xxOtherThanA404DuringTheWaitStillStopsThePump: only the server being away
// or in trouble is held for; a refusal of the request is the end of the wait, with
// nothing committed.
func TestA4xxOtherThanA404DuringTheWaitStillStopsThePump(t *testing.T) {
	f := newFake(t)
	f.set(func() { f.busyOnPost = true })
	h := newHarness(t, f, nil)
	h.start()

	h.peerSend(peerNick, "behind a turn")
	f.waitPosts(1)
	f.waitReads("inbox", 2)
	f.set(func() { f.failNext = map[string][]int{"message": {400}} })
	err := h.join()
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("Run returned %v, want the refusal", err)
	}
	if c := h.savedCursor(); c != "" {
		t.Fatalf("a cursor %q was saved", c)
	}
}

const interruptedLog = "the session was interrupted; holding deliveries until you send it something"

// interruptedSession is the history of a session whose turn the human stopped: the
// step the model was in is aborted, and the idle marker says interrupted.
func interruptedSession(f *fakeOpenCode) {
	f.addUser("the delivery the human interrupted")
	f.addAssistant("error")
	f.addIdle("interrupted")
}

// holding starts a pump over a session the human interrupted, with a delivery
// waiting in the stream, and returns once the pump has said it is holding and has
// looked again at the history a few times.
func holding(t *testing.T, f *fakeOpenCode) *harness {
	t.Helper()
	interruptedSession(f)
	h := newHarness(t, f, nil)
	h.start()
	h.peerSend(peerNick, "held behind the human's interrupt")
	h.waitLog(interruptedLog)
	reads := f.read("recent")
	f.waitReads("recent", reads+3)
	return h
}

// TestAnInterruptHoldsEveryDelivery: the session is idle on the list of active
// sessions, but the human stopped it and has sent it nothing since, so a post would
// start it again over their stop. Nothing is posted, the pump says so once however
// long it holds, and keeps looking.
func TestAnInterruptHoldsEveryDelivery(t *testing.T) {
	f := newFake(t)
	h := holding(t, f)
	if n := len(f.postBodies()); n != 0 {
		t.Fatalf("%d prompt(s) posted into a session the human interrupted", n)
	}
	if n := strings.Count(h.logs.String(), interruptedLog); n != 1 {
		t.Fatalf("the hold was said %d times, want once:\n%s", n, h.logs.String())
	}
	if c := h.savedCursor(); c != "" {
		t.Fatalf("a cursor %q was saved by a pump that delivered nothing", c)
	}
}

// TestAHumanPromptAndTheTurnThatEndedLetTheDeliveryGo: the human sends the session
// something, it runs, and its idle marker says succeeded: the interrupt is behind it.
func TestAHumanPromptAndTheTurnThatEndedLetTheDeliveryGo(t *testing.T) {
	f := newFake(t)
	h := holding(t, f)
	f.set(func() {
		f.msgs = append(f.msgs,
			fakeMsg{id: "msg_u_human", text: "carry on"},
			fakeMsg{id: "msg_a_human", kind: "assistant", finish: "stop"},
			fakeMsg{id: "msg_i_human", kind: "idle", outcome: "succeeded"})
	})
	posts := f.waitPosts(1)
	h.cursorPast("")
	if !strings.Contains(postedText(t, posts[0].Body), "held behind the human's interrupt") {
		t.Fatalf("the delivery is %q", postedText(t, posts[0].Body))
	}
	if n := strings.Count(h.logs.String(), interruptedLog); n != 1 {
		t.Fatalf("the hold was said %d times, want once", n)
	}
}

// TestANewerUserMessageAloneEndsTheHold: the human sent the session something and it
// has not finished (or has not yet begun) its turn: the stop is no longer the last
// word, whatever the marker before it says.
func TestANewerUserMessageAloneEndsTheHold(t *testing.T) {
	f := newFake(t)
	h := holding(t, f)
	f.addUser("carry on")
	f.waitPosts(1)
	h.cursorPast("")
}

// TestAnInterruptedMarkerBehindATurnThatWentThroughHoldsNothing: a marker that is
// not the newest one is history.
func TestAnInterruptedMarkerBehindATurnThatWentThroughHoldsNothing(t *testing.T) {
	f := newFake(t)
	interruptedSession(f)
	f.addUser("the human, after the interrupt")
	f.addAssistant("stop")
	f.addIdle("succeeded")
	h := newHarness(t, f, nil)
	h.start()
	h.peerSend(peerNick, "not held")
	f.waitPosts(1)
	h.cursorPast("")
	if strings.Contains(h.logs.String(), interruptedLog) {
		t.Fatalf("the pump held for a marker that was not the newest:\n%s", h.logs.String())
	}
}

// TestAServiceInTroubleDuringTheCheckForAnInterruptIsHeldFor: a 503 on the read of
// the history is the service starting, held for as a read of the idle wait is; the
// delivery goes once it is well, and nothing was posted meanwhile.
func TestAServiceInTroubleDuringTheCheckForAnInterruptIsHeldFor(t *testing.T) {
	f := newFake(t)
	f.set(func() { f.failNext = map[string][]int{"recent": {503, 500}} })
	h := newHarness(t, f, nil)
	h.start()
	h.peerSend(peerNick, "sent as the service came up")
	f.waitPosts(1)
	h.cursorPast("")
	if n := len(f.postBodies()); n != 1 {
		t.Fatalf("%d posts, want the one", n)
	}
	if !strings.Contains(h.logs.String(), "OpenCode is not answering") {
		t.Fatalf("the outage was not logged:\n%s", h.logs.String())
	}
}
