package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/colinsurprenant/parley/internal/state"
)

func (b *lineBench) lset(id string, extra ...string) []string {
	return append([]string{"line", "set", id, "--state-dir", b.base}, extra...)
}

// mcpArgsWith is mcpArgs for a line whose cap and window are these.
func (b *lineBench) mcpArgsWith(id, capN, window string) []string {
	args := b.mcpArgs(id)
	for i, a := range args {
		switch a {
		case "--cap":
			args[i+1] = capN
		case "--window":
			args[i+1] = window
		}
	}
	return args
}

// readMCP is the command and the args of the one server a .mcp.json holds.
func readMCP(t *testing.T, path string) (string, []string) {
	t.Helper()
	var doc struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	raw := mustRead(t, path)
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("%s is not JSON: %v\n%s", path, err, raw)
	}
	srv, ok := doc.MCPServers["parley"]
	if !ok || len(doc.MCPServers) != 1 {
		t.Fatalf("%s holds servers %v, want only parley", path, doc.MCPServers)
	}
	return srv.Command, srv.Args
}

func (b *lineBench) wantMCP(t *testing.T, path, id, capN, window string) {
	t.Helper()
	cmd, args := readMCP(t, path)
	if want := b.mcpArgsWith(id, capN, window); cmd != binPath || strings.Join(args, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("%s runs %q with\n%q\nwant %q with\n%q", path, cmd, args, binPath, want)
	}
}

// wantNoTempFiles fails if a directory holds what an interrupted atomic write leaves.
func wantNoTempFiles(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		ents, err := os.ReadDir(d)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range ents {
			if strings.HasPrefix(e.Name(), ".tmp-") {
				t.Errorf("%s holds the temp file %s", d, e.Name())
			}
		}
	}
}

// TestLineSetRaisesTheCapOfAClaudeAndACodexSession: the Claude session's .mcp.json
// is written again with the new cap and nothing else changed, the digest the record
// holds for it follows, so that `line drop` still shows it is the line's; the Codex
// config, which carries neither number, is left as it is; the record holds the new
// cap, the window it had, and the output says what each session has to do.
func TestLineSetRaisesTheCapOfAClaudeAndACodexSession(t *testing.T) {
	b := newLineBench(t)
	lineUp(t, b, "demo")
	co1, co2 := b.gitWorkdir(t, "co-plan"), b.gitWorkdir(t, "co-build")
	pristine1, pristine2 := treeListing(t, co1), treeListing(t, co2)
	addPlan := b.mustAdd(t, "demo", "plan", "claude", co1, "--model", "opus")
	b.mustAdd(t, "demo", "build", "codex", co2)
	mcp := filepath.Join(co1, ".mcp.json")
	cfg := filepath.Join(co2, ".codex", "config.toml")
	cfgBefore := mustRead(t, cfg)
	before := b.record(t, "demo")
	if fi, err := os.Stat(mcp); err != nil || fi.Mode().Perm() != 0o644 {
		t.Fatalf("%s: %v, %v", mcp, fi, err)
	}

	res := mustLine(t, b, b.lset("demo", "--cap", "30")...)
	t.Logf("stdout:\n%s", res.stdout)
	t.Logf("stderr:\n%s", res.stderr)

	b.wantMCP(t, mcp, "demo", "30", "10m")
	if fi, err := os.Stat(mcp); err != nil || fi.Mode().Perm() != 0o644 {
		t.Errorf("%s mode %v (%v), want 0644 as it was", mcp, fi, err)
	}
	if got := mustRead(t, cfg); string(got) != string(cfgBefore) {
		t.Errorf("the Codex config changed:\n%s\nwas\n%s", got, cfgBefore)
	}
	wantNoTempFiles(t, co1, filepath.Dir(cfg), co2)
	if got := gitIn(t, co1, "status", "--porcelain"); got != "" {
		t.Errorf("the working directory is not clean: %s", got)
	}

	rec := b.record(t, "demo")
	if rec.Cap != 30 || rec.Window != "10m" {
		t.Errorf("record cap %d window %q, want 30 and 10m", rec.Cap, rec.Window)
	}
	plan, build := sessionByNick(t, rec, "plan"), sessionByNick(t, rec, "build")
	oldPlan := sessionByNick(t, before, "plan")
	if f := plan.Files[0]; f.SHA256 != fileDigestOf(t, mcp) || f.SHA256 == oldPlan.Files[0].SHA256 || f.ExcludeFile != oldPlan.Files[0].ExcludeFile {
		t.Errorf("plan's file entry %+v, want the new digest %s and the rest as it was (%+v)", f, fileDigestOf(t, mcp), oldPlan.Files[0])
	}
	if fmt.Sprint(build) != fmt.Sprint(sessionByNick(t, before, "build")) {
		t.Errorf("build's session changed: %+v", build)
	}

	pump := fmt.Sprintf("'%s' codex --stream '%s' --nick build --state-dir '%s' --thread auto --skill line --chair colin --cap 30 --window 10m", binPath, b.stream("demo"), b.base)
	wantInOrder(t, res.stdout, []string{
		"Line demo now has cap 30 (was 8) and window 10m (unchanged).",
		"Rewrote " + mcp + " (session plan).",
		"Left " + cfg + " as it is (session build): it carries neither number.",
		"Updated " + filepath.Join(b.base, "lines", "demo", "line.json") + ".",
		"Nothing that is running changes yet: each session's front process reads its cap and window once, when it starts.",
		"plan (Claude Code): restart it, resuming its conversation:",
		fmt.Sprintf("cd '%s'", co1),
		"PARLEY_NICK=plan claude --dangerously-load-development-channels server:parley --model 'opus' --resume <session-id>",
		"build (Codex): stop its pump, which holds the old numbers, and start this one in its place; the Codex session itself keeps running:",
		pump,
		"Each session's primer still states the old numbers, a Claude Code session's channel instructions too, and a session started outside line add is not in the record, so it was not touched.",
	})
	if strings.Contains(res.stdout, "codex resume") {
		t.Errorf("a Codex session is not resumed again:\n%s", res.stdout)
	}
	// The Claude launch is the one `line add` printed, development-channels flag
	// included, and nothing speaks of reconnecting a server.
	if resume := lineWith(t, addPlan.stdout, "claude --dangerously-load-development-channels", "--resume <session-id>"); !strings.Contains(res.stdout, resume) || !strings.Contains(resume, "--dangerously-load-development-channels server:parley") {
		t.Errorf("the restart line is not the one line add printed (%q):\n%s", resume, res.stdout)
	}
	if strings.Contains(strings.ToLower(res.stdout), "reconnect") {
		t.Errorf("the output tells a session to reconnect:\n%s", res.stdout)
	}

	// The digests are what `line drop` proves a file is the line's by.
	mustLine(t, b, b.ldrop("demo", "plan")...)
	mustLine(t, b, b.ldrop("demo", "build")...)
	if got := treeListing(t, co1); got != pristine1 {
		t.Errorf("co-plan is not as it was.\nbefore:\n%s\nafter:\n%s", pristine1, got)
	}
	if got := treeListing(t, co2); got != pristine2 {
		t.Errorf("co-build is not as it was.\nbefore:\n%s\nafter:\n%s", pristine2, got)
	}
}

// runBinaryIn runs the parley binary at bin, which is not the one the tests built,
// in the bench's environment.
func (b *lineBench) runBinaryIn(t *testing.T, bin string, args ...string) result {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), b.env()...)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	res := result{}
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("run %s %v: %v", bin, args, err)
		}
		res.code = exit.ExitCode()
	}
	res.stdout, res.stderr = stdout.String(), stderr.String()
	return res
}

