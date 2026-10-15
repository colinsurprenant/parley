package irc

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/colinsurprenant/parley/internal/stream"
)

// wantCaps are the capabilities this adapter asks for, in the order it asks.
// Only those the server advertises are requested: a REQ for a cap the server
// never listed is a protocol error, and a server that does not have one is told
// apart by requireCaps below, with a message naming what is missing.
var wantCaps = []string{
	"server-time",
	"message-tags",
	"batch",
	"echo-message",
	"labeled-response",
	"standard-replies",
	"sasl",
	"draft/account-registration",
	"draft/chathistory",
	"draft/event-playback",
	"draft/multiline",
}

// requireCaps are the capabilities the adapter cannot work without, each for a
// reason it cannot route around:
//
//   - server-time and message-tags carry the timestamp and the msgid that are
//     a message's TS and ID, and the msgid is the cursor;
//   - batch frames both the history reply and a multiline message;
//   - echo-message is how Send learns the id the server gave its message, and
//     how a participant's own line reaches Read at all;
//   - labeled-response is what tells two concurrent replies apart, since a
//     batch carries no other mark of which request it answers;
//   - draft/chathistory is the history itself, which is the whole of Read;
//   - draft/multiline is what makes a body with newlines one message with one
//     id rather than one message per line;
//   - sasl is how every connection for a participant logs into that
//     participant's account, which is what lets the pump and the reply that
//     answers it hold the same nick at once.
//
// draft/account-registration is not required: it is needed only the first time
// a participant connects to a server, and its absence is reported then, with
// what it would have been for.
var requireCaps = []string{
	"server-time",
	"message-tags",
	"batch",
	"echo-message",
	"labeled-response",
	"sasl",
	"draft/chathistory",
	"draft/event-playback",
	"draft/multiline",
}

// RequiredCaps returns the capabilities this adapter cannot work without, in
// the order it asks for them, so that a check of a server — `parley doctor` —
// reads the one list the connection itself enforces rather than keeping a
// second one in step with it.
func RequiredCaps() []string { return append([]string(nil), requireCaps...) }

// saslChunk is the most base64 a single AUTHENTICATE line may carry. A message
// that divides exactly by it is followed by an empty continuation, which is how
// the far end knows it ended.
const saslChunk = 400

// raceRetryDelay is how long a connection that lost the race to register an
// account waits before trying the winner's password a second time: long enough
// for a registration already in flight on a loopback socket, short enough not
// to be felt.
const raceRetryDelay = 500 * time.Millisecond

// errRegisterRefused marks the server declining a registration outright, which
// is the only answer that proves no account was made. A timeout, a dead socket
// or a reply that never arrived prove nothing: the account may well exist, so
// the password that names it is kept and the next connection either logs in
// with it or registers with it.
var errRegisterRefused = errors.New("irc: the server refused the registration")

// errAccountExists marks the server refusing to register an account that is
// already there. It is the one registration failure that says something about
// the password the caller holds rather than about the request.
var errAccountExists = errors.New("irc: the account is already registered")

// errBadCredentials marks the server refusing a password (904). It is the one
// SASL failure that can be transient: an account registered a moment ago by
// another process is not one this connection knows about yet.
var errBadCredentials = errors.New("irc: the server did not accept the account password")

// passwordBytes is how much entropy an account password gets. It is never typed
// by anyone and never shown: it is generated here, stored 0600, and read back
// by the next connection.
const passwordBytes = 32

// defaultHistLimit is how many history items one CHATHISTORY page asks for when
// the server's 005 does not say what it will give.
const defaultHistLimit = 100

// Ergo's own defaults, used when the draft/multiline cap arrives without the
// values it normally carries. They are a floor, not a promise: a body over the
// server's real limit comes back as a FAIL, which Send reports as it stands.
const (
	defaultMaxBytes = 4096
	defaultMaxLines = 100
)

// registerTimeout bounds registration when the caller's context carries no
// deadline of its own. A server that has accepted the connection but not
// finished the handshake is not one to wait on for ever. Running out of this
// one is the i/o timeout it is; only a deadline the caller set is reported as
// the context's (see deadlineErr).
const registerTimeout = 30 * time.Second

// quitTimeout bounds the parting QUIT. It is best-effort — the socket closes
// either way — so a server that has stopped reading must not hold up a CLI verb
// that is otherwise finished.
const quitTimeout = time.Second

// writeTimeout bounds one write. A peer that has stopped reading would
// otherwise block the writer for as long as the kernel keeps the socket.
const writeTimeout = 30 * time.Second

// replyTimeout bounds the wait for a labeled reply. It is a variable only so
// that a test can shorten it; nothing changes it at run time. Every request this adapter
// makes is answered by a server that is working — the echo of a message, the
// batch of a history query — so a silence this long is a server that is not.
// The bound is the adapter's own: a caller that passed no deadline of its own
// (`parley send` is one) must not be left waiting on a socket for ever with
// nothing to show for it.
var replyTimeout = 30 * time.Second

