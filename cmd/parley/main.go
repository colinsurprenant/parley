// Command parley posts messages to a peer chat stream and blocks for new ones.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/colinsurprenant/parley/internal/channel"
	"github.com/colinsurprenant/parley/internal/codex"
	"github.com/colinsurprenant/parley/internal/primer"
	"github.com/colinsurprenant/parley/internal/state"
	"github.com/colinsurprenant/parley/internal/stream"
	"github.com/colinsurprenant/parley/internal/stream/file"
	"github.com/colinsurprenant/parley/internal/stream/irc"
)

const (
	exitOK      = 0
	exitRuntime = 1
	exitConfig  = 2
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		usage(os.Stderr)
		return exitConfig
	}
	var err error
	switch args[0] {
	case "-h", "-help", "--help", "help":
		usage(os.Stdout)
		return exitOK
	case "send":
		err = cmdSend(args[1:])
	case "wait":
		err = cmdWait(args[1:])
	case "channel":
		err = cmdChannel(args[1:])
	case "codex":
		err = cmdCodex(args[1:])
	case "primer":
		err = cmdPrimer(args[1:])
	case "server":
		err = cmdServer(args[1:])
	case "doctor":
		err = cmdDoctor(args[1:])
	case "version", "-version", "--version":
		err = cmdVersion(args[1:])
	case "table":
		err = cmdTable(args[1:])
	case "build":
		err = cmdBuild(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "parley: unknown verb %q\n\n", args[0])
		usage(os.Stderr)
		return exitConfig
	}
	if err == nil {
		return exitOK
	}
	fmt.Fprintf(os.Stderr, "parley: %v\n", err)
	var cfg *configError
	if errors.As(err, &cfg) {
		return exitConfig
	}
	// A verb that runs another program for as long as it lives ends with that
	// program's status.
	var st *exitStatus
	if errors.As(err, &st) {
		return st.code
	}
	return exitRuntime
}

