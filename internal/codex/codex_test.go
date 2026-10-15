package codex

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/colinsurprenant/parley/internal/state"
	"github.com/colinsurprenant/parley/internal/stream"
	"github.com/colinsurprenant/parley/internal/stream/file"
)

const (
	selfNick = "codex-a"
	peerNick = "claude-b"
	// thread is a stand-in for the session UUID the human hands the pump; it is
	// opaque to parley and must reach codex unchanged, which is what the argv
	// assertions check.
	thread = "0199a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b"
	// testDeadline bounds every wait for something the pump should have done.
	// It is generous: it exists so a wedged pump fails with a useful message
	// rather than a package-wide timeout, not to time anything.
	testDeadline = 10 * time.Second
	// pollInterval is the file adapter's change-detection interval under test.
	pollInterval = 5 * time.Millisecond
	// waitSlack is what a bounded wait is allowed on top of its own bound
	// before the test calls it unbounded. A loaded machine under -race is slow;
	// a wait that is not bounded at all is not slow but stuck, and overshoots
	// this by an order of magnitude.
	waitSlack = 5 * time.Second
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
// with the codex call itself replaced by a recording runner: everything below
// the exec is the production path, and what would have been run is the argv the
// assertions read.
type harness struct {
	t    *testing.T
	path string
	dir  *state.Dir
	logs *syncBuffer
	pump *Pump

	calls chan []string

	// done is closed once the pump has returned, and runErr is what it
	// returned; the close is what publishes it to the test goroutine.
	done   chan struct{}
	runErr error
}

func newHarness(t *testing.T, debounce, timeout time.Duration, run Runner) *harness {
	t.Helper()
	path := filepath.Join(t.TempDir(), "chat.ndjson")
	dir, err := state.Open(t.TempDir(), selfNick, "file:"+path)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	return newHarnessOn(t, path, dir, debounce, timeout, run)
}

// newHarnessOn builds a pump over a stream and a state directory that already
// exist, which is what a restart after a failure looks like.
func newHarnessOn(t *testing.T, path string, dir *state.Dir, debounce, timeout time.Duration, run Runner) *harness {
	t.Helper()
	s, err := file.Open(path, selfNick, file.WithPollInterval(pollInterval))
	if err != nil {
		t.Fatalf("file.Open: %v", err)
	}
	h := &harness{
		t: t, path: path, dir: dir, logs: &syncBuffer{},
		calls: make(chan []string, 8),
	}
	p, err := New(Config{
		Deliverer: stream.NewDeliverer(s, selfNick),
		Cursors:   dir,
		Thread:    thread,
		Timeout:   timeout,
		Debounce:  debounce,
		Log:       h.logs,
		// Recorded before the injected runner answers, so a call that fails is
		// still a call the test can see.
		Run: func(ctx context.Context, argv []string) error {
			h.calls <- append([]string(nil), argv...)
			if run == nil {
				return nil
			}
			return run(ctx, argv)
		},
	})
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
// call more than once, which is what lets a test join explicitly and still be
// joined again by the cleanup.
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

// nextCall returns the argv of the next codex invocation the pump made.
func (h *harness) nextCall() []string {
	h.t.Helper()
	select {
	case argv := <-h.calls:
		return argv
	case <-time.After(testDeadline):
		h.t.Fatalf("no codex call within %s; logs:\n%s", testDeadline, h.logs.String())
		return nil
	}
}

// noCall reports that the pump ran nothing for d. It is only ever used behind a
// positive assertion, so that a merely slow pump fails the test that follows
// rather than passing this one.
func (h *harness) noCall(d time.Duration) {
	h.t.Helper()
	select {
	case argv := <-h.calls:
		h.t.Fatalf("the pump made a codex call it should not have: %q", argv)
	case <-time.After(d):
	}
}

// savedCursor waits for the pump to persist a non-empty cursor and returns it.
func (h *harness) savedCursor() string { return h.cursorPast("") }

// cursorPast waits for the pump to persist a cursor that is neither empty nor
// prev, which is how a test waits for the write that follows a delivery: the
// call the runner recorded happens before the commit, not after it.
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

// read is what a second reader of the same stream, under the same nick, collects
// from everything in it: the delivery the pump itself is looking at, built from
// a Deliverer of the test's own.
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

// render is the text the pump's message argument has to equal for a delivery
// one call can carry, built by the same Render the pump calls.
func (h *harness) render() string {
	h.t.Helper()
	return h.read().Render()
}

// renderEach is the same for a delivery the pump had to split: one rendering per
// message, in stream order, each of them what one call of its own carries. It is
// Render over a delivery of one message, which is what the split path builds.
func renderEach(msgs []stream.Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		one := stream.Delivery{Messages: []stream.Message{m}}
		out[i] = one.Render()
	}
	return out
}

// markerCall is what the call carrying the gap marker of a split delivery has to
// equal: Render over a delivery that is the marker and nothing else, which is
// the marker line and its blank line.
func markerCall(cursor string) string {
	one := stream.Delivery{Gap: true, GapCursor: stream.Cursor(cursor)}
	return one.Render()
}

// gapped returns a harness whose first delivery carries a history gap, and the
// cursor that gap is named for: one message is written and a cursor saved past
// it, as a pump that had delivered and committed it would have left behind, and
// the file is then truncated under that cursor — the offset it names no longer
// ends the line it was taken against, which is what the file adapter reports as
// a history gap. What the test sends next is what the pump reads behind the
// marker, at offset zero of an empty file.
func gapped(t *testing.T, debounce time.Duration) (*harness, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "chat.ndjson")
	dir, err := state.Open(t.TempDir(), selfNick, "file:"+path)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	h := newHarnessOn(t, path, dir, debounce, time.Minute, nil)
	h.peerSend(peerNick, "the history that is about to be lost")
	lost := string(h.read().Cursor)
	if err := dir.SaveCursor(lost); err != nil {
		t.Fatalf("SaveCursor: %v", err)
	}
	if err := os.Truncate(path, 0); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	return h, lost
}

