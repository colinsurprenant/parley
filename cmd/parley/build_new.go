package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/colinsurprenant/parley/internal/channel"
	"github.com/colinsurprenant/parley/internal/codex"
	"github.com/colinsurprenant/parley/internal/doctor"
	"github.com/colinsurprenant/parley/internal/primer"
	"github.com/colinsurprenant/parley/internal/state"
	"github.com/colinsurprenant/parley/internal/stream"
)

// buildPlan is everything `build new` has validated before it changes anything.
type buildPlan struct {
	id       string
	stream   string // canonical
	target   string
	chair    string
	peers    []*peerPlan
	base     string
	buildDir string
	server   string
	self     string
	// commonDir is the target's common git dir and baseCommit the target's HEAD,
	// both read once: every peer's branch starts at the same commit, whatever the
	// target does meanwhile.
	commonDir  string
	baseCommit string
	// writableRoots are the physical paths --writable-root named, for the Codex
	// peers' config; none by default.
	writableRoots []string
	codexBin      string
	codexGiven    bool
	serverDown    bool
	// global is, when --writable-root named any, what carrying the user's global
	// Codex roots alongside them came to; nil otherwise.
	global *globalRoots
	// unlockID drops the id's lock claimID took; nil until it has. The caller
	// calls releaseID when the run is over.
	unlockID func()
}

// releaseID drops the id lock claimID took, if it did.
func (p *buildPlan) releaseID() {
	if p != nil && p.unlockID != nil {
		p.unlockID()
		p.unlockID = nil
	}
}

// peerPlan is one peer: a seatPlan, whose thread, replyFile and answer a Codex
// peer's start fills in, and the worktree and branch it works on.
type peerPlan struct {
	seatPlan
	worktree string
	branch   string
	// adminDir is the worktree's admin directory in the target's git dir, found
	// once git has made the worktree.
	adminDir string
}

func (p *buildPlan) peersOf(harness string) []*peerPlan {
	var out []*peerPlan
	for _, s := range p.peers {
		if s.harness == harness {
			out = append(out, s)
		}
	}
	return out
}

func (p *buildPlan) codexName() string {
	if p.codexGiven {
		return q(p.codexBin)
	}
	return "codex"
}

// marker is the content of both markers build new writes first: the build's id
// and the peers' nicks, to which sealing adds the digest of build.json.
func (p *buildPlan) marker() tableMarker {
	nicks := make([]string, len(p.peers))
	for i, s := range p.peers {
		nicks[i] = s.nick
	}
	return newTableMarker(p.id, nicks)
}

// buildInput is what planBuild validates: the flags as given.
type buildInput struct {
	id            string
	target        string
	targetGiven   bool
	chair         string
	chairGiven    bool
	peers         []string
	server        string
	stateDir      string
	codexBin      string
	codexGiven    bool
	writableRoots []string
}

// cmdBuildNew sets a build up: a worktree and a branch for each of two peers,
// their configs, the reply directory and each Codex peer's session started with
// its primer, its thread recorded. It prints what the human runs next.
// Everything it made is taken down again if any step fails, but the peers' lock
// files, which stay as a pump's do.
func cmdBuildNew(args []string) error {
	fs := flag.NewFlagSet("parley build new", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), buildUsageNew)
		fs.PrintDefaults()
	}
	target := fs.String("target", "", "required: the repo the build works on, the root of a git checkout, adopted in Director before this runs; each peer gets a worktree of its HEAD on a new branch, and nothing else is written into it but git's registration of the worktrees")
	chair := fs.String("chair", "", "required: your registered nick on the server, the human whose channel lines rule the build")
	var peerArgs stringList
	fs.Var(&peerArgs, "peer", "required, exactly twice: a peer as <nick>=<claude|codex>[:<model>], e.g. --peer claude-a=claude:opus --peer codex-b=codex:gpt-5; a model is at most 128 bytes of letters, digits and . _ - : / @ [ ] +; opencode peers are not supported yet")
	server := fs.String("server", defaultListen, "the loopback IP:port of the IRC server the build uses (a host name is refused)")
	stateDir := fs.String("state-dir", "", "state directory holding <nick>/ and builds/ (default $XDG_STATE_HOME/parley)")
	codexBin := fs.String("codex-bin", codex.DefaultBin, "the codex CLI to run, resolved on PATH unless it holds a separator")
	var roots stringList
	fs.Var(&roots, "writable-root", "repeatable, opt-in: an absolute directory that exists, added to the Codex peers' sandbox as a writable root (resolved to its physical path), and the global roots of $CODEX_HOME/config.toml (default ~/.codex/config.toml) are carried over with it. None by default; see docs/build.md before naming the target's git directory")
	id, rest, hasID := splitTableID(args)
	if err := parseFlags(fs, rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return &configError{err: err}
	}
	switch {
	case !hasID && fs.NArg() == 1:
		id = fs.Arg(0)
	case !hasID:
		return cfgErrf("the build id is required, first: parley build new <id> --target <repo> --chair <nick> --peer <nick>=<harness> --peer <nick>=<harness>")
	case fs.NArg() > 0:
		return cfgErrf("unexpected argument %q", fs.Arg(0))
	}

	ctx, stop := signalContext()
	defer stop()
	plan, err := planBuild(ctx, buildInput{
		id: id, target: *target, targetGiven: given(fs, "target"),
		chair: *chair, chairGiven: given(fs, "chair"),
		peers: peerArgs, server: *server, stateDir: *stateDir,
		codexBin: *codexBin, codexGiven: given(fs, "codex-bin"),
		writableRoots: roots,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "parley build new: checking the machine (parley doctor's static checks)\n")
	if err := plan.checkMachine(ctx, os.Stderr); err != nil {
		return err
	}
	if err := plan.measureClaudePeers(); err != nil {
		return err
	}
	plan.warnWritableRoots(os.Stderr)
	for _, line := range plan.globalRootsLog() {
		fmt.Fprintf(os.Stderr, "parley build new: %s\n", line)
	}
	// From here the run looks at the base: under the id's lock, held for the whole
	// run, rollback and the runbook included.
	if err := plan.claimID(ctx); err != nil {
		return err
	}
	defer plan.releaseID()

	run := &buildRun{plan: plan, log: os.Stderr}
	err = run.setUp(ctx)
	if err == nil && ctx.Err() != nil {
		// An interrupt that lands after the last step that watches ctx is still
		// an interrupt: roll back rather than report a setup the user stopped.
		err = ctx.Err()
	}
	if err != nil {
		if ctx.Err() != nil {
			err = fmt.Errorf("interrupted: %w", err)
		}
		run.rollback()
	}
	// The peers' locks are held through the rollback, and no longer. Their lock
	// files stay, as a pump's do: see lockSeats.
	run.release()
	if err != nil {
		return err
	}
	signal.Ignore(syscall.SIGPIPE)
	if _, err := io.WriteString(os.Stdout, plan.runbook(run)); err != nil {
		return fmt.Errorf("build %s is set up but its instructions could not be written (%v): parley build end %s takes it down", plan.id, err, plan.id)
	}
	return nil
}

