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
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/colinsurprenant/parley/internal/channel"
	"github.com/colinsurprenant/parley/internal/doctor"
	"github.com/colinsurprenant/parley/internal/opencode"
	"github.com/colinsurprenant/parley/internal/primer"
	"github.com/colinsurprenant/parley/internal/state"
	"github.com/colinsurprenant/parley/internal/stream"
)

// codexNetworkNotice is said where `line add` writes a Codex config, and in the
// docs: the file turns network access on for Codex in that checkout.
const codexNetworkNotice = "network access is ON for Codex in this checkout: the config.toml this line wrote sets network_access = true under workspace-write, " +
	"so that parley send can reach the loopback IRC server from inside the sandbox. It applies to every Codex session in the checkout, not only this one."

// cmdLineAdd puts one session on an existing line: its launch file in its
// checkout (an OpenCode session has none), for a Codex or an OpenCode session its
// reply directory and its thread or session, started on the line's primer and
// recorded for `parley codex --thread auto` or `parley opencode --session auto`.
// It prints what is left for that session. Everything it made is taken down again
// if a step fails or the run is interrupted; the Codex thread or OpenCode
// session itself is deleted by the rollback, and by nothing else.
func cmdLineAdd(args []string) error {
	fs := flag.NewFlagSet("parley line add", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), lineUsageAdd)
		fs.PrintDefaults()
	}
	nick := fs.String("nick", "", "required: the session's nick; it must differ from the chair's and from every other session's, without regard to ASCII case")
	harness := fs.String("harness", "", "required: claude, codex or opencode (OpenCode 1.x or 2.x)")
	model := fs.String("model", "", "the model, at most 128 bytes of letters, digits and . _ - : / @ [ ] +; passed to codex exec -m for a Codex session, to opencode run -m (provider/model) for an OpenCode one, and printed as --model on a Claude one's launch line")
	checkout := fs.String("checkout", "", "required: the absolute path of the session's own checkout, an existing directory; a Claude session gets .mcp.json there, a Codex session .codex/config.toml, unless this line already wrote that file for another session of the checkout; an OpenCode session gets no file there")
	stateDir := fs.String("state-dir", "", "the state directory the line was set up under (default $XDG_STATE_HOME/parley)")
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return &configError{err: err}
	}
	switch {
	case len(pos) == 0:
		return cfgErrf("the line id is required: parley line add <id> --nick <nick> --harness claude|codex|opencode --checkout <absolute dir>")
	case len(pos) > 1:
		return cfgErrf("unexpected argument %q", pos[1])
	}
	p, err := planLineAdd(lineAddInput{
		id: pos[0], nick: *nick, nickGiven: given(fs, "nick"), harness: *harness, harnessGiven: given(fs, "harness"),
		model: *model, modelGiven: given(fs, "model"), checkout: *checkout, checkoutGiven: given(fs, "checkout"),
		stateDir: *stateDir,
	})
	if err != nil {
		return err
	}
	// A line that is not there is the invocation's mistake and takes no lock.
	if !lineDirIsThere(p.base, p.id) {
		return noLineErr(p.base, p.id)
	}

	ctx, stop := signalContext()
	defer stop()
	// The id's lock comes before the record is read, and is held to the end: a
	// line verb for this id, or a table or build one, is refused while this one
	// runs, so the record read below is the one that is changed.
	unlock, err := lockID(p.base, p.id)
	if err != nil {
		return err
	}
	defer unlock()
	if err := p.bind(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "parley line add: checking the machine (parley doctor's static checks)\n")
	if err := p.checkMachine(ctx, os.Stderr); err != nil {
		// A machine that cannot host the session fails before anything is made,
		// exit 1, as `table new` and `line new` fail on doctor's FAIL.
		return err
	}
	if err := p.measureClaude(); err != nil {
		return err
	}

	run := &lineAddRun{lineRun: lineRun{log: os.Stderr, verb: "line add"}, plan: p}
	err = run.setUp(ctx)
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		if ctx.Err() != nil {
			err = fmt.Errorf("interrupted: %w", err)
		}
		run.rollback()
	}
	// The nick's lock is held through the rollback, and no longer. Its lock file
	// stays, as a pump's does: see lockNick.
	run.release()
	if err != nil {
		return err
	}
	signal.Ignore(syscall.SIGPIPE)
	if _, err := io.WriteString(os.Stdout, p.runbook(run)); err != nil {
		return fmt.Errorf("session %s is added but its instructions could not be written (%v): parley line drop %s %s takes it off", p.nick, err, p.id, p.nick)
	}
	return nil
}

// lineAddInput is what planLineAdd validates: the flags as given.
type lineAddInput struct {
	id            string
	nick          string
	nickGiven     bool
	harness       string
	harnessGiven  bool
	model         string
	modelGiven    bool
	checkout      string
	checkoutGiven bool
	stateDir      string
}

// linePlan is everything `line add` has validated; the part that needs the
// record is bind's, made under the id's lock.
type linePlan struct {
	id, nick, harness, model string
	checkout                 string
	base, self, tmp          string

	rec      lineRecord
	recBytes []byte
	window   string
	// group is the line's sessions that already use the launch file this session
	// would have: the same harness in the same checkout.
	group []lineSession
	// reuse is whether that file is there with the digest the record holds, so
	// this session shares it and nothing is written.
	reuse bool
	// data is the launch file to write when there is none to share.
	data []byte
	// codexBin is the resolved codex a Codex session runs; codexGiven whether the
	// line was made with --codex-bin, in which case the lines printed carry it.
	// openCodeBin and openCodeGiven are the same for an OpenCode session and
	// --opencode-bin.
	codexBin      string
	codexGiven    bool
	openCodeBin   string
	openCodeGiven bool
	serverDown    bool
}

