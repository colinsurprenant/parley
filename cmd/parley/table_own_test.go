package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"testing"

	"github.com/colinsurprenant/parley/internal/state"
)

// These are the tests of what `table end` and the rollback of `table new` will
// delete: only what they can show is the table's.

// recordPath is table id's record.
func recordPath(b *tableBench, id string) string {
	return filepath.Join(b.base, "tables", id, "table.json")
}

// digestOf is the digest the markers carry for these bytes of table.json, built
// from the contract in the doc on tableMarker rather than by the code under test:
// "sha256:" and the lower-case hex of the SHA-256 of the exact bytes.
func digestOf(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// tamperRecord rewrites table <id>'s record the way a person or a bad disk might,
// and leaves the markers as table new sealed them. The record that results is not
// the one they bind, which is what table end must refuse.
func tamperRecord(t *testing.T, b *tableBench, id string, edit func(rec *tableRecord)) tableRecord {
	t.Helper()
	path := recordPath(b, id)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rec tableRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	edit(&rec)
	raw, err = json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return rec
}

// editRecord is tamperRecord followed by sealing table id's markers to the record
// it left, as if table new had written that record: the tests that edit the
// record to put a claim in it, to see the claim refused on its own account, would
// otherwise all be refused by the digest. Only markers of table id are sealed, at
// the paths the record named before and the paths it names now; a marker of
// another table, which the edit may point at, is left as it is.
func editRecord(t *testing.T, b *tableBench, id string, edit func(rec *tableRecord)) tableRecord {
	t.Helper()
	var before tableRecord
	if err := json.Unmarshal(mustRead(t, recordPath(b, id)), &before); err != nil {
		t.Fatal(err)
	}
	rec := tamperRecord(t, b, id, edit)
	resealMarkers(t, b, id,
		filepath.Join(before.Worktree, tableWorktreeMarker), filepath.Join(before.ReplyDir, tableReplyMarker),
		filepath.Join(rec.Worktree, tableWorktreeMarker), filepath.Join(rec.ReplyDir, tableReplyMarker))
	return rec
}

// resealMarkers puts the digest of table id's table.json, as it is now, on the
// third line of each of these markers that is a regular file of three lines whose
// first is id.
func resealMarkers(t *testing.T, b *tableBench, id string, paths ...string) {
	t.Helper()
	digest := digestOf(mustRead(t, recordPath(b, id)))
	for _, path := range paths {
		if fi, err := os.Lstat(path); err != nil || !fi.Mode().IsRegular() {
			continue
		}
		lines := strings.Split(string(mustRead(t, path)), "\n")
		if len(lines) != 4 || lines[3] != "" || lines[0] != id {
			continue
		}
		lines[2] = digest
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// tableOneMarker is the marker of table t1 made with the bench's default seats,
// codex-a and claude-a, as table new seals it: with the digest of the table.json
// that is there now.
func tableOneMarker(t *testing.T, b *tableBench) string {
	t.Helper()
	return "t1\nclaude-a codex-a\n" + digestOf(mustRead(t, recordPath(b, "t1"))) + "\n"
}

// markerOf is the marker content table new writes for a table of this id whose
// seats are rec's, built from the contract in the doc on tableMarker rather than
// by the code under test: the id, a newline, the seat nicks exactly as given,
// sorted bytewise and separated by spaces, a newline, the digest line. The digest
// is a placeholder that editRecord seals to the record it leaves.
func markerOf(id string, rec tableRecord) string {
	var nicks []string
	for _, s := range rec.Seats {
		nicks = append(nicks, s.Nick)
	}
	sort.Strings(nicks)
	return id + "\n" + strings.Join(nicks, " ") + "\n" + digestOf(nil) + "\n"
}

// precious makes a directory with a file in it and returns the file's path.
func precious(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "keep.txt")
	if err := os.WriteFile(file, []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return file
}

func wantExist(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s is gone (%v), though the end was refused", p, err)
		}
	}
}

// TestTableEndRefusesWhatIsNotProvablyTheTables: the record is a file, and a
// path in it is a claim. A worktree git does not list, a reply directory that
// table new did not mark, one that is not directly in the temp directory or is a
// link: each is refused, exit 1, naming the path, even under --force, and
// nothing else is removed first.
func TestTableEndRefusesWhatIsNotProvablyTheTables(t *testing.T) {
	cases := []struct {
		name string
		want string
		// edit rewrites the record and returns the paths that must survive; env,
		// when it is not nil, is the environment `table end` is run in.
		edit func(t *testing.T, b *tableBench, rec *tableRecord) []string
		env  func(b *tableBench) []string
	}{
		{"a worktree git does not list", "does not list it", func(t *testing.T, b *tableBench, rec *tableRecord) []string {
			rec.Worktree = filepath.Join(b.root, "precious")
			return []string{precious(t, rec.Worktree)}
		}, nil},
		{"a path under the table's directory that git does not list", "does not list it", func(t *testing.T, b *tableBench, rec *tableRecord) []string {
			rec.Worktree = filepath.Join(b.base, "tables", "t1", "precious")
			return []string{precious(t, rec.Worktree)}
		}, nil},
		{"the target's own checkout under another spelling", "does not list it", func(t *testing.T, b *tableBench, rec *tableRecord) []string {
			link := filepath.Join(b.root, "alias")
			if err := os.Symlink(b.repo, link); err != nil {
				t.Fatal(err)
			}
			rec.Worktree = link
			return []string{filepath.Join(b.repo, "README.md")}
		}, nil},
		{"a worktree outside the table's directory when the target is gone", "git cannot say", func(t *testing.T, b *tableBench, rec *tableRecord) []string {
			if err := os.RemoveAll(b.repo); err != nil {
				t.Fatal(err)
			}
			rec.Worktree = filepath.Join(b.root, "precious")
			return []string{precious(t, rec.Worktree)}
		}, nil},
		{"a reply directory with no marker", "marker", func(t *testing.T, b *tableBench, rec *tableRecord) []string {
			rec.ReplyDir = filepath.Join(b.tmp, "parley-table-t1-fake")
			return []string{precious(t, rec.ReplyDir)}
		}, nil},
		{"a reply directory whose marker names another table", `holds "t2"`, func(t *testing.T, b *tableBench, rec *tableRecord) []string {
			rec.ReplyDir = filepath.Join(b.tmp, "parley-table-t1-fake")
			file := precious(t, rec.ReplyDir)
			if err := os.WriteFile(filepath.Join(rec.ReplyDir, tableReplyMarker), []byte(markerOf("t2", *rec)), 0o600); err != nil {
				t.Fatal(err)
			}
			return []string{file}
		}, nil},
		{"a reply directory that is not directly in the temp directory", "not directly in", func(t *testing.T, b *tableBench, rec *tableRecord) []string {
			rec.ReplyDir = filepath.Join(b.tmp, "sub", "parley-table-t1-fake")
			file := precious(t, rec.ReplyDir)
			if err := os.WriteFile(filepath.Join(rec.ReplyDir, tableReplyMarker), []byte(markerOf("t1", *rec)), 0o600); err != nil {
				t.Fatal(err)
			}
			return []string{file}
		}, nil},
		{"a reply directory that is a link to a marked one", "not a directory", func(t *testing.T, b *tableBench, rec *tableRecord) []string {
			real := rec.ReplyDir
			rec.ReplyDir = filepath.Join(b.tmp, "parley-table-t1-link")
			if err := os.Symlink(real, rec.ReplyDir); err != nil {
				t.Fatal(err)
			}
			return []string{filepath.Join(real, tableReplyMarker)}
		}, nil},
		{"a reply directory when the temp directory is not the one table new ran under", "temp directory", func(t *testing.T, b *tableBench, rec *tableRecord) []string {
			return []string{filepath.Join(rec.ReplyDir, tableReplyMarker)}
		}, func(b *tableBench) []string {
			other := filepath.Join(b.root, "other-tmp")
			if err := os.Mkdir(other, 0o755); err != nil {
				panic(err)
			}
			return append(b.env(), "TMPDIR="+other)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := newTableBench(t)
			wt, _ := tableUp(t, b, "t1")
			var keep []string
			editRecord(t, b, "t1", func(rec *tableRecord) {
				keep = c.edit(t, b, rec)
			})
			env := b.env()
			if c.env != nil {
				env = c.env(b)
			}
			res := runParleyEnvIn(t, "", env, b.endArgs("t1", "--force")...)
			if res.code != 1 || !strings.Contains(res.stderr, c.want) {
				t.Fatalf("exit %d, want 1 saying %q\nstderr: %s", res.code, c.want, res.stderr)
			}
			// It names the path it will not remove.
			if !strings.Contains(res.stderr, "will not remove") {
				t.Errorf("the refusal does not say what it will not remove:\n%s", res.stderr)
			}
			wantExist(t, keep...)
			wantExist(t, filepath.Join(b.base, "tables", "t1", "table.json"))
			// The refusal came before any removal: the table's own worktree, which
			// the record may still name, is there.
			if strings.Contains(c.name, "reply directory") {
				wantExist(t, wt)
			}
		})
	}
}

// TestTableEndDeletesByHandOnlyUnderTheTablesDirectory: when git cannot remove a
// worktree (here, because it is locked), the fallback deletes the directory
// itself, and only if it is under <base>/tables/<id>/.
func TestTableEndDeletesByHandOnlyUnderTheTablesDirectory(t *testing.T) {
	t.Run("under the table's directory", func(t *testing.T) {
		b := newTableBench(t)
		wt, _ := tableUp(t, b, "t1")
		gitIn(t, b.repo, "worktree", "lock", wt)
		mustTable(t, b, b.endArgs("t1", "--force")...)
		if _, err := os.Lstat(wt); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the worktree git could not remove is still there (%v)", err)
		}
		if _, err := os.Lstat(filepath.Join(b.base, "tables", "t1")); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the table directory is still there (%v)", err)
		}
	})
	t.Run("elsewhere", func(t *testing.T) {
		b := newTableBench(t)
		wt := filepath.Join(b.root, "shared")
		mustTable(t, b, b.newArgs("t1", nil, "--worktree", wt)...)
		gitIn(t, b.repo, "worktree", "lock", wt)
		res := b.run(t, b.endArgs("t1", "--force")...)
		if res.code != 1 || !strings.Contains(res.stderr, wt) || !strings.Contains(res.stderr, "not deleted by hand") {
			t.Fatalf("exit %d, want 1 naming %s and that it is not deleted by hand\n%s", res.code, wt, res.stderr)
		}
		wantExist(t, filepath.Join(wt, "README.md"), filepath.Join(wt, ".mcp.json"))
	})
}

