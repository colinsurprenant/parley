package main

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAppendAndRemoveExcludeLine: the exclude file is only appended to, and the
// line comes out again with the rest as it was.
func TestAppendAndRemoveExcludeLine(t *testing.T) {
	const line = "/.mcp.json"
	for _, c := range []struct {
		name, content string
		exists        bool
		wantAdded     bool
		wantSep       bool
		wantAfterAdd  string
	}{
		{"no file", "", false, true, false, "/.mcp.json\n"},
		{"an empty file", "", true, true, false, "/.mcp.json\n"},
		{"a terminated file", "a\nb\n", true, true, false, "a\nb\n/.mcp.json\n"},
		{"an unterminated file", "a\nb", true, true, true, "a\nb\n/.mcp.json\n"},
		{"the line is there", "a\n/.mcp.json\nb\n", true, false, false, "a\n/.mcp.json\nb\n"},
		{"the line is there, last and unterminated", "a\n/.mcp.json", true, false, false, "a\n/.mcp.json"},
		{"lookalikes only", "/.mcp.json.bak\n .mcp.json\n/.mcp.json \n", true, true, false, "/.mcp.json.bak\n .mcp.json\n/.mcp.json \n/.mcp.json\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "exclude")
			if c.exists {
				if err := os.WriteFile(path, []byte(c.content), 0o640); err != nil {
					t.Fatal(err)
				}
			}
			added, sep, note, err := appendExcludeLine(path, line)
			if err != nil || note != "" || added != c.wantAdded || sep != c.wantSep {
				t.Fatalf("appendExcludeLine = %v, %v, %q, %v; want %v, %v, no note", added, sep, note, err, c.wantAdded, c.wantSep)
			}
			if got := string(mustRead(t, path)); got != c.wantAfterAdd {
				t.Fatalf("after the append: %q, want %q", got, c.wantAfterAdd)
			}
			if !added {
				return
			}
			removed, err := removeExcludeLine(path, line, sep)
			if err != nil || !removed {
				t.Fatalf("removeExcludeLine = %v, %v", removed, err)
			}
			if got := string(mustRead(t, path)); got != c.content {
				t.Errorf("after the removal: %q, want the file as it was: %q", got, c.content)
			}
		})
	}
}

// TestRemoveExcludeLineTakesOnlyTheFirstExactLine.
func TestRemoveExcludeLineTakesOnlyTheFirstExactLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "exclude")
	if err := os.WriteFile(path, []byte("x\n/.mcp.json\ny\n/.mcp.json\nz"), 0o644); err != nil {
		t.Fatal(err)
	}
	if removed, err := removeExcludeLine(path, "/.mcp.json", false); err != nil || !removed {
		t.Fatalf("= %v, %v", removed, err)
	}
	if got := string(mustRead(t, path)); got != "x\ny\n/.mcp.json\nz" {
		t.Errorf("file = %q", got)
	}
	if removed, err := removeExcludeLine(path, "/.codex/config.toml", false); err != nil || removed {
		t.Errorf("a line that is not there: %v, %v", removed, err)
	}
	if removed, err := removeExcludeLine(filepath.Join(t.TempDir(), "none"), "x", false); err != nil || removed {
		t.Errorf("a file that is not there: %v, %v", removed, err)
	}
}

// TestExcludeFilesAreNotFollowedThroughLinks: a link is a note for the add and an
// error for the removal, and the file it points at is not touched.
func TestExcludeFilesAreNotFollowedThroughLinks(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("/.mcp.json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "exclude")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if added, _, note, err := appendExcludeLine(link, "/x"); err != nil || added || !strings.Contains(note, "no exclude line added") {
		t.Errorf("append through a link = %v, %q, %v", added, note, err)
	}
	if _, err := removeExcludeLine(link, "/.mcp.json", false); err == nil {
		t.Error("removal through a link did not fail")
	}
	if got := string(mustRead(t, target)); got != "/.mcp.json\n" {
		t.Errorf("the target = %q, want it untouched", got)
	}
	// A dangling link is a link too, and is not created through.
	dangling := filepath.Join(dir, "dangling")
	if err := os.Symlink(filepath.Join(dir, "nowhere"), dangling); err != nil {
		t.Fatal(err)
	}
	if added, _, _, err := appendExcludeLine(dangling, "/x"); err != nil || added {
		t.Errorf("append to a dangling link = %v, %v", added, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "nowhere")); err == nil {
		t.Error("the dangling link's target was created")
	}
	// A file past the bound is left alone.
	big := filepath.Join(dir, "big")
	if err := os.WriteFile(big, []byte(strings.Repeat("x\n", excludeMax)), 0o644); err != nil {
		t.Fatal(err)
	}
	if added, _, note, err := appendExcludeLine(big, "/x"); err != nil || added || !strings.Contains(note, "larger") {
		t.Errorf("append to a huge file = %v, %q, %v", added, note, err)
	}
}