// bodyRenderingTo returns a body whose message renders to exactly n bytes when
// it is the first message on a stream — the length the bound is measured
// against, hit on the nose rather than approached. What a body costs above
// itself is measured and not counted out of Render's format: a one-byte probe
// is sent on a stream of its own and rendered by the code the pump renders
// with, and the difference is the header, the two-space indent and the blank
// line. It carries over to the message this body is sent as because the id a
// first message is given is the offset zero its line begins at whatever the body
// holds, and the timestamp Render stamps is fixed-width UTC — and the test that
// uses it checks the length it got rather than trusting this.
func bodyRenderingTo(t *testing.T, n int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "probe.ndjson")
	w, err := file.Open(path, peerNick)
	if err != nil {
		t.Fatalf("file.Open for the probe: %v", err)
	}
	if _, err := w.Send(context.Background(), "x"); err != nil {
		t.Fatalf("send the probe: %v", err)
	}
	r, err := file.Open(path, selfNick)
	if err != nil {
		t.Fatalf("file.Open to read the probe: %v", err)
	}
	d, err := stream.NewDeliverer(r, selfNick).Read(context.Background(), "")
	if err != nil {
		t.Fatalf("Read the probe: %v", err)
	}
	if len(d.Messages) != 1 {
		t.Fatalf("the probe stream holds %d messages, want 1", len(d.Messages))
	}
	overhead := len(renderEach(d.Messages)[0]) - 1
	if n <= overhead {
		t.Fatalf("a message renders to %d bytes around a body of one; %d is not reachable", overhead, n)
	}
	return strings.Repeat("x", n-overhead)
}

// wantStubFor checks the one call that stands in for a message no argv could
// carry: the message's own header line, byte for byte, over a notice that sits
// in body position — indented, where a header cannot hide — naming what the
// message rendered to, the bound it was over and the id it is still readable
// under, and nothing of the body itself. The length is the rendering's and not
// the body's, because the rendering is what was measured against the bound: a
// body inside the bound can render past it, and naming the body's length there
// would send a participant a number that is not the one that went over.
//
// The notice is spelled out here rather than formatted from withheldNotice, so
// that what this pins is the wording a peer reads and not just its arguments.
func wantStubFor(t *testing.T, stub string, m stream.Message) {
	t.Helper()
	header, body, ok := strings.Cut(stub, "\n")
	if !ok {
		t.Fatalf("the stub is a single line, so it carries no body:\n%q", stub)
	}
	rendered := renderEach([]stream.Message{m})[0]
	wantHeader, _, _ := strings.Cut(rendered, "\n")
	if header != wantHeader {
		t.Fatalf("the stub's header line is\n%q\nwant the message's own\n%q", header, wantHeader)
	}
	notice, _, _ := strings.Cut(body, "\n")
	wantNotice := fmt.Sprintf("  [parley codex] body withheld: this message renders to %d bytes, over the %d byte bound of one exec argument on this front; the full text stays in the stream at id=%s", len(rendered), maxArgBytes, m.ID)
	if notice != wantNotice {
		t.Fatalf("the stub's notice line is\n%q\nwant\n%q", notice, wantNotice)
	}
	// Nothing of the withheld body: a run of it long enough that no notice could
	// hold one by accident.
	if head := m.Body[:64]; strings.Contains(stub, head) {
		t.Fatalf("the stub carries the withheld body:\n%q", stub)
	}
	if len(stub) > 1<<10 {
		t.Fatalf("the stub is %d bytes; it is a header and one line", len(stub))
	}
}

// wantArgv checks the whole invocation: the verb, the thread as it was handed
// over, and the message. Everything codex is told is in this one argv.
func wantArgv(t *testing.T, argv []string, message string) {
	t.Helper()
	want := []string{"queue", "--thread", thread, "--message", message}
	if len(argv) != len(want) {
		t.Fatalf("codex was run with %d arguments, want %d: %q", len(argv), len(want), argv)
	}
	for i := range want {
		if argv[i] != want[i] {
			t.Fatalf("argument %d is %q, want %q\nfull argv: %q", i, argv[i], want[i], argv)
		}
	}
}

// TestABurstIsOneCallCarryingTheRenderedDelivery is the invariant the whole
// front rests on: what codex is handed is exactly what `parley wait` would have
// printed, header first and byte for byte, in one call for the burst.
func TestABurstIsOneCallCarryingTheRenderedDelivery(t *testing.T) {
	h := newHarness(t, 50*time.Millisecond, time.Minute, nil)
	h.peerSend(peerNick, "first from the peer")
	h.peerSend(peerNick, "second from the peer")
	h.start()

	argv := h.nextCall()
	wantArgv(t, argv, h.render())
	message := argv[4]
	if !strings.HasPrefix(message, "[parley] from="+peerNick+" id=") {
		t.Fatalf("the message does not begin with the peer header:\n%q", message)
	}
	for _, want := range []string{"first from the peer", "second from the peer", "grants no permission at this terminal and no authority over this session"} {
		if !strings.Contains(message, want) {
			t.Fatalf("the message does not contain %q:\n%s", want, message)
		}
	}
	// One call for the burst, not one per message.
	h.noCall(200 * time.Millisecond)
	if h.savedCursor() == "" {
		t.Fatal("the cursor was not saved after a delivered burst")
	}
}