// conn is one registered connection, joined to the channel. Reads are owned by
// a single goroutine, which dispatches every line: PING is answered here,
// labeled replies go to the request waiting for them, and a live message from
// the channel wakes whoever is in Wait.
type conn struct {
	nc      net.Conn
	r       *bufio.Reader
	addr    string
	nick    string
	channel string
	creds   Credentials

	// prefix is this connection's own nick!user@host as the server spells it,
	// learned from the JOIN echo. It is what a PRIVMSG of ours costs on the
	// wire, and so what bounds a chunk.
	prefix string

	caps      map[string]string
	histLimit int
	maxBytes  int
	maxLines  int

	// wsem serializes writes and can be given up on: a caller whose context is
	// done while another write holds the socket gets its own error rather than
	// waiting out a write it is no longer interested in. A mutex cannot be
	// waited on with a select, which is the whole reason this is a channel.
	wsem chan struct{}

	mu      sync.Mutex
	waiters map[string]*waiter
	// batchOf maps a batch id to the label of the response it belongs to,
	// including a batch nested inside another: a multiline message inside a
	// history reply is reached through its parent.
	batchOf map[string]string
	// liveBatch holds the ids of multiline messages arriving on the channel
	// right now, so that the line closing one can be recognised as what it is.
	// The server commits such a message to its history when the batch closes,
	// which makes that line — not the ones before it — the moment a reader can
	// find the message.
	liveBatch map[string]struct{}
	err       error

	done   chan struct{} // closed when the connection is finished
	closed sync.Once

	amu     sync.Mutex
	arrival chan struct{} // closed and replaced on every live channel message

	labels atomic.Uint64
}

// waiter collects one labeled response. A response is either a single line or a
// batch: lines holds them in arrival order, the nested BATCH lines included, and
// root is the batch whose close ends the response.
//
// err is how a request ends that was never answered. A connection that dies
// releases every waiter on it, and what those waiters hold at that moment is
// half of something — the opening line of a batch whose contents never came. A
// caller handed that would read a truncated echo as a receipt for a message the
// server never took, or an empty history as a channel with nothing in it, so
// what it is handed instead is the reason the connection ended.
type waiter struct {
	lines []line
	root  string
	err   error
	done  chan struct{}
}

// dial opens one connection and registers it, joined to the channel and ready
// for CHATHISTORY. Nothing before the JOIN confirmation is optional: ergo
// answers CHATHISTORY from a non-member with FAIL CHATHISTORY INVALID_TARGET,
// so a Read racing the JOIN would look like an empty channel.
func dial(ctx context.Context, addr, nick, channel string, creds Credentials) (*conn, error) {
	var d net.Dialer
	nc, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("irc: dial %s: %w", addr, err)
	}
	// The spec was checked when it was parsed; this checks what the name
	// actually resolved to, before a password is put on the socket. A
	// `localhost` that answers somewhere else is the case that matters, and it
	// is not this adapter's to trust.
	if err := loopbackAddr(nc.RemoteAddr()); err != nil {
		nc.Close()
		return nil, err
	}
	c := &conn{
		nc:        nc,
		r:         bufio.NewReaderSize(nc, maxLineLen),
		addr:      addr,
		nick:      nick,
		channel:   channel,
		creds:     creds,
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
	if err := c.register(ctx); err != nil {
		nc.Close()
		return nil, err
	}
	go c.readLoop()
	return c, nil
}

// loopbackAddr reports whether the connection landed on this machine.
func loopbackAddr(a net.Addr) error {
	ta, ok := a.(*net.TCPAddr)
	if !ok {
		return fmt.Errorf("irc: refusing %s: only a TCP connection to this machine is in this scheme", a)
	}
	if !ta.IP.IsLoopback() {
		return fmt.Errorf("irc: refusing to speak to %s: it resolved off this machine, and this scheme sends the account password in the clear", ta)
	}
	return nil
}

