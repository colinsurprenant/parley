package state

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/colinsurprenant/parley/internal/stream"
)

const spec = "file:/tmp/chat.ndjson"

func TestSingleProcessGuard(t *testing.T) {
	base := t.TempDir()
	first, err := Open(base, "claude", spec)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	second, err := Open(base, "claude", spec)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	held, err := first.Lock()
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if _, err := second.Lock(); err == nil {
		t.Fatal("the second lock succeeded while the first was held")
	} else {
		var locked *LockedError
		if !errors.As(err, &locked) {
			t.Fatalf("err %v is not a *LockedError", err)
		}
		if locked.Nick != "claude" || locked.Path != first.LockPath() {
			t.Fatalf("error %+v, want nick claude and path %s", locked, first.LockPath())
		}
	}

	// Another stream for the same nick is the same lock: one participant, one
	// process, whatever it is pointed at.
	otherStream, err := Open(base, "claude", "file:/tmp/other.ndjson")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := otherStream.Lock(); err == nil {
		t.Fatal("a second process for the same nick took the lock by changing stream")
	}

	// A different nick is a different lock.
	other, err := Open(base, "codex", spec)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	otherLock, err := other.Lock()
	if err != nil {
		t.Fatalf("lock for another nick: %v", err)
	}
	if err := otherLock.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	if err := held.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	again, err := second.Lock()
	if err != nil {
		t.Fatalf("Lock after release: %v", err)
	}
	if err := again.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

func TestCursorRoundTrip(t *testing.T) {
	d, err := Open(t.TempDir(), "claude", spec)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, err := d.LoadCursor()
	if err != nil {
		t.Fatalf("LoadCursor: %v", err)
	}
	if got != "" {
		t.Fatalf("cursor %q, want empty before anything is stored", got)
	}
	if err := d.SaveCursor("4096:abcd"); err != nil {
		t.Fatalf("SaveCursor: %v", err)
	}
	if got, err = d.LoadCursor(); err != nil || got != "4096:abcd" {
		t.Fatalf("LoadCursor = %q, %v; want 4096:abcd", got, err)
	}
	if err := d.SaveCursor("8192:ef01"); err != nil {
		t.Fatalf("SaveCursor: %v", err)
	}
	if got, err = d.LoadCursor(); err != nil || got != "8192:ef01" {
		t.Fatalf("LoadCursor = %q, %v; want 8192:ef01", got, err)
	}
	entries, err := os.ReadDir(filepath.Dir(d.CursorPath()))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != "cursor" {
			t.Fatalf("leftover file %q in the stream directory", e.Name())
		}
	}
}

func TestCursorValueIsOpaque(t *testing.T) {
	d, err := Open(t.TempDir(), "claude", spec)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Whatever a transport puts in a cursor comes back byte for byte; only the
	// newline SaveCursor adds is taken off again.
	const cursor = " 4096:abcd"
	if err := d.SaveCursor(cursor); err != nil {
		t.Fatalf("SaveCursor: %v", err)
	}
	got, err := d.LoadCursor()
	if err != nil {
		t.Fatalf("LoadCursor: %v", err)
	}
	if got != cursor {
		t.Fatalf("LoadCursor = %q, want %q unchanged", got, cursor)
	}
}

func TestCursorIsPerStream(t *testing.T) {
	base := t.TempDir()
	a, err := Open(base, "claude", "file:/tmp/a.ndjson")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	b, err := Open(base, "claude", "file:/tmp/b.ndjson")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if a.CursorPath() == b.CursorPath() {
		t.Fatalf("two streams share the cursor file %s", a.CursorPath())
	}
	if a.LockPath() != b.LockPath() {
		t.Fatalf("lock paths %s and %s differ; the lock is per nick", a.LockPath(), b.LockPath())
	}
	if err := a.SaveCursor("4096:abcd"); err != nil {
		t.Fatalf("SaveCursor: %v", err)
	}
	got, err := b.LoadCursor()
	if err != nil {
		t.Fatalf("LoadCursor: %v", err)
	}
	if got != "" {
		t.Fatalf("the second stream starts at %q, want the empty cursor", got)
	}

	// The same spec resolves to the same directory across calls.
	again, err := Open(base, "claude", "file:/tmp/a.ndjson")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got, err := again.LoadCursor(); err != nil || got != "4096:abcd" {
		t.Fatalf("LoadCursor = %q, %v; want the cursor saved for the same spec", got, err)
	}
}

