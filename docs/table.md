# Running a table

A table is a few coding-agent sessions, the **seats**, and you, the **chair**, in
one IRC channel thinking about one question. It produces a map of the ideas,
objections and open questions, and decides nothing. This is the chair's guide to
setting one up and running it. The protocol the seats follow is the
`parley-table` skill (`skills/parley-table/SKILL.md`); a sheet template for the
question is `skills/parley-table/sheet.md`.

Placeholders below are in angle brackets: `<chair>` is your nick, `<id>` the
table's id, which is also its channel, `#<id>`, `<target>` the absolute path of
the main checkout of the repo the table thinks about, `<base>` the state
directory (`$XDG_STATE_HOME/parley`, by default `~/.local/state/parley`, or
`--state-dir`), and `<nick>` one seat's nick.

## Once per machine

1. **The binary.** From a parley checkout, `go build -o bin/parley ./cmd/parley`,
   and put `parley` on the `PATH` your terminals start with. Every command
   `parley table new` prints names the binary by its absolute path, so a copy
   that is not on the `PATH` works too.
2. **ergo.** Install it as `contrib/ergo/README.md` says, once. `parley server`
   makes its datastore directory itself.
3. **The skills.** `sh skills/install.sh` links `parley-build`, `parley-table`
   and `parley-line` into `~/.claude/skills` and `~/.agents/skills`. The table skill is then
   `/parley-table` in Claude Code and `$parley-table` in Codex.
4. **The harnesses** your seats run on, `claude` and `codex`, on the `PATH`.
5. **An IRC client** for the chair, any one that connects to
   `127.0.0.1:6667` in plaintext (`contrib/ergo/README.md` lists some).

`parley doctor` checks all of this, and everything else parley depends on,
against the versions on the machine; `--harness claude` or `--harness codex`
(repeatable) limits it to the harnesses you use, and without one it checks Claude
Code and Codex. `parley table new` runs its static checks itself.

## Per table

### Set it up: `parley table new`

```
parley table new <id> --target <target> --chair <chair> --rounds 3 \
  --seat codex-a=codex:<model> --seat claude-a=claude:<model> --seat claude-b=claude
```

`--seat <nick>=<claude|codex>[:<model>]` is repeatable, one per seat. OpenCode
seats are not supported for now (a line takes OpenCode sessions, see `docs/line.md`),
and open to explore. A model is at most 128 bytes of ASCII letters and
digits and `. _ - : / @ [ ] +`, which holds the ids Claude Code and Codex take; it
does not begin with `-` and holds no control character. It is kept in `table.json`, so the
bound is also what keeps that record under the 64 KiB `table end` reads. The
optional flags: `--worktree <dir>` (default
`<base>/tables/<id>/worktree`), `--server 127.0.0.1:6667`, `--state-dir <dir>`
and `--codex-bin codex`.

