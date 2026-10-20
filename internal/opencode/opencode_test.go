package opencode

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/colinsurprenant/parley/internal/state"
	"github.com/colinsurprenant/parley/internal/stream"
	"github.com/colinsurprenant/parley/internal/stream/file"
)

const (
	selfNick = "oc-a"
	peerNick = "claude-b"
	// testDeadline bounds every wait for something the pump should have done.
	// It is generous: it exists so a wedged pump fails with a useful message
	// rather than a package-wide timeout, not to time anything.
	testDeadline = 10 * time.Second
	// pollInterval is the file adapter's change-detection interval under test.
	pollInterval = 5 * time.Millisecond
)

// syncBuffer is a log sink the pump writes to while the test reads it; the race
// detector is part of this package's test run.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// harness runs one Pump over a real file stream and a real state directory,
// against the fake OpenCode server through the real HTTP client: everything the
// production path does is done, and what reached the session is what the fake
// recorded.
type harness struct {
	t    *testing.T
	path string
	dir  *state.Dir
	logs *syncBuffer
	pump *Pump

	// done is closed once the pump has returned, and runErr is what it
	// returned; the close is what publishes it to the test goroutine.
	done   chan struct{}
	runErr error
}

// tuned shortens the pump's own clocks to what a test can wait out. A test that
// is about one of them overrides it.
func tuned(c *Config) {
	c.Debounce = 20 * time.Millisecond
	c.IdlePoll = 5 * time.Millisecond
	c.ReadBackPoll = 5 * time.Millisecond
	c.PostTimeout = 5 * time.Second
	c.MaxBackoff = 20 * time.Millisecond
}

func newHarness(t *testing.T, f *fakeOpenCode, tune func(*Config)) *harness {
	t.Helper()
	return newGatedHarness(t, f, tune)
}

// newGatedHarness is newHarness for a pump whose Deliverer holds what a table or
// a line holds back.
func newGatedHarness(t *testing.T, f *fakeOpenCode, tune func(*Config), gate ...stream.Option) *harness {
	t.Helper()
	path := filepath.Join(t.TempDir(), "chat.ndjson")
	dir, err := state.Open(t.TempDir(), selfNick, "file:"+path)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	return newHarnessOn(t, f.client(), path, dir, tune, gate...)
}

