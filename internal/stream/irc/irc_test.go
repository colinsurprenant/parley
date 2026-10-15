package irc

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/colinsurprenant/parley/internal/stream"
)

// waitSlack is what a Wait is allowed past its own bound for scheduling: the
// bound is enforced by timers and a context deadline, and neither fires to the
// microsecond on a machine running several servers and the race detector.
const waitSlack = 50 * time.Millisecond

// The default history ring for a test that does not care how much is retained.
const testHistory = 2048

// msgidLen is how long an ergo msgid is: 26 characters of base32. The tests
// pin it because it is what a participant sees in the peer header, and a
// transport that stopped minting ids would otherwise look like one that is
// simply quiet.
const msgidLen = 26

func newStream(t *testing.T, srv *ergoServer, channel, nick string) *Stream {
	t.Helper()
	spec, err := ParseSpec("irc://" + srv.Addr + "/" + strings.TrimPrefix(channel, "#"))
	if err != nil {
		t.Fatalf("ParseSpec: %v", err)
	}
	// The real credential store, under the test's own state directory: what the
	// live tests exercise is what the CLI builds.
	s, err := Open(spec, nick, NewStateCredentials(srv.State, nick))
	if err != nil {
		t.Fatalf("Open as %s: %v", nick, err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// passwordPath is where the account password for this server and nick lives.
func passwordPath(srv *ergoServer, nick string) string {
	return NewStateCredentials(srv.State, nick).Location(srv.Addr)
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func mustSend(t *testing.T, s *Stream, body string) {
	t.Helper()
	if _, err := s.Send(testContext(t), body); err != nil {
		t.Fatalf("Send(%.30q): %v", body, err)
	}
}

func mustRead(t *testing.T, s *Stream, cursor stream.Cursor) ([]stream.Message, stream.Cursor) {
	t.Helper()
	msgs, next, err := s.Read(testContext(t), cursor)
	if err != nil {
		t.Fatalf("Read(%q): %v", string(cursor), err)
	}
	return msgs, next
}

// newTestConn is a connection over a socket the test owns, built the way dial
// builds one so that everything below the handshake — writing, reading lines,
// dispatching — behaves as it does on a real one.
func newTestConn(nc net.Conn) *conn {
	return &conn{
		nc:        nc,
		r:         bufio.NewReaderSize(nc, maxLineLen),
		addr:      "127.0.0.1:6667",
		nick:      "alpha",
		channel:   "#parley",
		caps:      make(map[string]string),
		histLimit: defaultHistLimit,
		maxBytes:  defaultMaxBytes,
		maxLines:  defaultMaxLines,
		wsem:      make(chan struct{}, 1),
		waiters:   make(map[string]*waiter),
		batchOf:   make(map[string]string),
		liveBatch: make(map[string]struct{}),
		done:      make(chan struct{}),
		arrival:   make(chan struct{}),
	}
}

// heldConn is the connection the stream is holding, taken under the same slot
// the adapter itself uses. A test that reaches for it is about what happens to
// a connection, which is the one thing no public method exposes.
func heldConn(t *testing.T, s *Stream) *conn {
	t.Helper()
	s.sem <- struct{}{}
	defer func() { <-s.sem }()
	if s.conn == nil {
		t.Fatal("the stream holds no connection")
	}
	return s.conn
}

func bodies(msgs []stream.Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.Body
	}
	return out
}

// TestSendThenRead is the whole of the adapter in one pass: a message goes out,
// comes back with the identity the server gave it, and the cursor is that
// identity.
func TestSendThenRead(t *testing.T) {
	srv := startErgo(t, testHistory)
	s := newStream(t, srv, fmtChannel(t), "alpha")

	before := time.Now().Add(-time.Second)
	mustSend(t, s, "hello from parley")
	msgs, cursor := mustRead(t, s, "")

	if len(msgs) != 1 {
		t.Fatalf("Read returned %d messages (%q), want 1", len(msgs), bodies(msgs))
	}
	m := msgs[0]
	if len(m.ID) != msgidLen {
		t.Errorf("ID = %q, want %d characters of server msgid", m.ID, msgidLen)
	}
	if m.Author != "alpha" {
		t.Errorf("Author = %q, want %q", m.Author, "alpha")
	}
	if m.Body != "hello from parley" {
		t.Errorf("Body = %q", m.Body)
	}
	if m.TS.Before(before) || m.TS.After(time.Now().Add(time.Second)) {
		t.Errorf("TS = %v, which is not within seconds of now (%v)", m.TS, time.Now())
	}
	if m.TS.Location() != time.UTC {
		t.Errorf("TS is in %v, want UTC", m.TS.Location())
	}
	if string(cursor) != m.ID {
		t.Errorf("cursor = %q, want the id of the last message (%q)", string(cursor), m.ID)
	}
}

// TestOrderAndIDsAcrossTwoParticipants checks the thing the transport is for:
// two processes writing to one channel come back in one order, the server's,
// with ids neither of them chose.
func TestOrderAndIDsAcrossTwoParticipants(t *testing.T) {
	srv := startErgo(t, testHistory)
	channel := fmtChannel(t)
	a := newStream(t, srv, channel, "alpha")
	b := newStream(t, srv, channel, "beta")

	want := []string{"a one", "b one", "a two", "b two"}
	for i, body := range want {
		if i%2 == 0 {
			mustSend(t, a, body)
		} else {
			mustSend(t, b, body)
		}
	}

	msgs, cursor := mustRead(t, b, "")
	if got := bodies(msgs); !equal(got, want) {
		t.Fatalf("Read returned %q, want %q", got, want)
	}
	authors := []string{"alpha", "beta", "alpha", "beta"}
	ids := make(map[string]bool)
	for i, m := range msgs {
		if m.Author != authors[i] {
			t.Errorf("message %d author = %q, want %q", i, m.Author, authors[i])
		}
		if ids[m.ID] {
			t.Errorf("message %d repeats the id %q", i, m.ID)
		}
		ids[m.ID] = true
	}

	// A second read from the position the first returned has nothing to say,
	// and says it without moving.
	more, next, err := b.Read(testContext(t), cursor)
	if err != nil {
		t.Fatalf("second Read: %v", err)
	}
	if len(more) != 0 {
		t.Errorf("second Read returned %q, want nothing", bodies(more))
	}
	if next != cursor {
		t.Errorf("second Read moved the cursor from %q to %q", string(cursor), string(next))
	}
}

// TestBodiesRoundTripWhole covers the shapes a body takes that an IRC line
// cannot hold on its own: longer than a line, more than one line, and lines
// that are empty.
func TestBodiesRoundTripWhole(t *testing.T) {
	srv := startErgo(t, testHistory)
	channel := fmtChannel(t)
	a := newStream(t, srv, channel, "alpha")
	b := newStream(t, srv, channel, "beta")

	bodies := []string{
		strings.Repeat("long", 1000),                                 // one 4000-byte line
		"first\n\nthird\nfourth\n",                                   // blank line and a trailing newline
		"before\n\nafter",                                            // a bare \n\n
		"héllo wörld — ünïcode ✅ that must not be cut",               // multi-byte runes
		strings.Repeat("x", 1000) + "\n" + strings.Repeat("y", 1000), // two long lines
	}
	for _, body := range bodies {
		mustSend(t, a, body)
	}

	msgs, _ := mustRead(t, b, "")
	if len(msgs) != len(bodies) {
		t.Fatalf("Read returned %d messages, want %d (one per Send)", len(msgs), len(bodies))
	}
	ids := make(map[string]bool)
	for i, m := range msgs {
		if m.Body != bodies[i] {
			t.Errorf("message %d did not round trip\n got %d bytes: %.60q…\nwant %d bytes: %.60q…", i, len(m.Body), m.Body, len(bodies[i]), bodies[i])
		}
		if ids[m.ID] {
			t.Errorf("message %d shares an id with an earlier one", i)
		}
		ids[m.ID] = true
	}
}

// TestSendRefusesWhatTheServerWillNotTake checks that a body the server would
// reject is refused before anything goes out, so a half-sent message never
// reaches the channel.
func TestSendRefusesWhatTheServerWillNotTake(t *testing.T) {
	srv := startErgo(t, testHistory)
	channel := fmtChannel(t)
	a := newStream(t, srv, channel, "alpha")
	mustSend(t, a, "a message that is fine")
	_, cursor := mustRead(t, a, "")

	tests := map[string]string{
		"empty":            "",
		"NUL byte":         "before\x00after",
		"carriage return":  "before\rafter",
		"over the maximum": strings.Repeat("x", 70000),
	}
	for name, body := range tests {
		if _, err := a.Send(testContext(t), body); err == nil {
			t.Errorf("Send of a body with %s was accepted", name)
		}
	}
	msgs, next, err := a.Read(testContext(t), cursor)
	if err != nil {
		t.Fatalf("Read after the refused sends: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("a refused Send still put %q in the channel", bodies(msgs))
	}
	if next != cursor {
		t.Errorf("the cursor moved from %q to %q with nothing sent", string(cursor), string(next))
	}
}

// TestHistoryGap is the case the cursor design exists for: a cursor whose
// message the server no longer retains. Ergo answers CHATHISTORY AFTER for an
// expired msgid with an empty batch and no failure — the same answer as
// "nothing new" — so the adapter confirms the cursor itself, and it is that
// confirmation this test pins.
func TestHistoryGap(t *testing.T) {
	// Three history items per channel: the JOIN and two messages fill it, so
	// anything older than the last three items is gone.
	srv := startErgo(t, 3)
	a := newStream(t, srv, fmtChannel(t), "alpha")

	mustSend(t, a, "message 1")
	_, first := mustRead(t, a, "")
	for i := 2; i <= 5; i++ {
		mustSend(t, a, fmt.Sprintf("message %d", i))
	}

	msgs, restart, err := a.Read(testContext(t), first)
	var gap *stream.HistoryGapError
	if !errors.As(err, &gap) {
		t.Fatalf("Read from an expired cursor returned (%q, %v), want a *stream.HistoryGapError", bodies(msgs), err)
	}
	if !errors.Is(err, stream.ErrHistoryGap) {
		t.Errorf("the gap error does not match stream.ErrHistoryGap: %v", err)
	}
	if gap.Cursor != first {
		t.Errorf("the gap names cursor %q, want %q", string(gap.Cursor), string(first))
	}
	if gap.Restart != "" {
		t.Errorf("Restart = %q, want the zero cursor: everything still retained is replayed", string(gap.Restart))
	}
	if restart != gap.Restart {
		t.Errorf("Read returned cursor %q but the gap says to restart at %q", string(restart), string(gap.Restart))
	}

	tail, cursor := mustRead(t, a, restart)
	if len(tail) == 0 {
		t.Fatal("the restart cursor returned nothing; the retained tail is lost")
	}
	if got := tail[len(tail)-1].Body; got != "message 5" {
		t.Errorf("the retained tail ends at %q, want %q", got, "message 5")
	}
	for _, m := range tail {
		if m.Body == "message 1" {
			t.Error("the restart replayed a message the server no longer retains")
		}
	}

	// The negative: a cursor at the latest message is not a gap, it is silence.
	more, next, err := a.Read(testContext(t), cursor)
	if err != nil {
		t.Fatalf("Read from the newest cursor: %v", err)
	}
	if len(more) != 0 {
		t.Errorf("Read from the newest cursor returned %q", bodies(more))
	}
	if next != cursor {
		t.Errorf("Read from the newest cursor moved it from %q to %q", string(cursor), string(next))
	}
}

// TestCursorAtTheOldestRetainedItem is the gap check's hardest case: the
// cursor names the oldest item the server still holds, and there is nothing
// after it. Nothing was lost, so nothing may be reported as lost.
func TestCursorAtTheOldestRetainedItem(t *testing.T) {
	// A ring of one: the message just sent is both the newest item and the
	// oldest, which is the case a history query anchored on it cannot see
	// around.
	srv := startErgo(t, 1)
	a := newStream(t, srv, fmtChannel(t), "alpha")
	mustSend(t, a, "the only message the server still holds")

	msgs, cursor := mustRead(t, a, "")
	if len(msgs) != 1 {
		t.Fatalf("Read returned %d messages, want 1", len(msgs))
	}
	more, next, err := a.Read(testContext(t), cursor)
	if err != nil {
		t.Fatalf("Read from a cursor that is the oldest retained item: %v", err)
	}
	if len(more) != 0 {
		t.Errorf("Read returned %q, want nothing", bodies(more))
	}
	if next != cursor {
		t.Errorf("the cursor moved from %q to %q with nothing to read", string(cursor), string(next))
	}
}

func TestWaitReturnsWhenAPeerSends(t *testing.T) {
	srv := startErgo(t, testHistory)
	channel := fmtChannel(t)
	a := newStream(t, srv, channel, "alpha")
	b := newStream(t, srv, channel, "beta")
	mustSend(t, a, "opening line")
	_, cursor := mustRead(t, a, "")

	const (
		timeout  = 5 * time.Second
		debounce = 200 * time.Millisecond
	)
	go func() {
		time.Sleep(150 * time.Millisecond)
		b.Send(context.Background(), "from beta")
	}()

	start := time.Now()
	msgs, next, err := a.Wait(testContext(t), cursor, timeout, debounce)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Body != "from beta" {
		t.Fatalf("Wait returned %q, want one message from beta", bodies(msgs))
	}
	if elapsed > 2*time.Second {
		t.Errorf("Wait took %v to notice a message sent after 150ms", elapsed)
	}
	if string(next) != msgs[0].ID {
		t.Errorf("cursor = %q, want the id of the message returned (%q)", string(next), msgs[0].ID)
	}
}

func TestWaitTimesOutQuietly(t *testing.T) {
	srv := startErgo(t, testHistory)
	a := newStream(t, srv, fmtChannel(t), "alpha")
	mustSend(t, a, "opening line")
	_, cursor := mustRead(t, a, "")

	const (
		timeout  = 600 * time.Millisecond
		debounce = 200 * time.Millisecond
	)
	start := time.Now()
	msgs, next, err := a.Wait(testContext(t), cursor, timeout, debounce)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("Wait on a quiet channel returned %q", bodies(msgs))
	}
	if next != cursor {
		t.Errorf("Wait moved the cursor from %q to %q with nothing to deliver", string(cursor), string(next))
	}
	if elapsed < timeout {
		t.Errorf("Wait returned after %v, before its %v timeout", elapsed, timeout)
	}
	if elapsed > timeout+debounce+waitSlack {
		t.Errorf("Wait took %v, past timeout+debounce", elapsed)
	}
}

// TestWaitCoalescesABurst checks both halves of the debounce: a stream of
// messages closer together than the quiet window comes back as one delivery,
// and the wait still ends inside its bound.
func TestWaitCoalescesABurst(t *testing.T) {
	srv := startErgo(t, testHistory)
	channel := fmtChannel(t)
	a := newStream(t, srv, channel, "alpha")
	b := newStream(t, srv, channel, "beta")
	mustSend(t, a, "opening line")
	_, cursor := mustRead(t, a, "")

	const (
		timeout  = 2 * time.Second
		debounce = 300 * time.Millisecond
		burst    = 5
	)
	go func() {
		for i := range burst {
			time.Sleep(120 * time.Millisecond)
			b.Send(context.Background(), fmt.Sprintf("burst %d", i))
		}
	}()

	start := time.Now()
	msgs, _, err := a.Wait(testContext(t), cursor, timeout, debounce)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if len(msgs) < 2 {
		t.Errorf("Wait returned %d messages (%q); a burst inside the quiet window should come back as one delivery", len(msgs), bodies(msgs))
	}
	if elapsed > timeout+debounce+waitSlack {
		t.Errorf("Wait took %v, past timeout+debounce even though the sender never paused", elapsed)
	}
}

// TestWaitAfterTheConnectionDiesStillBlocks: a wait whose connection had to be
// replaced must listen to the one that replaced it. Watching the dead
// connection's wake-up would end the wait at once — a gone connection releases
// everything waiting on it — and Wait would report an empty channel a fraction
// of a second after it was asked to wait for minutes.
func TestWaitAfterTheConnectionDiesStillBlocks(t *testing.T) {
	srv := startErgo(t, testHistory)
	channel := fmtChannel(t)
	a := newStream(t, srv, channel, "alpha")
	b := newStream(t, srv, channel, "beta")
	mustSend(t, b, "opening line")
	_, cursor := mustRead(t, a, "")

	c := heldConn(t, a)
	c.nc.Close()
	// Wait until the adapter has noticed, so that the wait below starts from a
	// connection it knows is gone rather than racing the discovery.
	deadline := time.Now().Add(5 * time.Second)
	for !c.isDead() {
		if !time.Now().Before(deadline) {
			t.Fatal("the adapter never noticed its connection had died")
		}
		time.Sleep(10 * time.Millisecond)
	}

	const peerSendsAfter = 700 * time.Millisecond
	go func() {
		time.Sleep(peerSendsAfter)
		b.Send(context.Background(), "from beta, after the wait re-dialed")
	}()

	start := time.Now()
	msgs, _, err := a.Wait(testContext(t), cursor, 6*time.Second, 200*time.Millisecond)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Wait after the connection died: %v", err)
	}
	if elapsed < peerSendsAfter {
		t.Fatalf("Wait returned after %v, before the peer had sent anything: it was watching a connection that was already gone", elapsed)
	}
	if len(msgs) != 1 || msgs[0].Body != "from beta, after the wait re-dialed" {
		t.Fatalf("Wait returned %q, want the message the peer sent", bodies(msgs))
	}

	// And the other order: the peer speaks while the connection is down, so
	// what the wait re-dials into already holds the message. It must be
	// delivered by the read that follows the re-dial rather than waited out.
	_, cursor, err = a.Read(testContext(t), cursor)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	c = heldConn(t, a)
	c.nc.Close()
	for !c.isDead() {
		time.Sleep(10 * time.Millisecond)
	}
	mustSend(t, b, "sent while alpha had no connection at all")

	start = time.Now()
	msgs, _, err = a.Wait(testContext(t), cursor, 6*time.Second, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("Wait after a message landed while the connection was down: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Body != "sent while alpha had no connection at all" {
		t.Fatalf("Wait returned %q, want the message sent while the connection was down", bodies(msgs))
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("Wait took %v to hand over a message that was already there", elapsed)
	}
}

func TestWaitUnblocksOnCancel(t *testing.T) {
	srv := startErgo(t, testHistory)
	a := newStream(t, srv, fmtChannel(t), "alpha")
	mustSend(t, a, "opening line")
	_, cursor := mustRead(t, a, "")

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, next, err := a.Wait(ctx, cursor, time.Minute, time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait after a cancel returned %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Wait took %v to notice the cancel", elapsed)
	}
	if next != cursor {
		t.Errorf("a cancelled Wait moved the cursor from %q to %q", string(cursor), string(next))
	}
}

// TestSendDuringWait checks that the adapter is one connection two goroutines
// can use: a Send has to get through while a Wait is blocked on the same
// socket, because that is exactly what the channel front does.
func TestSendDuringWait(t *testing.T) {
	srv := startErgo(t, testHistory)
	channel := fmtChannel(t)
	a := newStream(t, srv, channel, "alpha")
	b := newStream(t, srv, channel, "beta")
	mustSend(t, b, "opening line")
	_, cursor := mustRead(t, a, "")

	type waited struct {
		msgs []stream.Message
		err  error
	}
	done := make(chan waited, 1)
	start := time.Now()
	go func() {
		msgs, _, err := a.Wait(testContext(t), cursor, 5*time.Second, 200*time.Millisecond)
		done <- waited{msgs, err}
	}()
	time.Sleep(150 * time.Millisecond)
	if _, err := a.Send(testContext(t), "sent while waiting"); err != nil {
		t.Fatalf("Send while a Wait is blocked: %v", err)
	}
	got := <-done
	if got.err != nil {
		t.Fatalf("the blocked Wait failed: %v", got.err)
	}
	// Our own message is a message that landed, so the wait ends on it; it is
	// the Deliverer above that decides a participant is not told about its own.
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("Wait took %v to notice a message this adapter sent itself", elapsed)
	}
	if b := bodies(got.msgs); len(b) != 1 || b[0] != "sent while waiting" {
		t.Errorf("the blocked Wait returned %q, want the message sent during it", b)
	}
	msgs, _ := mustRead(t, b, "")
	if got := bodies(msgs); len(got) != 2 || got[1] != "sent while waiting" {
		t.Fatalf("the channel holds %q, want the message sent during the wait", got)
	}
}

// TestRedialsAfterTheConnectionDies: the socket is not the stream. A
// connection that goes away is re-dialed and re-registered by the next call
// that needs it, and the cursor carries across it because the ids are the
// server's, not the connection's.
func TestRedialsAfterTheConnectionDies(t *testing.T) {
	srv := startErgo(t, testHistory)
	channel := fmtChannel(t)
	a := newStream(t, srv, channel, "alpha")
	// Nobody else is in the channel, and nobody needs to be: the participant is
	// always-on, so it stays in the room between connections and the room keeps
	// its history. Before accounts, a channel emptied by a dropped connection
	// took its history with it and this read came back as a gap.
	mustSend(t, a, "before the connection died")
	msgs, cursor := mustRead(t, a, "")
	if len(msgs) != 1 {
		t.Fatalf("Read returned %d messages, want 1", len(msgs))
	}

	// Cut the socket from under the adapter, the way a server restart or a
	// dropped link would.
	c := heldConn(t, a)
	c.nc.Close()

	// The server needs a moment to notice the socket is gone and free the
	// nick; until it has, re-registering is refused as a nick in use, which is
	// the same answer a second process would get. Retrying is the caller's
	// business, and one retry per call is what the adapter promises.
	deadline := time.Now().Add(10 * time.Second)
	var err error
	for time.Now().Before(deadline) {
		if _, err = a.Send(testContext(t), "after the connection died"); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("Send after the connection died never succeeded: %v", err)
	}
	more, next, err := a.Read(testContext(t), cursor)
	if err != nil {
		t.Fatalf("Read after the redial: %v", err)
	}
	if got := bodies(more); len(got) != 1 || got[0] != "after the connection died" {
		t.Fatalf("Read after the redial returned %q, want the message sent after it", got)
	}
	if string(next) != more[0].ID {
		t.Errorf("cursor = %q, want the id of the message returned", string(next))
	}
}

// TestConcurrentReadsDoNotMix: two reads in flight at once are two batches on
// one socket, told apart only by the label each carries.
func TestConcurrentReadsDoNotMix(t *testing.T) {
	srv := startErgo(t, testHistory)
	channel := fmtChannel(t)
	a := newStream(t, srv, channel, "alpha")
	for i := range 8 {
		mustSend(t, a, fmt.Sprintf("message %d", i))
	}

	const readers = 6
	type result struct {
		bodies []string
		cursor stream.Cursor
		err    error
	}
	results := make(chan result, readers)
	for range readers {
		go func() {
			msgs, cursor, err := a.Read(context.Background(), "")
			results <- result{bodies(msgs), cursor, err}
		}()
	}
	var want []string
	for i := range readers {
		got := <-results
		if got.err != nil {
			t.Fatalf("concurrent Read: %v", got.err)
		}
		if i == 0 {
			want = got.bodies
			if len(want) != 8 {
				t.Fatalf("a Read returned %d messages, want 8", len(want))
			}
			continue
		}
		if !equal(got.bodies, want) {
			t.Fatalf("concurrent reads disagree\n one: %q\nother: %q", got.bodies, want)
		}
	}
}

// TestSendRefusedByTheServerIsReported: the server can say no to a message
// that is well formed and comes from a client that is joined and logged in —
// here because the human who owns the channel moderated it. Send has to come
// back with what the server said rather than report a message that is not
// there.
func TestSendRefusedByTheServerIsReported(t *testing.T) {
	srv := startErgo(t, testHistory)
	channel := fmtChannel(t)
	// The first client in a channel owns it, so the human's client is the one
	// that can moderate it.
	human := dialRaw(t, srv.Addr, "human", channel)
	human.send(t, "MODE "+channel+" +m")
	var moderated bool
	for _, l := range human.collect(t, 2*time.Second) {
		if l.command == "MODE" && strings.Contains(strings.Join(l.params, " "), "+m") {
			moderated = true
		}
	}
	if !moderated {
		t.Skip("the server did not moderate the channel; there is no refusal to observe")
	}

	a := newStream(t, srv, channel, "alpha")
	start := time.Now()
	_, err := a.Send(testContext(t), "a message into a channel that will not take it")
	if err == nil {
		t.Fatal("Send reported success for a message the server refused")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Send took %v to report the refusal, so it waited for a reply it had already been given", elapsed)
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("the error does not carry the server's numeric: %v", err)
	}
	t.Logf("the server refused it with: %v", err)
}

// TestASilentServerDoesNotHangARequest: a server that takes a request and
// never answers is the one failure a client cannot see, and `parley send`
// carries no deadline of its own. The adapter brings one.
func TestASilentServerDoesNotHangARequest(t *testing.T) {
	srv := startErgo(t, testHistory)
	a := newStream(t, srv, fmtChannel(t), "alpha")
	mustSend(t, a, "sent while the server was answering")

	was := replyTimeout
	replyTimeout = 300 * time.Millisecond
	defer func() { replyTimeout = was }()

	srv.pause(t)
	defer srv.resume(t)

	start := time.Now()
	_, err := a.Send(context.Background(), "sent into a server that will not answer")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("a send into a silent server reported success")
	}
	if elapsed > 5*time.Second {
		t.Errorf("the send took %v to give up, with a %v deadline", elapsed, replyTimeout)
	}
	for _, want := range []string{srv.Addr, "did not answer", replyTimeout.String()} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
}

// TestStockClientInterop is the human in the room: a client that negotiated no
// capabilities at all — the state every stock client is in until it asks for
// more — can say something the adapter reads as a message, and can read what
// the adapter sends as ordinary lines.
func TestStockClientInterop(t *testing.T) {
	srv := startErgo(t, testHistory)
	channel := fmtChannel(t)
	a := newStream(t, srv, channel, "alpha")
	mustSend(t, a, "opening line")
	_, cursor := mustRead(t, a, "")

	human := dialRaw(t, srv.Addr, "human", channel)
	human.send(t, "PRIVMSG "+channel+" :hello from a human")

	deadline := time.Now().Add(5 * time.Second)
	var msgs []stream.Message
	for time.Now().Before(deadline) {
		msgs, cursor = mustRead(t, a, cursor)
		if len(msgs) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(msgs) != 1 {
		t.Fatalf("the adapter read %d messages from the stock client, want 1", len(msgs))
	}
	m := msgs[0]
	if m.Author != "human" || m.Body != "hello from a human" {
		t.Errorf("read %q from %q, want %q from %q", m.Body, m.Author, "hello from a human", "human")
	}
	if len(m.ID) != msgidLen {
		t.Errorf("a stock client's message came back with the id %q, want a server msgid", m.ID)
	}
	if m.TS.IsZero() {
		t.Error("a stock client's message came back with no timestamp")
	}

	// And the other direction: what a multiline message looks like to someone
	// whose client never heard of draft/multiline — three plain lines, the
	// empty one included.
	human.drain(t, 200*time.Millisecond)
	mustSend(t, a, "first line\n\nthird line")
	seen := human.collect(t, 1500*time.Millisecond)
	var got []string
	for _, l := range seen {
		if l.command == "PRIVMSG" && strings.EqualFold(l.param(0), channel) {
			got = append(got, l.trailing())
		}
	}
	want := []string{"first line", "", "third line"}
	if !equal(got, want) {
		t.Fatalf("the stock client saw %q, want %q", got, want)
	}
}

// TestHistorySurvivesAnEmptyRoom: a message sent by a participant whose
// process then exits is still there for the next one. The participant is
// always-on, so its presence in the channel outlives its connection and the
// room is not destroyed under it — which is what lets a bare `parley send` be
// the whole of one side of a conversation.
func TestHistorySurvivesAnEmptyRoom(t *testing.T) {
	srv := startErgo(t, testHistory)
	channel := fmtChannel(t)
	a := newStream(t, srv, channel, "alpha")
	mustSend(t, a, "sent into a room with nobody watching")
	// QUIT and drop the socket, as the CLI does when the process ends: nothing
	// of alpha is connected from here on.
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	b := newStream(t, srv, channel, "beta")
	msgs, _ := mustRead(t, b, "")
	var found bool
	for _, m := range msgs {
		if m.Body == "sent into a room with nobody watching" && m.Author == "alpha" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the message did not survive its sender's exit; the channel holds %q", bodies(msgs))
	}
}

// TestSameNickOnTwoConnections is the reply path: the pump holds the nick on a
// long-lived connection blocked in Wait, and the send that answers it runs as
// the same participant from another process. Both are sessions of one account,
// so the server does not refuse the second — and the message the send posts
// reaches the pump like any other, with the participant as its author, for the
// Deliverer above to drop as its own.
func TestSameNickOnTwoConnections(t *testing.T) {
	srv := startErgo(t, testHistory)
	channel := fmtChannel(t)
	pump := newStream(t, srv, channel, "alpha")
	mustSend(t, pump, "the pump is here")
	_, cursor := mustRead(t, pump, "")

	type waited struct {
		msgs []stream.Message
		err  error
	}
	done := make(chan waited, 1)
	go func() {
		msgs, _, err := pump.Wait(testContext(t), cursor, 8*time.Second, 200*time.Millisecond)
		done <- waited{msgs, err}
	}()
	time.Sleep(200 * time.Millisecond)

	// A second process for the same participant, on its own connection, while
	// the first is blocked.
	reply := newStream(t, srv, channel, "alpha")
	start := time.Now()
	if _, err := reply.Send(testContext(t), "the reply, sent as the same participant"); err != nil {
		t.Fatalf("a second connection for the same nick was refused: %v", err)
	}
	got := <-done
	if got.err != nil {
		t.Fatalf("the pump's Wait failed: %v", got.err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("the pump took %v to see a message from its own participant's other session", elapsed)
	}
	if len(got.msgs) != 1 {
		t.Fatalf("the pump was handed %q, want the one reply", bodies(got.msgs))
	}
	m := got.msgs[0]
	if m.Body != "the reply, sent as the same participant" {
		t.Errorf("the pump read %q", m.Body)
	}
	if m.Author != "alpha" {
		t.Errorf("the reply is from %q, want alpha: it is the participant's own message, which the Deliverer drops", m.Author)
	}
	if len(m.ID) != msgidLen {
		t.Errorf("the reply has the id %q, want one server msgid", m.ID)
	}
}

// TestASecondSpellingOfARegisteredNickSendsAndIsDelivered: ergo renames a
// connection to the spelling its account was registered under, so the JOIN echo
// of "beta" arrives as "Beta". The registration must take that echo for its
// own, or a `send --nick beta` after `send --nick Beta` hangs at register until
// its deadline.
func TestASecondSpellingOfARegisteredNickSendsAndIsDelivered(t *testing.T) {
	srv := startErgo(t, testHistory)
	channel := fmtChannel(t)
	first := newStream(t, srv, channel, "Beta")
	mustSend(t, first, "from the spelling that registered the account")

	second := newStream(t, srv, channel, "beta")
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if _, err := second.Send(ctx, "from the other spelling"); err != nil {
		t.Fatalf("a second spelling of a registered nick could not send: %v", err)
	}

	peer := newStream(t, srv, channel, "alpha")
	msgs, _ := mustRead(t, peer, "")
	if got, want := bodies(msgs), []string{"from the spelling that registered the account", "from the other spelling"}; !equal(got, want) {
		t.Fatalf("the peer read %q, want %q", got, want)
	}
	for _, m := range msgs {
		if !stream.NickEqual(m.Author, "beta") {
			t.Errorf("the message %q is from %q, want a spelling of beta", m.Body, m.Author)
		}
	}
	// One participant: the second spelling kept the first one's password.
	if upper, lower := passwordPath(srv, "Beta"), passwordPath(srv, "beta"); upper != lower {
		t.Errorf("the two spellings keep their passwords apart: %s and %s", upper, lower)
	}
}

// TestAccountIsRegisteredOnceAndReused: the first connection for a participant
// makes its account; every one after it authenticates with what the first
// stored. Nobody types a password and nobody sees one.
func TestAccountIsRegisteredOnceAndReused(t *testing.T) {
	srv := startErgo(t, testHistory)
	channel := fmtChannel(t)
	first := newStream(t, srv, channel, "alpha")
	mustSend(t, first, "the first connection registered the account")

	path := passwordPath(srv, "alpha")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("no account password was stored at %s: %v", path, err)
	}
	if mode := fi.Mode().Perm(); mode != 0o600 {
		t.Errorf("the account password is mode %04o, want 0600", mode)
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the stored password: %v", err)
	}
	if len(strings.TrimSpace(string(stored))) != 2*passwordBytes {
		t.Errorf("the stored password is %d characters, want %d hex characters", len(strings.TrimSpace(string(stored))), 2*passwordBytes)
	}

	// A second connection for the same participant: it must authenticate with
	// the stored password rather than register again. A second REGISTER would
	// be refused by the server (the account exists), so a send that succeeds
	// and a password file untouched are together the proof it did not.
	second := newStream(t, srv, channel, "alpha")
	if _, err := second.Send(testContext(t), "the second connection reused it"); err != nil {
		t.Fatalf("a second connection could not authenticate: %v", err)
	}
	again, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat the password after the second connection: %v", err)
	}
	if !again.ModTime().Equal(fi.ModTime()) {
		t.Errorf("the password file was written again, so the second connection registered a second time")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-read the stored password: %v", err)
	}
	if string(after) != string(stored) {
		t.Errorf("the stored password changed under the second connection")
	}
}

// TestAccountWithoutItsPasswordIsADeadEnd: the account outlives the state
// directory that holds its password, and parley cannot guess it back. The
// error has to name the account and say how to be rid of it, because nothing
// else the participant can do will fix it.
func TestAccountWithoutItsPasswordIsADeadEnd(t *testing.T) {
	srv := startErgo(t, testHistory)
	channel := fmtChannel(t)
	first := newStream(t, srv, channel, "alpha")
	mustSend(t, first, "registering the account")
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	path := passwordPath(srv, "alpha")
	if err := os.Remove(path); err != nil {
		t.Fatalf("wipe the stored password: %v", err)
	}

	stranded := newStream(t, srv, channel, "alpha")
	_, err := stranded.Send(testContext(t), "this cannot work")
	if err == nil {
		t.Fatal("a connection with no password for an existing account was accepted")
	}
	for _, want := range []string{"alpha", "already exists", path, "UNREGISTER"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
	// The server refused the registration outright, which is the one answer
	// that proves the password just claimed names no account of ours: it is
	// dropped rather than left to be read back as the account's own.
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the password claimed for an account the server refused is still at %s (stat err %v)", path, err)
	}
}

// TestReservedNickRefusesAStranger: the account is what makes a participant's
// nick its own. A client that is not it — the human's, or anything else on the
// server — is refused the nick rather than given it.
func TestReservedNickRefusesAStranger(t *testing.T) {
	srv := startErgo(t, testHistory)
	channel := fmtChannel(t)
	a := newStream(t, srv, channel, "alpha")
	mustSend(t, a, "alpha is a registered account now")

	c, err := net.Dial("tcp", srv.Addr)
	if err != nil {
		t.Fatalf("dial as a stranger: %v", err)
	}
	defer c.Close()
	raw := &rawClient{c: c, r: bufio.NewReaderSize(c, maxLineLen+2)}
	raw.send(t, "NICK alpha")
	raw.send(t, "USER alpha 0 * :alpha")
	var refused bool
	for _, l := range raw.collect(t, 2*time.Second) {
		if l.command == "433" {
			refused = true
		}
		if l.command == "001" {
			t.Fatal("a stranger was given the participant's registered nick")
		}
	}
	if !refused {
		t.Error("a stranger asking for a registered nick was not refused with 433")
	}
}

// TestDelivererOverIRC runs the participant-side layer on top of the adapter,
// which is how every parley verb uses it: own messages suppressed, a peer's
// message under the header a participant reads.
func TestDelivererOverIRC(t *testing.T) {
	srv := startErgo(t, testHistory)
	channel := fmtChannel(t)
	a := newStream(t, srv, channel, "alpha")
	b := newStream(t, srv, channel, "beta")

	d := stream.NewDeliverer(a, "alpha")
	if _, err := d.Send(testContext(t), "mine, which I must not be told about"); err != nil {
		t.Fatalf("Deliverer.Send: %v", err)
	}
	mustSend(t, b, "yours, which I must see")

	delivery, err := d.Read(testContext(t), "")
	if err != nil {
		t.Fatalf("Deliverer.Read: %v", err)
	}
	if len(delivery.Messages) != 1 {
		t.Fatalf("the delivery holds %q, want only the peer's message", bodies(delivery.Messages))
	}
	m := delivery.Messages[0]
	if m.Author != "beta" {
		t.Fatalf("the delivery is from %q, want beta", m.Author)
	}
	rendered := delivery.Render()
	head := fmt.Sprintf("[parley] from=beta id=%s ts=%s — ", m.ID, m.TS.UTC().Format(time.RFC3339))
	if !strings.HasPrefix(rendered, head) {
		t.Fatalf("the rendered delivery does not open with the peer header\n got: %.120q\nwant prefix: %q", rendered, head)
	}
	if !strings.Contains(rendered, "yours, which I must see") {
		t.Errorf("the rendered delivery does not hold the message:\n%s", rendered)
	}
	if strings.Contains(rendered, "mine, which I must not be told about") {
		t.Errorf("the participant was delivered its own message:\n%s", rendered)
	}
}

// rawClient is a client with no capabilities at all: the view a stock IRC
// client has of the channel.
type rawClient struct {
	c net.Conn
	r *bufio.Reader
}

func dialRaw(t *testing.T, addr, nick, channel string) *rawClient {
	t.Helper()
	return dialRawCaps(t, addr, nick, channel)
}

// dialRawCaps is the same client with capabilities it asks for by name, for the
// one thing a client with none cannot do: send a message in a batch.
func dialRawCaps(t *testing.T, addr, nick, channel string, caps ...string) *rawClient {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial as a stock client: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	raw := &rawClient{c: c, r: bufio.NewReaderSize(c, maxLineLen+2)}
	if len(caps) > 0 {
		raw.send(t, "CAP LS 302")
	}
	raw.send(t, "NICK "+nick)
	raw.send(t, "USER "+nick+" 0 * :"+nick)
	if len(caps) > 0 {
		raw.send(t, "CAP REQ :"+strings.Join(caps, " "))
		raw.send(t, "CAP END")
	}
	raw.send(t, "JOIN "+channel)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, l := range raw.collect(t, 500*time.Millisecond) {
			if l.command == "JOIN" && nickFromPrefix(l.prefix) == nick {
				return raw
			}
		}
	}
	t.Fatalf("the stock client never joined %s", channel)
	return nil
}