// goodLineRecord is a record `line new` and two `line add`s could have written
// under base.
func goodLineRecord(base string) lineRecord {
	co := "/work/co"
	return lineRecord{
		ID: "demo", Stream: "irc://127.0.0.1:6667/#demo", Server: "127.0.0.1:6667", StateDir: base,
		Chair: "colin", Cap: 8, Window: "10m", Created: "2026-01-01T00:00:00Z",
		Sessions: []lineSession{
			{Nick: "plan", Harness: "claude", Model: "opus", Checkout: co,
				Files: []lineFile{{Path: co + "/.mcp.json", SHA256: "sha256:" + strings.Repeat("a", 64), ExcludeFile: co + "/.git/info/exclude"}}},
			{Nick: "build", Harness: "codex", Checkout: co, Thread: fakeThread(1),
				ReplyDir: "/tmp/parley-line-demo-123",
				Files:    []lineFile{{Path: co + "/.codex/config.toml", SHA256: "sha256:" + strings.Repeat("b", 64), MadeDir: true}}},
		},
	}
}

// TestLineRecordCheckRefusesWhatTheVerbsDidNotWrite: line.json is read through a
// bound and held to the shape the verbs write, because the paths in it are the
// ones a drop acts on.
func TestLineRecordCheckRefusesWhatTheVerbsDidNotWrite(t *testing.T) {
	base := t.TempDir()
	write := func(t *testing.T, raw []byte) {
		t.Helper()
		dir := filepath.Join(base, "lines", "demo")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "line.json"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	encode := func(t *testing.T, r lineRecord) []byte {
		t.Helper()
		raw, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	write(t, encode(t, goodLineRecord(base)))
	if rec, raw, err := readLineRecord(base, "demo"); err != nil || len(rec.Sessions) != 2 || len(raw) == 0 {
		t.Fatalf("a good record is refused: %v", err)
	}
	for _, c := range []struct {
		name   string
		mutate func(r *lineRecord)
		want   string
	}{
		{"another id", func(r *lineRecord) { r.ID = "other" }, `it is line "other", not "demo"`},
		{"another state directory", func(r *lineRecord) { r.StateDir = "/elsewhere" }, "state directory"},
		{"a relative state directory", func(r *lineRecord) { r.StateDir = "rel" }, "state directory"},
		{"a stream on another server", func(r *lineRecord) { r.Server = "127.0.0.1:7000" }, "is not on server"},
		{"no stream", func(r *lineRecord) { r.Stream = "" }, "is not on server"},
		{"a chair that is not a nick", func(r *lineRecord) { r.Chair = "co lin" }, "chair"},
		{"a negative cap", func(r *lineRecord) { r.Cap = -1 }, "negative"},
		{"a window that is not a duration", func(r *lineRecord) { r.Window = "soon" }, "window"},
		{"a window of zero", func(r *lineRecord) { r.Window = "0s" }, "window"},
		{"a relative codex_bin", func(r *lineRecord) { r.CodexBin = "codex" }, "codex_bin"},
		{"a session nick that is the chair's", func(r *lineRecord) { r.Sessions[0].Nick = "Colin" }, "chair's"},
		{"a session nick twice", func(r *lineRecord) { r.Sessions[1].Nick = "PLAN" }, "also session"},
		{"a session nick that is reserved", func(r *lineRecord) { r.Sessions[0].Nick = "lines" }, "nick"},
		{"an unknown harness", func(r *lineRecord) { r.Sessions[0].Harness = "gemini" }, "harness"},
		{"a model with a flag in it", func(r *lineRecord) { r.Sessions[0].Model = "-x" }, "model"},
		{"a relative checkout", func(r *lineRecord) { r.Sessions[0].Checkout = "co" }, "checkout"},
		{"a thread that is not a UUID", func(r *lineRecord) { r.Sessions[1].Thread = "nope" }, "UUID"},
		{"a thread on a Claude session", func(r *lineRecord) { r.Sessions[0].Thread = fakeThread(2) }, "UUID"},
		{"a Codex session with no thread", func(r *lineRecord) { r.Sessions[1].Thread = "" }, "no thread"},
		{"a reply directory of another line", func(r *lineRecord) { r.Sessions[1].ReplyDir = "/tmp/parley-line-other-123" }, "reply directory"},
		{"a reply directory on a Claude session", func(r *lineRecord) { r.Sessions[0].ReplyDir = "/tmp/parley-line-demo-1" }, "reply directory"},
		{"no launch file", func(r *lineRecord) { r.Sessions[0].Files = nil }, "exactly one"},
		{"two launch files", func(r *lineRecord) { r.Sessions[0].Files = append(r.Sessions[0].Files, r.Sessions[0].Files[0]) }, "exactly one"},
		{"a launch file that is not the session's", func(r *lineRecord) { r.Sessions[0].Files[0].Path = "/etc/passwd" }, "launch file"},
		{"a Claude session with the Codex file", func(r *lineRecord) { r.Sessions[0].Files[0].Path = "/work/co/.codex/config.toml" }, "launch file"},
		{"a digest that is not one", func(r *lineRecord) { r.Sessions[0].Files[0].SHA256 = "sha256:abc" }, "digest"},
		{"an upper-case digest", func(r *lineRecord) { r.Sessions[0].Files[0].SHA256 = "sha256:" + strings.Repeat("A", 64) }, "digest"},
		{"a relative exclude file", func(r *lineRecord) { r.Sessions[0].Files[0].ExcludeFile = "info/exclude" }, "exclude_file"},
		{"a made .codex on a Claude file", func(r *lineRecord) { r.Sessions[0].Files[0].MadeDir = true }, "directory the line made"},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := goodLineRecord(base)
			rec.Sessions = append([]lineSession(nil), rec.Sessions...)
			for i := range rec.Sessions {
				rec.Sessions[i].Files = append([]lineFile(nil), rec.Sessions[i].Files...)
			}
			c.mutate(&rec)
			write(t, encode(t, rec))
			_, _, err := readLineRecord(base, "demo")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("readLineRecord = %v, want an error saying %q", err, c.want)
			}
		})
	}
	t.Run("not JSON", func(t *testing.T) {
		write(t, []byte("{"))
		if _, _, err := readLineRecord(base, "demo"); err == nil || !strings.Contains(err.Error(), "is not valid") {
			t.Fatalf("= %v", err)
		}
	})
	t.Run("larger than the bound", func(t *testing.T) {
		write(t, []byte(strings.Repeat(" ", lineRecordMax+1)))
		if _, _, err := readLineRecord(base, "demo"); err == nil || !strings.Contains(err.Error(), "larger than") {
			t.Fatalf("= %v", err)
		}
	})
	t.Run("a link", func(t *testing.T) {
		path := filepath.Join(base, "lines", "demo", "line.json")
		real := filepath.Join(base, "real.json")
		if err := os.WriteFile(real, encode(t, goodLineRecord(base)), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, path); err != nil {
			t.Fatal(err)
		}
		if _, _, err := readLineRecord(base, "demo"); err == nil || !strings.Contains(err.Error(), "link") {
			t.Fatalf("= %v", err)
		}
	})
	t.Run("not there", func(t *testing.T) {
		if _, _, err := readLineRecord(base, "other"); err == nil || !strings.Contains(err.Error(), `no line "other"`) {
			t.Fatalf("= %v", err)
		}
	})
}

