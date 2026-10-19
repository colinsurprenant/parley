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

// ThreadRecord is the Codex thread a seat's pump delivers into, kept beside the
// seat's cursor for the stream: <base>/<nick>/<streamkey>/codex-thread, one
// line holding the thread's UUID, mode 0600. `parley table new`, `parley build
// new` and `parley line add` write it when they start the seat's, peer's or
// session's Codex session, and `table end`, `build end` and `line drop` remove
// it; `parley codex --thread auto` reads it to find the thread, and `parley send`
// reads it to tell a session that is no longer that thread. It is a pointer, not stream content: nothing a peer said is ever kept
// here.
type ThreadRecord struct{ path string }

// OpenThreadRecord names the record for nick on the stream spec, under the base
// --state-dir resolves to. It touches nothing: a reader on a machine that has
// never written one leaves no directory behind, and Write creates it. As for
// Open, the spec must be the canonical one.
func OpenThreadRecord(base, nick, spec string) (*ThreadRecord, error) {
	if spec == "" {
		return nil, errors.New("state: empty stream spec")
	}
	root, err := nickRoot(base, nick)
	if err != nil {
		return nil, err
	}
	return &ThreadRecord{path: filepath.Join(root, StreamKey(spec), "codex-thread")}, nil
}

// Path is the file the record is kept in.
func (r *ThreadRecord) Path() string { return r.path }

// ValidThreadID reports whether id has the shape of the UUID Codex gives a
// thread: 36 characters, hex digits in groups of 8-4-4-4-12.
func ValidThreadID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i := 0; i < len(id); i++ {
		switch c := id[i]; {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if c != '-' {
				return false
			}
		case '0' <= c && c <= '9', 'a' <= c && c <= 'f', 'A' <= c && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// Write stores the thread id, replacing any record there. The write goes through
// a temp file and a rename, as the cursor's does: a reader must see the old
// record or the whole new one, never half of either. An id that is not a UUID is
// refused, since Read would refuse the record it made.
func (r *ThreadRecord) Write(id string) error {
	if !ValidThreadID(id) {
		return fmt.Errorf("state: thread id %q is not a UUID", id)
	}
	dir := filepath.Dir(r.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("state: create %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, "codex-thread-*")
	if err != nil {
		return fmt.Errorf("state: create temp thread record in %s: %w", dir, err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return fmt.Errorf("state: set permissions on %s: %w", tmp, err)
	}
	if _, err := f.WriteString(id + "\n"); err != nil {
		f.Close()
		return fmt.Errorf("state: write temp thread record %s: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("state: sync temp thread record %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("state: close temp thread record %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, r.path); err != nil {
		return fmt.Errorf("state: replace thread record %s: %w", r.path, err)
	}
	return nil
}

// threadRecordMax is the length of a well-formed record: a 36-character UUID
// and its newline.
const threadRecordMax = 37

// Read returns the recorded thread id, or the empty string when there is no
// record. A record that is there but is not exactly one UUID line, newline included, is an error
// naming the file: acting on a thread id that was never written would deliver a
// peer's text into the wrong session.
func (r *ThreadRecord) Read() (string, error) {
	f, err := OpenRegular(r.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("state: read thread record %s: %w", r.path, err)
	}
	defer f.Close()
	// One UUID line is all a record holds, so a longer file is malformed and is
	// never read whole.
	b, err := io.ReadAll(io.LimitReader(f, threadRecordMax+1))
	if err != nil {
		return "", fmt.Errorf("state: read thread record %s: %w", r.path, err)
	}
	// Write ends the line, so a record without its newline is not one Write made.
	id, ok := strings.CutSuffix(string(b), "\n")
	if !ok || len(b) > threadRecordMax || !ValidThreadID(id) {
		return "", fmt.Errorf("state: thread record %s is malformed: want one UUID line ending in a newline, found %s", r.path, excerpt(b))
	}
	return id, nil
}

// Remove deletes the record, and says nothing about one that was not there. A
// record that is a link is removed as the link, never through it.
func (r *ThreadRecord) Remove() error {
	if err := os.Remove(r.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("state: remove thread record %s: %w", r.path, err)
	}
	return nil
}

// excerpt quotes the start of a file's content for an error message, so a
// record that is a stray large file does not become a large message.
func excerpt(b []byte) string {
	const limit = 48
	if len(b) == 0 {
		return "an empty file"
	}
	if len(b) > limit {
		return fmt.Sprintf("%q… (%d bytes)", b[:limit], len(b))
	}
	return fmt.Sprintf("%q", b)
}
