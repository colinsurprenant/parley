package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// realErgo is the ergo the live server tests run, or a skip.
func realErgo(t *testing.T) string {
	t.Helper()
	if bin := os.Getenv("PARLEY_ERGO"); bin != "" {
		return bin
	}
	bin, err := exec.LookPath("ergo")
	if err != nil {
		t.Skip("no ergo: set $PARLEY_ERGO or put ergo on PATH (see contrib/ergo/README.md)")
	}
	return bin
}

// fakeErgo writes a stand-in for ergo that records the arguments it was run
// with and its working directory, then does body. It records its pid too, as the
// first thing it does, or where body says @PID@: a body that traps signals says
// so after the trap, so that a test which waits for the pid cannot signal it
// before it is ready. It returns the stand-in's path and the directory the
// records are in.
func fakeErgo(t *testing.T, body string) (bin, records string) {
	t.Helper()
	dir := t.TempDir()
	records = t.TempDir()
	pid := fmt.Sprintf("echo $$ > %q/pid", records)
	pre := ""
	if strings.Contains(body, "@PID@") {
		body = strings.ReplaceAll(body, "@PID@", pid)
	} else {
		pre = pid + "\n"
	}
	script := fmt.Sprintf(`#!/bin/sh
case "$1" in --version) echo ergo-fake; exit 0 ;; esac
pwd -P > %[1]q/cwd
for a in "$@"; do printf '%%s\n' "$a"; done > %[1]q/argv
[ "$1" = run ] && [ "$2" = --conf ] && cp "$3" %[1]q/conf
%[2]s%[3]s
`, records, pre, body)
	bin = filepath.Join(dir, "ergo")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write the fake ergo: %v", err)
	}
	return bin, records
}

func TestServerRefusesAListenThatIsNotLoopback(t *testing.T) {
	bin, _ := fakeErgo(t, "exit 0")
	for _, listen := range []string{"0.0.0.0:6667", ":6667", "192.0.2.1:6667", "example.com:6667", "localhost:6667", "[::]:6667", "127.0.0.1", "127.0.0.1:0", "127.0.0.1:99999", "127.0.0.1:http"} {
		t.Run(listen, func(t *testing.T) {
			ds := filepath.Join(t.TempDir(), "store")
			res := runParley(t, "", "server", "--listen", listen, "--datastore", ds, "--ergo-bin", bin)
			if res.code != exitConfig {
				t.Fatalf("exit %d, want 2\nstderr: %s", res.code, res.stderr)
			}
			if !strings.Contains(res.stderr, "--listen") {
				t.Errorf("stderr does not name --listen: %s", res.stderr)
			}
			if _, err := os.Stat(ds); err == nil {
				t.Error("a refused invocation created the datastore")
			}
		})
	}
}

func TestServerWithoutErgoExits2PointsToTheInstallAndWritesNothing(t *testing.T) {
	ds := filepath.Join(t.TempDir(), "store")
	res := runParley(t, "", "server", "--datastore", ds, "--ergo-bin", filepath.Join(t.TempDir(), "no-ergo"))
	if res.code != exitConfig {
		t.Fatalf("exit %d, want 2\nstderr: %s", res.code, res.stderr)
	}
	if !strings.Contains(res.stderr, "contrib/ergo/README.md") {
		t.Errorf("stderr does not point to the install instructions: %s", res.stderr)
	}
	if _, err := os.Stat(ds); err == nil {
		t.Error("the datastore was created before ergo was found")
	}
}

func TestServerTakesNoArgumentsAndNoEmptyDatastore(t *testing.T) {
	bin, _ := fakeErgo(t, "exit 0")
	if res := runParley(t, "", "server", "--ergo-bin", bin, "extra"); res.code != exitConfig {
		t.Errorf("a stray argument exited %d, want 2", res.code)
	}
	if res := runParley(t, "", "server", "--ergo-bin", bin, "--datastore", ""); res.code != exitConfig {
		t.Errorf("an empty --datastore exited %d, want 2", res.code)
	}
}

