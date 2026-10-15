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
// harnesses their seats or peers use; here it is every harness unless --harness
// names the ones this machine has. Without --live nothing costs anything: no model
// turn, and nothing written outside the system temp directory.
func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("parley doctor", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: parley doctor [--harness claude|codex]... [--live] [--server 127.0.0.1:6667] [--codex-bin codex] [--claude-bin claude] [--ergo-bin ergo]")
		fs.PrintDefaults()
	}
	var harnesses stringList
	fs.Var(&harnesses, "harness", "claude or codex, repeatable: check only the harnesses you use, their binary, version, skill links and, with --live, a Codex turn; without it both are checked")
	live := fs.Bool("live", false, "also try what costs something: one tiny Codex turn, whose thread is deleted afterwards, and a throwaway ergo on a free loopback port that two parley clients send a message through")
	server := fs.String("server", defaultListen, "the loopback IP:port of the running IRC server to ask for its capabilities")
	codexBin := fs.String("codex-bin", doctor.DefaultCodexBin, "the codex CLI to check, resolved on PATH unless it holds a separator")
	claudeBin := fs.String("claude-bin", doctor.DefaultClaudeBin, "the claude CLI to check, resolved on PATH unless it holds a separator")
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
	useClaude, useCodex, err := chosenHarnesses(harnesses)
	if err != nil {
		return err
	}
	addr, err := irc.LoopbackAddr(*server)
	if err != nil {
		return cfgErrf("--server %q: %v", *server, err)
	}
	for _, b := range []struct{ flag, value string }{{"codex-bin", *codexBin}, {"claude-bin", *claudeBin}, {"ergo-bin", *ergoBin}} {
		if b.value == "" {
			return cfgErrf("--%s must not be empty", b.flag)
		}
	}

	ctx, stop := signalContext()
	defer stop()
	results := doctor.Check(ctx, doctor.Options{
		Codex:     useCodex,
		Claude:    useClaude,
		CodexBin:  *codexBin,
		ClaudeBin: *claudeBin,
		ErgoBin:   *ergoBin,
		Server:    addr,
		Skills:    true,
		Live:      *live,
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

// chosenHarnesses is which harnesses `parley doctor` checks: the ones --harness
// names, and both when it names none.
func chosenHarnesses(named []string) (claude, codex bool, err error) {
	if len(named) == 0 {
		return true, true, nil
	}
	for _, h := range named {
		switch h {
		case harnessClaude:
			claude = true
		case harnessCodex:
			codex = true
		default:
			return false, false, cfgErrf("--harness %q: want %s or %s", h, harnessClaude, harnessCodex)
		}
	}
	return claude, codex, nil
}
