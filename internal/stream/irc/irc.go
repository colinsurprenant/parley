// Package irc implements the parley stream over one IRC channel on a server
// that speaks IRCv3, which in practice means a loopback ergo (see
// contrib/ergo/). It is the transport a human can join from a stock client
// while two agents talk on it.
//
// The server is the sequencing authority, and the only one: a message's ID is
// the IRCv3 msgid the server minted, its TS is the server-time tag, and the
// order is the order CHATHISTORY replays. Nothing is ordered or stamped here,
// so a line typed into WeeChat and a line sent by parley are the same kind of
// message, told apart only by their author.
//
// A cursor is a msgid — the one the server gave the last message Read returned.
// Read asks CHATHISTORY AFTER for what follows it. Ergo answers an unknown or
// expired msgid with an empty batch and no FAIL, which is the same answer as
// "nothing new", so an empty result has to be resolved by asking something
// else. The check is two steps and anchors on nothing:
//
//  1. CHATHISTORY LATEST <chan> * 1 — the newest item the server holds. If it
//     is the cursor, the cursor is retained and is the end of the history:
//     nothing new, no gap.
//  2. Otherwise something newer exists, so ask AFTER <cursor> again. If the
//     cursor is retained, that newer item is after it and comes back — those
//     are the messages, and there is no gap. Only an AFTER that is still empty
//     while a newer item exists means the cursor itself is gone, which is the
//     gap.
//
// A message landing between the two steps cannot make a false gap: it only
// makes step 1 report a newer item, and step 2 then returns it. The reverse —
// the cursor expiring between the steps — reports a gap, which is what it is.
//
// CHATHISTORY AROUND <cursor> 1 looks like a shorter way to ask the same
// question and is not: ergo builds AROUND out of a query for the item strictly
// before the anchor, so an anchor that is the oldest item the server still
// holds comes back empty, and the cursor of a participant that is merely up to
// date would be read as expired. BETWEEN msgid=c msgid=c is useless too: ergo
// answers it with an empty batch whether or not c is retained.
//
// A message is a PRIVMSG to the channel, or a draft/multiline batch of them,
// which is one message with one msgid and one history item — Send always uses
// one, even for a single line, so that a body never becomes several messages a
// reader could interleave. Long lines are cut at rune boundaries into chunks
// that fit a 512-byte line once the server has stamped its prefix on them, and
// the continuation chunks carry draft/multiline-concat, so a body arrives byte
// for byte however long its lines are. An empty line travels as a PRIVMSG with
// an empty trailing parameter, which is legal inside a multiline batch (and
// only there: on its own it is ERR_NOTEXTTOSEND).
//
// Everything else on the channel is not a message and is dropped: JOIN, PART,
// QUIT, MODE, NOTICE, TAGMSG, a history item with no msgid or a synthetic one,
// and CTCP other than ACTION, which is delivered as the `/me text` a client
// would show. A synthetic item is ergo's own narration of an event — HistServ
// replies like "alpha joined the channel", which are what a client that did not
// negotiate draft/event-playback gets in place of the JOIN line, and which are
// marked by a msgid starting '_'. They are dropped both as messages and as
// anchors: no later query resolves one, so paging from it would come back empty
// and read as a gap.
//
// Every connection logs into the participant's own account with SASL PLAIN,
// and that account is what makes a nick something several parley processes can
// hold at once: the long-lived pump blocked in Wait and the one-shot send that
// answers it are two sessions of one client, not two clients colliding on 433.
// There is no password for anyone to choose or lose sight of — the first
// connection for a (nick, server) registers the account with 32 random bytes
// and stores them 0600 under the participant's state directory, at
// <state>/<nick>/irc/<host>_<port>/password, and every connection after it
// reads them back. The account name is the nick, so the server's nick
// reservation refuses the nick to anyone else. If that file is lost while the
// account remains, parley says so and names the account: the account has to be
// dropped on the server before the nick can be used again.
//
// The participant is always-on, which is what keeps the room alive: ergo holds
// an unregistered channel only while it has members, so without it a channel
// whose last connection dropped would take its history with it. An always-on
// participant stays in the channel between connections, and the history is
// there for the next one.
//
// The adapter holds one connection, dialed on first use and re-dialed once by
// the next call if it dies. One goroutine owns the socket: it answers PING,
// hands each labeled reply to the request waiting for it, and wakes Wait when a
// live message lands. That wake-up is only a wake-up — every message a
// participant sees comes from CHATHISTORY, so a message that arrives while
// nobody is waiting is not lost, and the live stream never has to be trusted as
// a second source of order.
//
// Boundaries. Loopback only, and enforced twice: ParseSpec takes 127.0.0.0/8,
// ::1 and localhost and nothing else, and dial checks what the name actually
// resolved to before a password goes anywhere near the socket. The reason is
// the posture — plaintext TCP with the account password sent as SASL PLAIN over
// it, no TLS, no certificates, no other mechanism — which is acceptable exactly
// as far as the bytes never leave the machine that wrote them. A remote server
// would need transport security this adapter does not have, so it is refused
// rather than discouraged; a password anyone types is likewise out of scope.
//
// One channel per adapter. There is no reconnect loop: a dead connection is
// re-dialed once per call, and a server that stays down is an error the caller
// sees. The nick this adapter carries is a parley nick unchanged: Open checks
// it against stream.ValidNick and adds no rule of its own, so what a particular
// server will take is the server's to say — that grammar admits Unicode letters
// and digits, and a server free to refuse some of them does so at the end of
// the handshake, where the failure is reported with the server's own reply.
//
// One call is not bounded by its caller: a Send or a Read that has to open the
// connection first spends up to the registration timeout inside the handshake
// — a dial, the capability exchange, an account registration or a SASL
// exchange, a JOIN — if the server accepts the socket and then goes quiet. A
// caller that needs a bound must pass a context with a deadline; Wait bounds its
// own handshake, because its whole contract is a bound. Once a connection is up,
// every request is bounded by the adapter itself.
package irc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/colinsurprenant/parley/internal/stream"
)