func TestDefaultStateDirFollowsXDG(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	d, err := Open("", "claude", spec)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	want := filepath.Join(root, "parley", "claude")
	if d.Path() != want {
		t.Fatalf("path %q, want %q", d.Path(), want)
	}
	streamDir := filepath.Join(want, StreamKey(spec))
	if fi, err := os.Stat(streamDir); err != nil || !fi.IsDir() {
		t.Fatalf("stream directory was not created: %v", err)
	}
	if d.CursorPath() != filepath.Join(streamDir, "cursor") {
		t.Fatalf("cursor path %q", d.CursorPath())
	}
	if d.LockPath() != filepath.Join(want, "lock") {
		t.Fatalf("lock path %q", d.LockPath())
	}
}

func TestExplicitStateDirHoldsNick(t *testing.T) {
	base := t.TempDir()
	d, err := Open(base, "codex", spec)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if want := filepath.Join(base, "codex"); d.Path() != want {
		t.Fatalf("path %q, want %q", d.Path(), want)
	}
}

func TestOpenRejectsBadNick(t *testing.T) {
	for _, nick := range []string{"", ".", "..", "a/b", `a\b`} {
		if _, err := Open(t.TempDir(), nick, spec); err == nil {
			t.Fatalf("nick %q was accepted", nick)
		}
	}
	if _, err := Open(t.TempDir(), "claude", ""); err == nil {
		t.Fatal("an empty stream spec was accepted")
	}
}

// TestNickKeyedPathsFoldASCIICase: every path keyed by a nick is the same path
// for the spellings of it that differ in ASCII case, because they are one nick to
// the server and to stream.NickEqual. The paths are compared as strings: on a
// case-insensitive volume two spellings of one directory share a lock whether or
// not parley folded them.
func TestNickKeyedPathsFoldASCIICase(t *testing.T) {
	base := t.TempDir()
	const irc = "irc://127.0.0.1:6667/#t"
	paths := func(nick string) map[string]string {
		t.Helper()
		got := map[string]string{}
		d, err := Open(base, nick, irc)
		if err != nil {
			t.Fatalf("Open(%q): %v", nick, err)
		}
		got["Dir.Path"], got["Dir.LockPath"], got["Dir.CursorPath"], got["Dir.Base"] = d.Path(), d.LockPath(), d.CursorPath(), d.Base()
		if got["NickLockPath"], err = NickLockPath(base, nick); err != nil {
			t.Fatalf("NickLockPath(%q): %v", nick, err)
		}
		if got["StreamDir"], err = StreamDir(base, nick, irc); err != nil {
			t.Fatalf("StreamDir(%q): %v", nick, err)
		}
		sec, err := OpenSecret(base, nick, "irc", "127.0.0.1_6667", "password")
		if err != nil {
			t.Fatalf("OpenSecret(%q): %v", nick, err)
		}
		got["Secret.Path"] = sec.Path()
		rec, err := OpenThreadRecord(base, nick, irc)
		if err != nil {
			t.Fatalf("OpenThreadRecord(%q): %v", nick, err)
		}
		got["ThreadRecord.Path"] = rec.Path()
		return got
	}
	want := paths("alpha")
	for _, nick := range []string{"Alpha", "ALPHA", "aLpHa"} {
		got := paths(nick)
		for name, w := range want {
			if got[name] != w {
				t.Errorf("%s for %q = %s, want %s, the path for %q", name, nick, got[name], w, "alpha")
			}
		}
	}
	if want["NickLockPath"] != filepath.Join(base, "alpha", "lock") {
		t.Errorf("NickLockPath = %s, want %s", want["NickLockPath"], filepath.Join(base, "alpha", "lock"))
	}
	// The directory made is the lower-case one, not the spelling that made it.
	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "alpha" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the state base holds %q, want one directory, alpha", names)
	}
}