// planBuild validates everything that can be validated before a change. Each
// refusal is a configuration error, exit 2, and nothing has been created, run
// or connected to but read-only git questions. What depends on the state of the
// base for this id (whether it is live, the peers' cursors, the branches) is
// looked at later, under the id's lock: see claimID.
func planBuild(ctx context.Context, in buildInput) (*buildPlan, error) {
	if err := checkBuildID(in.id); err != nil {
		return nil, err
	}
	p := &buildPlan{id: in.id, codexGiven: in.codexGiven}

	if !in.targetGiven || in.target == "" {
		return nil, cfgErrf("--target is required: the repo the build works on")
	}
	target, err := filepath.Abs(in.target)
	if err != nil {
		return nil, cfgErrf("--target %q: %v", in.target, err)
	}
	p.target = target

	if !in.chairGiven {
		return nil, cfgErrf("--chair is required: your registered nick on the server")
	}
	if err := stream.ValidNick(in.chair); err != nil {
		return nil, cfgErrf("--chair %q %v", in.chair, err)
	}
	p.chair = in.chair

	switch {
	case len(in.peers) == 0:
		return nil, cfgErrf("two --peer are required: --peer <nick>=<claude|codex>[:<model>], twice")
	case len(in.peers) > buildPeerCount:
		return nil, cfgErrf("--peer was given %d times: a build is two peers, because the parley-build protocol is two-peer, and a third is refused (a question for several agents is a table)", len(in.peers))
	case len(in.peers) < buildPeerCount:
		return nil, cfgErrf("one --peer is not a build: the protocol is two-peer, so name two")
	}
	for _, raw := range in.peers {
		m, err := parseMember("--peer", raw)
		if err != nil {
			return nil, err
		}
		if stream.NickEqual(m.nick, in.chair) {
			return nil, cfgErrf("--peer %q: the nick is the chair's; the chair is another voice than any peer, and nicks are the same without regard to ASCII case", raw)
		}
		for _, prev := range p.peers {
			if stream.NickEqual(m.nick, prev.nick) {
				return nil, cfgErrf("--peer %q: the nick is also peer %q's, and nicks are the same without regard to ASCII case", raw, prev.nick)
			}
		}
		p.peers = append(p.peers, &peerPlan{seatPlan: *m, branch: buildBranch(in.id, m.nick)})
	}

	env, err := resolvePlanEnv(in.id, in.server, in.stateDir)
	if err != nil {
		return nil, err
	}
	p.server, p.stream, p.self, p.base = env.server, env.stream, env.self, env.base

	p.buildDir = buildDirOf(p.base, in.id)
	for _, s := range p.peers {
		s.worktree = filepath.Join(p.buildDir, s.nick)
	}
	// The reply directory goes under $TMPDIR, outside the target and the
	// worktrees whose status the chair reads and build end judges.
	tmp, err := filepath.Abs(os.TempDir())
	if err != nil {
		return nil, cfgErrf("resolve the temp directory %q: %v", os.TempDir(), err)
	}
	// Every path that build.json, an .mcp.json or the Codex config will name must
	// survive encoding/json as it is, so each is checked before anything is made. The
	// reply directory is $TMPDIR plus a name of digits; the build directory and the
	// worktrees are the state directory plus the id and the nicks, which are ASCII.
	utf8Paths := []pathField{{"--target", p.target}, {"the state directory (--state-dir, or $XDG_STATE_HOME, or $HOME)", p.base}, {"this binary's path", p.self},
		{"the temp directory ($TMPDIR), which the reply directory is made under,", tmp}}
	for _, s := range p.peers {
		utf8Paths = append(utf8Paths, pathField{"peer " + s.nick + "'s worktree", s.worktree})
	}
	if err := checkUTF8(utf8Paths...); err != nil {
		return nil, err
	}
	// The worktrees live under the state base, so a base inside the target would
	// put them inside the target checkout, which nothing is written into but the
	// registrations of the worktrees; and a target inside the build's directory
	// would be taken down with it.
	if within(p.target, p.base) {
		return nil, cfgErrf("the state directory %s is inside the target %s: the peers' worktrees live under it, and nothing is written into the target checkout; name a --state-dir outside it", p.base, p.target)
	}
	if within(p.buildDir, p.target) {
		return nil, cfgErrf("the target %s is inside the build's directory %s, which build end removes; name a --state-dir elsewhere", p.target, p.buildDir)
	}
	hosts := []struct{ what, path string }{{"the target checkout", p.target}}
	for _, s := range p.peers {
		hosts = append(hosts, struct{ what, path string }{"peer " + s.nick + "'s worktree", s.worktree})
	}
	for _, host := range hosts {
		if within(host.path, tmp) {
			return nil, cfgErrf("the temp directory ($TMPDIR), which holds the reply directory, %s is inside %s %s: run with a TMPDIR outside it", tmp, host.what, host.path)
		}
	}

	// The paths land in a .mcp.json whose command and args Claude Code expands
	// ${...} in, and in text the peers read: a '$' would be expanded, and a
	// control character would break the line it is printed on.
	paths := []struct{ name, path string }{{"--target", p.target}, {"this binary's path", p.self}}
	for _, s := range p.peers {
		paths = append(paths, struct{ name, path string }{"peer " + s.nick + "'s worktree", s.worktree})
	}
	if len(p.peersOf(harnessClaude)) > 0 {
		paths = append(paths, struct{ name, path string }{"the state directory", p.base})
	}
	for _, x := range paths {
		if strings.Contains(x.path, "$") {
			return nil, cfgErrf("%s %q holds a '$': Claude Code expands ${...} in a .mcp.json's command and args, so the path would not reach the channel as written", x.name, x.path)
		}
	}
	paths = append(paths, struct{ name, path string }{"the state directory", p.base})
	for _, x := range paths {
		if hasControl(x.path) {
			return nil, cfgErrf("%s %q holds a control character: it is printed into commands and into the text the peers read", x.name, x.path)
		}
	}

	if len(p.peersOf(harnessCodex)) > 0 {
		if in.codexBin == "" {
			return nil, cfgErrf("--codex-bin must not be empty")
		}
		if err := checkUTF8(pathField{"--codex-bin", in.codexBin}); err != nil {
			return nil, err
		}
		if p.codexBin, err = doctor.Resolve(in.codexBin); err != nil {
			return nil, cfgErrf("--codex-bin %q: %v", in.codexBin, err)
		}
		if err := checkUTF8(pathField{"--codex-bin, resolved,", p.codexBin}); err != nil {
			return nil, err
		}
	}
	if p.writableRoots, err = physicalRoots(in.writableRoots); err != nil {
		return nil, err
	}
	if len(p.writableRoots) > 0 && len(p.peersOf(harnessCodex)) == 0 {
		return nil, cfgErrf("--writable-root widens a Codex peer's sandbox, and no peer is Codex")
	}
	if len(p.writableRoots) > 0 {
		p.global = carryGlobal(p.writableRoots)
	}

	if err := checkTargetTracks(ctx, p.target, buildWorktreeMarker, "build new", "each peer's worktree"); err != nil {
		return nil, err
	}
	out, err := git(ctx, "-C", p.target, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return nil, cfgErrf("--target %q has no HEAD to branch the peers' worktrees from: %v", p.target, err)
	}
	if p.baseCommit = strings.TrimSpace(out); !commitRE.MatchString(p.baseCommit) {
		return nil, fmt.Errorf("git rev-parse HEAD in %s printed %q, which is not a commit id", p.target, p.baseCommit)
	}
	if p.commonDir, err = commonGitDir(ctx, p.target); err != nil {
		return nil, err
	}
	// The common git dir goes into build.json, and the admin directory of each
	// worktree, which is a name of git's making directly under it, with it.
	if err := checkUTF8(pathField{"the target's common git directory", p.commonDir}); err != nil {
		return nil, err
	}
	return p, nil
}