// maxChunk bounds one PRIVMSG line inside a multiline batch, in bytes. The
// budget is the 512-byte line the server sends to every reader, spent on the
// prefix it stamps on our message:
//
//	512 − 2 (CRLF) − 1 (":") − 64 (nick) − 1 ("!") − 21 (user) − 1 ("@")
//	    − 63 (host) − 9 (" PRIVMSG ") − 64 (channel) − 2 (" :")  =  284
//
// which is the worst case; the real prefix is known once the connection is
// joined, so chunkSize spends what is actually left and stops at 300 — a round
// number well inside the budget for any ordinary nick, and the size the tests
// pin. Clients that do not understand draft/multiline see the chunks as
// separate lines, which is what a human in WeeChat sees for a long message.
const maxChunk = 300

// maxPages bounds how many CHATHISTORY pages one Read walks. Retained history
// is finite, so paging ends on its own; the bound is there so that a server
// answering a page with the same page cannot spin a Read for ever. It is a
// variable only so that a test can lower it; nothing changes it at run time.
var maxPages = 64

// maxCursor bounds a cursor, in bytes. A msgid is 26 characters on ergo; the
// bound is there so that a stored cursor cannot become an unbounded parameter
// on a line with a length limit of its own.
const maxCursor = 128

// ergoTimeLayout is how ergo spells the server-time tag. RFC3339Nano parses it
// too and is the fallback for any other server's spelling of the same instant.
const ergoTimeLayout = "2006-01-02T15:04:05.000Z"

// Stream is one IRC channel, read and written as a parley stream. The zero
// value is not usable; call Open.
type Stream struct {
	spec  Spec
	nick  string
	creds Credentials

	// sem guards conn and closed, and is held across the dial that fills them:
	// one adapter holds one connection, and two callers arriving together must
	// not open two. It is a channel rather than a mutex so that the wait for it
	// can be given up on — a caller whose deadline passed while another was in
	// a handshake gets its own error, not the tail of someone else's wait.
	sem    chan struct{}
	conn   *conn
	closed bool
}

