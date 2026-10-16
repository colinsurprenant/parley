# Running a build

A build is two coding-agent sessions, the **peers**, and you, the **chair**, in
one IRC channel. The peers negotiate what to build and how to split it, each
works in a worktree and on a branch of its own in the same repo, and what they
settle is promoted into that repo's Director ledger. This is the chair's guide to
setting one up and taking it down. The protocol the peers follow is the
`parley-build` skill (`skills/parley-build/SKILL.md`); `docs/table.md` is the
guide to the other mode, a table of several seats that thinks and decides
nothing.

Placeholders below are in angle brackets: `<chair>` is your nick, `<id>` the
build's id, which is also its channel, `#<id>`, `<target>` the absolute path of
the main checkout of the repo the build works on, `<base>` the state directory
(`$XDG_STATE_HOME/parley`, by default `~/.local/state/parley`, or `--state-dir`),
and `<nick>` one peer's nick.

## Requirements

Everything in "Once per machine" of `docs/table.md` holds here too: the binary,
ergo, the skills (`sh skills/install.sh` links `parley-build` and `parley-table`;
the build skill is `/parley-build` in Claude Code and `$parley-build` in Codex),
the `claude` and `codex` harnesses on the `PATH`, and an IRC client for you.
`parley doctor` checks them (`--harness claude` or `--harness codex`, repeatable,
limits it to the harnesses you use; without one it checks both); `parley build
new` runs its static checks itself, with the `parley-build` link under the root
each peer's harness reads.

A build needs two things of the target that a table does not. A table needs
nothing beyond parley; a build is promoted into Director's ledger, so:

1. **The target is adopted in Director.** Run `director adopt` in the target
   before `parley build new`. What the peers settle is emitted into the target's
   ledger, so the ledger has to exist. parley never calls `director` and does
   not check that you did.
2. **The target ignores `.director/`.** Director writes `.director/` into every
   checkout an emit runs in, and each peer emits from its own worktree. `parley
   build end` treats an untracked file as the peer's own work and refuses to
   remove it without `--force`, so an unignored `.director/repo-key` blocks every
   end. In the target, `git check-ignore -q .director/x` exits 0 when it is
   ignored. If it is not, add `/.director/` to the target's `.gitignore`, and
   commit it before `build new`: the peers' worktrees are made from the target's
   HEAD, so an uncommitted line does not reach them. Or add the line to the
   `info/exclude` file of the target's common git directory, which every worktree
   of the target shares and which holds for worktrees that already exist. That
   directory is `<target>/.git` for an ordinary checkout, and `git -C <target>
   rev-parse --git-common-dir` names it for any; `build new` prints the full path
   in its runbook. It prints the check as the first peer's worktree sees it: the
   line names that worktree's git directory in the target (`--git-dir`) and its
   work tree, and does not follow the worktree's own `.git` file, which a peer
   can rewrite.

## Per build

### Set it up: `parley build new`

```
parley build new <id> --target <target> --chair <chair> \
  --peer codex-a=codex:<model> --peer claude-a=claude:<model>
```

`--peer <nick>=<claude|codex>[:<model>]` is given exactly twice: the protocol is
two-peer, and a third is refused, naming that. The order of the two flags
counts: the first peer is the one you hand message one to, listed first on its
`participants:` line, and the second is the **integrator**, the peer that merges
the two branches at the end (see "After the build"), unless the task names
another. OpenCode peers are not supported yet; OpenCode is the next harness on
the list. A model is at most 128 bytes of ASCII letters and digits and `. _ - : / @ [ ] +`,
which holds the ids Claude Code and Codex take (`opus`, `sonnet[1m]`,
`claude-sonnet-4-5-20250929`, `us.anthropic.claude-sonnet-4-5-20250929-v1:0`,
`gpt-5.1-codex-max`, `qwen2.5-coder:32b`, a Bedrock inference-profile ARN); it
does not begin with `-` and holds no control character. The model is kept in
`build.json`, so the bound is also what keeps the record under the 64 KiB that
`build end` reads. The
optional flags: `--server 127.0.0.1:6667`, `--state-dir <dir>`, `--codex-bin
codex` and `--writable-root <abs dir>`, repeatable (see "The Codex sandbox"
below). There is no `--rounds` and no `--cap`: the round budget and the elapsed
time cap are agreed in message one, between the peers, as the skill says.