// newHarnessOn builds a pump over a stream and a state directory that already
// exist, which is what a restart after a failure looks like, and over any client.
func newHarnessOn(t *testing.T, c Client, path string, dir *state.Dir, tune func(*Config), gate ...stream.Option) *harness {
	t.Helper()
	s, err := file.Open(path, selfNick, file.WithPollInterval(pollInterval))
	if err != nil {
		t.Fatalf("file.Open: %v", err)
	}
	h := &harness{t: t, path: path, dir: dir, logs: &syncBuffer{}}
	cfg := Config{
		Deliverer: stream.NewDeliverer(s, selfNick, gate...),
		Cursors:   dir,
		Session:   testSession,
		Client:    c,
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

// start runs the pump until the test ends or the returned cancel is called, and
// joins it on the way out so a leaked pump fails its own test rather than a
// later one.
func (h *harness) start() context.CancelFunc {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.done = make(chan struct{})
	go func() {
		h.runErr = h.pump.Run(ctx)
		close(h.done)
	}()
	h.t.Cleanup(func() {
		cancel()
		h.join()
	})
	return cancel
}

// join waits for the pump to return and reports what it returned. It is safe to
// call more than once.
func (h *harness) join() error {
	h.t.Helper()
	select {
	case <-h.done:
	case <-time.After(testDeadline):
		h.t.Fatalf("the pump did not stop within %s; logs:\n%s", testDeadline, h.logs.String())
	}
	return h.runErr
}

// peerSend appends a message authored by nick, the way another participant's
// `parley send` would.
func (h *harness) peerSend(nick, body string) {
	h.t.Helper()
	s, err := file.Open(h.path, nick)
	if err != nil {
		h.t.Fatalf("file.Open for %s: %v", nick, err)
	}
	if _, err := s.Send(context.Background(), body); err != nil {
		h.t.Fatalf("send as %s: %v", nick, err)
	}
}

// cursorPast waits for the pump to persist a cursor that is neither empty nor
// prev, which is how a test waits for the write that follows a delivery.
func (h *harness) cursorPast(prev string) string {
	h.t.Helper()
	deadline := time.Now().Add(testDeadline)
	for {
		c, err := h.dir.LoadCursor()
		if err != nil {
			h.t.Fatalf("LoadCursor: %v", err)
		}
		if c != "" && c != prev {
			return c
		}
		if !time.Now().Before(deadline) {
			h.t.Fatalf("no cursor was saved within %s; logs:\n%s", testDeadline, h.logs.String())
		}
		time.Sleep(pollInterval)
	}
}

// savedCursor is the cursor on disk now, without waiting for one.
func (h *harness) savedCursor() string {
	h.t.Helper()
	c, err := h.dir.LoadCursor()
	if err != nil {
		h.t.Fatalf("LoadCursor: %v", err)
	}
	return c
}

// read is what a second reader of the same stream, under the same nick, collects
// from everything in it: the delivery the pump itself is looking at.
func (h *harness) read() *stream.Delivery {
	h.t.Helper()
	s, err := file.Open(h.path, selfNick)
	if err != nil {
		h.t.Fatalf("file.Open: %v", err)
	}
	d, err := stream.NewDeliverer(s, selfNick).Read(context.Background(), "")
	if err != nil {
		h.t.Fatalf("Read: %v", err)
	}
	return d
}

// render is the text the prompt has to equal, built by the same Render the pump
// calls.
func (h *harness) render() string {
	h.t.Helper()
	return h.read().Render()
}

// waitLog blocks until the pump's log holds want.
func (h *harness) waitLog(want string) {
	h.t.Helper()
	deadline := time.Now().Add(testDeadline)
	for !strings.Contains(h.logs.String(), want) {
		if !time.Now().Before(deadline) {
			h.t.Fatalf("the log never said %q; logs:\n%s", want, h.logs.String())
		}
		time.Sleep(pollInterval)
	}
}

// postedText is the text of a recorded prompt, parsed as the one shape the front is
// allowed to send.
func postedText(t *testing.T, body []byte) string {
	t.Helper()
	_, text, _ := post(t, body)
	return text
}

// waitActiveReads blocks until the pump has asked which sessions are in a turn n
// times, which for a session held busy is how a test knows the pump is in its wait
// for the turn.
func waitActiveReads(t *testing.T, f *fakeOpenCode, h *harness, n int) {
	t.Helper()
	deadline := time.Now().Add(testDeadline)
	for f.read("active") < n {
		if !time.Now().Before(deadline) {
			t.Fatalf("the pump never polled the active list %d times; logs:\n%s", n, h.logs.String())
		}
		time.Sleep(pollInterval)
	}
}

func TestSavesTheCursorAndDoesNotDeliverTwice(t *testing.T) {
	f := newFake(t)
	h := newHarness(t, f, nil)
	h.start()

	h.peerSend(peerNick, "first")
	f.waitPosts(1)
	c1 := h.cursorPast("")
	h.peerSend(peerNick, "second")
	posts := f.waitPosts(2)
	h.cursorPast(c1)

	second := postedText(t, posts[1].Body)
	if !strings.Contains(second, "second") || strings.Contains(second, "first") {
		t.Fatalf("the second delivery is\n%q\nwant the second message alone", second)
	}
	time.Sleep(100 * time.Millisecond)
	if n := len(f.postBodies()); n != 2 {
		t.Fatalf("%d prompts, want 2", n)
	}
	if first, _, _ := post(t, posts[0].Body); first == mustID(t, posts[1].Body) {
		t.Fatal("two deliveries were posted under one message id")
	}
}

func mustID(t *testing.T, body []byte) string {
	t.Helper()
	id, _, _ := post(t, body)
	return id
}

// TestTheNextDeliveryWaitsOutTheTurnTheLastOneStarted: a delivery starts a turn, and
// the session reads busy for a while after the message shows; the next delivery waits
// for that turn rather than being posted into it.
func TestTheNextDeliveryWaitsOutTheTurnTheLastOneStarted(t *testing.T) {
	f := newFake(t)
	f.set(func() { f.turnReads = 4 })
	h := newHarness(t, f, nil)
	h.start()

	h.peerSend(peerNick, "first")
	f.waitPosts(1)
	h.peerSend(peerNick, "second")
	posts := f.waitPosts(2)

	if posts[1].TurnRunning {
		t.Fatalf("the second delivery was posted while the first one's turn was running; logs:\n%s", h.logs.String())
	}
	if !strings.Contains(postedText(t, posts[1].Body), "second") {
		t.Fatalf("the second prompt is %q", postedText(t, posts[1].Body))
	}
}

// TestRedeliveryAfterACrashIsDeliveredAgain: a delivery the pump died before it
// saved the cursor for is posted again, as a new message under an id of its own.
// That is what at-least-once means.
func TestRedeliveryAfterACrashIsDeliveredAgain(t *testing.T) {
	f := newFake(t)
	f.set(func() { f.busyOnPost = true })
	h := newHarness(t, f, nil)
	cancel := h.start()

	h.peerSend(peerNick, "delivered, then the pump died before it saved the cursor")
	first := f.waitPosts(1)
	cancel()
	if err := h.join(); err != nil {
		t.Fatalf("Run returned %v on cancel, want nil", err)
	}
	if c := h.savedCursor(); c != "" {
		t.Fatalf("a cursor %q was saved by a pump that had not seen the message", c)
	}

	f.set(func() { f.busyOnPost = false })
	f.release()
	again := newHarnessOn(t, f.client(), h.path, h.dir, nil)
	again.start()
	posts := f.waitPosts(2)
	again.cursorPast("")
	want := again.render()
	if postedText(t, posts[1].Body) != want || postedText(t, first[0].Body) != want {
		t.Fatalf("the redelivery is\n%q\nwant\n%q", postedText(t, posts[1].Body), want)
	}
	if mustID(t, first[0].Body) == mustID(t, posts[1].Body) {
		t.Fatal("the redelivery was posted under the id of the first")
	}
}

func TestAnUnknownSessionStopsThePump(t *testing.T) {
	f := newFake(t)
	f.set(func() { f.exists = false })
	h := newHarness(t, f, nil)
	h.start()

	h.peerSend(peerNick, "anyone there")
	err := h.join()
	if !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("Run returned %v, want ErrSessionNotFound", err)
	}
	if c := h.savedCursor(); c != "" {
		t.Fatalf("a cursor %q was saved", c)
	}
}

// TestARefusedPostStopsThePumpAndIsNotRetried: a status the server answers a post
// with other than 200 is not a post that got no answer, and sending it again would
// not mend it.
func TestARefusedPostStopsThePumpAndIsNotRetried(t *testing.T) {
	f := newFake(t)
	srv := serving(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/info":
			writeJSON(w, 200, map[string]any{"version": "2.0.32", "pid": 7})
		case r.URL.Path == "/api/session/active":
			writeJSON(w, 200, map[string]any{"data": map[string]any{}})
		case strings.HasSuffix(r.URL.Path, "/prompt"):
			writeJSON(w, 500, map[string]any{"_tag": "Boom"})
		default:
			writeJSON(w, 200, map[string]any{"data": map[string]any{"id": testSession, "location": map[string]any{"directory": testDir}}})
		}
	})
	_ = f
	path := filepath.Join(t.TempDir(), "chat.ndjson")
	dir, err := state.Open(t.TempDir(), selfNick, "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	h := newHarnessOn(t, srv, path, dir, nil)
	h.start()

	h.peerSend(peerNick, "refused")
	err = h.join()
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 500 {
		t.Fatalf("Run returned %v, want a 500 StatusError", err)
	}
	if c := h.savedCursor(); c != "" {
		t.Fatalf("a cursor %q was saved", c)
	}
}

