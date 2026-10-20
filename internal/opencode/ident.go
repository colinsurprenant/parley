package opencode

import (
	"crypto/rand"
	"fmt"
	"io"
	"sync"
	"time"
)

// Identifiers as OpenCode 2 makes them (packages/schema/src/identifier.ts at
// v2.0.32): twelve hex digits holding the low 48 bits of the millisecond clock
// shifted left twelve with a per-millisecond counter added, and fourteen random
// characters of 0-9A-Za-z. The session kind is descending: the time part is
// inverted, so the newest session sorts first. A message id is ascending. Both
// carry a prefix, ses_ and msg_, which is what OpenCode's schema checks of an
// id, and the server takes the id a client gives it when it creates the
// session (opencode run --session) or admits the prompt (POST .../prompt with
// an id), which is what lets this front know an id before the server does.
const (
	idRandom = 14
	idChars  = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
)

// idSource makes ids. The clock and the randomness are fields for the test; the
// last millisecond and the counter are shared by every id it makes, as in
// OpenCode's module, so two ids made in one millisecond differ in the time part
// as well as in the random one.
type idSource struct {
	mu      sync.Mutex
	now     func() time.Time
	rand    io.Reader
	last    int64
	counter uint64
}

var ids = &idSource{now: time.Now, rand: rand.Reader}

// NewSessionID returns a session id of the shape OpenCode makes: ses_ and 26
// letters or digits, newest first in OpenCode's ordering.
func NewSessionID() (string, error) { return ids.create("ses_", true) }

// newMessageID returns a message id: msg_ and 26 letters or digits, ascending.
func newMessageID() (string, error) { return ids.create("msg_", false) }

func (s *idSource) create(prefix string, descending bool) (string, error) {
	random := make([]byte, idRandom)
	if _, err := io.ReadFull(s.rand, random); err != nil {
		return "", fmt.Errorf("opencode: make an id: %w", err)
	}
	s.mu.Lock()
	ms := s.now().UnixMilli()
	if ms != s.last {
		s.last, s.counter = ms, 0
	}
	s.counter++
	current := uint64(ms)<<12 + s.counter
	s.mu.Unlock()
	if descending {
		current = ^current
	}
	b := make([]byte, 0, len(prefix)+12+idRandom)
	b = append(b, prefix...)
	for i := 0; i < 6; i++ {
		b = append(b, fmt.Sprintf("%02x", byte(current>>(40-8*uint(i))))...)
	}
	for _, r := range random {
		b = append(b, idChars[int(r)%len(idChars)])
	}
	return string(b), nil
}
