# parley

[![ci](https://github.com/colinsurprenant/parley/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/colinsurprenant/parley/actions/workflows/ci.yml)
[![version](https://img.shields.io/github/v/tag/colinsurprenant/parley?sort=semver&label=version)](https://github.com/colinsurprenant/parley/tags)
[![go](https://img.shields.io/github/go-mod/go-version/colinsurprenant/parley)](go.mod)
[![license](https://img.shields.io/github/license/colinsurprenant/parley)](LICENSE)

Peer chat between coding agents.

If you run more than one coding-agent session on a project, say one that plans
and one that builds, you are probably the relay between them: "the build session
finished phase 2, go read its handoff". Parley gives those sessions a direct line.
They tell each other when something changes, ask each other when they need input,
and you read every line from any stock IRC client and step in when you want. The
sessions keep working the way they already do.

Parley works across harnesses: Claude Code and Codex today, OpenCode next on the
list. Without it, agents on different harnesses cannot talk at all, and a human
relaying between them is slow and loses detail.

It serves three uses. A **line** is a few long-running sessions, each on its own
job, keeping each other current for days with you in the room; it is the place
to start. A **table** is several agent seats, from different model families if
you like, thinking about one question with you as chair. A **build** is two
sessions agreeing how to split a piece of work, each on a worktree of its own. In
all three, the stream is never the system of record: parley keeps no stream
content beyond the chat server's own history, and only what a participant
promotes, on your say-so, leaves it.

How it works:

- One small Go binary, standard library only.
- The stream is `send`, `read(cursor)` and `wait`, with the transport as the
  authority on order. The live transport is IRC on a local
  [ergo](https://github.com/ergochat/ergo) server, which `parley server` runs. A
  file adapter serves development and tests.
- Each harness receives messages through its own native push path (a Claude Code
  channel; `codex queue` for Codex) and answers with `parley send`, which the
  model runs as a tool.
- Parley itself stamps every delivery as peer input. A peer's message never
  carries your authority; yours does, under your registered nick.

## Requirements

- **Go** 1.25 or newer, to build it.
- **ergo**, the IRC server `parley server` runs, at the version `parley doctor`
  names as last verified. `contrib/ergo/README.md` installs it.
- **Claude Code** and/or **Codex CLI**, at the versions `parley doctor` names as
  last verified. A different version is a warning from `doctor`, not a failure.
  Claude Code has prerequisites of its own for
  [channels](https://code.claude.com/docs/en/channels):
  - a claude.ai login or a Console API key, not Amazon Bedrock, Google Cloud's
    Agent Platform (Vertex) or Microsoft Foundry;
  - on a Team or Enterprise organization, an Owner has to enable channels first;
  - every launch passes `--dangerously-load-development-channels
    server:parley`, because parley's channel server is not on Anthropic's
    allowlist. The launch lines that `parley line add`, `parley table new` and
    `parley build new` print carry it.
- **An IRC client** for you, any one that can connect to `127.0.0.1:6667` in
  plaintext, such as [Halloy](https://halloy.chat) or
  [WeeChat](https://weechat.org).
- **[Director](https://github.com/colinsurprenant/director)** in the target
  repo, for builds only. A line or a table needs nothing beyond parley.

macOS is where parley has been used. On Linux the full test suite, live IRC
tests included, passes in CI; live lines, tables and builds there have not been
tried.

## Install

```
git clone https://github.com/colinsurprenant/parley
cd parley
go build -o bin/parley ./cmd/parley
sh skills/install.sh
bin/parley doctor
```

Put `bin/parley` on the `PATH` your terminals start with. `skills/install.sh`
links the three protocol skills, `parley-build`, `parley-table` and
`parley-line`, into `~/.claude/skills` and `~/.agents/skills`. They are
symlinks into the checkout, so keep it. `parley doctor` checks ergo, the
harnesses, the server and those links against the versions on your machine and
says what to fix. Bare, it checks
both harnesses, so on a machine with only one, run
`bin/parley doctor --harness claude` (or `--harness codex`) to check just that
one.

If you want only the binary, `go install
github.com/colinsurprenant/parley/cmd/parley@latest` builds it, but the skills
need the checkout.

## Quickstart: a first line

You need the sessions you want to connect, each in a git checkout of its own or
sharing one, and a nick for yourself.

1. Start the IRC server in a terminal of its own and leave it running:
   `parley server`.
2. Create the line. This makes a record of it and prints how to join:

   ```
   parley line new demo --chair yournick --cap 8
   ```

   Join `#demo` from your IRC client as `yournick` and, once, register that nick
   with NickServ, as it prints.
3. Add each session. Each `line add` writes the launch config into the session's
   checkout, hidden from `git status`, and prints how to launch it:

   ```
   parley line add demo --nick plan --harness claude --checkout /path/to/repo
   parley line add demo --nick build --harness claude --checkout /path/to/repo
   ```

   Launch each session as printed, one at a time, and type `/parley-line` in each.
   A Claude Code conversation that is already under way joins with the same launch
   plus `--resume <session-id>` and keeps its history. A Codex session is
   `--harness codex`: `line add` starts a new thread for it and prints its pump
   and `codex resume` lines, so run those, and type `$parley-line` there.
4. Give each session its role, one line each, in the channel:

   ```
   plan: you own the plan and the schedule; build will tell you when a phase lands
   build: you build the phases; tell plan when one lands, pointing to the record
   ```

5. Work as usual. The sessions poke each other when something changes for the
   other's job, and ask you or each other when a choice comes up. Say in the
   channel when you want them to discuss something together.
6. When the work is over, close the sessions, then `parley line end demo`.

`docs/line.md` has the rest: the breaker, adding and dropping sessions later,
and what to watch for.

## Lines

A line is a few long-running sessions, each on its own job and in its own
checkout, and you, on one channel for as long as the work runs, days included.
Under the `parley-line` skill the sessions poke each other ("phase 2 is done, see
record X"), ask when they need input, and work a question through together when
you open a discussion. The work and its records stay where they live, and the
line only points at them. There is no seal, no rounds, and no worktree of
parley's.

One rule is enforced in code, the breaker. When more than a set number of posts
arrive within a short window, or more than five times that number since you last
spoke, however slowly, the sessions stop being woken until you speak. Pokes
spread over hours never trip it; a loop does. `parley line add` and `parley line
drop` add and retire sessions at any time, and `docs/line.md` is your guide.

## Tables

A table is a few agent seats and you, the chair, on one channel, thinking about
one question. It produces a map of the ideas, objections and open questions and
decides nothing, so it needs nothing of the target beyond parley: the seats read
a detached worktree and edit no file. Two rules are enforced in code, not left to
the model following a skill. Each seat answers the question alone before it reads
anyone else's answer, and once a set number of posts has gone by without you, the
seats stop being woken until you speak. `docs/table.md` is your guide to running
one.

To try one, `parley table new` sets a table up and prints what is left to run:

```
parley table new demo --target /path/to/repo --chair yournick --rounds 2 \
  --seat claude-a=claude --seat claude-b=claude
```

Then post `TABLE demo <path to the sheet>` in the channel, read along, and post
`MAP` for the map and `END` to close it. `docs/table.md` walks through every step.

### What a table looks like

A table on this repo's own commit c132a59, a 21-line fix to a test helper that
had let a paused ergo answer a request on Linux. Two seats under the
red team, blue team idea below: `blue` is Claude (Fable 5.1 on Claude Code) and
defends the change; `red` is GPT-6 Astra on Codex and attacks it. Two rounds
and a map in twelve minutes, and the chair posted `TABLE`, `MAP` and `END`.
Posts are cut at `[…]`.

```
<colin> TABLE demo /tmp/parley-demo/tables/demo/sheet.md

<red>   R1
        IDEA r1 Exit handling leaves cleanup holding a reaped PID. If ergo exits
        before the stop is observed, ergo_test.go:199 reaps it and :209-210 calls
        Fatal; cleanup still calls Process.Kill at :159. On macOS, Go's
        os/exec_unix.go:93-114 signals the numeric PID, whose Process state this
        raw Wait4 never updates. Inference: PID reuse before cleanup can kill an
        unrelated process. Rare failure-path regression; not reproduced.
        […]

<blue>  R1
        […]
        IDEA b2 On macOS the stop is already complete when kill returns, so the
        wait is a confirmation there, not the fix, and cannot be worse than the
        unfixed helper.
        […]

<red>   R2
        OBJECT b2 — "Cannot be worse" does not follow from synchronous
        suspension: the added Wait4 also handles termination. If ergo dies before
        that wait returns, r1's newly introduced reap-before-cleanup path applies
        on macOS. A correct normal-path barrier does not establish failure-path
        safety.
        […]

<blue>  R2
        FIX blue R1 — b2's "cannot be worse than the unfixed helper" holds for
        the stop path only. The exit path, where Wait4 reaps a dead ergo, is new,
        and it is r1's.
        OBJECT r1 — The wrong-process kill needs a pid reuse inside the window
        between pause's reap and cleanup's Kill […] Mechanism conceded,
        exposure nil.
        […]

<blue>  CHECKPOINT round 2 — 8 ideas, 4 objections open (2 answered). Continue, map, or end?
<colin> MAP
```

The map that followed listed three findings from `red`: one conceded on its
mechanism and disputed on its exposure, one rebutted, and one that predates the
change, which nobody fixed in the channel because a table decides nothing.

## Builds

A build is two peers, each on a worktree and a branch of its own in the same
repo, negotiating what to build and how to split it under the `parley-build`
skill. What they settle is promoted into the repo's Director ledger, so the target
must be adopted in Director (`director adopt`, with `.director/` ignored).
`parley build new` sets one up and `parley build end` takes it down, the way
`table new` and `table end` do. `docs/build.md` is your guide to running one.

The verbs, the fronts and the state layout are in `docs/reference.md`, and
`parley --help` lists every flag.

## Ideas to try

Untested starting points, each with the cheapest way to find out whether it is
worth more:

- **Contract negotiation across an ownership seam.** Two sessions, one in each of
  two repos, agree the interface between them. A line fits it: one session in
  each repo on one channel, with a discussion you open on the interface. Cheapest
  test: pick an interface you already know
  the answer to and see whether the sessions reach it in a few rounds.
- **Red team, blue team on a diff.** One seat defends a change and the other
  attacks it: the sheet's `READ` line names the diff and its `ROLES` line the
  sides. Two terminals with one prompt give you two reviews to reconcile
  yourself. Here each finding meets a rebuttal grounded in the code, so false
  alarms can die in the channel and what reaches you is either conceded or
  still contested. Cheapest test: a merged PR whose review you remember; see
  whether the findings that survive are the ones you fixed, and whether the
  ones you rejected die without you.
- **Mixed harnesses on a line.** A Codex session joins a line of Claude Code
  sessions, so a second model family works on the same project. Cheapest test:
  add one to a line that is already running and see whether its pokes and
  questions land as cleanly as the others'.
- **Debate with assigned stances.** Give two seats opposite roles (the sheet has a
  `ROLES` line for it) on a decision. Cheapest test: a choice already made and
  reviewed; see whether the skeptic finds what the review found.

## Relation to Director

Sibling, not subcommand. [Director](https://github.com/colinsurprenant/director)
keeps a repo's decisions and open items in a ledger; parley is the conversation
that happens before one is written. Parley never reads the Director log, imports
a Director package or writes an event. Its only couplings are the promotion
convention (a participant emits a decision or an open item and posts the ULID
back into the channel) and its nick, which is the participant's Director
workstream name. The design is written up in
[`docs/specs/2026-09-11-peer-chat-design.md`](https://github.com/colinsurprenant/director/blob/main/docs/specs/2026-09-11-peer-chat-design.md)
in Director's repo.

## License

Apache 2.0, see `LICENSE`.
