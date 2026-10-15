package irc

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/colinsurprenant/parley/internal/stream"
)

// page is one CHATHISTORY reply: the messages it held, and what it says about
// the history around them.
type page struct {
	msgs []stream.Message
	// items is how many history items the batch carried, messages or not. It is
	// what tells an empty reply from one holding nothing deliverable: a page of
	// JOINs is not a gap.
	items int
	// itemIDs are the msgids of those items, in order, minus any this adapter
	// cannot ask the server about again. They are what the next page is
	// anchored on, so paging steps over a JOIN rather than round it.
	itemIDs []string
	// end is the draft/chathistory-end tag: the server says there is nothing
	// further in the direction that was asked.
	end bool
}

func (p page) first() string {
	if len(p.itemIDs) == 0 {
		return ""
	}
	return p.itemIDs[0]
}

func (p page) last() string {
	if len(p.itemIDs) == 0 {
		return ""
	}
	return p.itemIDs[len(p.itemIDs)-1]
}

// anchor is the msgid the next page is asked for, at whichever end of this one,
// and — on the newest end — the position a read resumes from.
//
// A page with items but no usable anchor is a page this adapter cannot walk
// past: every item in it carries a msgid the server will not resolve — its own
// synthetic narration — so asking for what is before or after it would come
// back empty and read as the end of the history, and resuming from it is not
// possible at all. Either way it is an error rather than something done
// quietly, and the caller's cursor stays where it is.
func (p page) anchor(end string) (string, error) {
	var id string
	if end == "oldest" {
		id = p.first()
	} else {
		id = p.last()
	}
	if id == "" {
		if p.end {
			return "", fmt.Errorf("irc: the %s history item the server returned carries no id parley can page from, so there is no position to read on from", end)
		}
		return "", fmt.Errorf("irc: the %s history item the server returned carries no id parley can page from, and the server says there is more beyond it", end)
	}
	return id, nil
}

func (p page) holds(msgid string) bool {
	for _, id := range p.itemIDs {
		if id == msgid {
			return true
		}
	}
	return false
}

// limit is the page size to ask for, as the server's 005 advertised it.
func (c *conn) limit() string { return strconv.Itoa(c.histLimit) }

// nextBatchID returns an id for an outgoing batch, unique on this connection.
func (c *conn) nextBatchID() string {
	return "p" + strconv.FormatUint(c.labels.Add(1), 36)
}

// chathistory runs one CHATHISTORY query and decodes the batch it answers with.
func (c *conn) chathistory(ctx context.Context, args ...string) (page, error) {
	lines, err := c.request(ctx, func(label string) []line {
		return []line{{
			tags:    map[string]string{"label": label},
			command: "CHATHISTORY",
			params:  args,
		}}
	})
	if err != nil {
		return page{}, err
	}
	return c.decodePage(lines)
}

// newest returns the last item the server still holds for the channel, or ""
// when it holds none. It is one item, and it is asked for by position rather
// than anchored on a msgid, which is what makes it usable as the gap check's
// first step: no anchor means nothing to be wrong about the anchor.
func (c *conn) newest(ctx context.Context) (string, error) {
	p, err := c.chathistory(ctx, "LATEST", c.channel, "*", "1")
	if err != nil {
		return "", err
	}
	return p.last(), nil
}

// decodePage turns a labeled reply into a page. The reply is a flat list of
// lines in arrival order; what makes it a tree is the batch tag, and the only
// nesting that matters is a multiline message inside the history batch.
func (c *conn) decodePage(lines []line) (page, error) {
	var p page
	if len(lines) == 0 {
		return p, errors.New("irc: the server answered CHATHISTORY with nothing")
	}
	head := lines[0]
	if head.command != "BATCH" {
		return p, fmt.Errorf("irc: history: %s", replyText(head))
	}
	root := strings.TrimPrefix(head.param(0), "+")
	p.end = head.hasTag("draft/chathistory-end")

	var ml *multiline
	for _, l := range lines[1:] {
		switch {
		case l.command == "BATCH" && strings.HasPrefix(l.param(0), "+"):
			// A nested batch is one history item, and its msgid and time are on
			// this line rather than on any of the lines it holds.
			p.record(l)
			ml = openMultiline(l)
		case l.command == "BATCH" && strings.HasPrefix(l.param(0), "-"):
			if ml != nil && strings.TrimPrefix(l.param(0), "-") == ml.batchID {
				if m, ok := ml.message(c.channel); ok {
					p.msgs = append(p.msgs, m)
				}
				ml = nil
			}
		case ml != nil && l.tags["batch"] == ml.batchID:
			ml.add(l)
		case l.tags["batch"] == root:
			p.record(l)
			if m, ok := message(l, c.channel); ok {
				p.msgs = append(p.msgs, m)
			}
		}
	}
	return p, nil
}

// record counts a history item and keeps its msgid if the server can be asked
// about it again. Ergo stamps its own synthetic replies (HistServ) with a msgid
// starting '_', which no later query resolves: anchoring a page on one would
// come back empty and read as a gap. With draft/event-playback required, ergo
// never sends one at all; should one appear anyway, a cursor left before it
// only re-reads an item nothing delivers.
func (p *page) record(l line) {
	p.items++
	if id := l.tags["msgid"]; id != "" && !strings.HasPrefix(id, "_") {
		p.itemIDs = append(p.itemIDs, id)
	}
}

// multiline assembles a draft/multiline batch into one message. Its identity —
// msgid, time, author — is on the BATCH line; the PRIVMSG lines carry only text.
type multiline struct {
	batchID string
	target  string
	head    line
	b       strings.Builder
	n       int
}

