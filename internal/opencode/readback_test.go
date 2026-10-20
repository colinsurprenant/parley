package opencode

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// ---- the read-back pages back past a turn that outran it ----

// TestAQuickTurnDoesNotHideTheDeliveredMessage: the turn the delivery starts
// writes twelve assistant messages before a read-back poll gets an answer, so the
// delivered user message is on the second page of ten, not the first. A read-back
// of the newest page alone never sees it, times out and sends the delivery again.
func TestAQuickTurnDoesNotHideTheDeliveredMessage(t *testing.T) {
	f := newFake(t)
	for i := 0; i < 3; i++ {
		f.seed("user", fmt.Sprintf("old %d", i))
	}
	f.set(func() { f.burst = 12 })
	sp := &spy{Client: f.client()}
	h := newHarness(t, f, func(c *Config) { c.Client = sp; c.ReadBackTimeout = 400 * time.Millisecond })
	h.start()

	h.peerSend(peerNick, "behind a burst")
	f.waitPosts(1)
	h.cursorPast("")
	if n := len(f.postBodies()); n != 1 {
		t.Fatalf("%d prompts, want 1", n)
	}
	if sp.pagedReads() == 0 {
		t.Fatalf("the delivered message was found without paging back, which the burst made impossible: %v", sp.befores)
	}
	if strings.Contains(h.logs.String(), "no user message holding it appeared") {
		t.Fatalf("the read-back timed out:\n%s", h.logs.String())
	}
}

// TestAQuickTurnDoesNotHideTheDeliveredMessageAtTheNarrowWindow is the same at
// the window a heavy history narrows the read to: five assistant messages are
// more than one page of three.
func TestAQuickTurnDoesNotHideTheDeliveredMessageAtTheNarrowWindow(t *testing.T) {
	f := newFake(t)
	f.set(func() { f.burst = 5 })
	sp := &spy{Client: f.client(), messages: func(limit, _ int) error {
		if limit == historyWindow {
			return tooLong()
		}
		return nil
	}}
	h := newHarness(t, f, func(c *Config) { c.Client = sp; c.ReadBackTimeout = 400 * time.Millisecond })
	h.start()

	h.peerSend(peerNick, "behind a burst, narrowed")
	f.waitPosts(1)
	h.cursorPast("")
	for _, l := range sp.seenLimits()[1:] {
		if l != narrowWindow {
			t.Fatalf("the reads asked for %v: after narrowing, every one asks for %d", sp.seenLimits(), narrowWindow)
		}
	}
	if sp.pagedReads() == 0 {
		t.Fatalf("the delivered message was found without paging back: %v", sp.befores)
	}
}

// TestThePagingStopsAtTheFirstPageThatPredatesThePost: a page with a message the
// snapshot already held on it ends the look, since everything older than it was
// there before the post. Here the delivery never lands, and the look, poll after
// poll, never asks for a second page, however much history there is behind it.
func TestThePagingStopsAtTheFirstPageThatPredatesThePost(t *testing.T) {
	f := newFake(t)
	for i := 0; i < 100; i++ {
		f.seed("user", fmt.Sprintf("old %03d", i))
	}
	f.set(func() { f.hold = true })
	sp := &spy{Client: f.client()}
	h := newHarness(t, f, func(c *Config) { c.Client = sp; c.ReadBackTimeout = 200 * time.Millisecond })
	h.start()

	h.peerSend(peerNick, "lost")
	if err := h.join(); err == nil || !strings.Contains(err.Error(), "no user message holding it appeared") {
		t.Fatalf("Run returned %v, want the read-back timeout", err)
	}
	if n := sp.pagedReads(); n != 0 {
		t.Fatalf("%d reads paged back through history that predates the post", n)
	}
}

// stubClient serves pages a test makes up, and counts them.
type stubClient struct {
	Client
	calls int
	page  func(call int, before string) (MessagePage, error)
}

func (s *stubClient) Messages(_ context.Context, _ string, _ int, before string) (MessagePage, error) {
	s.calls++
	return s.page(s.calls, before)
}