// register runs the handshake to the JOIN confirmation. It reads the socket
// directly: the dispatcher's routing is built on caps that are not negotiated
// yet, and everything here is a reply to something this function just sent.
func (c *conn) register(ctx context.Context) error {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(registerTimeout)
	}
	if err := c.nc.SetDeadline(deadline); err != nil {
		return fmt.Errorf("irc: %w", err)
	}
	stopWatch := watchCancel(ctx, func() { c.nc.SetDeadline(time.Now()) })
	defer stopWatch()

	if err := c.writeLines(ctx,
		line{command: "CAP", params: []string{"LS", "302"}},
		line{command: "NICK", params: []string{c.nick}},
		line{command: "USER", params: []string{c.nick, "0", "*", c.nick}, colon: true},
	); err != nil {
		return err
	}

	var (
		offered = make(map[string]string)
		reqSent bool
		joined  bool
	)
	for !joined {
		l, err := c.readLine()
		if err != nil {
			// The socket was armed at the caller's deadline, so its timer can
			// beat the context's: deadlineErr tells that from a real timeout.
			if ctxErr := deadlineErr(ctx, err); ctxErr != nil {
				return ctxErr
			}
			return fmt.Errorf("irc: register as %s: %w", c.nick, err)
		}
		switch l.command {
		case "PING":
			if err := c.writeLines(ctx, line{command: "PONG", params: l.params, colon: true}); err != nil {
				return err
			}
		case "ERROR":
			return fmt.Errorf("irc: register as %s: server said: %s", c.nick, l.trailing())
		case "CAP":
			switch strings.ToUpper(l.param(1)) {
			case "LS":
				// `CAP * LS * :…` means another LS line follows; only the last
				// one completes the list.
				more := l.param(2) == "*"
				for _, v := range strings.Fields(l.trailing()) {
					name, value, _ := strings.Cut(v, "=")
					offered[name] = value
				}
				if more || reqSent {
					continue
				}
				reqSent = true
				var req []string
				for _, want := range wantCaps {
					if _, ok := offered[want]; ok {
						req = append(req, want)
					}
				}
				if len(req) == 0 {
					if err := c.writeLines(ctx, line{command: "CAP", params: []string{"END"}}); err != nil {
						return err
					}
					continue
				}
				if err := c.writeLines(ctx, line{command: "CAP", params: []string{"REQ", strings.Join(req, " ")}, colon: true}); err != nil {
					return err
				}
			case "ACK":
				for _, name := range strings.Fields(l.trailing()) {
					c.caps[name] = offered[name]
				}
				if err := c.missingCaps(); err != nil {
					return err
				}
				c.readLimits()
				// Before CAP END, because that is the only point at which a
				// connection can register an account, and because a connection
				// that reached 001 unauthenticated has already been refused the
				// nick it came for.
				if err := c.authenticate(ctx); err != nil {
					return err
				}
				if err := c.writeLines(ctx, line{command: "CAP", params: []string{"END"}}); err != nil {
					return err
				}
			case "NAK":
				return fmt.Errorf("irc: register as %s: server refused the capabilities %s", c.nick, l.trailing())
			}
		case "001":
			if err := c.missingCaps(); err != nil {
				return err
			}
			if err := c.writeLines(ctx, line{command: "JOIN", params: []string{c.channel}}); err != nil {
				return err
			}
		case "005":
			c.readISupport(l)
		case "JOIN":
			// Our own JOIN echo is the confirmation; someone else joining
			// before it is not.
			if stream.NickEqual(nickFromPrefix(l.prefix), c.nick) {
				c.prefix = l.prefix
				joined = true
			}
		case "432", "433", "436", "437":
			// Erroneous nick, nick in use, collision, unavailable: a second
			// process for one participant is refused, not renamed.
			return fmt.Errorf("irc: register as %s: %s %s", c.nick, l.command, l.trailing())
		case "403", "405", "471", "473", "474", "475", "476", "477", "479":
			return fmt.Errorf("irc: join %s as %s: %s %s", c.channel, c.nick, l.command, l.trailing())
		case "464", "465":
			return fmt.Errorf("irc: register as %s: %s %s", c.nick, l.command, l.trailing())
		case "421":
			// Unknown command: a server without CAP would answer the handshake
			// this way, and it has nothing this adapter can use.
			if strings.EqualFold(l.param(1), "CAP") {
				return fmt.Errorf("irc: register as %s: the server does not support capability negotiation", c.nick)
			}
		}
	}
	// Joined before the deadline is cleared, not after: a cancel landing in
	// between would otherwise set an expired deadline on a connection that is
	// registered and about to be read from for the rest of its life.
	stopWatch()
	if err := c.nc.SetDeadline(time.Time{}); err != nil {
		return fmt.Errorf("irc: %w", err)
	}
	return nil
}

// authenticate logs this connection into the participant's account, which is
// what makes a nick something several parley processes can hold at once: the
// long-lived pump and the one-shot send that answers it are two sessions of one
// account, not two clients fighting over a nick.
//
// There is no password for a human to choose, type or lose. The first
// connection for a (nick, server) registers the account with 32 random bytes
// and stores them 0600 under the participant's state directory; every
// connection after it reads them back and authenticates. The account is the
// nick, so a stranger cannot take it (the server's nick reservation refuses
// them) and the participant's own processes always can.
func (c *conn) authenticate(ctx context.Context) error {
	if c.creds == nil {
		return errors.New("irc: no credential store: the adapter cannot hold an account for this participant")
	}
	password, err := c.creds.Load(c.addr)
	if err != nil {
		return err
	}
	if password != "" {
		return c.loginOrRecover(ctx, password)
	}
	if err := c.canRegister(); err != nil {
		return err
	}
	password, err = newPassword()
	if err != nil {
		return err
	}
	// Claimed before the account is asked for, not after it is granted. A
	// password that reached the server but not the disk is an account nobody
	// can ever log into again, and the window for that is however long the
	// server takes to answer — a crash or a lost reply inside it would strand
	// the participant's own name. The other way round is recoverable, and
	// loginOrRecover is where it is recovered.
	mine, err := c.creds.Claim(c.addr, password)
	if err != nil {
		return err
	}
	if !mine {
		// Another parley process for this participant got there first. Its
		// password is the account's, so use that rather than registering a
		// second one.
		return c.saslAfterRace(ctx)
	}
	if err := c.registerAccount(ctx, password); err != nil {
		if !errors.Is(err, errRegisterRefused) {
			// Nothing was refused — the answer was lost, or never came. The
			// password stays: loginOrRecover registers with it next time if the
			// account was never made, and logs in with it if it was.
			return err
		}
		if errors.Is(err, errAccountExists) {
			// The account exists, and it may be this very password's: another
			// process for this participant can have read the claim and
			// registered with it while this one was asking. Finding out costs
			// one login, and getting it wrong would mean deleting a healthy
			// account's only password.
			switch serr := c.loginWithRetry(ctx, password); {
			case serr == nil:
				return nil
			case !errors.Is(serr, errBadCredentials):
				return serr
			}
		}
		// The server declined and the password opens nothing: leaving it would
		// make every later connection authenticate with it and be told its
		// credentials are invalid, which says nothing true about what happened
		// here.
		return c.discardAnd(err)
	}
	return nil
}