// TestLineSetWarnsWhenTheCommandPathChanges: a launch file that runs one parley
// binary, rewritten by another (a dev build, a moved binary), is named with both
// paths before anything is said of the sessions, and the run goes on; run again
// by the binary the file now names, it says nothing.
func TestLineSetWarnsWhenTheCommandPathChanges(t *testing.T) {
	b := newLineBench(t)
	lineUp(t, b, "demo")
	co := b.gitWorkdir(t, "co")
	b.mustAdd(t, "demo", "plan", "claude", co)
	mcp := filepath.Join(co, ".mcp.json")
	if cmd, _ := readMCP(t, mcp); cmd != binPath {
		t.Fatalf("line add wrote the command %q, want %q", cmd, binPath)
	}
	dev := filepath.Join(b.root, "parley-dev")
	if err := os.WriteFile(dev, mustRead(t, binPath), 0o755); err != nil {
		t.Fatal(err)
	}

	res := b.runBinaryIn(t, dev, b.lset("demo", "--cap", "30")...)
	if res.code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", res.code, res.stdout, res.stderr)
	}
	// The command is whatever path the other binary knows itself by.
	cmd, _ := readMCP(t, mcp)
	if cmd == binPath || filepath.Base(cmd) != "parley-dev" {
		t.Fatalf("the rewritten file runs %q, want the other binary", cmd)
	}
	note := fmt.Sprintf("Note: the parley command in %s is %s; this run writes %s, the binary running line set.", mcp, binPath, cmd)
	wantInOrder(t, res.stdout, []string{
		note,
		"Line demo now has cap 30 (was 8) and window 10m (unchanged).",
		"Rewrote " + mcp + " (session plan).",
		"plan (Claude Code): restart it, resuming its conversation:",
	})
	if got := strings.Count(res.stdout, "Note:"); got != 1 {
		t.Errorf("%d notes, want one:\n%s", got, res.stdout)
	}
	if rec := b.record(t, "demo"); rec.Cap != 30 || sessionByNick(t, rec, "plan").Files[0].SHA256 != fileDigestOf(t, mcp) {
		t.Errorf("the run did not go on: %+v", rec)
	}

	// The file runs this binary now, so the same binary has nothing to say.
	res = b.runBinaryIn(t, dev, b.lset("demo", "--cap", "40")...)
	if res.code != 0 || strings.Contains(res.stdout, "Note:") || strings.Contains(res.stderr, "Note:") {
		t.Errorf("a second run by the same binary: exit %d\nstdout: %s\nstderr: %s", res.code, res.stdout, res.stderr)
	}
	if cmd2, args := readMCP(t, mcp); cmd2 != cmd || strings.Join(args, "\x00") != strings.Join(b.mcpArgsWith("demo", "40", "10m"), "\x00") {
		t.Errorf("the file runs %q with %q", cmd2, args)
	}
}

// TestLineSetStaysQuietWhenTheCommandPathIsTheSame: the binary that wrote the file
// is the one that rewrites it, and nothing is said of its path.
func TestLineSetStaysQuietWhenTheCommandPathIsTheSame(t *testing.T) {
	b := newLineBench(t)
	lineUp(t, b, "demo")
	co1, co2 := b.gitWorkdir(t, "co-plan"), b.gitWorkdir(t, "co-build")
	b.mustAdd(t, "demo", "plan", "claude", co1)
	b.mustAdd(t, "demo", "build", "codex", co2)
	for _, args := range [][]string{b.lset("demo", "--cap", "30"), b.lset("demo", "--window", "5m")} {
		res := mustLine(t, b, args...)
		if strings.Contains(res.stdout, "Note:") || strings.Contains(res.stdout, "parley command") || strings.Contains(res.stderr, "Note:") {
			t.Errorf("%v said something of the command path:\nstdout: %s\nstderr: %s", args[3:], res.stdout, res.stderr)
		}
	}
}

// TestLaunchSettingsReadOnlyBytesThatHashToTheDigest: the command and the numbers are
// read from the file only when its bytes are the ones the record has a digest of.
func TestLaunchSettingsReadOnlyBytesThatHashToTheDigest(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".mcp.json")
	data := []byte(`{"mcpServers":{"parley":{"command":"/opt/parley","args":["channel","--cap","30","--window","5m","--chair","boss"]}}}`)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readLaunchSettings(lineFile{Path: path, SHA256: tableDigest(data)}); got != (launchSettings{command: "/opt/parley", numbers: true, capN: 30, window: "5m"}) {
		t.Errorf("settings %+v, want the command /opt/parley and cap 30, window 5m", got)
	}
	if got := readLaunchSettings(lineFile{Path: path, SHA256: tableDigest([]byte("other"))}); got != (launchSettings{}) {
		t.Errorf("settings %+v from bytes that do not hash to the digest, want none", got)
	}
	if got := readLaunchSettings(lineFile{Path: path + ".gone", SHA256: tableDigest(data)}); got != (launchSettings{}) {
		t.Errorf("settings %+v from a file that is not there, want none", got)
	}
	// Bytes that hash to the digest and lack a number, or hold one that is not a
	// number, carry no numbers: the file is then written again, never taken as current.
	for name, doc := range map[string]string{
		"no window":         `{"mcpServers":{"parley":{"command":"/opt/parley","args":["--cap","30"]}}}`,
		"no cap":            `{"mcpServers":{"parley":{"command":"/opt/parley","args":["--window","5m"]}}}`,
		"a cap that is not": `{"mcpServers":{"parley":{"command":"/opt/parley","args":["--cap","many","--window","5m"]}}}`,
		"a flag at the end": `{"mcpServers":{"parley":{"command":"/opt/parley","args":["--window","5m","--cap"]}}}`,
	} {
		if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := readLaunchSettings(lineFile{Path: path, SHA256: tableDigest([]byte(doc))}); got.numbers || got.command != "/opt/parley" {
			t.Errorf("%s: settings %+v, want the command and no numbers", name, got)
		}
	}
	bad := []byte(`{"mcpServers":`)
	if err := os.WriteFile(path, bad, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readLaunchSettings(lineFile{Path: path, SHA256: tableDigest(bad)}); got != (launchSettings{}) {
		t.Errorf("settings %+v from JSON that is cut off, want none", got)
	}
}

// TestLineSetWindowAloneChangesOnlyTheWindow: the cap is the one the record had, in
// the file and in the record, and the output says it is unchanged.
func TestLineSetWindowAloneChangesOnlyTheWindow(t *testing.T) {
	b := newLineBench(t)
	lineUp(t, b, "demo")
	co := b.gitWorkdir(t, "co")
	b.mustAdd(t, "demo", "plan", "claude", co)
	mcp := filepath.Join(co, ".mcp.json")

	res := mustLine(t, b, b.lset("demo", "--window", "15m")...)
	b.wantMCP(t, mcp, "demo", "8", "15m")
	rec := b.record(t, "demo")
	if rec.Cap != 8 || rec.Window != "15m" {
		t.Errorf("record cap %d window %q, want 8 and 15m", rec.Cap, rec.Window)
	}
	if f := rec.Sessions[0].Files[0]; f.SHA256 != fileDigestOf(t, mcp) {
		t.Errorf("file entry %+v, want digest %s", f, fileDigestOf(t, mcp))
	}
	if !strings.Contains(res.stdout, "Line demo now has cap 8 (unchanged) and window 15m (was 10m).") {
		t.Errorf("stdout:\n%s", res.stdout)
	}

	// A window that is a number of seconds is recorded and written as a person writes it.
	mustLine(t, b, b.lset("demo", "--window", "90s", "--cap", "0")...)
	b.wantMCP(t, mcp, "demo", "0", "1m30s")
	if rec := b.record(t, "demo"); rec.Cap != 0 || rec.Window != "1m30s" {
		t.Errorf("record cap %d window %q, want 0 and 1m30s", rec.Cap, rec.Window)
	}
	mustLine(t, b, b.ldrop("demo", "plan")...)
}

// TestLineSetSaysWhenThereIsNothingToChange: the numbers the line has already, in the
// record and in every file, are not written again, and the run says nothing was
// changed only then.
func TestLineSetSaysWhenThereIsNothingToChange(t *testing.T) {
	b := newLineBench(t)
	lineUp(t, b, "demo")
	co := b.gitWorkdir(t, "co")
	b.mustAdd(t, "demo", "plan", "claude", co)
	before := b.lsnap(t, co)
	res := mustLine(t, b, b.lset("demo", "--cap", "8", "--window", "10m")...)
	if !strings.Contains(res.stdout, "Line demo already has cap 8 and window 10m, and no launch file needs a change: nothing was changed.") {
		t.Errorf("stdout:\n%s", res.stdout)
	}
	b.wantLineUnchanged(t, before, co)
}

// TestLineSetWritesWhatLineAddWould: the bytes of a rewritten file are the ones `line
// add` writes for a line that had these numbers from the start.
func TestLineSetWritesWhatLineAddWould(t *testing.T) {
	b := newLineBench(t)
	lineUp(t, b, "demo")
	co := b.gitWorkdir(t, "co")
	b.mustAdd(t, "demo", "plan", "claude", co, "--model", "opus")
	mustLine(t, b, b.lset("demo", "--cap", "30", "--window", "15m")...)
	mcp := filepath.Join(co, ".mcp.json")
	got := mustRead(t, mcp)

	mustLine(t, b, b.lend("demo")...)
	mustLine(t, b, b.lnew("demo", "--cap", "30", "--window", "15m")...)
	b.mustAdd(t, "demo", "plan", "claude", co, "--model", "opus")
	if want := mustRead(t, mcp); string(got) != string(want) {
		t.Errorf("line set wrote\n%s\nline add writes\n%s", got, want)
	}
}

