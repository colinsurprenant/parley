package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/colinsurprenant/parley/internal/state"
	"github.com/colinsurprenant/parley/internal/stream"
)

// A build is two peers, each on a worktree and a branch of the target, talking
// over one channel under the parley-build skill. What it keeps under
// <state base>/builds/<id>/ is build.json, the record `build new` leaves for
// `build end`, the disputed file `build end` leaves when a marker disagreed with
// it, and each peer's worktree, <nick> beside them. Its other state is
// elsewhere in the base: each peer's thread record and cursor under that peer's
// own directory, <base>/<nick>/<streamkey>/, the peer's lock <base>/<nick>/lock,
// and the id's lock <base>/.ids/<id>.lock, which `build end` could not take with
// the build directory and which no nick can be called (a nick holds no '.').
// "builds" sits beside the per-nick directories of the same base, as "tables"
// does, which is why no participant may be called either.
//
// What parley trusts, for the commands in build_new.go and build_end.go (written
// out in docs/build.md, "What parley trusts"):
//
//   - Untrusted: what a peer, a model or another program produced. Channel
//     messages, Codex's JSONL and the thread id it announces, IRC replies, git's
//     output, and every file in a peer's worktree, the .git file, the marker and
//     the launch files (.mcp.json, .codex/config.toml) included. parley bounds
//     and quotes these, and never lets them choose what git runs against
//     (gitwt.go).
//   - Trusted: parley's own state (build.json, and the disputed file a build end
//     writes when a marker disagrees, under <state base>/builds/<id>/, which is
//     made 0700; the peers' thread records, cursors and lock files under their
//     own directories of the base; the id's lock), and the target repo's own git
//     dir and config. The markers are evidence about build.json, not state: the
//     peers' can be edited by a peer. build end reads every peer's, claimed
//     worktree or not. One that is there and disagrees with build.json is
//     evidence the record changed, and build end then lists the peers' state and
//     does not remove it, now or at a later end while the disputed file is
//     there (--force clears it); one that is missing is no evidence.
//   - Out of scope: a deliberate hand edit of parley's state; a peer editing or
//     committing the launch files (the skill forbids it, and build end reports
//     the commits); what a peer does with the permissions its harness grants it.
//   - Destructive scope: build end removes only the recorded worktrees under
//     <state base>/builds/<id>/, the recorded reply directory, the build
//     directory and the peers' state for the build's stream. Never the target's
//     main checkout, never a branch.
const (
	buildsDirName       = state.BuildsDir
	buildRecordName     = "build.json"
	buildWorktreeMarker = ".parley-build"
	// buildReplyMarker is the marker in the reply directory, the same name.
	buildReplyMarker = ".parley-build"
	buildPeerCount   = 2
)

// buildRecordMax bounds what readBuildRecord reads: two peers' ids, paths and
// nicks, a few KiB at the most.
const buildRecordMax = 64 << 10

var buildWords = markerWords{kind: "build", members: "peers", record: buildRecordName, maker: "build new"}

func buildDirOf(base, id string) string { return filepath.Join(base, buildsDirName, id) }

// buildReplyPrefix is the name of a build's reply directory up to the random
// string os.MkdirTemp adds.
func buildReplyPrefix(id string) string { return "parley-build-" + id + "-" }

// buildBranch is the branch a peer works on: parley/<id>/<nick>.
func buildBranch(id, nick string) string { return "parley/" + id + "/" + nick }

func checkBuildID(id string) error {
	if !tableIDRE.MatchString(id) {
		return cfgErrf("build id %q must match [a-z0-9][a-z0-9-]{0,31}: it is the channel #<id> and a directory name", id)
	}
	return nil
}

// cmdBuild runs the build verbs: new sets a build up, end takes it down.
func cmdBuild(args []string) error {
	if len(args) == 0 {
		buildUsage(os.Stderr)
		return cfgErrf("parley build needs a verb: new or end")
	}
	switch args[0] {
	case "-h", "-help", "--help", "help":
		buildUsage(os.Stdout)
		return nil
	case "new":
		return cmdBuildNew(args[1:])
	case "end":
		return cmdBuildEnd(args[1:])
	}
	buildUsage(os.Stderr)
	return cfgErrf("unknown build verb %q: the verbs are new and end", args[0])
}

func buildUsage(w io.Writer) {
	fmt.Fprint(w, buildUsageNew+"\n"+buildUsageEnd+"\n")
}

const (
	buildUsageNew = "usage: parley build new <id> --target <repo> --chair <nick> --peer <nick>=<claude|codex>[:<model>] --peer <nick>=<claude|codex>[:<model>]\n" +
		"                       [--server 127.0.0.1:6667] [--state-dir <dir>] [--codex-bin codex] [--writable-root <abs dir>]..."
	buildUsageEnd = "usage: parley build end <id> [--force] [--state-dir <dir>]"
)

// buildRecord is what `build new` keeps of a build so that `build end` can take
// it down: where everything is, and the thread each Codex peer was started as. It
// holds no stream content, only names and paths.
type buildRecord struct {
	ID         string       `json:"id"`
	Stream     string       `json:"stream"`
	Target     string       `json:"target"`
	CommonGit  string       `json:"common_git_dir"`
	BaseCommit string       `json:"base_commit"`
	Chair      string       `json:"chair"`
	ReplyDir   string       `json:"reply_dir"`
	Created    string       `json:"created"`
	Peers      []peerRecord `json:"peers"`

	// digest is tableDigest of the bytes the record was read from, set by
	// readBuildRecord and never part of the file: it is what the markers build new
	// sealed are compared with.
	digest string
}

