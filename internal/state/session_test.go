package state

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	sessionA = "ses_ea8bbe5a9ffePFZ3AOhDdaNgbW"
	sessionB = "ses_ea8a722dcffeB5AVROj9ovn5Uq"
)

func TestValidSessionID(t *testing.T) {
	for _, id := range []string{sessionA, sessionB, "ses_" + strings.Repeat("0", 26), "ses_" + strings.Repeat("Z", 26)} {
		if !ValidSessionID(id) {
			t.Errorf("ValidSessionID(%q) = false, want true", id)
		}
	}
	for name, id := range map[string]string{
		"empty":         "",
		"auto":          "auto",
		"prefix only":   "ses_",
		"short":         sessionA[:29],
		"long":          sessionA + "0",
		"wrong prefix":  "msg_" + sessionA[4:],
		"no underscore": "ses-" + sessionA[4:],
		"upper prefix":  "SES_" + sessionA[4:],
		"dash":          "ses_ea8bbe5a9ffePFZ3AOhDdaNgb-",
		"slash":         "ses_ea8bbe5a9ffePFZ3AOhDdaNg/W",
		"dot":           "ses_ea8bbe5a9ffePFZ3AOhDdaNg.W",
		"space":         "ses_ea8bbe5a9ffePFZ3AOhDdaNg W",
		"newline":       "ses_ea8bbe5a9ffePFZ3AOhDdaNgb\n",
		"non-ascii":     "ses_ea8bbe5a9ffePFZ3AOhDdaNgé",
		"a uuid":        threadA,
	} {
		if ValidSessionID(id) {
			t.Errorf("ValidSessionID(%s = %q) = true, want false", name, id)
		}
	}
}

func TestSessionRecordRoundTrip(t *testing.T) {
	base := t.TempDir()
	r, err := OpenSessionRecord(base, "oc-a", spec)
	if err != nil {
		t.Fatalf("OpenSessionRecord: %v", err)
	}

	// Opening and reading touch nothing: a machine that never wrote a record
	// has no directory for it.
	got, err := r.Read()
	if err != nil || got != "" {
		t.Fatalf("Read before any write = %q, %v; want empty and no error", got, err)
	}
	if _, err := os.Stat(filepath.Join(base, "oc-a")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("reading made the nick's directory (stat err %v)", err)
	}

	if err := r.Write(sessionA); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got, err = r.Read(); err != nil || got != sessionA {
		t.Fatalf("Read = %q, %v; want %s", got, err, sessionA)
	}

	// Beside the cursor: the stream's directory, named as the cursor's is.
	d, err := Open(base, "oc-a", spec)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if want := filepath.Join(filepath.Dir(d.CursorPath()), "opencode-session"); r.Path() != want {
		t.Fatalf("record at %s, want %s", r.Path(), want)
	}
	fi, err := os.Stat(r.Path())
	if err != nil {
		t.Fatalf("stat the record: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("record mode %v, want 0600", fi.Mode().Perm())
	}
	if b, _ := os.ReadFile(r.Path()); string(b) != sessionA+"\n" {
		t.Fatalf("record holds %q, want the id and one newline", b)
	}

	// A second write replaces the first whole, and leaves no temp file.
	if err := r.Write(sessionB); err != nil {
		t.Fatalf("second Write: %v", err)
	}
	if got, err = r.Read(); err != nil || got != sessionB {
		t.Fatalf("Read after rewrite = %q, %v; want %s", got, err, sessionB)
	}
	entries, err := os.ReadDir(filepath.Dir(r.Path()))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != "opencode-session" {
			t.Fatalf("leftover file %q in the stream directory", e.Name())
		}
	}

	if err := r.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if got, err = r.Read(); err != nil || got != "" {
		t.Fatalf("Read after Remove = %q, %v; want empty", got, err)
	}
	if err := r.Remove(); err != nil {
		t.Fatalf("Remove of an absent record: %v", err)
	}
}

