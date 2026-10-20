package doctor

import (
	"context"
	"fmt"
)

// openCodeRemedy and openCodeRemedy2 are what a person does about an OpenCode
// that is not the one verified for its major version: nothing here can try it,
// so it is the next session that shows.
const (
	openCodeRemedy  = "If an OpenCode session misbehaves, install OpenCode " + OpenCodeVerified + " (" + OpenCodeInstallURL + "), e.g. npm i -g opencode-ai@" + OpenCodeVerified + "."
	openCodeRemedy2 = "If an OpenCode session misbehaves, install OpenCode " + OpenCodeVerified2 + " (" + OpenCodeInstallURL + "). It was verified against a server in simulation mode, not yet against a live model."
)

// openCodeSurfaces are the flags and the subcommand of OpenCode 1's CLI that
// parley uses or tells a human to use, read from the 1.18.30 help. The default
// command is the TUI, started with --port so that it serves the HTTP API the
// front posts through, and with -s/--session to resume a session; run is how a
// session is started with its primer as the first message; session delete is
// how an abandoned one is removed, and session list --format json -n is how a
// session a stopped run made without naming it is found.
var openCodeSurfaces = []surface{
	{label: "opencode tui", flags: []string{"--port", "-s/--session"}},
	{cmd: []string{"run"}, flags: []string{"--format", "-m/--model", "--title"}},
	{cmd: []string{"session"}, subs: []string{"delete", "list"}},
	{cmd: []string{"session", "list"}, flags: []string{"--format", "-n/--max-count"}},
}

// openCode2Surfaces are OpenCode 2's, read from the 2.0.32 help. The default
// command has no --port: it and run reach one background service, and -s/--session
// is how the TUI continues a session, and how run continues one or creates it
// under the id it is given, which is how a session is started with its primer as
// the first message. A session is removed again with session delete, and nothing
// is searched for: the id is parley's own.
var openCode2Surfaces = []surface{
	{label: "opencode tui", flags: []string{"-s/--session"}},
	{cmd: []string{"run"}, flags: []string{"--format", "-m/--model", "--title", "-s/--session"}},
	{cmd: []string{"session"}, subs: []string{"delete", "list"}},
}

// OpenCodeVersion is the version number in what opencode --version printed:
// OpenCode 1 prints it bare, "1.18.30", and 2 as "opencode v2.0.32". It is empty
// if there is none.
func OpenCodeVersion(stdout string) string { return versionRE.FindString(stdout) }

// checkOpenCode is the static OpenCode check: the binary, its version, and the
// flags and subcommands of the commands parley uses, by the major version the
// binary says it is: 1.x and 2.x are different CLIs, and parley speaks to both.
// A major version before 1 or after 2 is a FAIL, since the pump refuses it. There is no live
// half: the front is verified against a server by hand. OpenCode creates its own
// empty data, cache, config and state directories on any invocation, --version
// and --help included.
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
	verified, remedy, surfaces := OpenCodeVerified, openCodeRemedy, openCodeSurfaces
	if have := OpenCodeVersion(stdout); have != "" {
		switch major := numericParts(have)[0]; {
		case major == 2:
			verified, remedy, surfaces = OpenCodeVerified2, openCodeRemedy2, openCode2Surfaces
		case major > 2 || major < 1:
			return append(rs, Result{Fail, "opencode version", fmt.Sprintf("OpenCode %s is not a major version of the 1.x and 2.x that parley speaks to (a later major version, or an earlier one): its pump refuses a server of any other, so a session on it could not be delivered into; install OpenCode 1.x or 2.x (%s)", have, OpenCodeInstallURL)})
		}
	}
	rs = append(rs, compareVersion("opencode", stdout, verified, remedy))
	for _, s := range surfaces {
		rs = append(rs, checkSurface(ctx, "opencode", path, s))
	}
	return rs
}
