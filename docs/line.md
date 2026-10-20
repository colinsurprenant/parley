# Running a line

A line is a few long-running coding-agent sessions, each on its own job and in
its own checkout, and you, in one IRC channel for as long as the work runs, days
included. The sessions poke each other ("phase 2 is done, see record X") and ask
questions, and when you open a discussion they work a question through together;
you read along and steer. This is your guide to setting one up and
running it. The protocol the sessions follow is the `parley-line` skill
(`skills/parley-line/SKILL.md`).

A line is neither a table nor a build. A table is one question asked in rounds,
sealed so the seats answer alone first, and it decides nothing; a build is two
peers splitting one piece of work, each on a worktree parley made for it. A line
has no seal, no rounds, no target and no worktree of parley's: the sessions work
in the checkouts they already use, and you are in the room without chairing it.
Use one when you are the one carrying "go read what the other one left you" from
terminal to terminal. The work and its records stay where they live, in a
ledger, a commit, a PR; the line only points at them.

`parley line new` makes a line and `parley line add` puts a session on it, the way
`parley table new` sets a table up and `parley build new` a build; `parley line
drop` takes a session off, and `parley line end` takes the line down. They write
what the steps below would have you write by hand, check it first, undo it if a
step fails, and print what is left for you. What they do not cover is in "By
hand", after the steps.

Placeholders are in angle brackets: `<channel>` is the line's channel,
`#<channel>`, which is also its id, `<chair>` your nick, `<nick>` one session's,
`<base>` the state directory (`$XDG_STATE_HOME/parley`, by default
`~/.local/state/parley`, or `--state-dir`), `<checkout>` the absolute path of a
session's own checkout, and `<cap>` and `<window>` the breaker's cap and window.
Example nicks are `plan`, `build` and `review`.

## Once per machine

The same as for a table, from `docs/table.md` ("Once per machine"): the binary
on the `PATH` your terminals start with, ergo, the harnesses your sessions run
on, and an IRC client for you. `sh skills/install.sh` links all three skills,
`parley-build`, `parley-table` and `parley-line`, into `~/.claude/skills` and
`~/.agents/skills`; the line skill is then `/parley-line` in Claude Code and
`$parley-line` in Codex, and OpenCode, which reads both roots, loads it with its
skill tool. `parley doctor` checks the links with the rest. A line takes OpenCode
1.x and 2.x sessions, and `line add` tells which by `opencode --version`. For 1.x
install `npm i -g opencode-ai@1` or use the installer at `opencode.ai/install`
(`curl -fsSL https://opencode.ai/install | bash`); Homebrew core now ships OpenCode
2, and its installer is `opencode.ai/v2/install`. Support for 2.x is verified
against 2.0.32 in OpenCode's simulation mode, which blocks all model traffic, and
**not yet on a live model**: run one 2.x line live before relying on it.

## Per line

1. **Start the server**, unless one is already running: one ergo serves every
   table, build and line on the machine, each on its own channel. It runs in the
   foreground, in a terminal of its own, and Ctrl-C stops it:

   ```
   parley server
   ```

2. **Join as yourself, registered.** Connect your client to `127.0.0.1:6667` as
   `<chair>` and `/join #<channel>`. Before any session or pump connects, check
   that your nick is registered:

   ```
   /msg NickServ INFO <chair>
   ```

   If it is not, `/msg NickServ REGISTER <password>`, and identify on every later
   connect. A line runs for days, so let the client identify for you with SASL.
   Some clients still need their nickname and username fields filled in when
   SASL is set: left empty, the connection authenticates but never completes,
   and every command answers "You need to register before you can use that
   command". A line under your registered nick is the only line that steers a
   session; an unregistered nick could be taken by a session's process, and its
   lines would then pass for yours. Nicks match without regard to ASCII case, as
   the server's do. `--chair` is refused, with exit 2, when it matches a
   session's own `--nick` that way.

