package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// idsDirName is the directory of the id locks, beside the per-nick directories of
// the state base. A nick cannot be called this: stream.ValidNick refuses a '.',
// so no participant's directory is ever in this namespace.
const idsDirName = ".ids"

// IDLockedError reports that another parley command already holds the lock of this
// table or build id.
type IDLockedError struct {
	ID   string
	Path string
	Err  error
}

func (e *IDLockedError) Error() string {
	return fmt.Sprintf("another parley command holds the lock of id %q (lock held on %s)", e.ID, e.Path)
}

func (e *IDLockedError) Unwrap() error { return e.Err }

// IDLockPath names the file LockID locks, <base>/.ids/<id>.lock, and touches
// nothing. It is under the base and not under the table's or the build's own
// directory because `table end` and `build end` remove that directory, and a lock
// is held on the file: once its path is unlinked another process could create a
// new file there and take its own lock while the old one is still held.
func IDLockPath(base, id string) (string, error) {
	if id == "" || id == "." || id == ".." || strings.HasPrefix(id, ".") || strings.ContainsAny(id, "/\\\x00") {
		return "", fmt.Errorf("state: invalid id %q", id)
	}
	base, err := ResolveBase(base)
	if err != nil {
		return "", err
	}
	return filepath.Join(base, idsDirName, id+".lock"), nil
}

// LockID takes the lock of a table or build id under base without blocking, with
// the same primitive as a nick's lock: a flock on a file that is made if it is
// not there. It is held by every command that makes or unmakes the id's table or
// build (`table new`, `table end`, `build new`, `build end`) for the whole run,
// so that no two of them act on one id at once, whichever kind they are. It fails
// with an *IDLockedError when another process holds it. The lock file stays, as a
// nick's does, and is released with Release or by process exit.
func LockID(base, id string) (*Lock, error) {
	path, err := IDLockPath(base, id)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("state: create %s: %w", dir, err)
	}
	l, err := lockFile("", path)
	if err != nil {
		var locked *LockedError
		if errors.As(err, &locked) {
			return nil, &IDLockedError{ID: id, Path: path, Err: locked.Err}
		}
		return nil, err
	}
	return l, nil
}
