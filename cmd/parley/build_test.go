package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/colinsurprenant/parley/internal/state"
)

// buildArgs is a valid `build new` for id, with the peers named (a codex peer
// codex-a and a claude peer claude-a by default) and extra flags after.
func (b *tableBench) buildArgs(id string, peers []string, extra ...string) []string {
	if peers == nil {
		peers = []string{"codex-a=codex", "claude-a=claude"}
	}
	args := []string{"build", "new", id, "--target", b.repo, "--chair", "colin",
		"--state-dir", b.base, "--server", b.db.server}
	for _, p := range peers {
		args = append(args, "--peer", p)
	}
	return append(args, extra...)
}

func (b *tableBench) buildEndArgs(id string, extra ...string) []string {
	return append([]string{"build", "end", id, "--state-dir", b.base}, extra...)
}

func (b *tableBench) buildStream(id string) string { return "irc://" + b.db.server + "/#" + id }

// buildSnapshot is snapshot and the target's branches: everything a build new
// could leave behind.
func (b *tableBench) buildSnapshot(t *testing.T) string {
	t.Helper()
	return b.snapshot(t) + "refs:\n" + gitIn(t, b.repo, "for-each-ref", "--format=%(refname) %(objectname)") + "\n"
}

// withoutCalls is a snapshot less the count of codex calls, for a run that
// called codex and rolled back.
func withoutCalls(snap string) string { return strings.Split(snap, "codex calls:")[0] }

// buildUp runs a build new that must succeed and reads its record.
func buildUp(t *testing.T, b *tableBench, id string, peers []string, extra ...string) (buildRecord, result) {
	t.Helper()
	res := b.run(t, b.buildArgs(id, peers, extra...)...)
	if res.code != 0 {
		t.Fatalf("parley build new exited %d\nstdout: %s\nstderr: %s", res.code, res.stdout, res.stderr)
	}
	return readRecordForTest(t, b, id), res
}

func readRecordForTest(t *testing.T, b *tableBench, id string) buildRecord {
	t.Helper()
	raw := mustRead(t, filepath.Join(b.base, "builds", id, "build.json"))
	var rec buildRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("build.json: %v\n%s", err, raw)
	}
	rec.digest = digestOf(raw)
	return rec
}

func (rec buildRecord) peer(t *testing.T, nick string) peerRecord {
	t.Helper()
	for _, p := range rec.Peers {
		if p.Nick == nick {
			return p
		}
	}
	t.Fatalf("no peer %s in the record", nick)
	return peerRecord{}
}