// planLineAdd validates everything that needs no look at the line's record. Each
// refusal is a configuration error, exit 2, and nothing has been created.
func planLineAdd(in lineAddInput) (*linePlan, error) {
	if err := checkLineID(in.id); err != nil {
		return nil, err
	}
	p := &linePlan{id: in.id}
	if !in.nickGiven || in.nick == "" {
		return nil, cfgErrf("--nick is required: the session's nick")
	}
	if err := stream.ValidNick(in.nick); err != nil {
		return nil, cfgErrf("--nick %q %v", in.nick, err)
	}
	p.nick = in.nick

	if !in.harnessGiven {
		return nil, cfgErrf("--harness is required: claude, codex or opencode")
	}
	switch in.harness {
	case harnessClaude, harnessCodex, harnessOpenCode:
	default:
		return nil, cfgErrf("--harness %q: the harnesses are claude, codex and opencode", in.harness)
	}
	p.harness = in.harness

	if in.modelGiven {
		if in.model == "" {
			return nil, cfgErrf("--model is empty: name a model, or leave the flag out")
		}
		if err := checkModel("--model", in.model, in.model); err != nil {
			return nil, err
		}
		// opencode run -m takes provider/model; anything else would make the session
		// and then fail its turn.
		if in.harness == harnessOpenCode {
			if provider, model, ok := strings.Cut(in.model, "/"); !ok || provider == "" || model == "" {
				return nil, cfgErrf("--model %q: an OpenCode model is provider/model, as `opencode models` lists them", in.model)
			}
		}
		p.model = in.model
	}

	if !in.checkoutGiven || in.checkout == "" {
		return nil, cfgErrf("--checkout is required: the absolute path of the session's own checkout")
	}
	if err := checkUTF8(pathField{"--checkout", in.checkout}); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(in.checkout) {
		return nil, cfgErrf("--checkout %q must be an absolute path", in.checkout)
	}
	if strings.Contains(in.checkout, "$") {
		return nil, cfgErrf("--checkout %q holds a '$': it is printed into commands, and a shell would expand it", in.checkout)
	}
	if hasControl(in.checkout) {
		return nil, cfgErrf("--checkout %q holds a control character: it is printed into commands and into the text the session reads", in.checkout)
	}
	if fi, err := os.Stat(in.checkout); err != nil {
		return nil, cfgErrf("--checkout %q: %v", in.checkout, err)
	} else if !fi.IsDir() {
		return nil, cfgErrf("--checkout %q is not a directory", in.checkout)
	}
	// The checkout is resolved once, here, and the resolved path is the only one
	// used from now on, recorded and printed: two spellings of one directory (a
	// symlinked parent, /tmp and /private/tmp) would otherwise be two checkouts to
	// the record's text and one to its sharing, and the record would not hold. The
	// path is resolved as spelled, not cleaned first: a ".." after a link climbs
	// from where the link leads, as the Stat above did, and cleaning it away first
	// would land on a different directory than the one checked.
	resolved, err := filepath.EvalSymlinks(in.checkout)
	if err != nil {
		return nil, cfgErrf("--checkout %q: %v", in.checkout, err)
	}
	if resolved != filepath.Clean(in.checkout) {
		// What a link led to is held to what was asked of the spelling that led there.
		if err := checkUTF8(pathField{"--checkout, resolved,", resolved}); err != nil {
			return nil, err
		}
		if strings.Contains(resolved, "$") {
			return nil, cfgErrf("--checkout %q resolves to %q, which holds a '$': it is printed into commands, and a shell would expand it", in.checkout, resolved)
		}
		if hasControl(resolved) {
			return nil, cfgErrf("--checkout %q resolves to %q, which holds a control character: it is printed into commands and into the text the session reads", in.checkout, resolved)
		}
	}
	p.checkout = resolved

	base, err := state.ResolveBase(in.stateDir)
	if err != nil {
		return nil, cfgErrf("--state-dir: %v", err)
	}
	p.base = base
	p.self = selfPath()
	if !filepath.IsAbs(p.self) {
		return nil, cfgErrf("cannot find this binary's absolute path (got %q): the .mcp.json and every command printed name it", p.self)
	}
	if p.tmp, err = filepath.Abs(os.TempDir()); err != nil {
		return nil, cfgErrf("resolve the temp directory %q: %v", os.TempDir(), err)
	}
	// Every path that line.json or a launch file will name must survive
	// encoding/json as it is, so each is checked before anything is made.
	if err := checkUTF8(pathField{"the state directory (--state-dir, or $XDG_STATE_HOME, or $HOME)", p.base},
		pathField{"this binary's path", p.self},
		pathField{"the temp directory ($TMPDIR), which the reply directory is made under,", p.tmp}); err != nil {
		return nil, err
	}
	for _, x := range []struct{ name, path string }{{"the state directory", p.base}, {"this binary's path", p.self}} {
		if hasControl(x.path) {
			return nil, cfgErrf("%s %q holds a control character: it is printed into commands and into the text the session reads", x.name, x.path)
		}
		// A Claude session's .mcp.json carries both, and Claude Code expands ${...}
		// in a .mcp.json's command and args.
		if p.harness == harnessClaude && strings.Contains(x.path, "$") {
			return nil, cfgErrf("%s %q holds a '$': Claude Code expands ${...} in a .mcp.json's command and args, so the path would not reach the channel as written", x.name, x.path)
		}
	}
	// The state directory holds every nick's IRC password, and the reply directory
	// is a Codex or an OpenCode session's scratch: neither belongs in a checkout,
	// where git status shows them and a careless add commits them.
	if within(p.checkout, p.base) {
		return nil, cfgErrf("the state directory %s is inside the checkout %s: it holds the IRC passwords, so name a --state-dir outside it", p.base, p.checkout)
	}
	if p.harness != harnessClaude && within(p.checkout, p.tmp) {
		return nil, cfgErrf("the temp directory ($TMPDIR) %s is inside the checkout %s: the reply directory is made in it, so run with a TMPDIR outside it", p.tmp, p.checkout)
	}
	return p, nil
}