// Open prepares the stream for the participant nick, with creds holding the
// account password every connection for that nick authenticates with. It does
// not connect: the socket is dialed by the first Send, Read or Wait, so
// constructing an adapter for a server that is not running fails where the
// failure belongs, in the call that needed it.
//
// The credential store is not optional. Without it a connection could only join
// anonymously, and the second parley process for a participant would be refused
// the nick — which is the whole of what the account is for.
func Open(spec Spec, nick string, creds Credentials) (*Stream, error) {
	if spec.Addr == "" || spec.Channel == "" {
		return nil, errors.New("irc: incomplete stream spec")
	}
	if err := stream.ValidNick(nick); err != nil {
		return nil, fmt.Errorf("irc: nick %q %v", nick, err)
	}
	if creds == nil {
		return nil, errors.New("irc: no credential store for the account this participant connects with")
	}
	return &Stream{spec: spec, nick: nick, creds: creds, sem: make(chan struct{}, 1)}, nil
}

// Spec returns the canonical spelling of what this stream is.
func (s *Stream) Spec() string { return s.spec.String() }

// Close sends QUIT and drops the connection, so the server frees the nick
// immediately rather than holding it until the socket times out. A stream that
// was never used closes without touching the network, and a closed stream is
// not reopened.
func (s *Stream) Close() error {
	// No context: closing is what a caller does when it is finished, and it
	// waits for a handshake in flight rather than leaving a connection nobody
	// owns. Nothing else blocks here — a wait blocked on the connection is
	// released by the quit below, not by this.
	s.sem <- struct{}{}
	c := s.conn
	s.conn, s.closed = nil, true
	<-s.sem
	if c == nil {
		return nil
	}
	return c.quit()
}

// ensure returns the live connection, dialing and registering one if there is
// none or the one there has died. One dial per call: a server that is down
// stays down for this call, and the caller decides whether to try again.
func (s *Stream) ensure(ctx context.Context) (*conn, error) {
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if s.closed {
		return nil, errors.New("irc: stream is closed")
	}
	if s.conn != nil {
		if !s.conn.isDead() {
			return s.conn, nil
		}
		s.conn = nil
	}
	c, err := dial(ctx, s.spec.Addr, s.nick, s.spec.Channel, s.creds)
	if err != nil {
		return nil, err
	}
	s.conn = c
	return c, nil
}