// TestLineSetPrintsThePumpLinesLineAddPrints: a Codex and an OpenCode session's
// lines are `line add`'s, with the new numbers, and name the codex the line was made
// with; an OpenCode session has no launch file, so nothing is written for it.
func TestLineSetPrintsThePumpLinesLineAddPrints(t *testing.T) {
	b := newOCLineBench(t)
	lineUp(t, b.lineBench, "demo", "--codex-bin", b.codex)
	cx, oc := b.gitWorkdir(t, "co-build"), b.gitWorkdir(t, "co-review")
	addCodex := b.mustAdd(t, "demo", "build", "codex", cx)
	addOC := mustLine(t, b.lineBench, b.ladd("demo", "review", "opencode", oc)...)
	wantCodex := strings.Replace(lineWith(t, addCodex.stdout, " codex --stream "), "--cap 8", "--cap 30", 1)
	wantOC := strings.Replace(lineWith(t, addOC.stdout, " opencode --stream "), "--cap 8", "--cap 30", 1)
	if !strings.Contains(wantCodex, "--cap 30") || !strings.Contains(wantCodex, "--codex-bin '"+b.codex+"'") || !strings.Contains(wantOC, "--cap 30") {
		t.Fatalf("the lines line add printed are not what the test expects:\n%s\n%s", wantCodex, wantOC)
	}
	codexCalls, ocCalls := b.lineBench.callCount(), b.callCount()
	treeOC := treeListing(t, oc)

	res := mustLine(t, b.lineBench, b.lset("demo", "--cap", "30")...)
	t.Logf("stdout:\n%s", res.stdout)
	wantInOrder(t, res.stdout, []string{
		"Line demo now has cap 30 (was 8) and window 10m (unchanged).",
		"Left " + filepath.Join(cx, ".codex", "config.toml") + " as it is (session build): it carries neither number.",
		"build (Codex): stop its pump, which holds the old numbers, and start this one in its place; the Codex session itself keeps running:",
		wantCodex,
		"review (OpenCode): stop its pump, which holds the old numbers, and start this one in its place; the OpenCode session itself keeps running:",
		wantOC,
	})
	if strings.Count(res.stdout, "Rewrote") != 0 || strings.Contains(res.stdout, oc+"/") {
		t.Errorf("a file was written, or the OpenCode working directory named as one:\n%s", res.stdout)
	}
	if got := treeListing(t, oc); got != treeOC {
		t.Errorf("the OpenCode working directory changed.\nbefore:\n%s\nafter:\n%s", treeOC, got)
	}
	// Nothing was run: neither codex nor opencode.
	if b.lineBench.callCount() != codexCalls || b.callCount() != ocCalls {
		t.Errorf("set ran a harness: codex calls %d (was %d), opencode calls %d (was %d)", b.lineBench.callCount(), codexCalls, b.callCount(), ocCalls)
	}
	if rec := b.record(t, "demo"); rec.Cap != 30 || rec.CodexBin != b.codex {
		t.Errorf("record cap %d codex_bin %q", rec.Cap, rec.CodexBin)
	}
}

// TestLineSetRewritesASharedFileOnce: two Claude sessions of one working directory
// share a file, which is written once, and the record holds its new digest under both.
func TestLineSetRewritesASharedFileOnce(t *testing.T) {
	b := newLineBench(t)
	lineUp(t, b, "demo")
	co := b.gitWorkdir(t, "co")
	b.mustAdd(t, "demo", "plan", "claude", co)
	b.mustAdd(t, "demo", "review", "claude", co)
	mcp := filepath.Join(co, ".mcp.json")
	before, err := os.Stat(mcp)
	if err != nil {
		t.Fatal(err)
	}

	res := mustLine(t, b, b.lset("demo", "--cap", "30")...)
	t.Logf("stdout:\n%s", res.stdout)
	b.wantMCP(t, mcp, "demo", "30", "10m")
	if got := strings.Count(res.stdout, "Rewrote"); got != 1 || !strings.Contains(res.stdout, "Rewrote "+mcp+" (sessions plan, review).") {
		t.Errorf("stdout says it rewrote %d files, want the shared one once:\n%s", got, res.stdout)
	}
	if after, err := os.Stat(mcp); err != nil || os.SameFile(before, after) {
		t.Errorf("the file was not replaced (or is gone): %v", err)
	}
	rec := b.record(t, "demo")
	want := fileDigestOf(t, mcp)
	for _, s := range rec.Sessions {
		if len(s.Files) != 1 || s.Files[0].SHA256 != want {
			t.Errorf("session %s holds %+v, want the digest %s", s.Nick, s.Files, want)
		}
	}
	if !strings.Contains(res.stdout, "plan (Claude Code)") || !strings.Contains(res.stdout, "review (Claude Code)") {
		t.Errorf("each session is told what to do:\n%s", res.stdout)
	}
	mustLine(t, b, b.ldrop("demo", "plan")...)
	mustLine(t, b, b.ldrop("demo", "review")...)
}

// TestLineSetRefusesBeforeAnyChange: a missing or invalid flag is exit 2 and the
// line, its files and its record are as they were.
func TestLineSetRefusesBeforeAnyChange(t *testing.T) {
	b := newLineBench(t)
	lineUp(t, b, "demo")
	co := b.gitWorkdir(t, "co")
	b.mustAdd(t, "demo", "plan", "claude", co)
	before := b.lsnap(t, co)
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"neither flag", b.lset("demo"), "--cap or --window is required"},
		{"a negative cap", b.lset("demo", "--cap", "-1"), "--cap must not be negative"},
		{"a cap that is no number", b.lset("demo", "--cap", "many"), "invalid value"},
		{"a window of zero", b.lset("demo", "--window", "0"), "--window must be above zero"},
		{"a negative window", b.lset("demo", "--window", "-5m"), "--window must be above zero"},
		{"a window that is no duration", b.lset("demo", "--window", "soon"), "invalid value"},
		{"a window with no unit", b.lset("demo", "--window", "5"), "invalid value"},
		{"a good cap and a bad window", b.lset("demo", "--cap", "30", "--window", "0s"), "--window must be above zero"},
		{"no id", []string{"line", "set", "--cap", "30", "--state-dir", b.base}, "the line id is required"},
		{"a bad id", b.lset("Demo!", "--cap", "30"), "must match"},
		{"a second id", b.lset("demo", "--cap", "30", "other"), `unexpected argument "other"`},
		{"an unknown flag", b.lset("demo", "--cap", "30", "--force"), "flag provided but not defined"},
	} {
		res := b.run(t, tc.args...)
		if res.code != 2 || !strings.Contains(res.stderr, tc.want) {
			t.Errorf("%s: exit %d, want 2 saying %q\nstdout: %s\nstderr: %s", tc.name, res.code, tc.want, res.stdout, res.stderr)
		}
		b.wantLineUnchanged(t, before, co)
	}
	// And a cap of 0 is not a refusal: it turns the breaker off.
	mustLine(t, b, b.lset("demo", "--cap", "0")...)
	b.wantMCP(t, filepath.Join(co, ".mcp.json"), "demo", "0", "10m")
}

// TestLineSetRefusesALaunchFileThatIsNotTheLines: one file edited, wherever it is in
// the record, refuses the whole run, names the file, and leaves every file, those
// before it in the record included, and the record as they were.
func TestLineSetRefusesALaunchFileThatIsNotTheLines(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tamper func(t *testing.T, mcp1, mcp2, cfg string) string
		want   string
	}{
		{"a Claude file edited", func(t *testing.T, mcp1, mcp2, cfg string) string {
			edit(t, mcp2, "\n")
			return mcp2
		}, "it was edited or replaced"},
		{"a Claude file replaced by one that is not the line's", func(t *testing.T, mcp1, mcp2, cfg string) string {
			if err := os.WriteFile(mcp2, []byte(`{"mcpServers":{}}`), 0o644); err != nil {
				t.Fatal(err)
			}
			return mcp2
		}, "it was edited or replaced"},
		{"the first Claude file edited", func(t *testing.T, mcp1, mcp2, cfg string) string {
			edit(t, mcp1, " ")
			return mcp1
		}, "it was edited or replaced"},
		{"a Codex config edited, which would not have been written", func(t *testing.T, mcp1, mcp2, cfg string) string {
			edit(t, cfg, "# mine\n")
			return cfg
		}, "it was edited or replaced"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newLineBench(t)
			lineUp(t, b, "demo")
			co1, co2, co3 := b.gitWorkdir(t, "co-a"), b.gitWorkdir(t, "co-b"), b.gitWorkdir(t, "co-c")
			b.mustAdd(t, "demo", "plan", "claude", co1)
			b.mustAdd(t, "demo", "review", "claude", co2)
			b.mustAdd(t, "demo", "build", "codex", co3)
			mcp1, mcp2 := filepath.Join(co1, ".mcp.json"), filepath.Join(co2, ".mcp.json")
			cfg := filepath.Join(co3, ".codex", "config.toml")
			bad := tc.tamper(t, mcp1, mcp2, cfg)
			before := b.lsnap(t, co1, co2, co3)

			res := b.run(t, b.lset("demo", "--cap", "30")...)
			if res.code != 1 || !strings.Contains(res.stderr, "will not rewrite "+bad+": ") || !strings.Contains(res.stderr, tc.want) || !strings.Contains(res.stderr, "nothing was changed") {
				t.Fatalf("exit %d, want 1 naming %s and saying %q\nstdout: %s\nstderr: %s", res.code, bad, tc.want, res.stdout, res.stderr)
			}
			if res.stdout != "" {
				t.Errorf("a refusal printed on stdout:\n%s", res.stdout)
			}
			b.wantLineUnchanged(t, before, co1, co2, co3)
			wantNoTempFiles(t, co1, co2, co3)
		})
	}
}