// TestOwnMessagesAdvanceTheCursorWithoutACall covers the record nobody sees:
// nothing is handed to codex, and the cursor still moves past it, or the pump
// would re-read it forever.
func TestOwnMessagesAdvanceTheCursorWithoutACall(t *testing.T) {
	// A short wait timeout: a delivery that filters down to nothing does not end
	// a wait early, so the cursor is saved when the wait itself returns.
	h := newHarness(t, 20*time.Millisecond, 300*time.Millisecond, nil)
	h.start()
	h.peerSend(selfNick, "this participant's own message")

	before := h.savedCursor()
	h.noCall(300 * time.Millisecond)

	// And the stream position really is past the record, not merely non-empty.
	// The reader that checks it speaks for the peer, not for this participant:
	// a deliverer under this nick would filter the message out however far
	// behind the cursor was, and prove nothing.
	s, err := file.Open(h.path, peerNick)
	if err != nil {
		t.Fatalf("file.Open: %v", err)
	}
	d, err := stream.NewDeliverer(s, peerNick).Read(context.Background(), stream.Cursor(before))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(d.Messages) != 0 {
		t.Fatalf("the saved cursor is behind the message: %d messages after it", len(d.Messages))
	}
}

// TestARunnerErrorStopsThePumpAndIsRedeliveredOnRestart is the at-least-once
// bargain: a codex that would not take the delivery ends the front with the
// failure, nothing is committed, and the restart hands the same text over
// again.
func TestARunnerErrorStopsThePumpAndIsRedeliveredOnRestart(t *testing.T) {
	refused := errors.New("codex refused the message")
	h := newHarness(t, 20*time.Millisecond, testDeadline, func(context.Context, []string) error {
		return refused
	})
	h.peerSend(peerNick, "delivered twice")

	ctx, cancel := context.WithTimeout(context.Background(), testDeadline)
	defer cancel()
	err := h.pump.Run(ctx)
	if !errors.Is(err, refused) {
		t.Fatalf("the pump returned %v, want the runner's error", err)
	}
	if !strings.Contains(err.Error(), thread) {
		t.Fatalf("the error does not name the thread: %v", err)
	}
	first := h.nextCall()

	// Nothing was committed and nothing was saved: there is no cursor file at
	// all, so a restart begins where this run did.
	if c, lerr := h.dir.LoadCursor(); lerr != nil || c != "" {
		t.Fatalf("LoadCursor returned %q, %v after a failed delivery; want the empty cursor", c, lerr)
	}
	if _, serr := os.Stat(h.dir.CursorPath()); !errors.Is(serr, fs.ErrNotExist) {
		t.Fatalf("a cursor file was written for a delivery codex refused (stat err %v)", serr)
	}

	restarted := newHarnessOn(t, h.path, h.dir, 20*time.Millisecond, time.Minute, nil)
	restarted.start()
	again := restarted.nextCall()
	wantArgv(t, again, first[4])
	if !strings.Contains(again[4], "delivered twice") {
		t.Fatalf("the restart delivered something else:\n%s", again[4])
	}
	if restarted.savedCursor() == "" {
		t.Fatal("the cursor was not saved after the redelivery")
	}
}

// TestABodyHoldingANULIsDeliveredWithItReplaced covers a body parley accepts and
// that no argv can carry: NUL terminates an argv string, so a rendered message
// holding one could only ever be skipped. Render takes it out first — a body's
// control characters become '?' so that none of them can draw over the indent
// that frames a body — and the message is handed over like any other. The
// front's own NUL check is what is left for the rest of the rendered text.
func TestABodyHoldingANULIsDeliveredWithItReplaced(t *testing.T) {
	h := newHarness(t, 20*time.Millisecond, testDeadline, nil)
	h.start()

	h.peerSend(peerNick, "a body with a\x00NUL in the middle of it")
	argv := h.nextCall()
	if !strings.Contains(argv[4], "a body with a?NUL in the middle of it") {
		t.Fatalf("the NUL was not replaced in the delivered message:\n%q", argv[4])
	}
	if strings.ContainsRune(argv[4], 0) {
		t.Fatalf("the delivered message carries a NUL:\n%q", argv[4])
	}
	if logs := h.logs.String(); strings.Contains(logs, "skipping") {
		t.Fatalf("the delivery was skipped rather than handed over:\n%s", logs)
	}
}

