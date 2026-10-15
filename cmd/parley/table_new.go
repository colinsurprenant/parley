package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/colinsurprenant/parley/internal/channel"
	"github.com/colinsurprenant/parley/internal/codex"
	"github.com/colinsurprenant/parley/internal/doctor"
	"github.com/colinsurprenant/parley/internal/primer"
	"github.com/colinsurprenant/parley/internal/stream"
)

const (
	harnessClaude = "claude"
	harnessCodex  = "codex"

	// seatTurnTimeout bounds one Codex seat's start: a primer is one short turn.
	seatTurnTimeout = 5 * time.Minute
	// codexDeleteTimeout bounds one `codex delete`.
	codexDeleteTimeout = time.Minute
	// maxExecOutput bounds what is kept of a seat's JSONL.
	maxExecOutput = 1 << 20
)

// codexConfigTOML is the project config written into the worktree for the Codex
// seats: the sandbox and approval policy they run under, and the network access
// that lets `parley send` reach the loopback IRC server from inside the sandbox.
const codexConfigTOML = `sandbox_mode = "workspace-write"
approval_policy = "on-request"

[sandbox_workspace_write]
network_access = true
`

// stringList is a repeatable string flag.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// seatPlan is one seat. thread, replyFile and answer are a Codex seat's, filled
// in as it is started.
type seatPlan struct {
	nick, harness, model string
	replyFile            string
	thread               string
	answer               string
}

// tablePlan is everything `table new` has validated before it changes anything.
type tablePlan struct {
	id       string
	stream   string // canonical
	target   string
	worktree string
	chair    string
	rounds   int
	cap      int
	seats    []*seatPlan
	// base is the state base, resolved and absolute; every seat and every pump
	// resolves its state from it, so it is named in each command printed.
	base     string
	tableDir string
	// server is the loopback IP:port of the IRC server.
	server string
	// self is this binary's absolute path.
	self string
	// codexBin is the resolved codex when a seat uses it; codexGiven is whether
	// --codex-bin was named, in which case the lines printed for the human carry it.
	codexBin   string
	codexGiven bool
	// serverDown is set by the machine check when nothing answers at server.
	serverDown bool
	// unlockID drops the id's lock claimID took; nil until it has. The caller
	// calls releaseID when the run is over.
	unlockID func()
}

// releaseID drops the id lock claimID took, if it did.
func (p *tablePlan) releaseID() {
	if p != nil && p.unlockID != nil {
		p.unlockID()
		p.unlockID = nil
	}
}

// codexName is how the lines printed for the human, and the rollback's own
// account of what it ran, spell the codex this run used: the one --codex-bin
// named, shell-quoted, else plain codex, which is what the PATH gave.
func (p *tablePlan) codexName() string {
	if p.codexGiven {
		return q(p.codexBin)
	}
	return "codex"
}

func (p *tablePlan) seatsOf(harness string) []*seatPlan {
	var out []*seatPlan
	for _, s := range p.seats {
		if s.harness == harness {
			out = append(out, s)
		}
	}
	return out
}

// marker is the content of both markers table new writes first: the table's id
// and the seats it is made with, to which sealed adds the digest of table.json.
func (p *tablePlan) marker() tableMarker {
	nicks := make([]string, len(p.seats))
	for i, s := range p.seats {
		nicks[i] = s.nick
	}
	return newTableMarker(p.id, nicks)
}

// cmdTableNew sets a table up: the worktree, the configs, the reply directory and
// each Codex seat's session started with its primer, its thread recorded. It
// prints what the human runs next. Everything it made is taken down again if any
// step fails, but the seats' lock files, which stay as a pump's do.
func cmdTableNew(args []string) error {
	fs := flag.NewFlagSet("parley table new", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), tableUsageNew)
		fs.PrintDefaults()
	}
	target := fs.String("target", "", "required: the repo the table thinks about, the root of a git checkout; nothing is written into it but a worktree registration")
	chair := fs.String("chair", "", "required: your registered nick on the server, the human who chairs the table")
	rounds := fs.Int("rounds", 0, "required: rounds the table runs before a checkpoint; the gates' cap is rounds times the number of seats")
	var seatArgs stringList
	fs.Var(&seatArgs, "seat", "required, repeatable: a seat as <nick>=<claude|codex>[:<model>], e.g. --seat codex-a=codex:gpt-5 --seat claude-a=claude:opus; a model is at most 128 bytes of letters, digits and . _ - : / @ [ ] +; opencode seats come after v1")
	worktree := fs.String("worktree", "", "the shared worktree to make, an absolute path that does not exist yet (default <state base>/tables/<id>/worktree)")
	server := fs.String("server", defaultListen, "the loopback IP:port of the IRC server the table uses (a host name is refused)")
	stateDir := fs.String("state-dir", "", "state directory holding <nick>/ and tables/ (default $XDG_STATE_HOME/parley)")
	codexBin := fs.String("codex-bin", codex.DefaultBin, "the codex CLI to run, resolved on PATH unless it holds a separator")
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
		return cfgErrf("the table id is required, first: parley table new <id> --target <repo> --chair <nick> --rounds <n> --seat <nick>=<harness> ...")
	case fs.NArg() > 0:
		return cfgErrf("unexpected argument %q", fs.Arg(0))
	}

	ctx, stop := signalContext()
	defer stop()
	plan, err := planTable(ctx, tableInput{
		id: id, target: *target, targetGiven: given(fs, "target"),
		chair: *chair, chairGiven: given(fs, "chair"),
		rounds: *rounds, seats: seatArgs,
		worktree: *worktree, worktreeGiven: given(fs, "worktree"),
		server: *server, stateDir: *stateDir,
		codexBin: *codexBin, codexGiven: given(fs, "codex-bin"),
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "parley table new: checking the machine (parley doctor's static checks)\n")
	if err := plan.checkMachine(ctx, os.Stderr); err != nil {
		return err
	}
	if err := plan.measureClaudeSeats(); err != nil {
		return err
	}
	// From here the run looks at the base: under the id's lock, held for the whole
	// run, rollback and the runbook included.
	if err := plan.claimID(); err != nil {
		return err
	}
	defer plan.releaseID()

	run := &tableRun{plan: plan, log: os.Stderr}
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
	// The seats' locks are held through the rollback, and no longer. Their lock
	// files stay, as a pump's do: see lockSeats.
	run.release()
	if err != nil {
		return err
	}
	signal.Ignore(syscall.SIGPIPE)
	if _, err := io.WriteString(os.Stdout, plan.runbook(run)); err != nil {
		return fmt.Errorf("table %s is set up but its instructions could not be written (%v): parley table end %s takes it down", plan.id, err, plan.id)
	}
	return nil
}

