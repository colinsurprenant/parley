package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runInstall runs skills/install.sh against a HOME of the test's own, so nothing
// of the developer's is read or touched, with PATH as given.
func runInstall(t *testing.T, home, path string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	script, err := filepath.Abs(filepath.Join("..", "..", "skills", "install.sh"))
	if err != nil {
		t.Fatalf("locate install.sh: %v", err)
	}
	cmd := exec.Command("sh", append([]string{script}, args...)...)
	cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+path)
	var out, errs strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errs
	if err := cmd.Run(); err != nil {
		exit, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run install.sh: %v", err)
		}
		code = exit.ExitCode()
	}
	return out.String(), errs.String(), code
}

func skillLinks(home string) []string {
	var out []string
	for _, root := range []string{".claude/skills", ".agents/skills"} {
		for _, name := range []string{"parley-build", "parley-table", "parley-line"} {
			out = append(out, filepath.Join(home, root, name))
		}
	}
	return out
}

func isLink(path string) bool {
	fi, err := os.Lstat(path)
	return err == nil && fi.Mode()&os.ModeSymlink != 0
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// TestInstallLinksAndUninstallsOnlyWhatIsOurs: a plain install, a repeat, a
// foreign path in the way, and an uninstall that leaves the foreign path alone.
func TestInstallLinksAndUninstallsOnlyWhatIsOurs(t *testing.T) {
	home := t.TempDir()
	path := os.Getenv("PATH")

	if _, stderr, code := runInstall(t, home, path); code != 0 {
		t.Fatalf("install exited %d\nstderr: %s", code, stderr)
	}
	for _, l := range skillLinks(home) {
		if !isLink(l) {
			t.Fatalf("%s is not a link after the install", l)
		}
	}
	if out, _, code := runInstall(t, home, path); code != 0 || strings.Count(out, "already linked:") != 6 {
		t.Fatalf("a second install exited %d and printed %q, want 6 already-linked lines", code, out)
	}

	// A foreign directory at one destination: uninstall removes the other five
	// links, reports the foreign path, leaves it, and exits 1.
	foreign := filepath.Join(home, ".agents", "skills", "parley-build")
	if err := os.Remove(foreign); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.Mkdir(foreign, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, stderr, code := runInstall(t, home, path); code != 1 || !strings.Contains(stderr, "leaving it alone") {
		t.Fatalf("an install over a foreign path exited %d with stderr %q, want 1 and a refusal", code, stderr)
	}
	_, stderr, code := runInstall(t, home, path, "--uninstall")
	if code != 1 || !strings.Contains(stderr, "leaving it alone") {
		t.Fatalf("an uninstall beside a foreign path exited %d with stderr %q, want 1 and a refusal", code, stderr)
	}
	for _, l := range skillLinks(home) {
		if l == foreign {
			if fi, err := os.Lstat(l); err != nil || !fi.IsDir() {
				t.Fatalf("the foreign directory %s was disturbed (err %v)", l, err)
			}
			continue
		}
		if exists(l) {
			t.Fatalf("%s is still there after the uninstall", l)
		}
	}
}

// TestInstallRollbackLeavesAPathItNoLongerOwns: linking fails after the first
// links are made — ~/.agents cannot be made a directory — and the rollback takes
// the links this run made back out, except one that somebody has replaced in the
// meantime, which is theirs. A mkdir that swaps one link for a directory of
// somebody's before it fails is that somebody.
func TestInstallRollbackLeavesAPathItNoLongerOwns(t *testing.T) {
	home := t.TempDir()
	realMkdir, err := exec.LookPath("mkdir")
	if err != nil {
		t.Skip("no mkdir on PATH")
	}
	shims := t.TempDir()
	shim := `#!/bin/sh
# Fails making the agents skills root, having first replaced the Claude Code link
# for parley-build with a directory that is not the script's.
case "$*" in
*.agents/skills*)
	rm -- "$HOME/.claude/skills/parley-build"
	` + realMkdir + ` -- "$HOME/.claude/skills/parley-build"
	: > "$HOME/.claude/skills/parley-build/mine"
	exit 1
	;;
esac
exec ` + realMkdir + ` "$@"
`
	if err := os.WriteFile(filepath.Join(shims, "mkdir"), []byte(shim), 0o755); err != nil {
		t.Fatalf("write the shim: %v", err)
	}
	_, stderr, code := runInstall(t, home, shims+string(os.PathListSeparator)+os.Getenv("PATH"))
	if code != 1 || !strings.Contains(stderr, "could not link") {
		t.Fatalf("install exited %d with stderr %q, want 1 and the failure", code, stderr)
	}
	mine := filepath.Join(home, ".claude", "skills", "parley-build")
	if fi, err := os.Lstat(filepath.Join(mine, "mine")); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("the rollback removed or replaced the path it no longer owned (err %v); stderr:\n%s", err, stderr)
	}
	if !strings.Contains(stderr, "no longer a link") || strings.Contains(stderr, "nothing from this run is left behind") {
		t.Fatalf("stderr does not say the replaced path was left:\n%s", stderr)
	}
	// What was still ours is gone: the other links this run made.
	for _, name := range []string{"parley-table", "parley-line"} {
		if exists(filepath.Join(home, ".claude", "skills", name)) {
			t.Fatalf("the rollback left the %s link it still owned; stderr:\n%s", name, stderr)
		}
	}
}

// TestInstallRollbackTakesOutEverythingItMade: the ordinary rollback, with
// nothing replaced, leaves none of the links and says so.
func TestInstallRollbackTakesOutEverythingItMade(t *testing.T) {
	home := t.TempDir()
	// ~/.agents is a file, so ~/.agents/skills cannot be made.
	if err := os.WriteFile(filepath.Join(home, ".agents"), nil, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, stderr, code := runInstall(t, home, os.Getenv("PATH"))
	if code != 1 || !strings.Contains(stderr, "nothing from this run is left behind") {
		t.Fatalf("install exited %d with stderr %q, want 1 and nothing left behind", code, stderr)
	}
	for _, l := range skillLinks(home) {
		if exists(l) {
			t.Fatalf("%s is left after the rollback", l)
		}
	}
}

// checkoutSkills is the skills directory of the checkout the tests run in, the
// one install.sh links from.
func checkoutSkills(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "skills"))
	if err != nil {
		t.Fatalf("locate skills: %v", err)
	}
	return dir
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
}

