package irc

import (
	"net"
	"strings"
	"testing"
	"time"

	"github.com/colinsurprenant/parley/internal/stream"
)

// The notice tests are about what a trip notice is on the wire, against a real
// ergo: a NOTICE to the chair's nick that the chair's client receives and that
// the channel never carries, whose echo and whose absent-nick answer the
// front's own connection drops.

// firstNotice returns the first NOTICE the client receives within wait, or the
// zero line.
func (r *rawClient) firstNotice(t *testing.T, wait time.Duration) line {
	t.Helper()
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		for _, l := range r.collect(t, 250*time.Millisecond) {
			if l.command == "NOTICE" {
				return l
			}
		}
	}
	return line{}
}

// TestATrippedLineFrontNoticesTheChairAndNoOneElse: the breaker of a line front
// trips on a burst, and the human's client receives one NOTICE from the front,
// word for word what the Deliverer built, while a reader of the channel sees no
// NOTICE and nothing from the front, and the channel's history holds only the
// three posts.
func TestATrippedLineFrontNoticesTheChairAndNoOneElse(t *testing.T) {
	srv := startErgo(t, testHistory)
	channel := fmtChannel(t)
	chair := dialRaw(t, srv.Addr, "chair", channel)
	reader := dialRaw(t, srv.Addr, "reader", channel)
	front := newStream(t, srv, channel, "plan")
	build := newStream(t, srv, channel, "build")
	review := newStream(t, srv, channel, "review")

	d := stream.NewDeliverer(front, "plan", stream.WithLine("chair", 2, 10*time.Minute))
	mustSend(t, build, "one")
	mustSend(t, review, "two")
	mustSend(t, build, "three") // the third within ten minutes: trips

	del, err := d.Read(testContext(t), "")
	if err != nil {
		t.Fatalf("Deliverer.Read: %v", err)
	}
	if got := bodies(del.Messages); !equal(got, []string{"one", "two"}) {
		t.Fatalf("delivered %q, want the first two with the third held", got)
	}
	d.Commit(del)

	n := chair.firstNotice(t, 10*time.Second)
	if n.command == "" {
		t.Fatal("the chair received no NOTICE from the tripped front")
	}
	if from := nickFromPrefix(n.prefix); from != "plan" {
		t.Errorf("the NOTICE is from %q, want the front's nick plan", from)
	}
	if to := n.param(0); to != "chair" {
		t.Errorf("the NOTICE is addressed to %q, want the chair's nick", to)
	}
	want := "parley: plan is holding line deliveries in " + channel + ": more than 2 posts within 10m. Post in " + channel + " to release."
	if got := n.trailing(); got != want {
		t.Errorf("the NOTICE reads %q, want %q", got, want)
	}

	// Nobody on the channel is told, and nothing of it is a post: not in what a
	// reader's client received, and not in the history.
	for _, l := range reader.collect(t, time.Second) {
		if l.command == "NOTICE" {
			t.Errorf("a reader of the channel received a NOTICE: %s", l.String())
		}
		if l.command == "PRIVMSG" && nickFromPrefix(l.prefix) == "plan" {
			t.Errorf("a reader of the channel received a post from the front: %s", l.String())
		}
	}
	msgs, _ := mustRead(t, build, "")
	if got := bodies(msgs); !equal(got, []string{"one", "two", "three"}) {
		t.Fatalf("the channel's history holds %q, want only the three posts", got)
	}

	// The echo of the notice came back to the front's own connection and changed
	// nothing there: the held post is still held, and nothing new is delivered.
	again, err := d.Read(testContext(t), del.Cursor)
	if err != nil {
		t.Fatalf("Deliverer.Read: %v", err)
	}
	if !again.Empty() {
		t.Fatalf("the front was handed %q after the notice", bodies(again.Messages))
	}
	// One trip is one notice: none more arrives for the held post.
	if extra := chair.firstNotice(t, time.Second); extra.command != "" {
		t.Fatalf("a second NOTICE arrived for one trip: %s", extra.String())
	}
}

