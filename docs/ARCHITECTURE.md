# QOGE Go Explorer — Architecture (Phase 1)

Status: reconnaissance + skeleton only. No indexing implemented yet.
Qogecoin Core is the sole authoritative source of chain truth; this document
describes how the explorer observes and represents that truth, never how it
decides it.

## 1. Component overview

Single Go service, no microservices, for the foreseeable future:

```
cmd/qoge-explorer/        entry point, subcommand dispatch (check-rpc, serve, ...)
internal/config/          env-var configuration loading (no credential logging)
internal/logging/         structured slog setup
internal/rpc/             Qogecoin Core JSON-RPC client (generic Call + typed helpers)
internal/chain/           canonical block/tx/output domain model (Core-shape-independent)
internal/script/          script classification (P2PK/P2PKH/.../P2QPK/UNKNOWN)
internal/indexer/         sync loop: fetch from rpc, classify via script, write via store
internal/store/           PostgreSQL persistence, idempotent writes, reorg handling
internal/api/             HTTP/JSON API (not wired to a public port yet)
internal/web/             HTML presentation layer (not built yet)
```

Data flows one direction under normal operation:

```
Qogecoin Core RPC → internal/rpc → internal/indexer → internal/chain (canonical model)
                                          │
                                          ├─→ internal/script (classify each output)
                                          └─→ internal/store (PostgreSQL, one TX per block)
                                                     │
                                    internal/api / internal/web ← read-only queries
```

`internal/chain` holds Core-shape-independent Go structs (Block, Transaction,
Output, Input) so that `internal/store`, `internal/api`, and `internal/web`
never depend on Core's JSON field names directly — only `internal/rpc` and
`internal/indexer` know about Core's wire format.

## 2. What was intentionally NOT copied from eIquidus

Findings are from the cross-validation performed against the live eIquidus
reference instance (see chat history; not duplicated in this repo). Each item
below is a deliberate divergence, not an oversight:

1. **No `$inc`-style additive balance counters.** eIquidus accumulates
   `Address.received/sent/balance` with MongoDB `$inc`. Confirmed in live
   data: after an ungraceful crash, resuming sync replayed an
   already-committed block and every address touched by it ended up with
   exactly double the correct `sent`/`received` values (verified against
   Core RPC for four addresses; root cause traced to the exact crash/resume
   boundary block). The Go explorer computes address balances by
   **re-deriving them from canonical indexed outputs** (`SUM(value) WHERE
   spent = false`), so replaying a block produces the same answer every time.
2. **No coinbase-output-value-as-supply.** eIquidus's `COINBASE` supply mode
   sums everything ever paid to coinbase outputs, which is subsidy **+
   fees**. Fees are pre-existing QOGE moving to a miner, not new issuance;
   this methodology permanently overstates supply by the cumulative fee
   total. See §6.
3. **No silent transaction-shape transformations.** eIquidus aggregates
   same-block/same-address `vin` entries into one summed record, and drops
   zero-value `nulldata` outputs (segwit witness commitments) from what it
   stores. Verified harmless in the specific case checked, but undocumented
   and irreversible. The Go explorer stores one row per raw input and one row
   per raw output, unmodified — see §3.
4. **A real block index.** eIquidus has no `Block` model at all; block hash,
   prevhash, difficulty, and size are fetched live from Core on every page
   view. The Go explorer persists a minimal block index (§3), so browsing
   doesn't have a hard per-request dependency on Core RPC latency, and reorg
   detection has something durable to compare against.
5. **No ad hoc `tx_type` flag.** eIquidus's `tx_type` is only ever
   `p2pk`/`zerocoin`/`zksnarks`/`null` — most transactions are silently
   unclassified. The Go explorer classifies every output explicitly (§7).

## 3. PostgreSQL schema

Integer satoshis (`BIGINT`) everywhere; no floating point for QOGE values
anywhere in the schema or the Go model.

