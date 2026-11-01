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
fs status [--project P] [--session S] [--format json|table]
```

States: `progress`, `dev-done`, `pass`, `fail`, `blocked`.

The store is `$MEMSTORE_DB`, else `~/.memstore/memory.db`.

## Workflow

A development task and its review are two tasks, worked by two sessions.

```text
USER                     DEV AGENT (session s-dev)          REVIEW AGENT (session s-rev)
────                     ─────────────────────────           ────────────────────────────
fs add  ──► t-dev
         claim s-dev ───► holds t-dev, round 1
                          report progress ──┐
                          report dev-done ──┴─► (not complete)
                                                    fs add --review-of t-dev ──► t-rev
                                                                                 claim s-rev
                                                                                 runs each declared check
                                                                                 report pass+exit ──► fs done
                                                                                                       │
                                                                                 t-rev closed, then t-dev ┘

anyone: fs status  (read-only; a session sees only its binding, the user sees every project)
```

```bash
fs add    --project demo --goal "ship X" --check "go test ./..."   # → t-dev
fs claim  --project demo --session s-dev --task t-dev              # binds + holds
fs report --project demo --session s-dev --task t-dev --state progress --note "half done"
fs report --project demo --session s-dev --task t-dev --state dev-done
fs add    --project demo --goal "review t-dev" --review-of t-dev   # → t-rev, checks inherited
fs claim  --project demo --session s-rev --task t-rev
fs report --project demo --session s-rev --task t-rev --state pass --role review \
          --command "go test ./..." --exit 0
fs done   --project demo --session s-rev --task t-rev              # closes t-rev, then t-dev
fs status --project demo            # user view;  --session s-dev gives the agent's own view
```

What each verb writes:

| verb | rows written | notes |
|---|---|---|
| `add` | `fs.task` (`task:<tid>`) | no goal, no scope or no check is refused before any write |
| `claim` | `fs.bind` (`session:<sid>`), then `fs.claim` (`claim:<tid>:<round>`) | the bind is once per session and never expires |
| `report` | `fs.event` (`ev:<tid>:<sid>:<n>`) | a check needs `--command` **and** `--exit`; this write is also the keepalive |
| `done` | no new row when it passes: closes the review row, then the development row | a refusal records one `fs.event` saying why |
| `status` | nothing | derived from rows on every call |

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

### The completion gate

```text
done on a development task                  → refused: a worker cannot complete its own work
same session as the developer               → refused: a reviewer must be a different session
the development task was never claimed      → refused: there is no developer to be independent from
reviewed task already closed                → refused: a completed task is never reopened
no review-role run of a declared check      → refused, and a fail event records why
newest run of a check exits non-zero        → refused, naming the check and its exit
all checks pass, run by this reviewer       → closes the review, then the development task
```

The gate matches the newest `role=review` event from this session with the exact declared
command, so a reviewer that fixed and re-ran reports the rerun.

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

demo
  TASK                KIND    STATE    HOLDER            ROUND  LEASE     AGENT      GOAL
  t-0a7b9b637a41a3c6  dev     claimed  s-1               1      1m44s     progress   add lease-expiry tests
  t-f64fa63ac1ddac3e  dev     open     -                 -      -         unknown    a goal long enough to be cut off…

3 projects with no tasks: 3d-house, ann, cap
```

- `HOLDER`/`ROUND` are who holds the task and which claim attempt that is; `-` means nobody
  holds it.
- `LEASE` is what the holder has left before it must write again; `lapsed` frees the task for
  a new session and never means done.
- `AGENT` is the holder's last reported state, `unknown` when that report is older than the
  lease. Absence is not health.
- Projects with no tasks are folded into one line: a scope keeps its name for as long as it
  holds any `fs:` row, so empty projects say nothing about current work. The JSON view still
  carries every project, including `"tasks": []`.

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
