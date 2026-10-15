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
	threadA = "0199c0de-c0de-7c0d-8c0d-ec0dec0dec0d"
	threadB = "0199c0de-beef-7c0d-8c0d-ec0dec0dec0d"
)

func TestThreadRecordRoundTrip(t *testing.T) {
	base := t.TempDir()
	r, err := OpenThreadRecord(base, "codex-a", spec)
	if err != nil {
		t.Fatalf("OpenThreadRecord: %v", err)
	}

	// Opening and reading touch nothing: a machine that never wrote a record
	// has no directory for it.
	got, err := r.Read()
	if err != nil || got != "" {
		t.Fatalf("Read before any write = %q, %v; want empty and no error", got, err)
	}
	if _, err := os.Stat(filepath.Join(base, "codex-a")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("reading made the nick's directory (stat err %v)", err)
	}

	if err := r.Write(threadA); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got, err = r.Read(); err != nil || got != threadA {
		t.Fatalf("Read = %q, %v; want %s", got, err, threadA)
	}

	// Beside the cursor: the stream's directory, named as the cursor's is.
	d, err := Open(base, "codex-a", spec)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if want := filepath.Join(filepath.Dir(d.CursorPath()), "codex-thread"); r.Path() != want {
		t.Fatalf("record at %s, want %s", r.Path(), want)
	}
	fi, err := os.Stat(r.Path())
	if err != nil {
		t.Fatalf("stat the record: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("record mode %v, want 0600", fi.Mode().Perm())
	}
	if b, _ := os.ReadFile(r.Path()); string(b) != threadA+"\n" {
		t.Fatalf("record holds %q, want the id and one newline", b)
	}

	// A second write replaces the first whole, and leaves no temp file.
	if err := r.Write(threadB); err != nil {
		t.Fatalf("second Write: %v", err)
	}
	if got, err = r.Read(); err != nil || got != threadB {
		t.Fatalf("Read after rewrite = %q, %v; want %s", got, err, threadB)
	}
	entries, err := os.ReadDir(filepath.Dir(r.Path()))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != "codex-thread" {
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

func TestThreadRecordRemoveActsOnALink(t *testing.T) {
	r, err := OpenThreadRecord(t.TempDir(), "codex-a", spec)
	if err != nil {
		t.Fatalf("OpenThreadRecord: %v", err)
	}
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(target, []byte(threadA+"\n"), 0o600); err != nil {
		t.Fatalf("write the target: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(r.Path()), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink(target, r.Path()); err != nil {
		t.Fatalf("symlink: %v", err)
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

func TestThreadRecordMalformedContentIsAnErrorNamingTheFile(t *testing.T) {
	for name, content := range map[string]string{
		"empty file":            "",
		"just a newline":        "\n",
		"a session name":        "my-session\n",
		"truncated uuid":        threadA[:35] + "\n",
		"no trailing newline":   threadA,
		"too long":              threadA + "0\n",
		"non-hex digit":         "0199c0de-c0de-7c0d-8c0d-ec0dec0dec0g\n",
		"misplaced dash":        "0199c0dec-0de-7c0d-8c0d-ec0dec0dec0d\n",
		"two lines":             threadA + "\n" + threadB + "\n",
		"trailing space":        threadA + " \n",
		"leading space":         " " + threadA + "\n",
		"carriage return":       threadA + "\r\n",
		"two newlines":          threadA + "\n\n",
		"binary":                "\x00\x01\x02",
		"large stray file":      strings.Repeat("x", 100_000),
		"uuid then a long tail": threadA + "\n" + strings.Repeat("x", 1<<20),
		"uuid in a sentence":    "thread " + threadA + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			r, err := OpenThreadRecord(t.TempDir(), "codex-a", spec)
			if err != nil {
				t.Fatalf("OpenThreadRecord: %v", err)
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

func TestThreadRecordAcceptsEitherCaseOfHex(t *testing.T) {
	r, err := OpenThreadRecord(t.TempDir(), "codex-a", spec)
	if err != nil {
		t.Fatalf("OpenThreadRecord: %v", err)
	}
	upper := strings.ToUpper(threadA)
	if err := r.Write(upper); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got, err := r.Read(); err != nil || got != upper {
		t.Fatalf("Read = %q, %v; want %s", got, err, upper)
	}
}

func TestThreadRecordWriteRefusesWhatReadWould(t *testing.T) {
	r, err := OpenThreadRecord(t.TempDir(), "codex-a", spec)
	if err != nil {
		t.Fatalf("OpenThreadRecord: %v", err)
	}
	for _, id := range []string{"", "auto", "my-session", threadA + "\n", " " + threadA} {
		if err := r.Write(id); err == nil {
			t.Fatalf("Write(%q) succeeded", id)
		}
	}
	if _, err := os.Stat(r.Path()); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a refused write left a record (stat err %v)", err)
	}
}

func TestThreadRecordIsPerNickAndPerStream(t *testing.T) {
	base := t.TempDir()
	open := func(nick, s string) *ThreadRecord {
		r, err := OpenThreadRecord(base, nick, s)
		if err != nil {
			t.Fatalf("OpenThreadRecord(%q, %q): %v", nick, s, err)
		}
		return r
	}
	if err := open("codex-a", spec).Write(threadA); err != nil {
		t.Fatalf("Write: %v", err)
	}
	for _, other := range []*ThreadRecord{open("codex-b", spec), open("codex-a", "file:/tmp/other.ndjson")} {
		if got, err := other.Read(); err != nil || got != "" {
			t.Fatalf("%s reads %q, %v; want no record", other.Path(), got, err)
		}
	}
}

func TestThreadRecordFollowsTheStateDirectory(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "xdg"))
	given := filepath.Join(root, "given")

	viaFlag, err := OpenThreadRecord(given, "codex-a", spec)
	if err != nil {
		t.Fatalf("OpenThreadRecord: %v", err)
	}
	viaDefault, err := OpenThreadRecord("", "codex-a", spec)
	if err != nil {
		t.Fatalf("OpenThreadRecord: %v", err)
	}
	if err := viaFlag.Write(threadA); err != nil {
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

func TestOpenThreadRecordRefusesWhatOpenRefuses(t *testing.T) {
	if _, err := OpenThreadRecord(t.TempDir(), "codex-a", ""); err == nil {
		t.Fatal("an empty stream spec was accepted")
	}
	for _, nick := range []string{"", ".", "..", "a/b", `a\b`} {
		if _, err := OpenThreadRecord(t.TempDir(), nick, spec); err == nil {
			t.Fatalf("nick %q was accepted", nick)
		}
	}
}

// TestOpenRegularNeitherBlocksNorFollows: a FIFO, a link or a directory where a
// record belongs is refused at once, not waited on or read through.
func TestOpenRegularNeitherBlocksNorFollows(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "regular")
	if err := os.WriteFile(regular, []byte(threadA+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(regular, link); err != nil {
		t.Fatal(err)
	}
	if f, err := OpenRegular(regular); err != nil {
		t.Fatalf("a regular file was refused: %v", err)
	} else {
		f.Close()
	}
	for name, path := range map[string]string{"a FIFO": fifo, "a link": link, "a directory": dir} {
		done := make(chan error, 1)
		go func() {
			f, err := OpenRegular(path)
			if f != nil {
				f.Close()
			}
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil {
				t.Errorf("%s was opened as a regular file", name)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("opening %s blocked", name)
		}
	}
	if _, err := OpenRegular(filepath.Join(dir, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a missing file is not reported as missing: %v", err)
	}
}

// TestThreadRecordThatIsAFIFOIsAnErrorNotAHang: --thread auto and send read
// this record, and a FIFO in its place must not hold them.
func TestThreadRecordThatIsAFIFOIsAnErrorNotAHang(t *testing.T) {
	r, err := OpenThreadRecord(t.TempDir(), "codex-a", spec)
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