// bind reads the line's record, under the id's lock, and checks the session
// against it: the nick, the launch file in the checkout (an OpenCode session has
// none), the Claude launch and the Codex or OpenCode binary. It sets what the
// machine check and the run need.
func (p *linePlan) bind() error {
	rec, raw, err := readLineRecord(p.base, p.id)
	if err != nil {
		return err
	}
	p.rec, p.recBytes = rec, raw
	if stream.NickEqual(p.nick, rec.Chair) {
		return cfgErrf("--nick %q is the chair's (%q): the chair is another voice than any session, and nicks are the same without regard to ASCII case", p.nick, rec.Chair)
	}
	if i, ok := rec.session(p.nick); ok {
		return cfgErrf("--nick %q is already session %q of line %q, and nicks are the same without regard to ASCII case: parley line drop %s %s takes it off first", p.nick, rec.Sessions[i].Nick, p.id, p.id, rec.Sessions[i].Nick)
	}
	p.window = rec.Window
	p.group = rec.sharing(p.harness, p.checkout, -1)

	if p.harness == harnessCodex {
		p.codexGiven = rec.CodexBin != ""
		p.codexBin = rec.CodexBin
		if p.codexBin == "" {
			if p.codexBin, err = doctor.Resolve(codexDefault); err != nil {
				return cfgErrf("codex: %v: install it, or make the line with `parley line new --codex-bin <path>`", err)
			}
		}
		if err := checkUTF8(pathField{"the codex binary's path", p.codexBin}); err != nil {
			return err
		}
		tr, err := state.OpenThreadRecord(p.base, p.nick, rec.Stream)
		if err != nil {
			return cfgErrf("--nick %q: %v", p.nick, err)
		}
		if _, err := os.Lstat(tr.Path()); err == nil {
			return cfgErrf("--nick %q already has a Codex thread record for this line's stream, %s: a session that is not in line.json made it (a drop that did not finish, or a pump of your own); remove it, or give this session another nick", p.nick, tr.Path())
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("look for %s: %w", tr.Path(), err)
		}
	}

	if p.harness == harnessOpenCode {
		p.openCodeGiven = rec.OpenCodeBin != ""
		p.openCodeBin = rec.OpenCodeBin
		if p.openCodeBin == "" {
			if p.openCodeBin, err = doctor.Resolve(doctor.DefaultOpenCodeBin); err != nil {
				return cfgErrf("opencode: %v: %s, or make the line with `parley line new --opencode-bin <path>`", err, opencode.InstallHint)
			}
		}
		if err := checkUTF8(pathField{"the opencode binary's path", p.openCodeBin}); err != nil {
			return err
		}
		sr, err := state.OpenSessionRecord(p.base, p.nick, rec.Stream)
		if err != nil {
			return cfgErrf("--nick %q: %v", p.nick, err)
		}
		if _, err := os.Lstat(sr.Path()); err == nil {
			return cfgErrf("--nick %q already has an OpenCode session record for this line's stream, %s: a session that is not in line.json made it (a drop that did not finish, or a pump of your own); remove it, or give this session another nick", p.nick, sr.Path())
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("look for %s: %w", sr.Path(), err)
		}
		// No launch file: nothing is looked for in the checkout, and nothing will
		// be written there.
		return nil
	}

	if err := p.checkLaunchFile(); err != nil {
		return err
	}
	if p.harness == harnessClaude && !p.reuse {
		if p.data, err = p.mcpJSON(); err != nil {
			return err
		}
	}
	return nil
}

// codexDefault is the codex a line that was made without --codex-bin runs.
const codexDefault = "codex"

// checkLaunchFile holds the launch file in the checkout to what the line may
// write over, which is nothing: the file is not there, or it is the one the line
// wrote for another session of the checkout and still holds the digest the record
// has. Anything else is the user's, and is never overwritten, shared or followed.
func (p *linePlan) checkLaunchFile() error {
	path, _ := launchFileOf(p.harness, p.checkout)
	if p.harness == harnessCodex {
		dir := filepath.Dir(path)
		if fi, err := os.Lstat(dir); err == nil && !fi.IsDir() {
			kind := "not a directory"
			if fi.Mode()&fs.ModeSymlink != 0 {
				kind = "a symlink"
			}
			return cfgErrf("%s is %s: a Codex session's config goes in a real .codex directory, and nothing is written through a link", dir, kind)
		} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return cfgErrf("look for %s: %v", dir, err)
		}
	}
	fi, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// Absent from the checkout is not enough: a file git still tracks there,
		// moved aside by the user, would come back as a tracked modification,
		// which no exclude line can hide and a commit could carry.
		rel, rerr := filepath.Rel(p.checkout, path)
		if rerr != nil {
			return cfgErrf("place %s in %s: %v", path, p.checkout, rerr)
		}
		if tracked(p.checkout, rel) {
			return cfgErrf("%s is tracked by git in %s, though it is not there now: a file parley wrote at that path would show as a change to it and could be committed; set this session up by hand as docs/line.md describes, or use another checkout", rel, p.checkout)
		}
		return nil
	case err != nil:
		return cfgErrf("look for %s: %v", path, err)
	}
	if len(p.group) == 0 {
		return cfgErrf("%s exists and is not one this line wrote (the record of line %q holds no digest for it): it is yours, so it is not touched; move it aside, or set this session up by hand as docs/line.md describes", path, p.id)
	}
	want := p.group[0].Files[0].SHA256
	if !fi.Mode().IsRegular() {
		return cfgErrf("%s is not a regular file, and it is the file this line wrote for session %s of this checkout: it is not shared or touched", path, p.group[0].Nick)
	}
	got, err := fileDigest(path)
	if err != nil {
		return cfgErrf("read %s: %v", path, err)
	}
	if got != want {
		return cfgErrf("%s is not the file this line wrote for session %s of this checkout: its digest is %s and the record has %s, so it was edited or replaced; it is not shared or touched", path, p.group[0].Nick, got, want)
	}
	p.reuse = true
	return nil
}

