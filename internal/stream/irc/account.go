package irc

import (
	"fmt"
	"strings"

	"github.com/colinsurprenant/parley/internal/state"
)

// Credentials is where the adapter keeps the account password it holds on a
// participant's behalf, one per server. There is no user-visible secret: parley
// generates the password, registers the account with it and stores it here, and
// every later connection for that nick reads it back. A participant that loses
// this store loses the account, which is why Location exists — an error about a
// credential has to be able to say which file it means.
type Credentials interface {
	// Load returns the stored password for the server at addr, or "" when
	// parley has never registered an account there for this participant.
	Load(addr string) (string, error)
	// Claim stores password for addr if nothing is stored yet, and reports
	// whether it did. It is how two parley processes starting at the same
	// moment for one participant end up with one account between them rather
	// than one account and one stranded password: the loser reads the winner's.
	Claim(addr, password string) (bool, error)
	// Discard removes the stored password for addr. It is for a password the
	// server refused to make an account for, which must not be read back later
	// as if it were the account's.
	Discard(addr string) error
	// Location names where the password for addr is kept.
	Location(addr string) string
}

// StateCredentials keeps each password under the participant's own state
// directory, at <state>/<nick>/irc/<host>_<port>/password (<nick> with its ASCII capitals folded to lower case,
// as the state package folds it), mode 0600. It sits
// beside the lock and the per-stream cursors because it is the same kind of
// thing: state this participant's processes share and no one else's. The key is
// the server rather than the stream, since one account reaches every channel on
// it.
type StateCredentials struct {
	base string
	nick string
}

// NewStateCredentials binds a credential store to a --state-dir (empty for the
// default) and a nick.
func NewStateCredentials(base, nick string) *StateCredentials {
	return &StateCredentials{base: base, nick: nick}
}

func (c *StateCredentials) secret(addr string) (*state.Secret, error) {
	s, err := state.OpenSecret(c.base, c.nick, "irc", addrKey(addr), "password")
	if err != nil {
		return nil, fmt.Errorf("irc: account password for %s: %w", addr, err)
	}
	return s, nil
}

// Load returns the stored password for addr, or "" when there is none.
func (c *StateCredentials) Load(addr string) (string, error) {
	s, err := c.secret(addr)
	if err != nil {
		return "", err
	}
	pw, err := s.Load()
	if err != nil {
		return "", fmt.Errorf("irc: %w", err)
	}
	return pw, nil
}

// Claim stores the password for addr if there is none, and reports whether it
// did.
func (c *StateCredentials) Claim(addr, password string) (bool, error) {
	s, err := c.secret(addr)
	if err != nil {
		return false, err
	}
	ok, err := s.Claim(password)
	if err != nil {
		return false, fmt.Errorf("irc: %w", err)
	}
	return ok, nil
}

// Discard removes the stored password for addr.
func (c *StateCredentials) Discard(addr string) error {
	s, err := c.secret(addr)
	if err != nil {
		return err
	}
	if err := s.Discard(); err != nil {
		return fmt.Errorf("irc: %w", err)
	}
	return nil
}

// Location names the file holding the password for addr.
func (c *StateCredentials) Location(addr string) string {
	s, err := c.secret(addr)
	if err != nil {
		return "the state directory"
	}
	return s.Path()
}

// addrKey turns host:port into one path element. The colon is legal in a file
// name on every platform parley runs on, but reads as a drive or a separator to
// enough tools that it is not worth keeping; an IPv6 host keeps its brackets,
// which are only ever literal here.
func addrKey(addr string) string {
	return strings.ReplaceAll(addr, ":", "_")
}