```sql
-- ── sync checkpoint ─────────────────────────────────────────────────────
CREATE TABLE sync_state (
    name                TEXT PRIMARY KEY,        -- 'main' for phase 1 (room for future named indexers)
    indexed_height      BIGINT NOT NULL,
    indexed_block_hash  TEXT NOT NULL,
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ── blocks ──────────────────────────────────────────────────────────────
CREATE TABLE blocks (
    hash                TEXT PRIMARY KEY,
    height              BIGINT NOT NULL,
    prev_hash           TEXT,                    -- NULL only for genesis
    merkle_root         TEXT NOT NULL,
    "time"              BIGINT NOT NULL,          -- block header timestamp (unix seconds)
    bits                TEXT NOT NULL,
    difficulty          DOUBLE PRECISION NOT NULL,-- display only; never used for consensus decisions
    nonce               BIGINT NOT NULL,
    size                INT NOT NULL,
    weight              INT NOT NULL,
    tx_count            INT NOT NULL,
    orphaned            BOOLEAN NOT NULL DEFAULT FALSE,
    orphaned_at         TIMESTAMPTZ,
    indexed_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Exactly one canonical block per height; orphaned blocks are kept (audit trail),
-- not deleted, so the unique constraint on height only applies to the canonical row.
CREATE UNIQUE INDEX blocks_height_canonical_uidx ON blocks (height) WHERE NOT orphaned;
CREATE INDEX blocks_prev_hash_idx ON blocks (prev_hash);

-- ── transactions ────────────────────────────────────────────────────────
CREATE TABLE transactions (
    txid                TEXT PRIMARY KEY,
    block_hash          TEXT NOT NULL REFERENCES blocks(hash),
    block_height        BIGINT NOT NULL,          -- denormalized for range queries
    tx_index            INT NOT NULL,             -- position within the block
    version             INT NOT NULL,
    locktime            BIGINT NOT NULL,
    size                INT NOT NULL,
    vsize               INT NOT NULL,
    weight              INT NOT NULL,
    is_coinbase         BOOLEAN NOT NULL,
    fee_satoshis        BIGINT,                   -- NULL for coinbase; computed once all inputs resolved
    indexed_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX transactions_block_height_idx ON transactions (block_height);

-- ── transaction inputs (raw, one row per vin — no aggregation) ─────────
CREATE TABLE transaction_inputs (
    txid                TEXT NOT NULL REFERENCES transactions(txid),
    vin_index           INT NOT NULL,
    prev_txid           TEXT,                     -- NULL for coinbase
    prev_vout_index     INT,                       -- NULL for coinbase
    coinbase_hex        TEXT,                      -- set only for coinbase input
    script_sig_hex      TEXT,
    sequence            BIGINT NOT NULL,
    has_witness         BOOLEAN NOT NULL DEFAULT FALSE,
    PRIMARY KEY (txid, vin_index)
);
CREATE INDEX transaction_inputs_prevout_idx ON transaction_inputs (prev_txid, prev_vout_index);

-- Witness stack data lives in its own table so ordinary listing/detail
-- queries never have to pull ~17KB P2QPK signatures along for the ride.
-- See §8.
CREATE TABLE transaction_input_witness (
    txid                TEXT NOT NULL,
    vin_index           INT NOT NULL,
    item_index          INT NOT NULL,              -- position in the witness stack, 0 = bottom
    data                BYTEA NOT NULL,
    PRIMARY KEY (txid, vin_index, item_index),
    FOREIGN KEY (txid, vin_index) REFERENCES transaction_inputs (txid, vin_index)
);

-- ── transaction outputs (the canonical UTXO ledger) ─────────────────────
CREATE TABLE transaction_outputs (
    txid                TEXT NOT NULL REFERENCES transactions(txid),
    vout_index          INT NOT NULL,
    value_satoshis      BIGINT NOT NULL,
    script_pubkey_hex   TEXT NOT NULL,
    script_type         TEXT NOT NULL,             -- see §7; CHECK constraint, not a native ENUM (see note)
    witness_version     INT,                        -- NULL for non-witness scripts
    witness_program_hex TEXT,                       -- NULL for non-witness scripts
    is_p2qpk            BOOLEAN NOT NULL DEFAULT FALSE, -- witness_version=2 AND len(program)=32; see §9
    creation_block_height BIGINT NOT NULL,
    spent               BOOLEAN NOT NULL DEFAULT FALSE,
    spending_txid       TEXT,
    spending_vin_index  INT,
    spending_block_height BIGINT,
    PRIMARY KEY (txid, vout_index),
    CHECK (script_type IN (
        'p2pk','p2pkh','p2sh','p2wpkh','p2wsh','p2tr','p2qpk',
        'nulldata','multisig','unknown_witness','unknown'
    ))
);
CREATE INDEX transaction_outputs_unspent_idx ON transaction_outputs (txid, vout_index) WHERE NOT spent;
CREATE INDEX transaction_outputs_spending_txid_idx ON transaction_outputs (spending_txid);

-- Resolved DESTINATION address(es) per output — the addresses whose
-- balance/received/sent accounting (the `addresses` cache below) is
-- actually derived from this output. For every currently-supported script
-- type this is exactly zero or one row: single-key types (P2PK, P2PKH,
-- P2WPKH, P2QPK, etc.) resolve to one destination address; unparseable/
-- unknown scripts resolve to zero. Bare MULTISIG is deliberately NOT
-- represented here — see output_participants below and §13.A — because a
-- multisig output has no single owner to credit. This table, not
-- output_participants, is the one balance aggregation joins against.
CREATE TABLE output_addresses (
    txid                TEXT NOT NULL,
    vout_index          INT NOT NULL,
    address             TEXT NOT NULL,
    PRIMARY KEY (txid, vout_index, address),
    FOREIGN KEY (txid, vout_index) REFERENCES transaction_outputs (txid, vout_index)
);
CREATE INDEX output_addresses_address_idx ON output_addresses (address);

-- Pubkey-derived PARTICIPANT identities for bare MULTISIG outputs —
-- searchable/displayable ("this address co-signs this UTXO"), but
-- deliberately never joined into balance/received/sent aggregation. An
-- m-of-n multisig output's value is jointly controlled by all n named
-- participants, not individually owned by each of them; if this table were
-- (mistakenly) joined the same way output_addresses is, a single multisig
-- UTXO's value would be credited in full to every participant's balance —
-- summing all address balances would then overcount total supply by
-- (participants - 1) times the output value for every multisig UTXO in
-- existence. See §13.A ("role model": output_addresses rows carry an
-- implicit role=destination; this table's rows are role=participant, and
-- only role=destination ever contributes to balance math).
CREATE TABLE output_participants (
    txid                TEXT NOT NULL,
    vout_index          INT NOT NULL,
    address             TEXT NOT NULL, -- derived from the participant pubkey, display/search only
    pubkey_hex          TEXT NOT NULL,
    PRIMARY KEY (txid, vout_index, address),
    FOREIGN KEY (txid, vout_index) REFERENCES transaction_outputs (txid, vout_index)
);
CREATE INDEX output_participants_address_idx ON output_participants (address);

-- ── addresses (derived cache, not a source of truth) ────────────────────
-- Rebuilt by re-aggregating output_addresses (destination rows ONLY —
-- never output_participants) + transaction_outputs for the touched
-- addresses inside the SAME block transaction. Every write is a SET of a
-- freshly computed absolute value (see §4) — never an increment.
CREATE TABLE addresses (
    address             TEXT PRIMARY KEY,
    total_received_satoshis BIGINT NOT NULL,
    total_sent_satoshis      BIGINT NOT NULL,
    balance_satoshis          BIGINT NOT NULL,
    tx_count                  INT NOT NULL,
    first_seen_height         BIGINT NOT NULL,
    last_seen_height           BIGINT NOT NULL,
    updated_at                TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ── deployment status cache (display only; Core remains authoritative) ──
CREATE TABLE chain_deployments (
    name                TEXT PRIMARY KEY,          -- e.g. 'p2qpk'
    status              TEXT NOT NULL,              -- defined/started/locked_in/active/failed
    since_height        BIGINT,
    raw_json            JSONB NOT NULL,
    checked_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

**Note on `script_type`:** a `TEXT` column with a `CHECK` constraint was
chosen over a native Postgres `ENUM` deliberately. Adding a value to a native
enum type is a schema migration with transactional caveats (`ALTER TYPE ...
ADD VALUE` cannot run inside the same transaction as its first use, in some
Postgres versions). New witness/script types are expected — most notably
P2QPK activating for real — so the classification list needs to be cheap to
extend. A `CHECK` constraint is a single, ordinary migration.

## 4. Idempotency model

**Core guarantee:** re-indexing the same block, any number of times, in any
order relative to a crash, produces byte-identical database state to
indexing it exactly once.

Mechanisms:

1. **Natural-key uniqueness everywhere.** `blocks.hash`, `transactions.txid`,
   `(txid, vin_index)`, `(txid, vout_index)` are all real primary/unique
   keys. All inserts during indexing use `INSERT ... ON CONFLICT DO
   NOTHING` (or `DO UPDATE` only where the update is itself idempotent,
   e.g. re-setting `spent = true` on an output that's already spent by the
   same `spending_txid` is a no-op in effect). Replaying a block's inserts
   is always safe.
2. **One SQL transaction per block.** All work for block N — inserting the
   block row, its transactions, inputs, outputs, witness data, marking
   spent outputs, recomputing the `addresses` rows touched by this block,
   and finally updating `sync_state` — happens inside a single
   `BEGIN...COMMIT`. Postgres transactions are all-or-nothing and durable
   only on `COMMIT`; there is no way for "half of block N" to survive a
   crash.
3. **Checkpoint update is the last statement inside that same transaction,**
   not a separate one issued afterward. This directly closes the exact gap
   that corrupted eIquidus: there, the checkpoint (`coinstats.last`) and the
   balance deltas were written as unrelated MongoDB operations with no
   shared transaction, so a crash between them left a checkpoint that didn't
   match the data it was supposed to describe, and the resume logic
   (correctly, given its own assumptions) replayed the block — which was
   only safe if the writes were idempotent, and they weren't. Making
   checkpoint-and-data one atomic unit removes the need to reason about that
   gap at all.
4. **Balances are `SET`, not incremented** (see §3's `addresses` table).
   Because the value written is always "the fresh aggregate over canonical
   outputs right now," writing it twice is identical to writing it once.

### Crash scenarios

| # | When the process dies | What's durable afterward | What happens on restart |
|---|---|---|---|
| A | Before the block's DB transaction begins | Nothing for block N; `sync_state` still at N-1 | Indexer re-fetches block N from Core and starts fresh — identical to first attempt |
| B | Mid-transaction (any point before `COMMIT`) | Nothing — Postgres discards the entire uncommitted transaction on connection loss (standard WAL/crash-recovery behavior); `sync_state` still at N-1 | Same as A: re-fetch and re-index block N from scratch. `ON CONFLICT DO NOTHING` + set-based address recompute make this a clean no-op replay for anything that happened to partially apply in a *different* still-open connection (it won't have, since it was never committed) |
| C | After data commit but before checkpoint update | **Cannot occur** — checkpoint update is inside the same transaction as the data, so "data committed, checkpoint not" is not a reachable state | N/A by construction |
| D | Immediately after checkpoint commit | Block N fully durable, checkpoint at N | Indexer proceeds to N+1 normally |

Row C is eliminated by design rather than handled — that is the point.

## 5. Reorg strategy

Every stored block carries `hash`, `prev_hash`, and `height`, which is
sufficient for correctness-first reorg handling (no deep-reorg optimizations
yet):

1. **Detect.** Before indexing a new block from Core, compare its
   `previousblockhash` (from `getblock`) against the locally stored
   canonical tip's `hash`. Mismatch ⇒ possible fork; also treat a
   `getbestblockhash` that doesn't match our expected next-block's parent
   the same way.
2. **Find common ancestor.** Walk Core's chain backward via
   `previousblockhash` and our locally stored canonical chain backward via
   `blocks.prev_hash`, height by height, until the hashes agree at some
   height H. Because our local chain is exactly as long as what we've
   indexed, this terminates in at most `our_tip_height - H` steps.
3. **Roll back, in one transaction:** for every locally canonical block
   above H (highest height first):
   - Mark it `orphaned = true`, `orphaned_at = now()` (kept for audit, not
     deleted).
   - For every output *created* in that block: delete its row (and its
     `output_addresses`/`output_participants` rows) — it never existed on
     the canonical chain.
   - For every output *spent* by a transaction in that block: restore it
     (`spent = false`, `spending_txid = NULL`, `spending_vin_index = NULL`,
     `spending_block_height = NULL`).
   - Delete the block's `transactions`/`transaction_inputs`/
     `transaction_input_witness` rows (identified via `block_hash`).
   - Recompute `addresses` rows for every address touched by the rolled-back
     block, using the same set-based aggregate as normal indexing — this is
     safe and correct by the same idempotency argument as §4.
   - Set `sync_state` to height H / the common-ancestor hash.
4. **Resume** normal linear indexing from H+1 using Core's now-canonical
   chain. The blocks that get (re-)indexed there are brand new to the
   database (their heights were just vacated), so ordinary `INSERT` applies.

**Depth safety valve (resolved — see §13.B):** reorgs of depth ≤ 100 blocks
roll back automatically via the procedure above; a detected reorg deeper than
100 blocks halts indexing before any rollback and requires manual review.

## 6. Supply model

Four distinct concepts, computed independently so a bug in one can't
contaminate another:

- **Block subsidy** — a pure function of height, confirmed directly from
  Qogecoin Core source (`src/validation.cpp`, `GetBlockSubsidy`):
  `subsidy(height) = 100 QOGE >> (height / 500000)` (`nSubsidyHalvingInterval
  = 500000` per `src/chainparams.cpp`), zero once the shift exceeds 63
  halvings. Cross-checked empirically against the live chain: height
  1,000,000 (2 halvings) → 25 QOGE; height 2,000,000 (4 halvings) → 6.25
  QOGE — both match the formula exactly.
- **Transaction fees** — per block, `SUM(non-coinbase input values) -
  SUM(non-coinbase output values)`, computed from indexed data once all
  referenced inputs are resolved.
- **Total issued supply** — `SUM(subsidy(h) for h in 0..tip)`. Monotonic,
  height-derived, never touches indexed coinbase-output data at all.
- **Currently unspent ("circulating") supply** *(future, optional)* —
  `SUM(value_satoshis) FROM transaction_outputs WHERE NOT spent AND
  script_type != 'nulldata'`. This is `total_issued_supply` minus everything
  provably burned (`nulldata`/OP_RETURN outputs, which are consensus-
  unspendable) minus nothing else — spent-and-respent value doesn't
  disappear, it just moves.

**Recommendation:** compute issued supply from the **height-based subsidy
formula**, not from `coinbase outputs − fees` derived from indexed data. The
formula is a pure function of height — it's correct even for blocks not yet
indexed, requires trusting no indexed data at all, and can't be corrupted by
an indexing bug. Deriving it from indexed coinbase totals minus a separately
indexed fee figure requires *both* numbers to be correct, which is exactly
the kind of double bookkeeping that let eIquidus's bug through unnoticed.
The two numbers should still be cross-checked against each other
periodically as an integrity check (`cumulative coinbase output value` should
always equal `total issued supply + cumulative fees`) — a mismatch means an
indexing bug, and is exactly the kind of alarm eIquidus had no way to raise.

## 7. Script classification model

```go
type ScriptType string