func usage(w io.Writer) {
	fmt.Fprint(w, `parley — peer chat between coding agents

usage:
  parley send    --stream <spec> --nick <nick> [--body-file <path>] [--state-dir <dir>] [--] [<body>]
  parley wait    --stream <spec> --nick <nick> [--timeout 30s] [--debounce 250ms] [--state-dir <dir>]
  parley channel --stream <spec> --nick <nick> --chair <nick> --target <dir> --worktree <dir> [--debounce 250ms]
                 [--state-dir <dir>] [--skill build|table --cap <n>] [--check]
  parley channel --stream <spec> --nick <nick> --skill line --chair <nick> --cap <n> [--window 10m]
                 [--debounce 250ms] [--state-dir <dir>] [--check]
  parley codex   --stream <spec> --nick <nick> --thread <thread>|auto [--debounce 250ms] [--state-dir <dir>] [--codex-bin codex]
                 [--skill build|table|line --chair <nick> --cap <n> [--window 10m]]
  parley primer  --front claude|codex --stream <spec> --nick <nick> --chair <nick> --target <dir> --worktree <dir>
                 [--reply-file <path>] [--state-dir <dir>] [--command <spelling>] [--reply-command <cmd>]
                 [--skill build|table]
  parley primer  --front claude|codex --stream <spec> --nick <nick> --skill line --chair <nick> --cap <n>
                 [--window 10m] [--reply-file <path>] [--state-dir <dir>] [--command <spelling>] [--reply-command <cmd>]
  parley server  [--listen 127.0.0.1:6667] [--datastore <dir>] [--ergo-bin ergo]
  parley doctor  [--harness claude|codex]... [--live] [--server 127.0.0.1:6667] [--codex-bin codex] [--claude-bin claude] [--ergo-bin ergo]
  parley table   new <id> --target <repo> --chair <nick> --rounds <n> --seat <nick>=<claude|codex>[:<model>] [--seat ...]
                 [--worktree <dir>] [--server 127.0.0.1:6667] [--state-dir <dir>] [--codex-bin codex]
  parley table   end <id> [--force] [--state-dir <dir>]
  parley build   new <id> --target <repo> --chair <nick> --peer <nick>=<claude|codex>[:<model>] --peer <nick>=<claude|codex>[:<model>]
                 [--server 127.0.0.1:6667] [--state-dir <dir>] [--codex-bin codex] [--writable-root <abs dir>]...
  parley build   end <id> [--force] [--state-dir <dir>]
  parley version

verbs:
  send     post one message; the body is the <body> argument, or the whole of
           --body-file <path>, or stdin when neither is given — a body that
           never becomes a shell word is the safe way to send peer prose. On
           success it prints one line, "[parley] sent id=<id> ts=<ts>", the id
           and ts the post has in the stream, spelled as another participant's
           header spells them; a field the transport did not report is "-". When
           $CODEX_THREAD_ID is set — a Codex session's shell — and the stream has
           a Codex thread record (see codex), a thread that differs from it
           gets one warning on stderr, that channel posts will not reach this
           session, and the post is sent regardless
  wait     block until peer messages arrive, print them with the parley peer
           header, then exit; nothing to deliver prints nothing and exits 0
  channel  run as an MCP stdio channel server, pushing every delivery into a
           live Claude Code session; JSON-RPC on stdout, logs on stderr. With
           --check it serves nothing: it validates the flags, builds the
           instructions the session would send, prints their length as one
           line, "instructions: <n> of 2048 UTF-16 units", and exits 0 if they
           are within Claude Code's cap and 2 if over. The length is taken at
           the longest reply directory name a launch can draw, so within the
           cap means the launch fits. It opens no stream and takes no lock
  codex    run in the foreground and deliver into a live Codex CLI thread, one
           "codex queue --thread <thread> --message <text>" per delivery, or
           one per message when the delivery is over the argument bound; the
           peer header is the only mark Codex gets, so it is passed unchanged.
           --thread is a session UUID or an exact session name, or auto: the
           thread id that "parley table new" or "parley build new" recorded for
           this nick and stream,
           in the codex-thread file beside the cursor. auto is reserved, so a
           session literally named auto is passed by its UUID; with no record
           it exits 2
  primer   print the framing a front gives the model — the reply command first,
           then what a delivery is and what it is worth — for a front whose
           harness has none to hand over, so the text is the session's first
           message: a human pastes it in, or the session is started with it as
           its first prompt, as "parley table new" starts a Codex seat and
           "parley build new" a Codex peer.
           --front claude is what "parley channel" sends at connect time;
           --front codex adds what is true of that front.
           It opens nothing: no stream, no state directory, no lock, and no IRC
           account, and every value is printed as it was spelled. A live channel
           prints the canonical spec (irc: gains its '#', file: becomes
           absolute), the state directory resolved to an absolute path and a
           reply file of its own in a private directory, so a primer run with
           the raw spellings differs in those values and in its length.
           --reply-command <cmd> replaces the whole send line with a command of
           your own — a wrapper script holding the run's fixed arguments, where
           an approval rule has to name one command and no suffix; codex only
  server   run ergo, the per-machine IRC server every table shares, in the
           foreground: one per machine, each table on a channel of its own. It
           renders the configuration built into parley into --datastore
           (default $XDG_DATA_HOME/parley-ergo, else ~/.local/share/parley-ergo,
           which is also used while it exists and the XDG one does not; made
           0700 if missing and otherwise left as it is) and runs ergo there,
           so the accounts in that directory carry from one run to the next.
           --listen must be a loopback IP and port (a name such as localhost is
           refused): plaintext, and nothing outside
           this machine can reach it; anything else exits 2, as does an ergo
           that cannot be found. ergo's output passes through, an interrupt or
           a terminate signal is passed on to it, and a hangup (the terminal
           closing) is passed on as a terminate, because ergo takes a hangup
           for a rehash; parley exits with ergo's status, 0 after it was
           stopped by the first signal passed on. One line on stderr at the
           start names the address and the datastore. ergo locks the datastore
           (ircd.lock) while it runs, so a second server on one datastore, or
           on a port already taken, exits 1 after ergo's own words, and parley
           adds one line naming the datastore or the address and pgrep -l
           ergo; a lock left by a killed ergo does not block the next start
  doctor   check every external surface parley depends on against this machine's
           current versions, one line each: "ok", "warn" or "FAIL", the surface,
           and the detail; exit 1 if any FAIL, else 0. Static by default, which
           costs nothing: the codex CLI's version against the last verified one
           and every flag of its commands that parley uses, the claude CLI's
           version, ergo and its version against the one the built-in
           configuration was written for, whether the server at --server offers
           every IRC capability parley requires, with PLAIN among the
           mechanisms if its sasl lists any (a server that is not up is a
           warn), and the skill links skills/install.sh makes. --harness
           claude|codex, repeatable, narrows the binary, version, skill-link
           and --live checks to the harnesses you have, both without it; the
           server and ergo are checked either way. A binary that is missing is
           a FAIL that links the project's install page; a version other than
           the last verified one is a warn that says it may work as it is, and
           what to install if a table or a build misbehaves. The claude line
           says a launch needs --dangerously-load-development-channels, which
           doctor cannot check. --live also
           spends one tiny Codex turn (none if codex delete has no --force to
           clean it up without a prompt), checks the thread id it announces
           against the one the model reads in its shell (a turn that reports an
           error or a failed turn, has no turn.completed event, or whose output
           passed 1 MiB is a FAIL whatever codex exits with), and
           deletes the thread; and runs a throwaway ergo on a free loopback
           port that two parley clients send a message through
  table    set a table up, or take it down. new does everything about a table
           that is not a decision, and prints what is left for the human to
           run, in order, every path and id quoted for a shell. It checks its
           flags first (the id is [a-z0-9][a-z0-9-]{0,31}, the channel #<id>
           and a directory name; the target is the root of a git checkout whose
           HEAD tracks no .mcp.json of any kind, no .codex that is not a
           directory, no .codex/config.toml and no .parley-table; the seats'
           nicks are valid, distinct from each other and from the chair, none
           of them "tables" or "builds"; no opencode seats yet; a model
           of at most 128 bytes of letters, digits and . _ - : / @ [ ] + ;
           no '$' in the target, the worktree or
           this binary's path; no whitespace in the state directory, which
           holds the sheet the TABLE line names; neither the state directory
           nor $TMPDIR inside the target or the worktree) and runs parley
           doctor's static checks for the seats' harnesses, the server, ergo
           when nothing answers at the server, and the parley-table skill under
           the root each harness reads (Claude Code ~/.claude/skills, Codex
           ~/.agents/skills), a FAIL ending it before any change. Then it takes
           the id's lock (<state base>/.ids/<id>.lock, which table new and end
           and build new and end all take, first, and hold to the end of the
           run): a second command for the id refuses at once, exit 1, and does
           not wait. Under it an id that is live as a table or a build is
           refused, exit 2. Then it makes a detached
           worktree of the target's HEAD at --worktree (default <state
           base>/tables/<id>/worktree); a .parley-table in it, for every
           table, holding the table's id on its first line and the seats'
           nicks (exactly as given, sorted, space-separated) on its second, written
           before any Codex turn runs; a .mcp.json in it for the Claude seats, whose args are the
           channel's launch flags with --state-dir and a --cap of rounds times
           seats, each Claude seat's instructions measured against Claude
           Code's cap the way channel --check does; a
           .codex/config.toml in it for the Codex seats (workspace-write,
           on-request, network on); and a private reply directory under
           $TMPDIR. Each Codex seat is started by codex exec --json with its
           table primer as the prompt, stdin closed, network off for that one
           turn and a 5-minute limit; it counts as started only if its turn
           emitted turn.completed in output not cut off at 1 MiB, and the
           thread id it announces is recorded in the codex-thread file
           --thread auto reads. A table.json larger than the 64 KiB end reads
           is refused before it is written, with the run rolled back. The table's
           record, tables/<id>/table.json under the state base, names the
           paths and threads and holds no stream content, and the sheet
           template the skills link is copied beside it. The output lists the
           server (if nothing answers), the chair's join and NickServ steps,
           each Codex seat's pump line, its codex resume line and its answer
           to the primer, each Claude seat's launch line, the gate, and the
           TABLE line. new takes every seat's nick lock before it makes
           anything and holds them until it is done, so a nick that a running
           pump or Claude Code session holds is refused, exit 1, naming it,
           with nothing changed. If any step fails, everything the run made is
           taken down again, the Codex threads it started included, and the
           exit is 1; the seats' lock files and the id's stay, as a pump's do,
           since no parley code unlinks one. end first takes the id's lock,
           then reads the markers it can and
           refuses, exit 1, force or not, a table.json that is not the one
           the table was made with, before it takes a lock or removes anything:
           they hold the table's id, its seats and the digest of the record's
           exact bytes, so an edit to any field, a byte included, is caught,
           and so is a marker without its digest.
           Then it takes every seat's lock and holds them to the end,
           refusing, naming the seat, while one is held. It removes the seats'
           thread records first, then the worktree, the reply directory and
           the table directory, and only what it can show is the table's, --force
           or not: the worktree if git lists it and its .parley-table holds
           the table's id, seats and digest, a regular file and not a link
           (when git cannot say because the target is gone, if it is under the
           table's directory; any other failure of git refuses, quoting it), the
           reply directory if it is directly in $TMPDIR, named for the table and
           holds the marker new wrote;
           any other path in the record it refuses, exit 1, naming it. It
           removes a seat's thread record only if a marker it read matched
           table.json and the record holds the thread table.json gives that
           seat, and refuses one that holds another or is malformed; with
           neither marker left to read it removes no thread record and lists
           each as left in place. table.json itself is refused over 64 KiB. Unless
           --force it also refuses, exit 1, a worktree that holds changes,
           ignored files included, beyond the files new wrote: the
           .parley-table always, the .mcp.json
           when a seat is Claude's, the .codex/config.toml when one is Codex's.
           When every such change is an ignored file, the refusal says so, and
           that a tool's session hook may have written them: look, then --force.
           A table.json that lists no seat is refused as not valid. It prints
           a codex delete line for each Codex seat and never runs one; the
           record keeps no --codex-bin, so those lines say plain codex. Every
           git command it runs in the worktree is given the worktree's git
           directory, common directory and work tree from the target's side, with
           core.fsmonitor and core.hooksPath off and no look inside a
           submodule, never from the worktree's own .git file, which a seat can
           rewrite
  build    set a two-peer build up, or take it down: two sessions negotiating
           as peers over one channel under the parley-build skill, each on a
           worktree and branch of the target, which must be adopted in Director
           (parley neither checks nor calls director). new checks its flags
           first: the id as a table's; the target the root of a git checkout whose HEAD tracks no .mcp.json of
           any kind, no .codex that is not a directory, no .codex/config.toml
           and no .parley-build; exactly two --peer <nick>=<claude|codex>[:<model>]
           (a third is refused, the protocol being two-peer), their nicks valid,
           distinct from each other and the chair,
           none of them "tables" or "builds", each model at most 128 bytes of
           letters, digits and . _ - : / @ [ ] + ; each --writable-root
           absolute, an existing directory, resolved to its physical path; and
           parley doctor's static checks, with the parley-build skill under the
           root each harness reads. Then it takes the id's lock (<state
           base>/.ids/<id>.lock, which table new and end and build new and end
           all take, first, and hold to the end of the run; a second command for
           the id refuses at once, exit 1, and does not wait), and under it
           refuses an id that is live as a table or a build, a branch
           parley/<id>/<nick> that exists, and a peer with state on this
           stream, since a stale cursor ends a run at its first delivery with a
           history gap (exit 2). Then, for each peer: a branch parley/<id>/<nick>
           made at the target's HEAD by a step that fails if it is there (so
           that a branch someone else made meanwhile is never deleted by the
           rollback), a worktree <state base>/builds/<id>/<nick> on it, a
           .parley-build in it, a .mcp.json for a Claude peer
           (--skill build, no --cap) and a .codex/config.toml for a Codex one
           (workspace-write, on-request, network on, and a writable_roots only
           when --writable-root is given: the roots named, then the global ones
           of $CODEX_HOME/config.toml, or ~/.codex/config.toml, which a project
           list would otherwise replace, each checked as a named one is and
           skipped, with a line saying why, if it fails; parley never widens a
           sandbox into the target's git directory by itself). A private reply
           directory under $TMPDIR; each Codex peer is started as table new
           starts a seat, with the build primer, and its thread recorded for
           --thread auto; every change has an undo, as table new's. build.json
           under <state base>/builds/<id>/ is the record. The output lists the
           prerequisites, the server, the chair's join, each peer's pump and
           resume or launch line, the gate, message one (a peer's to post,
           with participants: and target: filled in) and the integration
           step. end takes the id's lock,
           then reads build.json, takes both peers' locks (refusing, naming the
           peer, while one is held) and
           removes only what it can show is the build's: a worktree that is the
           recorded path under the build's directory and that the target lists
           (when git cannot say, only if the target is gone and the worktree is
           under the build's directory; any other failure of git leaves the
           worktree, quoting it);
           the reply directory as table end claims it. What it cannot claim is
           reported and left. Unless --force it refuses, listing every peer's
           result at once, a worktree with tracked changes or untracked files
           beyond the launch files (.parley-build, and .mcp.json or
           .codex/config.toml for the peer's harness), or that git cannot be
           asked about (the target gone); a clean end removes the worktree's
           files, while what a peer committed stays on its branch. Ignored files
           do not block, and it counts them, forced or not. It reports, and never refuses on, a
           commit on a peer's branch that touches a launch file. It removes the
           peers' cursor state and thread records (listing them instead when a
           marker that is there disagrees with build.json), the worktrees, the reply
           directory and the build directory, never a branch or a Codex
           session: it prints the kept branches and the git branch -D line for
           each, and a codex delete line for each Codex peer
  version  print one line, "parley <version> rev <revision> <go version>", from
           the build information the Go toolchain stamped into the binary: the
           module version ((devel) for a build made from a checkout), the first
           7 characters of the commit it was built at with -modified after them
           when the work tree had uncommitted changes (unknown when the build
           carries no revision), and the Go version. parley --version and
           parley -version print the same. For every verb, -h and --help print
           its usage on stdout and exit 0; a flag error prints on stderr and
           exits 2

skills:
  primer and channel take --skill build|table|line, build by default. build
  is the two-session mode. table is a seat at a table: the rules are the
  parley-table skill's, which the human starts at the seat. line is a session
  on a line, a few long-running sessions and one human in one channel, with no
  target and no worktree: the rules are the parley-line skill's. Under build
  and table the text carries three facts as values, set at launch and not by
  the channel: --chair <nick>, your human partner's registered nick (the
  chair, at a table), --target <dir>, the target repo, and --worktree <dir>,
  the session's own worktree of it. All three are required on both skills; the
  two directories are each an absolute path to an existing directory, not the
  same one (links followed; a worktree inside the target is fine), and all
  three are printed as spelled. The chair is another voice than --nick. primer
  also requires --reply-file with --skill table or line. Under line the facts
  are --chair <nick>, your human partner's registered nick, and --cap <n>, the
  breaker's cap, both required, and --window <duration>, the breaker's window,
  10m by default; --target and --worktree are refused. primer takes --cap and
  --window under line only, to state them: the text is printed, nothing is
  held. --cap is for --skill table and --skill line only, --window for
  --skill line only. channel and primer
  --front claude refuse the text, on any skill, when it is over Claude Code's
  2048-unit cap, which would cut its end; primer measures a shorter text than
  channel does, which has a resolved state directory and a reply directory of
  its own.
  The pre-check for a launch is the channel's own, with the launch's arguments:
  PARLEY_NICK=<nick> parley channel <the .mcp.json args> --check
  with the nick written out where an arg is ${PARLEY_NICK}, which the shell
  would expand before the assignment applied.

tables:
  With --skill table, channel and codex take --chair <nick> --cap <n>, both
  required, for a table of several agents and one human chair on one channel.
  codex takes --skill too, build by default, and neither flag without
  table or line; it takes no --target or --worktree. Until this participant has
  posted once, posts by anyone but the chair are held back; after that they
  arrive as usual until more than <n> messages by anyone but the chair have
  followed the chair's last post, and then they are held again until the chair
  posts. The chair's posts always pass and release what the cap held, in
  order, and reset the count; a post held because this participant has not yet
  posted stays held until it does. --cap 0 turns the cap off and keeps the
  first hold. Held posts live in this process's memory only, so a restart
  loses them, and so does a gap in the stream's history, which the delivery
  says. A delivery that is never handed over leaves the holds as they were.
  wait takes neither flag. --skill build holds nothing back.

lines:
  With --skill line, channel and codex take --chair <nick> --cap <n>, both
  required, and --window <duration>, for a few long-running sessions and one
  human on one channel. The gate is two rules and nothing else: there is no
  first hold, so this participant receives every other session's post at once,
  before it has posted. Every message by anyone but the chair, this
  participant's own included, is counted and timed, by its timestamp or by the
  time it was received when it has none. A message that makes more than <n> of
  them within --window (a Go duration, 10m by default, above zero), counting it
  and the <n> before it, trips the breaker, and so does one that makes more than
  five times <n> since the chair last posted, however slowly they came. The
  message that trips it is held, with every later one, until the chair posts;
  the chair's post releases them in order ahead of itself and clears the count
  and the record of times. Nothing is released by time passing, so a loop, fast
  or slow, stays stopped until the chair looks, while pokes spread over hours
  and a backlog spread over days do not trip it. --cap 0 turns the breaker off.
  Held posts live in this process's memory only, so a restart loses them, and so
  does a gap in the stream's history, which the delivery says; a gap keeps the
  count and the record of times. The chair is the human's registered nick,
  another voice than --nick. --window is refused with the other skills.

stream specs:
  file:<path>              one append-only NDJSON file, which stock tools can
                           read and write: tail -f to watch, >> to post
  irc://host:port/channel  one channel on a local IRC server (ergo, which
                           parley server runs; see contrib/ergo/README.md),
                           where a human can join from any stock client; ids
                           and timestamps are the server's

nicks:
  letters, digits, '_' and '-', up to 64 bytes: the nick is a directory
  in the state tree and the name a peer reads in the header, unchanged. Nicks
  match without regard to ASCII case, as the bundled ergo's do, so a chair
  posting as Colin is the chair given as colin

state:
  the lock lives in $XDG_STATE_HOME/parley/<nick>/ (by default
  ~/.local/state/parley/<nick>/), or in <dir>/<nick>/ with --state-dir <dir>,
  <nick> being the nick with its ASCII capitals folded to lower case, and
  the cursor in a per-stream directory under it, where a Codex seat's
  thread record, codex-thread, also sits: one line, newline included, holding
  the thread's UUID.
  Tables live in <dir>/tables/<id>/, beside the per-nick directories, which is
  why no participant is called tables; builds live in <dir>/builds/<id>/ the same
  way, and no participant is called builds. A table or a build id has a lock of
  its own, <dir>/.ids/<id>.lock, that table new and end and build new and end
  hold while they run; no nick can be .ids, a nick holding no '.'.
  An irc: stream also keeps the participant's own account password there, in
  irc/<host>_<port>/password, which parley generates and nobody types

exit codes:
  0 success   1 runtime error   2 configuration error
  server ends with ergo's own status when ergo exits non-zero
`)
}

