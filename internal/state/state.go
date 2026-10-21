// Package state holds a participant's per-nick state directory: one cursor per
// stream the nick follows, the lock that makes two parley processes for one
// nick a refused configuration error rather than something to reconcile, and
// any credential a transport needs parley to keep for the participant.
//
// The layout is <base>/<nick>/<streamkey>/cursor and <base>/<nick>/lock, with
// secrets at paths of the transport's choosing under <base>/<nick>. <nick> is
// the nick with its ASCII capitals folded to lower case, so one participant is
// one directory however its nick is spelled — as the bundled ergo, which runs
// casemapping ascii, and stream.NickEqual already take them to be one. The fold
// came after the layout did, and nothing migrates a directory made under a
// capitalised spelling: no other machine holds parley state, and on a
// case-insensitive volume that directory is the folded one anyway. The cursor
// is per stream because a cursor is a position in one transport and means
// nothing in another: replaying one nick's cursor against a second stream would
// skip its history or report a false gap. The lock is per nick because two
// processes speaking for one participant are a mistake whichever streams they
// are pointed at. A secret is per nick and per server rather than per stream,
// because one account reaches every channel on that server. A Codex seat's
// thread record, <base>/<nick>/<streamkey>/codex-thread, sits beside the
// cursor: it names the session that stream's pump delivers into. An OpenCode
// session's record, opencode-session, sits there in the same way.
package state

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// appDir is the component added under the XDG state home when no explicit base
// directory is given.
const appDir = "parley"

// TablesDir, BuildsDir and LinesDir are the directories under the base that hold
// the tables, the builds and the lines, beside the per-nick ones. A nick of any
// of the three names would put its lock, cursors and credentials inside them, so
// nickRoot refuses them; stream.ValidNick refuses them first
// (stream.ReservedNicks spells the same names, and a test here holds them equal).
const (
	TablesDir = "tables"
	BuildsDir = "builds"
	LinesDir  = "lines"
)

// streamKeyLen is how much of the stream spec's digest names its directory.
// Truncation is safe here: a collision only means two specs share a cursor
// file, and 64 bits of digest makes that vanishingly unlikely for the handful
// of streams one participant follows.
const streamKeyLen = 16

// Dir is one participant's state directory, bound to one stream.
type Dir struct {
	nick   string
	path   string
	stream string
}

// Open resolves and creates the state directory for nick and the stream spec.
// base is the --state-dir value; when empty, $XDG_STATE_HOME/parley (or
// ~/.local/state/parley) is used. The cursor lives in <base>/<nick>/<key>,
// where key is derived from the spec exactly as given, and the lock in
// <base>/<nick>, the nick with its ASCII capitals folded to lower case (see the package comment). This package cannot tell two spellings of one stream apart, so
// the caller must hand it a canonical spec — an absolute path for file:.
func Open(base, nick, spec string) (*Dir, error) {
	if spec == "" {
		return nil, errors.New("state: empty stream spec")
	}
	root, err := nickRoot(base, nick)
	if err != nil {
		return nil, err
	}
	d := &Dir{nick: nick, path: root, stream: StreamKey(spec)}
	if err := os.MkdirAll(d.streamPath(), 0o700); err != nil {
		return nil, fmt.Errorf("state: create %s: %w", d.streamPath(), err)
	}
	return d, nil
}