// loginOrRecover authenticates with the password already stored for this
// participant, and makes the account it names if the server has never heard of
// it. That is not a hypothetical: the password is claimed before the account is
// registered, so a connection that died in between — or a server that lost the
// reply — leaves a password with nothing behind it, and every connection after
// would be refused for ever with no way out. One refusal is therefore a
// question rather than an answer: if the server will still take a registration,
// the account is made now, with the password that was already stored.
func (c *conn) loginOrRecover(ctx context.Context, password string) error {
	err := c.saslPlain(ctx, password)
	if !errors.Is(err, errBadCredentials) || c.canRegister() != nil {
		return err
	}
	rerr := c.registerAccount(ctx, password)
	if rerr == nil {
		return nil
	}
	if !errors.Is(rerr, errAccountExists) {
		return rerr
	}
	// The account does exist. It may have come into existence between the two
	// steps — the process that claimed this password finishing its own
	// registration — so the password gets one more try before it is called
	// worthless.
	switch err := c.saslPlain(ctx, password); {
	case err == nil:
		return nil
	case !errors.Is(err, errBadCredentials):
		// The second attempt did not get an answer either. Nothing here says
		// the password is wrong, so it stays.
		return err
	}
	return c.discardAnd(rerr)
}

// discardAnd drops the stored password and returns why it was dropped. A
// password that authenticates nothing and can register nothing is not a
// credential, and keeping it would make the next connection fail the same way
// with less to say about it.
func (c *conn) discardAnd(err error) error {
	location := c.creds.Location(c.addr)
	if derr := c.creds.Discard(c.addr); derr != nil {
		return fmt.Errorf("%w (and the password parley held for it could not be removed: %v)", err, derr)
	}
	if errors.Is(err, errAccountExists) {
		return fmt.Errorf("irc: the account %s already exists on %s but parley holds no password for it (%s does not exist): the state directory it was stored in is gone. Drop the account on the server and let parley make it again — with ergo, /msg NickServ UNREGISTER %s from a client logged into it, or stop the server and delete its datastore (contrib/ergo/README.md)",
			c.nick, c.addr, location, c.nick)
	}
	return err
}

// saslAfterRace authenticates with the password another process claimed. That
// process may still be registering the account, so a refusal is given one
// second chance before it is believed: the two are a pump and the send that
// answers it, starting together, and neither is wrong.
func (c *conn) saslAfterRace(ctx context.Context) error {
	password, err := c.creds.Load(c.addr)
	if err != nil {
		return err
	}
	if password == "" {
		return fmt.Errorf("irc: another process claimed the account password for %s on %s and then left nothing in %s", c.nick, c.addr, c.creds.Location(c.addr))
	}
	return c.loginWithRetry(ctx, password)
}

// loginWithRetry authenticates, and gives a refusal one second chance: an
// account registered a moment ago by another process for this participant is
// not one this connection knows about yet, and that is the one SASL failure
// that answers itself given a moment.
func (c *conn) loginWithRetry(ctx context.Context, password string) error {
	err := c.saslPlain(ctx, password)
	if !errors.Is(err, errBadCredentials) {
		return err
	}
	if werr := sleep(ctx, raceRetryDelay); werr != nil {
		return werr
	}
	return c.saslPlain(ctx, password)
}

// sleep waits, and gives up if the caller does.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// canRegister reports whether the server will take a registration on this
// connection, before it is a connection proper.
func (c *conn) canRegister() error {
	v, ok := c.caps["draft/account-registration"]
	if !ok {
		return fmt.Errorf("irc: parley has no account for %s on %s and the server does not offer the draft/account-registration capability, so it cannot make one", c.nick, c.addr)
	}
	for _, mode := range strings.Split(v, ",") {
		if mode == "before-connect" {
			return nil
		}
	}
	return fmt.Errorf("irc: parley has no account for %s on %s and the server does not allow registration before connecting (draft/account-registration=%s); set accounts.registration.allow-before-connect", c.nick, c.addr, v)
}