// tableInput is what planTable validates: the flags as given.
type tableInput struct {
	id            string
	target        string
	targetGiven   bool
	chair         string
	chairGiven    bool
	rounds        int
	seats         []string
	worktree      string
	worktreeGiven bool
	server        string
	stateDir      string
	codexBin      string
	codexGiven    bool
}

// planTable validates everything that can be validated before a change. Each
// refusal is a configuration error, exit 2, and nothing has been created, run
// or connected to but read-only git questions. What depends on the state of the
// base for this id (whether it is live as a table or as a build) is looked at
// later, under the id's lock: see claimID.
func planTable(ctx context.Context, in tableInput) (*tablePlan, error) {
	if err := checkTableID(in.id); err != nil {
		return nil, err
	}
	p := &tablePlan{id: in.id, rounds: in.rounds, codexGiven: in.codexGiven}

	if !in.targetGiven || in.target == "" {
		return nil, cfgErrf("--target is required: the repo the table thinks about")
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

	if len(in.seats) == 0 {
		return nil, cfgErrf("at least one --seat is required: --seat <nick>=<claude|codex>[:<model>]")
	}
	for _, raw := range in.seats {
		s, err := parseMember("--seat", raw)
		if err != nil {
			return nil, err
		}
		if stream.NickEqual(s.nick, in.chair) {
			return nil, cfgErrf("--seat %q: the nick is the chair's; the chair is another voice than any seat, and nicks are the same without regard to ASCII case", raw)
		}
		for _, prev := range p.seats {
			if stream.NickEqual(s.nick, prev.nick) {
				return nil, cfgErrf("--seat %q: the nick is also seat %q's, and nicks are the same without regard to ASCII case", raw, prev.nick)
			}
		}
		p.seats = append(p.seats, s)
	}

	if in.rounds < 1 {
		return nil, cfgErrf("--rounds is required and at least 1: the cap is rounds times seats")
	}
	if in.rounds > math.MaxInt32/len(p.seats) {
		return nil, cfgErrf("--rounds %d is too large", in.rounds)
	}
	p.cap = in.rounds * len(p.seats)

	env, err := resolvePlanEnv(in.id, in.server, in.stateDir)
	if err != nil {
		return nil, err
	}
	p.server, p.stream, p.self, p.base = env.server, env.stream, env.self, env.base
	// The sheet lives under the state base, and the TABLE line the chair posts
	// names it: a line the seats split on whitespace.
	if strings.IndexFunc(p.base, unicode.IsSpace) >= 0 {
		return nil, cfgErrf("the state directory %q holds whitespace, and the TABLE line the chair posts names the sheet under it as <id> <path>, split on whitespace: name a --state-dir without spaces", p.base)
	}
	p.tableDir = tableDirOf(p.base, in.id)

	p.worktree = filepath.Join(p.tableDir, tableWorktreeName)
	if in.worktreeGiven {
		if err := checkUTF8(pathField{"--worktree", in.worktree}); err != nil {
			return nil, err
		}
		if !filepath.IsAbs(in.worktree) {
			return nil, cfgErrf("--worktree %q must be an absolute path", in.worktree)
		}
		p.worktree = filepath.Clean(in.worktree)
		if _, err := os.Lstat(p.worktree); err == nil {
			return nil, cfgErrf("--worktree %q already exists: name a path that does not", in.worktree)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, cfgErrf("--worktree %q: %v", in.worktree, err)
		}
		if fi, err := os.Stat(filepath.Dir(p.worktree)); err != nil || !fi.IsDir() {
			return nil, cfgErrf("--worktree %q: its parent directory must exist", in.worktree)
		}
	}
	tmp, err := filepath.Abs(os.TempDir())
	if err != nil {
		return nil, cfgErrf("resolve the temp directory %q: %v", os.TempDir(), err)
	}
	// Every path that table.json or an .mcp.json will name must survive
	// encoding/json as it is, so each is checked before anything is made. The reply
	// directory is $TMPDIR plus a name of digits; the table directory is the state
	// directory plus the id, which is ASCII.
	if err := checkUTF8(pathField{"--target", p.target},
		pathField{"the state directory (--state-dir, or $XDG_STATE_HOME, or $HOME)", p.base}, pathField{"the worktree", p.worktree},
		pathField{"this binary's path", p.self},
		pathField{"the temp directory ($TMPDIR), which the reply directory is made under,", tmp}); err != nil {
		return nil, err
	}
	if within(p.target, p.worktree) {
		return nil, cfgErrf("the worktree %s is inside the target %s: nothing is written into the target checkout, so the worktree goes elsewhere", p.worktree, p.target)
	}
	// What the run writes besides the worktree is the state base and the reply
	// directory under $TMPDIR; neither goes into the target checkout, whose
	// status the chair reads to see nothing was written, or into the worktree
	// the seats read and `table end` judges for changes.
	for _, d := range []struct{ what, path, remedy string }{
		{"the state directory", p.base, "name a --state-dir outside it"},
		{"the temp directory ($TMPDIR), which holds the reply directory,", tmp, "run with a TMPDIR outside it"},
	} {
		for _, host := range []struct{ what, path string }{{"the target checkout", p.target}, {"the worktree", p.worktree}} {
			if within(host.path, d.path) {
				return nil, cfgErrf("%s %s is inside %s %s: %s", d.what, d.path, host.what, host.path, d.remedy)
			}
		}
	}

	// The paths land in a .mcp.json whose command and args Claude Code expands
	// ${...} in, and in text the seats read: a '$' would be expanded, and a
	// control character would break the line it is printed on.
	paths := []struct{ name, path string }{{"--target", p.target}, {"the worktree", p.worktree}, {"this binary's path", p.self}}
	if len(p.seatsOf(harnessClaude)) > 0 {
		paths = append(paths, struct{ name, path string }{"the state directory", p.base})
	}
	for _, x := range paths {
		if strings.Contains(x.path, "$") {
			return nil, cfgErrf("%s %q holds a '$': Claude Code expands ${...} in a .mcp.json's command and args, so the path would not reach the channel as written", x.name, x.path)
		}
	}
	for _, x := range []struct{ name, path string }{{"--target", p.target}, {"the worktree", p.worktree}, {"this binary's path", p.self}, {"the state directory", p.base}} {
		if hasControl(x.path) {
			return nil, cfgErrf("%s %q holds a control character: it is printed into commands and into the text the seats read", x.name, x.path)
		}
	}

	if len(p.seatsOf(harnessCodex)) > 0 {
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

	if err := p.checkTarget(ctx); err != nil {
		return nil, err
	}
	return p, nil
}

// claimID takes the id's lock and, under it, looks at what the state base holds
// for the id: whether it is live as a table or as a build. These are the checks
// that another table or build command could change, so they are made only while
// no other can: the id is one namespace for a table and a build (the channel, a
// directory of this table), and nothing else serializes two commands on it. The
// lock is held until releaseID, which the caller calls when the run is over; on a
// refusal it is already released. Nothing before it looked at the base, so a
// plain refusal (a flag, a doctor FAIL, a cap) has made no file there.
func (p *tablePlan) claimID() error {
	unlock, err := lockID(p.base, p.id)
	if err != nil {
		return err
	}
	p.unlockID = unlock
	if err := p.checkIDFree(); err != nil {
		p.releaseID()
		return err
	}
	return nil
}

// checkIDFree is the checks claimID makes under the lock.
func (p *tablePlan) checkIDFree() error {
	if _, err := os.Lstat(p.tableDir); err == nil {
		return cfgErrf("table %q already exists under %s: parley table end %s takes it down", p.id, p.base, p.id)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("look for %s: %w", p.tableDir, err)
	}
	// The id is also the channel and, for a build, a directory of its own: one
	// that is live as a build is not free for a table.
	if _, err := os.Lstat(buildDirOf(p.base, p.id)); err == nil {
		return cfgErrf("%q is already a build under %s: parley build end %s takes it down, and a table takes another id", p.id, p.base, p.id)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("look for %s: %w", buildDirOf(p.base, p.id), err)
	}
	return nil
}

// parseMember parses one --seat or --peer value (flagName), <nick>=<harness>[:<model>].
// The harness is claude or codex; OpenCode comes after v1. The two directories
// the state base keeps tables and builds in sit beside the per-nick ones, so
// neither is a nick: stream.ValidNick refuses both.
func parseMember(flagName, raw string) (*seatPlan, error) {
	nick, spec, ok := strings.Cut(raw, "=")
	if !ok {
		return nil, cfgErrf("%s %q: want <nick>=<claude|codex>[:<model>], e.g. codex-a=codex:gpt-5", flagName, raw)
	}
	if err := stream.ValidNick(nick); err != nil {
		return nil, cfgErrf("%s %q: nick %q %v", flagName, raw, nick, err)
	}
	harness, model, hasModel := strings.Cut(spec, ":")
	switch harness {
	case harnessClaude, harnessCodex:
	case "opencode":
		return nil, cfgErrf("%s %q: OpenCode %ss come after v1; the harnesses are claude and codex", flagName, raw, strings.TrimPrefix(flagName, "--"))
	default:
		return nil, cfgErrf("%s %q: unknown harness %q: the harnesses are claude and codex", flagName, raw, harness)
	}
	if hasModel {
		switch {
		case model == "":
			return nil, cfgErrf("%s %q: the model after ':' is empty", flagName, raw)
		case hasControl(model):
			return nil, cfgErrf("%s %q: the model holds a control character", flagName, raw)
		case strings.HasPrefix(model, "-"):
			return nil, cfgErrf("%s %q: the model begins with '-', which a command line would take for a flag", flagName, raw)
		case len(model) > modelMax:
			return nil, cfgErrf("%s: the model is %d bytes, over the %d bytes of the longest model id a harness takes (it is kept in the record, printed into commands and handed to `codex exec -m` and `claude --model`)", flagName, len(model), modelMax)
		}
		if i := strings.IndexFunc(model, func(r rune) bool { return !modelRune(r) }); i >= 0 {
			bad, _ := utf8.DecodeRuneInString(model[i:])
			return nil, cfgErrf("%s %q: the model holds %q, which no model id of Claude Code or Codex has: a model is letters, digits and . _ - : / @ [ ] +", flagName, raw, bad)
		}
	}
	return &seatPlan{nick: nick, harness: harness, model: model}, nil
}

// modelMax bounds a --seat or --peer model: the longest ids Claude Code and Codex
// take are a Bedrock inference-profile ARN of about 100 bytes, so 128 leaves room.
// The bound is what keeps the model, which is kept in the table or build record,
// from making a record that `table end` or `build end` would refuse to read.
const modelMax = 128

// modelRune is the characters a model id is written in, from what Claude Code and
// Codex take for a model: aliases (opus, sonnet[1m], gpt-5), dated ids
// (claude-sonnet-4-5-20250929, claude-sonnet-4-5@20250929), provider-qualified
// ones (us.anthropic.claude-sonnet-4-5-v1:0, openai/gpt-5, qwen2.5-coder:32b) and
// ARNs. Letters and digits of ASCII, and . _ - : / @ [ ] +.
func modelRune(r rune) bool {
	switch {
	case 'a' <= r && r <= 'z', 'A' <= r && r <= 'Z', '0' <= r && r <= '9':
		return true
	}
	return strings.ContainsRune("._-:/@[]+", r)
}

// checkTarget asks git the read-only questions about the target: that it is the
// root of a checkout, and that its HEAD tracks nothing `table new` writes into
// the worktree. A tracked .mcp.json, whatever it is, or a tracked
// .codex/config.toml would be checked out there already, and the seat would read
// the repo's configuration rather than the table's; a tracked .parley-table would
// be there already too, and table end could not tell it from the table's own
// marker; and a tracked .codex that is not a directory, a symlink most
// dangerously, would send the config write wherever it points.
func (p *tablePlan) checkTarget(ctx context.Context) error {
	return checkTargetTracks(ctx, p.target, tableWorktreeMarker, "table new", "the worktree")
}

// checkTargetTracks is checkTarget for the command verb (`table new`, `build
// new`) that writes marker, .mcp.json and .codex/config.toml into where, a
// worktree made from the target's HEAD.
func checkTargetTracks(ctx context.Context, target, marker, verb, where string) error {
	if fi, err := os.Stat(target); err != nil || !fi.IsDir() {
		return cfgErrf("--target %q is not a directory", target)
	}
	out, err := git(ctx, "-C", target, "rev-parse", "--show-toplevel")
	if err != nil {
		return cfgErrf("--target %q is not a git checkout: %v", target, err)
	}
	top := strings.TrimSpace(out)
	if resolveExisting(top) != resolveExisting(target) {
		return cfgErrf("--target %q is not the root of its checkout (the root is %s)", target, top)
	}
	// Two questions, because a pathspec that goes below .codex makes ls-tree list
	// what is in it instead of the entry for .codex itself.
	var tracked []string
	for _, paths := range [][]string{{".mcp.json", ".codex", marker}, {".codex/config.toml"}} {
		out, err = git(ctx, append([]string{"-C", target, "ls-tree", "HEAD", "--"}, paths...)...)
		if err != nil {
			return cfgErrf("--target %q has no HEAD to branch the worktree from: %v", target, err)
		}
		for _, line := range strings.Split(out, "\n") {
			meta, path, ok := strings.Cut(line, "\t")
			if !ok {
				continue
			}
			mode, _, _ := strings.Cut(meta, " ")
			switch {
			case path == ".codex" && mode == treeMode:
			case path == ".codex":
				return cfgErrf("--target %q tracks .codex at HEAD as %s, not as a directory: %s writes .codex/config.toml into %s, and through a link it would land elsewhere", target, modeName(mode), verb, where)
			default:
				tracked = append(tracked, path)
			}
		}
	}
	if len(tracked) > 0 {
		return cfgErrf("--target %q tracks %s at HEAD: %s writes those into %s, and a tracked one would already be there", target, strings.Join(tracked, " and "), verb, where)
	}
	return nil
}

// treeMode is the mode git ls-tree prints for a directory.
const treeMode = "040000"

func modeName(mode string) string {
	switch mode {
	case "120000":
		return "a symlink"
	case "160000":
		return "a submodule"
	}
	return "a file (mode " + mode + ")"
}

// checkMachine runs doctor's static checks for the harnesses the seats use, the
// server and the table skill under the roots those harnesses read. A FAIL ends
// the run before any change; the rest are printed, and a server that is not
// answering is remembered, because the instructions then start with the command
// that starts it, and so ergo, which that command runs, has to be there too.
func (p *tablePlan) checkMachine(ctx context.Context, w io.Writer) error {
	down, err := checkMachineWith(ctx, w, "parley table new", doctor.Options{
		Codex:      len(p.seatsOf(harnessCodex)) > 0,
		Claude:     len(p.seatsOf(harnessClaude)) > 0,
		CodexBin:   p.codexBin,
		Server:     p.server,
		TableSkill: true,
	})
	p.serverDown = down
	return err
}

// checkMachineWith is checkMachine for a command (`table new`, `build new`) and
// the surfaces o picks. It reports whether the server was not answering.
func checkMachineWith(ctx context.Context, w io.Writer, command string, o doctor.Options) (serverDown bool, err error) {
	results := doctor.Check(ctx, o)
	for _, r := range results {
		if r.Surface == "server" && r.Level == doctor.Warn {
			serverDown = true
		}
	}
	if serverDown && ctx.Err() == nil {
		results = append(results, doctor.Check(ctx, doctor.Options{ErgoBin: doctor.DefaultErgoBin})...)
	}
	if ctx.Err() != nil {
		return serverDown, errors.New("interrupted")
	}
	failed := 0
	for _, r := range results {
		if r.Level != doctor.OK {
			fmt.Fprintf(w, "%s: doctor: %s\n", command, r)
		}
		if r.Level == doctor.Fail {
			failed++
		}
	}
	if failed > 0 {
		return serverDown, fmt.Errorf("%d of %d doctor checks failed, and nothing was changed: parley doctor shows the whole report", failed, len(results))
	}
	return serverDown, nil
}

// measureClaudeSeats builds each Claude seat's instructions in this process, the
// way `parley channel --check` does, and refuses a table whose launch would be
// cut at Claude Code's cap. The worktree and the state base are the ones the
// .mcp.json will carry, so the measure is the launch's.
func (p *tablePlan) measureClaudeSeats() error {
	for _, s := range p.seatsOf(harnessClaude) {
		if err := measureClaude("seat", "--target, --worktree, --state-dir, --stream or the path of the parley binary", channel.Config{
			Spec:     p.stream,
			Skill:    primer.SkillTable,
			Chair:    p.chair,
			Target:   p.target,
			Worktree: p.worktree,
		}, s.nick, p.base, []stream.Option{stream.WithTable(p.chair, p.cap)}); err != nil {
			return err
		}
	}
	return nil
}

// measureClaude refuses a Claude session (noun, "seat" or "peer") whose channel
// instructions, built from cfg as a launch would build them, are over Claude
// Code's cap; remedy names what to shorten.
func measureClaude(noun, remedy string, cfg channel.Config, nick, base string, gate []stream.Option) error {
	srv, err := measuringServer(cfg, nick, base, gate)
	if err != nil {
		return fmt.Errorf("measure %s %s's instructions: %w", noun, nick, err)
	}
	n := primer.UTF16Len(srv.LongestInstructions())
	srv.Close()
	if n > primer.ClaudeInstructionsCap {
		return cfgErrf("claude %s %s: %v", noun, nick, overCapBy(n, remedy))
	}
	return nil
}

// undoStep is one thing a run made and how to take it down.
type undoStep struct {
	what string
	do   func() error
}

// tableRun is one `table new` in progress: what it has made, so a failure can
// take exactly that down again.
type tableRun struct {
	plan     *tablePlan
	log      io.Writer
	undo     []undoStep
	replyDir string
	// unlock drops the seats' nick locks lockSeats took; nil until it has.
	unlock func()
	// sheet is the copy of the sheet template, empty when none was found.
	sheet string
}

func (r *tableRun) logf(format string, a ...any) {
	fmt.Fprintf(r.log, "parley table new: "+format+"\n", a...)
}

func (r *tableRun) push(what string, do func() error) {
	r.undo = append(r.undo, undoStep{what: what, do: do})
}

// rollback takes down what the run made, newest first, and says what it did and
// what it could not do.
func (r *tableRun) rollback() {
	if len(r.undo) == 0 {
		return
	}
	rollbackSteps(r.logf, r.undo)
	r.undo = nil
}

// mkdirAll makes dir and whatever is missing above it at 0700, and remembers
// each directory it made, so that a rollback removes the ones that are still
// empty (see mkdirAllWith).
func (r *tableRun) mkdirAll(dir string) error { return mkdirAllWith(dir, r.push) }

// release drops the locks lockSeats took. The caller calls it once setUp has
// returned and any rollback is done.
func (r *tableRun) release() {
	if r.unlock != nil {
		r.unlock()
		r.unlock = nil
	}
}

// lockSeats takes every seat's nick lock, which is per nick across all
// streams, before anything is made: a nick that a running pump or Claude Code
// session holds would make a printed launch fail after Codex turns were spent.
// The locks are held until release. The nick directories this makes are
// registered for the rollback, which removes the ones that are still empty.
//
// The lock files are not registered, and no parley code unlinks one: a lock is
// held on the file's inode, so once the path is unlinked another process can
// create a new file there and lock it while the old inode is still locked, and
// two holders of one nick's lock would each believe they were alone. A failed or
// refused run therefore leaves <base>/<nick>/lock, and the directory holding
// it, behind, just as a pump does.
func (r *tableRun) lockSeats() error {
	p := r.plan
	nicks := make([]string, len(p.seats))
	for i, s := range p.seats {
		nicks[i] = s.nick
	}
	release, err := lockNicksMaking(p.base, nicks, r.mkdirAll, func(nick, dir string) error {
		return fmt.Errorf("seat %q is in use by a running pump or Claude Code session: its lock under %s is held; stop it, or give this seat another nick", nick, dir)
	})
	if err != nil {
		return err
	}
	r.unlock = release
	return nil
}

// setUp makes the table, step by step; any error is the caller's cue to roll
// back, and then to release.
func (r *tableRun) setUp(ctx context.Context) error {
	p := r.plan
	if err := r.lockSeats(); err != nil {
		return err
	}
	if err := r.mkdirAll(filepath.Dir(p.tableDir)); err != nil {
		return err
	}
	if err := os.Mkdir(p.tableDir, 0o700); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return cfgErrf("table %q already exists under %s: parley table end %s takes it down", p.id, p.base, p.id)
		}
		return fmt.Errorf("create %s: %w", p.tableDir, err)
	}
	r.push("table directory "+p.tableDir, func() error { return os.RemoveAll(p.tableDir) })

	// The path is claimed before git runs, atomically: a run that loses the race
	// stops here with nothing of the winner's removed, and what this run registers
	// for removal is a directory it made itself, so it is registered before git
	// can leave anything in it. Git keeps a new worktree when the target's
	// post-checkout hook fails, and says so only by its exit status.
	if err := os.Mkdir(p.worktree, 0o700); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return cfgErrf("the worktree %s already exists: another run made it first; name a path that does not exist", p.worktree)
		}
		return fmt.Errorf("create the worktree %s: %w", p.worktree, err)
	}
	r.push("worktree "+p.worktree, func() error { return removeMadeWorktree(p.target, p.worktree) })
	r.logf("git worktree add --detach %s HEAD", p.worktree)
	if _, err := git(ctx, "-C", p.target, "worktree", "add", "--detach", p.worktree, "HEAD"); err != nil {
		return err
	}
	// What lets `table end` know the worktree is this table's, and the seats the
	// table was made with: git lists the other tables' worktrees too, and a record
	// could be edited to name one of them or to change its seats. It has two lines
	// until table.json is written; see sealMarker below.
	marker := p.marker().String()
	if err := writeNewFileIn(p.worktree, filepath.Join(p.worktree, tableWorktreeMarker), []byte(marker), 0o644); err != nil {
		return err
	}

	if len(p.seatsOf(harnessClaude)) > 0 {
		data, err := p.mcpJSON()
		if err != nil {
			return err
		}
		if err := writeNewFileIn(p.worktree, filepath.Join(p.worktree, ".mcp.json"), data, 0o644); err != nil {
			return err
		}
	}
	if len(p.seatsOf(harnessCodex)) > 0 {
		dir := filepath.Join(p.worktree, ".codex")
		if err := makeRealDir(dir); err != nil {
			return err
		}
		if err := writeNewFileIn(p.worktree, filepath.Join(dir, "config.toml"), []byte(codexConfigTOML), 0o644); err != nil {
			return err
		}
	}

	// The same kind of directory the channel front makes for a Claude seat's
	// reply: 0700 under $TMPDIR, because Codex's workspace-write sandbox can
	// write $TMPDIR and, of the state base, only the table's worktree.
	dir, err := os.MkdirTemp("", replyDirPrefix(p.id))
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
	// What lets `table end` know the directory is this table's before it removes
	// it, and the seats the table was made with: a record alone could be edited
	// to name any directory or to change its seats.
	if err := writeNewFile(filepath.Join(dir, tableReplyMarker), []byte(marker), 0o600); err != nil {
		return err
	}
	r.replyDir = dir

	for _, s := range p.seatsOf(harnessCodex) {
		if err := r.startCodexSeat(ctx, s); err != nil {
			return err
		}
	}

	rec := tableRecord{
		ID: p.id, Stream: p.stream, Target: p.target, Worktree: p.worktree, ReplyDir: r.replyDir,
		Chair: p.chair, Rounds: p.rounds, Cap: p.cap, Created: time.Now().UTC().Format(time.RFC3339),
	}
	for _, s := range p.seats {
		rec.Seats = append(rec.Seats, seatRecord{Nick: s.nick, Harness: s.harness, Model: s.model, Thread: s.thread, ReplyFile: s.replyFile})
	}
	digest, err := writeTableRecord(p.tableDir, rec)
	if err != nil {
		return err
	}
	// The record is final, so the markers are: each gets the digest of the exact
	// bytes written, as a third line, and table.json is not written again. A
	// marker of two lines is a table new that never got here, and table end
	// refuses it.
	sealed := p.marker().sealed(digest)
	if err := sealMarker(p.worktree, "the worktree", filepath.Join(p.worktree, tableWorktreeMarker), sealed); err != nil {
		return err
	}
	if err := sealMarker(r.replyDir, "the reply directory", filepath.Join(r.replyDir, tableReplyMarker), sealed); err != nil {
		return err
	}
	return r.copySheet()
}

