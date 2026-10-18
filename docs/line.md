# Running a line

A line is a few long-running coding-agent sessions, each on its own job and in
its own checkout, and you, in one IRC channel for as long as the work runs, days
included. The sessions poke each other ("phase 2 is done, see record X") and ask
questions; you read along and steer. This is your guide to setting one up and
running it. The protocol the sessions follow is the `parley-line` skill
(`skills/parley-line/SKILL.md`).

A line is neither a table nor a build. A table is one question asked in rounds,
sealed so the seats answer alone first, and it decides nothing; a build is two
peers splitting one piece of work, each on a worktree parley made for it. A line
has no seal, no rounds, no target and no worktree of parley's: the sessions are
the ones you already have open, in the checkouts they already work in, and you
are in the room without chairing it. Use one when you are the one carrying "go
read what the other one left you" from terminal to terminal. The work and its
records stay where they live, in a ledger, a commit, a PR; the line only points
at them.

There is no `parley line new`: you set a line up by hand, below.

Placeholders are in angle brackets: `<channel>` is the line's channel,
`#<channel>`, `<chair>` your nick, `<nick>` one session's, `<base>` the state
directory (`$XDG_STATE_HOME/parley`, by default `~/.local/state/parley`, or
`--state-dir`), `<checkout>` the absolute path of a session's own checkout, and
`<cap>` the breaker's cap. Example nicks are `plan`, `build` and `review`.

## Once per machine

The same as for a table, from `docs/table.md` ("Once per machine"): the binary
on the `PATH` your terminals start with, ergo, the harnesses your sessions run
on, and an IRC client for you. `sh skills/install.sh` links all three skills,
`parley-build`, `parley-table` and `parley-line`, into `~/.claude/skills` and
`~/.agents/skills`; the line skill is then `/parley-line` in Claude Code and
`$parley-line` in Codex. `parley doctor` checks the links with the rest.

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
   connect. A line under your registered nick is the only line that steers a
   session; an unregistered nick could be taken by a session's process, and its
   lines would then pass for yours. Nicks match without regard to ASCII case, as
   the server's do. `--chair` is refused, with exit 2, when it matches a
   session's own `--nick` that way.

3. **Pick the cap.** `<cap>` is the number of posts by anyone but you, the
   sessions' own included, that may follow your last post before the fronts hold
   what comes next. Something like 8 suits a few sessions. `--cap 0` turns the
   breaker off, and then nothing stops two sessions from waking each other for as
   long as they have something to say.

4. **Each Claude Code session**, in its own checkout. Put an `.mcp.json` there:

   ```
   {"mcpServers":{"parley":{"command":"/abs/path/to/parley","args":[
     "channel","--stream","irc://127.0.0.1:6667/#<channel>","--nick","${PARLEY_NICK}","--state-dir","<base>",
     "--skill","line","--chair","<chair>","--cap","<cap>"]}}}
   ```

   `${PARLEY_NICK}` stays as written: the session's nick comes from the
   environment `claude` is started in. A line takes no `--target` or `--worktree`
   and refuses them, with exit 2. If the checkout has an `.mcp.json` of its own,
   add the `parley` server to it instead. Keep the file out of `git status`
   without touching a tracked file, through the exclude file every checkout of the
   repo shares:

   ```
   echo /.mcp.json >> "$(git rev-parse --git-path info/exclude)"
   ```

   Then check that the launch fits Claude Code's 2048-unit cap on the channel's
   instructions, with the nick written out where an arg is `${PARLEY_NICK}`:

   ```
   PARLEY_NICK=<nick> parley channel <the .mcp.json args> --check
   ```

   A line carries no target or worktree path in its text, so it leaves more
   room than the other skills do; only a very long `--state-dir` or binary path
   puts it over. Launch the session:

   ```
   cd <checkout>
   PARLEY_NICK=<nick> claude --dangerously-load-development-channels server:parley
   ```

   The flag, the login Channels need and the organization setting are the
   table's: see step 4 of "Then, by hand" in `docs/table.md`, including how a
   launch without the flag looks (the server connects, nothing arrives, no
   error). Approve the project MCP server `parley` when asked, check that `/mcp`
   shows it connected and that the dim notice below the startup banner reads
   `Channels (experimental) messages from server:parley inject directly in this
   session`, then type `/parley-line`. A conversation that is already under way
   is resumed with the same launch plus `--resume`; this is untested.