// TestSessionRecordLeavesTheThreadRecordAlone: the two records sit in one stream
// directory under names of their own, and neither write, read nor remove
// reaches the other's.
func TestSessionRecordLeavesTheThreadRecordAlone(t *testing.T) {
	base := t.TempDir()
	th, err := OpenThreadRecord(base, "oc-a", spec)
	if err != nil {
		t.Fatalf("OpenThreadRecord: %v", err)
	}
	se, err := OpenSessionRecord(base, "oc-a", spec)
	if err != nil {
		t.Fatalf("OpenSessionRecord: %v", err)
	}
	if th.Path() == se.Path() || filepath.Dir(th.Path()) != filepath.Dir(se.Path()) {
		t.Fatalf("records at %s and %s: want one directory, two names", th.Path(), se.Path())
	}
	if err := th.Write(threadA); err != nil {
		t.Fatalf("thread Write: %v", err)
	}
	if err := se.Write(sessionA); err != nil {
		t.Fatalf("session Write: %v", err)
	}
	if err := se.Remove(); err != nil {
		t.Fatalf("session Remove: %v", err)
	}
	if got, err := th.Read(); err != nil || got != threadA {
		t.Fatalf("the thread record reads %q, %v after the session record's removal; want %s", got, err, threadA)
	}
	if err := th.Write(threadB); err != nil {
		t.Fatalf("thread Write: %v", err)
	}
	if got, err := se.Read(); err != nil || got != "" {
		t.Fatalf("the session record reads %q, %v after the thread record's write; want none", got, err)
	}
}

func TestSessionRecordRemoveActsOnALink(t *testing.T) {
	r, err := OpenSessionRecord(t.TempDir(), "oc-a", spec)
	if err != nil {
		t.Fatalf("OpenSessionRecord: %v", err)
	}
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(target, []byte(sessionA+"\n"), 0o600); err != nil {
		t.Fatalf("write the target: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(r.Path()), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink(target, r.Path()); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := r.Read(); err == nil {
		t.Fatal("Read followed a link")
	}
	if err := r.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("Remove followed the link and took its target: %v", err)
	}
	if _, err := os.Lstat(r.Path()); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the link is still there (lstat err %v)", err)
	}
}

func TestSessionRecordMalformedContentIsAnErrorNamingTheFile(t *testing.T) {
	for name, content := range map[string]string{
		"empty file":                  "",
		"just a newline":              "\n",
		"a session name":              "my-session\n",
		"truncated id":                sessionA[:29] + "\n",
		"no trailing newline":         sessionA,
		"too long":                    sessionA + "0\n",
		"a thread uuid":               threadA + "\n",
		"wrong prefix":                "msg_" + sessionA[4:] + "\n",
		"punctuation":                 "ses_ea8bbe5a9ffePFZ3AOhDdaNg-W\n",
		"two lines":                   sessionA + "\n" + sessionB + "\n",
		"trailing space":              sessionA + " \n",
		"leading space":               " " + sessionA + "\n",
		"carriage return":             sessionA + "\r\n",
		"two newlines":                sessionA + "\n\n",
		"binary":                      "\x00\x01\x02",
		"large stray file":            strings.Repeat("x", 100_000),
		"id then a long tail":         sessionA + "\n" + strings.Repeat("x", 1<<20),
		"id in a sentence":            "session " + sessionA + "\n",
		"the command that made it":    "opencode --session " + sessionA + "\n",
		"the id with a path in front": "../" + sessionA + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			r, err := OpenSessionRecord(t.TempDir(), "oc-a", spec)
			if err != nil {
				t.Fatalf("OpenSessionRecord: %v", err)
			}
			if err := os.MkdirAll(filepath.Dir(r.Path()), 0o700); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := os.WriteFile(r.Path(), []byte(content), 0o600); err != nil {
				t.Fatalf("write the record: %v", err)
			}
			got, err := r.Read()
			if err == nil {
				t.Fatalf("Read of %q = %q, want an error", content, got)
			}
			if got != "" {
				t.Fatalf("Read returned %q beside its error", got)
			}
			if !strings.Contains(err.Error(), r.Path()) {
				t.Fatalf("error does not name the file %s: %v", r.Path(), err)
			}
			if len(err.Error()) > 400 {
				t.Fatalf("error quotes the whole file (%d bytes)", len(err.Error()))
			}
		})
	}
}