// TestAMessageOverTheBoundIsStubbedAndTheBurstAroundItIsDelivered is what the
// split exists for. The debounce window joins a burst into one delivery, so a
// single message no argv can carry used to take every message beside it down
// with it; now the burst goes one message per call, the oversized one is stood
// in for by a stub carrying its own header, and what the cursor moves past is a
// thread that says by id what it is not showing.
func TestAMessageOverTheBoundIsStubbedAndTheBurstAroundItIsDelivered(t *testing.T) {
	// What the cursor file held at the moment of each call, recorded on the
	// pump's own goroutine: a read from this one could always be racing a pump
	// that has already finished, and what is being checked here is that nothing
	// was committed part-way through the split.
	cursorAtCall := make(chan string, 8)
	var h *harness
	h = newHarness(t, 50*time.Millisecond, time.Minute, func(context.Context, []string) error {
		c, err := h.dir.LoadCursor()
		if err != nil {
			return err
		}
		cursorAtCall <- c
		return nil
	})
	h.peerSend(peerNick, "ahead of the oversized one")
	h.peerSend(peerNick, strings.Repeat("x", maxArgBytes+1))
	h.peerSend(peerNick, "behind the oversized one")
	msgs := h.read().Messages
	if len(msgs) != 3 {
		t.Fatalf("the stream holds %d messages, want the 3 of the burst", len(msgs))
	}
	want := renderEach(msgs)
	h.start()

	// Three calls, in stream order: the two that fit carry their own rendering,
	// and the one that does not carries its stub.
	wantArgv(t, h.nextCall(), want[0])
	stub := h.nextCall()
	// The argv around the stub is the argv around any other call; what is in the
	// message argument is the stub's own assertion, below.
	wantArgv(t, stub, stub[4])
	wantStubFor(t, stub[4], msgs[1])
	wantArgv(t, h.nextCall(), want[2])
	// Three and no more: the batch was not re-delivered behind the split.
	h.noCall(200 * time.Millisecond)
	cursor := h.savedCursor()

	// Committed once, after the last of the three: every call saw an empty
	// cursor file, so nothing was handed off as committed part-way through.
	for i := 1; i <= 3; i++ {
		select {
		case c := <-cursorAtCall:
			if c != "" {
				t.Fatalf("the cursor was %q before call %d of the split; the batch commits once, at the end", c, i)
			}
		default:
			t.Fatalf("call %d recorded no cursor", i)
		}
	}

	// And the pump goes on: the delivery behind the split is an ordinary one.
	h.peerSend(peerNick, "a later message, delivered whole")
	later := h.nextCall()
	if !strings.Contains(later[4], "a later message, delivered whole") {
		t.Fatalf("the message behind the split was not delivered:\n%s", later[4])
	}
	h.cursorPast(cursor)

	logs := h.logs.String()
	if strings.Contains(logs, "skipping") {
		t.Fatalf("a message was logged as skipped though every one of them was delivered:\n%s", logs)
	}
	if !strings.Contains(logs, "one message per call") {
		t.Fatalf("the split was not logged with its reason:\n%s", logs)
	}
	if !strings.Contains(logs, "as a stub") || !strings.Contains(logs, "id="+msgs[1].ID) {
		t.Fatalf("the stubbed message was not logged as delivered as a stub:\n%s", logs)
	}
}

// TestASingleMessageOverTheBoundIsDeliveredAsAStub is the same rule with no
// burst around it: the message is alone in its delivery, so the split has one
// call to make and the stub is the whole of it. Nothing about it is a skip —
// the cursor moves past a thread that was told what it is not seeing.
func TestASingleMessageOverTheBoundIsDeliveredAsAStub(t *testing.T) {
	h := newHarness(t, 20*time.Millisecond, time.Minute, nil)
	h.peerSend(peerNick, strings.Repeat("x", maxArgBytes+1))
	msgs := h.read().Messages
	if len(msgs) != 1 {
		t.Fatalf("the stream holds %d messages, want 1", len(msgs))
	}
	h.start()

	stub := h.nextCall()
	wantArgv(t, stub, stub[4])
	wantStubFor(t, stub[4], msgs[0])
	h.noCall(200 * time.Millisecond)
	cursor := h.savedCursor()

	h.peerSend(peerNick, "the message behind it")
	behind := h.nextCall()
	if !strings.Contains(behind[4], "the message behind it") {
		t.Fatalf("the message behind the stubbed one was not delivered:\n%s", behind[4])
	}
	h.cursorPast(cursor)
	if logs := h.logs.String(); strings.Contains(logs, "skipping") {
		t.Fatalf("the stubbed message was logged as skipped:\n%s", logs)
	}
}

// TestTheStubNamesTheRenderedLengthAndNotTheBodys is the gap between the two
// lengths, which is the header and the indent. The body here is inside the
// bound and only its rendering is over it, so a notice built from len(m.Body)
// would name a number that fits — telling a participant their message was
// withheld for exceeding a bound it does not exceed. What was measured is what
// is named.
func TestTheStubNamesTheRenderedLengthAndNotTheBodys(t *testing.T) {
	h := newHarness(t, 20*time.Millisecond, time.Minute, nil)
	h.peerSend(peerNick, strings.Repeat("x", maxArgBytes-50))
	msgs := h.read().Messages
	if len(msgs) != 1 {
		t.Fatalf("the stream holds %d messages, want 1", len(msgs))
	}
	// The premise, checked rather than assumed: the body fits and the rendering
	// of it does not, so the two lengths the notice could name differ.
	body, rendered := len(msgs[0].Body), len(renderEach(msgs)[0])
	if body > maxArgBytes {
		t.Fatalf("the body is %d bytes, over the %d bound: the test drives the ordinary stub", body, maxArgBytes)
	}
	if rendered <= maxArgBytes {
		t.Fatalf("the message renders to %d bytes, inside the %d bound: nothing is stubbed", rendered, maxArgBytes)
	}
	h.start()

	stub := h.nextCall()
	wantArgv(t, stub, stub[4])
	wantStubFor(t, stub[4], msgs[0])
	// And the body's own length is nowhere in it: wantStubFor pins the whole
	// notice line, but this is the regression by name.
	if strings.Contains(stub[4], strconv.Itoa(body)) {
		t.Fatalf("the stub names the body's %d bytes rather than the rendered %d:\n%s", body, rendered, stub[4])
	}
	h.noCall(200 * time.Millisecond)
	h.cursorPast("")
	if logs := h.logs.String(); strings.Contains(logs, "skipping") {
		t.Fatalf("the stubbed message was logged as skipped:\n%s", logs)
	}
}

