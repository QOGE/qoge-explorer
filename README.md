# qoge-explorer

Permanent QOGE block explorer (Go). Phase 1: environment reconnaissance and
project skeleton only — no indexing yet. See `docs/ARCHITECTURE.md` for the
full design.

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
QOGE_RPC_USER=... QOGE_RPC_PASSWORD=... ./bin/qoge-explorer serve   # loopback health endpoint only
```