// mcpJSON is the worktree's .mcp.json: one server, parley, whose args are the
// channel's launch flags with the seat's nick left to the environment it is
// started in.
func (p *tablePlan) mcpJSON() ([]byte, error) {
	return mcpConfigJSON(p.self, []string{"channel", "--stream", p.stream, "--nick", "${PARLEY_NICK}", "--state-dir", p.base,
		"--skill", "table", "--chair", p.chair, "--cap", strconv.Itoa(p.cap),
		"--target", p.target, "--worktree", p.worktree})
}

// mcpConfigJSON is a worktree's .mcp.json for the one server, parley, run as
// command with args.
func mcpConfigJSON(command string, args []string) ([]byte, error) {
	type server struct {
		Command string   `json:"command"`
		Args    []string `json:"args"`
	}
	doc := struct {
		MCPServers map[string]server `json:"mcpServers"`
	}{map[string]server{"parley": {Command: command, Args: args}}}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("encode .mcp.json: %w", err)
	}
	return buf.Bytes(), nil
}

// writeNewFile creates path with data and fails if it is there: the worktree is
// fresh from a HEAD that tracks neither file, and a write over something would
// be a surprise.
func writeNewFile(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	return nil
}

// writeNewFileIn is writeNewFile for a file that has to land inside root: the
// directory it is made in, as the filesystem resolves it, must be root or under
// it, so that a link along the way cannot send the write elsewhere.
func writeNewFileIn(root, path string, data []byte, mode os.FileMode) error {
	if err := checkParentIn(root, "the worktree", path); err != nil {
		return err
	}
	return writeNewFile(path, data, mode)
}