func TestAServerThatIsAwayIsWaitedForAndComesBack(t *testing.T) {
	f := newFake(t)
	h := newHarness(t, f, nil)
	h.start()

	addr := f.address()
	f.stop()
	h.peerSend(peerNick, "sent while the server was restarting")
	h.waitLog("OpenCode is not answering")
	// Several polls later it is still one line: it is logged once per outage.
	time.Sleep(200 * time.Millisecond)
	if n := strings.Count(h.logs.String(), "OpenCode is not answering"); n != 1 {
		t.Fatalf("the outage was logged %d times, want once:\n%s", n, h.logs.String())
	}
	if n := len(f.postBodies()); n != 0 {
		t.Fatalf("%d prompt(s) reached a server that was away", n)
	}

	f.start(addr)
	posts := f.waitPosts(1)
	if want := h.render(); postedText(t, posts[0].Body) != want {
		t.Fatalf("the delivery after the outage is\n%q\nwant\n%q", postedText(t, posts[0].Body), want)
	}
	h.cursorPast("")
	if !strings.Contains(h.logs.String(), "OpenCode is back") {
		t.Fatalf("the return was not logged:\n%s", h.logs.String())
	}
}

func TestAServerThatComesBackWithoutTheSessionStopsThePump(t *testing.T) {
	f := newFake(t)
	h := newHarness(t, f, nil)
	h.start()

	addr := f.address()
	f.stop()
	h.peerSend(peerNick, "sent while the server was restarting")
	h.waitLog("OpenCode is not answering")
	f.set(func() { f.exists = false })
	f.start(addr)

	if err := h.join(); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("Run returned %v, want ErrSessionNotFound", err)
	}
}