// TestABurstOverTheBoundIsDeliveredOneMessagePerCall is the case the old rule
// got most wrong: neither message is anywhere near the bound, and it is only
// the debounce window joining them that put the delivery over it. Both are
// delivered, one call each, and nothing is stubbed or skipped.
func TestABurstOverTheBoundIsDeliveredOneMessagePerCall(t *testing.T) {
	h := newHarness(t, 50*time.Millisecond, time.Minute, nil)
	h.peerSend(peerNick, strings.Repeat("y", maxArgBytes/2))
	h.peerSend(peerNick, strings.Repeat("z", maxArgBytes/2))
	msgs := h.read().Messages
	if len(msgs) != 2 {
		t.Fatalf("the stream holds %d messages, want the 2 of the burst", len(msgs))
	}
	want := renderEach(msgs)
	// The premise, checked rather than assumed: each one fits in a call and the
	// two of them together do not.
	if joint := len(h.render()); joint <= maxArgBytes {
		t.Fatalf("the burst renders to %d bytes, which is inside the %d bound: the test drives nothing", joint, maxArgBytes)
	}
	for i, w := range want {
		if len(w) > maxArgBytes {
			t.Fatalf("message %d renders to %d bytes on its own, over the %d bound: the test drives the stub instead", i, len(w), maxArgBytes)
		}
	}
	h.start()

	wantArgv(t, h.nextCall(), want[0])
	wantArgv(t, h.nextCall(), want[1])
	h.noCall(200 * time.Millisecond)
	if h.savedCursor() == "" {
		t.Fatal("the cursor was not saved after a delivered split")
	}

	logs := h.logs.String()
	if strings.Contains(logs, "skipping") || strings.Contains(logs, "as a stub") {
		t.Fatalf("a burst of two carryable messages was stubbed or skipped:\n%s", logs)
	}
	if !strings.Contains(logs, "one message per call") {
		t.Fatalf("the split was not logged with its reason:\n%s", logs)
	}
}

// TestTheGapMarkerGetsACallOfItsOwnInASplitDelivery keeps the marker what it
// is: a statement about the delivery and not about any message in it. The
// single call it would have opened has become several, so it takes the first of
// them for itself and carries nothing else — delivered once, ahead of every
// message, exactly where a participant would have read it, and measured against
// the bound as nobody's message.
func TestTheGapMarkerGetsACallOfItsOwnInASplitDelivery(t *testing.T) {
	h, lost := gapped(t, 50*time.Millisecond)
	h.peerSend(peerNick, "the first message after the gap")
	h.peerSend(peerNick, strings.Repeat("x", maxArgBytes+1))
	msgs := h.read().Messages
	if len(msgs) != 2 {
		t.Fatalf("the stream holds %d messages after the truncation, want 2", len(msgs))
	}
	want := renderEach(msgs)
	h.start()

	// The marker and its blank line, and no message under it.
	wantArgv(t, h.nextCall(), markerCall(lost))
	// Then the messages behind it, each rendered as a delivery of one: the first
	// whole, the second — over the bound on its own — as its stub.
	wantArgv(t, h.nextCall(), want[0])
	stub := h.nextCall()
	wantArgv(t, stub, stub[4])
	wantStubFor(t, stub[4], msgs[1])
	if strings.Contains(stub[4], "[parley] gap:") {
		t.Fatalf("the gap marker was delivered a second time:\n%s", stub[4])
	}
	h.noCall(200 * time.Millisecond)
	if h.savedCursor() == "" {
		t.Fatal("the cursor was not saved after a delivered split")
	}
}

// TestAMessageAtTheBoundIsDeliveredWholeUnderAGapMarker is what the marker's
// own call buys. This message renders to exactly maxArgBytes — the largest text
// a call can carry — and the delivery is over the bound only because the marker
// sits in front of it. A marker riding this message's call would measure the
// message with bytes that are not its own and stub a message that fits, under a
// notice naming a length the message does not have; on a call of its own it
// costs the message nothing and both are delivered whole.
func TestAMessageAtTheBoundIsDeliveredWholeUnderAGapMarker(t *testing.T) {
	h, lost := gapped(t, 50*time.Millisecond)
	h.peerSend(peerNick, bodyRenderingTo(t, maxArgBytes))
	msgs := h.read().Messages
	if len(msgs) != 1 {
		t.Fatalf("the stream holds %d messages after the truncation, want 1", len(msgs))
	}
	want := renderEach(msgs)
	// The premise, checked rather than assumed: the message is exactly at the
	// bound on its own, and it is the marker in front of it that puts the
	// delivery over.
	if len(want[0]) != maxArgBytes {
		t.Fatalf("the message renders to %d bytes, want exactly the %d bound", len(want[0]), maxArgBytes)
	}
	if joint := len(markerCall(lost)) + len(want[0]); joint <= maxArgBytes {
		t.Fatalf("the marker and the message render to %d bytes together, inside the %d bound: nothing is split", joint, maxArgBytes)
	}
	h.start()

	wantArgv(t, h.nextCall(), markerCall(lost))
	wantArgv(t, h.nextCall(), want[0])
	h.noCall(200 * time.Millisecond)
	if h.savedCursor() == "" {
		t.Fatal("the cursor was not saved after a delivered split")
	}
	logs := h.logs.String()
	if strings.Contains(logs, "as a stub") {
		t.Fatalf("a message at the bound was stubbed:\n%s", logs)
	}
	if strings.Contains(logs, "skipping") || strings.Contains(logs, "skipped") {
		t.Fatalf("something was skipped though the marker and the message both fit:\n%s", logs)
	}
}