const (
    ScriptP2PK           ScriptType = "p2pk"
    ScriptP2PKH          ScriptType = "p2pkh"
    ScriptP2SH           ScriptType = "p2sh"
    ScriptP2WPKH         ScriptType = "p2wpkh"
    ScriptP2WSH          ScriptType = "p2wsh"
    ScriptP2TR           ScriptType = "p2tr"    // witness v1, 32-byte program (Core: TxoutType::WITNESS_V1_TAPROOT)
    ScriptP2QPK          ScriptType = "p2qpk"
    ScriptNullData       ScriptType = "nulldata"
    ScriptMultisig       ScriptType = "multisig"
    ScriptUnknownWitness ScriptType = "unknown_witness" // witness version >0, not one of the recognized version/length combinations above
    ScriptUnknown        ScriptType = "unknown"          // not a witness program at all, OR a witness-v0 program whose length is neither 20 nor 32 (Core: Solver() falls through to NONSTANDARD, not WITNESS_UNKNOWN, for that specific v0 case)
)
```

(The Phase 2A implementation in `internal/script` names these `Type`/`TypeP2PK`/etc. — same values, `Type` instead of `ScriptType` as the prefix.)

Classification rule: **never invent an address for a script we can't
identify.** `ScriptUnknown` / `ScriptUnknownWitness` outputs get zero rows in
`output_addresses` — no `unknown_address` placeholder string, which is the
eIquidus pattern this explicitly avoids (a placeholder string is
indistinguishable from a real, meaningful label once it accumulates balance,
exactly what happened during eIquidus's own pre-fix P2PK bug).

### P2PK handling

Confirmed from Core source and cross-validated against live chain data
(blocks 1 and 5,000; both matched independently re-derived addresses):

- Core's RPC (`ScriptToUniv` in `src/core_write.cpp`) deliberately omits the
  `address` field for `type: "pubkey"` outputs — `include_address &&
  type != TxoutType::PUBKEY` is the exact condition. So unlike every other
  standard type, **Core will never hand the explorer a ready-made address
  for a bare P2PK output.**
- The correct, Core-validated fallback: derive the P2PKH address a P2PK
  output's bare key would correspond to, via `getdescriptorinfo("pkh(<pubkey
  hex>)")` → `deriveaddresses(<canonical descriptor>)`. This was verified
  live against two real mainnet P2PK coinbase outputs (blocks 1 and 5,000)
  and matched eIquidus's independently-stored value exactly in both cases.
  QOGE's coinbase format used P2PK only for blocks 1–7,985 (confirmed via
  binary search against live RPC); block 7,986 onward is `pubkeyhash`.