func TestSessionRecordWriteRefusesWhatReadWould(t *testing.T) {
	r, err := OpenSessionRecord(t.TempDir(), "oc-a", spec)
	if err != nil {
		t.Fatalf("OpenSessionRecord: %v", err)
	}
	for _, id := range []string{"", "auto", "my-session", sessionA + "\n", " " + sessionA, threadA} {
		if err := r.Write(id); err == nil {
			t.Fatalf("Write(%q) succeeded", id)
		}
	}
	if _, err := os.Stat(r.Path()); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a refused write left a record (stat err %v)", err)
	}
}

func TestSessionRecordIsPerNickAndPerStream(t *testing.T) {
	base := t.TempDir()
	open := func(nick, s string) *SessionRecord {
		r, err := OpenSessionRecord(base, nick, s)
		if err != nil {
			t.Fatalf("OpenSessionRecord(%q, %q): %v", nick, s, err)
		}
		return r
	}
	if err := open("oc-a", spec).Write(sessionA); err != nil {
		t.Fatalf("Write: %v", err)
	}
	for _, other := range []*SessionRecord{open("oc-b", spec), open("oc-a", "file:/tmp/other.ndjson")} {
		if got, err := other.Read(); err != nil || got != "" {
			t.Fatalf("%s reads %q, %v; want no record", other.Path(), got, err)
		}
	}
}

func TestSessionRecordFollowsTheStateDirectory(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "xdg"))
	given := filepath.Join(root, "given")

	viaFlag, err := OpenSessionRecord(given, "oc-a", spec)
	if err != nil {
		t.Fatalf("OpenSessionRecord: %v", err)
	}
	viaDefault, err := OpenSessionRecord("", "oc-a", spec)
	if err != nil {
		t.Fatalf("OpenSessionRecord: %v", err)
	}
	if err := viaFlag.Write(sessionA); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got, err := viaDefault.Read(); err != nil || got != "" {
		t.Fatalf("the default base read %q, %v from a record written under --state-dir", got, err)
	}
	wantBase, err := ResolveBase(given)
	if err != nil {
		t.Fatalf("ResolveBase: %v", err)
	}
	if !strings.HasPrefix(viaFlag.Path(), wantBase+string(filepath.Separator)) {
		t.Fatalf("record at %s, want it under %s", viaFlag.Path(), wantBase)
	}
}

func TestOpenSessionRecordRefusesWhatOpenRefuses(t *testing.T) {
	if _, err := OpenSessionRecord(t.TempDir(), "oc-a", ""); err == nil {
		t.Fatal("an empty stream spec was accepted")
	}
	for _, nick := range []string{"", ".", "..", "a/b", `a\b`} {
		if _, err := OpenSessionRecord(t.TempDir(), nick, spec); err == nil {
			t.Fatalf("nick %q was accepted", nick)
		}
	}
}

// TestSessionRecordThatIsAFIFOIsAnErrorNotAHang: --session auto reads this
// record, and a FIFO in its place must not hold it.
func TestSessionRecordThatIsAFIFOIsAnErrorNotAHang(t *testing.T) {
	r, err := OpenSessionRecord(t.TempDir(), "oc-a", spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(r.Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(r.Path(), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := r.Read(); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), r.Path()) {
			t.Fatalf("got %v, want an error naming the record", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reading a FIFO record blocked")
	}
}
