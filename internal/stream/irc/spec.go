package irc

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// MaxChannel bounds a channel name, in bytes, matching the CHANNELLEN this
// package's ergo config sets. A name longer than the server accepts is refused
// here rather than at the JOIN, where the failure would arrive as a numeric
// after a connection was already made.
const MaxChannel = 64

// scheme is the whole of what this adapter answers to.
const scheme = "irc://"

// Spec is a parsed irc:// stream spec: where to connect, and which channel is
// the stream. It is the whole of what the scheme carries — no user, no
// password, no options — because everything else about the connection is fixed
// by this package.
type Spec struct {
	// Addr is the server as host:port, both required.
	Addr string
	// Channel is the channel name, '#' included and lower-cased.
	Channel string
}

// String returns the canonical spelling of the spec, which is what keys the
// stream's state: one channel on one server keeps one cursor however the caller
// spelled it. IRC channel names are case-insensitive (ergo runs casemapping
// ascii), so the channel is lower-cased, and so is a host name; an address
// keeps its own spelling, since a name and an address are not interchangeable
// to a connection.
func (s Spec) String() string {
	return scheme + s.Addr + "/" + s.Channel
}

// ParseSpec parses an `irc://host:port/channel` spec. The path is the channel
// name and gets its '#' if it was left off, so `irc://127.0.0.1:6667/parley`,
// `…/#parley` and `…/%23parley` all name the same stream. The '#' is why this
// is not net/url's job: to a URL parser an unescaped '#' opens a fragment, and
// a channel is the one place where the character means itself.
//
// Anything the scheme has no meaning for — userinfo, a query, a second path
// element, a '#' anywhere but the front — is refused rather than ignored: a
// spec that carries something parley does not honour is a spec whose author
// expects something parley will not do.
func ParseSpec(spec string) (Spec, error) {
	if len(spec) < len(scheme) || !strings.EqualFold(spec[:len(scheme)], scheme) {
		// A spec is echoed only when it cannot hold a password: anything with
		// an '@' in it is described, not repeated.
		shown := fmt.Sprintf("%q", spec)
		if strings.Contains(spec, "@") {
			shown = "with a user or password in it"
		}
		if s, _, ok := strings.Cut(spec, ":"); ok && !strings.EqualFold(s, "irc") {
			return Spec{}, fmt.Errorf("bad irc stream %s: scheme is %q, not irc", shown, s)
		}
		return Spec{}, fmt.Errorf("bad irc stream %s: expected irc://host:port/channel", shown)
	}
	authority, name, ok := strings.Cut(spec[len(scheme):], "/")
	if strings.Contains(authority, "@") {
		// The spec is not echoed here: what stands before the '@' may be a
		// password, and an error is the one place it must not be written.
		return Spec{}, fmt.Errorf("bad irc stream: no user or password before the host; parley connects as --nick and keeps the account password itself")
	}
	if authority == "" {
		return Spec{}, fmt.Errorf("bad irc stream %q: no host; expected irc://host:port/channel", spec)
	}
	host, port, err := net.SplitHostPort(authority)
	if err != nil {
		return Spec{}, fmt.Errorf("bad irc stream %q: expected host:port, e.g. irc://127.0.0.1:6667/parley", spec)
	}
	if host == "" {
		return Spec{}, fmt.Errorf("bad irc stream %q: empty host", spec)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return Spec{}, fmt.Errorf("bad irc stream %q: %q is not a port", spec, port)
	}
	// One spelling of a number: :06667 and :6667 are the same port, and a state
	// key that told them apart would give one channel two cursors.
	port = strconv.Itoa(n)
	if err := loopbackHost(host); err != nil {
		return Spec{}, fmt.Errorf("bad irc stream %q: %v", spec, err)
	}
	// A name is case-insensitive, so two spellings of one are one server: they
	// must key one cursor and one account password. An address is not a name
	// and is left exactly as it was written.
	if net.ParseIP(host) == nil {
		host = asciiLower(host)
	}

	if !ok || name == "" {
		return Spec{}, fmt.Errorf("bad irc stream %q: no channel; expected irc://host:port/channel", spec)
	}
	if strings.Contains(name, "/") {
		return Spec{}, fmt.Errorf("bad irc stream %q: the path is one channel name, not a path", spec)
	}
	if strings.ContainsAny(name, "?") {
		return Spec{}, fmt.Errorf("bad irc stream %q: no query is accepted", spec)
	}
	name, err = url.PathUnescape(name)
	if err != nil {
		return Spec{}, fmt.Errorf("bad irc stream %q: %v", spec, err)
	}
	if strings.Contains(strings.TrimLeft(name, "#"), "#") {
		return Spec{}, fmt.Errorf("bad irc stream %q: no fragment is accepted; a channel's '#' belongs at the front of the name", spec)
	}
	if !strings.HasPrefix(name, "#") {
		name = "#" + name
	}
	if err := validChannel(name); err != nil {
		return Spec{}, fmt.Errorf("bad irc stream %q: channel %q %v", spec, name, err)
	}
	return Spec{Addr: net.JoinHostPort(host, port), Channel: asciiLower(name)}, nil
}