// registerAccount claims the account named by this connection's nick. `*` for
// the account name means "the nick I registered with" and `*` for the email
// means "none", which is why the server must have email verification off.
func (c *conn) registerAccount(ctx context.Context, password string) error {
	if err := c.writeLines(ctx, line{command: "REGISTER", params: []string{"*", "*", password}, colon: true}); err != nil {
		return err
	}
	for {
		l, err := c.readReply(ctx)
		if err != nil {
			return err
		}
		switch registerReply(l) {
		case replyDone:
			return nil
		case replyRefused:
			// What to do about it is the caller's: a password just claimed and
			// one stored by an earlier run are the same refusal and not the
			// same situation.
			// A FAIL is the server declining: nothing was created, and after an
			// exclusive claim ACCOUNT_EXISTS means the account was there before
			// this connection ever asked. Any other reply — a verification the
			// account now waits on, something this adapter has not met — says
			// the server did do something, and is not grounds to throw the
			// password away.
			if l.command == "FAIL" {
				if strings.EqualFold(l.param(1), "ACCOUNT_EXISTS") {
					return fmt.Errorf("%w: %w: irc: registering the account %s on %s: %s", errRegisterRefused, errAccountExists, c.nick, c.addr, replyText(l))
				}
				return fmt.Errorf("%w: irc: registering the account %s on %s: %s", errRegisterRefused, c.nick, c.addr, replyText(l))
			}
			return fmt.Errorf("irc: registering the account %s on %s: %s", c.nick, c.addr, replyText(l))
		}
	}
}

// saslPlain runs the SASL PLAIN exchange. The password never reaches the wire
// in any other form and the connection is plaintext loopback, which is the
// posture this adapter is for: see the package documentation.
func (c *conn) saslPlain(ctx context.Context, password string) error {
	if err := c.writeLines(ctx, line{command: "AUTHENTICATE", params: []string{"PLAIN"}}); err != nil {
		return err
	}
	for {
		l, err := c.readReply(ctx)
		if err != nil {
			return err
		}
		if l.command == "AUTHENTICATE" {
			if l.param(0) != "+" {
				return fmt.Errorf("irc: authenticating as %s on %s: the server asked for something parley cannot answer: %s", c.nick, c.addr, replyText(l))
			}
			if err := c.writeLines(ctx, saslLines(c.nick, password)...); err != nil {
				return err
			}
			continue
		}
		switch saslReply(l) {
		case replyDone:
			return nil
		case replyRefused:
			err := fmt.Errorf("irc: authenticating as %s on %s: %s — the password parley holds for this account is in %s", c.nick, c.addr, replyText(l), c.creds.Location(c.addr))
			if l.command == "904" {
				// The account does not know this password — or does not know it
				// yet, which is a different thing to a caller that has just
				// lost a race to register it.
				err = fmt.Errorf("%w: %w", errBadCredentials, err)
			}
			return err
		}
	}
}

// replyKind is what one line means to the exchange that is waiting for an
// answer. An exchange that waited only for the reply it hoped for would sit
// there until its deadline every time the server said no in a way it had not
// been taught, which is the worst way to fail: silently, and slowly.
type replyKind int

const (
	// replyPassing is a line on its way past: it says nothing about this
	// exchange.
	replyPassing replyKind = iota
	// replyDone is the exchange succeeding.
	replyDone
	// replyRefused is the server saying no. Its own text is the explanation.
	replyRefused
)

// registerReply sorts a line arriving while an account registration is in
// flight.
func registerReply(l line) replyKind {
	switch {
	case l.command == "REGISTER":
		if strings.EqualFold(l.param(0), "SUCCESS") {
			return replyDone
		}
		// VERIFICATION_REQUIRED, or anything else a server answers with:
		// parley has no way to verify anything, so every other answer is one
		// this connection cannot go on from.
		return replyRefused
	case l.command == "FAIL" && strings.EqualFold(l.param(0), "REGISTER"):
		return replyRefused
	case l.command == "900":
		// Registering logs the connection in, and that numeric can arrive
		// before the REGISTER reply it belongs to.
		return replyPassing
	}
	return numericReply(l)
}

// saslReply sorts a line arriving while SASL is in flight. The challenge
// itself is the caller's to answer and is not classified here.
func saslReply(l line) replyKind {
	switch l.command {
	case "903", "907":
		// Authenticated, or authenticated already: either way there is nothing
		// left to do.
		return replyDone
	case "900":
		// Logged in; the exchange still ends with its own reply.
		return replyPassing
	case "FAIL":
		if strings.EqualFold(l.param(0), "AUTHENTICATE") || strings.EqualFold(l.param(0), "SASL") {
			return replyRefused
		}
	}
	return numericReply(l)
}