func (r *rawClient) send(t *testing.T, s string) {
	t.Helper()
	if _, err := r.c.Write([]byte(s + "\r\n")); err != nil {
		t.Fatalf("stock client write: %v", err)
	}
}

// collect returns every line that arrives within d, answering PING so the
// server does not drop the connection under a slow test.
func (r *rawClient) collect(t *testing.T, d time.Duration) []line {
	t.Helper()
	r.c.SetReadDeadline(time.Now().Add(d))
	var out []line
	for {
		s, err := r.r.ReadString('\n')
		if s != "" {
			l, perr := parseLine(s)
			if perr == nil {
				if l.command == "PING" {
					r.send(t, "PONG "+l.trailing())
				}
				out = append(out, l)
			}
		}
		if err != nil {
			return out
		}
	}
}

func (r *rawClient) drain(t *testing.T, d time.Duration) {
	t.Helper()
	r.collect(t, d)
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestARequestCutShortByADeadConnectionFails: a connection that dies releases
// every request waiting on it, and what a request holds at that moment is half
// of an answer — here the opening line of an echo whose contents never came.
// Handed back as a result, that reads as a receipt for a message the server
// never took.
func TestARequestCutShortByADeadConnectionFails(t *testing.T) {
	srv := startErgo(t, testHistory)
	channel := fmtChannel(t)
	a := newStream(t, srv, channel, "alpha")
	mustSend(t, a, "sent while the connection was whole")

	c := heldConn(t, a)

	// Nothing will be answered from here on, so the send below is certain to be
	// waiting when the connection is cut.
	srv.pause(t)
	defer srv.resume(t)

	sent := make(chan error, 1)
	go func() { sent <- sendErr(a, context.Background(), "a message whose echo never arrives") }()

	label := awaitWaiter(t, c)
	// The first line of the echo, and nothing else: the batch it opens is never
	// closed because the connection dies first.
	echo, err := parseLine("@label=" + label + ";msgid=rzsj6pm4xkqvxz76xb7ymkqw6y :alpha!~alpha@localhost BATCH +9 draft/multiline " + channel)
	if err != nil {
		t.Fatalf("parseLine: %v", err)
	}
	c.dispatch(echo)
	c.nc.Close()

	select {
	case err := <-sent:
		if err == nil {
			t.Fatal("Send reported success on a connection that died before the server answered")
		}
		if !strings.Contains(err.Error(), "connection") {
			t.Errorf("the error does not say the connection ended: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Send never returned after its connection died")
	}
}

// awaitWaiter returns the label of the request in flight on c.
func awaitWaiter(t *testing.T, c *conn) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		for label := range c.waiters {
			c.mu.Unlock()
			return label
		}
		c.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no request was in flight")
	return ""
}

// TestTheAccountPasswordIsClaimedBeforeItIsRegistered: the password is on disk
// before the server is asked for the account, so a crash or a lost reply in
// between cannot leave an account nobody holds the password for. The proof is
// the other way round: with a store that refuses to keep it, the account is
// never created at all — a second connection with a working store can still
// register it.
func TestTheAccountPasswordIsClaimedBeforeItIsRegistered(t *testing.T) {
	srv := startErgo(t, testHistory)
	channel := fmtChannel(t)
	spec, err := ParseSpec("irc://" + srv.Addr + "/" + strings.TrimPrefix(channel, "#"))
	if err != nil {
		t.Fatalf("ParseSpec: %v", err)
	}
	refusing := &refusingClaim{Credentials: NewStateCredentials(srv.State, "alpha")}
	s, err := Open(spec, "alpha", refusing)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	if _, err := s.Send(testContext(t), "this cannot be sent"); err == nil {
		t.Fatal("Send succeeded with a credential store that refuses to keep anything")
	}
	if !refusing.tried {
		t.Fatal("the adapter never tried to claim a password")
	}

	// The account was never registered, because the claim comes first.
	working := newStream(t, srv, channel, "alpha")
	mustSend(t, working, "registered by the connection that could store its password")
	if _, err := os.Stat(passwordPath(srv, "alpha")); err != nil {
		t.Fatalf("no password was stored by the working connection: %v", err)
	}
}

// refusingClaim is a credential store that will not keep anything.
type refusingClaim struct {
	Credentials
	tried bool
}

func (r *refusingClaim) Claim(addr, password string) (bool, error) {
	r.tried = true
	return false, errors.New("this store keeps nothing")
}

// TestTwoFirstConnectionsShareOneAccount: a pump and the send that answers it
// can start at the same moment, neither having an account yet. The barrier
// below holds both at the point where they have read the credential store and
// found it empty, which is the race exactly: from there one must register and
// the other authenticate with what it stored. What must not happen is two
// registrations, or either being told to delete a healthy account.
func TestTwoFirstConnectionsShareOneAccount(t *testing.T) {
	srv := startErgo(t, testHistory)
	channel := fmtChannel(t)
	spec, err := ParseSpec("irc://" + srv.Addr + "/" + strings.TrimPrefix(channel, "#"))
	if err != nil {
		t.Fatalf("ParseSpec: %v", err)
	}
	creds := &rendezvous{Credentials: NewStateCredentials(srv.State, "alpha"), ready: make(chan struct{})}

	errs := make(chan error, 2)
	for i := range 2 {
		go func() {
			s, err := Open(spec, "alpha", creds)
			if err != nil {
				errs <- err
				return
			}
			defer s.Close()
			errs <- sendErr(s, context.Background(), fmt.Sprintf("from connection %d", i))
		}()
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("a connection racing for the same fresh account failed: %v", err)
		}
	}
	if !creds.raced {
		t.Fatal("the two connections did not reach the store together; the race was not exercised")
	}

	// One account, one password, and both messages in the channel.
	if _, err := os.Stat(passwordPath(srv, "alpha")); err != nil {
		t.Fatalf("no password was stored: %v", err)
	}
	b := newStream(t, srv, channel, "beta")
	msgs, _ := mustRead(t, b, "")
	if len(msgs) != 2 {
		t.Fatalf("the channel holds %q, want both messages", bodies(msgs))
	}
	for _, m := range msgs {
		if m.Author != "alpha" {
			t.Errorf("a message came from %q, want alpha", m.Author)
		}
	}
}

