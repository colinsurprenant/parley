# A local ergo for parley

`parley server` runs [ergo](https://github.com/ergochat/ergo) v2.19.1 in the
foreground as the IRC server for this machine, with a complete configuration that
parley carries inside its binary (`internal/ergoconf/ircd.yaml` in the source):
plaintext on `127.0.0.1:6667`, no TLS, accounts for the agents and none required
for the human (the chair of a table or a build registers a nick, see "Join as
the chair, registered" in `docs/table.md`), history in memory. It is what
`--stream irc://…` is developed and tested against, and it is enough for two
agents and a human in one channel, or for a table: one server per machine, each
table on a channel of its own.

## Install

There is no brew formula. Take the release archive for your platform from
<https://github.com/ergochat/ergo/releases>, at v2.19.1, the version this
configuration was written for. The archives are named
`ergo-2.19.1-<os>-<arch>.tar.gz`, for example `linux-x86_64`, `linux-arm64`,
`macos-arm64` and `macos-x86_64`, and the release lists its checksums. Unpack
the one for your machine and put the `ergo` binary in a directory on your `PATH`.
On macOS arm64, with `~/.local/bin` on the `PATH`:

```
tar xzf ergo-2.19.1-macos-arm64.tar.gz
mkdir -p ~/.local/bin
install -m 0755 ergo-2.19.1-macos-arm64/ergo ~/.local/bin/ergo
ergo --version        # ergo-2.19.1
```

The test suite finds the binary through `$PARLEY_ERGO`, then through `ergo` on
`PATH`, and skips the live tests when it finds neither.

## Run

```
parley server
```

in a terminal of its own; Ctrl-C stops it, and parley exits with ergo's status.
Closing the terminal stops it too: parley passes the hangup on to ergo as a
terminate, because ergo would take a hangup for a rehash and run on.
It writes the configuration it runs into the datastore directory, as a file of
its own (`ircd-<random>.yaml`) that it removes when ergo exits, on every start,
with the listener from `--listen` and the account database at `ergo.db` in the
same directory, by absolute path. Run from anywhere, it:

- listens on `127.0.0.1:6667`, or the loopback IP and port of `--listen` (a host
  name, even `localhost`, is refused, since it could resolve elsewhere); an
  address that is not on this machine is refused with exit 2, since the scheme
  speaks in the clear;
- keeps ergo's files in `~/.local/share/parley-ergo`, or
  `$XDG_DATA_HOME/parley-ergo` when that is set (but `~/.local/share/parley-ergo`
  while it exists and the XDG one does not), or the directory of
  `--datastore <dir>`. A missing directory is made 0700; one that exists is left
  as it is, and the accounts in it carry over, so a datastore you made by hand
  and ran `ergo` from keeps working;
- finds `ergo` on `PATH`, or at `--ergo-bin`; a missing one exits 2 with a
  pointer to this file.

ergo writes `ergo.db` (the accounts) and `ircd.lock` into the datastore
directory, and creates the database on first run. It holds an exclusive lock on
`ircd.lock` for as long as it runs, so a second `parley server` on the same
datastore is refused, on whatever port, and a second server on a taken port is
refused by the operating system; either exits 1 with ergo's own words, then one
line from parley naming the datastore or the address and `pgrep -l ergo` to see
what is running. The lock goes with the process: the file stays after ergo stops
or is killed, and the next start takes the lock again. Channel history is in memory
only: stopping the server forgets every message, which is what a channel between
two agent sessions wants. Between restarts it is the accounts that persist, and
with them each participant's always-on presence: a participant stays in its
channels while nothing of it is connected, so the room and its history outlive any
one `parley` process.

`parley doctor` says whether the server is answering with every capability
parley's IRC client requires (and `PLAIN` among `sasl`'s mechanisms, if it lists
any), and whether the ergo installed is the version this configuration was
written for.

Then, in two other terminals:

```
parley send --stream irc://127.0.0.1:6667/parley --nick alpha -- "hello from alpha"
parley wait --stream irc://127.0.0.1:6667/parley --nick beta --timeout 5m
```

## Accounts

parley registers one account per participant, the first time that participant
connects to this server, and authenticates every connection with SASL PLAIN. It
is a loopback-only scheme for that reason: the password is in the clear on the
socket, and parley refuses any host that is not `127.0.0.0/8`, `::1` or
`localhost`.
This is not optional and there is nothing to set up: the password is 32 random
bytes parley generates, stores 0600 at
`$XDG_STATE_HOME/parley/<nick>/irc/<host>_<port>/password` (or under
`--state-dir`), and reads back on every later connection. Nobody types it and
nobody needs to see it.

The account is what makes the reply path work. `parley wait`, `parley channel`
and `parley codex` hold a nick for as long as they run, and the model's reply
goes out through a separate `parley send` process as the same participant —
without accounts the server would refuse the second connection with 433. With
them, both are sessions of one client.

To see the accounts on a running server, from a client: `/msg NickServ INFO
<nick>`. To remove one — the only fix if the state directory holding its
password is lost, which parley will tell you about by name — either
`/msg NickServ UNREGISTER <nick>` from a client logged into that account, or
stop the server and delete `ergo.db` in its datastore directory, which forgets
every account at once.

## Join from a client

Any IRC client works; nothing parley does needs client support, and the human
needs no account: connect to `127.0.0.1` port `6667`, plaintext, no password,
and `/join #parley`. The one rule is that a nick an agent has registered belongs
to that agent — asking for it gets the usual 433.

- [Halloy](https://halloy.chat) — a modern desktop client, one server block in
  its config (`server = "127.0.0.1"`, `port = 6667`, `use_tls = false`).
- [WeeChat](https://weechat.org) —
  `/server add parley 127.0.0.1/6667 -notls`, `/connect parley`,
  `/join #parley`.

A message parley sends is one message with one id, whatever its shape. A client
that speaks `draft/multiline` shows it as one; every other client shows a long
or multi-line message as several ordinary lines, which is the same thing a
human would have typed. Anything typed in the channel is delivered to the
agents with the server's own id and timestamp, exactly like a message parley
sent.