// configError marks a bad invocation, which exits 2 rather than 1.
type configError struct{ err error }

func (e *configError) Error() string { return e.err.Error() }
func (e *configError) Unwrap() error { return e.err }

func cfgErrf(format string, a ...any) error {
	return &configError{err: fmt.Errorf(format, a...)}
}

// given reports whether the named flag was passed, as against left at its
// default. A flag whose empty value is a refusable one — an explicit --flag ""
// — cannot be told from an absent one by its value alone.
func given(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

// skillFlags are the flags channel and primer share for the skill a session is
// handed: which one, and the facts its text carries as values. gate says whether
// the verb installs the skill's gate, as channel does; primer prints the text
// and holds nothing, and takes --cap and --window under the line skill only, to
// state them.
type skillFlags struct {
	skill    *string
	chair    *string
	target   *string
	worktree *string
	cap      *int
	window   *time.Duration
	gate     bool
}

// defaultLineWindow is the --window of a line when none is given: long enough
// that sessions poking each other as work finishes do not reach the cap, short
// enough that a loop does within minutes.
const defaultLineWindow = 10 * time.Minute

func addSkillFlags(fs *flag.FlagSet, gate bool) skillFlags {
	k := skillFlags{
		skill:    fs.String("skill", string(primer.SkillBuild), "the protocol the text names: build (two peers), table (a seat at a table, which also needs --cap on channel) or line (a session on a line, which needs --chair and --cap and takes no --target or --worktree)"),
		chair:    fs.String("chair", "", "required: your human partner's registered nick, matched without regard to ASCII case, which the text names as the one whose channel lines carry their authority (at a table, the chair, whose posts also pass the gate; on a line, the human, whose posts release the breaker)"),
		target:   fs.String("target", "", "required with --skill build or table, refused with --skill line: the target repo, an absolute path to an existing directory, printed as spelled"),
		worktree: fs.String("worktree", "", "required with --skill build or table, refused with --skill line: this session's own worktree of the target repo, an absolute path to an existing directory, printed as spelled"),
		gate:     gate,
	}
	if gate {
		k.cap = addCapFlag(fs)
	} else {
		k.cap = fs.Int("cap", 0, "required with --skill line, refused with the others: the breaker's cap, the number the text states as a fact; primer holds nothing, so set the same value on the channel or codex front; 0 is no breaker")
	}
	k.window = addWindowFlag(fs, gate)
	return k
}

func addCapFlag(fs *flag.FlagSet) *int {
	return fs.Int("cap", 0, "with --skill table: hold every post by anyone but the chair once more than this many have gone by since the chair last posted, until the chair posts; with --skill line: hold every post by anyone but the chair once more than this many have arrived within --window, or more than five times this many since the chair last posted, until the chair posts; 0 turns the cap off; required with --skill table and --skill line. "+
		"Held posts are kept in memory only, so a restart or a gap in the history loses them")
}

// addWindowFlag adds --window, the line breaker's window. It has a default, so
// whether it was given is what tells a skill that does not take it.
func addWindowFlag(fs *flag.FlagSet, gate bool) *time.Duration {
	if !gate {
		return fs.Duration("window", defaultLineWindow, "with --skill line, refused with the others: the breaker's window, a Go duration above zero such as 10m or 90s, the number the text states as a fact; primer holds nothing, so set the same value on the channel or codex front")
	}
	return fs.Duration("window", defaultLineWindow, "with --skill line, refused with the others: the breaker's window, a Go duration above zero such as 10m or 90s: a post by anyone but the chair that makes more than --cap of them within this window, counting this participant's own, is held with every later one until the chair posts; nothing is released by time passing")
}

// launch is what channel and primer resolve their skill flags to.
type launch struct {
	skill    primer.Skill
	chair    string
	target   string
	worktree string
	// cap and window are the line's breaker, which the line text states.
	cap    int
	window time.Duration
	// gate is the skill's gate, installed iff the skill is table or line.
	gate []stream.Option
}

// resolve validates the flags. Everything refused is a bad invocation. The
// facts are values the text prints for the model, so the chair must be a nick
// another voice than this participant's, and under build and table each
// directory must exist and be a directory the way the human spelled it, with no
// control character that would break the text it lands in. A line has no target
// and no worktree, and is refused either one: a path it was handed would be one
// nobody reads.
func (k skillFlags) resolve(fs *flag.FlagSet, nick string) (launch, error) {
	skill, err := parseSkill(*k.skill)
	if err != nil {
		return launch{}, err
	}
	if err := checkChair(fs, *k.chair, nick, skill); err != nil {
		return launch{}, err
	}
	l := launch{skill: skill, chair: *k.chair}
	if skill == primer.SkillLine {
		for _, name := range []string{"target", "worktree"} {
			if given(fs, name) {
				return launch{}, cfgErrf("--%s is not taken with --skill line: a line has no target repo and no worktree of the session's, the session works in its own checkout", name)
			}
		}
	} else {
		targetInfo, err := checkDir("target", *k.target, skill)
		if err != nil {
			return launch{}, err
		}
		worktreeInfo, err := checkDir("worktree", *k.worktree, skill)
		if err != nil {
			return launch{}, err
		}
		// The worktree is a checkout of the target made for this session, so it is
		// another directory; one inside the target, as under .claude/worktrees, is
		// normal. os.SameFile compares what the paths resolve to, links followed.
		if os.SameFile(targetInfo, worktreeInfo) {
			return launch{}, cfgErrf("--target %q and --worktree %q are the same directory (links followed): the worktree is this session's own checkout of the target, a directory of its own", *k.target, *k.worktree)
		}
		l.target, l.worktree = *k.target, *k.worktree
	}
	switch {
	case k.gate:
		if l.gate, err = gateOptions(fs, skill, *k.chair, *k.cap, *k.window); err != nil {
			return launch{}, err
		}
	case skill == primer.SkillLine:
		// primer installs no gate, but the text states the cap and the window, so
		// they are as required and as checked as they are where the gate is built.
		if err := checkCap(fs, skill, *k.cap); err != nil {
			return launch{}, err
		}
		if err := checkWindow(fs, skill, *k.window); err != nil {
			return launch{}, err
		}
	case given(fs, "cap"):
		return launch{}, cfgErrf("--cap is taken by primer with --skill line only: the text states a line's cap, and %s has none to state", skill)
	default:
		if err := checkWindow(fs, skill, *k.window); err != nil {
			return launch{}, err
		}
	}
	if skill == primer.SkillLine {
		l.cap, l.window = *k.cap, *k.window
	}
	return l, nil
}

func parseSkill(s string) (primer.Skill, error) {
	skill, err := primer.ParseSkill(s)
	if err != nil {
		return "", &configError{err: fmt.Errorf("--skill: %w", err)}
	}
	return skill, nil
}

// checkChair validates the chair the text names, which the long-running fronts
// also hand to the table or line gate. It is the human's nick, so it is never this
// participant's own, and nicks are the same without regard to ASCII case.
func checkChair(fs *flag.FlagSet, chair, nick string, skill primer.Skill) error {
	if !given(fs, "chair") {
		return cfgErrf("--chair is required with --skill %s: your human partner's registered nick", skill)
	}
	if err := stream.ValidNick(chair); err != nil {
		return cfgErrf("--chair %q %v", chair, err)
	}
	if stream.NickEqual(chair, nick) {
		return cfgErrf("--chair %q is this participant's own --nick %q: your human partner is another voice, and nicks are the same without regard to ASCII case", chair, nick)
	}
	return nil
}

// checkCap validates the cap a gated skill is launched with: required, not
// negative. The cap has no default: a table or a line whose limit was never
// named would be one whose limit nobody chose, so "no limit" is spelled --cap 0.
// A build has no gate, and is refused a cap.
func checkCap(fs *flag.FlagSet, skill primer.Skill, capN int) error {
	if skill != primer.SkillTable && skill != primer.SkillLine {
		if given(fs, "cap") {
			return cfgErrf("--cap applies to --skill table or line only: a build holds nothing back")
		}
		return nil
	}
	if !given(fs, "cap") {
		return cfgErrf("--cap is required with --skill %s: name the cap, or --cap 0 for none", skill)
	}
	if capN < 0 {
		return cfgErrf("--cap must not be negative")
	}
	return nil
}

// checkWindow validates the window a line's breaker is launched with: above
// zero. It has a default, 10m, so it is never missing; a window is a line's alone
// and is refused, by name, with a table, whose breaker counts posts since the
// chair last posted, and with a build, which has none.
func checkWindow(fs *flag.FlagSet, skill primer.Skill, window time.Duration) error {
	if skill != primer.SkillLine {
		if given(fs, "window") {
			return cfgErrf("--window applies to --skill line only: a table's breaker counts posts since the chair last posted, and a build holds nothing back")
		}
		return nil
	}
	if window <= 0 {
		return cfgErrf("--window must be above zero, e.g. --window 10m: the breaker counts the posts that arrive within it, and a window with no length has none to count")
	}
	return nil
}

// gateOptions returns the Deliverer options the skill stands for: the gate is
// installed iff the skill is table (the seal and the breaker) or line (the
// breaker alone, a rate over --window), and then --cap is required. The chair is
// the caller's, and checked by it.
func gateOptions(fs *flag.FlagSet, skill primer.Skill, chair string, capN int, window time.Duration) ([]stream.Option, error) {
	if err := checkCap(fs, skill, capN); err != nil {
		return nil, err
	}
	if err := checkWindow(fs, skill, window); err != nil {
		return nil, err
	}
	switch skill {
	case primer.SkillTable:
		return []stream.Option{stream.WithTable(chair, capN)}, nil
	case primer.SkillLine:
		return []stream.Option{stream.WithLine(chair, capN, window)}, nil
	}
	return nil, nil
}

// checkDir validates one of the skill text's directories: given, absolute, free
// of control characters, and an existing directory, whose info it returns. The
// path itself is never rewritten: what the text prints is what the human typed.
func checkDir(flagName, path string, skill primer.Skill) (os.FileInfo, error) {
	switch {
	case path == "":
		return nil, cfgErrf("--%s is required with --skill %s", flagName, skill)
	case !filepath.IsAbs(path):
		return nil, cfgErrf("--%s %q must be an absolute path", flagName, path)
	case strings.IndexFunc(path, unicode.IsControl) >= 0:
		return nil, cfgErrf("--%s %q holds a control character: it is printed into the text the model reads", flagName, path)
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, cfgErrf("--%s %q: %v", flagName, path, err)
	}
	if !fi.IsDir() {
		return nil, cfgErrf("--%s %q is not a directory", flagName, path)
	}
	return fi, nil
}

// overCap is the refusal for instructions the client would truncate: one line,
// naming the length, the cap and what to shorten. The same wording serves
// channel, which refuses to start and under --check, and primer's claude form,
// which refuses the same text for the same reason but measures a shorter one:
// it is not the pre-check for a launch, channel --check is.
func overCap(skill primer.Skill, n int) error {
	if skill == primer.SkillLine {
		return overCapBy(n, "--state-dir, --stream or the path of the parley binary")
	}
	return overCapBy(n, "--target, --worktree, --state-dir, --stream or the path of the parley binary")
}

// overCapBy is overCap for a caller that knows which of its inputs make the text
// long: remedy names what to shorten.
func overCapBy(n int, remedy string) error {
	return cfgErrf("the instructions would be %d UTF-16 code units, over Claude Code's cap of %d, which cuts off their end; shorten %s", n, primer.ClaudeInstructionsCap, remedy)
}

func cmdSend(args []string) error {
	fs := flag.NewFlagSet("parley send", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: parley send --stream <spec> --nick <nick> [--body-file <path>] [--state-dir <dir>] [--] [<body>]")
		fs.PrintDefaults()
	}
	spec := fs.String("stream", "", "stream to post to, e.g. file:/path/to/chat.ndjson")
	nick := fs.String("nick", "", "this participant's nick")
	bodyFile := fs.String("body-file", "", "regular file holding the message body, read whole and never followed as a symbolic link; the body then never passes through a shell word")
	// No lock and no cursor: send has no position to keep. It takes the state
	// directory because a transport may hold a credential for this participant
	// there — the IRC adapter's account password — and a send that could not
	// read it would be refused the nick the pump is already holding.
	stateDir := fs.String("state-dir", "", "state directory holding <nick>/ (default $XDG_STATE_HOME/parley)")
	if err := parseFlags(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return &configError{err: err}
	}
	if err := checkNick(*nick); err != nil {
		return err
	}
	// One message comes from one place. An invocation naming two of them says
	// nothing about which was meant, and guessing would send the wrong one.
	if *bodyFile != "" && fs.NArg() > 0 {
		return cfgErrf("--body-file and a <body> argument are mutually exclusive: pass one or the other")
	}
	// Every flag is checked before stdin is touched: a misspelled flag must not
	// consume a body the caller cannot hand over a second time.
	s, canonical, err := openStream(*spec, *nick, *stateDir)
	if err != nil {
		return err
	}
	defer closeStream(s)
	warnIfNotThePumpsThread(os.Stderr, *stateDir, *nick, canonical)

	body := strings.Join(fs.Args(), " ")
	switch {
	case *bodyFile != "":
		body, err = readBodyFile(*bodyFile)
		if err != nil {
			return err
		}
	case fs.NArg() == 0:
		body, err = readBody(os.Stdin, "stdin")
		if err != nil {
			return err
		}
	}
	if strings.TrimSpace(body) == "" {
		return cfgErrf("empty message body")
	}

	ctx, stop := signalContext()
	defer stop()
	sent, err := stream.NewDeliverer(s, *nick).Send(ctx, body)
	if err != nil {
		return fmt.Errorf("send: %w", err)
	}
	// The post has happened, so a failure to say so must not become a non-zero
	// exit: the caller would send it again. A write to a closed stdout is not
	// only that failure but a signal: Go ends a process that is not handling
	// SIGPIPE when a write to stdout hits a broken pipe, with the post already
	// made and an exit status that says it was not. Ignoring the signal turns it
	// into the EPIPE the line below discards.
	signal.Ignore(syscall.SIGPIPE)
	_, _ = fmt.Fprintln(os.Stdout, sent.Receipt())
	return nil
}

// readBodyFile reads a message body from a file, which is the form the channel
// front's reply recipe uses: a body that reaches parley by path is a body no
// shell ever parses, so nothing inside it can be cut short or executed.
//
// The link is not followed and the file must be a regular one, because the path
// commonly sits in a directory the model itself writes and an allowed command
// runs where the sandbox does not: a symlink planted there would otherwise post
// whatever it points at. O_NONBLOCK is what keeps the check reachable — opening
// a FIFO read-only blocks until a writer appears — and is inert on a regular
// file.
func readBodyFile(path string) (string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		// Both darwin and linux report a refused O_NOFOLLOW open as ELOOP, which
		// as a bare "too many levels of symbolic links" names neither the file
		// nor what parley refused to do with it.
		if errors.Is(err, syscall.ELOOP) {
			return "", fmt.Errorf("read --body-file: %s is a symbolic link, and send does not follow one", path)
		}
		return "", fmt.Errorf("read --body-file: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("read --body-file: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("read --body-file: %s is not a regular file", path)
	}
	return readBody(f, "file "+path)
}

// readBody reads one whole message body from r. Every path that is not a
// command-line argument comes through here, so a body is bounded and trimmed
// the same way however it arrived; source names it in the errors.
func readBody(r io.Reader, source string) (string, error) {
	// Bounded, and one byte past the bound so the overflow is visible: a
	// producer handing a large file to send must be told its body is too large,
	// not have parley buffer it whole to find that out.
	b, err := io.ReadAll(io.LimitReader(r, file.MaxLine+1))
	if err != nil {
		return "", fmt.Errorf("read body from %s: %w", source, err)
	}
	if len(b) > file.MaxLine {
		return "", fmt.Errorf("body from %s exceeds the maximum message size of %d bytes", source, file.MaxLine)
	}
	return strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r"), nil
}

func cmdWait(args []string) error {
	fs := flag.NewFlagSet("parley wait", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: parley wait --stream <spec> --nick <nick> [--timeout 30s] [--debounce 250ms] [--state-dir <dir>]")
		fs.PrintDefaults()
	}
	spec := fs.String("stream", "", "stream to read, e.g. file:/path/to/chat.ndjson")
	nick := fs.String("nick", "", "this participant's nick")
	timeout := fs.Duration("timeout", 30*time.Second, "how long to block for peer messages; the call returns by --timeout plus --debounce at the latest")
	debounce := fs.Duration("debounce", 250*time.Millisecond, "quiet window that joins a burst into one delivery")
	stateDir := fs.String("state-dir", "", "state directory holding <nick>/ (default $XDG_STATE_HOME/parley)")
	if err := parseFlags(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return &configError{err: err}
	}
	if err := checkNick(*nick); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return cfgErrf("unexpected argument %q", fs.Arg(0))
	}
	if *timeout < 0 {
		return cfgErrf("--timeout must not be negative")
	}
	if *debounce < 0 {
		return cfgErrf("--debounce must not be negative")
	}

	s, canonical, err := openStream(*spec, *nick, *stateDir)
	if err != nil {
		return err
	}
	defer closeStream(s)
	// The cursor is per nick and per stream, keyed on the canonical spec so that
	// the same stream keeps one position however the caller spelled it; a
	// failure to create the directory is an I/O problem, not a bad invocation.
	dir, err := state.Open(*stateDir, *nick, canonical)
	if err != nil {
		return err
	}
	// Two parley processes for one participant would race on the cursor; the
	// second is refused rather than reconciled.
	lock, err := dir.Lock()
	if err != nil {
		var held *state.LockedError
		if errors.As(err, &held) {
			return &configError{err: err}
		}
		return err
	}
	defer lock.Release()

	cursor, err := dir.LoadCursor()
	if err != nil {
		return err
	}
	ctx, stop := signalContext()
	defer stop()

	d := stream.NewDeliverer(s, *nick)
	delivery, err := d.Wait(ctx, stream.Cursor(cursor), *timeout, *debounce)
	if err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("wait: %w", err)
	}
	if out := delivery.Render(); out != "" {
		if _, err := io.WriteString(os.Stdout, out); err != nil {
			// Nothing is committed and nothing is saved: the delivery never
			// reached the participant, so the next wait must repeat it.
			return fmt.Errorf("write delivery: %w", err)
		}
	}
	// Reached here even for a delivery that rendered as nothing: what was read
	// and dropped was this participant's own or a duplicate, and it will never
	// be delivered. Leaving the cursor behind it would re-read it forever.
	d.Commit(delivery)
	if err := dir.SaveCursor(string(delivery.Cursor)); err != nil {
		return err
	}
	return nil
}