// claimID takes the id's lock and, under it, looks at what the state base and the
// target hold for the id: whether it is live as a build or as a table, whether a
// peer has a cursor for the stream, and whether a peer's branch is there. These
// are the checks that another table or build command could change, so they are
// made only while no other can: the id is one namespace for a table and a build
// (the channel, a directory of this build, a branch prefix), and nothing else
// serializes two commands on it. The lock is held until releaseID, which the
// caller calls when the run is over; on a refusal it is already released. Nothing
// before it looked at the base, so a plain refusal (a flag, a doctor FAIL, a cap)
// has made no file there.
func (p *buildPlan) claimID(ctx context.Context) error {
	unlock, err := lockID(p.base, p.id)
	if err != nil {
		return err
	}
	p.unlockID = unlock
	if err := p.checkIDFree(ctx); err != nil {
		p.releaseID()
		return err
	}
	return nil
}

// checkIDFree is the checks claimID makes under the lock.
func (p *buildPlan) checkIDFree(ctx context.Context) error {
	if _, err := os.Lstat(p.buildDir); err == nil {
		return cfgErrf("build %q already exists under %s: parley build end %s takes it down", p.id, p.base, p.id)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("look for %s: %w", p.buildDir, err)
	}
	if _, err := os.Lstat(tableDirOf(p.base, p.id)); err == nil {
		return cfgErrf("%q is already a table under %s: parley table end %s takes it down, and a build takes another id", p.id, p.base, p.id)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("look for %s: %w", tableDirOf(p.base, p.id), err)
	}
	if err := checkNotALine(p.base, p.id, "a build"); err != nil {
		return err
	}
	if err := p.checkNoCursorState(); err != nil {
		return err
	}
	for _, s := range p.peers {
		if exists, err := branchExists(ctx, p.target, s.branch); err != nil {
			return err
		} else if exists {
			return cfgErrf("the branch %s already exists in %s: build new makes it, and will not move or reuse a branch; delete it (git -C %s branch -D %s) if it is a finished build's, or name another id", s.branch, p.target, q(p.target), q(s.branch))
		}
	}
	return nil
}

