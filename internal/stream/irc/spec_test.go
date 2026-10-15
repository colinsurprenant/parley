package irc

import (
	"context"
	"strings"
	"testing"

	"github.com/colinsurprenant/parley/internal/stream"
)

func TestParseSpec(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string // the canonical spelling, or "" when the spec must be refused
	}{
		{name: "bare channel name", in: "irc://127.0.0.1:6667/parley", want: "irc://127.0.0.1:6667/#parley"},
		{name: "escaped hash", in: "irc://127.0.0.1:6667/%23parley", want: "irc://127.0.0.1:6667/#parley"},
		{name: "channel case is folded", in: "irc://127.0.0.1:6667/#Parley", want: "irc://127.0.0.1:6667/#parley"},
		{name: "host name", in: "irc://localhost:6667/parley", want: "irc://localhost:6667/#parley"},
		{name: "ipv6 host", in: "irc://[::1]:6667/parley", want: "irc://[::1]:6667/#parley"},
		{name: "scheme case is folded", in: "IRC://127.0.0.1:6667/parley", want: "irc://127.0.0.1:6667/#parley"},
		{name: "trailing dot in a channel", in: "irc://127.0.0.1:6667/parley.m2", want: "irc://127.0.0.1:6667/#parley.m2"},
		{name: "a port with a leading zero", in: "irc://127.0.0.1:06667/parley", want: "irc://127.0.0.1:6667/#parley"},
		{name: "a port with several", in: "irc://127.0.0.1:0006667/parley", want: "irc://127.0.0.1:6667/#parley"},
		{name: "a host name in another case", in: "irc://LocalHost:6667/parley", want: "irc://localhost:6667/#parley"},
		{name: "a double-hash channel", in: "irc://127.0.0.1:6667/%23%23parley", want: "irc://127.0.0.1:6667/##parley"},
		{name: "the shortest double-hash channel", in: "irc://127.0.0.1:6667/##", want: "irc://127.0.0.1:6667/##"},

		{name: "no port", in: "irc://127.0.0.1/parley"},
		{name: "a host off this machine", in: "irc://irc.libera.chat:6667/parley"},
		{name: "port is not a number", in: "irc://127.0.0.1:irc/parley"},
		{name: "port out of range", in: "irc://127.0.0.1:70000/parley"},
		{name: "no host", in: "irc:///parley"},
		{name: "no channel", in: "irc://127.0.0.1:6667"},
		{name: "empty channel", in: "irc://127.0.0.1:6667/"},
		{name: "a path, not a channel", in: "irc://127.0.0.1:6667/parley/m2"},
		{name: "userinfo", in: "irc://colin@127.0.0.1:6667/parley"},
		{name: "password", in: "irc://colin:hunter2@127.0.0.1:6667/parley"},
		{name: "password behind a wrong scheme", in: "ircs://colin:hunter2@127.0.0.1:6697/parley"},
		{name: "password behind no scheme", in: "colin:hunter2@127.0.0.1:6667/parley"},
		{name: "query", in: "irc://127.0.0.1:6667/parley?tls=1"},
		{name: "fragment", in: "irc://127.0.0.1:6667/parley#m2"},
		{name: "wrong scheme", in: "file:/tmp/chat.ndjson"},
		{name: "opaque", in: "irc:127.0.0.1:6667/parley"},
		{name: "channel too long", in: "irc://127.0.0.1:6667/" + strings.Repeat("c", MaxChannel)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseSpec(tc.in)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("ParseSpec(%q) = %+v, want an error", tc.in, got)
				}
				if user, _, ok := strings.Cut(tc.in[len("irc://"):], "@"); ok && strings.Contains(tc.in, "@") {
					// What stands before the '@' may be a password: the error
					// must not carry it, nor the spec that holds it.
					if strings.Contains(err.Error(), user) || strings.Contains(err.Error(), tc.in) {
						t.Errorf("the error for a spec with userinfo echoes it: %v", err)
					}
					return
				}
				if !strings.Contains(err.Error(), tc.in) {
					t.Errorf("the error for %q does not name the spec: %v", tc.in, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseSpec(%q): %v", tc.in, err)
			}
			if got.String() != tc.want {
				t.Fatalf("ParseSpec(%q).String() = %q, want %q", tc.in, got.String(), tc.want)
			}
		})
	}
}

