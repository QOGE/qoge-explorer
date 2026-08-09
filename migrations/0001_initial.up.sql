-- Phase 2B.1: final relational schema.
--
-- Design invariants (see docs/ARCHITECTURE.md for full rationale):
--
--   1. transaction != transaction occurrence. `transactions` holds the
--      immutable transaction body; `block_transactions` records which
--      block(s) a given txid appeared in. The same txid CAN be linked to
--      two different block_hash rows (e.g. across a reorg) without
--      duplicating the transaction body.
--   2. Immutable body vs canonical derived state. `transaction_outputs`
--      describes an output as it was created — value, script — forever.
--      `utxo_state` is the separate, mutable table describing whether that
--      output is currently unspent on the CANONICAL chain. A reorg rolls
--      back utxo_state (and the `addresses` cache), never the immutable
--      transaction/block bodies.
--   3. Reorg keeps an audit trail. Blocks are never deleted, only marked
--      canonical = false. Their transactions, inputs, witness data, and
--      outputs all remain queryable.
--   4. output_addresses (balance-accounting destinations) is structurally
--      separate from output_participants (bare-multisig co-signer
--      identities). Triggers below make it impossible to insert a
--      multisig output into output_addresses, or a non-multisig output
--      into output_participants.
--   5. Raw binary data (scripts, witness items, coinbase data) is BYTEA.
--      Hashes/txids remain lowercase hex TEXT — see docs/ARCHITECTURE.md
--      §3 for why, and note the format CHECK constraints below that make
--      that an enforced invariant rather than just a convention.
--   6. script_type = 'p2qpk' is the ONLY source of truth for P2QPK
--      classification — there is no separate is_p2qpk boolean to drift out
--      of sync with it. A structural CHECK (not a consensus check) ties
--      script_type = 'p2qpk' to witness_version = 2 AND a 32-byte program.

-- ── sync checkpoint ─────────────────────────────────────────────────────
-- Bootstrap state (an explorer that has never synced) is represented
-- explicitly, not with a fake genesis-adjacent hash: indexed_height = -1,
-- indexed_block_hash = NULL. The CHECK constraint makes the two other
-- combinations (height=-1 with a hash, or height>=0 without one)
-- unrepresentable.
CREATE TABLE sync_state (
    name                TEXT PRIMARY KEY,
    indexed_height      BIGINT NOT NULL DEFAULT -1,
    indexed_block_hash  TEXT,
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT sync_state_height_floor CHECK (indexed_height >= -1),
    CONSTRAINT sync_state_hash_format CHECK (
        indexed_block_hash IS NULL OR indexed_block_hash ~ '^[0-9a-f]{64}$'
    ),
    CONSTRAINT sync_state_bootstrap_consistency CHECK (
        (indexed_height = -1 AND indexed_block_hash IS NULL) OR
        (indexed_height >= 0 AND indexed_block_hash IS NOT NULL)
    )
);
INSERT INTO sync_state (name, indexed_height, indexed_block_hash) VALUES ('main', -1, NULL);

-- ── blocks ──────────────────────────────────────────────────────────────
-- canonical = true/false replaces the Phase-1 sketch's "orphaned" flag
-- (inverse polarity, same idea) — kept for the entire lifetime of the row;
-- a block is never deleted, only demoted off the canonical chain.
CREATE TABLE blocks (
    hash                TEXT PRIMARY KEY,
    height              BIGINT NOT NULL,
    prev_hash           TEXT REFERENCES blocks (hash), -- NULL only for genesis
    merkle_root         TEXT NOT NULL,
    "time"              BIGINT NOT NULL, -- block header timestamp, unix seconds
    bits                TEXT NOT NULL,   -- compact-target, hex, as reported by Core
    difficulty          DOUBLE PRECISION NOT NULL, -- display only; never used for consensus decisions
    nonce               BIGINT NOT NULL,
    size                INT NOT NULL,
    weight              INT NOT NULL,
    tx_count            INT NOT NULL,
    canonical           BOOLEAN NOT NULL DEFAULT TRUE,
    orphaned_at         TIMESTAMPTZ,
    indexed_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT blocks_hash_format CHECK (hash ~ '^[0-9a-f]{64}$'),
    CONSTRAINT blocks_prev_hash_format CHECK (prev_hash IS NULL OR prev_hash ~ '^[0-9a-f]{64}$'),
    CONSTRAINT blocks_merkle_root_format CHECK (merkle_root ~ '^[0-9a-f]{64}$'),
    CONSTRAINT blocks_height_nonnegative CHECK (height >= 0),
    CONSTRAINT blocks_size_nonnegative CHECK (size >= 0),
    CONSTRAINT blocks_weight_nonnegative CHECK (weight >= 0),
    CONSTRAINT blocks_tx_count_nonnegative CHECK (tx_count >= 0),
    -- nonce is consensus-wire uint32; stored as BIGINT (Postgres has no
    -- native unsigned type) but range-checked to match.
    CONSTRAINT blocks_nonce_uint32_range CHECK (nonce >= 0 AND nonce <= 4294967295),
    CONSTRAINT blocks_orphaned_at_consistency CHECK (
        (canonical AND orphaned_at IS NULL) OR (NOT canonical AND orphaned_at IS NOT NULL)
    )
);
-- Exactly one canonical block per height — the core reorg invariant.
CREATE UNIQUE INDEX blocks_height_canonical_uidx ON blocks (height) WHERE canonical;
CREATE INDEX blocks_prev_hash_idx ON blocks (prev_hash);

