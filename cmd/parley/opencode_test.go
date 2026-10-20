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

	"github.com/colinsurprenant/parley/internal/doctor"
	"github.com/colinsurprenant/parley/internal/state"
)

const otherOCSession = "ses_ea8a722dcffeB5AVROj9ovn5Uq"

// ocEnv is the test process's environment less any OpenCode credentials a
// developer's own shell has set, plus extra assignments: a child whose behaviour
// turns on $OPENCODE_SERVER_PASSWORD must not inherit one.
func ocEnv(extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, opencodeLegacyPasswordEnv+"=") && !strings.HasPrefix(kv, opencodePasswordEnv+"=") {
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
		"opencode serve --port <n>", "opencode -s <id>", "service.json",
		"OPENCODE_PASSWORD", "OPENCODE_SERVER_PASSWORD",
		"parley was " + doctor.OpenCodeVerifiedNote,
		"the legacy 1.x is told the way to OpenCode 2",
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

// closedPort is a loopback port nothing listens on.
func closedPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// TestOpencodeFindsTheServiceAndDeliversIntoTheSession drives the real binary the
// way the human runs it beside an OpenCode TUI: no port, the service found from
// its file, one queued prompt carrying exactly what `parley wait` would have
// printed, peer header included, and the service's password nowhere in sight.
func TestOpencodeFindsTheServiceAndDeliversIntoTheSession(t *testing.T) {
	dir := resolvedTempDir(t)
	spec := "file:" + filepath.Join(dir, "chat.ndjson")
	stateDir := t.TempDir()
	stateHome := t.TempDir()
	srv := newOCServiceServer(t)
	writeService(t, stateHome, srv.url(), ocPID, 0o600)

	cmd, stdout, stderr := startOpencode(t, ocStateEnv(t, stateHome), ocServiceArgs(spec, "oc-a", ocSession, stateDir)...)
	waitForStderr(t, stderr, "into OpenCode session "+ocSession)
	if want := "on the OpenCode service at " + srv.url() + " (OpenCode 2.0.32)"; !strings.Contains(stderr.String(), want) {
		t.Fatalf("the start line does not name the service (%s):\n%s", want, stderr.String())
	}

	mustRun(t, "", "send", "--stream", spec, "--nick", "claude-b", "--", "hello from the peer")
	posts := srv.waitPosts(1, stderr)
	id, got := ocPosted(t, posts[0])
	if !strings.HasPrefix(id, "msg_") || len(id) != 30 {
		t.Fatalf("the message id %q is not one OpenCode makes", id)
	}
	want := mustRun(t, "", "wait", "--stream", spec, "--nick", "watcher-c",
		"--state-dir", t.TempDir(), "--timeout", "10s", "--debounce", "100ms").stdout
	if want == "" || got != want {
		t.Fatalf("the posted text is\n%q\nwant what parley wait prints\n%q", got, want)
	}
	if !strings.HasPrefix(got, "[parley] from=claude-b id=") {
		t.Fatalf("the posted text does not begin with the peer header:\n%q", got)
	}
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
		t.Fatalf("opencode exited %v on SIGTERM, want 0\nstderr: %s", err, stderr.String())
	}
	if stdout.String() != "" {
		t.Fatalf("the pump printed on stdout: %q", stdout.String())
	}
	if strings.Contains(stderr.String(), ocPassword) || strings.Contains(stdout.String(), ocPassword) {
		t.Fatalf("the service's password is in the output:\n%s", stderr.String())
	}
	if n := srv.refusedCount(); n != 0 {
		t.Fatalf("%d requests were turned away for their credentials", n)
	}
}

