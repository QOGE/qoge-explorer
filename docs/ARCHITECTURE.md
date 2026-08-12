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
   independent correctness gate right before `COMMIT`. The checkpoint can
   only ever be re-affirmed or advanced by exactly one block, never rewound
   or skipped ahead: `ApplyBlock`'s first statement is `lockCheckpoint`, a
   `SELECT ... FOR UPDATE` against `sync_state('main')` that both reads the
   current checkpoint and serializes every concurrent `ApplyBlock`/
   `RollbackTo` call against it (across goroutines and separate `Store`
   instances/processes alike). For an already-initialized store, `block`
   must then be either an exact replay of that checkpoint (same hash and
   height — a harmless no-op re-affirmation) or its immediate canonical
   child (height = checkpoint height + 1, `PreviousHash` = checkpoint
   hash); every other relationship — a height jump, a mismatched
   `PreviousHash`, or any block, canonical or orphaned, below the current
   tip — is rejected as `ErrNonSequentialBlock` before any write, so a
   stale historical or orphaned block can never move the checkpoint
   backward or sideways merely because `ApplyBlock` was called on it. Once
   that relationship is proven, the checkpoint update is unconditional — no
   further height comparison is needed, since continuity already performed
   the only comparison that matters. See §16 "Canonical tip continuity, and
   safe orphan re-promotion" for the full detail, including the one
   deliberate exception (an uninitialized store accepts any block as its
   bootstrap point) and safe orphan re-promotion.
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
  `(wtxid, vin_index, item_index)` — witness-*variant*-specific, since
  witness data is exactly what differs between two variants of the same
  txid (§3a/§3b) — with `txid` retained as a plain column (not part of the
  key) for the non-witness-body relation. `data BYTEA`. No JSON/array
  column on `transaction_inputs` itself — a block-listing or tx-listing
  query that joins/selects from `transactions`/`transaction_inputs` never
  touches this table.
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
never a raw `map[string]any` RPC object. Translating Core's own JSON into
`chain.Block`/`chain.Transaction` is `internal/decode`'s job, built on top
of (never modifying) this package — see §17; the continuous block-fetch
loop and live reorg detection remain a later phase, built on top of both.
Proven by
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

## 17. RPC decoder: Core JSON-RPC → canonical chain model (Phase 2C.1)

**Status: IMPLEMENTED.** `internal/decode` (`amount.go`, `block.go`,
`resolver.go`) is the strict boundary between Qogecoin Core's untyped
JSON-RPC responses (`internal/rpc`'s raw DTOs — `RawBlock`,
`RawTransaction`, `RawVin`, `RawVout`, `RawScriptPubKey`, `internal/rpc/
block.go`) and the canonical `chain.Block`/`chain.Transaction` model
`Store.ApplyBlock` consumes (§16). `internal/rpc` gained exactly the typed
methods this needs — `GetBlockHash`, `GetBlockVerbose2`,
`GetRawTransactionVerbose`, `GetDescriptorInfo`, `DeriveAddresses` —
without growing into a wrapper around every RPC Core exposes; the generic
`Call`/`CallInto` transport (`client.go`) is unchanged. Neither
`internal/chain`, `internal/script`, `internal/store`, nor
`migrations/0001_initial.*` were modified to build this boundary — every
one of those was already a frozen, independently-reviewed contract by the
time this phase began, and this decoder was built strictly as their
consumer.

Proven by `internal/decode/*_test.go` — deterministic, no live node
required for a normal `go test ./...` run — plus a decoder→`Store`
pipeline test against real, per-test-isolated PostgreSQL
(`store_test.go`), and opt-in live-node integration tests
(`integration_test.go`, `QOGE_RPC_INTEGRATION=1`) exercised against the
project's own reference node during this phase.

This is a representation boundary, not a second Qogecoin consensus
validator — the same posture §16 establishes for `Store`, and
`internal/decode` inherits it exactly: no proof-of-work verification, no
merkle recomputation, no ECDSA/SLH-DSA signature verification, no script
execution. The structural checks it does perform exist only to reject a
partial or malformed RPC response before it becomes a `chain.Block` that
`Store` would otherwise have to reject with less context (a missing txid,
an empty vin/vout, a coinbase input mixed with a real one, an `nTx`/
transaction-count mismatch, and so on).

### Exact decimal amount conversion

`DecodeAmount` (`amount.go`) converts Core's decimal-QOGE JSON number
(e.g. `"6.25000000"`) into exact integer satoshis without ever passing the
value through `float64`/`float32`. `internal/rpc.RawVout.Value` is
`json.Number` — Go's `encoding/json` decodes a JSON number token into
`json.Number` as its original decimal *text*, not a float, whenever the
destination field has that type — and `DecodeAmount` parses that text with
pure integer/string arithmetic: an optional sign, an integer part, an
optional `.` and up to 8 fractional digits, padded to exactly 8 and
concatenated into a satoshi integer via `strconv.ParseInt`. Rejected
outright, never rounded or silently substituted: a malformed number, a
negative value, more than 8 fractional digits (unrepresentable exactly in
satoshis — this package never rounds), and `int64` overflow.
`amount_test.go` covers the required boundary/precision cases directly:
`0` → `0`, `0.00000001` → `1`, `1` → `100000000`, `6.25` → `625000000`,
`100` → `10000000000`, plus malformed/negative/9-fractional-digit/overflow
rejection.

### txid vs. RPC hash (wtxid)