**What it refuses, before it changes anything (exit 2).** An id outside
`[a-z0-9][a-z0-9-]{0,31}`; a `--target` that is not the root of a git checkout, or whose HEAD already tracks
a `.mcp.json` of any kind, a `.codex/config.toml`, a `.parley-table`, or a
`.codex` that is not a directory (a symlink would send the write elsewhere),
which `table new` writes into the worktree;
a chair or seat nick that is not a valid nick, a seat that repeats another's or
the chair's (nicks match without regard to ASCII case, as the server's do), and
`tables`, `builds` and `lines`, the directories the state base keeps beside the
per-nick ones; a
`--worktree` that is not absolute, exists already, or is inside the target (which
includes the target itself); a
`$` in the target, the worktree or the binary's path, because Claude Code
expands `${...}` in an `.mcp.json`'s command and args; whitespace in the state
directory, because the TABLE line you post names the sheet under it and the seats
split that line on whitespace (name a `--state-dir` without spaces); a path that
goes into `table.json` or an `.mcp.json` and is not valid UTF-8 (`--target`,
`--worktree`, `--state-dir`, the `$TMPDIR` the reply directory is made under, the
binary's path, `--codex-bin`), because `encoding/json` replaces an invalid byte
with U+FFFD and the record would then name another path than the one `table new`
used (the refusal names the flag or the path); a state
directory or a `$TMPDIR`, which holds the reply directory, inside the target or
the worktree; and a Claude seat whose instructions would be cut at Claude Code's
2048-unit cap, which it measures in process the way `parley channel --check`
does. Then it runs `parley doctor`'s static checks for the harnesses the seats
use, the server, ergo when nothing answers at the server (the first step it
prints is `parley server`) and the `parley-table` skill under the root each
harness reads (`~/.claude/skills` for a Claude seat, `~/.agents/skills` for a
Codex one); a `FAIL` ends it (exit 1) with nothing changed, and a `warn` is
printed on stderr. None of this looks at what the state base holds, so a refusal
so far has made no file there.

**The id's lock, and what the base holds for the id.** The first look at the
state base is the id's lock, `<base>/.ids/<id>.lock`, which `table new`, `table
end`, `build new`, `build end` and the `line` verbs all take, for the whole of
their run, before anything else of theirs: the id is one namespace for a table,
a build and a line (the channel `#<id>` and a directory name), and nothing else orders two commands on
it. A `table new` that finds the lock held refuses at once, and does not wait
(exit 1, nothing changed), naming the id and the file: another table, build or
line command for this id is still running, and this one is run again after it. Under
the lock it refuses an id that already has a table under `<base>/tables/<id>/`
("table end takes it down"), a build under `<base>/builds/<id>/` or a line
under `<base>/lines/<id>/` (exit 2,
nothing changed but the lock file). The lock file is in `<base>/.ids/`, which no
nick can be (a nick holds no `.`), and outside the table's own directory, which
`table end` removes. It stays, released, as the seats' lock files do, for the same
reason (below), and `docs/build.md` ("One lock per id") has the rest.

**What it refuses when a seat is in use (exit 1, nothing changed).** The first
step of making the table, after the id's lock, is every seat's nick lock, which
is per nick across all streams. A nick that a running pump or Claude Code session already holds is
refused, naming it: its printed launch would fail after Codex turns were spent.
The locks are held until `table new` is done, rollback included, so a seat
started meanwhile is refused too, and they are free again once it returns. The
lock files stay: no parley code unlinks one, because a lock is held on the
file, and once its path is unlinked another process could create a new file
there and take its own lock while the old one is still held. A refused or failed
`table new` therefore leaves `<base>/<nick>/lock`, and the directory holding it,
as a pump does, even though it rolls back everything else; so does the id's
`<base>/.ids/<id>.lock`. `<nick>` in that path is the nick with its ASCII capitals folded
to lower case, so `Colin` and `colin`, one nick to the server, are one directory
and one lock.

**What it makes.**

- A detached **worktree** of the target's HEAD. No seat edits it, so one serves
  all of them; a seat that wants to try a change builds a disposable copy under
  a temp directory. Nothing is written into the target checkout itself.
- In the worktree, for every table, whatever its seats' harnesses, a
  **`.parley-table`** marker, written as soon as git has made the worktree and
  before any Codex turn runs: the table's id on the first line, and on the
  second the seats' nicks exactly as given, sorted bytewise and separated by
  spaces (a nick's case counts: `Zed` is not `zed`), each line ending in a
  newline (`t1`, then `claude-a codex-a`). Once `table.json` is written, `table
  new` replaces this marker, and the reply directory's, with the same two lines
  and a third, `sha256:` and the hex digest of `table.json`'s exact bytes, each
  through a temp file and a rename at the mode it had; `table.json` is not
  written again. Git lists every worktree of the target, other tables'
  included, and a record could be edited to name one of them, to change its
  seats, or to change any other field; the marker is how `table end` knows the
  worktree is this table's, which seats it was made with, and that `table.json`
  is the record `table new` wrote.
- For Claude seats, the worktree's **`.mcp.json`**: one server, `parley`, whose
  command is the binary and whose args are the channel's launch flags,
  `channel --stream irc://<server>/#<id> --nick ${PARLEY_NICK} --state-dir <base>
  --skill table --chair <chair> --cap <cap> --target <target> --worktree
  <worktree>`. One file serves every Claude seat, because the seat's nick comes
  from the environment `claude` is started in. Claude Code expands
  `${PARLEY_NICK}`; an unset variable stays literal and parley rejects it as an
  invalid nick. The channel has to come from a project `.mcp.json`:
  `--mcp-config` does not work for it.
- For Codex seats, the worktree's **`.codex/config.toml`**: `workspace-write`,
  `on-request`, and `network_access = true` in `[sandbox_workspace_write]`.
  Network access is what lets `send` reach the loopback IRC from the sandbox.
- A **reply directory**, `parley-table-<id>-*` under `$TMPDIR`, mode 0700. Each
  Codex seat's reply file is `<nick>.reply` in it: the file the seat writes each
  post to before it sends. It is outside the worktree and in a place the Codex
  workspace-write sandbox can write, which the rest of the state directory is
  not. It holds
  a `.parley-table` marker with the same lines (the digest is the third, added
  once `table.json` is written), which is how `table end` knows the directory is
  the table's, which seats it was made with, and that the record is the one `table
  new` wrote. A Claude seat's reply directory is the channel's own, made when its
  session starts.
- Each **Codex seat, started.** `codex exec --json` with the seat's table primer
  as the prompt, in the worktree, `workspace-write`, stdin closed, `-m <model>`
  if the seat named one, and network off for that one turn: the seat must not
  post, and you may not have registered yet. Progress is on stderr, with a
  five-minute limit per seat. A turn that reports an `error` or a `turn.failed`
  fails the seat even if codex exits 0, and so does one with no `turn.completed`
  event, or whose output passed the 1 MiB parley reads of it (the end of such a
  run is unseen); the table is rolled back, and the announced thread, if it is
  a UUID, is deleted. parley reads the thread id the session announces
  and records it for the seat in `<base>/<nick>/<stream key>/codex-thread` (the
  nick with its ASCII capitals folded to lower case), the
  record `parley codex --thread auto` reads, and `parley send` reads to tell a
  session that is no longer that thread. The seat's answer to the primer is
  printed for you.
- The **table record**, `<base>/tables/<id>/table.json`: the paths, the chair,
  the rounds and cap, and each seat's harness, model, thread and reply file. It
  holds no stream content.
- The **sheet**: `sheet.md` from the skill install, copied to
  `<base>/tables/<id>/` for you to fill in. Without one it says where to get it.

**The cap** is rounds times seats, the number after `--cap` in the `.mcp.json`
and in each pump line: three rounds of three seats is `--cap 9`. Both fronts a
table uses, `parley channel` and `parley codex`, take `--chair` and `--cap` with
`--skill table` and require both, and `--cap 0` turns the breaker off and keeps
the seal. What the two rules do is under "What to watch".

**If any step fails after the worktree exists** (exit 1), everything the run made
is taken down: the worktree, the reply directory, the table directory, the thread
records, any state directory it created, and each Codex thread it started
(`codex delete --force`, with the `--codex-bin` binary when one was named). It says
what it rolled back, and what it could not; for a thread it could not delete it
prints the command to run by hand, with that binary, shell-quoted.
Interrupting it does the same.

**What it prints** is what is left, in order, with every path, stream, thread id
and model shell-quoted so a line can be pasted into zsh. The rest of this section
is those steps, and why.

### Then, by hand

1. **Start the server**, unless one is already running: one ergo serves every
   table on the machine, each on its own channel, so a second table starts none.
   `table new` prints this step only when nothing answers at `--server`. It runs
   in the foreground, in a terminal of its own, and Ctrl-C stops it:

   ```
   parley server
   ```

   It keeps its accounts in `~/.local/share/parley-ergo`
   (`$XDG_DATA_HOME/parley-ergo` when that is set, unless only the former
   exists; `--datastore <dir>` for another), so they carry over from one run to
   the next, and listens on
   `127.0.0.1:6667` (`--listen` takes another loopback IP and port; a host name
   such as `localhost` is refused).

2. **Join as the chair, registered.** Connect your client to the server as
   `<chair>` and `/join #<id>`. Before any seat or pump connects, check that your
   nick is registered:

   ```
   /msg NickServ INFO <chair>
   ```

   If it is not, `/msg NickServ REGISTER <password>`, and identify on every
   later connect. This is not optional: parley registers a nick the first time
   a process connects with it, so an unregistered chair nick could be taken by a
   seat's process, and the gates would then treat that seat's posts as chair
   lines. Nicks match without regard to ASCII case, as the server's do: a
   chair who posts as `Chair` is the chair a seat was given as `chair`, and a
   seat's own nick is its own in any case. `--chair` is refused, with exit 2,
   when it matches the seat's `--nick` that way.

3. **Each Codex seat** takes two terminals, in the worktree's Codex session and
   beside it:
   1. **The pump**, which carries channel messages into the Codex thread. Without
      it the seat hears nothing. It takes the same gates as the Claude Code
      fronts, registers the nick, and **starts before the TABLE line**:

      ```
      parley codex --stream 'irc://<server>/#<id>' --nick <nick> --state-dir <base> \
        --thread auto --skill table --chair <chair> --cap <cap>
      ```

      `--thread auto` reads the record `table new` made and names the thread it
      found on its start line. `auto` is reserved, so a session literally named
      `auto` is passed by its UUID. A seat that posts from a session other than
      the recorded thread, after `/clear` or in a sub-agent, gets a warning from
      `send`: its channel posts will not reach it.
   2. **The session**, resumed from the thread `table new` started:

      ```
      codex resume <thread> -C <worktree> -a on-request
      ```

      The resume line carries no `-c`: a `-c` forces Codex in-process, with a
      ten-second queue poll, instead of the shared daemon the pump's `codex queue`
      reaches at once. Then type `$parley-table`. The seat tells you at its
      terminal that it is waiting for the TABLE line.

4. **Each Claude Code seat**, one terminal each, in the worktree, with its own
   nick. Before the launch command:

   - The launch needs `--dangerously-load-development-channels`. A server in
     `.mcp.json` is a channel only when the launch names it, and parley's is
     not on Anthropic's allowlist. Without the flag the MCP server still
     connects and `/mcp` still lists `parley`, but the startup screen has no
     "Channels (experimental)" notice and channel messages never arrive: the
     seat hears nothing and shows no error.
   - parley was tested with the Claude Code version `parley doctor` names as
     last verified.
   - Channels need a claude.ai login or a Console API key, not Amazon Bedrock,
     Google Cloud's Agent Platform (Vertex) or Microsoft Foundry. On a Team or
     Enterprise organization an Owner must enable them first, under
     Organization settings > Claude Code > Channels (the managed setting
     `channelsEnabled`); while it is off the server connects and no channel
     message arrives, even with the flag.

   ```
   cd <worktree>
   PARLEY_NICK=<nick> claude --dangerously-load-development-channels server:parley
   ```

   with `--model <model>` where the seat named one. Start them **one at a time**:
   approve the project MCP server `parley` when asked, check that `/mcp` shows it
   connected and that the dim notice below the startup banner reads `Channels
   (experimental) messages from server:parley inject directly in this session`,
   then type `/parley-table`, and only then start the next. A second
   seat started before the first approved the server never loads it. The seat tells
   you at its terminal that it is waiting for the TABLE line. A Claude Code seat
   silent from the start has an over-cap or failing channel, which shows only in
   `/mcp`; or a launch without the flag, or channels off for its organization,
   which show as `parley` connected with no "Channels (experimental)" notice
   (a startup warning about the organization stands in for it in the second
   case). A hand-written `.mcp.json` is checked with
   `PARLEY_NICK=<nick> parley channel <the .mcp.json args> --check`, with the nick
   written out where an arg is `${PARLEY_NICK}`.

5. **The gate.** Every seat has said at its own terminal that it is waiting for
   TABLE. Do not post TABLE before all of them have. A seat that posts before
   TABLE, even a "ready", opens its own seal and loses its independent round 1.
   Seats confirm at their terminals and never in the channel. Every Claude
   seat's `/mcp` shows `parley` connected and the startup notice `Channels
   (experimental) messages from server:parley inject directly in this session`
   is below its banner: `/mcp` alone also passes on a launch without
   `--dangerously-load-development-channels`, which never hears the channel.
   Each Codex seat's `/status` shows `workspace-write` and network on, and its
   model if `--seat` named one; the model check is unverified on a resumed
   session, so confirm it at the first table that uses one.

6. **Fill in the sheet and open the table.** Fill in `<base>/tables/<id>/sheet.md`
   and post one line in the channel:

   ```
   TABLE <id> <absolute path to the sheet>
   ```

   The sheet names the seats; there is no `--seats` flag. The order of SEATS
   picks the checkpoint seat: the one that posts the checkpoint and that a bare
   `MAP` asks for the map. Its optional `SCOPE:` line says
   what an answer may cover, for example one call site or every site of the
   same shape; a seat that sees two seats disagree about scope asks you instead
   of arguing it.

## Chairing

Every command is one line, because an IRC client sends each line as its own
message. Plain language works anywhere.

| line | effect |
|---|---|
| `TABLE <id> <path>` | opens the table |
| `CONTINUE 2` | two more rounds, then another checkpoint |
| `MAP` | the checkpoint seat (the first in SEATS) writes the map; the others get one post each, `FIX` or `REPLY` lines, or `PASS` (a `FIX` corrects the map; a `REPLY` answers an `ASK` that was still unanswered when the last round closed), then the map seat posts the final map (first line `FINAL MAP`) with the accepted corrections folded in and each `REPLY` recorded; nobody posts after it until you speak |
| `MAP <nick>` | the same, with that seat writing the map |
| `DROP <id>` | that idea is off the table for good |
| `END` | the table is over |
| `DEBRIEF` | after END: each seat posts once on what the protocol got wrong |
| `<nick>, emit the final map as a note` | the only way anything reaches the ledger; it is the final map, not the first |

## What to watch

- **Silence means the breaker tripped.** After `--cap` posts by anyone but you
  since you last posted, each front holds the deliveries that follow until you
  post, so the seats stop waking. It is a backstop for runaway cost; the
  checkpoint is still the first seat's job and arrives in the channel.
- **Any line from you releases what the breaker held, and resets the count.**
  The held deliveries go out in stream order, ahead of your line, and the count
  starts again, so a question to one seat also buys a full new cap. Posts held
  by a seat's seal are not released by it: they stay held until that seat posts.
  Only `END` stops a table.
- **A front restart re-seals that seat and loses what it held.** Held posts
  live in the front's memory only. A restarted seat hears only you until it
  posts again, so ask it to post, and ask the others to repeat anything it
  missed; your client's scrollback has what was said. A gap in the channel's
  history, which the seat's next delivery reports, loses the held posts the same
  way, because the transport's ids may start over across it. A delivery that
  never reaches its seat loses nothing: the holds are as they were, and the
  delivery is made again.
- **An over-cap channel shows only in `/mcp`.** `table new` measures each Claude
  seat's instructions before it changes anything, so this is a hand-written
  `.mcp.json`'s failure.
- **Cost.** Each post wakes every other seat.

## After the table

- `git status` in `<target>` shows nothing new. The seats edit no file in the
  target or in the worktree and make no worktree of their own. Each writes its
  posts to its reply file, outside the repo, and may leave disposable copies of
  the target under a temp directory; delete those.
- Close the seats and the pumps. `parley table end` takes the id's lock first
  and then every seat's lock, without waiting, and refuses while one is held,
  naming the id or the nick, so a seat still running is not left talking to
  nothing; the locks it did take are held until the table is down, so a seat
  started meanwhile is refused too, and so is a `table new` or `build new` for
  the id. The lock files stay, as they do after `table new`.
- Take the table down:

  ```
  parley table end <id>
  ```

  with the same `--state-dir` as `table new` if it had one, and the same
  `$TMPDIR`. It reads `table.json` only after it holds the id's lock, so the
  record it takes down is the one no other command can be changing. It removes
  the seats' thread records, the worktree, the reply
  directory and `<base>/tables/<id>/`, the filled-in sheet with it, in that
  order: the markers in the worktree and the reply directory are what show the
  thread records are the table's, so an end that stops partway still has them
  when you run it again. It refuses
  (exit 1) when the worktree holds changes, ignored files included, beyond the
  files `table new` wrote:
  the `.parley-table` marker, always, and what its record's seats say, the
  `.mcp.json` when a seat is Claude's and the `.codex/config.toml` when one is
  Codex's, and nothing else, so any other file in `.codex/`, or a `.parley-table`
  anywhere but the root, is a change; look at them first, or `--force`. When
  every change is an ignored file (and git's status was not cut off, so there
  can be no others) the refusal says so: `the worktree <path> has changes
  beyond the files table new wrote, and all <n> are ignored files (<list>): a
  tool's session hook may have written them; look at them, then --force to
  remove it anyway`. Any other mix of changes gets `the worktree <path> has
  changes beyond the files table new wrote (<list>); look at them, or --force
  to remove it anyway`. It is the same refusal, exit 1, either way.
- **On a machine with Director,** its session hook writes `.director/repo-key`
  and `.director/workstream-id` into the table worktree when the first seat
  starts. They show as untracked or ignored, depending on the target's
  `.gitignore`, and `table end` refuses on them, naming up to five changes.
  Check that those two are all it lists (when they are ignored, the refusal
  says all of the changes are ignored files and that a tool's session hook may
  have written them), then run it again with `--force`.
- `table end` deletes only what it can show is the table's, `--force` or not,
  because the record it reads is a file, and everything in it is a claim: whose
  locks it takes, whose thread records it removes and under which stream, what
  it deletes. Before it takes a lock or removes anything it reads `table.json`
  once, through its bound, and holds it to the markers: every marker it can read
  (the worktree's, when the worktree is there and git lists it, and the reply
  directory's, when that is there) must hold exactly the table's id, exactly the
  record's seat nicks, in the form `table new` wrote them, and on its third line
  the digest of the bytes just read. A record with a seat added, removed or
  renamed is refused (exit 1, "will not remove", naming the marker and saying
  the record's seats differ from the ones the table was made with); so is a
  record changed in any other field, one byte included, even a blank added at
  the end (the same refusal, saying the record is not the one the table was made
  with); so is a marker of only two lines, which a `table new` that never
  finished left (it says so). `--force` lifts none of them; put `table.json`
  back, or finish by hand what the message names. The roster check is kept
  beside the digest rather than folded into it, for its more exact message; a
  seat edit is refused as the roster before the digest is looked at. When
  neither marker can be read, because the worktree and the reply directory are
  both gone, there is nothing to compare and it goes on, but nothing then
  vouches for the stream and the threads `table.json` names, so it removes no
  thread record: its report lists each one that is there under "Left in place",
  and you remove it yourself if it is this table's. It removes the
  worktree only if git
  lists it as one of the target's and its `.parley-table` is a regular file, not
  a link, holding exactly the table's id, seats and digest (a worktree with the marker
  missing or holding another id, another live table's for one, is refused,
  naming what the marker said; remove it yourself if it is the table's), or, when git cannot say
  because the target is gone (its path is not there any more), if it lies under
  `<base>/tables/<id>/`, and
  deletes a worktree git cannot remove by hand only under that directory. That
  fallback is for a target that is gone and for nothing else: any other failure
  of `git worktree list`, a target that is there with a repository git cannot
  read included, refuses the worktree, quoting git's error, and says the path
  being under the table's directory is not enough. A worktree deleted without git
  leaves git's registration of it where the repository's git directory is, and
  the report says so, with the `git worktree prune` that drops it. A
  worktree that is already gone needs no marker. A `table.json` that lists no
  seats is not a record `table new` wrote, and is refused as not valid. It removes the reply
  directory only if it sits directly in `$TMPDIR`, is named for the table, and
  holds the marker file `table new` wrote, with the table's id, seats and digest in it. Given a
  marker it read and matched, it removes
  a Codex seat's thread record only if the record holds the thread that
  `table.json` gives the seat (one that is not there is fine); a record that
  holds another thread (it is another table's, or no one's), or is not a
  thread id on one line that ends in its newline, is refused. Anything else it refuses, exit 1, naming the path, before it removes
  anything. It reads `table.json` through 64 KiB, far more than a table's
  record is, and refuses a larger file as corrupt.
- `table end` never touches a Codex session. It prints a `codex delete <thread>`
  line for each Codex seat; run those if you do not want to keep that history.
  The record keeps no `--codex-bin`, so those lines say plain `codex`; if the
  table was set up with another binary, run that one. (`table new`'s own
  rollback, which has the binary, names it, shell-quoted, in the command it says
  failed and in the line it prints for you.)
- The server can stay up for the next table; it is one per machine, and Ctrl-C in
  its terminal stops it. The channel history stays in ergo's own store, never in
  the repo, and ergo keeps it in memory, so stopping the server forgets it
  (`contrib/ergo/README.md`). Save the map from your client first if you want it.

Change no Director hook or setting for a table: the skill tells each seat what
to do about them. A table needs nothing of the target beyond parley; the
two-peer mode, a build, which promotes into Director's ledger, has its own guide
and needs the target adopted in Director: `docs/build.md`.

## What parley trusts

The table commands guard each input according to who could have written it.

- **Untrusted: what a peer, a model or another program produced.** Channel
  messages (the header rule), Codex's output (its JSONL, the thread id it
  announces, its answers), the IRC server's replies, `$CODEX_THREAD_ID`, git's
  output, and the files the seats create in the worktree. parley bounds what it
  reads of each, checks an id before it reaches a command line, quotes or strips
  control characters before they reach your terminal, and removes no file a seat
  made, ignored ones included, without `--force`. It never lets a file in the
  worktree choose what git runs against: a seat could rewrite the worktree's
  `.git` file to point git at a directory of its own whose config names a
  program as `core.fsmonitor`. So `table end`'s dirty check names the worktree's
  git directory, the target's common directory and the work tree from the
  target's side (`GIT_DIR`, `GIT_COMMON_DIR`, `GIT_WORK_TREE`, found among the
  target's own `worktrees/*/gitdir` files, never in the worktree), turns off
  `core.fsmonitor` and `core.hooksPath` on the command line, and does not let
  `git status` look inside a submodule: a gitlink whose commit changed is a
  change, what is dirty inside one is not asked.
- **Trusted: parley's own state.** `table.json`, the thread records, the two
  markers, the lock files (each seat's, and the id's `<base>/.ids/<id>.lock`) and
  the reply directory are written by parley, under
  your account, in directories only you can write. parley checks them for
  damage: a size bound, a regular file, a well-formed value, and the sha256 seal
  that ties `table.json` to the markers. A corrupt or stale record, or one left
  by a `table new` that never finished, therefore cannot make `table end` remove
  another table's files; with no marker left to check the record against, it
  removes no thread record and no git registration, and lists them instead.
- **Out of scope: a deliberate edit of that state.** Whoever can rewrite
  `table.json` and the markers by hand is you, or a program running as you, and
  can delete those files directly. parley does not try to stop a crafted record
  from steering `table end` or from changing what it prints. A seat's own
  harness keeps it out of the rest of the state base. The table's worktree sits
  in the state base, and Codex's workspace-write sandbox writes it, and the
  temporary directories; `table.json`, the markers, the lock and cursor files
  and the other tables are outside its roots. Claude Code asks you before it
  writes outside its working directory. Another local user is kept out by the
  directories' 0700 mode.