// Send posts one message as a draft/multiline batch and waits for the server to
// say what became of it: the echo of our own message means it is in the
// channel's history with an id, and a FAIL or an error numeric means it is not.
// The message returned carries the msgid and time the echo's BATCH line holds,
// which are the ID and TS a reader gets from history. An echo that lacks either
// still means the message is posted, so it is not an error: the field comes back
// empty, and the caller says so rather than fail a post that already happened.
func (s *Stream) Send(ctx context.Context, body string) (stream.Message, error) {
	if err := ctx.Err(); err != nil {
		return stream.Message{}, err
	}
	// Checked before the connection is touched: these are faults in the body,
	// not in the transport, and they read the same whether a server is up.
	if body == "" {
		return stream.Message{}, errors.New("irc: empty message body")
	}
	if i := strings.IndexByte(body, 0); i >= 0 {
		return stream.Message{}, fmt.Errorf("irc: message body holds a NUL byte at offset %d; an IRC line cannot carry one", i)
	}
	if i := strings.IndexByte(body, '\r'); i >= 0 {
		return stream.Message{}, fmt.Errorf("irc: message body holds a carriage return at offset %d; a line ends at one, so it cannot travel inside a message", i)
	}
	// A body wrapped in 0x01 is CTCP to every client that reads it, and this
	// adapter delivers only the one CTCP that is speech (ACTION, as /me): a
	// message sent like this would be shown to the human and dropped by every
	// parley reader, which is worse than not sending it.
	if i := strings.IndexByte(body, 0x01); i >= 0 {
		return stream.Message{}, fmt.Errorf("irc: message body holds the CTCP delimiter 0x01 at offset %d; it would reach a human's client as a client command and no parley reader as a message", i)
	}
	c, err := s.ensure(ctx)
	if err != nil {
		return stream.Message{}, err
	}
	chunks, err := c.frame(body)
	if err != nil {
		return stream.Message{}, err
	}

	batchID := c.nextBatchID()
	resp, err := c.request(ctx, func(label string) []line {
		out := make([]line, 0, len(chunks)+2)
		out = append(out, line{
			tags:    map[string]string{"label": label},
			command: "BATCH",
			params:  []string{"+" + batchID, "draft/multiline", c.channel},
		})
		for _, ch := range chunks {
			tags := map[string]string{"batch": batchID}
			if ch.concat {
				tags["draft/multiline-concat"] = ""
			}
			out = append(out, line{tags: tags, command: "PRIVMSG", params: []string{c.channel, ch.text}, colon: true})
		}
		return append(out, line{command: "BATCH", params: []string{"-" + batchID}})
	})
	if err != nil {
		return stream.Message{}, err
	}
	if len(resp) == 0 {
		return stream.Message{}, errors.New("irc: the server acknowledged the message with nothing")
	}
	// The echo is the receipt: a BATCH opening our own message back at us. Any
	// other reply to the label is the server refusing it, and says why.
	first := resp[0]
	if first.command != "BATCH" {
		return stream.Message{}, fmt.Errorf("irc: the server refused the message: %s", replyText(first))
	}
	// The echo came back as this request's own reply rather than as live
	// traffic, so nothing else saw it land. A Wait on this adapter is waiting
	// for a message after its cursor, and this is one.
	c.wake()
	return stream.Message{
		ID:     first.tags["msgid"],
		Author: s.nick,
		TS:     parseTime(first.tags["time"]),
		Body:   body,
	}, nil
}

// Read returns the messages after cursor, in the order the server replays them.
// A zero cursor is the start of retained history, which is walked backwards
// page by page and assembled in order. It does not block beyond the round trips
// it takes.
func (s *Stream) Read(ctx context.Context, cursor stream.Cursor) ([]stream.Message, stream.Cursor, error) {
	if err := ctx.Err(); err != nil {
		return nil, cursor, err
	}
	// Checked before anything is opened: a cursor this adapter could not have
	// issued is the caller's mistake, and finding that out should not cost a
	// connection to a server that may not even be running.
	if err := checkCursor(cursor); err != nil {
		return nil, cursor, err
	}
	c, err := s.ensure(ctx)
	if err != nil {
		return nil, cursor, err
	}
	if cursor == "" {
		return s.readHistory(ctx, c)
	}
	return s.readAfter(ctx, c, cursor)
}

// readHistory returns everything the server still retains. A page that fails
// takes the whole read with it: the pages are assembled oldest-last, so
// returning a partial one would advance the cursor past messages the earlier
// pages have not brought back yet, and those would never be delivered.
func (s *Stream) readHistory(ctx context.Context, c *conn) ([]stream.Message, stream.Cursor, error) {
	p, err := c.chathistory(ctx, "LATEST", c.channel, "*", c.limit())
	if err != nil {
		return nil, "", err
	}
	out := p.msgs
	// The newest page is the first one read, so the position to resume from is
	// settled here; the pages behind it are older and move nothing. A page with
	// items and no id to resume from leaves the cursor at zero, and a zero
	// cursor replays the whole history on the next read and the one after that
	// — which is worse than saying so, terminal page or not.
	if p.items > 0 {
		if _, err := p.anchor("newest"); err != nil {
			return nil, "", err
		}
	}
	advance := p.last()
	for pages := 1; !p.end; pages++ {
		// Ergo marks every page it cannot fill — an empty one included — as the
		// end of the history. A page that is short of that mark and holds
		// nothing is a server saying both that there is more and that there is
		// none, and reading on from it would mean guessing which.
		if p.items == 0 {
			return nil, "", fmt.Errorf("irc: the server returned an empty page of %s's history without saying it was the end of it", c.channel)
		}
		oldest, err := p.anchor("oldest")
		if err != nil {
			return nil, "", err
		}
		if pages >= maxPages {
			// Returning what was collected would advance the cursor past
			// history that is still there and was never delivered — a silent
			// version of the gap this adapter exists to report. The caller is
			// told instead, and its cursor does not move.
			return nil, "", fmt.Errorf("irc: %s holds more retained history than %d pages of %s items; parley will not read it in one pass", c.channel, maxPages, c.limit())
		}
		next, err := c.chathistory(ctx, "BEFORE", c.channel, "msgid="+oldest, c.limit())
		if err != nil {
			return nil, "", err
		}
		// A page that begins where the one before it began is a server that
		// ignored the anchor. Taking it would repeat what is already collected;
		// stopping would leave the cursor past history never read. Neither is
		// an answer, so neither is given.
		if next.first() == oldest {
			return nil, "", fmt.Errorf("irc: the server answered for %s's history before %s with a page that starts at %s again", c.channel, oldest, oldest)
		}
		out = append(next.msgs, out...)
		p = next
	}
	return out, cursorAt(advance, ""), nil
}