// TestANoticeAndItsAnswersAreDroppedByTheFrontsConnection: what the server sends
// the sender for a NOTICE — the echo, and for a nick that is not there whatever
// it chooses to say — carries no label and is no message to the channel, so the
// connection drops it: it wakes no Wait, resolves no request, and the connection
// goes on working. ergo is silent for an absent nick, which is the case this
// pins; the dispatcher test below covers a server that answers 401.
func TestANoticeAndItsAnswersAreDroppedByTheFrontsConnection(t *testing.T) {
	srv := startErgo(t, testHistory)
	channel := fmtChannel(t)
	chair := dialRaw(t, srv.Addr, "chair", channel)
	front := newStream(t, srv, channel, "plan")
	mustSend(t, front, "hello") // the connection exists and is joined
	c := heldConn(t, front)
	arrived := c.arrivals()

	// The chair is there: the echo returns to the front.
	if err := front.Notice(testContext(t), "chair", "parley: one line"); err != nil {
		t.Fatalf("Notice to a connected nick: %v", err)
	}
	if n := chair.firstNotice(t, 10*time.Second); n.trailing() != "parley: one line" {
		t.Fatalf("the chair received %q", n.trailing())
	}
	// The chair is not: nothing waits for a reply, and the call returns at once.
	begin := time.Now()
	if err := front.Notice(testContext(t), "ghost", "parley: to nobody"); err != nil {
		t.Fatalf("Notice to a nick that is not connected: %v", err)
	}
	if took := time.Since(begin); took > 2*time.Second {
		t.Fatalf("Notice to an absent nick took %v: it waited for something", took)
	}

	select {
	case <-arrived:
		t.Fatal("a NOTICE, its echo or its answer woke a Wait: only a PRIVMSG to the channel does")
	case <-time.After(750 * time.Millisecond):
	}
	c.mu.Lock()
	waiting := len(c.waiters)
	c.mu.Unlock()
	if waiting != 0 {
		t.Fatalf("%d requests are waiting after a notice, which sends none", waiting)
	}
	if c.isDead() {
		t.Fatal("the connection ended")
	}

	// And the connection is as good as it was.
	mustSend(t, front, "still here")
	msgs, _ := mustRead(t, front, "")
	if got := bodies(msgs); len(got) != 2 || got[1] != "still here" {
		t.Fatalf("the history reads %q after the notices", got)
	}
}

// TestTheDispatcherDropsANoticeItsEchoAndANoSuchNickReply: the same three lines
// as scripted, for a server that does answer a NOTICE to an absent nick: with no
// label and no batch they are nobody's reply, and wake nothing.
func TestTheDispatcherDropsANoticeItsEchoAndANoSuchNickReply(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	c := newTestConn(client)
	arrived := c.arrivals()

	for _, raw := range []string{
		"@msgid=abc;time=2026-11-15T10:00:00.000Z :plan!~plan@localhost NOTICE chair :parley: one line",
		":ergo.test 401 plan ghost :No such nick",
		":chair!~chair@localhost NOTICE plan :a notice that came the other way",
	} {
		l, err := parseLine(raw)
		if err != nil {
			t.Fatalf("parseLine(%q): %v", raw, err)
		}
		c.dispatch(l)
	}
	select {
	case <-arrived:
		t.Fatal("a NOTICE or a 401 woke a Wait")
	default:
	}
	if len(c.waiters) != 0 || len(c.batchOf) != 0 || len(c.liveBatch) != 0 {
		t.Fatalf("the dispatcher kept state for lines that are nobody's: %d waiters, %d batches, %d live", len(c.waiters), len(c.batchOf), len(c.liveBatch))
	}
}

// TestAnInboundNoticeIsNotAMessage: a NOTICE to the channel is history to the
// server and not a message to a reader, here or at any other front: Read and Wait
// skip it, as every line that is not a PRIVMSG.
func TestAnInboundNoticeIsNotAMessage(t *testing.T) {
	srv := startErgo(t, testHistory)
	channel := fmtChannel(t)
	human := dialRaw(t, srv.Addr, "human", channel)
	reader := newStream(t, srv, channel, "reader")
	peer := newStream(t, srv, channel, "peer")

	mustSend(t, peer, "a post")
	human.send(t, "NOTICE "+channel+" :a notice to the channel")
	human.send(t, "NOTICE reader :a notice to the reader")
	mustSend(t, peer, "another post")

	msgs, _ := mustRead(t, reader, "")
	if got := bodies(msgs); !equal(got, []string{"a post", "another post"}) {
		t.Fatalf("Read returned %q, want the two posts and no notice", got)
	}
	got, _, err := reader.Wait(testContext(t), "", 300*time.Millisecond, 0)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if bs := bodies(got); !equal(bs, []string{"a post", "another post"}) {
		t.Fatalf("Wait returned %q, want the two posts and no notice", bs)
	}
}