- Store the resolved address in `output_addresses` with `script_type =
  'p2pk'`, so the explorer's own historical record stays distinguishable
  from a native P2PKH output even though the *address* looks identical to
  one.

### P2QPK handling — see §9 (dedicated section, given the task's emphasis).

## 8. Large PQ-witness handling (~17,088-byte SLH-DSA signatures)

Confirmed from source (`src/script/interpreter.h`): `SLHDSA_SIG_LEN = 17088`
(exact — SLH-DSA has no variable-length fields, so this is not a "≤ max," it
is the only valid length). A witness stack item this large must never be an
implicit cost of an ordinary block or transaction *listing* call.

Design:

- **Storage:** `transaction_input_witness` (§3) is a separate table keyed by
  `(txid, vin_index, item_index)`, `data BYTEA`. No JSON/array column on
  `transaction_inputs` itself — a block-listing or tx-listing query that
  joins/selects from `transactions`/`transaction_inputs` never touches this
  table.
- **Raw storage, no truncation.** `BYTEA` has no practical length limit
  relevant here (Postgres row/TOAST limits are far above 17KB); the full
  signature and pubkey are stored byte-for-byte. Consensus data is never
  truncated, abbreviated, or hashed-and-discarded.
- **API shape:** block/transaction *list* endpoints return input summaries
  (`has_witness: bool`, item count) only. The full witness stack is only
  returned by a dedicated transaction-detail endpoint
  (`GET /api/tx/{txid}`, or a further-dedicated
  `GET /api/tx/{txid}/witness/{vin_index}` for truly large payloads), so a
  page rendering "20 recent transactions" never has to inline 20×17KB of
  hex.