// TestNickFoldAgreesWithNickEqual: two nicks share a state directory exactly
// when stream.NickEqual calls them one nick, so ASCII capitals fold and nothing
// else does.
func TestNickFoldAgreesWithNickEqual(t *testing.T) {
	base := t.TempDir()
	nicks := []string{"alpha", "Alpha", "ALPHA", "alpha2", "al-pha", "Al_Pha", "al_pha", "\u00c9clair", "\u00e9clair", "a\u00c9", "A\u00e9"}
	for _, a := range nicks {
		for _, b := range nicks {
			pa, err := NickLockPath(base, a)
			if err != nil {
				t.Fatalf("NickLockPath(%q): %v", a, err)
			}
			pb, err := NickLockPath(base, b)
			if err != nil {
				t.Fatalf("NickLockPath(%q): %v", b, err)
			}
			if same := pa == pb; same != stream.NickEqual(a, b) {
				t.Errorf("%q and %q: same lock path %v, NickEqual %v", a, b, same, stream.NickEqual(a, b))
			}
		}
	}
}

// TestTheCaseFoldedLockIsOneLock: a lock taken for one spelling of a nick holds
// off every other spelling.
func TestTheCaseFoldedLockIsOneLock(t *testing.T) {
	base := t.TempDir()
	d, err := Open(base, "Alpha", spec)
	if err != nil {
		t.Fatal(err)
	}
	held, err := d.Lock()
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	var locked *LockedError
	if _, err := LockNick(base, "aLPHA"); !errors.As(err, &locked) {
		t.Fatalf("LockNick for another spelling = %v, want a *LockedError", err)
	}
	if locked.Path != d.LockPath() {
		t.Errorf("the refusal names %s, want the one lock, %s", locked.Path, d.LockPath())
	}
}

// TestReservedNicksAreTheThreeDirectories: stream.ValidNick refuses the names in
// stream.ReservedNicks, which that package spells out because it cannot import
// this one; they must be the three directories nickRoot refuses.
func TestReservedNicksAreTheThreeDirectories(t *testing.T) {
	got := map[string]bool{}
	for _, n := range stream.ReservedNicks {
		got[n] = true
	}
	if len(got) != 3 || !got[TablesDir] || !got[BuildsDir] || !got[LinesDir] {
		t.Errorf("stream.ReservedNicks is %v, want %q, %q and %q", stream.ReservedNicks, TablesDir, BuildsDir, LinesDir)
	}
}

// TestTablesBuildsAndLinesAreNotNicks: the base keeps the tables, the builds and
// the lines beside the per-nick directories, so no nick-keyed path may land in
// any of them, in any case, whichever function names it. Nothing is made in
// refusing.
func TestTablesBuildsAndLinesAreNotNicks(t *testing.T) {
	for _, nick := range []string{"tables", "TABLES", "Tables", "builds", "Builds", "bUILDS", "lines", "Lines", "LINES"} {
		base := t.TempDir()
		check := func(what string, err error) {
			t.Helper()
			if err == nil {
				t.Errorf("%s accepted the nick %q", what, nick)
			}
		}
		_, err := Open(base, nick, spec)
		check("Open", err)
		_, err = OpenSecret(base, nick, "irc", "x", "password")
		check("OpenSecret", err)
		_, err = OpenThreadRecord(base, nick, spec)
		check("OpenThreadRecord", err)
		_, err = StreamDir(base, nick, spec)
		check("StreamDir", err)
		_, err = NickLockPath(base, nick)
		check("NickLockPath", err)
		_, err = LockNick(base, nick)
		check("LockNick", err)
		check("RemoveStreamDir", RemoveStreamDir(base, nick, spec))
		if left, _ := os.ReadDir(base); len(left) != 0 {
			t.Errorf("refusing the nick %q left %d entries in the state base", nick, len(left))
		}
	}
	// A nick that merely starts with one of the names is an ordinary nick.
	for _, nick := range []string{"tables2", "mybuilds", "table", "build", "line", "mylines"} {
		if _, err := NickLockPath(t.TempDir(), nick); err != nil {
			t.Errorf("NickLockPath(%q): %v", nick, err)
		}
	}
}

