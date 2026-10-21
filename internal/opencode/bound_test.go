package opencode

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/colinsurprenant/parley/internal/stream"
)

// msgRendering builds a message that renders to exactly n bytes in a delivery of
// its own: the overhead of the header, the indent and the blank line is measured
// with Render and not counted out of its format, as the Codex tests do.
func msgRendering(t *testing.T, id string, n int) stream.Message {
	t.Helper()
	m := stream.Message{ID: id, Author: "claude-b", TS: time.Date(2026, 10, 20, 1, 2, 3, 0, time.UTC), Body: "x"}
	overhead := renderedLen(m) - 1
	if n <= overhead {
		t.Fatalf("a message renders to %d bytes around a body of one; %d is not reachable", overhead+1, n)
	}
	m.Body = strings.Repeat("x", n-overhead)
	if got := renderedLen(m); got != n {
		t.Fatalf("built a message of %d bytes, wanted %d", got, n)
	}
	return m
}

func deliveryOf(msgs ...stream.Message) *stream.Delivery {
	return &stream.Delivery{Messages: msgs}
}

// isStub reports whether m, as it stands in a bounded delivery, is a stub.
func isStub(m stream.Message) bool {
	return strings.HasPrefix(m.Body, "[parley opencode] body withheld:")
}

func TestADeliveryWithinTheBoundsGoesOverUnchanged(t *testing.T) {
	d := deliveryOf(msgRendering(t, "1", 1000), msgRendering(t, "2", maxMessageBytes), msgRendering(t, "3", 300))
	d.Gap, d.GapCursor = true, "7"
	out, withheld := boundedDelivery(d)
	if withheld != 0 {
		t.Fatalf("withheld %d message(s) of a delivery within the bounds", withheld)
	}
	if out.Render() != d.Render() {
		t.Fatalf("the bounded rendering differs from the delivery's own")
	}
}

// TestAMessageOverTheBoundIsAStubUnderItsOwnHeader: the stub is Render's of the
// message with its body replaced, so the header is the message's own, byte for
// byte, and the notice sits in body position, indented, where a header cannot
// hide.
func TestAMessageOverTheBoundIsAStubUnderItsOwnHeader(t *testing.T) {
	big := msgRendering(t, "big-1", maxMessageBytes+1)
	small := msgRendering(t, "small-2", 300)
	d := deliveryOf(small, big, small)
	d.Messages[2].ID = "small-3"
	out, withheld := boundedDelivery(d)
	if withheld != 1 || len(out.Messages) != 3 {
		t.Fatalf("withheld %d of %d message(s), want the one", withheld, len(out.Messages))
	}
	if isStub(out.Messages[0]) || isStub(out.Messages[2]) || !isStub(out.Messages[1]) {
		t.Fatalf("the stubs are in the wrong places: %+v", out.Messages)
	}
	text := out.Render()
	header := strings.SplitN(deliveryOf(big).Render(), "\n", 2)[0]
	if !strings.Contains(text, "\n"+header+"\n  [parley opencode] body withheld: this message renders to ") {
		t.Fatalf("the stub is not the message's own header over an indented notice:\n%s", text)
	}
	for _, want := range []string{
		fmt.Sprintf("renders to %d bytes, over the %d byte bound of one message on this front", maxMessageBytes+1, maxMessageBytes),
		"the full text stays in the stream at id=big-1",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("the notice does not say %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, strings.Repeat("x", 100)) && len(text) > 3000 {
		t.Fatalf("the withheld body leaked into the text (%d bytes)", len(text))
	}
	// Every line the text has at column zero is a header parley wrote.
	for _, line := range strings.Split(text, "\n") {
		if line != "" && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "[parley] from=") {
			t.Fatalf("a line at column zero that is not a header: %q", line)
		}
	}
}

func TestMessagesPastTheDeliveryBoundAreStubsAndOnesAfterItToo(t *testing.T) {
	// Four messages that each fit, the third of which does not fit what the first
	// two left; the fourth is small enough to fit the room that is left, and is a
	// stub all the same: once the delivery is full, it is full.
	a := msgRendering(t, "a", 120<<10)
	b := msgRendering(t, "b", 120<<10)
	c := msgRendering(t, "c", 120<<10)
	d4 := msgRendering(t, "d", 120<<10)
	e := msgRendering(t, "e", 120<<10)
	tiny := msgRendering(t, "tiny", 300)
	out, withheld := boundedDelivery(deliveryOf(a, b, c, d4, e, tiny))
	// 4 x 120 KiB = 480 KiB fits in 512 KiB; the fifth would take it to 600.
	if withheld != 2 {
		t.Fatalf("withheld %d, want 2 (e and tiny)", withheld)
	}
	for i, wantStub := range []bool{false, false, false, false, true, true} {
		if isStub(out.Messages[i]) != wantStub {
			t.Fatalf("message %d (%s): stub = %v, want %v", i, out.Messages[i].ID, isStub(out.Messages[i]), wantStub)
		}
	}
	text := out.Render()
	if want := fmt.Sprintf("this delivery is over its %d byte bound, so this message, which renders to %d bytes, is not included; the full text stays in the stream at id=e", maxDeliveryBytes, renderedLen(e)); !strings.Contains(text, want) {
		t.Fatalf("the over-the-bound notice is not there:\n%s", text[len(text)-1200:])
	}
	if len(text) > maxDeliveryBytes {
		t.Fatalf("the delivery is %d bytes, over its bound %d", len(text), maxDeliveryBytes)
	}
}