// fileMax bounds what is read to take a digest of a launch file: the ones the
// line writes are a few hundred bytes.
const fileMax = 1 << 20

// fileDigest is the digest of the exact bytes of the regular file at path, which
// is not opened if it is a link. A file past fileMax is not one the line wrote.
func fileDigest(path string) (string, error) {
	f, err := state.OpenRegular(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, fileMax+1))
	if err != nil {
		return "", err
	}
	if len(b) > fileMax {
		return "", fmt.Errorf("%s is larger than %d bytes", path, fileMax)
	}
	return tableDigest(b), nil
}

// mcpJSON is the checkout's .mcp.json: one server, parley, whose args are the
// channel's launch flags with the session's nick left to the environment it is
// started in.
func (p *linePlan) mcpJSON() ([]byte, error) {
	return mcpConfigJSON(p.self, []string{"channel", "--stream", p.rec.Stream, "--nick", "${PARLEY_NICK}", "--state-dir", p.base,
		"--skill", "line", "--chair", p.rec.Chair, "--cap", strconv.Itoa(p.rec.Cap), "--window", p.window})
}

// checkMachine runs doctor's static checks for the session's harness, the server
// and the parley-line skill under the root that harness reads; a FAIL ends the
// run before any change. A server that is not answering is remembered only to be
// reported by `line new`; here it is a warning.
func (p *linePlan) checkMachine(ctx context.Context, w io.Writer) error {
	down, err := checkMachineWith(ctx, w, "parley line add", doctor.Options{
		Codex:       p.harness == harnessCodex,
		Claude:      p.harness == harnessClaude,
		OpenCode:    p.harness == harnessOpenCode,
		CodexBin:    p.codexBin,
		OpenCodeBin: p.openCodeBin,
		Server:      p.rec.Server,
		LineSkill:   true,
	})
	p.serverDown = down
	return err
}

// measureClaude builds a Claude session's instructions in this process, the way
// `parley channel --check` does, and refuses a launch that would be cut at Claude
// Code's cap.
func (p *linePlan) measureClaude() error {
	if p.harness != harnessClaude {
		return nil
	}
	return measureClaude("session", "--state-dir, --stream or the path of the parley binary", channel.Config{
		Spec:   p.rec.Stream,
		Skill:  primer.SkillLine,
		Chair:  p.rec.Chair,
		Cap:    p.rec.Cap,
		Window: p.rec.windowDuration(),
	}, p.nick, p.base, []stream.Option{stream.WithLine(p.rec.Chair, p.rec.Cap, p.rec.windowDuration())})
}

// lineAddRun is one `line add` in progress.
type lineAddRun struct {
	lineRun
	plan *linePlan
	// file is the launch file entry the session is recorded with; wrote is whether
	// this run wrote the file, and excludeNote what it has to say of the exclude.
	file        lineFile
	wrote       bool
	excludeNote string
	// thread is the Codex thread or the OpenCode session id; port is the loopback
	// port an OpenCode 1 session's runbook names for its TUI and its pump, and
	// openCode2 is whether the session is OpenCode 2's, which has no port.
	thread    string
	answer    string
	replyDir  string
	replyFile string
	port      int
	openCode2 bool
}

// lockNick takes the session's nick lock without waiting, before anything is
// made: a nick that a running pump or Claude Code session holds would make a
// printed launch fail after Codex turns were spent. The directory this makes is
// registered for the rollback; the lock file is not, and no parley code unlinks
// one: a lock is held on the file's inode, so once the path is unlinked another
// process can create a new file there and lock it while the old inode is still
// locked. A refused or failed run therefore leaves <base>/<nick>/lock behind, as
// a pump does.
func (r *lineAddRun) lockNick() error {
	p := r.plan
	release, err := lockNicksMaking(p.base, []string{p.nick}, r.mkdirAll, func(nick, dir string) error {
		return fmt.Errorf("session %q is in use by a running pump or Claude Code session: its lock under %s is held; stop it, or give this session another nick", nick, dir)
	})
	if err != nil {
		return err
	}
	r.unlock = release
	return nil
}

// setUp adds the session, step by step; any error is the caller's cue to roll
// back, and then to release.
func (r *lineAddRun) setUp(ctx context.Context) error {
	p := r.plan
	if err := r.lockNick(); err != nil {
		return err
	}
	if hasLaunchFile(p.harness) {
		if err := r.writeLaunchFile(ctx); err != nil {
			return err
		}
	}
	switch p.harness {
	case harnessCodex:
		if err := r.startCodex(ctx); err != nil {
			return err
		}
	case harnessOpenCode:
		if err := r.startOpenCode(ctx); err != nil {
			return err
		}
	}
	return r.record()
}

