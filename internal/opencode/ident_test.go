package opencode

import (
	"bytes"
	"regexp"
	"testing"
	"time"
)

var (
	sessionIDRE = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)
	messageIDRE = regexp.MustCompile(`^msg_[0-9a-f]{12}[0-9A-Za-z]{14}$`)
)

func TestIDsHaveTheShapeOpenCodeMakes(t *testing.T) {
	for i := 0; i < 50; i++ {
		s, err := NewSessionID()
		if err != nil || !sessionIDRE.MatchString(s) {
			t.Fatalf("NewSessionID = %q, %v", s, err)
		}
		m, err := newMessageID()
		if err != nil || !messageIDRE.MatchString(m) {
			t.Fatalf("newMessageID = %q, %v", m, err)
		}
	}
}

// TestIDsFollowTheAlgorithmOfIdentifierTS pins the time part to what
// packages/schema/src/identifier.ts computes for a clock and a counter: the low
// 48 bits of ms<<12 + counter, inverted for a session. The expected values are
// worked out from that source, and the first ones agree with ids a 2.0.32 server
// made (ses_...ffe is a counter of 1 inverted, msg_...001 a counter of 1).
func TestIDsFollowTheAlgorithmOfIdentifierTS(t *testing.T) {
	const ms = 1792467151423
	src := &idSource{
		now:  func() time.Time { return time.UnixMilli(ms) },
		rand: bytes.NewReader(bytes.Repeat([]byte{0, 61, 62, 123}, 20)),
	}
	got := func(prefix string, desc bool) string {
		t.Helper()
		id, err := src.create(prefix, desc)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	// want is the 12 hex digits of the low 48 bits of ms<<12 + counter, inverted
	// first for the descending kind.
	want := func(counter uint64, desc bool) string {
		cur := uint64(ms)<<12 + counter
		if desc {
			cur = ^cur
		}
		const hex = "0123456789abcdef"
		b := make([]byte, 0, 12)
		for shift := 44; shift >= 0; shift -= 4 {
			b = append(b, hex[(cur>>uint(shift))&0xf])
		}
		return string(b)
	}
	// The random part is chars[byte % 62]: 0, 61, 62%62=0, 123%62=61.
	suffix := "0z0z0z0z0z0z0z"
	if s := got("ses_", true); s != "ses_"+want(1, true)+suffix {
		t.Errorf("first session id = %q, want ses_%s%s", s, want(1, true), suffix)
	}
	if s := got("ses_", true); s != "ses_"+want(2, true)+suffix {
		t.Errorf("second session id in the same millisecond = %q, want counter 2", s)
	}
	// The counter is shared by both kinds, as the module's is.
	if m := got("msg_", false); m != "msg_"+want(3, false)+suffix {
		t.Errorf("message id = %q, want ms<<12 + 3", m)
	}
	// Spot-check the arithmetic against a hand-worked value: counter 1, inverted,
	// ends in ffe; counter 1, not inverted, ends in 001.
	if w := want(1, true); w[9:] != "ffe" {
		t.Errorf("descending counter 1 should end in ffe: %s", w)
	}
	if w := want(1, false); w[9:] != "001" {
		t.Errorf("ascending counter 1 should end in 001: %s", w)
	}
}

func TestSessionIDsDescendAndMessageIDsAscend(t *testing.T) {
	now := time.UnixMilli(1792467151423)
	src := &idSource{now: func() time.Time { return now }, rand: bytes.NewReader(bytes.Repeat([]byte{7}, 400))}
	var sessions, messages []string
	for i := 0; i < 5; i++ {
		s, _ := src.create("ses_", true)
		m, _ := src.create("msg_", false)
		sessions, messages = append(sessions, s), append(messages, m)
		if i%2 == 1 {
			now = now.Add(time.Millisecond)
		}
	}
	for i := 1; i < len(sessions); i++ {
		if sessions[i][4:16] >= sessions[i-1][4:16] {
			t.Errorf("session ids do not descend: %s then %s", sessions[i-1], sessions[i])
		}
		if messages[i][4:16] <= messages[i-1][4:16] {
			t.Errorf("message ids do not ascend: %s then %s", messages[i-1], messages[i])
		}
	}
}

func TestIDsAreDistinct(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 2000; i++ {
		id, err := newMessageID()
		if err != nil {
			t.Fatal(err)
		}
		if seen[id] {
			t.Fatalf("id %s was made twice", id)
		}
		seen[id] = true
	}
}

func TestAFailingRandomSourceIsAnError(t *testing.T) {
	src := &idSource{now: time.Now, rand: bytes.NewReader(nil)}
	if id, err := src.create("ses_", true); err == nil {
		t.Fatalf("an id %q was made without randomness", id)
	}
}