// TestARunnerErrorPartWayThroughASplitRedeliversTheWholeBatch is the
// at-least-once bargain where a delivery costs several calls: the calls are not
// one transaction, so a codex that refuses the second of three stops the front
// with nothing committed, and the restart hands over all three — the first one
// again among them. A duplicate in the thread is the price of never losing the
// two behind it.
func TestARunnerErrorPartWayThroughASplitRedeliversTheWholeBatch(t *testing.T) {
	refused := errors.New("codex refused the message")
	// The runner is only ever called from the pump's goroutine, which is this
	// one: Run is driven here rather than started in the background, so the
	// counter needs nothing around it.
	calls := 0
	h := newHarness(t, 50*time.Millisecond, testDeadline, func(context.Context, []string) error {
		calls++
		if calls == 2 {
			return refused
		}
		return nil
	})
	h.peerSend(peerNick, strings.Repeat("x", maxArgBytes/3))
	h.peerSend(peerNick, strings.Repeat("y", maxArgBytes/3))
	h.peerSend(peerNick, strings.Repeat("z", maxArgBytes/3))
	msgs := h.read().Messages
	if len(msgs) != 3 {
		t.Fatalf("the stream holds %d messages, want the 3 of the burst", len(msgs))
	}
	want := renderEach(msgs)
	if joint := len(h.render()); joint <= maxArgBytes {
		t.Fatalf("the burst renders to %d bytes, which is inside the %d bound: nothing is split", joint, maxArgBytes)
	}

	ctx, cancel := context.WithTimeout(context.Background(), testDeadline)
	defer cancel()
	err := h.pump.Run(ctx)
	if !errors.Is(err, refused) {
		t.Fatalf("the pump returned %v, want the runner's error", err)
	}
	if !strings.Contains(err.Error(), thread) {
		t.Fatalf("the error does not name the thread: %v", err)
	}
	// Two calls and no third: the split stops where the failure was.
	wantArgv(t, h.nextCall(), want[0])
	wantArgv(t, h.nextCall(), want[1])
	h.noCall(100 * time.Millisecond)

	// Nothing was committed and nothing was saved: there is no cursor file at
	// all, so a restart begins where this run did.
	if c, lerr := h.dir.LoadCursor(); lerr != nil || c != "" {
		t.Fatalf("LoadCursor returned %q, %v after a failed split; want the empty cursor", c, lerr)
	}
	if _, serr := os.Stat(h.dir.CursorPath()); !errors.Is(serr, fs.ErrNotExist) {
		t.Fatalf("a cursor file was written for a split codex refused (stat err %v)", serr)
	}

	restarted := newHarnessOn(t, h.path, h.dir, 50*time.Millisecond, time.Minute, nil)
	restarted.start()
	for _, w := range want {
		wantArgv(t, restarted.nextCall(), w)
	}
	if restarted.savedCursor() == "" {
		t.Fatal("the cursor was not saved after the redelivery")
	}
}

// TestAnE2BIGStopsThePumpRatherThanSkippingTheDelivery is the other half of that
// rule. E2BIG is the kernel refusing the whole argv and the inherited
// environment together, not this message: the message here is a small one, well
// inside the bound, so what the kernel is refusing is the environment. Skipping
// it would silently drop this delivery and every delivery behind it for as long
// as that environment lives, so it is a failure — nothing committed, and the
// same text delivered again by the next run.
func TestAnE2BIGStopsThePumpRatherThanSkippingTheDelivery(t *testing.T) {
	// What os/exec really returns when the kernel refuses the argv.
	refused := &os.PathError{Op: "fork/exec", Path: "/usr/local/bin/codex", Err: syscall.E2BIG}
	h := newHarness(t, 20*time.Millisecond, testDeadline, func(context.Context, []string) error {
		return refused
	})
	h.peerSend(peerNick, "a small message an oversized environment leaves no room for")

	ctx, cancel := context.WithTimeout(context.Background(), testDeadline)
	defer cancel()
	err := h.pump.Run(ctx)
	if !errors.Is(err, syscall.E2BIG) {
		t.Fatalf("the pump returned %v, want an error wrapping E2BIG", err)
	}
	if !strings.Contains(err.Error(), thread) {
		t.Fatalf("the error does not name the thread: %v", err)
	}
	// The wording is the whole point of keeping E2BIG apart from every other
	// runner error: the message is not what has to shrink.
	if !strings.Contains(err.Error(), "environment") {
		t.Fatalf("the error does not say what has to be trimmed: %v", err)
	}
	if call := h.nextCall(); !strings.Contains(call[4], "a small message") {
		t.Fatalf("the call carried something else:\n%s", call[4])
	}

	// Nothing was committed and nothing was saved: there is no cursor file at
	// all, so a restart begins where this run did.
	if c, lerr := h.dir.LoadCursor(); lerr != nil || c != "" {
		t.Fatalf("LoadCursor returned %q, %v after an E2BIG; want the empty cursor", c, lerr)
	}
	if _, serr := os.Stat(h.dir.CursorPath()); !errors.Is(serr, fs.ErrNotExist) {
		t.Fatalf("a cursor file was written for a delivery the kernel refused (stat err %v)", serr)
	}
	if logs := h.logs.String(); strings.Contains(logs, "skipping a delivery") {
		t.Fatalf("an E2BIG was logged as a skipped delivery:\n%s", logs)
	}
}

// TestCancelingTheContextEndsThePumpCleanly is the shutdown path: a signal at
// the terminal is not a failure, and the verb exits 0 on it.
func TestCancelingTheContextEndsThePumpCleanly(t *testing.T) {
	h := newHarness(t, 20*time.Millisecond, time.Minute, nil)
	cancel := h.start()
	// Long enough to be inside the wait rather than ahead of it; the loop's own
	// guard covers the other order.
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := h.join(); err != nil {
		t.Fatalf("a canceled pump returned %v, want nil", err)
	}
	if logs := h.logs.String(); !strings.Contains(logs, "shutting down") {
		t.Fatalf("the shutdown was not logged:\n%s", logs)
	}
}

