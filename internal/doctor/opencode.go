package doctor

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/colinsurprenant/parley/internal/codex"
	"github.com/colinsurprenant/parley/internal/opencode"
)

// openCodeRemedy is what a person is told about an OpenCode 2 that warns: one
// older than the one verified, or equal in number with a different suffix (any
// other major version never gets here, it is a FAIL in checkOpenCode). Nothing here
// can try it, so it is the next session that shows; and it is not a reason to
// install an exact version: a newer 2.x is expected to work, and the report says
// which one parley was last verified against.
const openCodeRemedy = "Parley was " + OpenCodeVerifiedNote + "; a newer 2.x is expected to work, and an older one may lack something it needs (" + OpenCodeInstallURL + "). If an OpenCode session misbehaves, note the OpenCode version it ran."

// openCodeSurfaces are the flags and subcommands of OpenCode's CLI that parley
// uses or tells a human to use, read from the help of the version last verified.
// The default command is the TUI, which reaches the background service, and
// -s/--session is how it continues a session; run is how a session is started with
// its primer as the first message, under the id it is given with -s/--session
// (which creates the session if it does not exist); session delete is how it is
// removed again, and session list is how a human finds an id. Nothing is searched
// for: the id is parley's own.
var openCodeSurfaces = []surface{
	{label: "opencode tui", flags: []string{"-s/--session"}},
	{cmd: []string{"run"}, flags: []string{"--format", "-m/--model", "--title", "-s/--session"}},
	{cmd: []string{"session"}, subs: []string{"delete", "list"}},
}

// OpenCodeVersion is the version number in what opencode --version printed:
// "opencode v2.0.32" (a bare "1.18.30", as the legacy 1.x prints it, is read too, so
// that it can be refused). It is empty if there is none.
func OpenCodeVersion(stdout string) string { return versionRE.FindString(stdout) }

// checkOpenCode is the static OpenCode check: the binary, its version, and the
// flags and subcommands of the commands parley uses. Only a 2 is spoken to: the
// legacy 1.x has no API parley can use, and is a FAIL that says where OpenCode 2
// comes from, as is any other major version, since the pump refuses a server of it.
// There is no live half: the front is verified against a server by hand. OpenCode
// creates its own empty data, cache, config and state directories on any
// invocation, --version and --help included.
func checkOpenCode(ctx context.Context, bin string) []Result {
	path, err := Resolve(bin)
	if err != nil {
		return []Result{{Fail, "opencode", fmt.Sprintf("%v: install OpenCode 2 (%s), or name it with --opencode-bin", err, OpenCodeInstallURL)}}
	}
	rs := []Result{{OK, "opencode", "found " + path}}
	stdout, stderr, err := probe(ctx, path, "--version")
	if err != nil {
		return append(rs, Result{Fail, "opencode version", fmt.Sprintf("opencode --version: %s", describe(err, stdout, stderr))})
	}
	have := OpenCodeVersion(stdout)
	switch major := numericParts(have)[0]; {
	case have == "":
		// A build that does not say what it is cannot be told to be OpenCode 2, and
		// the pump refuses a server whose version it cannot read.
		return append(rs, Result{Fail, "opencode version", fmt.Sprintf("opencode --version printed %s, which holds no version parley can read, so it cannot be told to be OpenCode 2, the only major version its pump speaks to; %s", printed(stdout), opencode.InstallHint)})
	case major == 1:
		return append(rs, Result{Fail, "opencode version", fmt.Sprintf("OpenCode %s is the legacy 1.x, which has no API parley can speak to; %s", have, opencode.InstallHint)})
	case major != 2:
		return append(rs, Result{Fail, "opencode version", fmt.Sprintf("OpenCode %s is not a major version parley speaks to (OpenCode 2): its pump refuses a server of any other, so a session on it could not be delivered into; %s", have, opencode.InstallHint)})
	}
	rs = append(rs, compareVersion("opencode", stdout, OpenCodeVerified, openCodeRemedy))
	for _, s := range openCodeSurfaces {
		rs = append(rs, checkSurface(ctx, "opencode", path, s))
	}
	return rs
}

// printed is the first line of what a binary printed, made safe for a report line
// and bounded, quoted.
func printed(stdout string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(stdout), "\n")
	line = codex.PlainLine(line)
	const limit = 80
	if r := []rune(line); len(r) > limit {
		return strconv.Quote(string(r[:limit])) + "…"
	}
	return strconv.Quote(line)
}