// rendezvous holds the first two readers of a credential store until both have
// read it, so that both see it empty. Later reads — the loser of the race
// coming back for the winner's password — pass straight through.
type rendezvous struct {
	Credentials
	mu    sync.Mutex
	n     int
	raced bool
	ready chan struct{}
}

func (r *rendezvous) Load(addr string) (string, error) {
	v, err := r.Credentials.Load(addr)
	r.mu.Lock()
	r.n++
	wait := r.n <= 2
	if r.n == 2 {
		r.raced = true
		close(r.ready)
	}
	r.mu.Unlock()
	if wait {
		<-r.ready
	}
	return v, err
}

// TestWaitUnderContinuousTrafficStillDelivers: a channel busier than the quiet
// window spends the whole wait in it, and the read that collects what arrived
// would have no time left to run in. A wait that came back empty from a busy
// channel would be the worst of both.
func TestWaitUnderContinuousTrafficStillDelivers(t *testing.T) {
	srv := startErgo(t, testHistory)
	channel := fmtChannel(t)
	a := newStream(t, srv, channel, "alpha")
	b := newStream(t, srv, channel, "beta")
	mustSend(t, a, "opening line")
	_, cursor := mustRead(t, a, "")

	const (
		timeout  = 300 * time.Millisecond
		debounce = 100 * time.Millisecond
	)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
			b.Send(context.Background(), fmt.Sprintf("firehose %d", i))
		}
	}()

	start := time.Now()
	msgs, _, err := a.Wait(testContext(t), cursor, timeout, debounce)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if len(msgs) == 0 {
		t.Fatal("Wait returned nothing while the channel never stopped talking")
	}
	// The bound is the contract, not a target: a channel that never pauses does
	// not get to hold the wait open, and the read that collects the messages
	// happens inside the window rather than after it.
	t.Logf("Wait returned after %v, %v past its %v bound", elapsed, elapsed-(timeout+debounce), timeout+debounce)
	if elapsed > timeout+debounce+waitSlack {
		t.Errorf("Wait took %v, past the timeout+debounce it promises", elapsed)
	}
}