// TestBuildNewSetsTheBuildUp is the acceptance run: one Codex peer (a fake) and
// one Claude peer on a temp repo. It checks the worktrees and branches, the files
// in each, build.json, the markers, what codex was asked, and what is printed.
func TestBuildNewSetsTheBuildUp(t *testing.T) {
	b := newTableBench(t)
	head := b.head(t)
	rec, res := buildUp(t, b, "b1", nil)
	t.Logf("stdout:\n%s", res.stdout)
	t.Logf("stderr:\n%s", res.stderr)

	buildDir := filepath.Join(b.base, "builds", "b1")
	stream := b.buildStream("b1")
	codexWT, claudeWT := filepath.Join(buildDir, "codex-a"), filepath.Join(buildDir, "claude-a")

	if fi, err := os.Stat(buildDir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("the build directory is %v (%v), want mode 0700", fi, err)
	}
	if fi, err := os.Stat(filepath.Join(buildDir, "build.json")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("build.json is %v (%v), want mode 0600", fi, err)
	}

	// build.json: every field.
	if rec.ID != "b1" || rec.Stream != stream || rec.Target != b.repo || rec.Chair != "colin" || rec.BaseCommit != head {
		t.Errorf("build.json = %+v, want id b1, stream %s, target %s, chair colin, base commit %s", rec, stream, b.repo, head)
	}
	if want := filepath.Join(b.repo, ".git"); rec.CommonGit != want {
		t.Errorf("common_git_dir %s, want %s", rec.CommonGit, want)
	}
	if len(rec.Peers) != 2 {
		t.Fatalf("build.json lists %d peers, want 2", len(rec.Peers))
	}
	for _, p := range rec.Peers {
		wt := filepath.Join(buildDir, p.Nick)
		if p.Worktree != wt || p.Branch != "parley/b1/"+p.Nick {
			t.Errorf("peer %s: worktree %s branch %s, want %s and parley/b1/%s", p.Nick, p.Worktree, p.Branch, wt, p.Nick)
		}
		if want := filepath.Join(rec.CommonGit, "worktrees"); filepath.Dir(p.AdminGitDir) != want {
			t.Errorf("peer %s: admin_git_dir %s, want one under %s", p.Nick, p.AdminGitDir, want)
		}
		if !adminOwns(p.AdminGitDir, wt) {
			t.Errorf("peer %s: the admin directory %s does not name %s", p.Nick, p.AdminGitDir, wt)
		}
		// Two worktrees, on branches of their own, at the base commit.
		if got := gitIn(t, wt, "rev-parse", "--abbrev-ref", "HEAD"); got != p.Branch {
			t.Errorf("peer %s's worktree is on %s, want %s", p.Nick, got, p.Branch)
		}
		if got := gitIn(t, b.repo, "rev-parse", p.Branch); got != head {
			t.Errorf("branch %s is at %s, want the base commit %s", p.Branch, got, head)
		}
	}
	if list := gitIn(t, b.repo, "worktree", "list", "--porcelain"); strings.Count(list, "worktree ") != 3 {
		t.Errorf("git worktree list:\n%s\nwant the target and two worktrees", list)
	}
	if got := gitIn(t, b.repo, "status", "--porcelain"); got != "" {
		t.Errorf("the target checkout is not clean: %s", got)
	}
	if got := gitIn(t, b.repo, "branch", "--show-current"); got == "" || strings.HasPrefix(got, "parley/") {
		t.Errorf("the target checkout is on %q", got)
	}

	// The reply directory: 0700 under TMPDIR, named for the build, marked.
	fi, err := os.Stat(rec.ReplyDir)
	if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("reply directory %s: %v (%v), want a directory at 0700", rec.ReplyDir, fi, err)
	}
	if filepath.Dir(rec.ReplyDir) != b.tmp || !strings.HasPrefix(filepath.Base(rec.ReplyDir), "parley-build-b1-") {
		t.Errorf("reply directory %s, want parley-build-b1-* under %s", rec.ReplyDir, b.tmp)
	}

	// The markers: id, the nicks sorted, the digest of build.json, in the
	// worktrees and in the reply directory.
	sealed := "b1\nclaude-a codex-a\n" + digestOf(mustRead(t, filepath.Join(buildDir, "build.json"))) + "\n"
	for _, path := range []string{filepath.Join(codexWT, ".parley-build"), filepath.Join(claudeWT, ".parley-build"), filepath.Join(rec.ReplyDir, ".parley-build")} {
		if got := string(mustRead(t, path)); got != sealed {
			t.Errorf("%s holds %q, want %q", path, got, sealed)
		}
	}
	if m := mustRead(t, filepath.Join(rec.ReplyDir, ".parley-build")); len(m) == 0 {
		t.Error("the reply directory's marker is empty")
	}

	// The Claude peer: .mcp.json with the build's launch flags, none of the table's.
	var mcp struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(mustRead(t, filepath.Join(claudeWT, ".mcp.json")), &mcp); err != nil {
		t.Fatal(err)
	}
	srv, ok := mcp.MCPServers["parley"]
	if !ok || len(mcp.MCPServers) != 1 || srv.Command != binPath {
		t.Fatalf(".mcp.json servers %v, want only parley, run as %s", mcp.MCPServers, binPath)
	}
	wantArgs := []string{"channel", "--stream", stream, "--nick", "${PARLEY_NICK}", "--state-dir", b.base,
		"--skill", "build", "--chair", "colin", "--target", b.repo, "--worktree", claudeWT}
	if strings.Join(srv.Args, "\x00") != strings.Join(wantArgs, "\x00") {
		t.Errorf(".mcp.json args\n%q\nwant\n%q", srv.Args, wantArgs)
	}
	check := append([]string(nil), srv.Args...)
	for i, a := range check {
		if a == "${PARLEY_NICK}" {
			check[i] = "claude-a"
		}
	}
	if c := runCheck(t, b.tmp, check...); c.code != 0 {
		t.Errorf("channel --check on the .mcp.json args exited %d\n%s%s", c.code, c.stdout, c.stderr)
	}
	if _, err := os.Lstat(filepath.Join(claudeWT, ".codex")); err == nil {
		t.Error("the Claude peer's worktree has a .codex")
	}

	// The Codex peer: .codex/config.toml, workspace-write, on-request, network on,
	// and no writable_roots.
	wantCfg := "sandbox_mode = \"workspace-write\"\napproval_policy = \"on-request\"\n\n[sandbox_workspace_write]\nnetwork_access = true\n"
	if got := string(mustRead(t, filepath.Join(codexWT, ".codex", "config.toml"))); got != wantCfg {
		t.Errorf(".codex/config.toml\n%s\nwant\n%s", got, wantCfg)
	}
	if _, err := os.Lstat(filepath.Join(codexWT, ".mcp.json")); err == nil {
		t.Error("the Codex peer's worktree has a .mcp.json")
	}

	// The Codex peer's session: started in its worktree on the build primer, its
	// thread recorded for --thread auto and in the record, its reply file in the
	// reply directory.
	argv := b.call(t, 1)
	wantHead := []string{"exec", "--json", "-C", codexWT, "-s", "workspace-write", "-c", "sandbox_workspace_write.network_access=false"}
	if len(argv) != len(wantHead)+1 || strings.Join(argv[:len(wantHead)], "\x00") != strings.Join(wantHead, "\x00") {
		t.Fatalf("codex was called with %q, want %q and the primer", argv, wantHead)
	}
	primerText := argv[len(argv)-1]
	for _, want := range []string{"parley-build skill, invoked as $parley-build", "your worktree " + codexWT, "target repo " + b.repo,
		filepath.Join(rec.ReplyDir, "codex-a.reply"), "--nick codex-a"} {
		if !strings.Contains(primerText, want) {
			t.Errorf("the primer lacks %q:\n%s", want, primerText)
		}
	}
	if b.callCount() != 1 {
		t.Errorf("codex was called %d times, want once, for the one Codex peer", b.callCount())
	}
	cp := rec.peer(t, "codex-a")
	if cp.Thread != fakeThread(1) || cp.ReplyFile != filepath.Join(rec.ReplyDir, "codex-a.reply") || cp.Harness != "codex" {
		t.Errorf("codex-a's record = %+v", cp)
	}
	if lp := rec.peer(t, "claude-a"); lp.Thread != "" || lp.ReplyFile != "" || lp.Harness != "claude" {
		t.Errorf("claude-a's record = %+v", lp)
	}
	tr, _ := state.OpenThreadRecord(b.base, "codex-a", stream)
	if got, err := tr.Read(); err != nil || got != fakeThread(1) {
		t.Errorf("the thread record = %q, %v, want %s", got, err, fakeThread(1))
	}

	// The locks were released: both nicks can be taken again.
	for _, nick := range []string{"codex-a", "claude-a"} {
		l, err := state.LockNick(b.base, nick)
		if err != nil {
			t.Errorf("%s's lock is held after build new: %v", nick, err)
			continue
		}
		l.Release()
	}

	// What is printed.
	words := func(subs ...string) []string { return shellWords(t, lineWith(t, res.stdout, subs...)) }
	if got, want := words(" codex --stream ", "--nick codex-a"), []string{binPath, "codex", "--stream", stream, "--nick", "codex-a", "--state-dir", b.base, "--thread", "auto", "--skill", "build"}; strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("pump line %q, want %q", got, want)
	}
	if got, want := words(" resume ", "-a on-request"), []string{"codex", "resume", fakeThread(1), "-C", codexWT, "-a", "on-request"}; strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("resume line %q, want %q", got, want)
	}
	wantInOrder(t, res.stdout, []string{
		"cd '" + claudeWT + "' && PARLEY_NICK=claude-a claude --dangerously-load-development-channels server:parley",
		"RUN b1",
		"budget: <n> rounds",
		"cap: <duration>",
		"human: colin",
		"participants: codex-a claude-a",
		"target: " + b.repo,
		"task: <one or two sentences>",
	})
	for _, want := range []string{"director adopt", "check-ignore -q .director/x", filepath.Join(b.repo, ".git", "info", "exclude"),
		"Nothing answers at " + b.db.server, "/join #b1", "/msg NickServ INFO colin", "$parley-build", "/parley-build",
		"build end b1 --state-dir"} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, res.stdout)
		}
	}
	for _, bad := range []string{"--cap", "sheet.md", "table end"} {
		if strings.Contains(res.stdout, bad) {
			t.Errorf("stdout, a build's, holds %q:\n%s", bad, res.stdout)
		}
	}
}

// TestBuildNewWritesOnlyTheConfigsItsPeersNeed: a build of two Claude peers never
// calls codex and has no .codex anywhere, and one of two Codex peers has no
// .mcp.json and needs no claude on the machine.
func TestBuildNewWritesOnlyTheConfigsItsPeersNeed(t *testing.T) {
	t.Run("claude only", func(t *testing.T) {
		b := newTableBench(t)
		rec, res := buildUp(t, b, "b1", []string{"claude-a=claude", "claude-b=claude:opus"})
		for _, p := range rec.Peers {
			if _, err := os.Stat(filepath.Join(p.Worktree, ".codex")); err == nil {
				t.Errorf("%s's worktree has a .codex directory", p.Nick)
			}
			var mcp struct {
				MCPServers map[string]struct {
					Args []string `json:"args"`
				} `json:"mcpServers"`
			}
			if err := json.Unmarshal(mustRead(t, filepath.Join(p.Worktree, ".mcp.json")), &mcp); err != nil {
				t.Fatal(err)
			}
			args := mcp.MCPServers["parley"].Args
			if args[len(args)-1] != p.Worktree || strings.Contains(strings.Join(args, " "), "--cap") {
				t.Errorf("%s's .mcp.json args %q, want its own --worktree and no --cap", p.Nick, args)
			}
		}
		if n := b.callCount(); n != 0 {
			t.Errorf("a Claude-only build called codex %d times", n)
		}
		if strings.Contains(res.stdout, "Codex") || strings.Contains(res.stdout, "codex resume") {
			t.Errorf("a Claude-only build prints Codex steps:\n%s", res.stdout)
		}
		lineWith(t, res.stdout, "PARLEY_NICK=claude-a claude")
		if line := lineWith(t, res.stdout, "PARLEY_NICK=claude-b claude"); !strings.HasSuffix(line, "--model 'opus'") {
			t.Errorf("claude-b's launch line lacks its model: %s", line)
		}
	})
	t.Run("codex only", func(t *testing.T) {
		b := newTableBench(t)
		res := runParleyEnvIn(t, "", b.envWithPath(b.pathWith(t, "codex", "ergo", "git", "cat", "mv")), b.buildArgs("b1", []string{"codex-a=codex", "codex-b=codex"})...)
		if res.code != 0 {
			t.Fatalf("exit %d\n%s", res.code, res.stderr)
		}
		rec := readRecordForTest(t, b, "b1")
		for _, p := range rec.Peers {
			if _, err := os.Stat(filepath.Join(p.Worktree, ".mcp.json")); err == nil {
				t.Errorf("%s's worktree has a .mcp.json", p.Nick)
			}
			if _, err := os.Stat(filepath.Join(p.Worktree, ".codex", "config.toml")); err != nil {
				t.Errorf("%s has no .codex/config.toml: %v", p.Nick, err)
			}
		}
		if strings.Contains(res.stdout, "Claude peers") {
			t.Errorf("a Codex-only build prints Claude steps:\n%s", res.stdout)
		}
	})
}