// peerRecord is one peer. Thread and ReplyFile are a Codex peer's; a Claude
// peer's reply directory is the channel's own, made when its session starts.
type peerRecord struct {
	Nick        string `json:"nick"`
	Harness     string `json:"harness"`
	Model       string `json:"model,omitempty"`
	Worktree    string `json:"worktree"`
	AdminGitDir string `json:"admin_git_dir"`
	Branch      string `json:"branch"`
	Thread      string `json:"thread,omitempty"`
	ReplyFile   string `json:"reply_file,omitempty"`
}

// marker is the marker build new seals into the worktrees and the reply directory
// for this record: its id and its peers' nicks, bound to the bytes it was read
// from.
func (r buildRecord) marker() tableMarker {
	nicks := make([]string, len(r.Peers))
	for i, p := range r.Peers {
		nicks[i] = p.Nick
	}
	return newTableMarker(r.ID, nicks).sealed(r.digest)
}

var commitRE = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// noBuildErr is the refusal for a build that is not there.
func noBuildErr(base, id string) error {
	return cfgErrf("no build %q under %s (looked for %s)", id, base, filepath.Join(buildDirOf(base, id), buildRecordName))
}

// buildDirIsThere is whether anything is at the build's directory: `build end`
// asks it before it takes the id's lock, so that an id that was never a build
// (a slip of the finger, a wrong --state-dir) leaves no lock file in the base.
// The record is read after the lock is taken, and that read is the one that
// counts.
func buildDirIsThere(base, id string) bool {
	_, err := os.Lstat(buildDirOf(base, id))
	return !errors.Is(err, os.ErrNotExist)
}

// readBuildRecord loads and checks the record of build id under base, and keeps
// the digest of the bytes it read once. A build that is not there is the
// invocation's mistake; a record that is there and is not one this binary wrote
// is not trusted, because `build end` removes the paths it names.
func readBuildRecord(base, id string) (buildRecord, error) {
	path := filepath.Join(buildDirOf(base, id), buildRecordName)
	b, err := readRecordFile(path, "build", buildRecordMax)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return buildRecord{}, noBuildErr(base, id)
		}
		return buildRecord{}, err
	}
	var rec buildRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return buildRecord{}, fmt.Errorf("build record %s is not valid: %w", path, err)
	}
	rec.digest = tableDigest(b)
	if err := rec.check(base, id); err != nil {
		return buildRecord{}, fmt.Errorf("build record %s is not valid: %w", path, err)
	}
	return rec, nil
}

// check holds the record to what build new writes: it guards against damage (a
// bad disk, a stray write), not against a record crafted by hand.
func (r buildRecord) check(base, id string) error {
	if r.ID != id {
		return fmt.Errorf("it is build %q, not %q", r.ID, id)
	}
	for _, p := range []struct{ name, path string }{{"target", r.Target}, {"common_git_dir", r.CommonGit}, {"reply_dir", r.ReplyDir}} {
		if !filepath.IsAbs(p.path) {
			return fmt.Errorf("%s %q is not an absolute path", p.name, p.path)
		}
	}
	if !strings.HasPrefix(filepath.Base(r.ReplyDir), buildReplyPrefix(id)) {
		return fmt.Errorf("reply_dir %q is not a reply directory of build %q", r.ReplyDir, id)
	}
	if r.Stream == "" || hasControl(r.Stream) {
		return errors.New("no stream, or one with a control character")
	}
	if !commitRE.MatchString(r.BaseCommit) {
		return fmt.Errorf("base_commit %q is not a commit id", r.BaseCommit)
	}
	if len(r.Peers) != buildPeerCount {
		return fmt.Errorf("it lists %d peers, and a build new made always has %d", len(r.Peers), buildPeerCount)
	}
	buildDir := buildDirOf(base, id)
	for i, p := range r.Peers {
		if err := stream.ValidNick(p.Nick); err != nil {
			return fmt.Errorf("peer nick %q %v", p.Nick, err)
		}
		for _, prev := range r.Peers[:i] {
			if stream.NickEqual(p.Nick, prev.Nick) {
				return fmt.Errorf("peer nick %q is also peer %q's", p.Nick, prev.Nick)
			}
		}
		if p.Harness != harnessClaude && p.Harness != harnessCodex {
			return fmt.Errorf("peer %s: harness %q is neither claude nor codex", p.Nick, p.Harness)
		}
		if want := filepath.Join(buildDir, p.Nick); p.Worktree != want {
			return fmt.Errorf("peer %s: worktree %q is not %q", p.Nick, p.Worktree, want)
		}
		if want := buildBranch(id, p.Nick); p.Branch != want {
			return fmt.Errorf("peer %s: branch %q is not %q", p.Nick, p.Branch, want)
		}
		if !filepath.IsAbs(p.AdminGitDir) || filepath.Dir(p.AdminGitDir) != filepath.Join(r.CommonGit, "worktrees") {
			return fmt.Errorf("peer %s: admin_git_dir %q is not a directory under %s", p.Nick, p.AdminGitDir, filepath.Join(r.CommonGit, "worktrees"))
		}
		if hasControl(p.Model) {
			return fmt.Errorf("peer %s: the model holds a control character", p.Nick)
		}
		if p.Thread != "" && !state.ValidThreadID(p.Thread) {
			return fmt.Errorf("peer %s: thread %q is not a UUID", p.Nick, p.Thread)
		}
		if p.ReplyFile != "" && p.ReplyFile != filepath.Join(r.ReplyDir, p.Nick+".reply") {
			return fmt.Errorf("peer %s: reply_file %q is not in the reply directory", p.Nick, p.ReplyFile)
		}
	}
	return nil
}
