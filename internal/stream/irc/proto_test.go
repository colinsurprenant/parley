package irc

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/colinsurprenant/parley/internal/stream"
)

func TestParseLine(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    line
		wantErr bool
	}{
		{
			name: "no prefix, no tags",
			in:   "PING :ergo.test",
			want: line{command: "PING", params: []string{"ergo.test"}},
		},
		{
			name: "prefix and trailing with spaces",
			in:   ":alpha!~alpha@localhost PRIVMSG #parley :hello there, world",
			want: line{prefix: "alpha!~alpha@localhost", command: "PRIVMSG", params: []string{"#parley", "hello there, world"}},
		},
		{
			name: "empty trailing survives",
			in:   ":alpha!~alpha@localhost PRIVMSG #parley :",
			want: line{prefix: "alpha!~alpha@localhost", command: "PRIVMSG", params: []string{"#parley", ""}},
		},
		{
			name: "trailing that begins with a colon",
			in:   "PRIVMSG #parley ::-)",
			want: line{command: "PRIVMSG", params: []string{"#parley", ":-)"}},
		},
		{
			name: "tags with escapes",
			in:   `@msgid=abc;time=2026-09-14T01:02:03.456Z;x=a\:b\sc\\d :ergo.test NOTICE #parley :hi`,
			want: line{
				tags:    map[string]string{"msgid": "abc", "time": "2026-09-14T01:02:03.456Z", "x": `a;b c\d`},
				prefix:  "ergo.test",
				command: "NOTICE",
				params:  []string{"#parley", "hi"},
			},
		},
		{
			name: "valueless tag is present and empty",
			in:   "@draft/multiline-concat;batch=1 :alpha!u@h PRIVMSG #parley :more",
			want: line{
				tags:    map[string]string{"draft/multiline-concat": "", "batch": "1"},
				prefix:  "alpha!u@h",
				command: "PRIVMSG",
				params:  []string{"#parley", "more"},
			},
		},
		{
			name: "command is upper-cased",
			in:   "privmsg #parley :shouting quietly",
			want: line{command: "PRIVMSG", params: []string{"#parley", "shouting quietly"}},
		},
		{
			name: "batch close has no trailing",
			in:   ":ergo.test BATCH -3",
			want: line{prefix: "ergo.test", command: "BATCH", params: []string{"-3"}},
		},
		{name: "tags but no command", in: "@only=tags", wantErr: true},
		{name: "prefix but no command", in: ":ergo.test", wantErr: true},
		{name: "empty line", in: "", wantErr: true},
		{name: "trailing before a command", in: ":a :b", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseLine(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseLine(%q) = %+v, want an error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseLine(%q): %v", tc.in, err)
			}
			if got.prefix != tc.want.prefix || got.command != tc.want.command {
				t.Fatalf("parseLine(%q) = %+v, want %+v", tc.in, got, tc.want)
			}
			if len(got.params) != len(tc.want.params) {
				t.Fatalf("parseLine(%q) params = %q, want %q", tc.in, got.params, tc.want.params)
			}
			for i := range got.params {
				if got.params[i] != tc.want.params[i] {
					t.Fatalf("parseLine(%q) params = %q, want %q", tc.in, got.params, tc.want.params)
				}
			}
			if len(got.tags) != len(tc.want.tags) {
				t.Fatalf("parseLine(%q) tags = %v, want %v", tc.in, got.tags, tc.want.tags)
			}
			for k, v := range tc.want.tags {
				if got.tags[k] != v {
					t.Fatalf("parseLine(%q) tag %q = %q, want %q", tc.in, k, got.tags[k], v)
				}
			}
		})
	}
}

func TestLineRoundTrip(t *testing.T) {
	tests := []string{
		"PING :ergo.test",
		":alpha!~alpha@localhost PRIVMSG #parley :hello there",
		":alpha!~alpha@localhost PRIVMSG #parley :",
		"@batch=1;draft/multiline-concat PRIVMSG #parley :more text",
		"@msgid=abc;time=2026-09-14T01:02:03.456Z :a!u@h PRIVMSG #parley :hi",
		":ergo.test BATCH -3",
	}
	for _, in := range tests {
		l, err := parseLine(in)
		if err != nil {
			t.Fatalf("parseLine(%q): %v", in, err)
		}
		if got := l.String(); got != in {
			t.Errorf("round trip\n got: %q\nwant: %q", got, in)
		}
	}
}

