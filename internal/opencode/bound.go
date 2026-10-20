package opencode

import (
	"fmt"

	"github.com/colinsurprenant/parley/internal/stream"
)

// What one delivery may hand over, because the front reads the session's history
// back to confirm it and reads that history under a bound of its own
// (maxMessageBody): a delivery the read-back cannot be shown could never be
// confirmed, and neither could it be after a restart, which would take it up
// again from the same cursor. The Codex front bounds its argument for the kernel's
// sake and this one bounds its message for its own, and both do it the same way:
// by what Render says a message comes to, and by replacing a message that does
// not fit with a stub rather than dropping it or cutting it.
const (
	// maxMessageBytes is the most one message may render to. Past it the message
	// is replaced by a stub.
	maxMessageBytes = 128 << 10

	// maxDeliveryBytes is the most a delivery hands over before the messages that
	// would take it past are replaced by stubs, each of them whole in itself: a
	// message that fits by its own measure is still a stub when the delivery has
	// no room left for it.
	maxDeliveryBytes = 512 << 10

	// maxStubbedBytes is the most a delivery comes to with its stubs counted. A
	// stub is small but not nothing, and a delivery of very many messages is very
	// many stubs: past this, the rest of the delivery is one notice.
	maxStubbedBytes = 1 << 20
)

// withheldNotice is the body of the stub that stands in for a message over
// maxMessageBytes. It is a body and nothing else: Render stamps the message's own
// header above it and indents this line as it indents every body line, so it can
// never be read as a line parley wrote at column zero. The id it names is the one
// the header above it already carries, said a second time because the sentence
// has to hold on its own — quoted out of the session, it is the whole of what a
// participant needs to go and read what was withheld. The arguments are the
// message's rendered byte count, the bound, and the transport id.
const withheldNotice = "[parley opencode] body withheld: this message renders to %d bytes, over the %d byte bound of one message on this front; the full text stays in the stream at id=%s"

// overBoundNotice is the body of the stub for a message that fits the bound of
// one message but not what is left of the delivery's. The arguments are the
// delivery's bound, the message's rendered byte count, and the transport id.
const overBoundNotice = "[parley opencode] body withheld: this delivery is over its %d byte bound, so this message, which renders to %d bytes, is not included; the full text stays in the stream at id=%s"

// elidedNotice is the body of the one stub that stands in for the rest of a
// delivery whose stubs would themselves pass maxStubbedBytes. It sits under the
// header of the first message it stands in for. The arguments are how many more
// messages follow it in the delivery, the transport id of the last of them, and
// the delivery's bound.
const elidedNotice = "[parley opencode] body withheld: this message and %d more after it, through id=%s, are not included, because this delivery is over its %d byte bound; their full text stays in the stream"

// boundedDelivery returns the delivery the pump posts for d, and how many of d's
// messages it replaced. It is d's gap marker, whole and first, and d's messages
// in order, each as it is where it fits, and otherwise as a stub: the message's
// own header, which Render stamps on a copy of it whose body is the notice, over
// one indented line. Every byte of what is posted is still Render's, of a
// Delivery, which is what keeps the peer header on everything the session reads;
// the pump assembles no line of its own.
//
// d itself is what is committed afterwards, whatever came of it here: a message
// that went over as a stub is read, handed over as a stub, and not read again.
// The full text stays in the stream, where the human and every other participant
// can still read it, and the stub says so.
func boundedDelivery(d *stream.Delivery) (out *stream.Delivery, withheld int) {
	out = &stream.Delivery{Gap: d.Gap, GapCursor: d.GapCursor}
	total := len(out.Render())
	// full is set by the first message that finds no room in the delivery, and
	// from then on every message is a stub: a small one that would still fit
	// is not let in ahead of the larger one it follows.
	full := false
	for i, m := range d.Messages {
		n := renderedLen(m)
		if !full && n <= maxMessageBytes && total+n > maxDeliveryBytes {
			full = true
		}
		if !full && n <= maxMessageBytes {
			out.Messages = append(out.Messages, m)
			total += n
			continue
		}
		stub := m
		if n > maxMessageBytes {
			stub.Body = fmt.Sprintf(withheldNotice, n, maxMessageBytes, m.ID)
		} else {
			stub.Body = fmt.Sprintf(overBoundNotice, maxDeliveryBytes, n, m.ID)
		}
		sn := renderedLen(stub)
		if rest := d.Messages[i:]; total+sn > maxStubbedBytes && len(rest) > 1 {
			stub.Body = fmt.Sprintf(elidedNotice, len(rest)-1, rest[len(rest)-1].ID, maxDeliveryBytes)
			out.Messages = append(out.Messages, stub)
			withheld += len(rest)
			break
		}
		out.Messages = append(out.Messages, stub)
		total += sn
		withheld++
	}
	return out, withheld
}

// renderedLen is what m comes to in a delivery of its own, which is what it
// comes to in any delivery: Render writes a gap marker once and then each
// message's block, one after another.
func renderedLen(m stream.Message) int {
	one := stream.Delivery{Messages: []stream.Message{m}}
	return len(one.Render())
}
