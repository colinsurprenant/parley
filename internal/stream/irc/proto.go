package irc

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// The IRCv3 line budget: 8191 bytes of message tags, then the 512 bytes
// (CRLF included) an RFC 1459 line may occupy. Their sum is the longest line a
// conforming server sends; anything longer is a protocol error rather than a
// line to buffer, since the only thing on the other end of an over-long line is
// a peer that is not speaking IRC.
const (
	maxTagsLen = 8191
	maxRestLen = 512
	maxLineLen = maxTagsLen + maxRestLen
)

// errLineTooLong reports a line over maxLineLen. It ends the connection: the
// reader cannot tell where the next line starts.
var errLineTooLong = errors.New("irc: line exceeds the maximum length")

// line is one IRC protocol line, in either direction. A tag with no value is
// held as an empty string, which is also how it is written back out: the
// message-tags grammar makes `@a` and `@a=` the same tag.
type line struct {
	tags    map[string]string
	prefix  string
	command string
	params  []string
	// colon marks the last parameter as the trailing one — the free text a
	// command ends with. It is set by the parser when it saw the colon and by
	// every line this package builds that ends in text, so a body of one word
	// is written the same way as a body of two.
	colon bool
}

// parseLine decodes one line, without its CRLF. The command is upper-cased —
// commands are case-insensitive and numerics are digits either way — so every
// comparison in this package is against an upper-case literal.
func parseLine(s string) (line, error) {
	s = strings.TrimRight(s, "\r\n")
	var l line
	if rest, ok := strings.CutPrefix(s, "@"); ok {
		tags, after, found := strings.Cut(rest, " ")
		if !found {
			return line{}, fmt.Errorf("irc: line has tags but no command: %q", s)
		}
		l.tags = parseTags(tags)
		s = strings.TrimLeft(after, " ")
	}
	if rest, ok := strings.CutPrefix(s, ":"); ok {
		prefix, after, found := strings.Cut(rest, " ")
		if !found {
			return line{}, fmt.Errorf("irc: line has a prefix but no command: %q", s)
		}
		l.prefix = prefix
		s = strings.TrimLeft(after, " ")
	}
	for s != "" {
		// A colon opens the trailing parameter: everything after it is one
		// parameter, spaces and all, and it may be empty. Before the command
		// there is nothing for it to trail.
		if rest, ok := strings.CutPrefix(s, ":"); ok {
			if l.command == "" {
				return line{}, fmt.Errorf("irc: line has no command: %q", s)
			}
			l.params = append(l.params, rest)
			l.colon = true
			s = ""
			break
		}
		tok, after, _ := strings.Cut(s, " ")
		if tok != "" {
			if l.command == "" {
				l.command = strings.ToUpper(tok)
			} else {
				l.params = append(l.params, tok)
			}
		}
		s = strings.TrimLeft(after, " ")
	}
	if l.command == "" {
		return line{}, errors.New("irc: empty line")
	}
	return l, nil
}

// String encodes the line for the wire, without its CRLF. Tags are written in
// sorted order: a server reads them as a set, and a stable order is one less
// thing for a test to sort out.
func (l line) String() string {
	var b strings.Builder
	if len(l.tags) > 0 {
		b.WriteByte('@')
		for i, k := range slices.Sorted(maps.Keys(l.tags)) {
			if i > 0 {
				b.WriteByte(';')
			}
			b.WriteString(k)
			if v := l.tags[k]; v != "" {
				b.WriteByte('=')
				b.WriteString(escapeTagValue(v))
			}
		}
		b.WriteByte(' ')
	}
	if l.prefix != "" {
		b.WriteByte(':')
		b.WriteString(l.prefix)
		b.WriteByte(' ')
	}
	b.WriteString(l.command)
	for i, p := range l.params {
		b.WriteByte(' ')
		// Only the last parameter may hold spaces or be empty, and it needs the
		// colon to say so; a leading colon in the text itself needs it too, or
		// the reader would eat it.
		if i == len(l.params)-1 && (l.colon || p == "" || strings.Contains(p, " ") || strings.HasPrefix(p, ":")) {
			b.WriteByte(':')
		}
		b.WriteString(p)
	}
	return b.String()
}

// tag returns the value of a tag and whether it was present: a valueless tag —
// draft/multiline-concat is the one that matters here — is present with an
// empty value.
func (l line) tag(k string) (string, bool) {
	v, ok := l.tags[k]
	return v, ok
}

func (l line) hasTag(k string) bool {
	_, ok := l.tags[k]
	return ok
}

// param returns the i-th parameter, or "" when the line is shorter than that.
// Every server reply this package reads is indexed through here, so a truncated
// or unexpected line is a missing field rather than a panic.
func (l line) param(i int) string {
	if i < 0 || i >= len(l.params) {
		return ""
	}
	return l.params[i]
}

// trailing returns the last parameter, which for a PRIVMSG is the message text.
func (l line) trailing() string {
	if len(l.params) == 0 {
		return ""
	}
	return l.params[len(l.params)-1]
}

func parseTags(s string) map[string]string {
	tags := make(map[string]string)
	for _, t := range strings.Split(s, ";") {
		if t == "" {
			continue
		}
		k, v, _ := strings.Cut(t, "=")
		if k == "" {
			continue
		}
		tags[k] = unescapeTagValue(v)
	}
	return tags
}

// escapeTagValue applies the message-tags value escape. The five sequences are
// the whole of it: anything else a value holds travels as itself.
func escapeTagValue(v string) string {
	var b strings.Builder
	for i := range len(v) {
		switch c := v[i]; c {
		case ';':
			b.WriteString(`\:`)
		case ' ':
			b.WriteString(`\s`)
		case '\\':
			b.WriteString(`\\`)
		case '\r':
			b.WriteString(`\r`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// unescapeTagValue reverses escapeTagValue. An unrecognised escape drops the
// backslash and keeps the character, and a trailing lone backslash is dropped:
// both are what the spec requires of a reader, so a value written by a sloppy
// sender still decodes to something.
func unescapeTagValue(v string) string {
	if !strings.Contains(v, `\`) {
		return v
	}
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		if v[i] != '\\' {
			b.WriteByte(v[i])
			continue
		}
		if i == len(v)-1 {
			break
		}
		i++
		switch v[i] {
		case ':':
			b.WriteByte(';')
		case 's':
			b.WriteByte(' ')
		case '\\':
			b.WriteByte('\\')
		case 'r':
			b.WriteByte('\r')
		case 'n':
			b.WriteByte('\n')
		default:
			b.WriteByte(v[i])
		}
	}
	return b.String()
}

// nickFromPrefix reduces a nick!user@host prefix to the nick. A server prefix
// (ergo.test) has neither separator and comes back whole, which is what marks
// it as not a participant.
func nickFromPrefix(prefix string) string {
	if i := strings.IndexAny(prefix, "!@"); i >= 0 {
		return prefix[:i]
	}
	return prefix
}
