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