// TestTableNewLosingTheRaceForTheWorktreeLeavesTheWinnersCheckout: the path was
// free when the plan was made, and another run took it before this one claimed it
// with os.Mkdir. This run stops there, and its rollback has no worktree of its
// own to take down.
func TestTableNewLosingTheRaceForTheWorktreeLeavesTheWinnersCheckout(t *testing.T) {
	b := newTableBench(t)
	wt := filepath.Join(b.root, "shared")
	ctx := context.Background()
	plan, err := planTable(ctx, tableInput{
		id: "t1", target: b.repo, targetGiven: true, chair: "colin", chairGiven: true, rounds: 3,
		seats: []string{"claude-a=claude"}, worktree: wt, worktreeGiven: true,
		server: b.db.server, stateDir: b.base,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The winner.
	gitIn(t, b.repo, "worktree", "add", "--detach", wt)
	file := filepath.Join(wt, "winners.txt")
	if err := os.WriteFile(file, []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	run := &tableRun{plan: plan, log: io.Discard}
	var ce *configError
	if err := run.setUp(ctx); !errors.As(err, &ce) || !strings.Contains(err.Error(), "already exists") || !strings.Contains(err.Error(), wt) {
		t.Fatalf("setUp = %v, want a configuration error saying the worktree %s already exists", err, wt)
	}
	run.rollback()
	run.release()

	wantExist(t, file)
	if list := gitIn(t, b.repo, "worktree", "list", "--porcelain"); !strings.Contains(list, "shared") {
		t.Errorf("the rollback deregistered the winner's worktree:\n%s", list)
	}
	// What this run did make is gone.
	if _, err := os.Lstat(plan.tableDir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("this run's table directory is still there (%v)", err)
	}
}

// TestTableNewWritesNothingThroughALink: the config files land in the worktree
// or not at all. A link along the way, or a file where a directory is due, is
// refused, and what the link pointed at is untouched.
func TestTableNewWritesNothingThroughALink(t *testing.T) {
	t.Run("a directory that resolves outside the worktree", func(t *testing.T) {
		root, outside := t.TempDir(), t.TempDir()
		if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
			t.Fatal(err)
		}
		err := writeNewFileIn(root, filepath.Join(root, "link", "config.toml"), []byte("x"), 0o644)
		if err == nil || !strings.Contains(err.Error(), "outside the worktree") {
			t.Fatalf("writeNewFileIn through a link out = %v, want a refusal", err)
		}
		if _, err := os.Lstat(filepath.Join(outside, "config.toml")); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the file was written outside the worktree (%v)", err)
		}
	})
	t.Run("a file that is a link", func(t *testing.T) {
		root, outside := t.TempDir(), t.TempDir()
		target := filepath.Join(outside, "victim")
		if err := os.Symlink(target, filepath.Join(root, ".mcp.json")); err != nil {
			t.Fatal(err)
		}
		if err := writeNewFileIn(root, filepath.Join(root, ".mcp.json"), []byte("x"), 0o644); err == nil {
			t.Fatal("writeNewFileIn followed a dangling link")
		}
		if _, err := os.Lstat(target); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the link's target was created (%v)", err)
		}
	})
	t.Run("a file that is already there", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "config.toml")
		if err := os.WriteFile(path, []byte("mine"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := writeNewFileIn(root, path, []byte("x"), 0o644); err == nil {
			t.Fatal("writeNewFileIn wrote over a file")
		}
		if got, _ := os.ReadFile(path); string(got) != "mine" {
			t.Errorf("the file now holds %q", got)
		}
	})
	t.Run("inside the worktree", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := writeNewFileIn(root, filepath.Join(root, "sub", "config.toml"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	t.Run(".codex as a symlink or a file", func(t *testing.T) {
		root, outside := t.TempDir(), t.TempDir()
		link := filepath.Join(root, "link")
		if err := os.Symlink(outside, link); err != nil {
			t.Fatal(err)
		}
		if err := makeRealDir(link); err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Errorf("makeRealDir on a symlink = %v, want a refusal", err)
		}
		file := filepath.Join(root, "file")
		if err := os.WriteFile(file, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := makeRealDir(file); err == nil || !strings.Contains(err.Error(), "not a directory") {
			t.Errorf("makeRealDir on a file = %v, want a refusal", err)
		}
		fresh := filepath.Join(root, "fresh")
		if err := makeRealDir(fresh); err != nil {
			t.Errorf("makeRealDir on a missing path: %v", err)
		}
		if err := makeRealDir(fresh); err != nil {
			t.Errorf("makeRealDir on a directory: %v", err)
		}
	})
}

// TestLockSeatsHoldsEveryLockUntilReleased: `table end` takes each seat's lock
// without blocking and keeps all of them through its teardown, so a seat that
// starts meanwhile is refused; a lock already held ends it, naming the seat and
// leaving none of the others taken.
func TestLockSeatsHoldsEveryLockUntilReleased(t *testing.T) {
	base := t.TempDir()
	rec := tableRecord{Seats: []seatRecord{{Nick: "alpha"}, {Nick: "beta"}, {Nick: "gamma"}}}
	release, err := lockSeats(base, rec)
	if err != nil {
		t.Fatal(err)
	}
	var locked *state.LockedError
	for _, s := range rec.Seats {
		if _, err := state.LockNick(base, s.Nick); !errors.As(err, &locked) {
			t.Errorf("seat %s's lock is not held after lockSeats: %v", s.Nick, err)
		}
	}
	// And a seat that starts now is refused.
	d, err := state.Open(base, "beta", "irc://127.0.0.1:6667/#t1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Lock(); !errors.As(err, &locked) {
		t.Errorf("a seat started during the teardown got its lock: %v", err)
	}
	release()
	for _, s := range rec.Seats {
		l, err := state.LockNick(base, s.Nick)
		if err != nil {
			t.Fatalf("seat %s's lock is held after the release: %v", s.Nick, err)
		}
		l.Release()
	}

	// A lock held by a running seat refuses, names it, and takes no other.
	gamma, err := state.LockNick(base, "gamma")
	if err != nil {
		t.Fatal(err)
	}
	defer gamma.Release()
	if _, err := lockSeats(base, rec); err == nil || !strings.Contains(err.Error(), `"gamma"`) || !strings.Contains(err.Error(), "still running") {
		t.Fatalf("lockSeats with gamma running = %v, want a refusal naming gamma", err)
	}
	for _, nick := range []string{"alpha", "beta"} {
		l, err := state.LockNick(base, nick)
		if err != nil {
			t.Fatalf("the refused lockSeats left %s's lock held: %v", nick, err)
		}
		l.Release()
	}
}

// TestGitWorktreeAddTakesAnEmptyDirectory pins the git behaviour table new's
// claim of the worktree path rests on: `git worktree add --detach <dir> HEAD`
// checks out into a directory that exists and is empty, keeps its mode, and
// refuses one that holds anything.
func TestGitWorktreeAddTakesAnEmptyDirectory(t *testing.T) {
	b := newTableBench(t)
	empty := filepath.Join(b.root, "empty")
	if err := os.Mkdir(empty, 0o700); err != nil {
		t.Fatal(err)
	}
	gitIn(t, b.repo, "worktree", "add", "--detach", empty, "HEAD")
	if _, err := os.Stat(filepath.Join(empty, "README.md")); err != nil {
		t.Errorf("git did not check out into the empty directory: %v", err)
	}
	if fi, err := os.Stat(empty); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("the directory's mode after the add is %v, %v; want 0700 kept", fi.Mode().Perm(), err)
	}

	full := filepath.Join(b.root, "full")
	precious(t, full)
	cmd := exec.Command("git", "-C", b.repo, "worktree", "add", "--detach", full, "HEAD")
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Errorf("git worktree add took a directory that holds a file:\n%s", out)
	}
	wantExist(t, filepath.Join(full, "keep.txt"))
}

// failingPostCheckout installs a post-checkout hook in the target that says why
// it failed and exits 1. Git keeps the worktree it has just made and returns the
// hook's status.
func failingPostCheckout(t *testing.T, b *tableBench) {
	t.Helper()
	hook := filepath.Join(b.repo, ".git", "hooks", "post-checkout")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho 'hook says no' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestTableNewRollsBackFullyWhenThePostCheckoutHookFails: git keeps the checkout
// and its registration when the target's post-checkout hook fails. The run exits
// 1 and takes both down, and a second run at the same --worktree is not blocked
// by a leftover.
func TestTableNewRollsBackFullyWhenThePostCheckoutHookFails(t *testing.T) {
	for _, tc := range []struct {
		name     string
		explicit bool
	}{{"the default worktree", false}, {"an explicit --worktree", true}} {
		t.Run(tc.name, func(t *testing.T) {
			b := newTableBench(t)
			failingPostCheckout(t, b)
			wt := filepath.Join(b.base, "tables", "t1", "worktree")
			var extra []string
			if tc.explicit {
				wt = filepath.Join(b.root, "shared")
				extra = []string{"--worktree", wt}
			}
			before := b.snapshot(t)

			res := b.run(t, b.newArgs("t1", nil, extra...)...)
			if res.code != 1 {
				t.Fatalf("exit %d, want 1\nstdout: %s\nstderr: %s", res.code, res.stdout, res.stderr)
			}
			if !strings.Contains(res.stderr, "worktree add") || !strings.Contains(res.stderr, "rolled back") || strings.Contains(res.stderr, "could NOT") {
				t.Errorf("stderr does not show the failed add and a clean rollback:\n%s", res.stderr)
			}
			if res.stdout != "" {
				t.Errorf("a failed run printed on stdout: %q", res.stdout)
			}
			if _, err := os.Lstat(wt); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("the worktree %s is still there (%v)", wt, err)
			}
			if got := b.callCount(); got != 0 {
				t.Errorf("codex was called %d times before the worktree existed", got)
			}
			// git worktree list, the admin directories, the state base (less the
			// seats' lock files, which stay) and TMPDIR are as they were.
			b.wantUnchanged(t, before)

			// Nothing is left to block the next run.
			if err := os.Remove(filepath.Join(b.repo, ".git", "hooks", "post-checkout")); err != nil {
				t.Fatal(err)
			}
			mustTable(t, b, b.newArgs("t1", nil, extra...)...)
			if _, err := os.Stat(filepath.Join(wt, ".mcp.json")); err != nil {
				t.Errorf("the run after the failed one did not make the worktree: %v", err)
			}
		})
	}
}

// paddedRecord is table id's record as json with trailing blanks added up to n
// bytes: still valid JSON, so that only its size can be what is refused. Its
// markers are sealed to it, as if table new had written it so.
func paddedRecord(t *testing.T, b *tableBench, id string, n int) {
	t.Helper()
	path := recordPath(b, id)
	raw := bytes.TrimRight(mustRead(t, path), " \n")
	if len(raw) > n {
		t.Fatalf("the record is already %d bytes, more than %d", len(raw), n)
	}
	raw = append(raw, bytes.Repeat([]byte(" "), n-len(raw))...)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	var rec tableRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	resealMarkers(t, b, id, filepath.Join(rec.Worktree, tableWorktreeMarker), filepath.Join(rec.ReplyDir, tableReplyMarker))
}

// TestTableRecordIsReadThroughAMaximumOnly: the record is a file a person or a
// bad disk may have changed, so a table.json of 1 MiB is refused as corrupt, exit
// 1, naming the file, with nothing removed; one of exactly the maximum is read.
func TestTableRecordIsReadThroughAMaximumOnly(t *testing.T) {
	b := newTableBench(t)
	wt, rec := tableUp(t, b, "t1")
	path := filepath.Join(b.base, "tables", "t1", "table.json")

	for _, size := range []int{1 << 20, tableRecordMax + 1} {
		paddedRecord(t, b, "t1", size)
		res := b.run(t, b.endArgs("t1", "--force")...)
		if res.code != 1 || !strings.Contains(res.stderr, path) || !strings.Contains(res.stderr, "not valid") || !strings.Contains(res.stderr, "larger than") {
			t.Errorf("a record of %d bytes: exit %d, want 1 naming %s as too large\n%s", size, res.code, path, res.stderr)
		}
		wantExist(t, wt, rec.ReplyDir, path)
	}

	paddedRecord(t, b, "t1", tableRecordMax)
	mustTable(t, b, b.endArgs("t1")...)
	if _, err := os.Lstat(wt); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a record of exactly the maximum was not read: the worktree is still there (%v)", err)
	}
}

// TestTableEndRefusesARecordThatIsAFIFO: opening a FIFO for reading waits for a
// writer, so a table.json replaced by one must be refused at once, not hang.
func TestTableEndRefusesARecordThatIsAFIFO(t *testing.T) {
	b := newTableBench(t)
	wt, rec := tableUp(t, b, "t1")
	path := filepath.Join(b.base, "tables", "t1", "table.json")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	res := b.run(t, b.endArgs("t1", "--force")...)
	if res.code != 1 || !strings.Contains(res.stderr, "not a regular file") {
		t.Fatalf("exit %d, want 1 saying the record is not a regular file\n%s", res.code, res.stderr)
	}
	wantExist(t, wt, rec.ReplyDir)
}