func openMultiline(l line) *multiline {
	if l.param(1) != "draft/multiline" {
		return nil
	}
	return &multiline{
		batchID: strings.TrimPrefix(l.param(0), "+"),
		target:  l.param(2),
		head:    l,
	}
}

// add appends one line of the message. A line tagged draft/multiline-concat
// continues the line before it; any other line starts a new one, and an empty
// one is a blank line in the body rather than nothing.
func (m *multiline) add(l line) {
	if l.command != "PRIVMSG" || len(l.params) < 2 {
		return
	}
	if m.n > 0 && !l.hasTag("draft/multiline-concat") {
		m.b.WriteByte('\n')
	}
	m.b.WriteString(l.trailing())
	m.n++
}

func (m *multiline) message(channel string) (stream.Message, bool) {
	if m == nil || !asciiEqualFold(m.target, channel) {
		return stream.Message{}, false
	}
	return buildMessage(m.head, m.b.String())
}

// message decodes one plain PRIVMSG to the channel.
func message(l line, channel string) (stream.Message, bool) {
	if l.command != "PRIVMSG" || !asciiEqualFold(l.param(0), channel) {
		return stream.Message{}, false
	}
	return buildMessage(l, l.trailing())
}

// buildMessage stamps a body with the identity the server gave it. A message
// with no msgid is not deliverable — the id is the cursor and the
// de-duplication key — so it is dropped rather than delivered with an id parley
// made up.
func buildMessage(head line, body string) (stream.Message, bool) {
	id := head.tags["msgid"]
	if id == "" || strings.HasPrefix(id, "_") {
		return stream.Message{}, false
	}
	text, ok := decodeBody(body)
	if !ok {
		return stream.Message{}, false
	}
	return stream.Message{
		ID:     id,
		Author: nickFromPrefix(head.prefix),
		TS:     parseTime(head.tags["time"]),
		Body:   text,
	}, true
}

// decodeBody unwraps CTCP. ACTION is the one a channel really carries, and it
// is delivered as the `/me text` every client renders it as; any other CTCP is
// a client-to-client request, not something a participant said.
func decodeBody(body string) (string, bool) {
	if !strings.HasPrefix(body, "\x01") {
		return body, true
	}
	// The trailing delimiter is optional in the CTCP spec and several real
	// clients leave it off, so it is trimmed if it is there and not required to
	// be: a /me from one of those clients is still a /me.
	s := strings.TrimSuffix(strings.TrimPrefix(body, "\x01"), "\x01")
	if s == "ACTION" {
		return "/me", true
	}
	if rest, ok := strings.CutPrefix(s, "ACTION "); ok {
		return "/me " + rest, true
	}
	return "", false
}

// parseTime reads the server-time tag. Ergo spells it with exactly three
// fractional digits; RFC3339Nano covers every other spelling of the same
// instant. A stamp that parses as neither leaves the zero time, which is how a
// message with no usable time still reaches the participant.
func parseTime(v string) time.Time {
	if v == "" {
		return time.Time{}
	}
	if ts, err := time.Parse(ergoTimeLayout, v); err == nil {
		return ts.UTC()
	}
	if ts, err := time.Parse(time.RFC3339Nano, v); err == nil {
		return ts.UTC()
	}
	return time.Time{}
}

// chunk is one PRIVMSG line of an outgoing message. concat marks a line that
// continues the one before it rather than starting a new one.
type chunk struct {
	text   string
	concat bool
}

// frame cuts a body into the lines of a multiline batch and checks it against
// what the server said it would accept. The check is on the framed form, not
// the body: max-lines counts the lines on the wire, which is what a long line
// cut into chunks turns into.
func (c *conn) frame(body string) ([]chunk, error) {
	if len(body) > c.maxBytes {
		return nil, fmt.Errorf("irc: message body is %d bytes, the server accepts %d", len(body), c.maxBytes)
	}
	limit := c.chunkSize()
	var out []chunk
	for _, ln := range strings.Split(body, "\n") {
		if ln == "" {
			out = append(out, chunk{})
			continue
		}
		for first := true; ln != ""; first = false {
			n := cutRunes(ln, limit)
			out = append(out, chunk{text: ln[:n], concat: !first})
			ln = ln[n:]
		}
	}
	if len(out) > c.maxLines {
		return nil, fmt.Errorf("irc: message body needs %d lines, the server accepts %d", len(out), c.maxLines)
	}
	return out, nil
}

// chunkSize is how much text one PRIVMSG line may carry: what is left of the
// 512-byte line once the server has stamped this connection's own prefix and
// the channel on it, and never more than maxChunk.
//
// The tags a continuation line carries — batch, draft/multiline-concat — are
// not part of this budget and do not need to be: a server sizes the two
// separately, ergo's irc-go by MaxlenTags (8191) for everything before the
// prefix and DefaultMaxLineLen (512) for the rest. Counting tags against the
// 512 would shrink every chunk for nothing.
func (c *conn) chunkSize() int {
	left := maxRestLen - len("\r\n") - len(":") - len(c.prefix) - len(" PRIVMSG ") - len(c.channel) - len(" :")
	return max(1, min(maxChunk, left))
}

// cutRunes returns the largest prefix of s that fits in limit bytes without
// splitting a rune. A single rune longer than the limit is still taken whole:
// half a rune is not a smaller message, it is a broken one.
func cutRunes(s string, limit int) int {
	if len(s) <= limit {
		return len(s)
	}
	n := limit
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	if n == 0 {
		_, size := utf8.DecodeRuneInString(s)
		return size
	}
	return n
}