// TestParseSpecTakesOnlyLoopbackHosts: the scheme puts the account password on
// the wire in the clear, so the wire is not allowed to leave the machine. This
// is where that is enforced, and there is no flag to turn it off.
func TestParseSpecTakesOnlyLoopbackHosts(t *testing.T) {
	accepted := []string{
		"irc://127.0.0.1:6667/parley",
		"irc://127.0.0.2:6667/parley",
		"irc://127.255.255.254:6667/parley",
		"irc://localhost:6667/parley",
		"irc://LocalHost:6667/parley",
		"irc://[::1]:6667/parley",
	}
	for _, in := range accepted {
		if _, err := ParseSpec(in); err != nil {
			t.Errorf("ParseSpec(%q) was refused: %v", in, err)
		}
	}
	refused := []string{
		"irc://irc.libera.chat:6667/parley",
		"irc://10.0.0.5:6667/parley",
		"irc://192.168.1.10:6667/parley",
		"irc://0.0.0.0:6667/parley",
		"irc://[2001:db8::1]:6667/parley",
		"irc://localhost.evil.example:6667/parley",
		"irc://example.com:6667/parley",
	}
	for _, in := range refused {
		_, err := ParseSpec(in)
		if err == nil {
			t.Errorf("ParseSpec(%q) was accepted; it is not loopback", in)
			continue
		}
		if !strings.Contains(err.Error(), "loopback") {
			t.Errorf("the error for %q does not name the boundary: %v", in, err)
		}
	}
}

// TestParseSpecRoundTripsItsOwnSpelling: Spec.String is the state key, so it
// has to be a spec in its own right — parsing what it writes must give the same
// spec back. That holds only because the channel grammar refuses every
// character String would have to escape.
func TestParseSpecRoundTripsItsOwnSpelling(t *testing.T) {
	for _, in := range []string{
		"irc://127.0.0.1:6667/parley",
		"irc://127.0.0.1:6667/%23parley",
		"irc://127.0.0.1:6667/#Parley",
		"irc://localhost:6667/parley.m2",
		"irc://[::1]:6667/parley-m2_x",
		"irc://127.0.0.1:6667/#ä",
		"irc://127.0.0.1:6667/parley+m2",
		"irc://127.0.0.1:6667/parley:m2",
		"irc://127.0.0.1:6667/parley@m2",
		"irc://127.0.0.1:6667/parley&m2",
	} {
		first, err := ParseSpec(in)
		if err != nil {
			t.Errorf("ParseSpec(%q): %v", in, err)
			continue
		}
		again, err := ParseSpec(first.String())
		if err != nil {
			t.Errorf("ParseSpec(%q) wrote %q, which does not parse: %v", in, first.String(), err)
			continue
		}
		if again != first {
			t.Errorf("ParseSpec(%q) = %+v, but its own spelling %q parses as %+v", in, first, first.String(), again)
		}
	}

	// The characters a URL reader would take for its own are refused rather
	// than escaped, in either spelling.
	for _, in := range []string{
		"irc://127.0.0.1:6667/parley%25m2",
		"irc://127.0.0.1:6667/parley%2Fm2",
		"irc://127.0.0.1:6667/parley%3Fm2",
		"irc://127.0.0.1:6667/parley%20m2",
		"irc://127.0.0.1:6667/parley%2Cm2",
		"irc://127.0.0.1:6667/parley%07m2",
		"irc://127.0.0.1:6667/parley%0Am2",
	} {
		if got, err := ParseSpec(in); err == nil {
			t.Errorf("ParseSpec(%q) was accepted as %q", in, got.String())
		}
	}
}