func edit(t *testing.T, path, more string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(more); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestLineSetRefusesALaunchFileThatIsALink: a link at the path, to a file with the
// very bytes the line wrote, is not the line's file, and neither is a file in a
// .codex that became a link. Nothing is written through either.
func TestLineSetRefusesALaunchFileThatIsALink(t *testing.T) {
	b := newLineBench(t)
	lineUp(t, b, "demo")
	co1, co2 := b.gitWorkdir(t, "co-plan"), b.gitWorkdir(t, "co-build")
	b.mustAdd(t, "demo", "plan", "claude", co1)
	b.mustAdd(t, "demo", "build", "codex", co2)
	mcp := filepath.Join(co1, ".mcp.json")

	elsewhere := filepath.Join(b.root, "elsewhere.json")
	if err := os.WriteFile(elsewhere, mustRead(t, mcp), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(mcp); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, mcp); err != nil {
		t.Fatal(err)
	}
	before := b.lsnap(t, co1, co2)
	res := b.run(t, b.lset("demo", "--cap", "30")...)
	if res.code != 1 || !strings.Contains(res.stderr, "will not rewrite "+mcp+": it is a symlink") {
		t.Fatalf("exit %d, want 1 naming the link\nstderr: %s", res.code, res.stderr)
	}
	b.wantLineUnchanged(t, before, co1, co2)
	if got, _ := os.Readlink(mcp); got != elsewhere || fileDigestOf(t, elsewhere) != sessionByNick(t, b.record(t, "demo"), "plan").Files[0].SHA256 {
		t.Errorf("the link now leads to %q, or its target was written", got)
	}

	// Put the file back, and make the Codex session's .codex a link.
	if err := os.Remove(mcp); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(elsewhere, mcp); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(b.root, "dot-codex")
	if err := os.Rename(filepath.Join(co2, ".codex"), moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, filepath.Join(co2, ".codex")); err != nil {
		t.Fatal(err)
	}
	before = b.lsnap(t, co1, co2)
	res = b.run(t, b.lset("demo", "--cap", "30")...)
	if res.code != 1 || !strings.Contains(res.stderr, "will not rewrite "+filepath.Join(co2, ".codex", "config.toml")+": its directory "+filepath.Join(co2, ".codex")+" is a symlink now") {
		t.Fatalf("exit %d, want 1 naming the linked directory\nstderr: %s", res.code, res.stderr)
	}
	b.wantLineUnchanged(t, before, co1, co2)
	b.wantMCP(t, mcp, "demo", "8", "10m")
}

// TestLineSetLeavesAFileThatIsNotThereNotThere: a launch file the user removed is
// not made again, and the record still gets the numbers.
func TestLineSetLeavesAFileThatIsNotThereNotThere(t *testing.T) {
	b := newLineBench(t)
	lineUp(t, b, "demo")
	co1, co2 := b.gitWorkdir(t, "co-plan"), b.gitWorkdir(t, "co-review")
	b.mustAdd(t, "demo", "plan", "claude", co1)
	b.mustAdd(t, "demo", "review", "claude", co2)
	gone, kept := filepath.Join(co1, ".mcp.json"), filepath.Join(co2, ".mcp.json")
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	goneDigest := sessionByNick(t, b.record(t, "demo"), "plan").Files[0].SHA256

	res := mustLine(t, b, b.lset("demo", "--cap", "30")...)
	if _, err := os.Lstat(gone); err == nil {
		t.Error("the file that was not there is there")
	}
	b.wantMCP(t, kept, "demo", "30", "10m")
	if !strings.Contains(res.stdout, gone+" is not there (session plan), so nothing was written for it.") || !strings.Contains(res.stdout, "Rewrote "+kept+" (session review).") {
		t.Errorf("stdout:\n%s", res.stdout)
	}
	rec := b.record(t, "demo")
	if got := sessionByNick(t, rec, "plan").Files[0].SHA256; rec.Cap != 30 || got != goneDigest {
		t.Errorf("record cap %d, plan's digest %s, want 30 and the one it had (%s)", rec.Cap, got, goneDigest)
	}
	mustLine(t, b, b.ldrop("demo", "plan")...)
	mustLine(t, b, b.ldrop("demo", "review")...)
}

// TestLineSetFailurePartWayIsReportedAndRunningAgainFinishes: the second of two files
// cannot be written. The first is, and its digest is recorded at once, with the
// numbers the record had; the output says which were and which were not, what the
// record holds, and that running line set again goes on; nothing is rolled back.
// Once the cause is gone, the command it printed finishes: the first file already
// has the numbers, so it is left, and the second is written.
func TestLineSetFailurePartWayIsReportedAndRunningAgainFinishes(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a directory's mode does not stop root writing in it")
	}
	b := newLineBench(t)
	lineUp(t, b, "demo")
	co1, co2 := b.gitWorkdir(t, "co-plan"), b.gitWorkdir(t, "co-review")
	b.mustAdd(t, "demo", "plan", "claude", co1)
	b.mustAdd(t, "demo", "review", "claude", co2)
	mcp1, mcp2 := filepath.Join(co1, ".mcp.json"), filepath.Join(co2, ".mcp.json")
	oldDigest2 := fileDigestOf(t, mcp2)

	if err := os.Chmod(co2, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(co2, 0o755) })
	res := b.run(t, b.lset("demo", "--cap", "30")...)
	if res.code != 1 {
		t.Fatalf("exit %d, want 1\nstdout: %s\nstderr: %s", res.code, res.stdout, res.stderr)
	}
	finish := b.setLine(binPath, 30, "10m")
	for _, want := range []string{
		"rewritten: " + mcp1 + ";", "not rewritten: " + mcp2 + ";",
		"line.json holds the new digest of " + mcp1 + ", and cap 8, window 10m, which are written last",
		"to go on, once the cause is cleared, run line set again with the numbers you want",
		"(" + finish + " repeats this run)",
	} {
		if !strings.Contains(res.stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, res.stderr)
		}
	}
	b.wantMCP(t, mcp1, "demo", "30", "10m")
	b.wantMCP(t, mcp2, "demo", "8", "10m")
	// The record is true after the first file: its digest is the file's, and the
	// numbers are the old ones; the second file is as it was.
	rec := b.record(t, "demo")
	if got := sessionByNick(t, rec, "plan").Files[0].SHA256; rec.Cap != 8 || rec.Window != "10m" || got != fileDigestOf(t, mcp1) {
		t.Errorf("record cap %d window %s, plan's digest %s, want 8, 10m and %s", rec.Cap, rec.Window, got, fileDigestOf(t, mcp1))
	}
	if got := sessionByNick(t, rec, "review").Files[0].SHA256; got != oldDigest2 || got != fileDigestOf(t, mcp2) {
		t.Errorf("review's digest %s, want the file's own, %s", got, oldDigest2)
	}
	wantNoTempFiles(t, co1, co2)

	if err := os.Chmod(co2, 0o755); err != nil {
		t.Fatal(err)
	}
	// The line the failure printed runs as it stands.
	res = b.runBinaryIn(t, "sh", "-c", finish)
	if res.code != 0 {
		t.Fatalf("the printed command exited %d\nstdout: %s\nstderr: %s", res.code, res.stdout, res.stderr)
	}
	t.Logf("stdout:\n%s", res.stdout)
	for _, want := range []string{
		"Left " + mcp1 + " as it is (session plan): it already has cap 30 and window 10m.",
		"Rewrote " + mcp2 + " (session review).",
	} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, res.stdout)
		}
	}
	b.wantMCP(t, mcp2, "demo", "30", "10m")
	rec = b.record(t, "demo")
	if got := sessionByNick(t, rec, "plan").Files[0].SHA256; rec.Cap != 30 || got != fileDigestOf(t, mcp1) || sessionByNick(t, rec, "review").Files[0].SHA256 != fileDigestOf(t, mcp2) || fileDigestOf(t, mcp2) == oldDigest2 {
		t.Errorf("record %+v", rec)
	}
	mustLine(t, b, b.ldrop("demo", "plan")...)
	mustLine(t, b, b.ldrop("demo", "review")...)
}

