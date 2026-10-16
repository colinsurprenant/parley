# parley

[![ci](https://github.com/colinsurprenant/parley/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/colinsurprenant/parley/actions/workflows/ci.yml)
[![version](https://img.shields.io/github/v/tag/colinsurprenant/parley?sort=semver&label=version)](https://github.com/colinsurprenant/parley/tags)
[![go](https://img.shields.io/github/go-mod/go-version/colinsurprenant/parley)](go.mod)
[![license](https://img.shields.io/github/license/colinsurprenant/parley)](LICENSE)

Peer chat between coding agents.

Parley lets coding-agent sessions (Claude Code and Codex today) talk to each
other as peers over a plain chat stream, with you in the room through any stock
IRC client. Without it, agents on different harnesses cannot talk at all, and a
human relaying between them is slow and loses detail. With it they negotiate in
the open, where you can read every line and step in.

It serves two uses. A **table** is several agent seats, from different model
families if you like, thinking about one question with you as chair. A **build**
is two sessions agreeing how to split a piece of work, each on a worktree of its
own. In both, the stream is never the system of record: parley keeps no stream
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
    allowlist. The launch lines that `parley table new` and `parley build new`
    print carry it.
- **An IRC client** for you, any one that can connect to `127.0.0.1:6667` in
  plaintext, such as [Halloy](https://halloy.chat) or
  [WeeChat](https://weechat.org).
- **[Director](https://github.com/colinsurprenant/director)** in the target
  repo, for builds only. A table needs nothing beyond parley.

macOS is where parley has been used. On Linux the full test suite, live IRC
tests included, passes in CI; live tables and builds there have not been tried.

## Install

```
git clone https://github.com/colinsurprenant/parley
cd parley
go build -o bin/parley ./cmd/parley
sh skills/install.sh
bin/parley doctor
```

Put `bin/parley` on the `PATH` your terminals start with. `skills/install.sh`
links the two protocol skills, `parley-build` and `parley-table`, into
`~/.claude/skills` and `~/.agents/skills`. They are symlinks into the checkout,
so keep it. `parley doctor` checks ergo, the harnesses, the server and those
links against the versions on your machine and says what to fix. Bare, it checks
both harnesses, so on a machine with only one, run
`bin/parley doctor --harness claude` (or `--harness codex`) to check just that
one.

If you want only the binary, `go install
github.com/colinsurprenant/parley/cmd/parley@latest` builds it, but the skills
need the checkout.

## Quickstart: a first table

You need a git repository for the table to think about (nothing is written into
it but a worktree registration) and a nick for yourself, the chair.

1. Start the IRC server in a terminal of its own and leave it running:
   `parley server`.
2. Set the table up. This makes a worktree, the launch files and a record of the
   table, and prints what is left for you to run, in order:

   ```
   parley table new demo --target /path/to/repo --chair yournick --rounds 2 \
     --seat claude-a=claude --seat claude-b=claude
   ```

   A Codex seat is `--seat codex-a=codex`.
3. Run what it printed. Join `#demo` from your IRC client as `yournick` and, once,
   register that nick with NickServ. Then start each seat's session in the order
   shown, type `/parley-table` in it (`$parley-table` in Codex), and wait until
   it says it is waiting for the TABLE line.
4. Write your question into the sheet `table new` copied for you (it prints the
   path) and post one line in the channel: `TABLE demo <path to the sheet>`.
5. Read along. A bare `MAP` asks only the checkpoint seat, the first one in the
   sheet's SEATS, for a map of the ideas, objections and open questions; the
   other seats then correct it or reply, and that seat posts the final map.
   `END` closes the table.
6. Close the seats, then `parley table end demo`.

`docs/table.md` has the rest: every flag, what to watch for, and how a table is
taken down.

## Tables

A table is a few agent seats and you, the chair, on one channel, thinking about
one question. It produces a map of the ideas, objections and open questions and
decides nothing, so it needs nothing of the target beyond parley: the seats read
a detached worktree and edit no file. Two rules are enforced in code, not left to
the model following a skill. Each seat answers the question alone before it reads
anyone else's answer, and once a set number of posts has gone by without you, the
seats stop being woken until you speak. `docs/table.md` is your guide to running
one.

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
  two repos, agree the interface between them. `build new` targets one repo, so
  this one is set up by hand. Cheapest test: pick an interface you already know
  the answer to and see whether the sessions reach it in a few rounds.
- **Red team, blue team on a diff.** One seat defends a change and the other
  attacks it: the sheet's `READ` line names the diff and its `ROLES` line the
  sides. Two terminals with one prompt give you two reviews to reconcile
  yourself. Here each finding meets a rebuttal grounded in the code, so false
  alarms can die in the channel and what reaches you is either conceded or
  still contested. Cheapest test: a merged PR whose review you remember; see
  whether the findings that survive are the ones you fixed, and whether the
  ones you rejected die without you.
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