// checkNoCursorState refuses a peer that already has state for this build's
// stream: the cursor a pump or a channel left under <base>/<nick>/<streamkey>/
// is a position in a history that is not this run's, and the first delivery
// would end the run with a history gap.
func (p *buildPlan) checkNoCursorState() error {
	for _, s := range p.peers {
		dir, err := state.StreamDir(p.base, s.nick, p.stream)
		if err != nil {
			return cfgErrf("peer %q: %v", s.nick, err)
		}
		if _, err := os.Lstat(dir); err == nil {
			return cfgErrf("peer %q already has state for this stream under %s: a cursor left by an earlier run would end this one at its first delivery with a history gap; if that run is over, remove the directory, or name another id", s.nick, dir)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("look for %s: %w", dir, err)
		}
	}
	return nil
}

// branchExists asks the target whether refs/heads/<branch> is there: git
// show-ref says no with exit 1 and nothing else.
func branchExists(ctx context.Context, target, branch string) (bool, error) {
	_, _, err := gitCapped(ctx, "-C", target, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	if err == nil {
		return true, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}

// physicalRoots resolves each --writable-root to its physical path (see
// physicalRoot). A path given twice is kept once.
func physicalRoots(raw []string) ([]string, error) {
	var roots []string
	seen := map[string]bool{}
	for _, r := range raw {
		if r == "" {
			return nil, cfgErrf("--writable-root must not be empty")
		}
		phys, err := physicalRoot(r)
		if err != nil {
			return nil, cfgErrf("--writable-root %s", printable(err.Error()))
		}
		if !seen[phys] {
			seen[phys] = true
			roots = append(roots, phys)
		}
	}
	return roots, nil
}

// warnWritableRoots says, on w, that a writable root which holds or sits in the
// target's git directory hands the Codex peers the repository's hooks and config.
// It is the human's choice to make and is never a refusal.
func (p *buildPlan) warnWritableRoots(w io.Writer) {
	for _, root := range p.writableRoots {
		if within(root, p.commonDir) || within(p.commonDir, root) {
			fmt.Fprintf(w, "parley build new: warning: --writable-root %s holds or is inside the target's git directory %s: a Codex peer can then write the repository's hooks and config, which later run as you outside any sandbox (docs/build.md)\n", root, p.commonDir)
		}
	}
	if p.global == nil {
		return
	}
	for _, root := range p.global.carried {
		if within(root, p.commonDir) || within(p.commonDir, root) {
			fmt.Fprintf(w, "parley build new: warning: the global writable root %s, carried over from %s, holds or is inside the target's git directory %s: a Codex peer can then write the repository's hooks and config, which later run as you outside any sandbox (docs/build.md)\n", root, printable(p.global.path), p.commonDir)
		}
	}
}

// checkMachine runs doctor's static checks for the harnesses the peers use, the
// server and the build skill under the roots those harnesses read; see
// checkMachineWith.
func (p *buildPlan) checkMachine(ctx context.Context, w io.Writer) error {
	down, err := checkMachineWith(ctx, w, "parley build new", doctor.Options{
		Codex:      len(p.peersOf(harnessCodex)) > 0,
		Claude:     len(p.peersOf(harnessClaude)) > 0,
		CodexBin:   p.codexBin,
		Server:     p.server,
		BuildSkill: true,
	})
	p.serverDown = down
	return err
}

// measureClaudePeers builds each Claude peer's instructions in this process, the
// way `parley channel --check` does, with the peer's own nick and worktree, and
// refuses a build whose launch would be cut at Claude Code's cap. The values are
// the .mcp.json's, so the measure is the launch's, and it is made before any
// change.
func (p *buildPlan) measureClaudePeers() error {
	for _, s := range p.peersOf(harnessClaude) {
		if err := measureClaude("peer", "the build id, the peer's nick, --state-dir (the peer's worktree is <state-dir>/builds/<id>/<nick>), --target or the path of the parley binary", channel.Config{
			Spec:     p.stream,
			Skill:    primer.SkillBuild,
			Chair:    p.chair,
			Target:   p.target,
			Worktree: s.worktree,
		}, s.nick, p.base, nil); err != nil {
			return err
		}
	}
	return nil
}

// buildRun is one `build new` in progress: what it has made, so a failure can
// take exactly that down again.
type buildRun struct {
	plan     *buildPlan
	log      io.Writer
	undo     []undoStep
	replyDir string
	// unlock drops the peers' nick locks lockPeers took; nil until it has.
	unlock func()
	// nonce is this run's random string, in the reflog message of every branch it
	// makes: see branchMessage.
	nonce string
}

func (r *buildRun) logf(format string, a ...any) {
	fmt.Fprintf(r.log, "parley build new: "+format+"\n", a...)
}

func (r *buildRun) push(what string, do func() error) {
	r.undo = append(r.undo, undoStep{what: what, do: do})
}

func (r *buildRun) rollback() {
	if len(r.undo) == 0 {
		return
	}
	rollbackSteps(r.logf, r.undo)
	r.undo = nil
}

func (r *buildRun) mkdirAll(dir string) error { return mkdirAllWith(dir, r.push) }

func (r *buildRun) release() {
	if r.unlock != nil {
		r.unlock()
		r.unlock = nil
	}
}

// lockPeers takes both peers' nick locks before anything is made, as table new
// does the seats' (tableRun.lockSeats): a nick that a running pump or Claude Code
// session holds would make a printed launch fail after Codex turns were spent.
func (r *buildRun) lockPeers() error {
	p := r.plan
	nicks := make([]string, len(p.peers))
	for i, s := range p.peers {
		nicks[i] = s.nick
	}
	release, err := lockNicksMaking(p.base, nicks, r.mkdirAll, func(nick, dir string) error {
		return fmt.Errorf("peer %q is in use by a running pump or Claude Code session: its lock under %s is held; stop it, or give this peer another nick", nick, dir)
	})
	if err != nil {
		return err
	}
	r.unlock = release
	return nil
}

// setUp makes the build, step by step; any error is the caller's cue to roll
// back, and then to release.
func (r *buildRun) setUp(ctx context.Context) error {
	p := r.plan
	if err := r.lockPeers(); err != nil {
		return err
	}
	// Looked at once more with the locks held: nothing can start a pump or a
	// channel for these nicks now.
	if err := p.checkNoCursorState(); err != nil {
		return err
	}
	if err := r.mkdirAll(filepath.Dir(p.buildDir)); err != nil {
		return err
	}
	if err := os.Mkdir(p.buildDir, 0o700); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return cfgErrf("build %q already exists under %s: parley build end %s takes it down", p.id, p.base, p.id)
		}
		return fmt.Errorf("create %s: %w", p.buildDir, err)
	}
	r.push("build directory "+p.buildDir, func() error { return os.RemoveAll(p.buildDir) })

	for _, s := range p.peers {
		if err := r.makeWorktree(ctx, s); err != nil {
			return err
		}
	}

	// The same kind of directory the channel front makes for a Claude peer's
	// reply: 0700 under $TMPDIR, because Codex's workspace-write sandbox can
	// write $TMPDIR.
	dir, err := os.MkdirTemp("", buildReplyPrefix(p.id))
	if err != nil {
		return fmt.Errorf("create the reply directory: %w", err)
	}
	r.push("reply directory "+dir, func() error { return os.RemoveAll(dir) })
	if dir, err = filepath.Abs(dir); err != nil {
		return fmt.Errorf("resolve the reply directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("set permissions on %s: %w", dir, err)
	}
	// What lets `build end` know the directory is this build's before it removes
	// it, and the peers the build was made with.
	if err := writeNewFile(filepath.Join(dir, buildReplyMarker), []byte(p.marker().String()), 0o600); err != nil {
		return err
	}
	r.replyDir = dir

	for _, s := range p.peersOf(harnessCodex) {
		if err := r.startCodexPeer(ctx, s); err != nil {
			return err
		}
	}

	rec := buildRecord{
		ID: p.id, Stream: p.stream, Target: p.target, CommonGit: p.commonDir, BaseCommit: p.baseCommit,
		Chair: p.chair, ReplyDir: r.replyDir, Created: time.Now().UTC().Format(time.RFC3339),
	}
	for _, s := range p.peers {
		rec.Peers = append(rec.Peers, peerRecord{
			Nick: s.nick, Harness: s.harness, Model: s.model, Worktree: s.worktree, AdminGitDir: s.adminDir,
			Branch: s.branch, Thread: s.thread, ReplyFile: s.replyFile,
		})
	}
	digest, err := writeRecordFile(p.buildDir, buildRecordName, "build", rec, buildRecordMax, "build end")
	if err != nil {
		return err
	}
	// The record is final, so the markers are: each gets the digest of the exact
	// bytes written, as a third line, and build.json is not written again.
	sealed := p.marker().sealed(digest)
	for _, s := range p.peers {
		if err := sealMarker(s.worktree, "the worktree", filepath.Join(s.worktree, buildWorktreeMarker), sealed); err != nil {
			return err
		}
	}
	return sealMarker(r.replyDir, "the reply directory", filepath.Join(r.replyDir, buildReplyMarker), sealed)
}

// makeWorktree makes a peer's branch and its worktree, and writes what the
// peer's session launches from. The branch is made by one atomic call, and its
// undo is registered BEFORE that call: update-ref can create the branch and still
// fail or be interrupted, and a branch this run made is this run's to undo. What
// makes the early registration safe is what the undo checks (deleteMadeBranch):
// that the branch's reflog begins with this run's own message, which carries a
// nonce no other creator can have written, and that the branch still points at the
// base commit. A branch another creator made in the window has neither, and is
// never touched.
func (r *buildRun) makeWorktree(ctx context.Context, s *peerPlan) error {
	p := r.plan
	msg, err := r.branchMessage()
	if err != nil {
		return err
	}
	r.push("branch "+s.branch, func() error { return deleteMadeBranch(p.target, s.branch, p.baseCommit, msg) })
	// update-ref with an empty old value creates the ref only if it does not exist,
	// in one step; --create-reflog makes the reflog whatever core.logAllRefUpdates
	// says, so that the message is always there for the undo to find. The plan's
	// answer may be stale by now; a branch that turned up since is not this run's to
	// move, reuse or remove, and failing here leaves it as it is.
	r.logf("git update-ref refs/heads/%s %s (new branch)", s.branch, p.baseCommit)
	if _, err := git(ctx, "-C", p.target, "update-ref", "--create-reflog", "-m", msg, "refs/heads/"+s.branch, p.baseCommit, ""); err != nil {
		if exists, _ := branchExists(ctx, p.target, s.branch); exists {
			return cfgErrf("the branch %s already exists in %s (git update-ref said: %v): another run may have made it first, and build new will not move or reuse a branch; if this run made it, the rollback removes it, and says if it left it", s.branch, p.target, err)
		}
		return err
	}

	// The path is claimed before git runs, atomically, as table new's is: a run
	// that loses the race stops here with nothing of the winner's removed.
	if err := os.Mkdir(s.worktree, 0o700); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return cfgErrf("the worktree %s already exists: another run made it first", s.worktree)
		}
		return fmt.Errorf("create the worktree %s: %w", s.worktree, err)
	}
	r.push("worktree "+s.worktree, func() error { return removeMadeWorktree(p.target, s.worktree) })
	r.logf("git worktree add %s %s", s.worktree, s.branch)
	if _, err := git(ctx, "-C", p.target, "worktree", "add", s.worktree, s.branch); err != nil {
		return err
	}
	// Recorded from the target's side: the worktree's own .git file is the peer's
	// to rewrite, and build end runs git against what is recorded here.
	admin, err := worktreeAdminDir(p.commonDir, s.worktree)
	if err != nil {
		return err
	}
	// A name git chose under the common directory, which was checked at plan time:
	// looked at once more, because it goes into build.json too.
	if err := checkUTF8(pathField{"peer " + s.nick + "'s admin git directory", admin}); err != nil {
		return err
	}
	s.adminDir = admin

	// What `build end` can report when it is gone or changed (a peer's git clean
	// can remove it), and the peers the build was made with. It has two lines
	// until build.json is written; see the sealing in setUp.
	if err := writeNewFileIn(s.worktree, filepath.Join(s.worktree, buildWorktreeMarker), []byte(p.marker().String()), 0o644); err != nil {
		return err
	}
	switch s.harness {
	case harnessClaude:
		data, err := mcpConfigJSON(p.self, []string{"channel", "--stream", p.stream, "--nick", "${PARLEY_NICK}", "--state-dir", p.base,
			"--skill", "build", "--chair", p.chair, "--target", p.target, "--worktree", s.worktree})
		if err != nil {
			return err
		}
		return writeNewFileIn(s.worktree, filepath.Join(s.worktree, ".mcp.json"), data, 0o644)
	case harnessCodex:
		dir := filepath.Join(s.worktree, ".codex")
		if err := makeRealDir(dir); err != nil {
			return err
		}
		return writeNewFileIn(s.worktree, filepath.Join(dir, "config.toml"), []byte(buildCodexConfig(p.configRoots())), 0o644)
	}
	return nil
}