**What it refuses, before it changes anything (exit 2).** An id outside
`[a-z0-9][a-z0-9-]{0,31}`; a `--target` that is not the root of a git
checkout, or whose HEAD already tracks a `.mcp.json` of any kind, a
`.codex/config.toml`, a `.parley-build`, or a `.codex` that is not a directory;
a chair or peer nick that is not a valid nick, a peer that repeats the other's or
the chair's (nicks match without regard to ASCII case, as the server's do), and
`tables` and `builds`, the directories the state base keeps beside the per-nick
ones, and `.ids`, which no nick can be (a nick holds no `.`); a model that is
over the bound above or holds a character outside the set; a state directory inside the
target, which would put the worktrees inside it, or a `$TMPDIR` that holds the
reply directory inside the target or a worktree; a `$` in the target, a worktree
or the binary's path, because Claude Code expands `${...}` in an `.mcp.json`'s
command and args; a `--writable-root` that is not absolute, does not exist, is
not a directory or holds a control character; a path that goes into `build.json`,
an `.mcp.json` or the Codex config and is not valid UTF-8 (`--target`,
`--state-dir`, the `$TMPDIR` the reply directory is made under, the target's
common git directory, `--codex-bin`, `--writable-root`, the binary's path;
`--worktree` for `table new`), because `encoding/json` replaces an invalid byte
with U+FFFD and the record would then name another path than the one `build new`
used; the refusal names the flag or the path; and a Claude peer whose
instructions would be cut at Claude Code's 2048-unit cap, which it measures in
process, with the peer's own nick and worktree, the way `parley channel --check`
does. The worktree path is `<base>/builds/<id>/<nick>`, so a long state
directory, a 32-character id and a 64-character nick can by themselves be over
the cap; the refusal names the length and what to shorten. Then it runs `parley
doctor`'s static checks for the harnesses the peers use, the server, ergo when
nothing answers at the server, and the `parley-build` skill under the root each
harness reads; a `FAIL` ends it (exit 1) with nothing changed, and a `warn` is
printed on stderr. None of this looks at what the state base holds, so a refusal
so far has made no file there.