-- ── transactions (immutable body; block-independent) ────────────────────
CREATE TABLE transactions (
    txid                TEXT PRIMARY KEY,
    version             INT NOT NULL,
    locktime            BIGINT NOT NULL,
    size                INT NOT NULL,
    vsize               INT NOT NULL,
    weight              INT NOT NULL,
    is_coinbase         BOOLEAN NOT NULL,
    fee_satoshis        BIGINT, -- NULL for coinbase; deferred/optional otherwise (not computed in Phase 2B.1)
    indexed_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT transactions_txid_format CHECK (txid ~ '^[0-9a-f]{64}$'),
    CONSTRAINT transactions_size_nonnegative CHECK (size >= 0),
    CONSTRAINT transactions_vsize_nonnegative CHECK (vsize >= 0),
    CONSTRAINT transactions_weight_nonnegative CHECK (weight >= 0),
    -- locktime is consensus-wire uint32.
    CONSTRAINT transactions_locktime_uint32_range CHECK (locktime >= 0 AND locktime <= 4294967295),
    CONSTRAINT transactions_fee_nonnegative CHECK (fee_satoshis IS NULL OR fee_satoshis >= 0),
    -- A coinbase transaction has no "fee" concept (its value comes from
    -- subsidy + collected fees, not from spending prior outputs) — see
    -- docs/ARCHITECTURE.md §6. NULL is required, not merely allowed, when
    -- is_coinbase is true.
    CONSTRAINT transactions_coinbase_has_no_fee CHECK (NOT is_coinbase OR fee_satoshis IS NULL)
);

-- ── block_transactions (occurrence: which block(s) contained this txid) ─
-- The same txid can be linked to more than one block_hash — e.g. a
-- transaction that appears in an orphaned block and is later re-included
-- in the new canonical block during a reorg — without duplicating the
-- transaction body. block_height is derived automatically by trigger
-- (below) from blocks.height for the given block_hash, so it can never
-- drift out of sync with the blocks table.
CREATE TABLE block_transactions (
    block_hash          TEXT NOT NULL REFERENCES blocks (hash),
    tx_index            INT NOT NULL,
    txid                TEXT NOT NULL REFERENCES transactions (txid),
    block_height        BIGINT NOT NULL,
    PRIMARY KEY (block_hash, tx_index),
    UNIQUE (block_hash, txid),
    CONSTRAINT block_transactions_tx_index_nonnegative CHECK (tx_index >= 0)
);
CREATE INDEX block_transactions_txid_idx ON block_transactions (txid);
CREATE INDEX block_transactions_block_height_idx ON block_transactions (block_height);

CREATE OR REPLACE FUNCTION block_transactions_set_height() RETURNS trigger AS $$
BEGIN
    SELECT height INTO NEW.block_height FROM blocks WHERE hash = NEW.block_hash;
    IF NEW.block_height IS NULL THEN
        RAISE EXCEPTION 'block_transactions: no block found for block_hash %', NEW.block_hash;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER block_transactions_set_height_trigger
    BEFORE INSERT OR UPDATE ON block_transactions
    FOR EACH ROW EXECUTE FUNCTION block_transactions_set_height();