// cmdChannel runs the MCP channel server. It opens the stream, the state
// directory, the lock and the cursor exactly as cmdWait does — a channel
// session is one long-lived wait — and hands them to the channel package, which
// owns the protocol. Nothing is printed on stdout here: it belongs to JSON-RPC.
func cmdChannel(args []string) error {
	fs := flag.NewFlagSet("parley channel", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: parley channel --stream <spec> --nick <nick> --chair <nick> --target <dir> --worktree <dir> [--debounce 250ms] [--state-dir <dir>] [--skill build|table --cap <n>] [--check]")
		fmt.Fprintln(fs.Output(), "       parley channel --stream <spec> --nick <nick> --skill line --chair <nick> --cap <n> [--window 10m] [--debounce 250ms] [--state-dir <dir>] [--check]")
		fs.PrintDefaults()
	}
	spec := fs.String("stream", "", "stream to read, e.g. file:/path/to/chat.ndjson")
	nick := fs.String("nick", "", "this participant's nick")
	debounce := fs.Duration("debounce", 250*time.Millisecond, "quiet window that joins a burst into one delivery")
	stateDir := fs.String("state-dir", "", "state directory holding <nick>/ (default $XDG_STATE_HOME/parley)")
	skills := addSkillFlags(fs, true)
	check := fs.Bool("check", false, "serve nothing: validate the flags, build the instructions the session would send, print their length as \"instructions: <n> of 2048 UTF-16 units\", and exit 0 within Claude Code's cap or 2 over it; opens no stream and takes no lock")
	if err := parseFlags(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return &configError{err: err}
	}
	if err := checkNick(*nick); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return cfgErrf("unexpected argument %q", fs.Arg(0))
	}
	if *debounce < 0 {
		return cfgErrf("--debounce must not be negative")
	}
	// The gate is the skill's: a seat at a table is told who the chair is, and
	// the gates that make the chair's word first travel with that, a line session
	// gets the breaker of those and no seal, while a build holds nothing back.
	l, err := skills.resolve(fs, *nick)
	if err != nil {
		return err
	}
	if *check {
		return checkChannel(channel.Config{
			Spec:     *spec,
			Skill:    l.skill,
			Chair:    l.chair,
			Target:   l.target,
			Worktree: l.worktree,
			Cap:      l.cap,
			Window:   l.window,
		}, *nick, *stateDir, l.gate)
	}

	s, canonical, err := openStream(*spec, *nick, *stateDir)
	if err != nil {
		return err
	}
	defer closeStream(s)
	dir, err := state.Open(*stateDir, *nick, canonical)
	if err != nil {
		return err
	}
	lock, err := dir.Lock()
	if err != nil {
		var held *state.LockedError
		if errors.As(err, &held) {
			return &configError{err: err}
		}
		return err
	}
	defer lock.Release()

	srv, err := channel.New(channel.Config{
		Deliverer: stream.NewDeliverer(s, *nick, l.gate...),
		Cursors:   dir,
		// Resolved and absolute: the reply runs as a separate process from a
		// shell whose working directory is nobody's to predict, and it has to
		// find the same state — the IRC adapter's account password for this
		// nick lives there.
		StateDir: dir.Base(),
		// The canonical spec, not the spelling on the command line: the reply
		// command in the instructions has to work from whatever directory the
		// model's shell happens to be in.
		Spec:     canonical,
		Command:  selfPath(),
		In:       os.Stdin,
		Out:      os.Stdout,
		Log:      os.Stderr,
		Debounce: *debounce,
		Skill:    l.skill,
		Chair:    l.chair,
		Target:   l.target,
		Worktree: l.worktree,
		Cap:      l.cap,
		Window:   l.window,
	})
	if err != nil {
		return err
	}
	// A session is refused at launch rather than served truncated: the text
	// carries the user's paths, and the client keeps the head of anything over
	// its cap and drops the tail, which is the trust rule.
	if n := primer.UTF16Len(srv.Instructions()); n > primer.ClaudeInstructionsCap {
		srv.Close()
		return overCap(l.skill, n)
	}
	ctx, stop := signalContext()
	defer stop()
	if err := srv.Serve(ctx); err != nil {
		return fmt.Errorf("channel: %w", err)
	}
	return nil
}