- **UI:** transaction-detail pages lazy-load witness data for P2QPK inputs
  behind a "show signature" affordance rather than inlining ~34KB of hex
  (17,088 bytes → 34,176 hex characters) into the initial page render.
- **API response limits:** the general JSON API should paginate/limit
  collection endpoints independent of this issue, but for the witness data
  specifically, a single input's witness (a few tens of KB) is small enough
  that no special chunking is needed once it's behind its own endpoint —
  only the *default-included-everywhere* pattern is the actual risk, and
  that's what the separate table and separate endpoint both eliminate.

## 9. P2QPK — detection, addressing, activation state

All of the following is confirmed by direct inspection of
`/home/ion/qogecoin` source, not assumption, and cross-checked live against
the running node.

- **Witness version / program length:** `WITNESS_V2_SLHDSA` (`SigVersion`
  enum, `src/script/interpreter.h`) — witness version **2**, program length
  exactly **32 bytes** (`SLHDSA_PK_LEN = 32`).
- **The 32-byte witness program is a *commitment*, not the raw public key.**
  From `VerifyWitnessProgram` (`src/script/interpreter.cpp`, `witversion ==
  2` branch): the program must equal `HASH256(pubkey)`. The actual 32-byte
  SLH-DSA public key is only ever revealed in the witness stack at spend
  time, alongside the 17,088-byte signature — **before an output is spent,
  the explorer cannot know the real public key, only its hash**, exactly
  analogous to P2WSH. Witness stack order (confirmed via `SpanPopBack`
  usage, bottom→top): `[signature (17,088 bytes), pubkey (32 bytes)]` — the
  same order Core's `getrawtransaction` reports in `txinwitness`.
  Stack size must be exactly 2; any other size is a script error.
