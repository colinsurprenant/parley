package state

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// SessionRecord is the OpenCode session a seat's pump delivers into, kept beside
// the seat's cursor for the stream: <base>/<nick>/<streamkey>/opencode-session,
// one line holding the session's id, mode 0600. It is ThreadRecord's counterpart
// for the OpenCode front, and it is a sibling and not a generalization of it so
// that codex-thread stays what it was byte for byte: the same place, the same
// atomic write, the same bounded read, a different id shape and its own
// wording. `parley opencode --session auto` reads it to find the session. It is
// a pointer, not stream content: nothing a peer said is ever kept here.
type SessionRecord struct{ path string }

// OpenSessionRecord names the record for nick on the stream spec, under the base
// --state-dir resolves to. It touches nothing: a reader on a machine that has
// never written one leaves no directory behind, and Write creates it. As for
// Open, the spec must be the canonical one.
func OpenSessionRecord(base, nick, spec string) (*SessionRecord, error) {
	if spec == "" {
		return nil, errors.New("state: empty stream spec")
	}
	root, err := nickRoot(base, nick)
	if err != nil {
		return nil, err
	}
	return &SessionRecord{path: filepath.Join(root, StreamKey(spec), "opencode-session")}, nil
}

// Path is the file the record is kept in.
func (r *SessionRecord) Path() string { return r.path }

// sessionIDLen is the length of an OpenCode session id: "ses_" and 26 more
// characters, 12 hex digits of time and 14 base62 ones, as OpenCode 1.18 makes
// them. The id travels in a URL path and in a log line, so the check is on the
// shape alone and not on anything the server could be asked.
const sessionIDLen = 30

// ValidSessionID reports whether id has the shape of an id OpenCode gives a
// session: "ses_" followed by 26 ASCII letters or digits.
func ValidSessionID(id string) bool {
	if len(id) != sessionIDLen || !strings.HasPrefix(id, "ses_") {
		return false
	}
	for i := len("ses_"); i < len(id); i++ {
		switch c := id[i]; {
		case '0' <= c && c <= '9', 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z':
		default:
			return false
		}
	}
	return true
}

// Write stores the session id, replacing any record there. The write goes
// through a temp file and a rename, as the cursor's does: a reader must see the
// old record or the whole new one, never half of either. An id that is not an
// OpenCode session id is refused, since Read would refuse the record it made.
func (r *SessionRecord) Write(id string) error {
	if !ValidSessionID(id) {
		return fmt.Errorf("state: session id %q is not an OpenCode session id (ses_ and 26 letters or digits)", id)
	}
	dir := filepath.Dir(r.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("state: create %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, "opencode-session-*")
	if err != nil {
		return fmt.Errorf("state: create temp session record in %s: %w", dir, err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return fmt.Errorf("state: set permissions on %s: %w", tmp, err)
	}
	if _, err := f.WriteString(id + "\n"); err != nil {
		f.Close()
		return fmt.Errorf("state: write temp session record %s: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("state: sync temp session record %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("state: close temp session record %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, r.path); err != nil {
		return fmt.Errorf("state: replace session record %s: %w", r.path, err)
	}
	return nil
}

// sessionRecordMax is the length of a well-formed record: a 30-character
// session id and its newline.
const sessionRecordMax = sessionIDLen + 1

// Read returns the recorded session id, or the empty string when there is no
// record. A record that is there but is not exactly one session id line,
// newline included, is an error naming the file: acting on an id that was never
// written would deliver a peer's text into the wrong session.
func (r *SessionRecord) Read() (string, error) {
	f, err := OpenRegular(r.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("state: read session record %s: %w", r.path, err)
	}
	defer f.Close()
	// One id line is all a record holds, so a longer file is malformed and is
	// never read whole.
	b, err := io.ReadAll(io.LimitReader(f, sessionRecordMax+1))
	if err != nil {
		return "", fmt.Errorf("state: read session record %s: %w", r.path, err)
	}
	// Write ends the line, so a record without its newline is not one Write made.
	id, ok := strings.CutSuffix(string(b), "\n")
	if !ok || len(b) > sessionRecordMax || !ValidSessionID(id) {
		return "", fmt.Errorf("state: session record %s is malformed: want one OpenCode session id line ending in a newline, found %s", r.path, excerpt(b))
	}
	return id, nil
}

// Remove deletes the record, and says nothing about one that was not there. A
// record that is a link is removed as the link, never through it.
func (r *SessionRecord) Remove() error {
	if err := os.Remove(r.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("state: remove session record %s: %w", r.path, err)
	}
	return nil
}