// branchMessage is the reflog message every branch of this run is made with:
// "parley build new <id> <nonce>", the nonce random per run, made once. A reader
// of the reflog finds it only on a branch this run made.
func (r *buildRun) branchMessage() (string, error) {
	if r.nonce == "" {
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", fmt.Errorf("make this run's nonce: %w", err)
		}
		r.nonce = hex.EncodeToString(b[:])
	}
	return "parley build new " + r.plan.id + " " + r.nonce, nil
}

// deleteMadeBranch undoes the creation of a branch by this run. The branch is
// this run's only if the oldest entry of its reflog (the first one ever written)
// is msg, the message this run made it with, and it is deleted only while it still
// points at the commit it was made at: update-ref -d with the old value is the
// compare-and-delete, so a branch that has moved since is not removed. A branch
// that is not there was never made (the call failed before it did), and one that
// is there and not shown to be this run's is another creator's, and is left.
//
// The reflog is read through git in the target, the target's own repository,
// which is trusted, and through the bound on git's output: a reflog longer than
// that cannot be read to its oldest entry, so the branch is left and the report
// says so. Reflog messages are one line; the oldest is the last line printed.
func deleteMadeBranch(target, branch, at, msg string) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	exists, err := branchExists(ctx, target, branch)
	if err != nil {
		return err
	}
	if !exists {
		return errNothingToUndo
	}
	out, err := git(ctx, "-C", target, "reflog", "show", "--no-show-signature", "--format=%gs", "refs/heads/"+branch)
	if err != nil {
		return leftAlone(fmt.Sprintf("its reflog could not be read to show it is this run's (%v)", err))
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if first := lines[len(lines)-1]; first != msg {
		return leftAlone("its reflog does not begin with this run's message, so another creator made it")
	}
	if _, err := git(ctx, "-C", target, "update-ref", "-d", "refs/heads/"+branch, at); err != nil {
		// The branch is this run's but is no longer at the commit it was made at (or
		// went away meanwhile): it is not removed on a guess.
		if still, _ := branchExists(ctx, target, branch); !still {
			return nil
		}
		return leftAlone(fmt.Sprintf("it is this run's but no longer at %s (%v)", at[:12], err))
	}
	return nil
}