// noCursors is the cursor store of a session that is not going to run: nothing
// is stored, so nothing is read and nothing is created.
type noCursors struct{}

func (noCursors) LoadCursor() (string, error) { return "", nil }
func (noCursors) SaveCursor(string) error     { return nil }

// measuringServer is the channel server a launch with these flags would build,
// made only to be measured: the canonical spec, the resolved state directory,
// this binary's path, no stream, no cursor and no lock. `parley table new` and
// `parley build new` build the same one for each Claude seat or peer, so what
// they refuse is what `channel --check` would. The caller closes it, which removes its reply directory.
func measuringServer(cfg channel.Config, nick, stateDir string, gate []stream.Option) (*channel.Server, error) {
	canonical, _, err := resolveSpec(cfg.Spec)
	if err != nil {
		return nil, err
	}
	base, err := state.ResolveBase(stateDir)
	if err != nil {
		return nil, err
	}
	cfg.Deliverer = stream.NewDeliverer(nil, nick, gate...)
	cfg.Cursors = noCursors{}
	cfg.StateDir = base
	cfg.Spec = canonical
	cfg.Command = selfPath()
	cfg.In = strings.NewReader("")
	cfg.Out = io.Discard
	return channel.New(cfg)
}

// checkChannel is `parley channel --check`: it measures the instructions a
// session launched with these flags would send, and nothing else. They are built
// by the channel package's own constructor and its own Instructions method, from
// the values a live run hands them — the canonical spec, the state directory
// resolved to an absolute path, this binary's path, and a reply directory made in
// the system temp directory for the purpose and removed before it returns — so
// the length is the one the client will see and not an estimate of it. The one
// thing a launch draws at random is the name of its reply directory, whose tail
// is not always the same length, so the measure is taken at the longest name
// there can be: a pass guarantees the launch fits, and the number can be a few
// units over what a launch sends. What it does not do is the rest of a launch:
// no stream is opened (an irc: spec is not connected to, a file: spec is not
// created), no state directory is made, no lock is taken and nothing is served.
//
// cfg carries the flags the instructions are built from; the rest is filled in
// here. Over the cap is a configuration error, so the exit is 2 and the line
// that says why is on stderr; the one line on stdout is the measurement.
func checkChannel(cfg channel.Config, nick, stateDir string, gate []stream.Option) error {
	srv, err := measuringServer(cfg, nick, stateDir, gate)
	if err != nil {
		return err
	}
	defer srv.Close()
	n := primer.UTF16Len(srv.LongestInstructions())
	// The reply directory is removed by the deferred Close, so the process must
	// outlive the write: without this a reader that has gone away ends it on
	// SIGPIPE first, the way it would a send, and the directory stays. The
	// write's own failure is still reported, as the exit that says the
	// measurement did not arrive.
	signal.Ignore(syscall.SIGPIPE)
	if _, err := fmt.Fprintf(os.Stdout, "instructions: %d of %d UTF-16 units\n", n, primer.ClaudeInstructionsCap); err != nil {
		return fmt.Errorf("write the measurement: %w", err)
	}
	if n <= primer.ClaudeInstructionsCap {
		return nil
	}
	return overCap(cfg.Skill, n)
}