// TestSecretRoundTrip: a credential parley holds for a participant is written
// where only they can read it, and read back exactly.
func TestSecretRoundTrip(t *testing.T) {
	base := t.TempDir()
	s, err := OpenSecret(base, "claude", "irc", "127.0.0.1_6667", "password")
	if err != nil {
		t.Fatalf("OpenSecret: %v", err)
	}
	want := filepath.Join(base, "claude", "irc", "127.0.0.1_6667", "password")
	if s.Path() != want {
		t.Fatalf("Path() = %q, want %q", s.Path(), want)
	}
	// Naming a secret must not create one: a participant that has never stored
	// a credential leaves nothing behind by asking for it.
	if got, err := s.Load(); err != nil || got != "" {
		t.Fatalf("Load of a secret never stored = (%q, %v), want (\"\", nil)", got, err)
	}
	if _, err := os.Stat(filepath.Join(base, "claude")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("naming a secret created %s (stat err %v)", filepath.Join(base, "claude"), err)
	}

	const password = "3d1f0c9ab2e84775af1c0e5d6b3a9182"
	claimed, err := s.Claim(password)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if !claimed {
		t.Fatal("Claim of a secret that was not there reported it was taken")
	}
	got, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != password {
		t.Fatalf("Load() = %q, want %q", got, password)
	}
	fi, err := os.Stat(s.Path())
	if err != nil {
		t.Fatalf("stat the secret: %v", err)
	}
	if mode := fi.Mode().Perm(); mode != 0o600 {
		t.Fatalf("the secret is mode %04o, want 0600", mode)
	}
	// A claim is not a write: the second one leaves the first alone, which is
	// what two processes starting together for one participant depend on.
	claimed, err = s.Claim("00112233")
	if err != nil {
		t.Fatalf("second Claim: %v", err)
	}
	if claimed {
		t.Fatal("the second Claim reported it had taken a secret that was already there")
	}
	if got, _ := s.Load(); got != password {
		t.Fatalf("the second Claim overwrote the first: Load() = %q", got)
	}

	// Discarding is how a credential that turned out to be worthless goes
	// away, and discarding nothing is not an error.
	if err := s.Discard(); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	if got, err := s.Load(); err != nil || got != "" {
		t.Fatalf("after Discard, Load() = (%q, %v)", got, err)
	}
	if err := s.Discard(); err != nil {
		t.Fatalf("Discard of a secret that is not there: %v", err)
	}
}

