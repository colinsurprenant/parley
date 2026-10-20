package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/colinsurprenant/parley/internal/state"
	"github.com/colinsurprenant/parley/internal/stream"
	"github.com/colinsurprenant/parley/internal/stream/file"
)

// newHarness2 is newHarness for a pump that speaks to the fake OpenCode 2 server.
func newHarness2(t *testing.T, f *fakeOC2, tune func(*Config)) *harness {
	t.Helper()
	path := filepath.Join(t.TempDir(), "chat.ndjson")
	dir, err := state.Open(t.TempDir(), selfNick, "file:"+path)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	return newHarness2On(t, f.client(), path, dir, tune)
}

func newHarness2On(t *testing.T, c Client2, path string, dir *state.Dir, tune func(*Config)) *harness {
	t.Helper()
	s, err := file.Open(path, selfNick, file.WithPollInterval(pollInterval))
	if err != nil {
		t.Fatalf("file.Open: %v", err)
	}
	h := &harness{t: t, path: path, dir: dir, logs: &syncBuffer{}}
	cfg := Config{
		Deliverer: stream.NewDeliverer(s, selfNick),
		Cursors:   dir,
		Session:   testSession,
		Client2:   c,
		Timeout:   time.Minute,
		Log:       h.logs,
	}
	tuned(&cfg)
	if tune != nil {
		tune(&cfg)
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.pump = p
	return h
}

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
	f := newFake2(t)
	h := newHarness2(t, f, nil)
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
	f := newFake2(t)
	f.set(func() { f.manualBusy = true })
	h := newHarness2(t, f, nil)
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
	f := newFake2(t)
	f.set(func() { f.loseResponses = 2 })
	h := newHarness2(t, f, nil)
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
	f := newFake2(t)
	f.set(func() { f.loseResponses = 1 << 20 })
	h := newHarness2(t, f, func(c *Config) { c.ReadBackTimeout = 150 * time.Millisecond })
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
	f := newFake2(t)
	f.set(func() { f.lagReads = 1 })
	h := newHarness2(t, f, nil)
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
	f := newFake2(t)
	f.set(func() { f.lagReads = 2 })
	h := newHarness2(t, f, func(c *Config) { c.IdlePoll = 300 * time.Millisecond })
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
	f := newFake2(t)
	f.set(func() { f.parked = true })
	h := newHarness2(t, f, func(c *Config) { c.ReadBackTimeout = 150 * time.Millisecond })
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
	f := newFake2(t)
	f.set(func() { f.busyOnPost = true })
	h := newHarness2(t, f, func(c *Config) { c.ReadBackTimeout = 150 * time.Millisecond })
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
	f := newFake2(t)
	f.set(func() { f.cancelOnPost = true })
	h := newHarness2(t, f, nil)
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
	f := newFake2(t)
	f.set(func() { f.corrupt = true })
	h := newHarness2(t, f, nil)
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
	f := newFake2(t)
	f.set(func() { f.deleteOnPost, f.cancelOnPost = true, true })
	h := newHarness2(t, f, nil)
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

func TestTheSessionBeingAnotherVersionStopsThePump(t *testing.T) {
	f := newFake2(t)
	f.set(func() { f.version = "1.18.30" })
	h := newHarness2(t, f, nil)
	h.start()
	h.peerSend(peerNick, "to the wrong server")
	err := h.join()
	if err == nil || !strings.Contains(err.Error(), "not OpenCode 2") {
		t.Fatalf("Run returned %v", err)
	}
	if n := len(f.postBodies()); n != 0 {
		t.Fatalf("%d prompt(s) reached a server that is not OpenCode 2", n)
	}
}

// TestAServerThatAsksForAPasswordStopsThePumpWithoutLeakingIt: a 401 is the
// pump's own configuration to put right, so it stops with the advice, and the
// password that was sent is nowhere in what it says.
func TestAServerThatAsksForAPasswordStopsThePumpWithoutLeakingIt(t *testing.T) {
	f := newFake2(t)
	const secret = "s3cr3t-pa55-word"
	f.set(func() { f.password = secret })
	c, err := NewHTTPClient2(f.url(), Auth{User: ServiceUser, Password: "wrong-" + secret}, WithRequestTimeout(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "chat.ndjson")
	dir, err := state.Open(t.TempDir(), selfNick, "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness2On(t, c, path, dir, nil)
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

// spy2 wraps a Client2 to make a post fail the way a real one can, which a fake
// over a real listener cannot be made to do on demand.
type spy2 struct {
	Client2

	mu      sync.Mutex
	prompts int
	// promptErr, if set, is asked before each post with the number of the post; a
	// non-nil answer stands for it.
	promptErr func(call int) error
}

func (s *spy2) Prompt(ctx context.Context, session, id, text string) error {
	s.mu.Lock()
	s.prompts++
	n, hook := s.prompts, s.promptErr
	s.mu.Unlock()
	if hook != nil {
		if err := hook(n); err != nil {
			return err
		}
	}
	return s.Client2.Prompt(ctx, session, id, text)
}

// TestADeliveryThatNeverConnectedIsStartedOver: every attempt failed to connect, so
// nothing was sent; the pump goes back to waiting for the server, and the delivery
// goes over, under an id of its own, once.
func TestADeliveryThatNeverConnectedIsStartedOver(t *testing.T) {
	f := newFake2(t)
	dial := &TransportError{Op: "POST /api/session/x/prompt", Err: errors.New("connection refused"), Dial: true}
	s := &spy2{Client2: f.client(), promptErr: func(call int) error {
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
	h := newHarness2On(t, s, path, dir, nil)
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
	f := newFake2(t)
	reset := &TransportError{Op: "POST", Err: errors.New("connection reset")}
	dial := &TransportError{Op: "POST", Err: errors.New("connection refused"), Dial: true}
	s := &spy2{Client2: f.client(), promptErr: func(call int) error {
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
	h := newHarness2On(t, s, path, dir, nil)
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
// delivery applies to OpenCode 2 as to OpenCode 1, and what is posted is the
// rendering of the bounded delivery.
func TestADeliveryOverTheBoundGoesOverAsAStubTheReadBackConfirms(t *testing.T) {
	f := newFake2(t)
	h := newHarness2(t, f, nil)
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

// TestTheServerIsCheckedBeforeEveryDelivery: the version and the session are asked
// for again for each delivery, whatever the pump did in between.
func TestTheServerIsCheckedBeforeEveryDelivery(t *testing.T) {
	f := newFake2(t)
	h := newHarness2(t, f, nil)
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
	f := newFake2(t)
	f.set(func() { f.busyOnPost = true })
	h := newHarness2(t, f, nil)
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

func TestNewTakesOneClient(t *testing.T) {
	f := newFake2(t)
	g := newFake(t)
	path := filepath.Join(t.TempDir(), "chat.ndjson")
	dir, err := state.Open(t.TempDir(), selfNick, "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	s, err := file.Open(path, selfNick)
	if err != nil {
		t.Fatal(err)
	}
	base := Config{Deliverer: stream.NewDeliverer(s, selfNick), Cursors: dir, Session: testSession}
	both := base
	both.Client, both.Client2 = g.client(), f.client()
	if _, err := New(both); err == nil {
		t.Error("New accepted a client for each version")
	}
	if _, err := New(base); err == nil {
		t.Error("New accepted no client")
	}
	only2 := base
	only2.Client2 = f.client()
	if _, err := New(only2); err != nil {
		t.Errorf("New refused a client for OpenCode 2: %v", err)
	}
}

// TestAnInboxTooLargeToReadIsWaitedOutAndNeverCommitted: the inbox carries every
// queued entry's payload, attachments included, and the human's large queued prompt
// can pass the bound it is read under. Unread, it cannot say whether the delivery's
// entry is still there, so the pump does not call it cancelled and does not give up:
// it says so once and goes on asking for the message.
func TestAnInboxTooLargeToReadIsWaitedOutAndNeverCommitted(t *testing.T) {
	f := newFake2(t)
	// The entry is gone from the inbox, as a cancelled one is; the inbox cannot say.
	f.set(func() { f.inboxTooLong, f.cancelOnPost = true, true })
	h := newHarness2(t, f, func(c *Config) { c.ReadBackTimeout = 150 * time.Millisecond })
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
	f.set(func() { f.msgs = append(f.msgs, fake2Msg{id: id, text: text}) })
	h.cursorPast("")
}

// TestAPromptTheServerEchoesAsOtherTextIsSaidToBeAdmitted: after admission the entry
// is in the inbox and will probably be taken up as the server holds it. The pump
// stops, commits nothing, and says the entry is there and that a restart sends the
// delivery again under a new id.
func TestAPromptTheServerEchoesAsOtherTextIsSaidToBeAdmitted(t *testing.T) {
	f := newFake2(t)
	f.set(func() { f.echoOther = true })
	h := newHarness2(t, f, nil)
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
func writeFakeService(t *testing.T, path string, f *fakeOC2, password string) {
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
	b, err := json.Marshal(map[string]any{"id": "svc", "version": version, "url": url, "pid": pid, "password": password})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
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
	return newHarness2On(t, c, chat, dir, func(cfg *Config) { cfg.Discovery = d })
}

func (f *fakeOC2) setPassword(pw string) { f.set(func() { f.password = pw }) }

// TestTheServiceIsFoundAgainAfterItRestartsElsewhere: a restarted service has a new
// address, password and process id. The pump read the file once, at the start, and
// reads it again each time it attaches, so the next delivery goes to the new one.
func TestTheServiceIsFoundAgainAfterItRestartsElsewhere(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode", "service.json")
	a := newFake2(t)
	a.setPassword("password-of-a")
	writeFakeService(t, path, a, "password-of-a")
	h := newDiscoveryHarness(t, path)
	h.start()

	h.peerSend(peerNick, "to the first service")
	a.waitPosts(1)
	first := h.cursorPast("")

	a.stop()
	b := newFake2(t)
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
	a := newFake2(t)
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
	b := newFake2(t)
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
	a := newFake2(t)
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
	a := newFake2(t)
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
	f := newFake2(t)
	f.set(func() { f.busyOnPost = true })
	h := newHarness2(t, f, func(c *Config) { c.ReadBackTimeout = 100 * time.Millisecond })
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