// TestAServerThatComesBackAsAnotherVersionStopsThePump: the server restarted on an
// install of the legacy 1.x, or of a version after 2.
func TestAServerThatComesBackAsAnotherVersionStopsThePump(t *testing.T) {
	for _, version := range []string{"1.18.30", "3.0.0"} {
		f := newFake(t)
		h := newHarness(t, f, nil)
		h.start()

		h.peerSend(peerNick, "first")
		f.waitPosts(1)
		first := h.cursorPast("")
		f.set(func() { f.version = version })
		h.peerSend(peerNick, "second")
		err := h.join()
		if err == nil || !strings.Contains(err.Error(), "the server reports OpenCode version") || !strings.Contains(err.Error(), "brew install opencode") {
			t.Fatalf("%s: Run returned %v, want the refusal of a version this front does not speak to", version, err)
		}
		if n := len(f.postBodies()); n != 1 {
			t.Fatalf("%s: %d prompts, want only the first", version, n)
		}
		if c := h.savedCursor(); c != first {
			t.Fatalf("%s: the cursor is %q, want it left at %q", version, c, first)
		}
	}
}

func TestCancelWhileWaitingForAnIdleSessionCommitsNothing(t *testing.T) {
	f := newFake(t)
	f.set(func() { f.manualBusy = true })
	h := newHarness(t, f, nil)
	cancel := h.start()

	h.peerSend(peerNick, "never delivered")
	waitActiveReads(t, f, h, 2)
	cancel()
	if err := h.join(); err != nil {
		t.Fatalf("Run returned %v on cancel, want nil", err)
	}
	if !strings.Contains(h.logs.String(), "shutting down") {
		t.Fatalf("the shutdown was not logged:\n%s", h.logs.String())
	}
	if c := h.savedCursor(); c != "" {
		t.Fatalf("a cursor %q was saved by a pump that delivered nothing", c)
	}
	if n := len(f.postBodies()); n != 0 {
		t.Fatalf("%d prompt(s) posted", n)
	}
}

// TestCancelAfterTheDeliveryShowedStillCommitsIt: the session holds the message, so
// it was delivered, and a shutdown after that must not send it again at the next
// start.
func TestCancelAfterTheDeliveryShowedStillCommitsIt(t *testing.T) {
	f := newFake(t)
	h := newHarness(t, f, nil)
	cancel := h.start()

	h.peerSend(peerNick, "delivered, then Ctrl-C")
	f.waitPosts(1)
	h.cursorPast("")
	cancel()
	if err := h.join(); err != nil {
		t.Fatalf("Run returned %v on cancel, want nil", err)
	}

	// The next start finds nothing to deliver.
	again := newHarnessOn(t, f.client(), h.path, h.dir, nil)
	again.start()
	time.Sleep(200 * time.Millisecond)
	if n := len(f.postBodies()); n != 1 {
		t.Fatalf("%d prompts after the restart, want the one from before", n)
	}
}

// cancelOnConfirm is a Client whose Message cancels the pump's context just before
// it returns the message it was asked for, when the session has it: the shutdown
// that lands between the read-back that confirms a delivery and the commit.
type cancelOnConfirm struct {
	Client
	mu     sync.Mutex
	cancel context.CancelFunc
	fired  bool
}

func (c *cancelOnConfirm) arm(cancel context.CancelFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cancel = cancel
}

func (c *cancelOnConfirm) canceled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fired
}

func (c *cancelOnConfirm) Message(ctx context.Context, session, id string) (UserMessage, bool, error) {
	msg, found, err := c.Client.Message(ctx, session, id)
	if err == nil && found {
		c.mu.Lock()
		cancel := c.cancel
		c.cancel, c.fired = nil, c.fired || cancel != nil
		c.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	}
	return msg, found, err
}