**The id's lock, and what the base and the target hold for the id.** The first
look at the state base is the id's lock, `<base>/.ids/<id>.lock`, which `table
new`, `table end`, `build new` and `build end` all take, for the whole of their
run, before anything else of theirs (see "One lock per id" below). A `build new`
that finds it held refuses at once (exit 1, nothing changed), naming the id and
the file: another table or build command for this id is running, and it is run
again once that one is done. Under the lock it checks what the id may not
already be: a build under `<base>/builds/<id>/` ("build end takes it down"), a
table under `<base>/tables/<id>/` (the id is the channel and a directory name,
and `table new` refuses an id that is live as a build, in turn); a peer that
already has state for this stream under `<base>/<nick>/<stream key>/` (`<nick>`
with its ASCII capitals folded to lower case, so `Claude-A` and `claude-a` are
one directory), because a
cursor left by an earlier run ends a run at its first delivery with a history
gap; and a branch `parley/<id>/<nick>` that already exists, because `build new`
will not move or reuse a branch. Each of those is a refusal with exit 2 and
nothing changed but the lock file.

**What it refuses when a peer is in use (exit 1, nothing changed).** The first
step of making the build, after the id's lock, is both peers' nick locks, which
are per nick across all streams, as `table new` takes the seats'. A nick that a
running pump or Claude Code session holds is refused, naming it. The locks are
held until `build new` is done, rollback included, and the lock files stay (see
`docs/table.md`).

**What it makes**, in this order, each step with an undo:

- `<base>/builds/<id>/`, mode 0700, and in it **`build.json`**, the record
  `build end` reads: the id, the stream, the target, the target's common git
  directory, the base commit (the target's HEAD when `build new` ran), the chair,
  the reply directory, when it was made, and for each peer its nick, harness,
  model, worktree, admin git directory (the worktree's registration inside the
  target's git directory), branch, and, for a Codex peer, its thread and reply
  file. It holds no stream content.
- For each peer, a new branch `parley/<id>/<nick>` made at the base commit, and
  a **worktree** at `<base>/builds/<id>/<nick>` on it: `git -C <target>
  update-ref --create-reflog -m "parley build new <id> <nonce>"
  refs/heads/parley/<id>/<nick> <base commit> ""` (an empty old value makes the
  ref only if it is not there, in one step; the nonce is random, per run, and
  the reflog entry it lands in is how a rollback tells this run's branch from
  anyone else's), then `git -C <target> worktree add <worktree>
  parley/<id>/<nick>`. The two peers do not share a worktree, because Director
  keys a workstream on the checkout an emit runs in. Nothing is written into the
  target checkout itself but git's registration of the worktrees and the
  branches.
- In each worktree, a **`.parley-build`** marker: the build's id, the peers'
  nicks sorted and separated by spaces, and, once `build.json` is written, the
  `sha256:` digest of its exact bytes, one per line. It is how `build end` can
  tell you a worktree has been changed. A peer's `git clean` can remove it, so
  its absence is reported and never a refusal.
- For a Claude peer, the worktree's **`.mcp.json`**: one server, `parley`, whose
  args are the channel's launch flags, `channel --stream irc://<server>/#<id>
  --nick ${PARLEY_NICK} --state-dir <base> --skill build --chair <chair>
  --target <target> --worktree <its worktree>`: no `--cap`, because a build holds
  nothing back. Each Claude peer has a file of its own, since each has a worktree
  of its own; its nick still comes from the environment `claude` is started in.
- For a Codex peer, the worktree's **`.codex/config.toml`**: `workspace-write`,
  `on-request`, and `network_access = true` in `[sandbox_workspace_write]`, which
  is what lets `send` reach the loopback IRC from the sandbox, and a
  `writable_roots` line only when `--writable-root` is given, holding the roots
  you named and the global ones `build new` carried over (see "The Codex
  sandbox").
- A **reply directory**, `parley-build-<id>-*` under `$TMPDIR`, mode 0700, with
  the same marker. Each Codex peer's reply file is `<nick>.reply` in it, outside
  the worktree and where the Codex sandbox can write. A Claude peer's reply
  directory is the channel's own, made when its session starts.
- Each **Codex peer, started**, as `table new` starts a seat: `codex exec --json`
  with the peer's build primer as the prompt, in its worktree, `workspace-write`,
  stdin closed, network off for that one turn, five minutes at the most. The
  thread id it announces is recorded for the peer in
  `<base>/<nick>/<stream key>/codex-thread` (the nick with its ASCII capitals folded to lower case), the
  record `parley codex --thread auto` reads. Every failure of the turn that `docs/table.md` lists fails the
  peer, the build is rolled back, and the announced thread, if it is a UUID, is
  deleted.

**If any step fails** (exit 1), everything the run made is taken down: each
worktree, each branch it made, the reply directory, the thread records,
`<base>/builds/<id>/`, and each Codex thread it started (`codex delete
--force`). The undo of a worktree, a directory or a thread is registered once the
thing it undoes is there. The undo of a **branch** is registered *before* the
`update-ref` that makes it, because that call can create the branch and still
fail or be interrupted, and the branch is then this run's to undo. What makes it
safe is what the undo checks: it deletes the branch only when the first (oldest)
entry of the branch's reflog carries this run's own message, `parley build new
<id> <nonce>`, and the branch still points at the base commit (a compare-and-delete,
`update-ref -d` with the old value). A branch someone else made in the meantime,
whatever commit it holds, has no entry with this run's nonce and is never
deleted (the run fails, naming it); neither is a branch this run made that has
since moved. The reflog is read through `git` in the target, the target's own
repository, which is trusted, and bounded: past 1 MiB of output the undo cannot
tell and leaves the branch, saying so. The record, `build.json`, is checked against the 64 KiB
`build end` reads before it is written, and the run fails, naming the bound, if
it would be larger. The nick directories it made are removed if they are still
empty, the lock files (each nick's and the id's) stay. Interrupting it does the
same.

#### One lock per id

The id is one namespace for a table and a build: it is the channel `#<id>`, a
directory under `<base>/tables/` or `<base>/builds/`, and a branch prefix. Nothing
about the nicks tells a `table new` and a `build new` for one id apart when their
nicks are disjoint, and nothing else orders two `build end` and a `build new`.
So `table new`, `table end`, `build new` and `build end` each take
`<base>/.ids/<id>.lock` first (the same non-blocking flock a nick's lock is,
in a directory no nick can be: a nick holds no `.`) and hold it to the end of
the run, rollback and the report included. The checks of what the base holds for
the id are made only under it, and an end reads its record only under it. A
command that finds the lock held **refuses at once and does not wait**, exit 1,
naming the id and the file, and changes nothing; the lock is another table or
build command for the id that is still running, and this one is run again after
it. The lock file is outside the table's and the build's own directory, which an
end removes, and stays, released, as a nick's does: no parley code unlinks a
lock file, because a lock is held on the file and not on its path. An end for an
id that was never there is refused before the lock (exit 2, "no build"), so a
slip of the finger leaves no file.