// TestServerRunsErgoOnTheRenderedConfigInTheDatastore: the argv, the working
// directory, the absolute datastore path in the file, the listener, the mode of
// a directory it made, the one line at the start, and the status it ends with.
func TestServerRunsErgoOnTheRenderedConfigInTheDatastore(t *testing.T) {
	bin, records := fakeErgo(t, "exit 0")
	root := resolvedTempDir(t)
	// A relative datastore, from a working directory of the test's choosing.
	res := runParleyEnvIn(t, root, nil, "server", "--listen", "127.0.0.1:7788", "--datastore", "rel/store", "--ergo-bin", bin)
	if res.code != 0 {
		t.Fatalf("exit %d\nstderr: %s", res.code, res.stderr)
	}
	ds := filepath.Join(root, "rel", "store")

	argv, _ := os.ReadFile(filepath.Join(records, "argv"))
	got := strings.Split(strings.TrimSpace(string(argv)), "\n")
	if len(got) != 3 || got[0] != "run" || got[1] != "--conf" {
		t.Fatalf("ergo was run with %q, want run --conf <this run's config>", got)
	}
	conf := got[2]
	if ok, _ := filepath.Match(filepath.Join(ds, "ircd-*.yaml"), conf); !ok {
		t.Errorf("ergo's config %q is not a file of this run's own in the datastore", conf)
	}
	if _, err := os.Stat(conf); err == nil {
		t.Errorf("this run's config %s is still there after ergo exited", conf)
	}
	cwd, _ := os.ReadFile(filepath.Join(records, "cwd"))
	if strings.TrimSpace(string(cwd)) != ds {
		t.Errorf("ergo ran in %q, want the datastore %q", strings.TrimSpace(string(cwd)), ds)
	}
	b, err := os.ReadFile(filepath.Join(records, "conf"))
	if err != nil {
		t.Fatalf("ergo was not given a readable config: %v", err)
	}
	if !strings.Contains(string(b), `path: "`+filepath.Join(ds, "ergo.db")+`"`) {
		t.Errorf("the datastore path in the config is not the absolute one:\n%s", b)
	}
	if !strings.Contains(string(b), `"127.0.0.1:7788": {}`) || strings.Contains(string(b), "6667") {
		t.Errorf("the listener in the config is not --listen's:\n%s", b)
	}
	if fi, err := os.Stat(ds); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("the datastore it made has mode %v (%v), want 0700", fi.Mode().Perm(), err)
	}
	first, _, _ := strings.Cut(res.stderr, "\n")
	if !strings.Contains(first, "127.0.0.1:7788") || !strings.Contains(first, ds) {
		t.Errorf("the first stderr line %q names neither the address nor the datastore", first)
	}
}

func TestServerLeavesAnExistingDatastoreItsMode(t *testing.T) {
	bin, _ := fakeErgo(t, "exit 0")
	ds := filepath.Join(resolvedTempDir(t), "store")
	if err := os.Mkdir(ds, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ds, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ds, "ergo.db"), []byte("accounts"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := runParley(t, "", "server", "--datastore", ds, "--ergo-bin", bin)
	if res.code != 0 {
		t.Fatalf("exit %d\nstderr: %s", res.code, res.stderr)
	}
	if fi, err := os.Stat(ds); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("the existing datastore is now mode %v (%v), want it left at 0755", fi.Mode().Perm(), err)
	}
	if b, _ := os.ReadFile(filepath.Join(ds, "ergo.db")); string(b) != "accounts" {
		t.Errorf("the existing account database was touched: %q", b)
	}
}

func TestServerDefaultDatastoreIsXDGDataHomeThenHome(t *testing.T) {
	bin, records := fakeErgo(t, "exit 0")
	xdg, home := resolvedTempDir(t), resolvedTempDir(t)
	cwd := func() string {
		b, _ := os.ReadFile(filepath.Join(records, "cwd"))
		return strings.TrimSpace(string(b))
	}
	if res := runParleyEnvIn(t, "", []string{"XDG_DATA_HOME=" + xdg, "HOME=" + home}, "server", "--ergo-bin", bin); res.code != 0 {
		t.Fatalf("exit %d\nstderr: %s", res.code, res.stderr)
	}
	if got := cwd(); got != filepath.Join(xdg, "parley-ergo") {
		t.Errorf("with XDG_DATA_HOME set ergo ran in %q, want $XDG_DATA_HOME/parley-ergo", got)
	}
	if _, err := os.Stat(filepath.Join(home, ".local")); err == nil {
		t.Error("HOME was written to though XDG_DATA_HOME was set")
	}

	if res := runParleyEnvIn(t, "", []string{"XDG_DATA_HOME=", "HOME=" + home}, "server", "--ergo-bin", bin); res.code != 0 {
		t.Fatalf("exit %d\nstderr: %s", res.code, res.stderr)
	}
	if got := cwd(); got != filepath.Join(home, ".local", "share", "parley-ergo") {
		t.Errorf("without XDG_DATA_HOME ergo ran in %q, want ~/.local/share/parley-ergo", got)
	}
}

