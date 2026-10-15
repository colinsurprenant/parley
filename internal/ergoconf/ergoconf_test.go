package ergoconf

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderSetsTheListenerAndTheAbsoluteDatastore(t *testing.T) {
	out, err := Render("127.0.0.1:7777", "/var/lib/parley ergo/ergo.db")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	got := string(out)
	for _, want := range []string{`        "127.0.0.1:7777": {}`, `    path: "/var/lib/parley ergo/ergo.db"`} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered config lacks %q", want)
		}
	}
	if strings.Contains(got, "6667") || strings.Contains(got, "path: ergo.db") {
		t.Errorf("rendered config still holds a default it should have replaced")
	}
	// Everything else is the embedded file's, line for line.
	rest := strings.NewReplacer(`"127.0.0.1:7777": {}`, `"127.0.0.1:6667": {}`, `path: "/var/lib/parley ergo/ergo.db"`, `path: ergo.db`).Replace(got)
	if rest != YAML {
		t.Errorf("rendering changed more than the listener and the datastore")
	}
}

// TestTheConfigurationLocksItsDatastore: without a top-level lock-file ergo
// takes no lock at all, and two servers on one datastore both start. The name
// is relative, so the lock file lands in the datastore directory ergo is run in
// and Render, which replaces two values, leaves it as it is.
func TestTheConfigurationLocksItsDatastore(t *testing.T) {
	const line = "\nlock-file: \"ircd.lock\"\n"
	if strings.Count(YAML, line) != 1 {
		t.Errorf("the embedded ircd.yaml has no top-level %q line, exactly one", strings.TrimSpace(line))
	}
	out, err := Render("127.0.0.1:7777", "/var/lib/parley-ergo/ergo.db")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(string(out), line) {
		t.Error("the rendered configuration lost the lock-file line")
	}
}

func TestRenderQuotesWhatYAMLWouldMisread(t *testing.T) {
	out, err := Render("[::1]:6667", "/tmp/a: b #c/ergo.db")
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, want := range []string{`"[::1]:6667": {}`, `path: "/tmp/a: b #c/ergo.db"`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("rendered config lacks %q", want)
		}
	}
}

func TestRenderRefusesARelativeDatastoreAndAnEmptyListener(t *testing.T) {
	if _, err := Render("127.0.0.1:6667", "ergo.db"); err == nil {
		t.Error("a relative datastore path was rendered")
	}
	if _, err := Render("", "/tmp/ergo.db"); err == nil {
		t.Error("an empty listener was rendered")
	}
}

func TestInstallWritesTheConfigurationBesideTheDatastore(t *testing.T) {
	dir := t.TempDir()
	path, err := Install(dir, "127.0.0.1:7778")
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if ok, _ := filepath.Match(filepath.Join(dir, ConfigPattern), path); !ok {
		t.Errorf("Install returned %q, want a %s file in %q", path, ConfigPattern, dir)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the configuration: %v", err)
	}
	if !strings.Contains(string(b), `path: "`+filepath.Join(dir, DatastoreFile)+`"`) {
		t.Errorf("the datastore path in the config is not the absolute one:\n%s", b)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("Install left %d entries in the directory, want only the config", len(entries))
	}
	// A second run gets a file of its own and leaves the first one as it was:
	// two starts against one datastore cannot rewrite each other's config.
	second, err := Install(dir, "127.0.0.1:7779")
	if err != nil {
		t.Fatalf("second Install: %v", err)
	}
	if second == path {
		t.Fatalf("both runs got %q", path)
	}
	if b, _ = os.ReadFile(path); !strings.Contains(string(b), "127.0.0.1:7778") || strings.Contains(string(b), "127.0.0.1:7779") {
		t.Errorf("the second Install changed the first run's config:\n%s", b)
	}
	if b, _ = os.ReadFile(second); !strings.Contains(string(b), "127.0.0.1:7779") {
		t.Errorf("the second run's config does not hold its listener:\n%s", b)
	}
}

func TestInstallLeavesNothingBehindWhenItCannotWrite(t *testing.T) {
	dir := t.TempDir()
	if _, err := Install(filepath.Join(dir, "missing"), "127.0.0.1:6667"); err == nil {
		t.Error("Install into a directory that is not there succeeded")
	}
	if _, err := Install("relative", "127.0.0.1:6667"); err == nil {
		t.Error("Install into a relative directory succeeded")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("a failed Install left %d entries behind", len(entries))
	}
}