// numericReply is the fallback both exchanges share: during a handshake, a
// numeric at or above 400 is the server refusing something, and the only thing
// there is to refuse is the handshake. Below 400 is information — the welcome
// burst, ISUPPORT — and anything that is not a numeric is not an answer to
// anything.
func numericReply(l line) replyKind {
	if !isNumeric(l.command) {
		return replyPassing
	}
	n, err := strconv.Atoi(l.command)
	if err != nil || n < 400 {
		return replyPassing
	}
	return replyRefused
}

// saslLines encodes the PLAIN response and cuts it into the lines the protocol
// allows. A response that divides exactly by the chunk size needs the empty
// continuation after it, or the server would still be waiting for the rest.
func saslLines(nick, password string) []line {
	blob := base64.StdEncoding.EncodeToString([]byte("\x00" + nick + "\x00" + password))
	var out []line
	for len(blob) >= saslChunk {
		out = append(out, line{command: "AUTHENTICATE", params: []string{blob[:saslChunk]}})
		blob = blob[saslChunk:]
	}
	if blob == "" {
		blob = "+"
	}
	return append(out, line{command: "AUTHENTICATE", params: []string{blob}})
}

// newPassword mints an account password. It is hex so that it is safe in an
// IRC parameter and in a file, and long enough that it is not worth guessing.
func newPassword() (string, error) {
	b := make([]byte, passwordBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("irc: generate an account password: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// readReply reads one line during registration, answering PING and ending on
// anything that means the connection is over. It is the registration loop's
// reader: the dispatcher is not running yet, because the caps it routes by are
// still being negotiated.
func (c *conn) readReply(ctx context.Context) (line, error) {
	for {
		l, err := c.readLine()
		if err != nil {
			// As in register: the socket deadline register armed is the
			// caller's own, and may fire before the context reports it.
			if ctxErr := deadlineErr(ctx, err); ctxErr != nil {
				return line{}, ctxErr
			}
			return line{}, fmt.Errorf("irc: register as %s: %w", c.nick, err)
		}
		switch l.command {
		case "PING":
			if err := c.writeLines(ctx, line{command: "PONG", params: l.params, colon: true}); err != nil {
				return line{}, err
			}
		case "ERROR":
			return line{}, fmt.Errorf("irc: register as %s: server said: %s", c.nick, l.trailing())
		default:
			return l, nil
		}
	}
}

// missingCaps reports the first required capability the server did not give.
func (c *conn) missingCaps() error {
	for _, name := range requireCaps {
		if _, ok := c.caps[name]; !ok {
			return fmt.Errorf("irc: the server at %s does not offer the %s capability, which parley needs to read and write this channel", c.nc.RemoteAddr(), name)
		}
	}
	return nil
}

// readLimits takes the multiline bounds out of the cap value, which ergo spells
// `draft/multiline=max-bytes=65536,max-lines=1000`.
func (c *conn) readLimits() {
	for _, kv := range strings.Split(c.caps["draft/multiline"], ",") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			continue
		}
		switch k {
		case "max-bytes":
			c.maxBytes = n
		case "max-lines":
			c.maxLines = n
		}
	}
}

// readISupport takes the CHATHISTORY page size out of a 005 reply.
func (c *conn) readISupport(l line) {
	for _, tok := range l.params {
		k, v, ok := strings.Cut(tok, "=")
		if !ok {
			continue
		}
		if !strings.EqualFold(k, "CHATHISTORY") && !strings.EqualFold(k, "draft/CHATHISTORY") {
			continue
		}
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.histLimit = n
		}
	}
}

// watchCancel arranges for a cancelled context to reach an operation blocked on
// the socket: the only thing that interrupts one is a deadline, so cancellation
// sets one in the past. The returned function stops the watch and waits for it
// to be over — waiting is the point, because a watcher still running is a
// deadline about to land on whatever uses the socket next.
func watchCancel(ctx context.Context, expire func()) func() {
	done, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		select {
		case <-ctx.Done():
			expire()
		case <-done:
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() { close(done) })
		<-stopped
	}
}

