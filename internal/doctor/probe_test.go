package doctor

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// processGone is whether the process is not there: no such process, or a zombie,
// which is what a killed orphan is until init reaps it (and for good in a
// container with no init), and which kill 0 still finds.
func processGone(pid int) bool {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return true
	}
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	return err != nil || strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}

// launcherWithChild installs a stand-in for a harness's CLI that is a launcher, as
// an npm install's is: it runs a long sleep as a child, writes the child's pid, and
// then does what body says. The child holds the probe's pipes.
func launcherWithChild(t *testing.T, body string) (bin string, pid func() int) {
	t.Helper()
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child-pid")
	bin = filepath.Join(dir, "launcher")
	script := "#!/bin/sh\nsleep 300 &\necho $! > " + strconv.Quote(pidFile) + "\n" + body + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, func() int {
		t.Helper()
		var raw []byte
		for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			raw, _ = os.ReadFile(pidFile)
			if strings.HasSuffix(string(raw), "\n") {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the launcher never wrote its child's pid")
			}
		}
		n, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = syscall.Kill(n, syscall.SIGKILL) })
		return n
	}
}

func waitGone(t *testing.T, pid int, what string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !processGone(pid); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%s (process %d) is still there", what, pid)
		}
	}
}

// TestAProbeThatHangsTakesItsWholeProcessGroupAtTheTimeout: a launcher whose child
// hangs on --version is stopped with the child at the probe's bound, and not the
// launcher alone, which would leave the child running.
func TestAProbeThatHangsTakesItsWholeProcessGroupAtTheTimeout(t *testing.T) {
	bin, childPID := launcherWithChild(t, "wait")
	start := time.Now()
	_, _, err := probeFor(context.Background(), 300*time.Millisecond, bin, "--version")
	if err == nil {
		t.Fatal("a probe that hung returned no error")
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("the probe took %s against a 300ms bound", took)
	}
	waitGone(t, childPID(), "the child of the hung launcher")
}

// TestAProbeWhoseLauncherLeftAChildHoldingThePipesKillsIt: a launcher that exits at
// once while a child still holds the pipes makes Run return at the wait delay
// without the cancellation having run, so the group is taken after it.
func TestAProbeWhoseLauncherLeftAChildHoldingThePipesKillsIt(t *testing.T) {
	bin, childPID := launcherWithChild(t, "exit 0")
	_, _, err := probeFor(context.Background(), 20*time.Second, bin, "--version")
	if err == nil {
		t.Fatal("a probe whose pipes a child held returned no error, though the wait delay expired")
	}
	waitGone(t, childPID(), "the child the launcher left")
}

// TestACanceledProbeTakesItsWholeProcessGroup: the human's interrupt ends a probe
// the same way its bound does.
func TestACanceledProbeTakesItsWholeProcessGroup(t *testing.T) {
	bin, childPID := launcherWithChild(t, "wait")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, _, err := probe(ctx, bin, "--version"); done <- err }()
	pid := childPID()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a canceled probe returned no error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a canceled probe did not return")
	}
	waitGone(t, pid, "the child of the canceled launcher")
}
