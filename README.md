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
fs status [--project P] [--session S]
```

States: `progress`, `dev-done`, `pass`, `fail`, `blocked`.

The store is `$MEMSTORE_DB`, else `~/.memstore/memory.db`.

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