// TestNoticeRefusesWhatItCannotSendAsOneLine: a text with a control character in
// it — the NUL, CR and CTCP delimiter Send refuses, and the LF that would end the
// line — a nick outside the grammar and an empty text are errors, and none of
// them reaches the network: the stream here has no server to dial.
func TestNoticeRefusesWhatItCannotSendAsOneLine(t *testing.T) {
	spec, err := ParseSpec("irc://127.0.0.1:1/parley")
	if err != nil {
		t.Fatalf("ParseSpec: %v", err)
	}
	s, err := Open(spec, "plan", NewStateCredentials(t.TempDir(), "plan"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for name, tc := range map[string]struct{ nick, text, want string }{
		"empty text":    {"chair", "", "empty"},
		"a NUL":         {"chair", "a\x00b", "control character 0x00"},
		"a CR":          {"chair", "a\rb", "control character 0x0d"},
		"an LF":         {"chair", "a\nb", "control character 0x0a"},
		"a CTCP mark":   {"chair", "\x01ACTION x\x01", "control character 0x01"},
		"an ESC":        {"chair", "a\x1b[31mb", "control character 0x1b"},
		"a DEL":         {"chair", "a\x7fb", "control character 0x7f"},
		"a spaced nick": {"chair PRIVMSG #x", "x", "notice target"},
		"a CRLF nick":   {"chair\r\nQUIT", "x", "notice target"},
		"an empty nick": {"", "x", "notice target"},
	} {
		t.Run(name, func(t *testing.T) {
			err := s.Notice(testContext(t), tc.nick, tc.text)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Notice(%q, %q) = %v, want an error naming %q", tc.nick, tc.text, err, tc.want)
			}
			if strings.Contains(err.Error(), "dial") {
				t.Fatalf("Notice dialed before refusing: %v", err)
			}
		})
	}
}

// TestNoticeIsBoundedByItsContextAndFailsAlone: a server that is not there is an
// error returned at once, not retried, and a closed stream is one too.
func TestNoticeIsBoundedByItsContextAndFailsAlone(t *testing.T) {
	spec, err := ParseSpec("irc://127.0.0.1:" + freePort(t) + "/parley")
	if err != nil {
		t.Fatalf("ParseSpec: %v", err)
	}
	s, err := Open(spec, "plan", NewStateCredentials(t.TempDir(), "plan"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	begin := time.Now()
	if err := s.Notice(testContext(t), "chair", "parley: nobody is listening"); err == nil {
		t.Fatal("Notice with no server returned nil")
	}
	if took := time.Since(begin); took > 5*time.Second {
		t.Fatalf("Notice with no server took %v", took)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Notice(testContext(t), "chair", "parley: closed"); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("Notice on a closed stream = %v, want the closed error", err)
	}
}

// TestNoticeRefusesALineTheServerWouldCut: a line is what is left of 512 bytes
// after this connection's own prefix and the target, and one longer is an error
// rather than a half sentence.
func TestNoticeRefusesALineTheServerWouldCut(t *testing.T) {
	srv := startErgo(t, testHistory)
	channel := fmtChannel(t)
	chair := dialRaw(t, srv.Addr, "chair", channel)
	front := newStream(t, srv, channel, "plan")
	mustSend(t, front, "hello")
	limit := heldConn(t, front).noticeLimit("chair")
	if limit < 300 || limit > 512 {
		t.Fatalf("the notice limit is %d bytes, want a few hundred", limit)
	}
	if err := front.Notice(testContext(t), "chair", strings.Repeat("x", limit+1)); err == nil || !strings.Contains(err.Error(), "relays at most") {
		t.Fatalf("a line over the limit: %v, want the refusal", err)
	}
	full := strings.Repeat("y", limit)
	if err := front.Notice(testContext(t), "chair", full); err != nil {
		t.Fatalf("a line at the limit: %v", err)
	}
	if n := chair.firstNotice(t, 10*time.Second); n.trailing() != full {
		t.Fatalf("the chair received %d bytes of the %d sent", len(n.trailing()), len(full))
	}
}
