package main

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/colinsurprenant/parley/internal/state"
)

const otherOCSession = "ses_ea8a722dcffeB5AVROj9ovn5Uq"

// ocEnv is the test process's environment less any OpenCode credentials a
// developer's own shell has set, plus extra assignments: a child whose behaviour
// turns on $OPENCODE_SERVER_PASSWORD must not inherit one.
func ocEnv(extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, opencodePasswordEnv+"=") && !strings.HasPrefix(kv, opencodeUserEnv+"=") && !strings.HasPrefix(kv, opencodePassword2Env+"=") {
			env = append(env, kv)
		}
	}
	return append(env, extra...)
}

// startOpencode starts `parley opencode` with the given arguments and returns
// the running command and its stderr. Stdout is captured too, to say that
// nothing is ever printed on it. The pump is stopped when the test ends.
func startOpencode(t *testing.T, env []string, args ...string) (*exec.Cmd, *syncBuffer, *syncBuffer) {
	t.Helper()
	cmd := exec.Command(binary(t), append([]string{"opencode"}, args...)...)
	cmd.Env = env
	stdout, stderr := &syncBuffer{}, &syncBuffer{}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start opencode: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-joinCmd(cmd):
		case <-time.After(childDeadline):
			_ = cmd.Process.Kill()
		}
	})
	return cmd, stdout, stderr
}

// stopOpencode ends a running pump with SIGTERM, the way the human does, and
// returns how it ended.
func stopOpencode(t *testing.T, cmd *exec.Cmd, stderr *syncBuffer) error {
	t.Helper()
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal the pump: %v", err)
	}
	select {
	case err := <-joinCmd(cmd):
		return err
	case <-time.After(childDeadline):
		_ = cmd.Process.Kill()
		t.Fatalf("the pump did not exit within %s of the SIGTERM\nstderr: %s", childDeadline, stderr.String())
		return nil
	}
}

func ocArgs(spec, nick string, port int, session, stateDir string, extra ...string) []string {
	return append([]string{"--stream", spec, "--nick", nick, "--session", session, "--port", fmt.Sprint(port),
		"--state-dir", stateDir, "--debounce", "100ms"}, extra...)
}

func noLockTaken(t *testing.T, stateDir, nick string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(stateDir, nick, "lock")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a refused invocation took the nick's lock (stat err %v)", err)
	}
}