// checkParentIn is the check writeNewFileIn makes: the directory path is in, as
// the filesystem resolves it, is root or under it. what names root in the refusal.
func checkParentIn(root, what, path string) error {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", root, err)
	}
	realParent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("resolve %s: %w", filepath.Dir(path), err)
	}
	if rel, err := filepath.Rel(realRoot, realParent); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("refusing to write %s: its directory resolves to %s, outside %s %s", path, realParent, what, realRoot)
	}
	return nil
}

// makeRealDir makes dir if it is not there, and accepts one that is a directory
// itself: a symlink to one is not, because what is written through it lands
// where it points.
func makeRealDir(dir string) error {
	fi, err := os.Lstat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := os.Mkdir(dir, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("look for %s: %w", dir, err)
	case !fi.IsDir():
		kind := "not a directory"
		if fi.Mode()&fs.ModeSymlink != 0 {
			kind = "a symlink"
		}
		return fmt.Errorf("%s is %s: refusing to write through it", dir, kind)
	}
	return nil
}

// startCodexSeat starts a seat's Codex session with its primer as the first
// prompt and records the thread it announces (see startCodexSession).
func (r *tableRun) startCodexSeat(ctx context.Context, s *seatPlan) error {
	p := r.plan
	s.replyFile = filepath.Join(r.replyDir, s.nick+".reply")
	text := primer.Render(primer.Params{
		Front:     primer.Codex,
		Command:   p.self,
		Spec:      p.stream,
		Nick:      s.nick,
		StateDir:  p.base,
		ReplyPath: s.replyFile,
		Skill:     primer.SkillTable,
		Chair:     p.chair,
		Target:    p.target,
		Worktree:  p.worktree,
	})
	thread, answer, err := startCodexSession(ctx, r, codexSession{
		noun: "seat", nick: s.nick, model: s.model, dir: p.worktree,
		bin: p.codexBin, name: p.codexName(), prompt: text, base: p.base, stream: p.stream,
	})
	if err != nil {
		return err
	}
	s.thread, s.answer = thread, answer
	return nil
}