### The Codex sandbox

parley never widens a Codex peer's sandbox into the target's git directory. A
writable common git directory lets a peer plant hooks, or a `core.fsmonitor`,
that later run as you, outside any sandbox. So the worktree's
`.codex/config.toml` is the table's, with `writable_roots` only when you gave
`--writable-root`. The cost is in what the peer is asked to do: a commit writes
`<target>/.git/worktrees/<name>/` and the object store, and `director emit`
writes the ledger. Inside the sandbox those fail with `Operation not permitted`,
and the peer's `git commit` and `director emit` then go through Codex's own
approval (`on-request`): the peer asks, and you approve each one at its terminal.
A Codex peer's primer tells it to ask for escalation for `git add`, `git commit`,
`git merge` and `git reset` directly instead of trying them in the sandbox first,
unless a writable root in its launch config (`.codex/config.toml` in its
worktree) or, when that lists none, in its global Codex config
(`$CODEX_HOME/config.toml`, else `~/.codex/config.toml`) is the target's git
directory. The primer is written without the roots, so that condition is the
peer's to read. Codex keeps `<root>/.git` read-only even under a
writable root (its seatbelt rules at rust-v0.161.0), so only a root on the git
directory itself would make git writable.

You can opt a root in, once, for a build:

- `~/.director/projects/<repo-key>/` is the narrow root that lets `director emit`
  run without a prompt. `director adopt` prints it. `--writable-root
  ~/.director/projects/<repo-key>`.
- **Do not add the target's git directory** (`<target>/.git`, or any directory
  that holds it) unless you accept what it hands the peer: write access to the
  repository's hooks and config. `build new` warns on stderr when a root holds or
  is inside the target's git directory.
- A project `writable_roots` replaces your global list rather than adding to
  it: with Codex 0.161.0 a root in your `~/.codex/config.toml` is absent from
  the roots a worktree's own `writable_roots` yields. So when you give any
  `--writable-root`, `build new` carries your global roots over. It reads the
  `writable_roots` of `[sandbox_workspace_write]` (a table, or the dotted key
  `sandbox_workspace_write.writable_roots`; profiles are not read) from
  `$CODEX_HOME/config.toml`, or `~/.codex/config.toml` when `$CODEX_HOME` is
  unset, puts each root through the checks a `--writable-root` gets, and writes
  them after the ones you named, each once. A global root that does not exist
  or fails the checks is skipped, and the log and the runbook name it and say
  why. When the file is absent, has no `writable_roots` key or lists no roots,
  or cannot be read or parsed, nothing is carried: the log and the runbook say
  why, and that naming a root replaced your global ones, so re-pass any a peer
  needs (a build cache, say) with `--writable-root`. Without `--writable-root`
  `build new` writes no `writable_roots` at all, and your global roots apply as
  they always did.

Each `--writable-root` is resolved to its physical path, symbolic links
evaluated, and written to the config as a TOML string. `codex debug
prompt-input`, run in a worktree of a trusted project, prints the roots the
config yields. Without `--writable-root` they are the worktree, `/tmp`,
`$TMPDIR` and your global roots. With any `--writable-root` they are the
worktree, `/tmp`, `$TMPDIR`, the roots you named and the global roots `build
new` carried over; a global root it skipped, or could not read, is not among
them. Codex loads a project's `.codex/config.toml` only after you
accept its trust prompt for the directory (step 3 below).

### Then, by hand

`build new` prints these steps, with every path, stream, thread id and model
quoted for a shell. It starts with the two requirements above, as a reminder:
parley does not check them.

1. **Start the server**, unless one is already running, as `docs/table.md` says:
   `build new` prints this step only when nothing answers at `--server`.

2. **Join as the chair, registered.** Connect your client as `<chair>`, `/join
   #<id>`, and check `/msg NickServ INFO <chair>` before any peer or pump
   connects. If it is not registered, `/msg NickServ REGISTER <password>`, and
   identify on every later connect. An unregistered chair nick could be taken by
   a peer's process, and your lines would no longer be the ones that rule the
   build. `--chair` is refused when it matches a peer's nick.

