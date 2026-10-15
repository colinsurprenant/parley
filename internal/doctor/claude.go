package doctor

import (
	"context"
	"fmt"
)

// claudeRemedy is what a person does about a Claude Code version that is not
// the one verified: nothing here can try it, so it is the next table or build
// that shows.
const claudeRemedy = "If a table or a build misbehaves, install Claude Code " + ClaudeVerified + " (" + ClaudeInstallURL + ")."

// claudeChannelFlag is the flag a Claude Code seat or peer is launched with to
// load parley's channel server.
const claudeChannelFlag = "--dangerously-load-development-channels"

// checkClaude checks the Claude Code binary and its version. That is all that
// can be checked without a session: the channel front is loaded with
// --dangerously-load-development-channels, which claude's --help does not list,
// so nothing short of starting a seat shows whether it still works, and the
// report on the binary's line says so. The version is the stand-in — the last
// one a table was run on is the one trusted.
func checkClaude(ctx context.Context, bin string) []Result {
	path, err := Resolve(bin)
	if err != nil {
		return []Result{{Fail, "claude", fmt.Sprintf("%v: install Claude Code (%s), or name it with --claude-bin", err, ClaudeInstallURL)}}
	}
	found := fmt.Sprintf("found %s; a launch needs %s, which doctor cannot check", path, claudeChannelFlag)
	return []Result{{OK, "claude", found}, checkVersion(ctx, "claude", path, ClaudeVerified, claudeRemedy)}
}