// writeLaunchFile makes the session's launch file in the checkout, unless this
// line already wrote it for another session there, and puts its line in the
// checkout's exclude file when it wrote it.
func (r *lineAddRun) writeLaunchFile(ctx context.Context) error {
	p := r.plan
	path, excludeLine := launchFileOf(p.harness, p.checkout)
	if len(p.group) > 0 {
		r.file = p.group[0].Files[0]
	}
	if p.reuse {
		r.logf("sharing %s with the other session of this checkout", path)
		return nil
	}

	// The checkout was resolved when planning began, and the doctor checks ran
	// since: a checkout moved and replaced by a link meanwhile would put the file
	// in another directory, which teardown then refuses. Look again just before
	// anything is written.
	if moved := checkoutMoved(p.checkout); moved != "" {
		return fmt.Errorf("session %s: %s; nothing was written there", p.nick, moved)
	}
	var data []byte
	if p.harness == harnessClaude {
		data = p.data
	} else {
		var err error
		if data, err = lineCodexConfig(p.id); err != nil {
			return err
		}
		dir := filepath.Dir(path)
		_, lerr := os.Lstat(dir)
		made := errors.Is(lerr, fs.ErrNotExist)
		if err := makeRealDir(dir); err != nil {
			return err
		}
		if made {
			r.file.MadeDir = true
			r.push("directory "+dir, func() error {
				if moved := checkoutMoved(p.checkout); moved != "" {
					return leftAlone(moved)
				}
				return removeEmptyDir(dir)
			})
		}
	}
	if err := writeNewFileWithin(p.checkout, "the checkout", path, data, 0o644); err != nil {
		return err
	}
	digest := tableDigest(data)
	r.push("launch file "+path, func() error { return removeIfDigest(path, digest, p.checkout) })
	r.file.Path, r.file.SHA256 = path, digest
	r.wrote = true
	r.logf("wrote %s", path)

	file, note := excludeFileOf(ctx, p.checkout)
	if file == "" {
		r.excludeNote = note
		return nil
	}
	res, note, err := appendExcludeLine(file, excludeLine)
	switch {
	case err != nil:
		return err
	case note != "":
		r.excludeNote = note
	case res.added:
		// The file's creation is the line's whichever session made it: a Codex
		// session appending to the file a Claude session of this line created
		// shares the duty to remove it once empty, so teardown in any order leaves
		// no file behind. Rollback undoes only this append, so it keeps res.made.
		made := res.made || p.lineMadeExclude(file)
		r.file.ExcludeFile, r.file.ExcludeSep, r.file.ExcludeMade = file, res.sep, made
		r.push("exclude line "+excludeLine+" in "+file, func() error {
			_, err := removeExcludeLine(file, excludeLine, res.sep, res.made)
			return err
		})
		r.logf("added %s to %s", excludeLine, file)
	default:
		// The line is there. When another session of this line put it there (a
		// worktree of the same repository shares the exclude file), it is this
		// session's to take out too, once nothing else needs it.
		if owner, ok := p.excludeOwner(file, excludeLine); ok {
			r.file.ExcludeFile, r.file.ExcludeSep, r.file.ExcludeMade = file, owner.ExcludeSep, owner.ExcludeMade || p.lineMadeExclude(file)
		}
	}
	return nil
}

// lineMadeExclude reports whether a session of the line, of either harness,
// recorded creating the exclude file.
func (p *linePlan) lineMadeExclude(excludeFile string) bool {
	for _, s := range p.rec.Sessions {
		if len(s.Files) == 1 && s.Files[0].ExcludeMade && s.Files[0].ExcludeFile != "" &&
			resolveExisting(s.Files[0].ExcludeFile) == resolveExisting(excludeFile) {
			return true
		}
	}
	return false
}

// excludeOwner finds a session of the line that put the line in the exclude file:
// one whose launch file is of this harness's kind (it has the same line) and
// records that exclude file.
func (p *linePlan) excludeOwner(excludeFile, line string) (lineFile, bool) {
	for _, s := range p.rec.Sessions {
		_, l := launchFileOf(s.Harness, s.Checkout)
		if l != line || len(s.Files) != 1 || s.Files[0].ExcludeFile == "" {
			continue
		}
		if resolveExisting(s.Files[0].ExcludeFile) == resolveExisting(excludeFile) {
			return s.Files[0], true
		}
	}
	return lineFile{}, false
}

// lineCodexConfig is the Codex config a line writes: the constant body `table
// new` writes, behind a first comment line that names the line and carries a
// nonce. The body is a constant, so a copy of it that anyone else made would hash
// to the same digest; the nonce is what makes the digest a proof that this run
// wrote the file, and the record keeps the digest of the whole.
func lineCodexConfig(id string) ([]byte, error) {
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, fmt.Errorf("make a nonce for the Codex config: %w", err)
	}
	return []byte(fmt.Sprintf("# written by parley line add for line %s (%s)\n%s", id, hex.EncodeToString(nonce[:]), codexConfigTOML)), nil
}

// removeIfDigest removes the file at path if it is a regular file whose bytes
// hash to digest, which is the proof it is the one this run wrote; anything else
// there is left alone, and said.
func removeIfDigest(path, digest, checkout string) error {
	// The checkout was resolved when the run began; a path above it that became a
	// link since, or a checkout moved away and replaced, puts the name on another
	// directory, and a file there is not shown to be this run's.
	if moved := checkoutMoved(checkout); moved != "" {
		return leftAlone(moved)
	}
	// The same holds for the directory the file is in, .codex for a Codex
	// config: one replaced by a link since it was written points the name
	// outside the checkout.
	if dir := filepath.Dir(path); filepath.Clean(dir) != filepath.Clean(checkout) {
		if di, err := os.Lstat(dir); err == nil && di.Mode()&fs.ModeSymlink != 0 {
			return leftAlone(dir + " is a symlink now")
		}
	}
	li, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return errNothingToUndo
	case err != nil:
		return err
	case !li.Mode().IsRegular():
		return leftAlone("it is no longer a regular file")
	}
	got, err := fileDigest(path)
	if err != nil {
		return leftAlone(err.Error())
	}
	if got != digest {
		return leftAlone("it no longer holds what this run wrote")
	}
	return os.Remove(path)
}

// makeReplyDir makes the session's reply directory, the one its reply file goes
// in, and registers its removal.
func (r *lineAddRun) makeReplyDir() error {
	p := r.plan
	// The same kind of directory the channel front makes for a Claude session's
	// reply: 0700 under $TMPDIR, because Codex's workspace-write sandbox can write
	// $TMPDIR and, of the state base, nothing. For an OpenCode session it is a
	// place under $TMPDIR of the session's own, which OpenCode asks about before
	// the first write (external_directory) and the human answers in the TUI.
	dir, err := os.MkdirTemp("", lineReplyPrefix(p.id))
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
	// What lets `line drop` know the directory is this session's before it removes
	// it: the line's id and the session's nick.
	if err := writeNewFile(filepath.Join(dir, lineReplyMarker), []byte(lineReplyMarkerContent(p.id, p.nick)), 0o600); err != nil {
		return err
	}
	r.replyDir = dir
	r.replyFile = filepath.Join(dir, p.nick+".reply")
	return nil
}