func eventList(events []string) string {
	if len(events) == 0 {
		return "none"
	}
	return strings.Join(events, ", ")
}

// runCodexExec runs one `codex exec --json` turn and parses what it said, even
// when it did not end well: a turn that was killed has still announced the
// thread that has to be deleted. Stdin is /dev/null, because codex exec waits on
// it otherwise, and $CODEX_THREAD_ID is not passed on.
func runCodexExec(ctx context.Context, bin, dir string, args []string) (codex.Turn, string, error) {
	ctx, cancel := context.WithTimeout(ctx, seatTurnTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Stdin = nil
	cmd.Env = withoutThreadEnv()
	stdout, stderr := &limitBuffer{limit: maxExecOutput}, &limitBuffer{limit: 4096}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	// A descendant that inherited the pipes must not hold the run hostage.
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	turn := codex.ParseTurn(stdout.String())
	turn.Truncated = stdout.truncated
	if err != nil && ctx.Err() == context.DeadlineExceeded {
		err = fmt.Errorf("no end to the turn within %s: %w", seatTurnTimeout, err)
	}
	return turn, strings.TrimSpace(stderr.String()), err
}

// codexDelete removes a Codex thread this run created. --force is what keeps
// codex from asking, and the call has no stdin to answer with. The caller's
// cancellation is not honoured: an interrupted run is the one most likely to
// have a thread to delete. name is how the failure spells bin to the human, who
// is told the command to run by hand.
func codexDelete(bin, name, id string) error {
	ctx, cancel := context.WithTimeout(context.Background(), codexDeleteTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "delete", "--force", id)
	cmd.Stdin = nil
	cmd.Env = withoutThreadEnv()
	var out limitBuffer
	out.limit = 4096
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s delete --force %s: %w: %s (remove it yourself with: %s delete %s)", name, id, err, codex.PlainLine(firstLine(strings.TrimSpace(out.String()))), name, id)
	}
	return nil
}