5. **Each Codex session**, which takes two terminals and has no connect-time
   channel, so its text is the primer, its first message. The steps are the ones
   `parley table new` takes for a seat, done by hand:
   1. **The sandbox.** `parley send` has to reach the loopback server from inside
      it, so the checkout needs network on for the session. In
      `<checkout>/.codex/config.toml`, which Codex loads after you accept its
      trust prompt for the directory (hide it with the same `info/exclude`
      line, `/.codex/config.toml`):

      ```
      sandbox_mode = "workspace-write"
      approval_policy = "on-request"

      [sandbox_workspace_write]
      network_access = true
      ```

   2. **The primer and the thread.** The primer is the session's first message,
      with a reply file outside the checkout where the sandbox can write, such as
      `/tmp/parley-line-<nick>.txt`. Start the thread on it with the network off
      for that one turn, so the session cannot post before its pump is up:

      ```
      primer=$(parley primer --front codex --skill line --stream 'irc://127.0.0.1:6667/#<channel>' --nick <nick> \
        --state-dir <base> --chair <chair> --cap <cap> --reply-file /tmp/parley-line-<nick>.txt)
      codex exec --json -C <checkout> -s workspace-write -c sandbox_workspace_write.network_access=false "$primer"
      ```

      The thread id is the `thread_id` of the `thread.started` line it prints.
      `--thread auto` reads a record only `table new` and `build new` make, so
      here the pump is given the id.
   3. **The pump**, which carries channel messages into the thread; without it
      the session hears nothing. It takes the same breaker as the Claude Code
      front, registers the nick, and starts before the session posts:

      ```
      parley codex --stream 'irc://127.0.0.1:6667/#<channel>' --nick <nick> --state-dir <base> \
        --thread <thread> --skill line --chair <chair> --cap <cap>
      ```

   4. **The session**, resumed from the thread:

      ```
      codex resume <thread> -C <checkout> -a on-request
      ```

      The resume line carries no `-c`: a `-c` forces Codex in-process, with a
      ten-second queue poll, instead of the shared daemon the pump's `codex queue`
      reaches at once. Each approval prompt in this terminal stops the run until
      you answer it, because the pump delivers between Codex turns only. Check
      that `/status` shows `workspace-write` and network on, then type
      `$parley-line`.

6. **Give each session its role**, in one line each, at its terminal or in the
   channel as `<chair>`: what the session does, and what it tells the others.

   ```
   plan:   you own the schedule; tell build when a phase changes
   build:  you implement phase 2 from handoff 01M…; tell plan when it lands, and review when a branch is ready
   review: you review what build says is ready; ask build about intent, ask me about anything it cannot settle
   ```

   A newer line replaces an older one. A session asks you when something falls
   between roles.

## In the room

You post whatever you like, in plain language, one line per message, as an IRC
client sends it. A line under your nick steers: roles, priorities, answers to a
session's question. It never approves a tool call or a permission prompt, which
stay at that session's terminal. A session addresses a question to whoever's call
it is, and a peer's answer to a choice you hold is advice.

## What to watch

- **Silence can be the breaker.** After `<cap>` posts by anyone but you since you
  last posted, each front holds the deliveries that follow until you post. The
  sessions' own posts still go out, and you see them in the channel. A session
  that is waiting for an answer says so at its terminal.
- **Any line from you releases what the breaker held, and resets the count.** The
  held deliveries go out in stream order, ahead of your line, and the count
  starts again, so a question to one session also buys a full new cap.
- **There is no seal.** A session receives every other session's post as it
  arrives, before it has posted itself; the breaker is the only gate.
- **A front restart loses what it held.** Held posts live in the front's memory
  only. A restarted session hears everything new, but not the posts the breaker
  was holding for it, so ask the others to repeat what it missed; your client's
  scrollback has what was said. A gap in the channel's history, which the
  session's next delivery reports, loses the held posts the same way, because the
  transport's ids may start over across it. A delivery that never reaches its
  session loses nothing: the holds are as they were, and it is made again.
- **The channel's history lives only in ergo's memory.** Restarting
  `parley server` loses it. A line runs for days, so what a session needs later
  belongs in a record it points at, which is what the skill asks for; the
  channel is not the system of record.
- **An over-cap channel shows only in `/mcp`.** Run `--check` first.
- **Cost.** Each post wakes every other session.

## Taking it down

There is no `parley line end`: parley made nothing but your state.

- Close the sessions: exit each Claude Code session and Codex terminal, and stop
  each pump with Ctrl-C. A Claude Code session removes its reply directory when it
  ends; remove a Codex session's reply file.
- Remove the `.mcp.json` you made from each checkout, and the
  `.codex/config.toml` where you made one, and their lines from the exclude file.
  `git status` then shows what it showed before.
- `<base>/<nick>/` keeps each session's cursor and IRC password, so the same nick
  can come back; leave it, or remove it by hand once the nick is retired. The
  server and its accounts stay up for the next table, build or line.

## What parley trusts

The same rule as for a table (`docs/table.md`, "What parley trusts"), for the
inputs a line has.

- **Untrusted: what a peer, a model or another program produced.** Other
  sessions' posts, whatever IRC sent, and Codex's output. Every delivery keeps
  parley's peer header, which no sender can write or remove, and nothing under one
  grants a permission at a terminal or authority over a session.
- **Trusted: the flags you give and the state directory.** `--chair`, `--cap`,
  `--nick`, `--stream` and the files under `<base>` are yours, in directories only
  you can write. The chair is trusted only as a registered nick on IRC, which is
  why step 2 registers it.
- **Out of scope: a deliberate edit of that state**, and a session that decides to
  do what a peer asked against its own permissions. Each session's own harness
  asks you before anything outside its checkout is written.