func TestTagValueEscapeRoundTrip(t *testing.T) {
	values := []string{
		"",
		"plain",
		"semi;colon",
		"a space",
		`back\slash`,
		"carriage\rreturn",
		"new\nline",
		"all\\ of\r\n them;at once",
	}
	for _, v := range values {
		escaped := escapeTagValue(v)
		if strings.ContainsAny(escaped, "; \r\n") {
			t.Errorf("escapeTagValue(%q) = %q, which still holds a separator", v, escaped)
		}
		if got := unescapeTagValue(escaped); got != v {
			t.Errorf("round trip of %q through %q gave %q", v, escaped, got)
		}
	}
}

func TestUnescapeTagValueIsLenient(t *testing.T) {
	tests := map[string]string{
		`a\qb`:   "aqb",   // unknown escape: the backslash goes, the letter stays
		`trail\`: "trail", // a lone trailing backslash is dropped
		`\s\:\\`: " ;\\",
	}
	for in, want := range tests {
		if got := unescapeTagValue(in); got != want {
			t.Errorf("unescapeTagValue(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLineWithTagsSerializesSorted(t *testing.T) {
	l := line{
		tags:    map[string]string{"label": "7", "batch": "2", "draft/multiline-concat": ""},
		command: "PRIVMSG",
		params:  []string{"#parley", "hi"},
		colon:   true,
	}
	want := "@batch=2;draft/multiline-concat;label=7 PRIVMSG #parley :hi"
	if got := l.String(); got != want {
		t.Fatalf("line.String()\n got: %q\nwant: %q", got, want)
	}
}

func TestNickFromPrefix(t *testing.T) {
	tests := map[string]string{
		"alpha!~alpha@localhost": "alpha",
		"alpha@localhost":        "alpha",
		"ergo.test":              "ergo.test",
		"":                       "",
	}
	for in, want := range tests {
		if got := nickFromPrefix(in); got != want {
			t.Errorf("nickFromPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseTime(t *testing.T) {
	want := time.Date(2026, 9, 14, 1, 2, 3, 456000000, time.UTC)
	for _, in := range []string{"2026-09-14T01:02:03.456Z", "2026-09-14T01:02:03.456000Z", "2026-09-14T01:02:03.456+00:00"} {
		got := parseTime(in)
		if !got.Equal(want) {
			t.Errorf("parseTime(%q) = %v, want %v", in, got, want)
		}
		if got.Location() != time.UTC {
			t.Errorf("parseTime(%q) is in %v, want UTC", in, got.Location())
		}
	}
	for _, in := range []string{"", "not a time", "14/09/2026"} {
		if got := parseTime(in); !got.IsZero() {
			t.Errorf("parseTime(%q) = %v, want the zero time", in, got)
		}
	}
}

func TestDecodeBody(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		deliver bool
	}{
		{in: "plain text", want: "plain text", deliver: true},
		{in: "\x01ACTION waves\x01", want: "/me waves", deliver: true},
		{in: "\x01ACTION\x01", want: "/me", deliver: true},
		{in: "\x01VERSION\x01", deliver: false},
		{in: "\x01PING 12345\x01", deliver: false},
	}
	for _, tc := range tests {
		got, ok := decodeBody(tc.in)
		if ok != tc.deliver {
			t.Fatalf("decodeBody(%q) delivered = %v, want %v", tc.in, ok, tc.deliver)
		}
		if ok && got != tc.want {
			t.Errorf("decodeBody(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCheckCursor(t *testing.T) {
	for _, ok := range []string{"6egu3hpfg8gwuyq9dyqjmr7jws", "a", "0:deadbeef", strings.Repeat("m", maxCursor)} {
		if err := checkCursor(stream.Cursor(ok)); err != nil {
			t.Errorf("checkCursor(%q) = %v, want it accepted", ok, err)
		}
	}
	bad := []string{
		"msgid with a space",
		"line\r\nQUIT :injected",
		"tag;separator",
		"comma,separator",
		"nul\x00byte",
		strings.Repeat("m", maxCursor+1),
	}
	for _, in := range bad {
		if err := checkCursor(stream.Cursor(in)); err == nil {
			t.Errorf("checkCursor(%q) was accepted", in)
		}
	}
}

// testConn is a connection with no socket, for the parts of the adapter that
// are pure: framing a body is arithmetic on the limits the server gave, and
// needs nothing from it once they are known.
func testConn(maxBytes, maxLines int) *conn {
	return &conn{
		nick:     "alpha",
		channel:  "#parley",
		prefix:   "alpha!~alpha@localhost",
		maxBytes: maxBytes,
		maxLines: maxLines,
	}
}

func TestChunkSize(t *testing.T) {
	c := testConn(65536, 1000)
	if got := c.chunkSize(); got != maxChunk {
		t.Fatalf("chunkSize() = %d for an ordinary prefix, want %d", got, maxChunk)
	}
	// A 64-byte nick on a long host and a long channel leave less than the round
	// number, and the chunk has to shrink to what the line can still carry.
	c.prefix = strings.Repeat("n", 64) + "!~" + strings.Repeat("u", 19) + "@" + strings.Repeat("h", 63)
	c.channel = "#" + strings.Repeat("c", 63)
	want := maxRestLen - 2 - 1 - len(c.prefix) - len(" PRIVMSG ") - len(c.channel) - 2
	if got := c.chunkSize(); got != want {
		t.Fatalf("chunkSize() = %d for a long prefix, want %d", got, want)
	}
	if got := c.chunkSize(); got >= maxChunk {
		t.Fatalf("chunkSize() = %d, which is not smaller than the %d ceiling", got, maxChunk)
	}
}

func TestFrameChunksAtRuneBoundaries(t *testing.T) {
	c := testConn(65536, 1000)
	// One ASCII byte and then 150 three-byte runes: 451 bytes, and the 300-byte
	// mark lands inside a rune, so the first chunk stops at the boundary before
	// it — 298 bytes, not 300.
	body := "x" + strings.Repeat("\u20ac", 150)
	chunks, err := c.frame(body)
	if err != nil {
		t.Fatalf("frame: %v", err)
	}
	if len(chunks) != 2 {
		t.Fatalf("frame gave %d chunks, want 2", len(chunks))
	}
	if len(chunks[0].text) != 298 {
		t.Errorf("first chunk is %d bytes, want 298 (the last rune boundary at or under %d)", len(chunks[0].text), maxChunk)
	}
	if chunks[0].concat {
		t.Error("the first chunk is marked as a continuation")
	}
	if !chunks[1].concat {
		t.Error("the second chunk of one line is not marked as a continuation")
	}
	if got := chunks[0].text + chunks[1].text; got != body {
		t.Error("the chunks do not rejoin into the body")
	}
	for i, ch := range chunks {
		if !utf8.ValidString(ch.text) {
			t.Errorf("chunk %d was cut inside a rune", i)
		}
		if len(ch.text) > maxChunk {
			t.Errorf("chunk %d is %d bytes, over the %d limit", i, len(ch.text), maxChunk)
		}
	}
}

func TestFrameKeepsEmptyLines(t *testing.T) {
	c := testConn(65536, 1000)
	chunks, err := c.frame("one\n\nthree\n")
	if err != nil {
		t.Fatalf("frame: %v", err)
	}
	want := []chunk{{text: "one"}, {}, {text: "three"}, {}}
	if len(chunks) != len(want) {
		t.Fatalf("frame gave %d chunks (%+v), want %d", len(chunks), chunks, len(want))
	}
	for i := range want {
		if chunks[i] != want[i] {
			t.Fatalf("chunk %d = %+v, want %+v", i, chunks[i], want[i])
		}
	}
}

func TestFrameRefusesWhatTheServerWillNot(t *testing.T) {
	c := testConn(64, 4)
	if _, err := c.frame(strings.Repeat("x", 65)); err == nil {
		t.Error("a body over max-bytes was framed")
	}
	// Five empty lines are five lines on the wire and only four bytes.
	if _, err := c.frame("\n\n\n\n"); err == nil {
		t.Error("a body over max-lines was framed")
	}
	if _, err := c.frame("fine"); err != nil {
		t.Errorf("a body inside both limits was refused: %v", err)
	}
}

// TestAuthReplyClassification pins what each exchange makes of the lines a
// server can answer with. The lines are literal because that is how they arrive
// — the classifier's whole job is to not need a server to be right about them.
func TestAuthReplyClassification(t *testing.T) {
	tests := []struct {
		line     string
		register replyKind
		sasl     replyKind
	}{
		{
			line:     ":ergo.test REGISTER SUCCESS alpha :Account successfully registered",
			register: replyDone, sasl: replyPassing,
		},
		{
			line:     ":ergo.test REGISTER VERIFICATION_REQUIRED alpha :Check your email",
			register: replyRefused, sasl: replyPassing,
		},
		{
			line:     ":ergo.test FAIL REGISTER ACCOUNT_EXISTS alpha :Username is already registered or otherwise unavailable",
			register: replyRefused, sasl: replyPassing,
		},
		{
			line:     ":ergo.test FAIL REGISTER BAD_ACCOUNT_NAME alpha :Invalid account name",
			register: replyRefused, sasl: replyPassing,
		},
		{
			line:     ":ergo.test FAIL AUTHENTICATE MECHANISM_UNSUPPORTED :PLAIN is not available",
			register: replyPassing, sasl: replyRefused,
		},
		{
			line:     ":ergo.test FAIL SASL ALREADY_AUTHENTICATED :Already logged in",
			register: replyPassing, sasl: replyRefused,
		},
		{
			// A FAIL for some other command is not this exchange's answer.
			line:     ":ergo.test FAIL CHATHISTORY INVALID_TARGET LATEST #parley :Messages could not be retrieved",
			register: replyPassing, sasl: replyPassing,
		},
		{
			// Logged in: true of both exchanges, the end of neither.
			line:     ":ergo.test 900 * * alpha :You are now logged in as alpha",
			register: replyPassing, sasl: replyPassing,
		},
		{
			line:     ":ergo.test 903 * :Authentication successful",
			register: replyRefused, sasl: replyDone,
		},
		{
			line:     ":ergo.test 907 * :You're already logged into an account",
			register: replyRefused, sasl: replyDone,
		},
		{
			line:     ":ergo.test 904 * :SASL authentication failed: Invalid account credentials",
			register: replyRefused, sasl: replyRefused,
		},
		{
			line:     ":ergo.test 902 * :You must use a nick assigned to you",
			register: replyRefused, sasl: replyRefused,
		},
		{
			line:     ":ergo.test 905 * :SASL message too long",
			register: replyRefused, sasl: replyRefused,
		},
		{
			line:     ":ergo.test 906 * :SASL authentication aborted",
			register: replyRefused, sasl: replyRefused,
		},
		{
			line:     ":ergo.test 908 alpha PLAIN :are the available SASL mechanisms",
			register: replyRefused, sasl: replyRefused,
		},
		{
			line:     ":ergo.test 433 * alpha :Nickname is already in use",
			register: replyRefused, sasl: replyRefused,
		},
		{
			line:     ":ergo.test 421 alpha REGISTER :Unknown command",
			register: replyRefused, sasl: replyRefused,
		},
		{
			// Everything below 400 is the server talking, not answering.
			line:     ":ergo.test 001 alpha :Welcome to the parley IRC Network alpha",
			register: replyPassing, sasl: replyPassing,
		},
		{
			line:     ":ergo.test 005 alpha CHATHISTORY=1000 :are supported by this server",
			register: replyPassing, sasl: replyPassing,
		},
		{
			line:     ":ergo.test NOTICE * :*** Looking up your hostname...",
			register: replyPassing, sasl: replyPassing,
		},
		{
			line:     ":ergo.test CAP * ACK :sasl",
			register: replyPassing, sasl: replyPassing,
		},
	}
	for _, tc := range tests {
		l, err := parseLine(tc.line)
		if err != nil {
			t.Fatalf("parseLine(%q): %v", tc.line, err)
		}
		if got := registerReply(l); got != tc.register {
			t.Errorf("registerReply(%q) = %s, want %s", tc.line, kindName(got), kindName(tc.register))
		}
		if got := saslReply(l); got != tc.sasl {
			t.Errorf("saslReply(%q) = %s, want %s", tc.line, kindName(got), kindName(tc.sasl))
		}
	}
}

func kindName(k replyKind) string {
	switch k {
	case replyPassing:
		return "passing"
	case replyDone:
		return "done"
	case replyRefused:
		return "refused"
	}
	return "unknown"
}

// TestAPageOfSyntheticItemsHasNoAnchor: a page whose every item carries a msgid
// the server will not resolve — its own narration of an event — is one this
// adapter cannot page past. Stopping there would look like the end of the
// history; it has to be said out loud instead.
func TestAPageOfSyntheticItemsHasNoAnchor(t *testing.T) {
	c := &conn{channel: "#parley"}
	page := func(lines ...string) page {
		t.Helper()
		var parsed []line
		for _, s := range lines {
			l, err := parseLine(s)
			if err != nil {
				t.Fatalf("parseLine(%q): %v", s, err)
			}
			parsed = append(parsed, l)
		}
		p, err := c.decodePage(parsed)
		if err != nil {
			t.Fatalf("decodePage: %v", err)
		}
		return p
	}

	// What ergo sends a client that did not negotiate draft/event-playback: a
	// PRIVMSG in shape, from a service, with a msgid nothing resolves.
	synthetic := page(
		":ergo.test BATCH +1 chathistory #parley",
		"@msgid=_av5mue7hy5ce4v8nzgp7h2gj9a;time=2026-09-14T01:02:03.456Z;batch=1 :HistServ!HistServ@localhost PRIVMSG #parley :alpha joined the channel",
		":ergo.test BATCH -1",
	)
	if synthetic.items != 1 {
		t.Fatalf("the page holds %d items, want 1", synthetic.items)
	}
	if len(synthetic.msgs) != 0 {
		t.Fatalf("a synthetic item was delivered as a message: %+v", synthetic.msgs)
	}
	if synthetic.end {
		t.Fatal("the page claims to end the history")
	}
	for _, end := range []string{"oldest", "newest"} {
		if _, err := synthetic.anchor(end); err == nil {
			t.Errorf("the %s end of a page of synthetic items offered an anchor", end)
		} else if !strings.Contains(err.Error(), "no id parley can page from") {
			t.Errorf("the error does not name the condition: %v", err)
		}
	}

	// And the ordinary case, for contrast: a page with real items anchors at
	// either end.
	real := page(
		":ergo.test BATCH +2 chathistory #parley",
		"@msgid=gfdf2qma95tquqv8j46p4g866s;time=2026-09-14T01:02:03.456Z;batch=2 :alpha!~alpha@localhost PRIVMSG #parley :first",
		"@msgid=avsdrig8mry9e9bp5bt3y9rfcn;time=2026-09-14T01:02:04.456Z;batch=2 :alpha!~alpha@localhost PRIVMSG #parley :second",
		":ergo.test BATCH -2",
	)
	oldest, err := real.anchor("oldest")
	if err != nil || oldest != "gfdf2qma95tquqv8j46p4g866s" {
		t.Errorf("oldest anchor = (%q, %v)", oldest, err)
	}
	newest, err := real.anchor("newest")
	if err != nil || newest != "avsdrig8mry9e9bp5bt3y9rfcn" {
		t.Errorf("newest anchor = (%q, %v)", newest, err)
	}
}