// TestThePagingIsBoundedAndStopsAtTheLastPage: a history of unknown messages with
// no end is looked through for maxReadBackPages pages and no more, and one that
// ends is looked through to its end and no further.
func TestThePagingIsBoundedAndStopsAtTheLastPage(t *testing.T) {
	p := &Pump{session: testSession}
	known := map[string]struct{}{"old": {}}

	endless := &stubClient{page: func(call int, _ string) (MessagePage, error) {
		return MessagePage{Messages: []Message{{ID: fmt.Sprintf("new-%d", call), Role: "assistant"}}, Next: fmt.Sprintf("c%d", call)}, nil
	}}
	p.client = endless
	if id, err := p.findDelivered(context.Background(), known, 1, "text"); id != "" || err != nil || endless.calls != maxReadBackPages {
		t.Fatalf("an endless history: id %q, err %v, %d pages; want none found after %d pages", id, err, endless.calls, maxReadBackPages)
	}

	ends := &stubClient{page: func(call int, before string) (MessagePage, error) {
		if call == 1 {
			return MessagePage{Messages: []Message{{ID: "n1", Role: "assistant"}}, Next: "c1"}, nil
		}
		if before != "c1" {
			return MessagePage{}, fmt.Errorf("asked for before=%q, want the cursor of the page before", before)
		}
		return MessagePage{Messages: []Message{{ID: "n0", Role: "assistant"}}}, nil
	}}
	p.client = ends
	if id, err := p.findDelivered(context.Background(), known, 1, "text"); id != "" || err != nil || ends.calls != 2 {
		t.Fatalf("a history of two pages: id %q, err %v, %d pages; want none found after 2", id, err, ends.calls)
	}

	empty := &stubClient{page: func(int, string) (MessagePage, error) { return MessagePage{Next: "c"}, nil }}
	p.client = empty
	if id, err := p.findDelivered(context.Background(), known, 1, "text"); id != "" || err != nil || empty.calls != 1 {
		t.Fatalf("an empty page: id %q, err %v, %d pages; want one", id, err, empty.calls)
	}

	found := &stubClient{page: func(call int, _ string) (MessagePage, error) {
		if call < 3 {
			return MessagePage{Messages: []Message{{ID: fmt.Sprintf("n%d", call), Role: "assistant"}}, Next: "c"}, nil
		}
		return MessagePage{Messages: []Message{{ID: "mine", Role: "user", Parts: []Part{{Type: "text", Text: "text"}}}}, Next: "c"}, nil
	}}
	p.client = found
	if id, err := p.findDelivered(context.Background(), known, 1, "text"); id != "mine" || err != nil || found.calls != 3 {
		t.Fatalf("found on the third page: id %q, err %v, %d pages", id, err, found.calls)
	}
}

// ---- the read-back is byte equality, of a user message ----

// TestTheReadBackConfirmsOnlyAUserMessageHoldingExactlyTheText plants, in place of
// the delivery, each message a looser comparison would take for it. The server
// accepts the post and loses it, so only what is planted can confirm, and none of
// these may: the STOP invariant is that the session holds the header-bearing text
// whole, and a prefix, a longer text, an answer or a part that is not text is not
// that.
func TestTheReadBackConfirmsOnlyAUserMessageHoldingExactlyTheText(t *testing.T) {
	text := func(h *harness) string { return h.render() }
	for name, plant := range map[string]func(string) plantedMsg{
		"the text and more after it":  func(s string) plantedMsg { return plantedMsg{role: "user", text: s + "\nextra"} },
		"the text and more before it": func(s string) plantedMsg { return plantedMsg{role: "user", text: "extra\n" + s} },
		"a prefix of the text":        func(s string) plantedMsg { return plantedMsg{role: "user", text: s[:len(s)-3]} },
		"the text without its last newline": func(s string) plantedMsg {
			return plantedMsg{role: "user", text: strings.TrimSuffix(s, "\n")}
		},
		"the text in another case":                   func(s string) plantedMsg { return plantedMsg{role: "user", text: strings.ToUpper(s)} },
		"an assistant message with exactly the text": func(s string) plantedMsg { return plantedMsg{role: "assistant", text: s} },
		"a part that is not text, with exactly the text": func(s string) plantedMsg {
			return plantedMsg{role: "user", partType: "reasoning", text: s}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			h := newHarness(t, f, func(c *Config) { c.ReadBackTimeout = 200 * time.Millisecond })
			h.peerSend(peerNick, "what must arrive whole")
			f.set(func() { f.hold = true; f.plant = []plantedMsg{plant(text(h))} })
			h.start()

			err := h.join()
			if err == nil || !strings.Contains(err.Error(), "no user message holding it appeared") {
				t.Fatalf("Run returned %v: the planted message confirmed a delivery it is not", err)
			}
			if c := h.savedCursor(); c != "" {
				t.Fatalf("a cursor %q was saved", c)
			}
		})
	}
}