// cmdCodex runs the Codex front: a foreground pump that blocks on the stream
// and hands every delivery to a live Codex session with one `codex queue` call,
// or one call per message when the delivery is over the argument bound.
// It opens the stream, the state directory, the lock and the cursor exactly as
// cmdWait does — the Codex front is one long-lived wait — and differs in where
// the delivery goes. Nothing is printed on stdout: the deliveries go into the
// Codex session, and everything this process has to say goes to stderr, so the
// terminal running it stays a log and nothing else.
func cmdCodex(args []string) error {
	fs := flag.NewFlagSet("parley codex", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: parley codex --stream <spec> --nick <nick> --thread <thread>|auto [--debounce 250ms] [--state-dir <dir>] [--codex-bin codex] [--skill build|table|line --chair <nick> --cap <n> [--window 10m]]")
		fs.PrintDefaults()
	}
	spec := fs.String("stream", "", "stream to read, e.g. file:/path/to/chat.ndjson")
	nick := fs.String("nick", "", "this participant's nick")
	thread := fs.String("thread", "", "the live Codex thread to deliver into: a session UUID or an exact session name, or auto for the thread id recorded for this nick and stream by parley table new or parley build new; auto is reserved, so a session literally named auto must be passed by its UUID")
	debounce := fs.Duration("debounce", 250*time.Millisecond, "quiet window that joins a burst into one delivery")
	stateDir := fs.String("state-dir", "", "state directory holding <nick>/ (default $XDG_STATE_HOME/parley)")
	codexBin := fs.String("codex-bin", codex.DefaultBin, "the codex CLI to run, resolved on PATH unless it holds a separator")
	skillFlag := fs.String("skill", string(primer.SkillBuild), "the skill the session was started with: build (two peers, nothing held back), table (a seat at a table, which needs --chair and --cap) or line (a session on a line, which needs --chair and --cap)")
	chair := fs.String("chair", "", "with --skill table or line, required: the human's nick, matched without regard to ASCII case: its posts always pass and release what the cap held; at a table they also hold the other agents' posts until this participant has posted once")
	capN := addCapFlag(fs)
	window := addWindowFlag(fs, true)
	if err := parseFlags(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return &configError{err: err}
	}
	if err := checkNick(*nick); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return cfgErrf("unexpected argument %q", fs.Arg(0))
	}
	if *debounce < 0 {
		return cfgErrf("--debounce must not be negative")
	}
	// The pump holds what the skill says is held: the gate is installed iff the
	// skill is table or line. It is told the chair for that and nothing else; the
	// facts the session's text carries are the primer's and the channel's.
	skill, err := parseSkill(*skillFlag)
	if err != nil {
		return err
	}
	if skill == primer.SkillTable || skill == primer.SkillLine {
		if err := checkChair(fs, *chair, *nick, skill); err != nil {
			return err
		}
	} else if given(fs, "chair") {
		return cfgErrf("--chair applies to --skill table or line only: a build has no chair to hold the others back for")
	}
	gate, err := gateOptions(fs, skill, *chair, *capN, *window)
	if err != nil {
		return err
	}
	// There is no default thread: a pump pointed at nothing would queue every
	// delivery into a session that does not exist. The one way to find it is the
	// record the seat's start wrote, and only when asked for by name.
	if strings.TrimSpace(*thread) == "" {
		if skill == primer.SkillLine {
			return cfgErrf("--thread is required: the Codex session UUID or exact session name to deliver into (a line has no thread record, so the pump takes the thread id)")
		}
		return cfgErrf("--thread is required: the Codex session UUID or exact session name to deliver into, or auto for the thread recorded by parley table new or parley build new")
	}
	// Resolved before the lock is taken, and before anything else can fail: a
	// codex that is not installed is the invocation's mistake, and the nick's
	// lock must not be taken — and then handed back — to say so.
	bin, err := exec.LookPath(*codexBin)
	if err != nil {
		return cfgErrf("--codex-bin %q: %v", *codexBin, err)
	}
	// Read before the stream is opened, which for an irc: stream connects and
	// registers: a seat with no record is the invocation's mistake, and it must
	// not cost the nick an account, a connection or its lock to say so.
	threadID, source := *thread, ""
	if *thread == threadAuto {
		if threadID, source, err = recordedThread(*stateDir, *nick, *spec, skill); err != nil {
			return err
		}
	}

	s, canonical, err := openStream(*spec, *nick, *stateDir)
	if err != nil {
		return err
	}
	defer closeStream(s)
	dir, err := state.Open(*stateDir, *nick, canonical)
	if err != nil {
		return err
	}
	lock, err := dir.Lock()
	if err != nil {
		var held *state.LockedError
		if errors.As(err, &held) {
			return &configError{err: err}
		}
		return err
	}
	defer lock.Release()
	// The record was read before the lock, so that a seat with none costs
	// nothing; a table end or build end that ran in between has released the lock and
	// removed the record, so it is read again now that this pump holds the nick.
	if *thread == threadAuto {
		again, _, err := recordedThread(*stateDir, *nick, *spec, skill)
		if err != nil {
			return fmt.Errorf("the recorded thread is gone since this pump started (a table end or build end ran meanwhile?): %w", err)
		}
		if !strings.EqualFold(again, threadID) {
			return fmt.Errorf("the recorded thread for %s changed from %s to %s while this pump started: run it again", *nick, threadID, again)
		}
	}

	pump, err := codex.New(codex.Config{
		Deliverer: stream.NewDeliverer(s, *nick, gate...),
		Cursors:   dir,
		Thread:    threadID,
		Debounce:  *debounce,
		Log:       os.Stderr,
		Run:       codex.ExecRunner(bin),
	})
	if err != nil {
		return err
	}
	ctx, stop := signalContext()
	defer stop()
	fmt.Fprintf(os.Stderr, "parley codex: %s as %s into Codex thread %s via %s%s\n", canonical, *nick, threadID, bin, source)
	if err := pump.Run(ctx); err != nil {
		return fmt.Errorf("codex: %w", err)
	}
	return nil
}