// TestBuildNewIsTheSameBuildWithTwoCodexPeers: both are started, each in its own
// worktree, each with its own thread and reply file.
func TestBuildNewIsTheSameBuildWithTwoCodexPeers(t *testing.T) {
	b := newTableBench(t)
	rec, res := buildUp(t, b, "b2", []string{"codex-a=codex:gpt-5", "codex-b=codex"}, "--codex-bin", b.codex)
	if b.callCount() != 2 {
		t.Fatalf("codex was called %d times, want 2", b.callCount())
	}
	for i, nick := range []string{"codex-a", "codex-b"} {
		p := rec.peer(t, nick)
		argv := b.call(t, i+1)
		if argv[3] != p.Worktree {
			t.Errorf("call %d ran in %s, want %s", i+1, argv[3], p.Worktree)
		}
		if p.Thread != fakeThread(i+1) || p.ReplyFile != filepath.Join(rec.ReplyDir, nick+".reply") {
			t.Errorf("%s's record = %+v", nick, p)
		}
		if _, err := os.Lstat(filepath.Join(p.Worktree, ".codex", "config.toml")); err != nil {
			t.Errorf("%s has no .codex/config.toml: %v", nick, err)
		}
	}
	if argv := b.call(t, 1); !containsAll(argv, "-m", "gpt-5") {
		t.Errorf("the model was not passed: %q", argv)
	}
	if argv := b.call(t, 2); containsAll(argv, "-m") {
		t.Errorf("codex-b named no model, yet: %q", argv)
	}
	if !strings.Contains(res.stdout, "(model gpt-5)") || !strings.Contains(res.stdout, "--codex-bin '"+b.codex+"'") {
		t.Errorf("stdout lacks the model and the --codex-bin in the pump line:\n%s", res.stdout)
	}
	if strings.Contains(res.stdout, "Claude peers") {
		t.Errorf("stdout has a Claude step for a build without a Claude peer:\n%s", res.stdout)
	}
}

