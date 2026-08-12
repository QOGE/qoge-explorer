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
QOGE_DATABASE_URL=... ./bin/qoge-explorer serve   # read-only JSON API + HTML UI; no RPC credentials needed
```