Core's verbose transaction JSON uses `"txid"` for the non-witness
transaction id and, confusingly, `"hash"` for the *witness* transaction id
(`internal/rpc.RawTransaction.Hash` — not a block hash). `DecodeTransaction`
maps these straight across — `raw.TxID` → `chain.Transaction.TxID`,
`raw.Hash` → `chain.Transaction.WTxID` — never deriving, recomputing, or
swapping either one; `Store` already independently enforces that
`WTxID == TxID` iff the transaction carries no witness data (§16), so the
decoder's only job is preserving Core's own reported values exactly. Both
are validated as syntactically well-formed (64 lowercase hex characters,
matching every hash-shaped column's schema `CHECK` constraint — §3) before
being handed to `Store`; uppercase is deliberately *rejected*, not
normalized — there is no confirmed case of Core legitimately emitting it
for these fields, and silently accepting it would risk masking genuinely
malformed data.

### Coinbase and ordinary input mapping

Core reports exactly one of two mutually exclusive vin shapes;
`internal/rpc.RawVin.Coinbase` is a `*string` specifically so the decoder
can tell "field present" (coinbase) from "field absent" (ordinary) by
presence, matching Core's own discriminator, rather than guessing from
content. A coinbase input decodes to `PreviousOut = nil`,
`Coinbase = <hex-decoded bytes>`, `ScriptSig = nil` — Core's coinbase bytes
are never placed into `ScriptSig`. An ordinary input decodes to
`PreviousOut = {TxID, Index}` from `vin.txid`/`vin.vout`, `Coinbase = nil`,
`ScriptSig = <hex-decoded scriptSig.hex>` (legitimately empty for a pure
witness spend — preserved as empty, never rejected or synthesized).
`DecodeTransaction` additionally requires at most one coinbase-shaped input
per transaction and, if present, that it be the transaction's *only*
input — catching a malformed RPC object here rather than deferring to
`Store`'s own, later structural-coinbase check (§16 "Coinbase structural
consistency").

### Witness mapping

Every `txinwitness` hex string is hex-decoded independently into
`chain.WitnessStack`, preserving item order, zero-length items (an empty
hex string decodes to a genuine zero-length `[]byte`, not a dropped entry),
and arbitrary length — up to and including a full 17,088-byte P2QPK
signature, verified byte-exact end to end by
`TestDecodeInput_P2QPKWitnessSpendVector` and, through a real
`Store.ApplyBlock` call, `TestDecodeBlock_ThroughStore_PipelineTest`.
Witness items are never concatenated or truncated.

### Structural classification precedence over RPC type

`decodeOutput` calls `script.Classify` on the raw, hex-decoded
`scriptPubKey` bytes and uses *only* that result for
`chain.Output.ScriptType`/`WitnessVersion`/`WitnessProgram`/`PubKeys`.
Core's own `scriptPubKey.type` string (`internal/rpc.RawScriptPubKey.Type`)
is retained on the raw DTO for diagnostics/tests only and is never read by
the decoder — confirmed directly by
`TestDecodeOutput_RPCTypeNeverDrivesClassification` (a script whose RPC
type lies is still classified from its real bytes) and
`TestDecodeOutput_UnknownWitnessNeverUpgradedToP2QPK`/
`TestDecodeOutput_RealVector_P2TR` (neither a generic witness-unknown
program nor a real Taproot output is ever misclassified as P2QPK). This is
exactly what correctly turns Core's own generic `"witness_unknown"` report
for a real P2QPK (witness v2, 32-byte program) output into
`script.TypeP2QPK`.

### Core-reported address handling

For every script type except bare P2PK and bare multisig,
`chain.Output.Address` is Core's own `scriptPubKey.address` field, copied
as-is — including the generic witness-unknown address Core reports for a
structurally-P2QPK output. No Bech32m encoder, no address derivation from
the public key: the P2QPK witness program is `HASH256(pubkey)`, not the raw
key, so nothing here could derive it correctly even if it tried.
`TypeNullData`/`TypeUnknown`/unrecognized-witness outputs simply inherit
whatever Core did (in practice, did not) report for them — never invented.

### P2PK / multisig descriptor resolution

Core deliberately omits an address for bare P2PK and bare multisig outputs.
`AddressResolver` (`resolver.go`) is the abstraction the decoder uses
instead — `CoreAddressResolver` calls
`getdescriptorinfo("pkh(<pubkey hex>)")` for the canonical,
checksum-appended descriptor, then `deriveaddresses(<canonical
descriptor>)`, requiring exactly one address back; resolved
`(pubkey → address)` pairs are memoized in-memory for the resolver's
lifetime (`TestCoreAddressResolver_MemoizesPerPubKey` proves repeated
resolutions of the same pubkey issue the descriptor RPC round-trip once).
For a P2PK output, the resolved address is stored in `Output.Address` while
`ScriptType` stays `p2pk` — never relabeled `p2pkh`. For a multisig output,
every participant pubkey is resolved the same way into
`Output.ParticipantAddresses`, positionally parallel to `PubKeys`
(duplicate pubkeys preserved here, never deduplicated — `Store` already
applies identity-set deduplication at persistence, §16 "Multisig
participants"); if any single participant fails to resolve, the whole
output is rejected rather than silently shortening `ParticipantAddresses`
into a shape `Store` would reject. Decoder unit tests use a deterministic
in-memory fake (`decode_test.go`'s `fakeResolver`) and never require a
running `qogecoind`; `CoreAddressResolver` itself is exercised against an
in-process fake JSON-RPC server (`fakerpc_test.go`) and, opt-in, against
the project's real reference node. QOGE's Base58/Bech32 address version
bytes are never hardcoded anywhere in this package — Core's own descriptor
machinery remains the sole authority on what an address derives to, exactly
as it already is for every address the decoder just copies from Core's RPC
output.

### Real mainnet vectors

`vectors_test.go` exercises the real QOGE genesis block hash/txid/reward/
version/scriptPubKey (P2PK; cross-checked against a locally-running
reference node during this phase — see `integration_test.go`'s
`TestLiveRPC_GenesisBlockDecodesAgainstRealNode`), plus block 1 (P2PK),
block 8,000 (P2PKH), block 38,393 (OP_RETURN), height 494,289 (P2WPKH), and
height 1,284,510 (P2TR) — the same documented vectors
`internal/script/classify_test.go` and `internal/store/apply_test.go`'s
`TestApplyBlock_RealMainnetFixtures` already establish, with scriptPubKey
hex, txid, and decimal value all cross-checked live against the reference
node during this phase (`TestLiveRPC_KnownVectorsMatchLiveNode` re-verifies
this on demand, opt-in). No real P2QPK spend exists on-chain yet; the P2QPK
vector is explicitly synthetic — a structurally exact `OP_2 <push 32>
<32-byte program>` output and a 17,088-byte + 32-byte witness spend — never
presented as a captured real transaction.

### Live RPC integration tests (opt-in)

`integration_test.go` exercises a real, currently-running Qogecoin Core
node — genesis decode, P2PK descriptor resolution, a repeatable re-check of
every hardcoded real-mainnet vector against live chain data, and a decode
of whatever the live tip block currently is. Every test there calls
`requireRPCIntegration` first, which skips unless `QOGE_RPC_INTEGRATION` is
set — a normal `go test ./...` run therefore never needs a running node,
RPC credentials, or network access. The RPC username/password/Authorization
header are never printed anywhere in this file; only
`config.RPCConfig.Redacted()`'s fixed placeholder is ever logged.

### What Phase 2C.1 does NOT include

This phase ends at `chain.Block`/`chain.Transaction` in memory. There is no
continuous historical sync loop, no live fork/reorg detection against a
moving chain tip, and nothing here calls `Store.ApplyBlock` from a
production fetch loop — `TestDecodeBlock_ThroughStore_PipelineTest` is a
one-shot pipeline proof (decode → apply → assert), not a poller. Those
remain a later phase, built on top of this decoder once it has been
independently reviewed.

## 18. Indexer: historical sync + live reorg orchestration (Phase 2C.2)

`internal/indexer` is orchestration only — it coordinates the already-
reviewed pipeline (`rpc.Client` → `decode.DecodeBlock` → `store.Store`) into
a production fetch/apply loop. It parses no SQL, classifies no scripts,
and decodes no raw Core JSON itself. It does not redesign the schema,
change `Store.ApplyBlock`/`RollbackTo` semantics, or implement a public
API/UI/mempool/wallet — those remain out of scope.

### `Store.Tip` is the sole durable resume point

The database checkpoint is exactly `sync_state('main')`, read via
`Store.Tip`. The indexer never adds a second checkpoint (no JSON resume
file, no LevelDB marker, no in-memory-only durable height). On every
restart — clean or crashed — the indexer's only source of "where was I"
is `Store.Tip()`.

### Fresh production sync always starts at genesis

If `Store.Tip().Height == -1` (an uninitialized store), the indexer's
forward-sync loop starts at `local.Height + 1 == 0` — height 0, genesis —
unconditionally. `Store.ApplyBlock` itself still permits an arbitrary
bootstrap height for this codebase's own synthetic tests (see §16,
"Canonical tip continuity"); the indexer never exercises that freedom in
production. This guarantees the explorer's history is always complete
from genesis forward, never starting mid-chain at whatever height Core
happens to currently report as its tip.

### Sequential fetch/decode/apply pipeline, with a race-closing recheck

For each height `H`, in order:

1. `hash1 := rpc.GetBlockHash(H)`
2. `raw := rpc.GetBlockVerbose2(hash1)`
3. `block := decode.DecodeBlock(raw)`
4. verify `block.Height == H && block.Hash == hash1`
5. `hash2 := rpc.GetBlockHash(H)` (a **second**, independent call)
6. require `hash2 == hash1` before calling `ApplyBlock` at all
7. `store.ApplyBlock(block)`

Step 5/6 closes the race where Core reorganizes between step 1 and the
point `ApplyBlock` would otherwise commit: fetching/decoding an
already-orphaned block is wasted work, but *applying* one would let a
block that's no longer on Core's active chain reach `Store.ApplyBlock`.
If `hash2 != hash1`, the stale block is discarded — this is
`ErrRemoteChainMoved`, an internal control-flow sentinel, never a
terminal error — and reconciliation restarts from the top rather than
being treated as corruption. Blocks are always applied one at a time, in
height order; a failed height is never skipped, and PostgreSQL writes are
never batched across blocks — the reviewed invariant "one SQL transaction
per block" is unchanged.

### Local-tip/Core-tip reconciliation, every pass

Every `SyncToTip` pass begins by reconciling the local checkpoint against
Core before any forward syncing: if `local.Height <= remoteHeight` and
Core's hash at `local.Height` equals the local tip's hash, the local tip
is confirmed still on Core's active chain and forward sync may proceed
from `local.Height + 1`. Any other case — a hash mismatch at that height,
or the local tip being taller than Core's current tip at all (a "tip
retreat") — triggers common-ancestor discovery.

### Common ancestor discovery

Ancestor discovery compares the two actual canonical chains by height,
walking downward from `min(localTip.Height, coreTipHeight)`, using the one
minimal read-only Store API this phase adds:

```go
func (s *Store) CanonicalBlockHash(ctx context.Context, height int64) (hash string, found bool, err error)
```

`found == false` means no canonical row at that height (e.g. above the
current tip) — a normal, non-error outcome. If local canonical data is
unexpectedly *missing* at a height `<= local tip`, that is
`ErrLocalChainGap`, a local integrity failure, and is never conflated with
"no match found." The first height where the local and Core hashes agree
is the common ancestor. Immediately before calling `RollbackTo`, the
ancestor's Core hash is re-read one more time; if it no longer matches the
candidate (Core moved again during the search), the candidate is
discarded via `ErrRemoteChainMoved` and reconciliation restarts — this
avoids rolling back based on a hash mixed from two different Core
branches observed at different instants.

### Reorg depth policy: ≤100 automatic, >100 halt

`MaxAutomaticReorgDepth = 100` is a code constant
(`internal/indexer/errors.go`), not an environment variable — an operator
cannot accidentally weaken it via process configuration. Depth is
`localTip.Height - ancestorHeight`. The search is bounded, not
exhaustive: if Core's own tip is already more than 100 blocks behind the
local tip, that alone proves any ancestor would violate the policy, so the
indexer halts (`ErrReorgTooDeep`) without searching at all; otherwise the
walk is bounded to `localTip.Height - 100`. `Store.RollbackTo` is never
called before this policy check. Depth exactly 100 is automatic; depth
101 halts with `ErrReorgTooDeep` and the database is left completely
unchanged (this is exercised directly — see `internal/indexer/indexer_test.go`,
items J/K/L).

### Reorg execution: `RollbackTo` + the normal `ApplyBlock` path

Once a stable ancestor within policy is found, the indexer logs the old
tip, ancestor, and depth (no secrets), calls `Store.RollbackTo(ancestorHash)`,
re-reads `Store.Tip()` to confirm it now equals the ancestor, and resumes
ordinary forward syncing from `ancestorHeight + 1`. Replacement blocks use
the exact same fetch/decode/apply pipeline as any other forward sync —
there is no second "replacement branch insertion" code path. This is
deliberate: `Store` already knows how to restore UTXOs, preserve orphan
audit history, re-promote a previously orphaned block that becomes
canonical again, recompute address caches, and preserve txid/wtxid
witness variants (§16) — the indexer does not reimplement any of that.
Branch flip-back (A→B→A) is exercised directly, confirming the old A
branch's blocks are safely re-promoted rather than reinserted, and the B
branch remains queryable orphan audit history.

### Multi-writer policy

`Store` itself is fully cross-process transaction-safe: `ApplyBlock` and
`RollbackTo` each acquire the same `sync_state('main')` row lock
(`lockCheckpoint`, §16) before touching anything, so any number of
processes sharing one database can never corrupt canonical state, race
each other into an inconsistent write, or apply two conflicting blocks at
the same height — Postgres serializes them regardless of what any
particular writer believed the checkpoint was.

**Phase 2C.2 production policy is exactly ONE active indexer orchestrator
per database.** The indexer layer — not `Store` — is where this matters:
`reorg`'s automatic-reorg depth policy (`localTip.Height - ancestorHeight
<= 100`) is *computed* against a checkpoint read moments earlier, and the
pre-`RollbackTo` local-tip recheck (above) only detects — never
atomically prevents — a second writer having changed that checkpoint in
the interim (it re-reads `Store.Tip()` and discards the decision via
`ErrRemoteChainMoved` if it no longer matches, rather than trusting a
stale snapshot). That check closes the *correctness* gap — an indexer
never rolls back based on a depth computed against a checkpoint that is no
longer current — but it does not make the depth-approval decision itself
atomic with a second writer's own concurrent mutation the way `Store`'s
internal row lock makes `ApplyBlock`/`RollbackTo` atomic with each other.
Running two indexer orchestrators against the same database concurrently
is therefore unsupported/undefined behavior for this phase: `Store`'s data
will never become inconsistent, but the two orchestrators' reorg-depth
decisions are not coordinated, and either could observe
`ErrRemoteChainMoved`/`ErrNonSequentialBlock` churn from the other's
writes rather than making forward progress.

A `pg_advisory_lock`-based singleton lease was considered as an
enforcement mechanism (it needs no schema/migration change), but the
indexer only holds a `*store.Store`, which deliberately exposes no raw SQL
execution surface (§16) — implementing one cleanly would mean either
adding new `Store` API surface beyond `CanonicalBlockHash`, or having the
indexer open a second, independent database connection outside `Store`
entirely. Both are more than this phase's minimal read-only Store addition
was scoped for; enforcing single-orchestrator as a hard guarantee (rather
than an operational policy) is left for a later phase. For now: run
exactly one `qoge-explorer index` process per database.

### Sync target moving forward

A `SyncToTip` pass snapshots `target := rpc.GetBlockCount()` and
sequentially forward-syncs to it, but does not return "caught up" just
because that snapshot was reached: it loops back to reconcile again and
re-read the current tip height, continuing if Core advanced further or
reorganizing if Core changed branch. This is what lets historical
catch-up transition naturally into live operation, and correctly handles
Core continuing to produce blocks throughout a long historical sync.

### Live loop

`Indexer.Run(ctx)` is a thin loop: sync/reconcile to Core's current tip,
wait `QOGE_INDEX_POLL_SECONDS` (default 10), repeat, until `ctx` is
cancelled. `SyncToTip` calls never overlap — a second concurrent call
fails fast with `ErrSyncInProgress` rather than blocking or queuing. A
chain-moved race is retried immediately inside `SyncToTip` itself, never
surfaced to `Run`, so it's never delayed by a full poll interval. A
deterministic decoder/store/local-integrity/deep-reorg error is returned
from `Run` and halts the process — failing safely (letting a process
supervisor restart it) is preferred over endlessly retrying an error
whose meaning is unknown.

### Startup safety: network, pruning, IBD

`qoge-explorer index` validates the Core/database environment before any
database mutation: `QOGE_NETWORK` must exactly match Core's
`getblockchaininfo().chain` (`indexer.ErrNetworkMismatch` otherwise) —
never inferred from an address HRP, since network/address representations
can overlap. A pruned node (`pruned: true`) is rejected outright
(`indexer.ErrPrunedNode`): historical sync needs every block from genesis.
A node still in initial block download is also rejected outright rather
than retried — indexing an unstable, partial history is worse than making
the operator re-run the command once IBD completes; this is the simplest
auditable behavior for a fresh production start. `txindex` is not
required — `getblock <hash> 2` doesn't need it, and none of this phase's
RPC calls do either. RPC credentials are never printed; only
`config.RPCConfig.Redacted()`'s fixed placeholder ever reaches a log line.

### Crash/restart semantics

Because `Store.ApplyBlock` is one atomic transaction per block with the
checkpoint as its final write (§16), and the indexer always resumes from
`Store.Tip()`:

  - stopping between blocks resumes at `checkpoint + 1`;
  - a failure inside `ApplyBlock` before commit leaves the checkpoint at
    the prior block, and a restart retries the same height;
  - stopping right after a block's commit leaves the checkpoint including
    that block, and a restart starts at the next height;
  - restarting when the exact tip is already indexed reconciles (confirms
    the local tip is still on Core's active chain) and is a no-op — no
    duplicate balance/accounting changes.

No additional checkpoint state (file, LevelDB marker, etc.) is ever
introduced.

### Testing

`internal/indexer` tests run against a deterministic in-memory fake RPC
client (`RPCClient` — `GetBlockCount`/`GetBlockHash`/`GetBlockVerbose2`,
the only interface this phase adds) and a real, isolated PostgreSQL
schema per test — the same "fake the RPC boundary, use a real database"
split established in Phase 2B.2/2C.1. A synthetic P2QPK fixture (exactly
17,088-byte and 32-byte witness items, a structural witness v2/32-byte-
program output) is run through the real `Indexer → rpc.RawBlock →
decode.DecodeBlock → Store.ApplyBlock → PostgreSQL` path to prove
byte-exact persistence end to end; the indexer itself never inspects or
transforms witness bytes. An opt-in suite (`QOGE_INDEXER_INTEGRATION=1`)
runs the real `Indexer` against a real local `qogecoind`, capped to a
small height via a test-only RPC wrapper — normal `go test ./...` never
needs `qogecoind`, RPC credentials, or network access.

This phase does not implement a public explorer API or web UI.

## 19. Read-only query layer + JSON API (Phase 2D.1)

Phase 2D.1 exposes the already-indexed PostgreSQL state read-only. It adds
two new packages and upgrades `qoge-explorer serve` from a placeholder
health endpoint into a real process — nothing in `migrations/`, `Store`'s
write path (`ApplyBlock`/`RollbackTo`), `internal/decode`,
`internal/script`, `internal/chain`, or `internal/indexer` changes.
**PostgreSQL is the sole source of truth for every explorer read** — this
phase never calls Qogecoin Core RPC to reconstruct indexed data.

### Two packages, one direction of dependency

`internal/query` wraps the same `*pgxpool.Pool` `Store` writes through in
a second, read-only `query.Store` — every method it exposes issues
`SELECT` only (see `internal/query/doc.go`). `internal/api` is a thin
`net/http` layer (`internal/api/server.go` + `handlers_*.go`) that calls
`internal/query` and nothing else: no handler contains SQL, and no
handler reconstructs accounting logic Store/query.Store didn't already
compute. This mirrors the existing `decode → chain → store → indexer`
layering: each package only reaches into the layer directly below it
through its exported API.

### Read-only enforcement

There is no separate read-only database role in this phase (task-scoped
out; `query.Store` shares `Store`'s pool and its ordinary write
privileges at the connection level) — the guarantee is structural, not
credential-based: every query in `internal/query/*.go` is a `SELECT`.
Verified by `internal/query/readonly_test.go` two ways: (1) row COUNTS
across every mutable table (`blocks`, `transactions`,
`transaction_variants`, `block_transactions`, `transaction_inputs`,
`transaction_input_witness`, `transaction_outputs`, `addresses`,
`output_addresses`, `output_participants`, `utxo_state`) — a coarse,
cheap check that nothing was inserted or deleted; and (2) exact table
CONTENT for every column an UPDATE could realistically touch —
`sync_state` (height/hash), `blocks` (`canonical`, `orphaned_at IS
NULL`), `utxo_state` (identity, creation, spend state in full), and
`addresses` (every cached balance field) — via `mutableContentSnapshot`,
compared with `reflect.DeepEqual`. Row counts alone cannot catch an
UPDATE-only mutation (`blocks.canonical` flipping, `utxo_state.spent`
flipping, `addresses.balance_satoshis` changing) — the leaves-count-
unchanged class of bug review specifically flagged — which is exactly
what the content comparison exists to catch; it was verified (in a
throwaway, not-committed test) to actually fail when a `blocks.canonical`
flip was injected between snapshots, before being relied on as a real
regression gate. `query.Store` never calls `lockCheckpoint`'s row lock
(`internal/store/store.go`) — ordinary reads are never serialized behind
canonical-mutation writers.

### Multi-statement read consistency

A single `SELECT` is trivially internally consistent — Postgres gives it
one statement-level snapshot regardless of isolation level, so
single-statement methods (`Status`, `RecentBlocks`, `AddressSummary`,
`AddressHistory`) need nothing extra when called standalone. (§20 later
reuses these same statements' underlying SQL, unchanged, inside composite
multi-statement snapshots for the web UI's home and address pages — see
§20 "Composite read snapshots: home and address pages" — via the same
`querier`-parameterized helper shape described below.)

A DETAIL response assembled from several related `SELECT`s is a different
problem. Under the default READ COMMITTED isolation, EACH statement in an
implicit (non-transactional) sequence gets its OWN, independently-taken
snapshot — read committed does **not** make a multi-statement handler
behave as one consistent snapshot, and an earlier draft of this section
incorrectly claimed otherwise. Concretely, without explicit
transactional scoping, `TransactionByTxID` could read `block_transactions`
+ `blocks` and see branch A (`canonical: true`), then have a concurrent
`Store.RollbackTo` + `Store.ApplyBlock` sequence commit a full reorg to
branch B, and only THEN read `utxo_state` for the outputs — observing
branch B's spend state. The resulting JSON response would silently mix
two different committed states that never coexisted.

The fix: `TransactionByTxID`, `TransactionByWTxID`, `BlockByHeight`, and
`BlockByHash` each open one explicit `pgx.Tx` via `Store.readTx`
(`internal/query/query.go`) —

```go
pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}
```

— before issuing any of their statements. PostgreSQL fixes a REPEATABLE
READ transaction's snapshot at its FIRST statement and holds it for
every later statement in the same transaction, so every read composing
one response — for `TransactionByTxID`/`TransactionByWTxID`: the
transaction body, block occurrences/canonical flags, the chosen witness
variant, witness data, inputs, and outputs/`utxo_state`; for
`BlockByHeight`/`BlockByHash`: the header/canonical flag and its ordered
transaction list — comes from one indivisible view, no matter what
commits concurrently in the meantime. `AccessMode: ReadOnly` means the
transaction takes no locks a writer would ever wait on — it never
acquires `lockCheckpoint`'s row lock and never blocks `ApplyBlock`/
`RollbackTo` beyond PostgreSQL's ordinary MVCC bookkeeping. The
transaction is always rolled back (`readTx`'s returned `done` func), never
committed — there is nothing to persist either way, so an abandon is
exactly as correct as a commit and simpler to reason about.

For `TransactionByWTxID` specifically, the wtxid→txid identity resolution
is the transaction's FIRST statement, inside the same `pgx.Tx` as every
subsequent detail read — resolving it as an earlier, separate statement
(which an initial version of this code did) would let a concurrent reorg
land in the gap between identity resolution and the detail reads that
depend on it.

Private helpers below `Store`'s exported methods (`txOccurrences`,
`witnessByVin`, `txInputs`, `txOutputs`, `attachParticipants`,
`blockTxRefs`) take a `querier` interface —

```go
type querier interface {
    Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
    QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}
```

— rather than being hardcoded to `s.pool`, so the identical SQL runs
whether the caller is a single-statement method passing the pool directly
or a multi-statement method passing its `pgx.Tx`. This is deliberately
just an interface (both `*pgxpool.Pool` and `pgx.Tx` already satisfy it),
not a query builder or ORM.

`internal/query/snapshot_test.go` proves this deterministically — never
via `sleep` — using a package-level test hook
(`snapshotTestHook`, always `nil` in production) invoked exactly once,
synchronously, immediately after a wrapped method's first statement has
fixed its snapshot. The test blocks the in-flight query there, commits a
full reorg through the REAL `Store` (`RollbackTo` + two `ApplyBlock`
calls) on a concurrent goroutine, then releases the hook and asserts the
in-flight response still reflects branch-A truth (`canonical: true` for
a block/occurrence that is, by the time the response is actually
returned, already orphaned) while a brand-new query issued afterward
correctly sees branch B. Covers both `TransactionByTxID` (combining
canonical occurrence state with mutable `utxo_state` — the review's
specific concern) and `BlockByHash`.

### Cross-request consistency (intentionally not guaranteed)

Snapshot consistency is a per-response guarantee, not a per-connection or
per-session one. Two SEPARATE API requests — even milliseconds apart —
are two separate `readTx` calls with two separate snapshots; a later
request may legitimately observe newer indexed state than an earlier one
did, including a block that was canonical in the first response's
snapshot becoming orphaned by the time the second request runs. This is
expected and correct: the API is explicitly designed to tolerate the
indexer concurrently advancing canonical state between requests (§19's
opening summary), and nothing in this phase holds any cross-request
transaction or lock — doing so would contradict "never acquire
`lockCheckpoint`" above and would block the indexer for no benefit.

### Canonical vs. orphan semantics

Enforced by construction, not left to callers:

  - Height-based lookups (`RecentBlocks`, `BlockByHeight`) only ever
    `WHERE canonical` — an orphaned block can never be reached by height,
    mirroring `blocks_height_canonical_uidx`'s "exactly one canonical
    block per height" invariant.
  - Hash-based block lookup (`BlockByHash`) may return an orphaned block
    (historical/audit data), always explicitly tagged
    `"canonical": false` in the response — never silently presented as
    current.
  - Address balances (`AddressSummary`) and history (`AddressHistory`)
    are derived exclusively from `utxo_state`/`addresses`/
    `output_addresses`, which `RollbackTo` already keeps canonical-only
    (§16, §18) — an orphaned output's creation or spend never contributes
    to either. `internal/query/reorg_test.go` and `addresses_test.go`
    exercise a full branch flip (A→B) and flip-back (B→A) through
    `Store.RollbackTo`, confirming height lookups, recent-block listing,
    and address balance/history all follow the new canonical branch each
    time, while the losing branch's blocks remain queryable by hash.

### txid vs. wtxid lookup

`GET /api/v1/tx/{id}` accepts either a txid or a wtxid — `id` must be
exactly 64 lowercase hex characters; the lookup tries it as a txid first
(`query.Store.TransactionByTxID`) and only on `ErrNotFound` retries it as
a wtxid (`TransactionByWTxID`). This never conflates the two identity
spaces: the response always reports its own `txid` and `wtxid` as
separate fields, so which space `id` matched is never ambiguous in the
result. `TransactionByTxID` shows the transaction's CANONICAL witness
variant when one exists (falling back to the most recently observed
orphaned variant otherwise — `pickRepresentativeWTxID`,
`internal/query/transactions.go`); `TransactionByWTxID` always shows
exactly the variant requested, canonical or not, never silently
substituted. `Occurrences` on the response lists every block (canonical
and orphaned) the transaction has ever appeared in, each tagged with its
own `wtxid`, height, and `canonical` flag — reflecting the schema's own
`transactions`/`transaction_variants`/`block_transactions` split (§3a).

### Exact money representation

No JSON monetary value is ever a float. Every amount exposes an exact
integer `..._sats` field alongside an exact decimal `..._qoge` STRING
derived via `chain.Amount(sats).String()` (§2's fixed-point integer
arithmetic, already reviewed in Phase 2A) — `internal/query` never
reimplements that formatting. `transactions_test.go` items L/M and
`decoded_vectors_test.go`'s genesis vector (100 QOGE, `"100.00000000"`)
pin this down directly against known integer satoshi values.

### P2QPK large-witness response policy

A P2QPK signature witness item is exactly 17,088 bytes
(`script.P2QPKSignatureLength`, §8). Ordinary transaction-detail responses
never embed it: `InputDetail.Witness` is `[]query.WitnessItem{ItemIndex,
SizeBytes}` by default — metadata only, fetched via a `SELECT
octet_length(data)`, never pulling the `BYTEA` column itself
(`witnessByVin`, `internal/query/transactions.go`). Raw bytes are an
explicit opt-in: `GET /api/v1/tx/{id}?include_witness=true` sets
`DataHex` on every witness item, byte-exact after hex decoding. One route
with a query flag was chosen over a separate `/witness` endpoint to keep
the "one clean design" the task called for; `internal/query`'s
`TransactionByTxID`/`TransactionByWTxID` both take an explicit
`includeRawWitness bool` rather than defaulting to raw. Verified
end-to-end (`decoded_vectors_test.go`'s `TestDecoded_P2QPKPipeline`,
`internal/api/api_test.go`'s
`TestTransactionWitness_DefaultOmitsRaw_OptInIncludesIt`): the default
response body never contains the signature's hex substring; the opt-in
response contains it byte-exact, and item-1 (the 32-byte SLH-DSA public
key) is likewise byte-exact. The API/query layers never re-verify or
reinterpret the signature — exactly as before, this project never treats
witness bytes as anything but opaque data to persist and later return
unchanged.

### P2QPK output presentation vs. P2TR negative control

A structural P2QPK output is presented as `script_type: "p2qpk"`,
`witness_version: 2`, a 32-byte `witness_program`, and the address
`Store` already persisted (from Core's own RPC-reported destination) —
`internal/query` never re-derives the address and never treats the
witness program as the SLH-DSA public key itself (that key lives in the
SPENDING input's witness stack, item 1, not the output). Real P2TR stays
`script_type: "p2tr"`, `witness_version: 1` — structurally identical in
shape (both v/32-byte programs) but classified distinctly by
`script.Classify` (§7, §9) long before `internal/query` ever sees the
row; `decoded_vectors_test.go`'s `TestDecoded_P2TRDistinctFromP2QPK`
builds both outputs in the same transaction and asserts the returned
`script_type`/`witness_version` differ.

### Address balance vs. multisig participant semantics

`AddressSummary` reads only the `addresses` cache (`Store`'s
`recomputeAddress`, §16/§4) — the same canonical, spend-aware balance the
indexer already maintains; `internal/query` never recomputes it
independently. An address with no `addresses` row (never received a
canonical output, or every output that ever named it was rolled back) is
not `ErrNotFound`: it returns an all-zero `AddressSummary`, because the
schema has no address-validity concept to distinguish "malformed
address" from "syntactically fine, never used" — `AddressSummary`'s doc
comment in `internal/query/addresses.go` states this explicitly.
`output_participants` (bare-multisig co-signer identities, §7/§13.A)
never contributes to `AddressSummary` or `AddressHistory`. Balance
(`AddressSummary`) is built exclusively from the `addresses` cache, as
above; history (`AddressHistory`) is built from IMMUTABLE destination/
input relations (`output_addresses`/`transaction_inputs`) joined against
canonical block OCCURRENCE state (`block_transactions`/`blocks WHERE
canonical`) — deliberately NOT from `utxo_state` (see "Address history
vs. UTXO eligibility" below), even though the two happen to agree for
every ordinary spendable output. A multisig output's participants ARE
surfaced, but only as `OutputDetail.
Participants []string` on the owning TRANSACTION's output list
(`attachParticipants`, `internal/query/transactions.go`) — structurally
separate from `Address` (the sole balance-accounting field) on the same
struct, so a caller can never mistake "who could sign this" for "who
owns this."

### Address history vs. UTXO eligibility

Balance semantics and historical visibility are two different questions,
and an earlier version of `AddressHistory` conflated them: it joined
`output_addresses` through `utxo_state` for BOTH the receive and spend
sides, which makes UTXO eligibility a silent prerequisite for even
APPEARING in an address's history. That drops legitimate canonical
destinations `Store` genuinely persists but Core intentionally never
inserts into its UTXO set at all — `utxo_state` is deliberately missing a
row for every height-0 (genesis) output AND every `script.IsUnspendable`
output, regardless of height (`Store.ApplyBlock`'s "Core UTXO semantics"
doc comment, `internal/store/apply.go`). The clearest concrete case: the
genesis coinbase's destination address has real, canonical
`transaction_outputs`/`output_addresses` rows, forever — but with the old
query, its address history was silently empty.

The fix keeps `AddressSummary`'s balance semantics completely unchanged
(still exclusively the `addresses` cache — a genesis-only destination
correctly shows a zero balance, since Core never treats that coinbase as
spendable) and rebuilds `AddressHistory` to require only canonical block
OCCURRENCE, never UTXO eligibility:

  - RECEIVE side: `output_addresses` → `transaction_outputs` (proves the
    output really has this exact address/txid/vout) → `block_transactions`
    (that txid's occurrence) → `blocks WHERE canonical`.
  - SPEND side: `output_addresses`' `(txid, vout_index)` →
    `transaction_inputs` whose `(prev_txid, prev_vout_index)` reference
    exactly that output. `transaction_inputs` is immutable per-transaction
    body data kept forever regardless of branch (§3 "Reorg keeps an audit
    trail"), so this step ALONE would include orphaned spend attempts —
    it's the join through the SPENDING transaction's own
    `block_transactions`/`blocks WHERE canonical` that actually restricts
    results to canonical spends only.

An orphaned output's creation or spend still never appears (both sides
require their own canonical `block_transactions`/`blocks` join) —
verified directly, alongside the genesis case, reorg branch-following,
flip-back, and multisig-participant exclusion, by
`internal/query/addresses_test.go`'s
`TestAddressHistory_GenesisDestinationVisibleDespiteZeroBalance`,
`TestAddressHistory_ReorgAndFlipBack`,
`TestAddressHistory_CanonicalSpendAppears`, and
`TestAddressHistory_MultisigParticipantsExcluded`.

### API routes and versioning

Every route is under `/api/v1/`, plus process-level `/healthz`
(liveness, no database dependency) and `/readyz` (confirms the
configured PostgreSQL database is reachable and holds the expected
`sync_state` checkpoint row — never Core RPC):

```
GET /healthz
GET /readyz
GET /api/v1/status
GET /api/v1/blocks
GET /api/v1/block/{height-or-hash}
GET /api/v1/tx/{txid-or-wtxid}
GET /api/v1/tx/{txid-or-wtxid}?include_witness=true
GET /api/v1/address/{address}
GET /api/v1/address/{address}/transactions
```

No mutation endpoint exists (`internal/api/api_test.go`'s
`TestNoMutationEndpoints` confirms `POST`/`PUT`/`PATCH`/`DELETE` never
return 200 on an existing route). EVERY response uses a consistent JSON
envelope for its errors (`{"error":{"code":...,"message":...}}`,
`internal/api/errors.go`) — including a wrong HTTP method on a real route
(405, `error.code = "method_not_allowed"`, with an `Allow` header) and a
truly unregistered path (404, `error.code = "not_found"`), not just the
400/404/500s individual handlers produce. `server.go`'s `routes()`
registers each known path pattern TWICE: once as `"GET <pattern>"` for the
real handler, once as the bare, method-agnostic `"<pattern>"` returning
the JSON 405. `net/http.ServeMux` resolves a request against the more
specific of two overlapping patterns — a method-qualified pattern beats a
bare one for a request whose method actually matches it — so a GET hits
the real handler and any other method falls through to that path's 405
registration; a final unrestricted-method `"/"` pattern catches anything
left over as the JSON 404. This is the opposite of what a naive single
catch-all does: registering ONLY an unrestricted `"/"` pattern (with no
per-path 405 registrations) makes it a valid, if low-priority, match for
a WRONG-method request on a real path too — which was confirmed directly
against the stdlib to silently turn a should-be-405 into a 404 instead,
and is exactly the bug an earlier version of this file had (see
`internal/api/server.go`'s `knownGETRoutes`/`routes()` doc comments for
the full mechanism, and `TestMethodNotAllowed`/`TestUnknownRoute` in
`internal/api/api_test.go` for the routing-precedence proof: GET on a
known route reaches the real handler, every other method on that same
route gets a JSON 405 with `Allow`, an unknown path gets a JSON 404
regardless of method). List endpoints are keyset-paginated
(`before`/`before_txid` cursors, never raw `OFFSET`) with a hard maximum
page size (`query.MaxPageSize = 100`) enforced in the query layer
regardless of what a caller requests. Identifier validation happens
before any database call: block/tx hashes must be exactly 64 lowercase
hex characters (uppercase is rejected, never silently normalized —
`internal/api/validate.go`); heights must be non-negative decimal
integers; addresses get a length/character-shape bound only — this phase
never re-implements full consensus address validation.

### `index` and `serve` remain separate processes

`qoge-explorer serve` requires `QOGE_DATABASE_URL` but no Core RPC
credentials at all — it never dials Qogecoin Core. `qoge-explorer index`
is unchanged and remains the only process that writes. Composing the two
(e.g. exposing Core's live tip/sync-lag via the API) is explicitly
deferred — `query.Status` reports only the INDEXED DATABASE's checkpoint
(`sync_state('main')`), never a live RPC-derived height. The default bind
address remains loopback-only (`127.0.0.1:8532`); public exposure through
a reverse proxy, and authentication, are both out of scope for this
read-only phase. `http.Server` sets explicit `ReadHeaderTimeout`,
`ReadTimeout`, `WriteTimeout`, and `IdleTimeout` (`cmd/qoge-explorer/
main.go`'s `runServe`), and the process shuts down cleanly on
SIGINT/SIGTERM via `http.Server.Shutdown`, mirroring `index`'s existing
signal handling.

### Testing

`internal/query`'s tests (A–T in the phase task list, plus a dedicated
`snapshot_test.go` for concurrent-reorg snapshot consistency — see
"Multi-statement read consistency" above) build every fixture through the
real `Store.ApplyBlock`/`RollbackTo` — never ad-hoc SQL — using either
plain `chain.Block` literals or, for the P2QPK/P2TR/real script-type
vectors, the actual `decode.DecodeBlock` pipeline
(`decoded_vectors_test.go`), so the query layer is always tested against
data that passed through the same decoder/classifier every other phase
already reviewed. `internal/api`'s tests exercise the same fixtures one
layer up, through `Server.ServeHTTP` via `httptest`, confirming HTTP
status codes, JSON shapes, the witness opt-in behavior, and the JSON
404/405 error-envelope routing precedence end-to-end. Both packages skip
their PostgreSQL-backed tests (never fail `go test ./...`) when
`QOGE_TEST_DATABASE_URL` is unset, matching every prior phase's
convention.

This phase does not implement a public explorer HTML web UI — see §20 for
Phase 2E.1, which adds one as a sibling of this API over the same
`query.Store`.

## 20. Server-rendered HTML explorer UI (Phase 2E.1)

Phase 2E.1 adds the first public HTML interface: `internal/web`, a
PRESENTATION-ONLY package server-rendering pages from the same
already-reviewed `query.Store` §19 introduced. Nothing in `migrations/`,
`Store`'s write path, `internal/decode`, `internal/script`,
`internal/chain`, or `internal/indexer` changes; §19's `internal/api` also
has ZERO behavioral diff — every fact this phase displays already existed
as an exported `query.Store` method before this phase began, so no new
query capability was needed for any page. `internal/query` itself gained a
small, deliberate, READ-ONLY exception after an internal review of the
first draft of this phase — see "Composite read snapshots: home and
address pages" below — to fix a page-level snapshot-consistency bug the
same class as the one §19's "Multi-statement read consistency" already
fixed at the single-response level; no write SQL, schema change, or
canonical-accounting change was involved.

### `internal/web` is a sibling of `internal/api`, not a client of it

```
Qogecoin Core -> decoder -> Store/PostgreSQL -> indexer -> query.Store
                                                              ├── internal/api  (JSON)
                                                              └── internal/web  (HTML)
```

Both packages depend only on `internal/query`'s exported API and hold a
`*query.Store`, never a write `internal/store.Store`, `internal/indexer`,
`internal/rpc`, or `internal/decode`. Critically, `internal/web` never
makes an HTTP call to `internal/api` (or vice versa) — that would add a
loopback network dependency and a second, redundant read path for data
`query.Store` already serves directly. `cmd/qoge-explorer/main.go`'s
`newRootHandler` composes them as siblings under one `http.ServeMux`:
`/api/`, `/healthz`, and `/readyz` delegate to `api.New(...)`; every other
path delegates to `web.New(...)`. Both handlers run inside the same
`qoge-explorer serve` process/listener `index` and `serve` have always
been split from — `serve` still requires `QOGE_DATABASE_URL` and never
Core RPC credentials, and `index` remains the only process that writes.

### Rendering: `html/template`, embedded templates and assets, no build step

`internal/web` uses `html/template` exclusively — never `text/template` —
so every blockchain-derived string (addresses, script/witness hex, txids,
block hashes, the raw `?q=` search value) is auto-escaped by default; no
handler ever wraps a stored value in `template.HTML`. Templates
(`internal/web/templates/*.tmpl`) and the one stylesheet
(`internal/web/static/app.css`) are embedded via `//go:embed`
(`templates.go`), so the built binary has no working-directory dependency
and can be launched from anywhere. There is no Node/npm/webpack/Vite/
Tailwind-build step and no external CDN dependency — a system font stack
only. Each page is parsed as `layout.tmpl` + `header.tmpl` + `footer.tmpl`
+ `pagination.tmpl` + `blocktable.tmpl` + that page's own file defining a
`"body"` block (`loadTemplates`), the standard shared-layout idiom that
lets every page reuse the same `"body"`/`"layout"` template names without
a name collision, and is rendered via `ExecuteTemplate(w, "layout", ...)`.
A parse failure is a build-time defect in the embedded template set
itself (never a runtime/data condition), so `web.New` panics on one and
`TestTemplatesParse` parses every page in every test run.

### Routes

```
GET /                              home: indexed tip + recent canonical blocks
GET /blocks                        canonical blocks, keyset-paginated
GET /block/{id}                    height (canonical-only) or 64-hex hash (canonical or orphan)
GET /tx/{id}                       txid, falling back to wtxid on a miss
GET /address/{address}             balance summary + canonical history
GET /search?q=...                  conservative redirect/resolve, see below
GET /static/{path}                 embedded CSS
```

Routing mirrors `internal/api/server.go`'s reviewed precedence pattern
exactly: every known path is registered twice — once as `"GET
<pattern>"` for the real handler, once as the bare `"<pattern>"` for a
same-path wrong-method fallback — plus one final unrestricted `"/"`
catch-all. The fallback and catch-all render the shared HTML error page
(`templates/error.tmpl`) instead of JSON — `internal/web/web.go`'s
`knownGETRoutes`/`routes()`.

### Composite read snapshots: home and address pages

The home page and the address page each assemble their response from TWO
logically related `query.Store` calls: home needs `Status` (the indexed
checkpoint) plus `RecentBlocks` (the newest canonical blocks); address
needs `AddressSummary` (balance/accounting) plus `AddressHistory`
(canonical destination visibility). An internal review of the first draft
of this phase caught that calling these two methods independently
reintroduces, at the PAGE level, the exact class of bug §19's "Multi-
statement read consistency" already fixed at the single-response level: a
concurrent `ApplyBlock`/`RollbackTo` committing between the two calls can
make one rendered HTML page describe two different canonical states that
never coexisted — e.g. `Status` reporting tip `A2` while `RecentBlocks`'s
own first row is already `B2` after a reorg, or an address page pairing a
balance that belongs to branch A with a history that belongs to branch B.

The fix mirrors §19's existing pattern exactly, at a coarser grain:
`internal/query/composite.go` adds two new exported methods —

```go
func (s *Store) ExplorerOverview(ctx context.Context, recentBlocksPageSize int) (ExplorerOverview, error)
func (s *Store) AddressDetail(ctx context.Context, address string, beforeHeight *int64, beforeTxID *string, pageSize int) (AddressDetail, error)
```

— each opening one `Store.readTx` (`REPEATABLE READ`, `ReadOnly`) and
issuing both underlying SELECTs through that same `pgx.Tx`, firing the
existing `snapshotTestHook` immediately after the first one (same
convention `BlockByHash`/`TransactionByTxID` already use). `internal/web`'s
`handleHome`/`handleAddress` call ONLY these composite methods now; they
never independently call `Status`/`RecentBlocks`/`AddressSummary`/
`AddressHistory` themselves.

No SQL was duplicated to build this: `Status`, `RecentBlocks`,
`AddressSummary`, and `AddressHistory`'s bodies were each split into a
public single-statement method (still called with `s.pool`, unchanged
behavior/signature, `internal/api` keeps calling exactly these) plus a
private `*From(ctx, querier, ...)` helper (`statusFrom`, `recentBlocksFrom`,
`addressSummaryFrom`, `addressHistoryFrom`) that takes the same `querier`
interface §19 already introduced for this exact reason. The composite
methods call the `*From` helpers with the open `pgx.Tx`; the public methods
call them with `s.pool`. Same SQL, two callers. No `SELECT FOR UPDATE`, no
new lock, no checkpoint mutation — `AccessMode: ReadOnly` still means
neither composite method ever blocks or is blocked by `ApplyBlock`/
`RollbackTo` beyond ordinary MVCC.

`internal/query/snapshot_test.go` proves both composites deterministically
(no `sleep`, same `snapshotTestHook`-based synchronization §19's tests
use): `TestSnapshotConsistency_ExplorerOverview_ConcurrentReorg` and
`TestSnapshotConsistency_AddressDetail_ConcurrentReorg` each fix a
composite read's snapshot on branch A, commit a real reorg to branch B on a
concurrent goroutine through the real `Store`, and assert the in-flight
result is entirely branch-A-consistent (never a mix) while a fresh call
issued afterward is entirely branch-B-consistent.

Separately, `/blocks` and `/address/{address}`'s "next page" links now
also preserve the caller's originally-requested `limit` query parameter
(`blocksPagination`/`addressPagination` in `internal/web/viewmodels.go`) —
a caller paging forward from `?limit=2` no longer gets silently reset back
to `query.DefaultPageSize` on the next click, a small independent cleanup
found by the same review. HTML 405 responses on a known GET-only route now
also set an `Allow: GET` header (`internal/web/web.go`'s bare-pattern
fallback), matching ordinary HTTP semantics for method-not-allowed; unknown
routes remain a plain 404 with no `Allow` header.

### Canonical/orphan visual semantics

Block/transaction lookups reuse `query.Store`'s existing semantics
unchanged: `BlockByHeight` is canonical-only (there is no orphan-by-height
route or lookup), `BlockByHash` returns canonical OR orphaned blocks, and
the orphan case renders a clearly-labeled, non-alarmist banner ("Orphaned
block — retained for historical/audit view") — never styled to look
canonical. Transaction occurrence rows likewise show each occurrence's own
canonical/orphaned status, never collapsed to a single flag.

### Address balance vs. history remain independent

`templates/address.tmpl` renders `AddressSummary` (balance/accounting,
from the `addresses` cache) and `AddressHistory` (canonical destination
visibility, built from immutable relations) exactly as `query.Store`
returns them, with no reconciliation attempted in the template or
handler. A genesis-only P2PK destination therefore legitimately renders
`0.00000000 QOGE` balance while still showing its canonical genesis
transaction in history — this is not "fixed" in the UI, matching §19's
"Address history vs. UTXO eligibility". Bare multisig participant
addresses are rendered under a clearly separate "Participants" label on
the transaction page, are never called a recipient/owner, and (being
`output_participants` identities, not `output_addresses` ones) never
appear in their own `AddressHistory`/`AddressSummary`.

### P2QPK large-witness default-hidden policy

`templates/tx.tmpl` never renders a witness item's `data_hex` unless the
caller explicitly requested it — the same `?include_witness=true` opt-in
`internal/api` already used, now also available as a same-page link/toggle
on `/tx/{id}`. The default view shows each witness item's `item_index` and
exact `size_bytes` (e.g. "Item 0: 17088 bytes... (raw data hidden by
default)") with no hex at all; opting in renders the byte-exact hex inside
a wrapping, horizontally-scrollable `<pre><code>` block
(`pre.raw-hex` in `app.css`) so a 17,088-byte signature cannot force the
page wider than the viewport. The raw bytes are never logged and no
SLH-DSA verification is attempted anywhere in this package. The witness
items on an INPUT (a spend) are rendered generically ("Item 0", "Item 1"
+ size) with no semantic label — they are never called "the public key,"
which is deliberately reserved for a P2QPK OUTPUT's separate,
structurally-distinct `witness_program` field (v2/32 bytes, shown next to
the OUTPUT's own script type and stored address) — the two are visibly
different sections of the page (Inputs vs. Outputs) so they are never
conflated. P2TR (v1/32) and P2QPK (v2/32) outputs render with the
decoder-assigned `script_type` string `query.Store` already returns
(`p2tr` vs. `p2qpk`) — `internal/web` performs no script classification
of its own.

### Search

`GET /search?q=...` resolves conservatively and deterministically, never
querying Core and never running a broad SQL search
(`handlers_search.go`): a non-negative decimal integer redirects straight
to `/block/{height}` (the block page's own existence check applies, not
duplicated in search); exactly 64 lowercase hex characters is tried, in
this fixed order, against `BlockByHash`, then `TransactionByTxID`, then
`TransactionByWTxID` — the first hit redirects there, and no hit renders
an explicit "nothing matched" result page rather than guessing which
identity space it might have belonged to; anything else within the
ordinary address-shape bound (`isValidAddressShape`, mirroring
`internal/api/validate.go`'s) redirects to `/address/{address}`. Input is
length-bounded before any of this runs.

### HTML error pages and security headers

`/`, `/blocks`, `/block/{id}`, `/tx/{id}`, `/address/{address}`, and
`/search` return HTML 400/404/405/500 pages sharing the same layout
(`templates/error.tmpl`) — never a JSON body, and never a raw Go error
string, SQL fragment, database URL, or stack trace; a real 500 is logged
server-side via `slog` and the client only ever sees a fixed, generic
message (`errors.go`'s `renderInternalError`, mirroring
`internal/api/errors.go`'s `writeInternalError`). Every `internal/web`
response — HTML pages, the static handler, and error pages alike — sets
`X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`, and a
self-only `Content-Security-Policy` (`default-src 'self'; style-src
'self'; img-src 'self'`; no inline scripts/styles are used anywhere in
this phase — Phase 2E.2 later adds `script-src 'self'` explicitly when it
introduces the first embedded script, see §21). `internal/api`'s existing
JSON headers/error contract are completely unmodified — `internal/web`
never touches that package.

### Time and money display

Block timestamps render as an unambiguous absolute UTC string (e.g.
`2026-08-11 02:34:56 UTC`, `formatTimeUTC` in `helpers.go`) — never
server-local time, and this phase does not (yet) add a relative-time
display. Monetary values use `query.Store`'s existing exact QOGE decimal
strings (`ValueQOGE`, `BalanceQOGE`, `FeeQOGE`, ...) directly; satoshis
are shown alongside as secondary text. `internal/web` never parses a
monetary string into `float64` for formatting — the same "no float
monetary path" invariant §19 established.

### No live refresh yet

Phase 2E.1 pages are deterministic, fully server-rendered on each
request — no WebSockets, no SSE, no polling/auto-refresh JavaScript, and
no mempool UI. The explorer remains fully usable with JavaScript
disabled; this phase ships no JavaScript at all. Live-refresh is added,
CONFIRMED-CHAIN ONLY, in Phase 2E.2 (§21); mempool remains deferred to a
separate future phase.

### Testing

`internal/web`'s tests build canonical (and, for reorg/orphan cases,
deliberately non-canonical) state through the real `Store.ApplyBlock`/
`RollbackTo` — never ad-hoc SQL — using the same `chain.Block`-literal and
`decode.DecodeBlock`-pipeline fixture patterns `internal/api` and
`internal/query` already established (duplicated per-package on purpose,
per this repo's existing convention — see `internal/web/dbtest_test.go`'s
note), then exercise `Server.ServeHTTP` via `httptest` and assert on the
rendered HTML body. This includes a dedicated escaping-regression test
(`security_test.go`) that writes a real canonical output whose `address`
is `<script>alert(1)</script>&"'` through `Store.ApplyBlock` — addresses
are treated as opaque, unvalidated strings at every layer down to the
schema, so this is a genuinely reachable value, not a weakened model —
and asserts the rendered `/address/{...}` and `/tx/{...}` pages contain
the escaped form and never the raw tag. `cmd/qoge-explorer/serve_test.go`
separately confirms the actual route composition (`newRootHandler`): JSON
routes stay JSON, HTML routes stay HTML, and `/healthz`/`/readyz` behavior
is byte-for-byte unchanged from §19. All PostgreSQL-backed tests skip
(never fail `go test ./...`) when `QOGE_TEST_DATABASE_URL` is unset,
matching every prior phase.

This phase does not add WebSockets/SSE/polling, a mempool view, or any
JavaScript beyond what a future progressive-enhancement pass might add.

## 21. Confirmed-chain live UI refresh (Phase 2E.2)

Phase 2E.2 adds the first JavaScript this project ships:
`internal/web/static/live.js`, a small polling client that notices when
the INDEXED PostgreSQL checkpoint has moved and either reloads the page or
shows a passive notification, so a browser tab left open doesn't silently
go stale. It is explicitly CONFIRMED CHAIN ONLY — no mempool, no
unconfirmed transactions, no WebSockets/SSE, no Core RPC access from
`serve`, no background indexing in `serve`, no new persistence, and no
client-side reconstruction of canonical state. `internal/query`,
`internal/api`, `internal/store`, `internal/indexer`, `internal/decode`,
`internal/script`, `internal/chain`, `internal/rpc`, and `migrations/` all
have ZERO diff from Phase 2E.1 — this phase is confined to `internal/web`
(plus `docs/ARCHITECTURE.md`).

### The browser is a notifier, not a second explorer engine

Phase 2E.1 deliberately guarantees that every server-rendered page is one
coherent snapshot (§19 "Multi-statement read consistency", §20 "Composite
read snapshots"). Piecemeal client-side updates — fetching several API
objects and patching individual DOM nodes — would silently undo that
guarantee by letting a single browser view mix data from different
snapshots the same way the reviewed bugs in §19/§20 did at the server
layer. `live.js` is deliberately kept incapable of this: it polls exactly
one endpoint,

```
GET /api/v1/status
```

(already existing, unchanged — Phase 2E.2 needed no `internal/api`,
`internal/query`, or schema change; the existing `indexed_height` /
`indexed_block_hash` fields were already sufficient), and its only two
actions are (a) trigger a normal full-page reload — `window.location.
reload()`, which re-fetches the page through the exact same server-render
path §19/§20 already guarantee is coherent — or (b) show a static
notification banner asking the user to do that manually. It never issues
a second `fetch` for block/transaction/address data and never writes
blockchain-derived content into the DOM itself.

### Polling behavior

Every page loads `/static/live.js` (embedded via `go:embed`, same
mechanism as `app.css` — no Node/npm/bundler, no CDN) via
`<script src="/static/live.js" defer></script>` in `templates/
layout.tmpl` — no inline script anywhere. It polls every 10 seconds with

```js
fetch("/api/v1/status", { cache: "no-store", headers: { "Accept": "application/json" } })
```

guarded by an in-flight flag so a slow response never overlaps the next
scheduled poll. While `document.hidden` is true (a background tab), the
poll function still fires on the interval but exits immediately without
issuing a request — so a backgrounded tab generates no network load at
all — and `visibilitychange` triggers one immediate poll the instant the
tab becomes visible again, so the notification/reload decision is never
stale by up to a full 10-second interval after switching back. A failed
`fetch` (network hiccup, transient 5xx) is swallowed silently: the last
known checkpoint is retained, nothing reloads, no false banner appears,
and the console is never spammed — matching Phase 2E.1's principle that
JavaScript failure must never break a page that already rendered
correctly server-side.

### Baseline tip: harmless data attributes, never executable data

The home page and the first (cursor-less) `/blocks` page each expose the
tip they were server-rendered from via plain HTML data attributes —
`data-indexed-height`/`data-indexed-hash` on the home page's hero section
(`templates/home.tmpl`, sourced from the same `Status` §20's
`ExplorerOverview` already reads), and `data-block-height`/
`data-block-hash` on every canonical block row (`templates/
blocktable.tmpl`). `html/template`'s ordinary auto-escaping is what
protects these — there is no separate escaping path to get wrong, and
nothing is ever placed inside a `<script>` block or `javascript:` URL.
`live.js` reads these once at load time as its baseline, before the first
poll — this is what lets the FIRST poll detect a block that was indexed
in the gap between the HTML snapshot being rendered and the script's
first request actually firing.

### Two baseline modes (post-review correction)

An internal review of the first draft caught that pages with no rendered
tip — every detail page — initialized their comparison state to `null`,
so the very first successful `/api/v1/status` response (a real, non-null
hash on any normally-functioning database) looked like a "change" and
showed a false "chain updated" banner immediately on page load, even
though nothing had changed since the page rendered. The fix
(`live.js`'s `hasStatusBaseline` flag) makes the distinction explicit
rather than inferring it from a null check:

- **Auto-refresh pages** (`data-live-refresh="home"`/`"blocks"`):
  `hasStatusBaseline` is `true` from the start — the rendered HTML
  attributes above already ARE a valid baseline, including the
  legitimate `null`/`-1` case on an empty database, so the very first
  poll must be able to detect a change immediately (this is the whole
  point of exposing the rendered baseline at all).
- **Notify-only pages** (everything else: block/tx/address detail,
  `?include_witness=true`, historical `/blocks?before=...`):
  `hasStatusBaseline` starts `false`. The first SUCCESSFUL status
  response silently sets `lastKnownHash` and flips the flag, producing no
  banner and no reload — only a SUBSEQUENT response with a different
  hash is a real, bannerable change. A failed request never touches the
  flag, so a transient failure before the first success simply delays
  when the baseline gets established; it never fabricates one.

This preserves every other invariant unchanged: notify-only pages still
never auto-reload once their baseline is established, and the
same/lower-height reorg-safety logic (§21 "Tip-change semantics") applies
identically once a real change is observed on either kind of page.

### Auto-refresh is opt-in per page, via one marker attribute

A page is only ever eligible for an automatic reload if it contains an
element carrying `data-live-refresh="home"` or `data-live-refresh="blocks"`
— `live.js` looks for exactly one such element via
`document.querySelector("[data-live-refresh]")`; its absence (every
detail page, every paginated `/blocks?before=...`, and
`?include_witness=true`) means "notify only, never reload," with no
separate configuration needed. Concretely:

- `templates/home.tmpl`'s hero section carries `data-live-refresh="home"`.
- `templates/blocks.tmpl` wraps its block table in
  `<div data-live-refresh="blocks">` only when `blocksView.FirstPage` is
  true (`handlers_blocks.go` sets it from `before == nil`) — a historical
  page never auto-reloads just because the global tip advances (a
  requirement carried over unchanged from §20's height-cursor pagination
  design).
- `/block/{id}`, `/tx/{id}`, `/address/{address}`, and any
  `?include_witness=true` transaction view carry no such marker at all,
  so a person reading a transaction, inspecting an orphan block, or
  viewing a raw 17,088-byte P2QPK witness is never interrupted by a
  background reload, never loses scroll position, and never has that
  witness view silently re-rendered.

### Tip-change semantics: hash-changed, not "new block"

The comparison signal is `indexed_block_hash` changing from what
`live.js` last observed — never simply "height increased," because a
reorg can replace the canonical block at an unchanged height, and
`RollbackTo` followed by a replacement `ApplyBlock` can transiently move
the reported height backward before the replacement lands. `live.js`
never attempts to interpret *why* the hash changed (no client-side reorg
logic) and never claims "New block" — the banner text is the neutral "The
indexed canonical tip changed." For an auto-refresh-eligible page, a
reload is only actually triggered once the newly observed height is at or
above that page's own baseline height; if it's below (the rollback half
of a reorg observed mid-flight), `live.js` shows the banner instead and
lets a later poll observe the stabilized tip rather than reloading into a
transient ancestor state.

### The live banner

One shared element, `#live-chain-banner` in `templates/layout.tmpl`,
present (and `hidden`) on every page regardless of live-refresh
eligibility — including detail pages, which only ever get the banner, never
an automatic reload. It carries `role="status"` and `aria-live="polite"`
so assistive technology announces it without stealing keyboard focus, and
its "Refresh" control is a real `<button>` (not a div/span with a click
handler bolted on) so it's reachable and operable via normal keyboard
navigation. `live.js` only ever sets the banner's message via
`textContent` — never `innerHTML` — and the message text itself is a
fixed string, never interpolated from the API response body (§16 below).
With JavaScript disabled, the banner's `hidden` attribute is never
cleared, so it never appears and the "Refresh" button is simply inert
dead markup — the rest of the page is completely unaffected, preserving
Phase 2E.1's "usable with JavaScript disabled" property exactly.

### Security posture

The CSP gains an explicit `script-src 'self'` (`internal/web/web.go`):
`default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'`
— still no `unsafe-inline`, no `unsafe-eval`, no external host of any
kind. `live.js` itself never calls `eval`, `new Function`, or
`document.write`, and never sets `.innerHTML` — a dedicated Go test
(`live_test.go`'s `TestLive_ScriptContract`) reads the embedded script
source directly and asserts both the required substrings
(`/api/v1/status`, `no-store`, `indexed_height`, `indexed_block_hash`) and
the forbidden ones (`WebSocket`, `EventSource`, `/mempool`,
`getrawmempool`, `innerHTML`, `eval(`, `new Function`, any `http://`/
`https://` literal) are exactly as expected — pinning the architectural
contract without needing a JS runtime in the Go test suite. `live.js`
only ever reads `indexed_height`/`indexed_block_hash` from the status
response; it never touches addresses, script bytes, witness data,
transaction bodies, or monetary values, and never logs a raw API response
body.

### Testing

`internal/web/live_test.go` covers the full contract: the layout loads
`/static/live.js`; the banner is present, hidden, and carries
`role="status"`/`aria-live="polite"`; the home page exposes its baseline
tip via data attributes (including the "no blocks indexed yet" case,
where `data-indexed-height="-1"` is present but `data-indexed-hash` is
correctly absent); canonical block rows expose their own height/hash and
only the cursor-less `/blocks` page carries the live-refresh-eligible
wrapper; `/static/live.js` serves `200` with a JavaScript-compatible
`Content-Type`; the CSP explicitly permits self-hosted script with no
unsafe directives; no page contains an external `http(s)://` or CDN
reference; and existing pages still render their full, meaningful content
with nothing depending on script execution. All of Phase 2E.1's existing
tests — canonical/orphan, txid/wtxid, P2QPK/P2TR, address history,
genesis, multisig, pagination, escaping, security headers, error pages,
and the `internal/query` concurrent-reorg snapshot tests
(`TestSnapshotConsistency_ExplorerOverview_ConcurrentReorg`,
`TestSnapshotConsistency_AddressDetail_ConcurrentReorg`, and the
`BlockDetail`/`TransactionDetail` ones) — continue passing unmodified,
confirming the live UI doesn't weaken any server-side snapshot guarantee.
`internal/api`'s existing test suite also runs completely unmodified.
`TestLive_ScriptContract` additionally pins that the notify-only
baseline-initialization guard (`hasStatusBaseline`) exists in the shipped
source, since the earlier `live_test.go` suite — being purely HTML/
static-contract tests — did not catch the false-banner-on-first-load
defect the internal review found; the state machine itself (both baseline
modes, the failure-before-first-success case, and the existing home-page
reorg-safety behavior) was additionally verified directly by executing
the actual, unmodified `live.js` against a minimal hand-written DOM/fetch
shim under Node's built-in `vm` module — a throwaway, uncommitted
verification harness, not a project dependency (no npm, no jsdom, no
`package.json` added anywhere in the repo).

### Explicitly deferred

No mempool, no unconfirmed transactions, no WebSockets/SSE, no live
balance/fee updates, no client-side monetary arithmetic, no address-page
auto-refresh (address pages only update on an explicit user refresh, same
as every other detail page). Mempool has different, ephemeral consistency
semantics from confirmed-chain PostgreSQL state and remains a dedicated
future phase.

## 22. Mempool cache foundation (Phase 2F.1)

Phase 2F.1 adds the first mempool (unconfirmed-transaction) support:
Core mempool RPC acquisition, strict transaction decoding, and an
isolated, atomically-replaceable PostgreSQL cache (`internal/mempool`,
`migrations/0002_mempool_cache.up.sql`). It adds NO public read API or
UI — `internal/query`, `internal/api`, and `internal/web` all have ZERO
diff from Phase 2E.2. `internal/store`, `internal/chain`,
`internal/script`, and `internal/decode`'s own decoding *behavior* are
similarly untouched; the only decode-adjacent change is a new optional
`BlockHash` field on `rpc.RawTransaction` (see "Confirmed-transaction
detection" below), which `DecodeBlock`/`DecodeTransaction` never inspect.

### Mempool state is a fundamentally different model from confirmed state

Confirmed chain state flows Core -> `internal/indexer` -> immutable
confirmed transaction/block tables + canonical mutable UTXO/address
state, forever (subject only to an audited reorg rollback). Mempool state
flows Core -> `internal/mempool.Synchronizer` -> a completely
**replaceable** ephemeral PostgreSQL cache. A mempool transaction can
disappear, be replaced (RBF), expire, conflict, or become confirmed at
any moment; it must never become permanent historical data merely
because it was observed once. Concretely:

- No `mempool_*` table has a REQUIRED foreign key into `transactions`,
  `blocks`, `utxo_state`, `addresses`, or `sync_state` — a mempool
  transaction can depend on a confirmed prevout or another mempool
  transaction without coupling its lifetime to confirmed tables.
- Confirmed tables are never written by anything in `internal/mempool` —
  enforced in tests by `TestConfirmedState_UnaffectedByMempoolReplacement`,
  which snapshots the exact (not just row-count) content of every
  confirmed table before and after a mempool replacement.
- `addresses`/`utxo_state` are never updated for mempool activity — no
  pending balance, no pending received/sent, no spendable-including-
  mempool figure exists anywhere yet. That is a separately reviewed
  future decision, not an implicit side effect of this phase.

### `serve` remains Core-RPC-independent

`qoge-explorer serve` still requires only PostgreSQL and never Core RPC
credentials — unchanged from every prior phase. `internal/mempool` is
wired in only by `qoge-explorer index` (`cmd/qoge-explorer/main.go`'s
`runIndex`), the process that already holds Core credentials. The
mempool synchronizer runs as a second goroutine alongside the confirmed
`indexer.Indexer.Run` loop, sharing the same `rpc.Client` and the same
`decode.AddressResolver` (safe to share — it's an in-memory pubkey ->
address cache guarded by its own mutex) but writing to entirely separate
tables.

### mempool_state: explicit initialized/uninitialized, never a fake anchor

`mempool_state` is a singleton row (`name = 'main'`) whose `initialized`
boolean distinguishes "never successfully synchronized" from
"successfully synchronized" — including the legitimate "successfully
synchronized and currently empty" case (`initialized = true,
tx_count = 0`, which is NOT the same state as never having synchronized
at all). There is no fake/placeholder Core tip hash used as bootstrap
state, mirroring `sync_state`'s own `indexed_height = -1,
indexed_block_hash = NULL` bootstrap representation. A CHECK constraint
(`mempool_state_initialized_consistency`) makes every other combination
of fields structurally unrepresentable, written so it always evaluates
to TRUE or FALSE (never an ambiguous NULL that Postgres would treat as
"passes") — the same defensive pattern
`transaction_outputs_witness_metadata_consistency` in
`0001_initial.up.sql` already established.

### Isolated, normalized mempool tables

`mempool_transactions` / `mempool_inputs` / `mempool_input_witness` /
`mempool_outputs` / `mempool_output_addresses` /
`mempool_output_participants` / `mempool_dependencies` mirror the
confirmed schema's shape and invariants closely, with two deliberate
differences:

- A mempool transaction has exactly one observed witness serialization
  at a time (the current snapshot) — there is no confirmed-style
  `transactions`/`transaction_variants` split; txid and wtxid both live
  on `mempool_transactions` directly.
- `mempool_inputs.prev_txid`/`prev_vout_index` are `NOT NULL` — a
  coinbase-shaped transaction can never be valid mempool data (Core's
  mempool never contains one; this is defense in depth, enforced at
  three independent layers: `Candidate.validate()` in-memory,
  `mempool.Synchronizer.fetchAndDecode`'s explicit `IsCoinbase` check,
  and this NOT NULL).

`mempool_output_addresses` (balance-accounting destination, at most one
row per output) vs. `mempool_output_participants` (bare-multisig
co-signer identities) is the exact same structurally-enforced
distinction as confirmed `output_addresses`/`output_participants`
(trigger-enforced: a multisig output can never get an
`mempool_output_addresses` row, and vice versa) — never double-crediting
a multisig output's value once per participant.

`mempool_dependencies` — Core's per-entry `"depends"` list — is two
foreign keys into `mempool_transactions(txid)`, so a dependency can only
ever reference another transaction that is part of the SAME candidate
snapshot; a dangling mempool-only reference is rejected, not silently
stored (`TestReplaceSnapshot_DanglingDependencyRejected`).

### Full replacement, not additive deltas

`internal/mempool.Store.ReplaceSnapshot` is the only write path. Every
`mempool_*` child table cascades from `mempool_transactions` via
`ON DELETE CASCADE`, so `DELETE FROM mempool_transactions` atomically
clears the entire previous snapshot in one statement; the complete new
snapshot is then inserted, and `mempool_state` is updated LAST — all
inside one PostgreSQL transaction that also holds a `FOR UPDATE` lock on
the `mempool_state` row for its whole duration, serializing concurrent
writers exactly the way `internal/store`'s canonical-mutation lock does.
If any step fails, the whole transaction rolls back and the previous
complete snapshot is exactly as it was — a half-replaced mempool is
never observable. `generation` increments once per successful commit,
including a non-empty -> empty transition (an observed empty mempool is
real synchronized state), and never on a failed or skipped refresh.

### Dependency insertion is a two-pass, order-independent operation

`mempool_dependencies.txid` and `.depends_on_txid` are BOTH immediate
foreign keys into `mempool_transactions(txid)`. Candidate transactions
are not, and must not be required to be, topologically sorted by the
caller — `Synchronizer.fetchAndDecode` lists them in plain lexicographic
txid order (an opaque hash has no dependency meaning at all), so a
single "insert transaction, then its dependencies, then the next
transaction" pass fails the FK whenever a child transaction's txid
happens to sort before its parent's (a real, unremarkable case: nothing
about Core's own txid assignment correlates with mempool ancestry).

`ReplaceSnapshot` therefore inserts in two passes, inside the SAME
transaction: pass 1 inserts every `mempool_transactions` row (and every
non-dependency child row — inputs, witness, outputs,
addresses/participants) for the WHOLE candidate; only once every parent
txid this candidate could possibly reference is guaranteed to already
exist does pass 2 insert every `mempool_dependencies` row. This makes
`Store` accept a valid candidate in ANY slice order, proven directly by
`TestReplaceSnapshot_DependencyOrderIndependent` (a hand-ordered
child-before-parent PostgreSQL test) and
`TestRefreshOnce_ChildBeforeParentLexicalOrder` (the same shape driven
through the real `getrawmempool -> lexical sort -> decode ->
ReplaceSnapshot` pipeline). A dependency referencing a txid outside the
candidate entirely is still rejected, unchanged — both the in-memory
`Candidate.validate()` check and the FK itself still reject a genuinely
dangling reference; only the FALSE positive (a valid same-snapshot
reference rejected merely because of insertion order) was the bug.

### RPC response identity and metadata cross-checks

`fetchAndDecode` requires the transaction `decode.DecodeTransaction`
actually decodes from `GetRawTransactionVerbose(txid)` to have
`TxID == txid` — the exact txid that was requested. A mismatch
(`ErrRPCIdentityMismatch`) discards the whole candidate; it is
deliberately NOT treated as `ErrMempoolRace`: a disappeared or
newly-confirmed transaction is an expected, ordinary mempool race, but
Core answering a request for txid A with transaction B's data is a
wire-level integrity problem, and this project never attaches one
mempool entry's fee/time/dependency metadata to a different decoded
transaction merely because they arrived in the same fetch loop.

`getrawmempool`'s verbose per-entry `vsize` and `weight` are documented
by Core's own `help getrawmempool` as always-present — unlike
`fee`/`modifiedfee`/`descendantfees`/etc., which Core's help text
explicitly marks "(numeric, optional)". A first pass at this project
cross-checked BOTH fields for equality against the strictly decoded
`getrawtransaction` response's `VSize`/`Weight`; a subsequent internal
review round found the `vsize` half of that check invalid and it was
removed. **`weight` alone is cross-checked; `vsize` deliberately is
not.** The reason is a real semantic difference confirmed directly
against QOGE/qogecoin's own source (`help` text alone does not carry
this distinction and must not be relied on for it):

- Verbose `getrawtransaction`'s `vsize` (`core_write.cpp`'s `TxToUniv`)
  is computed purely as
  `ceil(GetTransactionWeight(tx) / WITNESS_SCALE_FACTOR)` — the plain
  BIP141 virtual size, nothing else.
- `getrawmempool`'s `vsize` (`txmempool.cpp`'s
  `CTxMemPoolEntry::GetTxSize`, called via `policy/settings.h`'s
  `nBytesPerSigOp`-aware overload) resolves to `policy.cpp`'s
  `GetVirtualTransactionSize`, which computes
  `ceil(max(weight, sigOpCost * bytes_per_sigop) / WITNESS_SCALE_FACTOR)`.
  This is Core's mempool **policy** virtual size: for a
  high-sigop-cost transaction, the sigop term can exceed the plain
  weight term, making this value legitimately larger than the
  `getrawtransaction` vsize for the exact same transaction.

Comparing these two values for equality would reject valid, honestly
reported mempool snapshots whenever a listed transaction's sigop cost
pushes its policy vsize above its BIP141 vsize — this is not RPC
corruption and must never be treated as such. `rpc.RawMempoolEntry.VSize`
is retained as mempool policy metadata (documented as such at its
declaration site) but is neither cross-checked nor persisted in Phase
2F.1; `CandidateTransaction.Transaction.VSize` always comes from the
strictly decoded `getrawtransaction` response and is never overwritten
by it. `TestRefreshOnce_MempoolPolicyVSizeMayDifferFromTransactionVSize`
pins this: a candidate whose mempool-entry vsize disagrees with its
decoded transaction vsize still publishes successfully, with the
persisted `vsize` column equal to the decoded transaction's own value.

`weight`, by contrast, has no such divergence: `entryToJSON`'s
`"weight"` (`e.GetTxWeight()`, i.e. the mempool entry's stored
`nTxWeight`) and `TxToUniv`'s `"weight"` (`GetTransactionWeight(tx)`)
both report the exact same underlying BIP141 weight for the same
transaction, with no policy adjustment on either side. A disagreement
there remains a genuine RPC/wire-integrity problem, so
`fetchAndDecode` still rejects the candidate when
`entry.Weight != nil && *entry.Weight != txn.Weight`, unchanged from
before this round.
`TestRefreshOnce_WeightMismatchRejected` covers this negative case:
previous snapshot retained, generation unchanged.

### Confirmed-transaction detection: `getrawtransaction`'s optional `blockhash`

`rpc.RawTransaction` gained one new optional field, `BlockHash`
(`json:"blockhash,omitempty"`) — Core's verbose transaction response
includes `"blockhash"` once a transaction is confirmed, and omits it
while unconfirmed. `getblock <hash> 2` never supplies this key (every
transaction in a block response is confirmed by construction), so
`DecodeBlock` never inspects it; only `internal/mempool`'s
`fetchAndDecode` checks it, to detect a transaction that became
confirmed between being listed by `getrawmempool` and being fetched by
`getrawtransaction` (see "Snapshot acquisition" below).

### Snapshot acquisition: anchored, not atomic — because it cannot be

Core RPC and PostgreSQL can never share one atomic snapshot. Each
`Synchronizer.refreshOnce` cycle closes the practical race window
(without eliminating it mathematically) by checking, in order:

1. Read the confirmed PostgreSQL checkpoint and Core's active tip;
   require them to match exactly (height AND hash) before doing any
   mempool work at all. If the confirmed index is still catching up to
   Core (initial historical sync, or merely lagging), this cycle
   publishes nothing (`ErrConfirmedIndexBehind`) — a mempool snapshot
   must never be anchored to a confirmed-state view a future read layer
   cannot trust.
2. `getrawmempool true` for the candidate transaction id set.
3. Sequentially (never unbounded-concurrent — spec item 32)
   `getrawtransaction <txid> true` and strictly decode each one via the
   SAME `decode.DecodeTransaction` confirmed-chain decoding uses. A
   transaction that disappeared (RPC error) or gained a `BlockHash`
   (became confirmed) mid-fetch discards the WHOLE candidate
   (`ErrMempoolRace`) — this is a normal, expected mempool race, not
   corruption; the previously committed snapshot is left untouched and
   the next poll cycle retries.
4. Re-read Core's active tip and the confirmed checkpoint; require BOTH
   to still exactly match their initial readings from step 1. Any
   mismatch discards the candidate the same way step 3 does.
5. Only then: `Store.ReplaceSnapshot`, anchored to the tip height/hash
   observed in step 4.

Every fee value (`getrawmempool`'s `fees.base`) is converted to exact
satoshis via the SAME `decode.DecodeAmount` confirmed-chain amounts use
— `json.Number` text/integer arithmetic only, never `float64`; more than
8 fractional digits, a negative value, or a malformed number all discard
the candidate rather than substituting a rounded or zero value
(`TestRefreshOnce_ExactFeeParsing`).

### Mempool failures never halt confirmed indexing

`Synchronizer.Run` never returns an error at all — every failure mode
(a plain RPC error, `ErrConfirmedIndexBehind`, `ErrMempoolRace`, a
malformed candidate) is logged (`Warn` for a race/failure, `Debug` for
the ordinary "confirmed index still catching up" skip) and retried on
the next poll cycle, preserving whatever snapshot was last committed.
`cmd/qoge-explorer`'s `runIndex` starts the mempool synchronizer in a
second goroutine bound to a context derived from (and always cancelled
alongside) confirmed indexing's own context, and `wg.Wait()`s for it
before returning — a confirmed-indexing halt (a real error, not just a
clean SIGINT/SIGTERM shutdown) still stops the mempool synchronizer
rather than leaking its goroutine, but a mempool failure can never
surface as `runIndex`'s exit code. Confirmed indexing's existing fatal-
error behavior (docs §18) is completely unchanged.

### Poll interval

A conservative package default, `mempool.DefaultPollInterval = 10s` — no
sub-second polling against Core, no new environment variable (a bare
package constant is sufficient for this phase, matching
`indexer.DefaultPollInterval`'s own precedent).

### Testing

Every `internal/mempool` test that needs PostgreSQL uses the same
disposable-per-test-schema pattern as `internal/store`/`internal/query`
(skips, never fails, when `QOGE_TEST_DATABASE_URL` is unset). Race/retry
behavior (confirmed-index-behind, Core tip moving mid-acquisition, DB tip
moving mid-acquisition, a listed transaction disappearing or becoming
confirmed mid-fetch, a transient RPC error) is tested with a fully
deterministic fake `RPCClient`/`ConfirmedTipReader` — no sleep-based
timing assumptions anywhere. P2QPK (structural witness v2/32, exact
17,088+32-byte spend witness, byte-exact on read-back), the P2TR
negative control, an explicitly-guarded txid != wtxid fixture, bare
multisig (participants never credited as a balance destination), and
OP_RETURN/zero-value-output persistence are all driven through the real
`rpc.RawTransaction -> decode.DecodeTransaction -> mempool.Candidate ->
Store.ReplaceSnapshot -> raw PostgreSQL verification` pipeline — never a
hand-inserted row that bypasses the decoder or classifier.
`TestConfirmedState_UnaffectedByMempoolReplacement` seeds a real
confirmed chain through `store.ApplyBlock`, captures exact per-table
content (not just row counts) of every confirmed table, runs successful
mempool replacements, and requires byte-identical confirmed state
afterward.

### Explicitly deferred to Phase 2F.2

No `/api/v1/mempool`, no `/mempool` page, no mempool navigation, no
transaction-detail fallback to mempool data, no address pending-activity
display, no `live.js` mempool polling, no pending/spendable-including-
mempool balance figures. Phase 2F.2 adds the read-only query layer, JSON
API, and functional (unstyled) SSR mempool pages on top of the
foundation this phase lays down; visual polishing remains deferred until
all functional phases are complete, per the project's existing phase
ordering.

## 23. Read-only mempool explorer: query layer, JSON API, SSR pages (Phase 2F.2)

Phase 2F.1 built the isolated, atomic mempool_* PostgreSQL cache and its
Core synchronizer. Phase 2F.2 makes that cache READABLE, adding nothing
to the write side: `internal/mempool`, `internal/rpc`, `internal/store`,
`internal/indexer`, `internal/decode`, `internal/script`,
`internal/chain`, and every migration are all byte-for-byte unchanged by
this phase — verified by an explicit `git diff --stat` against the
Phase 2F.1 baseline before this phase's PR was opened. All new
production code lives in `internal/query/mempool.go`,
`internal/api/handlers_mempool.go`, and `internal/web/handlers_mempool.go`
+ two new templates; `cmd/qoge-explorer/main.go` needed no change at all
(the mempool synchronizer was already wired into `index` only, back in
Phase 2F.1 — see §22).

### `serve` remains Core-independent

`internal/query`, `internal/api`, and `internal/web` never call
`getrawmempool`, `getrawtransaction`, or any other Core RPC method —
every mempool read in this phase is an ordinary `SELECT` against the
`mempool_*` tables, using exactly the same `querier`/`readTx` machinery
`internal/query`'s confirmed-chain code already used (see §19's
"Multi-statement read consistency"). `runServe` in
`cmd/qoge-explorer/main.go` is unchanged: it never constructs an
`internal/rpc.Client` and never receives Core RPC credentials — confirmed
directly by this phase's `serve`-binary route smoke (§ "Serve smoke",
below), which ran the real binary against an isolated PostgreSQL schema
with no Core process reachable at all.

### Read model: `internal/query/mempool.go`

A new file, deliberately NOT depending on `internal/mempool.Store` (the
Phase 2F.1 writer) — it reads the `mempool_*` schema directly, exactly
the way the confirmed query layer reads `blocks`/`transactions` directly
rather than importing `internal/store`. The only cross-package test-time
dependency is in `*_test.go` files (`mempool_fixtures_test.go` in each of
`internal/query`, `internal/api`, `internal/web`), which build real
`mempool_*` rows through the real `internal/mempool.Store.ReplaceSnapshot`
writer — never ad-hoc SQL — mirroring exactly how those same test files
already build confirmed fixtures through `internal/store.Store.ApplyBlock`.

### `MempoolState`: initialized vs. synchronized-empty vs. never-synchronized

`query.MempoolState` is a read of `mempool_state('main')` PLUS, from the
SAME PostgreSQL snapshot, the confirmed chain's own `sync_state`
checkpoint (`statusFrom` — the exact function `query.Status` already
uses). The two are compared to derive `Status`/`Stale`:

- **`"uninitialized"`** (`Initialized=false`): `internal/mempool` has
  never successfully published a snapshot. `Stale` is unconditionally
  `false` in this case — never `true` — so a reader can never mistake
  "never synchronized" for "synchronized but out of date"; this is a
  third, explicit state, not a degenerate case of the other two (spec
  requirement: "stale is not equivalent to true; represent uninitialized
  explicitly").
- **`"fresh"`** (`Initialized=true`, `mempool_state.core_tip_height`/
  `core_tip_hash` == `sync_state.indexed_height`/`indexed_block_hash`
  exactly): the cached rows were observed against the exact confirmed
  chain state a reader is also seeing right now.
- **`"stale"`** (`Initialized=true`, anchor != confirmed tip): the
  confirmed indexed tip no longer matches the snapshot's anchor. The only
  fact this state asserts is that inequality — **it does NOT imply
  forward advancement**. The common case is confirmed indexing having
  advanced past the anchor since the snapshot was acquired, but the same
  mismatch can equally result from a same-height canonical reorg (a
  different hash at the same height), a rollback to a lower height, or a
  replacement tip at another hash entirely — `query.MempoolState`'s
  comparison (`mempoolStateFrom`) only ever checks `!=`, never an
  ordering relationship, and the wording used everywhere this state is
  surfaced (JSON, HTML) is written to match: "the confirmed indexed tip
  no longer matches this mempool snapshot's anchor", never "advanced
  past"/"ahead of"/"newer than". Either way, **this is normal
  asynchronous operation, not corruption** — Phase 2F.1's synchronizer
  publishes a snapshot anchored at whatever tip it observed at
  acquisition time (§22 "Snapshot acquisition: anchored, not atomic"),
  and the confirmed checkpoint can move again (forward, or sideways via
  reorg) before the NEXT mempool cycle runs. A reader must never be left
  to assume a stale snapshot is current, nor that it proves current Core
  mempool membership — every mempool response, JSON or HTML, surfaces
  `stale`/`status` explicitly rather than silently presenting cached rows
  as live.

Both `MempoolOverview` (list) and `mempoolTransactionDetail` (detail)
call the same `mempoolStateFrom` helper against their own already-open
`readTx`, so the freshness comparison always shares one snapshot with
every other read in the same response — never two independent queries
that could observe two different instants.

### Repeatable-read snapshots, extended to the mempool cache

Every multi-statement mempool read opens the SAME `s.readTx` (`BEGIN
ISOLATION LEVEL REPEATABLE READ, READ ONLY`) the confirmed-chain query
layer already uses — no new transaction machinery was introduced.
`MempoolOverview` reads `MempoolState` then one page of transactions from
one snapshot; `mempoolTransactionDetail` reads the transaction body,
`MempoolState`, dependencies, inputs (+ witness), and outputs (+
participants) from one snapshot. A concurrent
`internal/mempool.Store.ReplaceSnapshot` can therefore never produce a
response mixing rows from two different generations —
`TestSnapshotConsistency_MempoolOverview_ConcurrentReplacement` and
`TestSnapshotConsistency_MempoolDetail_ConcurrentReplacement` (in
`internal/query`) prove this directly: a REAL concurrent
`ReplaceSnapshot` is driven from generation 1 to generation 2 while an
in-flight read's snapshot is deliberately held open via the same
`snapshotTestHook` rendezvous pattern `snapshot_test.go` already uses for
confirmed-chain reorgs (never sleep-based). The in-flight read always
observes the complete, coherent PRE-replacement generation (state
generation N, only generation-N rows, never a generation-N+1 row mixed
in) — PostgreSQL's REPEATABLE READ isolation guarantees this at the
engine level once the snapshot is fixed, so the in-flight detail read for
a transaction that gets deleted-and-replaced mid-read never has to
"discover" a not-found partway through; it simply keeps seeing the
pre-replacement row for the rest of that transaction.

### Generation-safe pagination

A mempool list cursor (`query.MempoolCursor`) carries THREE fields, not
two: `Generation`, `EntryTime`, `TxID` — ordered `entry_time DESC, txid
DESC` (deterministic, never relying on PostgreSQL's physical row order).
Because `ReplaceSnapshot` always fully replaces `mempool_transactions`
(never an additive delta — see §22 "Full replacement, not additive
deltas"), a cursor's `(entry_time, txid)` pair only has meaning relative
to the EXACT generation it was read from. `MempoolOverview` therefore
requires `cursor.Generation == state.Generation` (read from the same
snapshot) before honoring the cursor at all; a mismatch is
`query.ErrMempoolGenerationChanged`, never silently paginated against the
new snapshot. `internal/api` maps this to HTTP 409 with the stable
machine-readable code `mempool_generation_changed`; `internal/web`
redirects to a clean `/mempool` first page instead of rendering an error
— a stale cursor from a page the user still has open is expected,
ordinary behavior, not a fault.
`TestMempoolOverview_GenerationSafePagination` /
`TestMempoolEndpoint_GenerationCursor` /
`TestMempoolPage_GenerationCursorRedirects` cover the query/API/web
layers respectively.

### `GET /api/v1/mempool` and `GET /api/v1/mempool/tx/{id}`

Mirror the confirmed-chain endpoints' conventions exactly:
`?include_witness=true` opt-in (default hidden — see below),
`?limit=`/keyset-cursor pagination, a `query.ErrNotFound` mapped to 404,
a JSON error envelope with a stable `code` field for every non-2xx
response. `GET /api/v1/mempool`'s cursor is three query parameters
(`generation`, `before_entry_time`, `before_txid`) that must be supplied
together or all omitted — a partial cursor is 400.

An **uninitialized** mempool cache is a valid HTTP 200 response with
`state.initialized=false` and no transactions — never a fake
synchronized-and-empty response (`TestMempoolEndpoint_Uninitialized`). A
**stale** cache still returns its cached rows, but with `stale:true` and
both the mempool anchor and the actual confirmed indexed tip visible
side by side (`TestMempoolEndpoint_Stale`) — a caller is never left to
assume a stale snapshot describes Core's current mempool.

`GET /api/v1/mempool/tx/{id}` tries `id` as a txid first, then (only on a
miss) as a wtxid — identical fallback order to the confirmed
`/api/v1/tx/{id}` endpoint. A missing id in an initialized mempool is a
plain 404: never a Core RPC fallback, and never old/expired mempool
history, since none exists by design (full-replacement snapshots only —
§22).

### `GET /mempool` and `GET /mempool/tx/{id}` (SSR)

Minimal, functional, unstyled-beyond-existing-conventions HTML pages
using the same `html/template` layout/security-header machinery every
other page already uses (`app.css`'s existing `.kv-table`/`.data-table`/
`.io-card`/`.witness-block` classes — no new CSS was needed). `/mempool`
shows the state summary (initialized/fresh/stale, generation, observed
time, anchor height/hash, confirmed indexed height, tx count, total
vsize, total fees) above a paginated transaction table.
`/mempool/tx/{id}` labels the transaction **"UNCONFIRMED / MEMPOOL"**
when the snapshot is fresh, or the qualified **"Present in cached
mempool snapshot ... snapshot stale"** wording when it is not — a stale
cache is never presented as proof of current mempool membership. Neither
page participates in `live.js`'s auto-refresh (deliberately deferred —
see "No browser mempool polling yet" below); a stale snapshot page
requires a manual reload to pick up a newer generation, same as any other
manually-refreshed page in this project today.

A single "Mempool" link was added to the shared site header
(`templates/header.tmpl`) — the existing nav/header layout itself was not
redesigned.

### Default witness omission, P2QPK exact opt-in, P2TR distinct

Identical policy to the confirmed transaction page, reusing the exact
same `query.WitnessItem` type (no new witness-item shape was
introduced): `mempool_input_witness` rows are only fetched from
PostgreSQL at all when the caller passes `?include_witness=true`
(`mempoolWitnessByVin`, mirroring `witnessByVin`). Default responses
report each witness item's size only; explicit opt-in returns the exact
bytes, never truncated — including a full 17,088-byte P2QPK signature
item and its accompanying exactly-32-byte public-key item
(`TestMempoolTransaction_P2QPKAndP2TR`,
`TestMempoolTransactionWitness_DefaultOmitsRaw_OptInIncludesIt`,
`TestMempoolTxWitness_DefaultHidden_OptInIncludes` across the three
layers). `script_type`/`witness_version`/`witness_program` are read
as-is from what Phase 2F.1's writer already persisted — this phase
performs NO script classification of its own; P2TR (`witness_version=1`)
and P2QPK (`witness_version=2`) remain visibly distinct through query,
API, and SSR alike, and the 32-byte P2QPK output witness program is
never called a public key anywhere in this phase's code or templates.

### No derived input accounting; no policy-feerate confusion

Mempool transaction detail shows input OUTPOINTS only (`prev_txid`,
`prev_vout_index`) — this phase does not compute total input value,
pending spend/receive balances, or a "spendable after mempool" figure.
Fee is displayed exactly as `internal/mempool` already persisted it
(`fee_satoshis`, Core-authored exact value via
`decode.DecodeAmount` — never reconstructed here by resolving prevouts).
Separately: the persisted `vsize` a mempool transaction list/detail shows
is always the transaction's own BIP141 vsize as decoded from
`getrawtransaction` (frozen by Phase 2F.1 — see §22's vsize/weight
semantics section); Core's sigop-adjusted mempool POLICY vsize was never
persisted in Phase 2F.1 and this phase does not compute or display a
"Core mempool feerate" from `fee / persisted vsize` — doing so would
misrepresent Core's actual policy feerate, which depends on the (not
persisted) policy vsize denominator.

### Bare multisig: participants shown separately, never a monetary destination

`MempoolOutputDetail.Address` is `nil` for a multisig output (no single
balance-accounting destination exists for it — mirrors the confirmed
schema's `output_addresses` vs. `output_participants` split exactly, see
§7/§13.A); `Participants` lists the co-signer identities separately, and
neither the JSON API nor the HTML template ever sums an output's value
once per participant or calls a participant an "owner" of the full
output value (`TestMempoolTransaction_Multisig`,
`TestMempoolEndpoint_ListAndDetail`-adjacent coverage).

### Search: mempool as a lower-priority fallback, confirmed always wins

`/search`'s fixed 64-hex lookup order gained two new steps at the END:
`BlockByHash` -> `TransactionByTxID` -> `TransactionByWTxID` ->
`MempoolTransactionByTxID` -> `MempoolTransactionByWTxID` — the first hit
redirects there, and confirmed lookups are always attempted before
mempool lookups. `handleSearch` issues these as separate, sequential
`query.Store` calls (not one cross-domain `REPEATABLE READ` composite
read spanning confirmed AND mempool tables together — no such composite
exists), so this guarantees confirmed precedence WITHIN one stable
database state, not an absolute point-in-time guarantee across an
arbitrary concurrent transition between the individual calls. Within a
stable state this ordering is deterministic:
`TestSearch_ConfirmedTakesPriorityOverMempool` seeds the SAME txid as
both a real confirmed transaction (via `Store.ApplyBlock`) and a cached
mempool row (via `mempool.Store.ReplaceSnapshot`) — the exact state that
can genuinely arise during the asynchronous interval after confirmed
indexing observes a transaction but before the next mempool
`ReplaceSnapshot` clears its now-stale cached row — and requires the
redirect still lands on `/tx/{id}`, never `/mempool/tx/{id}`. A
concurrent transition landing between the individual confirmed and
mempool calls is out of scope for Phase 2F.2: a mempool hit's destination
page always carries its own honest fresh/stale qualification (§ above)
regardless, so worst case a transiently-stale mempool redirect still
never claims current mempool membership — search itself performs no
additional filtering (`TestSearch_MempoolFallback`).

### Explicitly NOT added in this phase

No mempool balance/activity on `/address/{address}` (confirmed-only,
unchanged); no `live.js` mempool polling (mempool pages require a manual
reload to see a newer generation); no pending/spendable-including-mempool
balance anywhere; no new script classification logic in
`internal/query`/`internal/api`/`internal/web` (script_type is read
as-is from what Phase 2F.1 already classified and persisted); no schema
change (`migrations/` is byte-for-byte unchanged); no change to Phase
2F.1 write-side semantics (`ReplaceSnapshot`, synchronization interval,
Core anchor acquisition, dependency persistence, generation semantics,
vsize/weight semantics, mempool transaction decoding — all untouched).
Final visual/UI polishing remains deferred until all functional phases
are complete, per the project's existing phase ordering.

### Serve smoke

The real `qoge-explorer serve` binary (built from this branch, no source
changes to `cmd/`) was run against an isolated, freshly migrated
PostgreSQL schema seeded with one confirmed block and one mempool
snapshot, with NO Core process reachable and NO Core RPC environment
variables set. Every route responded 200:
`/`, `/blocks`, `/block/{id}`, `/tx/{id}`, `/address/{address}`,
`/mempool`, `/mempool/tx/{id}`, `/api/v1/status`, `/api/v1/mempool`,
`/api/v1/mempool/tx/{id}`, `/healthz`, `/readyz`. A follow-up manual
fresh/stale cycle against the same running process — (A) confirmed tip ==
mempool anchor -> `fresh`; (B) confirmed tip advanced without a new
mempool snapshot -> `stale`; (C) a new mempool snapshot anchored to the
advanced tip -> `fresh` again — was verified through both the JSON API
and the rendered HTML page at each step, confirming the freshness
comparison behaves correctly against a real running server, not just
inside the test suite.

## 24. Consensus deployment observer: write/cache foundation (Phase 2G.1)

Phase 2G.1 adds explorer-native observation of Qogecoin Core's BIP9/
versionbits deployment state (`getdeploymentinfo`), following the exact
shape Phase 2F.1 established for the mempool cache: a dedicated package
(`internal/deployments`) that calls Core RPC, strictly decodes the
response, and atomically replaces an isolated PostgreSQL cache — used by
`qoge-explorer index` only. This phase is the write/cache foundation
ONLY: no public deployment API, no deployment web page, no P2QPK
activation UI, no signalling visualization, no browser polling. Those
belong to Phase 2G.2.

### Core is authoritative

The explorer never recreates VersionBits state transitions, never infers
LOCKED_IN/ACTIVE itself, and never counts signalling blocks
independently of Core's own reported statistics. `internal/deployments`
strictly decodes and caches whatever `getdeploymentinfo` returns; it has
no opinion of its own about what a deployment's status "should" be.

### Scope: BIP9 deployments only

`getdeploymentinfo` reports two kinds of deployment: `"buried"` (static
historical consensus rules — e.g. BIP34/65/66, CSV, SegWit on a real
chain — activated unconditionally at a fixed height, with no ongoing
status model) and `"bip9"` (the versionbits signalling/threshold model:
`defined` -> `started` -> `locked_in` -> `active`, or `started` ->
`failed`). The existing `chain_deployments` table (migration
0001_initial) is shaped around the BIP9 status enum
(`defined`/`started`/`locked_in`/`active`/`failed`) plus `since_height` —
there is no equivalent status concept for a buried deployment, so this
phase does not invent one. `internal/deployments.DecodeDeploymentInfo`
decodes every deployment object just enough to prove it isn't malformed
(so a genuinely broken response is still rejected outright), then
persists ONLY the entries whose `"type"` is `"bip9"`; buried entries are
intentionally dropped before ever reaching a database write.

P2QPK is a BIP9 deployment (Core's `DeploymentInfo` includes
`Consensus::DEPLOYMENT_P2QPK` alongside every other enabled deployment),
so it is naturally included by this same, non-special-cased path — the
deployment map key `internal/deployments` persists is whatever name Core
reports (`DeploymentName`'s result), never hardcoded, and its
status/statistics are never estimated, forced, or precomputed locally.

### Schema: `deployment_state` singleton (migration 0003)

`migrations/0001_initial.*` and `migrations/0002_mempool_cache.*` are
untouched by this phase. A new migration,
`migrations/0003_deployment_state.*`, adds exactly one table:
`deployment_state`, a singleton (`name = 'main'`) shaped identically to
`mempool_state` (§22) — `initialized`, `generation`, `core_tip_height`,
`core_tip_hash`, `deployment_count`, `observed_at` — with the same
CHECK-constraint-enforced UNINITIALIZED/INITIALIZED consistency:

- **UNINITIALIZED** (`initialized=false`): never successfully observed —
  `generation=0`, `core_tip_height`/`core_tip_hash`/`observed_at` all
  NULL, `deployment_count=0`.
- **INITIALIZED** (`initialized=true`): successfully observed at least
  once — `generation>=1`, a valid Core tip anchor, `observed_at` set.
  This includes the legitimate **initialized-empty** case
  (`deployment_count=0`, `initialized=true`): a real observation that
  happened to find zero BIP9 deployments is synchronized state, not
  "nothing happened" — indistinguishable from "never synchronized" by
  `SELECT count(*) FROM chain_deployments` alone, which is exactly why
  this explicit state table exists (mirrors `mempool_state`'s identical
  rationale in §22).

`chain_deployments` (from migration 0001) remains the per-deployment
cache table; this phase does not rewrite 0001 merely because that table
was created there. `deployment_state.deployment_count` and
`chain_deployments`'s actual row count are kept in lockstep by
`Store.ReplaceSnapshot` writing both inside the same transaction (below).
Migration 0003's down migration removes only `deployment_state` —
`chain_deployments` is never dropped by rolling 0003 back, since it
belongs to 0001 and schema v2 must remain valid afterward. A migration
round-trip test (v2 -> v3 -> v2 -> v3) with real pre-existing confirmed,
address, and mempool data seeded beforehand proves none of it is
disturbed by 0003 in either direction.

### Anchored acquisition: explicit hash, before/after tip checks

`internal/deployments.Synchronizer.refreshOnce` never calls
`getdeploymentinfo` with no argument (Core's implicit, possibly-moving,
active tip). It always supplies an EXPLICIT block hash — the confirmed
PostgreSQL checkpoint's own hash — and follows the same anchored
acquisition shape Phase 2F.1 established for the mempool cache (§22):

1. Read the confirmed checkpoint (`sync_state` via `store.Checkpoint`).
   An uninitialized checkpoint (`Height < 0`, the explorer has never
   synced) skips publication.
2. Read Core's active tip (`GetBlockCount` then `GetBlockHash(height)` —
   a height-indexed lookup, not `GetBestBlockHash`, mirroring
   `indexer.Indexer.confirmCaughtUp`'s style). Require it equal the
   confirmed checkpoint; if the confirmed index is still behind (or
   simply disagrees at the same height, e.g. mid-reorg), this cycle
   skips publication — historical confirmed indexing is never blocked
   or slowed by this check.
3. Call `getdeploymentinfo <confirmed-checkpoint-hash>`. Require the
   response's own self-reported `hash`/`height` match what was queried;
   a disagreement here is a wire-integrity problem and discards the
   candidate exactly like a race would.
4. Strictly decode every deployment; keep only `type == "bip9"`.
5. Re-read Core's active tip and the confirmed checkpoint.
6. Require every anchor observed during this cycle — the initial
   confirmed tip, the initial Core tip, `getdeploymentinfo`'s own
   response anchor, the final Core tip, and the final confirmed tip —
   all agree. If anything moved, the candidate is discarded and the
   previous snapshot is preserved untouched; the next poll cycle retries.

`getdeploymentinfo <hash>` can inspect a known block by hash regardless
of whether it's still Core's active tip — querying the confirmed
checkpoint's own hash is not, by itself, proof that hash is STILL Core's
canonical tip if a reorg happens concurrently with acquisition. The
before/after active-tip re-reads close that practical race window. As
with every other cross-system check in this project (§18's
`confirmCaughtUp`, §22's mempool acquisition), this is NOT a claim of
mathematical cross-system atomicity — Core and PostgreSQL can never share
one atomic snapshot. A genuine mismatch here just means this cycle
publishes nothing; deterministic, race-free fixture tests (a fake RPC
client and a fake confirmed-tip reader, no sleeps) cover every one of
these skip/discard paths, including a temporary RPC failure and a
malformed deployment object, in both cases proving the previously
committed snapshot survives untouched.

### Atomic replacement: `internal/deployments.Store.ReplaceSnapshot`

One PostgreSQL transaction: lock `deployment_state('main')` FOR UPDATE
(serializing concurrent writers, the same row-lock-first pattern
`mempool.Store.ReplaceSnapshot` and `internal/store`'s
`lockCheckpoint` use), `DELETE FROM chain_deployments` (full replacement,
never incremental deltas), insert every surviving BIP9 deployment,
`UPDATE deployment_state` LAST (`initialized=true`,
`generation=generation+1`, the anchor, `deployment_count`,
`observed_at`), then `COMMIT`. Any error at any step rolls back the
whole transaction — the prior complete snapshot is exactly as it was; a
half-replaced deployment cache is never observable. A reader always sees
either the whole of generation N or the whole of generation N+1, never a
partial mix. `generation` increments on every successful commit —
including a non-empty -> empty transition, an empty -> non-empty
transition, or a snapshot whose values happen to be byte-identical to
the previous one — and is left unchanged by any failed or skipped
observation. Each row of one snapshot shares the exact same
`checked_at`/`observed_at` timestamp
(`internal/deployments.Candidate.ObservedAt`), computed once per
acquisition cycle.

Store-level tests (real PostgreSQL, isolated per-test schema) exercise:
initial publication (uninitialized -> generation 1, correct rows,
correct anchor); full replacement (snapshot A's `p2qpk started` becomes
B's `p2qpk locked_in` — only B is visible after commit); non-empty ->
empty and empty -> non-empty transitions; and a failure injected AFTER
`DELETE`/some rows' `INSERT` have already run inside the transaction
(a deployment candidate with syntactically invalid JSON bytes, which
passes Go-level candidate validation but is rejected by PostgreSQL's
JSONB column type at `INSERT` time) — proving the previous snapshot's
rows and `deployment_state` row are both completely preserved, with
`generation` unchanged and none of the failed candidate's rows leaked
in.

### Exact raw JSON preservation

`chain_deployments.raw_json` for a persisted BIP9 deployment is the exact
semantic JSON object `getdeploymentinfo` returned for that deployment —
never reconstructed from the normalized `name`/`status`/`since_height`
columns after decoding. `internal/rpc.RawDeploymentInfo` deliberately
keeps each deployment's bytes as `json.RawMessage` rather than eagerly
unmarshaling into a typed map value, so the bytes that reach
`Store.ReplaceSnapshot` and PostgreSQL are the same bytes Core sent, not
a round-tripped Go-struct re-marshal (which could reorder fields or
reformat numbers even when semantically identical). JSONB's own
canonical key-ordering on read-back is acceptable per this phase's
scope — the invariant is no field loss, no invented fields, and exact
value preservation, not byte-for-byte key order — and a round-trip test
using a realistic BIP9 object with every documented field
(`type`, `active`, `bip9.bit`, `start_time`, `timeout`,
`min_activation_height`, `status`, `since`, `status_next`,
`statistics.period`, `statistics.threshold`, `statistics.elapsed`,
`statistics.count`, `statistics.possible`, `signalling`) present
confirms semantic equality survives both the decode step and a real
PostgreSQL write/read cycle.

### Strict decoding, honoring Core's actual field optionality

`internal/deployments.DecodeDeploymentInfo` rejects a malformed or
out-of-range field rather than silently normalizing it: hash format
(lowercase 64-hex), non-negative heights, deployment name shape, `type`
constrained to `"buried"`/`"bip9"`, BIP9 `status`/`status_next`
constrained to the exact five-value enum, `bit` in `[0,28]` when
present, and `statistics` internal consistency (`period > 0`,
`0 <= elapsed <= period`, `0 <= count <= elapsed`, and
`0 < threshold <= period` when threshold is present) when Core supplies
a `statistics` object at all. Crucially, the decoder never REQUIRES a
field in a state where Core legitimately omits it: `bit`, `statistics`,
`statistics.threshold`, `statistics.possible`, `signalling`, and the
top-level `height` (on `bip9`-type entries) are all optional and decoded
as such — DEFINED carries no signalling statistics, LOCKED_IN may omit
`threshold`/`possible`, and ACTIVE may omit `bit`/`statistics`/
`signalling` entirely. Fixtures covering every BIP9 status
(defined/started/locked_in/active/failed) deliberately do NOT give every
state an identical optional-field shape, so the decoder is proven
against Core's actual behavior rather than one hand-invented fixed JSON
shape. (Fixture note: any deployment JSON used in this phase's tests,
including the `p2qpk`-named fixtures, uses synthetic constants for
illustration — none are asserted as real Qogecoin mainnet consensus
values.)

**Required-field presence, not just value ranges.** A field Core always
emits (top-level `height`/`deployments`; per-deployment `active`; every
`bip9` deployment's `start_time`/`timeout`/`min_activation_height`/
`status`/`status_next`/`since`; `bip9.statistics`'s `period`/`elapsed`/
`count` whenever `statistics` itself is present; a buried deployment's
`height`) is decoded through a `*T` field in `internal/rpc`'s DTOs
(`RawDeploymentInfo.Height`, `RawDeployment.Active`,
`RawBIP9Deployment.StartTime`/`Timeout`/`MinActivationHeight`/`Since`,
`RawBIP9Statistics.Period`/`Elapsed`/`Count`), never a bare Go scalar.
This matters because encoding/json cannot otherwise tell "the field is
absent, or explicitly `null`" apart from "the field is present with its
legitimate zero value" — several of these fields (`since`, `start_time`,
`elapsed`, `count`, buried `height`) can genuinely BE zero, so a bare
`int64`/`bool` would let a malformed or truncated Core response pass
strict decoding by silently becoming its zero value. `Deployments`
itself stays a plain (non-pointer) map: Go's own `encoding/json`
already leaves a map field `nil` for both a missing key and an explicit
`null`, while allocating a non-nil (possibly empty) map for a present
`{}` — exactly the three-way distinction needed to reject a
missing/null `deployments` object while still accepting Core's
legitimate `"deployments": {}` (an initialized, zero-BIP9-deployment
snapshot; see the STATE section above). `Status`/`StatusNext` stay
plain strings deliberately: their missing/null zero value (`""`) is
already outside `validBIP9Statuses` and is rejected by the existing
enum check without needing a pointer. This required/optional split is
verified directly against `QOGE/qogecoin` stable's
`rpc/blockchain.cpp` (`SoftForkDescPushBack`), not inferred from
observed responses alone, and re-confirmed by feeding a real captured
`getdeploymentinfo` response from a synced mainnet node through the
corrected decoder.

### Non-mutation of confirmed and mempool state

`internal/deployments` never reads or writes any confirmed-chain table
(`sync_state`, `blocks`, `transactions`, `transaction_variants`,
`block_transactions`, `transaction_inputs`, `transaction_input_witness`,
`transaction_outputs`, `output_addresses`, `output_participants`,
`utxo_state`, `addresses`) or any `mempool_*` table. Tests seed real
confirmed state via `internal/store.Store.ApplyBlock` and a real mempool
snapshot via `internal/mempool.Store.ReplaceSnapshot`, fingerprint every
one of those tables (row count plus an order-independent content
digest), run a deployment `ReplaceSnapshot`, and require every
fingerprint is byte-for-byte unchanged.

### Runtime integration: `index` only, `serve` untouched

The deployment observer is wired into `qoge-explorer index`'s existing
orchestration in `cmd/qoge-explorer/main.go`, exactly alongside the
Phase 2F.1 mempool synchronizer: its own `context.WithCancel` derived
from the top-level shutdown context, its own `sync.WaitGroup`, started
in a goroutine right after the mempool synchronizer's. If confirmed
indexing (`idx.Run`) halts — cleanly or fatally — both the mempool
context and the deployment context are cancelled and both goroutines are
waited on before the process exits, so neither can leak regardless of
why confirmed indexing stopped. `qoge-explorer serve` requires no code
change at all: it never constructs an `internal/deployments.Synchronizer`
or an `internal/rpc.Client`, and continues to require no Core RPC
credentials whatsoever — deployment observation, like mempool
observation, needs live Core RPC access that `serve` deliberately never
has.

### Failure policy: non-fatal, always

A deployment observation failure — RPC unavailable, a malformed
`getdeploymentinfo` response, a PostgreSQL write failure, a Core-tip or
confirmed-tip race during acquisition — is logged (`Warn` for an
unexpected failure, `Debug` for the ordinary "confirmed index not
caught up yet" skip) and retried on the next poll cycle
(`DefaultPollInterval` = 30s, deliberately conservative: deployment state
changes on the order of blocks/weeks, not seconds). It NEVER halts
confirmed indexing, and it never halts or is halted by mempool
observation — the three observers are independent goroutines sharing
only the same underlying Core RPC client and the same read-only
confirmed-checkpoint reader.

### Explicitly deferred to Phase 2G.2

No `/deployments` or `/consensus` page, no `/api/v1/deployments`
endpoint, no P2QPK status indicator on the homepage, no activation
progress bar, no signalling visualization, no browser polling of
deployment state. `internal/query`, `internal/api`, and `internal/web`
are byte-for-byte unchanged by this phase — verified by an explicit
`git diff --stat` against the Phase 2F.2 baseline before this phase's PR
was opened, exactly as §23 did against the Phase 2F.1 baseline. This
phase's only job is making sure Core's deployment state is being
observed, strictly validated, and safely cached; giving anyone a way to
actually SEE that cache is Phase 2G.2's job.