3. **Pick the cap and the window.** The breaker has two rules, both counting
   posts by anyone but you, the sessions' own included. A burst: more than
   `<cap>` posts within `<window>`. A slow loop: more than five times `<cap>`
   since you last posted, however slowly they came. When either happens, each
   front holds that post and every later one until you post. Nothing releases
   them when time passes, so a loop, fast or slow, stays stopped until you look.
   The cap says how many posts a burst may have; the window how short a stretch
   counts as a burst. A line runs for days and its pokes are slow, spread over
   hours, while a loop is fast, so the pair tells them apart: 8 posts in 10
   minutes (`--window 10m` is the default, any Go duration such as `90s` or `1h`
   works) lets a few sessions finish work and answer each other, and holds a
   back and forth that does not end within minutes; the second rule, 40 posts
   since you last posted at a cap of 8, ends the exchange that runs on at a pace
   too slow for the first, such as a post every two minutes, and is why a line
   left alone does not run forever. A larger cap or a shorter window holds later.
   `--cap 0` turns both off, and then nothing stops two sessions from waking each
   other for as long as they have something to say; the window must still be above
   zero.

4. **Make the line.** The channel names the line, and the line is a record of its
   facts, which every session added to it inherits:

   ```
   parley line new <channel> --chair <chair> --cap <cap> [--window <window>] \
     [--server 127.0.0.1:6667] [--state-dir <base>] [--codex-bin codex] [--opencode-bin opencode]
   ```

   The id is the channel and a directory name, `[a-z0-9][a-z0-9-]{0,31}`, and it is
   one namespace with the tables and the builds: an id that already has a table, a
   build or a line is refused, with exit 2. `--chair` is refused when it is not a
   valid nick, and `--cap` is required (`--cap 0` for no breaker). The command runs
   `parley doctor`'s static checks for the server, and for ergo when nothing
   answers at the server, makes `<base>/lines/<channel>/line.json` with no
   sessions in it, and prints the steps that remain: the server if nothing answers,
   your join and the NickServ check of steps 1 and 2, and one `parley line add` per
   session. `--codex-bin` and `--opencode-bin` are for the Codex and the OpenCode
   sessions you add later: each is resolved now and kept in the record, since
   `line add` takes no such flag, and the lines `line add` and `line drop` print
   spell it that way.