-- ── transaction inputs (raw, one row per vin — immutable, block-independent) ──
CREATE TABLE transaction_inputs (
    txid                TEXT NOT NULL REFERENCES transactions (txid),
    vin_index           INT NOT NULL,
    prev_txid           TEXT,   -- NULL for coinbase
    prev_vout_index     INT,    -- NULL for coinbase
    coinbase            BYTEA,  -- set only for the coinbase input
    script_sig          BYTEA,
    sequence            BIGINT NOT NULL,
    PRIMARY KEY (txid, vin_index),
    CONSTRAINT transaction_inputs_vin_index_nonnegative CHECK (vin_index >= 0),
    CONSTRAINT transaction_inputs_prev_txid_format CHECK (prev_txid IS NULL OR prev_txid ~ '^[0-9a-f]{64}$'),
    CONSTRAINT transaction_inputs_prev_vout_index_nonnegative CHECK (prev_vout_index IS NULL OR prev_vout_index >= 0),
    CONSTRAINT transaction_inputs_coinbase_xor_prevout CHECK (
        (prev_txid IS NULL AND prev_vout_index IS NULL AND coinbase IS NOT NULL) OR
        (prev_txid IS NOT NULL AND prev_vout_index IS NOT NULL AND coinbase IS NULL)
    ),
    -- sequence is consensus-wire uint32.
    CONSTRAINT transaction_inputs_sequence_uint32_range CHECK (sequence >= 0 AND sequence <= 4294967295)
);
CREATE INDEX transaction_inputs_prevout_idx ON transaction_inputs (prev_txid, prev_vout_index);

-- Witness stack data lives in its own table so ordinary listing/detail
-- queries never have to pull ~17KB P2QPK signatures along for the ride —
-- see docs/ARCHITECTURE.md §8.
CREATE TABLE transaction_input_witness (
    txid                TEXT NOT NULL,
    vin_index           INT NOT NULL,
    item_index          INT NOT NULL, -- position in the witness stack, 0 = bottom
    data                BYTEA NOT NULL,
    PRIMARY KEY (txid, vin_index, item_index),
    FOREIGN KEY (txid, vin_index) REFERENCES transaction_inputs (txid, vin_index),
    CONSTRAINT transaction_input_witness_item_index_nonnegative CHECK (item_index >= 0)
);