// TestLineSetOnALineThatIsNotThere: exit 2, naming it, with no lock made.
func TestLineSetOnALineThatIsNotThere(t *testing.T) {
	b := newLineBench(t)
	res := b.run(t, b.lset("nope", "--cap", "30")...)
	if res.code != 2 || !strings.Contains(res.stderr, `no line "nope"`) {
		t.Errorf("exit %d, want 2 naming the line\n%s", res.code, res.stderr)
	}
	if _, err := os.Stat(filepath.Join(b.base, ".ids")); err == nil {
		t.Error("set on a line that is not there made the id lock")
	}
}

// TestLineSetHoldsTheIDLock: with the id's lock held by another command, set refuses
// at once, as the other line verbs do, and changes nothing; with it free, it runs,
// and leaves it free.
func TestLineSetHoldsTheIDLock(t *testing.T) {
	b := newLineBench(t)
	lineUp(t, b, "demo")
	co := b.gitWorkdir(t, "co")
	b.mustAdd(t, "demo", "plan", "claude", co)
	before := b.lsnap(t, co)
	l, err := state.LockID(b.base, "demo")
	if err != nil {
		t.Fatal(err)
	}
	res := b.run(t, b.lset("demo", "--cap", "30")...)
	if res.code != 1 || !strings.Contains(res.stderr, idInUse) || !strings.Contains(res.stderr, "line command (new, add, set, drop or end)") {
		t.Errorf("set with the id's lock held: exit %d\n%s", res.code, res.stderr)
	}
	b.wantLineUnchanged(t, before, co)
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	mustLine(t, b, b.lset("demo", "--cap", "30")...)
	free, err := state.LockID(b.base, "demo")
	if err != nil {
		t.Fatalf("a finished line set left the id's lock held: %v", err)
	}
	free.Release()
}

// TestLineSetDialsNothing: with something listening at the line's server, set
// makes no connection to it, and runs no harness.
func TestLineSetDialsNothing(t *testing.T) {
	b := newLineBench(t)
	lineUp(t, b, "demo")
	co1, co2 := b.gitWorkdir(t, "co-plan"), b.gitWorkdir(t, "co-build")
	b.mustAdd(t, "demo", "plan", "claude", co1)
	b.mustAdd(t, "demo", "build", "codex", co2)
	// The line's server is the bench's own free address; the adds are done, so
	// whatever they asked of it is behind us. Take the address, and watch it.
	ln, err := net.Listen("tcp", b.db.server)
	if err != nil {
		t.Skipf("the bench's address was taken since the adds: %v", err)
	}
	defer ln.Close()
	calls := b.callCount()

	mustLine(t, b, b.lset("demo", "--cap", "30", "--window", "20m")...)

	if err := ln.(*net.TCPListener).SetDeadline(time.Now().Add(300 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if c, err := ln.Accept(); err == nil {
		c.Close()
		t.Error("set connected to the line's server")
	}
	if b.callCount() != calls {
		t.Error("set ran codex")
	}
}

// TestLineSetStopsWhenInterrupted: an interrupt that has come before the first file
// is written stops the run with nothing written, and says so.
func TestLineSetStopsWhenInterrupted(t *testing.T) {
	b := newLineBench(t)
	lineUp(t, b, "demo")
	co := b.gitWorkdir(t, "co")
	b.mustAdd(t, "demo", "plan", "claude", co)
	before := b.lsnap(t, co)
	rec, _, err := readLineRecord(b.base, "demo")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planLineSet(rec, b.base, binPath, 30, "10m")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = plan.apply(ctx, filepath.Join(b.base, "lines", "demo"))
	if err == nil || !strings.Contains(err.Error(), "interrupted") || !strings.Contains(err.Error(), "rewritten: none; not rewritten: "+filepath.Join(co, ".mcp.json")) {
		t.Fatalf("apply under a cancelled context: %v", err)
	}
	b.wantLineUnchanged(t, before, co)
}

// TestPlanLineSetRefusesAClaudeLaunchItWouldPushOverTheCap: the instructions of a
// Claude session are measured with the new numbers, and a window that makes them
// longer than Claude Code's cap refuses the run before any file is judged.
func TestPlanLineSetRefusesAClaudeLaunchItWouldPushOverTheCap(t *testing.T) {
	const longest = "2562047h47m16.854775807s"
	rec := goodLineRecord("/b")
	fits := func(n int, window string) bool {
		rec.StateDir = "/" + strings.Repeat("b", n)
		next := rec
		next.Window = window
		return measureLineClaude(next, "plan", rec.StateDir, "x") == nil
	}
	// The longest state directory the instructions still hold at the default window.
	lo, hi := 0, 4096
	if !fits(lo, "10m") || fits(hi, "10m") {
		t.Fatalf("the search does not start from a fit and an overflow")
	}
	for hi-lo > 1 {
		mid := (lo + hi) / 2
		if fits(mid, "10m") {
			lo = mid
		} else {
			hi = mid
		}
	}
	rec.StateDir = "/" + strings.Repeat("b", lo)
	if fits(lo, longest) {
		t.Fatalf("the longest window does not push the instructions over the cap")
	}
	_, err := planLineSet(rec, rec.StateDir, "/bin/parley", rec.Cap, longest)
	var cfg *configError
	if err == nil || !errors.As(err, &cfg) || !strings.Contains(err.Error(), "claude session plan") ||
		!strings.Contains(err.Error(), "over Claude Code's cap") || !strings.Contains(err.Error(), "shorten --cap, --window,") {
		t.Errorf("planLineSet with a window that overflows: %v (a configuration error, exit 2, naming the session and what to shorten, wanted)", err)
	}
	// The same record and a window that fits is judged on to the files, which are not there.
	if _, err := planLineSet(rec, rec.StateDir, "/bin/parley", rec.Cap, "15m"); err != nil {
		t.Errorf("planLineSet with a window that fits: %v", err)
	}
}

// setLine is the line a failed run prints to finish it: the numbers it had, as the
// binary that ran it, every value quoted.
func (b *lineBench) setLine(bin string, capN int, window string) string {
	return fmt.Sprintf("%s line set %s --cap %d --window %s --state-dir %s", q(bin), q("demo"), capN, q(window), q(b.base))
}

// partWay makes line demo of two Claude sessions in two working directories whose
// second directory cannot be written, and runs `line set --cap 30` by bin (the
// tests' own binary when empty) so that it fails after it wrote the first file. It
// returns the two files and the failure; the second directory is writable again.
func (b *lineBench) partWay(t *testing.T, bin string) (mcp1, mcp2 string, res result) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("a directory's mode does not stop root writing in it")
	}
	lineUp(t, b, "demo")
	co1, co2 := b.gitWorkdir(t, "co-plan"), b.gitWorkdir(t, "co-review")
	b.mustAdd(t, "demo", "plan", "claude", co1)
	b.mustAdd(t, "demo", "review", "claude", co2)
	if err := os.Chmod(co2, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(co2, 0o755) })
	args := b.lset("demo", "--cap", "30")
	if bin == "" {
		res = b.run(t, args...)
	} else {
		res = b.runBinaryIn(t, bin, args...)
	}
	if err := os.Chmod(co2, 0o755); err != nil {
		t.Fatal(err)
	}
	if res.code != 1 || !strings.Contains(res.stderr, "not rewritten: "+filepath.Join(co2, ".mcp.json")) {
		t.Fatalf("the run did not fail part way: exit %d\nstdout: %s\nstderr: %s", res.code, res.stdout, res.stderr)
	}
	return filepath.Join(co1, ".mcp.json"), filepath.Join(co2, ".mcp.json"), res
}