3. **Each Codex peer** takes two terminals:
   1. **The pump**, which carries channel messages into the Codex thread. It
      starts before message one:

      ```
      parley codex --stream 'irc://<server>/#<id>' --nick <nick> --state-dir <base> \
        --thread auto --skill build
      ```

   2. **The session**, resumed from the thread `build new` started:

      ```
      codex resume <thread> -C <worktree> -a on-request
      ```

      Each approval prompt in this terminal stops the run until you answer it,
      because the pump delivers between Codex turns only: keep it in view. Codex
      asks whether to trust the directory: accept, so that the worktree's
      `.codex/config.toml` applies. Check that `/status` shows `workspace-write`
      and network on, and its model if `--peer` named one (unverified on a resumed
      session), then type `$parley-build`.

4. **Each Claude Code peer**, one terminal each, in its own worktree. Before
   the launch command:

   - The launch needs `--dangerously-load-development-channels`. A server in
     `.mcp.json` is a channel only when the launch names it, and parley's is
     not on Anthropic's allowlist. Without the flag the MCP server still
     connects and `/mcp` still lists `parley`, but the startup screen has no
     "Channels (experimental)" notice and channel messages never arrive: the
     peer hears nothing and shows no error.
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

   with `--model <model>` where the peer named one. Approve the project MCP
   server `parley` when asked, check that `/mcp` shows it connected and that the
   dim notice below the startup banner reads `Channels (experimental) messages
   from server:parley inject directly in this session`, then type
   `/parley-build`. A peer silent from the start has an over-cap or failing
   channel, which shows only in `/mcp`; or a launch without the flag, or
   channels off for its organization, which show as `parley` connected with no
   "Channels (experimental)" notice (a startup warning about the organization
   stands in for it in the second case).

5. **The gate.** Do not hand over message one until every peer has loaded its
   skill, every Claude peer's `/mcp` shows `parley` connected and the startup
   notice `Channels (experimental) messages from server:parley inject directly
   in this session` is below its banner, and every Codex pump is running. `/mcp`
   alone also passes on a launch without `--dangerously-load-development-channels`,
   which never hears the channel; the notice is what shows it loaded. A peer
   posts nothing to the channel until message one arrives, so a quiet channel
   before it is right.