// startCodex makes the session's reply directory and starts its Codex
// session on the line's primer, recording the thread for --thread auto.
func (r *lineAddRun) startCodex(ctx context.Context) error {
	p := r.plan
	if err := r.makeReplyDir(); err != nil {
		return err
	}

	text := primer.Render(primer.Params{
		Front:     primer.Codex,
		Command:   p.self,
		Spec:      p.rec.Stream,
		Nick:      p.nick,
		StateDir:  p.base,
		ReplyPath: r.replyFile,
		Skill:     primer.SkillLine,
		Chair:     p.rec.Chair,
		Cap:       p.rec.Cap,
		Window:    p.rec.windowDuration(),
	})
	thread, answer, err := startCodexSession(ctx, r, codexSession{
		noun: "session", nick: p.nick, model: p.model, dir: p.checkout,
		bin: p.codexBin, name: p.codexName(), prompt: text, base: p.base, stream: p.rec.Stream,
		// The primer turn answers a text and posts nothing; in the user's checkout
		// it has no business writing.
		sandbox: "read-only",
		// A line's checkout may be a directory that is no git checkout.
		skipGitRepoCheck: true,
	})
	if err != nil {
		return err
	}
	r.thread, r.answer = thread, answer
	return nil
}

// startOpenCode makes the session's reply directory and starts its OpenCode
// session on the line's primer, recording the session for --session auto, and
// for OpenCode 1 picks the loopback port its runbook names. Which OpenCode it is
// comes from the binary: 1.x prints a bare version and 2.x "opencode v2.0.32",
// and the two start a session differently (a 2.x run creates the session under
// the id parley gives it) and are reached differently (a 2.x has no port).
func (r *lineAddRun) startOpenCode(ctx context.Context) error {
	p := r.plan
	version, err := openCodeVersion(ctx, p.openCodeBin, p.checkout)
	if err != nil {
		return err
	}
	r.openCode2 = version.major == 2
	if err := r.makeReplyDir(); err != nil {
		return err
	}
	text := primer.Render(primer.Params{
		Front:     primer.OpenCode,
		Command:   p.self,
		Spec:      p.rec.Stream,
		Nick:      p.nick,
		StateDir:  p.base,
		ReplyPath: r.replyFile,
		Skill:     primer.SkillLine,
		Chair:     p.rec.Chair,
		Cap:       p.rec.Cap,
		Window:    p.rec.windowDuration(),
	})
	// The title carries the random string of the reply directory, which is this
	// run's own: two adds of one nick on lines of one id under different state
	// directories give their sessions different titles, so a session found by its
	// title is this run's.
	token := strings.TrimPrefix(filepath.Base(r.replyDir), lineReplyPrefix(p.id))
	session := openCodeSession{
		nick: p.nick, model: p.model, dir: p.checkout,
		bin: p.openCodeBin, name: p.openCodeName(), title: fmt.Sprintf("parley line %s %s %s", p.id, p.nick, token),
		prompt: text, base: p.base, stream: p.rec.Stream,
	}
	if r.openCode2 {
		if session.sid, err = opencode.NewSessionID(); err != nil {
			return err
		}
	}
	id, answer, err := startOpenCodeSession(ctx, r, session)
	if err != nil {
		return err
	}
	r.thread, r.answer = id, answer
	if r.openCode2 {
		return nil
	}
	// Picked after the turn, which is the long step, so that it is as fresh as it
	// can be when the human uses it. Nothing holds the port: it is free now, and
	// the TUI says if it is not any more.
	if r.port, err = freeLoopbackPort(); err != nil {
		return err
	}
	return nil
}

