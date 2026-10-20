package opencode

import (
	"context"
	"encoding/json"
	"errors"
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
		t.Fatalf("%d steer request(s) for a delivery the session took up at once", n)
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
		t.Fatalf("a delivery that was only in the inbox for a moment was steered %d time(s)", n)
	}
}

// TestAnEntryTheSessionTakesUpWithinTheParkedPollsIsNotSteered: it takes two reads
// of an idle session, spaced by the idle poll, to call an entry parked, and an entry
// the session takes up before the second is left alone.
func TestAnEntryTheSessionTakesUpWithinTheParkedPollsIsNotSteered(t *testing.T) {
	f := newFake2(t)
	f.set(func() { f.lagReads = 2 })
	h := newHarness2(t, f, func(c *Config) { c.IdlePoll = 300 * time.Millisecond })
	h.start()

	h.peerSend(peerNick, "a session that is slow to start")
	f.waitPosts(1)
	h.cursorPast("")
	if n := len(f.patched()); n != 0 {
		t.Fatalf("an entry the session took up on its own was steered %d time(s)", n)
	}
}

// TestAParkedDeliveryIsChangedToSteerOnce: after an interrupt the queue is parked and nothing takes up an entry until it is steered. The entry is
// in the inbox with the session idle across two reads, and the pump changes it to
// steer, once, and says so.
func TestAParkedDeliveryIsChangedToSteerOnce(t *testing.T) {
	f := newFake2(t)
	f.set(func() { f.parked = true })
	h := newHarness2(t, f, nil)
	h.start()

	h.peerSend(peerNick, "into a parked session")
	f.waitPosts(1)
	h.cursorPast("")
	id, _, _ := post(t, f.postBodies()[0].Body)
	if got := f.patched(); len(got) != 1 || got[0] != id+` {"delivery":"steer"}` {
		t.Fatalf("steer requests %q, want one for %s", got, id)
	}
	if users := f.userTexts(); len(users) != 1 {
		t.Fatalf("the session holds %v", users)
	}
	if !strings.Contains(h.logs.String(), "changed it to steer") {
		t.Fatalf("the steer was not logged:\n%s", h.logs.String())
	}
	// The session is not parked for good by one steer: the next delivery waits for
	// two reads again and is steered again, for its own id.
	h.peerSend(peerNick, "and again")
	f.waitPosts(2)
	h.cursorPast(h.savedCursor())
	if got := f.patched(); len(got) != 2 {
		t.Fatalf("steer requests %q, want one per delivery", got)
	}
}

// TestADeliveryBehindAnotherTurnWaitsPastTheBoundOfTheReadBack: the human started
// a turn in the moment between the idle check and the post. The entry waits in the
// inbox behind it, which is not a failure and has no bound of its own: the read-back
// bound begins again while the session is seen busy.
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
		t.Fatalf("an entry behind a running turn was steered %d time(s)", n)
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

// TestASteerThatLosesTheRaceIsLookedAtAgain: the server answers a steer with 409,
// as it does for an entry that was taken up or cancelled since the read. The pump
// does not count that as the wake and looks again; the entry that is still parked
// is changed at the next look.
func TestASteerThatLosesTheRaceIsLookedAtAgain(t *testing.T) {
	f := newFake2(t)
	f.set(func() { f.parked, f.steerConflicts = true, 1 })
	h := newHarness2(t, f, nil)
	h.start()

	h.peerSend(peerNick, "steered into a conflict")
	f.waitPosts(1)
	h.cursorPast("")
	if got := f.patched(); len(got) != 2 {
		t.Fatalf("steer requests %q, want the refused one and the one that woke it", got)
	}
	if users := f.userTexts(); len(users) != 1 {
		t.Fatalf("the session holds %v", users)
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
