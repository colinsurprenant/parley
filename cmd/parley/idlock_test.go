package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colinsurprenant/parley/internal/state"
)

// The id lifecycle is serialized by one lock per id, <base>/.ids/<id>.lock, taken
// by table new, table end, build new and build end before anything else and held
// for the whole run. These tests drive the interleavings deterministically: the
// test holds the id's lock, which is exactly what a command of another kind that
// is still running holds, and shows that each command refuses and changes
// nothing, and that it runs once the lock is free. The design is to refuse and
// not to wait.

const idInUse = "is in use by another parley table or build command"

// holdID takes the id's lock the way the commands do, for the rest of the test or
// until the returned release is called.
func holdID(t *testing.T, b *tableBench, id string) (release func()) {
	t.Helper()
	l, err := state.LockID(b.base, id)
	if err != nil {
		t.Fatalf("take the lock of id %s: %v", id, err)
	}
	released := false
	release = func() {
		if !released {
			released = true
			if err := l.Release(); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Cleanup(release)
	return release
}

// wantBuildUnchanged fails the test unless the machine, the target's branches
// included, is as the snapshot before found it.
func wantBuildUnchanged(t *testing.T, b *tableBench, before string) {
	t.Helper()
	if after := b.buildSnapshot(t); after != before {
		t.Fatalf("the run left a trace.\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func wantIDRefusal(t *testing.T, b *tableBench, res result, id string) {
	t.Helper()
	if res.code != 1 {
		t.Fatalf("exit %d, want 1\nstdout: %s\nstderr: %s", res.code, res.stdout, res.stderr)
	}
	lock := filepath.Join(b.base, ".ids", id+".lock")
	if !strings.Contains(res.stderr, idInUse) || !strings.Contains(res.stderr, lock) || !strings.Contains(res.stderr, `"`+id+`"`) {
		t.Errorf("stderr does not say id %q is in use, naming %s:\n%s", id, lock, res.stderr)
	}
	if res.stdout != "" {
		t.Errorf("a refused run printed on stdout: %q", res.stdout)
	}
}

// TestIDLockRefusesEachVerbWhileItIsHeld: with the id's lock held, each of the four
// verbs refuses, names the id and the lock file, exits 1 and leaves the machine
// as it was; once the lock is released the same command runs.
func TestIDLockRefusesEachVerbWhileItIsHeld(t *testing.T) {
	t.Run("table new", func(t *testing.T) {
		b := newTableBench(t)
		release := holdID(t, b, "x1")
		before := b.buildSnapshot(t)
		res := b.run(t, b.newArgs("x1", nil)...)
		wantIDRefusal(t, b, res, "x1")
		wantBuildUnchanged(t, b, before)
		release()
		mustTable(t, b, b.newArgs("x1", nil)...)
	})
	t.Run("build new", func(t *testing.T) {
		b := newTableBench(t)
		release := holdID(t, b, "x1")
		before := b.buildSnapshot(t)
		res := b.run(t, b.buildArgs("x1", nil)...)
		wantIDRefusal(t, b, res, "x1")
		wantBuildUnchanged(t, b, before)
		release()
		buildUp(t, b, "x1", nil)
	})
	t.Run("table end", func(t *testing.T) {
		b := newTableBench(t)
		wt, _ := tableUp(t, b, "x1")
		release := holdID(t, b, "x1")
		before := b.buildSnapshot(t)
		res := b.run(t, b.endArgs("x1", "--force")...)
		wantIDRefusal(t, b, res, "x1")
		wantBuildUnchanged(t, b, before)
		wantExist(t, wt, filepath.Join(b.base, "tables", "x1", "table.json"))
		release()
		mustTable(t, b, b.endArgs("x1")...)
		wantGone(t, wt)
	})
	t.Run("build end", func(t *testing.T) {
		b := newTableBench(t)
		rec, _ := buildUp(t, b, "x1", nil)
		release := holdID(t, b, "x1")
		before := b.buildSnapshot(t)
		res := b.run(t, b.buildEndArgs("x1", "--force")...)
		wantIDRefusal(t, b, res, "x1")
		wantBuildUnchanged(t, b, before)
		wantExist(t, rec.Peers[0].Worktree, rec.Peers[1].Worktree, filepath.Join(b.base, "builds", "x1", "build.json"))
		release()
		mustBuildEnd(t, b, "x1")
		wantGone(t, rec.Peers[0].Worktree, rec.Peers[1].Worktree)
	})
}

// TestIDLockIsPerID: a lock held on one id leaves every other id free.
func TestIDLockIsPerID(t *testing.T) {
	b := newTableBench(t)
	holdID(t, b, "x1")
	buildUp(t, b, "x2", nil)
	mustTable(t, b, b.newArgs("x3", []string{"claude-t=claude"})...)
}

// TestTwoEndsAndANewOnOneID: the interleaving of two ends and a new. While one end
// is at work (its lock held), the second end and a new for the same id both
// refuse and change nothing; after it, the second end finds no build, and the
// new then makes one.
func TestTwoEndsAndANewOnOneID(t *testing.T) {
	b := newTableBench(t)
	rec, _ := buildUp(t, b, "x1", nil)
	release := holdID(t, b, "x1")
	before := b.buildSnapshot(t)

	endB := b.run(t, b.buildEndArgs("x1")...)
	wantIDRefusal(t, b, endB, "x1")
	// A new that would otherwise say "build already exists" says the lock is held:
	// the lock comes before the looks.
	newC := b.run(t, b.buildArgs("x1", []string{"codex-b=codex", "claude-b=claude"})...)
	wantIDRefusal(t, b, newC, "x1")
	wantBuildUnchanged(t, b, before)

	// End A finishes (the lock is released), and then end B, which was refused, finds
	// the build gone, exit 2.
	release()
	mustBuildEnd(t, b, "x1")
	wantGone(t, rec.Peers[0].Worktree, rec.Peers[1].Worktree)
	endB = b.run(t, b.buildEndArgs("x1")...)
	if endB.code != 2 || !strings.Contains(endB.stderr, `no build "x1"`) {
		t.Fatalf("a second end: exit %d, want 2 saying there is no build\n%s", endB.code, endB.stderr)
	}
	buildUp(t, b, "x1", []string{"codex-b=codex", "claude-b=claude"})
}

// TestATableNewAndABuildNewWithDisjointNicksCannotShareAnID: two commands for one
// id, one of each kind, with no nick in common (so no nick lock to tell them
// apart). Whichever holds the id's lock, the other refuses; and when they run one
// after the other, the second sees the first as a live table or build.
func TestATableNewAndABuildNewWithDisjointNicksCannotShareAnID(t *testing.T) {
	t.Run("held by the one that runs first", func(t *testing.T) {
		b := newTableBench(t)
		release := holdID(t, b, "x1")
		before := b.buildSnapshot(t)
		wantIDRefusal(t, b, b.run(t, b.newArgs("x1", []string{"claude-t=claude"})...), "x1")
		wantIDRefusal(t, b, b.run(t, b.buildArgs("x1", []string{"codex-b=codex", "claude-b=claude"})...), "x1")
		wantBuildUnchanged(t, b, before)
		release()
		mustTable(t, b, b.newArgs("x1", []string{"claude-t=claude"})...)
		res := b.run(t, b.buildArgs("x1", []string{"codex-b=codex", "claude-b=claude"})...)
		if res.code != 2 || !strings.Contains(res.stderr, "already a table") {
			t.Fatalf("a build over a live table: exit %d, want 2 saying so\n%s", res.code, res.stderr)
		}
	})
	t.Run("a build first, then a table", func(t *testing.T) {
		b := newTableBench(t)
		buildUp(t, b, "x1", []string{"codex-b=codex", "claude-b=claude"})
		res := b.run(t, b.newArgs("x1", []string{"claude-t=claude"})...)
		if res.code != 2 || !strings.Contains(res.stderr, "already a build") {
			t.Fatalf("a table over a live build: exit %d, want 2 saying so\n%s", res.code, res.stderr)
		}
	})
}

// TestTheChecksOfWhatTheBaseHoldsAreMadeUnderTheIDLock: the cross-mode existence
// checks are under the lock, not before it. With a table already there and the
// lock held by another command, claiming the id for a build is refused for the
// lock, and not for the table it would otherwise have found; and the reverse.
// What is planned before the claim looks at no state of the base.
func TestTheChecksOfWhatTheBaseHoldsAreMadeUnderTheIDLock(t *testing.T) {
	ctx := context.Background()
	t.Run("a build over a table", func(t *testing.T) {
		b := newTableBench(t)
		mustTable(t, b, b.newArgs("x1", []string{"claude-t=claude"})...)
		in := buildInput{id: "x1", target: b.repo, targetGiven: true, chair: "colin", chairGiven: true,
			peers: []string{"claude-a=claude", "claude-b=claude"}, server: b.db.server, stateDir: b.base, codexBin: "codex"}
		plan, err := planBuild(ctx, in)
		if err != nil {
			t.Fatalf("planBuild looked at the table: %v", err)
		}
		release := holdID(t, b, "x1")
		err = plan.claimID(ctx)
		if err == nil || !strings.Contains(err.Error(), idInUse) || strings.Contains(err.Error(), "already a table") {
			t.Fatalf("claimID with the lock held = %v, want the lock's refusal and not a look at the table", err)
		}
		release()
		err = plan.claimID(ctx)
		if err == nil || !strings.Contains(err.Error(), "already a table") {
			t.Fatalf("claimID with the lock free = %v, want it to find the table", err)
		}
		// A refusal leaves the lock free again.
		l, lerr := state.LockID(b.base, "x1")
		if lerr != nil {
			t.Fatalf("the lock is still held after a refused claim: %v", lerr)
		}
		l.Release()
	})
	t.Run("a table over a build", func(t *testing.T) {
		b := newTableBench(t)
		buildUp(t, b, "x1", []string{"codex-b=codex", "claude-b=claude"})
		in := tableInput{id: "x1", target: b.repo, targetGiven: true, chair: "colin", chairGiven: true, rounds: 3,
			seats: []string{"claude-t=claude"}, server: b.db.server, stateDir: b.base, codexBin: "codex"}
		plan, err := planTable(ctx, in)
		if err != nil {
			t.Fatalf("planTable looked at the build: %v", err)
		}
		release := holdID(t, b, "x1")
		err = plan.claimID()
		if err == nil || !strings.Contains(err.Error(), idInUse) || strings.Contains(err.Error(), "already a build") {
			t.Fatalf("claimID with the lock held = %v, want the lock's refusal and not a look at the build", err)
		}
		release()
		err = plan.claimID()
		if err == nil || !strings.Contains(err.Error(), "already a build") {
			t.Fatalf("claimID with the lock free = %v, want it to find the build", err)
		}
	})
	t.Run("a plain refusal takes no lock", func(t *testing.T) {
		// A flag, a doctor FAIL or the Claude cap refuse before the claim, so they
		// leave no lock file in the base.
		b := newTableBench(t)
		res := b.run(t, b.buildArgs("b1", []string{"codex-a=codex", "claude-a=claude", "claude-b=claude"})...)
		if res.code != 2 {
			t.Fatalf("exit %d, want 2\n%s", res.code, res.stderr)
		}
		if _, err := os.Lstat(b.base); err == nil {
			t.Errorf("a refusal for a third peer made %s", b.base)
		}
	})
}

// TestEndsReadTheRecordOnlyWithTheIDLockHeld: build end and table end take the
// id's lock before they read the record, so that what they take down is the
// record as it is once no other command can change it. With the lock held by
// another command and a record that is damaged, the refusal is for the lock; the
// damage is found only after it.
func TestEndsReadTheRecordOnlyWithTheIDLockHeld(t *testing.T) {
	t.Run("build end", func(t *testing.T) {
		b := newTableBench(t)
		buildUp(t, b, "x1", nil)
		record := filepath.Join(b.base, "builds", "x1", "build.json")
		if err := os.WriteFile(record, []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		release := holdID(t, b, "x1")
		wantIDRefusal(t, b, b.run(t, b.buildEndArgs("x1")...), "x1")
		release()
		res := b.run(t, b.buildEndArgs("x1")...)
		if res.code != 1 || !strings.Contains(res.stderr, "is not valid") {
			t.Fatalf("after the release: exit %d, want 1 for the damaged record\n%s", res.code, res.stderr)
		}
	})
	t.Run("table end", func(t *testing.T) {
		b := newTableBench(t)
		tableUp(t, b, "x1")
		record := filepath.Join(b.base, "tables", "x1", "table.json")
		if err := os.WriteFile(record, []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		release := holdID(t, b, "x1")
		wantIDRefusal(t, b, b.run(t, b.endArgs("x1")...), "x1")
		release()
		res := b.run(t, b.endArgs("x1")...)
		if res.code != 1 || !strings.Contains(res.stderr, "is not valid") {
			t.Fatalf("after the release: exit %d, want 1 for the damaged record\n%s", res.code, res.stderr)
		}
	})
}

// TestAnEndOfAnIDThatWasNeverThereTakesNoLock: a slip of the finger, or a wrong
// --state-dir, is a configuration error that leaves no lock file in the base.
func TestAnEndOfAnIDThatWasNeverThereTakesNoLock(t *testing.T) {
	b := newTableBench(t)
	for _, c := range []struct {
		args []string
		want string
	}{{b.buildEndArgs("nope"), `no build "nope"`}, {b.endArgs("nope"), `no table "nope"`}} {
		res := b.run(t, c.args...)
		if res.code != 2 || !strings.Contains(res.stderr, c.want) {
			t.Fatalf("%v: exit %d, want 2 saying %s\n%s", c.args, res.code, c.want, res.stderr)
		}
	}
	if _, err := os.Lstat(b.base); err == nil {
		t.Errorf("an end of an id that was never there made %s", b.base)
	}
}

// TestIDLocksStayAfterEveryVerbAndAreFreed: the lock file stays, released, after a
// run, as a nick's does, and the next command takes it.
func TestIDLocksStayAfterEveryVerbAndAreFreed(t *testing.T) {
	b := newTableBench(t)
	buildUp(t, b, "x1", nil)
	mustBuildEnd(t, b, "x1")
	lock := filepath.Join(b.base, ".ids", "x1.lock")
	fi, err := os.Lstat(lock)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() != 0 {
		t.Fatalf("the id lock after new and end: %v, %v", fi, err)
	}
	l, err := state.LockID(b.base, "x1")
	if err != nil {
		t.Fatalf("the id lock is held after the commands returned: %v", err)
	}
	l.Release()
}
