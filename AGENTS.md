# AGENTS.md — fs

## Intent

- `fs` coordinates isolated agent tasks: who is on what, one task per agent session, and
  completion only after an independent reviewer ran the declared checks.
- It is a **client of shared memstore**. Every record lives in `scope = "fs:<project>"` in
  the user's memstore file. It owns no database, cache, event ledger or registry.
- The reader of this repo is an agent that will change the claim path. The claim path is a
  safety argument, not plumbing — the docs below exist so it is not "simplified" away.

## How to run / build / test

```bash
nix build .#default                                    # ./result/bin/fs
nix profile install .#default                          # fs on PATH, like memstore
nix develop -c sh -c 'go build ./... && go vet ./... && gofmt -l . | grep -v ^vendor/'
nix develop -c sh -c 'go test ./... -count=1'
nix develop -c go test ./internal/coord -count=1 -race   # the barrier + binding tests
```

There is no network in this environment: `vendor/` is committed and `vendorHash = null`,
so the build is hermetic. If `go.mod` changes, regenerate the tree with
`GOPROXY=off GOFLAGS=-mod=mod go mod vendor` and commit it — nix copies only tracked files.

## Hot invariants

- **Two invariants, two rows.** `fs.bind` (`session:<sid>`, once per session, never expires)
  refuses a session's second task; `fs.claim` (`claim:<tid>:<round>`) refuses a second holder.
  One row each because `Put` enforces exactly one unique key. Collapsing them into one row
  silently drops one invariant.
- **Order is bind, then claim.** A failure after the bind burns the session and holds
  nothing. That is safe and accepted; the fix is a new session, not a retry.
- **`ErrDuplicateOrigin` is the arbiter.** If the claim path becomes a read-then-write, the
  guarantee is gone. `TestClaimBarrier*` fails if a winner is decided from a read.
- **`internal/memory` is the only importer of memstore.** `internal/coord` must not import
  it. A test enforces the direction.
- **Liveness is derived, never written.** No "expired" flag, no cleanup job, no renew verb:
  any write by the holder is the keepalive, and a lapsed lease reads as `unknown`.
- **Fail closed.** memstore unavailable or ambiguous → non-zero exit, no claim, no
  completion. No fallback store, no direct SQL, no cache.
- **No code path touches `~/.fs/store.db`.** That file belongs to the retired implementation.

## Architecture elevator

```
cmd/              flags → coord → JSON envelope; exit 0/1/2; no policy
internal/coord/   task shape, the claim protocol, the completion gate, status derivation
internal/memory/  the only memstore importer: scope guard, keys, liveness, one clock
internal/logging/ diagnostics to stderr + ~/.fs/logs/fs.log; never gates
```

Dependencies point one way: `cmd → coord → memory → memstore`. Key construction lives in
exactly one package, because the protocol is correct only if every racer builds the same key.

## Deeper docs

- `.agents/specs/fs-rebuild.md` — requirements: acceptance criteria, KPIs, failure signals.
- `.agents/specs/fs-rebuild-design.md` — records, the claim protocol, layers, failure modes.
- `skills/flagship/SKILL.md` in the **skills** repo (`~/Documents/apps/skills`) — how a user
  and an agent drive the CLI. That repo is a consumer; it does not own this code.