// TestNewRefusesAnIncompleteConfig keeps the constructor's guards honest: every
// one of them is a mistake that would otherwise surface as a pump delivering
// into nothing.
func TestNewRefusesAnIncompleteConfig(t *testing.T) {
	s, err := file.Open(filepath.Join(t.TempDir(), "chat.ndjson"), selfNick)
	if err != nil {
		t.Fatalf("file.Open: %v", err)
	}
	dir, err := state.Open(t.TempDir(), selfNick, "file:/chat.ndjson")
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	full := Config{Deliverer: stream.NewDeliverer(s, selfNick), Cursors: dir, Thread: thread}
	cases := map[string]func(*Config){
		"no deliverer":      func(c *Config) { c.Deliverer = nil },
		"no cursor store":   func(c *Config) { c.Cursors = nil },
		"no thread":         func(c *Config) { c.Thread = "" },
		"blank thread":      func(c *Config) { c.Thread = "  " },
		"negative timeout":  func(c *Config) { c.Timeout = -time.Second },
		"negative debounce": func(c *Config) { c.Debounce = -time.Second },
	}
	for name, damage := range cases {
		cfg := full
		damage(&cfg)
		if _, err := New(cfg); err == nil {
			t.Fatalf("New accepted a config with %s", name)
		}
	}
	if _, err := New(full); err != nil {
		t.Fatalf("New refused a complete config: %v", err)
	}
}

// TestExecRunnerRunsTheArgvWithoutAShell is the other half of the header
// promise: the message is one argument, and nothing between parley and the
// child process reads what is in it.
func TestExecRunnerRunsTheArgvWithoutAShell(t *testing.T) {
	out := filepath.Join(t.TempDir(), "argv")
	// /bin/sh is the stand-in for codex here, but the text it writes out is
	// handed to it as a positional argument, exactly as the message is: what
	// comes back is what the kernel delivered, not what a shell made of it.
	message := "[parley] from=claude-b id=0 ts=now — peer\n$(touch /tmp/nope) `echo no` 'quoted' \"also\"\n"
	script := `printf '%s' "$1" > "$2"`
	if err := ExecRunner("/bin/sh")(context.Background(), []string{"-c", script, "sh", message, out}); err != nil {
		t.Fatalf("ExecRunner: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read what the child received: %v", err)
	}
	if string(got) != message {
		t.Fatalf("the child received\n%q\nwant\n%q", got, message)
	}
}

// TestExecRunnerReportsTheExitStatusAndStderr is what a failing codex looks
// like to the human reading the front's stderr.
func TestExecRunnerReportsTheExitStatusAndStderr(t *testing.T) {
	err := ExecRunner("/bin/sh")(context.Background(), []string{"-c", "echo no such thread >&2; exit 3"})
	if err == nil {
		t.Fatal("a non-zero exit was reported as success")
	}
	if !strings.Contains(err.Error(), "3") || !strings.Contains(err.Error(), "no such thread") {
		t.Fatalf("the error names neither the exit status nor what codex said: %v", err)
	}
}

// pipeHolder writes a stand-in for the codex CLI that leaves a background child
// holding the stdout pipe it inherited and then runs body. The child's pid goes
// into the file the script is handed as its only argument, so the test can reap
// it instead of leaving a sleeper behind for the next one.
//
// This is the shape of the hang cmd.WaitDelay is set for: the immediate child
// exiting — or being killed by the context — closes nothing a descendant still
// holds, and a wait that reads those pipes to EOF waits for the descendant.
func pipeHolder(t *testing.T, body string) (bin, pidFile string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "codex")
	pidFile = filepath.Join(dir, "pid")
	script := "#!/bin/sh\n" +
		"sleep 30 &\n" +
		"printf '%s' \"$!\" > \"$1\"\n" +
		body + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatalf("write the fake codex: %v", err)
	}
	t.Cleanup(func() {
		b, err := os.ReadFile(pidFile)
		if err != nil {
			return
		}
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	return bin, pidFile
}

// waitForFile blocks until path exists or d has passed, and reports whether it
// appeared. It fails nothing itself because it is called from a goroutine of
// its own, where a t.Fatal would not end the test it belongs to.
func waitForFile(path string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(pollInterval)
	}
}

// TestExecRunnerReturnsWhenADescendantHoldsThePipes is the bound on a call that
// succeeded: codex exited 0 and left something behind holding the stdout it
// inherited, and the runner must return on the delivery rather than on whatever
// that descendant does next. The pump is one process holding one nick's lock,
// and a wait with no bound is that lock held for as long as a stray child lives.
func TestExecRunnerReturnsWhenADescendantHoldsThePipes(t *testing.T) {
	bin, pidFile := pipeHolder(t, "exit 0")

	start := time.Now()
	err := ExecRunner(bin)(context.Background(), []string{pidFile})
	elapsed := time.Since(start)

	// A queue call that exited 0 took the delivery, whatever it left holding a
	// pipe; reporting that as a failure would stop the pump and hand the thread
	// the same message twice.
	if err != nil {
		t.Fatalf("ExecRunner returned %v after a codex that exited 0, want nil", err)
	}
	if elapsed > waitDelay+waitSlack {
		t.Fatalf("ExecRunner took %s, want at most %s: the wait is not bounded by WaitDelay", elapsed, waitDelay+waitSlack)
	}
}