// TestSecretClaimIsExclusiveUnderConcurrency: the whole point of a claim is
// that exactly one of several processes wins it, and every loser can read what
// the winner wrote.
func TestSecretClaimIsExclusiveUnderConcurrency(t *testing.T) {
	base := t.TempDir()
	const racers = 8
	type outcome struct {
		claimed bool
		value   string
		err     error
	}
	results := make(chan outcome, racers)
	start := make(chan struct{})
	for i := range racers {
		go func() {
			s, err := OpenSecret(base, "claude", "irc", "127.0.0.1_6667", "password")
			if err != nil {
				results <- outcome{err: err}
				return
			}
			<-start
			claimed, err := s.Claim(fmt.Sprintf("password-from-%d", i))
			if err != nil {
				results <- outcome{err: err}
				return
			}
			stored, err := s.Load()
			results <- outcome{claimed: claimed, value: stored, err: err}
		}()
	}
	close(start)
	var winners int
	var value string
	for range racers {
		got := <-results
		if got.err != nil {
			t.Fatalf("racing claim: %v", got.err)
		}
		if got.claimed {
			winners++
		}
		if value == "" {
			value = got.value
		}
		if got.value != value {
			t.Fatalf("two racers read different secrets: %q and %q", got.value, value)
		}
	}
	if winners != 1 {
		t.Fatalf("%d racers claimed the secret, want exactly 1", winners)
	}
}

func TestOpenSecretRejectsPathsThatEscape(t *testing.T) {
	base := t.TempDir()
	for _, parts := range [][]string{
		{},
		{""},
		{".."},
		{"irc", "../../etc"},
		{"irc", "a/b"},
	} {
		if _, err := OpenSecret(base, "claude", parts...); err == nil {
			t.Errorf("OpenSecret with %q was accepted", parts)
		}
	}
	if _, err := OpenSecret(base, "..", "password"); err == nil {
		t.Error("OpenSecret with a dot-dot nick was accepted")
	}
}

// TestResolveBaseIsWhereOpenPutsTheState: a check that has to name the state
// directory without making it gets the one Open would use, and makes nothing.
func TestResolveBaseIsWhereOpenPutsTheState(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "xdg"))
	for _, base := range []string{"", filepath.Join(root, "given"), filepath.Join(root, "a", "..", "b")} {
		got, err := ResolveBase(base)
		if err != nil {
			t.Fatalf("ResolveBase(%q): %v", base, err)
		}
		if !filepath.IsAbs(got) {
			t.Fatalf("ResolveBase(%q) = %q, want an absolute path", base, got)
		}
		if _, err := os.Stat(got); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("ResolveBase(%q) made %s (stat err %v)", base, got, err)
		}
		d, err := Open(base, "claude", spec)
		if err != nil {
			t.Fatalf("Open(%q): %v", base, err)
		}
		if d.Base() != got {
			t.Fatalf("Open(%q).Base() = %q, ResolveBase = %q", base, d.Base(), got)
		}
	}
}

// TestLockNickConflictsWithAStreamLockAndHoldsUntilReleased: the lock `parley
// table end` takes of every seat is the one a pump or a channel holds, so each
// refuses the other, naming the nick; it is held until released, and a nick that
// never ran gets its lock file made.
func TestLockNickConflictsWithAStreamLockAndHoldsUntilReleased(t *testing.T) {
	base := t.TempDir()
	ghost, err := LockNick(base, "ghost")
	if err != nil {
		t.Fatalf("LockNick for a nick that never ran: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "ghost", "lock")); err != nil {
		t.Fatalf("the nick's lock file was not made: %v", err)
	}
	var locked *LockedError
	if _, err := LockNick(base, "ghost"); !errors.As(err, &locked) || locked.Nick != "ghost" {
		t.Fatalf("a second LockNick = %v, want a *LockedError naming ghost", err)
	}
	if err := ghost.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	d, err := Open(base, "claude", spec)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	held, err := d.Lock()
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if _, err := LockNick(base, "claude"); !errors.As(err, &locked) || locked.Nick != "claude" {
		t.Fatalf("LockNick while a stream lock is held = %v, want a *LockedError naming claude", err)
	}
	if err := held.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	taken, err := LockNick(base, "claude")
	if err != nil {
		t.Fatalf("LockNick after the release: %v", err)
	}
	// And the other way: while end holds the nick, a seat that starts is refused.
	if _, err := d.Lock(); !errors.As(err, &locked) {
		t.Fatalf("Lock while LockNick holds the nick = %v, want a *LockedError", err)
	}
	if err := taken.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	again, err := d.Lock()
	if err != nil {
		t.Fatalf("Lock after the release: %v", err)
	}
	again.Release()
}

func TestLockNickRejectsABadNick(t *testing.T) {
	if _, err := LockNick(t.TempDir(), "../x"); err == nil {
		t.Fatal("LockNick accepted a nick that escapes the state directory")
	}
}

// TestNickLockPathNamesTheFileLockNickLocks: the path is the one a stream's
// Dir.Lock shares, it touches nothing, and a nick that escapes the base is
// refused as LockNick refuses it.
func TestNickLockPathNamesTheFileLockNickLocks(t *testing.T) {
	base := t.TempDir()
	path, err := NickLockPath(base, "ghost")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(base, "ghost")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("NickLockPath made the nick's directory (%v)", err)
	}
	d, err := Open(base, "ghost", "irc://127.0.0.1:6667/#t")
	if err != nil {
		t.Fatal(err)
	}
	if path != d.LockPath() {
		t.Fatalf("NickLockPath = %s, want the stream lock's file %s", path, d.LockPath())
	}
	held, err := LockNick(base, "ghost")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	var locked *LockedError
	if _, err := d.Lock(); !errors.As(err, &locked) || locked.Path != path {
		t.Fatalf("Lock while LockNick holds the nick = %v, want a *LockedError on %s", err, path)
	}
	if _, err := NickLockPath(base, "../x"); err == nil {
		t.Fatal("NickLockPath accepted a nick that escapes the state directory")
	}
}

