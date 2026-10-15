package main

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/colinsurprenant/parley/internal/state"
)

// Tests for what a build or a table does when something else has touched the
// machine meanwhile, or has left it half-changed: a branch someone else made
// (rollback), a record that setup would write and teardown would refuse (the
// caps), and what `end` decides on evidence that is partial (markers, the
// fallback when git cannot answer, every peer's result, ignored files under
// --force).

// ---- Class B: the rollback deletes only what this run made ----

// racingGit puts a git of its own first on the PATH. It runs the real git, except
// that the first time it is asked to create the branch or to add a worktree on
// it, it makes the branch itself first, at the target's HEAD, as another run
// would have between parley's last look and its call. It returns the environment
// to run parley in.
func racingGit(t *testing.T, b *tableBench, branch string) []string {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"real=" + q(real) + "\nrepo=" + q(b.repo) + "\nbranch=" + q(branch) + "\nflag=" + q(filepath.Join(dir, "raced")) + "\n" +
		`case " $* " in
*" -d "*) exec "$real" "$@" ;;
esac
case " $* " in
*" update-ref "*"refs/heads/$branch "*|*" worktree add "*" $branch "*|*" worktree add "*" -b $branch "*)
	if [ ! -e "$flag" ]; then
		: > "$flag"
		"$real" -C "$repo" update-ref "refs/heads/$branch" "$("$real" -C "$repo" rev-parse HEAD)"
	fi ;;
esac
exec "$real" "$@"
`
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return append(b.env(), "PATH="+dir+string(os.PathListSeparator)+b.path+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestBuildNewKeepsABranchSomeoneElseMadeBetweenTheCheckAndTheAdd: the branch is
// made by this run, in one step that fails if the branch is there, and its undo is
// registered only after that. A branch that turned up in the window, at the very
// commit this run would have made it at, is not this run's to delete: the run
// fails, rolls back what it did make (the first peer's branch and worktree, when
// the second's is the one raced), and the other branch is still there.
func TestBuildNewKeepsABranchSomeoneElseMadeBetweenTheCheckAndTheAdd(t *testing.T) {
	for _, c := range []struct{ name, branch string }{
		{"the first peer's branch", "parley/b1/codex-a"},
		{"the second peer's branch", "parley/b1/claude-a"},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := newTableBench(t)
			head := b.head(t)
			env := racingGit(t, b, c.branch)
			res := runParleyEnvIn(t, "", env, b.buildArgs("b1", nil)...)
			if res.code != 2 || !strings.Contains(res.stderr, "already exists") || !strings.Contains(res.stderr, c.branch) {
				t.Fatalf("exit %d, want 2 saying the branch %s already exists\nstdout: %s\nstderr: %s", res.code, c.branch, res.stdout, res.stderr)
			}
			if refs := gitIn(t, b.repo, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads/parley"); refs != "refs/heads/"+c.branch+" "+head {
				t.Errorf("the branches left are\n%s\nwant only the one someone else made, %s at %s", refs, c.branch, head)
			}
			if wts := gitIn(t, b.repo, "worktree", "list", "--porcelain"); strings.Count(wts, "worktree ") != 1 {
				t.Errorf("a worktree of the run is left:\n%s", wts)
			}
			if got := baseListing(b.base); got != "(absent)\n" {
				t.Errorf("the run left a trace in the state base:\n%s", got)
			}
			if ents, _ := os.ReadDir(b.tmp); len(ents) != 0 {
				t.Errorf("the run left %d entries in the temp directory", len(ents))
			}
			if n := b.callCount(); n != 0 {
				t.Errorf("codex was called %d times before the branches were made", n)
			}
		})
	}
}

// ---- Class C: setup writes nothing that teardown refuses ----

// TestMemberModelsAreBoundedAtParse: a --seat or --peer model is at most modelMax
// bytes of the characters Claude Code's and Codex's model ids use, and the ids
// those take are accepted.
func TestMemberModelsAreBoundedAtParse(t *testing.T) {
	ok := []string{
		"opus", "sonnet[1m]", "opusplan", "claude-opus-4-5", "claude-sonnet-4-5-20250929", "claude-sonnet-4-5@20250929",
		"us.anthropic.claude-sonnet-4-5-20250929-v1:0",
		"arn:aws:bedrock:us-east-1:123456789012:inference-profile/us.anthropic.claude-sonnet-4-5-20250929-v1:0",
		"gpt-5", "gpt-5.1-codex-max", "o3", "openai/gpt-5", "qwen2.5-coder:32b", "a+b", strings.Repeat("a", modelMax),
	}
	for _, m := range ok {
		for _, flag := range []string{"--seat", "--peer"} {
			got, err := parseMember(flag, "x=codex:"+m)
			if err != nil || got.model != m {
				t.Errorf("%s with the model %q: %+v, %v", flag, m, got, err)
			}
		}
	}
	bad := []struct{ model, want string }{
		{strings.Repeat("a", modelMax+1), "over the 128 bytes"},
		{strings.Repeat("a", 70<<10), "over the 128 bytes"},
		{"two words", "a model is letters, digits"},
		{"semi;colon", "a model is letters, digits"},
		{"dollar$HOME", "a model is letters, digits"},
		{"back`tick", "a model is letters, digits"},
		{"quo'te", "a model is letters, digits"},
		{`dq"uote`, "a model is letters, digits"},
		{"back\\slash", "a model is letters, digits"},
		{"caf\u00e9", "a model is letters, digits"},
		{"new\nline", "control character"},
		{"-flag", "begins with '-'"},
		{"", "is empty"},
	}
	for _, c := range bad {
		_, err := parseMember("--peer", "x=codex:"+c.model)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("the model %.40q: %v, want a refusal saying %q", c.model, err, c.want)
		}
		if err != nil && len(err.Error()) > 1000 {
			t.Errorf("the refusal for a %d-byte model is %d bytes long: it echoes the model", len(c.model), len(err.Error()))
		}
	}
}