// TestCancelBetweenTheConfirmationAndTheCommitStillCommitsTheDelivery: the shutdown
// arrives with the read-back that proves the delivery, before the pump has
// committed it. The session holds the message, so it was delivered; the pump
// commits and saves the cursor all the same, and the next start does not post it
// again.
func TestCancelBetweenTheConfirmationAndTheCommitStillCommitsTheDelivery(t *testing.T) {
	f := newFake(t)
	cc := &cancelOnConfirm{Client: f.client()}
	path := filepath.Join(t.TempDir(), "chat.ndjson")
	dir, err := state.Open(t.TempDir(), selfNick, "file:"+path)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	h := newHarnessOn(t, cc, path, dir, nil)
	cc.arm(h.start())

	h.peerSend(peerNick, "delivered, and the shutdown came with the confirmation")
	if err := h.join(); err != nil {
		t.Fatalf("Run returned %v on cancel, want nil", err)
	}
	if !cc.canceled() {
		t.Fatalf("the context was never canceled at the confirmation; logs:\n%s", h.logs.String())
	}
	posts := f.postBodies()
	if len(posts) != 1 {
		t.Fatalf("%d prompts, want the one", len(posts))
	}
	if got, want := h.savedCursor(), string(h.read().Cursor); got == "" || got != want {
		t.Fatalf("the saved cursor is %q, want the end of the delivery %q", got, want)
	}

	// The next start finds nothing to deliver, and delivers what comes after.
	again := newHarnessOn(t, f.client(), path, dir, nil)
	again.start()
	again.peerSend(peerNick, "after the restart")
	posts = f.waitPosts(2)
	next := postedText(t, posts[1].Body)
	if !strings.Contains(next, "after the restart") || strings.Contains(next, "the shutdown came") {
		t.Fatalf("the delivery after the restart is\n%q\nwant the new message alone", next)
	}
	again.cursorPast(h.savedCursor())
	if n := len(f.postBodies()); n != 2 {
		t.Fatalf("%d prompts after the restart, want 2 in all", n)
	}
}

func TestAnEmptyDeliveryIsCommittedAndPostsNothing(t *testing.T) {
	f := newFake(t)
	h := newHarness(t, f, func(c *Config) { c.Timeout = 100 * time.Millisecond })
	h.peerSend(selfNick, "my own post, which I am not handed")
	h.start()

	h.cursorPast("")
	if n := len(f.postBodies()); n != 0 {
		t.Fatalf("%d prompt(s) posted for a delivery with nothing in it", n)
	}
}

func TestNewRefusesWhatItCannotRun(t *testing.T) {
	f := newFake(t)
	s, err := file.Open(filepath.Join(t.TempDir(), "chat.ndjson"), selfNick)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := state.Open(t.TempDir(), selfNick, "file:x")
	if err != nil {
		t.Fatal(err)
	}
	ok := Config{Deliverer: stream.NewDeliverer(s, selfNick), Cursors: dir, Session: testSession, Client: f.client()}
	if _, err := New(ok); err != nil {
		t.Fatalf("New of a good config: %v", err)
	}
	for name, mod := range map[string]func(*Config){
		"no deliverer":            func(c *Config) { c.Deliverer = nil },
		"no cursors":              func(c *Config) { c.Cursors = nil },
		"no client":               func(c *Config) { c.Client = nil },
		"no session":              func(c *Config) { c.Session = " " },
		"negative timeout":        func(c *Config) { c.Timeout = -1 },
		"negative debounce":       func(c *Config) { c.Debounce = -1 },
		"negative idle poll":      func(c *Config) { c.IdlePoll = -1 },
		"negative read-back poll": func(c *Config) { c.ReadBackPoll = -1 },
		"negative post bound":     func(c *Config) { c.PostTimeout = -1 },
		"negative backoff cap":    func(c *Config) { c.MaxBackoff = -1 },
	} {
		c := ok
		mod(&c)
		if _, err := New(c); err == nil {
			t.Errorf("New accepted a config with %s", name)
		}
	}
}