-- ── transaction outputs (immutable body; the canonical script/value record) ──
CREATE TABLE transaction_outputs (
    txid                    TEXT NOT NULL REFERENCES transactions (txid),
    vout_index              INT NOT NULL,
    value_satoshis          BIGINT NOT NULL,
    script_pubkey           BYTEA NOT NULL,
    script_type             TEXT NOT NULL,
    witness_version         INT,   -- NULL for non-witness scripts
    witness_program         BYTEA, -- NULL for non-witness scripts
    PRIMARY KEY (txid, vout_index),
    CONSTRAINT transaction_outputs_vout_index_nonnegative CHECK (vout_index >= 0),
    CONSTRAINT transaction_outputs_value_nonnegative CHECK (value_satoshis >= 0),
    CONSTRAINT transaction_outputs_script_type_valid CHECK (script_type IN (
        'p2pk', 'p2pkh', 'p2sh', 'p2wpkh', 'p2wsh', 'p2tr', 'p2qpk',
        'nulldata', 'multisig', 'unknown_witness', 'unknown'
    )),
    CONSTRAINT transaction_outputs_witness_version_range CHECK (
        witness_version IS NULL OR (witness_version >= 0 AND witness_version <= 16)
    ),
    -- Structural (not consensus) witness-metadata consistency, one rule per
    -- script_type. This replaces two earlier, narrower constraints
    -- (presence-consistency + a P2QPK-only check) that together had a real
    -- NULL loophole: Postgres CHECK constraints treat a NULL result as
    -- "satisfied," and `script_type <> 'p2qpk' OR (witness_version = 2 AND
    -- ...)` evaluates to NULL — not FALSE — when witness_version is NULL,
    -- so a row with script_type='p2qpk' and witness_version/witness_program
    -- both NULL previously passed. Every branch below is written so it
    -- always evaluates to TRUE or FALSE, never NULL: every comparison on a
    -- nullable column is preceded by an `IS NOT NULL`/`IS NULL` check
    -- (themselves always TRUE/FALSE), and AND/OR are only ever combined
    -- with values that are already guaranteed non-NULL by a preceding
    -- clause — so a NULL operand can never reach the top-level AND/OR and
    -- produce an ambiguous "pass by default" result.
    --
    -- This is a byte-length/version-number check only — never script
    -- execution or signature verification.
    CONSTRAINT transaction_outputs_witness_metadata_consistency CHECK (
        CASE script_type
            WHEN 'p2wpkh' THEN
                witness_version IS NOT NULL AND witness_version = 0
                AND witness_program IS NOT NULL AND octet_length(witness_program) = 20
            WHEN 'p2wsh' THEN
                witness_version IS NOT NULL AND witness_version = 0
                AND witness_program IS NOT NULL AND octet_length(witness_program) = 32
            WHEN 'p2tr' THEN
                witness_version IS NOT NULL AND witness_version = 1
                AND witness_program IS NOT NULL AND octet_length(witness_program) = 32
            WHEN 'p2qpk' THEN
                witness_version IS NOT NULL AND witness_version = 2
                AND witness_program IS NOT NULL AND octet_length(witness_program) = 32
            WHEN 'unknown_witness' THEN
                -- A structurally valid witness program whose version/length
                -- combination isn't one of the recognized types above — by
                -- definition its version is >0 (version 0 is fully defined
                -- by BIP141 with only the two lengths handled above; an
                -- off-length v0 program classifies as 'unknown', not
                -- 'unknown_witness' — see docs/ARCHITECTURE.md §7).
                witness_version IS NOT NULL AND witness_version > 0
                AND witness_program IS NOT NULL
            WHEN 'unknown' THEN
                -- Two legitimate shapes: a script that isn't a witness
                -- program at all (no witness metadata), OR a witness
                -- version-0 program whose length is neither 20 nor 32 —
                -- version 0 is fully BIP141-defined with only those two
                -- standard lengths, so Core's own Solver() (and this
                -- project's classifier — see docs/ARCHITECTURE.md §7)
                -- treats an off-length v0 program as unknown rather than
                -- unknown_witness, but the version/program are still
                -- present and worth keeping.
                (witness_version IS NULL AND witness_program IS NULL)
                OR (
                    witness_version IS NOT NULL AND witness_version = 0
                    AND witness_program IS NOT NULL AND octet_length(witness_program) NOT IN (20, 32)
                )
            ELSE
                -- Legacy (non-witness) types — p2pk, p2pkh, p2sh, nulldata,
                -- multisig — carry no witness metadata.
                witness_version IS NULL AND witness_program IS NULL
        END
    )
);
CREATE INDEX transaction_outputs_script_type_idx ON transaction_outputs (script_type);

-- ── output_addresses: BALANCE-ACCOUNTING destinations only ──────────────
-- At most ONE row per output — enforced by PRIMARY KEY (txid, vout_index),
-- not (txid, vout_index, address). Every currently-supported script type
-- other than MULTISIG resolves to a single owning address (or none, for
-- unparseable/unknown scripts); MULTISIG is deliberately never represented
-- here at all — see output_participants below and docs/ARCHITECTURE.md
-- §7/§13.A. A second, different address for the same output would silently
-- double-count that output's value in balance accounting even for an
-- ordinary non-multisig output, which is exactly the class of bug this
-- table exists to make structurally impossible.
CREATE TABLE output_addresses (
    txid                TEXT NOT NULL,
    vout_index          INT NOT NULL,
    address             TEXT NOT NULL,
    PRIMARY KEY (txid, vout_index),
    FOREIGN KEY (txid, vout_index) REFERENCES transaction_outputs (txid, vout_index)
);
CREATE INDEX output_addresses_address_idx ON output_addresses (address);

-- Enforced, not just documented: an output whose transaction_outputs row
-- is script_type='multisig' can never be inserted into output_addresses.
-- This is the concrete mechanism behind "make accidental multisig balance
-- multiplication difficult" (task item 7) — a multisig output's value must
-- never be credited to N participant balances independently.
CREATE OR REPLACE FUNCTION output_addresses_reject_multisig() RETURNS trigger AS $$
DECLARE
    st TEXT;