// nickRoot resolves <base>/<nick>, the directory everything this participant
// keeps lives under, with the nick's ASCII capitals folded to lower case: every
// path keyed by a nick comes through here, so one participant is one directory,
// lock and set of credentials whatever the spelling, on a case-sensitive volume
// as much as on a case-insensitive one. base is the --state-dir value; when
// empty, $XDG_STATE_HOME/parley (or ~/.local/state/parley) is used.
func nickRoot(base, nick string) (string, error) {
	if nick == "" {
		return "", errors.New("state: empty nick")
	}
	// The nick is a path element and a stream identity; keep it from escaping
	// the state directory.
	if nick == "." || nick == ".." || strings.ContainsAny(nick, `/\`) {
		return "", fmt.Errorf("state: invalid nick %q", nick)
	}
	folded := foldNick(nick)
	// The base keeps the tables, the builds and the lines beside the per-nick
	// directories; a nick of any of those names, in any case, would put its state
	// in them.
	for _, reserved := range []string{TablesDir, BuildsDir, LinesDir} {
		if folded == reserved {
			return "", fmt.Errorf("state: %q is the directory the %s live in, beside the per-nick ones, so it cannot be a nick", nick, reserved)
		}
	}
	base, err := ResolveBase(base)
	if err != nil {
		return "", err
	}
	return filepath.Join(base, folded), nil
}

// foldNick lower-cases the ASCII capitals of nick and nothing else: the rule of
// stream.NickEqual, and deliberately not strings.ToLower, which also folds
// Unicode and would join two nicks the server keeps apart.
func foldNick(nick string) string {
	b := []byte(nick)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// ResolveBase returns the absolute state directory for the --state-dir value:
// $XDG_STATE_HOME/parley (or ~/.local/state/parley) when base is empty. It is
// the directory Open puts <nick>/ under — Dir.Base is its result — and it
// touches nothing, so a caller that only has to name the directory, as a check
// of what a session would say, creates none.
func ResolveBase(base string) (string, error) {
	if base == "" {
		root := os.Getenv("XDG_STATE_HOME")
		if root == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", fmt.Errorf("state: locate home directory: %w", err)
			}
			root = filepath.Join(home, ".local", "state")
		}
		base = filepath.Join(root, appDir)
	}
	// Absolute, always: the path is handed to other processes — the reply
	// command a front spells out for its model is run from a shell whose
	// working directory is nobody's to predict — and a relative state
	// directory would name a different place there.
	abs, err := filepath.Abs(base)
	if err != nil {
		return "", fmt.Errorf("state: resolve %s: %w", base, err)
	}
	return abs, nil
}

// Secret is one file under a participant's state directory holding a
// credential parley keeps for them — a transport's account password, which the
// participant never sees and never types. Nothing about it is a stream
// position, so it lives beside the per-stream directories rather than in one.
type Secret struct{ path string }

// OpenSecret names a secret at <base>/<nick>/<parts...>. It touches nothing:
// the directory is created by Claim, so reading on a machine that has never
// claimed one leaves no trace.
func OpenSecret(base, nick string, parts ...string) (*Secret, error) {
	root, err := nickRoot(base, nick)
	if err != nil {
		return nil, err
	}
	if len(parts) == 0 {
		return nil, errors.New("state: secret with no name")
	}
	for _, p := range parts {
		if p == "" || p == "." || p == ".." || strings.ContainsAny(p, `/\`) {
			return nil, fmt.Errorf("state: invalid secret path element %q", p)
		}
	}
	return &Secret{path: filepath.Join(append([]string{root}, parts...)...)}, nil
}

// Path is the file the secret is kept in.
func (s *Secret) Path() string { return s.path }

// Load returns the stored secret, or the empty string when none has been
// stored. A secret is one line: the trailing newline Store writes is removed
// and nothing else is.
func (s *Secret) Load() (string, error) {
	b, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("state: read secret %s: %w", s.path, err)
	}
	return strings.TrimSuffix(string(b), "\n"), nil
}

// Claim stores the secret if there is none, and reports whether it did. It is
// the only way to write one: a credential is claimed once and read ever after,
// so two processes starting together for one participant cannot each write
// their own and leave one of them holding a password the other's account does
// not know.
//
// The write is complete before it is visible. A secret is built in a temp file
// and linked into place, because link fails rather than replaces: the loser of
// the race sees the winner's whole credential, never half of one, and never
// overwrites it.
func (s *Secret) Claim(value string) (bool, error) {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, fmt.Errorf("state: create %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, "secret-*")
	if err != nil {
		return false, fmt.Errorf("state: create temp secret in %s: %w", dir, err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return false, fmt.Errorf("state: set permissions on %s: %w", tmp, err)
	}
	if _, err := f.WriteString(value + "\n"); err != nil {
		f.Close()
		return false, fmt.Errorf("state: write temp secret %s: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return false, fmt.Errorf("state: sync temp secret %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return false, fmt.Errorf("state: close temp secret %s: %w", tmp, err)
	}
	if err := os.Link(tmp, s.path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return false, nil
		}
		return false, fmt.Errorf("state: store secret %s: %w", s.path, err)
	}
	return true, nil
}

// Discard removes the secret, and says nothing about one that was not there. It
// is for a credential that turned out to be worthless — a password for an
// account the server refused to create — which must not be left behind to be
// read back as the real one.
func (s *Secret) Discard() error {
	if err := os.Remove(s.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("state: discard secret %s: %w", s.path, err)
	}
	return nil
}

// StreamDir names <base>/<nick>/<streamkey>, the directory that holds nick's
// cursor, and a Codex seat's thread record, for the stream spec. It touches
// nothing. As for Open, the spec must be the canonical one.
func StreamDir(base, nick, spec string) (string, error) {
	if spec == "" {
		return "", errors.New("state: empty stream spec")
	}
	root, err := nickRoot(base, nick)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, StreamKey(spec)), nil
}

// RemoveStreamDir removes nick's state for the stream spec, the directory
// StreamDir names with the cursor and the thread record in it, and says nothing
// about one that is not there. A link in its place is removed as the link, never
// through it. The nick's lock file and the credentials beside it are not in
// that directory and are not touched.
func RemoveStreamDir(base, nick, spec string) error {
	dir, err := StreamDir(base, nick, spec)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("state: remove %s: %w", dir, err)
	}
	return nil
}

// StreamKey is the directory name a stream spec gets under the nick.
func StreamKey(spec string) string {
	sum := sha256.Sum256([]byte(spec))
	return hex.EncodeToString(sum[:])[:streamKeyLen]
}

// Path is the participant's state directory, the one holding the lock and a
// directory per stream.
func (d *Dir) Path() string { return d.path }

// Base is the resolved --state-dir this participant's directory sits in,
// absolute. It is what another parley process has to be given to find the same
// state: a front that tells its model how to reply has to name it, or the reply
// would run against the default directory and find none of this.
func (d *Dir) Base() string { return filepath.Dir(d.path) }

func (d *Dir) streamPath() string { return filepath.Join(d.path, d.stream) }

// CursorPath is the file holding the last delivered cursor for this stream.
func (d *Dir) CursorPath() string { return filepath.Join(d.streamPath(), "cursor") }

// LockPath is the file carrying the single-process advisory lock. It is shared
// by every stream this nick follows.
func (d *Dir) LockPath() string { return filepath.Join(d.path, "lock") }

// LoadCursor returns the stored cursor, or the empty cursor when none has been
// stored yet, which means the start of the stream's retained history.
func (d *Dir) LoadCursor() (string, error) {
	b, err := os.ReadFile(d.CursorPath())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("state: read cursor %s: %w", d.CursorPath(), err)
	}
	// Exactly the newline SaveCursor writes: a cursor is opaque to this package,
	// so trimming anything else could hand the transport a position it never
	// issued.
	return strings.TrimSuffix(string(b), "\n"), nil
}

// SaveCursor stores the cursor. The write goes through a temp file and a
// rename: a crash partway through must never leave a truncated cursor, which
// would silently replay or skip history.
func (d *Dir) SaveCursor(cursor string) error {
	f, err := os.CreateTemp(d.streamPath(), "cursor-*")
	if err != nil {
		return fmt.Errorf("state: create temp cursor in %s: %w", d.streamPath(), err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.WriteString(cursor + "\n"); err != nil {
		f.Close()
		return fmt.Errorf("state: write temp cursor %s: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("state: sync temp cursor %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("state: close temp cursor %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, d.CursorPath()); err != nil {
		return fmt.Errorf("state: replace cursor %s: %w", d.CursorPath(), err)
	}
	return nil
}

// LockedError reports that another process already holds this nick's lock. Two
// parley processes for one participant are a configuration error, not something
// to reconcile.
type LockedError struct {
	Nick string
	Path string
	Err  error
}

func (e *LockedError) Error() string {
	return fmt.Sprintf("another parley process is already running for nick %q (lock held on %s)", e.Nick, e.Path)
}

func (e *LockedError) Unwrap() error { return e.Err }

// Lock is a held advisory lock, released by Release or by process exit.
type Lock struct {
	f *os.File
}

// Lock takes the nick's advisory lock, failing immediately with a *LockedError
// if another process holds it. The lock is an OS-level flock so that a crashed
// process never leaves a stale lock behind.
func (d *Dir) Lock() (*Lock, error) {
	return lockFile(d.nick, d.LockPath())
}

// LockNick takes nick's lock under base without a stream: the lock file every
// pump and channel of that nick holds, made if the nick has never run. It is for
// the process that has to know no seat is running before it takes one down, and
// has to keep it so until it is done: `parley table end` takes every seat's, and
// holds them through its teardown. It fails as Lock does, with a *LockedError.
func LockNick(base, nick string) (*Lock, error) {
	path, err := NickLockPath(base, nick)
	if err != nil {
		return nil, err
	}
	root := filepath.Dir(path)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("state: create %s: %w", root, err)
	}
	return lockFile(nick, path)
}

// NickLockPath names the file LockNick locks, <base>/<nick>/lock, and touches
// nothing. A caller that makes the file by taking the lock can tell beforehand
// whether it will be the one to make it.
func NickLockPath(base, nick string) (string, error) {
	root, err := nickRoot(base, nick)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "lock"), nil
}

func lockFile(nick, path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("state: open lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, &LockedError{Nick: nick, Path: path, Err: err}
		}
		return nil, fmt.Errorf("state: lock %s: %w", path, err)
	}
	return &Lock{f: f}, nil
}

// Release drops the lock.
func (l *Lock) Release() error {
	// Closing the file would release the lock on its own; unlocking explicitly
	// surfaces the error instead of discarding it.
	uerr := syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	cerr := l.f.Close()
	if uerr != nil {
		return fmt.Errorf("state: unlock %s: %w", l.f.Name(), uerr)
	}
	if cerr != nil {
		return fmt.Errorf("state: close lock %s: %w", l.f.Name(), cerr)
	}
	return nil
}

// OpenRegular opens path for reading only if it is a regular file, and finds
// that out without blocking or following a link: O_NONBLOCK keeps a FIFO from
// waiting for a writer, O_NOFOLLOW refuses a link in the last component, and
// the opened descriptor itself is checked. A missing file is still an error
// that errors.Is(err, fs.ErrNotExist) recognises.
func OpenRegular(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, fmt.Errorf("%s is a link, not a regular file", path)
		}
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	return f, nil
}