// TestOpencodeServiceDiscoveryFailuresAreConfigurationErrors: each way the
// service file or the service can be wrong is exit 2 with the reason and the
// command that makes it right, nothing taken, and the password nowhere.
func TestOpencodeServiceDiscoveryFailuresAreConfigurationErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		setup func(t *testing.T, stateHome string, srv *ocServer)
		want  []string
	}{
		"no service file": {
			setup: func(*testing.T, string, *ocServer) {},
			want:  []string{"no OpenCode 2 service is registered", "service.json", "opencode -s " + ocSession, "--port"},
		},
		"a file the group can read": {
			setup: func(t *testing.T, h string, s *ocServer) { writeService(t, h, s.url(), ocPID, 0o640) },
			want:  []string{"readable by you alone", "chmod 600"},
		},
		"a file anyone can read": {
			setup: func(t *testing.T, h string, s *ocServer) { writeService(t, h, s.url(), ocPID, 0o644) },
			want:  []string{"readable by you alone"},
		},
		"a file that is a link": {
			setup: func(t *testing.T, h string, s *ocServer) {
				real := writeService(t, t.TempDir(), s.url(), ocPID, 0o600)
				if err := os.MkdirAll(filepath.Join(h, "opencode"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(real, filepath.Join(h, "opencode", "service.json")); err != nil {
					t.Fatal(err)
				}
			},
			want: []string{"service file", "service.json"},
		},
		"a file that is not JSON": {
			setup: func(t *testing.T, h string, s *ocServer) {
				p := writeService(t, h, s.url(), ocPID, 0o600)
				if err := os.WriteFile(p, []byte("url="+s.url()), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: []string{"not the JSON object OpenCode 2 writes"},
		},
		"an address that is not loopback": {
			setup: func(t *testing.T, h string, s *ocServer) {
				writeService(t, h, "http://192.0.2.7:51868", ocPID, 0o600)
			},
			want: []string{"will not send its password to", "not a loopback IP address"},
		},
		"an address by name": {
			setup: func(t *testing.T, h string, s *ocServer) {
				writeService(t, h, "http://localhost:51868", ocPID, 0o600)
			},
			want: []string{"will not send its password to", "not a loopback IP address"},
		},
		"an address that is not http": {
			setup: func(t *testing.T, h string, s *ocServer) {
				writeService(t, h, "https://127.0.0.1:51868", ocPID, 0o600)
			},
			want: []string{"will not send its password to"},
		},
		"an address with credentials": {
			setup: func(t *testing.T, h string, s *ocServer) {
				writeService(t, h, strings.Replace(s.url(), "http://", "http://user:hunter2@", 1), ocPID, 0o600)
			},
			want: []string{"will not send its password to", "http://127.0.0.1:"},
		},
		"an address with a path": {
			setup: func(t *testing.T, h string, s *ocServer) { writeService(t, h, s.url()+"/x", ocPID, 0o600) },
			want:  []string{"will not send its password to"},
		},
		"a service that is gone": {
			setup: func(t *testing.T, h string, s *ocServer) {
				writeService(t, h, fmt.Sprintf("http://127.0.0.1:%d", closedPort(t)), ocPID, 0o600)
			},
			want: []string{"does not answer", "may have stopped and left the file behind", "opencode -s " + ocSession},
		},
		"a file left by another service": {
			setup: func(t *testing.T, h string, s *ocServer) { writeService(t, h, s.url(), ocPID+1, 0o600) },
			want:  []string{"describes pid", "stale or the address belongs to another server"},
		},
		"a version other than 2": {
			setup: func(t *testing.T, h string, s *ocServer) {
				writeService(t, h, s.url(), ocPID, 0o600)
				s.set(func() { s.version = "1.18.30" })
			},
			want: []string{"the legacy OpenCode 1", "brew install opencode"},
		},
		"an unknown session": {
			setup: func(t *testing.T, h string, s *ocServer) {
				writeService(t, h, s.url(), ocPID, 0o600)
				s.set(func() { s.known = map[string]bool{} })
			},
			want: []string{"does not know session " + ocSession, "opencode -s " + ocSession},
		},
		"the wrong password in the file": {
			setup: func(t *testing.T, h string, s *ocServer) {
				writeService(t, h, s.url(), ocPID, 0o600)
				s.set(func() { s.password = "rotated" })
			},
			want: []string{"401", "OPENCODE_PASSWORD"},
		},
		"a service that is up and broken": {
			setup: func(t *testing.T, h string, s *ocServer) {
				writeService(t, h, s.url(), ocPID, 0o600)
				s.set(func() { s.down = true })
			},
			want: []string{"500"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv := newOCServiceServer(t)
			stateHome := t.TempDir()
			tc.setup(t, stateHome, srv)
			stateDir := t.TempDir()
			// An irc: stream nothing listens on: opened first, it would be the error.
			spec := fmt.Sprintf("irc://127.0.0.1:%d/probe", closedPort(t))
			res := runParleyEnv(t, ocStateEnv(t, stateHome), "", append([]string{"opencode"}, ocServiceArgs(spec, "oc-a", ocSession, stateDir)...)...)
			if res.code != 2 {
				t.Fatalf("exited %d, want 2\nstderr: %s", res.code, res.stderr)
			}
			for _, want := range tc.want {
				if !strings.Contains(res.stderr, want) {
					t.Errorf("stderr does not say %q:\n%s", want, res.stderr)
				}
			}
			for _, secret := range []string{ocPassword, "rotated", "hunter2"} {
				if strings.Contains(res.stderr, secret) || strings.Contains(res.stdout, secret) {
					t.Errorf("the output quotes a password:\n%s", res.stderr)
				}
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

// TestOpencodeStateHomeThatIsNotAbsoluteIsRefused: a relative XDG_STATE_HOME does
// not say where the service file is, and is not guessed at.
func TestOpencodeStateHomeThatIsNotAbsoluteIsRefused(t *testing.T) {
	spec := "file:" + filepath.Join(resolvedTempDir(t), "chat.ndjson")
	res := runParleyEnv(t, ocStateEnv(t, "state"), "", append([]string{"opencode"}, ocServiceArgs(spec, "oc-a", ocSession, t.TempDir())...)...)
	if res.code != 2 || !strings.Contains(res.stderr, "not an absolute path") || !strings.Contains(res.stderr, "--port") {
		t.Fatalf("exit %d, stderr: %s", res.code, res.stderr)
	}
}

// TestOpencodeWithAPortTakesThePasswordFromTheEnvironment: --port is probed with
// the password the server was started with, from OPENCODE_PASSWORD, else from the
// older OPENCODE_SERVER_PASSWORD, and a server that wants one the pump has not
// been given is exit 2 saying so.
func TestOpencodeWithAPortTakesThePasswordFromTheEnvironment(t *testing.T) {
	for name, tc := range map[string]struct {
		env      []string
		password string
		wantRun  bool
		want     []string
	}{
		"a server with no password": {password: "", wantRun: true},
		"OPENCODE_PASSWORD":         {env: []string{opencodePasswordEnv + "=" + ocPassword}, password: ocPassword, wantRun: true},
		"the older variable":        {env: []string{opencodeLegacyPasswordEnv + "=" + ocPassword}, password: ocPassword, wantRun: true},
		"the newer one wins": {
			env:      []string{opencodePasswordEnv + "=" + ocPassword, opencodeLegacyPasswordEnv + "=old-and-wrong"},
			password: ocPassword, wantRun: true,
		},
		"no password given": {password: ocPassword, want: []string{"401", opencodePasswordEnv}},
		"the wrong password": {
			env:      []string{opencodePasswordEnv + "=not-it"},
			password: ocPassword, want: []string{"401", opencodePasswordEnv},
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := resolvedTempDir(t)
			spec := "file:" + filepath.Join(dir, "chat.ndjson")
			srv := newOCServiceServer(t)
			srv.set(func() { srv.password = tc.password })
			env := ocStateEnv(t, t.TempDir(), tc.env...)
			if tc.wantRun {
				cmd, _, stderr := startOpencode(t, env, ocArgs(spec, "oc-a", srv.port(), ocSession, t.TempDir())...)
				waitForStderr(t, stderr, "into OpenCode session "+ocSession)
				if want := fmt.Sprintf("on 127.0.0.1:%d (OpenCode 2.0.32)", srv.port()); !strings.Contains(stderr.String(), want) {
					t.Fatalf("the start line does not name the server (%s):\n%s", want, stderr.String())
				}
				mustRun(t, "", "send", "--stream", spec, "--nick", "claude-b", "--", "to a 2.x server")
				posts := srv.waitPosts(1, stderr)
				if _, text := ocPosted(t, posts[0]); !strings.Contains(text, "to a 2.x server") {
					t.Fatalf("the delivery is\n%s", text)
				}
				if err := stopOpencode(t, cmd, stderr); err != nil {
					t.Fatalf("opencode exited %v", err)
				}
				if tc.password != "" && strings.Contains(stderr.String(), tc.password) {
					t.Fatalf("the password is in the output:\n%s", stderr.String())
				}
				return
			}
			res := runParleyEnv(t, env, "", append([]string{"opencode"}, ocArgs(spec, "oc-a", srv.port(), ocSession, t.TempDir())...)...)
			if res.code != 2 {
				t.Fatalf("exited %d, want 2\nstderr: %s", res.code, res.stderr)
			}
			for _, want := range tc.want {
				if !strings.Contains(res.stderr, want) {
					t.Errorf("stderr does not say %q:\n%s", want, res.stderr)
				}
			}
			for _, secret := range []string{ocPassword, "not-it", "old-and-wrong"} {
				if strings.Contains(res.stderr, secret) {
					t.Errorf("stderr quotes a password:\n%s", res.stderr)
				}
			}
		})
	}
}

// TestOpencodeWithAPortThatNothingListensOnSaysWhatToRun: the error names how to
// start a server on the port, and the background service, which needs no port.
func TestOpencodeWithAPortThatNothingListensOnSaysWhatToRun(t *testing.T) {
	spec := "file:" + filepath.Join(resolvedTempDir(t), "chat.ndjson")
	stateDir := t.TempDir()
	res := runParleyEnv(t, ocStateEnv(t, t.TempDir()), "", append([]string{"opencode"}, ocArgs(spec, "oc-a", closedPort(t), ocSession, stateDir)...)...)
	if res.code != 2 {
		t.Fatalf("exited %d, want 2\nstderr: %s", res.code, res.stderr)
	}
	for _, want := range []string{"cannot reach OpenCode on 127.0.0.1:", "opencode serve --port ", "opencode -s " + ocSession, "without --port"} {
		if !strings.Contains(res.stderr, want) {
			t.Errorf("stderr does not say %q:\n%s", want, res.stderr)
		}
	}
	noLockTaken(t, stateDir, "oc-a")
}

// TestOpencodeWithAPortOfAServerThatDoesNotKnowTheSession: a server that answers,
// and does not have the session, is told as that.
func TestOpencodeWithAPortOfAServerThatDoesNotKnowTheSession(t *testing.T) {
	srv := newOCServiceServer(t)
	srv.set(func() { srv.password, srv.known = "", map[string]bool{} })
	spec := "file:" + filepath.Join(resolvedTempDir(t), "chat.ndjson")
	res := runParleyEnv(t, ocStateEnv(t, t.TempDir()), "", append([]string{"opencode"}, ocArgs(spec, "oc-a", srv.port(), ocSession, t.TempDir())...)...)
	if res.code != 2 || !strings.Contains(res.stderr, "OpenCode on 127.0.0.1:") || !strings.Contains(res.stderr, "does not know session "+ocSession) || !strings.Contains(res.stderr, "opencode -s "+ocSession) {
		t.Fatalf("exit %d, stderr: %s", res.code, res.stderr)
	}
}

// TestOpencodeWithNoPortAndNoServiceSaysWhatStartsOne: with no --port the pump
// looks for OpenCode's service, and the error for finding none says what starts it,
// and that a server started with opencode serve is reached with --port. The environment names the state
// directory, so the developer's own is never looked in.
func TestOpencodeWithNoPortAndNoServiceSaysWhatStartsOne(t *testing.T) {
	spec := "file:" + filepath.Join(resolvedTempDir(t), "chat.ndjson")
	stateDir, stateHome := t.TempDir(), t.TempDir()
	res := runParleyEnv(t, ocStateEnv(t, stateHome), "", append([]string{"opencode"}, ocServiceArgs(spec, "oc-a", ocSession, stateDir)...)...)
	if res.code != 2 {
		t.Fatalf("exited %d, want 2\nstderr: %s", res.code, res.stderr)
	}
	for _, want := range []string{"no OpenCode 2 service is registered", filepath.Join(stateHome, "opencode", "service.json"), "opencode -s " + ocSession, "pass --port <n>", "opencode serve --port <n>"} {
		if !strings.Contains(res.stderr, want) {
			t.Errorf("stderr does not say %q:\n%s", want, res.stderr)
		}
	}
	if strings.Count(res.stderr, "no OpenCode 2 service is registered") != 1 {
		t.Errorf("the refusal says it twice:\n%s", res.stderr)
	}
	noLockTaken(t, stateDir, "oc-a")
}
