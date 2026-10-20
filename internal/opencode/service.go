package opencode

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// OpenCode 2 runs one background service per user and records how to reach it in
// a registration file, ${XDG_STATE_HOME:-$HOME/.local/state}/opencode/service.json:
// {"id", "version", "url", "pid", "password"}, written by the service itself with
// mode 0600 (packages/client/src/effect/service.ts, 2.0.32: "the complete
// discovery contract"). Reading it is how this front finds that service without
// a port to pass, and it is the one place it takes a credential from.
//
// What is trusted there is what the user's own `opencode` CLI trusts, and no
// more: a file in the user's own state directory that they own, that nobody else
// can read or write, naming a loopback address. Anything else is refused and the
// file is not read further: a file another user could have planted would turn
// "find the service" into "send the delivery, and the service's password, to an
// address of someone else's choosing". The password is a field and nothing else:
// it is not logged, not printed, and not put in an error.

// maxServiceFileBytes bounds the registration file, which is a few hundred bytes.
const maxServiceFileBytes = 16 << 10

// ErrNoService is the registration file not being there: no service has been
// started, or it was stopped. Test for it with errors.Is.
var ErrNoService = errors.New("opencode: no OpenCode 2 service is registered")

// Service is what the registration file says of the service. It never prints its
// password, whatever verb formats it.
type Service struct {
	ID      string
	Version string
	URL     string
	PID     int
	// Password is the service's password; the user name is ServiceUser.
	Password string `json:"-"`
}

func (s Service) String() string {
	return fmt.Sprintf("opencode.Service{ID:%q, Version:%q, URL:%q, PID:%d, Password:<redacted>}", s.ID, s.Version, s.URL, s.PID)
}

func (s Service) GoString() string { return s.String() }

// Client returns a client for the service: its address is checked to be a
// loopback one and its password becomes the HTTP basic credentials, sent to that
// address alone.
func (s Service) Client(opts ...Option) (*HTTPClient2, error) {
	return NewHTTPClient2(s.URL, Auth{User: ServiceUser, Password: s.Password}, opts...)
}

// ServicePath is where OpenCode 2 registers its service, given the environment:
// under XDG_STATE_HOME when that is set to an absolute path, else under
// $HOME/.local/state. getenv is os.Getenv, or a test's.
func ServicePath(getenv func(string) string) (string, error) {
	base := getenv("XDG_STATE_HOME")
	if base != "" && !filepath.IsAbs(base) {
		return "", fmt.Errorf("opencode: XDG_STATE_HOME %q is not an absolute path, so it does not say where OpenCode 2 registers its service", base)
	}
	if base == "" {
		home := getenv("HOME")
		if home == "" || !filepath.IsAbs(home) {
			return "", errors.New("opencode: neither XDG_STATE_HOME nor an absolute HOME is set, so it is not known where OpenCode 2 registers its service")
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "opencode", "service.json"), nil
}

// ReadService reads the registration file at path, after checking that it is
// what only the user's own OpenCode writes: a regular file, not reached through a
// symbolic link, owned by the current user, with no permission for the group or
// anyone else, of bounded size. The file is opened once and every check is made
// on the open file, so it cannot change between the check and the read. The
// address in it is not checked here: Service.Client does that, and whoever
// reaches the service through anything else must do the same.
func ReadService(path string) (Service, error) {
	// O_NOFOLLOW so that a link in place of the file is not followed, and O_NONBLOCK
	// so that a FIFO in its place is not waited on; neither is a regular file.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Service{}, fmt.Errorf("%w (looked for %s)", ErrNoService, path)
		}
		return Service{}, fmt.Errorf("opencode: open the service file %s: %w", path, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return Service{}, fmt.Errorf("opencode: stat the service file %s: %w", path, err)
	}
	if !fi.Mode().IsRegular() {
		return Service{}, fmt.Errorf("opencode: the service file %s is not a regular file", path)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return Service{}, fmt.Errorf("opencode: cannot tell who owns the service file %s on this system", path)
	}
	if int(st.Uid) != os.Getuid() {
		return Service{}, fmt.Errorf("opencode: the service file %s is owned by user %d and not by you (%d): it holds a password, and a file someone else owns is not trusted", path, st.Uid, os.Getuid())
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return Service{}, fmt.Errorf("opencode: the service file %s has mode %04o: it holds a password and must be readable by you alone (chmod 600 it, or stop and start the service, which writes it so)", path, fi.Mode().Perm())
	}
	if fi.Size() > maxServiceFileBytes {
		return Service{}, fmt.Errorf("opencode: the service file %s is %d bytes, over the %d it can be", path, fi.Size(), maxServiceFileBytes)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxServiceFileBytes+1))
	if err != nil {
		return Service{}, fmt.Errorf("opencode: read the service file %s: %w", path, err)
	}
	if len(b) > maxServiceFileBytes {
		return Service{}, fmt.Errorf("opencode: the service file %s is over the %d bytes it can be", path, maxServiceFileBytes)
	}
	var raw struct {
		ID       *string `json:"id"`
		Version  *string `json:"version"`
		URL      *string `json:"url"`
		PID      *int    `json:"pid"`
		Password *string `json:"password"`
	}
	// Not the decode helper: its error says what the answer of a server was, and
	// quotes the parse error, which for this file could quote the password.
	if err := json.Unmarshal(b, &raw); err != nil {
		return Service{}, fmt.Errorf("opencode: the service file %s is not the JSON object OpenCode 2 writes", path)
	}
	if raw.URL == nil || *raw.URL == "" || raw.PID == nil || *raw.PID < 1 {
		return Service{}, fmt.Errorf("opencode: the service file %s has no url and pid", path)
	}
	s := Service{URL: *raw.URL, PID: *raw.PID}
	if raw.ID != nil {
		s.ID = *raw.ID
	}
	if raw.Version != nil {
		s.Version = *raw.Version
	}
	if raw.Password != nil {
		s.Password = *raw.Password
	}
	return s, nil
}

// ServiceWhere names a service for a line of output: its address, and its
// process id, never its password.
func ServiceWhere(s Service) string {
	return s.URL + " (pid " + strconv.Itoa(s.PID) + ")"
}
