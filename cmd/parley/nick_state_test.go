package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stateEntries names what is directly under dir, as the file system spells it.
func stateEntries(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range es {
		names = append(names, e.Name())
	}
	return names
}

// TestSendRefusesTablesAndBuildsAsNicksAndLandsNothingInThem: a send is the verb
// that writes a credential under the nick's state directory, and a nick named
// for the directory the tables or the builds live in would put that credential
// inside it. The refusal is the nick check's, up front, so it holds for any
// spelling and for the server nobody asked.
func TestSendRefusesTablesAndBuildsAsNicksAndLandsNothingInThem(t *testing.T) {
	addr := startErgo(t)
	chat := "irc://" + addr + "/nickdirs"
	for _, nick := range []string{"tables", "Tables", "BUILDS", "builds"} {
		t.Run(nick, func(t *testing.T) {
			stateDir := t.TempDir()
			res := runParley(t, "", "send", "--stream", chat, "--nick", nick, "--state-dir", stateDir, "--", "hello")
			if res.code != exitConfig {
				t.Fatalf("send as %q exited %d, want %d\nstdout: %s", nick, res.code, exitConfig, res.stdout)
			}
			if !strings.Contains(res.stderr, "cannot be a nick") {
				t.Errorf("stderr does not say the nick is refused: %s", res.stderr)
			}
			if left := stateEntries(t, stateDir); len(left) != 0 {
				t.Errorf("a refused send left %v in the state directory", left)
			}
		})
	}
}

// TestWaitRefusesTablesAndBuildsAsNicks is the same refusal on a verb that needs
// no server: the nick's lock and cursor would have gone inside the directory.
func TestWaitRefusesTablesAndBuildsAsNicks(t *testing.T) {
	for _, nick := range []string{"tables", "TABLES", "Builds"} {
		t.Run(nick, func(t *testing.T) {
			stateDir := t.TempDir()
			chat := "file:" + filepath.Join(t.TempDir(), "chat.ndjson")
			res := runParley(t, "", "wait", "--stream", chat, "--nick", nick, "--state-dir", stateDir, "--timeout", "1s")
			if res.code != exitConfig || !strings.Contains(res.stderr, "cannot be a nick") {
				t.Errorf("wait as %q: exit %d, stderr %q; want a refusal with exit %d", nick, res.code, res.stderr, exitConfig)
			}
			if left := stateEntries(t, stateDir); len(left) != 0 {
				t.Errorf("a refused wait left %v in the state directory", left)
			}
		})
	}
}

// TestReservedNicksAreConfigErrorsBeforeAnythingIsTouched: tables and builds, in
// any case, are refused as a --nick with exit 2 by the check every nick goes
// through, before a stream is opened or a state directory made, whatever the
// stream is: a file: stream never asks the state package for the nick's
// directory, so only the up-front check can refuse it there. nickRoot's own
// refusal stays behind it as the backstop (internal/state).
func TestReservedNicksAreConfigErrorsBeforeAnythingIsTouched(t *testing.T) {
	for _, nick := range []string{"tables", "Tables", "BUILDS", "builds"} {
		for _, verb := range []string{"send", "wait"} {
			t.Run(verb+"/"+nick, func(t *testing.T) {
				root := t.TempDir()
				args := []string{verb, "--stream", "file:" + filepath.Join(root, "chat.ndjson"), "--nick", nick, "--state-dir", filepath.Join(root, "state")}
				if verb == "send" {
					args = append(args, "--", "hello")
				} else {
					args = append(args, "--timeout", "1s")
				}
				res := runParley(t, "", args...)
				if res.code != exitConfig || !strings.Contains(res.stderr, "--nick") || !strings.Contains(res.stderr, "cannot be a nick") {
					t.Errorf("%s as %q: exit %d, stderr %q; want exit %d naming --nick", verb, nick, res.code, res.stderr, exitConfig)
				}
				if left := stateEntries(t, root); len(left) != 0 {
					t.Errorf("a refused %s left %v behind", verb, left)
				}
			})
		}
	}
}

// TestACapitalisedNickKeepsItsStateUnderTheLowerCaseDirectory: Alpha and alpha
// are one account on the server, so what a send keeps for Alpha — the account
// password — goes in the one directory alpha would use, whatever the volume does
// with case. A second send as alpha is the same participant: ergo renames that
// connection to the account's spelling, the join check takes the rename for its
// own, and the message is delivered.
func TestACapitalisedNickKeepsItsStateUnderTheLowerCaseDirectory(t *testing.T) {
	addr := startErgo(t)
	chat := "irc://" + addr + "/spellings"
	stateDir := t.TempDir()
	if res := runParley(t, "", "send", "--stream", chat, "--nick", "Alpha", "--state-dir", stateDir, "--", "first"); res.code != 0 {
		t.Fatalf("send as Alpha: exit %d\n%s", res.code, res.stderr)
	}
	if got := stateEntries(t, stateDir); len(got) != 1 || got[0] != "alpha" {
		t.Fatalf("a send as Alpha made %v in the state directory, want only alpha", got)
	}
	pw := filepath.Join(stateDir, "alpha", "irc", strings.ReplaceAll(addr, ":", "_"), "password")
	if _, err := os.Stat(pw); err != nil {
		t.Errorf("no credential at %s: %v", pw, err)
	}

	if res := runParleyWithin(t, 30*time.Second, "send", "--stream", chat, "--nick", "alpha", "--state-dir", stateDir, "--", "second"); res.code != 0 {
		t.Fatalf("send as alpha after Alpha registered the account: exit %d\n%s", res.code, res.stderr)
	}
	if got := stateEntries(t, stateDir); len(got) != 1 || got[0] != "alpha" {
		t.Errorf("a send as alpha made %v in the state directory, want only alpha", got)
	}
	got := mustRun(t, "", "wait", "--stream", chat, "--nick", "beta", "--state-dir", t.TempDir(), "--timeout", "10s", "--debounce", "200ms")
	for _, body := range []string{"first", "second"} {
		if !strings.Contains(got.stdout, body) {
			t.Errorf("beta was not delivered %q:\n%s", body, got.stdout)
		}
	}
}