5. **Add each session.** One command each, with the session's own checkout:

   ```
   parley line add <channel> --nick <nick> --harness claude|codex|opencode [--model <model>] \
     --checkout <checkout> [--state-dir <base>]
   ```

   The chair, cap, window, server and state directory come from the line's record;
   `--state-dir` only says where to find it. The command refuses, with exit 2 and
   nothing changed, a nick that is invalid, reserved (`tables`, `builds`, `lines`),
   the chair's, or already in the line, all without regard to ASCII case; a model
   outside the charset `table new` takes (so `provider/model#variant`, which OpenCode
   2 takes, is not given here), or for OpenCode one that is not `provider/model`; a `<checkout>` that is not an absolute, existing directory,
   or holds a `$`; a Codex or OpenCode session with `$TMPDIR` inside the checkout;
   and a launch file that is in the checkout and is not this line's, below. A nick whose lock a running session or pump
   holds is refused with exit 1, and so is a machine that `parley doctor`'s static
   checks fail on, the `parley-line` skill not being linked under the root the
   harness reads among them, again with nothing changed. The checkout is resolved
   once, through any links in its path, and the resolved path is the one recorded
   and printed, so two spellings of one directory are one checkout. The launch
   file is the one thing `line add` writes in your checkout (an OpenCode session
   has none, and nothing is written there for it), and only where there is none:
   it never overwrites a file, writes through a link, or shares a file it cannot
   prove is the one it wrote, which it proves with the digest it keeps in the
   record.

   **A Claude Code session** gets an `.mcp.json` in `<checkout>`:

   ```
   {"mcpServers":{"parley":{"command":"/abs/path/to/parley","args":[
     "channel","--stream","irc://127.0.0.1:6667/#<channel>","--nick","${PARLEY_NICK}","--state-dir","<base>",
     "--skill","line","--chair","<chair>","--cap","<cap>","--window","<window>"]}}}
   ```

   `${PARLEY_NICK}` stays as written: the session's nick comes from the environment
   `claude` is started in. A line takes no `--target` or `--worktree` and refuses
   them, with exit 2. The command also checks that the launch fits Claude Code's
   2048-unit cap on the channel's instructions, as `parley channel --check` does,
   and refuses one that does not; only a very long `--state-dir` or binary path puts
   a line over it. A second Claude Code session in the same checkout shares the
   file the first wrote. A checkout that has an `.mcp.json` of its own is refused:
   add the `parley` server to it by hand, below. The command prints how to launch
   the session:

   ```
   cd <checkout>
   PARLEY_NICK=<nick> claude --dangerously-load-development-channels server:parley
   ```

   with `--model <model>` after it when you gave one. The flag, the login Channels
   need and the organization setting are the table's: see step 4 of "Then, by
   hand" in `docs/table.md`, including how a launch without the flag looks (the
   server connects, nothing arrives, no error). Approve the project MCP server
   `parley` when asked, check that `/mcp` shows it connected and that the dim
   notice below the startup banner reads `Channels (experimental) messages from
   server:parley inject directly in this session`, then type `/parley-line`. A
   conversation that is already under way joins the line with the same launch plus
   `--resume <session-id>`, and keeps its history; the id skips the resume picker.
   Relaunch it between tasks, since exiting stops whatever the session has
   running, its subagents included.

   **A Codex session** takes two terminals and has no connect-time channel, so its
   text is the primer, its first message, and `line add` starts the session on it:

   - It writes `<checkout>/.codex/config.toml`:

     ```
     # written by parley line add for line <channel> (<16 hex digits>)
     sandbox_mode = "workspace-write"
     approval_policy = "on-request"

     [sandbox_workspace_write]
     network_access = true
     ```

     The first line is a comment with a nonce of its own: the rest is the same text
     for every checkout, so only the comment makes the file's digest, which the
     record holds, a proof that `line add` wrote it and not a copy of it.
     **This turns network access on for Codex in that checkout**, for every Codex
     session there and not only this one. `parley send` has to reach the loopback
     server from inside the sandbox, which is why; it is what `parley table new`
     writes for a seat. Codex loads the file after you accept its trust prompt for
     the directory. The command says so in its output, and `parley line drop` takes
     the file out again.
   - It makes a private reply directory `parley-line-<channel>-*` under `$TMPDIR`
     (mode 0700, outside the checkout and where the sandbox can write), holding a
     marker with the line and the nick, and the primer carries the path of the
     session's reply file in it.
   - It starts the thread with `codex exec --json` on the primer, in a read-only
     sandbox (`-s read-only`) and with the network off for that one turn, so the
     session can neither post before its pump is up nor write in your checkout,
     with `--skip-git-repo-check`, since a checkout need not be a git checkout,
     stdin closed and a five-minute limit, `-m <model>` when you gave one. The
     turn must have
     completed; a thread that did not is deleted and the add fails and undoes
     itself. The thread id it announces is recorded for `--thread auto`, and
     printed with the session's answer to the primer.

   Then it prints the two commands that remain. The pump carries channel messages
   into the thread, takes the same breaker as the Claude Code front and registers
   the nick, and starts before the session posts:

   ```
   parley codex --stream 'irc://127.0.0.1:6667/#<channel>' --nick <nick> --state-dir <base> \
     --thread auto --skill line --chair <chair> --cap <cap> --window <window>
   ```

   and the session, resumed from the thread, in another terminal:

   ```
   codex resume <thread> -C <checkout> -a on-request
   ```

   The resume line carries no `-c`: a `-c` forces Codex in-process, with a
   ten-second queue poll, instead of the shared daemon the pump's `codex queue`
   reaches at once. Each approval prompt in this terminal stops the run until you
   answer it, because the pump delivers between Codex turns only. Codex keeps a
   checkout's git directory read-only in its sandbox, so `git add`, `git commit`,
   `git merge` and `git reset` are refused there; the primer tells the session to
   ask for escalation for each, and each is an approval prompt here. Check that
   `/status` shows `workspace-write` and network on, then type `$parley-line`.

   **An OpenCode session** (1.x or 2.x) takes two terminals and has no
   connect-time channel either, so its text is the primer, its first message, and
   `line add` starts the session on it. It writes nothing in `<checkout>`: no launch
   file and no exclude line. The two versions differ in how the session is started
   and reached; where the text below says 1.x or 2.x it is that version's, and
   where it does not, both.

   - It makes the same private reply directory `parley-line-<channel>-*` under
     `$TMPDIR` as for a Codex session (mode 0700, a marker with the line and the
     nick), and the primer carries the path of the session's reply file in it.
   - It starts the session with `opencode run --format json --title "parley line
     <channel> <nick> <token>"` in the checkout, `-m <model>` when you gave one (an
     OpenCode model is `provider/model`, as `opencode models` lists them), and a
     five-minute limit. The token is the random part of the reply directory's name,
     which no other add shares, so a session found by its title is this run's. The
     primer goes in on stdin, because `opencode run` wraps a message argument that
     holds spaces in double quotes and escapes the quotes in it, and the session's
     first message should be the primer as it is. Your environment is passed on as
     it is. The turn must have completed: every event of the run must carry the
     same session id, of the shape OpenCode makes (an event with none fails the
     turn), and the last step must end with `stop`. A session that did not
     complete, or did not end within the limit, is deleted (`opencode session
     delete`, run from the checkout), and the add fails and undoes itself. Events
     that name two sessions do not say which one the run made, so nothing is
     deleted then: the error names both ids, and you look with `opencode session
     list` in the checkout. A run stopped before its first event has made a session
     it never named: `opencode session list --format json -n 200` is asked for,
     from the checkout (OpenCode lists the sessions of the project of the directory
     it runs in), and the session with exactly this title and directory, made after
     the run began, is deleted too; the log says when that list ran and found
     nothing. `opencode run` has no terminal to ask in, so a permission ask in that
     turn is refused (OpenCode 1.18.30 refuses it), not answered. The session id is
     recorded for `--session auto`, and printed with the session's answer to the
     primer.
   - **On 2.x the session id is parley's own.** `opencode run --session <id>`
     "continues the session, or creates it if it does not exist" (2.0.32), so
     `line add` makes the id the way OpenCode does (`ses_` and 26 letters or digits,
     newest first) and passes it, with the title, `-m` and the primer on stdin as
     above. Nothing has to be read back from the run to know which session it made,
     so nothing is searched for: every event must carry that id, and if the add
     fails or you interrupt it, `opencode session delete <id>` is run from the
     checkout (an answer of "Session not found", for a run that failed before it made
     the session, is not a failure). A 2.x run starts the user's one background
     service if none runs, and the session lives in it.
   - **A limit of creating the session through `opencode run`, on 1.x:** it carries
     OpenCode's own session-level denies for the `question` tool and for plan mode
     (1.18.30 sets them when the run is not interactive). The session cannot ask
     you a structured question or enter plan mode, and asks in plain text; nothing
     parley does changes that. **On 2.x it adds no rule to the session** (the
     session has no `permissions`, 2.0.32 read from its source and from a session it
     made): the run answers the asks of its own turn by rejecting each one, with a
     message that tells the model to carry on without the action, and cancels each
     question the same way, and from the first message you send it the session's
     agent and your OpenCode configuration decide.

   Then it prints the commands that remain, in this order, because the pump checks
   as it starts that the server answers and knows the session. **On 1.x**, in the
   session's checkout, the TUI, which serves its HTTP API only while it runs with
   `--port`:

   ```
   cd <checkout>
   opencode --port <port> --session <session>
   ```

   and the pump, in another terminal:

   ```
   parley opencode --stream 'irc://127.0.0.1:6667/#<channel>' --nick <nick> --session auto \
     --port <port> --skill line --chair <chair> --cap <cap> --window <window> --state-dir <base>
   ```

   `<port>` is a port on `127.0.0.1` that was free when `line add` ran, the same in
   both; give both another if the TUI says it is taken. **On 2.x** there is no
   `--port`: the TUI, run in the checkout, reaches the one background service
   (and starts it if none runs), and the pump finds that service itself:

   ```
   cd <checkout>
   opencode -s <session>
   ```

   ```
   parley opencode --stream 'irc://127.0.0.1:6667/#<channel>' --nick <nick> --session auto \
     --skill line --chair <chair> --cap <cap> --window <window> --state-dir <base>
   ```

   Without `--port` the pump reads `service.json` in `$XDG_STATE_HOME/opencode`
   (`~/.local/state/opencode` if that is not set), which the service writes with
   its address and its password, so both commands need the same `HOME` and
   `XDG_STATE_HOME`. The pump refuses the file unless it is a regular file you
   own that nobody else can read or write, names an `http` address on a loopback
   IP, and the server there answers as the service the file describes (same
   process id and version). **Authentication on 2.x:** the service's password is
   the file's, sent to that address as HTTP basic credentials under the user
   `opencode` and never printed, so there is nothing to set; for an `opencode
   serve` you started yourself, give `--port` and set `OPENCODE_PASSWORD` (or the
   older `OPENCODE_SERVER_PASSWORD`) to the password it was started with. **On 1.x**
   `OPENCODE_SERVER_PASSWORD` is optional, as below. With `--port` the pump probes
   the address and tells the version by the answer: OpenCode 1's health JSON, else
   OpenCode 2's `/api/info`.

   The pump takes the same
   breaker as the other fronts and registers the nick. It posts each delivery into
   the session as one user message, the peer header included, **only while the
   session is idle**: on 1.x OpenCode has no queue, and a message sent during a turn
   may be folded into that turn; 2.x queues the post and starts a turn of its own
   for it, and the pump waits for an idle session there too, so that what arrives
   while the session works is held in the stream and goes over as one delivery when
   the turn ends. A delivery counts as
   made once the session shows it (1.x: its history holds a user message with those
   bytes; 2.x: the message the pump posted under its own id holds them), and a crash
   delivers again rather than drop (`docs/reference.md`, "opencode: the OpenCode
   front"). On 1.x a line you submit in the TUI at the instant of a post is merged
   into your own turn, and the server's API cannot prevent that; on 2.x it is not
   merged, it is a turn of its own. A delivery is posted whole, with one bound: a
   message whose rendering is over 128 KiB, and a message that would take one
   delivery past 512 KiB, is not posted, and a stub takes its place, its own
   header over a one-line notice naming it, while the full text stays in the stream
   for you to read.

   Three things the pump does not do for you. **On 1.x every write of the reply
   file raises OpenCode's `external_directory` ask**, since the reply directory is
   under `$TMPDIR`, outside the checkout, and "Allow once" asks again on the next
   reply (all 11 writes of a live run asked): answer **"Allow always"** for the
   reply directory in the TUI, where the ask appears, so that later replies do not
   stop on one. On 1.18.30 "always" lasts as long as the TUI's server runs: after a
   restart of the TUI, the next write asks once more (seen live).
   **On 2.x it asks nothing**: `external_directory` is allowed by default there
   (`GET /api/agent/build` on a 2.0.32 server shows the base policy, and the
   OpenCode permissions docs say "the default policy allows `external_directory`"),
   so the reply file is written without a prompt. To be asked, as 1.x asks, add
   this rule to your `opencode.jsonc` (rules are ordered and the last match wins, so
   a narrower `allow` after it can exempt the reply directory):

   ```
   { "permissions": [ { "action": "external_directory", "resource": "*", "effect": "ask" } ] }
   ```

   **On 1.x, `OPENCODE_SERVER_PASSWORD`**: the server `--port` opens answers any
   process on this machine unless you set a password, so set it (and
   `OPENCODE_SERVER_USERNAME` if you want a name other than `opencode`) in the
   environment of both commands; the pump reads the variables the TUI does. (2.x:
   above.) And
   there is no `/parley-line`: OpenCode loads a skill with its skill tool, which the
   primer tells the session to use, so check that the session's answer to the
   primer, printed by `line add`, shows it did, and ask it to if not.

   **In the Claude Code and Codex cases**, when the checkout is the root of a git
   checkout, the command appends `/.mcp.json` or `/.codex/config.toml` to the file `git rev-parse
   --git-path info/exclude` names, the exclude file every checkout of the repo
   shares, unless that exact line is already there, so the file stays out of `git
   status` without a tracked file touched. The file is made when the repository
   has none. A directory that is not a git checkout is allowed and gets no such
   line, and neither does one that is only inside a repository, where the anchored
   line would not match. An exclude file that is a link is not written through, and
   one the line would take past 4 MiB, the most `line drop` reads to take it out
   again, is not written to; the command says so and carries on. Linked worktrees
   of one repository share one exclude file, so a session of this line in another
   worktree that finds the line there takes it as the line's own, and it is taken
   out only when the last file it hides is gone ("Taking it down"). If anything
   fails, or you interrupt it, what this one command made is taken down again: the
   launch file, its line in the exclude file (and the file, if it made it), the
   reply directory, the thread record and the Codex thread (`codex delete
   --force`), or the session record and the OpenCode session (`opencode session
   delete`). What a Codex primer turn did is not on that list because it can write
   nothing in the checkout: its sandbox is read-only. An OpenCode primer turn has no
   such sandbox: it runs with the session's ordinary permissions and your own
   OpenCode plugins and hooks, which may write in the checkout as they do in any
   session of yours, and OpenCode's bash runs without approval by default ("What
   parley trusts").

6. **Give each session its role**, in one line each, at its terminal or in the
   channel as `<chair>`: what the session does, and what it tells the others.

   ```
   plan:   you own the schedule; tell build when a phase changes
   build:  you implement phase 2 from handoff 01M…; tell plan when it lands, and review when a branch is ready
   review: you review what build says is ready; ask build about intent, ask me about anything it cannot settle
   ```

   A newer line replaces an older one. A session asks you when something falls
   between roles. A role describes standing duties and starts nothing by itself,
   so say separately when a session should begin; the skill also asks each
   session to record its role where its next session will find it.

## By hand

The verbs cover a checkout with no launch file of its own. For one that has an
`.mcp.json` of its own, or a Codex or OpenCode session you want to start yourself,
you write what `line add` would have, and the pump takes the thread or session id
in place of `auto`, since no record of it is made. Nothing in `<base>/lines/` knows about a
session you set up this way, so `line drop` and `line end` leave it alone, and you
take it down by hand too ("Taking it down", below).

- **A Claude Code session.** Put the `parley` server of step 5 in the checkout's
  `.mcp.json`, adding it to the file if there is one, and keep the file out of `git
  status` with the exclude file every checkout of the repo shares:

  ```
  echo /.mcp.json >> "$(git rev-parse --git-path info/exclude)"
  ```

  Check that the launch fits the cap on the channel's instructions, with the nick
  written out where an arg is `${PARLEY_NICK}`:

  ```
  PARLEY_NICK=<nick> parley channel <the .mcp.json args> --check
  ```

  and launch it as step 5 prints.

- **A Codex session.** Write the `.codex/config.toml` of step 5 (and hide it with
  `/.codex/config.toml` in the exclude file). The primer is the session's first
  message. Its reply file goes in a private directory made with `mktemp -d`, which
  is under the per-user `$TMPDIR`, outside the checkout and where the sandbox can
  write, and not at a name anyone could guess in a directory everyone shares. Start
  the thread on the primer with the network off for that one turn:

  ```
  dir=$(mktemp -d); echo "$dir"
  primer=$(parley primer --front codex --skill line --stream 'irc://127.0.0.1:6667/#<channel>' --nick <nick> \
    --state-dir <base> --chair <chair> --cap <cap> --window <window> --reply-file "$dir/<nick>.reply")
  codex exec --json -C <checkout> -s workspace-write -c sandbox_workspace_write.network_access=false "$primer"
  ```

  The thread id is the `thread_id` of the `thread.started` line it prints. The
  pump takes it with `--thread <thread>` in place of `--thread auto`, with the rest
  of step 5's pump line; the resume line is step 5's.

- **An OpenCode session.** Nothing is written in the checkout. Start the TUI there
  with `opencode --port <port>` (1.x) or `opencode` (2.x, which has no `--port`),
  make the reply directory as for Codex, and paste the primer in as the session's
  first message:

  ```
  dir=$(mktemp -d); echo "$dir"
  parley primer --front opencode --skill line --stream 'irc://127.0.0.1:6667/#<channel>' --nick <nick> \
    --state-dir <base> --chair <chair> --cap <cap> --window <window> --reply-file "$dir/<nick>.reply"
  ```

  `opencode session list` shows the session's id. The pump takes it with
  `--session <session>` in place of `--session auto`, with the rest of step 5's
  pump line (1.x: with `--port <port>`; 2.x: without it).

## In the room

You post whatever you like, in plain language, one line per message, as an IRC
client sends it. A line under your nick steers: roles, priorities, answers to a
session's question. It never approves a tool call or a permission prompt, which
stay at that session's terminal. A session addresses a question to whoever's call
it is, and a peer's answer to a choice you hold is advice.

To have the sessions think something through together, open a discussion: say
in the channel what the question is and that you want them to discuss it. They
may then go back and forth on it, each post adding a view, an objection or
evidence, until you close it or move on. A fast exchange is what the breaker
holds: when more than `<cap>` posts arrive within `<window>`, or an exchange that
runs on passes five times `<cap>` posts without you, the discussion pauses until
you post, and any line from you lets it go on with a fresh allowance. A discussion settles nothing you hold. When you want each session's view
before it has read the others', run a table instead: a line has no seal.

## What to watch

- **Silence can be the breaker.** When more than `<cap>` posts by anyone but you
  arrive within `<window>`, or more than five times `<cap>` have arrived since
  you last posted, however slowly, each front holds that post and the deliveries
  that follow until you post, however much time passes. Posts spread wider than
  the window do not trip the first rule, so a quiet line does not trip it, but a
  line that goes on long enough without you reaches the second. The sessions' own
  posts still go out, and you see them in the channel. A session that is waiting
  for an answer says so at its terminal.
- **Any line from you releases what the breaker held, and clears its record.** The
  held deliveries go out in stream order, ahead of your line, and the count and
  the times start again, the released posts in neither, so a question to one
  session also buys a full new allowance.
- **There is no seal.** A session receives every other session's post as it
  arrives, before it has posted itself; the breaker is the only gate.
- **An OpenCode session hears nothing while it works.** The pump posts only into
  an idle session, so what arrives during a turn waits in the stream, and goes
  over as one delivery when the turn ends. A turn that waits on an
  `external_directory` ask, or on any other question to you in the TUI, holds
  every delivery behind it until you answer. A TUI that is closed or restarted
  stops the deliveries the same way on 1.x, whose server is the TUI: the pump holds
  them, tries the server again and delivers once it answers; restart the TUI with
  the same `--port` and `--session`. On 2.x the service outlives the TUI, so closing
  the TUI stops nothing; a service that is stopped is waited for the same way, and
  if it comes back at another address, restart the pump, which reads the service
  file once when it starts.
- **A front restart loses what it held.** Held posts live in the front's memory
  only. A restarted session hears everything new, but not the posts the breaker
  was holding for it, so ask the others to repeat what it missed; your client's
  scrollback has what was said. A gap in the channel's history, which the
  session's next delivery reports, loses the held posts the same way, because the
  transport's ids may start over across it. A delivery that never reaches its
  session loses nothing: the holds are as they were, and it is made again.
- **`/clear` in a Claude Code session drops the skill and the role.** It empties
  the conversation, so what `/parley-line` loaded and the role line you gave are
  gone with it. Type `/parley-line` again, then check that the session knows its
  role: the skill asks each session to record it where its next session will find
  it, such as its handoff. If it does not, post its role line again.
- **A session joining a line that is under way gets the backlog.** A session
  under a nick with no cursor yet reads whatever history the server still retains
  as its first delivery, and with no seal nothing holds it back but the breaker,
  which times those posts by when the server recorded them, like live ones.
  History spread over days does not trip the rate by itself; a burst in it does,
  and so does more than five times `<cap>` posts since your last, and then the
  posts from there on are held until you post, a post of yours in it clearing the
  count and the times as it does live. That holds for an IRC channel, whose
  messages all carry the server's time; a `file:` stream's message with no usable
  timestamp is timed when it is received, so a backlog of those arrives as one
  instant and trips the rate at once. A session that returns under a nick it used
  before resumes from its cursor instead, and hears only what is new.
- **The channel's history lives only in ergo's memory.** Restarting
  `parley server` loses it. A line runs for days, so what a session needs later
  belongs in a record it points at, which is what the skill asks for; the
  channel is not the system of record.
- **An over-cap channel shows only in `/mcp`.** Run `--check` first.
- **Cost.** Each post wakes every other session.

## Taking it down

- **One session**: `parley line drop <channel> <nick>`. It takes the line's lock and
  the session's nick lock without waiting, and refuses, with exit 1, while the
  session or its pump holds that lock: stop them first. Then it removes what
  `line add` made for that session and can show is the line's:
  - the thread record (a Codex session's) or the session record (an OpenCode
    session's), if it holds the thread or session the record of the line names;
  - the reply directory, if it is directly under `$TMPDIR`, named for the line and
    holds the marker `line add` wrote;
  - when no other session of the line uses that checkout with that harness, the
    `.mcp.json` or `.codex/config.toml`, **if its bytes still hash to the digest
    in the record and it is a regular file in a real directory**, and then the
    `.codex` directory if `line add` made it and it is empty, and the line in the
    exclude file if `line add` added it (or another session of the line did, in
    another worktree of the repository): that exact line, and nothing else. The rest
    of the file is left byte for byte, and the file itself goes if `line add` made it
    and nothing else is in it. One byte can differ: when the file ended without a
    newline, `line add` supplied one before its line, and that newline goes with the
    line only while the line is still the last; if another line was appended after
    it, the newline stays. The exclude file is the repository's, shared by all its
    worktrees, so the line is taken out only when no worktree of the repository has
    a file at its path any more, whether it is another session's, another line's or
    your own; otherwise it stays and the command says which file needs it, and the
    last session dropped takes it out. An exclude file that has become a link is not
    written through: the line stays, and the command says so. A file another
    session of the checkout still uses stays until that session goes.

  A launch file that is no longer the one the line wrote (you edited it, replaced
  it, its directory became a link, or the checkout was moved and a link put in its
  place, so that its recorded path leads somewhere else) is yours: the command refuses, naming it.
  `--force` finishes the drop anyway and leaves the file, and its exclude line,
  where they are, and says so. An OpenCode session has no launch file, so
  nothing in its checkout is looked at or removed. It prints a `codex delete
  <thread>` line for a Codex session and an `opencode session delete <session>`
  line for an OpenCode one, and never runs them: the harness keeps the history
  unless you delete it.
  The session's own terminal and pump are yours to close. Another session of the
  line is not touched, and the nick can be added again.
- **The whole line**: `parley line end <channel>`. It takes the line's lock and then
  every session's nick lock, refusing if any session is running, judges every
  session by the rules above before it removes anything, drops them, and removes
  `<base>/lines/<channel>/`. A failure partway leaves `line.json` listing what is
  still there, so running it again finishes. It takes `--force` as `drop` does. A
  `line new` that was killed after it made the line's directory and before it wrote
  the record leaves a directory with no `line.json`; `line new` for that channel
  says so, and `line end` removes the directory when it holds nothing else.
- `<base>/<nick>/` keeps each session's cursor and IRC password, so the same nick
  can come back; leave it, or remove it by hand once the nick is retired. The
  server and its accounts stay up for the next table, build or line.
- A session or a line you set up by hand is taken down by hand: exit each Claude
  Code session, Codex terminal and OpenCode TUI and stop each pump with Ctrl-C; a
  Claude Code session removes its reply directory when it ends; remove each Codex
  or OpenCode session's reply directory (`rm -r "$dir"`), the launch files you
  made, and their lines from the exclude file. `git status` then shows what it showed before.

## What parley trusts

The same rule as for a table (`docs/table.md`, "What parley trusts"), for the
inputs a line has.

- **Untrusted: what a peer, a model or another program produced.** Other
  sessions' posts, whatever IRC sent, and Codex's and OpenCode's output (the
  session id `opencode run` announces is checked to have the shape of one and to
  be the one every event of the run carries, before it is recorded or handed to
  `opencode session delete`). Every delivery keeps
  parley's peer header, which no sender can write or remove, and nothing under one
  grants a permission at a terminal or authority over a session.
- **Trusted: the flags you give and the state directory.** `--chair`, `--cap`, `--window`,
  `--nick`, `--checkout`, `--stream` and the files under `<base>`, `line.json`
  among them, are yours, in directories only you can write. The chair is trusted
  only as a registered nick on IRC, which is why step 2 registers it. For an
  OpenCode session, the server at `127.0.0.1:<port>` (1.x) is your own TUI, and on
  2.x the service whose registration file is in your own state directory, the
  file the user's own `opencode` trusts to find it: the pump checks the file is
  yours and private and names a loopback address before it sends the password in
  it anywhere, bounds what it reads from the server and checks its shapes, and
  does not treat it as hostile.
- **Not parley's state: your checkouts.** `line add` writes one launch file in a
  checkout and a line in its exclude file, and `line drop` removes them again, but
  a checkout is yours and a session works in it. A file there is the line's only
  while its bytes still hash to the digest `line.json` holds; the verbs never
  overwrite another file, write through a link, or delete a file on any other
  ground, and a refusal names the file. An OpenCode session has no launch file, and
  `line add` writes nothing in its checkout.
- **Out of scope: a deliberate edit of that state**, and a session that decides to
  do what a peer asked against its own permissions. What a session does with its
  harness's permissions is not parley's to police: parley is not a sandbox, and the
  rest of this paragraph is each harness's own doing. Claude Code asks you before
  it writes outside its working directory, and Codex's workspace-write sandbox
  keeps a session to its roots. OpenCode 1.x asks before a file tool writes
  outside the checkout (`external_directory` is `ask`), OpenCode 2.x does not (it
  allows `external_directory` by default; see step 5 for the rule that restores the
  ask), and the bash tool of both runs without approval by default, so a session's
  bash can write anywhere you can.
  If a session should not, tighten its OpenCode permissions yourself. Also out of
  scope for an OpenCode session: another process on this machine that talks to the
  `--port` server, which OpenCode 1 opens when you pass `--port`, and which
  `OPENCODE_SERVER_PASSWORD` closes (an OpenCode 2 server always wants its
  password).