// TestStreamDirNamesTheCursorsDirectoryAndRemoveStreamDirTakesItDown: the
// directory holding a nick's cursor and thread record for a stream is named
// without being made, and removing it takes both, leaves the nick's lock and
// the other streams alone, and says nothing about one that is not there.
func TestStreamDirNamesTheCursorsDirectoryAndRemoveStreamDirTakesItDown(t *testing.T) {
	base := t.TempDir()
	const spec, other = "irc://127.0.0.1:6667/#b1", "irc://127.0.0.1:6667/#b2"
	dir, err := StreamDir(base, "alpha", spec)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(base, "alpha", StreamKey(spec)); dir != want {
		t.Fatalf("StreamDir = %s, want %s", dir, want)
	}
	if _, err := os.Lstat(dir); err == nil {
		t.Fatalf("StreamDir made %s", dir)
	}
	if err := RemoveStreamDir(base, "alpha", spec); err != nil {
		t.Fatalf("removing a directory that is not there: %v", err)
	}

	d, err := Open(base, "alpha", spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SaveCursor("c1"); err != nil {
		t.Fatal(err)
	}
	tr, err := OpenThreadRecord(base, "alpha", spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.Write("0199c0de-0000-7000-8000-000000000001"); err != nil {
		t.Fatal(err)
	}
	keep, err := Open(base, "alpha", other)
	if err != nil {
		t.Fatal(err)
	}
	if err := keep.SaveCursor("c2"); err != nil {
		t.Fatal(err)
	}
	lock, err := d.Lock()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()

	if err := RemoveStreamDir(base, "alpha", spec); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(dir); err == nil {
		t.Errorf("%s is still there", dir)
	}
	if got, err := keep.LoadCursor(); err != nil || got != "c2" {
		t.Errorf("another stream's cursor = %q, %v, want it untouched", got, err)
	}
	if _, err := os.Lstat(d.LockPath()); err != nil {
		t.Errorf("the nick's lock file is gone: %v", err)
	}

	for _, nick := range []string{"", "..", "a/b"} {
		if _, err := StreamDir(base, nick, spec); err == nil {
			t.Errorf("StreamDir accepted the nick %q", nick)
		}
	}
	if _, err := StreamDir(base, "alpha", ""); err == nil {
		t.Error("StreamDir accepted an empty spec")
	}
}