// freeLoopbackPort is a port nothing listens on at 127.0.0.1 now: the OS is asked
// for one by listening on port 0, and the listener is closed again.
func freeLoopbackPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("find a free loopback port for the OpenCode TUI: %w", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// lineReplyMarkerContent is what a Codex or OpenCode session's reply directory
// holds in its marker: the line's id, then the session's nick, each ending in a
// newline.
func lineReplyMarkerContent(id, nick string) string { return id + "\n" + nick + "\n" }

// codexName is how the lines printed for the human, and the rollback's own
// account of what it ran, spell the codex this line uses: the one --codex-bin
// named when the line was made, shell-quoted, else plain codex.
func (p *linePlan) codexName() string {
	if p.codexGiven {
		return q(p.codexBin)
	}
	return codexDefault
}

// openCodeName is codexName's counterpart for the opencode --opencode-bin names.
func (p *linePlan) openCodeName() string {
	if p.openCodeGiven {
		return q(p.openCodeBin)
	}
	return doctor.DefaultOpenCodeBin
}

// record adds the session to line.json, through a temp file and a rename, after
// every other step; the previous record is put back by the rollback.
func (r *lineAddRun) record() error {
	p := r.plan
	rec := p.rec
	rec.Sessions = append([]lineSession(nil), rec.Sessions...)
	// A launch file rewritten for a checkout whose sessions had lost it has a new
	// digest, which every session of the checkout holds.
	if r.wrote {
		for i, s := range rec.Sessions {
			if s.Harness == p.harness && sameDir(s.Checkout, p.checkout) {
				rec.Sessions[i].Files = []lineFile{r.file}
			}
		}
	}
	// An OpenCode session has no launch file, and a record that says so with an
	// empty list.
	files := []lineFile{}
	if hasLaunchFile(p.harness) {
		files = []lineFile{r.file}
	}
	rec.Sessions = append(rec.Sessions, lineSession{
		Nick: p.nick, Harness: p.harness, Model: p.model, Checkout: p.checkout,
		Thread: r.thread, ReplyDir: r.replyDir, Files: files,
	})
	dir := lineDirOf(p.base, p.id)
	// The write goes through a temp file and a rename, so a failure leaves the old
	// record whole and there is nothing to put back; once it has succeeded, an
	// interrupt is the only thing left that can undo it.
	if _, err := writeLineRecord(dir, rec); err != nil {
		return err
	}
	r.push("line record "+filepath.Join(dir, lineRecordName), func() error {
		return writeFileAtomic(filepath.Join(dir, lineRecordName), p.recBytes, 0o600)
	})
	return nil
}

// runbook is what goes to stdout: what the human runs next, in order. Shell
// commands have every path, stream, thread id and model quoted.
func (p *linePlan) runbook(r *lineAddRun) string {
	var b strings.Builder
	step := 0
	next := func(format string, a ...any) {
		step++
		fmt.Fprintf(&b, "\n%d. "+format+"\n\n", append([]any{step}, a...)...)
	}
	cmd := func(line string) { fmt.Fprintf(&b, "    %s\n", line) }

	file, _ := launchFileOf(p.harness, p.checkout)
	fmt.Fprintf(&b, "Session %s (%s) is added to line %s, checkout %s.\n", p.nick, p.harness, p.id, p.checkout)
	switch {
	case !hasLaunchFile(p.harness):
		b.WriteString("Wrote nothing in the checkout.\n")
	case r.wrote:
		fmt.Fprintf(&b, "Wrote %s.", file)
		if r.file.ExcludeFile != "" {
			_, line := launchFileOf(p.harness, p.checkout)
			fmt.Fprintf(&b, " Added %s to %s, to keep it out of git status.", line, r.file.ExcludeFile)
		}
		b.WriteString("\n")
	default:
		fmt.Fprintf(&b, "Shares %s, which this line wrote for another session of the checkout.\n", file)
	}
	if r.excludeNote != "" {
		fmt.Fprintf(&b, "Note: %s.\n", r.excludeNote)
	}

	if p.harness == harnessClaude {
		launch := "PARLEY_NICK=" + p.nick + " claude --dangerously-load-development-channels server:parley"
		if p.model != "" {
			launch += " --model " + q(p.model)
		}
		b.WriteString("What is left is yours, in this order:\n")
		next("Launch the session from its checkout, and approve the project server parley when it asks.")
		cmd("cd " + q(p.checkout))
		cmd(launch)
		b.WriteString("\n   A conversation that is already under way joins the line with the same launch plus --resume <session-id>,\n   and keeps its history:\n\n")
		cmd(launch + " --resume <session-id>")
		next("Check that /mcp shows parley connected, and that the notice below the startup banner reads\n   %q;\n   /mcp alone also passes on a launch without --dangerously-load-development-channels, which never hears the channel.\n   Then type /parley-line.", channelNotice)
		return b.String()
	}

	if p.harness == harnessOpenCode {
		b.WriteString(p.openCodeRunbook(r))
		return b.String()
	}

	fmt.Fprintf(&b, "Codex thread %s.\n\n", r.thread)
	fmt.Fprintf(&b, "Network: %s\n", codexNetworkNotice)
	pumpExtra := ""
	if p.codexGiven {
		pumpExtra = " --codex-bin " + q(p.codexBin)
	}
	b.WriteString("\nWhat is left is yours, in this order:\n")
	next("In one terminal start its pump, before the session posts:")
	cmd(fmt.Sprintf("%s codex --stream %s --nick %s --state-dir %s --thread auto --skill line --chair %s --cap %d --window %s%s",
		q(p.self), q(p.rec.Stream), p.nick, q(p.base), p.rec.Chair, p.rec.Cap, p.window, pumpExtra))
	next("In another, resume its session, then type $parley-line:")
	cmd(fmt.Sprintf("%s resume %s -C %s -a on-request", p.codexName(), q(r.thread), q(p.checkout)))
	b.WriteString("\n   Check that /status shows workspace-write and network on")
	if p.model != "" {
		fmt.Fprintf(&b, ", and its model (%s); the model check is unverified on a resumed session, so confirm it here", p.model)
	}
	b.WriteString(".\n")
	fmt.Fprintf(&b, "\n   Session %s's answer to its primer (codex exec, thread %s):\n\n", p.nick, r.thread)
	answer := plainAnswer(r.answer)
	if answer == "" {
		answer = "(none: the turn ended without a final message)"
	}
	b.WriteString(indent(answer, "        ") + "\n")
	return b.String()
}

// openCodeRunbook is the part of the runbook that is an OpenCode session's: the
// TUI first, since the pump probes it as it starts, then the pump, then what to
// check and what to know. The port is the same in both commands, and is on the
// command line and nowhere else: it is not kept, and the pump takes it from the
// human.
func (p *linePlan) openCodeRunbook(r *lineAddRun) string {
	if r.openCode2 {
		return p.openCodeRunbook2(r)
	}
	var b strings.Builder
	step := 0
	next := func(format string, a ...any) {
		step++
		fmt.Fprintf(&b, "\n%d. "+format+"\n\n", append([]any{step}, a...)...)
	}
	cmd := func(line string) { fmt.Fprintf(&b, "    %s\n", line) }

	fmt.Fprintf(&b, "OpenCode session %s.\n", r.thread)
	b.WriteString("\nWhat is left is yours, in this order:\n")
	next("In one terminal, start OpenCode on its session, in its checkout. It comes first: the pump checks as it starts that the\n   TUI's server, which is there only while the TUI runs with --port, answers and knows the session. The port was free\n   when this was made; if the TUI says it is taken, use another, in both commands.")
	cmd("cd " + q(p.checkout))
	cmd(fmt.Sprintf("%s --port %d --session %s", p.openCodeName(), r.port, q(r.thread)))
	next("In another, start its pump:")
	cmd(fmt.Sprintf("%s opencode --stream %s --nick %s --session auto --port %d --skill line --chair %s --cap %d --window %s --state-dir %s",
		q(p.self), q(p.rec.Stream), p.nick, r.port, p.rec.Chair, p.rec.Cap, p.window, q(p.base)))
	next("Check that the session has loaded the parley-line skill. OpenCode loads a skill with its skill tool and has no /parley-line\n   command; if its answer to the primer below does not show it did, ask it to load the skill.")
	fmt.Fprintf(&b, "Notes:\n")
	fmt.Fprintf(&b, "  The reply file, %s, is outside the session's checkout, so each write of it raises OpenCode's external_directory ask in the TUI:\n", q(r.replyFile))
	b.WriteString("  answer \"Allow always\" for the reply directory, since \"Allow once\" asks again on every reply.\n")
	b.WriteString("  The session was made by opencode run, which denies it OpenCode's question tool and plan mode for that session (a known limit of creating it that way).\n")
	b.WriteString("  OPENCODE_SERVER_PASSWORD, if you use one, must be set for both commands, and OPENCODE_SERVER_USERNAME too if it is not opencode.\n")
	b.WriteString("  Without it the server on --port answers any local process.\n")
	fmt.Fprintf(&b, "\n   Session %s's answer to its primer (opencode run, session %s):\n\n", p.nick, r.thread)
	answer := plainAnswer(r.answer)
	if answer == "" {
		answer = "(none: the turn ended without a final message)"
	}
	b.WriteString(indent(answer, "        ") + "\n")
	return b.String()
}

// openCodeRunbook2 is openCodeRunbook for an OpenCode 2 session. OpenCode 2 has
// no --port: its TUI, run and session commands reach one background service per
// user, which the pump finds from the file the service writes. So the TUI
// still comes first, as it is what starts the service, but there is no port to
// share between the two commands and no password to carry: the service's own is
// in that file.
func (p *linePlan) openCodeRunbook2(r *lineAddRun) string {
	var b strings.Builder
	step := 0
	next := func(format string, a ...any) {
		step++
		fmt.Fprintf(&b, "\n%d. "+format+"\n\n", append([]any{step}, a...)...)
	}
	cmd := func(line string) { fmt.Fprintf(&b, "    %s\n", line) }

	fmt.Fprintf(&b, "OpenCode 2 session %s.\n", r.thread)
	b.WriteString("\nWhat is left is yours, in this order:\n")
	next("In one terminal, start OpenCode on its session, in its checkout. It comes first: it starts OpenCode 2's background\n   service if none runs, and the pump checks as it starts that the service answers and knows the session. There is no port\n   to choose: the pump finds the service from the file the service writes, service.json in $XDG_STATE_HOME/opencode\n   (~/.local/state/opencode if that is not set), so both commands need the same HOME and XDG_STATE_HOME.")
	cmd("cd " + q(p.checkout))
	cmd(fmt.Sprintf("%s -s %s", p.openCodeName(), q(r.thread)))
	next("In another, start its pump:")
	cmd(fmt.Sprintf("%s opencode --stream %s --nick %s --session auto --skill line --chair %s --cap %d --window %s --state-dir %s",
		q(p.self), q(p.rec.Stream), p.nick, p.rec.Chair, p.rec.Cap, p.window, q(p.base)))
	next("Check that the session has loaded the parley-line skill. OpenCode loads a skill with its skill tool and has no /parley-line\n   command; if its answer to the primer below does not show it did, ask it to load the skill.")
	fmt.Fprintf(&b, "Notes:\n")
	fmt.Fprintf(&b, "  The reply file, %s, is outside the session's checkout. OpenCode 2 allows external_directory by default,\n", q(r.replyFile))
	b.WriteString("  so writing it asks nothing. To be asked, as OpenCode 1 asks, add this rule to your opencode.jsonc and answer for the\n  reply directory when it asks:\n")
	b.WriteString("    { \"permissions\": [ { \"action\": \"external_directory\", \"resource\": \"*\", \"effect\": \"ask\" } ] }\n")
	b.WriteString("  The session was made by opencode run --session, which adds no permission rules of its own to the session. A run has no one\n")
	b.WriteString("  to ask, so during the primer turn it rejected any permission ask and cancelled any question; from here the session's agent\n  and your configuration decide.\n")
	b.WriteString("  The service's password is in its service file (mode 600) and the pump reads it from there and sends it to the service's\n")
	b.WriteString("  loopback address only; it is never printed. For an opencode serve of your own, run the pump with --port <n> and\n")
	b.WriteString("  OPENCODE_PASSWORD (or OPENCODE_SERVER_PASSWORD) set to the password it was started with.\n")
	b.WriteString("  OpenCode 2 support is verified against 2.0.32 in OpenCode's simulation mode, which blocks all model traffic, and\n  not yet on a live model.\n")
	fmt.Fprintf(&b, "\n   Session %s's answer to its primer (opencode run, session %s):\n\n", p.nick, r.thread)
	answer := plainAnswer(r.answer)
	if answer == "" {
		answer = "(none: the turn ended without a final message)"
	}
	b.WriteString(indent(answer, "        ") + "\n")
	return b.String()
}

// tracked reports whether git's index in dir holds rel. A directory outside any
// repository, or a git that cannot answer, holds nothing: such a checkout has no
// index a written file could become a change to.
func tracked(dir, rel string) bool {
	args := append(append([]string(nil), wtGitFlags...), "ls-files", "--", filepath.ToSlash(rel))
	out, _, err := runGit(context.Background(), dir, gitEnv(), args, args)
	return err == nil && strings.TrimSpace(out) != ""
}