// TestReadRefusesAHistoryItCannotPageThrough: a history longer than the pages
// one read may walk is not history to return half of. Returning the newest
// pages would move the cursor past everything older, which is the silent
// version of the gap this adapter exists to report.
func TestReadRefusesAHistoryItCannotPageThrough(t *testing.T) {
	// Two items per page, from the server's own advertised limit.
	srv := startErgoWith(t, testHistory, 2)
	channel := fmtChannel(t)
	a := newStream(t, srv, channel, "alpha")

	was := maxPages
	maxPages = 2
	defer func() { maxPages = was }()

	for i := range 6 {
		mustSend(t, a, fmt.Sprintf("message %d", i))
	}
	msgs, cursor, err := a.Read(testContext(t), "")
	if err == nil {
		t.Fatalf("Read returned %d messages for a history it could not page through", len(msgs))
	}
	if len(msgs) != 0 {
		t.Errorf("Read returned %q alongside the error", bodies(msgs))
	}
	if cursor != "" {
		t.Errorf("Read moved the cursor to %q despite refusing the history", string(cursor))
	}
	for _, want := range []string{channel, "2 pages"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
}

// TestTheLoserOfTheAccountRaceRetriesOnce: the connection that loses the race
// to register an account authenticates with the winner's password, and the
// winner may not have finished registering when it does. One refusal is not an
// answer yet; two are.
func TestTheLoserOfTheAccountRaceRetriesOnce(t *testing.T) {
	srv := startErgo(t, testHistory)
	channel := fmtChannel(t)
	spec, err := ParseSpec("irc://" + srv.Addr + "/" + strings.TrimPrefix(channel, "#"))
	if err != nil {
		t.Fatalf("ParseSpec: %v", err)
	}
	// A store that always loses the claim and hands back a password no account
	// on this server knows: the retry is taken and gets the same answer, which
	// is how a genuinely wrong password is told from one that is merely early.
	creds := &lostRace{
		Credentials: NewStateCredentials(srv.State, "alpha"),
		password:    strings.Repeat("ab", passwordBytes),
	}
	s, err := Open(spec, "alpha", creds)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	start := time.Now()
	_, err = s.Send(testContext(t), "this cannot be sent")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("a connection authenticating with a password no account knows was accepted")
	}
	if elapsed < raceRetryDelay {
		t.Errorf("the connection gave up after %v, without waiting %v for the account it was told to expect", elapsed, raceRetryDelay)
	}
	if !strings.Contains(err.Error(), "904") {
		t.Errorf("the error does not carry the server's refusal: %v", err)
	}
}