func TestTheGapMarkerGoesFirstAndWhole(t *testing.T) {
	big := msgRendering(t, "big", maxMessageBytes+10)
	d := deliveryOf(big, msgRendering(t, "ok", 300))
	d.Gap, d.GapCursor = true, "9000"
	out, _ := boundedDelivery(d)
	marker := (&stream.Delivery{Gap: true, GapCursor: "9000"}).Render()
	if !strings.HasPrefix(out.Render(), marker) || !out.Gap || out.GapCursor != "9000" {
		t.Fatalf("the gap marker is not first and whole:\n%s", out.Render())
	}
	// The marker is charged to the delivery's room: four messages that fill the
	// bound exactly fit without it, and with it the last is a stub.
	full := deliveryOf(msgRendering(t, "a", 128<<10), msgRendering(t, "b", 128<<10), msgRendering(t, "c", 128<<10), msgRendering(t, "e", 128<<10))
	if out, withheld := boundedDelivery(full); withheld != 0 || len(out.Render()) != maxDeliveryBytes {
		t.Fatalf("without a marker: withheld %d, %d bytes, want none and exactly %d", withheld, len(out.Render()), maxDeliveryBytes)
	}
	full.Gap, full.GapCursor = true, "9000"
	if out, withheld := boundedDelivery(full); withheld != 1 || !isStub(out.Messages[3]) {
		t.Fatalf("with a marker: withheld %d, want the last message alone as a stub", withheld)
	}
}

// TestAHugeDeliveryIsBoundedStubsIncluded: past maxStubbedBytes the rest is one
// notice, so what a delivery of any number of messages hands over is bounded by
// a constant. The read-back that confirms it reads the session's history under a
// bound of 16 MiB; this is what keeps a delivery from ever outgrowing that.
func TestAHugeDeliveryIsBoundedStubsIncluded(t *testing.T) {
	var msgs []stream.Message
	for i := 0; i < 20000; i++ {
		msgs = append(msgs, msgRendering(t, fmt.Sprintf("m-%05d", i), 600))
	}
	d := deliveryOf(msgs...)
	out, withheld := boundedDelivery(d)
	text := out.Render()
	if len(text) > maxStubbedBytes+2000 {
		t.Fatalf("a delivery of %d messages came to %d bytes, want it under %d", len(msgs), len(text), maxStubbedBytes+2000)
	}
	if withheld == 0 || withheld >= len(msgs) {
		t.Fatalf("withheld %d of %d", withheld, len(msgs))
	}
	last := out.Messages[len(out.Messages)-1]
	if !isStub(last) || !strings.Contains(last.Body, "and ") || !strings.Contains(last.Body, "through id=m-19999") {
		t.Fatalf("the last message is not the notice for the rest: %q", last.Body)
	}
	// Every message is accounted for: those kept, the stubs, and those the one
	// notice stands in for.
	kept, stubs := 0, 0
	for _, m := range out.Messages {
		if isStub(m) {
			stubs++
		} else {
			kept++
		}
	}
	if kept+withheld != len(msgs) {
		t.Fatalf("kept %d + withheld %d != %d (stubs %d)", kept, withheld, len(msgs), stubs)
	}
	// And the kept ones are the first ones, in order.
	for i := 0; i < kept; i++ {
		if out.Messages[i].ID != msgs[i].ID {
			t.Fatalf("message %d is %s, want %s", i, out.Messages[i].ID, msgs[i].ID)
		}
	}
}

// TestTheWholeOfThePostedTextIsRendersOutput: whatever was withheld, the text is
// the rendering of a Delivery, so the peer header is on every message the session
// reads and nothing is assembled by hand.
func TestTheWholeOfThePostedTextIsRendersOutput(t *testing.T) {
	d := deliveryOf(msgRendering(t, "1", maxMessageBytes+1), msgRendering(t, "2", 400))
	out, _ := boundedDelivery(d)
	rebuilt := (&stream.Delivery{Gap: out.Gap, GapCursor: out.GapCursor, Messages: out.Messages}).Render()
	if out.Render() != rebuilt {
		t.Fatal("the bounded delivery does not render as the Delivery it is")
	}
	if n := strings.Count(out.Render(), "[parley] from=claude-b id="); n != 2 {
		t.Fatalf("%d headers, want one per message", n)
	}
}
