package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestLockIDIsExclusivePerIDAndHoldsUntilReleased: the lock of one id is refused
// to a second taker without blocking, naming the id and the file; another id is
// free; and a release frees it. The lock file stays.
func TestLockIDIsExclusivePerIDAndHoldsUntilReleased(t *testing.T) {
	base := t.TempDir()
	first, err := LockID(base, "t1")
	if err != nil {
		t.Fatal(err)
	}
	var locked *IDLockedError
	if _, err := LockID(base, "t1"); !errors.As(err, &locked) || locked.ID != "t1" {
		t.Fatalf("a second LockID = %v, want an *IDLockedError naming t1", err)
	}
	if want := filepath.Join(base, ".ids", "t1.lock"); locked.Path != want {
		t.Errorf("the error names %s, want %s", locked.Path, want)
	}
	other, err := LockID(base, "t2")
	if err != nil {
		t.Fatalf("another id is not free: %v", err)
	}
	if err := other.Release(); err != nil {
		t.Fatal(err)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	again, err := LockID(base, "t1")
	if err != nil {
		t.Fatalf("LockID after the release: %v", err)
	}
	again.Release()

	fi, err := os.Stat(filepath.Join(base, ".ids", "t1.lock"))
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
		t.Errorf("the lock file after the releases: %v, %v", fi, err)
	}
	di, err := os.Stat(filepath.Join(base, ".ids"))
	if err != nil || di.Mode().Perm() != 0o700 {
		t.Errorf("the .ids directory: %v, %v", di, err)
	}
}

// TestIDLockIsInANamespaceNoNickCanTake: a nick is a directory of the base and
// holds no '.', so <base>/.ids/ is nobody's; and an id that would leave the
// directory is refused.
func TestIDLockIsInANamespaceNoNickCanTake(t *testing.T) {
	base := t.TempDir()
	nickLock, err := NickLockPath(base, "ghost")
	if err != nil {
		t.Fatal(err)
	}
	idLock, err := IDLockPath(base, "ghost")
	if err != nil {
		t.Fatal(err)
	}
	if nickLock == idLock || filepath.Dir(nickLock) == filepath.Dir(idLock) {
		t.Errorf("a nick's lock %s and an id's %s share a directory", nickLock, idLock)
	}
	for _, id := range []string{"", ".", "..", ".hidden", "a/b", `a\b`, "a\x00b", "../x"} {
		if _, err := IDLockPath(base, id); err == nil {
			t.Errorf("IDLockPath(%q) was accepted", id)
		}
		if _, err := LockID(base, id); err == nil {
			t.Errorf("LockID(%q) was accepted", id)
		}
	}
}