// lostRace is a credential store that never wins a claim and hands back a
// password of its own.
type lostRace struct {
	Credentials
	password string
	loads    int
}

func (l *lostRace) Load(addr string) (string, error) {
	l.loads++
	if l.loads == 1 {
		// Nothing stored yet, which is what sends the caller to claim one.
		return "", nil
	}
	return l.password, nil
}

func (l *lostRace) Claim(addr, password string) (bool, error) { return false, nil }

// TestAPasswordWithNoAccountIsRecovered: the password is claimed before the
// account is registered, so a connection that dies in between leaves a password
// with nothing behind it. Every later connection would be refused for ever with
// no way out, so one refusal is treated as a question: the account is made now,
// with the password that was already stored.
func TestAPasswordWithNoAccountIsRecovered(t *testing.T) {
	srv := startErgo(t, testHistory)
	channel := fmtChannel(t)

	// Exactly the state a connection that died mid-registration leaves behind.
	creds := NewStateCredentials(srv.State, "alpha")
	orphan := strings.Repeat("f0", passwordBytes)
	claimed, err := creds.Claim(srv.Addr, orphan)
	if err != nil {
		t.Fatalf("claim a password: %v", err)
	}
	if !claimed {
		t.Fatal("the store already held a password for a server it has never seen")
	}

	a := newStream(t, srv, channel, "alpha")
	mustSend(t, a, "sent by the connection that recovered the account")

	// The account is the one that password names, so it is still the stored
	// one — nothing was re-generated behind the participant's back.
	after, err := creds.Load(srv.Addr)
	if err != nil {
		t.Fatalf("re-read the stored password: %v", err)
	}
	if after != orphan {
		t.Fatalf("the stored password changed: the recovery registered a different one")
	}
	// And it is a normal account from here: a second connection authenticates
	// with it and has nothing to recover.
	b := newStream(t, srv, channel, "alpha")
	mustSend(t, b, "sent by a second connection with nothing to recover")
	msgs, _ := mustRead(t, b, "")
	if len(msgs) != 2 {
		t.Fatalf("the channel holds %q, want both messages", bodies(msgs))
	}
}

// TestWriteLinesHonoursTheCallersDeadline: a write that cannot finish inside
// what the caller asked for must not hold the socket for the ceiling instead.
func TestWriteLinesHonoursTheCallersDeadline(t *testing.T) {
	// net.Pipe is synchronous and nobody reads the other end, so the write
	// below cannot complete: what ends it is a deadline, and which one is the
	// whole question.
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	c := newTestConn(client)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := c.writeLines(ctx, line{command: "PRIVMSG", params: []string{"#parley", "into a socket nobody reads"}, colon: true})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("a write that could not be delivered reported success")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("the write took %v to give up, so it used the ceiling rather than the caller's deadline", elapsed)
	}
	// What ended the write is what the caller is told about: a wait that
	// reached its own deadline is a quiet timeout, and reporting an i/o
	// failure for it would send the caller looking for a fault.
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the write reported %v, want the caller's own deadline", err)
	}
	if !strings.Contains(err.Error(), c.addr) {
		t.Errorf("the error does not name the server: %v", err)
	}
	// And a context already spent is not worth a syscall at all.
	spent, cancelSpent := context.WithCancel(context.Background())
	cancelSpent()
	if err := c.writeLines(spent, line{command: "PING", params: []string{"x"}, colon: true}); !errors.Is(err, context.Canceled) {
		t.Fatalf("a write on a cancelled context returned %v, want context.Canceled", err)
	}
}

// TestRegisterHonoursTheCallersDeadline: registration arms the socket at the
// caller's deadline, and the socket's timer and the context's are separate.
// A server that takes the opening lines and never answers ends the handshake
// by a deadline, and which of the two noticed first must not decide whether
// the caller hears its own context.DeadlineExceeded or a raw i/o timeout.
func TestRegisterHonoursTheCallersDeadline(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		io.Copy(io.Discard, server)
	}()
	defer func() {
		server.Close()
		<-drained
	}()
	c := newTestConn(client)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := c.register(ctx)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("a registration the server never answered reported success")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("the registration took %v to give up, so it used the ceiling rather than the caller's deadline", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the registration reported %v, want the caller's own deadline", err)
	}
}

// laggingContext is a context whose deadline has passed and whose Err has not
// yet been told: the state the socket's timer finds when it wins the race.
type laggingContext struct {
	context.Context
	deadline time.Time
}

func (l laggingContext) Deadline() (time.Time, bool) { return l.deadline, true }
func (l laggingContext) Err() error                  { return nil }

// TestReadReplyHonoursTheCallersDeadline: readReply is the registration
// loop's reader, and the socket deadline register armed is the caller's. The
// context here never reports its end, so the socket's deadline error is all
// readReply has to go on: the race the timers make, held still.
func TestReadReplyHonoursTheCallersDeadline(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	c := newTestConn(client)
	deadline := time.Now().Add(100 * time.Millisecond)
	if err := client.SetDeadline(deadline); err != nil {
		t.Fatalf("arm the socket: %v", err)
	}

	_, err := c.readReply(laggingContext{Context: context.Background(), deadline: deadline})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("readReply reported %v, want the caller's own deadline", err)
	}

	// A deadline that is not the caller's is not reported as theirs: this one
	// is the socket's own, with no context deadline anywhere.
	if err := client.SetDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatalf("arm the socket: %v", err)
	}
	_, err = c.readReply(context.Background())
	if err == nil || errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("readReply under a ceiling of its own reported %v, want the socket's i/o timeout", err)
	}
	if !strings.Contains(err.Error(), c.nick) {
		t.Errorf("the error does not name the nick being registered: %v", err)
	}
}