// TestLineSetAfterAFailurePartWayNeverSaysNothingChanged: the first file holds cap 30
// and the record says 8. Naming the record's own numbers is not passed over as a
// no-op: the first file does not carry them, so it is written, and the run says so;
// only once the files and the record agree is a rerun a no-op.
func TestLineSetAfterAFailurePartWayNeverSaysNothingChanged(t *testing.T) {
	b := newLineBench(t)
	mcp1, mcp2, _ := b.partWay(t, "")

	res := mustLine(t, b, b.lset("demo", "--cap", "8")...)
	t.Logf("stdout:\n%s", res.stdout)
	if strings.Contains(res.stdout, "nothing was changed") {
		t.Errorf("a run that wrote a file says nothing was changed:\n%s", res.stdout)
	}
	for _, want := range []string{
		"Rewrote " + mcp1 + " (session plan).",
		"Left " + mcp2 + " as it is (session review): it already has cap 8 and window 10m.",
		"plan (Claude Code): restart it, resuming its conversation:",
	} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, res.stdout)
		}
	}
	// The record's numbers did not change, and review's file was not written: only the
	// session whose file was replaced has a restart to do, and no primer went stale.
	for _, bad := range []string{"review (Claude Code)", "still states the old numbers"} {
		if strings.Contains(res.stdout, bad) {
			t.Errorf("stdout says %q:\n%s", bad, res.stdout)
		}
	}
	b.wantMCP(t, mcp1, "demo", "8", "10m")
	b.wantMCP(t, mcp2, "demo", "8", "10m")
	res = mustLine(t, b, b.lset("demo", "--cap", "8")...)
	if !strings.Contains(res.stdout, "already has cap 8") || !strings.Contains(res.stdout, "nothing was changed") {
		t.Errorf("stdout:\n%s", res.stdout)
	}
	mustLine(t, b, b.ldrop("demo", "plan")...)
	mustLine(t, b, b.ldrop("demo", "review")...)
}

// TestLineSetFinishCommandNamesTheBinaryThatWroteTheFile: a run by another binary
// that failed part way prints the line that repeats it as that binary, which runs
// and finishes it, so that both files run that binary; run by this one, the
// file the other wrote already has the numbers, is left, and is named in a note.
func TestLineSetFinishCommandNamesTheBinaryThatWroteTheFile(t *testing.T) {
	b := newLineBench(t)
	dev := filepath.Join(b.root, "parley-dev")
	if err := os.WriteFile(dev, mustRead(t, binPath), 0o755); err != nil {
		t.Fatal(err)
	}
	mcp1, mcp2, res := b.partWay(t, dev)
	cmd, _ := readMCP(t, mcp1)
	if cmd == binPath || filepath.Base(cmd) != "parley-dev" {
		t.Fatalf("the first file runs %q, want the other binary", cmd)
	}
	finish := b.setLine(cmd, 30, "10m")
	if !strings.Contains(res.stderr, "("+finish+" repeats this run)") {
		t.Fatalf("stderr does not print %q:\n%s", finish, res.stderr)
	}

	// The same numbers from this binary: the other's file has them, so it is left,
	// and the note says whose it is; the second file is written with this binary's.
	res = mustLine(t, b, b.lset("demo", "--cap", "30")...)
	note := "Note: the parley command in " + mcp1 + " is " + cmd + ", not " + binPath + ", the binary running line set; the file already has the numbers asked for, so this run leaves it as it is."
	if !strings.Contains(res.stdout, note) {
		t.Errorf("stdout lacks %q:\n%s", note, res.stdout)
	}
	if got, _ := readMCP(t, mcp1); got != cmd {
		t.Errorf("the first file runs %q now, want it left as it was (%q)", got, cmd)
	}
	if got, _ := readMCP(t, mcp2); got != binPath {
		t.Errorf("the second file runs %q, want this binary", got)
	}

	// The printed line works the same from where the failure left things.
	b2 := newLineBench(t)
	mcp1, mcp2, res = b2.partWay(t, dev)
	finish = b2.setLine(cmd, 30, "10m")
	if !strings.Contains(res.stderr, "("+finish+" repeats this run)") {
		t.Fatalf("stderr does not print %q:\n%s", finish, res.stderr)
	}
	if res := b2.runBinaryIn(t, "sh", "-c", finish); res.code != 0 {
		t.Fatalf("the printed command exited %d\n%s%s", res.code, res.stdout, res.stderr)
	}
	rec := b2.record(t, "demo")
	if rec.Cap != 30 || sessionByNick(t, rec, "review").Files[0].SHA256 != fileDigestOf(t, mcp2) || sessionByNick(t, rec, "plan").Files[0].SHA256 != fileDigestOf(t, mcp1) {
		t.Errorf("record %+v", rec)
	}
	for _, f := range []string{mcp1, mcp2} {
		if got, _ := readMCP(t, f); got != cmd {
			t.Errorf("%s runs %q, want %q", f, got, cmd)
		}
	}
	// And `line drop` recognizes the files again.
	mustLine(t, b2, b2.ldrop("demo", "plan")...)
	mustLine(t, b2, b2.ldrop("demo", "review")...)
}

// TestLineSetRefusalEchoesNoByteOfAFileThatIsNotTheLines: a .mcp.json that is the
// shape `line set` writes, with a command and a window that hold shell
// metacharacters, a quote and a control character, is refused, and neither the
// refusal nor anything else printed holds one of them: nothing is decided from
// bytes that do not hash to the recorded digest.
func TestLineSetRefusalEchoesNoByteOfAFileThatIsNotTheLines(t *testing.T) {
	b := newLineBench(t)
	lineUp(t, b, "demo")
	co := b.gitWorkdir(t, "co")
	b.mustAdd(t, "demo", "plan", "claude", co)
	mcp := filepath.Join(co, ".mcp.json")
	marker := filepath.Join(b.root, "marker-touched")
	hostile := []string{"/tmp/not-parley", "touch " + marker, "marker-touched", "\a", "evil'quote", "$(id)"}
	rec := b.record(t, "demo")
	evil := rec
	evil.Cap = 30
	evil.Window = "10m; touch " + marker + "; echo evil'quote $(id)\a"
	data, err := lineMCPJSON("/tmp/not-parley", evil, b.base)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mcp, data, 0o644); err != nil {
		t.Fatal(err)
	}
	before := b.lsnap(t, co)

	res := b.run(t, b.lset("demo", "--cap", "40")...)
	if res.code != 1 || res.stdout != "" || !strings.Contains(res.stderr, "will not rewrite "+mcp+": ") || !strings.Contains(res.stderr, "nothing was changed") {
		t.Fatalf("exit %d, want a refusal\nstdout: %s\nstderr: %s", res.code, res.stdout, res.stderr)
	}
	for _, bad := range hostile {
		if strings.Contains(res.stderr, bad) || strings.Contains(res.stdout, bad) {
			t.Errorf("the refusal holds %q, which came from the file:\n%s", bad, res.stderr)
		}
	}
	b.wantLineUnchanged(t, before, co)
	// Running what the refusal prints does nothing the file asked for.
	for _, line := range strings.FieldsFunc(res.stderr, func(r rune) bool { return r == ';' }) {
		if strings.Contains(line, " line drop ") {
			b.runBinaryIn(t, "sh", "-c", strings.TrimSpace(line))
			break
		}
	}
	if _, err := os.Lstat(marker); err == nil {
		t.Errorf("%s was made: something in the refusal ran the file's bytes", marker)
	}
}