// threadAuto is the --thread value that asks for the recorded thread instead of
// naming one. It is reserved: a Codex session literally named "auto" is passed
// by its UUID.
const threadAuto = "auto"

// codexThreadEnv is the variable Codex sets in every model shell, since 0.95, to
// the thread that shell belongs to.
const codexThreadEnv = "CODEX_THREAD_ID"

// recordedThread reads the thread the seat's start recorded for nick on the
// stream spec, for --thread auto. It returns the id and a note for the start
// line saying where it came from. A missing or unreadable record is a
// configuration error: the invocation asked for something that is not there.
// The spec is canonicalized here as openStream will, and nothing is opened or
// created.
func recordedThread(stateDir, nick, spec string, skill primer.Skill) (id, source string, err error) {
	canonical, _, err := resolveSpec(spec)
	if err != nil {
		return "", "", err
	}
	rec, err := state.OpenThreadRecord(stateDir, nick, canonical)
	if err != nil {
		return "", "", &configError{err: err}
	}
	id, err = rec.Read()
	if err != nil {
		return "", "", cfgErrf("--thread %s: %v: pass --thread <id> instead", threadAuto, err)
	}
	if id == "" {
		// parley table new and parley build new make the record; a line is set up by
		// hand, so the advice to run one of them would send its user to a verb
		// that cannot start their session.
		if skill == primer.SkillLine {
			return "", "", cfgErrf("--thread %s: a line has no thread record (nothing recorded for nick %q on %s, looked for %s): the pump takes the thread id with --thread <id>", threadAuto, nick, canonical, rec.Path())
		}
		return "", "", cfgErrf("--thread %s: no Codex thread is recorded for nick %q on %s (looked for %s): run `parley table new` or `parley build new` to start the seat, or pass --thread <id>", threadAuto, nick, canonical, rec.Path())
	}
	return id, fmt.Sprintf(" (--thread %s, from %s)", threadAuto, rec.Path()), nil
}

// warnIfNotThePumpsThread tells a Codex session that its posts will not be
// answered in it: the thread its shell reports is not the one the record says
// the pump delivers into, as after /clear or in a sub-agent. It is advice and
// never a refusal — the post is wanted either way, and the record may be the
// stale half of the pair — so every failure to find out is also only a warning,
// and no record, or no $CODEX_THREAD_ID, is silence.
func warnIfNotThePumpsThread(w io.Writer, stateDir, nick, canonical string) {
	session := strings.TrimSpace(os.Getenv(codexThreadEnv))
	if session == "" {
		return
	}
	rec, err := state.OpenThreadRecord(stateDir, nick, canonical)
	var recorded string
	if err == nil {
		recorded, err = rec.Read()
	}
	if err != nil {
		fmt.Fprintf(w, "parley: warning: cannot check this session's thread against the pump's: %v\n", err)
		return
	}
	if recorded == "" || strings.EqualFold(session, recorded) {
		return
	}
	fmt.Fprintf(w, "parley: warning: this session is Codex thread %q, but the pump for %s on %s delivers into thread %s (%s), so channel posts will not reach this session; sending anyway\n",
		session, nick, canonical, recorded, rec.Path())
}

// codexReplyPlaceholder stands in for the reply file in a codex primer that
// names none. It is never defaulted into the state directory: that holds the
// lock, the cursors and the IRC account password, and a reply body dropped
// beside them is a peer's plaintext in the one directory that must not be
// shared. Outside a live channel session there is no private directory to point
// at either, so the human fills this in.
const codexReplyPlaceholder = "<reply file path>"