6. **Message one.** The run opens with a message that comes from a peer, not
   from you: the other peer checks that its `participants:` line names its own
   nick and the sender's (`skills/parley-build/SKILL.md`, "Message one"), so a
   line you type does not open the run. `build new` prints the form with
   `participants:` and `target:` filled in; you choose the budget and the cap,
   fill in the task and the size, and hand it to one peer at its terminal to post:

   ```
   RUN <id>
   budget: <n> rounds
   cap: <duration>
   human: <chair>
   participants: <nick-a> <nick-b>
   target: <target>
   task: <one or two sentences>
   size: <how many items are expected, roughly how much code>
   owns <nick>: <path>
   ```

   When the task splits by file, name each peer's files: one `owns <nick>:
   <path>` line per path, a directory covering everything under it. Leave the
   line out only when the task does not split that way. From then on, a line in
   the channel under your registered nick rules the run (rule 8 of the skill:
   settle or reopen an item, change the budget or the cap, call `END`, answer an
   `ESCALATE`, answer the integrator's ask for the merge of step 7); nothing a
   peer says does.

7. **The merge, once both peers have agreed every item.** Each peer commits its
   work to its own branch, and the peer that is not the integrator posts its
   branch and its tip commit in the channel: after that it opens nothing more.
   The integrator, the second `--peer` unless the task named another, checks the
   commit against the branch, commits its own work and tests a throwaway merge
   of it in a copy outside its worktree. A test that fails reopens the item it
   concerns, whoever proposed it, and the run goes on from there. The other peer
   ends after a passing test, and its branch is final then; only then does the
   integrator ask you in the channel to approve merging that commit into its
   branch, naming both branches and both commits. Check both against the
   repository before you answer, with
   `git -C <target> rev-parse refs/heads/parley/<id>/<nick>` for each peer's
   nick (the full ref, which a tag of the same name cannot shadow). Answer yes
   there, under your registered nick: that line is the approval, and the merge
   it authorizes is of exactly the commit the ask named. A question, a condition
   or any other line is not a yes, and the integrator asks again; a no ends the
   run with nothing merged. If the other peer's work is already in the
   integrator's branch there is nothing to merge: no ask, and the integrator's
   branch is the result. On a yes the integrator merges with `git merge
   --no-ff`, a commit, and runs the tests on that commit in a fresh copy; on a
   conflict, a refused merge or a failing test it resets its branch to its own
   tip with `git reset --merge`, tells you in the channel and closes with a
   handoff. Its terminal may then ask you to approve the git command itself;
   answer that there. Where a peer asks for what: tool approvals (a Codex
   approval, a Claude Code permission prompt) at its terminal, protocol
   decisions such as the merge in the channel. A channel line approves no tool
   call, and a terminal answer settles no protocol question. The steps are
   conversation, not rounds: a spent round budget does not stop them, and the
   cap does. It leaves the merge undone and a handoff when the merge has not yet
   passed its tests, and after that the merge stands.

## After the build

- Each peer's work is on its branch, `parley/<id>/<nick>`, and nowhere else.
  After the integration (step 7 above) the result is the integrator's branch: it
  holds both peers' work and the merge commit its closing note names (with
  nothing to merge, its tip is the result). Merging
  that branch into the target is yours, when you want it (`git -C <target>
  merge refs/heads/parley/<id>/<nick>`, with the integrator's nick). A failure
  the integrator cannot hand back to an item, a tip that keeps mismatching, or
  a conflict or a failing test at the real merge, comes to you in the channel,
  and the peers close with a handoff: the merge is then yours to settle.
  `build end` keeps both branches; delete a branch by hand when you are done
  with it (`git -C <target> branch -D parley/<id>/<nick>`; `-D` drops commits
  that are not merged).
- Close the peers and the pumps. `parley build end` takes the id's lock, then
  both peers' locks, without waiting, and refuses, naming the id or the nick,
  while one is held; the locks are held until the build is down, so a peer
  started meanwhile is refused too, and so is a `build new` or a `table new` for
  the id. The lock files stay, as after `build new`.
- Take the build down:

  ```
  parley build end <id>
  ```

  with the same `--state-dir` as `build new` if it had one, and the same
  `$TMPDIR`. **What a clean end does to a worktree: it removes it, the checked-out
  files with it.** A peer's committed work is kept, on its branch, which is never
  touched; uncommitted tracked changes and untracked files other than the launch
  files block the end (below), unless `--force`, which removes them too. It takes
  the id's lock first (see "One lock per id"), then reads `build.json` through 64
  KiB, so the record it takes down is the one no other command can be changing.
  It refuses a larger file, a file that is not a regular file, and a record that
  is not one `build new` wrote (a worktree that is not `<base>/builds/<id>/<nick>`,
  a reply directory that is not a build's, a peer count other than two): these
  are damage, and it removes nothing then. Then it removes the peers' cursor
  state and thread records for the build's stream, each worktree, the reply
  directory and `<base>/builds/<id>/`. It removes only what it can show is the
  build's:
  - a worktree if it is the recorded path under `<base>/builds/<id>/`, a real
    directory and not a link, that the target lists as one of its linked
    worktrees, and whose registration, which `build new` recorded, names it. When
    git cannot answer the question the path being under the build's directory
    stands in for its answer **only if the target is no longer there** (its path
    is gone); any other failure of `git worktree list`, the target being there
    with a repository git cannot read included, leaves the worktree in place,
    with git's error quoted. A worktree claimed without git is deleted by hand,
    and the report says what became of git's registration of it: dropped (the
    recorded admin directory only, when the target's common git directory is still
    there and the directory's `gitdir` file names the worktree), or left, with
    the `git worktree prune` that drops it. A missing or changed `.parley-build`
    never stops the removal of the worktree, but it can hold the peers' state
    (the next bullet);
  - the reply directory if it sits directly in `$TMPDIR`, is named for the build,
    and holds the marker `build new` wrote with the build's id, peers and digest;
  - the peers' cursor state and thread records, unless the marker gate holds
    them (below).

  An item it cannot claim is reported under "Left in place" and not touched, the
  rest goes on, and the build directory stays while a worktree is left in it, so
  that the same command finishes the job once you have dealt with it.
- **The marker gate: when `build end` keeps the peers' state.** The stream and
  the nicks whose state it removes are read from `build.json`, and a record that
  was changed since `build new` (a stray write, a bad disk) would name other
  ones. The markers are the check. `build end` reads **every peer's**
  `.parley-build` at its recorded path, **whether or not it could claim that
  worktree** (an unclaimable one is the likely place for a record to be wrong):
  `lstat` first, a regular file only, never through a link, through 64 KiB, and
  only when the worktree is itself a real directory. It reads the reply
  directory's marker when it claims that directory. A marker that is **there**
  and does not match the id, the peers and the digest of `build.json` that
  `build new` sealed is evidence that the record was changed: the stream and the
  nicks it names are then not shown to be the build's, so the state is **kept and
  listed** under "Left in place" with its paths, and the rest of the end goes on
  by its own proofs. A marker that is missing is no evidence (a peer's `git
  clean` removes it) and holds nothing. This is the gate `table end` applies,
  except that `table end` also refuses to remove anything when no marker is left
  to read, and a build end does not.

  **The gate outlives a partial teardown.** The evidence is in files an end
  removes: the first end of a build whose peer A's marker disagrees removes A's
  worktree, marker and all, and a second end, run after peer B's worktree is
  dealt with, would find nothing to disagree and remove the state the first kept.
  So when the gate trips, before it removes anything, the end writes
  **`<base>/builds/<id>/disputed`**, mode 0600, a few lines naming what disagreed.
  Every later end of that id finds the file, keeps the stream state and the
  thread records the same way, and lists them, whatever the markers then say.
  The file lives in the build directory: an end that leaves the directory (a
  worktree it could not claim keeps it) leaves the file for the next end, and an
  end that removes the directory removes it, since there is no later end then;
  the state it kept is yours to remove by hand, from the paths it listed.
  **To clear it:** look at the state (the report names each directory), then run
  `parley build end <id> --force`, which removes the state and the `disputed`
  file. `--force` lifts the file and nothing else of the gate: a marker that
  disagrees *now* still holds the state, so repair that marker, or remove the
  worktree or the reply directory it is in (the report names it), and run it
  again.
- **What blocks an end.** Unless `--force`, a claimed worktree with a tracked
  change (modified, deleted, or staged) or an untracked file beyond the launch
  files build new wrote: the `.parley-build` at the root, the `.mcp.json` when
  the peer is Claude's and the `.codex/config.toml` when it is Codex's. Any other
  file, `.codex/`'s other contents and a `.parley-build` below the root included,
  is a change. An untracked `.director/repo-key` blocks it in a target that does
  not ignore `.director/` (requirement 2). A worktree that git cannot be asked
  about blocks it too: when the target is gone there is no repository to read its
  status from, and the end is refused with that reason until you pass `--force`,
  which removes the worktree unasked; and when git is asked and fails for a
  peer, its error, quoted, is the reason. The check collects every peer's result
  before it refuses, so one refusal lists each peer's dirty paths or git's error,
  not the first it found. **Uncommitted work inside a submodule
  does not block**, as in `table end`: the status does not look inside one, so
  only a gitlink whose commit changed counts, and what is dirty in the
  submodule's work tree is removed with the worktree.
  **Ignored files do not block.** This deliberately differs from `table end`,
  whose worktree no seat should write, so that it counts every ignored file as a
  change. A build's peers write their worktrees, and the target's ignore rules
  say what of it is disposable. `build end` counts the ignored paths (an ignored
  directory is one path), prints the count and the first few names, quoted, and
  removes them with the worktree, with or without `--force`: the count is a
  report and not part of the decision to refuse. A marker that disagrees with
  `build.json` blocks nothing here: it holds the peers' state instead (the marker
  gate above).
- **A commit on a launch file is reported.** The skill forbids a peer to edit,
  stage or remove `.parley-build`, `.mcp.json` or `.codex/config.toml`, and to
  stage anything but by path. `git log <base commit>..parley/<id>/<nick> --
  .parley-build .mcp.json .codex/config.toml`, run in the target, names each
  commit on a peer's branch that touches one (the short hash and the subject,
  quoted, five at the most) as a warning. It never refuses: read those commits
  before you merge the branch.
- `build end` never touches a Codex session. It prints a `codex delete <thread>`
  line for each Codex peer; run those if you do not want that history.
- **Close the peers' Director workstreams, after the merge.** Each peer emitted
  from its own worktree, so each is a workstream of its own in the target's
  ledger, and nothing closes it but you. A peer does not run `/director:complete`
  itself: its work is not merged when it ends. In this order: merge the build's
  result; run `parley build end`, which removes the worktrees, so that
  `director status` reads each peer's workstream `gone` and names
  `/director:complete <workstream>` as its remedy; then, from the target's main
  checkout, run `/director:complete <workstream>` (`$director-complete` in Codex)
  once per peer. It lists the peer's open items for you to triage, emits the
  completion note and, last, archives the peer's fleet rows (`director done
  --workstream`). A peer that ended on a handoff is resumed, not closed: leave
  its workstream alone, and let a fresh session pick the handoff up from the
  ledger.
- The server can stay up for the next build or table. The channel history stays
  in ergo's own store, never in the repo, and ergo keeps it in memory.

## What parley trusts

The build commands guard each input according to who could have written it.

- **Untrusted: what a peer, a model or another program produced.** Channel
  messages (the header rule), Codex's output (its JSONL, the thread id it
  announces, its answers), the IRC server's replies, `$CODEX_THREAD_ID`, git's
  output, and every file in a peer's worktree: the `.git` file, the marker and
  the launch files included. parley bounds what it reads of each, checks an id
  before it reaches a command line, quotes or strips control characters before
  they reach your terminal. A clean `build end` removes a worktree, and the
  files in it with it, so what it takes for clean is spelled out under "What
  blocks an end": no tracked change and no untracked file beyond the launch
  files, ignored files aside; what a peer committed is on its branch, which is
  kept. Anything else a peer left there blocks the end, and `--force` removes it
  too. It never lets a file in a worktree choose what git runs
  against: a peer can rewrite the worktree's `.git` file to point git at a
  directory of its own whose config names a program as `core.fsmonitor`, and the
  next `git status` there would run it as you. So every git command parley runs
  inside a build worktree, and inside a table's, names the worktree's git
  directory, the target's common directory and the work tree in `GIT_DIR`,
  `GIT_COMMON_DIR` and `GIT_WORK_TREE`, all from the target's side (recorded when
  the worktree is made, or found among the target's own `worktrees/*/gitdir`
  files), passes `-c core.fsmonitor=false -c core.hooksPath=/dev/null`, and does
  not let `git status` look inside a submodule (`--ignore-submodules=dirty`): a
  gitlink alone, with a directory behind it that a peer made, is enough to make
  an unrestricted status run a clean filter planted there. Commands that run in the
  target itself (`worktree add`, `remove`, `list`, `log`) keep running there, and
  `git log` is run with `--no-show-signature`.
- **Trusted: parley's own state** and the target repo's own git directory and
  config. The state is `<base>/builds/<id>/build.json` and, once a marker has
  disagreed with it, `<base>/builds/<id>/disputed` (the only files of parley's in
  that directory; the peers' worktrees are beside them, and are the peers');
  each Codex peer's thread record and each peer's cursor, under the
  peer's own directory, `<base>/<nick>/<stream key>/`; and the lock files, each
  peer's `<base>/<nick>/lock` and the id's `<base>/.ids/<id>.lock`. They are
  written by parley or by you, in directories only your account can write. parley
  checks them for damage: a size bound, a regular file, a well-formed value. The
  markers are not state parley trusts, but evidence about `build.json`: the one
  in the reply directory, and the ones in the peers' worktrees, which a peer can
  edit (above). They carry the sha256 seal that ties `build.json` to the build
  `build new` made. `build end` reads every peer's marker at its recorded path,
  whether or not it could claim the worktree, without following a link and
  through a bound. A marker that is there and disagrees with the record is taken
  as evidence that the record changed, and `build end` then lists the peers' state
  and does not remove it, now or at any later end of the build, until you look
  and clear the `disputed` file with `--force` ("The marker gate"); a marker that
  is missing is no evidence, and does not stop it. All a peer gains by editing its
  own marker is that `build end` leaves the peers' state for you to remove.
- **Out of scope: a deliberate hand edit of that state; a peer that edits or
  commits the launch files** (the skill forbids it, and `build end` reports it);
  **and what a peer does with the permissions its harness grants it**: Claude
  Code's permission prompts, Codex's approvals, and any `--writable-root` you add.
  The peers run as you, so the 0700 mode of `<base>/builds/<id>/` does not keep a
  peer out of it; the harness does. The worktrees sit inside the state base, so
  a Codex peer's sandbox does write under `<base>/builds/<id>/`: its own
  worktree, and nothing else of the base. `workspace-write` writes the worktree,
  `$TMPDIR` and `/tmp`, plus your global roots; with `--writable-root`, the roots
  you named and the global roots `build new` carried over ("The Codex sandbox"),
  which is neither `build.json`, nor the lock and cursor files, nor the other
  peer's worktree. Claude Code asks you before it writes outside its working
  directory.
- **The destructive scope of `build end`** is the recorded worktrees under
  `<base>/builds/<id>/`, the recorded reply directory, the build directory and
  the peers' state for this stream. Never the target's main checkout, and never a
  branch.