// TestInstallRemovesTheStaleSelfLink: before parley-build the two-session skill
// was linked as parley. skills/parley is gone from this checkout, so the old link
// dangles; install removes it under both roots, whether it was made with an
// absolute target or a relative one, and links the new names.
func TestInstallRemovesTheStaleSelfLink(t *testing.T) {
	// The relative link below counts its ../ segments from the physical path of
	// its own directory, so the home must not be reached through a symlink (macOS
	// keeps /var/folders behind one).
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve the home: %v", err)
	}
	skills := checkoutSkills(t)
	claudeStale := filepath.Join(home, ".claude", "skills", "parley")
	agentsStale := filepath.Join(home, ".agents", "skills", "parley")
	mustSymlink(t, filepath.Join(skills, "parley"), claudeStale)
	rel, err := filepath.Rel(filepath.Dir(agentsStale), filepath.Join(skills, "parley"))
	if err != nil {
		t.Fatalf("rel: %v", err)
	}
	mustSymlink(t, rel, agentsStale)

	out, stderr, code := runInstall(t, home, os.Getenv("PATH"))
	if code != 0 || stderr != "" {
		t.Fatalf("install exited %d with stderr %q, want 0 and none", code, stderr)
	}
	for _, stale := range []string{claudeStale, agentsStale} {
		if exists(stale) {
			t.Fatalf("%s is still there after the install", stale)
		}
		if !strings.Contains(out, "removed stale: "+stale) {
			t.Fatalf("the install did not say it removed %s:\n%s", stale, out)
		}
	}
	for _, l := range skillLinks(home) {
		if !isLink(l) {
			t.Fatalf("%s is not a link after the install", l)
		}
	}
	// With the stale links gone a second install has nothing to remove.
	if out, stderr, code := runInstall(t, home, os.Getenv("PATH")); code != 0 || stderr != "" || strings.Contains(out, "stale") {
		t.Fatalf("a second install exited %d, stdout %q, stderr %q, want 0 and nothing about a stale link", code, out, stderr)
	}
}

// TestInstallKeepsWhatIsNotItsStaleLink: a parley entry that is not a link into
// this checkout is somebody's. A real directory and a link to anywhere else, one
// that resolves and one that dangles, are each left alone and named in one
// warning line, and the install is otherwise complete.
func TestInstallKeepsWhatIsNotItsStaleLink(t *testing.T) {
	home := t.TempDir()
	elsewhere := t.TempDir()
	if err := os.WriteFile(filepath.Join(elsewhere, "SKILL.md"), []byte("theirs\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	dir := filepath.Join(home, ".claude", "skills", "parley")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mine"), nil, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	foreign := filepath.Join(home, ".agents", "skills", "parley")
	mustSymlink(t, elsewhere, foreign)

	for _, tc := range []struct {
		name   string
		target string
	}{
		{"a live link elsewhere", elsewhere},
		{"a dangling link elsewhere", filepath.Join(elsewhere, "gone", "parley")},
		{"a link to the checkout above the skills directory", filepath.Dir(checkoutSkills(t))},
		{"a link ending in dot-dot", checkoutSkills(t) + "/.."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.Remove(foreign); err != nil {
				t.Fatalf("remove: %v", err)
			}
			mustSymlink(t, tc.target, foreign)
			out, stderr, code := runInstall(t, home, os.Getenv("PATH"))
			if code != 0 {
				t.Fatalf("install exited %d with stderr %q, want 0", code, stderr)
			}
			if strings.Contains(out, "stale") {
				t.Fatalf("the install removed something it should not have:\n%s", out)
			}
			for _, kept := range []string{dir, foreign} {
				if got := strings.Count(stderr, kept+" is not a link into "); got != 1 {
					t.Fatalf("stderr names %s %d times, want once:\n%s", kept, got, stderr)
				}
			}
			if strings.Count(stderr, "\n") != 2 {
				t.Fatalf("stderr is not one warning line per kept path:\n%s", stderr)
			}
			if fi, err := os.Lstat(filepath.Join(dir, "mine")); err != nil || !fi.Mode().IsRegular() {
				t.Fatalf("the directory's contents were disturbed (err %v)", err)
			}
			if got, err := os.Readlink(foreign); err != nil || got != tc.target {
				t.Fatalf("the foreign link is now %q (err %v), want it left at %q", got, err, tc.target)
			}
			for _, l := range skillLinks(home) {
				if !isLink(l) {
					t.Fatalf("%s is not a link after the install", l)
				}
			}
		})
	}
}