// cmdPrimer prints the framing a front gives the model: the reply command, then
// what a delivery is and what it is worth. `parley channel` renders the same
// text from the same package at connect time; this verb is for the fronts whose
// harness has nothing to hand it over with, where the text is the session's
// first message — a human pastes it in, or the session is started with it as its
// first prompt — and for reading the text without starting one.
//
// It opens nothing — no stream, no state directory, no lock. An irc: spec
// handed to openStream would connect and register an account, and a primer is
// not a participant; nothing here needs a stream to exist, let alone to be
// reachable. So every value is printed exactly as it was spelled, and the
// usage text says where a live session's own text differs.
func cmdPrimer(args []string) error {
	fs := flag.NewFlagSet("parley primer", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: parley primer --front claude|codex --stream <spec> --nick <nick> --chair <nick> --target <dir> --worktree <dir> [--reply-file <path>] [--state-dir <dir>] [--command <spelling>] [--reply-command <cmd>] [--skill build|table]")
		fmt.Fprintln(fs.Output(), "       parley primer --front claude|codex --stream <spec> --nick <nick> --skill line --chair <nick> --cap <n> [--window 10m] [--reply-file <path>] [--state-dir <dir>] [--command <spelling>] [--reply-command <cmd>]")
		fs.PrintDefaults()
	}
	front := fs.String("front", "", "the front the text is for: claude or codex")
	spec := fs.String("stream", "", "stream the participant posts to, as the reply command should name it")
	nick := fs.String("nick", "", "this participant's nick")
	replyFile := fs.String("reply-file", "", "file the reply recipe names; required for --front claude and with --reply-command, a placeholder for the plain codex form when omitted")
	stateDir := fs.String("state-dir", "", "state directory holding <nick>/, printed as spelled; left out of the text when empty")
	command := fs.String("command", selfPath(), "the parley binary as the model should type it, taken as spelled")
	replyCommand := fs.String("reply-command", "", "the whole command the model runs to reply, in place of the send line; for a fixed-argument wrapper, --front codex only")
	skills := addSkillFlags(fs, false)
	if err := parseFlags(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return &configError{err: err}
	}
	f, err := primer.ParseFront(*front)
	if err != nil {
		return &configError{err: err}
	}
	if err := checkNick(*nick); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return cfgErrf("unexpected argument %q", fs.Arg(0))
	}
	// Not openStream's check, since nothing is opened; the spec still has to be
	// there, because the reply command is useless without it.
	if *spec == "" {
		return cfgErrf("--stream is required, e.g. --stream file:/path/to/chat.ndjson")
	}
	// The flag defaults to this binary's own path, so an empty value is an
	// explicit --command "": it would open the reply command with an empty
	// shell word, and the model is told to run that line as written.
	if *command == "" {
		return cfgErrf("--command must not be empty, e.g. --command /usr/local/bin/parley")
	}
	// An empty --reply-command is an explicit one: the flag's own default means
	// "no override", so the value is only ever read when the flag was passed.
	if given(fs, "reply-command") {
		if f != primer.Codex {
			return cfgErrf("--reply-command applies to the codex front only: the claude form's reply line is the channel's own")
		}
		if *replyCommand == "" {
			return cfgErrf("--reply-command must not be empty, e.g. --reply-command /path/to/send.sh")
		}
		// The model is told to run one line, and a test lifts that line out of the
		// text. A value carrying a newline would be two of them.
		if strings.ContainsAny(*replyCommand, "\r\n") {
			return cfgErrf("--reply-command must be one line: it is printed as the only line the model runs")
		}
		// A wrapper reads one file, fixed inside it. The placeholder below would
		// leave step 1 naming `<reply file path>` while the wrapper reads that
		// fixed file, so the model would write a file nothing ever sends.
		if *replyFile == "" {
			return cfgErrf("--reply-file is required with --reply-command: the wrapper reads a fixed file, and step 1 must name that file")
		}
	}
	l, err := skills.resolve(fs, *nick)
	if err != nil {
		return err
	}
	// The table and line forms are all values, and the reply file is one of them:
	// the placeholder is a blank, and those forms have none.
	if (l.skill == primer.SkillTable || l.skill == primer.SkillLine) && *replyFile == "" {
		return cfgErrf("--reply-file is required with --skill %s: the text names the file, and there are no blanks in it", l.skill)
	}
	reply := *replyFile
	if reply == "" {
		if f == primer.Claude {
			return cfgErrf("--reply-file is required for --front claude: the channel front names a file in the private directory it makes for a session, and there is none outside a live one")
		}
		reply = codexReplyPlaceholder
	}
	// Render ends the text with its own newline, so nothing is added here: the
	// count a live session hands the client has to be the count printed here.
	text := primer.Render(primer.Params{
		Front:        f,
		Command:      *command,
		Spec:         *spec,
		Nick:         *nick,
		StateDir:     *stateDir,
		ReplyPath:    reply,
		ReplyCommand: *replyCommand,
		Skill:        l.skill,
		Chair:        l.chair,
		Target:       l.target,
		Worktree:     l.worktree,
		Cap:          l.cap,
		Window:       l.window,
	})
	// The claude form is what the channel front sends at connect time, so it is
	// held to the cap the channel front holds it to: this is the way to find out
	// before launching. The codex form is the session's first message, not
	// capped.
	if f == primer.Claude {
		if n := primer.UTF16Len(text); n > primer.ClaudeInstructionsCap {
			return overCap(l.skill, n)
		}
	}
	if _, err := io.WriteString(os.Stdout, text); err != nil {
		return fmt.Errorf("write the primer: %w", err)
	}
	return nil
}

// selfPath is this binary as the model should invoke it: absolute, so the reply
// command in the channel instructions does not depend on a working directory.
// A build that cannot say where it lives falls back to the plain name, which is
// right whenever parley is on PATH.
func selfPath() string {
	if exe, err := os.Executable(); err == nil {
		return exe
	}
	return "parley"
}

// checkNick validates the nick every verb takes against the one grammar in the
// stream package, the same one the peer header preserves, so a nick this
// accepts is the nick the peer sees.
func checkNick(nick string) error {
	if nick == "" {
		return cfgErrf("--nick is required")
	}
	if err := stream.ValidNick(nick); err != nil {
		return cfgErrf("--nick %q %v", nick, err)
	}
	return nil
}

// resolveSpec validates a <scheme>:<rest> spec and returns the canonical
// spelling of it, which is what names the stream's state, and the function that
// opens it. Nothing is opened here, and nothing is created or connected to:
// openStream is this plus the call, and `parley channel --check` is this alone,
// so what is measured is spelled the way a launch spells it.
func resolveSpec(spec string) (canonical string, open func(nick, stateDir string) (stream.Stream, error), err error) {
	if spec == "" {
		return "", nil, cfgErrf("--stream is required, e.g. --stream file:/path/to/chat.ndjson")
	}
	scheme, rest, ok := strings.Cut(spec, ":")
	if !ok || scheme == "" {
		return "", nil, cfgErrf("bad --stream %q: expected <scheme>:<rest>, e.g. file:/path/to/chat.ndjson", spec)
	}
	// A URL scheme is case-insensitive, and the adapters that parse the rest of
	// the spec treat it that way; the factory has to agree, or FILE: and IRC://
	// would be refused here by the one part of parley that never looked.
	scheme = strings.ToLower(scheme)
	switch scheme {
	case "file":
		if rest == "" {
			return "", nil, cfgErrf("bad --stream %q: file: needs a path", spec)
		}
		// The adapter resolves a relative path against the working directory, so
		// the state key has to resolve it the same way: hashing the spec as
		// typed would give `file:./chat.ndjson` one cursor across two different
		// files, and one file two cursors under two spellings. Abs cleans the
		// result, so `a/../b` and `b` are one key too. Symlinks are left alone:
		// resolving them would need the file to exist before it is created.
		path, err := filepath.Abs(rest)
		if err != nil {
			return "", nil, cfgErrf("bad --stream %q: %v", spec, err)
		}
		return scheme + ":" + path, func(nick, _ string) (stream.Stream, error) {
			s, err := file.Open(path, nick)
			if err != nil {
				return nil, err
			}
			return s, nil
		}, nil
	case "irc":
		// The whole spec goes to the adapter: everything after the scheme is
		// its grammar, and the canonical spelling it returns is what keys the
		// state, so one channel on one server keeps one cursor whether it was
		// spelled with the '#', without it, or in another case.
		parsed, err := irc.ParseSpec(spec)
		if err != nil {
			return "", nil, &configError{err: err}
		}
		return parsed.String(), func(nick, stateDir string) (stream.Stream, error) {
			// Every connection for this nick logs into the participant's own
			// account, so that the pump holding the nick and the send answering
			// it are two sessions of one account rather than two clients
			// fighting over a nick. parley owns that account: it registers it on
			// first use with a password nobody types, and keeps it under the
			// state directory.
			s, err := irc.Open(parsed, nick, irc.NewStateCredentials(stateDir, nick))
			if err != nil {
				return nil, &configError{err: err}
			}
			return s, nil
		}, nil
	default:
		return "", nil, cfgErrf("unknown stream scheme %q: the schemes are file: and irc:", scheme)
	}
}

// openStream turns a <scheme>:<rest> spec into an adapter and the canonical
// spelling of that spec, which is what names the stream's state. stateDir is
// the --state-dir value: an adapter that keeps a credential for this
// participant keeps it there, under the same <nick> directory as the lock and
// the cursors.
func openStream(spec, nick, stateDir string) (stream.Stream, string, error) {
	canonical, open, err := resolveSpec(spec)
	if err != nil {
		return nil, "", err
	}
	s, err := open(nick, stateDir)
	if err != nil {
		return nil, "", err
	}
	return s, canonical, nil
}

// closeStream releases whatever the adapter holds open once a verb is done with
// it. The file adapter holds nothing between calls; the IRC adapter holds a
// socket, and closing it sends QUIT, which frees the nick on the server at once
// rather than when the connection eventually times out — so the next run of the
// same nick is not refused by the ghost of the last one.
func closeStream(s stream.Stream) {
	if c, ok := s.(io.Closer); ok {
		_ = c.Close()
	}
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}