// TestNewRefusesAnOversizedModelBeforeAnyChange: build new and table new both,
// exit 2 with the machine as it was.
func TestNewRefusesAnOversizedModelBeforeAnyChange(t *testing.T) {
	long := strings.Repeat("m", modelMax+1)
	t.Run("build new", func(t *testing.T) {
		b := newTableBench(t)
		before := b.buildSnapshot(t)
		res := b.run(t, b.buildArgs("b1", []string{"codex-a=codex:" + long, "claude-a=claude"})...)
		if res.code != 2 || !strings.Contains(res.stderr, "the model is 129 bytes, over the 128 bytes") {
			t.Fatalf("exit %d, want 2 naming the cap\n%s", res.code, res.stderr)
		}
		wantBuildUnchanged(t, b, before)
	})
	t.Run("table new", func(t *testing.T) {
		b := newTableBench(t)
		before := b.buildSnapshot(t)
		res := b.run(t, b.newArgs("t1", []string{"codex-a=codex", "claude-a=claude:" + long})...)
		if res.code != 2 || !strings.Contains(res.stderr, "the model is 129 bytes, over the 128 bytes") {
			t.Fatalf("exit %d, want 2 naming the cap\n%s", res.code, res.stderr)
		}
		wantBuildUnchanged(t, b, before)
	})
}

// TestAModelAtTheCapRoundTripsThroughEnd: the longest model the parse allows, in
// every character the set holds, is kept in the record and the record is read
// back by end, which takes the build and the table down.
func TestAModelAtTheCapRoundTripsThroughEnd(t *testing.T) {
	model := strings.Repeat("a1._-:/@[]+", 12)[:modelMax]
	if len(model) != modelMax {
		t.Fatalf("the test model is %d bytes", len(model))
	}
	t.Run("build", func(t *testing.T) {
		b := newTableBench(t)
		rec, _ := buildUp(t, b, "b1", []string{"codex-a=codex:" + model, "claude-a=claude:" + model})
		for _, p := range rec.Peers {
			if p.Model != model {
				t.Errorf("peer %s's model in the record is %q", p.Nick, p.Model)
			}
		}
		mustBuildEnd(t, b, "b1")
		wantGone(t, rec.Peers[0].Worktree, rec.Peers[1].Worktree, filepath.Join(b.base, "builds", "b1"))
	})
	t.Run("table", func(t *testing.T) {
		b := newTableBench(t)
		mustTable(t, b, b.newArgs("t1", []string{"codex-a=codex:" + model, "claude-a=claude:" + model})...)
		mustTable(t, b, b.endArgs("t1")...)
		wantGone(t, filepath.Join(b.base, "tables", "t1"))
	})
}

// TestWriteRecordFileRefusesWhatItsReaderWouldRefuse: a record is checked against
// the bound its reader holds before anything is written, naming the bound; one at
// the bound is written and read back.
func TestWriteRecordFileRefusesWhatItsReaderWouldRefuse(t *testing.T) {
	type rec struct {
		S string `json:"s"`
	}
	dir := t.TempDir()
	empty, _ := encodedLen(t, rec{})
	// At the bound: the encoding is exactly max bytes.
	max := 200
	exact := rec{S: strings.Repeat("x", max-empty)}
	digest, err := writeRecordFile(dir, "r.json", "test", exact, max, "test end")
	if err != nil {
		t.Fatalf("a record of exactly %d bytes: %v", max, err)
	}
	b, err := readRecordFile(filepath.Join(dir, "r.json"), "test", max)
	if err != nil || len(b) != max || tableDigest(b) != digest {
		t.Fatalf("the record at the bound reads back as %d bytes, %v", len(b), err)
	}
	// One byte over: refused, naming the bound, nothing written.
	over := t.TempDir()
	_, err = writeRecordFile(over, "r.json", "test", rec{S: exact.S + "x"}, max, "test end")
	if err == nil || !strings.Contains(err.Error(), "would be 201 bytes, over the 200 bytes test end reads") {
		t.Fatalf("a record one byte over the bound: %v", err)
	}
	if ents, _ := os.ReadDir(over); len(ents) != 0 {
		t.Errorf("a refused record left %d files", len(ents))
	}
	// The real bounds are the readers' own.
	if _, err := writeTableRecord(over, tableRecord{ID: strings.Repeat("a", tableRecordMax)}); err == nil || !strings.Contains(err.Error(), "over the 65536 bytes table end reads") {
		t.Errorf("an oversized table record: %v", err)
	}
}

func encodedLen(t *testing.T, v any) (int, []byte) {
	t.Helper()
	dir := t.TempDir()
	if _, err := writeRecordFile(dir, "probe.json", "probe", v, 1<<20, "probe"); err != nil {
		t.Fatal(err)
	}
	raw := mustRead(t, filepath.Join(dir, "probe.json"))
	return len(raw), raw
}