func containsAll(argv []string, want ...string) bool {
	for _, w := range want {
		found := false
		for _, a := range argv {
			if a == w {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// TestBuildNewRefusesBeforeAnyChange: each refusal is exit 2 with a reason, and
// the machine is as it was, down to the target's branches.
func TestBuildNewRefusesBeforeAnyChange(t *testing.T) {
	commit := func(t *testing.T, b *tableBench, name, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(b.repo, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(b.repo, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		gitIn(t, b.repo, "add", name)
		gitIn(t, b.repo, "-c", "commit.gpgsign=false", "commit", "-q", "-m", "add "+name)
	}
	cases := []struct {
		name  string
		setup func(t *testing.T, b *tableBench)
		args  func(b *tableBench) []string
		want  string
	}{
		{"a third peer", nil, func(b *tableBench) []string {
			return b.buildArgs("b1", []string{"codex-a=codex", "claude-a=claude", "claude-b=claude"})
		}, "a third is refused"},
		{"one peer", nil, func(b *tableBench) []string { return b.buildArgs("b1", []string{"claude-a=claude"}) }, "one --peer is not a build"},
		{"no peer", nil, func(b *tableBench) []string { return b.buildArgs("b1", []string{}) }, "two --peer are required"},
		{"the branch exists", func(t *testing.T, b *tableBench) { gitIn(t, b.repo, "branch", "parley/b1/claude-a") },
			func(b *tableBench) []string { return b.buildArgs("b1", nil) }, "branch parley/b1/claude-a already exists"},
		{"a live build", func(t *testing.T, b *tableBench) { mustBuildUp(t, b, "b1") },
			func(b *tableBench) []string { return b.buildArgs("b1", []string{"codex-b=codex", "claude-b=claude"}) }, "build \"b1\" already exists"},
		{"a live table", func(t *testing.T, b *tableBench) { mustTable(t, b, b.newArgs("b1", nil)...) },
			func(b *tableBench) []string { return b.buildArgs("b1", []string{"codex-b=codex", "claude-b=claude"}) }, "already a table"},
		{"the reserved nick builds", nil, func(b *tableBench) []string { return b.buildArgs("b1", []string{"builds=codex", "claude-a=claude"}) }, "the directory the builds live in"},
		{"the reserved nick tables", nil, func(b *tableBench) []string { return b.buildArgs("b1", []string{"Tables=codex", "claude-a=claude"}) }, "the directory the tables live in"},
		{"the chair builds", nil, func(b *tableBench) []string {
			return append(b.buildArgs("b1", nil), "--chair", "builds")
		}, "the directory the builds live in"},
		{"cursor state already there", func(t *testing.T, b *tableBench) {
			d, err := state.Open(b.base, "claude-a", b.buildStream("b1"))
			if err != nil {
				t.Fatal(err)
			}
			if err := d.SaveCursor("stale"); err != nil {
				t.Fatal(err)
			}
		}, func(b *tableBench) []string { return b.buildArgs("b1", nil) }, "already has state for this stream"},
		{"a peer that is the chair", nil, func(b *tableBench) []string { return b.buildArgs("b1", []string{"Colin=codex", "claude-a=claude"}) }, "the nick is the chair's"},
		{"a nick twice", nil, func(b *tableBench) []string { return b.buildArgs("b1", []string{"Claude-A=codex", "claude-a=claude"}) }, "also peer"},
		{"an unknown harness", nil, func(b *tableBench) []string { return b.buildArgs("b1", []string{"x=gemini", "claude-a=claude"}) }, "unknown harness"},
		{"opencode", nil, func(b *tableBench) []string { return b.buildArgs("b1", []string{"x=opencode", "claude-a=claude"}) }, "OpenCode peers are not supported yet"},
		{"no harness", nil, func(b *tableBench) []string { return b.buildArgs("b1", []string{"x", "claude-a=claude"}) }, "want <nick>=<claude|codex>"},
		{"a model that looks like a flag", nil, func(b *tableBench) []string { return b.buildArgs("b1", []string{"x=codex:-m", "claude-a=claude"}) }, "begins with '-'"},
		{"an id with a capital", nil, func(b *tableBench) []string { return b.buildArgs("B1", nil) }, "build id"},
		{"an id of 33 characters", nil, func(b *tableBench) []string { return b.buildArgs(strings.Repeat("a", 33), nil) }, "build id"},
		{"no --chair", nil, func(b *tableBench) []string {
			var out []string
			args := b.buildArgs("b1", nil)
			for i := 0; i < len(args); i++ {
				if args[i] == "--chair" {
					i++
					continue
				}
				out = append(out, args[i])
			}
			return out
		}, "--chair is required"},
		{"no --target", nil, func(b *tableBench) []string {
			var out []string
			args := b.buildArgs("b1", nil)
			for i := 0; i < len(args); i++ {
				if args[i] == "--target" {
					i++
					continue
				}
				out = append(out, args[i])
			}
			return out
		}, "--target is required"},
		{"a target that is not a checkout root", func(t *testing.T, b *tableBench) {
			if err := os.Mkdir(filepath.Join(b.repo, "sub"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, func(b *tableBench) []string {
			return append(b.buildArgs("b1", nil), "--target", filepath.Join(b.repo, "sub"))
		}, "not the root of its checkout"},
		{"a target that is not a repo", nil, func(b *tableBench) []string {
			return append(b.buildArgs("b1", nil), "--target", filepath.Join(b.root, "w"))
		}, "not a git checkout"},
		{"HEAD tracks .parley-build", func(t *testing.T, b *tableBench) { commit(t, b, ".parley-build", "x\n") },
			func(b *tableBench) []string { return b.buildArgs("b1", nil) }, ".parley-build at HEAD"},
		{"HEAD tracks .mcp.json", func(t *testing.T, b *tableBench) { commit(t, b, ".mcp.json", "{}\n") },
			func(b *tableBench) []string { return b.buildArgs("b1", nil) }, ".mcp.json at HEAD"},
		{"HEAD tracks .codex/config.toml", func(t *testing.T, b *tableBench) { commit(t, b, ".codex/config.toml", "x = 1\n") },
			func(b *tableBench) []string { return b.buildArgs("b1", nil) }, ".codex/config.toml at HEAD"},
		{"a state directory inside the target", nil, func(b *tableBench) []string {
			return append(b.buildArgs("b1", nil), "--state-dir", filepath.Join(b.repo, "st"))
		}, "is inside the target"},
		{"a temp directory inside the target", nil, func(b *tableBench) []string { return b.buildArgs("b1", nil) }, "is inside the target checkout"},
		{"a --writable-root that is relative", nil, func(b *tableBench) []string {
			return append(b.buildArgs("b1", nil), "--writable-root", "rel/dir")
		}, "must be an absolute path"},
		{"a --writable-root that does not exist", nil, func(b *tableBench) []string {
			return append(b.buildArgs("b1", nil), "--writable-root", filepath.Join(b.root, "nope"))
		}, "--writable-root"},
		{"a --writable-root with a newline", nil, func(b *tableBench) []string {
			return append(b.buildArgs("b1", nil), "--writable-root", filepath.Join(b.root, "a\nb"))
		}, "holds a control character"},
		{"a --writable-root that is a file", nil, func(b *tableBench) []string {
			return append(b.buildArgs("b1", nil), "--writable-root", filepath.Join(b.repo, "README.md"))
		}, "is not a directory"},
		{"a --writable-root and no Codex peer", nil, func(b *tableBench) []string {
			return append(b.buildArgs("b1", []string{"claude-a=claude", "claude-b=claude"}), "--writable-root", b.root)
		}, "no peer is Codex"},
		{"an empty --codex-bin", nil, func(b *tableBench) []string { return append(b.buildArgs("b1", nil), "--codex-bin", "") }, "--codex-bin must not be empty"},
		{"a --codex-bin that is not there", nil, func(b *tableBench) []string {
			return append(b.buildArgs("b1", nil), "--codex-bin", filepath.Join(b.root, "no-codex"))
		}, "--codex-bin"},
		{"a server that is a host name", nil, func(b *tableBench) []string {
			return append(b.buildArgs("b1", nil), "--server", "localhost:6999")
		}, "--server"},
		{"an extra argument", nil, func(b *tableBench) []string { return append(b.buildArgs("b1", nil), "stray") }, "unexpected argument"},
		{"an unknown flag", nil, func(b *tableBench) []string { return append(b.buildArgs("b1", nil), "--rounds", "3") }, "flag provided but not defined: -rounds"},
		{"--cap is not a build's", nil, func(b *tableBench) []string { return append(b.buildArgs("b1", nil), "--cap", "6") }, "flag provided but not defined: -cap"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := newTableBench(t)
			if c.name == "a temp directory inside the target" {
				b.tmp = filepath.Join(b.repo, "tmp")
				if err := os.Mkdir(b.tmp, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if c.setup != nil {
				c.setup(t, b)
			}
			before := b.buildSnapshot(t)
			res := b.run(t, c.args(b)...)
			if res.code != 2 {
				t.Fatalf("exit %d, want 2\nstdout: %s\nstderr: %s", res.code, res.stdout, res.stderr)
			}
			if !strings.Contains(res.stderr, c.want) {
				t.Errorf("stderr does not say %q:\n%s", c.want, res.stderr)
			}
			if res.stdout != "" {
				t.Errorf("a refused run printed on stdout: %q", res.stdout)
			}
			if after := b.buildSnapshot(t); after != before {
				t.Fatalf("the refusal left a trace.\nbefore:\n%s\nafter:\n%s", before, after)
			}
		})
	}
}

func mustBuildUp(t *testing.T, b *tableBench, id string) buildRecord {
	t.Helper()
	rec, _ := buildUp(t, b, id, nil)
	return rec
}

// TestBuildNewRefusesWhenTheMachineIsNotReady: a FAIL in doctor's static checks
// ends it with nothing changed, exit 1, and the skill it needs is the build one
// under the roots its peers' harnesses read.
func TestBuildNewRefusesWhenTheMachineIsNotReady(t *testing.T) {
	t.Run("the build skill is missing under the Claude root", func(t *testing.T) {
		b := newTableBench(t)
		link := filepath.Join(b.db.home, ".claude", "skills", "parley-build")
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		before := b.buildSnapshot(t)
		res := b.run(t, b.buildArgs("b1", nil)...)
		if res.code != 1 || !strings.Contains(res.stderr, link) || !strings.Contains(res.stderr, "nothing was changed") {
			t.Fatalf("exit %d, want 1 naming %s\n%s", res.code, link, res.stderr)
		}
		b.wantUnchanged(t, withoutCalls(before)+"codex calls: 0\n")
	})
	t.Run("the table skill is not needed", func(t *testing.T) {
		b := newTableBench(t)
		for _, root := range []string{".claude", ".agents"} {
			if err := os.Remove(filepath.Join(b.db.home, root, "skills", "parley-table")); err != nil {
				t.Fatal(err)
			}
		}
		buildUp(t, b, "b1", nil)
	})
	t.Run("a Claude-only build does not read the Codex root", func(t *testing.T) {
		b := newTableBench(t)
		if err := os.Remove(filepath.Join(b.db.home, ".agents", "skills", "parley-build")); err != nil {
			t.Fatal(err)
		}
		before := b.buildSnapshot(t)
		res := b.run(t, b.buildArgs("b1", nil)...)
		if res.code != 1 || !strings.Contains(res.stderr, filepath.Join(".agents", "skills", "parley-build")) {
			t.Fatalf("a build with a Codex peer: exit %d, want 1 naming the Codex root's link\n%s", res.code, res.stderr)
		}
		b.wantUnchanged(t, withoutCalls(before)+"codex calls: 0\n")
		buildUp(t, b, "b2", []string{"claude-a=claude", "claude-b=claude"})
	})
	t.Run("a peer's nick is in use", func(t *testing.T) {
		b := newTableBench(t)
		held, err := state.LockNick(b.base, "claude-a")
		if err != nil {
			t.Fatal(err)
		}
		defer held.Release()
		before := b.buildSnapshot(t)
		res := b.run(t, b.buildArgs("b1", nil)...)
		if res.code != 1 || !strings.Contains(res.stderr, `"claude-a"`) || !strings.Contains(res.stderr, "is in use by a running pump or Claude Code session") {
			t.Fatalf("exit %d, want 1 naming claude-a\n%s", res.code, res.stderr)
		}
		b.wantUnchanged(t, withoutCalls(before)+"codex calls: 0\n")
	})
}

// TestBuildNewRollsBackFully: a step that fails midway leaves no worktree, no
// branch, no directory and no thread record, and every Codex thread it started
// is deleted. The peers' lock files stay, released.
func TestBuildNewRollsBackFully(t *testing.T) {
	cases := []struct {
		name  string
		peers []string
		modes map[int]string
		hook  string // a post-checkout hook for the target
		want  string
		// deleted maps the number of a delete call to the exec call whose thread
		// it deletes.
		deleted map[int]int
		calls   int
	}{
		{"codex exits nonzero", []string{"codex-a=codex", "claude-a=claude"}, map[int]string{1: "fail"}, "", "boom: not logged in", nil, 1},
		{"the second Codex peer fails after the first succeeded", []string{"codex-a=codex", "codex-b=codex"}, map[int]string{2: "fail"}, "", "boom", map[int]int{3: 1}, 3},
		{"the thread is announced, then the turn fails", []string{"codex-a=codex", "claude-a=claude"}, map[int]string{1: "failafter"}, "", "the model went away", map[int]int{2: 1}, 2},
		{"no turn.completed", []string{"claude-a=claude", "codex-b=codex"}, map[int]string{1: "nocomplete"}, "", "no turn.completed event", map[int]int{2: 1}, 2},
		{"a thread that is not a UUID", []string{"codex-a=codex", "claude-a=claude"}, map[int]string{1: "badid"}, "", "not a UUID", nil, 1},
		{"the second worktree's post-checkout hook fails", []string{"codex-a=codex", "claude-a=claude"}, nil,
			"#!/bin/sh\ncase \"$PWD\" in *claude-a) echo 'hook says no' >&2; exit 1 ;; esac\nexit 0\n", "worktree add", nil, 0},
		{"the first worktree's post-checkout hook fails", []string{"claude-a=claude", "codex-a=codex"}, nil,
			"#!/bin/sh\ncase \"$PWD\" in *claude-a) echo 'hook says no' >&2; exit 1 ;; esac\nexit 0\n", "worktree add", nil, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := newTableBench(t)
			for n, m := range c.modes {
				b.setMode(t, n, m)
			}
			if c.hook != "" {
				hook := filepath.Join(b.repo, ".git", "hooks", "post-checkout")
				if err := os.MkdirAll(filepath.Dir(hook), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(hook, []byte(c.hook), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			before := b.buildSnapshot(t)
			res := b.run(t, b.buildArgs("b1", c.peers)...)
			if res.code != 1 {
				t.Fatalf("exit %d, want 1\nstdout: %s\nstderr: %s", res.code, res.stdout, res.stderr)
			}
			if !strings.Contains(res.stderr, c.want) {
				t.Errorf("stderr does not say %q:\n%s", c.want, res.stderr)
			}
			if !strings.Contains(res.stderr, "rolled back") || !strings.Contains(res.stderr, "worktree") || !strings.Contains(res.stderr, "branch parley/b1/") {
				t.Errorf("stderr does not say what was rolled back:\n%s", res.stderr)
			}
			if res.stdout != "" {
				t.Errorf("a failed run printed on stdout: %q", res.stdout)
			}
			if got := b.callCount(); got != c.calls {
				t.Errorf("codex was called %d times, want %d", got, c.calls)
			}
			deletes := 0
			for n := 1; n <= b.callCount(); n++ {
				argv := b.call(t, n)
				if argv[0] != "delete" {
					continue
				}
				deletes++
				exec, ok := c.deleted[n]
				if !ok {
					t.Errorf("call %d is %q, an unexpected delete", n, argv)
					continue
				}
				if strings.Join(argv, " ") != "delete --force "+fakeThread(exec) {
					t.Errorf("call %d is %q, want delete --force %s", n, argv, fakeThread(exec))
				}
			}
			if deletes != len(c.deleted) {
				t.Errorf("%d delete calls, want %d", deletes, len(c.deleted))
			}
			// The machine, its branches included, is as it was, but for the fake's
			// call log.
			if after := b.buildSnapshot(t); withoutCalls(after) != withoutCalls(before) {
				t.Fatalf("the run left a trace.\nbefore:\n%s\nafter:\n%s", withoutCalls(before), withoutCalls(after))
			}
			// No thread record, no build directory, no reply directory, no branch.
			for _, nick := range []string{"codex-a", "claude-a", "codex-b"} {
				tr, _ := state.OpenThreadRecord(b.base, nick, b.buildStream("b1"))
				if got, err := tr.Read(); err != nil || got != "" {
					t.Errorf("a thread record for %s is left: %q, %v", nick, got, err)
				}
			}
			if _, err := os.Lstat(filepath.Join(b.base, "builds")); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("%s is left (%v)", filepath.Join(b.base, "builds"), err)
			}
			if refs := gitIn(t, b.repo, "for-each-ref", "refs/heads/parley"); refs != "" {
				t.Errorf("a branch is left:\n%s", refs)
			}
			// The lock files stay, released.
			for _, nick := range strings.Split(strings.Join(nicksOf(c.peers), " "), " ") {
				if _, err := os.Stat(filepath.Join(b.base, nick, "lock")); err != nil {
					t.Errorf("%s's lock file is gone: %v", nick, err)
				}
				l, err := state.LockNick(b.base, nick)
				if err != nil {
					t.Errorf("%s's lock is held after the rollback: %v", nick, err)
					continue
				}
				l.Release()
			}
		})
	}
}

func nicksOf(peers []string) []string {
	var out []string
	for _, p := range peers {
		nick, _, _ := strings.Cut(p, "=")
		out = append(out, nick)
	}
	return out
}

// TestDeleteMadeBranchDeletesOnlyWhatThisRunMadeAndLeftAtItsCommit: the rollback
// deletes a branch only when its reflog begins with this run's message and it
// still holds the commit it was made at. A branch another creator made (even at
// that commit, even with a message of the same shape but another nonce), one that
// moved, and one whose reflog cannot be read to its oldest entry are left, and a
// branch that is not there was never made.
func TestDeleteMadeBranchDeletesOnlyWhatThisRunMadeAndLeftAtItsCommit(t *testing.T) {
	b := newTableBench(t)
	head := b.head(t)
	const msg = "parley build new x 00112233445566778899aabbccddeeff"
	made := func(branch, message string) {
		t.Helper()
		gitIn(t, b.repo, "update-ref", "--create-reflog", "-m", message, "refs/heads/"+branch, head, "")
	}
	made("parley/x/a", msg)
	made("parley/x/b", msg)
	made("parley/x/otherrun", "parley build new x ffeeddccbbaa99887766554433221100")
	made("parley/x/long", msg)
	gitIn(t, b.repo, "branch", "parley/x/theirs") // reflog: "branch: Created from HEAD"
	if err := os.WriteFile(filepath.Join(b.repo, "new.txt"), []byte("n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, b.repo, "add", "new.txt")
	gitIn(t, b.repo, "-c", "commit.gpgsign=false", "commit", "-q", "-m", "move")
	gitIn(t, b.repo, "update-ref", "refs/heads/parley/x/b", b.head(t))

	exists := func(branch string) bool { return gitIn(t, b.repo, "for-each-ref", "refs/heads/"+branch) != "" }

	if err := deleteMadeBranch(b.repo, "parley/x/a", head, msg); err != nil {
		t.Fatalf("deleting a branch this run made, at the commit it was made at: %v", err)
	}
	if exists("parley/x/a") {
		t.Error("the branch this run made is still there")
	}
	var left leftAlone
	if err := deleteMadeBranch(b.repo, "parley/x/b", head, msg); !errors.As(err, &left) || !strings.Contains(err.Error(), "no longer at") {
		t.Errorf("a branch that moved: %v, want it left alone, saying it moved", err)
	}
	if !exists("parley/x/b") {
		t.Error("the branch that moved is gone")
	}
	for _, branch := range []string{"parley/x/otherrun", "parley/x/theirs"} {
		if err := deleteMadeBranch(b.repo, branch, head, msg); !errors.As(err, &left) || !strings.Contains(err.Error(), "another creator") {
			t.Errorf("%s: %v, want it left alone as another creator's", branch, err)
		}
		if !exists(branch) {
			t.Errorf("%s, which this run did not make, is gone", branch)
		}
	}
	if err := deleteMadeBranch(b.repo, "parley/x/never", head, msg); !errors.Is(err, errNothingToUndo) {
		t.Errorf("a branch that is not there: %v, want nothing to undo", err)
	}
	// A reflog past the bound cannot be read to its oldest entry: left, and said.
	defer func(old int) { gitOutputMax = old }(gitOutputMax)
	gitOutputMax = 10
	if err := deleteMadeBranch(b.repo, "parley/x/long", head, msg); !errors.As(err, &left) || !strings.Contains(err.Error(), "reflog could not be read") {
		t.Errorf("a reflog past the bound: %v, want it left alone, saying it could not be read", err)
	}
	if !exists("parley/x/long") {
		t.Error("a branch whose reflog could not be read is gone")
	}
}

// TestBuildNewCodexConfigNamesNoWritableRootUnlessAsked: without
// --writable-root the Codex config has no writable_roots; with it the config
// holds exactly the physical paths, TOML-quoted, and network access stays on.
func TestBuildNewCodexConfigNamesNoWritableRootUnlessAsked(t *testing.T) {
	b := newTableBench(t)
	real := filepath.Join(b.root, "rw")
	odd := filepath.Join(b.root, `we"ird\dir é`)
	for _, d := range []string{real, odd} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(b.root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	physical, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	physicalOdd, err := filepath.EvalSymlinks(odd)
	if err != nil {
		t.Fatal(err)
	}

	rec, res := buildUp(t, b, "b1", []string{"codex-a=codex", "codex-b=codex"})
	for _, p := range rec.Peers {
		cfg := string(mustRead(t, filepath.Join(p.Worktree, ".codex", "config.toml")))
		if strings.Contains(cfg, "writable_roots") || !strings.Contains(cfg, "network_access = true") {
			t.Errorf("%s's config without --writable-root:\n%s", p.Nick, cfg)
		}
	}
	if strings.Contains(res.stdout, "--writable-root asked") {
		t.Errorf("stdout names a writable root nobody asked for:\n%s", res.stdout)
	}
	mustTable(t, b, b.buildEndArgs("b1")...)

	rec, res = buildUp(t, b, "b2", []string{"codex-a=codex", "codex-b=codex"},
		"--writable-root", link, "--writable-root", odd, "--writable-root", real)
	wantRoots := `writable_roots = ["` + physical + `", "` + strings.ReplaceAll(strings.ReplaceAll(physicalOdd, `\`, `\\`), `"`, `\"`) + `"]` + "\n"
	for _, p := range rec.Peers {
		cfg := string(mustRead(t, filepath.Join(p.Worktree, ".codex", "config.toml")))
		want := "sandbox_mode = \"workspace-write\"\napproval_policy = \"on-request\"\n\n[sandbox_workspace_write]\nnetwork_access = true\n" + wantRoots
		if cfg != want {
			t.Errorf("%s's config\n%s\nwant\n%s", p.Nick, cfg, want)
		}
	}
	if !strings.Contains(res.stdout, physical) {
		t.Errorf("stdout does not name the writable root:\n%s", res.stdout)
	}
	if strings.Contains(res.stderr, "holds or is inside the target's git directory") {
		t.Errorf("a root that is not the target's git directory drew a warning about it:\n%s", res.stderr)
	}
}

// TestBuildNewRunbookChecksWhatIsReadFromTheTargetsSide: the printed check that
// the target ignores .director/ names the first peer's admin directory and work
// tree, so a peer that rewrote its worktree's .git file does not change what it
// reads. The line is run, from a directory that is not the worktree.
func TestBuildNewRunbookChecksWhatIsReadFromTheTargetsSide(t *testing.T) {
	b := newTableBench(t)
	rec, res := buildUp(t, b, "b1", []string{"claude-a=claude", "claude-b=claude"})
	first := rec.Peers[0]
	line := lineWith(t, res.stdout, "check-ignore -q .director/x")
	line, ok := strings.CutSuffix(line, " && echo ignored")
	if !ok {
		t.Fatalf("the check line %q does not end in && echo ignored", line)
	}
	words := shellWords(t, line)
	want := []string{"git", "-C", first.Worktree, "--git-dir", first.AdminGitDir, "--work-tree", first.Worktree, "check-ignore", "-q", ".director/x"}
	if strings.Join(words, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("check line %q, want %q", words, want)
	}
	run := func() int {
		cmd := exec.Command(words[0], words[1:]...)
		cmd.Dir = b.root
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		err := cmd.Run()
		var ee *exec.ExitError
		switch {
		case err == nil:
			return 0
		case errors.As(err, &ee):
			return ee.ExitCode()
		}
		t.Fatalf("run the check line: %v", err)
		return -1
	}

	// The target does not ignore .director/; the planted repository the peer
	// points its .git file at does.
	p := newPlant(t, b.root)
	exclude := func() { writeFileT(t, filepath.Join(p.gitdir, "info", "exclude"), "/.director/\n") }
	exclude()
	repoint(t, first.Worktree, p.gitdir)
	if _, err := gitOut(t, first.Worktree, "check-ignore", "-q", ".director/x"); err != nil {
		t.Fatalf("a plain git check-ignore in the worktree does not follow its .git file, so the test proves nothing: %v", err)
	}
	// That plain run was the plant's to run; the printed line meets it fresh.
	p.rebuild(t)
	exclude()
	if code := run(); code != 1 {
		t.Errorf("the printed check exited %d, want 1: it read the planted repository the peer pointed git at", code)
	}
	noSentinel(t, p)

	// The target ignores it, as the runbook says to arrange.
	writeFileT(t, filepath.Join(b.repo, ".git", "info", "exclude"), "/.director/\n")
	if code := run(); code != 0 {
		t.Errorf("the printed check exited %d, want 0 once the target's info/exclude names /.director/", code)
	}
	noSentinel(t, p)
}

// TestBuildNewRunbookWording pins what the runbook says where the wording is
// load-bearing: the Claude gate checks the channel notice, a sandbox sentence
// names the roots the proof showed, and message one holds nothing that is not
// the message.
func TestBuildNewRunbookWording(t *testing.T) {
	t.Run("the gate and the sandbox, no --writable-root", func(t *testing.T) {
		b := newTableBench(t)
		_, res := buildUp(t, b, "b1", nil)
		for _, want := range []string{
			"- every Claude peer's /mcp shows parley connected, and the notice below its startup banner reads",
			`"Channels (experimental) messages from server:parley inject directly in this session"`,
			"without --dangerously-load-development-channels, which never hears the channel",
			"Its sandbox writes its worktree, $TMPDIR and /tmp, plus any global roots in your ~/.codex/config.toml, and",
			// An approval prompt at the Codex peer's terminal stops the run.
			"Each approval prompt there stops the run until you answer it, because the",
			"pump delivers between Codex turns only: keep that terminal in view.",
		} {
			if !strings.Contains(res.stdout, want) {
				t.Errorf("stdout lacks %q:\n%s", want, res.stdout)
			}
		}
	})
	t.Run("the sandbox, --writable-root", func(t *testing.T) {
		b := newTableBench(t)
		root := filepath.Join(b.root, "rw")
		if err := os.Mkdir(root, 0o755); err != nil {
			t.Fatal(err)
		}
		physical, err := filepath.EvalSymlinks(root)
		if err != nil {
			t.Fatal(err)
		}
		_, res := buildUp(t, b, "b1", nil, "--writable-root", root)
		for _, want := range []string{
			"Its sandbox writes its worktree, $TMPDIR, /tmp and " + physical + ", as --writable-root asked.",
			"Naming a root replaced your global Codex roots: re-pass any it needs.",
		} {
			if !strings.Contains(res.stdout, want) {
				t.Errorf("stdout lacks %q:\n%s", want, res.stdout)
			}
		}
	})
	t.Run("message one", func(t *testing.T) {
		b := newTableBench(t)
		_, res := buildUp(t, b, "b1", nil)
		lines := strings.Split(res.stdout, "\n")
		start, end := -1, -1
		for i, l := range lines {
			switch strings.TrimSpace(l) {
			case "RUN b1":
				start = i
			case "owns <nick>: <path>":
				end = i
			}
		}
		if start < 0 || end < start {
			t.Fatalf("no message-one block in:\n%s", res.stdout)
		}
		for _, l := range lines[start : end+1] {
			if strings.Contains(l, "optional") || strings.Contains(l, "(") {
				t.Errorf("message one, handed over verbatim, holds the note %q", l)
			}
		}
		// The note prompts for the owns lines, and does not tell the human to delete one.
		if note := lineWith(t, res.stdout, "name each peer's files: one owns <nick>: <path> line per path"); lineIndex(lines, note) <= end {
			t.Errorf("the note about the owns lines is inside the block")
		}
		lineWith(t, res.stdout, "Leave the line out only when it does not split that way.")
		if strings.Contains(res.stdout, "delete that line") {
			t.Errorf("the runbook still tells the human to delete the owns line:\n%s", res.stdout)
		}
	})
	t.Run("integration", func(t *testing.T) {
		b := newTableBench(t)
		_, res := buildUp(t, b, "b1", nil)
		lines := strings.Split(res.stdout, "\n")
		// The integrator is the second --peer, the one message one does not go to;
		// the step sits after message one and before build end.
		step := lineWith(t, res.stdout, "Integration, once both peers have agreed every item: claude-a, the second participant")
		msgOne, over := lineIndex(lines, "RUN b1"), -1
		for i, l := range lines {
			if strings.HasPrefix(l, "When it is over:") {
				over = i
			}
		}
		if at := lineIndex(lines, step); at <= msgOne || over <= at {
			t.Errorf("the integration step (line %d) is not between message one (%d) and build end (%d):\n%s", at, msgOne, over, res.stdout)
		}
		// The step runs from its first line to the line that closes the runbook, and
		// carries the order the skill's integration sequence has: a throwaway test
		// merge of the other peer's branch, then the ask, the human's check of both
		// commits, and the yes that authorizes the real merge.
		section := strings.Join(lines[lineIndex(lines, step):over], "\n")
		for _, want := range []string{
			"tests a throwaway merge of codex-a's branch; codex-a posts END after a passing test, and only then does claude-a ask you in the channel to approve the real merge, naming both branches and both commits",
			"rev-parse refs/heads/parley/b1/codex-a refs/heads/parley/b1/claude-a",
			"there is nothing to merge: no ask, and claude-a's branch is the result",
			"that line is the approval, and the merge it authorizes",
			"is of exactly the commit the ask named",
			"A question or a condition is not a yes",
		} {
			if !strings.Contains(section, want) {
				t.Errorf("the integration step lacks %q:\n%s", want, section)
			}
		}
		if strings.Contains(section, "merges codex-a's branch into its own") {
			t.Errorf("the integration step still says the integrator merges before it asks:\n%s", section)
		}
		if strings.Contains(section, "which has posted END by then") {
			t.Errorf("the integration step still says the other peer has posted END before the integrator tests:\n%s", section)
		}
		// Both branches are named in full, so a tag of the same name cannot answer for one.
		if strings.Contains(section, "rev-parse parley/") {
			t.Errorf("the integration step checks a branch by its short name:\n%s", section)
		}
	})
}

func lineIndex(lines []string, trimmed string) int {
	for i, l := range lines {
		if strings.TrimSpace(l) == trimmed {
			return i
		}
	}
	return -1
}

// TestBuildNewWarnsOfAWritableRootThatHoldsTheTargetsGitDir: the human may
// choose it, and is told what it hands the peer.
func TestBuildNewWarnsOfAWritableRootThatHoldsTheTargetsGitDir(t *testing.T) {
	b := newTableBench(t)
	_, res := buildUp(t, b, "b1", []string{"codex-a=codex", "claude-a=claude"}, "--writable-root", filepath.Join(b.repo, ".git"))
	if !strings.Contains(res.stderr, "warning: --writable-root") || !strings.Contains(res.stderr, "hooks and config") {
		t.Errorf("no warning for a writable root that is the target's git directory:\n%s", res.stderr)
	}
}

// TestTomlQuote: a basic string, with the backslash, the quote and DEL escaped,
// and the rest of a path as it is.
func TestTomlQuote(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"/a/b", `"/a/b"`},
		{`/a"b`, `"/a\"b"`},
		{`/a\b`, `"/a\\b"`},
		{"/é/日本", `"/é/日本"`},
		{"/a\x7fb", `"/a\u007Fb"`},
		{"/a\x01b", `"/a\u0001b"`},
	} {
		if got := tomlQuote(c.in); got != c.want {
			t.Errorf("tomlQuote(%q) = %s, want %s", c.in, got, c.want)
		}
	}
}

// TestPhysicalRootsRefusesWhatIsNotAPhysicalDirectory.
func TestPhysicalRootsRefusesWhatIsNotAPhysicalDirectory(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	physical, _ := filepath.EvalSymlinks(real)
	got, err := physicalRoots([]string{link, real, link + "/../real"})
	if err != nil || len(got) != 1 || got[0] != physical {
		t.Errorf("physicalRoots = %q, %v, want %q once", got, err, physical)
	}
	if got, err := physicalRoots(nil); err != nil || got != nil {
		t.Errorf("physicalRoots(nil) = %q, %v", got, err)
	}
	for _, bad := range []string{"", "rel", file, filepath.Join(dir, "none"), filepath.Join(dir, "a\tb"), "/\xff"} {
		if _, err := physicalRoots([]string{bad}); err == nil {
			t.Errorf("physicalRoots accepted %q", bad)
		}
	}
}

// TestBuildRecordCheckRefusesWhatBuildNewDidNotWrite.
func TestBuildRecordCheckRefusesWhatBuildNewDidNotWrite(t *testing.T) {
	b := newTableBench(t)
	good, _ := buildUp(t, b, "b1", nil)
	if err := good.check(b.base, "b1"); err != nil {
		t.Fatalf("the record build new wrote is refused: %v", err)
	}
	for _, c := range []struct {
		name string
		edit func(r *buildRecord)
		want string
	}{
		{"another id", func(r *buildRecord) { r.ID = "b2" }, "not \"b1\""},
		{"a relative target", func(r *buildRecord) { r.Target = "t" }, "not an absolute path"},
		{"a relative common dir", func(r *buildRecord) { r.CommonGit = ".git" }, "not an absolute path"},
		{"a reply dir of another build", func(r *buildRecord) { r.ReplyDir = filepath.Join(b.tmp, "parley-build-b2-1") }, "not a reply directory of build"},
		{"a table's reply dir", func(r *buildRecord) { r.ReplyDir = filepath.Join(b.tmp, "parley-table-b1-1") }, "not a reply directory"},
		{"no stream", func(r *buildRecord) { r.Stream = "" }, "no stream"},
		{"a base commit that is not one", func(r *buildRecord) { r.BaseCommit = "HEAD" }, "not a commit id"},
		{"three peers", func(r *buildRecord) { r.Peers = append(r.Peers, r.Peers[0]) }, "lists 3 peers"},
		{"one peer", func(r *buildRecord) { r.Peers = r.Peers[:1] }, "lists 1 peers"},
		{"a reserved nick", func(r *buildRecord) { r.Peers[0].Nick = "builds" }, "directory the builds live in"},
		{"a bad nick", func(r *buildRecord) { r.Peers[0].Nick = "a/b" }, "peer nick"},
		{"one nick twice", func(r *buildRecord) { r.Peers[1].Nick = strings.ToUpper(r.Peers[0].Nick) }, "is also peer"},
		{"a harness that is not one", func(r *buildRecord) { r.Peers[0].Harness = "gemini" }, "neither claude nor codex"},
		{"a worktree elsewhere", func(r *buildRecord) { r.Peers[0].Worktree = b.repo }, "worktree"},
		{"a worktree of another build", func(r *buildRecord) {
			r.Peers[0].Worktree = filepath.Join(b.base, "builds", "b2", r.Peers[0].Nick)
		}, "worktree"},
		{"another branch", func(r *buildRecord) { r.Peers[0].Branch = "main" }, "branch"},
		{"an admin dir elsewhere", func(r *buildRecord) { r.Peers[0].AdminGitDir = b.root }, "admin_git_dir"},
		{"a thread that is not a UUID", func(r *buildRecord) { r.Peers[0].Thread = "nope" }, "not a UUID"},
		{"a reply file elsewhere", func(r *buildRecord) { r.Peers[0].ReplyFile = filepath.Join(b.root, "x.reply") }, "reply_file"},
		{"a model with a control character", func(r *buildRecord) { r.Peers[0].Model = "a\x1bb" }, "control character"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := good
			r.Peers = append([]peerRecord(nil), good.Peers...)
			c.edit(&r)
			if err := r.check(b.base, "b1"); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("check = %v, want a refusal saying %q", err, c.want)
			}
		})
	}
}

// TestBuildNewHelpAndUsage: the verbs are documented, and -h answers.
func TestBuildNewHelpAndUsage(t *testing.T) {
	top := mustRun(t, "", "-h").stdout
	for _, want := range []string{"parley build   new <id>", "--peer <nick>=<claude|codex>[:<model>]", "--writable-root <abs dir>", "parley build   end <id> [--force]",
		"build    set a two-peer build up", "never a branch", "parley neither checks nor calls director", "a third is refused", `none of them "tables" or "builds"`,
		"builds live in <dir>/builds/<id>/", "the worktree's git", "never from the worktree's own .git file", "table new\" or \"parley build new\""} {
		if !strings.Contains(top, want) {
			t.Errorf("usage does not mention %q", want)
		}
	}
	for _, verb := range []string{"new", "end"} {
		help := runParley(t, "", "build", verb, "-h")
		if help.code != 0 || !strings.Contains(help.stdout, "parley build "+verb) || help.stderr != "" {
			t.Errorf("build %s -h exited %d and printed %q on stdout, %q on stderr", verb, help.code, help.stdout, help.stderr)
		}
	}
	if help := mustRun(t, "", "build", "-h"); !strings.Contains(help.stdout, "parley build new") || !strings.Contains(help.stdout, "parley build end") {
		t.Errorf("build -h prints %q", help.stdout)
	}
	for _, args := range [][]string{{"build"}, {"build", "frobnicate"}, {"build", "new"}, {"build", "end"}, {"build", "end", "NOPE"}, {"build", "end", "gone"}} {
		if res := runParley(t, "", args...); res.code != 2 {
			t.Errorf("parley %v exited %d, want 2\n%s", args, res.code, res.stderr)
		}
	}
	if res := runParley(t, "", "build", "end", "gone", "--state-dir", t.TempDir()); res.code != 2 || !strings.Contains(res.stderr, `no build "gone"`) {
		t.Errorf("ending a build that is not there: exit %d\n%s", res.code, res.stderr)
	}
}

// TestTableNewRefusesAnIdLiveAsABuildAndTheNickBuilds: the two new refusals in
// table new, exit 2 with nothing changed.
func TestTableNewRefusesAnIdLiveAsABuildAndTheNickBuilds(t *testing.T) {
	t.Run("an id live as a build", func(t *testing.T) {
		b := newTableBench(t)
		buildUp(t, b, "x1", nil)
		before := b.buildSnapshot(t)
		res := b.run(t, b.newArgs("x1", []string{"claude-b=claude"})...)
		if res.code != 2 || !strings.Contains(res.stderr, `"x1" is already a build`) || !strings.Contains(res.stderr, "parley build end x1") {
			t.Fatalf("exit %d, want 2 naming the build\n%s", res.code, res.stderr)
		}
		b.wantUnchanged(t, before[:strings.Index(before, "refs:\n")])
	})
	t.Run("the seat nick builds", func(t *testing.T) {
		b := newTableBench(t)
		before := b.snapshot(t)
		for _, nick := range []string{"builds", "Builds"} {
			res := b.run(t, b.newArgs("t1", []string{nick + "=claude"})...)
			if res.code != 2 || !strings.Contains(res.stderr, "the directory the builds live in") {
				t.Fatalf("seat %s: exit %d, want 2\n%s", nick, res.code, res.stderr)
			}
		}
		b.wantUnchanged(t, before)
	})
	t.Run("the seat nick tables is still refused", func(t *testing.T) {
		b := newTableBench(t)
		res := b.run(t, b.newArgs("t1", []string{"tables=claude"})...)
		if res.code != 2 || !strings.Contains(res.stderr, "the directory the tables live in") {
			t.Fatalf("exit %d\n%s", res.code, res.stderr)
		}
	})
}

// gitOut runs git for a test that wants its exit status too.
func gitOut(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

var _ = context.Background
var _ = fmt.Sprintf