// readAfter returns what followed the cursor, paging forward. Partial progress
// is kept here: the pages come back oldest-first, so messages already collected
// are in order and the cursor just past them is a position the next call can
// resume from.
func (s *Stream) readAfter(ctx context.Context, c *conn, cursor stream.Cursor) ([]stream.Message, stream.Cursor, error) {
	p, err := c.chathistory(ctx, "AFTER", c.channel, "msgid="+string(cursor), c.limit())
	if err != nil {
		return nil, cursor, err
	}
	if p.items == 0 && !p.end {
		// A short page is a page the server marks as the end of the history,
		// and an empty one is the shortest there is. Without that mark it is
		// saying both that there is more and that there is none, and the gap
		// check below cannot tell which it meant.
		return nil, cursor, fmt.Errorf("irc: the server returned an empty page of %s's history after the cursor without saying it was the end of it", c.channel)
	}
	if p.items == 0 {
		// Nothing after the cursor, or no cursor left to be after: ergo answers
		// both with an empty batch and no failure, so the two have to be told
		// apart by asking a different question.
		newest, err := c.newest(ctx)
		if err != nil {
			return nil, cursor, err
		}
		if newest == string(cursor) {
			// The cursor is the last thing the server holds. Nothing was lost
			// and nothing is new.
			return nil, cursor, nil
		}
		// Something else is the newest item, so either the cursor expired or a
		// message landed between the two queries. Asking again separates them:
		// a retained cursor must now have that newer item after it.
		p, err = c.chathistory(ctx, "AFTER", c.channel, "msgid="+string(cursor), c.limit())
		if err != nil {
			return nil, cursor, err
		}
		if p.items == 0 {
			return nil, "", &stream.HistoryGapError{Cursor: cursor, Restart: ""}
		}
	}
	out := p.msgs
	// A page with items and nothing to resume from would have this read return
	// the caller's own cursor back to it, and the same page would come again on
	// every read after this one.
	if p.items > 0 {
		if _, err := p.anchor("newest"); err != nil {
			return out, cursor, err
		}
	}
	advance := p.last()
	// Stopping at maxPages loses nothing: the cursor returned is past the last
	// item collected, so the next call resumes from it and reads the rest. It
	// is readHistory that has to refuse instead — a zero cursor has no position
	// to resume from, so a partial answer there would move the cursor past
	// history nothing would ever come back for.
	for n := 1; !p.end && n < maxPages; n++ {
		if err := ctx.Err(); err != nil {
			return out, cursorAt(advance, cursor), err
		}
		if p.items == 0 {
			return out, cursorAt(advance, cursor), fmt.Errorf("irc: the server returned an empty page of %s's history after %s without saying it was the end of it", c.channel, string(cursorAt(advance, cursor)))
		}
		last, err := p.anchor("newest")
		if err != nil {
			return out, cursorAt(advance, cursor), err
		}
		next, err := c.chathistory(ctx, "AFTER", c.channel, "msgid="+last, c.limit())
		if err != nil {
			return out, cursorAt(advance, cursor), err
		}
		// The anchor is what the page comes after, so finding it at the head of
		// that page means the server answered with the anchor and whatever
		// followed it — taking that would deliver the anchor's message a second
		// time. Nothing of this page is taken and the position stays where the
		// last good page left it.
		if next.first() == last {
			return out, cursorAt(advance, cursor), fmt.Errorf("irc: the server answered for %s's history after %s with a page that begins at %s", c.channel, last, last)
		}
		if next.last() == last {
			break
		}
		out = append(out, next.msgs...)
		if id := next.last(); id != "" {
			advance = id
		}
		p = next
	}
	return out, cursorAt(advance, cursor), nil
}