// TestHostNameCaseIsFoldedButAnAddressIsNot: a name is case-insensitive, so
// two spellings of one name are one server and must key one cursor and one
// account password. An address is not a name and is left as it was written.
func TestHostNameCaseIsFoldedButAnAddressIsNot(t *testing.T) {
	for _, in := range []string{"irc://localhost:6667/parley", "irc://LocalHost:6667/parley", "irc://LOCALHOST:6667/parley"} {
		got, err := ParseSpec(in)
		if err != nil {
			t.Fatalf("ParseSpec(%q): %v", in, err)
		}
		if got.Addr != "localhost:6667" {
			t.Errorf("ParseSpec(%q).Addr = %q, want %q", in, got.Addr, "localhost:6667")
		}
	}
	// An IPv6 literal has no case to fold: its own spelling is what a
	// connection needs.
	got, err := ParseSpec("irc://[::1]:6667/parley")
	if err != nil {
		t.Fatalf("ParseSpec: %v", err)
	}
	if got.Addr != "[::1]:6667" {
		t.Errorf("Addr = %q, want %q", got.Addr, "[::1]:6667")
	}
}

// TestChannelCaseIsFoldedTheWayTheServerFoldsIt: ergo runs casemapping ascii,
// so #Ä and #ä are two channels there. Folding them together here would give
// two channels one cursor and one participant two histories.
func TestChannelCaseIsFoldedTheWayTheServerFoldsIt(t *testing.T) {
	lower, err := ParseSpec("irc://127.0.0.1:6667/#ä")
	if err != nil {
		t.Fatalf("ParseSpec: %v", err)
	}
	upper, err := ParseSpec("irc://127.0.0.1:6667/#Ä")
	if err != nil {
		t.Fatalf("ParseSpec: %v", err)
	}
	if lower.Channel == upper.Channel {
		t.Fatalf("#ä and #Ä were folded to the same channel (%q); the server keeps them apart", lower.Channel)
	}
	if upper.Channel != "#Ä" {
		t.Errorf("the non-ASCII channel came back as %q, want it unchanged", upper.Channel)
	}
	// ASCII still folds, because the server folds it.
	ascii, err := ParseSpec("irc://127.0.0.1:6667/#PARLEY")
	if err != nil {
		t.Fatalf("ParseSpec: %v", err)
	}
	if ascii.Channel != "#parley" {
		t.Errorf("an ASCII channel came back as %q, want %q", ascii.Channel, "#parley")
	}
}

func TestParseSpecCanonicalKeyIsOnePerChannel(t *testing.T) {
	specs := []string{
		"irc://127.0.0.1:6667/parley",
		"irc://127.0.0.1:6667/#parley",
		"irc://127.0.0.1:6667/%23parley",
		"irc://127.0.0.1:6667/#Parley",
		"irc://127.0.0.1:6667/PARLEY",
		// One port, however it is spelled: a state key that told these apart
		// would give one channel two cursors.
		"irc://127.0.0.1:06667/parley",
	}
	want := "irc://127.0.0.1:6667/#parley"
	for _, in := range specs {
		got, err := ParseSpec(in)
		if err != nil {
			t.Fatalf("ParseSpec(%q): %v", in, err)
		}
		if got.String() != want {
			t.Errorf("ParseSpec(%q).String() = %q, want %q", in, got.String(), want)
		}
	}
}