// TestCheckVersion: the policy of the front, which the pump, line add and doctor
// share. A 2 passes, a 1 is told where to get OpenCode 2, and anything else is
// refused.
func TestCheckVersion(t *testing.T) {
	for _, v := range []string{"2.0.32", "2.1.1", "2.0.0", "2", "2.0.0-rc.1", "2.0.32-beta.1"} {
		if err := CheckVersion(v); err != nil {
			t.Errorf("CheckVersion(%q) = %v, want nil", v, err)
		}
	}
	for _, v := range []string{"1.18.30", "1.19.3", "1.0.0", "1"} {
		err := CheckVersion(v)
		if err == nil || !strings.Contains(err.Error(), "legacy OpenCode 1") || !strings.Contains(err.Error(), "brew install opencode") || !strings.Contains(err.Error(), "@opencode/cli") {
			t.Errorf("CheckVersion(%q) = %v, want the refusal of OpenCode 1 with the way to OpenCode 2", v, err)
		}
	}
	for _, v := range []string{"3.1.4", "10.0.0", "0.9.1", "0", "0.0.0-dev-202610191200", "00.1.0"} {
		err := CheckVersion(v)
		if err == nil || !strings.Contains(err.Error(), "was verified on 2.x, not on a version") || !strings.Contains(err.Error(), "brew install opencode") {
			t.Errorf("CheckVersion(%q) = %v, want the refusal of a version other than 2", v, err)
		}
	}
	for _, v := range []string{"", "latest", "v2.0.32", "opencode v2.0.32", ".1.2", "+2.0.0", "-2.0.0", "99999999999999999999.0.0", "2 .0"} {
		err := CheckVersion(v)
		if err == nil || !strings.Contains(err.Error(), "cannot read an OpenCode version") {
			t.Errorf("CheckVersion(%q) = %v, want the unreadable-version refusal", v, err)
		}
	}
	// What a server says about itself is quoted bounded.
	err := CheckVersion(strings.Repeat("x", 10_000))
	if err == nil || len(err.Error()) > 600 {
		t.Errorf("CheckVersion of a long string = %v", err)
	}
}

func TestProbe(t *testing.T) {
	f := newFake(t)
	ctx := context.Background()
	if info, err := Probe(ctx, f.client(), testSession); err != nil || info.Version != "2.0.32" || info.PID != fakePID {
		t.Fatalf("Probe = %+v, %v", info, err)
	}
	if _, err := Probe(ctx, f.client(), "ses_000000000000000000000000AA"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("Probe of an unknown session = %v, want ErrSessionNotFound", err)
	}
	f.set(func() { f.version = "1.18.30" })
	if _, err := Probe(ctx, f.client(), testSession); err == nil || !strings.Contains(err.Error(), "legacy OpenCode 1") {
		t.Fatalf("Probe of another version = %v", err)
	}
	f.stop()
	if _, err := Probe(ctx, f.client(), testSession); !IsDial(err) {
		t.Fatalf("Probe of a server that is down = %v, want a dial error", err)
	}
}

// The re-read after a wait: the delivery is collected again from the cursor it
// began at, with the gates as they were before it. These are the pump's loop, not a
// server's, and hold for the queue as they did for any other delivery.

// TestTheReReadAfterABusyWaitDoesNotDoubleTheLineCount: the messages that arrive
// while a turn runs are collected again, from the state before, when it ends. If the
// count carried over, the first message would be counted twice and the third would
// trip a breaker of three.
func TestTheReReadAfterABusyWaitDoesNotDoubleTheLineCount(t *testing.T) {
	f := newFake(t)
	f.set(func() { f.manualBusy = true })
	h := newGatedHarness(t, f, nil, stream.WithLine("colin", 3, time.Hour))
	h.start()

	h.peerSend(peerNick, "one")
	waitActiveReads(t, f, h, 3)
	h.peerSend(peerNick, "two")
	h.peerSend(peerNick, "three")
	f.release()

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
// the first collection is held in the second, and is released by the chair's post as
// it would have been.
func TestTheReReadAfterABusyWaitKeepsAHeldMessageHeld(t *testing.T) {
	f := newFake(t)
	f.set(func() { f.manualBusy = true })
	h := newGatedHarness(t, f, nil, stream.WithLine("colin", 1, time.Hour))
	h.start()

	h.peerSend(peerNick, "one")
	waitActiveReads(t, f, h, 3)
	h.peerSend(peerNick, "two, over the cap of one")
	f.release()

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
// held until this seat has posted, and the re-read after the wait finds the seal as
// the first collection did, closed.
func TestTheReReadAfterABusyWaitKeepsATablesSealClosed(t *testing.T) {
	f := newFake(t)
	f.set(func() { f.manualBusy = true })
	h := newGatedHarness(t, f, nil, stream.WithTable("colin", 0))
	h.start()

	h.peerSend("colin", "chair opens")
	waitActiveReads(t, f, h, 3)
	h.peerSend(peerNick, "seat speaks too early")
	f.release()

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