// copySheet copies the sheet template the skill install links to the table's
// directory, where the chair fills it in. The first of the two skill roots that
// resolves wins; neither is not a failure, and the instructions say where to
// get it.
func (r *tableRun) copySheet() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	for _, root := range []string{".claude", ".agents"} {
		src := filepath.Join(home, root, "skills", "parley-table", tableSheetName)
		fi, err := os.Stat(src)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		data, err := os.ReadFile(src)
		if err != nil {
			return fmt.Errorf("read the sheet template %s: %w", src, err)
		}
		dst := filepath.Join(r.plan.tableDir, tableSheetName)
		if err := writeNewFile(dst, data, 0o644); err != nil {
			return err
		}
		r.sheet = dst
		r.logf("copied the sheet template %s", src)
		return nil
	}
	return nil
}

// q quotes a word for the shell the human pastes a printed command into.
func q(s string) string { return primer.ShellQuote(s) }

// channelNotice is the start of the dim notice Claude Code prints below the
// startup banner when a session has loaded the server as a development channel
// (https://code.claude.com/docs/en/channels-reference, "Test it"). /mcp shows the
// server connected on a launch without --dangerously-load-development-channels
// too, and a session that did not load it as a channel never hears the stream, so
// the notice is what the gate checks.
const channelNotice = "Channels (experimental) messages from server:parley inject directly in this session"