// TestTableEndRemovesOnlyThreadRecordsThatAreTheTables: the path of a seat's
// record comes from the stream and the nick in table.json, so a record that holds
// another thread than the seat's is another table's. The end is refused, exit 1,
// naming the record and both ids, even under --force, and the record and the rest
// of the table survive. A record that is not there is fine.
func TestTableEndRemovesOnlyThreadRecordsThatAreTheTables(t *testing.T) {
	t.Run("another table's record", func(t *testing.T) {
		b := newTableBench(t)
		wt, rec1 := tableUp(t, b, "t1")
		_, rec2 := tableUp(t, b, "t2")
		seat1 := rec1.Seats[0]
		seat2 := rec2.Seats[0]
		if seat1.Nick != seat2.Nick || seat1.Thread == "" || seat1.Thread == seat2.Thread {
			t.Fatalf("the two tables' first seats are %+v and %+v; want one nick, two threads", seat1, seat2)
		}
		other, err := state.OpenThreadRecord(b.base, seat2.Nick, rec2.Stream)
		if err != nil {
			t.Fatal(err)
		}
		if id, err := other.Read(); err != nil || id != seat2.Thread {
			t.Fatalf("table t2's record holds %q (%v), want %s", id, err, seat2.Thread)
		}

		editRecord(t, b, "t1", func(rec *tableRecord) { rec.Stream = rec2.Stream })
		for _, force := range [][]string{nil, {"--force"}} {
			res := b.run(t, b.endArgs("t1", force...)...)
			for _, want := range []string{"will not remove", other.Path(), seat1.Thread, seat2.Thread} {
				if res.code != 1 || !strings.Contains(res.stderr, want) {
					t.Fatalf("end %v: exit %d, want 1 saying %q\n%s", force, res.code, want, res.stderr)
				}
			}
			if id, err := other.Read(); err != nil || id != seat2.Thread {
				t.Errorf("end %v: table t2's record holds %q (%v), want it untouched", force, id, err)
			}
			wantExist(t, wt, rec1.ReplyDir, filepath.Join(b.base, "tables", "t1", "table.json"))
		}

		// With its own stream back, t1 ends, and t2's record is still there.
		editRecord(t, b, "t1", func(rec *tableRecord) { rec.Stream = rec1.Stream })
		mustTable(t, b, b.endArgs("t1")...)
		if id, err := other.Read(); err != nil || id != seat2.Thread {
			t.Errorf("after t1 ended, table t2's record holds %q (%v), want it untouched", id, err)
		}
	})

	t.Run("a record that is not one", func(t *testing.T) {
		b := newTableBench(t)
		wt, rec := tableUp(t, b, "t1")
		tr, err := state.OpenThreadRecord(b.base, rec.Seats[0].Nick, rec.Stream)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(tr.Path(), []byte("not a thread\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		res := b.run(t, b.endArgs("t1", "--force")...)
		if res.code != 1 || !strings.Contains(res.stderr, "will not remove") || !strings.Contains(res.stderr, tr.Path()) || !strings.Contains(res.stderr, "malformed") {
			t.Fatalf("exit %d, want 1 refusing the malformed record %s\n%s", res.code, tr.Path(), res.stderr)
		}
		wantExist(t, wt, rec.ReplyDir, tr.Path(), filepath.Join(b.base, "tables", "t1", "table.json"))
	})

	t.Run("a record that is not there", func(t *testing.T) {
		b := newTableBench(t)
		wt, rec := tableUp(t, b, "t1")
		tr, err := state.OpenThreadRecord(b.base, rec.Seats[0].Nick, rec.Stream)
		if err != nil {
			t.Fatal(err)
		}
		if err := tr.Remove(); err != nil {
			t.Fatal(err)
		}
		res := mustTable(t, b, b.endArgs("t1")...)
		if !strings.Contains(res.stdout, "thread record for seat "+rec.Seats[0].Nick+" (already gone)") {
			t.Errorf("the report does not say the record was already gone:\n%s", res.stdout)
		}
		if _, err := os.Lstat(wt); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the worktree is still there (%v)", err)
		}
	})
}

// threadRecordOf is the thread record of the seat of this nick in the stream.
func threadRecordOf(t *testing.T, b *tableBench, nick, stream string) *state.ThreadRecord {
	t.Helper()
	tr, err := state.OpenThreadRecord(b.base, nick, stream)
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

// TestGitOutputIsCapped: git's stdout is kept to gitOutputMax. A plain git call
// past it fails; the dirty check, which can act on a cut-off status, reports
// the complete records it kept and that there were more.
func TestGitOutputIsCapped(t *testing.T) {
	b := newTableBench(t)
	wt, rec := tableUp(t, b, "t1")
	for i := 0; i < 40; i++ {
		if err := os.WriteFile(filepath.Join(wt, fmt.Sprintf("scratch-%02d.txt", i)), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	defer func(old int) { gitOutputMax = old }(gitOutputMax)
	gitOutputMax = 200
	if _, err := git(context.Background(), "-C", wt, "status", "--porcelain=v1", "-z", "--untracked-files=all"); err == nil || !strings.Contains(err.Error(), "more than 200 bytes") {
		t.Fatalf("got %v, want a failure for output past the cap", err)
	}
	dirty, more, err := dirtyWorktree(context.Background(), tableRecord{Target: rec.Target, Worktree: wt, Seats: []seatRecord{{Nick: "x", Harness: harnessClaude}}})
	if err != nil || !more || len(dirty) == 0 {
		t.Fatalf("dirtyWorktree = %d paths, more=%v, %v; want some paths and more", len(dirty), more, err)
	}
	for _, e := range dirty {
		if !strings.HasPrefix(e.path, "scratch-") && e.path != ".codex/config.toml" && e.path != ".parley-table" {
			t.Errorf("a cut-off record reached the list: %q", e.path)
		}
	}
}

// TestTableEndLeavesAnUnvouchedRegistrationAlone: with both markers gone,
// nothing shows that a git registration at the record's worktree path is this
// table's, so the end neither removes nor prunes it, and lists it instead.
func TestTableEndLeavesAnUnvouchedRegistrationAlone(t *testing.T) {
	b := newTableBench(t)
	wt, rec := tableUp(t, b, "t1")
	for _, p := range []string{wt, rec.ReplyDir} {
		if err := os.RemoveAll(p); err != nil {
			t.Fatal(err)
		}
	}
	res := mustTable(t, b, b.endArgs("t1")...)
	_, left := splitReport(res.stdout)
	for _, want := range []string{"registration of the worktree " + wt, "worktree remove --force --force"} {
		if !strings.Contains(left, want) {
			t.Errorf("the report does not list %q under Left in place:\n%s", want, res.stdout)
		}
	}
	if list := gitIn(t, b.repo, "worktree", "list", "--porcelain"); !strings.Contains(list, "worktree "+wt+"\n") {
		t.Errorf("an unvouched end dropped the registration of %s:\n%s", wt, list)
	}
}

// splitReport splits table end's report at its "Left in place"
// section: what it says it removed, and what it says it left.
func splitReport(stdout string) (removed, left string) {
	removed, left, _ = strings.Cut(stdout, "Left in place")
	return removed, left
}

// TestTableEndRemovesNoThreadRecordOnAnUnauthenticatedRecord: a thread record's
// path and the thread it should hold are both read from table.json, and the only
// copy of that file's digest outside it is in the markers. With neither marker
// left to read, nothing shows the record is the one table new made, so no thread
// record is removed: each that is there is listed as left in place, with the
// reason and what to do, and the end takes down the rest.
func TestTableEndRemovesNoThreadRecordOnAnUnauthenticatedRecord(t *testing.T) {
	markersGone := func(t *testing.T, wt string, rec tableRecord) {
		t.Helper()
		for _, p := range []string{wt, rec.ReplyDir} {
			if err := os.RemoveAll(p); err != nil {
				t.Fatal(err)
			}
		}
	}

	t.Run("an edited stream and thread pointing at another stopped table's record", func(t *testing.T) {
		b := newTableBench(t)
		wt1, rec1 := tableUp(t, b, "t1")
		wt2, rec2 := tableUp(t, b, "t2")
		seat1, seat2 := rec1.Seats[0], rec2.Seats[0]
		if seat1.Nick != seat2.Nick || seat1.Thread == "" || seat1.Thread == seat2.Thread {
			t.Fatalf("the two tables' first seats are %+v and %+v; want one nick, two threads", seat1, seat2)
		}
		other := threadRecordOf(t, b, seat2.Nick, rec2.Stream)
		own := threadRecordOf(t, b, seat1.Nick, rec1.Stream)

		markersGone(t, wt1, rec1)
		// The edit makes the claim hold: the stream is t2's, the thread is t2's.
		tamperRecord(t, b, "t1", func(rec *tableRecord) {
			rec.Stream = rec2.Stream
			rec.Seats[0].Thread = seat2.Thread
		})

		res := mustTable(t, b, b.endArgs("t1")...)
		removed, left := splitReport(res.stdout)
		if strings.Contains(removed, "thread record") {
			t.Errorf("the report lists a thread record as removed:\n%s", res.stdout)
		}
		for _, want := range []string{other.Path(), "seat " + seat1.Nick, "no marker remained", "remove it yourself"} {
			if !strings.Contains(left, want) {
				t.Errorf("the report does not say %q under Left in place:\n%s", want, res.stdout)
			}
		}
		if id, err := other.Read(); err != nil || id != seat2.Thread {
			t.Errorf("table t2's record holds %q (%v), want it untouched", id, err)
		}
		if id, err := own.Read(); err != nil || id != seat1.Thread {
			t.Errorf("table t1's own record holds %q (%v), want it untouched: the edited record never named it", id, err)
		}
		if _, err := os.Lstat(filepath.Join(b.base, "tables", "t1")); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the table directory is still there (%v): the rest was not taken down", err)
		}
		wantExist(t, wt2, rec2.ReplyDir, filepath.Join(b.base, "tables", "t2", "table.json"))
	})

	t.Run("the unedited record: the table's own records are left too", func(t *testing.T) {
		b := newTableBench(t)
		mustTable(t, b, b.newArgs("t1", []string{"codex-a=codex", "codex-b=codex", "claude-a=claude"})...)
		var rec tableRecord
		if err := json.Unmarshal(mustRead(t, recordPath(b, "t1")), &rec); err != nil {
			t.Fatal(err)
		}
		markersGone(t, rec.Worktree, rec)

		res := mustTable(t, b, b.endArgs("t1")...)
		removed, left := splitReport(res.stdout)
		if strings.Contains(removed, "thread record") {
			t.Errorf("the report lists a thread record as removed:\n%s", res.stdout)
		}
		var codexSeats int
		for _, s := range rec.Seats {
			if s.Thread == "" {
				continue
			}
			codexSeats++
			tr := threadRecordOf(t, b, s.Nick, rec.Stream)
			if id, err := tr.Read(); err != nil || id != s.Thread {
				t.Errorf("seat %s's record holds %q (%v), want it left in place with %s", s.Nick, id, err, s.Thread)
			}
			if !strings.Contains(left, "seat "+s.Nick) || !strings.Contains(left, tr.Path()) {
				t.Errorf("the report does not list seat %s's record %s under Left in place:\n%s", s.Nick, tr.Path(), res.stdout)
			}
		}
		if codexSeats != 2 {
			t.Fatalf("%d Codex seats in %+v, want 2", codexSeats, rec.Seats)
		}
		if !strings.Contains(removed, "worktree "+rec.Worktree+" (already gone)") || !strings.Contains(removed, "table directory") {
			t.Errorf("the end did not take down the rest:\n%s", res.stdout)
		}
	})

	t.Run("a record that is not there is not listed", func(t *testing.T) {
		b := newTableBench(t)
		wt, rec := tableUp(t, b, "t1")
		markersGone(t, wt, rec)
		if err := threadRecordOf(t, b, rec.Seats[0].Nick, rec.Stream).Remove(); err != nil {
			t.Fatal(err)
		}
		res := mustTable(t, b, b.endArgs("t1")...)
		// The worktree's git registration is listed under Left in place (see
		// TestTableEndLeavesAnUnvouchedRegistrationAlone); no thread record is.
		if strings.Contains(res.stdout, "thread record") {
			t.Errorf("the report mentions a thread record that was not there:\n%s", res.stdout)
		}
	})

	// One marker that matches is enough: either vouches for the file, and the
	// records are removed as before.
	for _, which := range []string{"worktree", "reply directory"} {
		t.Run("only the "+which+"'s marker is left", func(t *testing.T) {
			b := newTableBench(t)
			wt, rec := tableUp(t, b, "t1")
			tr := threadRecordOf(t, b, rec.Seats[0].Nick, rec.Stream)
			gone := wt
			if which == "worktree" {
				gone = rec.ReplyDir
			}
			if err := os.RemoveAll(gone); err != nil {
				t.Fatal(err)
			}
			res := mustTable(t, b, b.endArgs("t1")...)
			if strings.Contains(res.stdout, "Left in place") {
				t.Errorf("a marker vouched for the record, yet the report leaves records in place:\n%s", res.stdout)
			}
			if !strings.Contains(res.stdout, "thread record for seat "+rec.Seats[0].Nick+"\n") {
				t.Errorf("the report does not say the thread record was removed:\n%s", res.stdout)
			}
			if _, err := os.Lstat(tr.Path()); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("the thread record is still there (%v)", err)
			}
		})
	}
}

// TestTableEndRemovesTheThreadRecordsFirst: the thread records are removed on the
// authority of the markers the worktree and the reply directory hold, so they go
// before either, and the table directory, which holds the record, is last. An end
// that stops partway then still has its markers when it is run again. The order
// is seen from the inside: a git that logs what is on disk when it is asked to
// remove the worktree, and from the report.
func TestTableEndRemovesTheThreadRecordsFirst(t *testing.T) {
	b := newTableBench(t)
	wt, rec := tableUp(t, b, "t1")
	tr := threadRecordOf(t, b, rec.Seats[0].Nick, rec.Stream)
	tableDir := filepath.Join(b.base, "tables", "t1")
	wantExist(t, tr.Path(), wt, rec.ReplyDir, tableDir)

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	spyDir, log := t.TempDir(), filepath.Join(t.TempDir(), "git.log")
	spy := fmt.Sprintf(`#!/bin/sh
case " $* " in
*" worktree remove "*)
	for p in %[1]q %[2]q %[3]q %[4]q; do
		if [ -e "$p" ]; then echo "present $p"; else echo "gone $p"; fi
	done >> %[5]q ;;
esac
exec %[6]q "$@"
`, tr.Path(), wt, rec.ReplyDir, tableDir, log, realGit)
	if err := os.WriteFile(filepath.Join(spyDir, "git"), []byte(spy), 0o755); err != nil {
		t.Fatal(err)
	}
	res := runParleyEnvIn(t, "", b.envWithPath(strings.Join([]string{spyDir, b.path, os.Getenv("PATH")}, string(os.PathListSeparator))), b.endArgs("t1")...)
	if res.code != 0 {
		t.Fatalf("exit %d\n%s", res.code, res.stderr)
	}

	got := string(mustRead(t, log))
	for _, want := range []string{"gone " + tr.Path(), "present " + wt, "present " + rec.ReplyDir, "present " + tableDir} {
		if !strings.Contains(got, want+"\n") {
			t.Errorf("when git was asked to remove the worktree, want %q; the log is:\n%s", want, got)
		}
	}

	// And the report lists them in the order they went.
	pos := func(sub string) int {
		i := strings.Index(res.stdout, sub)
		if i < 0 {
			t.Fatalf("the report lacks %q:\n%s", sub, res.stdout)
		}
		return i
	}
	order := []int{pos("thread record for seat " + rec.Seats[0].Nick), pos("worktree " + wt), pos("reply directory " + rec.ReplyDir), pos("table directory " + tableDir)}
	if !sort.IntsAreSorted(order) {
		t.Errorf("the report lists the thread record, worktree, reply directory and table directory in positions %v; want that order:\n%s", order, res.stdout)
	}
}

// TestTableNewTakesEverySeatsNickLock: the lock is per nick across all streams,
// so a nick that a running pump or Claude Code session holds would make the
// printed launch fail after Codex turns were spent. A held lock is refused before
// any change, naming the nick, with codex never called and the state base as it
// was bar the lock files, which stay (TestTableNewLeavesItsLockFilesBehind), the
// locks of the seats before it released.
func TestTableNewTakesEverySeatsNickLock(t *testing.T) {
	hold := map[string]func(t *testing.T, b *tableBench, nick string) *state.Lock{
		"the nick lock": func(t *testing.T, b *tableBench, nick string) *state.Lock {
			l, err := state.LockNick(b.base, nick)
			if err != nil {
				t.Fatal(err)
			}
			return l
		},
		"a pump's stream lock": func(t *testing.T, b *tableBench, nick string) *state.Lock {
			d, err := state.Open(b.base, nick, "irc://127.0.0.1:6667/#elsewhere")
			if err != nil {
				t.Fatal(err)
			}
			l, err := d.Lock()
			if err != nil {
				t.Fatal(err)
			}
			return l
		},
	}
	for name, take := range hold {
		for _, held := range []string{"codex-a", "claude-a"} {
			t.Run(name+" on "+held, func(t *testing.T) {
				b := newTableBench(t)
				l := take(t, b, held)
				before := b.snapshot(t)

				res := b.run(t, b.newArgs("t1", nil)...)
				if res.code != 1 || !strings.Contains(res.stderr, `"`+held+`"`) || !strings.Contains(res.stderr, "in use by a running pump or Claude Code session") {
					t.Fatalf("exit %d, want 1 saying seat %s is in use by a running pump or session\nstderr: %s", res.code, held, res.stderr)
				}
				if res.stdout != "" {
					t.Errorf("a refused run printed on stdout: %q", res.stdout)
				}
				if got := b.callCount(); got != 0 {
					t.Errorf("codex was called %d times", got)
				}
				b.wantUnchanged(t, before)

				// The refusal holds nothing: the other seat's lock is free again,
				// and once the holder lets go, the table can be made.
				for _, nick := range []string{"codex-a", "claude-a"} {
					if nick == held {
						continue
					}
					free, err := state.LockNick(b.base, nick)
					if err != nil {
						t.Fatalf("the refused run left seat %s's lock held: %v", nick, err)
					}
					free.Release()
				}
				if err := l.Release(); err != nil {
					t.Fatal(err)
				}
				mustTable(t, b, b.newArgs("t1", nil)...)
			})
		}
	}

	t.Run("a finished run holds nothing", func(t *testing.T) {
		b := newTableBench(t)
		_, rec := tableUp(t, b, "t1")
		for _, s := range rec.Seats {
			free, err := state.LockNick(b.base, s.Nick)
			if err != nil {
				t.Fatalf("table new left seat %s's lock held: %v", s.Nick, err)
			}
			free.Release()
		}
	})
}

// rawRecord edits table id's record as json, so that a field can be missing,
// which a tableRecord cannot say.
func rawRecord(t *testing.T, b *tableBench, id string, edit func(doc map[string]json.RawMessage)) {
	t.Helper()
	path := filepath.Join(b.base, "tables", id, "table.json")
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(mustRead(t, path), &doc); err != nil {
		t.Fatal(err)
	}
	edit(doc)
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestTableEndRefusesARecordWithNoSeats: table new never writes a record without
// a seat, and with none, table end would take no seat's lock and act on a table
// that nothing guards. A seats that is missing, null or empty is a record that is
// not valid: exit 1, naming the file, force or not, and nothing is removed.
func TestTableEndRefusesARecordWithNoSeats(t *testing.T) {
	forms := map[string]func(doc map[string]json.RawMessage){
		"missing": func(doc map[string]json.RawMessage) { delete(doc, "seats") },
		"null":    func(doc map[string]json.RawMessage) { doc["seats"] = json.RawMessage("null") },
		"empty":   func(doc map[string]json.RawMessage) { doc["seats"] = json.RawMessage("[]") },
	}
	for name, edit := range forms {
		t.Run(name, func(t *testing.T) {
			b := newTableBench(t)
			wt, rec := tableUp(t, b, "t1")
			if len(rec.Seats) == 0 {
				t.Fatal("table new wrote a record with no seats")
			}
			path := filepath.Join(b.base, "tables", "t1", "table.json")
			good := mustRead(t, path)
			tr, err := state.OpenThreadRecord(b.base, rec.Seats[0].Nick, rec.Stream)
			if err != nil {
				t.Fatal(err)
			}
			rawRecord(t, b, "t1", edit)

			for _, force := range [][]string{nil, {"--force"}} {
				res := b.run(t, b.endArgs("t1", force...)...)
				if res.code != 1 || !strings.Contains(res.stderr, path) || !strings.Contains(res.stderr, "not valid") || !strings.Contains(res.stderr, "no seats") {
					t.Fatalf("end %v: exit %d, want 1 refusing %s as a record with no seats\n%s", force, res.code, path, res.stderr)
				}
				wantExist(t, wt, filepath.Join(wt, ".mcp.json"), rec.ReplyDir, tr.Path(), path)
			}

			// The record as table new wrote it is taken down as ever.
			if err := os.WriteFile(path, good, 0o600); err != nil {
				t.Fatal(err)
			}
			mustTable(t, b, b.endArgs("t1")...)
		})
	}
}

// TestTableEndWorktreeIsBoundToItsTable: git lists every worktree of the target,
// the other tables' among them, so a listing does not show a worktree is this
// table's. table new writes the table's id into a marker at the worktree root,
// and table end removes a listed worktree only if that marker is a regular file
// holding exactly the record's id, force or not. Anything else is refused, exit
// 1, naming the worktree and what the marker said or that there is none.
func TestTableEndWorktreeIsBoundToItsTable(t *testing.T) {
	t.Run("a record edited to name another live table's worktree", func(t *testing.T) {
		for _, where := range []string{"under the table's directory", "elsewhere"} {
			t.Run(where, func(t *testing.T) {
				b := newTableBench(t)
				wt1, rec1 := tableUp(t, b, "t1")
				var wt2 string
				if where == "elsewhere" {
					wt2 = filepath.Join(b.root, "shared")
					mustTable(t, b, b.newArgs("t2", nil, "--worktree", wt2)...)
				} else {
					wt2, _ = tableUp(t, b, "t2")
				}
				t2json := filepath.Join(b.base, "tables", "t2", "table.json")
				keep := []string{wt1, rec1.ReplyDir, filepath.Join(b.base, "tables", "t1", "table.json"),
					wt2, filepath.Join(wt2, "README.md"), filepath.Join(wt2, ".mcp.json"), filepath.Join(wt2, ".parley-table"), t2json}

				editRecord(t, b, "t1", func(rec *tableRecord) { rec.Worktree = wt2 })
				for _, force := range [][]string{nil, {"--force"}} {
					res := b.run(t, b.endArgs("t1", force...)...)
					for _, want := range []string{"will not remove", wt2, filepath.Join(wt2, ".parley-table"), `holds "t2"`, `"t1"`} {
						if res.code != 1 || !strings.Contains(res.stderr, want) {
							t.Fatalf("end %v: exit %d, want 1 saying %q\n%s", force, res.code, want, res.stderr)
						}
					}
					wantExist(t, keep...)
					if list := gitIn(t, b.repo, "worktree", "list", "--porcelain"); !strings.Contains(list, "worktree "+resolveExisting(wt2)+"\n") {
						t.Errorf("end %v: the target no longer lists table t2's worktree:\n%s", force, list)
					}
				}

				// With its own worktree back, t1 ends, and t2's checkout is untouched.
				editRecord(t, b, "t1", func(rec *tableRecord) { rec.Worktree = wt1 })
				mustTable(t, b, b.endArgs("t1")...)
				wantExist(t, wt2, filepath.Join(wt2, ".mcp.json"), filepath.Join(wt2, ".parley-table"), t2json)
				if _, err := os.Lstat(wt1); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("table t1's own worktree is still there (%v)", err)
				}
				mustTable(t, b, b.endArgs("t2")...)
			})
		}
	})

	t.Run("a marker changed by hand", func(t *testing.T) {
		cases := []struct {
			name   string
			want   string
			tamper func(t *testing.T, b *tableBench, marker string)
		}{
			{"removed", "no marker file", func(t *testing.T, b *tableBench, marker string) {
				if err := os.Remove(marker); err != nil {
					t.Fatal(err)
				}
			}},
			{"rewritten to another table's id", `holds "t2"`, func(t *testing.T, b *tableBench, marker string) {
				if err := os.WriteFile(marker, []byte("t2\nclaude-a codex-a\n"+digestOf(mustRead(t, recordPath(b, "t1")))+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}},
			{"the id alone, as an earlier table new wrote it", `holds "t1"`, func(t *testing.T, b *tableBench, marker string) {
				if err := os.WriteFile(marker, []byte("t1"), 0o644); err != nil {
					t.Fatal(err)
				}
			}},
			{"the id and a newline", `holds "t1\n"`, func(t *testing.T, b *tableBench, marker string) {
				if err := os.WriteFile(marker, []byte("t1\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}},
			{"without its closing newline", `holds "t1\nclaude-a codex-a\nsha256:`, func(t *testing.T, b *tableBench, marker string) {
				if err := os.WriteFile(marker, []byte(strings.TrimSuffix(tableOneMarker(t, b), "\n")), 0o644); err != nil {
					t.Fatal(err)
				}
			}},
			{"given a fourth line", "holds", func(t *testing.T, b *tableBench, marker string) {
				if err := os.WriteFile(marker, []byte(tableOneMarker(t, b)+"extra\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}},
			{"with a third line that is no digest", "holds", func(t *testing.T, b *tableBench, marker string) {
				if err := os.WriteFile(marker, []byte("t1\nclaude-a codex-a\nextra\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}},
			{"with an empty third line", "holds", func(t *testing.T, b *tableBench, marker string) {
				if err := os.WriteFile(marker, []byte("t1\nclaude-a codex-a\n\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}},
			{"emptied", `holds ""`, func(t *testing.T, b *tableBench, marker string) {
				if err := os.WriteFile(marker, nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}},
			{"followed by a megabyte", "holds more than", func(t *testing.T, b *tableBench, marker string) {
				if err := os.WriteFile(marker, append([]byte(tableOneMarker(t, b)), bytes.Repeat([]byte("x"), 1<<20)...), 0o644); err != nil {
					t.Fatal(err)
				}
			}},
			{"replaced by a link to a file holding the marker", "not a regular file", func(t *testing.T, b *tableBench, marker string) {
				target := filepath.Join(filepath.Dir(filepath.Dir(marker)), "elsewhere")
				if err := os.WriteFile(target, []byte(tableOneMarker(t, b)), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(marker); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, marker); err != nil {
					t.Fatal(err)
				}
			}},
			{"replaced by a directory", "not a regular file", func(t *testing.T, b *tableBench, marker string) {
				if err := os.Remove(marker); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(marker, 0o755); err != nil {
					t.Fatal(err)
				}
			}},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				b := newTableBench(t)
				wt, rec := tableUp(t, b, "t1")
				marker := filepath.Join(wt, ".parley-table")
				c.tamper(t, b, marker)
				for _, force := range [][]string{nil, {"--force"}} {
					res := b.run(t, b.endArgs("t1", force...)...)
					for _, want := range []string{"will not remove", wt, marker, c.want} {
						if res.code != 1 || !strings.Contains(res.stderr, want) {
							t.Fatalf("end %v: exit %d, want 1 saying %q\n%s", force, res.code, want, res.stderr)
						}
					}
					wantExist(t, wt, filepath.Join(wt, "README.md"), filepath.Join(wt, ".mcp.json"), rec.ReplyDir, filepath.Join(b.base, "tables", "t1", "table.json"))
				}

				// The marker as table new wrote it is all that was in the way.
				if err := os.RemoveAll(marker); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(marker, []byte(tableOneMarker(t, b)), 0o644); err != nil {
					t.Fatal(err)
				}
				mustTable(t, b, b.endArgs("t1")...)
				if _, err := os.Lstat(wt); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("the worktree is still there (%v)", err)
				}
			})
		}
	})

	// The two rules the marker leaves as they were: a worktree that is gone, and a
	// worktree under the table's directory when git cannot be asked, need none.
	t.Run("a worktree that is gone needs no marker", func(t *testing.T) {
		b := newTableBench(t)
		wt, _ := tableUp(t, b, "t1")
		if err := os.RemoveAll(wt); err != nil {
			t.Fatal(err)
		}
		res := mustTable(t, b, b.endArgs("t1")...)
		if !strings.Contains(res.stdout, "worktree "+wt+" (already gone)") {
			t.Errorf("the report does not say the worktree was already gone:\n%s", res.stdout)
		}
	})
	t.Run("git cannot answer and the worktree is under the table's directory", func(t *testing.T) {
		b := newTableBench(t)
		wt, _ := tableUp(t, b, "t1")
		if err := os.Remove(filepath.Join(wt, ".parley-table")); err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(b.repo); err != nil {
			t.Fatal(err)
		}
		mustTable(t, b, b.endArgs("t1", "--force")...)
		if _, err := os.Lstat(wt); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the worktree is still there (%v)", err)
		}
	})
}

// TestTableNewWritesTheWorktreeMarker: for every table, whatever its seats'
// harnesses and wherever its worktree is, table new writes .parley-table at the
// worktree root with the table's id on the first line and the seats' nicks on
// the second, after git has made the worktree and before any Codex turn runs.
// Once table.json is written it replaces the marker, and the reply directory's,
// with the same two lines and the digest of table.json's bytes as the third, at
// the modes they had and leaving no temp file. It is a file git sees as new, and
// table end does not count it as a change.
func TestTableNewWritesTheWorktreeMarker(t *testing.T) {
	cases := []struct {
		name  string
		seats []string
		extra []string
		nicks []string // the marker's second line: the nicks as given, sorted bytewise
	}{
		{"claude and codex", nil, nil, []string{"claude-a", "codex-a"}},
		{"claude only", []string{"claude-a=claude"}, nil, []string{"claude-a"}},
		{"codex only", []string{"codex-a=codex"}, nil, []string{"codex-a"}},
		{"a worktree asked for", nil, []string{"--worktree", "SHARED"}, []string{"claude-a", "codex-a"}},
		{"mixed-case nicks, given out of order", []string{"Zed=claude", "alpha=codex", "Mid-1=claude"}, nil, []string{"Mid-1", "Zed", "alpha"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := newTableBench(t)
			wt := filepath.Join(b.base, "tables", "t1", "worktree")
			extra := append([]string(nil), c.extra...)
			for i, e := range extra {
				if e == "SHARED" {
					wt = filepath.Join(b.root, "shared")
					extra[i] = wt
				}
			}

			// A codex that says whether the marker was there when its turn began.
			seen := filepath.Join(b.fake, "marker-at-exec")
			real := b.codex + ".real"
			if err := os.Rename(b.codex, real); err != nil {
				t.Fatal(err)
			}
			wrapper := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = exec ]; then\n\tif [ -f .parley-table ]; then cat .parley-table; else echo MISSING; fi > %q\nfi\nexec %q \"$@\"\n", seen, real)
			if err := os.WriteFile(b.codex, []byte(wrapper), 0o755); err != nil {
				t.Fatal(err)
			}

			mustTable(t, b, b.newArgs("t1", c.seats, extra...)...)

			marker := filepath.Join(wt, ".parley-table")
			if fi, err := os.Lstat(marker); err != nil || !fi.Mode().IsRegular() {
				t.Fatalf("the marker is not a regular file at %s (%v)", marker, err)
			}
			wantMarker := "t1\n" + strings.Join(c.nicks, " ") + "\n"
			sealed := wantMarker + digestOf(mustRead(t, recordPath(b, "t1"))) + "\n"
			if got := string(mustRead(t, marker)); got != sealed {
				t.Errorf("the marker holds %q, want %q: the table id, the seats' nicks sorted, the digest of table.json", got, sealed)
			}
			var rec tableRecord
			if err := json.Unmarshal(mustRead(t, recordPath(b, "t1")), &rec); err != nil {
				t.Fatal(err)
			}
			replyMarker := filepath.Join(rec.ReplyDir, ".parley-table")
			if got := string(mustRead(t, replyMarker)); got != sealed {
				t.Errorf("the reply directory's marker holds %q, want %q", got, sealed)
			}
			for _, m := range []struct {
				path string
				mode os.FileMode
			}{{marker, 0o644}, {replyMarker, 0o600}} {
				if fi, err := os.Stat(m.path); err != nil || fi.Mode().Perm() != m.mode {
					t.Errorf("%s has mode %v (%v), want %v, the mode it was written with", m.path, fi.Mode().Perm(), err, m.mode)
				}
			}
			for _, dir := range []string{wt, rec.ReplyDir} {
				ents, err := os.ReadDir(dir)
				if err != nil {
					t.Fatal(err)
				}
				for _, e := range ents {
					if strings.HasPrefix(e.Name(), ".tmp-") {
						t.Errorf("sealing the marker left %s in %s", e.Name(), dir)
					}
				}
			}
			if status := gitIn(t, wt, "status", "--porcelain", "--untracked-files=all"); !strings.Contains(status, "?? .parley-table") {
				t.Errorf("git does not see the marker as a new file, so the dirty check's exemption is not exercised:\n%s", status)
			}
			if b.callCount() > 0 {
				if got := string(mustRead(t, seen)); got != wantMarker {
					t.Errorf("the marker was %q when the Codex turn began, want it written first, as %q", got, wantMarker)
				}
			}
			dirty, more, err := dirtyWorktree(context.Background(), tableRecord{Target: b.repo, Worktree: wt, Seats: []seatRecord{{Nick: "x", Harness: harnessClaude}, {Nick: "y", Harness: harnessCodex}}})
			if err != nil || len(dirty) != 0 || more {
				t.Errorf("dirtyWorktree = %v, %v, want the marker, .mcp.json and .codex/config.toml uncounted", dirty, err)
			}
			mustTable(t, b, b.endArgs("t1")...)
			if _, err := os.Lstat(wt); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("the worktree is still there (%v)", err)
			}
		})
	}
}

// TestBaseListingHidesOnlyLockFiles: the snapshot's view of the state base
// leaves out the nick lock files table new leaves behind, as a pump does, and
// nothing else. Whatever else could be left in a base is listed, so it cannot be
// hidden by the exclusion that makes the lock files not count.
func TestBaseListingHidesOnlyLockFiles(t *testing.T) {
	const absent = "(absent)\n"
	mk := func(t *testing.T, build func(base string)) string {
		t.Helper()
		base := filepath.Join(t.TempDir(), "st")
		if build != nil {
			if err := os.Mkdir(base, 0o700); err != nil {
				t.Fatal(err)
			}
			build(base)
		}
		return baseListing(base)
	}
	file := func(t *testing.T, path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mkdir := func(t *testing.T, path string) {
		t.Helper()
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	if got := mk(t, nil); got != absent {
		t.Errorf("a base that is not there lists as %q", got)
	}

	// What does not count: lock files, alone in their nick's directory.
	for name, build := range map[string]func(t *testing.T, base string){
		"one lock file": func(t *testing.T, base string) { file(t, filepath.Join(base, "codex-a", "lock"), "") },
		"two lock files": func(t *testing.T, base string) {
			file(t, filepath.Join(base, "codex-a", "lock"), "")
			file(t, filepath.Join(base, "claude-a", "lock"), "")
		},
		"an id lock file": func(t *testing.T, base string) { file(t, filepath.Join(base, ".ids", "t1.lock"), "") },
		"an id lock file beside a nick's": func(t *testing.T, base string) {
			file(t, filepath.Join(base, ".ids", "t1.lock"), "")
			file(t, filepath.Join(base, ".ids", "b1.lock"), "")
			file(t, filepath.Join(base, "codex-a", "lock"), "")
		},
	} {
		if got := mk(t, func(base string) { build(t, base) }); got != absent {
			t.Errorf("%s: the base lists as %q, want it as a base that is not there", name, got)
		}
	}

	// What does: each of these is a leftover, and shows.
	visible := map[string]struct {
		build func(t *testing.T, base string)
		want  []string // lines the listing must hold
		not   []string // paths it must not
	}{
		"an empty base": {func(t *testing.T, base string) {}, []string{". "}, nil},
		"a lock file with content": {func(t *testing.T, base string) { file(t, filepath.Join(base, "n", "lock"), "x") },
			[]string{"n ", "n/lock "}, nil},
		"a directory called lock": {func(t *testing.T, base string) { mkdir(t, filepath.Join(base, "n", "lock")) },
			[]string{"n ", "n/lock "}, nil},
		"a link called lock": {func(t *testing.T, base string) {
			file(t, filepath.Join(base, "n", "real"), "")
			if err := os.Symlink("real", filepath.Join(base, "n", "lock")); err != nil {
				t.Fatal(err)
			}
		}, []string{"n ", "n/lock ", "n/real "}, nil},
		"a lock file with another file beside it": {func(t *testing.T, base string) {
			file(t, filepath.Join(base, "n", "lock"), "")
			file(t, filepath.Join(base, "n", "cursor"), "")
		}, []string{"n ", "n/cursor "}, []string{"n/lock"}},
		"a lock file with a stream directory beside it": {func(t *testing.T, base string) {
			file(t, filepath.Join(base, "n", "lock"), "")
			mkdir(t, filepath.Join(base, "n", "s"))
		}, []string{"n ", "n/s "}, []string{"n/lock"}},
		"a lock file deeper": {func(t *testing.T, base string) { file(t, filepath.Join(base, "n", "s", "lock"), "") },
			[]string{"n ", "n/s ", "n/s/lock "}, nil},
		"a lock file at the top": {func(t *testing.T, base string) { file(t, filepath.Join(base, "lock"), "") },
			[]string{"lock "}, nil},
		"a file called lock2": {func(t *testing.T, base string) { file(t, filepath.Join(base, "n", "lock2"), "") },
			[]string{"n ", "n/lock2 "}, nil},
		"an empty nick directory": {func(t *testing.T, base string) { mkdir(t, filepath.Join(base, "n")) },
			[]string{"n "}, nil},
		"an empty tables directory": {func(t *testing.T, base string) { mkdir(t, filepath.Join(base, "tables")) },
			[]string{"tables "}, nil},
		"a lock file in the tables directory": {func(t *testing.T, base string) { file(t, filepath.Join(base, "tables", "lock"), "") },
			[]string{"tables ", "tables/lock "}, nil},
		"an empty .ids directory": {func(t *testing.T, base string) { mkdir(t, filepath.Join(base, ".ids")) },
			[]string{".ids "}, nil},
		"an id lock file with content": {func(t *testing.T, base string) { file(t, filepath.Join(base, ".ids", "t1.lock"), "x") },
			[]string{".ids ", ".ids/t1.lock "}, nil},
		"an id lock file with another file beside it": {func(t *testing.T, base string) {
			file(t, filepath.Join(base, ".ids", "t1.lock"), "")
			file(t, filepath.Join(base, ".ids", "t1"), "")
		}, []string{".ids ", ".ids/t1 "}, []string{".ids/t1.lock"}},
		"a file in .ids that is not a lock": {func(t *testing.T, base string) { file(t, filepath.Join(base, ".ids", "t1.txt"), "") },
			[]string{".ids ", ".ids/t1.txt "}, nil},
		"an id lock file deeper": {func(t *testing.T, base string) { file(t, filepath.Join(base, ".ids", "x", "t1.lock"), "") },
			[]string{".ids ", ".ids/x ", ".ids/x/t1.lock "}, nil},
		"a table beside a lock file": {func(t *testing.T, base string) {
			file(t, filepath.Join(base, "n", "lock"), "")
			file(t, filepath.Join(base, "tables", "t1", "table.json"), "{}")
		}, []string{". ", "tables ", "tables/t1 ", "tables/t1/table.json "}, []string{"n"}},
	}
	for name, c := range visible {
		t.Run(name, func(t *testing.T) {
			got := mk(t, func(base string) { c.build(t, base) })
			if got == absent {
				t.Fatalf("the base lists as one that is not there, hiding a leftover")
			}
			lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
			has := func(prefix string) bool {
				for _, l := range lines {
					if strings.HasPrefix(l, prefix) {
						return true
					}
				}
				return false
			}
			for _, want := range c.want {
				if !has(want) {
					t.Errorf("the listing lacks %q:\n%s", want, got)
				}
			}
			for _, not := range c.not {
				if has(not+" ") || has(not+"/") {
					t.Errorf("the listing holds %q, a lock file that is not a leftover:\n%s", not, got)
				}
			}
		})
	}
}

// statLock fails the test unless path is an empty regular file, and returns what
// it found.
func statLock(t *testing.T, path string) fs.FileInfo {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("the lock file %s is gone: %v", path, err)
	}
	if !fi.Mode().IsRegular() || fi.Size() != 0 {
		t.Fatalf("%s is %v, size %d; want a lock file", path, fi.Mode(), fi.Size())
	}
	return fi
}

// treeOf lists every path under root, relative, sorted.
func treeOf(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if rel != "." {
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// TestTableNewLeavesItsLockFilesBehind: no parley code unlinks a lock file. Once
// its path is unlinked another process can create a new file there and lock it
// while the old inode is still locked, and two processes would each hold "the"
// lock of one nick. A table new that fails or is refused therefore leaves
// <base>/<nick>/lock, and the directory holding it, just as a pump does, and
// <base>/.ids/<id>.lock, the id's lock, likewise, and nothing else; the files are
// the same ones the next run, and a table end, lock.
func TestTableNewLeavesItsLockFilesBehind(t *testing.T) {
	t.Run("a run that fails after the locks", func(t *testing.T) {
		b := newTableBench(t)
		failingPostCheckout(t, b)
		res := b.run(t, b.newArgs("t1", nil)...)
		if res.code != 1 || !strings.Contains(res.stderr, "rolled back") {
			t.Fatalf("exit %d, want 1 after a rollback\nstderr: %s", res.code, res.stderr)
		}
		// The id's lock file (<base>/.ids/t1.lock) stays too, for the same reason.
		if got, want := treeOf(t, b.base), []string{".ids", ".ids/t1.lock", "claude-a", "claude-a/lock", "codex-a", "codex-a/lock"}; !slices.Equal(got, want) {
			t.Fatalf("the failed run left %v under the base, want exactly %v", got, want)
		}
		statLock(t, filepath.Join(b.base, ".ids", "t1.lock"))
		codexLock := statLock(t, filepath.Join(b.base, "codex-a", "lock"))
		claudeLock := statLock(t, filepath.Join(b.base, "claude-a", "lock"))

		// The next run, and the end after it, lock those same files.
		if err := os.Remove(filepath.Join(b.repo, ".git", "hooks", "post-checkout")); err != nil {
			t.Fatal(err)
		}
		mustTable(t, b, b.newArgs("t1", nil)...)
		mustTable(t, b, b.endArgs("t1")...)
		for nick, before := range map[string]fs.FileInfo{"codex-a": codexLock, "claude-a": claudeLock} {
			if after := statLock(t, filepath.Join(b.base, nick, "lock")); !os.SameFile(before, after) {
				t.Errorf("seat %s's lock file is not the one the failed run left", nick)
			}
		}
	})

	t.Run("a run refused by a held lock", func(t *testing.T) {
		b := newTableBench(t)
		// Another process holds claude-a, the second seat: codex-a's lock was
		// taken first, by this run, and is released again by its refusal.
		held, err := state.LockNick(b.base, "claude-a")
		if err != nil {
			t.Fatal(err)
		}
		defer held.Release()
		heldPath := filepath.Join(b.base, "claude-a", "lock")
		heldBefore := statLock(t, heldPath)

		res := b.run(t, b.newArgs("t1", nil)...)
		if res.code != 1 || !strings.Contains(res.stderr, `"claude-a"`) {
			t.Fatalf("exit %d, want 1 naming claude-a\nstderr: %s", res.code, res.stderr)
		}
		if got, want := treeOf(t, b.base), []string{".ids", ".ids/t1.lock", "claude-a", "claude-a/lock", "codex-a", "codex-a/lock"}; !slices.Equal(got, want) {
			t.Fatalf("the refused run left %v under the base, want exactly %v", got, want)
		}
		statLock(t, filepath.Join(b.base, "codex-a", "lock"))
		// The holder's file is still the one at its path, and still locked: a
		// rollback that unlinked it would leave the holder with a lock on nothing.
		if after := statLock(t, heldPath); !os.SameFile(heldBefore, after) {
			t.Error("the held lock file was replaced by the refused run")
		}
		var locked *state.LockedError
		if _, err := state.LockNick(b.base, "claude-a"); !errors.As(err, &locked) {
			t.Errorf("the held lock is no longer held through its path: %v", err)
		}
	})
}

// TestTableEndBindsTheRecordsSeatsToTheMarkers: the record is a file, and its
// seats decide whose locks table end takes and whose thread records it removes,
// so a record that was edited could point the end at seats that are not the
// table's and leave the real ones running unguarded. table new writes the seats
// it was made with into both markers, and table end refuses, force or not,
// before it takes any lock or removes anything, a record whose seats differ from
// the ones in any marker it can read.
func TestTableEndBindsTheRecordsSeatsToTheMarkers(t *testing.T) {
	const differ = "the record's seats differ from the ones the table was made with"

	// up makes table t1 and holds every real seat's lock from here on, as a
	// running seat would: a table end that took a lock at all would say a seat is
	// still running instead of what is wrong with the record. The test process is
	// the holder, so a lock the end took and let go would not show; the held locks
	// are what make taking one visible. release lets the seats go. seats are the
	// --seat values, or nil for the bench's defaults.
	up := func(t *testing.T, seats []string) (b *tableBench, wt string, rec tableRecord, replyMarker, wtMarker string, release func()) {
		t.Helper()
		b = newTableBench(t)
		mustTable(t, b, b.newArgs("t1", seats)...)
		if err := json.Unmarshal(mustRead(t, filepath.Join(b.base, "tables", "t1", "table.json")), &rec); err != nil {
			t.Fatal(err)
		}
		wt = rec.Worktree
		var held []*state.Lock
		for _, s := range rec.Seats {
			l, err := state.LockNick(b.base, s.Nick)
			if err != nil {
				t.Fatal(err)
			}
			held = append(held, l)
		}
		release = func() {
			for _, l := range held {
				l.Release()
			}
			held = nil
		}
		t.Cleanup(release)
		return b, wt, rec, filepath.Join(rec.ReplyDir, tableReplyMarker), filepath.Join(wt, tableWorktreeMarker), release
	}
	// refused runs table end with and without --force, and wants the refusal.
	refused := func(t *testing.T, b *tableBench, marker string, keep ...string) {
		t.Helper()
		for _, force := range [][]string{nil, {"--force"}} {
			res := b.run(t, b.endArgs("t1", force...)...)
			for _, want := range []string{"will not remove", marker, differ} {
				if res.code != 1 || !strings.Contains(res.stderr, want) {
					t.Fatalf("end %v: exit %d, want 1 saying %q\n%s", force, res.code, want, res.stderr)
				}
			}
			if strings.Contains(res.stderr, "still running") {
				t.Fatalf("end %v tried a seat's lock before it judged the roster:\n%s", force, res.stderr)
			}
			if res.stdout != "" {
				t.Errorf("end %v printed on stdout: %q", force, res.stdout)
			}
			wantExist(t, keep...)
		}
	}

	// A nick's case counts. Nicks are directory names under the state base, and
	// "Zed" and "zed" are two directories on a case-sensitive filesystem, so a
	// record edited from one to the other would lock the wrong seat; where the
	// filesystem folds case the held lock shows the attempt instead of a new
	// directory.
	edits := []struct {
		name  string
		seats []string // the table's --seat values, nil for the defaults, codex-a first and claude-a second
		edit  func(rec *tableRecord)
		nick  string // a nick the edit puts in the record, whose directory must not appear
	}{
		{"every seat replaced by one valid dummy", nil, func(rec *tableRecord) {
			rec.Seats = []seatRecord{{Nick: "dummy", Harness: harnessClaude}}
		}, "dummy"},
		{"a seat added", nil, func(rec *tableRecord) {
			rec.Seats = append(rec.Seats, seatRecord{Nick: "extra", Harness: harnessClaude})
		}, "extra"},
		{"a seat removed", nil, func(rec *tableRecord) { rec.Seats = rec.Seats[:1] }, ""},
		{"a seat renamed", nil, func(rec *tableRecord) { rec.Seats[1].Nick = "claude-b" }, "claude-b"},
		{"a nick's case changed from claude-a to Claude-A", nil, func(rec *tableRecord) { rec.Seats[1].Nick = "Claude-A" }, ""},
		{"a nick's case changed from Zed to zed", []string{"codex-a=codex", "Zed=claude"}, func(rec *tableRecord) { rec.Seats[1].Nick = "zed" }, ""},
	}
	for _, e := range edits {
		t.Run(e.name, func(t *testing.T) {
			b, wt, rec, replyMarker, wtMarker, release := up(t, e.seats)
			good := mustRead(t, filepath.Join(b.base, "tables", "t1", "table.json"))
			tr, err := state.OpenThreadRecord(b.base, rec.Seats[0].Nick, rec.Stream)
			if err != nil {
				t.Fatal(err)
			}
			tamperRecord(t, b, "t1", e.edit)
			tree := treeOf(t, b.base)

			// Each marker alone binds the seats: the worktree's, then the reply
			// directory's, with the other one gone.
			refused(t, b, wtMarker, wt, rec.ReplyDir, tr.Path(), replyMarker, wtMarker, filepath.Join(b.base, "tables", "t1", "table.json"))
			if after := treeOf(t, b.base); !slices.Equal(after, tree) {
				t.Errorf("the refused end changed the state base: %v, was %v", after, tree)
			}
			if e.nick != "" {
				if _, err := os.Lstat(filepath.Join(b.base, e.nick)); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("the refused end took a lock for %s: %v", e.nick, err)
				}
			}
			saved := mustRead(t, wtMarker)
			if err := os.Remove(wtMarker); err != nil {
				t.Fatal(err)
			}
			refused(t, b, replyMarker, wt, rec.ReplyDir, tr.Path(), replyMarker)
			if err := os.WriteFile(wtMarker, saved, 0o644); err != nil {
				t.Fatal(err)
			}

			// The record as table new wrote it ends cleanly, once the seats are
			// let go: nothing the refusals did stood in its way.
			if err := os.WriteFile(filepath.Join(b.base, "tables", "t1", "table.json"), good, 0o600); err != nil {
				t.Fatal(err)
			}
			release()
			mustTable(t, b, b.endArgs("t1")...)
			for _, gone := range []string{wt, rec.ReplyDir, tr.Path(), filepath.Join(b.base, "tables", "t1")} {
				if _, err := os.Lstat(gone); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("%s is still there after the unedited record was ended (%v)", gone, err)
				}
			}
		})
	}

	t.Run("the unedited record ends cleanly", func(t *testing.T) {
		b := newTableBench(t)
		wt, rec := tableUp(t, b, "t1")
		res := mustTable(t, b, b.endArgs("t1")...)
		if !strings.Contains(res.stdout, "worktree "+wt) || !strings.Contains(res.stdout, "reply directory "+rec.ReplyDir) {
			t.Errorf("the report does not show the worktree and the reply directory removed:\n%s", res.stdout)
		}
		for _, gone := range []string{wt, rec.ReplyDir, filepath.Join(b.base, "tables", "t1")} {
			if _, err := os.Lstat(gone); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("%s is still there (%v)", gone, err)
			}
		}
	})

	t.Run("a marker with the right id and a different roster", func(t *testing.T) {
		// The roster line of a marker whose id and digest are right.
		rosters := map[string]string{
			"a seat more":               "claude-a codex-a extra",
			"a seat fewer":              "claude-a",
			"another seat":              "claude-a dummy",
			"the same, unsorted":        "codex-a claude-a",
			"the same, in another case": "Claude-A codex-a",
			"the same, two spaces":      "claude-a  codex-a",
			"none":                      "",
		}
		for name, roster := range rosters {
			for _, which := range []string{"worktree", "reply directory"} {
				t.Run(name+", in the "+which, func(t *testing.T) {
					b, wt, rec, replyMarker, wtMarker, _ := up(t, nil)
					marker := wtMarker
					if which == "reply directory" {
						marker = replyMarker
					}
					content := "t1\n" + roster + "\n" + digestOf(mustRead(t, recordPath(b, "t1"))) + "\n"
					if err := os.WriteFile(marker, []byte(content), 0o600); err != nil {
						t.Fatal(err)
					}
					refused(t, b, marker, wt, rec.ReplyDir, filepath.Join(b.base, "tables", "t1", "table.json"))
				})
			}
		}
	})

	t.Run("the worktree gone, the reply directory's marker still binds the seats", func(t *testing.T) {
		b, wt, rec, replyMarker, _, _ := up(t, nil)
		if err := os.RemoveAll(wt); err != nil {
			t.Fatal(err)
		}
		tamperRecord(t, b, "t1", func(rec *tableRecord) {
			rec.Seats = []seatRecord{{Nick: "dummy", Harness: harnessClaude}}
		})
		refused(t, b, replyMarker, rec.ReplyDir)
		if _, err := os.Lstat(filepath.Join(b.base, "dummy")); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the refused end took a lock for dummy: %v", err)
		}
	})

	t.Run("neither marker to read, it proceeds but removes no thread record", func(t *testing.T) {
		b := newTableBench(t)
		wt, rec := tableUp(t, b, "t1")
		if err := os.RemoveAll(wt); err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(rec.ReplyDir); err != nil {
			t.Fatal(err)
		}
		res := mustTable(t, b, b.endArgs("t1")...)
		for _, want := range []string{"worktree " + wt + " (already gone)", "reply directory " + rec.ReplyDir + " (already gone)"} {
			if !strings.Contains(res.stdout, want) {
				t.Errorf("the report lacks %q:\n%s", want, res.stdout)
			}
		}
		// Nothing vouches for the thread records any more, so they stay, listed
		// (TestTableEndRemovesNoThreadRecordOnAnUnauthenticatedRecord).
		tr := threadRecordOf(t, b, rec.Seats[0].Nick, rec.Stream)
		if _, left := splitReport(res.stdout); !strings.Contains(left, tr.Path()) {
			t.Errorf("the report does not list %s under Left in place:\n%s", tr.Path(), res.stdout)
		}
	})
}

// oneByteApart reports whether a and b differ by one byte: one replaced, one
// added or one removed.
func oneByteApart(a, b []byte) bool {
	if len(a) == len(b) {
		n := 0
		for i := range a {
			if a[i] != b[i] {
				n++
			}
		}
		return n == 1
	}
	long, short := a, b
	if len(b) > len(a) {
		long, short = b, a
	}
	if len(long) != len(short)+1 {
		return false
	}
	for i := range long {
		if bytes.Equal(append(append([]byte(nil), long[:i]...), long[i+1:]...), short) {
			return true
		}
	}
	return false
}

// wantEndRefused runs table end on table id with and without --force, and wants
// exit 1, no stdout, none of the seats' locks tried, and each of wants on stderr.
// What is in keep must still be there afterwards.
func wantEndRefused(t *testing.T, b *tableBench, id string, wants []string, keep ...string) {
	t.Helper()
	for _, force := range [][]string{nil, {"--force"}} {
		res := b.run(t, b.endArgs(id, force...)...)
		if res.code != 1 {
			t.Fatalf("end %v: exit %d, want 1\nstdout: %s\nstderr: %s", force, res.code, res.stdout, res.stderr)
		}
		for _, want := range wants {
			if !strings.Contains(res.stderr, want) {
				t.Fatalf("end %v: stderr does not say %q:\n%s", force, want, res.stderr)
			}
		}
		if res.stdout != "" {
			t.Errorf("end %v printed on stdout: %q", force, res.stdout)
		}
		wantExist(t, keep...)
	}
}

// TestTableEndBindsTheWholeRecordToTheMarkers: every field of table.json is a
// claim, not only the paths and the seats, and a thread record's check compares
// two values that both come from the file. So table new seals both markers with
// the digest of table.json's exact bytes, and table end refuses, force or not,
// before a lock or a removal, a record that is not byte for byte the one they
// were sealed with, and a marker that carries no digest.
func TestTableEndBindsTheWholeRecordToTheMarkers(t *testing.T) {
	const notThe = "the record is not the one the table was made with"

	t.Run("the stream and the thread of another stopped table", func(t *testing.T) {
		b := newTableBench(t)
		wt1, rec1 := tableUp(t, b, "t1")
		_, rec2 := tableUp(t, b, "t2")
		seat1, seat2 := rec1.Seats[0], rec2.Seats[0]
		if seat1.Nick != seat2.Nick || seat1.Thread == "" || seat1.Thread == seat2.Thread || rec1.Stream == rec2.Stream {
			t.Fatalf("the two tables' first seats are %+v on %s and %+v on %s; want one nick, two threads, two streams", seat1, rec1.Stream, seat2, rec2.Stream)
		}
		own, err := state.OpenThreadRecord(b.base, seat1.Nick, rec1.Stream)
		if err != nil {
			t.Fatal(err)
		}
		other, err := state.OpenThreadRecord(b.base, seat2.Nick, rec2.Stream)
		if err != nil {
			t.Fatal(err)
		}
		good := mustRead(t, recordPath(b, "t1"))

		// Both values the thread record's check compares are table t2's now, so
		// that check alone would take t2's record for t1's and remove it.
		tamperRecord(t, b, "t1", func(rec *tableRecord) {
			rec.Stream = rec2.Stream
			rec.Seats[0].Thread = seat2.Thread
		})
		wantEndRefused(t, b, "t1", []string{"will not remove", filepath.Join(wt1, ".parley-table"), notThe},
			wt1, rec1.ReplyDir, own.Path(), recordPath(b, "t1"))
		if id, err := other.Read(); err != nil || id != seat2.Thread {
			t.Errorf("table t2's thread record holds %q (%v), want %s untouched", id, err, seat2.Thread)
		}

		// The record as table new wrote it ends t1, and t2 is as it was.
		if err := os.WriteFile(recordPath(b, "t1"), good, 0o600); err != nil {
			t.Fatal(err)
		}
		mustTable(t, b, b.endArgs("t1")...)
		if id, err := other.Read(); err != nil || id != seat2.Thread {
			t.Errorf("after t1 ended, table t2's thread record holds %q (%v), want %s", id, err, seat2.Thread)
		}
		if _, err := os.Lstat(own.Path()); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("t1's own thread record is still there (%v)", err)
		}
	})

	t.Run("a one-byte edit to table.json", func(t *testing.T) {
		b := newTableBench(t)
		wt, rec := tableUp(t, b, "t1")
		path := recordPath(b, "t1")
		good := mustRead(t, path)
		// flip changes the byte after marker: a digit to the next, a letter to the next.
		flip := func(marker string) func([]byte) []byte {
			return func(raw []byte) []byte {
				if n := bytes.Count(raw, []byte(marker)); n != 1 {
					t.Fatalf("%q is in the record %d times, want once:\n%s", marker, n, raw)
				}
				out := append([]byte(nil), raw...)
				i := bytes.Index(out, []byte(marker)) + len(marker)
				switch c := out[i]; {
				case '0' <= c && c <= '9':
					out[i] = '0' + (c-'0'+1)%10
				case 'a' <= c && c <= 'z':
					out[i] = 'a' + (c-'a'+1)%26
				default:
					t.Fatalf("the byte after %q is %q, not a digit or a letter", marker, c)
				}
				return out
			}
		}
		edits := []struct {
			name string
			do   func(raw []byte) []byte
		}{
			{"a digit of rounds", flip(`"rounds": `)},
			{"a digit of cap", flip(`"cap": `)},
			{"a letter of the chair", flip(`"chair": "`)},
			{"a digit of the created time", flip(`"created": "202`)},
			{"a letter of the second seat's harness", flip(`"harness": "cla`)},
			{"a blank added at the end", func(raw []byte) []byte { return append(append([]byte(nil), raw...), ' ') }},
			{"a newline added at the end", func(raw []byte) []byte { return append(append([]byte(nil), raw...), '\n') }},
			{"a blank added at the start", func(raw []byte) []byte { return append([]byte{' '}, raw...) }},
			{"an indent blank removed", func(raw []byte) []byte {
				return bytes.Replace(raw, []byte("\n  \"id\""), []byte("\n \"id\""), 1)
			}},
		}
		for _, e := range edits {
			edited := e.do(good)
			if !oneByteApart(good, edited) {
				t.Fatalf("%s: the edit is not one byte:\n%s\n%s", e.name, good, edited)
			}
			if !json.Valid(edited) {
				t.Fatalf("%s: the edit is not valid JSON, so it would be refused for that:\n%s", e.name, edited)
			}
			if err := os.WriteFile(path, edited, 0o600); err != nil {
				t.Fatal(err)
			}
			wantEndRefused(t, b, "t1", []string{"will not remove", filepath.Join(wt, ".parley-table"), notThe}, wt, rec.ReplyDir, path)
		}

		// With the worktree gone, the reply directory's marker alone binds it.
		if err := os.WriteFile(path, edits[0].do(good), 0o600); err != nil {
			t.Fatal(err)
		}
		saved := mustRead(t, filepath.Join(wt, ".parley-table"))
		if err := os.Remove(filepath.Join(wt, ".parley-table")); err != nil {
			t.Fatal(err)
		}
		wantEndRefused(t, b, "t1", []string{"will not remove the reply directory", filepath.Join(rec.ReplyDir, ".parley-table"), notThe}, rec.ReplyDir, path)
		if err := os.WriteFile(filepath.Join(wt, ".parley-table"), saved, 0o644); err != nil {
			t.Fatal(err)
		}

		// The unedited record ends cleanly.
		if err := os.WriteFile(path, good, 0o600); err != nil {
			t.Fatal(err)
		}
		mustTable(t, b, b.endArgs("t1")...)
		for _, gone := range []string{wt, rec.ReplyDir, filepath.Dir(path)} {
			if _, err := os.Lstat(gone); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("%s is still there after the unedited record was ended (%v)", gone, err)
			}
		}
	})

	t.Run("a marker without its digest line", func(t *testing.T) {
		for _, which := range []string{"worktree", "reply directory", "both"} {
			t.Run(which, func(t *testing.T) {
				b := newTableBench(t)
				wt, rec := tableUp(t, b, "t1")
				markers := map[string]string{"worktree": filepath.Join(wt, ".parley-table"), "reply directory": filepath.Join(rec.ReplyDir, ".parley-table")}
				tr, err := state.OpenThreadRecord(b.base, rec.Seats[0].Nick, rec.Stream)
				if err != nil {
					t.Fatal(err)
				}
				saved := map[string][]byte{}
				for name, path := range markers {
					if which != name && which != "both" {
						continue
					}
					saved[path] = mustRead(t, path)
					twoLines := strings.Join(strings.SplitN(string(saved[path]), "\n", 3)[:2], "\n") + "\n"
					if err := os.WriteFile(path, []byte(twoLines), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				// The first marker table end reads is the worktree's, then the reply
				// directory's.
				first := markers["reply directory"]
				if which != "reply directory" {
					first = markers["worktree"]
				}
				wantEndRefused(t, b, "t1", []string{"will not remove", first, "no digest", "did not finish"}, wt, rec.ReplyDir, tr.Path(), recordPath(b, "t1"))

				for path, content := range saved {
					if err := os.WriteFile(path, content, 0o644); err != nil {
						t.Fatal(err)
					}
				}
				mustTable(t, b, b.endArgs("t1")...)
			})
		}
	})

	t.Run("a marker whose digest line is one character off", func(t *testing.T) {
		for _, which := range []string{"worktree", "reply directory"} {
			t.Run(which, func(t *testing.T) {
				b := newTableBench(t)
				wt, rec := tableUp(t, b, "t1")
				marker := filepath.Join(wt, ".parley-table")
				if which == "reply directory" {
					marker = filepath.Join(rec.ReplyDir, ".parley-table")
				}
				good := mustRead(t, marker)
				bad := append([]byte(nil), good...)
				i := len(bad) - 2 // the last hex digit, before the newline
				if bad[i] == '0' {
					bad[i] = '1'
				} else {
					bad[i] = '0'
				}
				if err := os.WriteFile(marker, bad, 0o644); err != nil {
					t.Fatal(err)
				}
				wantEndRefused(t, b, "t1", []string{"will not remove", marker, notThe}, wt, rec.ReplyDir, recordPath(b, "t1"))
				if err := os.WriteFile(marker, good, 0o644); err != nil {
					t.Fatal(err)
				}
				mustTable(t, b, b.endArgs("t1")...)
			})
		}
	})
}

// TestSealMarkerReplacesAMarkerInPlaceOrNotAtAll: the marker keeps its mode, a
// reader sees the old content or the whole new one, no temp file stays, and a
// marker that is a link, or whose directory leads out of the root, is refused
// with the file it pointed at untouched.
func TestSealMarkerReplacesAMarkerInPlaceOrNotAtAll(t *testing.T) {
	m := newTableMarker("t1", []string{"b", "a"}).sealed("sha256:abc")
	t.Run("a regular file", func(t *testing.T) {
		for _, mode := range []os.FileMode{0o644, 0o600} {
			root := t.TempDir()
			path := filepath.Join(root, ".parley-table")
			if err := os.WriteFile(path, []byte("t1\na b\n"), mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
			if err := sealMarker(root, "the worktree", path, m); err != nil {
				t.Fatal(err)
			}
			if got := string(mustRead(t, path)); got != "t1\na b\nsha256:abc\n" {
				t.Errorf("the marker holds %q", got)
			}
			if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != mode {
				t.Errorf("mode %v (%v), want %v", fi.Mode().Perm(), err, mode)
			}
			if ents, _ := os.ReadDir(root); len(ents) != 1 {
				t.Errorf("the directory holds %d entries after the seal, want the marker alone", len(ents))
			}
		}
	})
	t.Run("a link", func(t *testing.T) {
		root, outside := t.TempDir(), t.TempDir()
		victim := filepath.Join(outside, "victim")
		if err := os.WriteFile(victim, []byte("mine\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, ".parley-table")
		if err := os.Symlink(victim, path); err != nil {
			t.Fatal(err)
		}
		if err := sealMarker(root, "the worktree", path, m); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("sealMarker on a link = %v, want a refusal", err)
		}
		if got := string(mustRead(t, victim)); got != "mine\n" {
			t.Errorf("the link's target now holds %q", got)
		}
	})
	t.Run("a directory that leads outside the root", func(t *testing.T) {
		root, outside := t.TempDir(), t.TempDir()
		if err := os.WriteFile(filepath.Join(outside, ".parley-table"), []byte("mine\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(root, "sub")); err != nil {
			t.Fatal(err)
		}
		err := sealMarker(root, "the worktree", filepath.Join(root, "sub", ".parley-table"), m)
		if err == nil || !strings.Contains(err.Error(), "outside the worktree") {
			t.Fatalf("sealMarker through a link out = %v, want a refusal", err)
		}
		if got := string(mustRead(t, filepath.Join(outside, ".parley-table"))); got != "mine\n" {
			t.Errorf("the file outside now holds %q", got)
		}
	})
	t.Run("a marker that is not there", func(t *testing.T) {
		root := t.TempDir()
		if err := sealMarker(root, "the worktree", filepath.Join(root, ".parley-table"), m); err == nil {
			t.Fatal("sealMarker made a marker that was not there")
		}
		if ents, _ := os.ReadDir(root); len(ents) != 0 {
			t.Errorf("the failed seal left %d entries", len(ents))
		}
	})
}

// TestTableNewRollsBackWhenAMarkerCannotBeSealed: the seal is the last step that
// can fail before the sheet, and it is rolled back like the rest, nothing left.
// The codex stub swaps the worktree's marker for a link while its turn runs, so
// the seal that follows finds a marker that is not a regular file.
func TestTableNewRollsBackWhenAMarkerCannotBeSealed(t *testing.T) {
	b := newTableBench(t)
	real := b.codex + ".real"
	if err := os.Rename(b.codex, real); err != nil {
		t.Fatal(err)
	}
	// Only the seat's turn (exec --json), not doctor's exec --help.
	wrapper := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = exec ] && [ \"$2\" = --json ]; then\n\trm -f .parley-table && ln -s /nonexistent .parley-table\nfi\nexec %q \"$@\"\n", real)
	if err := os.WriteFile(b.codex, []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	before := b.snapshot(t)
	res := b.run(t, b.newArgs("t1", []string{"codex-a=codex"})...)
	if res.code != 1 || !strings.Contains(res.stderr, "not a regular file") || !strings.Contains(res.stderr, "rolled back") || strings.Contains(res.stderr, "could NOT") {
		t.Fatalf("exit %d, want 1 refusing the marker that is not a regular file, with a clean rollback\n%s", res.code, res.stderr)
	}
	if res.stdout != "" {
		t.Errorf("a failed run printed on stdout: %q", res.stdout)
	}
	// The thread the turn started is deleted, as for any failure after it, and the
	// machine is as it was but for the stub's own call log.
	if got := b.callCount(); got != 2 || b.call(t, 2)[0] != "delete" {
		t.Errorf("codex was called %d times, the second as %v; want the turn, then its delete", got, b.call(t, 2))
	}
	if after, want := strings.Split(b.snapshot(t), "codex calls:")[0], strings.Split(before, "codex calls:")[0]; after != want {
		t.Fatalf("the run left a trace.\nbefore:\n%s\nafter:\n%s", want, after)
	}
}