BEGIN
    SELECT script_type INTO st FROM transaction_outputs WHERE txid = NEW.txid AND vout_index = NEW.vout_index;
    IF st = 'multisig' THEN
        RAISE EXCEPTION 'output_addresses: output %:% is script_type=multisig; multisig outputs belong in output_participants, not output_addresses (see docs/ARCHITECTURE.md §7)', NEW.txid, NEW.vout_index;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER output_addresses_reject_multisig_trigger
    BEFORE INSERT OR UPDATE ON output_addresses
    FOR EACH ROW EXECUTE FUNCTION output_addresses_reject_multisig();

-- ── output_participants: MULTISIG co-signer identities, display/search only ──
-- Never joined into balance aggregation (addresses below) — an m-of-n
-- multisig output is jointly controlled by all n named keys, not owned by
-- each of them independently.
CREATE TABLE output_participants (
    txid                TEXT NOT NULL,
    vout_index          INT NOT NULL,
    address             TEXT NOT NULL, -- derived from the participant pubkey, display/search only
    pubkey              BYTEA NOT NULL,
    PRIMARY KEY (txid, vout_index, address),
    FOREIGN KEY (txid, vout_index) REFERENCES transaction_outputs (txid, vout_index)
);
CREATE INDEX output_participants_address_idx ON output_participants (address);

-- Symmetric enforcement: only a script_type='multisig' output may have
-- output_participants rows.
CREATE OR REPLACE FUNCTION output_participants_require_multisig() RETURNS trigger AS $$
DECLARE
    st TEXT;
BEGIN
    SELECT script_type INTO st FROM transaction_outputs WHERE txid = NEW.txid AND vout_index = NEW.vout_index;
    IF st IS DISTINCT FROM 'multisig' THEN
        RAISE EXCEPTION 'output_participants: output %:% is script_type=%, not multisig', NEW.txid, NEW.vout_index, st;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER output_participants_require_multisig_trigger
    BEFORE INSERT OR UPDATE ON output_participants
    FOR EACH ROW EXECUTE FUNCTION output_participants_require_multisig();

-- ── utxo_state: CANONICAL, MUTABLE spend state (separate from the immutable output body) ──
-- One row per output that has ever existed on the canonical chain. Rolled
-- back/rebuilt during reorgs; transaction_outputs itself is never touched.
--
-- Relational integrity beyond "this hash looks like a hash": the FKs below
-- prove the claimed creation/spending occurrences are real, not merely
-- well-formed strings —
--   (creation_block_hash, txid) -> block_transactions (block_hash, txid):
--     the transaction that created this output really occurred in the
--     claimed creation block.
--   (spending_txid, spending_vin_index) -> transaction_inputs (txid, vin_index):
--     the claimed spending input really exists.
--   (spending_block_hash, spending_txid) -> block_transactions (block_hash, txid):
--     the claimed spending transaction really occurred in the claimed
--     spending block.
-- All three composite FKs reference either a table's PRIMARY KEY or (for
-- block_transactions) its UNIQUE (block_hash, txid) constraint. Postgres's
-- default MATCH SIMPLE means a composite FK is satisfied whenever ANY of
-- its columns is NULL — correct here because utxo_state_spent_consistency
-- below already guarantees the three spending_* columns are NULL together
-- or NOT NULL together, so there's no partial-NULL state for MATCH SIMPLE
-- to paper over.
CREATE TABLE utxo_state (
    txid                    TEXT NOT NULL,
    vout_index              INT NOT NULL,
    creation_block_hash     TEXT NOT NULL,
    creation_block_height   BIGINT NOT NULL,
    spent                   BOOLEAN NOT NULL DEFAULT FALSE,
    spending_txid           TEXT,
    spending_vin_index      INT,
    spending_block_hash     TEXT,
    spending_block_height   BIGINT,
    PRIMARY KEY (txid, vout_index),
    FOREIGN KEY (txid, vout_index) REFERENCES transaction_outputs (txid, vout_index),
    FOREIGN KEY (creation_block_hash, txid) REFERENCES block_transactions (block_hash, txid),
    FOREIGN KEY (spending_txid, spending_vin_index) REFERENCES transaction_inputs (txid, vin_index),
    FOREIGN KEY (spending_block_hash, spending_txid) REFERENCES block_transactions (block_hash, txid),
    CONSTRAINT utxo_state_creation_block_hash_format CHECK (creation_block_hash ~ '^[0-9a-f]{64}$'),
    CONSTRAINT utxo_state_spending_block_hash_format CHECK (spending_block_hash IS NULL OR spending_block_hash ~ '^[0-9a-f]{64}$'),
    CONSTRAINT utxo_state_spending_txid_format CHECK (spending_txid IS NULL OR spending_txid ~ '^[0-9a-f]{64}$'),
    CONSTRAINT utxo_state_spending_vin_index_nonnegative CHECK (spending_vin_index IS NULL OR spending_vin_index >= 0),
    CONSTRAINT utxo_state_spent_consistency CHECK (
        (NOT spent AND spending_txid IS NULL AND spending_vin_index IS NULL AND spending_block_hash IS NULL AND spending_block_height IS NULL) OR
        (spent AND spending_txid IS NOT NULL AND spending_vin_index IS NOT NULL AND spending_block_hash IS NOT NULL AND spending_block_height IS NOT NULL)
    )
);
CREATE INDEX utxo_state_unspent_idx ON utxo_state (txid, vout_index) WHERE NOT spent;
CREATE INDEX utxo_state_spending_txid_idx ON utxo_state (spending_txid);
CREATE INDEX utxo_state_creation_block_hash_idx ON utxo_state (creation_block_hash);
CREATE INDEX utxo_state_spending_block_hash_idx ON utxo_state (spending_block_hash);