// TestDeadlineErrTellsTheCallersDeadlineFromTheSockets: the four things an I/O
// error can be next to a context, and a few of their edges.
func TestDeadlineErrTellsTheCallersDeadlineFromTheSockets(t *testing.T) {
	past := time.Now().Add(-time.Second)
	future := time.Now().Add(time.Hour)
	timeout := &net.OpError{Op: "read", Err: os.ErrDeadlineExceeded}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, cancelExpired := context.WithDeadline(context.Background(), past)
	defer cancelExpired()
	live, cancelLive := context.WithDeadline(context.Background(), future)
	defer cancelLive()

	for _, tc := range []struct {
		name string
		ctx  context.Context
		err  error
		want error
	}{
		{"cancelled, whatever the error was", cancelled, io.EOF, context.Canceled},
		{"deadline recorded by the context", expired, timeout, context.DeadlineExceeded},
		{"deadline passed, context not yet told", laggingContext{context.Background(), past}, timeout, context.DeadlineExceeded},
		{"deadline passed, error wrapped", laggingContext{context.Background(), past}, fmt.Errorf("irc: %w", timeout), context.DeadlineExceeded},
		{"socket deadline, caller's not reached", live, timeout, nil},
		{"socket deadline, no caller deadline", context.Background(), timeout, nil},
		{"unrelated error, deadline passed", laggingContext{context.Background(), past}, io.EOF, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := deadlineErr(tc.ctx, tc.err)
			if got != tc.want {
				t.Errorf("deadlineErr = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestCloseUnblocksAWait: a stream that is closed while a wait is blocked on it
// must say so at once. The wait is blocked on a connection, and Close takes the
// connection away; without a wake-up it would sit there until its timeout, long
// after the thing it was waiting on stopped existing.
func TestCloseUnblocksAWait(t *testing.T) {
	srv := startErgo(t, testHistory)
	a := newStream(t, srv, fmtChannel(t), "alpha")
	mustSend(t, a, "opening line")
	_, cursor := mustRead(t, a, "")

	type waited struct {
		msgs []stream.Message
		err  error
	}
	done := make(chan waited, 1)
	go func() {
		msgs, _, err := a.Wait(context.Background(), cursor, time.Minute, time.Second)
		done <- waited{msgs, err}
	}()
	time.Sleep(300 * time.Millisecond)

	closed := time.Now()
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case got := <-done:
		elapsed := time.Since(closed)
		t.Logf("the wait returned %v after Close", elapsed)
		if elapsed > 100*time.Millisecond {
			t.Errorf("the wait took %v to notice the stream had been closed", elapsed)
		}
		if got.err == nil {
			t.Fatalf("the wait returned %q and no error after the stream was closed", bodies(got.msgs))
		}
		if !strings.Contains(got.err.Error(), "closed") {
			t.Errorf("the error does not say the stream was closed: %v", got.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the wait never returned after the stream was closed")
	}
}

// TestAWakeThatIsNotAMessageDoesNotEndTheWait: most of what an IRC channel
// carries is not a message, and a wake-up for one of those is not something to
// hand back as an empty delivery — the wait has time left and must use it.
func TestAWakeThatIsNotAMessageDoesNotEndTheWait(t *testing.T) {
	srv := startErgo(t, testHistory)
	channel := fmtChannel(t)
	a := newStream(t, srv, channel, "alpha")
	mustSend(t, a, "opening line")
	_, cursor := mustRead(t, a, "")
	human := dialRaw(t, srv.Addr, "human", channel)

	go func() {
		// A client-to-client request: a PRIVMSG on the channel, which wakes the
		// wait, and not a message, which no reader delivers.
		human.send(t, "PRIVMSG "+channel+" :\x01PING 12345\x01")
		time.Sleep(300 * time.Millisecond)
		human.send(t, "PRIVMSG "+channel+" :a real message from the human")
	}()

	msgs, _, err := a.Wait(testContext(t), cursor, time.Second, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("Wait returned %q, want the one real message: a wake-up that was not a message ended the wait", bodies(msgs))
	}
	if msgs[0].Body != "a real message from the human" {
		t.Errorf("Wait returned %q", msgs[0].Body)
	}
}

// TestAnInterruptedRegistrationKeepsThePassword: a REGISTER that never got an
// answer proves nothing about whether the account was made. Throwing the
// password away there would strand the participant's own name on the server;
// keeping it costs nothing, because the next connection either logs in with it
// or registers with it.
func TestAnInterruptedRegistrationKeepsThePassword(t *testing.T) {
	const addr = "127.0.0.1:6667"
	creds := NewStateCredentials(t.TempDir(), "alpha")
	client, server := net.Pipe()
	defer client.Close()
	c := newTestConn(client)
	c.creds = creds
	c.caps["draft/account-registration"] = "before-connect"
	// The far end takes the REGISTER and dies without answering it, which is
	// every way a registration can be cut short: a timeout, a dropped link, a
	// server that fell over between doing the work and saying so.
	go func() {
		bufio.NewReader(server).ReadString('\n')
		server.Close()
	}()

	if err := c.authenticate(context.Background()); err == nil {
		t.Fatal("a registration that was never answered reported success")
	}
	pw, err := creds.Load(addr)
	if err != nil {
		t.Fatalf("read the password back: %v", err)
	}
	if pw == "" {
		t.Fatal("an interrupted registration threw the password away: the account it may have made is now unreachable")
	}
}

// TestReadLineRefusesALineOverTheBudget: the budget is the whole of what a line
// may occupy, CRLF included, and a line past it is not one this adapter can
// resynchronise after.
func TestReadLineRefusesALineOverTheBudget(t *testing.T) {
	body := func(total int) string {
		const head = "PRIVMSG #parley :"
		return head + strings.Repeat("x", total-len(head)-2) + "\r\n"
	}
	for _, tc := range []struct {
		name  string
		line  string
		wants bool // an error
	}{
		{name: "exactly the budget", line: body(maxLineLen)},
		{name: "one byte past it", line: body(maxLineLen + 1), wants: true},
	} {
		client, server := net.Pipe()
		c := newTestConn(client)
		go func() {
			server.Write([]byte(tc.line))
			server.Close()
		}()
		l, err := c.readLine()
		client.Close()
		switch {
		case tc.wants && !errors.Is(err, errLineTooLong):
			t.Errorf("%s: readLine returned (%+v, %v), want errLineTooLong", tc.name, l, err)
		case !tc.wants && err != nil:
			t.Errorf("%s: readLine returned %v for a line of exactly %d bytes", tc.name, err, maxLineLen)
		case !tc.wants && l.command != "PRIVMSG":
			t.Errorf("%s: readLine returned %+v", tc.name, l)
		}
	}
}

// TestTheCursorMovesOverWhatIsNotAMessage: most of what a channel carries is
// not a message, and a cursor left in front of one would fetch it again on
// every read for as long as nothing else arrived. The human's client sending a
// CTCP request is the everyday case.
func TestTheCursorMovesOverWhatIsNotAMessage(t *testing.T) {
	srv := startErgo(t, testHistory)
	channel := fmtChannel(t)
	a := newStream(t, srv, channel, "alpha")
	mustSend(t, a, "a real message")
	_, before := mustRead(t, a, "")

	human := dialRaw(t, srv.Addr, "human", channel)
	human.send(t, "PRIVMSG "+channel+" :\x01VERSION\x01")

	deadline := time.Now().Add(5 * time.Second)
	var after stream.Cursor
	for time.Now().Before(deadline) {
		var msgs []stream.Message
		msgs, after = mustRead(t, a, before)
		if len(msgs) != 0 {
			t.Fatalf("a client-to-client request was delivered as a message: %q", bodies(msgs))
		}
		if after != before {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if after == before {
		t.Fatalf("the cursor stayed at %q with a history item in front of it", string(before))
	}
	// And it is a position the server answers for: reading from it finds
	// nothing, rather than refusing it or replaying the channel.
	msgs, next, err := a.Read(testContext(t), after)
	if err != nil {
		t.Fatalf("Read from the cursor that moved over a non-message: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("Read returned %q, want nothing", bodies(msgs))
	}
	if next != after {
		t.Errorf("the cursor moved again, from %q to %q", string(after), string(next))
	}
}

// scriptedHistory answers CHATHISTORY requests on nc with canned pages, one per
// request, echoing each request's label. It stands in for nothing: a server
// that behaves is what the live tests are for, and this is here only to produce
// the two answers a correct server never gives.
func scriptedHistory(t *testing.T, nc net.Conn, pages ...[]string) {
	t.Helper()
	go func() {
		r := bufio.NewReader(nc)
		for _, page := range pages {
			var label string
			for label == "" {
				raw, err := r.ReadString('\n')
				if err != nil {
					return
				}
				l, err := parseLine(raw)
				if err != nil || l.command != "CHATHISTORY" {
					continue
				}
				label = l.tags["label"]
			}
			for i, line := range page {
				if i == 0 {
					// The label goes on the line that opens the batch, beside
					// whatever tags it already carries.
					if rest, ok := strings.CutPrefix(line, "@"); ok {
						line = "@label=" + label + ";" + rest
					} else {
						line = "@label=" + label + " " + line
					}
				}
				if _, err := nc.Write([]byte(line + "\r\n")); err != nil {
					return
				}
			}
		}
	}()
}

func TestReadHistoryRefusesAPageThatGoesNowhere(t *testing.T) {
	const item = "@msgid=gfdf2qma95tquqv8j46p4g866s;time=2026-09-14T01:02:03.456Z;batch=1 :alpha!~alpha@localhost PRIVMSG #parley :first"
	tests := map[string]struct {
		first  []string // the newest page, if it is not the default one
		second []string
		says   string
	}{
		// A page that is the end of the history and still leaves nothing to
		// resume from: every id in it is the server's own narration. Reporting
		// success would leave the cursor at zero and replay this for ever.
		"the end, and no way back to it": {
			first: []string{
				"@draft/chathistory-end :ergo.test BATCH +1 chathistory #parley",
				"@msgid=_av5mue7hy5ce4v8nzgp7h2gj9a;time=2026-09-14T01:02:03.456Z;batch=1 :HistServ!HistServ@localhost PRIVMSG #parley :alpha joined the channel",
				":ergo.test BATCH -1",
			},
			says: "no id parley can page from",
		},
		// A page that starts where the last one started: the server ignored the
		// anchor, so reading on would repeat what is already in hand.
		"the anchor came back": {
			second: []string{
				":ergo.test BATCH +2 chathistory #parley",
				strings.Replace(item, "batch=1", "batch=2", 1),
				":ergo.test BATCH -2",
			},
			says: "starts at",
		},
		// A page with nothing in it and no word that the history ended: the
		// server is saying both that there is more and that there is none.
		"nothing, and no end to it": {
			second: []string{
				":ergo.test BATCH +2 chathistory #parley",
				":ergo.test BATCH -2",
			},
			says: "empty page",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			client, server := net.Pipe()
			defer client.Close()
			defer server.Close()
			c := newTestConn(client)
			go c.readLoop()
			// The newest page, by default not marked as the end of the history
			// so that the read pages back from it.
			first := tc.first
			if first == nil {
				first = []string{
					":ergo.test BATCH +1 chathistory #parley",
					item,
					":ergo.test BATCH -1",
				}
			}
			pages := [][]string{first}
			if tc.second != nil {
				pages = append(pages, tc.second)
			}
			scriptedHistory(t, server, pages...)

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			msgs, cursor, err := (&Stream{}).readHistory(ctx, c)
			if err == nil {
				t.Fatalf("a page that goes nowhere was read as history: %q", bodies(msgs))
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("the error does not name the condition: %v", err)
			}
			if len(msgs) != 0 || cursor != "" {
				t.Errorf("the refused read returned %q and cursor %q, want nothing and no position", bodies(msgs), string(cursor))
			}
		})
	}
}

// TestAWriteGivesUpOnItsCallersContext: the write slot is one at a time, and a
// caller whose context is done while another write holds it has nothing to wait
// for. It must not wait anyway.
func TestAWriteGivesUpOnItsCallersContext(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	c := newTestConn(client)
	// Somebody else is writing, and never finishes.
	c.wsem <- struct{}{}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	err := c.writeLines(ctx, line{command: "PRIVMSG", params: []string{"#parley", "waiting for the slot"}, colon: true})
	elapsed := time.Since(start)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a write waiting for the slot returned %v, want context.Canceled", err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("the write waited %v for a slot its caller had given up on", elapsed)
	}
}

// TestACancelInterruptsAWriteInFlight: a context with no deadline still has to
// be able to stop a write that is going nowhere, and the only thing that
// reaches a blocked write is a deadline.
func TestACancelInterruptsAWriteInFlight(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	c := newTestConn(client)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	// Nobody reads the far end, so this write cannot finish on its own.
	err := c.writeLines(ctx, line{command: "PRIVMSG", params: []string{"#parley", "into a socket nobody reads"}, colon: true})
	elapsed := time.Since(start)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("the interrupted write returned %v, want context.Canceled", err)
	}
	if elapsed > writeTimeout/2 {
		t.Errorf("the write took %v to notice the cancel, with a %v ceiling", elapsed, writeTimeout)
	}
	if !strings.Contains(err.Error(), c.addr) {
		t.Errorf("the error does not name the server: %v", err)
	}
}

// TestWaitDoesNotQueueBehindAHandshake: one adapter opens one connection, so a
// call that needs it while another is still shaking hands has to wait — but
// only for as long as it said it would. A server that has stopped answering
// would otherwise hold a bounded wait for the whole registration timeout.
func TestWaitDoesNotQueueBehindAHandshake(t *testing.T) {
	srv := startErgo(t, testHistory)
	a := newStream(t, srv, fmtChannel(t), "alpha")

	// Stopped before anything connected: the handshake below gets as far as the
	// socket and no further.
	srv.pause(t)
	stuck := make(chan error, 1)
	go func() { stuck <- sendErr(a, context.Background(), "into a handshake that never finishes") }()
	time.Sleep(300 * time.Millisecond)

	const (
		timeout  = 300 * time.Millisecond
		debounce = 100 * time.Millisecond
	)
	start := time.Now()
	msgs, _, err := a.Wait(testContext(t), "", timeout, debounce)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Wait while another call was mid-handshake: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("Wait returned %q from a server that never answered", bodies(msgs))
	}
	if elapsed > timeout+debounce+time.Second {
		t.Fatalf("Wait took %v, so it queued behind the handshake instead of keeping to its own bound", elapsed)
	}

	// Let the stuck call finish, so the test leaves nothing behind.
	srv.resume(t)
	select {
	case <-stuck:
	case <-time.After(40 * time.Second):
		t.Error("the handshake never finished after the server came back")
	}
}

// TestReadAfterRefusesAPageThatRepeatsTheAnchor: the anchor is what the page
// comes after, so finding it at the head of that page means the server answered
// with the anchor and whatever followed. Taking it would hand the anchor's
// message over a second time.
func TestReadAfterRefusesAPageThatRepeatsTheAnchor(t *testing.T) {
	const anchor = "gfdf2qma95tquqv8j46p4g866s"
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	c := newTestConn(client)
	go c.readLoop()
	scriptedHistory(t, server,
		// What follows the caller's cursor: one message, and not the end of the
		// history, so the read asks for more after it.
		[]string{
			":ergo.test BATCH +1 chathistory #parley",
			"@msgid=" + anchor + ";time=2026-09-14T01:02:03.456Z;batch=1 :alpha!~alpha@localhost PRIVMSG #parley :first",
			":ergo.test BATCH -1",
		},
		// And the server answers that with the anchor again, plus something new.
		[]string{
			":ergo.test BATCH +2 chathistory #parley",
			"@msgid=" + anchor + ";time=2026-09-14T01:02:03.456Z;batch=2 :alpha!~alpha@localhost PRIVMSG #parley :first",
			"@msgid=avsdrig8mry9e9bp5bt3y9rfcn;time=2026-09-14T01:02:04.456Z;batch=2 :alpha!~alpha@localhost PRIVMSG #parley :second",
			":ergo.test BATCH -2",
		},
	)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	msgs, cursor, err := (&Stream{}).readAfter(ctx, c, "earlier")
	if err == nil {
		t.Fatalf("a page that repeats its anchor was read as history: %q", bodies(msgs))
	}
	if !strings.Contains(err.Error(), "begins at") {
		t.Errorf("the error does not name the condition: %v", err)
	}
	// What the first page held is kept, and the position is just past it: the
	// repeated page is refused, not half-taken.
	if got := bodies(msgs); len(got) != 1 || got[0] != "first" {
		t.Fatalf("the read returned %q, want only what the good page held", got)
	}
	if string(cursor) != anchor {
		t.Errorf("the cursor is %q, want the last item of the good page (%q)", string(cursor), anchor)
	}
}

// TestReadAfterRefusesAPageWithNoPositionToResumeFrom: a page of nothing but
// the server's own narration is not the end of the story, it is a page with no
// position after it. Reporting success would hand the caller its own cursor
// back and fetch the same page again on every read from here on.
func TestReadAfterRefusesAPageWithNoPositionToResumeFrom(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	c := newTestConn(client)
	go c.readLoop()
	scriptedHistory(t, server, []string{
		"@draft/chathistory-end :ergo.test BATCH +1 chathistory #parley",
		"@msgid=_av5mue7hy5ce4v8nzgp7h2gj9a;time=2026-09-14T01:02:03.456Z;batch=1 :HistServ!HistServ@localhost PRIVMSG #parley :alpha joined the channel",
		":ergo.test BATCH -1",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	msgs, cursor, err := (&Stream{}).readAfter(ctx, c, "earlier")
	if err == nil {
		t.Fatalf("a page with no position after it was read as history: %q", bodies(msgs))
	}
	if !strings.Contains(err.Error(), "no id parley can page from") {
		t.Errorf("the error does not name the condition: %v", err)
	}
	// The page ended the history, so the error says that rather than promising
	// there is more behind it.
	if strings.Contains(err.Error(), "more beyond it") {
		t.Errorf("the error claims there is more history after a page that said it was the end: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("the refused read returned %q", bodies(msgs))
	}
	if cursor != "earlier" {
		t.Errorf("the refused read moved the cursor to %q", string(cursor))
	}
}

// TestAnAnswerInHandSurvivesTheConnectionEnding: a server that replies and then
// drops the socket makes both outcomes ready at once, and a select picks
// between ready cases at random. The reply is the answer either way.
func TestAnAnswerInHandSurvivesTheConnectionEnding(t *testing.T) {
	client, _ := net.Pipe()
	defer client.Close()
	c := newTestConn(client)

	reply, err := parseLine("@label=1 :ergo.test BATCH +1 chathistory #parley")
	if err != nil {
		t.Fatalf("parseLine: %v", err)
	}
	w := &waiter{lines: []line{reply}, done: make(chan struct{})}
	close(w.done)
	// The connection is gone, and its answer is already in hand.
	c.fail(errors.New("the socket went away"))

	lines, err := c.await(context.Background(), w, "1")
	if err != nil {
		t.Fatalf("await returned %v for a request that was answered", err)
	}
	if len(lines) != 1 || lines[0].command != "BATCH" {
		t.Fatalf("await returned %+v, want the answer the waiter held", lines)
	}

	// And a waiter the connection released with nothing still reports why.
	empty := &waiter{err: errors.New("connection ended"), done: make(chan struct{})}
	close(empty.done)
	if _, err := c.await(context.Background(), empty, "2"); err == nil {
		t.Fatal("await reported success for a request the connection never answered")
	}
}

// TestACancelAfterAWriteDoesNotFailTheNext: the watch that lets a cancel reach
// a blocked write must be over before the next writer takes the socket, or its
// deadline lands under a write that has nothing to do with it.
func TestACancelAfterAWriteDoesNotFailTheNext(t *testing.T) {
	for i := range 300 {
		client, server := net.Pipe()
		// A reader that keeps the pipe moving, so the writes below complete.
		go func() {
			io.Copy(io.Discard, server)
		}()
		c := newTestConn(client)

		ctx, cancel := context.WithCancel(context.Background())
		go cancel()
		if err := c.writeLines(ctx, line{command: "PRIVMSG", params: []string{"#parley", "racing its own cancel"}, colon: true}); err != nil {
			// The cancel landed inside the write: nothing to check, and the
			// connection is finished by design.
			client.Close()
			server.Close()
			continue
		}
		if err := c.writeLines(context.Background(), line{command: "PRIVMSG", params: []string{"#parley", "the write after it"}, colon: true}); err != nil {
			t.Fatalf("round %d: the write after a cancelled one failed: %v", i, err)
		}
		client.Close()
		server.Close()
	}
}

// TestAnAccountRegisteredByAnotherProcessWithOurClaimIsKept: two connections for
// one participant, one of them holding the claim. The other reads that claim,
// finds no account behind it and makes one — with that same password. The first
// then hears ACCOUNT_EXISTS about an account that is its own, and must not
// delete the password it opens.
func TestAnAccountRegisteredByAnotherProcessWithOurClaimIsKept(t *testing.T) {
	srv := startErgo(t, testHistory)
	channel := fmtChannel(t)
	spec, err := ParseSpec("irc://" + srv.Addr + "/" + strings.TrimPrefix(channel, "#"))
	if err != nil {
		t.Fatalf("ParseSpec: %v", err)
	}

	stalled := &stallingClaim{
		Credentials: NewStateCredentials(srv.State, "alpha"),
		claimed:     make(chan struct{}),
		release:     make(chan struct{}),
	}
	first, err := Open(spec, "alpha", stalled)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer first.Close()
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- sendErr(first, context.Background(), "from the connection that claimed the password")
	}()

	// The claim is on disk and its connection is holding still.
	select {
	case <-stalled.claimed:
	case <-time.After(20 * time.Second):
		t.Fatal("the first connection never claimed a password")
	}

	// The second connection finds the claim, finds no account behind it, and
	// registers one with it.
	second := newStream(t, srv, channel, "alpha")
	if _, err := second.Send(testContext(t), "from the connection that registered it"); err != nil {
		t.Fatalf("the second connection could not make the account: %v", err)
	}

	close(stalled.release)
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("the connection whose claim was used was refused: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the first connection never finished")
	}

	// The password is still there, and it is the one that was claimed.
	path := passwordPath(srv, "alpha")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the password for a healthy account was deleted: %v", err)
	}
	b := newStream(t, srv, channel, "beta")
	msgs, _ := mustRead(t, b, "")
	if len(msgs) != 2 {
		t.Fatalf("the channel holds %q, want both messages", bodies(msgs))
	}
}

// stallingClaim holds a connection still after it has claimed a password, so
// that another can find the claim and act on it.
type stallingClaim struct {
	Credentials
	claimed chan struct{}
	release chan struct{}
}

func (s *stallingClaim) Claim(addr, password string) (bool, error) {
	ok, err := s.Credentials.Claim(addr, password)
	close(s.claimed)
	<-s.release
	return ok, err
}

// TestWaitReturnsAMultilineMessageAtItsBatchClose: a multiline message is one
// message, and the server has it only once the batch that carries it closes.
// Every line before that wakes a waiting reader too early to find it, so the
// line that matters is the one that ends the batch.
func TestWaitReturnsAMultilineMessageAtItsBatchClose(t *testing.T) {
	srv := startErgo(t, testHistory)
	channel := fmtChannel(t)
	a := newStream(t, srv, channel, "alpha")
	mustSend(t, a, "opening line")
	_, cursor := mustRead(t, a, "")

	// A stock client that speaks multiline holds its batch open for a while
	// before closing it. Ergo relays the batch to the channel only once it is
	// complete, so against this server the message is always found; what this
	// pins is that a message arriving as a batch is handed over promptly
	// rather than at the timeout. TestTheClosingLineOfALiveBatchWakesAReader
	// pins the wake-up on the closing line itself.
	human := dialRawCaps(t, srv.Addr, "human", channel, "message-tags", "batch", "draft/multiline")
	const body = "first line of the reply\n\nand the third, after a blank one"
	const hold = 400 * time.Millisecond
	go func() {
		human.send(t, "BATCH +ml draft/multiline "+channel)
		for _, l := range strings.Split(body, "\n") {
			human.send(t, "@batch=ml PRIVMSG "+channel+" :"+l)
		}
		time.Sleep(hold)
		human.send(t, "BATCH -ml")
	}()

	start := time.Now()
	msgs, _, err := a.Wait(testContext(t), cursor, 2*time.Second, 100*time.Millisecond)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Body != body {
		t.Fatalf("Wait returned %q, want the one multiline message", bodies(msgs))
	}
	// Found by the wake-up the closing line caused: soon after the hold plus
	// the debounce, and nowhere near the timeout.
	if elapsed > hold+time.Second {
		t.Errorf("Wait took %v to hand over a message whose batch closed after %v; it waited for something other than the message", elapsed, hold)
	}
}

// TestReadAfterRefusesAnEmptyPageThatIsNotTheEnd: the server marks every short
// page as the end of the history, and an empty one is the shortest there is.
// Without that mark it is saying both that there is more and that there is
// none.
func TestReadAfterRefusesAnEmptyPageThatIsNotTheEnd(t *testing.T) {
	const anchor = "gfdf2qma95tquqv8j46p4g866s"
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	c := newTestConn(client)
	go c.readLoop()
	scriptedHistory(t, server,
		// One message after the caller's cursor, and not the end of it.
		[]string{
			":ergo.test BATCH +1 chathistory #parley",
			"@msgid=" + anchor + ";time=2026-09-14T01:02:03.456Z;batch=1 :alpha!~alpha@localhost PRIVMSG #parley :first",
			":ergo.test BATCH -1",
		},
		// And then nothing, with no word that the history ended.
		[]string{
			":ergo.test BATCH +2 chathistory #parley",
			":ergo.test BATCH -2",
		},
	)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	msgs, cursor, err := (&Stream{}).readAfter(ctx, c, "earlier")
	if err == nil {
		t.Fatalf("an empty page that did not end the history was read as the end: %q", bodies(msgs))
	}
	if !strings.Contains(err.Error(), "empty page") {
		t.Errorf("the error does not name the condition: %v", err)
	}
	// What the good page held is kept, and the position is just past it.
	if got := bodies(msgs); len(got) != 1 || got[0] != "first" {
		t.Fatalf("the read returned %q, want what the good page held", got)
	}
	if string(cursor) != anchor {
		t.Errorf("the cursor is %q, want the last item of the good page (%q)", string(cursor), anchor)
	}
}

// TestTheClosingLineOfALiveBatchWakesAReader: a server commits a multiline
// message to its history when the batch carrying it closes, so the closing
// line is the one wake-up that is never too early. Ergo relays a batch only
// once it is complete, which is why the live test above cannot force the
// window open; this pins the dispatcher directly.
func TestTheClosingLineOfALiveBatchWakesAReader(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	c := newTestConn(client)
	feed := func(s string) {
		t.Helper()
		l, err := parseLine(s)
		if err != nil {
			t.Fatalf("parse %q: %v", s, err)
		}
		c.dispatch(l)
	}
	woke := func(ch <-chan struct{}) bool {
		select {
		case <-ch:
			return true
		default:
			return false
		}
	}

	opening := c.arrivals()
	feed("@msgid=abcdefghijkmnpqrstuvwxyz23;time=2026-09-14T01:02:03.456Z :beta!~beta@localhost BATCH +ml draft/multiline #parley")
	if !woke(opening) {
		t.Fatalf("the opening line of a live batch did not wake a reader")
	}
	feed("@batch=ml :beta!~beta@localhost PRIVMSG #parley :first")
	feed("@batch=ml :beta!~beta@localhost PRIVMSG #parley :second")

	closing := c.arrivals()
	feed(":beta!~beta@localhost BATCH -ml")
	if !woke(closing) {
		t.Fatalf("the closing line of a live batch did not wake a reader; the server has the message only from that line on")
	}

	// A batch nobody opened on this channel closing is not an arrival.
	stray := c.arrivals()
	feed(":ergo.test BATCH -zz")
	if woke(stray) {
		t.Fatalf("closing a batch that was never opened live woke a reader")
	}
}

// TestSendReturnsTheIDAndTimeAReaderGets: the message Send returns carries the
// msgid and time of the server's echo, which are the ID and TS another
// participant's Read reports for the same message.
func TestSendReturnsTheIDAndTimeAReaderGets(t *testing.T) {
	srv := startErgo(t, testHistory)
	channel := fmtChannel(t)
	a := newStream(t, srv, channel, "alpha")
	b := newStream(t, srv, channel, "beta")

	for _, body := range []string{"one line", "two\nlines"} {
		sent, err := a.Send(testContext(t), body)
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		if len(sent.ID) != msgidLen {
			t.Fatalf("Send returned id %q, want %d characters of server msgid", sent.ID, msgidLen)
		}
		if sent.TS.IsZero() {
			t.Fatalf("Send returned no time for %q", body)
		}
		msgs, _ := mustRead(t, b, "")
		var got *stream.Message
		for i := range msgs {
			if msgs[i].ID == sent.ID {
				got = &msgs[i]
			}
		}
		if got == nil {
			t.Fatalf("no message with id %q in what beta reads: %q", sent.ID, bodies(msgs))
		}
		if !sent.TS.Equal(got.TS) || got.Body != body || got.Author != "alpha" || sent.Author != "alpha" || sent.Body != body {
			t.Fatalf("Send returned %+v, beta reads %+v", sent, *got)
		}
	}
}

// TestSendSucceedsWhenTheEchoLacksATag: the echo says the message is posted, so
// a missing msgid or time is not a failure — a non-zero exit would have the
// caller send it twice — and the field comes back empty for the caller to say
// so.
func TestSendSucceedsWhenTheEchoLacksATag(t *testing.T) {
	for name, tc := range map[string]struct {
		tags   string
		wantID string
		wantTS bool
	}{
		"neither":    {tags: "", wantID: "", wantTS: false},
		"no time":    {tags: ";msgid=rzsj6pm4xkqvxz76xb7ymkqw6y", wantID: "rzsj6pm4xkqvxz76xb7ymkqw6y", wantTS: false},
		"no msgid":   {tags: ";time=2026-09-14T01:02:03.456Z", wantID: "", wantTS: true},
		"bad time":   {tags: ";msgid=rzsj6pm4xkqvxz76xb7ymkqw6y;time=not-a-time", wantID: "rzsj6pm4xkqvxz76xb7ymkqw6y", wantTS: false},
		"both tags":  {tags: ";msgid=rzsj6pm4xkqvxz76xb7ymkqw6y;time=2026-09-14T01:02:03.456Z", wantID: "rzsj6pm4xkqvxz76xb7ymkqw6y", wantTS: true},
		"empty tags": {tags: ";msgid=;time=", wantID: "", wantTS: false},
	} {
		t.Run(name, func(t *testing.T) {
			srv := startErgo(t, testHistory)
			channel := fmtChannel(t)
			a := newStream(t, srv, channel, "alpha")
			mustSend(t, a, "sent while the connection was whole")
			c := heldConn(t, a)

			// Nothing is answered by the real server from here on, so the echo
			// below is the only thing the send hears.
			srv.pause(t)
			defer srv.resume(t)

			type outcome struct {
				m   stream.Message
				err error
			}
			sent := make(chan outcome, 1)
			go func() {
				m, err := a.Send(context.Background(), "an echo with tags missing")
				sent <- outcome{m, err}
			}()

			label := awaitWaiter(t, c)
			for _, raw := range []string{
				"@label=" + label + tc.tags + " :alpha!~alpha@localhost BATCH +9 draft/multiline " + channel,
				"@batch=9 :alpha!~alpha@localhost PRIVMSG " + channel + " :an echo with tags missing",
				":alpha!~alpha@localhost BATCH -9",
			} {
				l, err := parseLine(raw)
				if err != nil {
					t.Fatalf("parseLine(%q): %v", raw, err)
				}
				c.dispatch(l)
			}
			select {
			case got := <-sent:
				if got.err != nil {
					t.Fatalf("Send failed on an echo that said the message was posted: %v", got.err)
				}
				if got.m.ID != tc.wantID {
					t.Errorf("id %q, want %q", got.m.ID, tc.wantID)
				}
				if got.m.TS.IsZero() == tc.wantTS {
					t.Errorf("ts %v, want a time: %v", got.m.TS, tc.wantTS)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("Send never returned")
			}
		})
	}
}

// sendErr is Send for a test that only wants to know whether it failed, as the
// value a goroutine reports on a channel.
func sendErr(s *Stream, ctx context.Context, body string) error {
	_, err := s.Send(ctx, body)
	return err
}