// buildCodexConfig is the project config written into a Codex peer's worktree:
// the table's (workspace-write, on-request, network on) and, only when the human
// named any with --writable-root, those roots and the user's carried global ones
// (see configRoots), each as a TOML string. The target's git directory is never
// among them unless the human or the user's own config put it there.
func buildCodexConfig(roots []string) string {
	cfg := codexConfigTOML
	if len(roots) == 0 {
		return cfg
	}
	quoted := make([]string, len(roots))
	for i, r := range roots {
		quoted[i] = tomlQuote(r)
	}
	return cfg + "writable_roots = [" + strings.Join(quoted, ", ") + "]\n"
}

// tomlQuote is s as a TOML basic string. The paths it quotes have no control
// character, but the backslash and the quote are legal in a path, and DEL is
// escaped as TOML requires.
func tomlQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// startCodexPeer starts a peer's Codex session with its build primer as the first
// prompt and records the thread it announces (see startCodexSession).
func (r *buildRun) startCodexPeer(ctx context.Context, s *peerPlan) error {
	p := r.plan
	s.replyFile = filepath.Join(r.replyDir, s.nick+".reply")
	text := primer.Render(primer.Params{
		Front:     primer.Codex,
		Command:   p.self,
		Spec:      p.stream,
		Nick:      s.nick,
		StateDir:  p.base,
		ReplyPath: s.replyFile,
		Skill:     primer.SkillBuild,
		Chair:     p.chair,
		Target:    p.target,
		Worktree:  s.worktree,
	})
	thread, answer, err := startCodexSession(ctx, r, codexSession{
		noun: "peer", nick: s.nick, model: s.model, dir: s.worktree,
		bin: p.codexBin, name: p.codexName(), prompt: text, base: p.base, stream: p.stream,
	})
	if err != nil {
		return err
	}
	s.thread, s.answer = thread, answer
	return nil
}