// TestOpencodePostsTheDeliveryIntoTheSession drives the real binary the way the
// human runs it beside a live OpenCode TUI: a foreground pump and one prompt
// carrying exactly what `parley wait` would have printed — the peer header
// included, since on this front the header is the only thing that says the text
// is not the human's.
func TestOpencodePostsTheDeliveryIntoTheSession(t *testing.T) {
	dir := resolvedTempDir(t)
	spec := "file:" + filepath.Join(dir, "chat.ndjson")
	stateDir := t.TempDir()
	srv := newOCServer(t)

	cmd, stdout, stderr := startOpencode(t, ocEnv(), ocArgs(spec, "oc-a", srv.port(), ocSession, stateDir)...)

	// A second process for this nick is refused while the pump holds the lock.
	// Sequenced behind the pump's startup line, which is printed only once the
	// lock is held.
	waitForStderr(t, stderr, "into OpenCode session "+ocSession)
	second := runParleyEnv(t, ocEnv(), "", append([]string{"opencode"}, ocArgs(spec, "oc-a", srv.port(), ocSession, stateDir)...)...)
	if second.code != 2 {
		t.Fatalf("a second opencode pump for the nick exited %d, want 2\nstderr: %s", second.code, second.stderr)
	}
	if !strings.Contains(second.stderr, `"oc-a"`) {
		t.Fatalf("the lock error does not name the nick: %s", second.stderr)
	}
	if !strings.Contains(stderr.String(), fmt.Sprintf("on 127.0.0.1:%d (OpenCode 1.18.30)", srv.port())) {
		t.Fatalf("the start line does not name the server:\n%s", stderr.String())
	}

	mustRun(t, "", "send", "--stream", spec, "--nick", "claude-b", "--", "hello from the peer")
	posts := srv.waitPosts(1, stderr)
	got := ocPostedText(t, posts[0])

	// What a third participant is handed for the same message is what the
	// session got: a different nick and a state directory of its own, so its
	// cursor starts where the pump's did.
	want := mustRun(t, "", "wait", "--stream", spec, "--nick", "watcher-c",
		"--state-dir", t.TempDir(), "--timeout", "10s", "--debounce", "100ms").stdout
	if want == "" {
		t.Fatal("the reference wait printed nothing")
	}
	if got != want {
		t.Fatalf("the posted text is\n%q\nwant what parley wait prints\n%q", got, want)
	}
	if !strings.HasPrefix(got, "[parley] from=claude-b id=") {
		t.Fatalf("the posted text does not begin with the peer header:\n%q", got)
	}

	// A signal is how the human ends the pump, and it is not a failure.
	if err := stopOpencode(t, cmd, stderr); err != nil {
		t.Fatalf("opencode exited %v on SIGTERM, want 0\nstderr: %s", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "shutting down") {
		t.Fatalf("the shutdown was not logged:\n%s", stderr.String())
	}
	if stdout.String() != "" {
		t.Fatalf("the pump printed on stdout: %q", stdout.String())
	}
	if n := len(srv.postBodies()); n != 1 {
		t.Fatalf("%d prompts, want 1", n)
	}
}

// TestOpencodeWaitsOutATurnItReadsUnderTheSessionsDirectory: the server lists a
// turn only under the directory the session belongs to, and the pump runs against
// one that is not where it is run from. A pump that read the status without the
// session's directory would see an idle session and post into the turn.
func TestOpencodeWaitsOutATurnItReadsUnderTheSessionsDirectory(t *testing.T) {
	dir := resolvedTempDir(t)
	spec := "file:" + filepath.Join(dir, "chat.ndjson")
	srv := newOCServer(t)
	srv.set(func() { srv.busy = true })

	cmd, _, stderr := startOpencode(t, ocEnv(), ocArgs(spec, "oc-a", srv.port(), ocSession, t.TempDir())...)
	waitForStderr(t, stderr, "into OpenCode session")
	mustRun(t, "", "send", "--stream", spec, "--nick", "claude-b", "--", "wait for it")
	// The pump polls a busy session every half second; give it several.
	deadline := time.Now().Add(childDeadline)
	for len(srv.statusDirsSeen()) < 3 {
		if !time.Now().Before(deadline) {
			t.Fatalf("the pump never polled the status:\n%s", stderr.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n := len(srv.postBodies()); n != 0 {
		t.Fatalf("%d prompt(s) posted into a session in the middle of a turn", n)
	}
	srv.set(func() { srv.busy = false })
	srv.waitPosts(1, stderr)
	for _, d := range srv.statusDirsSeen() {
		if d != ocDir {
			t.Fatalf("a status read named directory %q, want the session's %q", d, ocDir)
		}
	}
	if err := stopOpencode(t, cmd, stderr); err != nil {
		t.Fatalf("opencode exited %v", err)
	}
}

// TestOpencodeRestartDoesNotDeliverAgain: the cursor saved once the history
// showed a delivery carries a restart past it, and a message after it is a
// delivery of its own.
func TestOpencodeRestartDoesNotDeliverAgain(t *testing.T) {
	dir := resolvedTempDir(t)
	spec := "file:" + filepath.Join(dir, "chat.ndjson")
	stateDir := t.TempDir()
	srv := newOCServer(t)
	args := ocArgs(spec, "oc-a", srv.port(), ocSession, stateDir)

	cmd, _, stderr := startOpencode(t, ocEnv(), args...)
	waitForStderr(t, stderr, "into OpenCode session")
	mustRun(t, "", "send", "--stream", spec, "--nick", "claude-b", "--", "first")
	srv.waitPosts(1, stderr)
	// The cursor is written after the prompt is seen in the history, so the stop
	// must wait for it, or the restart legitimately delivers again.
	cursor := cursorPath(t, stateDir, "oc-a", spec)
	deadline := time.Now().Add(childDeadline)
	for {
		if b, err := os.ReadFile(cursor); err == nil && len(b) > 0 {
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("no cursor saved:\n%s", stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := stopOpencode(t, cmd, stderr); err != nil {
		t.Fatalf("first pump exited %v", err)
	}

	cmd2, _, stderr2 := startOpencode(t, ocEnv(), args...)
	waitForStderr(t, stderr2, "into OpenCode session")
	mustRun(t, "", "send", "--stream", spec, "--nick", "claude-b", "--", "second")
	posts := srv.waitPosts(2, stderr2)
	if text := ocPostedText(t, posts[1]); !strings.Contains(text, "second") || strings.Contains(text, "first") {
		t.Fatalf("the restarted pump delivered\n%q\nwant the second message alone", text)
	}
	if err := stopOpencode(t, cmd2, stderr2); err != nil {
		t.Fatalf("second pump exited %v", err)
	}
}

// TestOpencodeStartupProbeFailuresAreConfigurationErrors: each way the server can
// be wrong at startup is exit 2 with the reason, the TUI command where that is
// what fixes it, and nothing taken — no lock and no state directory for the nick.
// The adapters open lazily, so that is all that can be seen of the order; the
// stream is an irc: one that nothing listens on to show that it is never dialed
// first either.
func TestOpencodeStartupProbeFailuresAreConfigurationErrors(t *testing.T) {
	closed := func() int {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		return l.Addr().(*net.TCPAddr).Port
	}

	for name, tc := range map[string]struct {
		setup func(*ocServer) int // returns the port to use
		env   []string
		want  []string
	}{
		"nothing listens": {
			setup: func(*ocServer) int { return closed() },
			want:  []string{"cannot reach OpenCode on 127.0.0.1:", "opencode --port ", "--session " + ocSession, "opencode -s " + ocSession, "without --port"},
		},
		"OpenCode 2": {
			setup: func(s *ocServer) int { s.set(func() { s.version = "2.0.32" }); return s.port() },
			want:  []string{"not OpenCode 1", "2.0.32"},
		},
		"OpenCode 2 serving its web page": {
			setup: func(s *ocServer) int { s.set(func() { s.page = true }); return s.port() },
			want:  []string{"As OpenCode 1", "GET /global/health", "text/html", "As OpenCode 2", "GET /api/info", "404", "opencode -s " + ocSession},
		},
		"a version that cannot be read": {
			setup: func(s *ocServer) int { s.set(func() { s.version = "dev" }); return s.port() },
			want:  []string{"cannot read an OpenCode version"},
		},
		"an unknown session": {
			setup: func(s *ocServer) int { s.set(func() { s.known = map[string]bool{} }); return s.port() },
			want:  []string{"does not know session " + ocSession, "opencode --port ", "--session " + ocSession},
		},
		"a password the pump was not given": {
			setup: func(s *ocServer) int { s.set(func() { s.password = "hunter2-test" }); return s.port() },
			want:  []string{"401", opencodePasswordEnv},
		},
		"the wrong password": {
			setup: func(s *ocServer) int { s.set(func() { s.password = "hunter2-test" }); return s.port() },
			env:   []string{opencodePasswordEnv + "=not-it"},
			want:  []string{"401", opencodePasswordEnv},
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv := newOCServer(t)
			port := tc.setup(srv)
			stateDir := t.TempDir()
			// An irc: stream nothing listens on: opened first, it would be the
			// error.
			spec := fmt.Sprintf("irc://127.0.0.1:%d/probe", closed())
			res := runParleyEnv(t, ocEnv(tc.env...), "", append([]string{"opencode"}, ocArgs(spec, "oc-a", port, ocSession, stateDir)...)...)
			if res.code != 2 {
				t.Fatalf("exited %d, want 2\nstderr: %s", res.code, res.stderr)
			}
			for _, want := range tc.want {
				if !strings.Contains(res.stderr, want) {
					t.Errorf("stderr does not say %q:\n%s", want, res.stderr)
				}
			}
			if strings.Contains(res.stderr, "hunter2-test") || strings.Contains(res.stderr, "not-it") {
				t.Errorf("stderr quotes a password:\n%s", res.stderr)
			}
			if res.stdout != "" {
				t.Errorf("stdout is %q, want nothing", res.stdout)
			}
			noLockTaken(t, stateDir, "oc-a")
			if _, err := os.Stat(filepath.Join(stateDir, "oc-a")); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("a refused invocation left state behind (stat err %v)", err)
			}
		})
	}
}

// TestOpencodeSessionAutoDeliversIntoTheRecordedSession: with a record written
// through the state API, --session auto is that session, and the start line says
// where it came from.
func TestOpencodeSessionAutoDeliversIntoTheRecordedSession(t *testing.T) {
	dir := resolvedTempDir(t)
	spec := "file:" + filepath.Join(dir, "chat.ndjson")
	stateDir := t.TempDir()
	srv := newOCServer(t)
	srv.set(func() { srv.known[otherOCSession] = true })

	rec, err := state.OpenSessionRecord(stateDir, "oc-a", spec)
	if err != nil {
		t.Fatalf("OpenSessionRecord: %v", err)
	}
	if err := rec.Write(otherOCSession); err != nil {
		t.Fatalf("write the record: %v", err)
	}

	cmd, _, stderr := startOpencode(t, ocEnv(), ocArgs(spec, "oc-a", srv.port(), "auto", stateDir)...)
	waitForStderr(t, stderr, "into OpenCode session "+otherOCSession)
	if !strings.Contains(stderr.String(), rec.Path()) {
		t.Fatalf("the start line does not say where the session came from (%s):\n%s", rec.Path(), stderr.String())
	}
	mustRun(t, "", "send", "--stream", spec, "--nick", "claude-b", "--", "hello")
	posts := srv.waitPosts(1, stderr)
	if !strings.HasPrefix(ocPostedText(t, posts[0]), "[parley] from=claude-b id=") {
		t.Fatalf("the posted text does not begin with the peer header: %q", ocPostedText(t, posts[0]))
	}
	if err := stopOpencode(t, cmd, stderr); err != nil {
		t.Fatalf("opencode exited %v on SIGTERM, want 0", err)
	}
}

// TestOpencodeSessionAutoWithoutAUsableRecord: no record, a malformed one, or a
// record of a session the server lacks is exit 2 with the nick's lock never
// taken.
func TestOpencodeSessionAutoWithoutAUsableRecord(t *testing.T) {
	dir := resolvedTempDir(t)
	spec := "file:" + filepath.Join(dir, "chat.ndjson")
	srv := newOCServer(t)

	// What it is told to do about it depends on the skill: a line's session is
	// recorded by `parley line add`, and a table or a build takes no OpenCode
	// seat yet, so nothing records one for them.
	for name, tc := range map[string]struct {
		extra []string
		want  []string
		not   []string
	}{
		"no record on a line": {
			extra: []string{"--skill", "line", "--chair", "colin", "--cap", "3"},
			want:  []string{"parley line add --harness opencode", "or pass --session <id>"},
			not:   []string{"take no OpenCode seats"},
		},
		"no record on a build": {
			want: []string{"tables and builds take no OpenCode seats yet", "pass --session <id>"},
			not:  []string{"parley line add"},
		},
		"no record on a table": {
			extra: []string{"--skill", "table", "--chair", "colin", "--cap", "3"},
			want:  []string{"tables and builds take no OpenCode seats yet", "pass --session <id>"},
			not:   []string{"parley line add"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			stateDir := t.TempDir()
			res := runParleyEnv(t, ocEnv(), "", append([]string{"opencode"}, ocArgs(spec, "oc-a", srv.port(), "auto", stateDir, tc.extra...)...)...)
			if res.code != 2 {
				t.Fatalf("exited %d, want 2\nstderr: %s", res.code, res.stderr)
			}
			rec, _ := state.OpenSessionRecord(stateDir, "oc-a", spec)
			for _, want := range append([]string{"no OpenCode session is recorded", `"oc-a"`, rec.Path()}, tc.want...) {
				if !strings.Contains(res.stderr, want) {
					t.Errorf("stderr does not say %q:\n%s", want, res.stderr)
				}
			}
			for _, not := range tc.not {
				if strings.Contains(res.stderr, not) {
					t.Errorf("stderr says %q:\n%s", not, res.stderr)
				}
			}
			noLockTaken(t, stateDir, "oc-a")
			if n := len(srv.postBodies()); n != 0 {
				t.Errorf("%d prompts", n)
			}
		})
	}
	t.Run("a malformed record", func(t *testing.T) {
		stateDir := t.TempDir()
		rec, _ := state.OpenSessionRecord(stateDir, "oc-a", spec)
		if err := os.MkdirAll(filepath.Dir(rec.Path()), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(rec.Path(), []byte("not a session\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		res := runParleyEnv(t, ocEnv(), "", append([]string{"opencode"}, ocArgs(spec, "oc-a", srv.port(), "auto", stateDir)...)...)
		if res.code != 2 || !strings.Contains(res.stderr, "malformed") || !strings.Contains(res.stderr, rec.Path()) {
			t.Fatalf("exited %d, stderr: %s", res.code, res.stderr)
		}
		noLockTaken(t, stateDir, "oc-a")
	})
	t.Run("a recorded session the server lacks", func(t *testing.T) {
		stateDir := t.TempDir()
		rec, _ := state.OpenSessionRecord(stateDir, "oc-a", spec)
		if err := rec.Write(otherOCSession); err != nil {
			t.Fatal(err)
		}
		res := runParleyEnv(t, ocEnv(), "", append([]string{"opencode"}, ocArgs(spec, "oc-a", srv.port(), "auto", stateDir)...)...)
		if res.code != 2 || !strings.Contains(res.stderr, "does not know session "+otherOCSession) {
			t.Fatalf("exited %d, stderr: %s", res.code, res.stderr)
		}
		noLockTaken(t, stateDir, "oc-a")
	})
}

// TestOpencodeRefusesABadInvocation: each flag mistake is exit 2 with the nick's
// lock never taken and the server never asked.
func TestOpencodeRefusesABadInvocation(t *testing.T) {
	dir := resolvedTempDir(t)
	spec := "file:" + filepath.Join(dir, "chat.ndjson")
	srv := newOCServer(t)
	port := fmt.Sprint(srv.port())
	base := func(extra ...string) []string {
		return append([]string{"opencode", "--stream", spec, "--nick", "oc-a", "--session", ocSession, "--port", port}, extra...)
	}
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"no nick":               {[]string{"opencode", "--stream", spec, "--session", ocSession, "--port", port}, "--nick is required"},
		"no stream":             {[]string{"opencode", "--nick", "oc-a", "--session", ocSession, "--port", port}, "--stream is required"},
		"port zero":             {base("--port", "0"), "is not a port"},
		"port too big":          {base("--port", "70000"), "is not a port"},
		"no session":            {[]string{"opencode", "--stream", spec, "--nick", "oc-a", "--port", port}, "--session is required"},
		"a session name":        {base("--session", "my-session"), "is not an OpenCode session id"},
		"a thread uuid":         {base("--session", "0199c0de-c0de-7c0d-8c0d-ec0dec0dec0d"), "is not an OpenCode session id"},
		"a short id":            {base("--session", ocSession[:29]), "is not an OpenCode session id"},
		"a positional":          {base("extra"), `unexpected argument "extra"`},
		"negative debounce":     {base("--debounce", "-1s"), "--debounce must not be negative"},
		"an unknown skill":      {base("--skill", "nope"), "--skill"},
		"a chair on a build":    {base("--chair", "colin"), "--chair applies to --skill table or line only"},
		"a table with no chair": {base("--skill", "table", "--cap", "0"), "--chair is required"},
		"a table with no cap":   {base("--skill", "table", "--chair", "colin"), "--cap is required"},
		"the chair is me":       {base("--skill", "line", "--chair", "OC-A", "--cap", "3"), "own --nick"},
		"an unknown flag":       {base("--thread", "x"), "flag provided but not defined"},
		"a codex-only flag":     {base("--codex-bin", "codex"), "flag provided but not defined"},
		"an unknown scheme":     {append([]string{"opencode"}, "--stream", "ftp:x", "--nick", "oc-a", "--session", ocSession, "--port", port), "unknown stream scheme"},
	} {
		t.Run(name, func(t *testing.T) {
			stateDir := t.TempDir()
			res := runParleyEnv(t, ocEnv(), "", append(tc.args, "--state-dir", stateDir)...)
			if res.code != 2 || !strings.Contains(res.stderr, tc.want) {
				t.Fatalf("exited %d, want 2 saying %q\nstderr: %s", res.code, tc.want, res.stderr)
			}
			noLockTaken(t, stateDir, "oc-a")
		})
	}
	if n := len(srv.postBodies()); n != 0 {
		t.Fatalf("%d prompts", n)
	}
}

// TestOpencodeSendsBasicCredentialsFromTheEnvironment: the password and the user
// the server was started with are the two variables OpenCode's own CLI reads, and
// the pump presents them to the server it probes and posts to.
func TestOpencodeSendsBasicCredentialsFromTheEnvironment(t *testing.T) {
	for name, tc := range map[string]struct {
		user string
		env  []string
	}{
		"the default user": {"opencode", []string{opencodePasswordEnv + "=pw-one-xyz"}},
		"a named user":     {"alice", []string{opencodePasswordEnv + "=pw-one-xyz", opencodeUserEnv + "=alice"}},
	} {
		t.Run(name, func(t *testing.T) {
			dir := resolvedTempDir(t)
			spec := "file:" + filepath.Join(dir, "chat.ndjson")
			srv := newOCServer(t)
			srv.set(func() { srv.user, srv.password = tc.user, "pw-one-xyz" })

			cmd, _, stderr := startOpencode(t, ocEnv(tc.env...), ocArgs(spec, "oc-a", srv.port(), ocSession, t.TempDir())...)
			waitForStderr(t, stderr, "into OpenCode session")
			mustRun(t, "", "send", "--stream", spec, "--nick", "claude-b", "--", "hello")
			srv.waitPosts(1, stderr)
			if err := stopOpencode(t, cmd, stderr); err != nil {
				t.Fatalf("opencode exited %v", err)
			}
			if strings.Contains(stderr.String(), "pw-one-xyz") {
				t.Fatalf("the log quotes the password:\n%s", stderr.String())
			}
			srv.mu.Lock()
			refused := srv.refused
			srv.mu.Unlock()
			if refused != 0 {
				t.Fatalf("%d requests were turned away for their credentials", refused)
			}
		})
	}
}

// TestOpencodeHoldsTheOtherSeatsUntilItHasPosted drives the table flags through
// the whole of round one: a seat's post is held, the chair's passes. It is what
// proves the flags reach the Deliverer on this front.
func TestOpencodeHoldsTheOtherSeatsUntilItHasPosted(t *testing.T) {
	dir := resolvedTempDir(t)
	spec := "file:" + filepath.Join(dir, "chat.ndjson")
	srv := newOCServer(t)

	cmd, _, stderr := startOpencode(t, ocEnv(), ocArgs(spec, "oc-a", srv.port(), ocSession, t.TempDir(),
		"--skill", "table", "--chair", "colin", "--cap", "0")...)
	waitForStderr(t, stderr, "into OpenCode session")

	mustRun(t, "", "send", "--stream", spec, "--nick", "claude-b", "--", "seat talk")
	mustRun(t, "", "send", "--stream", spec, "--nick", "colin", "--", "chair talk")
	posts := srv.waitPosts(1, stderr)
	if msg := ocPostedText(t, posts[0]); !strings.Contains(msg, "from=colin") || strings.Contains(msg, "seat talk") {
		t.Fatalf("the sealed pump delivered\n%s\nwant the chair's post alone", msg)
	}
	if err := stopOpencode(t, cmd, stderr); err != nil {
		t.Fatalf("opencode exited %v", err)
	}
}

// TestUsageDocumentsTheOpencodeFront: the help is where a human finds the verb,
// its flags and the record it reads.
func TestUsageDocumentsTheOpencodeFront(t *testing.T) {
	top := mustRun(t, "", "-h").stdout
	flat := strings.Join(strings.Fields(top), " ")
	for _, want := range []string{
		"parley opencode --stream <spec> --nick <nick> --session <ses_id>|auto [--port <n>] [--debounce 250ms] [--state-dir <dir>]",
		"opencode run in the foreground and deliver into a live OpenCode session",
		"opencode --port <n> --session <id>", "opencode -s <id>", "service.json",
		"OPENCODE_SERVER_PASSWORD", "OPENCODE_SERVER_USERNAME", "OPENCODE_PASSWORD",
		"2.0.32 is the 2.x it was verified against, in OpenCode's simulation mode and not yet on a live model",
		"a message that renders to over 128 KiB, and every message once the delivery has 512 KiB, is replaced by a stub",
		"is made again before every delivery",
		"opencode-session",
		"channel, codex and opencode take --chair <nick> --cap <n>",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("usage does not mention %q", want)
		}
	}
	help := runParley(t, "", "opencode", "-h")
	if help.code != 0 {
		t.Fatalf("parley opencode -h exited %d", help.code)
	}
	for _, want := range []string{"usage: parley opencode", "-session", "-port", "-stream", "-nick", "-skill", "-chair", "-cap", "-window", "memory only"} {
		if !strings.Contains(help.stdout, want) {
			t.Errorf("parley opencode -h does not mention %q:\n%s", want, help.stdout)
		}
	}
	for _, unwanted := range []string{"-thread", "-codex-bin", "-host", "-url"} {
		if strings.Contains(help.stdout, unwanted) {
			t.Errorf("parley opencode -h mentions %q", unwanted)
		}
	}
}