func TestServerExitsWithErgosStatus(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"zero", "exit 0", 0},
		{"non-zero", "echo 'cannot bind' >&2; exit 7", 7},
		{"killed by a signal nobody here sent", "kill -KILL $$", 128 + int(syscall.SIGKILL)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin, _ := fakeErgo(t, tc.body)
			res := runParley(t, "", "server", "--datastore", filepath.Join(t.TempDir(), "store"), "--ergo-bin", bin)
			if res.code != tc.want {
				t.Fatalf("exit %d, want %d\nstderr: %s", res.code, tc.want, res.stderr)
			}
			if strings.Contains(tc.body, "cannot bind") && !strings.Contains(res.stderr, "cannot bind") {
				t.Errorf("ergo's output did not pass through: %s", res.stderr)
			}
		})
	}
}

// startServer runs `parley server` in the background and returns it with its
// stderr, once ergo has written its pid.
func startServer(t *testing.T, records string, args ...string) (*exec.Cmd, *syncBuffer, <-chan error) {
	t.Helper()
	cmd := exec.Command(binary(t), append([]string{"server"}, args...)...)
	stderr := &syncBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start parley server: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	done := joinCmd(cmd)
	deadline := time.Now().Add(childDeadline)
	for {
		if b, err := os.ReadFile(filepath.Join(records, "pid")); err == nil && strings.HasSuffix(string(b), "\n") {
			return cmd, stderr, done
		}
		if time.Now().After(deadline) {
			t.Fatalf("ergo was never started; stderr:\n%s", stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func ergoPid(t *testing.T, records string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(records, "pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

// gone reports whether pid is no longer a process, waiting a moment for it.
func gone(pid int) bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// TestServerForwardsInterruptAndTerminateAndLeavesNoErgo drives both signals
// through a stand-in that handles the signal and one that is simply killed by it.
func TestServerForwardsInterruptAndTerminateAndLeavesNoErgo(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		for name, body := range map[string]string{
			"ergo handles it":     "trap 'exit 0' INT TERM\n@PID@\nwhile :; do sleep 0.05; done",
			"ergo dies of it":     "exec sleep 1000",
			"ergo exits non-zero": "trap 'exit 5' INT TERM\n@PID@\nwhile :; do sleep 0.05; done",
		} {
			t.Run(sig.String()+"/"+name, func(t *testing.T) {
				bin, records := fakeErgo(t, body)
				cmd, stderr, done := startServer(t, records, "--datastore", filepath.Join(t.TempDir(), "store"), "--ergo-bin", bin)
				pid := ergoPid(t, records)
				if err := cmd.Process.Signal(sig); err != nil {
					t.Fatalf("signal parley: %v", err)
				}
				var err error
				select {
				case err = <-done:
				case <-time.After(childDeadline):
					t.Fatalf("parley server did not exit after %v; stderr:\n%s", sig, stderr.String())
				}
				want := 0
				if strings.Contains(name, "non-zero") {
					want = 5
				}
				got := 0
				var exit *exec.ExitError
				if errors.As(err, &exit) {
					got = exit.ExitCode()
				}
				if got != want {
					t.Errorf("exit %d, want %d\nstderr: %s", got, want, stderr.String())
				}
				if !gone(pid) {
					t.Errorf("ergo (pid %d) is still running after parley server exited", pid)
				}
			})
		}
	}
}

// TestServerHangupStopsErgoWithTerminateAndExits0: a hangup is what a closed
// terminal sends, and ergo takes SIGHUP for a rehash. parley turns it into a
// terminate, waits for ergo to be gone, and exits 0; ergo never sees the hangup.
func TestServerHangupStopsErgoWithTerminateAndExits0(t *testing.T) {
	hup := filepath.Join(t.TempDir(), "hup")
	bin, records := fakeErgo(t, "trap 'echo hup > "+hup+"' HUP\ntrap 'exit 0' INT TERM\n@PID@\nwhile :; do sleep 0.05; done")
	cmd, stderr, done := startServer(t, records, "--datastore", filepath.Join(t.TempDir(), "store"), "--ergo-bin", bin)
	pid := ergoPid(t, records)
	// However this test ends, no stand-in is left running.
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatalf("signal parley: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("parley server after SIGHUP: %v\nstderr: %s", err, stderr.String())
		}
	case <-time.After(childDeadline):
		t.Fatalf("parley server did not exit after SIGHUP; stderr:\n%s", stderr.String())
	}
	if !gone(pid) {
		t.Errorf("ergo (pid %d) is still running after parley server exited", pid)
	}
	if _, err := os.Stat(hup); err == nil {
		t.Error("ergo was sent the hangup, which it takes for a rehash")
	}
}

// TestServerOnlyTheFirstForwardedSignalDecidesTheStatus: ergo ignores the
// interrupt and is killed by the terminate that follows it. The first signal
// parley forwarded was the interrupt, so ergo's death by terminate is not the
// stop that was asked for, and the status says so rather than 0.
func TestServerOnlyTheFirstForwardedSignalDecidesTheStatus(t *testing.T) {
	bin, records := fakeErgo(t, "trap '' INT\n@PID@\nwhile :; do sleep 0.05; done")
	cmd, stderr, done := startServer(t, records, "--datastore", filepath.Join(t.TempDir(), "store"), "--ergo-bin", bin)
	pid := ergoPid(t, records)
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	// Long enough for parley to have forwarded the first before the second comes.
	time.Sleep(300 * time.Millisecond)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	var err error
	select {
	case err = <-done:
	case <-time.After(childDeadline):
		t.Fatalf("parley server did not exit; stderr:\n%s", stderr.String())
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 128+int(syscall.SIGTERM) {
		t.Errorf("parley server ended with %v, want exit status %d: ergo died of a signal that was not the first one forwarded\nstderr: %s", err, 128+int(syscall.SIGTERM), stderr.String())
	}
	if !gone(pid) {
		t.Errorf("ergo (pid %d) is still running after parley server exited", pid)
	}
}

// TestServerRunsARealErgoAndAParleyPairTalksThroughIt: the acceptance path.
// send and wait go through the server parley started, the config it wrote has
// the absolute datastore, the server stops on SIGINT with status 0, and no ergo
// is left.
func TestServerRunsARealErgoAndAParleyPairTalksThroughIt(t *testing.T) {
	real := realErgo(t)
	dir := resolvedTempDir(t)
	records := t.TempDir()
	bin := filepath.Join(dir, "ergo-wrapper")
	wrapper := fmt.Sprintf("#!/bin/sh\necho $$ > %q/pid\nexec %q \"$@\"\n", records, real)
	if err := os.WriteFile(bin, []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	addr := freeAddr(t)
	ds := filepath.Join(dir, "store")
	cmd, stderr, done := startServer(t, records, "--listen", addr, "--datastore", ds, "--ergo-bin", bin)
	pid := ergoPid(t, records)
	waitListening(t, addr, stderr)

	confs, _ := filepath.Glob(filepath.Join(ds, "ircd-*.yaml"))
	if len(confs) != 1 {
		t.Fatalf("want this run's one config in the datastore, found %q", confs)
	}
	conf, err := os.ReadFile(confs[0])
	if err != nil {
		t.Fatalf("read the config: %v", err)
	}
	if !strings.Contains(string(conf), `path: "`+filepath.Join(ds, "ergo.db")+`"`) {
		t.Errorf("the datastore path in the written config is not the absolute one:\n%s", conf)
	}

	chat := "irc://" + addr + "/server"
	state := t.TempDir()
	sent := mustRun(t, "", "send", "--stream", chat, "--nick", "alpha", "--state-dir", state, "--", "through parley server")
	id, _ := receiptFields(t, sent)
	got := mustRun(t, "", "wait", "--stream", chat, "--nick", "beta", "--state-dir", state, "--timeout", "10s", "--debounce", "100ms")
	if wid, _ := headerFields(t, got.stdout, "alpha"); wid != id || !strings.Contains(got.stdout, "through parley server") {
		t.Fatalf("beta did not get alpha's message (sent id %s):\n%s", id, got.stdout)
	}

	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("parley server after SIGINT: %v\nstderr: %s", err, stderr.String())
		}
	case <-time.After(childDeadline):
		t.Fatalf("parley server did not exit after SIGINT; stderr:\n%s", stderr.String())
	}
	if !gone(pid) {
		t.Errorf("ergo (pid %d) is still running after parley server exited", pid)
	}
	if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		c.Close()
		t.Errorf("%s still accepts connections after the server stopped", addr)
	}
}

func waitListening(t *testing.T, addr string, stderr *syncBuffer) {
	t.Helper()
	deadline := time.Now().Add(childDeadline)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
			c.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("nothing listened on %s in time; parley server stderr:\n%s", addr, stderr.String())
}

// TestServerRunsAnErgoNamedByARelativePath: ergo runs in the datastore, so a
// binary named relative to the working directory the user typed it in has to be
// made absolute before the working directory changes.
func TestServerRunsAnErgoNamedByARelativePath(t *testing.T) {
	bin, records := fakeErgo(t, "exit 0")
	res := runParleyEnvIn(t, filepath.Dir(bin), nil, "server", "--ergo-bin", "./ergo", "--datastore", filepath.Join(t.TempDir(), "store"))
	if res.code != 0 {
		t.Fatalf("exit %d\nstderr: %s", res.code, res.stderr)
	}
	if _, err := os.Stat(filepath.Join(records, "argv")); err != nil {
		t.Errorf("ergo never ran: %v", err)
	}
}

// TestServerDatastoreDefaultKeepsTheAccountsWhereTheyWere: with $XDG_DATA_HOME
// set, the directory contrib/ergo/README.md always named is still used while it
// exists and the XDG one does not, so the accounts made there carry over.
func TestServerDatastoreDefaultKeepsTheAccountsWhereTheyWere(t *testing.T) {
	home := t.TempDir()
	xdg := filepath.Join(home, "xdg")
	legacy := filepath.Join(home, ".local", "share", "parley-ergo")
	t.Setenv("HOME", home)

	t.Setenv("XDG_DATA_HOME", "")
	if got, err := datastoreDir(""); err != nil || got != legacy {
		t.Fatalf("no XDG_DATA_HOME: got %q, %v; want %q", got, err, legacy)
	}

	t.Setenv("XDG_DATA_HOME", xdg)
	if got, _ := datastoreDir(""); got != filepath.Join(xdg, "parley-ergo") {
		t.Errorf("neither directory there: got %q, want the XDG one", got)
	}
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	if got, _ := datastoreDir(""); got != legacy {
		t.Errorf("only the old directory there: got %q, want %q", got, legacy)
	}
	if err := os.MkdirAll(filepath.Join(xdg, "parley-ergo"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got, _ := datastoreDir(""); got != filepath.Join(xdg, "parley-ergo") {
		t.Errorf("both there: got %q, want the XDG one", got)
	}
	if got, _ := datastoreDir("rel"); !filepath.IsAbs(got) {
		t.Errorf("a --datastore value is made absolute, got %q", got)
	}
}

// TestServerDatastoreNeedsNoHomeWhenXDGNamesIt: $XDG_DATA_HOME alone fixes the
// default, so a missing $HOME is no failure there, only with no XDG either.
func TestServerDatastoreNeedsNoHomeWhenXDGNamesIt(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("HOME", "")
	t.Setenv("XDG_DATA_HOME", xdg)
	if got, err := datastoreDir(""); err != nil || got != filepath.Join(xdg, "parley-ergo") {
		t.Fatalf("got %q, %v; want the XDG datastore with no $HOME", got, err)
	}
	t.Setenv("XDG_DATA_HOME", "")
	if _, err := datastoreDir(""); err == nil {
		t.Error("with neither $HOME nor $XDG_DATA_HOME there is no default, yet no error")
	}
}

// fixture reads one of ergo's refusals to start as it was captured from the real
// ergo 2.19.1 (the temporary directory in its paths is replaced by /store), and
// the absolute path it is read from, which a stand-in ergo cats to its stderr.
func fixture(t *testing.T, name string) (text, path string) {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the captured ergo output: %v", err)
	}
	return string(b), path
}

// TestTheFixturesCarryTheTextsTheServerMatchesOn: the captured lines are the
// real ones, so a change to the texts parley matches on is a change to what it
// was measured against.
func TestTheFixturesCarryTheTextsTheServerMatchesOn(t *testing.T) {
	for name, text := range map[string]string{"ergo-flock.txt": ergoFlockText, "ergo-bind.txt": ergoBindText} {
		got, _ := fixture(t, name)
		if !strings.Contains(got, text) {
			t.Errorf("testdata/%s does not hold %q:\n%s", name, text, got)
		}
	}
}

// TestServerAddsOneLineAfterErgoRefusesToStart: for each of the two refusals a
// second server meets, ergo's own output passes through unchanged and one line
// follows it, naming what to look at; nothing else earns that line.
func TestServerAddsOneLineAfterErgoRefusesToStart(t *testing.T) {
	flock, flockPath := fixture(t, "ergo-flock.txt")
	bind, bindPath := fixture(t, "ergo-bind.txt")
	const addr = "127.0.0.1:7788"
	ds := filepath.Join(resolvedTempDir(t), "store")
	flockHint := []string{"another ergo is using this datastore", ds, "pgrep -l ergo"}
	bindHint := []string{"something already listens there, possibly another parley server", addr, "pgrep -l ergo"}

	for _, tc := range []struct {
		name, body string
		out        string   // what ergo said, to be found whole in stderr
		hint       []string // the one line that follows, or nil for none
		code       int
		onStdout   bool // ergo said it on stdout, so the line is not beside it
	}{
		{"the flock text", "cat " + flockPath + " >&2; exit 1", flock, flockHint, 1, false},
		{"the bind text", "cat " + bindPath + " >&2; exit 1", bind, bindHint, 1, false},
		{"the flock text on stdout", "cat " + flockPath + "; exit 1", flock, flockHint, 1, true},
		{"a text split across two writes", `printf "Couldn't acquire fl" >&2; sleep 0.2; printf "ock (is another Ergo running?)\n" >&2; exit 1`, ergoFlockText + "\n", flockHint, 1, false},
		{"a text with no newline after it", `printf "%s" "listen tcp: bind: address already in use" >&2; exit 1`, "address already in use", bindHint, 1, false},
		{"another failure", "echo 'cannot bind' >&2; exit 1", "cannot bind\n", nil, 1, false},
		{"the text of a server that then exited 0", "cat " + flockPath + " >&2; exit 0", flock, nil, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin, _ := fakeErgo(t, tc.body)
			res := runParley(t, "", "server", "--listen", addr, "--datastore", ds, "--ergo-bin", bin)
			if res.code != tc.code {
				t.Fatalf("exit %d, want %d\nstderr: %s", res.code, tc.code, res.stderr)
			}
			all := res.stdout + res.stderr
			at := strings.Index(all, tc.out)
			if at < 0 {
				t.Fatalf("ergo's output did not pass through unchanged; want %q in:\n%s", tc.out, all)
			}
			want := 0
			if tc.hint != nil {
				want = 1
			}
			if n := strings.Count(all, "pgrep -l ergo"); n != want {
				t.Fatalf("%d lines point at pgrep, want %d\n%s", n, want, all)
			}
			if tc.hint == nil {
				return
			}
			var line string
			for _, l := range strings.Split(res.stderr, "\n") {
				if strings.Contains(l, "pgrep -l ergo") {
					line = l
				}
			}
			if !strings.HasPrefix(line, "parley server: ") {
				t.Fatalf("the pgrep line is %q, want a parley server line", line)
			}
			// On stderr, the line comes straight after what ergo said and before
			// parley's own status line.
			if !tc.onStdout {
				after := strings.TrimPrefix(all[at+len(tc.out):], "\n")
				if !strings.HasPrefix(after, line+"\nparley: ergo exited with status 1") {
					t.Errorf("the hint does not follow ergo's output and precede the status line:\n%s", after)
				}
			}
			for _, want := range tc.hint {
				if !strings.Contains(line, want) {
					t.Errorf("the line %q does not hold %q", line, want)
				}
			}
		})
	}
}

// pidWrapper is a stand-in for the real ergo that records its pid and becomes
// it, so a test can signal the server and only the server.
func pidWrapper(t *testing.T, real string) (bin, records string) {
	t.Helper()
	records = t.TempDir()
	bin = filepath.Join(resolvedTempDir(t), "ergo-wrapper")
	wrapper := fmt.Sprintf("#!/bin/sh\necho $$ > %q/pid\nexec %q \"$@\"\n", records, real)
	if err := os.WriteFile(bin, []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, records
}

// stopServer interrupts a running `parley server` and waits for it.
func stopServer(t *testing.T, cmd *exec.Cmd, stderr *syncBuffer, done <-chan error) {
	t.Helper()
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("parley server after SIGINT: %v\nstderr: %s", err, stderr.String())
		}
	case <-time.After(childDeadline):
		t.Fatalf("parley server did not exit after SIGINT; stderr:\n%s", stderr.String())
	}
}

// TestASecondServerOnOneDatastoreOrOnePortIsRefusedWithAPointer: against the
// installed ergo. Two servers on one datastore and two ports both used to start
// and share one ergo.db; now the second ergo cannot take the datastore's lock
// and says so, and a second server on a taken port says that, each with the
// line `parley server` adds. The first server is untouched by either.
func TestASecondServerOnOneDatastoreOrOnePortIsRefusedWithAPointer(t *testing.T) {
	real := realErgo(t)
	bin, records := pidWrapper(t, real)
	ds := filepath.Join(resolvedTempDir(t), "store")
	first := freeAddr(t)
	cmd, stderr, done := startServer(t, records, "--listen", first, "--datastore", ds, "--ergo-bin", bin)
	waitListening(t, first, stderr)

	other := freeAddr(t)
	res := runParleyWithin(t, childDeadline, "server", "--listen", other, "--datastore", ds, "--ergo-bin", real)
	if res.code != 1 {
		t.Errorf("a second server on the datastore exited %d, want 1\nstderr: %s", res.code, res.stderr)
	}
	for _, want := range []string{ergoFlockText, "another ergo is using this datastore (" + ds + ")", "pgrep -l ergo"} {
		if !strings.Contains(res.stderr, want) {
			t.Errorf("a second server on the datastore did not print %q:\n%s", want, res.stderr)
		}
	}
	if c, err := net.DialTimeout("tcp", other, 300*time.Millisecond); err == nil {
		c.Close()
		t.Errorf("the second server is listening on %s", other)
	}

	res = runParleyWithin(t, childDeadline, "server", "--listen", first, "--datastore", filepath.Join(filepath.Dir(ds), "store2"), "--ergo-bin", real)
	if res.code != 1 {
		t.Errorf("a second server on the port exited %d, want 1\nstderr: %s", res.code, res.stderr)
	}
	for _, want := range []string{ergoBindText, first + ": something already listens there, possibly another parley server", "pgrep -l ergo"} {
		if !strings.Contains(res.stderr, want) {
			t.Errorf("a second server on the port did not print %q:\n%s", want, res.stderr)
		}
	}
	if strings.Contains(res.stderr, "another ergo is using this datastore") {
		t.Errorf("a server on a datastore of its own was told the datastore is taken:\n%s", res.stderr)
	}

	waitListening(t, first, stderr)
	stopServer(t, cmd, stderr, done)
}

// TestALockLeftByAKilledErgoDoesNotBlockTheNextStart: the lock is a flock, which
// dies with the process; the file it was taken on stays behind, and the next
// server on the datastore starts regardless.
func TestALockLeftByAKilledErgoDoesNotBlockTheNextStart(t *testing.T) {
	real := realErgo(t)
	bin, records := pidWrapper(t, real)
	ds := filepath.Join(resolvedTempDir(t), "store")
	first := freeAddr(t)
	cmd, stderr, done := startServer(t, records, "--listen", first, "--datastore", ds, "--ergo-bin", bin)
	pid := ergoPid(t, records)
	waitListening(t, first, stderr)
	lock := filepath.Join(ds, "ircd.lock")
	if _, err := os.Stat(lock); err != nil {
		t.Fatalf("a running server holds no lock file in its datastore: %v", err)
	}

	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Error("parley server exited 0 after its ergo was killed")
		}
	case <-time.After(childDeadline):
		_ = cmd.Process.Kill()
		t.Fatalf("parley server did not exit after its ergo was killed; stderr:\n%s", stderr.String())
	}
	if _, err := os.Stat(lock); err != nil {
		t.Fatalf("the killed ergo left no lock file, so this test proves nothing: %v", err)
	}

	next := freeAddr(t)
	if err := os.Remove(filepath.Join(records, "pid")); err != nil {
		t.Fatal(err)
	}
	cmd, stderr, done = startServer(t, records, "--listen", next, "--datastore", ds, "--ergo-bin", bin)
	waitListening(t, next, stderr)
	stopServer(t, cmd, stderr, done)
}
