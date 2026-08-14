# qoge-explorer

Permanent QOGE block explorer (Go): indexes the confirmed chain, mempool,
and consensus deployment state into PostgreSQL, and serves a read-only
JSON API plus a server-rendered HTML explorer UI over it. See
`docs/ARCHITECTURE.md` for the full design.

## Build

```
go build -o bin/qoge-explorer ./cmd/qoge-explorer
```

## Configure

Environment variables (see `qoge-explorer --help`):

```
QOGE_RPC_HOST=127.0.0.1
QOGE_RPC_PORT=8332
QOGE_RPC_USER=...
QOGE_RPC_PASSWORD=...
```

## Run

```
QOGE_RPC_USER=... QOGE_RPC_PASSWORD=... ./bin/qoge-explorer check-rpc
QOGE_RPC_USER=... QOGE_RPC_PASSWORD=... QOGE_NETWORK=main QOGE_DATABASE_URL=... ./bin/qoge-explorer index
QOGE_NETWORK=main QOGE_DATABASE_URL=... ./bin/qoge-explorer backfill-accounting  # one-time, only for a database indexed before block_accounting existed
QOGE_NETWORK=main QOGE_DATABASE_URL=... ./bin/qoge-explorer backfill-supply-rollup  # one-time, only for a database indexed before block_supply_rollup existed; run backfill-accounting first if needed
QOGE_DATABASE_URL=... ./bin/qoge-explorer serve   # read-only JSON API + HTML UI; no RPC credentials needed
```

`QOGE_NETWORK` must match the network the database was actually indexed
against (`main`/`test`/`signet`/`regtest`) — it selects the correct
consensus subsidy schedule and is never inferred or defaulted.
`backfill-accounting` has no Core RPC connection to cross-check this
itself, so before writing anything it verifies the database's own
canonical genesis block (height 0) against QOGE Core's known genesis hash
for the configured network; a mismatched or missing genesis exits nonzero
with zero rows written (`docs/ARCHITECTURE.md` §26 "Backfill network
identity verification"). signet is rejected as unsupported (QOGE Core
stable has no stable asserted signet genesis).

`backfill-supply-rollup` reconstructs `block_supply_rollup` (Phase 2H.2a's
immutable, reorg-safe cumulative monetary rollup — `docs/ARCHITECTURE.md`
§27) for the current canonical chain from already-indexed PostgreSQL data
alone. It uses the same network-identity preflight as
`backfill-accounting`, requires `block_accounting` to already be complete
for every canonical block, and independently cross-checks its own output
against a direct full scan of `utxo_state` and `block_accounting` before
publishing anything — any disagreement aborts the entire run with zero
rows written. `index` itself will refuse to start against a database that
has an indexed tip but no rollup coverage for it, with a message pointing
at this command. Both backfill commands require the `index` process to be
stopped first.