// TestWriteLineRecordRefusesOneTheReadersWouldRefuse: setup never leaves a record
// that the teardown would refuse to read.
func TestWriteLineRecordRefusesOneTheReadersWouldRefuse(t *testing.T) {
	dir := t.TempDir()
	rec := goodLineRecord(dir)
	rec.Sessions[0].Checkout = "/" + strings.Repeat("c", lineRecordMax)
	if _, err := writeLineRecord(dir, rec); err == nil || !strings.Contains(err.Error(), "over the") {
		t.Fatalf("writeLineRecord = %v, want a refusal for its size", err)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("a refused write left %d entries", len(ents))
	}
	// A record with no sessions is a list, not null.
	rec = goodLineRecord(dir)
	rec.Sessions = nil
	raw, err := writeLineRecord(dir, rec)
	if err != nil || !strings.Contains(string(raw), `"sessions": []`) {
		t.Fatalf("= %q, %v", raw, err)
	}
}

// TestParseInterleavedTakesPositionalsAnywhere.
func TestParseInterleavedTakesPositionalsAnywhere(t *testing.T) {
	for _, args := range [][]string{
		{"demo", "plan", "--force", "--state-dir", "x"},
		{"--force", "demo", "--state-dir", "x", "plan"},
		{"demo", "--force", "plan", "--state-dir", "x"},
		{"--state-dir", "x", "--force", "demo", "plan"},
	} {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		force := fs.Bool("force", false, "")
		dir := fs.String("state-dir", "", "")
		pos, err := parseInterleaved(fs, args)
		if err != nil || strings.Join(pos, ",") != "demo,plan" || !*force || *dir != "x" {
			t.Errorf("%v: positionals %v, force %v, state-dir %q, %v", args, pos, *force, *dir, err)
		}
	}
}