// claudeGateItem is the gate line for a Claude seat or peer (noun says which),
// shared by the table's gate and the build's.
func claudeGateItem(noun string) string {
	return fmt.Sprintf("every Claude %s's /mcp shows parley connected, and the notice below its startup banner reads\n"+
		"     %q; /mcp alone also passes on a launch\n"+
		"     without --dangerously-load-development-channels, which never hears the channel", noun, channelNotice)
}

// plainAnswer is a seat's answer made safe to print: every control character is
// gone but the line breaks, a tab is a space, and the Unicode line and paragraph
// separators are line breaks. It is a model's text, printed on the human's
// terminal.
func plainAnswer(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' || r == ' ' || r == ' ':
			b.WriteByte('\n')
		case r == '\t':
			b.WriteByte(' ')
		case unicode.IsControl(r):
		default:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

func indent(text, prefix string) string {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		if l = strings.TrimRight(l, " "); l != "" {
			lines[i] = prefix + l
		} else {
			lines[i] = ""
		}
	}
	return strings.Join(lines, "\n")
}

// runbook is what goes to stdout: what the human runs next, in order. Shell
// commands have every path, stream, thread id and model quoted; the lines meant
// for the IRC client are not, because a quote there would be part of the line.
func (p *tablePlan) runbook(r *tableRun) string {
	var b strings.Builder
	step := 0
	next := func(format string, a ...any) {
		step++
		fmt.Fprintf(&b, "\n%d. "+format+"\n\n", append([]any{step}, a...)...)
	}
	cmd := func(line string) { fmt.Fprintf(&b, "    %s\n", line) }

	fmt.Fprintf(&b, "Table %s is set up: worktree %s, %d seats, cap %d. What is left is yours, in this order;\n", p.id, p.worktree, len(p.seats), p.cap)
	fmt.Fprintf(&b, "indented lines starting with / go into your IRC client, the others into a shell.\n")

	if p.serverDown {
		server := q(p.self) + " server"
		if p.server != defaultListen {
			server += " --listen " + q(p.server)
		}
		next("Nothing answers at %s: start the server, in a terminal of its own.", p.server)
		cmd(server)
	}

	next("Join as the chair on %s, and check that your nick is registered, before any seat or pump connects.", p.server)
	cmd("/join #" + p.id)
	cmd("/msg NickServ INFO " + p.chair)
	b.WriteString("\n   If INFO says it is not registered, register it, and identify on every later connect:\n\n")
	cmd("/msg NickServ REGISTER <password>")
	b.WriteString("\n   An unregistered chair nick could be taken by a seat's process, and the gates would then treat\n   that seat's posts as the chair's.\n")

	codexName := p.codexName()
	pumpExtra := ""
	if p.codexGiven {
		pumpExtra = " --codex-bin " + q(p.codexBin)
	}
	for _, s := range p.seatsOf(harnessCodex) {
		model := ""
		if s.model != "" {
			model = " (model " + s.model + ")"
		}
		next("Codex seat %s%s. In one terminal start its pump, before TABLE:", s.nick, model)
		cmd(fmt.Sprintf("%s codex --stream %s --nick %s --state-dir %s --thread auto --skill table --chair %s --cap %d%s",
			q(p.self), q(p.stream), s.nick, q(p.base), p.chair, p.cap, pumpExtra))
		b.WriteString("\n   In another, resume its session, then type $parley-table:\n\n")
		cmd(fmt.Sprintf("%s resume %s -C %s -a on-request", codexName, q(s.thread), q(p.worktree)))
		fmt.Fprintf(&b, "\n   Seat %s's answer to its primer (codex exec, thread %s):\n\n", s.nick, s.thread)
		answer := plainAnswer(s.answer)
		if answer == "" {
			answer = "(none: the turn ended without a final message)"
		}
		b.WriteString(indent(answer, "        ") + "\n")
	}

	if claude := p.seatsOf(harnessClaude); len(claude) > 0 {
		next("Claude seats, one at a time. Start the first, approve the project server parley when it asks, check that /mcp shows it connected, then type /parley-table. Only then start the next: a second seat started before the first approved the server never loads it.")
		for _, s := range claude {
			line := fmt.Sprintf("cd %s && PARLEY_NICK=%s claude --dangerously-load-development-channels server:parley", q(p.worktree), s.nick)
			if s.model != "" {
				line += " --model " + q(s.model)
			}
			cmd(line)
		}
	}

	next("The gate. Do not post TABLE until every item holds:")
	items := []string{"every seat has said, at its own terminal, that it is waiting for the TABLE line. A seat that posts\n" +
		"     before TABLE, even a \"ready\", opens its own seal and loses its independent round 1; seats confirm at\n" +
		"     their terminals and never in the channel"}
	if len(p.seatsOf(harnessClaude)) > 0 {
		items = append(items, claudeGateItem("seat"))
	}
	if codexSeats := p.seatsOf(harnessCodex); len(codexSeats) > 0 {
		status := "each Codex seat's /status shows workspace-write and network on"
		var models []string
		for _, s := range codexSeats {
			if s.model != "" {
				models = append(models, s.nick+" "+s.model)
			}
		}
		if len(models) > 0 {
			status += fmt.Sprintf(", and its model (%s); the model check is unverified on a resumed session, so confirm it at this table", strings.Join(models, ", "))
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

	sheet := filepath.Join(p.tableDir, tableSheetName)
	if r.sheet != "" {
		next("Fill in the sheet, which names the seats, the question and its scope, at %s, then open the table with this one line in the channel:", sheet)
	} else {
		next("No sheet template was found at ~/.claude/skills/parley-table/sheet.md or ~/.agents/skills/parley-table/sheet.md (sh skills/install.sh from a parley checkout links them). Copy skills/parley-table/sheet.md from a parley checkout to %s, fill it in, then open the table with this one line in the channel:", sheet)
	}
	cmd("TABLE " + p.id + " " + sheet)
	fmt.Fprintf(&b, "\nWhen it is over: %s table end %s --state-dir %s\n", q(p.self), p.id, q(p.base))
	return b.String()
}
