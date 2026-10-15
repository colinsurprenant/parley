package stream

import (
	"errors"
	"fmt"
	"unicode"
)

// MaxNick bounds a nick, in bytes: it is a path element in the state tree and a
// field in the peer header, not a place to put a paragraph.
const MaxNick = 64

// ReservedNicks are the names no nick may take, in any case: the directories the
// state base keeps the tables and the builds in, beside the per-nick ones. This
// package cannot import internal/state, which imports it in its tests, so the
// names are spelled here and state's tests hold them equal to state.TablesDir
// and state.BuildsDir. A nick of either name would put its lock, cursor and
// credentials inside that directory.
var ReservedNicks = [...]string{"tables", "builds"}

// NickRune reports whether r may appear in a nick: letters, digits, '_' and
// '-'. Nothing else — no whitespace or control character to break the peer
// header, no path separator to escape the state directory, no mark a reader
// cannot tell from another, and no '.', which ergo refuses outright with 432
// and which the workstream names a nick is drawn from never carry.
//
// This is the one grammar: the CLI admits exactly the nicks it accepts, and
// sanitizeAuthor preserves exactly those runes, so every nick parley accepts
// reaches the header unchanged.
func NickRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-'
}

// ValidNick reports whether nick is a name parley can carry, and what is wrong
// with it if not. The error describes the fault only; the caller names the nick,
// since it knows where it came from.
//
// A nick is one to MaxNick bytes of NickRune runes — letters, digits, '_' and
// '-' — and nothing else, and it is not "tables" or "builds" in any case, the
// two directories the state base keeps beside the per-nick ones. It is one
// grammar for every adapter rather than a widest-common one with exceptions: an
// IRC server refuses a nick holding a '.' (ergo answers 432 Erroneous nickname),
// so a nick a file: stream carried would be a nick an irc: stream could not, and
// a participant's name would stop being portable between the two.
func ValidNick(nick string) error {
	if nick == "" {
		return errors.New("is empty")
	}
	if len(nick) > MaxNick {
		return fmt.Errorf("is %d bytes, the maximum is %d", len(nick), MaxNick)
	}
	// A dot-name is a directory that is not one: it would name the state tree
	// itself, or its parent. These two return here so that they get an error of
	// their own; every other dotted name falls through to the rune loop, which
	// refuses it for the '.' it contains.
	if nick == "." || nick == ".." {
		return errors.New("is not a name")
	}
	for _, r := range nick {
		if !NickRune(r) {
			return fmt.Errorf("contains %q: a nick holds only letters, digits, '_' and '-'", r)
		}
	}
	// state.nickRoot refuses these too; this is the refusal every verb meets
	// first, as a configuration error, before any state is touched or any
	// stream is opened.
	for _, name := range ReservedNicks {
		if NickEqual(nick, name) {
			return fmt.Errorf("is the directory the %s live in, beside the per-nick ones, so it cannot be a nick", name)
		}
	}
	return nil
}

// NickEqual reports whether a and b name the same participant: equal once the
// ASCII letters are folded, and byte for byte otherwise. That is the rule of the
// bundled ergo, which runs casemapping ascii — "Colin" and "colin" are one nick
// to the server, so a chair posting as one is the chair a seat was told is the
// other — and it is deliberately not strings.EqualFold, which also folds Unicode
// and would call two nicks the same that the server keeps apart. Every
// comparison of an author against a nick anywhere in parley goes through here.
func NickEqual(a, b string) bool {
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