// asciiLower lower-cases A-Z and leaves every other byte alone. It is not
// strings.ToLower on purpose: that folds Unicode, and a server running
// casemapping ascii — ergo does — treats #Ä and #ä as two different channels.
// Folding them together here would give two channels one cursor.
func asciiLower(s string) string {
	var b []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			if b == nil {
				b = []byte(s)
			}
			b[i] = c + ('a' - 'A')
		}
	}
	if b == nil {
		return s
	}
	return string(b)
}

// asciiEqualFold compares by the same rule: ASCII case-insensitive, everything
// else byte for byte. It is what decides whether a line the server sent is
// about this adapter's channel.
func asciiEqualFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if x >= 'A' && x <= 'Z' {
			x += 'a' - 'A'
		}
		if y >= 'A' && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

// LoopbackAddr validates a host:port that must name a socket on this machine —
// where a server listens, or where a check of one connects — by the rule a
// stream spec's authority is held to, and returns it with the port in its one
// spelling. The host is a loopback IP literal, never a name: these callers bind
// or dial it directly, with no check of where a name resolved to, and a name
// such as localhost could resolve to an address on another interface. The port
// is a real one: 0 is the kernel's to choose, which names nothing a second
// process could be told to dial. The errors do not repeat addr: the caller
// names the flag it came from.
func LoopbackAddr(addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("expected host:port, e.g. 127.0.0.1:6667")
	}
	if host == "" {
		return "", fmt.Errorf("no host: name a loopback address, e.g. 127.0.0.1:6667")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("%q is not a port", port)
	}
	if err := loopbackHost(host); err != nil {
		return "", err
	}
	if net.ParseIP(host) == nil {
		return "", fmt.Errorf("%q is a host name: name a loopback IP address, 127.0.0.1 or ::1, since a name is resolved where it is used and could reach another interface", host)
	}
	return net.JoinHostPort(host, strconv.Itoa(n)), nil
}

// loopbackHost reports whether host names this machine and nowhere else. The
// scheme is loopback-only and this is where that is enforced: everything on the
// wire — the account password in a SASL PLAIN blob, every message — is in the
// clear, and the only reason that is an acceptable posture is that the bytes
// never leave the machine that wrote them. A remote server would need TLS, and
// TLS is not in this adapter.
func loopbackHost(host string) error {
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("host %q is not loopback: this scheme speaks in the clear, password included, so it reaches 127.0.0.0/8, ::1 and localhost only", host)
}

// validChannel reports what is wrong with a channel name. Two grammars meet
// here and the narrower one wins. IRC forbids a space, a comma, NUL, BEL, CR
// and LF; this also forbids '%', '/', '?' and any other control character,
// because Spec.String writes the name into a URL verbatim and those are the
// characters a URL reader would take for its own. Refusing them is what makes
// String canonical: every spec this accepts spells itself, and parsing that
// spelling gives the same spec back.
func validChannel(name string) error {
	if len(name) > MaxChannel {
		return fmt.Errorf("is %d bytes, the maximum is %d", len(name), MaxChannel)
	}
	if name == "#" {
		return fmt.Errorf("is empty")
	}
	for i := 0; i < len(name); i++ {
		switch c := name[i]; {
		case c <= ' ', c == 0x7f:
			return fmt.Errorf("contains %q", rune(c))
		case c == ',', c == '%', c == '/', c == '?':
			return fmt.Errorf("contains %q, which a stream spec cannot carry", rune(c))
		}
	}
	return nil
}
