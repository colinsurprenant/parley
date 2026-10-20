package main

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
// way the human runs it beside an OpenCode 2 TUI: no port, the service found from
// its file, one queued prompt carrying exactly what `parley wait` would have
// printed, peer header included, and the service's password nowhere in sight.
func TestOpencodeFindsTheServiceAndDeliversIntoTheSession(t *testing.T) {
	dir := resolvedTempDir(t)
	spec := "file:" + filepath.Join(dir, "chat.ndjson")
	stateDir := t.TempDir()
	stateHome := t.TempDir()
	srv := newOC2Server(t)
	writeService(t, stateHome, srv.url(), oc2PID, 0o600)

	cmd, stdout, stderr := startOpencode(t, oc2Env(t, stateHome), oc2Args(spec, "oc-a", ocSession, stateDir)...)
	waitForStderr(t, stderr, "into OpenCode session "+ocSession)
	if want := "on the OpenCode 2 service at " + srv.url() + " (OpenCode 2.0.32)"; !strings.Contains(stderr.String(), want) {
		t.Fatalf("the start line does not name the service (%s):\n%s", want, stderr.String())
	}

	mustRun(t, "", "send", "--stream", spec, "--nick", "claude-b", "--", "hello from the peer")
	posts := srv.waitPosts(1, stderr)
	id, got := oc2PostedText(t, posts[0])
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
	if strings.Contains(stderr.String(), oc2Password) || strings.Contains(stdout.String(), oc2Password) {
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
		setup func(t *testing.T, stateHome string, srv *oc2Server)
		want  []string
	}{
		"no service file": {
			setup: func(*testing.T, string, *oc2Server) {},
			want:  []string{"no OpenCode 2 service is registered", "service.json", "opencode -s " + ocSession, "--port"},
		},
		"a file the group can read": {
			setup: func(t *testing.T, h string, s *oc2Server) { writeService(t, h, s.url(), oc2PID, 0o640) },
			want:  []string{"readable by you alone", "chmod 600"},
		},
		"a file anyone can read": {
			setup: func(t *testing.T, h string, s *oc2Server) { writeService(t, h, s.url(), oc2PID, 0o644) },
			want:  []string{"readable by you alone"},
		},
		"a file that is a link": {
			setup: func(t *testing.T, h string, s *oc2Server) {
				real := writeService(t, t.TempDir(), s.url(), oc2PID, 0o600)
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
			setup: func(t *testing.T, h string, s *oc2Server) {
				p := writeService(t, h, s.url(), oc2PID, 0o600)
				if err := os.WriteFile(p, []byte("url="+s.url()), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: []string{"not the JSON object OpenCode 2 writes"},
		},
		"an address that is not loopback": {
			setup: func(t *testing.T, h string, s *oc2Server) {
				writeService(t, h, "http://192.0.2.7:51868", oc2PID, 0o600)
			},
			want: []string{"will not send its password to", "not a loopback IP address"},
		},
		"an address by name": {
			setup: func(t *testing.T, h string, s *oc2Server) {
				writeService(t, h, "http://localhost:51868", oc2PID, 0o600)
			},
			want: []string{"will not send its password to", "not a loopback IP address"},
		},
		"an address that is not http": {
			setup: func(t *testing.T, h string, s *oc2Server) {
				writeService(t, h, "https://127.0.0.1:51868", oc2PID, 0o600)
			},
			want: []string{"will not send its password to"},
		},
		"an address with a path": {
			setup: func(t *testing.T, h string, s *oc2Server) { writeService(t, h, s.url()+"/x", oc2PID, 0o600) },
			want:  []string{"will not send its password to"},
		},
		"a service that is gone": {
			setup: func(t *testing.T, h string, s *oc2Server) {
				writeService(t, h, fmt.Sprintf("http://127.0.0.1:%d", closedPort(t)), oc2PID, 0o600)
			},
			want: []string{"does not answer", "may have stopped and left the file behind", "opencode -s " + ocSession},
		},
		"a file left by another service": {
			setup: func(t *testing.T, h string, s *oc2Server) { writeService(t, h, s.url(), oc2PID+1, 0o600) },
			want:  []string{"describes pid", "stale or the address belongs to another server"},
		},
		"a version other than 2": {
			setup: func(t *testing.T, h string, s *oc2Server) {
				writeService(t, h, s.url(), oc2PID, 0o600)
				s.set(func() { s.version = "1.18.30" })
			},
			want: []string{"not OpenCode 2"},
		},
		"an unknown session": {
			setup: func(t *testing.T, h string, s *oc2Server) {
				writeService(t, h, s.url(), oc2PID, 0o600)
				s.set(func() { s.known = map[string]bool{} })
			},
			want: []string{"does not know session " + ocSession, "opencode -s " + ocSession},
		},
		"the wrong password in the file": {
			setup: func(t *testing.T, h string, s *oc2Server) {
				writeService(t, h, s.url(), oc2PID, 0o600)
				s.set(func() { s.password = "rotated" })
			},
			want: []string{"401", "OPENCODE_PASSWORD"},
		},
		"a service that is up and broken": {
			setup: func(t *testing.T, h string, s *oc2Server) {
				writeService(t, h, s.url(), oc2PID, 0o600)
				s.set(func() { s.down = true })
			},
			want: []string{"500"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv := newOC2Server(t)
			stateHome := t.TempDir()
			tc.setup(t, stateHome, srv)
			stateDir := t.TempDir()
			// An irc: stream nothing listens on: opened first, it would be the error.
			spec := fmt.Sprintf("irc://127.0.0.1:%d/probe", closedPort(t))
			res := runParleyEnv(t, oc2Env(t, stateHome), "", append([]string{"opencode"}, oc2Args(spec, "oc-a", ocSession, stateDir)...)...)
			if res.code != 2 {
				t.Fatalf("exited %d, want 2\nstderr: %s", res.code, res.stderr)
			}
			for _, want := range tc.want {
				if !strings.Contains(res.stderr, want) {
					t.Errorf("stderr does not say %q:\n%s", want, res.stderr)
				}
			}
			for _, secret := range []string{oc2Password, "rotated"} {
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
	res := runParleyEnv(t, oc2Env(t, "state"), "", append([]string{"opencode"}, oc2Args(spec, "oc-a", ocSession, t.TempDir())...)...)
	if res.code != 2 || !strings.Contains(res.stderr, "not an absolute path") || !strings.Contains(res.stderr, "--port") {
		t.Fatalf("exit %d, stderr: %s", res.code, res.stderr)
	}
}

// TestOpencodeWithAPortTellsTheVersionByTheAnswer: --port is probed; OpenCode 1's
// health JSON is OpenCode 1, and anything else is asked as OpenCode 2, with the
// password from OPENCODE_PASSWORD, else from the older OPENCODE_SERVER_PASSWORD.
func TestOpencodeWithAPortTellsTheVersionByTheAnswer(t *testing.T) {
	for name, tc := range map[string]struct {
		env      []string
		password string
		wantRun  bool
		want     []string
	}{
		"a server with no password": {password: "", wantRun: true},
		"OPENCODE_PASSWORD":         {env: []string{opencodePassword2Env + "=" + oc2Password}, password: oc2Password, wantRun: true},
		"the older variable":        {env: []string{opencodePasswordEnv + "=" + oc2Password}, password: oc2Password, wantRun: true},
		"the newer one wins": {
			env:      []string{opencodePassword2Env + "=" + oc2Password, opencodePasswordEnv + "=old-and-wrong"},
			password: oc2Password, wantRun: true,
		},
		"no password given": {password: oc2Password, want: []string{"401", opencodePassword2Env, "As OpenCode 1", "As OpenCode 2"}},
		"the wrong password": {
			env:      []string{opencodePassword2Env + "=not-it"},
			password: oc2Password, want: []string{"401", opencodePassword2Env},
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := resolvedTempDir(t)
			spec := "file:" + filepath.Join(dir, "chat.ndjson")
			srv := newOC2Server(t)
			srv.set(func() { srv.password = tc.password })
			env := oc2Env(t, t.TempDir(), tc.env...)
			if tc.wantRun {
				cmd, _, stderr := startOpencode(t, env, ocArgs(spec, "oc-a", srv.port(), ocSession, t.TempDir())...)
				waitForStderr(t, stderr, "into OpenCode session "+ocSession)
				if want := fmt.Sprintf("on 127.0.0.1:%d (OpenCode 2.0.32)", srv.port()); !strings.Contains(stderr.String(), want) {
					t.Fatalf("the start line does not name the server (%s):\n%s", want, stderr.String())
				}
				mustRun(t, "", "send", "--stream", spec, "--nick", "claude-b", "--", "to a 2.x server")
				posts := srv.waitPosts(1, stderr)
				if _, text := oc2PostedText(t, posts[0]); !strings.Contains(text, "to a 2.x server") {
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
			for _, secret := range []string{oc2Password, "not-it", "old-and-wrong"} {
				if strings.Contains(res.stderr, secret) {
					t.Errorf("stderr quotes a password:\n%s", res.stderr)
				}
			}
		})
	}
}

// TestOpencodeWithAPortThatNothingListensOnNamesBothVersions: the one error that
// is either version's names what to run for each.
func TestOpencodeWithAPortThatNothingListensOnNamesBothVersions(t *testing.T) {
	spec := "file:" + filepath.Join(resolvedTempDir(t), "chat.ndjson")
	stateDir := t.TempDir()
	res := runParleyEnv(t, oc2Env(t, t.TempDir()), "", append([]string{"opencode"}, ocArgs(spec, "oc-a", closedPort(t), ocSession, stateDir)...)...)
	if res.code != 2 {
		t.Fatalf("exited %d, want 2\nstderr: %s", res.code, res.stderr)
	}
	for _, want := range []string{"cannot reach OpenCode on 127.0.0.1:", "opencode --port ", "--session " + ocSession, "opencode -s " + ocSession, "without --port"} {
		if !strings.Contains(res.stderr, want) {
			t.Errorf("stderr does not say %q:\n%s", want, res.stderr)
		}
	}
	noLockTaken(t, stateDir, "oc-a")
}

// TestOpencodeWithAPortOfAnOpenCode2ThatDoesNotKnowTheSession: a 2.x server that
// answers, and does not have the session, is told as that and not as a page.
func TestOpencodeWithAPortOfAnOpenCode2ThatDoesNotKnowTheSession(t *testing.T) {
	srv := newOC2Server(t)
	srv.set(func() { srv.password, srv.known = "", map[string]bool{} })
	spec := "file:" + filepath.Join(resolvedTempDir(t), "chat.ndjson")
	res := runParleyEnv(t, oc2Env(t, t.TempDir()), "", append([]string{"opencode"}, ocArgs(spec, "oc-a", srv.port(), ocSession, t.TempDir())...)...)
	if res.code != 2 || !strings.Contains(res.stderr, "OpenCode 2 on 127.0.0.1:") || !strings.Contains(res.stderr, "does not know session "+ocSession) || !strings.Contains(res.stderr, "opencode -s "+ocSession) {
		t.Fatalf("exit %d, stderr: %s", res.code, res.stderr)
	}
}

// TestOpencodeWithNoPortAndNoServiceNamesBothVersions: with no --port the pump
// looks for OpenCode 2's service, and the error for finding none says what starts
// it, and that OpenCode 1 is reached with --port. The environment names the state
// directory, so the developer's own is never looked in.
func TestOpencodeWithNoPortAndNoServiceNamesBothVersions(t *testing.T) {
	spec := "file:" + filepath.Join(resolvedTempDir(t), "chat.ndjson")
	stateDir, stateHome := t.TempDir(), t.TempDir()
	res := runParleyEnv(t, oc2Env(t, stateHome), "", append([]string{"opencode"}, oc2Args(spec, "oc-a", ocSession, stateDir)...)...)
	if res.code != 2 {
		t.Fatalf("exited %d, want 2\nstderr: %s", res.code, res.stderr)
	}
	for _, want := range []string{"no OpenCode 2 service is registered", filepath.Join(stateHome, "opencode", "service.json"), "opencode -s " + ocSession, "pass --port <n>", "opencode --port <n> --session " + ocSession} {
		if !strings.Contains(res.stderr, want) {
			t.Errorf("stderr does not say %q:\n%s", want, res.stderr)
		}
	}
	if strings.Count(res.stderr, "no OpenCode 2 service is registered") != 1 {
		t.Errorf("the refusal says it twice:\n%s", res.stderr)
	}
	noLockTaken(t, stateDir, "oc-a")
}