// runbook is what goes to stdout: what the human runs next, in order. Shell
// commands have every path, stream, thread id and model quoted; the lines meant
// for the IRC client are not, because a quote there would be part of the line.
func (p *buildPlan) runbook(r *buildRun) string {
	var b strings.Builder
	first, second := p.peers[0], p.peers[1]
	step := 0
	next := func(format string, a ...any) {
		step++
		fmt.Fprintf(&b, "\n%d. "+format+"\n\n", append([]any{step}, a...)...)
	}
	cmd := func(line string) { fmt.Fprintf(&b, "    %s\n", line) }

	fmt.Fprintf(&b, "Build %s is set up: target %s at %s, two peers, each in a worktree of its own on a branch of its own:\n\n", p.id, p.target, p.baseCommit[:12])
	for _, s := range p.peers {
		fmt.Fprintf(&b, "    %s (%s)  %s  branch %s\n", s.nick, s.harness, s.worktree, s.branch)
	}
	b.WriteString("\nBefore anything below, two things parley does not check, because it never calls director:\n\n")
	b.WriteString("   - The target is adopted in Director: `director adopt`, run in the target before `parley build new`.\n")
	b.WriteString("   - The target ignores .director/. Director writes it into every checkout, a peer's worktree included,\n")
	b.WriteString("     and `parley build end` takes an untracked file for the peer's own work. Check as the first peer's\n")
	b.WriteString("     worktree sees it. The line names that worktree's git directory in the target, and does not follow the\n")
	b.WriteString("     .git file in the worktree, which the peer can rewrite to point git, and its config, elsewhere:\n\n")
	cmd(fmt.Sprintf("git -C %s --git-dir %s --work-tree %s check-ignore -q .director/x && echo ignored",
		q(first.worktree), q(first.adminDir), q(first.worktree)))
	exclude := filepath.Join(p.commonDir, "info", "exclude")
	fmt.Fprintf(&b, "\n     If that prints nothing, add the line /.director/ to %s, which every worktree of the\n", exclude)
	b.WriteString("     target shares and which holds from now on. A line in the target's .gitignore reaches the peers' worktrees\n")
	b.WriteString("     only once it is committed, before `parley build new` made them from HEAD.\n")
	b.WriteString("\nWhat is left is yours, in this order; indented lines starting with / go into your IRC client, the others into a shell.\n")

	if p.serverDown {
		server := q(p.self) + " server"
		if p.server != defaultListen {
			server += " --listen " + q(p.server)
		}
		next("Nothing answers at %s: start the server, in a terminal of its own.", p.server)
		cmd(server)
	}

	next("Join as the chair on %s, and check that your nick is registered, before any peer or pump connects.", p.server)
	cmd("/join #" + p.id)
	cmd("/msg NickServ INFO " + p.chair)
	b.WriteString("\n   If INFO says it is not registered, register it, and identify on every later connect:\n\n")
	cmd("/msg NickServ REGISTER <password>")
	b.WriteString("\n   An unregistered chair nick could be taken by a peer's process, and your lines would no longer be\n   the ones that rule the build.\n")

	codexName := p.codexName()
	pumpExtra := ""
	if p.codexGiven {
		pumpExtra = " --codex-bin " + q(p.codexBin)
	}
	for _, s := range p.peersOf(harnessCodex) {
		model := ""
		if s.model != "" {
			model = " (model " + s.model + ")"
		}
		next("Codex peer %s%s. In one terminal start its pump, before message one:", s.nick, model)
		cmd(fmt.Sprintf("%s codex --stream %s --nick %s --state-dir %s --thread auto --skill build%s",
			q(p.self), q(p.stream), s.nick, q(p.base), pumpExtra))
		b.WriteString("\n   In another, resume its session. Each approval prompt there stops the run until you answer it, because the\n")
		b.WriteString("   pump delivers between Codex turns only: keep that terminal in view. Codex asks whether to trust the\n")
		b.WriteString("   directory: accept, so that the worktree's .codex/config.toml applies. Check that /status shows\n")
		b.WriteString("   workspace-write and network on, then type $parley-build:\n\n")
		cmd(fmt.Sprintf("%s resume %s -C %s -a on-request", codexName, q(s.thread), q(s.worktree)))
		if len(p.writableRoots) > 0 {
			fmt.Fprintf(&b, "\n   Its sandbox writes its worktree, $TMPDIR, /tmp and %s, as --writable-root asked.\n", strings.Join(p.writableRoots, ", "))
			for _, line := range p.globalRootsRunbook() {
				b.WriteString("   " + line + "\n")
			}
		} else {
			b.WriteString("\n   Its sandbox writes its worktree, $TMPDIR and /tmp, plus any global roots in your ~/.codex/config.toml, and\n")
			b.WriteString("   nothing else: its git commit and director emit go through Codex's approval (on-request) unless a root of\n")
			b.WriteString("   yours covers them; docs/build.md says which.\n")
		}
		fmt.Fprintf(&b, "\n   Peer %s's answer to its primer (codex exec, thread %s):\n\n", s.nick, s.thread)
		answer := plainAnswer(s.answer)
		if answer == "" {
			answer = "(none: the turn ended without a final message)"
		}
		b.WriteString(indent(answer, "        ") + "\n")
	}

	if claude := p.peersOf(harnessClaude); len(claude) > 0 {
		next("Claude peers, each in a terminal of its own, in its own worktree. Approve the project server parley when it asks, check that /mcp shows it connected, then type /parley-build:")
		for _, s := range claude {
			line := fmt.Sprintf("cd %s && PARLEY_NICK=%s claude --dangerously-load-development-channels server:parley", q(s.worktree), s.nick)
			if s.model != "" {
				line += " --model " + q(s.model)
			}
			cmd(line)
		}
	}

	next("The gate. Do not hand over message one until every item holds:")
	items := []string{"each peer has loaded its skill at its own terminal"}
	if len(p.peersOf(harnessClaude)) > 0 {
		items = append(items, claudeGateItem("peer"))
	}
	if codexPeers := p.peersOf(harnessCodex); len(codexPeers) > 0 {
		status := "each Codex peer's /status shows workspace-write and network on"
		var models []string
		for _, s := range codexPeers {
			if s.model != "" {
				models = append(models, s.nick+" "+s.model)
			}
		}
		if len(models) > 0 {
			status += fmt.Sprintf(", and its model (%s); the model check is unverified on a resumed session, so confirm it for this build", strings.Join(models, ", "))
		}
		items = append(items, "every Codex pump is running", status)
	}
	for i, item := range items {
		end := ";"
		if i == len(items)-1 {
			end = "."
		}
		fmt.Fprintf(&b, "   - %s%s\n", item, end)
	}

	next("Message one. It is a peer's to post, not yours: the other peer checks that its participants: line names its own nick and the sender's, so a line you type does not open the run. Choose the budget and the cap, fill in the task and the size, and hand this to %s at its terminal to post:", first.nick)
	cmd("RUN " + p.id)
	cmd("budget: <n> rounds")
	cmd("cap: <duration>")
	cmd("human: " + p.chair)
	cmd("participants: " + first.nick + " " + second.nick)
	cmd("target: " + p.target)
	cmd("task: <one or two sentences>")
	cmd("size: <how many items are expected, roughly how much code>")
	cmd("owns <nick>: <path>")
	b.WriteString("\n   When the task splits by file, name each peer's files: one owns <nick>: <path> line per path, a directory covering\n")
	b.WriteString("   everything under it. Leave the line out only when it does not split that way. Nothing else in the block is optional.\n")

	next("Integration, once both peers have agreed every item: %s, the second participant, tests a throwaway merge of %s's branch; %s posts END after a passing test, and only then does %s ask you in the channel to approve the real merge, naming both branches and both commits. Check the two commits before you answer; each branch's tip is:", second.nick, first.nick, first.nick, second.nick)
	cmd(fmt.Sprintf("git -C %s rev-parse refs/heads/%s refs/heads/%s", q(p.target), first.branch, second.branch))
	b.WriteString("\n   If both match, answer yes there under your registered nick: that line is the approval, and the merge it authorizes\n")
	b.WriteString("   is of exactly the commit the ask named. A question or a condition is not a yes, and the integrator asks again.\n")
	fmt.Fprintf(&b, "   If %s's work is already in %s's branch there is nothing to merge: no ask, and %s's branch is the result.\n", first.nick, second.nick, second.nick)
	b.WriteString("   Its terminal may still ask you to approve the git command. Name another peer as the integrator in the task if\n")
	b.WriteString("   you want it to be that one.\n\n")

	fmt.Fprintf(&b, "When it is over: %s build end %s --state-dir %s\n", q(p.self), p.id, q(p.base))
	return b.String()
}