// TestTheReadBackConfirmsTheExactUserMessageAmongTheLookAlikes is the control for
// the test above: all of those planted, and the exact one among them, confirms,
// and the pump goes on.
func TestTheReadBackConfirmsTheExactUserMessageAmongTheLookAlikes(t *testing.T) {
	f := newFake(t)
	h := newHarness(t, f, func(c *Config) { c.SettleTimeout = 50 * time.Millisecond })
	h.peerSend(peerNick, "what must arrive whole")
	s := h.render()
	f.set(func() {
		f.hold = true
		f.plant = []plantedMsg{
			{role: "user", text: s + "\nextra"},
			{role: "user", text: s[:len(s)-3]},
			{role: "assistant", text: s},
			{role: "user", partType: "reasoning", text: s},
			{role: "user", text: s},
		}
	})
	h.start()

	f.waitPosts(1)
	h.cursorPast("")
	if strings.Contains(h.logs.String(), "no user message holding it appeared") {
		t.Fatalf("the exact message did not confirm:\n%s", h.logs.String())
	}
}

// ---- the settle ends on the answer to the delivery, and only on that ----

// TestTheSettleEndsAtOnceOnAnAnswerToTheDelivery: the session never reads busy
// and has, already, an assistant message that answers the delivery's message. That
// is a turn that came and went, and the wait to see it has nothing left to wait
// for.
func TestTheSettleEndsAtOnceOnAnAnswerToTheDelivery(t *testing.T) {
	f := newFake(t)
	f.set(func() { f.turnPolls = 0; f.burst = 1 })
	h := newHarness(t, f, func(c *Config) { c.SettleTimeout = 5 * time.Second })
	h.start()

	h.peerSend(peerNick, "answered before it was looked for")
	f.waitPosts(1)
	posted := time.Now()
	h.cursorPast("")
	if elapsed := time.Since(posted); elapsed > time.Second {
		t.Fatalf("the delivery was committed %s after the post, against a settle bound of 5s: the answer did not end the wait", elapsed)
	}
	if strings.Contains(h.logs.String(), "did not go busy") {
		t.Fatalf("the settle ran out its bound:\n%s", h.logs.String())
	}
}

// TestTheSettleIsNotEndedByAnAnswerToAnotherMessage: an assistant message exists,
// and answers some other message, as the one before the delivery did. The turn
// the delivery started has not been seen, and the wait runs to its bound.
func TestTheSettleIsNotEndedByAnAnswerToAnotherMessage(t *testing.T) {
	f := newFake(t)
	f.set(func() { f.turnPolls = 0; f.burst = 1; f.burstParent = "msg_somebody_elses" })
	h := newHarness(t, f, func(c *Config) { c.SettleTimeout = 400 * time.Millisecond })
	h.start()

	h.peerSend(peerNick, "answered by nobody")
	f.waitPosts(1)
	posted := time.Now()
	h.cursorPast("")
	if elapsed := time.Since(posted); elapsed < 350*time.Millisecond {
		t.Fatalf("the delivery was committed %s after the post, before the settle bound of 400ms: another message's answer ended the wait", elapsed)
	}
	h.waitLog("did not go busy")
}
