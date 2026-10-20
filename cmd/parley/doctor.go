package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/colinsurprenant/parley/internal/doctor"
	"github.com/colinsurprenant/parley/internal/stream/irc"
)

// cmdDoctor checks every external surface parley depends on against this
// machine's current versions, and prints one line per check. The checks are
// internal/doctor's, which `parley table new` and `parley build new` run for the
// harnesses their seats or peers use; here it is Claude Code and Codex unless
// --harness names the ones this machine has, OpenCode among them: it is never
// part of the default, so a machine without it is not failed for lacking it.
// Without --live nothing costs anything: no model turn, and nothing written
// outside the system temp directory.
func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("parley doctor", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: parley doctor [--harness claude|codex|opencode]... [--live] [--server 127.0.0.1:6667] [--codex-bin codex] [--claude-bin claude] [--opencode-bin opencode] [--ergo-bin ergo]")
		fs.PrintDefaults()
	}
	var harnesses stringList
	fs.Var(&harnesses, "harness", "claude, codex or opencode, repeatable: check only the harnesses you use, their binary, version, skill links and, with --live, a Codex turn; without it claude and codex are checked, and opencode only when named")
	live := fs.Bool("live", false, "also try what costs something: one tiny Codex turn, whose thread is deleted afterwards, and a throwaway ergo on a free loopback port that two parley clients send a message through")
	server := fs.String("server", defaultListen, "the loopback IP:port of the running IRC server to ask for its capabilities")
	codexBin := fs.String("codex-bin", doctor.DefaultCodexBin, "the codex CLI to check, resolved on PATH unless it holds a separator")
	claudeBin := fs.String("claude-bin", doctor.DefaultClaudeBin, "the claude CLI to check, resolved on PATH unless it holds a separator")
	openCodeBin := fs.String("opencode-bin", doctor.DefaultOpenCodeBin, "the opencode CLI to check, resolved on PATH unless it holds a separator")
	ergoBin := fs.String("ergo-bin", doctor.DefaultErgoBin, "the ergo binary to check, resolved on PATH unless it holds a separator")
	if err := parseFlags(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return &configError{err: err}
	}
	if fs.NArg() > 0 {
		return cfgErrf("unexpected argument %q", fs.Arg(0))
	}
	use, err := chosenHarnesses(harnesses)
	if err != nil {
		return err
	}
	addr, err := irc.LoopbackAddr(*server)
	if err != nil {
		return cfgErrf("--server %q: %v", *server, err)
	}
	for _, b := range []struct{ flag, value string }{{"codex-bin", *codexBin}, {"claude-bin", *claudeBin}, {"opencode-bin", *openCodeBin}, {"ergo-bin", *ergoBin}} {
		if b.value == "" {
			return cfgErrf("--%s must not be empty", b.flag)
		}
	}

	ctx, stop := signalContext()
	defer stop()
	results := doctor.Check(ctx, doctor.Options{
		Codex:       use.codex,
		Claude:      use.claude,
		OpenCode:    use.openCode,
		CodexBin:    *codexBin,
		ClaudeBin:   *claudeBin,
		OpenCodeBin: *openCodeBin,
		ErgoBin:     *ergoBin,
		Server:      addr,
		Skills:      true,
		Live:        *live,
	})
	for _, r := range results {
		if _, err := fmt.Fprintln(os.Stdout, r); err != nil {
			return fmt.Errorf("write the report: %w", err)
		}
	}
	if ctx.Err() != nil {
		return errors.New("interrupted")
	}
	failed := 0
	for _, r := range results {
		if r.Level == doctor.Fail {
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d checks failed", failed, len(results))
	}
	return nil
}

// harnessSet is which harnesses `parley doctor` checks.
type harnessSet struct{ claude, codex, openCode bool }

// chosenHarnesses is which harnesses `parley doctor` checks: the ones --harness
// names, and Claude Code and Codex when it names none. OpenCode is only ever
// named: adding it to the default would fail every machine that has the other
// two and not it.
func chosenHarnesses(named []string) (harnessSet, error) {
	if len(named) == 0 {
		return harnessSet{claude: true, codex: true}, nil
	}
	var use harnessSet
	for _, h := range named {
		switch h {
		case harnessClaude:
			use.claude = true
		case harnessCodex:
			use.codex = true
		case "opencode":
			use.openCode = true
		default:
			return harnessSet{}, cfgErrf("--harness %q: want %s, %s or opencode", h, harnessClaude, harnessCodex)
		}
	}
	return use, nil
}
