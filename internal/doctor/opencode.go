package doctor

import (
	"context"
	"fmt"
)

// openCodeRemedy is what a person does about an OpenCode 1.x that is not the
// one verified: nothing here can try it, so it is the next session that shows.
const openCodeRemedy = "If an OpenCode session misbehaves, install OpenCode " + OpenCodeVerified + " (" + OpenCodeInstallURL + "), e.g. npm i -g opencode-ai@" + OpenCodeVerified + "."

// openCodeV2 is the report on an OpenCode 2. The front posts a delivery with
// OpenCode 1's prompt_async route, which OpenCode 2 does not have (read from
// the 2.0.32 source: its prompt route takes a delivery of queue or steer). The
// way out is spelled out: opencode-ai is OpenCode's npm package, and @1 picks
// the newest 1.x of it.
const openCodeV2 = "OpenCode 2 is not supported yet: parley's front speaks OpenCode 1's HTTP API (prompt_async); " +
	"install opencode 1.x, e.g. npm i -g opencode-ai@1"

// openCodeSurfaces are the flags and the subcommand of OpenCode's CLI that
// parley uses or tells a human to use, read from the 1.18.30 help. The default
// command is the TUI, started with --port so that it serves the HTTP API the
// front posts through, and with -s/--session to resume a session; run is how a
// session is started with its primer as the first message; session delete is
// how an abandoned one is removed.
var openCodeSurfaces = []surface{
	{label: "opencode tui", flags: []string{"--port", "-s/--session"}},
	{cmd: []string{"run"}, flags: []string{"--format", "-m/--model", "--title"}},
	{cmd: []string{"session"}, subs: []string{"delete"}},
}

// checkOpenCode is the static OpenCode check: the binary, its version, and the
// flags and subcommand of the commands parley uses. A 2.x is refused with the
// one line above and nothing else, since its CLI may differ in ways that would
// only bury it. There is no live half: the front is verified against a server by
// hand. OpenCode creates its own empty data, cache, config and state
// directories on any invocation, --version and --help included.
func checkOpenCode(ctx context.Context, bin string) []Result {
	path, err := Resolve(bin)
	if err != nil {
		return []Result{{Fail, "opencode", fmt.Sprintf("%v: install OpenCode (%s), or name it with --opencode-bin", err, OpenCodeInstallURL)}}
	}
	rs := []Result{{OK, "opencode", "found " + path}}
	stdout, stderr, err := probe(ctx, path, "--version")
	if err != nil {
		return append(rs, Result{Fail, "opencode version", fmt.Sprintf("opencode --version: %s", describe(err, stdout, stderr))})
	}
	if have := versionRE.FindString(stdout); have != "" && numericParts(have)[0] >= 2 {
		return append(rs, Result{Fail, "opencode version", openCodeV2 + " (found " + have + ")"})
	}
	rs = append(rs, compareVersion("opencode", stdout, OpenCodeVerified, openCodeRemedy))
	for _, s := range openCodeSurfaces {
		rs = append(rs, checkSurface(ctx, "opencode", path, s))
	}
	return rs
}