// TestLineSetRefusalNamesWhatUnblocksIt: for a file that is not the line's, the
// refusal prints the digest to restore, and the commands that take its sessions off
// with --force, and add them again once the path is clear; the commands run as they
// stand, and then set goes through. A shared Claude file names every session that
// uses it, and a Codex config, which set never writes, is refused all the same.
func TestLineSetRefusalNamesWhatUnblocksIt(t *testing.T) {
	type session struct{ nick, harness string }
	for _, tc := range []struct {
		name     string
		sessions []session
		tamper   string // the launch file, relative to the working directory
		codex    bool
	}{
		{"a shared Claude file edited", []session{{"plan", "claude"}, {"review", "claude"}}, ".mcp.json", false},
		{"a Codex config edited", []session{{"build", "codex"}}, ".codex/config.toml", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newLineBench(t)
			lineUp(t, b, "demo")
			co := b.gitWorkdir(t, "co")
			for _, s := range tc.sessions {
				b.mustAdd(t, "demo", s.nick, s.harness, co, "--model", "m1")
			}
			file := filepath.Join(co, filepath.FromSlash(tc.tamper))
			recorded := sessionByNick(t, b.record(t, "demo"), tc.sessions[0].nick).Files[0].SHA256
			edit(t, file, "# mine\n")

			res := b.run(t, b.lset("demo", "--cap", "30")...)
			if res.code != 1 {
				t.Fatalf("exit %d, want 1\n%s%s", res.code, res.stdout, res.stderr)
			}
			if strings.Contains(res.stderr, "holds what line set writes") {
				t.Errorf("an edited file is not an interrupted run's:\n%s", res.stderr)
			}
			wants := []string{"restore the file to the bytes the record has a digest of (" + recorded + ")", "move what is at " + file + " aside"}
			var drops, adds []string
			for _, s := range tc.sessions {
				drops = append(drops, fmt.Sprintf("%s line drop demo %s --force --state-dir %s", q(binPath), s.nick, q(b.base)))
				adds = append(adds, fmt.Sprintf("%s line add demo --nick %s --harness %s --workdir %s --model 'm1' --state-dir %s", q(binPath), s.nick, s.harness, q(co), q(b.base)))
			}
			wants = append(wants, drops...)
			wants = append(wants, adds...)
			for _, want := range wants {
				if !strings.Contains(res.stderr, want) {
					t.Errorf("stderr lacks %q:\n%s", want, res.stderr)
				}
			}
			if got := strings.Contains(res.stderr, "a Codex session then starts a new thread"); got != tc.codex {
				t.Errorf("the new-thread remark is %v, want %v:\n%s", got, tc.codex, res.stderr)
			}
			excludeFile := filepath.Join(co, ".git", "info", "exclude")
			excludeLine := "/" + tc.tamper
			if want := "the exclude line " + excludeLine + " in " + excludeFile + " stays through that"; !strings.Contains(res.stderr, want) {
				t.Errorf("stderr lacks %q:\n%s", want, res.stderr)
			}

			// The recipe, as printed.
			for _, d := range drops {
				if r := b.runBinaryIn(t, "sh", "-c", d); r.code != 0 {
					t.Fatalf("%s exited %d\n%s%s", d, r.code, r.stdout, r.stderr)
				}
			}
			if r := b.runBinaryIn(t, "sh", "-c", adds[0]); r.code == 0 {
				t.Fatalf("add succeeded with the file still at its path:\n%s", r.stdout)
			}
			if err := os.Rename(file, file+".mine"); err != nil {
				t.Fatal(err)
			}
			for _, a := range adds {
				if r := b.runBinaryIn(t, "sh", "-c", a); r.code != 0 {
					t.Fatalf("%s exited %d\n%s%s", a, r.code, r.stdout, r.stderr)
				}
			}
			mustLine(t, b, b.lset("demo", "--cap", "30")...)
			if rec := b.record(t, "demo"); rec.Cap != 30 || len(rec.Sessions) != len(tc.sessions) {
				t.Errorf("record %+v", rec)
			}
			if !tc.codex {
				b.wantMCP(t, file, "demo", "30", "10m")
			}
			// What the refusal says about the exclude line holds: the line is still in
			// the file once the line is ended.
			mustLine(t, b, b.lend("demo")...)
			if got := readExcludeForTest(t, co); !strings.Contains(got, excludeLine+"\n") {
				t.Errorf("info/exclude = %q: the line %s went with the line, and the refusal says it stays", got, excludeLine)
			}
		})
	}
}

// TestLineSetRestoringTheFileUnblocksIt: what the refusal says first holds too: the
// file put back to the recorded bytes, set goes through.
func TestLineSetRestoringTheFileUnblocksIt(t *testing.T) {
	b := newLineBench(t)
	lineUp(t, b, "demo")
	co := b.gitWorkdir(t, "co")
	b.mustAdd(t, "demo", "plan", "claude", co)
	mcp := filepath.Join(co, ".mcp.json")
	orig := mustRead(t, mcp)
	if err := os.WriteFile(mcp, append(append([]byte(nil), orig...), '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	if res := b.run(t, b.lset("demo", "--cap", "30")...); res.code != 1 {
		t.Fatalf("exit %d, want 1", res.code)
	}
	if err := os.WriteFile(mcp, orig, 0o644); err != nil {
		t.Fatal(err)
	}
	mustLine(t, b, b.lset("demo", "--cap", "30")...)
	b.wantMCP(t, mcp, "demo", "30", "10m")
}

// TestLineSetTellsAClaudeSessionWithNoLaunchFileHowToGetItBack: a restart would load
// no parley server, so the session is told to be taken off and added again, which
// writes the file with the new numbers for every session of the working directory;
// the commands run as they stand.
func TestLineSetTellsAClaudeSessionWithNoLaunchFileHowToGetItBack(t *testing.T) {
	b := newLineBench(t)
	lineUp(t, b, "demo")
	co := b.gitWorkdir(t, "co")
	b.mustAdd(t, "demo", "plan", "claude", co, "--model", "opus")
	b.mustAdd(t, "demo", "review", "claude", co)
	mcp := filepath.Join(co, ".mcp.json")
	if err := os.Remove(mcp); err != nil {
		t.Fatal(err)
	}

	res := mustLine(t, b, b.lset("demo", "--cap", "30")...)
	t.Logf("stdout:\n%s", res.stdout)
	if strings.Contains(res.stdout, "--resume") {
		t.Errorf("a session with no launch file is told to resume:\n%s", res.stdout)
	}
	for _, nick := range []string{"plan", "review"} {
		want := nick + " (Claude Code): " + mcp + " is not there, so restarting it loads no parley server."
		if !strings.Contains(res.stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, res.stdout)
		}
	}
	drop := lineWith(t, res.stdout, "line drop demo plan")
	add := lineWith(t, res.stdout, "line add demo --nick plan")
	if want := fmt.Sprintf("%s line add demo --nick plan --harness claude --workdir %s --model 'opus' --state-dir %s", q(binPath), q(co), q(b.base)); add != want {
		t.Errorf("the add line is\n%s\nwant\n%s", add, want)
	}
	for _, line := range []string{drop, add} {
		if r := b.runBinaryIn(t, "sh", "-c", line); r.code != 0 {
			t.Fatalf("%s exited %d\n%s%s", line, r.code, r.stdout, r.stderr)
		}
	}
	// The file is back with the new numbers, and both sessions hold its digest.
	b.wantMCP(t, mcp, "demo", "30", "10m")
	rec := b.record(t, "demo")
	for _, s := range rec.Sessions {
		if s.Files[0].SHA256 != fileDigestOf(t, mcp) {
			t.Errorf("session %s holds %+v, want the new file's digest", s.Nick, s.Files)
		}
	}
	mustLine(t, b, b.ldrop("demo", "review")...)
	mustLine(t, b, b.ldrop("demo", "plan")...)
}

// TestLineSetKeepsTheRecordShapeOfSessionsItDoesNotTouch: an OpenCode session's
// "files": [] stays an empty list and not null, and every session set leaves alone
// keeps its JSON in the record byte for byte.
func TestLineSetKeepsTheRecordShapeOfSessionsItDoesNotTouch(t *testing.T) {
	b := newOCLineBench(t)
	lineUp(t, b.lineBench, "demo")
	cx, oc, cl := b.gitWorkdir(t, "co-build"), b.gitWorkdir(t, "co-review"), b.gitWorkdir(t, "co-plan")
	b.mustAdd(t, "demo", "build", "codex", cx)
	mustLine(t, b.lineBench, b.ladd("demo", "review", "opencode", oc)...)
	b.mustAdd(t, "demo", "plan", "claude", cl)
	sessions := func() map[string]string {
		var doc struct {
			Sessions []json.RawMessage `json:"sessions"`
		}
		if err := json.Unmarshal(mustRead(t, filepath.Join(b.base, "lines", "demo", "line.json")), &doc); err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, raw := range doc.Sessions {
			var n struct {
				Nick string `json:"nick"`
			}
			if err := json.Unmarshal(raw, &n); err != nil {
				t.Fatal(err)
			}
			out[n.Nick] = string(raw)
		}
		return out
	}
	before := sessions()
	if !strings.Contains(before["review"], `"files": []`) {
		t.Fatalf("the OpenCode session's record has no empty files list:\n%s", before["review"])
	}

	mustLine(t, b.lineBench, b.lset("demo", "--cap", "30", "--window", "20m")...)
	after := sessions()
	for _, nick := range []string{"build", "review"} {
		if after[nick] != before[nick] {
			t.Errorf("session %s changed in the record.\nbefore: %s\nafter:  %s", nick, before[nick], after[nick])
		}
	}
	if !strings.Contains(after["review"], `"files": []`) || strings.Contains(after["review"], "null") {
		t.Errorf("the OpenCode session's files list is not an empty list:\n%s", after["review"])
	}
	if after["plan"] == before["plan"] {
		t.Errorf("the Claude session's record did not change its digest:\n%s", after["plan"])
	}
}

