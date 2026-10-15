package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/colinsurprenant/parley/internal/primer"
)

// measureBuildPeer is the length `parley channel --check` reports for a Claude
// peer of a build launched with these facts: the flags its .mcp.json would carry,
// the worktree where build new would make it (made here for the measure and
// removed after), at the longest reply directory name a launch can draw.
func measureBuildPeer(t *testing.T, b *tableBench, id, nick, chair, stateDir string) int {
	t.Helper()
	wt := filepath.Join(stateDir, "builds", id, nick)
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(stateDir)
	res := runCheck(t, b.tmp, "channel", "--stream", b.buildStream(id), "--nick", nick, "--state-dir", stateDir,
		"--skill", "build", "--chair", chair, "--target", b.repo, "--worktree", wt)
	return checkedLength(t, res)
}

var overrunRE = regexp.MustCompile(`the instructions would be (\d+) UTF-16 code units, over Claude Code's cap of 2048`)

// TestBuildNewHoldsAClaudePeerToTheInstructionCap: a Claude peer's channel
// instructions carry the facts build new gives it, and a worktree path that grows
// with the state directory, the id and the nick. A legal input can exceed the cap
// (a 32-character id and the longest legal nick already do, in an ordinary state
// directory), so build new measures each Claude peer in process before it changes
// anything, as `channel --check` would, and refuses one over it, naming the
// overrun. At the boundary: 2048 units is accepted, 2049 is refused.
func TestBuildNewHoldsAClaudePeerToTheInstructionCap(t *testing.T) {
	id := strings.Repeat("i", 32)

	t.Run("at the boundary", func(t *testing.T) {
		b := newTableBench(t)
		// The longest nick that leaves room, then the chair's length (one unit a
		// character) to land on the cap exactly.
		var nick string
		for l := 64; l >= 1; l-- {
			nick = strings.Repeat("n", l)
			if measureBuildPeer(t, b, id, nick, "c", b.base) <= primer.ClaudeInstructionsCap {
				break
			}
		}
		n1 := measureBuildPeer(t, b, id, nick, "c", b.base)
		chairLen := 1 + primer.ClaudeInstructionsCap - n1
		if chairLen < 1 || chairLen > 64 {
			t.Fatalf("a nick of %d characters measures %d, which a chair of %d characters cannot bring to the cap", len(nick), n1, chairLen)
		}
		chair := strings.Repeat("c", chairLen)
		if got := measureBuildPeer(t, b, id, nick, chair, b.base); got != primer.ClaudeInstructionsCap {
			t.Fatalf("the chair of %d characters measures %d, want exactly %d", chairLen, got, primer.ClaudeInstructionsCap)
		}
		over := strings.Repeat("c", chairLen+1)
		if got := measureBuildPeer(t, b, id, nick, over, b.base); got != primer.ClaudeInstructionsCap+1 {
			t.Fatalf("the chair of %d characters measures %d, want %d", chairLen+1, got, primer.ClaudeInstructionsCap+1)
		}
		t.Logf("a Claude peer of %d characters, a 32-character id, a chair of %d or %d characters: 2048 and 2049 units", len(nick), chairLen, chairLen+1)

		peers := []string{nick + "=claude", "codex-a=codex"}
		before := b.buildSnapshot(t)
		res := b.run(t, b.buildArgs(id, peers, "--chair", over)...)
		if res.code != 2 {
			t.Fatalf("2049 units: exit %d, want 2\nstdout: %s\nstderr: %s", res.code, res.stdout, res.stderr)
		}
		m := overrunRE.FindStringSubmatch(res.stderr)
		if m == nil || m[1] != "2049" || !strings.Contains(res.stderr, "claude peer "+nick) || !strings.Contains(res.stderr, "<state-dir>/builds/<id>/<nick>") {
			t.Errorf("stderr does not name the overrun (2049 of 2048), the peer and what to shorten:\n%s", res.stderr)
		}
		if after := b.buildSnapshot(t); after != before {
			t.Fatalf("the refusal left a trace.\nbefore:\n%s\nafter:\n%s", before, after)
		}
		if b.callCount() != 0 {
			t.Errorf("codex was called %d times before the refusal", b.callCount())
		}

		res = b.run(t, b.buildArgs(id, peers, "--chair", chair)...)
		if res.code != 0 {
			t.Fatalf("2048 units: exit %d, want 0\nstdout: %s\nstderr: %s", res.code, res.stdout, res.stderr)
		}
		// And the instructions the launched channel would send are the ones that
		// were measured: --check on the .mcp.json's own args says 2048.
		rec := readRecordForTest(t, b, id)
		var args []string
		var mcp struct {
			MCPServers map[string]struct {
				Args []string `json:"args"`
			} `json:"mcpServers"`
		}
		if err := json.Unmarshal(mustRead(t, filepath.Join(rec.peer(t, nick).Worktree, ".mcp.json")), &mcp); err != nil {
			t.Fatal(err)
		}
		for _, a := range mcp.MCPServers["parley"].Args {
			if a == "${PARLEY_NICK}" {
				a = nick
			}
			args = append(args, a)
		}
		check := runCheck(t, b.tmp, args...)
		if check.code != 0 || checkedLength(t, check) != primer.ClaudeInstructionsCap {
			t.Errorf("channel --check on the written .mcp.json: exit %d, %q, want 2048 units", check.code, check.stdout)
		}
	})

	t.Run("the longest legal nick", func(t *testing.T) {
		b := newTableBench(t)
		nick := strings.Repeat("n", 64)
		want := measureBuildPeer(t, b, id, nick, "colin", b.base)
		if want <= primer.ClaudeInstructionsCap {
			t.Skipf("a 32-character id and a 64-character nick measure %d here: within the cap, nothing to refuse", want)
		}
		before := b.buildSnapshot(t)
		res := b.run(t, b.buildArgs(id, []string{nick + "=claude", "codex-a=codex"})...)
		m := overrunRE.FindStringSubmatch(res.stderr)
		if res.code != 2 || m == nil || m[1] != strconv.Itoa(want) {
			t.Fatalf("exit %d, want 2 naming the %d units channel --check reports\n%s", res.code, want, res.stderr)
		}
		if after := b.buildSnapshot(t); after != before {
			t.Fatalf("the refusal left a trace.\nbefore:\n%s\nafter:\n%s", before, after)
		}
	})

	t.Run("a long home, the default state directory", func(t *testing.T) {
		b := newTableBench(t)
		home := filepath.Join(b.root, strings.Repeat("h", 200))
		state := filepath.Join(home, ".local", "state", "parley")
		nick := "claude-a"
		want := measureBuildPeer(t, b, "b1", nick, "colin", state)
		if err := os.RemoveAll(home); err != nil {
			t.Fatal(err)
		}
		t.Logf("a home of %d characters: %d units", len(home), want)
		before := b.buildSnapshot(t)
		env := append(b.env(), "XDG_STATE_HOME="+filepath.Join(home, ".local", "state"))
		args := []string{"build", "new", "b1", "--target", b.repo, "--chair", "colin", "--server", b.db.server, "--peer", nick + "=claude", "--peer", "codex-a=codex"}
		res := runParleyEnvIn(t, "", env, args...)
		if want <= primer.ClaudeInstructionsCap {
			// Within the cap: it goes through, in a state directory under that home.
			if res.code != 0 {
				t.Fatalf("exit %d, want 0 for %d units\n%s", res.code, want, res.stderr)
			}
			if _, err := os.Stat(filepath.Join(state, "builds", "b1", "build.json")); err != nil {
				t.Errorf("the build is not under the default state directory: %v", err)
			}
			return
		}
		m := overrunRE.FindStringSubmatch(res.stderr)
		if res.code != 2 || m == nil || m[1] != strconv.Itoa(want) {
			t.Fatalf("exit %d, want 2 naming the %d units channel --check reports\n%s", res.code, want, res.stderr)
		}
		if after := b.buildSnapshot(t); after != before {
			t.Fatalf("the refusal left a trace.\nbefore:\n%s\nafter:\n%s", before, after)
		}
		if _, err := os.Lstat(home); err == nil {
			t.Errorf("the refused run made %s", home)
		}
	})
}
