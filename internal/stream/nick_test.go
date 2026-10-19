package stream

import (
	"strings"
	"testing"
	"time"
)

func TestValidNick(t *testing.T) {
	cases := []struct {
		nick string
		ok   bool
	}{
		{"codex", true},
		{"claude", true},
		{"a", true},
		{"0", true},
		{"agent_1", true},
		{"a-b", true},
		{"élan", true},
		{"日本", true},
		{strings.Repeat("n", MaxNick), true},

		{"", false},
		// A '.' is out of the grammar for every adapter, not only for irc:
		// an IRC server answers a dotted nick with 432, so accepting one here
		// would make a file: nick a peer could not carry over IRC.
		{"a.b", false},
		{"codex-a.m2", false},
		{".", false},
		{"..", false},
		{"...", false},
		{"a/b", false},
		{`a\b`, false},
		{"two words", false},
		{"tab\there", false},
		{"line\nbreak", false},
		{"a+b", false},
		{"a@b", false},
		{"nick😀", false},
		{"#channel", false},
		{strings.Repeat("n", MaxNick+1), false},
		// The three directories the state base keeps beside the per-nick ones, in
		// any case; a name that merely contains one is a nick like any other.
		{"tables", false},
		{"Tables", false},
		{"TABLES", false},
		{"builds", false},
		{"bUiLdS", false},
		{"lines", false},
		{"Lines", false},
		{"LINES", false},
		{"table", true},
		{"tables2", true},
		{"my-builds", true},
		{"line", true},
		{"lines2", true},
	}
	for _, c := range cases {
		err := ValidNick(c.nick)
		if c.ok && err != nil {
			t.Errorf("ValidNick(%q) = %v, want it accepted", c.nick, err)
		}
		if !c.ok && err == nil {
			t.Errorf("ValidNick(%q) accepted it, want a rejection", c.nick)
		}
	}
}

// TestValidNickRendersUnchanged is the contract that ties the two ends of the
// grammar together: whatever the CLI admits reaches the peer header byte for
// byte, so two distinct nicks can never render as the same name.
func TestValidNickRendersUnchanged(t *testing.T) {
	nicks := []string{
		"codex",
		"claude-2",
		"agent_1",
		"élan",
		"日本",
		strings.Repeat("n", MaxNick),
	}
	for _, nick := range nicks {
		if err := ValidNick(nick); err != nil {
			t.Fatalf("test setup: ValidNick(%q) = %v", nick, err)
		}
		if got := sanitizeAuthor(nick); got != nick {
			t.Errorf("sanitizeAuthor(%q) = %q, want it unchanged", nick, got)
		}
		d := &Delivery{Messages: []Message{{ID: "0", Author: nick, TS: time.Unix(0, 0).UTC(), Body: "hi"}}}
		if want := "[parley] from=" + nick + " id=0"; !strings.HasPrefix(d.Render(), want) {
			t.Errorf("render starts %q, want the header to open %q", d.Render(), want)
		}
	}
}

func TestNickEqual(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"colin", "colin", true},
		{"colin", "Colin", true},
		{"COLIN", "colin", true},
		{"agent_1", "AGENT_1", true},
		{"a-b", "A-B", true},
		{"colin", "colin2", false},
		{"colin", "colon", false},
		{"", "", true},
		{"", "a", false},
		// ASCII only: ergo's casemapping does not fold these, so neither does this.
		{"élan", "Élan", false},
		{"k", "\u212a", false},
		{"ss", "ß", false},
	} {
		if got := NickEqual(tc.a, tc.b); got != tc.want {
			t.Errorf("NickEqual(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
		if got := NickEqual(tc.b, tc.a); got != tc.want {
			t.Errorf("NickEqual(%q, %q) = %v, want %v", tc.b, tc.a, got, tc.want)
		}
	}
}
