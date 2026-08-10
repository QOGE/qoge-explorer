# QOGE Go Explorer — Architecture

Status: Phase 2B.2 — canonical chain model and script classification are
implemented (`internal/chain`, `internal/script`); the PostgreSQL schema,
migrations, and migration tooling are implemented (`migrations/`,
`internal/store`, §3/§15); the atomic block-application/reorg store engine
is implemented (`internal/store.Store`, §16). No historical
synchronization, RPC→chain translation, continuous block-fetch loop,
public API, or web UI exists yet — that is the next phase, built on top of
the store engine this phase locks down. Qogecoin Core is the sole
authoritative source of chain truth; this document describes how the
explorer observes and represents that truth, never how it decides it.

## 1. Component overview

Single Go service, no microservices, for the foreseeable future:

```
cmd/qoge-explorer/        entry point, subcommand dispatch (check-rpc, serve, migrate)
internal/config/          env-var configuration loading (no credential logging)
internal/logging/         structured slog setup
internal/rpc/             Qogecoin Core JSON-RPC client (generic Call + typed helpers)
internal/chain/           canonical block/tx/output domain model (Core-shape-independent) — implemented
internal/script/          script classification (P2PK/P2PKH/.../P2QPK/UNKNOWN) — implemented
internal/indexer/         sync loop: fetch from rpc, classify via script, write via store — next phase
internal/store/           PostgreSQL connection + migration runner (§15) and the atomic
                          block-application/reorg store engine (§16) — both implemented
internal/api/             HTTP/JSON API (not wired to a public port yet)
internal/web/             HTML presentation layer (not built yet)
migrations/               versioned SQL schema (§3, §15) — implemented
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

**Status: FINAL for Phase 2B.1, implemented in `migrations/0001_initial.up.sql`
(and reversed in `0001_initial.down.sql`).** That migration file is the
authoritative DDL; what follows is a guided tour of its design, not a
duplicate copy — see the migration itself for every constraint verbatim.

Integer satoshis (`BIGINT`) everywhere; no floating point for QOGE values
anywhere in the schema or the Go model.

This is a full rewrite of the Phase-1/2A schema sketch, driven by four
corrections identified in Phase 2B.1 review before any migration was
written (not bolted on afterward):

1. **Transaction identity vs. transaction occurrence were conflated.**
   The old sketch put `block_hash`/`block_height`/`tx_index` directly on
   `transactions`, keyed by `txid PRIMARY KEY` — meaning a txid could only
   ever belong to one block. That's wrong for an explorer that keeps orphan
   blocks as an audit trail: the same transaction body can legitimately
   appear in two different blocks across a reorg. See §3a.
2. **Immutable output data and mutable canonical spend state were
   conflated.** The old sketch put `spent`/`spending_txid`/
   `creation_block_height` directly on `transaction_outputs`. That table
   should describe an output exactly as it was created, forever; whether
   it's currently unspent on the canonical chain is a separate, mutable
   fact that reorgs need to rewrite without touching the immutable body.
   See §3b.
3. **`is_p2qpk` duplicated what `script_type = 'p2qpk'` already said.**
   Removed entirely — see §3c.
4. **txid vs. wtxid were conflated.** `transactions` was keyed by txid
   alone, with `size`/`vsize`/`weight` and witness data (`block_transactions`,
   `transaction_input_witness`) also keyed only by txid. Because txid
   deliberately excludes witness data (Core's `GetHash()`), two different,
   equally valid witness serializations of the same txid can exist —
   observably, across two competing blocks — and a txid-only key cannot
   represent both without one silently overwriting the other. See §3a.

Raw binary data (scripts, witness items, coinbase data, pubkeys) is
`BYTEA`, not hex `TEXT` — see §3d for the reasoning and the one place hex
`TEXT` is kept anyway (hashes/txids).

**PR #2 independent review found four further issues, fixed before merge,
none requiring a schema redesign — hardening the design above rather than
changing it:**

- **A real NULL loophole in the P2QPK structural `CHECK`.** Postgres
  treats a `CHECK` expression that evaluates to `NULL` as *satisfied*, and
  `script_type <> 'p2qpk' OR (witness_version = 2 AND ...)` evaluates to
  `NULL` — not `FALSE` — when `witness_version` is `NULL`. A row with
  `script_type='p2qpk'` and `witness_version`/`witness_program` both
  `NULL` previously passed. Replaced with a single `CASE script_type ...
  END` constraint, one branch per script_type, every branch written so it
  always evaluates to `TRUE`/`FALSE` and never `NULL` (every nullable
  comparison is preceded by an `IS [NOT] NULL` check). Now covers
  structural consistency for every known witness type
  (P2WPKH/P2WSH/P2TR/P2QPK/unknown_witness/legacy), not just P2QPK. See
  §3b.
- **`output_addresses` could hold two different addresses for one
  output.** Fixed by changing its primary key — see §3d.
- **`utxo_state` proved nothing about whether its claimed
  creation/spending occurrences were real.** Added composite foreign keys
  against `block_transactions` and `transaction_inputs`, plus a
  `BEFORE INSERT OR UPDATE` trigger (mirroring
  `block_transactions_set_height`) that derives
  `creation_block_height`/`spending_block_height` from `blocks.height`
  rather than trusting the caller. See §3b.
- **Numeric range gaps.** `blocks.nonce`, `transactions.locktime`, and
  `transaction_inputs.sequence` are consensus-wire `uint32` fields; added
  `CHECK`s constraining them to `[0, 4294967295]`. Added
  `fee_satoshis >= 0` (when non-`NULL`) and a `CHECK` that a coinbase
  transaction never carries a fee value at all.

Also added: migration checksum drift detection (§15) and per-test-run
isolated PostgreSQL schemas instead of `DROP SCHEMA public` in tests (test
infrastructure, not part of the schema itself — see `internal/store`).

**A third review pass found one blocking issue and three smaller
relational-integrity gaps, all fixed before merge:**

- **The txid/wtxid split (blocking — see item 4 above and §3a).** Two
  different witness serializations of the same txid — legitimately
  observable across competing blocks — could not both be represented; the
  second would silently overwrite the first's witness data and
  size/vsize/weight. `transactions` now holds only the non-witness body;
  a new `transaction_variants` table (keyed by `wtxid`) holds one row per
  concrete witness serialization; `block_transactions` and
  `transaction_input_witness` are keyed by `(txid, wtxid)`/`wtxid`
  respectively, not txid alone.
- **`utxo_state`'s spend FK proved an input existed, not that it spent
  *this* output.** `(spending_txid, spending_vin_index) →
  transaction_inputs(txid, vin_index)` only proved the input row was real;
  an input that actually spends output A could still be used to mark
  unrelated output B "spent." Widened to a four-column FK — see §3b.
- **`unknown`/`unknown_witness` metadata was more permissive than
  `ParseWitnessProgram` allows.** Neither branch enforced the structural
  2–40 byte witness-program length range, and `unknown_witness` didn't
  exclude version/length combinations that actually belong to a named type
  (v1/32 is P2TR, v2/32 is P2QPK). Tightened — see §3b's `CASE` note.
- **Migration checksums only covered `UpSQL`.** An edited `DownSQL` on an
  already-applied migration went undetected. Now both directions are
  checksummed and verified before `Up` or `Down` proceeds, and an applied
  migration with no corresponding local `.sql` files is also an error —
  see §15.

**A fourth review pass found two representation invariants, both fixed
before merge:**

- **`transactions.version` couldn't represent Core's full RPC range.**
  Core's in-memory `CTransaction::nVersion` is `int32_t`, but `TxToUniv`
  exposes it to RPC (and treats it in consensus checks) as
  `static_cast<uint32_t>(tx.nVersion)` — the same uint32-not-int32
  distinction this project already applied to `locktime`/`sequence`/
  `nonce`. `chain.Transaction.Version` changed from `int32` to `uint32`;
  `transactions.version` changed from `INT` to `BIGINT` with a
  `CHECK (version >= 0 AND version <= 4294967295)`. See §3a.
- **`sync_state`'s initialized checkpoint could disagree with `blocks`.**
  The bootstrap-vs-initialized `CHECK` (below) never verified that an
  initialized `indexed_height`/`indexed_block_hash` pair actually matched a
  real, canonical block — a caller could persist a checkpoint pointing at a
  nonexistent hash, an orphaned block, or the wrong height for a real hash.
  A `BEFORE INSERT OR UPDATE` trigger now requires `indexed_block_hash` (when
  non-`NULL`) to reference an existing, `canonical = true` row in `blocks`,
  and mechanically derives `indexed_height` from `blocks.height` rather than
  trusting the caller — same pattern as `block_transactions_set_height` and
  `utxo_state_derive_heights`. This protects the durable checkpoint that
  Phase 2B.2 will update as the final statement of each block-indexing
  transaction. See §3c.

### 3a. Transaction identity, occurrence, and witness variants

**Terminology, precisely — five distinct concepts this section's tables
each represent exactly one of:**

| Term | Meaning | Core RPC field | Table |
|---|---|---|---|
| **txid** | Non-witness transaction identity | `"txid"` (Core's `GetHash()`) | `transactions.txid` |
| **wtxid** | Complete serialized transaction, including witness | `"hash"` — **not** `"txid"` (Core's `GetWitnessHash()`) | `transaction_variants.wtxid` |
| **transaction** | The non-witness body: inputs' prevouts/scriptSigs, outputs | — | `transactions` |
| **transaction variant** | One concrete witness serialization of a transaction | — | `transaction_variants` |
| **block occurrence** | The exact (txid, wtxid) variant appearing at a specific position in a specific block | — | `block_transactions` |

Confirmed from Core's `TxToUniv` (`src/core_write.cpp`): `entry.pushKV("txid",
tx.GetHash().GetHex())` and `entry.pushKV("hash", tx.GetWitnessHash().GetHex())`
— two different hashes, two different meanings, both present in every
verbose transaction RPC response. It is a real and easy mistake to read
Core's `"hash"` field as "the txid" — this project's Go model spells both
out explicitly (`chain.Transaction.TxID` / `.WTxID`) specifically to make
that mistake harder.

**Why this matters for orphan/reorg audit.** Because txid excludes witness
data, two different witness stacks can satisfy the exact same non-witness
txid — same inputs' prevouts, same outputs, same locktime — while producing
two different wtxids. This isn't a hypothetical: it's the same "transaction
malleability" property SegWit itself was deployed to make irrelevant to
consensus, but it is very relevant to an *explorer* that promises to keep
every block — canonical or orphaned — queryable as an audit trail. If block
A (now orphaned) contained txid T with witness W1, and the new canonical
block B recombined the same txid T with a different, equally valid witness
W2, a schema keyed by txid alone could only ever store one of W1/W2 — the
second write would silently destroy the first, which is exactly the kind
of silent data loss this project's audit-trail guarantee (§2, §5) exists to
prevent.

```sql
-- Immutable, NON-WITNESS transaction body. Block-independent: this row's
-- meaning never changes regardless of which block(s) reference it, which
-- witness variant(s) exist for it, or whether those blocks are canonical.
-- Inputs and outputs are keyed by txid (not wtxid) because scriptSig/
-- prevouts/scriptPubKeys/values are part of the txid-determining
-- serialization — witness data is the one thing that ISN'T.
CREATE TABLE transactions (
    txid                TEXT PRIMARY KEY,
    -- version is RPC/consensus-facing uint32 — Core's TxToUniv exposes
    -- static_cast<uint32_t>(tx.nVersion) despite the C++ in-memory type
    -- being int32_t. BIGINT (no native unsigned type), range-checked to
    -- [0, 4294967295] — same pattern as locktime/sequence/nonce.
    version             BIGINT NOT NULL,
    locktime            BIGINT NOT NULL,
    is_coinbase         BOOLEAN NOT NULL,
    fee_satoshis        BIGINT,   -- NULL for coinbase; optional/deferred otherwise (not computed in 2B.1)
    indexed_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One row per concrete witness serialization. A transaction with no
-- witness data at all still gets exactly one variant row, with
-- wtxid == txid. size/vsize/weight depend on the witness serialization
-- actually observed, so they live here, not on `transactions`.
CREATE TABLE transaction_variants (
    wtxid               TEXT PRIMARY KEY,
    txid                TEXT NOT NULL REFERENCES transactions (txid),
    size                INT NOT NULL,
    vsize               INT NOT NULL,
    weight              INT NOT NULL,
    indexed_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (wtxid, txid) -- FK target for block_transactions/transaction_input_witness below
);

-- Occurrence: which block(s) contained which (txid, wtxid) variant, and at
-- what position. The SAME txid — and even the SAME variant — can be linked
-- to two different block_hash rows — e.g. a transaction that appeared in
-- an orphaned block and was later re-included in the new canonical block
-- during a reorg — without duplicating the transaction body. Storing wtxid
-- here (not just txid) records EXACTLY which witness serialization
-- appeared in this specific block. Covered by TestInvariant_SameTxidTwoBlocks
-- and TestInvariant_WitnessVariantsDoNotOverwriteEachOther (internal/store).
CREATE TABLE block_transactions (
    block_hash          TEXT NOT NULL REFERENCES blocks (hash),
    tx_index            INT NOT NULL,
    txid                TEXT NOT NULL REFERENCES transactions (txid),
    wtxid               TEXT NOT NULL,
    block_height        BIGINT NOT NULL,  -- auto-derived by trigger from blocks.height; never supplied by the caller
    PRIMARY KEY (block_hash, tx_index),
    -- A valid block never contains the same txid twice, regardless of
    -- witness variant — duplicate transactions within one block are a
    -- consensus violation, not something this schema needs to represent.
    UNIQUE (block_hash, txid),
    FOREIGN KEY (wtxid, txid) REFERENCES transaction_variants (wtxid, txid)
);
```

`block_height` is set by a `BEFORE INSERT OR UPDATE` trigger
(`block_transactions_set_height`) that looks up `blocks.height` for the
given `block_hash` and raises an exception if no such block exists. This
was a deliberate choice over trusting the caller to keep a denormalized
column in sync: the whole reason this project exists is to not repeat
eIquidus's class of drift bug, and a trigger makes the derived column
provably correct rather than merely documented as such.

**transaction != transaction occurrence.** Every place this document (or
the Go code) says "a transaction's block," it means "the transaction's
occurrence in the currently-canonical block" — i.e. a `block_transactions`
row joined to a `blocks` row where `canonical = true`. A txid without any
`canonical = true` `block_transactions` row is fully orphaned data,
retrievable for audit but not part of the current chain view.

**The Go domain model does not split the way the SQL schema does.**
`chain.Transaction` (`internal/chain/transaction.go`) carries both `TxID`
and `WTxID`, plus `Size`/`VSize`/`Weight` — it represents one *observed*
serialization/variant, which is the natural unit to work with in memory
(matching exactly what one `getrawtransaction`/`decoderawtransaction`
verbose RPC response describes). Only the persistence layer needs the
txid/wtxid split, to keep two variants of the same txid from overwriting
each other in storage.

### 3b. Immutable output body vs. canonical UTXO state

`transaction_outputs` = **every** output that ever existed on-chain,
preserved 1:1, forever. `utxo_state` = the Core-*equivalent* canonical coin
state, **not** a mirror of `transaction_outputs` — `Store.ApplyBlock`
deliberately never creates a `utxo_state` row for a genesis-block (height 0)
output, or for any output `script.IsUnspendable` (Core's own
`CCoinsViewCache::AddCoin`/`CScript::IsUnspendable` semantics: an
`OP_RETURN`-prefixed script, or one longer than 10,000 bytes). See §16
"Core UTXO semantics" for the full detail and the tests proving it —
`transaction_outputs` is written unconditionally either way; only its
`utxo_state` row (spendability, and address-balance contribution) is
excluded.

```sql
-- Immutable output body: value, script, classification — as the output
-- was created, forever.
CREATE TABLE transaction_outputs (
    txid                TEXT NOT NULL REFERENCES transactions (txid),
    vout_index          INT NOT NULL,
    value_satoshis      BIGINT NOT NULL,
    script_pubkey       BYTEA NOT NULL,
    script_type         TEXT NOT NULL,    -- see §7; CHECK constraint, not a native ENUM (see note)
    witness_version     INT,              -- NULL for non-witness scripts
    witness_program     BYTEA,            -- NULL for non-witness scripts
    PRIMARY KEY (txid, vout_index)
);

-- Canonical, MUTABLE spend state — separate from the row above on purpose.
-- One row per output that has ever existed on the canonical chain; rolled
-- back/rebuilt during reorgs. transaction_outputs itself is never touched
-- by a reorg.
--
-- PR #2 review: the FKs below don't just check that the referenced hashes
-- look like real blocks — they prove the claimed creation/spending
-- TRANSACTION OCCURRENCES are real, by referencing block_transactions
-- (which links a txid to a block only when that txid genuinely occurred
-- there) and transaction_inputs. creation_block_height/spending_block_height
-- are never trusted from the caller either — a BEFORE INSERT OR UPDATE
-- trigger (utxo_state_derive_heights, mirroring block_transactions_set_height)
-- derives both from blocks.height.
--
-- Third-round fix: the spending-input FK originally only proved
-- (spending_txid, spending_vin_index) exists as a real transaction_inputs
-- row — NOT that it actually spends THIS output. An input that really
-- spends output A could have been used to mark unrelated output B "spent."
-- Widened to a four-column FK against a matching UNIQUE constraint on
-- transaction_inputs (txid, vin_index, prev_txid, prev_vout_index): this
-- output's own (txid, vout_index) is now part of the referenced tuple, via
-- prev_txid/prev_vout_index, so the claimed spending input must genuinely
-- point back at this exact output.
CREATE TABLE utxo_state (
    txid                    TEXT NOT NULL,
    vout_index              INT NOT NULL,
    creation_block_hash     TEXT NOT NULL,
    creation_block_height   BIGINT NOT NULL,  -- trigger-derived, not caller-supplied
    spent                   BOOLEAN NOT NULL DEFAULT FALSE,
    spending_txid           TEXT,
    spending_vin_index      INT,
    spending_block_hash     TEXT,
    spending_block_height   BIGINT,           -- trigger-derived, not caller-supplied
    PRIMARY KEY (txid, vout_index),
    FOREIGN KEY (txid, vout_index) REFERENCES transaction_outputs (txid, vout_index),
    FOREIGN KEY (creation_block_hash, txid) REFERENCES block_transactions (block_hash, txid),
    FOREIGN KEY (spending_txid, spending_vin_index, txid, vout_index)
        REFERENCES transaction_inputs (txid, vin_index, prev_txid, prev_vout_index),
    FOREIGN KEY (spending_block_hash, spending_txid) REFERENCES block_transactions (block_hash, txid)
);
```

The occurrence FKs use Postgres's default `MATCH SIMPLE`: a composite FK is
satisfied whenever *any* of its columns is `NULL`. That's correct here —
not a loophole — because `utxo_state_spent_consistency` (below) already
guarantees the three `spending_*` columns are `NULL` together or `NOT
NULL` together, and `txid`/`vout_index` are always `NOT NULL` (part of
this table's own primary key); so an unspent row's spending FKs are
trivially (and correctly) satisfied via the `NULL` `spending_*` columns,
and a spent row's are fully checked against all four columns. Confirmed by
`TestInvariant_UTXOSpendMustMatchExactPrevout` (`internal/store`): an input
that exists but points at a different txid, or the right txid but a
different vout, is rejected either way.

Why split them: an output whose creating block gets orphaned during a
reorg simply loses its `utxo_state` row (deleted — it no longer exists on
the canonical chain at all) while its `transaction_outputs` row is
untouched, permanently queryable for audit. If the exact same transaction
later reappears in the new canonical chain (common in shallow reorgs), the
indexer inserts a fresh `utxo_state` row referencing the *already-existing*
`transaction_outputs` row — no re-parsing, no risk of the script/value
data disagreeing with what was originally seen.

Companion tables. `transaction_inputs` is still keyed by txid, not wtxid —
inputs' prevouts/scriptSigs are part of the txid-determining serialization
(§3a). It carries one more constraint than the Phase-1 sketch: a `UNIQUE
(txid, vin_index, prev_txid, prev_vout_index)`, purely to serve as the
target for `utxo_state`'s four-column exact-prevout FK above — trivially
true given `(txid, vin_index)` is already the primary key, but Postgres
requires a unique constraint matching the exact referenced column set.
`transaction_input_witness`, by contrast, **is** keyed by wtxid — witness
data is exactly the thing that differs between two variants of the same
txid, so keying it by txid alone would let one variant's witness silently
overwrite another's (the bug item 4/§3a exists to prevent):

```sql
CREATE TABLE transaction_inputs (
    txid                TEXT NOT NULL REFERENCES transactions (txid),
    vin_index           INT NOT NULL,
    prev_txid           TEXT,    -- NULL for coinbase
    prev_vout_index     INT,     -- NULL for coinbase
    coinbase            BYTEA,   -- set only for the coinbase input
    script_sig          BYTEA,
    sequence            BIGINT NOT NULL,
    PRIMARY KEY (txid, vin_index),
    UNIQUE (txid, vin_index, prev_txid, prev_vout_index) -- FK target for utxo_state above
);

-- Witness stack data lives in its own table so ordinary listing/detail
-- queries never have to pull ~17KB P2QPK signatures along for the ride —
-- see §8. Keyed by wtxid (variant-specific), not txid.
CREATE TABLE transaction_input_witness (
    wtxid               TEXT NOT NULL,
    txid                TEXT NOT NULL,
    vin_index           INT NOT NULL,
    item_index          INT NOT NULL,   -- position in the witness stack, 0 = bottom
    data                BYTEA NOT NULL,
    PRIMARY KEY (wtxid, vin_index, item_index),
    -- Proves wtxid belongs to this txid's transaction_variants row, and
    -- that (txid, vin_index) is a real input of that txid's body.
    FOREIGN KEY (wtxid, txid) REFERENCES transaction_variants (wtxid, txid),
    FOREIGN KEY (txid, vin_index) REFERENCES transaction_inputs (txid, vin_index)
);
```

### 3c. Blocks, addresses, sync state, deployments

```sql
-- canonical replaces the earlier "orphaned" boolean sketch (same idea,
-- inverse polarity — reads better as `WHERE canonical`). A block row is
-- never deleted, only demoted.
CREATE TABLE blocks (
    hash                TEXT PRIMARY KEY,
    height              BIGINT NOT NULL,
    prev_hash           TEXT REFERENCES blocks (hash), -- NULL only for genesis; self-FK requires the parent to already exist
    merkle_root         TEXT NOT NULL,
    "time"              BIGINT NOT NULL,
    bits                TEXT NOT NULL,
    difficulty          DOUBLE PRECISION NOT NULL, -- display only; never used for consensus decisions
    nonce               BIGINT NOT NULL,
    size                INT NOT NULL,
    weight              INT NOT NULL,
    tx_count            INT NOT NULL,
    canonical           BOOLEAN NOT NULL DEFAULT TRUE,
    orphaned_at         TIMESTAMPTZ,
    indexed_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Exactly one canonical block per height — the core reorg invariant.
CREATE UNIQUE INDEX blocks_height_canonical_uidx ON blocks (height) WHERE canonical;

-- Derived balance cache — see §4 item 4 and §13.A/§7 for why
-- output_addresses (never output_participants) is what this is built from.
CREATE TABLE addresses (
    address                     TEXT PRIMARY KEY,
    total_received_satoshis     BIGINT NOT NULL DEFAULT 0,
    total_sent_satoshis         BIGINT NOT NULL DEFAULT 0,
    balance_satoshis            BIGINT NOT NULL DEFAULT 0,
    tx_count                    INT NOT NULL DEFAULT 0,
    first_seen_height           BIGINT,
    last_seen_height            BIGINT,
    updated_at                  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Bootstrap state (an explorer that has never synced) is explicit, not a
-- fake genesis-adjacent hash: height=-1 means "nothing indexed yet," and
-- the CHECK constraint makes -1-with-a-hash or >=0-without-one
-- unrepresentable. Confirmed by TestInvariant_UninitializedSyncStateValid
-- and TestInvariant_ContradictorySyncStateRejected (internal/store).
CREATE TABLE sync_state (
    name                TEXT PRIMARY KEY,
    indexed_height      BIGINT NOT NULL DEFAULT -1,
    indexed_block_hash  TEXT,
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT sync_state_bootstrap_consistency CHECK (
        (indexed_height = -1 AND indexed_block_hash IS NULL) OR
        (indexed_height >= 0 AND indexed_block_hash IS NOT NULL)
    )
);

-- PR #2 review round 4: the CHECK above only proved internal
-- self-consistency, never that an initialized checkpoint actually agreed
-- with `blocks`. This trigger (installed once `blocks` exists) requires
-- indexed_block_hash to reference a real, canonical block, and mechanically
-- derives indexed_height from blocks.height rather than trusting the
-- caller — same pattern as block_transactions_set_height/
-- utxo_state_derive_heights. Confirmed by
-- TestInvariant_SyncStateCheckpointMustMatchCanonicalBlock (internal/store).
CREATE OR REPLACE FUNCTION sync_state_validate_checkpoint() RETURNS trigger AS $$
DECLARE
    blk_height    BIGINT;
    blk_canonical BOOLEAN;
BEGIN
    IF NEW.indexed_block_hash IS NULL THEN
        IF NEW.indexed_height <> -1 THEN
            RAISE EXCEPTION 'sync_state: indexed_block_hash is NULL but indexed_height=%', NEW.indexed_height;
        END IF;
        RETURN NEW;
    END IF;

    SELECT height, canonical INTO blk_height, blk_canonical
    FROM blocks WHERE hash = NEW.indexed_block_hash;

    IF blk_height IS NULL THEN
        RAISE EXCEPTION 'sync_state: no block found for indexed_block_hash %', NEW.indexed_block_hash;
    END IF;
    IF NOT blk_canonical THEN
        RAISE EXCEPTION 'sync_state: block % is not canonical', NEW.indexed_block_hash;
    END IF;

    NEW.indexed_height := blk_height;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER sync_state_validate_checkpoint_trigger
    BEFORE INSERT OR UPDATE ON sync_state
    FOR EACH ROW EXECUTE FUNCTION sync_state_validate_checkpoint();

-- Deployment status cache (display only; Core remains authoritative).
CREATE TABLE chain_deployments (
    name                TEXT PRIMARY KEY, -- e.g. 'p2qpk'
    status              TEXT NOT NULL,    -- defined/started/locked_in/active/failed
    since_height        BIGINT,
    raw_json            JSONB NOT NULL,
    checked_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

### 3d. Output addressing: destinations vs. multisig participants

```sql
CREATE TABLE output_addresses (   -- balance-accounting destinations ONLY
    txid                TEXT NOT NULL,
    vout_index          INT NOT NULL,
    address             TEXT NOT NULL,
    PRIMARY KEY (txid, vout_index), -- AT MOST ONE destination per output — see PR #2 review below
    FOREIGN KEY (txid, vout_index) REFERENCES transaction_outputs (txid, vout_index)
);

CREATE TABLE output_participants (   -- MULTISIG co-signer identities, display/search only
    txid                TEXT NOT NULL,
    vout_index          INT NOT NULL,
    address             TEXT NOT NULL,
    pubkey              BYTEA NOT NULL,
    PRIMARY KEY (txid, vout_index, address), -- MANY participants per output is correct here
    FOREIGN KEY (txid, vout_index) REFERENCES transaction_outputs (txid, vout_index)
);
```

**PR #2 review fix: `output_addresses`'s primary key was originally
`(txid, vout_index, address)`, matching `output_participants`'s shape** —
which permitted a second, *different* destination address for the same
output. That would have silently double-counted an ordinary, non-multisig
output's value across two addresses' balances — the same class of bug §7's
multisig split exists to prevent, just not caught by that split since it
doesn't require `script_type = 'multisig'` to happen. Changed to `PRIMARY
KEY (txid, vout_index)`: at most one destination per output is now
structurally guaranteed, not just conventionally expected. Confirmed by
`TestInvariant_OneDestinationAddressPerOutput` (`internal/store`).

Both tables carry a `BEFORE INSERT OR UPDATE` trigger, not just a naming
convention: `output_addresses_reject_multisig` raises an exception if the
referenced output's `script_type = 'multisig'`, and
`output_participants_require_multisig` raises one if it *isn't*. It is
therefore a database error, not merely a code-review concern, to credit a
multisig output's value to a participant's balance. See §7 and §13.A.

### Notes

**`script_type` is `TEXT` + `CHECK`, not a native `ENUM`** — deliberately.
Adding a value to a native enum type is a schema migration with
transactional caveats (`ALTER TYPE ... ADD VALUE` cannot run inside the
same transaction as its first use, in some Postgres versions). New
witness/script types are expected — most notably P2QPK activating for
real — so the classification list needs to be cheap to extend. A `CHECK`
constraint is a single, ordinary migration. Current allowed values: `p2pk,
p2pkh, p2sh, p2wpkh, p2wsh, p2tr, p2qpk, nulldata, multisig,
unknown_witness, unknown`.

**`is_p2qpk` was removed, not merely deprecated.** `script_type = 'p2qpk'`
is the sole source of truth. A structural (not consensus)
`transaction_outputs_witness_metadata_consistency` `CHECK` — a
`CASE script_type ... END` covering every witness type, not just P2QPK,
written so every branch always evaluates to `TRUE`/`FALSE` and never the
`NULL` a naive `script_type <> 'p2qpk' OR (witness_version = 2 AND ...)`
expression would (Postgres treats a `NULL` `CHECK` result as satisfied —
see the PR #2 note above) — makes a row claiming the P2QPK classification
without the matching witness shape a database error rather than a
silently-possible contradiction. This is a byte-length/version-number
check, never script execution or signature verification.

**`unknown_witness`/`unknown` were tightened in the third review round to
match `ParseWitnessProgram` exactly, not just "roughly."** Both branches
now enforce the structural 2–40 byte witness-program length range (BIP141/
Core's `CScript::IsWitnessProgram`), and `unknown_witness` explicitly
excludes any version/length combination that actually belongs to a named
type (`v1/32` is P2TR, `v2/32` is P2QPK — those must never be left
classified as generic `unknown_witness`). 25-case table-driven test
(`TestInvariant_WitnessMetadataConsistency`, `internal/store`) covers every
boundary named in review, including the exact program-length-1 and
program-length-41 edge cases.

**Format constraints, not policy constraints.** Every hash-like `TEXT`
column (`blocks.hash`, `transactions.txid`, etc.) has a `CHECK (... ~
'^[0-9a-f]{64}$')`. This rejects malformed data structurally — it has
nothing to do with Bitcoin/QOGE relay-policy standardness, and never
rejects genuine historical chain data (see §9 "Do not over-constrain").
Similarly, `witness_version` is constrained to `0..16` because that range
is what BIP141's version-opcode encoding can structurally represent, not a
policy choice.

## 4. Idempotency model

**Status: IMPLEMENTED in Phase 2B.2** — this section described the intended
design before any Go code existed; `internal/store.Store.ApplyBlock`
(`internal/store/apply.go`) now implements exactly this, and
`internal/store/apply_test.go` proves it against a real PostgreSQL database.
See §16 for the implementation-level detail (immutable conflict semantics,
fee computation, block ordering assumptions) this section doesn't repeat.

**Core guarantee:** re-indexing the same block, any number of times, in any
order relative to a crash, produces byte-identical database state to
indexing it exactly once — as long as the replayed data is *identical* to
what's already stored. Replaying *contradictory* data for an already-stored
identity (same txid/wtxid/block hash, different body) is a distinct case —
an error, not a silent overwrite — see §16 "Immutable conflict semantics."

Mechanisms:

1. **Natural-key uniqueness everywhere.** `blocks.hash`, `transactions.txid`,
   `transaction_variants.wtxid`, `(block_hash, tx_index)`/`(block_hash, txid)`
   on `block_transactions`, `(txid, vin_index)`, `(txid, vout_index)` on both
   `transaction_outputs` and `utxo_state` — all real primary/unique keys.
   Every immutable-identity insert uses `INSERT ... ON CONFLICT (<key>) DO
   NOTHING`, immediately followed — only if the row already existed — by a
   verification `SELECT` proving the existing row's data matches what this
   call intended to insert. A match is a safe no-op (replaying a block's
   inserts is always safe); a mismatch is `ErrImmutableConflict`, never a
   silent overwrite. See §16 "Immutable conflict semantics." The one
   genuinely mutable write, `utxo_state`'s spend marker, uses a plain
   `UPDATE ... WHERE spent = false`, which is naturally idempotent: it's a
   no-op if the row is already spent by exactly the input attempting to
   spend it again (checked before this UPDATE ever runs — see §16), and
   never reached at all if it's already spent by a *different* input
   (`ErrDoubleSpend` at the check).
2. **One SQL transaction per block.** All work for block N — inserting the
   block row, its transactions (`ON CONFLICT DO NOTHING` — the same txid may
   already exist from an earlier block), their `transaction_variants` rows,
   `block_transactions` links, inputs, outputs, witness data, `utxo_state`
   rows (new unspent outputs inserted, spent outputs updated), recomputing
   the `addresses` rows touched by this block, and finally updating
   `sync_state` — happens inside a single `BEGIN...COMMIT`
   (`Store.ApplyBlock` calls `pool.Begin` exactly once and passes the same
   `pgx.Tx` to every helper it calls). Postgres transactions are
   all-or-nothing and durable only on `COMMIT`; there is no way for "half of
   block N" to survive a crash. `ApplyBlock` processes `block.Transactions`
   in the given order and assumes it is already valid: an input may only
   reference an output created earlier in the same block or in an
   already-applied block, never later in this same block. Any block Core
   itself considers valid satisfies this; `ApplyBlock` does not re-derive or
   re-sort the order itself, and a transaction referencing a not-yet-applied
   output fails as `ErrMissingPrevout` (see §16), not a reorder.
3. **Checkpoint update is the last statement inside that same transaction,**
   not a separate one issued afterward. This directly closes the exact gap
   that corrupted eIquidus: there, the checkpoint (`coinstats.last`) and the
   balance deltas were written as unrelated MongoDB operations with no
   shared transaction, so a crash between them left a checkpoint that didn't
   match the data it was supposed to describe, and the resume logic
   (correctly, given its own assumptions) replayed the block — which was
   only safe if the writes were idempotent, and they weren't. Making
   checkpoint-and-data one atomic unit removes the need to reason about that
   gap at all. `sync_state_validate_checkpoint_trigger` (§3c) fires on this
   final `UPDATE` too, so an `ApplyBlock` call can never commit a checkpoint
   pointing at a hash that isn't a real, canonical block — a second,
   independent correctness gate right before `COMMIT`. The checkpoint is
   also only ever advanced, never rewound: `ApplyBlock` reads the current
   `indexed_height` at the start of the transaction and only issues the
   checkpoint `UPDATE` if `block.Height >= indexed_height`, so replaying an
   old, already-superseded block (a deliberate historical re-index run,
   say) cannot accidentally move the checkpoint backward.
4. **Balances are `SET`, not incremented** (see §3c's `addresses` table).
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

**Status: step 3 (the rollback itself) is IMPLEMENTED in Phase 2B.2** as
`Store.RollbackTo` (`internal/store/reorg.go`), proven against synthetic
already-persisted fork data by `internal/store/apply_test.go`'s
`TestRollbackTo_*` tests. Steps 1, 2, and 4 — detecting a fork against live
Core RPC output, walking backward to find the common ancestor, and driving
the resulting sequence of `RollbackTo` + `ApplyBlock` calls — are indexer
work, deliberately not built in this phase (no RPC translator or historical
sync loop exists yet — see §16).

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
3. **Roll back, in one transaction — `Store.RollbackTo(ctx, ancestorHash)`.**
   This is the reorg invariant this phase exists to get right: **only
   canonical *derived* state is rolled back or rebuilt — `utxo_state`,
   `addresses`, `sync_state`. Immutable bodies (`blocks`, `transactions`,
   `transaction_variants`, `block_transactions`, `transaction_inputs`,
   `transaction_input_witness`, `transaction_outputs`) are never deleted.**
   `RollbackTo` finds every currently-canonical block above the ancestor's
   height in one query — the schema's "exactly one canonical block per
   height" invariant (`blocks_height_canonical_uidx`) guarantees this set IS
   the orphaned range, no `prev_hash` walk needed inside the store itself —
   then, for that whole set at once:
   - Marks every block in it `canonical = false`, `orphaned_at = now()`.
     The row stays — this is the audit trail.
   - `transactions`, `transaction_variants`, `transaction_inputs`,
     `transaction_input_witness`, and `transaction_outputs` for those
     blocks' transactions are left completely alone. `block_transactions`
     is left alone too — it's the historical record of "this exact
     (txid, wtxid) variant was once in this (now-orphaned) block," which
     remains true forever, and is exactly what lets a later replacement
     block reuse the same txid under a *different* wtxid without either
     occurrence overwriting the other (§3a, §16).
   - For every output *created* in that set (found via
     `utxo_state.creation_block_hash`): **deletes its `utxo_state` row** —
     an output whose creating block isn't canonical doesn't currently exist
     on the canonical chain, so there is no canonical spend state left to
     describe. Its `transaction_outputs` row is untouched.
   - For every output *spent* by a transaction in that set (found via
     `utxo_state.spending_block_hash`): restores it (`spent = false`,
     `spending_txid = NULL`, `spending_vin_index = NULL`,
     `spending_block_hash = NULL`, `spending_block_height = NULL`) — a
     no-op for a row already deleted in the previous step (i.e. it was also
     created within the orphaned range).
   - Recomputes `addresses` rows for every address touched by either
     change above, using the exact same set-based aggregate (§16) normal
     `ApplyBlock` indexing uses — safe and correct by the same idempotency
     argument as §4. An address left with zero remaining canonical activity
     has its cache row deleted rather than left as a phantom zero entry.
   - Sets `sync_state` to the ancestor's hash as the final write; the
     `sync_state_validate_checkpoint_trigger` re-verifies it's canonical
     (it always is — it was never in the orphaned set) and re-derives
     `indexed_height`.
4. **Resume** normal linear indexing from H+1 using Core's now-canonical
   chain, via ordinary `ApplyBlock` calls. If a block being (re-)applied
   here contains a transaction whose `txid` already has a `transactions`
   row (because it also appeared in the orphaned branch), that row is
   reused as-is via the idempotent-insert path — only a new
   `transaction_variants` row (if the witness differs — a new wtxid),
   `block_transactions` link, and fresh `utxo_state` row are needed, not a
   re-parse.

**Depth safety valve (resolved — see §13.B):** reorgs of depth ≤ 100 blocks
roll back automatically via the procedure above; a detected reorg deeper than
100 blocks halts indexing before any rollback and requires manual review.
`RollbackTo` itself has no depth limit or opinion on this policy — it rolls
back to whatever ancestor it's given, of any depth. The 100-block threshold
is enforced by the future indexer, which can compute depth as
`Tip().Height - ancestorHeight` before deciding whether to call
`RollbackTo` at all; `Store` deliberately exposes just enough (`Tip`) for
that decision without owning the policy loop itself.

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
  `SELECT SUM(o.value_satoshis) FROM transaction_outputs o JOIN utxo_state u
  USING (txid, vout_index) WHERE NOT u.spent AND o.script_type !=
  'nulldata'` (spend state lives in `utxo_state`, not on
  `transaction_outputs` itself — see §3b). This is `total_issued_supply`
  minus everything provably burned (`nulldata`/OP_RETURN outputs, which are
  consensus-unspendable) minus nothing else — spent-and-respent value doesn't
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
  set `script_type = 'p2qpk'` (rather than leaving it classified as
  `unknown_witness`). There is no separate `is_p2qpk` flag to keep in sync
  with that decision — see §3c.
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

**Phase 2B.1 update: this is now database-enforced, not just an
application convention.** `migrations/0001_initial.up.sql` adds a
`BEFORE INSERT OR UPDATE` trigger on each table:
`output_addresses_reject_multisig` raises an exception if the referenced
output's `script_type = 'multisig'`; `output_participants_require_multisig`
raises one if it isn't. Confirmed by
`TestInvariant_MultisigParticipantsNeverCreateDestinationRows`
(`internal/store`): inserting participant rows for a multisig output
succeeds, and a subsequent attempt to insert a destination row for that
same output is rejected by Postgres itself.

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

## 15. Migrations and Go database tooling (Phase 2B.1)

**File layout:** `migrations/NNNN_name.up.sql` / `migrations/NNNN_name.down.sql`
at the repository root — plain, numbered, reviewable SQL files, no
templating or code generation. `0001_initial` is the schema described in
§3.

**Migration runner: a small hand-written Go package
(`internal/store/migrate.go`), not a third-party framework.** It does
exactly four things: load `.up.sql`/`.down.sql` pairs from a directory
(`LoadMigrations`), track applied versions in a `schema_migrations` table
the runner creates itself if missing, apply not-yet-applied migrations in
order (`Up`), and roll back the N most recent in reverse order (`Down`) —
each migration's DDL and its `schema_migrations` bookkeeping row committed
together in one transaction, so a failure partway through never records a
migration as applied unless it fully succeeded. `LoadMigrations` refuses to
load a migration that has an `.up.sql` but no matching `.down.sql` — every
migration in this repository must be reversible by construction, not by
discipline.

Why not `golang-migrate/migrate` or a similar framework: this project
targets exactly one database engine (PostgreSQL) and needs exactly four
operations. A general multi-database migration framework's main value —
pluggable source/database drivers, multiple SQL dialects, CLI packaging —
isn't needed here, and every dependency added is something a reviewer has
to trust. ~250 lines that a reviewer can read start-to-finish in a few
minutes is a better fit for a project whose whole premise is auditability
than an opaque well-tested-elsewhere dependency would be. If Phase 2B.2 or
later needs migration features this runner doesn't have (e.g. concurrent-
safe advisory locking for multi-instance deployments), that's a reason to
revisit this decision explicitly then, not to guess at it now.

Exposed via the CLI: `qoge-explorer migrate up`, `migrate down [n]`,
`migrate version` (`cmd/qoge-explorer/main.go`), reading `QOGE_DATABASE_URL`
and `QOGE_MIGRATIONS_DIR` (default `./migrations`).

**Go database driver: `github.com/jackc/pgx/v5`, via `pgxpool` for
connection pooling.** Chosen over `database/sql` + `lib/pq` per task
guidance to prefer a well-maintained PostgreSQL-native driver: pgx maps
PostgreSQL types (including `BYTEA`, `TIMESTAMPTZ`, arrays) directly
without `database/sql`'s generic-driver abstraction layer in the way, and
`pgxpool` is the connection-pooling story the future write-heavy indexer
(Phase 2B.2) will want regardless. Pinned to **v5.7.4** specifically — not
"latest" — because v5.7.5 and later require Go ≥ 1.23 while this repository's
toolchain is go1.22.2 (`go.mod`); v5.7.4 is the newest release still
compatible with that toolchain and was a deliberate, checked choice, not an
accident of dependency resolution.

**Migration checksum drift detection.** `schema_migrations` records
`up_checksum`/`down_checksum` (SHA-256 of `UpSQL`/`DownSQL`) alongside each
applied migration's version and name. `VerifyChecksums` runs at the start
of both `Up` and `Down`, before anything else, and fails loudly — naming
the exact version — if:

- an applied version has no corresponding entry in the loaded migrations
  at all (its `.sql` files were deleted from the repository — migration
  history is append-only, so this must never silently become acceptable
  just because other migrations still exist);
- the applied name no longer matches the loaded name (a rename);
- the applied `UpSQL` checksum no longer matches (the file was edited
  after being applied — the live schema may no longer match it); or
- the applied `DownSQL` checksum no longer matches (a future rollback
  would run something that's no longer the correct inverse of what was
  actually applied).

Only `UpSQL` was checksummed in the second review round; a third-round
finding pointed out that a silently-edited `DownSQL` was just as dangerous
(a future `Down()` would run the wrong rollback) and went completely
undetected. Both directions are covered now.

Only the minimal connection/migration plumbing was added in Phase 2B.1
(`store.Connect`, `store.Up`/`Down`/`LoadMigrations`/`CurrentVersion`/
`VerifyChecksums`) — the block-indexing write API (`INSERT ... ON CONFLICT`
per block, UTXO/address maintenance, reorg rollback execution) is
Phase 2B.2, documented next in §16.

## 16. Store: block-application engine (Phase 2B.2)

**Status: IMPLEMENTED.** `internal/store.Store` (`internal/store/store.go`,
`apply.go`, `reorg.go`) persists already-parsed `chain.Block`/
`chain.Transaction` data into the schema §3 locks down. It is deliberately
small and explicit — `New`, `ApplyBlock`, `Tip`, `RollbackTo`, `GetUTXO` —
not an ORM or generic repository, and knows nothing about Qogecoin Core RPC
or historical synchronization: it consumes canonical `chain.*` values only,
never a raw `map[string]any` RPC object. RPC decoding (translating Core's
JSON into `chain.Block`/`chain.Transaction`) and the continuous block-fetch
loop are the next phase, built on top of this one. Proven by
`internal/store/apply_test.go` and `internal/store/continuity_test.go`
against a real, per-test-isolated PostgreSQL database (same infrastructure
as §3's invariant tests) — single-block atomicity, idempotent replay, every
immutable-conflict shape (including child-set completeness, below), UTXO
spend/double-spend/missing-prevout, fee computation, multisig
non-aggregation, reorg down to multi-block depth, canonical tip continuity,
safe orphan re-promotion, canonical-mutation concurrency, Core-equivalent
UTXO semantics (genesis and `IsUnspendable` exclusion, below), and coinbase
structural consistency — plus a real-mainnet-vector exercise reusing the
P2PK/P2PKH/OP_RETURN/P2WPKH/P2TR scriptPubKeys already documented in
`internal/script/classify_test.go`, and a source-derived genesis
identity/UTXO-semantics fixture (real block hash/txid/reward/version/
scriptPubKey, synthetic coinbase input script — see the test's doc
comment) for the genesis exclusion specifically.

### Canonical tip continuity, and safe orphan re-promotion

**Added in an independent review round, after the behavior above was first
implemented.** `ApplyBlock`'s first statement is `lockCheckpoint`: a
`SELECT ... FOR UPDATE` against `sync_state('main')`, held for the whole
transaction. This is the canonical-mutation lock — it fully serializes
every `ApplyBlock`/`RollbackTo` call against every other one, across
goroutines and across separate `Store` instances/processes sharing the
database (`TestApplyBlock_ConcurrentCompetingChildrenSerialize`,
`TestRollbackTo_SerializesAgainstApplyBlock`).

Once locked, `block` must have an explicit, provable relationship to the
checkpoint just read (`checkCanonicalContinuity`) — for an initialized
store, exactly one of **exact tip replay** (same hash and height as the
checkpoint) or **immediate canonical append** (height = checkpoint height +
1, `PreviousHash` = checkpoint hash). Anything else — a height jump, a
mismatched `PreviousHash`, or any block (canonical or orphaned) below the
current tip — is `ErrNonSequentialBlock`, rejected before any write. A
lower historical block can never mutate canonical UTXO/address state
merely because `ApplyBlock` was called on it. For an *uninitialized* store
(`sync_state` still at its bootstrap `-1`/`NULL` row), any block is
accepted as this store's bootstrap point — there is no existing tip to
compare against yet; production historical sync always starts from genesis,
and this codebase's own tests use arbitrary synthetic "genesis" blocks at
arbitrary heights, both of which this policy keeps working.

If `block.Hash` refers to an already-persisted block that is currently
orphaned (`canonical = false`, from an earlier `RollbackTo`) and it passes
continuity as the immediate child of the current tip, `applyBlockHeader`
promotes it (`canonical = true`, `orphaned_at = NULL`) instead of treating
the pre-existing, header-matching row as a plain no-op. Its canonical
UTXO/address state is then rebuilt by the ordinary per-transaction path —
`RollbackTo` only ever deleted its `utxo_state` rows, never its
transactions/inputs/outputs/witness rows, so "rebuild" is just those rows'
normal idempotent-insert path recreating what was removed
(`TestApplyBlock_OrphanRePromotion`). An orphan that is *not* the immediate
child of the current tip is never promoted — it's rejected by the
continuity check first, before `ApplyBlock` even looks at its `canonical`
flag.

### Immutable child-set completeness

**Also added in that review round.** `insertOrVerifyIdempotent` alone
proves an individual row's *content* is immutable, but not that a whole
child-row *set* is complete: a second observation of an already-known txid
that supplies an extra output at a previously-unused `vout_index`, or an
extra witness item at a previously-unused `item_index`, would insert
cleanly on its own natural key, silently growing what was supposed to be
an immutable transaction body. `insertOrVerifyIdempotent` now returns
whether the parent identity (block hash, txid, wtxid, or a specific
output's `(txid, vout_index)`) was freshly inserted or already existed. A
freshly-inserted parent guarantees its children are freshly inserted too —
nothing could have written them before the parent existed — so no further
check is needed. An *already-existing* parent triggers `verifyExactCount`
(`store.go`): a `SELECT count(*)` against the child table, which must
exactly equal the newly-supplied child count, run *before* any child rows
for this call are touched. Applied to: `transaction_inputs`/
`transaction_outputs` per txid, `transaction_input_witness` per
`(wtxid, vin_index)` — checked per-input, not aggregated across the whole
wtxid, so a witness stack "moved" between two inputs (same total count,
different distribution) is still caught — `block_transactions` per
`block_hash`, and `output_participants` per output.

This relies on one more structural rule, checked up front by
`validateBlockShape` before continuity or any database access at all:
`chain.Input.Index`/`chain.Output.Index` must equal the element's position
in its array (Core's own vin/vout array semantics — see
`internal/chain/input.go`/`output.go`), which makes an "index moved to
another slot" attempt structurally inexpressible, and reduces the
completeness check for indexed children to a simple, cheap count
comparison rather than a full index-set comparison. `applyOutput` closes
two related shape gaps the same way: a replay that supplies no `Address`
for an output that previously had one is a conflict, not a silent skip
(`out.Address == ""` no longer means "say nothing"); and a multisig
output's `ParticipantAddresses` must have exactly one entry per `PubKeys`
entry, none empty — never silently tolerating a partial/missing mapping.

None of this computes or validates txid/wtxid cryptographically — that
remains explicitly out of scope (representation-integrity checking, not
consensus validation).

`markSpent`'s `UPDATE ... WHERE spent = false` no longer assumes success
just because `checkPrevout` earlier observed `spent = false`:
`RowsAffected` is inspected directly, and a zero-row result triggers a
re-read that distinguishes "already spent by exactly this input"
(idempotent replay — safe) from any other spender (`ErrDoubleSpend`),
rather than silently treating zero affected rows as success
(`TestMarkSpent_ZeroRowUpdateDetectsConflict`). Fee accumulation
(`addChecked`) uses checked `int64` addition, returning
`ErrAmountOverflow` rather than silently wrapping — real QOGE values never
approach this range, but the possibility is kept structurally impossible
rather than merely improbable.

### Core UTXO semantics: `transaction_outputs` vs. `utxo_state`

**Added in a second, Core-facing independent review round**, after the
continuity/completeness round above. §3b already documents that
`transaction_outputs` and `utxo_state` are deliberately separate tables;
this round closes a gap in *which* outputs `utxo_state` gets a row for at
all:

- `transaction_outputs` = **every** output that ever existed on-chain,
  preserved 1:1, forever — unconditionally, regardless of anything below.
- `utxo_state` = the Core-*equivalent* canonical coin state — a row exists
  **only** for an output Core's own `CCoinsViewCache::AddCoin` would
  actually add to its coins view. It is not, and was never intended to be,
  a mirror of `transaction_outputs`.

`applyOutput` (`apply.go`) skips `utxo_state` row creation — but still
writes `transaction_outputs` (and any destination/participant metadata)
unconditionally — for exactly two categories, both taken directly from
Qogecoin Core source:

1. **Genesis (block height 0).** Core's `ConnectBlock` special-cases the
   genesis block, skipping connection of its transactions entirely ("its
   coinbase is unspendable"); QOGE's own chainparams document the same for
   the genesis output specifically — it never existed in the coins database
   at all. `ApplyBlock` derives `isGenesis := block.Height == 0` and threads
   it down to `applyOutput`. `TestApplyBlock_GenesisCoinbaseUnspendable`
   proves this against a source-derived fixture — the real QOGE mainnet
   genesis block hash, txid, coinbase reward (100 QOGE), transaction
   version (1), and genesis output scriptPubKey (bare P2PK; the coinbase
   input's raw script is a synthetic placeholder, not reproduced from
   source — see the test's doc comment): the transaction and its output
   persist exactly (including the real scriptPubKey round-tripping through
   `script.Classify` as `TypeP2PK`), but `GetUTXO` is `nil` and no
   `addresses` cache row is created from it.
2. **`script.IsUnspendable`.** Mirrors Core's `CScript::IsUnspendable()`
   (`src/script/script.h`) exactly: `(len(script) > 0 && script[0] ==
   OP_RETURN) || len(script) > script.MaxScriptSize` (10,000, Core's
   `MAX_SCRIPT_SIZE`) — a structural, byte-level check on the raw
   `scriptPubKey`, deliberately independent of `script.Type`/`Classify`: a
   non-`nulldata` script larger than `MaxScriptSize` is unspendable too,
   even though `Classify` has no dedicated "too large" type for it. Applies
   at every height, not just genesis. `TestApplyBlock_UnspendableOutputs`
   covers an ordinary spendable output (UTXO created), an `OP_RETURN`
   script (output persisted, no UTXO), an oversized script (output
   persisted, no UTXO), the exact `MaxScriptSize`-byte boundary for a
   non-`OP_RETURN` script (still spendable — hitting the boundary alone
   must not reject it), and that safe orphan re-promotion's rebuild path
   respects the exclusion too, not just plain tip replay.

Neither exclusion touches `transaction_outputs`, `output_addresses`, or
`output_participants` — an excluded output remains fully queryable audit
history. The exclusion only ever affects `utxo_state` (so it can never be
spent — attempting to reference it as a prevout is `ErrMissingPrevout`,
exactly as if it didn't exist, which is structurally correct: Core would
reject spending it too) and, transitively, `recomputeAddress` (which reads
`utxo_state` via an inner join — an address whose only activity is an
excluded output gets no `addresses` cache row at all, not a zero-value one).

### Transaction completeness and input field exclusivity

**Added in a third, final review round.** Two more `validateBlockShape`
checks, run before any database access, alongside the ones above:

1. **Every transaction must have at least one input and one output.**
   `ApplyBlock` claims to persist a fully decoded transaction, so an empty
   `vin`/`vout` must never be silently accepted as a possibly-partial RPC
   translation — the same completeness concern as `ErrIncompleteBlock`
   above, applied one level down. This mirrors the *shape* (not the
   reachability) of Core's `CheckTransaction` `bad-txns-vin-empty`/
   `bad-txns-vout-empty` checks; `Store` is not becoming a consensus
   validator. `ErrInvalidTransactionShape`.
2. **`chain.Input`'s `PreviousOut`/`Coinbase`/`ScriptSig` fields are
   mutually exclusive by construction** (`internal/chain/input.go`):
   `Coinbase` is set only when `PreviousOut` is `nil`; `ScriptSig` is empty
   for a coinbase input. Previously `applyInput` silently ignored
   `Input.Coinbase` whenever `PreviousOut != nil` — a raw-preservation gap
   if a future RPC decoder ever constructed an inconsistent model.
   `validateBlockShape` now requires, per input: if `PreviousOut == nil`,
   `Coinbase` must be non-empty and `ScriptSig` must be empty; if
   `PreviousOut != nil`, `Coinbase` must be empty (`ScriptSig` may
   legitimately be empty *or* non-empty either way — a pure witness spend
   has no `ScriptSig`, and that alone is never rejected).
   `ErrInvalidTransactionShape`.

`TestApplyBlock_TransactionCompleteness` covers: a coinbase with zero
outputs (rejected, zero writes); a non-coinbase with zero inputs (rejected,
zero writes, checkpoint unmoved); a transaction with normal vin/vout
(accepted). `TestApplyBlock_InputFieldExclusivity` covers: a normal
coinbase representation (accepted); a coinbase with a non-empty `ScriptSig`
(rejected); a coinbase with missing/empty `Coinbase` bytes (rejected); a
non-coinbase with `Coinbase` bytes populated (rejected); and a pure-witness
non-coinbase with an empty `ScriptSig` (accepted).

### Coinbase structural consistency

**Also added in that review round.** `Store` uses `chain.Transaction
.IsCoinbase` to make two monetary decisions: skip fee computation, and skip
prevout spend marking. Previously this flag was trusted independently of
the transaction's actual input shape — a caller-constructed
`IsCoinbase = true` transaction carrying a real `PreviousOut` would silently
skip marking that prevout spent while still creating the transaction's
outputs, corrupting canonical UTXO state without any error.
`validateBlockShape` now requires, for every transaction, before any
database access:

```
structurallyCoinbase := len(txn.Inputs) == 1 && txn.Inputs[0].PreviousOut == nil
txn.IsCoinbase == structurallyCoinbase
```

mirroring Core's `IsCoinBase()` (`src/primitives/transaction.h`) exactly. It
also requires canonical block-level shape — at least one transaction,
transaction 0 is coinbase, no other transaction is — matching the fact that
every real Core block has exactly one coinbase transaction, always at
position 0. Any violation is `ErrInvalidTransactionShape`, rejected before
any write. This does not replace Core consensus validation; it only ensures
the already-parsed canonical Go model is internally self-consistent before
`Store` trusts `IsCoinbase`. `TestApplyBlock_CoinbaseStructuralConsistency`
covers: a normal one-input coinbase (accepted); `IsCoinbase = true` with a
real prevout (rejected, zero writes); `IsCoinbase = false` with a null
prevout (rejected); a two-input coinbase (rejected); a block whose first
transaction isn't coinbase (rejected); a block whose second transaction is
coinbase (rejected); and a zero-transaction block (rejected).

### Block transaction ordering

`ApplyBlock` processes `block.Transactions` strictly in the given slice
order and does not re-derive or re-sort it. This is safe because it assumes
— as any block Qogecoin Core itself considers valid guarantees — that every
input references an output created either in an already-applied block or
*earlier* in this same block, never later in this same block. A
transaction that violates this (e.g. a caller-constructed test block with
inputs out of order) fails with `ErrMissingPrevout` at the point it's
processed, exactly as if the referenced output didn't exist yet — `Store`
does not attempt to detect or repair ordering problems, since a real block
from Core cannot have this problem in the first place.

### Immutable conflict semantics

Every immutable-identity table write goes through
`insertOrVerifyIdempotent` (`store.go`): `INSERT ... ON CONFLICT (<natural
key>) DO NOTHING`, and — only if that affected zero rows, meaning the key
already existed — a follow-up `SELECT` proving the existing row's non-key
columns exactly match what this call intended to insert (`IS NOT DISTINCT
FROM` throughout, so `NULL`-vs-`NULL` correctly counts as a match). A
mismatch returns an error wrapping `ErrImmutableConflict`, naming the
table/identity, and aborts the whole block (see "Atomicity," below) —
never a silent overwrite. This applies uniformly to `blocks` (keyed by
`hash`; canonical/orphaned_at are intentionally excluded from the
comparison — see §5), `transactions` (`txid`), `transaction_variants`
(`wtxid`), `block_transactions` (`block_hash, tx_index`),
`transaction_inputs` (`txid, vin_index`), `transaction_input_witness`
(`wtxid, vin_index, item_index`), `transaction_outputs` (`txid,
vout_index`), `output_addresses` (`txid, vout_index`), and
`output_participants` (`txid, vout_index, address`). `utxo_state`'s
*creation* row follows the same pattern (comparing only
`creation_block_hash`, since a genuine identity conflict there should never
occur in practice given every txid/wtxid conflict above is already caught
first) — but its *mutable* spend fields are never part of that comparison,
precisely because they're expected to change over the row's lifetime; see
"UTXO spend handling" below.

### Idempotent replay

Reapplying the exact same already-applied block calls every
`insertOrVerifyIdempotent` site with byte-identical arguments, so every one
resolves to "already exists, and matches" — a safe no-op. The one exception
requiring explicit handling is `utxo_state`'s spend marker: `checkPrevout`
(`apply.go`) treats "already spent by exactly this (txid, vin_index)" as
success (the idempotent case), and only treats "already spent by a
*different* spender" as `ErrDoubleSpend`. The subsequent `UPDATE ... WHERE
spent = false` is then a correct no-op in the idempotent case (nothing to
change) and a correct first-time spend otherwise. Address caches are
recomputed (see below) regardless of whether anything actually changed,
which is safe by construction since recomputation is a pure function of
current source-table state, not a delta.

### Fee computation

For each non-coinbase transaction, before any of its rows are written,
`applyTransaction` resolves every input's previous output via
`checkPrevout` (which also enforces the UTXO existence/unspent checks
below) and sums their `value_satoshis`; it separately sums the
transaction's own output values. `fee = sum(input values) - sum(output
values)`, computed and compared entirely in integer satoshis (`int64`) —
never `float64` anywhere in this path. A negative result is
`ErrNegativeFee`: an internal indexing inconsistency (inputs worth less
than outputs is not a state a valid block can produce), not a value this
codebase infers total issued supply from — see §6, which this does not
change. Coinbase transactions never go through this path at all;
`fee_satoshis` is unconditionally `NULL` for them, matching the schema's
`transactions_coinbase_has_no_fee` `CHECK`.

### UTXO spend handling

`checkPrevout` requires a `utxo_state` row to already exist for the
referenced `(prev_txid, prev_vout_index)` — `ErrMissingPrevout` otherwise;
`Store` never invents a missing previous output, consistent with §2's
"never invent data Core didn't provide." If the row exists but is already
spent by a different transaction/input, that's `ErrDoubleSpend`. Only once
every input of a transaction has passed this check, and that transaction's
own `transaction_inputs`/`block_transactions` rows have been written
(satisfying `utxo_state`'s occurrence foreign keys — §3b), does
`markSpent` actually flip `spent = true` on the referenced row.

### Address cache: SET recomputation, exact semantics

`recomputeAddress` (`apply.go`) is the only place `addresses` rows are ever
written, called for every address `ApplyBlock`/`RollbackTo` touched in this
call — a receiving `output_addresses` row inserted or verified, or a
`utxo_state` row this call just spent, deleted, or restored. It always
computes a fresh absolute value from current source tables and `SET`s it
(`INSERT ... ON CONFLICT (address) DO UPDATE SET <every column> =
EXCLUDED.<every column>`) — never `balance = balance + ...` or any other
increment. Exact semantics, over every `output_addresses` row naming this
address joined against its `transaction_outputs`/`utxo_state` state:

- `total_received_satoshis` = `SUM(value_satoshis)` over every output ever
  created to this address, spent or not.
- `total_sent_satoshis` = `SUM(value_satoshis)` over this address's outputs
  that are currently spent.
- `balance_satoshis` = `SUM(value_satoshis)` over this address's currently
  *unspent* outputs (equivalently `total_received - total_sent`).
- `tx_count` = count of DISTINCT transactions this address participated in,
  as either a creation (received an output) or a spend (a previously-
  received output was later spent by some transaction) — the union of
  creating txids and spending txids, not just one or the other.
- `first_seen_height` = `MIN(creation_block_height)` over this address's
  outputs.
- `last_seen_height` = `MAX` of, for each output, its creation height or —
  if spent — its spending height, whichever is higher: the highest block
  height at which any activity touching this address occurred.

Because this is a pure function of current canonical source-table state,
it is automatically correct after `ApplyBlock`, after `RollbackTo`, and
after any sequence of the two — there is no separate "undo" logic for the
address cache during a reorg, only the same recomputation rerun for
whatever addresses that reorg touched (`TestRollbackTo_*`'s address-cache
assertions in `apply_test.go` confirm the cache after a rollback-then-
replace sequence exactly matches an independent, from-scratch aggregate
query against the source tables). If recomputation finds zero remaining
canonical activity for an address (every output that ever named it has
been rolled back and never replaced), its `addresses` row is deleted
rather than left behind as a phantom all-zero entry — `output_participants`
is never part of this computation at all (§7/§13.A): a multisig output's
participants never gain or lose cache rows from it, by construction, not
by a special-cased exclusion.

### Atomicity

`ApplyBlock` and `RollbackTo` each open exactly one `pgx.Tx`
(`pool.Begin`) and pass it to every helper they call; the deferred
`tx.Rollback` is a safe no-op once `tx.Commit` has already succeeded, and
the only path to persisting anything is that final `Commit` call after
every step — including address-cache recomputation — has already
succeeded without error. Any error at any point, from a genuine
`ErrImmutableConflict`/`ErrDoubleSpend`/`ErrMissingPrevout`/
`ErrNegativeFee` down to an unexpected database error, causes the function
to return before `Commit`, so the deferred `Rollback` discards every
statement issued so far — including the block header insert itself. Task
tests P and Q (`apply_test.go`) confirm this directly: a block engineered
to fail partway through leaves neither its `blocks` row nor any of its
`transactions` rows behind, and leaves `sync_state` and every `utxo_state`
row it might have touched completely unchanged.

### Multisig participants

`output_participants` rows are written only for `script_type = 'multisig'`
outputs, using `chain.Output.ParticipantAddresses` — a caller-supplied,
parallel array to `PubKeys` (added in this phase; see
`internal/chain/output.go`) for the per-signer addresses a future RPC
translator will resolve from Core's own bare-multisig `addresses` array,
exactly as `Output.Address` is already "taken as-is" for every other type.
`Store` never derives or invents an address itself. `recomputeAddress`
never reads `output_participants` — see "Address cache" above — so writing
these rows can never affect any address's balance, by construction.

**`output_participants` is a SET of `(address, pubkey)` participant
identities, keyed by `(txid, vout_index, address)`** — added in the
Core-facing review round. A bare multisig script can structurally list the
same pubkey more than once; the raw `scriptPubKey` bytes preserve that
duplication exactly regardless of anything below. Two identical
`(address, pubkey)` entries in `ParticipantAddresses`/`PubKeys` are the
*same* participant identity, not two — `applyOutput` deduplicates them
before persistence and before completeness counting (`verifyExactCount`
now compares against the deduplicated count, not `len(PubKeys)`), so an
exact replay of a duplicate-participant output remains idempotent
(previously it did not: a fresh apply persisted 1 row via the second
insert's harmless idempotent no-op against the same natural key, while a
replay's completeness check compared the persisted count against the
un-deduplicated `len(PubKeys)`, spuriously failing —
`TestApplyBlock_MultisigDuplicateParticipantReplay`). The *same* address
claimed with two *different* pubkeys is a genuine identity conflict, not a
duplicate, and remains `ErrImmutableConflict`
(`TestApplyBlock_MultisigSameAddressDifferentPubkeyConflict`).
