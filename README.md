# fs

Task coordination for isolated agents, over shared memstore.

One agent session works on one task. `fs` records who is on what, keeps a session
bound to a single task, and refuses to mark work complete unless an independent
reviewer ran the declared checks itself.

## What it is not

Not a coordinator daemon, not a scheduler, not a UI, and not a store. `fs` is a
client of [memstore](https://github.com/IanLuo/memstore): every record lives in
`scope = "fs:<project>"` in your memstore file. It owns no database, no cache and
no event ledger.

## Verbs

```text
fs add    --project P --goal G --check C [--check C]... [--path DIR] [--review-of TID]
fs claim  --project P --session S [--task TID]
fs report --project P --session S --task TID --state S [--role dev|review|runtime]
          [--command C --exit N] [--digest D] [--note TEXT]
fs done   --project P --session S --task TID [--note TEXT]
fs dispatch --project P --task TID [--kind pi|claude|codex] [--dir DIR]
            [--wait-seconds N] [--no-wait]
fs path   --project P --path DIR
fs reap   [--project P]
fs status [--project P] [--session S] [--format json|table]
```

`--path` is resolved when the task is written, so `--path .` records the directory you ran
it from, not a dot that means whatever directory the reader stands in.

`fs path` records where a project's work runs from now on, as a new row: a repository
that moved is one command, and no task row changes. Dispatch resolves its directory in
this order — `--dir`, then the project's newest `fs path` binding, then the task's own
recorded path — and refuses before starting anything when the answer is not an existing
directory, naming the task and the path.

States: `progress`, `dev-done`, `pass`, `fail`, `blocked`, `refused`. `refused` is
written by `fs done` when the gate declines to close; it is not a reviewer verdict.
`closed` is written by `fs done` on success and is never a state `fs report` accepts.

A round has two gated handovers below `claim`. `dev-done` is refused until the review
task exists and carries the tree the developer finished (`--digest <commit>`);
`fs add --review-of` is refused while one open review of that task has no verdict yet.
`fs done` then compares the developer's digest with the one the reviewer's passing run
names, and closes nothing when they differ. It also compares its own working directory
with the development task's recorded path, and closes nothing when they are one directory:
the reviewer signs from its own tree, and the close records the directory it was signed
from. That close is one `fs.event` per closed task, written when the round ends, so the
records can answer how long a round took and how long the review took.

The store is `$MEMSTORE_DB`, else `~/.memstore/memory.db`.

## Workflow

A development task and its review are two tasks, worked by two sessions.

```text
USER                     DEV AGENT (session s-dev)            REVIEW AGENT (session s-rev)
────                     ─────────────────────────             ────────────────────────────
fs add  ──► t-dev
         claim s-dev ───► holds t-dev, round 1
                          report progress
                          fs add --review-of t-dev ──────────► t-rev
                          report dev-done --digest <commit> ──► (not complete)
                                                                claim s-rev
                                                                runs each declared check
                                                                report pass+exit+--digest ──► fs done
                                                                                              │
                                                                t-rev closed, then t-dev ─────┘

anyone: fs status  (read-only; a session sees only its binding, the user sees every project)
```

```bash
fs add    --project demo --goal "ship X" --check "go test ./..."   # → t-dev
fs claim  --project demo --session s-dev --task t-dev              # binds + holds
fs report --project demo --session s-dev --task t-dev --state progress --note "half done"
fs add    --project demo --goal "review t-dev" --review-of t-dev   # → t-rev, checks inherited
fs report --project demo --session s-dev --task t-dev --state dev-done \
          --digest "$(git rev-parse HEAD)" --note "parser wired"     # refused without --digest
fs claim  --project demo --session s-rev --task t-rev
fs report --project demo --session s-rev --task t-rev --state pass --role review \
          --command "go test ./..." --exit 0 --digest "$(git rev-parse HEAD)"
fs done   --project demo --session s-rev --task t-rev              # closes t-rev, then t-dev
fs status --project demo            # user view;  --session s-dev gives the agent's own view
```

`fs dispatch` starts the agent for you; `fs reap` closes the ones whose round is
over. Both are optional — an agent session started by a harness works the same way.

```bash
fs dispatch --project demo --task t-dev      # fs mints a session id, starts one agent,
                                             # records the launch, returns the pane
fs reap --project demo                       # closes agents fs started whose round ended
```

What each verb writes:

| verb | rows written | notes |
|---|---|---|
| `add` | `fs.task` (`task:<tid>`) | no goal, no scope or no check is refused before any write; a review is refused while one open review of the same task has no verdict |
| `claim` | `fs.bind` (`session:<sid>`), then `fs.claim` (`claim:<tid>:<round>`) | the bind is once per session and never expires |
| `report` | `fs.event` (`ev:<tid>:<sid>:<n>`) | a check needs `--command` **and** `--exit`; a `dev-done` needs a `--digest` and an existing review task; this write is also the keepalive |
| `done` | one `fs.event` per closed task, then closes the review row, then the development row | each close carries `closed`, the tree it blessed and the directory the reviewer signed from, so a round's duration is computable from rows; a refusal records one `fs.event` saying why; refuses when the reviewer's digest differs from the developer's, and when the signature is made in the developer's checkout |
| `dispatch` | `fs.launch` (`launch:<tid>:<sid>`) | refuses a closed task, a live holder, a duplicate start, a directory that is not there, a review started in the developer's checkout and an agent that never takes the prompt; a launcher failure is a refusal, never a retry |
| `path` | `fs.path` (`path:<project>:<n>`) | appended, newest wins; no task row is touched, so a moved repository is one command |
| `reap` | nothing | closes only an agent it has a launch row for, and never one still working |
| `status` | nothing | derived from rows on every call |

### Dispatch and reap

`fs dispatch` is a convenience, not a scheduler: it starts **one** agent for **one**
task, waits for that agent to take it, and returns. There is no retry, no
restart-on-crash and no fallback launcher.

```text
read only ──► refuse if: task closed · live holder · its agent still running · no directory
     │                                                    (--dir, else fs path, else
     │                                                     the task's own path — and it
     │                                                     must be an existing directory)
     │
     ▼
start one agent (one launcher call) ──► failure = refusal, nothing else happens
     │
     ▼
wait up to --wait-seconds (60 by default) for that session to claim the task
     │                                        or write any evidence
     ├── signal seen ──► write launch:<tid>:<sid> with outcome started ──► ok
     │
     └── no signal ────► write launch:<tid>:<sid> with outcome unconfirmed
                          ──► exit non-zero, name the pane and the session, leave the pane open
```

A pane that exists is not proof the agent read its prompt. So the launch row says
`unconfirmed` when nothing came back, and a re-dispatch of that task is allowed —
the no-retry rule is about a broken launcher, not an agent that never woke. Use
`--no-wait` for the old fire-and-forget behaviour; the output then marks the launch
`unconfirmed`.

The agent gets a prompt that names the store (`MEMSTORE_DB`), the session id to
claim as, and every declared check — because a new pane inherits the launcher's
environment, not the caller's. A reviewer is told to run the checks and report the
real exit codes; a developer is told to create its review task before the `dev-done`,
carry the commit in `--digest`, and dispatch the review afterwards.

`fs path --project P --path DIR` is how a project moves. The binding is appended to the
project's own scope and the newest one wins, so dispatch follows it without a task row
being rewritten, and `fs status` reports the directory each project would dispatch into.

"Still running" is asked of the launcher, never inferred from a clock: a lapsed lease
means the session stopped writing, not that its process ended.

`fs reap` closes an agent fs recorded when its round is over: the task is closed, its
newest word is a `fail`/`blocked` verdict, or its lease lapsed and it holds nothing. A
`refused` record is a gate refusal, not a verdict, so it never ends a round. A
live claim outranks an older verdict — a reviewer that re-claimed for the next round is
working, not finished. A working agent is never closed, and neither is a pane fs has no
launch row for: reap closes what fs opened.

`fs status --format table` carries a `PROCESS` column for the same reason: `working`,
`idle`, `gone`, or `-` when fs has no launch record and therefore no opinion. A lapsed
lease beside a `working` process is a slow agent; a `gone` process beside a live claim is
a dead one, and only the launcher can tell them apart.

### Claim, the only intricate step

```text
0. read: does the task exist, is it closed?          (a read decides nothing)
1. Put session:<sid>  ── duplicate? ─► REFUSE: session spent, nothing else written
2. read rows; current round = the highest seen
3. live by another session? ─► REFUSE, naming holder and expiry
4. live by me? ─► success, it is already mine
5. Put claim:<tid>:<round+1> ── duplicate? ─► another racer won; retry from 2, 3 attempts
                             └─ inserted ─► I hold it
```

The winner is decided by memstore refusing a duplicate key, never by a comparison fs
makes. The order is bind, then claim: a failure after step 1 burns the session and holds
nothing, which is safe — the fix is a new session, not a retry.

A review task is dispatched into a checkout of its own. `--dir` is compared with the
development task's recorded path, both resolved through symlinks, and one directory is
refused before an agent exists — naming both directories and `git worktree add <dir>
<commit>`. The gate asks the same question again at the signature, so asking here spends a
dispatch instead of a finished review. A development task's own dispatch is unchanged, a
development row with no recorded path refuses nothing, and the developer's prompt says to
create the worktree at the commit under review.

### The completion gate

```text
done on a development task                  → refused: a worker cannot complete its own work
same session as the developer               → refused: a reviewer must be a different session
the development task was never claimed      → refused: there is no developer to be independent from
reviewed task already closed                → refused: a completed task is never reopened
signed from the developer's checkout        → refused, naming both directories
no dev-done digest on the development task  → refused: the verdict must name the tree it judged
no review-role run of a declared check      → refused, and a `refused` event records why
newest run of a check exits non-zero        → refused, naming the check and its exit
the newest passing run names another tree   → refused, naming both digests
all checks pass, run by this reviewer       → records one close per closed task, then closes the review and the development task
```

The gate matches the newest `role=review` event from this session with the exact declared
command, so a reviewer that fixed and re-ran reports the rerun.

A verdict names the tree it judged, and the close names the directory it was signed from.
`fs done` compares its own working directory with the development task's recorded `--path`,
both resolved through symlinks first, and refuses when they are one directory: a review is
a judgement made in its own tree, and a signature from the developer's checkout is the same
context twice. A development task with no recorded path (a row written before `--path`)
has nothing to compare, so nothing is refused. The check makes the claim **checkable, not
true**: a reviewer can `cd` elsewhere before signing, and fs never runs anything, so what
the records hold is what each party declared.

`round_state` in the JSON is where the round stands between its steps, derived on every
read and never stored: `awaiting-review` for development work reported done with no review
task (a row written before the refusal — append-only records are not rewritten), and
`review-open` for a review task with no verdict yet. It is a new field rather than a new
`state` value, so a task a dispatcher sees as `open` stays actable.

A refusal is recorded as `state: refused`, not `fail`: the gate declined to close, which
is not a reviewer's verdict that a check failed. The review stays open, reap does not treat
the refusal as a verdict, and a later passing run plus `fs done` still closes it as pass.

A close is recorded as `state: closed`, one row on each task the gate closed, written
before the rows are closed so a failure between the two is recoverable. It carries the
reviewer's session, the tree it blessed and the directory it signed from, and it is not an
agent state: the derivation that feeds `agent` and `runtime` skips it, so those cells keep
their vocabulary while the close stays in `evidence`. `closed` is not a state `fs report`
accepts — a caller that could report a close could close a round without passing the gate.

fs has no repair or migration path and does not rewrite shared rows. A `fail` row written
before the `refused` state existed, whose note begins `completion refused`, keeps that
shape; read it as a refusal, not a verdict.

### Liveness

- A claim is live while some row by its session is newer than `now − 120s`. There is no
  renew verb and no expired flag: `fs report` is the heartbeat.
- A lapsed lease reads as `unknown` for the agent and `open` for the task, which frees it
  for a new session. It never means done, and the old session may no longer write.
- One session holds one task for life. A session bound to `t-dev` is refused on `t-rev`
  before any write, even after `t-dev` completes: more work needs a new session id.

### Reading it as a human

The default answer of `status` is the JSON envelope every verb prints. `--format table`
lays the same report out to read, from the same single read — no second query, no stored
view:

```text
$ fs status --format table
2 projects, 3 tasks · 2 open · 1 claimed

demo  /Users/me/src/demo
  TASK                KIND    STATE    HOLDER            ROUND  LEASE     AGENT      PROCESS  WAITING          GOAL
  t-0a7b9b637a41a3c6  dev     claimed  s-1               1      1m44s     progress   working  -                add lease-expiry tests
  t-f64fa63ac1ddac3e  dev     open     -                 -      -         dev-done~  -        awaiting-review  ship the parser

3 projects with no tasks: 3d-house, ann, cap
```

- The line under each project names the directory its next dispatch starts in: its newest
  `fs path` binding. A project with no binding shows its name alone, and each task's own
  `project_path` decides. The JSON carries it as `dir` on the project.
- `HOLDER`/`ROUND` are who holds the task and which claim attempt that is; `-` means nobody
  holds it.
- `LEASE` is what the holder has left before it must write again; `lapsed` frees the task for
  a new session and never means done.
- `AGENT` is the holder's last reported state, `unknown` when that report is older than the
  lease. Absence is not health. A stale report is shown with a `~`, e.g. `dev-done~`: the
  verdict is unknown, and the state shown is what the agent last said, so a finished agent
  and a vanished one are not conflated. The JSON carries both as `agent.state` and
  `agent.reported`, with `agent.age_seconds`.
- `PROCESS` is what the launcher says about the agent fs started for the task: `working`,
  `idle` or `gone`. `-` means fs has no launch record — a pane somebody else opened, which fs
  does not judge and will not close. A launcher fs cannot reach leaves the cell `-` and adds
  one warning line under the table; the rows still answer. Read it beside `LEASE`: a lapsed
  lease with a `working` process is a silent agent, not a dead one. The JSON carries it as
  `process.status` with `process.pane`, `process.name` and `process.launched`.
- `WAITING` is `round_state`: `awaiting-review` means the development work is reported done
  and no review task exists, so the round is stalled until someone adds one; `review-open`
  means a review task exists with no verdict yet. `-` means the round is not between those
  two steps. It is derived from rows on every read, never stored, and it is a separate field
  from `STATE` so a task a dispatcher sees as `open` stays actable.
- Projects with no tasks are folded into one line: a scope keeps its name for as long as it
  holds any `fs:` row, so empty projects say nothing about current work. The JSON view still
  carries every project, including `"tasks": []`.

### Picking up a task whose session died

The agent's context is gone and fs keeps no transcript, so a new session resumes from records.
`evidence` in the JSON is every report the task carries, newest first, bounded at 20: `role`,
`state`, `session`, `age_seconds`, `note`, `digest`, and for a check run the exact `command`
and `exit`. A development report is what says where the work stopped.

```bash
fs status --project P --format table            # LEASE lapsed, AGENT progress~ / dev-done~
fs claim  --project P --session s-take --task T # round+1; reuse this id if you retry
fs status --project P | jq -r '.data.projects[].tasks[] | select(.task=="T") | .evidence[0]'
# then read the work: git status, git log, git diff — and run the declared checks
fs report --project P --session s-take --task T --state progress --note "…" --digest "…"
```

The last line is not bookkeeping: it is the keepalive, and it is the only thing the next
session will have if this one dies too.

## Build and install

```bash
nix build .#default            # ./result/bin/fs
nix profile install .#default  # puts fs on PATH, like memstore
nix develop                    # go + gopls
```

## Test

```bash
nix develop -c sh -c 'go build ./... && go vet ./... && gofmt -l . | grep -v ^vendor/'
nix develop -c sh -c 'go test ./... -count=1'
nix develop -c go test ./internal/coord -count=1 -race
```

The tests defend the two invariants rather than the shapes: a barrier test (goroutines
and separate processes on one task, exactly one winner) and a binding test (a session
bound to one task is refused on every other, with the rows unchanged).

## Contracts

- `.agents/specs/fs-rebuild.md` — the requirements contract.
- `.agents/specs/fs-rebuild-design.md` — records, the claim protocol, layers.

Read them before changing a key, the claim order, or the completion gate: the safety
argument is the order of two writes, not the shape of the code.