// Wait blocks for messages after cursor. The change signal is the server's own
// push — a live PRIVMSG on the channel — and the messages themselves still come
// from CHATHISTORY, so what Wait returns is what Read would have returned, just
// later. It returns no later than timeout+debounce: the quiet window can delay
// the return, but only once, and a read still running when the bound arrives is
// cut short and gives back what it had.
//
// A wake-up that turns out to be nothing — a line on the channel that is not a
// message, which is most of what an IRC channel carries — does not end the
// wait: it goes back to waiting for the rest of its time.
func (s *Stream) Wait(ctx context.Context, cursor stream.Cursor, timeout, debounce time.Duration) ([]stream.Message, stream.Cursor, error) {
	deadline := time.Now().Add(timeout)
	hard := deadline.Add(debounce)
	// The bound binds the reads too, and a read cut short by it is the bound
	// working rather than a failure: it returns what it had, and the caller's
	// own cancellation is still told apart by whose context is done.
	readCtx, cancel := context.WithDeadline(ctx, hard)
	defer cancel()

	// What Read refuses without a connection, Wait refuses the same way: a
	// cursor that cannot be spoken on the wire opens no connection here either.
	if err := checkCursor(cursor); err != nil {
		return nil, cursor, err
	}
	w := &waitState{stream: s, cursor: cursor}
	// Bounded like the reads: a handshake that stalls would otherwise spend the
	// connection's own registration timeout on top of what this call promised.
	if err := w.resync(readCtx); err != nil {
		return nil, cursor, bounded(ctx, err)
	}
	if err := w.read(readCtx); err != nil {
		return w.batch, w.cursor, bounded(ctx, err)
	}

	// Nothing yet: wait for the server to say something, and keep waiting if
	// what it said turns out not to be a message.
	for len(w.batch) == 0 {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, w.cursor, nil
		}
		t := time.NewTimer(remaining)
		select {
		case <-w.arrivals:
			t.Stop()
		case <-t.C:
			return nil, w.cursor, nil
		case <-ctx.Done():
			t.Stop()
			return nil, w.cursor, ctx.Err()
		}
		if err := w.read(readCtx); err != nil {
			return w.batch, w.cursor, bounded(ctx, err)
		}
	}

	// From here the batch is not empty and the only question is when to stop
	// adding to it. Every arrival is read as it lands and restarts the quiet
	// window, so a burst is one delivery; the bound is what stops a channel
	// that never pauses from holding the wait open for as long as it keeps
	// talking. Reading on each arrival rather than after the window is what
	// keeps the bound honest: there is no read left to do when it arrives.
	for {
		quiet := min(debounce, time.Until(hard))
		if quiet <= 0 {
			return w.batch, w.cursor, nil
		}
		t := time.NewTimer(quiet)
		select {
		case <-w.arrivals:
			t.Stop()
			if err := w.read(readCtx); err != nil {
				return w.batch, w.cursor, bounded(ctx, err)
			}
		case <-t.C:
			return w.batch, w.cursor, nil
		case <-ctx.Done():
			return w.batch, w.cursor, ctx.Err()
		}
	}
}

