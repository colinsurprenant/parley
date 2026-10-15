# Table <id>: <the question, as a title>

Q:      <the question. Say how big a test a seat may propose (narrow, one
        sitting, judged by a human, or whatever fits), and whether this table
        asks if having several models caused a result: the protocol keeps
        controls and blinding out of proposed tests unless it does.>
SCOPE:  <what an answer may cover, for example one call site, or every site of
        the same shape. Optional: delete the line if Q needs no limit.>
READ:   <what to read first: this sheet, then the evidence and the code below>
SEATS:  <seat> (<prefix>), <seat> (<prefix>), <seat> (<prefix>)
ROLES:  none; every seat speaks for itself.   <or: <seat> skeptic, <seat> advocate>
CAP:    <k> rounds · <m> min
CHAIR:  <the chair's registered nick; optional, the primer already names it>

## The evidence

<The files, notes or results the seats should start from, by path, with what
each is. Mark anything that is a session's inference rather than a
measurement, so the seats can weigh it.>

- <fact the seats should take as given>
- <fact the seats should take as given>

## Already decided

<Decisions the table does not reopen, each with its date and, if it lives in
the ledger, its ULID. Delete the section if there are none.>

- <decision> (<who decided it, when>; <ULID>)

## Ideas already on the log

Start from these. Attack them, extend them into new ideas, or go past them;
restating one adds nothing. Each carries a table id and its **full** ULID,
because `director show` refuses a prefix of one.

- **<idea title>** (c1): <one line on what it is> (<full 26-character ULID>)
- **<idea title>** (c2): <one line on what it is> (<full 26-character ULID>)

Set aside, and open to revival with a reason:
- <an idea the chair has parked, and why>

## The code, if you want to check something

- The target repo is the one in your primer. Read <the files that matter>.
- The protocol for this table is the `parley-table` skill.
