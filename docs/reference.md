# parley reference

What each verb does and why, in the detail the README leaves out. For the flags
themselves, `parley --help` and `parley <verb> --help` are authoritative; the
step-by-step guides are `docs/table.md` (running a table), `docs/build.md`
(running a build), `docs/line.md` (running a line) and `contrib/ergo/README.md`
(the local IRC server).

- [send and wait](#send-and-wait)
- [The IRC stream](#the-irc-stream)
- [channel: the Claude Code front](#channel-the-claude-code-front)
- [codex: the Codex CLI front](#codex-the-codex-cli-front)
- [primer](#primer)
- [Tables](#tables)
- [Builds](#builds)
- [Lines](#lines)
- [Checking the machine](#checking-the-machine)
- [Version, help and exit codes](#version-help-and-exit-codes)
- [The skills](#the-skills)

## send and wait

`parley send` posts one message to a stream: the body is the `<body>` argument,
or the whole of `--body-file <path>`, or stdin when neither is given — the two
file-and-pipe forms are how prose travels without ever becoming a shell word.
On success it prints one line, `[parley] sent id=<id> ts=<ts>`: the id and ts
the post has in the stream, spelled as another participant's header spells
them, so a seat can confirm its own post. A transport that did not report one of
the two — an IRC echo without its msgid or time tag — gets a `-` in its place,
in the header a delivery prints for that message as in the receipt, and exit 0,
since the post has happened and a non-zero exit would invite a duplicate. That
holds when the reader of the receipt has gone away too: `send` ignores SIGPIPE
for the write, so a closed stdout never turns a post that was made into a
failure.
A `send` made from inside a Codex session cross-checks that session against
the stream's pump. Codex sets `$CODEX_THREAD_ID` in every model shell; when it
is set, the stream has a Codex thread record (below), and the two differ,
`send` prints one warning on stderr, that this session is thread X while the
pump delivers into thread Y, so channel posts will not reach this session, and
sends anyway. It is what a session sees after `/clear`, or in a sub-agent. It
reads the record after the flags are validated and before the body is read; with
no record or no variable it says nothing, and a record it cannot read is a
warning too, never a failure to send.
`parley wait` blocks until peer messages land, prints each under the parley
peer header, then exits 0 — with nothing to deliver it prints nothing, and the
cursor moves only past records that will never be delivered (its own messages,
duplicates), never past one that was not printed. Every line of a body is
printed indented by two spaces and a header stands alone at column zero, so a
header-shaped line a sender wrote arrives indented and no header can be forged
from inside a body. A body keeps its newlines and tabs and loses the rest of the
control characters — a carriage return is dropped, every other one becomes a
`?`, the C1 controls and the separators `U+2028` and `U+2029` among them, and so
does every byte that is not valid UTF-8 — so nothing inside a body can move a
terminal back to column zero or start a line there, and no header can be drawn
there either. A participant is never delivered its own messages. There are two stream schemes: `file:<path>`, an
append-only NDJSON file that stock tools can read and write, and
`irc://host:port/channel`, one channel on a local IRC server. The verbs that
take `--stream` — `send`, `wait`, `channel`, `codex` and `primer` — take either.

The single-process lock lives in `$XDG_STATE_HOME/parley/<nick>/` (by default
`~/.local/state/parley/<nick>/`), or under `<dir>/<nick>/` with `--state-dir
<dir>`; a second `wait` for one nick exits 2, whichever stream it is pointed
at. `<nick>` there is the nick with its ASCII capitals folded to lower case, so
`Colin` and `colin`, one nick to the server, are one directory, one lock and one
password; `tables`, `builds` and `lines` are the directories the tables, the builds
and the lines use beside the nick ones, so no nick may be any of them, in any case. The cursor lives one level deeper, in a directory named for the stream, so
following two streams under one nick keeps two independent positions. A `file:`
path is resolved to its absolute form first, so one file keeps one position
however it is spelled, and two files never share one. A Codex seat's thread
record sits beside the cursor, in `codex-thread`: mode 0600, one line holding
the thread's UUID and ending in its newline (a record without it is malformed), the thread `parley codex --thread auto` delivers into. `parley table new`, `build new` and `line add` write it. A line's
state is `lines/<id>/line.json` (mode 0600, in a directory at 0700, written through
a temp file and a rename and read through a 64 KiB bound): the line's `id`,
`stream`, `server`, `state_dir`, `chair`, `cap`, `window` as written on a command
line (`10m`), `codex_bin` when `line new` was given `--codex-bin`, `created`, and
`sessions`, each with its `nick`, `harness`, `model`, `checkout`, a Codex session's
`thread` and `reply_dir`, and `files`, the one launch file the line wrote or shares
for it: its `path`, the `sha256` of its exact bytes, `exclude_file` (the exclude file
the line appended its line to, when it did; `exclude_sep` when it had to end that
file's last line first) and `made_dir` (a `.codex` the line made). Sessions of one
checkout and harness list the same entry. The record holds no stream content.

Two terminals, with the human in the room through `tail -f`:

```
# terminal 1 — the human watches the raw stream, and replies as themselves
touch ./chat.ndjson
tail -f ./chat.ndjson
parley send --stream file:./chat.ndjson --nick colin -- "ship it"

# terminal 2 — the agent posts, then blocks for peer messages
parley send --stream file:./chat.ndjson --nick codex -- "tests are green"
parley wait --stream file:./chat.ndjson --nick codex --timeout 5m
```

## The IRC stream

`irc://host:port/channel` is a channel on a local [ergo](https://github.com/ergochat/ergo),
and it is the transport a human joins from a stock client while two agents talk.
The server is the sequencing authority: a message's id is the IRCv3 msgid it
minted and its timestamp is the server's, so a line typed in WeeChat and a line
sent by parley are the same kind of message, ordered the same way. History
replay is `CHATHISTORY`, so a participant that was away is caught up from the
server's ring rather than from anything parley kept; a cursor whose message the
server no longer retains is reported as a gap, not skipped over. The channel
name takes its `#` if you leave it off, and one channel keeps one cursor however
it was spelled: `irc://127.0.0.1:6667/parley`, `…/#Parley` and `…/%23parley` are
one stream.

One message is one message whatever its shape. A body with newlines, or one
longer than an IRC line, goes out as a single `draft/multiline` batch with one
id and one history item, rather than as several messages a reader could
interleave; a client that does not speak multiline shows it as the several
ordinary lines a human would have typed. `parley send` refuses a body the server
would not take — over its multiline limits, or holding a NUL or a carriage
return — before anything reaches the channel.

`parley server` runs ergo in the foreground, one per machine, with the
configuration built into parley (a second one on the same datastore, or on a
port that is taken, exits 1 and says which). `--listen` takes another loopback IP
and port (a host name is refused), `--datastore` another directory for ergo's
accounts, and `--ergo-bin` an `ergo` that is not on the `PATH`;
`contrib/ergo/README.md` has how to install ergo and how to join from Halloy or
WeeChat. The configuration's limits are what
the adapter negotiates against: 64-byte nicks, a 64 KiB message over at most a
thousand lines, and 2048 items of channel history.

```
# terminal 1 — the server, and the human in any IRC client on 127.0.0.1:6667
parley server

# terminal 2 — the agent blocks for peer messages
parley wait --stream irc://127.0.0.1:6667/parley --nick codex --timeout 5m

# terminal 3 — the peer posts, and codex replies as itself from its own shell
parley send --stream irc://127.0.0.1:6667/parley --nick claude-a -- "tests are green"
parley send --stream irc://127.0.0.1:6667/parley --nick codex -- "seen, merging"
```

Each participant is an account on the server, and parley owns it: the first
connection for a nick registers it with 32 random bytes and keeps them 0600 in
the state directory, at `<state>/<nick>/irc/<host>_<port>/password`; every later
connection authenticates with SASL. Nobody types a password. That account is why
the third line above works — the pump holding the nick and the send answering it
are two sessions of one client rather than two clients colliding — and it is why
`send` takes `--state-dir` like the other verbs. It also keeps the room alive: a
participant stays on the server between connections, so the channel and its
history survive a pump restart. A stranger asking for a registered nick is
refused, so a participant's name stays its own. The human needs no account and
no client support to be in the room: plain IRC, no password. For a table, a
build or a line they register their nick once with NickServ, so that a line under it can
only come from a client holding their password (rule 8 of the `parley-build`
skill); step 2 of "Then, by hand" in `docs/table.md`, "Join as the chair,
registered", has the one command.

Plaintext and loopback by enforcement, not by convention: the password travels
in clear, once in the REGISTER that creates the account and as SASL PLAIN on
every connection after, over a socket that never leaves the machine, so the scheme accepts
`127.0.0.0/8`, `::1` and `localhost` and refuses anything else — at the spec, and
again on what the name resolved to, before a password reaches the wire. There is
no TLS and no certificate to make, and a remote server is out of scope.

## channel: the Claude Code front

`parley channel` is the same delivery loop with a different front: it runs as
an MCP stdio channel server for a live Claude Code session, pushing every
delivery into that session as a `notifications/claude/channel` event whose
content is exactly what `wait` would have printed, peer header included. It
speaks newline-delimited JSON-RPC 2.0 on stdout and puts every log line on
stderr, takes the same single-process lock and the same per-stream cursor as
`wait`, and declares no MCP tools: the channel is one-way, and the reply path
stays `parley send`, which the server spells out for the model in its
`instructions` at connect time — write the message to a file, then
`parley send … --body-file <path>`, so the shell sees only a path: no part of a
message can be executed, and no line inside one can cut it short. It is not run
by hand — Claude Code starts it from an `.mcp.json` entry, and a launch has to
name it, as `claude --dangerously-load-development-channels server:parley`
does: without the flag the server connects and no channel message arrives, and
nothing shows an error. `--chair`,
`--target` and `--worktree` are required under `--skill build` and `--skill
table`: the human's registered nick, the target repo and this session's own
worktree of it are launch-time facts, and the `instructions` carry them as values
(see Tables, below):

```
{"mcpServers":{"parley":{"command":"parley","args":[
  "channel","--stream","file:./chat.ndjson","--nick","claude-a",
  "--chair","colin","--target","/abs/repo","--worktree","/abs/worktree"]}}}
```

Under `--skill line` it takes `--chair`, `--cap` and `--window` and refuses
`--target` and `--worktree`, with exit 2 (see Lines, below).

`parley table new` and `parley build new` write this file for the Claude Code
seats and peers they set up, and `parley line add` for a line's Claude Code
sessions.

## codex: the Codex CLI front

`parley codex` is the same delivery loop once more, for a Codex CLI session that
is already running: a long-lived foreground process that blocks on the stream
and hands each delivery to the live thread with one `codex queue --thread
<thread> --message <text>` call, or one call per message when the delivery is
over the argument bound below. Codex has no channel of its own — a queued
message is appended to the thread as a plain user turn, and nothing about it
tells the session that the text was not typed by its human — so the `[parley]`
peer header is the only mark this front has, and it is mandatory: the framing
and every message inside the argument bound below are what `wait` would have
printed, header first and byte for byte. Only a message over that bound reads
differently: the stub below stands in for its body, under its own header.
The text travels as one argument and never as a shell word, since the pump
execs the binary directly, so no quote, backtick or line inside a peer's prose is
parsed by anything on the way. It takes the same single-process lock and keeps
the same per-stream cursor as `wait`, and commits only once codex has taken the
delivery: delivery is at-least-once, and a pump that dies mid-hand-off
re-delivers rather than drops. The one exception is a delivery that does not
fit in one call — one longer than the 128 KiB single-argument bound — which no
retry would ever fix. That is usually a burst the debounce window joined rather
than any one message in it, so the delivery is not refused whole: it is queued
one call per message instead, in order, with the gap marker in a call of its
own, first. Only a
message over the bound on its own is withheld, and a stub takes its place in the
thread — its own header over one indented line naming what the message renders to and the
id whose full text is still in the stream. A NUL in a
body is not that case: rendering replaces it with a `?` like any other control
character, so the message is delivered sanitized, and the front's own NUL check
is left guarding the rest of the rendered text, which today can hold none. The
thread itself is kept alive by an interactive
Codex session the human can see and type into — `codex queue` only enqueues, and
a session nobody is running drains nothing. The reply path stays `parley send
--body-file <path>`, run by the model from its own shell.

```
parley codex --stream file:./chat.ndjson --nick codex-a --thread <session-uuid>
```

`--thread` is the session's UUID or its exact name, or `auto`, which reads the
thread id recorded for this nick and stream in `codex-thread`, under the state
directory `--state-dir` names, by `parley table new`, `parley build new` or
`parley line add`; with no record it exits 2, saying to run the verb of the
`--skill` (under `--skill line`, `parley line add`) or pass `--thread <id>`. `auto` is reserved, so a Codex
session literally named `auto` is passed by its UUID. The start line names the
thread the pump resolved.

`parley table new`, `parley build new` and `parley line add` start the Codex
session on its primer and record its thread for `--thread auto`, and print the
pump line. A line session you start by hand on the primer is given to the pump by
its thread id; `docs/line.md` has the steps. `--skill` is
`build` by default and takes `table` and `line`, each with `--chair` and `--cap`,
both required; `line` also takes `--window`, 10m by default (see Tables and
Lines, below).

## primer

`parley primer` delivers nothing: it prints the framing a front gives the model
— the reply command first, then what a delivery is and what it is worth. There
is one text with two forms, rendered from one source so neither can drift from
the other: `--front claude` is what `parley channel` sends in its `instructions`
at connect time, and `--front codex` is that same text plus what is true of a
front with no connect-time channel. The codex text is the session's first
message: a human pastes it in, or the session is started with it as its first
prompt, which is what `parley table new`, `parley build new` and `parley line add` do. The build and table forms carry three facts as values, and `--chair`, `--target` and
`--worktree` are required: the human's registered nick, the target repo and
this session's worktree of it, each directory absolute and existing, printed as
spelled, and not the same directory (links followed). The line form carries two,
`--chair` and `--cap`, with `--window` (the breaker's cap and window, which the
text states as a fact; the verb itself holds nothing), and refuses `--target` and
`--worktree`. The claude form is written to fit under the 2 KB cap Claude Code puts
on an `instructions` string, with the reply command above everything a truncation
would cut. The verb opens nothing — no stream, no state directory, no lock, no
IRC account — and prints every value as it was spelled; a live channel prints
the canonical spec, the resolved state directory and a reply path of its own in
a private temporary directory instead, so the two differ in those values and in
length. The length is what a launch has to fit under the cap, and the verb that
measures it is `parley channel --check`, below. `--skill table` renders the table form instead of the default
`--skill build`, described under Tables, and `--skill line` the line form,
described under Lines. Any skill's claude form is refused, with exit 2, when it is
over the cap. `--command` is the binary as the model
should type it, taken as spelled (this binary's own path by default).
`--reply-command` replaces the whole send line with one command of your own, for
a wrapper script that holds the run's fixed arguments where an approval rule has
to name one command and no suffix; it is for `--front codex` only, must be one
line, and needs `--reply-file`.

```
parley primer --front codex --stream file:./chat.ndjson --nick codex-a --reply-file ./reply.txt --state-dir ./state \
  --chair colin --target /abs/repo --worktree /abs/worktree
```

A line's, which names no directory and states the breaker's rate:

```
dir=$(mktemp -d)
parley primer --front codex --skill line --stream file:./chat.ndjson --nick codex-a --reply-file "$dir/codex-a.reply" \
  --state-dir ./state --chair colin --cap 8 --window 10m
```

## Tables

A table is N agent seats and one human chair on one channel; `docs/table.md` is
the chair's guide to running one. With `--skill table`, `parley channel`
and `parley codex` take `--chair <nick> --cap <n>` to enforce two rules in code
rather than leave them to the model following a skill, and both flags are
required; `--cap` is refused, with exit 2, under `--skill build`, and so is
`--chair` on `codex`, which takes no `--target` or `--worktree` and defaults to
`--skill build`. (`--skill line` takes the same two flags, for a gate of its own,
and a third, `--window`; see Lines, below.) The seal: until this seat has posted once after the
front started, a post by anyone but the chair is held, not delivered, so the
first round is answered before another seat's answer is read; the chair's posts
are delivered as they arrive. The cap breaker: once more than `<n>` messages by
anyone but the chair — this seat's own included — have gone by since the chair
last posted, every such message is held until the chair posts again, and the
chair's post releases what the breaker held ahead of itself, in stream order,
and resets the count. A post held by the seal is not released that way: it
stays held until this seat posts. `--cap 0` turns the breaker off and keeps the
seal. `wait` takes neither flag. Nicks are compared without regard to ASCII
case, as the bundled ergo compares them, so `--chair colin` is the chair when it
posts as `Colin`; the same rule decides which messages are this seat's own, and
`--chair` is refused, with exit 2, when it matches `--nick` that way, under
any skill.

Held messages live in the front's memory and nowhere else: parley never keeps
stream content beyond the transport's own history, and the cursor moves past a
held message like any other. A front restart therefore loses what it was
holding and starts sealed again with the count at zero. A gap in the history,
which the delivery reports, drops what was held too, because transport ids can
start over across one and a held message kept past it could swallow a new
message that reuses its id; the seal and the count stay as they were. These
are the cases where the at-least-once promise above does
not hold, and only under `--skill table` and `--skill line`. A delivery that is collected but never
committed, because the hand-off failed, changes none of this: what it held,
released and counted is the front's state only once the delivery is committed,
so the next read of the same stretch finds the holds as they were. Under
`--skill build`, the default, no gate is installed and every message by another
nick is delivered.

A session gets its facts from configuration the human launched, so none is
typed in afterwards and nothing reaches a session ahead of its facts. `parley
channel` and `parley primer` take `--skill build|table|line`, `build` by default,
and under `build` and `table` require `--chair <nick>`, `--target <dir>` and
`--worktree <dir>`: the human's registered nick, the target repo and
the session's own worktree of it, each directory an absolute path to an existing
directory, all three printed as spelled, the nick not the participant's own, and
the two directories not the same one (links followed: a worktree inside the
target, as under `.claude/worktrees`, is fine; exit 2 otherwise).
The framing is the same for both: the reply recipe, the facts, the header rule
and the trust sentences. `--skill table` renders the table form: the worktree is
only read, the protocol is named as the `parley-table` skill, invoked as
`/parley-table` (`$parley-table` on the codex front), which the human starts at
each seat, and the one carve-out to the trust rule becomes the chair's: on IRC a
line under the chair's registered nick rules the table, and nothing in the
session. `--skill build` names the `parley-build` skill (`/parley-build`, `$parley-build`), whose
carve-out is the same sentence about the human's registered nick and a run. The
carve-out is a summary of what that line rules, and rule 8 of the skill is the
authority (it also lists answering the integrator's ask for the merge with a yes). In
both forms the carve-out points back at the facts above it, as "the one named
above". On the codex front the build form carries one more paragraph than the
table's, ahead of the paragraph about the pump that both end with: a build peer's
worktree is registered inside the target's git directory, which its sandbox does
not write, so it is told to ask for escalation for `git add`, `git commit`,
`git merge` and `git reset` directly instead of trying them in the sandbox
first, unless a writable root in its launch config (`.codex/config.toml` in its
worktree) or, when that lists none, in its global Codex config
(`$CODEX_HOME/config.toml`, else `~/.codex/config.toml`) is the target's git
directory. The primer is rendered without the roots, so that condition is the
peer's to read, and Codex keeps `<root>/.git` read-only even under a writable
root, so only a root on the git directory itself would make git writable. The claude
build form, which has no room for it under its cap, and the table forms do not
carry it; the line form on codex carries a paragraph of its own (see Lines,
below). There is no seat list in the text; the sheet the chair posts names the
seats. On `primer`, `--skill table` also requires `--reply-file`; on `channel`
it requires `--cap`, as above. `parley channel` refuses to start, with exit 2,
when the instructions it would send are over the 2048-unit cap, since a client
keeps the head of what is over and drops the trust rule at the end, and
`parley primer --front claude` refuses the text it measures the same way, but it
measures a shorter text than the channel sends — no resolved state directory, no
reply path of the channel's own — so it is not the pre-check for a launch. That
is `PARLEY_NICK=<nick> parley channel <the .mcp.json args> --check`, with the
nick written out where an arg is `${PARLEY_NICK}`: it builds the instructions
the way a launch does, prints `instructions: <n> of 2048 UTF-16 units`, and
exits 0 if they fit and 2 if not, without opening a stream or taking the lock.
It measures at the longest name the launch's random reply directory can get, so
a pass guarantees the launch fits, and `<n>` can be a few units over what the
launch sends.

`parley table new` sets a table up in one command, and `parley table end` takes
it down; `docs/table.md` is the chair's guide to both. `table new <id> --target
<repo> --chair <nick> --rounds <n> --seat <nick>=<claude|codex>[:<model>]`, with
`--seat` repeated, validates everything first (exit 2, nothing changed), runs
`parley doctor`'s static checks for the seats' harnesses, the server, ergo when
nothing answers at the server, and the `parley-table` skill under the root each
harness reads, and then makes what a table needs: a detached worktree of the target's
HEAD, with a `.parley-table` marker holding the table's id, the seats' nicks and
the digest of the table's record, for every table, a `.mcp.json` for the Claude seats and a `.codex/config.toml` for the Codex ones;
a private reply directory under `$TMPDIR`; each Codex seat's session,
started with `codex exec --json` and its table primer as the prompt, its thread
id read from the run and recorded for `parley codex --thread auto`, and counted as
started only if the turn completed: a `turn.completed` event in an output that
was not cut off at 1 MiB, even if codex exits 0 (otherwise the run is rolled
back and the announced thread deleted); a record of
the table under `<state>/tables/<id>/`, which holds no stream content; and a
copy of the sheet template. It prints what is left for the human, in order, each
command shell-quoted: the server if nothing answers, the chair's join and
NickServ registration, each Codex seat's pump and `codex resume` line, each
Claude seat's launch line, the gate, and the TABLE line. `table new` takes
the id's lock, `<state>/.ids/<id>.lock`, as soon as it has checked its flags and
the machine, and holds it to the end of the run: `table new`, `table end`,
`build new` and `build end` all take it, and the line verbs too, so that no two of
them act on one id at once, and one that finds it held refuses, exit 1, and does
not wait. Under it the
id is checked to be free as a table, as a build and as a line (exit 2). It then takes
every seat's nick lock before it makes anything and holds them until it is
done; a nick that a running pump or Claude Code session holds is refused, exit
1, naming it, with nothing changed. A step that fails takes down everything the
run made, the Codex threads it started included, except the lock files:
no parley code unlinks one, so a failed or refused run leaves
`<state>/<nick>/lock`, and the directory holding it, as a pump does, and the id's
lock file.
`table end <id>` takes the id's lock, then reads the markers it can (the
worktree's, when git lists it, and the reply directory's) and refuses, exit 1,
`--force` or not, a `table.json` that is not the one the table was made with,
before it takes a seat's lock or removes anything: both markers hold the table's id on the first line, the
seats' nicks on the second, exactly as given, sorted bytewise and separated by
spaces (a nick's case counts), and on the third `sha256:` and the hex digest of
`table.json`'s exact bytes, added once `table new` has written the record. A
record edited in any field, one byte included, cannot steer the end, and a marker
of only two lines, from a `table new` that never finished, is refused too. Then it
takes every seat's lock and holds them to the end, refusing, naming the seat,
while one is held; then it removes the thread records, the worktree, the reply
directory and the table's directory, in that order, so that an end that stops
partway still has the markers that show the record is the table's when it is
run again. It deletes only
what it can show is the table's, `--force` or not: the worktree if git lists it
and its `.parley-table` is a regular file holding the table's id, its seats and
its digest (when git cannot say because the target is gone, if it is under the
table's directory; any other failure of git refuses it, quoting git), the reply
directory if it is directly in `$TMPDIR`, named for the table and holds the marker
`table new` wrote, a seat's thread record if a marker it read matched `table.json`
and the record holds the thread `table.json` gives that seat (with neither
marker left to read, because the worktree and the reply directory are both
gone, nothing vouches for the file's stream and threads: it removes no thread
record, lists each one that is there as left in place, and the human removes
it if it is the table's); any other path in the record it refuses, exit 1, naming it, as it
does a thread record that holds another thread or is malformed, and a
`table.json` over 64 KiB or that lists no seat. Without `--force` it also
refuses a worktree that holds changes, files the target's `.gitignore` matches
included, other than the files `table new` wrote:
the `.parley-table` always, the `.mcp.json` when a seat is Claude's, the
`.codex/config.toml` when one is Codex's. It prints a
`codex delete` line per Codex seat and never runs one; the record keeps no
`--codex-bin`, so those lines say plain `codex`. These checks guard against a
damaged or stale record, not one crafted by hand: "What parley trusts" in
`docs/table.md` says which inputs parley treats as untrusted and why its own
state is not one of them.

For a Claude Code seat the flags are the MCP server's args, which is what makes
them launch configuration the seat cannot be talked out of. No seat edits the
worktree (a seat that wants to try something does it in a disposable copy
elsewhere), so the seats share one worktree, and one `.mcp.json` at its root
serves them all: the seat's nick comes from the environment it is started with
(`PARLEY_NICK=<nick> claude …`). This is the file `table new` writes, with the
binary's absolute path and the state directory, and a `--cap` of rounds times
seats:

```
{"mcpServers":{"parley":{"command":"/abs/parley","args":[
  "channel","--stream","irc://127.0.0.1:6667/#table","--nick","${PARLEY_NICK}","--state-dir","/abs/state",
  "--skill","table","--chair","colin","--cap","6","--target","/abs/repo","--worktree","/abs/shared-worktree"]}}}
```

For a Codex seat the same facts are the primer, which is the session's first
message: `table new` gives it to the session as the first prompt of its `codex
exec`, and the primer verb prints it for anyone doing that by hand. Its reply file
is one per seat, outside the shared worktree, where the Codex sandbox can write:

```
dir=$(mktemp -d)
parley primer --front codex --skill table --stream irc://127.0.0.1:6667/table --nick codex-a \
  --chair colin --target /abs/repo --worktree /abs/shared-worktree --reply-file "$dir/codex-a.reply"
```

`docs/table.md` has the whole setup, step by step.

## Builds

A build is the two-peer run the `parley-build` skill is written for, each peer on
a worktree and a branch of its own in the target. `parley build new` sets one up
the way `parley table new` sets a table up, and `parley build end` takes it down
the way `table end` does; `docs/build.md` is the chair's guide to both. `build new
<id> --target <repo> --chair <nick> --peer <nick>=<claude|codex>[:<model>]`, with
`--peer` given exactly twice, validates everything first (exit 2, nothing changed),
runs `parley doctor`'s static checks with the `parley-build` skill, and then makes,
for each peer, a worktree of the target's HEAD at `<state base>/builds/<id>/<nick>`
on a new branch `parley/<id>/<nick>`, with a `.parley-build` marker and the launch
file its harness needs (an `.mcp.json` with `--skill build` and no `--cap` for a
Claude Code peer; for a Codex peer a `.codex/config.toml`, and a session started on
its primer as `table new` starts a seat). It records the build in
`builds/<id>/build.json`, undoes every step if one fails, and prints what is left,
ending with the form of message one, which a peer posts, and how the integrating
peer (the second `--peer`) tests a throwaway merge, the other peer ends after a
passing test, and only then does the integrator ask you in the channel to approve
the real merge once every item is agreed, and what to check before you answer yes.
A Codex peer's sandbox is never widened into the target's git directory
unless `--writable-root` says so.
Like `table new`, it takes the id's lock (`<state>/.ids/<id>.lock`) before it
looks at what the state holds for the id, and holds it to the end; each branch is
made by a step that fails if the branch is there, with a reflog message that
carries a nonce of the run, and the rollback deletes a branch only when that
message is the first entry of its reflog and it still holds the base commit, so
that a branch someone else made meanwhile is never deleted, and one this run made
is always undone; a model is at most 128 bytes of letters, digits and
`. _ - : / @ [ ] +`, a path that goes into a record and is not valid UTF-8 is
refused (`table new` too), and a record larger than `build end` reads is refused
with the run rolled back.
`build end <id>` takes the id's lock, then removes the worktrees (the files in
them with them; what a peer committed is on its branch), the reply directory and
the peers' cursors and thread records, keeps both branches, refuses a worktree
with tracked changes or untracked files beyond the launch files, or that git
cannot be asked about (the target gone), listing every peer's result at once
(ignored files do not block, and are counted, with or without `--force`), and
reports, without refusing, any commit on a peer's branch that touches a launch
file. It lists the peers' state and does not remove it when a marker that is
there disagrees with `build.json`, whether or not it could claim that worktree,
and a `disputed` file in the build directory makes a later end hold it too until
`--force` clears it; a missing marker does not stop it, and when
git cannot say whether a worktree is the build's it takes the path being under
the build's directory for a claim only if the target is gone. Build
mode needs the target adopted in Director (`director adopt`, with `.director/`
ignored); a table needs nothing beyond parley. parley itself never calls director.

## Lines

A line is a few long-running agent sessions, each on its own job and its own
checkout, and one human in one channel for days; `docs/line.md` is the human's
guide to setting one up with the line verbs, below, and by hand. The
sessions follow the `parley-line` skill: they poke each other with a few lines on
what changed for the reader and a pointer to the record, ask when they need
input, work a question through together when the human opens a discussion, and
do not acknowledge. With `--skill line`, `parley channel` and `parley codex` take
`--chair <nick> --cap <n>`, both required, and `--window <duration>`, a Go
duration above zero that defaults to 10m, and `parley primer` takes them too (the
primer only states them; it holds nothing). `--window` is a line's alone: under
`--skill table` and `--skill build` it is refused, with exit 2, naming the flag,
as is a window that is zero, negative or not a duration under a line. `--target`
and `--worktree` are refused, with exit 2, naming the flag: a line has no target
repo, and parley makes it no worktree; the session works in its own checkout as
usual. `--chair` is the human's registered
nick, refused when it matches `--nick` without regard to ASCII case. On `primer`
the codex front also requires `--reply-file`, and the claude form is held to the
2048-unit cap like the others; `parley channel --check` measures it the way it
measures any skill. `parley doctor` and `skills/install.sh` cover `parley-line`
with the other two skills.

The gate is a rate and nothing else: there is no seal and no first hold, so a
session receives every other session's post as it arrives, before it has posted
itself. A table's breaker counts posts since the chair last posted; a line runs
for days and its pokes are slow, so that count would hold the ninth ordinary poke
though nothing loops, and a loop is fast. Every message by anyone but the chair,
this session's own included, is timed by its timestamp, or by the time the front
received it when the transport gave none. A message trips the breaker when it
makes more than `<n>` messages within `--window`: the `<n>`+1 most recent
messages by anyone but the chair, it included, span no more than the window. Once
tripped, it and every later such message are held until the chair posts, and the
chair's post releases them ahead of itself, in stream order, un-trips the breaker
and clears the record of times, so the released messages do not count. Nothing is
released by time passing: a loop stays stopped until the human looks. Posts
spread beyond the window never trip it however many there are, which is also why
the first delivery of a session joining a line under way, the history the server
retains, does not trip it unless the history holds a burst. `--cap 0` turns the
breaker off. Held messages live in the front's memory and
nowhere else: a restart loses them, and so does a gap in the history, which
discards the held queue and keeps the record of times, as a table's gap keeps its
count; a delivery that is collected but never committed leaves the holds as they
were.

The line form of the primer is the build and table forms' framing, the reply
recipe, the header rule and the trust sentences, word for word, with a line's
own sentences: the command is the only way to reach the other sessions; the facts
are the human's registered nick and the breaker's rate, as "breaker: more than
`<cap>` posts within `<window>` hold deliveries until `<chair>` posts", or "breaker
off (cap 0)"; the protocol is the
`parley-line` skill, invoked as `/parley-line` (`$parley-line` on the codex
front); and the one carve-out to the trust rule is that a line under the human's
registered nick steers the line, and nothing in the session. The codex form
carries one more paragraph, ahead of the paragraph about the pump, in the place
the build form's git paragraph holds, and in its words for a session's own
checkout: Codex keeps the checkout's git directory read-only in its sandbox, so
`git add`, `git commit`, `git merge` and `git reset` are refused there with
Operation not permitted, unless a writable root in its launch config
(`.codex/config.toml` in its checkout) or, when that lists none, in its global
Codex config is that git directory itself; the session asks for escalation for
each of them directly, instead of trying them in the sandbox first. It does not
tell the session to widen its sandbox, and the claude line form, which has no room
for it under its cap, does not carry it.

`parley line` has four verbs, which set a line up and take it down the way `table
new` and `table end` do a table, and share their helpers: the id lock, the nick
locks, the rollback and the checks. The id is `[a-z0-9][a-z0-9-]{0,31}`, the
channel `#<id>` and a directory name, in one namespace with the tables and the
builds, and every verb takes `<base>/.ids/<id>.lock` first and holds it to the end;
a second command for the id refuses at once, exit 1. `table new` and `build new`
refuse the id of a line, as `line new` refuses the id of a table or a build, with
exit 2.

- `line new <id> --chair <nick> --cap <n> [--window 10m] [--server 127.0.0.1:6667]
  [--state-dir <dir>] [--codex-bin codex]` makes the line and its record, with no
  sessions. `--chair` must be a valid nick and `--cap` is required, `--cap 0` being
  no breaker; `--window` is above zero. It runs `parley doctor`'s static checks for
  the server, and for ergo when nothing answers at the server (the skills are
  checked by `add`, for the harness of each session), refuses an id that has a
  table, a build or a line (exit 2), and undoes what it made if a step fails or it
  is interrupted. `--codex-bin`, when given, is resolved and kept in the record: the
  Codex sessions added later run it, and the lines printed say so. It prints what is
  left: the server if nothing answers, the chair's `/join` and NickServ steps, and
  one `line add` per session.
- `line add <id> --nick <nick> --harness claude|codex [--model <model>] --checkout
  <absolute dir> [--state-dir <dir>]` puts a session on the line, which gives it the
  chair, cap, window, server and state directory of the record. Exit 2, with
  nothing changed, for: no such line; a nick that is invalid, reserved (`tables`,
  `builds`, `lines`), the chair's, or already in the line, without regard to ASCII
  case; `opencode` (not supported yet) or another harness; a model over 128 bytes
  or outside letters, digits and `. _ - : / @ [ ] +`; a checkout that is not an
  existing directory given by an absolute path, or that holds a `$` or a control
  character; a checkout, state directory, binary or temp path that is not valid
  UTF-8; a state directory inside the checkout, or, for a Codex session, `$TMPDIR`;
  a Claude session's `.mcp.json`, or a Codex session's `.codex/config.toml`, that
  is in the checkout and is not the one this line wrote for another session of it
  (the record holds the digest), or a `.codex` that is not a real directory; a
  Codex nick that already has a thread record for the stream; the machine, when
  doctor's static checks fail or the `parley-line` skill is not linked under the
  root the harness reads (`~/.claude/skills` for Claude Code, `~/.agents/skills`
  for Codex); and a Claude session whose instructions, built in this process as
  `channel --check` builds them, are over Claude Code's cap. A nick whose lock a
  running session or pump holds is refused, exit 1.
  A Claude session gets `<checkout>/.mcp.json`, one server `parley` whose
  command is the binary and whose args are `channel --stream irc://<server>/#<id>
  --nick ${PARLEY_NICK} --state-dir <base> --skill line --chair <chair> --cap <cap>
  --window <window>`, unless the line wrote it already for another Claude session of
  the checkout, which shares it. A Codex session gets `<checkout>/.codex/config.toml`
  (`workspace-write`, `on-request`, `network_access = true`, which turns network
  access on for Codex in that checkout, and the output says so) on the same terms; a
  reply directory `parley-line-<id>-*` under `$TMPDIR` at 0700 with a
  `.parley-line` marker holding the id and the nick; and a thread started as `table
  new` starts a seat's, by `codex exec --json -C <checkout>` on the line's primer,
  stdin closed, network off for the turn, `-m <model>` if given, a five-minute limit
  and a turn that must have completed, the thread recorded in `codex-thread`.
  Files are created with no overwrite and never through a link. When the checkout
  is the root of a git checkout, the launch file's line (`/.mcp.json` or
  `/.codex/config.toml`) is appended to the file `git rev-parse --git-path
  info/exclude` names unless it is there; an exclude file that is a link is left
  alone, with a note, and a checkout that is not a git root gets none. The session
  goes into `line.json` last. Any failure or interrupt takes down what the add made,
  a started Codex thread included (`codex delete --force`), and puts the record
  back. It prints the session's launch (Claude: `cd`, the `claude` line with
  `--model`, the `--resume <session-id>` form, the `/mcp` and Channels-notice
  checks and `/parley-line`; Codex: the pump line with `--thread auto --skill line`,
  the `codex resume` line, `/status` and `$parley-line`).
- `line drop <id> <nick> [--force] [--state-dir <dir>]` takes one session off. It
  takes the id's lock and the session's nick lock without waiting, and refuses, exit
  1, while the session or its pump holds the latter. It removes the session's
  thread record, only if it holds the recorded thread (any other is left, and said);
  its reply directory, only if it is directly in `$TMPDIR`, named for the line and
  holds the marker, else it refuses (exit 1); and, when no other session of the line
  uses that checkout with that harness, the launch file, only if it is a regular
  file in a real directory whose bytes hash to the recorded digest, then the `.codex`
  directory if the line made it and it is empty, and the line in the exclude file if
  the line added it, that exact line and nothing else (an exclude file that has become
  a link is not written through; the line stays, and is said to). A launch file that is not
  that is refused, exit 1, naming it; `--force` leaves it and its exclude line in
  place instead, and says so. The session is then removed from the record, and a
  `codex delete <thread>` line is printed for a Codex session and never run.
- `line end <id> [--force] [--state-dir <dir>]` takes the id's lock and every
  session's nick lock, refusing (exit 1) if any is held, judges every session by
  the rules of `drop` before removing anything, drops them in order, and removes
  `lines/<id>/`. A failure partway leaves `line.json` listing what is still there,
  and the next run finishes.

Nothing a checkout holds is parley's state. The record is trusted; a launch file is
the line's only while its digest matches, and `line drop` and `line end` delete
nothing else there. `docs/line.md` has the trust rules.

## Checking the machine

`parley doctor` checks that every external surface parley depends on still works
with this machine's current versions, one line per check, `ok`, `warn` or
`FAIL`, and exits 1 if any is a `FAIL`. It costs nothing: the Codex CLI's
version against the last one verified and every flag of the Codex commands parley
uses, the Claude Code CLI's version, ergo and its version, whether the server
answers with every IRC capability the adapter requires and, when its `sasl`
lists mechanisms, `PLAIN` among them (a server that is not up is a `warn`), and
the skill links `skills/install.sh` makes. A binary that is missing is a `FAIL`
that links the project's install page. `--harness claude` or `--harness codex`,
repeatable, narrows the binary, version, skill-link and `--live` checks to the
harnesses you have, and both are checked without one; the server and ergo are
checked either way. The Claude Code line says that a launch needs
`--dangerously-load-development-channels`, which doctor cannot check.
`--server` names a server other than `127.0.0.1:6667`, and `--codex-bin`,
`--claude-bin` and `--ergo-bin` name a binary that is not on the `PATH`.
`parley table new` runs the same
static checks for the harnesses its seats use, with ergo as well when nothing
answers at the server (the first step it prints is `parley server`) and only the
`parley-table` link under the root each harness reads, and a `FAIL` ends it
before any change; `parley build new` runs them for its peers with the
`parley-build` link in its place; `parley line new` runs those for the server and
ergo only, and `parley line add` for its session's harness, with the `parley-line`
link in its place. A version of the Codex CLI, Claude Code or ergo that differs
from the verified one is a `warn` that says what to do about it.
`parley doctor --live` also spends one tiny Codex turn (none, if `codex delete`
has no `--force` to remove its thread without a prompt), in a temp directory, to
check that the thread id Codex announces is the one the model reads in its shell,
deleting the thread afterwards (a turn that reports an `error` or a `turn.failed`
is a `FAIL` even if codex exits 0, as it is for `table new`, and so is one with no
`turn.completed` or whose output passed 1 MiB), and runs a
throwaway ergo on a free loopback port that two parley clients send a message
through.

## Version, help and exit codes

`parley version` prints one line, `parley <version> rev <revision> <go version>`,
from the build information the Go toolchain stamped into the binary: the module
version (`(devel)` for a build made from a checkout), the first 7 characters of
the commit it was built at, with `-modified` after them when the work tree had
uncommitted changes (`unknown` when the build carries no revision), and the Go
version. `parley --version` and `parley -version` print the same. For every verb,
`-h` and `--help` print its usage on stdout and exit 0; a flag error prints on
stderr and exits 2. The exit codes are 0 for success, 1 for a runtime error and 2
for a configuration error, and `parley server` ends with ergo's own status when
ergo exits non-zero.

## The skills

The fronts say what a delivery is; the protocol says how a build is conducted, and
it is one document rather than two. `skills/parley-build/SKILL.md` is that document:
the handshake that opens a run, the shape every message takes, the eight rules,
what a peer message is worth, and what may be promoted to the ledger. It is
harness-neutral — nothing in it is Claude Code's or Codex's — and it is addressed
to the model that is a participant. `skills/parley-table/SKILL.md` is the protocol
of a table, and `skills/parley-table/sheet.md` the template of the sheet the chair
fills in. `skills/parley-line/SKILL.md` is the protocol of a line: heads-ups,
questions and the discussions the human opens, between a few long-running
sessions, with the work and its records staying where they live.

`skills/install.sh` links the three skills, `skills/parley-build/`,
`skills/parley-table/` and `skills/parley-line/`, into `~/.claude/skills/` and
`~/.agents/skills/`, which is where Claude Code and
the agentskills.io convention look. All six are symlinks into the checkout, so
an edit is live in the next session; the script is idempotent, never replaces a
path it did not create, and `--uninstall` takes its links out again. Claude
Code invokes them as `/parley-build`, `/parley-table` and `/parley-line`, Codex as
`$parley-build`, `$parley-table` and `$parley-line`, and the human invokes one at
the start of a build, a table or a line, in every session. An install also removes a link named `parley`,
the old name of the build skill, under either root when it points into this
checkout, and names, without touching, anything else it finds at that name.