- **RPC classification is NOT source-distinguishable as P2QPK.** Confirmed
  in `src/script/standard.cpp`: the `TxoutType` enum has no P2QPK entry at
  all — the `Solver()` witness-version dispatch only special-cases version 0
  (P2WPKH/P2WSH) and version 1 32-byte (Taproot); every other
  version/length, **including version 2 32-byte, falls through to
  `TxoutType::WITNESS_UNKNOWN`**, which RPC serializes as `"type":
  "witness_unknown"`. This is exactly the ambiguity the task specification
  anticipated — confirmed true from source, not merely assumed.
- **Detection rule (matches Core's own policy code exactly):**
  `src/policy/policy.cpp`, `AreInputsStandard`, treats an input as standard
  P2QPK precisely when
  `witnessversion == 2 && witnessprogram.size() == SLHDSA_PK_LEN`. The Go
  explorer uses this identical rule — **not** the RPC `"type"` string — to
  set `is_p2qpk = true` and `script_type = 'p2qpk'` (rather than leaving it
  classified as `unknown_witness`).
- **Address handling: Core already encodes a valid address; do not hand-roll
  one.** `key_io.cpp`'s `EncodeDestination` has a generic `WitnessUnknown`
  path that Bech32m-encodes *any* witness version 1–16 / length 2–40 program
  using the network's `bech32_hrp` — it is not Taproot/P2QPK-specific code,
  it simply doesn't require special-casing. Confirmed mainnet HRP `"bq"`,
  testnet `"bqt"` (`src/chainparams.cpp`). `ExtractDestination` (used
  internally by `ScriptToUniv`) succeeds for `WITNESS_UNKNOWN` the same way.
  **Net effect: Core's `getrawtransaction`/`getblock` RPC already returns a
  correctly-formed `bq1...` address for a P2QPK output today, alongside
  `type: "witness_unknown"`.** The Go explorer should read that `address`
  field as-is and only use the witness-version/length rule to *upgrade its
  own classification label* from `unknown_witness` to `p2qpk` — it does not
  need, and must not implement, its own Bech32m encoder for this.
- **`txinwitness` exposure:** confirmed present in RPC verbose output
  (`TxToUniv`, `src/core_write.cpp`) as a hex-string array whenever
  `scriptWitness` is non-null — no special flag or verbosity level required
  beyond the usual verbose `getrawtransaction`/`getblock 2`.
- **size/vsize/weight:** confirmed present at the **transaction** level in
  RPC verbose output (`entry.pushKV("size"/"vsize"/"weight", ...)` in
  `TxToUniv`) — available today, independent of P2QPK. (eIquidus stores none
  of these; the Go schema does, per §3.)
- **Deployment / activation state (live, checked against the running
  node):** `getdeploymentinfo` reports `p2qpk`: `type: bip9`, **`active:
  false`**, `status: "defined"` — the signaling window has not even started.
  Pre-activation, spending a P2QPK output requires **no valid signature at
  all** (`VerifyWitnessProgram`'s `witversion==2` branch returns success
  immediately unless `SCRIPT_VERIFY_P2QPK` is set) — i.e. it is
  anyone-can-spend by consensus design until activation, matching Taproot's
  own historical rollout pattern. Practical implication: no real P2QPK
  transaction exists on this chain yet, and none should be manufactured to
  test against (per the task's explicit instruction) — the above is derived
  entirely from source reading and the live (inactive) deployment status,
  not from observed transactions.
- **`chain_deployments`** (§3) caches `getdeploymentinfo` per sync cycle
  purely for display (e.g. a "P2QPK: not yet active" banner) — Core remains
  the sole authority on actual activation state; this cache is never
  consulted for consensus/script-verification decisions, only for UI.

## 10. Initial API direction

Not implemented yet (no public server per constraints). Planned shape for
Phase 2, loopback-only until explicitly authorized to expose it:

- `GET /healthz` — implemented today (§ skeleton), no chain data.
- `GET /api/block/{hash|height}` — block summary, tx list (txids + summary
  only, no witness payloads).
- `GET /api/tx/{txid}` — full transaction detail, including witness stack
  hex for each input (this is the endpoint allowed to be "big").
- `GET /api/address/{address}` — balance, tx count, from the `addresses`
  cache table (§3), never recomputed inline per-request.
- `GET /api/supply` — issued supply (height-derived, §6), plus the
  cross-check figure (coinbase totals − fees) for integrity monitoring.
- `GET /api/deployments` — cached `chain_deployments` contents (§9), for a
  "network status" style page.

## 11. Bare multisig (resolved — see §13.A)

**Corrected in PR #1 review, before Phase 2B.** The original version of
this section said every participating pubkey-derived address for a
bare-multisig output should be stored in `output_addresses`, one row each.
That was an accounting hazard: `output_addresses` is exactly the table
balance aggregation joins against (§3, `addresses`), so storing all n
participant addresses there would credit the *entire* output value to
*every* participant independently — an m-of-n multisig output's value would
inflate the sum of all address balances by (n − 1) times its value, for
every multisig UTXO that exists. A multisig output is jointly controlled by
all n named keys; it is not owned n times over.

**Corrected policy:** two separate tables, one role each (§3):

- `output_addresses` — the actual destination(s) that balance accounting is
  derived from. Zero or **one** row for every currently-supported script
  type (P2PK, P2PKH, P2WPKH, P2QPK, etc.); **never** populated for bare
  MULTISIG.
- `output_participants` — every legitimately resolved participating
  pubkey-derived address for a bare-multisig output, one row each, for
  search/display ("this address co-signs this UTXO") — but structurally
  excluded from ever being joined into `addresses`.

Equivalently, every output-address relationship has an implicit
`role ∈ {destination, participant}`, and only `role = destination`
(`output_addresses`) ever participates in balance/received/sent
calculations. This is the "Option A" design named in the PR #1 review: two
tables rather than one `role` column, because it makes the balance-query
join target syntactically impossible to get wrong (you'd have to
deliberately query the wrong table) rather than merely a `WHERE role = ...`
clause someone could forget.

## 12. Wallet cross-check (Symbiont Wallet)

`/home/ion/symbiont-wallet` (QOGE/symbiont-wallet, MIT licensed) was cloned
and inspected at commit `2973fe9a0c02d366658c71878cf1c93d1fae80ed`. It
independently implements the wallet side of the same P2QPK design described
in §9, and its own source comments cross-reference the exact same Core files
this document does (e.g. `address/address.go` cites
`qogecoin/qogecoin src/script/interpreter.cpp`). Every fact previously
established from Core source alone was checked against the wallet's
implementation; **all of it agrees, with zero disagreements**:

| Property | Core (this repo's §9) | Symbiont Wallet | Agreement |
|---|---|---|---|
| Witness version | 2 | `address.WitnessVersion = 2`; scriptPubKey byte `0x52` (`OP_2`) | ✅ |
| Program length | 32 bytes | `address.AddressLength = 32` | ✅ |
| Program construction | `HASH256(pubkey)` | `hash256(pubkey)` = SHA256(SHA256(pubkey)), documented explicitly as "the fundamental HNDL defence" | ✅ |
| scriptPubKey layout | witness program (op_2, push32, program) | `OP_2 (0x52) \| PUSH32 (0x20) \| HASH256(pubkey)` — `wallet/wallet.go: p2qpkScriptPubKey` | ✅ |
| Address encoding | Bech32m (generic `WitnessUnknown` path) | Bech32m, with an explicit BIP350 checksum-constant/witness-version binding check on decode | ✅ |
| Mainnet HRP | `bq` | `bq` | ✅ |
| Testnet HRP | `bqt` | `bqt` | ✅ |
| Regtest HRP | `bq` (chainparams.cpp: "matches Symbiont Wallet's P2QPK address format for regtest testing") | `bq` (`address.go`: "same as mainnet for now") | ✅ (intentionally shared — see §13.C) |
| Algorithm | SLH-DSA-SHA2-128f | `slhdsa.AlgorithmName = "SLH_DSA_PURE_SHA2_128F"` (liboqs) | ✅ |
| Pubkey length | 32 bytes (`SLHDSA_PK_LEN`) | `slhdsa.PublicKeySize = 32` | ✅ |
| Signature length | 17,088 bytes (`SLHDSA_SIG_LEN`), exact | `slhdsa.SignatureSize = 17088` | ✅ |
| Witness stack order | `[signature, pubkey]` (`SpanPopBack` order in `VerifyWitnessProgram`) | `[signature, pubkey]`, stated explicitly and repeatedly in the wallet's own SIP design docs (`docs/sips/SIP QOGE PQC 02a P2QPK.md`) | ✅ |

No corrections to §9 were needed as a result — this cross-check is
independent confirmation, not new information. Two things worth carrying
forward as *reference*, not as corrections:

- The wallet computes the P2QPK sighash (`wallet/wallet.go:
  computeP2QPKSighash`) as `TaggedHash("P2QPKSighash"; epoch ‖ SIGHASH_ALL ‖
  nVersion ‖ nLockTime ‖ hashPrevouts ‖ hashAmounts ‖ hashScriptPubkeys ‖
  hashSequences ‖ hashOutputs ‖ in_pos)`, explicitly mirroring
  `SignatureHashP2QPK` in Core's `interpreter.cpp`. The explorer never signs
  anything, so it has no need to compute this — noted only in case a future
  "verify this P2QPK signature" display feature is ever wanted.
- `address/bech32m.go` in the wallet is a self-contained BIP350 Bech32m
  codec (`encodeGeneric`/`decodeGeneric`, parameterized checksum constant),
  written because the wallet's btcutil dependency only implements classic
  Bech32 (BIP173). The explorer needs the same BIP350 decode capability
  (§9) and this is a correctly-implemented, MIT-licensed reference for it —
  see §14 for the reuse recommendation.

## 13. Resolved Phase-1 policy decisions

**A. Bare multisig** — see §11 (revised in the PR #1 review): store every
legitimately resolved participating address in `output_participants` for
search/display, one row each — never collapse to a single owner — but keep
it structurally separate from `output_addresses`, the table balance
accounting actually joins against. A multisig output gets zero
`output_addresses` rows; crediting its full value to every participant's
individual balance would overcount total supply.

**B. Deep reorg safety valve** — see §5: reorg depth ≤ 100 blocks rolls back
automatically; depth > 100 blocks halts indexing and requires manual human
review before proceeding. Rationale: a reorg that deep is far outside normal
QOGE chain behavior and more likely indicates a bug in the detection logic,
a corrupted local chain, or a genuinely exceptional network event — any of
which warrant a human looking before the indexer starts deleting/restoring
potentially large amounts of indexed state automatically.

**C. Network identity** — the explorer's configured network (mainnet /
testnet / regtest) must come from explicit operator configuration
(`internal/config`), never be inferred from an address's HRP alone. This is
confirmed necessary, not merely theoretical: both Core (`chainparams.cpp`)
and Symbiont Wallet (`address.go`) intentionally use the **same** HRP
(`"bq"`) for mainnet and regtest (§12), so a `bq1...` address string alone
is genuinely ambiguous between the two networks by design. The explorer
must carry its own network identity as configuration state and use it to
validate that indexed data (and any address a user pastes into a search box)
belongs to the expected network, rather than trusting the HRP to disambiguate.

## 14. Useful Symbiont Wallet references for Phase 2

Identified as worth consulting or mirroring — **not copied into this repo**:

- `address/bech32m.go` (`encodeGeneric`/`decodeGeneric`) — correct, tested
  BIP350 Bech32m codec including the checksum-constant/witness-version
  binding-rule check. The explorer needs equivalent decode logic for
  P2QPK/witness-unknown addresses; reimplementing this from scratch risks
  reintroducing a known class of bech32/bech32m confusion bugs this code
  already guards against.
- `address/address.go` constants — `WitnessVersion = 2`, `AddressLength =
  32`, `HRP = "bq"`, the `Network`/`knownHRPs` pattern for HRP↔network
  mapping. Worth mirroring the *shape* of this (small, explicit, no magic)
  rather than importing the package directly, since the package is
  signing/wallet-oriented (pulls in address generation, not just decode).
- `signer/slhdsa.go` constants only — `PublicKeySize = 32`,
  `SignatureSize = 17088`, `AlgorithmName`. Useful for the explorer's own
  witness-shape validation/display (e.g. flagging a P2QPK witness whose
  pubkey/signature lengths don't match expectations). The explorer should
  **not** import this package or take a liboqs/CGo dependency — it never
  signs or verifies signatures, only displays already-mined data.
- `wallet/wallet.go: p2qpkScriptPubKey` — confirms the exact
  `OP_2 | PUSH32 | HASH256(pubkey)` byte layout independently of Core, for
  anyone implementing a from-scratch script parser (the explorer instead
  reads Core's own parsed `scriptPubKey.hex`/`asm`, so this is confirmatory
  reference, not something to port).
- `docs/sips/SIP QOGE PQC 02a P2QPK.md` and `SIP QOGE PQC 02 P2QPK.md` — the
  design-rationale documents behind everything above; useful background if
  a question comes up later that isn't answered by the code comments alone.