// TestLineSetRechecksTheFileJustBeforeItWritesIt: a file that was the line's when
// planLineSet looked and was edited before apply wrote it is not written, and the
// failure says what was and was not.
func TestLineSetRechecksTheFileJustBeforeItWritesIt(t *testing.T) {
	b := newLineBench(t)
	lineUp(t, b, "demo")
	co := b.gitWorkdir(t, "co")
	b.mustAdd(t, "demo", "plan", "claude", co)
	mcp := filepath.Join(co, ".mcp.json")
	rec, _, err := readLineRecord(b.base, "demo")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planLineSet(rec, b.base, binPath, 30, "10m")
	if err != nil {
		t.Fatal(err)
	}
	edit(t, mcp, "\n")
	edited := mustRead(t, mcp)
	recBefore := string(mustRead(t, filepath.Join(b.base, "lines", "demo", "line.json")))

	err = plan.apply(context.Background(), filepath.Join(b.base, "lines", "demo"))
	if err == nil || !strings.Contains(err.Error(), "will not rewrite "+mcp+": ") || !strings.Contains(err.Error(), "it was edited or replaced; it changed since it was checked") ||
		!strings.Contains(err.Error(), "rewritten: none; not rewritten: "+mcp+";") {
		t.Fatalf("apply on a file edited since it was judged: %v", err)
	}
	if got := mustRead(t, mcp); string(got) != string(edited) {
		t.Errorf("the edited file was written over:\n%s", got)
	}
	if got := string(mustRead(t, filepath.Join(b.base, "lines", "demo", "line.json"))); got != recBefore {
		t.Errorf("the record changed:\n%s", got)
	}
	wantNoTempFiles(t, co)
}

// TestLineSetReprovesARewrittenFileBeforeRecordingIt: the file just written is proved
// again, by the digest of the bytes written, and one that was changed (or replaced by a
// link) after the rename is not recorded as the line's.
func TestLineSetReprovesARewrittenFileBeforeRecordingIt(t *testing.T) {
	b := newLineBench(t)
	lineUp(t, b, "demo")
	co := b.gitWorkdir(t, "co")
	b.mustAdd(t, "demo", "plan", "claude", co)
	mcp := filepath.Join(co, ".mcp.json")
	rec, _, err := readLineRecord(b.base, "demo")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planLineSet(rec, b.base, binPath, 30, "10m")
	if err != nil {
		t.Fatal(err)
	}
	lf := plan.files[0]
	if err := plan.rewrite(lf); err != nil {
		t.Fatal(err)
	}
	if err := plan.reprove(lf); err != nil {
		t.Fatalf("a file just written is not proved: %v", err)
	}
	edit(t, mcp, "\n")
	if err := plan.reprove(lf); err == nil || !strings.Contains(err.Error(), "will not record the digest of "+mcp+": ") || !strings.Contains(err.Error(), "it changed while this run was going") {
		t.Errorf("reprove of an edited file: %v", err)
	}
	// A link at the path, to bytes that would hash right, is not the line's file either.
	elsewhere := filepath.Join(b.root, "elsewhere.json")
	if err := os.WriteFile(elsewhere, lf.data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(mcp); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, mcp); err != nil {
		t.Fatal(err)
	}
	if err := plan.reprove(lf); err == nil || !strings.Contains(err.Error(), "it is a symlink") {
		t.Errorf("reprove of a link: %v", err)
	}
	if err := os.Remove(mcp); err != nil {
		t.Fatal(err)
	}
	if err := plan.reprove(lf); err == nil || !strings.Contains(err.Error(), "it is not there any more") {
		t.Errorf("reprove of a file that is gone: %v", err)
	}
}

// TestLineSetDoesNotTakeAFileAsCurrentThatFailsTheProof: a file that says the numbers
// asked for, but whose bytes do not hash to the recorded digest, is not current and is
// not read: it is refused, as any file that is not the line's, and nothing is written.
func TestLineSetDoesNotTakeAFileAsCurrentThatFailsTheProof(t *testing.T) {
	b := newLineBench(t)
	lineUp(t, b, "demo")
	co := b.gitWorkdir(t, "co")
	b.mustAdd(t, "demo", "plan", "claude", co)
	mcp := filepath.Join(co, ".mcp.json")
	rec := b.record(t, "demo")
	other := rec
	other.Cap = 30
	data, err := lineMCPJSON("/tmp/not-parley", other, b.base)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mcp, data, 0o644); err != nil {
		t.Fatal(err)
	}
	before := b.lsnap(t, co)
	res := b.run(t, b.lset("demo", "--cap", "30")...)
	if res.code != 1 || res.stdout != "" || !strings.Contains(res.stderr, "will not rewrite "+mcp+": ") || strings.Contains(res.stderr, "/tmp/not-parley") {
		t.Fatalf("exit %d, want a refusal that does not read the file\nstdout: %s\nstderr: %s", res.code, res.stdout, res.stderr)
	}
	b.wantLineUnchanged(t, before, co)
}

// TestLineSetRefusesAWorkingDirectoryThatBecameALink: a recorded working directory
// that now resolves elsewhere is refused, with nothing written behind the link; one
// that is gone is left alone, and said.
func TestLineSetRefusesAWorkingDirectoryThatBecameALink(t *testing.T) {
	b := newLineBench(t)
	lineUp(t, b, "demo")
	co := b.gitWorkdir(t, "co")
	b.mustAdd(t, "demo", "plan", "claude", co)
	real := filepath.Join(b.root, "co-real")
	if err := os.Rename(co, real); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, co); err != nil {
		t.Fatal(err)
	}
	mcp := filepath.Join(co, ".mcp.json")
	data := mustRead(t, filepath.Join(real, ".mcp.json"))
	before := b.lsnap(t, real)

	res := b.run(t, b.lset("demo", "--cap", "30")...)
	if res.code != 1 || !strings.Contains(res.stderr, "will not rewrite "+mcp+": its working directory "+co+" now resolves to "+real) || !strings.Contains(res.stderr, "nothing was changed") {
		t.Fatalf("exit %d, want 1 naming the directory\nstderr: %s", res.code, res.stderr)
	}
	b.wantLineUnchanged(t, before, real)
	if got := mustRead(t, filepath.Join(real, ".mcp.json")); string(got) != string(data) {
		t.Errorf("the file behind the link was written:\n%s", got)
	}

	// Gone altogether: nothing is there to rewrite.
	if err := os.Remove(co); err != nil {
		t.Fatal(err)
	}
	res = mustLine(t, b, b.lset("demo", "--cap", "30")...)
	if !strings.Contains(res.stdout, mcp+" is not there (session plan)") {
		t.Errorf("stdout:\n%s", res.stdout)
	}
	if rec := b.record(t, "demo"); rec.Cap != 30 {
		t.Errorf("record cap %d, want 30", rec.Cap)
	}
}

// TestPlanLineSetSameNumbersWithAStaleDigestIsNotANoOp: numbers the record already
// has, with a file that holds what would be written under another digest, is a
// change (the digest is recorded); with the digest right, it is not.
func TestPlanLineSetSameNumbersWithAStaleDigestIsNotANoOp(t *testing.T) {
	b := newLineBench(t)
	lineUp(t, b, "demo")
	co := b.gitWorkdir(t, "co")
	b.mustAdd(t, "demo", "plan", "claude", co)
	rec, _, err := readLineRecord(b.base, "demo")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planLineSet(rec, b.base, binPath, rec.Cap, rec.Window)
	if err != nil || plan.changes() {
		t.Fatalf("the record is right: %v, changes %v", err, plan != nil && plan.changes())
	}
	rec.Sessions[0].Files[0].SHA256 = "sha256:" + strings.Repeat("0", 64)
	plan, err = planLineSet(rec, b.base, binPath, rec.Cap, rec.Window)
	if err != nil || !plan.changes() {
		t.Fatalf("the record's digest is stale: %v, changes %v", err, plan != nil && plan.changes())
	}
	// Such a run changes no number, so it tells nobody to restart, and does not call
	// the numbers a pump holds old.
	out := plan.report(filepath.Join(b.base, "lines", "demo", "line.json"))
	if !strings.Contains(out, "already holds what this run writes (session plan): its digest is recorded.") ||
		!strings.Contains(out, "The numbers are the ones the line had, so no session needs a restart") {
		t.Errorf("the report of a run that only records a digest:\n%s", out)
	}
	for _, bad := range []string{"restart it", "stop its pump", "still states the old numbers", "Nothing that is running changes yet"} {
		if strings.Contains(out, bad) {
			t.Errorf("the report of a run that changed no number says %q:\n%s", bad, out)
		}
	}
	// With other numbers, on a record whose digest is right, it is a change that has to
	// reach the sessions.
	fresh, _, err := readLineRecord(b.base, "demo")
	if err != nil {
		t.Fatal(err)
	}
	plan, err = planLineSet(fresh, b.base, binPath, fresh.Cap+1, fresh.Window)
	if err != nil {
		t.Fatal(err)
	}
	if out := plan.report("line.json"); !strings.Contains(out, "plan (Claude Code): restart it") || strings.Contains(out, "no session needs a restart") {
		t.Errorf("the report of a run that changed the cap:\n%s", out)
	}
}