// deadlineErr reports the context's own error when it is what ended an I/O
// call, and nil when it is not. The sockets are armed at the caller's deadline
// so that a late call is refused before any byte moves, but the socket's timer
// and the context's are separate and unordered: the socket can fire while
// ctx.Err() is still nil. A deadline error from the socket, with the caller's
// deadline passed, is therefore the caller's deadline and is returned as
// context.DeadlineExceeded. Anything else — including the socket running out
// of a ceiling the caller did not set — is nil, and the site's own handling of
// the error applies.
func deadlineErr(ctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	if d, ok := ctx.Deadline(); ok && !time.Now().Before(d) && errors.Is(err, os.ErrDeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return nil
}

// readLine reads one line, bounded: a line over the protocol's budget is not a
// line this adapter can resynchronise after, so it ends the connection.
func (c *conn) readLine() (line, error) {
	var (
		buf     []byte
		n       int
		tooLong bool
	)
	for {
		frag, err := c.r.ReadSlice('\n')
		n += len(frag)
		// maxRestLen counts the CRLF, so maxLineLen is the whole of what a line
		// may occupy on the wire: one byte past it is one byte too many.
		if n > maxLineLen {
			tooLong = true
		}
		switch {
		case err == nil:
			if tooLong {
				return line{}, errLineTooLong
			}
			return parseLine(string(append(buf, frag...)))
		case errors.Is(err, bufio.ErrBufferFull):
			if !tooLong {
				buf = append(buf, frag...)
			}
		default:
			return line{}, err
		}
	}
}

func (c *conn) readLoop() {
	for {
		l, err := c.readLine()
		if err != nil {
			c.fail(err)
			return
		}
		c.dispatch(l)
	}
}

// dispatch routes one line. Three destinations and nothing else: the server
// (PING), the request that asked for it (a label, or a batch a label opened),
// or a wake-up for Wait. A line that is none of those is not the adapter's
// business — NOTICE, MODE, a numeric nobody asked for — and is dropped.
func (c *conn) dispatch(l line) {
	switch l.command {
	case "PING":
		// A write failure here is the connection going away, which the reader
		// will see on its own; there is no one to report it to.
		// The reader goroutine answers on nobody's behalf, so this write has
		// only the ceiling.
		_ = c.writeLines(context.Background(), line{command: "PONG", params: l.params, colon: true})
		return
	case "ERROR":
		c.fail(fmt.Errorf("irc: server closed the connection: %s", l.trailing()))
		return
	}

	c.mu.Lock()
	if l.command == "BATCH" {
		ref := l.param(0)
		id := ref[min(1, len(ref)):]
		switch {
		case strings.HasPrefix(ref, "+"):
			label, ok := l.tag("label")
			if !ok {
				// A batch inside a labeled response belongs to that response;
				// one that belongs to nothing is live traffic.
				if parent, has := l.tag("batch"); has {
					label, ok = c.batchOf[parent]
				}
			}
			if w := c.waiters[label]; ok && w != nil {
				if w.root == "" {
					w.root = id
				}
				c.batchOf[id] = label
				w.lines = append(w.lines, l)
				c.mu.Unlock()
				return
			}
			// A live multiline message announces itself on its BATCH line, which
			// is where its msgid is; its PRIVMSG lines carry only the batch tag.
			live := c.isChannelBatch(l)
			if live {
				c.liveBatch[id] = struct{}{}
			}
			c.mu.Unlock()
			if live {
				c.wake()
			}
			return
		case strings.HasPrefix(ref, "-"):
			label, ok := c.batchOf[id]
			if !ok {
				_, live := c.liveBatch[id]
				delete(c.liveBatch, id)
				c.mu.Unlock()
				// The message is one message, and the server has it only now
				// that the batch is closed: the wake-ups its lines caused
				// before this one were all too early to find it.
				if live {
					c.wake()
				}
				return
			}
			delete(c.batchOf, id)
			if w := c.waiters[label]; w != nil {
				w.lines = append(w.lines, l)
				if w.root == id {
					c.finish(label, nil)
				}
			}
			c.mu.Unlock()
			return
		}
	}
	if b, ok := l.tag("batch"); ok {
		if label, known := c.batchOf[b]; known {
			if w := c.waiters[label]; w != nil {
				w.lines = append(w.lines, l)
			}
			c.mu.Unlock()
			return
		}
		c.mu.Unlock()
		if c.isChannelMessage(l) {
			c.wake()
		}
		return
	}
	if label, ok := l.tag("label"); ok {
		if w := c.waiters[label]; w != nil {
			w.lines = append(w.lines, l)
			c.finish(label, nil)
			c.mu.Unlock()
			return
		}
	}
	c.mu.Unlock()
	if c.isChannelMessage(l) {
		c.wake()
	}
}

// finish releases the request waiting under label: with err nil the response is
// complete and its lines are the answer, and with err set there is no answer
// and never will be. The caller holds c.mu.
func (c *conn) finish(label string, err error) {
	w := c.waiters[label]
	if w == nil {
		return
	}
	w.err = err
	delete(c.waiters, label)
	close(w.done)
}

func (c *conn) isChannelMessage(l line) bool {
	return l.command == "PRIVMSG" && asciiEqualFold(l.param(0), c.channel)
}

func (c *conn) isChannelBatch(l line) bool {
	return l.command == "BATCH" && l.param(1) == "draft/multiline" && asciiEqualFold(l.param(2), c.channel)
}

// wake releases everyone waiting on an arrival. The channel is a broadcast: it
// is closed and replaced, so a waiter that took it before the message cannot
// miss it, and one that takes it after gets the next.
func (c *conn) wake() {
	c.amu.Lock()
	close(c.arrival)
	c.arrival = make(chan struct{})
	c.amu.Unlock()
}

// arrivals returns the channel closed by the next live channel message. Take it
// before reading, or an arrival between the read and the wait is lost.
func (c *conn) arrivals() <-chan struct{} {
	c.amu.Lock()
	defer c.amu.Unlock()
	return c.arrival
}

// request sends the lines build returns, labeled, and waits for the reply that
// carries the label back. Every caller is bounded by its own context; the
// connection dying ends the wait too.
func (c *conn) request(ctx context.Context, build func(label string) []line) ([]line, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	label := strconv.FormatUint(c.labels.Add(1), 36)
	w := &waiter{done: make(chan struct{})}
	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return nil, err
	}
	c.waiters[label] = w
	c.mu.Unlock()
	defer c.drop(label)

	if err := c.writeLines(ctx, build(label)...); err != nil {
		return nil, err
	}
	// The request is on the wire from here: everything below is about how long
	// to wait for the answer, and by whose clock.
	return c.await(ctx, w, label)
}