// TestExecRunnerReturnsPromptlyWhenTheCallIsCanceled is the same bound on the
// shutdown path: Ctrl-C at the pump's terminal kills the codex it is waiting
// on, and the pipes a descendant kept open must not turn that into a hang. The
// deadline path is the same code with a timer in place of the signal, which is
// why only one of the two is driven here — the other would cost 30 seconds.
func TestExecRunnerReturnsPromptlyWhenTheCallIsCanceled(t *testing.T) {
	// `wait` rather than a sleep of its own: the script must outlive the cancel
	// without leaving a second child behind, since killing the script orphans a
	// foreground child of its own and nothing has that one's pid.
	bin, pidFile := pipeHolder(t, "wait")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Cancel once the script has recorded its background child, not on a timer:
	// a cancel that lands before the fork kills a child holding nothing but its
	// own pipes, which is the easy case and not the one this test is about.
	spawned := make(chan bool, 1)
	go func() {
		spawned <- waitForFile(pidFile, testDeadline)
		cancel()
	}()

	start := time.Now()
	err := ExecRunner(bin)(ctx, []string{pidFile})
	elapsed := time.Since(start)

	if !<-spawned {
		t.Fatal("the fake codex never recorded a background child; the pipes were never held and the test proves nothing")
	}
	if err == nil {
		t.Fatal("a call the cancel cut short was reported as success")
	}
	if !strings.Contains(err.Error(), bin) {
		t.Fatalf("the error does not name the binary: %v", err)
	}
	// The caller's own cancel speaks for itself: the deadline wording is for the
	// other case, a codex that is simply not coming back.
	if strings.Contains(err.Error(), "no answer within") {
		t.Fatalf("a canceled call was reported as the call timeout: %v", err)
	}
	if elapsed > waitDelay+waitSlack {
		t.Fatalf("ExecRunner took %s after the cancel, want at most %s: the wait is not bounded by WaitDelay", elapsed, waitDelay+waitSlack)
	}
}

// TestExecRunnerRefusesAMissingBinary keeps the spawn failure a failure: it is
// the CLI's job to resolve the binary before the lock, and a runner handed one
// that is gone must not report success.
func TestExecRunnerRefusesAMissingBinary(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "codex")
	err := ExecRunner(missing)(context.Background(), []string{"queue"})
	if err == nil {
		t.Fatal("running a binary that does not exist was reported as success")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the error does not wrap the underlying one: %v", err)
	}
}

// TestAFailedHandOffLeavesTheTableGatesAsTheyWere: at a table the pump's Deliverer
// holds seat posts in memory, and a delivery that carried the posts an own post
// released is not committed when codex refuses it. The same Deliverer, read again
// from the cursor the pump last saved, must find those posts still held and
// release them again, or the failed hand-off would have lost them for good.
func TestAFailedHandOffLeavesTheTableGatesAsTheyWere(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chat.ndjson")
	dir, err := state.Open(t.TempDir(), selfNick, "file:"+path)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	s, err := file.Open(path, selfNick, file.WithPollInterval(pollInterval))
	if err != nil {
		t.Fatalf("file.Open: %v", err)
	}
	// One Deliverer for both runs, as a front that retried in process would have.
	d := stream.NewDeliverer(s, selfNick, stream.WithTable("colin", 0))
	build := func(run Runner, logs *syncBuffer) *Pump {
		p, err := New(Config{Deliverer: d, Cursors: dir, Thread: thread, Timeout: 100 * time.Millisecond,
			Debounce: 20 * time.Millisecond, Log: logs, Run: run})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return p
	}

	// The seat's seal is shut, so the peer's post is held, and the cursor is saved
	// past it as it is past anything read.
	peer, err := file.Open(path, peerNick)
	if err != nil {
		t.Fatalf("file.Open: %v", err)
	}
	if _, err := peer.Send(context.Background(), "held for the seal"); err != nil {
		t.Fatalf("send: %v", err)
	}
	refused := errors.New("codex refused the message")
	var refusedCalls syncBuffer
	failing := build(func(_ context.Context, argv []string) error {
		fmt.Fprintf(&refusedCalls, "%q\n", argv)
		return refused
	}, &syncBuffer{})
	ctx, cancel := context.WithTimeout(context.Background(), 3*testDeadline)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- failing.Run(ctx) }()
	deadline := time.Now().Add(testDeadline)
	var held string
	for held == "" {
		if held, err = dir.LoadCursor(); err != nil {
			t.Fatalf("LoadCursor: %v", err)
		}
		if held == "" && !time.Now().Before(deadline) {
			t.Fatal("the pump saved no cursor past the held post")
		}
		time.Sleep(pollInterval)
	}

	// The seat answers: the own post opens the seal, the held post is released
	// into a delivery, and codex refuses it.
	if _, err := s.Send(context.Background(), "my answer"); err != nil {
		t.Fatalf("send: %v", err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, refused) {
			t.Fatalf("the pump returned %v, want the runner's error", err)
		}
	case <-time.After(testDeadline):
		t.Fatalf("the pump did not fail within %s; calls: %s", testDeadline, refusedCalls.String())
	}
	if !strings.Contains(refusedCalls.String(), "held for the seal") {
		t.Fatalf("the refused call did not carry the released post: %s", refusedCalls.String())
	}
	if c, err := dir.LoadCursor(); err != nil || c != held {
		t.Fatalf("the cursor is %q (%v) after a refused delivery, want it still at %q", c, err, held)
	}

	// The retry reads from that cursor and is handed the same post.
	calls := make(chan []string, 4)
	retry := build(func(_ context.Context, argv []string) error {
		calls <- append([]string(nil), argv...)
		return nil
	}, &syncBuffer{})
	rctx, rcancel := context.WithCancel(context.Background())
	rdone := make(chan error, 1)
	go func() { rdone <- retry.Run(rctx) }()
	defer func() {
		rcancel()
		<-rdone
	}()
	select {
	case argv := <-calls:
		if !strings.Contains(argv[4], "held for the seal") {
			t.Fatalf("the retry delivered %q, want the held post", argv[4])
		}
	case <-time.After(testDeadline):
		t.Fatal("the retry delivered nothing: the refused delivery took the held post with it")
	}
}