-- creation_block_height and spending_block_height are mechanically derived
-- from blocks.height, never trusted from the caller — same pattern as
-- block_transactions_set_height above. Kept as real columns (not obtained
-- purely via JOIN) because they're useful for range queries, but a caller
-- cannot make either one persist a value that disagrees with the
-- corresponding block's actual height.
CREATE OR REPLACE FUNCTION utxo_state_derive_heights() RETURNS trigger AS $$
BEGIN
    SELECT height INTO NEW.creation_block_height FROM blocks WHERE hash = NEW.creation_block_hash;
    IF NEW.creation_block_height IS NULL THEN
        RAISE EXCEPTION 'utxo_state: no block found for creation_block_hash %', NEW.creation_block_hash;
    END IF;

    IF NEW.spending_block_hash IS NOT NULL THEN
        SELECT height INTO NEW.spending_block_height FROM blocks WHERE hash = NEW.spending_block_hash;
        IF NEW.spending_block_height IS NULL THEN
            RAISE EXCEPTION 'utxo_state: no block found for spending_block_hash %', NEW.spending_block_hash;
        END IF;
    ELSE
        NEW.spending_block_height := NULL;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER utxo_state_derive_heights_trigger
    BEFORE INSERT OR UPDATE ON utxo_state
    FOR EACH ROW EXECUTE FUNCTION utxo_state_derive_heights();

-- ── addresses: derived balance cache (canonical derived state, not a source of truth) ──
-- Rebuilt by re-aggregating output_addresses (destination rows ONLY —
-- never output_participants) joined against utxo_state, for the touched
-- addresses, inside the same block transaction as normal indexing. Every
-- write is a SET of a freshly computed absolute value, never an increment
-- — see docs/ARCHITECTURE.md §4.
CREATE TABLE addresses (
    address                     TEXT PRIMARY KEY,
    total_received_satoshis     BIGINT NOT NULL DEFAULT 0,
    total_sent_satoshis         BIGINT NOT NULL DEFAULT 0,
    balance_satoshis             BIGINT NOT NULL DEFAULT 0,
    tx_count                     INT NOT NULL DEFAULT 0,
    first_seen_height            BIGINT,
    last_seen_height             BIGINT,
    updated_at                   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT addresses_received_nonnegative CHECK (total_received_satoshis >= 0),
    CONSTRAINT addresses_sent_nonnegative CHECK (total_sent_satoshis >= 0),
    CONSTRAINT addresses_tx_count_nonnegative CHECK (tx_count >= 0)
);

-- ── deployment status cache (display only; Core remains authoritative) ──
CREATE TABLE chain_deployments (
    name                TEXT PRIMARY KEY, -- e.g. 'p2qpk'
    status              TEXT NOT NULL,    -- defined/started/locked_in/active/failed
    since_height        BIGINT,
    raw_json            JSONB NOT NULL,
    checked_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