// await blocks until the response is complete, the connection ends, the
// adapter's own patience runs out or the caller's does.
func (c *conn) await(ctx context.Context, w *waiter, label string) ([]line, error) {
	t := time.NewTimer(replyTimeout)
	defer t.Stop()
	select {
	case <-w.done:
		return w.answer()
	case <-c.done:
		// The answer arriving and the connection ending can land together — a
		// server that replies and then drops the socket does both — and a
		// select picks between ready cases at random. An answer in hand is an
		// answer, whatever happened to the connection afterwards.
		select {
		case <-w.done:
			if lines, err := w.answer(); err == nil {
				return lines, nil
			}
		default:
		}
		return nil, c.connErr()
	case <-t.C:
		return nil, fmt.Errorf("irc: the server at %s took the request but did not answer label %s within %s", c.addr, label, replyTimeout)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// answer is what a finished waiter holds. A response released by the connection
// dying is not a response: what it holds is whatever had arrived, which is not
// what was asked for.
func (w *waiter) answer() ([]line, error) {
	if w.err != nil {
		return nil, w.err
	}
	return w.lines, nil
}

// drop forgets a request that is over, along with any batch still open under
// it: a response cut short by a cancelled context must not leave the dispatcher
// routing lines into a waiter nobody reads.
func (c *conn) drop(label string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.waiters, label)
	for id, l := range c.batchOf {
		if l == label {
			delete(c.batchOf, id)
		}
	}
}

// writeLines writes whole lines, serialized against every other writer. The
// deadline is the caller's when it has one and writeTimeout otherwise: a write
// that cannot finish inside what the caller asked for is not one to keep the
// socket for, and a caller with no deadline of its own still gets a ceiling.
func (c *conn) writeLines(ctx context.Context, lines ...line) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l.String())
		b.WriteString("\r\n")
	}
	select {
	case c.wsem <- struct{}{}:
		defer func() { <-c.wsem }()
	case <-ctx.Done():
		return ctx.Err()
	}
	deadline := time.Now().Add(writeTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := c.nc.SetWriteDeadline(deadline); err != nil {
		return fmt.Errorf("irc: %w", err)
	}
	// A context with no deadline still has to be able to interrupt a write that
	// is going nowhere. The watch is joined by this defer, which runs before
	// the one that releases the slot: a cancel landing just as the write
	// finished would otherwise set its deadline under whoever writes next.
	stopWatch := watchCancel(ctx, func() { c.nc.SetWriteDeadline(time.Now()) })
	defer stopWatch()
	if _, err := c.nc.Write([]byte(b.String())); err != nil {
		// Half a line may have gone out, so the connection is finished either
		// way; what the caller is told about is its own context when that is
		// what ended the write, since a wait reaching its deadline is a quiet
		// timeout and not a failure. That includes a socket deadline armed at
		// the caller's, which can fire before the context has recorded it.
		c.fail(err)
		if cerr := deadlineErr(ctx, err); cerr != nil {
			return fmt.Errorf("irc: write to %s: %w", c.addr, cerr)
		}
		return fmt.Errorf("irc: write: %w", err)
	}
	return nil
}

// fail records why the connection ended and releases everything waiting on it.
// The first cause wins: the ones after it are what the first caused.
func (c *conn) fail(err error) {
	c.mu.Lock()
	if c.err == nil {
		c.err = fmt.Errorf("irc: connection to the server ended: %w", err)
	}
	for label := range c.waiters {
		c.finish(label, c.err)
	}
	c.mu.Unlock()
	c.closed.Do(func() {
		close(c.done)
		c.nc.Close()
	})
	// Anything blocked on an arrival is blocked on a connection that is gone.
	c.wake()
}

func (c *conn) connErr() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	return errors.New("irc: connection to the server ended")
}

func (c *conn) isDead() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

// quit closes the connection the polite way, so the server frees the nick at
// once instead of holding it until the socket times out.
func (c *conn) quit() error {
	if !c.isDead() {
		ctx, cancel := context.WithTimeout(context.Background(), quitTimeout)
		_ = c.writeLines(ctx, line{command: "QUIT", params: []string{"parley: done"}, colon: true})
		cancel()
	}
	var err error
	c.closed.Do(func() {
		close(c.done)
		err = c.nc.Close()
	})
	// Whoever is waiting for something to arrive on this connection is waiting
	// on one that is gone: releasing them is how a Close reaches a blocked Wait.
	c.wake()
	if err != nil {
		return fmt.Errorf("irc: close: %w", err)
	}
	return nil
}