// TestUsageDocumentsTheLineVerbs: the top usage lists the four verbs and says
// what each does, and each verb answers -h on stdout with exit 0.
func TestUsageDocumentsTheLineVerbs(t *testing.T) {
	top := mustRun(t, "", "-h").stdout
	for _, want := range []string{
		"parley line    new <id> --chair <nick> --cap <n> [--window 10m]",
		"parley line    add <id> --nick <nick> --harness claude|codex [--model <model>] --checkout <absolute dir>",
		"parley line    drop <id> <nick> [--force]",
		"parley line    end <id> [--force]",
		"line     set up a line, put sessions on it, take them off and take it down",
		"lines/<id>/line.json under the state base",
		"sha256 of its exact bytes",
		"turns network on for Codex in that checkout",
		"codex-thread file --thread auto reads",
		"git rev-parse --git-path info/exclude",
		"exact line and nothing else",
		"a failure partway leaves",
		"participant is called lines",
	} {
		if !strings.Contains(top, want) {
			t.Errorf("usage does not mention %q", want)
		}
	}
	for _, verb := range []string{"new", "add", "drop", "end"} {
		help := runParley(t, "", "line", verb, "-h")
		if help.code != 0 {
			t.Errorf("line %s -h exited %d", verb, help.code)
		}
		if !strings.Contains(help.stdout, "usage: parley line "+verb) || help.stderr != "" {
			t.Errorf("line %s -h prints %q on stdout, %q on stderr", verb, help.stdout, help.stderr)
		}
	}
	all := mustRun(t, "", "line", "-h").stdout
	for _, verb := range []string{"new", "add", "drop", "end"} {
		if !strings.Contains(all, "parley line "+verb) {
			t.Errorf("line -h does not list %s: %q", verb, all)
		}
	}
	if res := runParley(t, ""); res.code != 2 {
		t.Errorf("bare parley exited %d", res.code)
	}
	if res := runParley(t, "", "line"); res.code != 2 || !strings.Contains(res.stderr, "needs a verb") {
		t.Errorf("parley line exited %d: %s", res.code, res.stderr)
	}
	if res := runParley(t, "", "line", "frob"); res.code != 2 || !strings.Contains(res.stderr, "unknown line verb") {
		t.Errorf("parley line frob exited %d: %s", res.code, res.stderr)
	}
}