// waitState is one Wait in progress: what it has collected, where it is in the
// stream, and which connection's wake-up it is listening to. The last of those
// is the one that needs looking after — a read can re-dial underneath it, and a
// wait listening to a connection that is gone hears nothing or hears it at once,
// neither of which is waiting.
type waitState struct {
	stream   *Stream
	conn     *conn
	arrivals <-chan struct{}
	batch    []stream.Message
	cursor   stream.Cursor
}

// resync points the wait at the live connection, dialing one if there is none.
// When the connection has changed it takes the new wake-up first and then reads,
// so a message that landed while the old one was dying is collected rather than
// waited for.
func (w *waitState) resync(ctx context.Context) error {
	live, err := w.stream.ensure(ctx)
	if err != nil {
		return err
	}
	if live == w.conn {
		return nil
	}
	first := w.conn == nil
	w.conn, w.arrivals = live, live.arrivals()
	if first {
		return nil
	}
	return w.collect(ctx)
}

// read takes the next wake-up before asking for what is already there, so a
// message arriving during the read is not lost between the two, and then makes
// sure it is still listening to the connection it read from.
func (w *waitState) read(ctx context.Context) error {
	w.arrivals = w.conn.arrivals()
	if err := w.collect(ctx); err != nil {
		return err
	}
	return w.resync(ctx)
}

// collect reads from the cursor and appends whatever came back.
func (w *waitState) collect(ctx context.Context) error {
	msgs, next, err := w.stream.Read(ctx, w.cursor)
	w.batch = append(w.batch, msgs...)
	if next != "" || err == nil {
		w.cursor = next
	}
	return err
}

// bounded turns the deadline Wait imposed on its own reads into a normal
// return. A caller that cancelled still gets its own error.
func bounded(ctx context.Context, err error) error {
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		return nil
	}
	return err
}

// checkCursor refuses a cursor that cannot have come from this adapter. A
// cursor is a msgid and it travels inside an IRC line: one holding a space or a
// line break is not a position to resume from but a second command the server
// would read off the end of the first, so a cursor that has been tampered with
// or corrupted in the state file is refused rather than sent. Ergo answers a
// well-formed msgid it does not know with an empty batch, which is the history
// gap, so only the unsendable shapes are errors here.
func checkCursor(c stream.Cursor) error {
	if len(c) > maxCursor {
		return fmt.Errorf("irc: bad cursor: %d bytes, the maximum is %d", len(c), maxCursor)
	}
	for i := range len(c) {
		if b := c[i]; b <= ' ' || b > '~' || b == ';' || b == ',' {
			return fmt.Errorf("irc: bad cursor %q: a cursor is a server msgid, and holds no space, separator or control character", string(c))
		}
	}
	return nil
}

// cursorAt is the position just past what a read consumed: the id of the last
// history item it saw, or the caller's own cursor when it saw none.
//
// The item, not the last message: most of what a channel carries is not a
// message — a JOIN, a client's CTCP, a NOTICE — and a cursor left in front of
// one would have every later read fetch it again, for as long as nothing else
// arrived. What the cursor may never be is an id the server will not answer
// for, which is why the ids it comes from are the ones page.record kept: real
// ids, never the server's own synthetic narration.
func cursorAt(item string, held stream.Cursor) stream.Cursor {
	if item == "" {
		return held
	}
	return stream.Cursor(item)
}

// replyText renders a server reply as the text it carries, which is what the
// participant needs to see: `FAIL CHATHISTORY INVALID_TARGET … :Messages could
// not be retrieved`, or a numeric and its message.
func replyText(l line) string {
	parts := make([]string, 0, len(l.params)+1)
	if l.command != "PRIVMSG" {
		parts = append(parts, l.command)
	}
	for i, p := range l.params {
		// The first parameter of a numeric is the nick it is addressed to,
		// which says nothing about what went wrong.
		if i == 0 && isNumeric(l.command) {
			continue
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, " ")
}

func isNumeric(cmd string) bool {
	if len(cmd) != 3 {
		return false
	}
	for i := range len(cmd) {
		if cmd[i] < '0' || cmd[i] > '9' {
			return false
		}
	}
	return true
}