// TestBuildNewRefusesARecordPastTheReadCapAndRollsBack: when the record build new
// has made would be larger than build end reads, the run fails at the write,
// naming the bound, and takes down everything it made. (The model's own bound
// keeps a real run far from it; the peer's model is lengthened here after
// planning, to put the record over.)
func TestBuildNewRefusesARecordPastTheReadCapAndRollsBack(t *testing.T) {
	b := newTableBench(t)
	t.Setenv("TMPDIR", b.tmp)
	before := b.buildSnapshot(t)
	plan, err := planBuild(context.Background(), buildInput{id: "b1", target: b.repo, targetGiven: true, chair: "colin", chairGiven: true,
		peers: []string{"claude-a=claude", "claude-b=claude"}, server: b.db.server, stateDir: b.base, codexBin: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	defer plan.releaseID()
	plan.peers[0].model = strings.Repeat("a", buildRecordMax)
	run := &buildRun{plan: plan, log: io.Discard}
	err = run.setUp(context.Background())
	run.rollback()
	run.release()
	if err == nil || !strings.Contains(err.Error(), "over the 65536 bytes build end reads") {
		t.Fatalf("setUp with a record past the cap = %v, want it refused naming the cap", err)
	}
	wantBuildUnchanged(t, b, before)
	if refs := gitIn(t, b.repo, "for-each-ref", "refs/heads/parley"); refs != "" {
		t.Errorf("a branch is left:\n%s", refs)
	}
}

// ---- Class D: what end decides on partial evidence ----

// removedSection is the part of a build end report between "Removed:" and the
// next blank line.
func removedSection(t *testing.T, stdout string) string {
	t.Helper()
	_, rest, ok := strings.Cut(stdout, "Removed:\n")
	if !ok {
		t.Fatalf("the report has no Removed section:\n%s", stdout)
	}
	section, _, _ := strings.Cut(rest, "\n\n")
	return section
}

// leftSection is the part of the report under "Left in place", or "".
func leftSection(stdout string) string {
	_, rest, ok := strings.Cut(stdout, "Left in place")
	if !ok {
		return ""
	}
	_, rest, _ = strings.Cut(rest, ":\n")
	section, _, _ := strings.Cut(rest, "\n\n")
	return section
}

// TestBuildEndKeepsThePeersStateWhenAMarkerDisagreesWithTheRecord: a marker that is
// there and does not match the sealed record is evidence the record changed, so
// the stream and the nicks it names are not shown to be the build's: the cursor
// state and thread records are listed, not removed. A marker that is missing is no
// evidence, and the state goes as before.
func TestBuildEndKeepsThePeersStateWhenAMarkerDisagreesWithTheRecord(t *testing.T) {
	zeros := "sha256:" + strings.Repeat("0", 64)
	type setup func(t *testing.T, b *tableBench, rec buildRecord)
	keep := map[string]setup{
		"a worktree marker with another digest": func(t *testing.T, b *tableBench, rec buildRecord) {
			writeFileT(t, filepath.Join(rec.peer(t, "claude-a").Worktree, ".parley-build"), "b1\nclaude-a codex-a\n"+zeros+"\n")
		},
		"a worktree marker for another build": func(t *testing.T, b *tableBench, rec buildRecord) {
			writeFileT(t, filepath.Join(rec.peer(t, "codex-a").Worktree, ".parley-build"), "b9\nclaude-a codex-a\n"+zeros+"\n")
		},
		"a worktree marker that is not a marker": func(t *testing.T, b *tableBench, rec buildRecord) {
			writeFileT(t, filepath.Join(rec.peer(t, "codex-a").Worktree, ".parley-build"), "hello\n")
		},
		"the reply directory's marker with another digest": func(t *testing.T, b *tableBench, rec buildRecord) {
			writeFileT(t, filepath.Join(rec.ReplyDir, ".parley-build"), "b1\nclaude-a codex-a\n"+zeros+"\n")
		},
		"a build.json changed by a byte": func(t *testing.T, b *tableBench, rec buildRecord) {
			f, err := os.OpenFile(filepath.Join(b.base, "builds", "b1", "build.json"), os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if _, err := f.WriteString("\n"); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, edit := range keep {
		t.Run(name, func(t *testing.T) {
			b := newTableBench(t)
			rec, _ := buildUp(t, b, "b1", nil)
			edit(t, b, rec)
			res := mustBuildEnd(t, b, "b1")
			t.Logf("stdout:\n%s", res.stdout)
			tr, _ := state.OpenThreadRecord(b.base, "codex-a", rec.Stream)
			if got, err := tr.Read(); err != nil || got != fakeThread(1) {
				t.Errorf("the thread record was removed on a marker that disagrees with the record: %q, %v", got, err)
			}
			dir, _ := state.StreamDir(b.base, "codex-a", rec.Stream)
			if left := leftSection(res.stdout); !strings.Contains(left, "cursor state and thread record of peer codex-a: "+dir) {
				t.Errorf("the thread record is not listed as left in place:\n%s", res.stdout)
			}
			if removed := removedSection(t, res.stdout); strings.Contains(removed, "cursor state and thread record") {
				t.Errorf("the report says the state was removed:\n%s", res.stdout)
			}
			// The rest goes on: the worktrees and the build directory are the build's by
			// their own proofs.
			wantGone(t, rec.Peers[0].Worktree, rec.Peers[1].Worktree, filepath.Join(b.base, "builds", "b1"))
		})
	}
	t.Run("every marker missing", func(t *testing.T) {
		b := newTableBench(t)
		rec, _ := buildUp(t, b, "b1", nil)
		for _, p := range rec.Peers {
			if err := os.Remove(filepath.Join(p.Worktree, ".parley-build")); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Remove(filepath.Join(rec.ReplyDir, ".parley-build")); err != nil {
			t.Fatal(err)
		}
		res := mustBuildEnd(t, b, "b1")
		tr, _ := state.OpenThreadRecord(b.base, "codex-a", rec.Stream)
		if got, err := tr.Read(); err != nil || got != "" {
			t.Errorf("the thread record is left when every marker is missing: %q, %v", got, err)
		}
		if removed := removedSection(t, res.stdout); !strings.Contains(removed, "cursor state and thread record of peer codex-a") {
			t.Errorf("the report does not say the state was removed:\n%s", res.stdout)
		}
		wantGone(t, rec.Peers[0].Worktree, rec.Peers[1].Worktree, filepath.Join(b.base, "builds", "b1"))
	})
}

// TestEndDoesNotTakeThePathUnderTheOwnersDirectoryForAClaimWhenGitFailsForAnotherReason:
// the fallback claim ("git cannot say, but the path is under the build's, the
// table's, directory") is for a target that is gone. A target that is there and
// whose repository git cannot read is not: the worktrees are left (a build) or
// refused (a table), with git's error quoted, and the end finishes once the
// repository is back.
func TestEndDoesNotTakeThePathUnderTheOwnersDirectoryForAClaimWhenGitFailsForAnotherReason(t *testing.T) {
	breakRepo := func(t *testing.T, b *tableBench) (restore func()) {
		t.Helper()
		if err := os.Rename(filepath.Join(b.repo, ".git"), filepath.Join(b.repo, ".git-off")); err != nil {
			t.Fatal(err)
		}
		return func() {
			if err := os.Rename(filepath.Join(b.repo, ".git-off"), filepath.Join(b.repo, ".git")); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Run("build end", func(t *testing.T) {
		b := newTableBench(t)
		rec, _ := buildUp(t, b, "b1", nil)
		restore := breakRepo(t, b)
		res := mustBuildEnd(t, b, "b1", "--force")
		t.Logf("stdout:\n%s", res.stdout)
		left := leftSection(res.stdout)
		for _, p := range rec.Peers {
			if !strings.Contains(left, "worktree "+p.Worktree+" of peer "+p.Nick+": git cannot say whether it is a worktree of "+b.repo) {
				t.Errorf("the report does not leave peer %s's worktree for git's failure:\n%s", p.Nick, res.stdout)
			}
		}
		if !strings.Contains(left, "the target is there") || !strings.Contains(left, "not a git repository") {
			t.Errorf("the report does not quote git's error or say the target is there:\n%s", res.stdout)
		}
		wantExist(t, rec.Peers[0].Worktree, rec.Peers[1].Worktree, filepath.Join(b.base, "builds", "b1", "build.json"))
		restore()
		mustBuildEnd(t, b, "b1")
		wantGone(t, rec.Peers[0].Worktree, rec.Peers[1].Worktree, filepath.Join(b.base, "builds", "b1"))
	})
	t.Run("table end", func(t *testing.T) {
		b := newTableBench(t)
		wt, _ := tableUp(t, b, "t1")
		restore := breakRepo(t, b)
		res := b.run(t, b.endArgs("t1", "--force")...)
		if res.code != 1 || !strings.Contains(res.stderr, "will not remove "+wt) || !strings.Contains(res.stderr, "git cannot say whether it is a worktree of") ||
			!strings.Contains(res.stderr, "the target is there") || !strings.Contains(res.stderr, "not a git repository") {
			t.Fatalf("exit %d, want 1 refusing the worktree for git's error\n%s", res.code, res.stderr)
		}
		wantExist(t, wt, filepath.Join(b.base, "tables", "t1", "table.json"))
		restore()
		mustTable(t, b, b.endArgs("t1")...)
		wantGone(t, wt)
	})
}

// TestEndSaysWhatBecameOfGitsRegistrationWhenTheTargetIsGone: with the target gone
// the worktrees are deleted by hand, and the report says whether git's
// registration of each was dropped (a build, when the common git directory is
// still reachable: the target was itself a worktree of a repository that is still
// there) or is left (when it is not).
func TestEndSaysWhatBecameOfGitsRegistrationWhenTheTargetIsGone(t *testing.T) {
	t.Run("build end, common git directory reachable", func(t *testing.T) {
		b := newTableBench(t)
		main := filepath.Join(b.root, "mainrepo")
		if err := os.Mkdir(main, 0o755); err != nil {
			t.Fatal(err)
		}
		initRepo(t, main, nil)
		target := filepath.Join(b.root, "tg")
		gitIn(t, main, "worktree", "add", "--detach", target)
		rec, _ := buildUp(t, b, "b1", nil, "--target", target)
		if err := os.RemoveAll(target); err != nil {
			t.Fatal(err)
		}
		admins := filepath.Join(main, ".git", "worktrees")
		wantExist(t, rec.Peers[0].AdminGitDir, rec.Peers[1].AdminGitDir, filepath.Join(admins, "tg"))

		res := mustBuildEnd(t, b, "b1", "--force")
		t.Logf("stdout:\n%s", res.stdout)
		wantGone(t, rec.Peers[0].Worktree, rec.Peers[1].Worktree, rec.Peers[0].AdminGitDir, rec.Peers[1].AdminGitDir)
		// Only the build's own registrations: the target's own is another's.
		wantExist(t, filepath.Join(admins, "tg"))
		for _, p := range rec.Peers {
			want := "worktree " + p.Worktree + " (deleted by hand: the target is gone; git's registration of it, " + p.AdminGitDir + ", is dropped)"
			if !strings.Contains(res.stdout, want) {
				t.Errorf("the report lacks %q:\n%s", want, res.stdout)
			}
		}
	})
	t.Run("build end, nothing reachable", func(t *testing.T) {
		b := newTableBench(t)
		rec, _ := buildUp(t, b, "b1", nil)
		if err := os.RemoveAll(b.repo); err != nil {
			t.Fatal(err)
		}
		res := mustBuildEnd(t, b, "b1", "--force")
		for _, p := range rec.Peers {
			want := "worktree " + p.Worktree + " (deleted by hand: the target is gone; git's registration of it was in " + rec.CommonGit + ", which is not reachable, so it is left;"
			if !strings.Contains(res.stdout, want) {
				t.Errorf("the report lacks %q:\n%s", want, res.stdout)
			}
		}
		wantGone(t, rec.Peers[0].Worktree, rec.Peers[1].Worktree)
	})
	t.Run("table end", func(t *testing.T) {
		b := newTableBench(t)
		wt, _ := tableUp(t, b, "t1")
		if err := os.RemoveAll(b.repo); err != nil {
			t.Fatal(err)
		}
		res := mustTable(t, b, b.endArgs("t1", "--force")...)
		if want := "worktree " + wt + " (deleted by hand: the target " + b.repo + " is gone, so git's registration of it is left"; !strings.Contains(res.stdout, want) {
			t.Errorf("the report lacks %q:\n%s", want, res.stdout)
		}
		wantGone(t, wt)
	})
}

// TestBuildEndCollectsEveryPeersResultBeforeRefusing: one peer's worktree that git
// cannot be asked about and another's that holds changes are two reasons in one
// refusal, not the first one found.
func TestBuildEndCollectsEveryPeersResultBeforeRefusing(t *testing.T) {
	b := newTableBench(t)
	rec, _ := buildUp(t, b, "b1", nil)
	// codex-a is dirty; claude-a's admin directory loses its HEAD, so that git is
	// told a repository that is not one.
	writeFileT(t, filepath.Join(rec.peer(t, "codex-a").Worktree, "one.txt"), "1\n")
	broken := rec.peer(t, "claude-a")
	if err := os.Remove(filepath.Join(broken.AdminGitDir, "HEAD")); err != nil {
		t.Fatal(err)
	}
	res := b.run(t, b.buildEndArgs("b1")...)
	if res.code != 1 {
		t.Fatalf("exit %d, want 1\nstdout: %s\nstderr: %s", res.code, res.stdout, res.stderr)
	}
	for _, want := range []string{"peer codex-a's worktree", "one.txt", "peer claude-a's worktree " + broken.Worktree + ": git could not say what it holds", "--force"} {
		if !strings.Contains(res.stderr, want) {
			t.Errorf("the refusal lacks %q:\n%s", want, res.stderr)
		}
	}
	wantExist(t, rec.Peers[0].Worktree, rec.Peers[1].Worktree, filepath.Join(b.base, "builds", "b1", "build.json"))
}

// TestBuildEndForceStillCountsIgnoredFiles: --force lifts the refusal for changes
// and nothing else; the ignored files are still collected and printed.
func TestBuildEndForceStillCountsIgnoredFiles(t *testing.T) {
	b := newTableBench(t)
	if err := os.RemoveAll(b.repo); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(b.repo, 0o755); err != nil {
		t.Fatal(err)
	}
	initRepo(t, b.repo, map[string]string{"README.md": "target\n", ".gitignore": "*.log\nbuild/\n/.codex/*\n"})
	rec, _ := buildUp(t, b, "b1", nil)
	cw := rec.peer(t, "codex-a").Worktree
	for _, f := range []string{"a.log", "b.log", "build/x/y.o"} {
		writeFileT(t, filepath.Join(cw, f), "x\n")
	}
	// A change that would refuse the end without --force.
	writeFileT(t, filepath.Join(cw, "work.txt"), "w\n")
	writeFileT(t, filepath.Join(rec.peer(t, "claude-a").Worktree, "only.log"), "x\n")
	if res := b.run(t, b.buildEndArgs("b1")...); res.code != 1 || !strings.Contains(res.stderr, "work.txt") {
		t.Fatalf("without --force: exit %d, want 1 naming work.txt\n%s", res.code, res.stderr)
	}

	res := mustBuildEnd(t, b, "b1", "--force")
	for _, want := range []string{"Peer codex-a's worktree held 3 ignored paths, removed with it", `"a.log", "b.log", "build/"`,
		"Peer claude-a's worktree held 1 ignored path, removed with it", `"only.log"`} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("under --force, stdout lacks %q:\n%s", want, res.stdout)
		}
	}
	wantGone(t, rec.Peers[0].Worktree, rec.Peers[1].Worktree)
}

// ---- Cycle two: every marker counts, the gate outlives a partial teardown, a
// branch this run made is always its own to undo ----

// seedCursors gives both peers a cursor for the build's stream, so that there is
// state for the gate to hold.
func seedCursors(t *testing.T, b *tableBench, rec buildRecord) {
	t.Helper()
	for _, p := range rec.Peers {
		d, err := state.Open(b.base, p.Nick, rec.Stream)
		if err != nil {
			t.Fatal(err)
		}
		if err := d.SaveCursor("c-" + p.Nick); err != nil {
			t.Fatal(err)
		}
	}
}

// unregister makes peer p's worktree one build end cannot claim: the registration
// in the target's git directory is unreadable, so git does not list the worktree.
// It returns what puts the registration back.
func unregister(t *testing.T, p peerRecord) (restore func()) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("running as root: a mode 000 file is still readable")
	}
	gitdir := filepath.Join(p.AdminGitDir, "gitdir")
	if err := os.Chmod(gitdir, 0); err != nil {
		t.Fatal(err)
	}
	return func() {
		if err := os.Chmod(gitdir, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// staleMarker is a marker of this build with the peers it was made with and a
// digest that is not build.json's.
func staleMarker() string {
	return "b1\nclaude-a codex-a\nsha256:" + strings.Repeat("0", 64) + "\n"
}

// wantStateKept fails unless every peer's stream directory is still there and the
// report lists it under "Left in place".
func wantStateKept(t *testing.T, b *tableBench, rec buildRecord, stdout string) {
	t.Helper()
	for _, p := range rec.Peers {
		dir, _ := state.StreamDir(b.base, p.Nick, rec.Stream)
		wantExist(t, dir)
		if left := leftSection(stdout); !strings.Contains(left, "cursor state and thread record of peer "+p.Nick+": "+dir) {
			t.Errorf("peer %s's state %s is not listed as left in place:\n%s", p.Nick, dir, stdout)
		}
	}
	if removed := removedSection(t, stdout); strings.Contains(removed, "cursor state and thread record") {
		t.Errorf("the report says state was removed:\n%s", stdout)
	}
}

// wantStateGone fails unless every peer's stream directory is gone.
func wantStateGone(t *testing.T, b *tableBench, rec buildRecord) {
	t.Helper()
	for _, p := range rec.Peers {
		dir, _ := state.StreamDir(b.base, p.Nick, rec.Stream)
		wantGone(t, dir)
	}
}

// TestBuildEndReadsEveryPeersMarkerWhetherOrNotItClaimedTheWorktree: a peer whose
// worktree cannot be claimed is where a changed record shows, and its marker, when
// it is there and disagrees with build.json, holds both peers' stream state. On
// the previous commit the marker of a worktree that was not claimed was not read,
// so the state was removed.
func TestBuildEndReadsEveryPeersMarkerWhetherOrNotItClaimedTheWorktree(t *testing.T) {
	for _, c := range []struct {
		name string
		make func(t *testing.T, b *tableBench, p peerRecord)
	}{
		{"its registration is unreadable", func(t *testing.T, b *tableBench, p peerRecord) { unregister(t, p) }},
		{"the target does not list it", func(t *testing.T, b *tableBench, p peerRecord) {
			gitIn(t, b.repo, "worktree", "remove", "--force", p.Worktree)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := newTableBench(t)
			rec, _ := buildUp(t, b, "b1", nil)
			seedCursors(t, b, rec)
			a := rec.peer(t, "claude-a")
			c.make(t, b, a)
			writeFileT(t, filepath.Join(a.Worktree, ".parley-build"), staleMarker())

			res := mustBuildEnd(t, b, "b1")
			t.Logf("stdout:\n%s", res.stdout)
			if left := leftSection(res.stdout); !strings.Contains(left, "worktree "+a.Worktree+" of peer claude-a:") {
				t.Errorf("claude-a's worktree is not left as unclaimable:\n%s", res.stdout)
			}
			if !strings.Contains(res.stdout, "peer claude-a's worktree "+a.Worktree+" is left in place, and the record's peers differ") &&
				!strings.Contains(res.stdout, "peer claude-a's worktree "+a.Worktree+" is left in place, and the record is not the one the build was made with") {
				t.Errorf("the report does not say the unclaimed worktree's marker disagrees:\n%s", res.stdout)
			}
			wantStateKept(t, b, rec, res.stdout)
			wantGone(t, rec.peer(t, "codex-a").Worktree)
			wantExist(t, a.Worktree, filepath.Join(b.base, "builds", "b1", "build.json"))
		})
	}
	// A marker of the unclaimed worktree that is missing, or the worktree not a
	// directory, is no evidence: the state goes.
	t.Run("a marker that is missing is no evidence", func(t *testing.T) {
		b := newTableBench(t)
		rec, _ := buildUp(t, b, "b1", nil)
		seedCursors(t, b, rec)
		a := rec.peer(t, "claude-a")
		unregister(t, a)
		if err := os.Remove(filepath.Join(a.Worktree, ".parley-build")); err != nil {
			t.Fatal(err)
		}
		mustBuildEnd(t, b, "b1")
		wantStateGone(t, b, rec)
	})
	t.Run("a marker behind a link is not read", func(t *testing.T) {
		b := newTableBench(t)
		rec, _ := buildUp(t, b, "b1", nil)
		seedCursors(t, b, rec)
		a := rec.peer(t, "claude-a")
		other := filepath.Join(b.root, "elsewhere")
		writeFileT(t, filepath.Join(other, ".parley-build"), staleMarker())
		if err := os.RemoveAll(a.Worktree); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(other, a.Worktree); err != nil {
			t.Fatal(err)
		}
		mustBuildEnd(t, b, "b1")
		wantStateGone(t, b, rec)
		wantExist(t, filepath.Join(other, ".parley-build"))
	})
}

// TestTheGateOutlivesAPartialTeardown: peer A's marker disagrees, and B's worktree
// cannot be claimed. The first end removes A's worktree, marker and all, and keeps
// the state; the evidence is gone with it. The end after that, with B repaired,
// sees no marker disagree, and on the previous commit removed the state. Now the
// first end wrote <base>/builds/<id>/disputed before removing anything, and every
// later end keeps the state until --force.
func TestTheGateOutlivesAPartialTeardown(t *testing.T) {
	first := func(t *testing.T) (b *tableBench, rec buildRecord, restore func(), dispute string) {
		t.Helper()
		b = newTableBench(t)
		rec, _ = buildUp(t, b, "b1", nil)
		seedCursors(t, b, rec)
		writeFileT(t, filepath.Join(rec.peer(t, "codex-a").Worktree, ".parley-build"), staleMarker())
		restore = unregister(t, rec.peer(t, "claude-a"))

		res := mustBuildEnd(t, b, "b1")
		t.Logf("first end:\n%s", res.stdout)
		wantGone(t, rec.peer(t, "codex-a").Worktree)
		wantExist(t, rec.peer(t, "claude-a").Worktree, filepath.Join(b.base, "builds", "b1", "build.json"))
		wantStateKept(t, b, rec, res.stdout)
		dispute = filepath.Join(b.base, "builds", "b1", "disputed")
		fi, err := os.Lstat(dispute)
		if err != nil {
			t.Fatalf("the first end left no record of the dispute: %v", err)
		}
		if !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
			t.Errorf("the record of the dispute is %v, want a regular file of mode 0600", fi.Mode())
		}
		if !strings.Contains(string(mustRead(t, dispute)), "a marker disagreed with build.json") {
			t.Errorf("the record of the dispute does not say what it is:\n%s", mustRead(t, dispute))
		}
		return b, rec, restore, dispute
	}

	t.Run("B repaired, the second end still keeps the state", func(t *testing.T) {
		b, rec, restore, _ := first(t)
		restore()
		res := mustBuildEnd(t, b, "b1")
		t.Logf("second end:\n%s", res.stdout)
		wantStateKept(t, b, rec, res.stdout)
		if left := leftSection(res.stdout); !strings.Contains(left, "an earlier end of this build found a marker that did not match build.json") {
			t.Errorf("the report does not say an earlier end found the dispute:\n%s", res.stdout)
		}
		// Everything else of the build is claimed and gone, the directory with the
		// record of the dispute: there is no later end.
		wantGone(t, rec.Peers[0].Worktree, rec.Peers[1].Worktree, filepath.Join(b.base, "builds", "b1"))
	})

	t.Run("--force clears it", func(t *testing.T) {
		b, rec, restore, dispute := first(t)
		// B is not repaired yet: the end after the first still holds the state, and the
		// directory with the record stays, because B's worktree is left in it.
		res := mustBuildEnd(t, b, "b1")
		t.Logf("second end:\n%s", res.stdout)
		wantStateKept(t, b, rec, res.stdout)
		if left := leftSection(res.stdout); !strings.Contains(left, "parley build end b1 --force --state-dir") {
			t.Errorf("the report does not say how to clear it:\n%s", res.stdout)
		}
		wantExist(t, dispute)

		res = mustBuildEnd(t, b, "b1", "--force")
		t.Logf("forced end:\n%s", res.stdout)
		wantStateGone(t, b, rec)
		wantGone(t, dispute)
		if removed := removedSection(t, res.stdout); !strings.Contains(removed, "cursor state and thread record of peer codex-a") || !strings.Contains(removed, "the record of the dispute "+dispute) {
			t.Errorf("the forced end does not say it removed the state and the record:\n%s", res.stdout)
		}
		wantExist(t, rec.peer(t, "claude-a").Worktree, filepath.Join(b.base, "builds", "b1", "build.json"))

		restore()
		mustBuildEnd(t, b, "b1")
		wantGone(t, rec.Peers[1].Worktree, filepath.Join(b.base, "builds", "b1"))
	})

	t.Run("--force does not lift a marker that disagrees now", func(t *testing.T) {
		b := newTableBench(t)
		rec, _ := buildUp(t, b, "b1", nil)
		seedCursors(t, b, rec)
		bp := rec.peer(t, "claude-a")
		sealed := string(mustRead(t, filepath.Join(bp.Worktree, ".parley-build")))
		restore := unregister(t, bp)
		writeFileT(t, filepath.Join(bp.Worktree, ".parley-build"), staleMarker())
		mustBuildEnd(t, b, "b1")
		dispute := filepath.Join(b.base, "builds", "b1", "disputed")
		wantExist(t, dispute)

		res := mustBuildEnd(t, b, "b1", "--force")
		wantStateKept(t, b, rec, res.stdout)
		wantExist(t, dispute)

		restore()
		writeFileT(t, filepath.Join(bp.Worktree, ".parley-build"), sealed)
		res = mustBuildEnd(t, b, "b1", "--force")
		wantStateGone(t, b, rec)
		wantGone(t, filepath.Join(b.base, "builds", "b1"))
		if strings.Contains(res.stdout, "Left in place") {
			t.Errorf("the end that cleared the dispute still leaves something:\n%s", res.stdout)
		}
	})

	t.Run("a dispute that cannot be written stops the end before it removes anything", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("running as root: a read-only directory is still writable")
		}
		b := newTableBench(t)
		rec, _ := buildUp(t, b, "b1", nil)
		seedCursors(t, b, rec)
		writeFileT(t, filepath.Join(rec.peer(t, "codex-a").Worktree, ".parley-build"), staleMarker())
		buildDir := filepath.Join(b.base, "builds", "b1")
		if err := os.Chmod(buildDir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(buildDir, 0o700) })

		res := b.run(t, b.buildEndArgs("b1")...)
		if res.code != 1 || !strings.Contains(res.stderr, "nothing was removed") || !strings.Contains(res.stderr, "disputed") {
			t.Fatalf("exit %d, want 1 saying nothing was removed and naming the file\nstderr: %s", res.code, res.stderr)
		}
		wantExist(t, rec.Peers[0].Worktree, rec.Peers[1].Worktree, rec.ReplyDir)
		for _, p := range rec.Peers {
			dir, _ := state.StreamDir(b.base, p.Nick, rec.Stream)
			wantExist(t, dir)
		}
		wantGone(t, filepath.Join(buildDir, "disputed"))
	})
}

// ---- Cycle two, item 3: the branch ----

// createThenFailGit puts a git of its own first on the PATH. It runs the real git
// for everything, but a call that creates the branch runs for real and then exits
// non-zero, as a git killed or failing after it made the ref would. It returns the
// environment to run parley in.
func createThenFailGit(t *testing.T, b *tableBench, branch string) []string {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"real=" + q(real) + "\nbranch=" + q(branch) + "\n" +
		`case " $* " in
*" -d "*) exec "$real" "$@" ;;
esac
case " $* " in
*" update-ref "*"refs/heads/$branch "*)
	"$real" "$@"
	echo "fatal: simulated failure after the ref was made" >&2
	exit 3 ;;
esac
exec "$real" "$@"
`
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return append(b.env(), "PATH="+dir+string(os.PathListSeparator)+b.path+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestBuildNewRollsBackABranchWhoseUpdateRefFailedAfterMakingIt: the undo of the
// branch is registered before the call that makes it, and recognises this run's
// branch by the nonce in the first entry of its reflog, so a call that made the
// ref and still failed leaves nothing behind. On the previous commit the undo was
// registered only after the call succeeded, and the branch was left.
func TestBuildNewRollsBackABranchWhoseUpdateRefFailedAfterMakingIt(t *testing.T) {
	for _, branch := range []string{"parley/b1/codex-a", "parley/b1/claude-a"} {
		t.Run(branch, func(t *testing.T) {
			b := newTableBench(t)
			before := b.buildSnapshot(t)
			env := createThenFailGit(t, b, branch)
			res := runParleyEnvIn(t, "", env, b.buildArgs("b1", nil)...)
			t.Logf("stderr:\n%s", res.stderr)
			if res.code == 0 || !strings.Contains(res.stderr, "simulated failure") {
				t.Fatalf("exit %d, want a failure reporting the git failure\nstdout: %s\nstderr: %s", res.code, res.stdout, res.stderr)
			}
			if !strings.Contains(res.stderr, "rolled back") || !strings.Contains(res.stderr, "branch "+branch) {
				t.Errorf("stderr does not say the branch was rolled back:\n%s", res.stderr)
			}
			if strings.Contains(res.stderr, "could NOT") || strings.Contains(res.stderr, "left branch") {
				t.Errorf("the rollback reports a failure or leaves the branch:\n%s", res.stderr)
			}
			if refs := gitIn(t, b.repo, "for-each-ref", "refs/heads/parley"); refs != "" {
				t.Errorf("a branch is left:\n%s", refs)
			}
			if got := withoutCalls(b.buildSnapshot(t)); got != withoutCalls(before) {
				t.Errorf("the run left a trace.\nbefore:\n%s\nafter:\n%s", before, got)
			}
		})
	}
}

// TestBuildNewMakesItsBranchesWithAReflogMessageCarryingTheRunsNonce: the oldest
// entry of each branch's reflog is "parley build new <id> <nonce>", the same nonce
// for both peers' branches, and a different one on another run.
func TestBuildNewMakesItsBranchesWithAReflogMessageCarryingTheRunsNonce(t *testing.T) {
	b := newTableBench(t)
	buildUp(t, b, "b1", nil)
	first := gitIn(t, b.repo, "reflog", "show", "--format=%gs", "refs/heads/parley/b1/codex-a")
	second := gitIn(t, b.repo, "reflog", "show", "--format=%gs", "refs/heads/parley/b1/claude-a")
	re := regexp.MustCompile(`^parley build new b1 [0-9a-f]{32}$`)
	if !re.MatchString(first) || first != second {
		t.Errorf("the reflog messages are %q and %q, want one \"parley build new b1 <32 hex>\" for both", first, second)
	}
	mustBuildEnd(t, b, "b1")
	gitIn(t, b.repo, "branch", "-D", "parley/b1/codex-a", "parley/b1/claude-a")
	buildUp(t, b, "b1", nil)
	if again := gitIn(t, b.repo, "reflog", "show", "--format=%gs", "refs/heads/parley/b1/codex-a"); again == first || !re.MatchString(again) {
		t.Errorf("the second run's reflog message is %q, want another nonce than %q", again, first)
	}
}