// TestSendRefusesABodyBeforeItConnects: a body that no IRC line can carry, or
// that no parley reader would deliver, is refused where it was written rather
// than at a socket — none of these touch the network, and the stream below
// points at a port with nothing on it.
func TestSendRefusesABodyBeforeItConnects(t *testing.T) {
	spec, err := ParseSpec("irc://127.0.0.1:1/parley")
	if err != nil {
		t.Fatalf("ParseSpec: %v", err)
	}
	s, err := Open(spec, "alpha", NewStateCredentials(t.TempDir(), "alpha"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	tests := map[string]struct {
		body string
		says string
	}{
		"empty":                     {body: "", says: "empty"},
		"a NUL":                     {body: "before\x00after", says: "NUL"},
		"a carriage return":         {body: "before\rafter", says: "carriage return"},
		"a CTCP delimiter":          {body: "\x01ACTION waves\x01", says: "CTCP"},
		"a CTCP delimiter mid-body": {body: "plain text with \x01 in it", says: "CTCP"},
	}
	for name, tc := range tests {
		_, err := s.Send(context.Background(), tc.body)
		if err == nil {
			t.Errorf("Send of a body with %s was accepted", name)
			continue
		}
		if !strings.Contains(err.Error(), tc.says) {
			t.Errorf("the error for %s does not say what is wrong: %v", name, err)
		}
	}
}

func TestOpenChecksTheNick(t *testing.T) {
	spec, err := ParseSpec("irc://127.0.0.1:6667/parley")
	if err != nil {
		t.Fatalf("ParseSpec: %v", err)
	}
	creds := NewStateCredentials(t.TempDir(), "alpha")
	// The dotted nick is here because a server answers one with 432 at the end
	// of a handshake; stream.ValidNick refuses it before a socket is opened.
	for _, nick := range []string{"", "two words", "slash/nick", "codex-a.m2", strings.Repeat("n", 65)} {
		if _, err := Open(spec, nick, creds); err == nil {
			t.Errorf("Open with nick %q was accepted", nick)
		}
	}
	if _, err := Open(spec, "alpha", nil); err == nil {
		t.Error("Open with no credential store was accepted")
	}
	s, err := Open(spec, "alpha", creds)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got := s.Spec(); got != "irc://127.0.0.1:6667/#parley" {
		t.Errorf("Spec() = %q", got)
	}
	// Nothing was dialed: there is no server on that port in this test, and
	// Open still succeeded.
	if err := s.Close(); err != nil {
		t.Errorf("Close of an unused stream: %v", err)
	}
}

// TestReadRefusesABadCursorWithoutConnecting: a cursor this adapter could not
// have issued is the caller's mistake, and learning that should not cost a
// connection — the port below has nothing on it, so a dial would be the error
// if one were attempted.
func TestReadRefusesABadCursorWithoutConnecting(t *testing.T) {
	spec, err := ParseSpec("irc://127.0.0.1:1/parley")
	if err != nil {
		t.Fatalf("ParseSpec: %v", err)
	}
	s, err := Open(spec, "alpha", NewStateCredentials(t.TempDir(), "alpha"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	msgs, next, err := s.Read(context.Background(), stream.Cursor("not a\r\nmsgid"))
	if err == nil {
		t.Fatalf("a cursor holding a line break was accepted, returning %d messages", len(msgs))
	}
	if !strings.Contains(err.Error(), "bad cursor") {
		t.Fatalf("Read reported %v, want the cursor to be refused before anything was dialed", err)
	}
	if next != stream.Cursor("not a\r\nmsgid") {
		t.Errorf("the refused read moved the cursor to %q", string(next))
	}
}

// TestLoopbackAddrTakesOnlyLoopbackLiterals: the server's listener and a
// check's dial use the address as given, so a name, even localhost, is refused.
func TestLoopbackAddrTakesOnlyLoopbackLiterals(t *testing.T) {
	for in, want := range map[string]string{"127.0.0.1:6667": "127.0.0.1:6667", "127.0.0.2:06667": "127.0.0.2:6667", "[::1]:6667": "[::1]:6667"} {
		if got, err := LoopbackAddr(in); err != nil || got != want {
			t.Errorf("LoopbackAddr(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"localhost:6667", "LocalHost:6667", "example.com:6667", "0.0.0.0:6667", "127.0.0.1:0", ":6667"} {
		if got, err := LoopbackAddr(in); err == nil {
			t.Errorf("LoopbackAddr(%q) = %q, want a refusal", in, got)
		}
	}
	if _, err := LoopbackAddr("localhost:6667"); err == nil || !strings.Contains(err.Error(), "host name") {
		t.Errorf("localhost is not refused as a host name: %v", err)
	}
}
