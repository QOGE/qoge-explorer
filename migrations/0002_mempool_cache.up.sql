-- Phase 2F.1: mempool cache foundation.
--
-- Design invariants (see docs/ARCHITECTURE.md §22 for full rationale):
--
--   1. Mempool state is a COMPLETELY SEPARATE model from the confirmed
--      chain. No mempool_* table has a REQUIRED foreign key into
--      transactions/blocks/utxo_state/addresses/sync_state, and no
--      confirmed table is touched by anything in this migration. A
--      mempool transaction is ephemeral: it can disappear, be replaced,
--      expire, conflict, or become confirmed, and must never become
--      permanent historical data merely by having been observed once.
--   2. Full-replacement semantics. mempool_state.generation increments
--      once per successfully committed snapshot; there are no incremental
--      counters. The mempool synchronizer (internal/mempool) always
--      deletes the entire previous transaction set and inserts the entire
--      new one inside a single transaction — see the ON DELETE CASCADE
--      chain below, which lets a single `DELETE FROM mempool_transactions`
--      atomically clear every child row.
--   3. mempool_state distinguishes "never successfully synchronized" from
--      "successfully synchronized and currently empty" via an explicit
--      `initialized` flag — never a fake/bootstrap block hash.
--   4. txid and wtxid are preserved exactly as Core reports them, never
--      derived. Both are unique within the current snapshot.
--   5. A mempool transaction can never be coinbase-shaped: prev_txid/
--      prev_vout_index on mempool_inputs are NOT NULL, unlike the
--      confirmed schema's transaction_inputs (which allows a coinbase
--      input via NULL prevout).
--   6. output_addresses (balance-accounting destination) vs.
--      output_participants (bare-multisig co-signer identity) mirrors the
--      confirmed schema's distinction exactly, enforced by the same
--      shape of triggers, for the same reason: never double-count a
--      multisig output's value once per participant.
--   7. mempool_dependencies rows must reference another transaction in
--      the SAME candidate snapshot — enforced structurally by both
--      columns being foreign keys into mempool_transactions(txid), never
--      a dangling mempool-only reference.

-- ── mempool_state: singleton snapshot state ──────────────────────────────
-- Exactly one row ('main'). UNINITIALIZED means "never successfully
-- synchronized" (initialized=false, generation=0, observed_at=NULL, no
-- Core anchor, zero counts). INITIALIZED means "successfully synchronized
-- at least once" (initialized=true, generation>=1, observed_at set, a
-- valid Core tip anchor) — including the legitimate "successfully
-- synchronized and currently empty" case (tx_count=0 with initialized=true
-- is NOT the same state as never having synchronized at all).
CREATE TABLE mempool_state (
    name                TEXT PRIMARY KEY,
    initialized         BOOLEAN NOT NULL DEFAULT FALSE,
    generation          BIGINT NOT NULL DEFAULT 0,
    core_tip_height     BIGINT,
    core_tip_hash       TEXT,
    tx_count            INT NOT NULL DEFAULT 0,
    total_vsize         BIGINT NOT NULL DEFAULT 0,
    total_fee_satoshis  BIGINT NOT NULL DEFAULT 0,
    observed_at         TIMESTAMPTZ,
    CONSTRAINT mempool_state_generation_nonnegative CHECK (generation >= 0),
    CONSTRAINT mempool_state_tx_count_nonnegative CHECK (tx_count >= 0),
    CONSTRAINT mempool_state_total_vsize_nonnegative CHECK (total_vsize >= 0),
    CONSTRAINT mempool_state_total_fee_nonnegative CHECK (total_fee_satoshis >= 0),
    CONSTRAINT mempool_state_core_tip_height_nonnegative CHECK (core_tip_height IS NULL OR core_tip_height >= 0),
    CONSTRAINT mempool_state_core_tip_hash_format CHECK (core_tip_hash IS NULL OR core_tip_hash ~ '^[0-9a-f]{64}$'),
    -- Every field below is written so it always evaluates to TRUE or
    -- FALSE, never NULL, for the same reason as
    -- transaction_outputs_witness_metadata_consistency in
    -- 0001_initial.up.sql: an ambiguous NULL result would let a
    -- half-initialized row silently pass this CHECK.
    CONSTRAINT mempool_state_initialized_consistency CHECK (
        (
            NOT initialized
            AND generation = 0
            AND observed_at IS NULL
            AND core_tip_height IS NULL
            AND core_tip_hash IS NULL
            AND tx_count = 0
            AND total_vsize = 0
            AND total_fee_satoshis = 0
        ) OR (
            initialized
            AND generation >= 1
            AND observed_at IS NOT NULL
            AND core_tip_height IS NOT NULL
            AND core_tip_hash IS NOT NULL
        )
    )
);
INSERT INTO mempool_state (name) VALUES ('main');

-- ── mempool_transactions: current-snapshot transaction bodies ───────────
-- One row per mempool transaction, keyed by txid. Unlike the confirmed
-- `transactions`/`transaction_variants` split, a mempool transaction has
-- exactly one observed witness serialization at a time (the current
-- snapshot), so txid and wtxid both live on this single table — there is
-- no cross-block "same txid, different wtxid" concern for an ephemeral,
-- fully-replaced cache.
CREATE TABLE mempool_transactions (
    txid                TEXT PRIMARY KEY,
    wtxid               TEXT NOT NULL,
    version             BIGINT NOT NULL,
    locktime            BIGINT NOT NULL,
    size                INT NOT NULL,
    vsize               INT NOT NULL,
    weight              INT NOT NULL,
    fee_satoshis        BIGINT NOT NULL,
    entry_time          BIGINT NOT NULL, -- unix seconds, Core's mempool entry "time"
    entry_height        BIGINT,          -- Core's mempool entry "height", if reported
    replaceable         BOOLEAN,         -- Core's "bip125-replaceable", if reported (NULL = unknown)
    observed_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (wtxid),
    CONSTRAINT mempool_transactions_txid_format CHECK (txid ~ '^[0-9a-f]{64}$'),
    CONSTRAINT mempool_transactions_wtxid_format CHECK (wtxid ~ '^[0-9a-f]{64}$'),
    -- version/locktime are RPC/consensus-facing uint32, same pattern as
    -- 0001_initial.up.sql's transactions table.
    CONSTRAINT mempool_transactions_version_uint32_range CHECK (version >= 0 AND version <= 4294967295),
    CONSTRAINT mempool_transactions_locktime_uint32_range CHECK (locktime >= 0 AND locktime <= 4294967295),
    CONSTRAINT mempool_transactions_size_nonnegative CHECK (size >= 0),
    CONSTRAINT mempool_transactions_vsize_nonnegative CHECK (vsize >= 0),
    CONSTRAINT mempool_transactions_weight_nonnegative CHECK (weight >= 0),
    CONSTRAINT mempool_transactions_fee_nonnegative CHECK (fee_satoshis >= 0),
    CONSTRAINT mempool_transactions_entry_height_nonnegative CHECK (entry_height IS NULL OR entry_height >= 0)
);

-- ── mempool_inputs: one row per vin — coinbase is NEVER valid here ──────
-- prev_txid/prev_vout_index are NOT NULL (unlike confirmed
-- transaction_inputs): a coinbase-shaped transaction must never enter the
-- mempool cache (see docs/ARCHITECTURE.md §22 and internal/mempool's
-- candidate validation, which rejects it before this table is ever
-- reached — this NOT NULL is defense in depth, not the only check).
CREATE TABLE mempool_inputs (
    txid                TEXT NOT NULL REFERENCES mempool_transactions (txid) ON DELETE CASCADE,
    vin_index           INT NOT NULL,
    prev_txid           TEXT NOT NULL,
    prev_vout_index     INT NOT NULL,
    script_sig          BYTEA,
    sequence            BIGINT NOT NULL,
    PRIMARY KEY (txid, vin_index),
    CONSTRAINT mempool_inputs_vin_index_nonnegative CHECK (vin_index >= 0),
    CONSTRAINT mempool_inputs_prev_txid_format CHECK (prev_txid ~ '^[0-9a-f]{64}$'),
    CONSTRAINT mempool_inputs_prev_vout_index_nonnegative CHECK (prev_vout_index >= 0),
    CONSTRAINT mempool_inputs_sequence_uint32_range CHECK (sequence >= 0 AND sequence <= 4294967295)
);
CREATE INDEX mempool_inputs_prevout_idx ON mempool_inputs (prev_txid, prev_vout_index);

-- Witness data lives separately, same rationale as confirmed
-- transaction_input_witness — ordinary listing must never have to pull a
-- 17,088-byte P2QPK signature along for the ride.
CREATE TABLE mempool_input_witness (
    txid                TEXT NOT NULL,
    vin_index           INT NOT NULL,
    item_index          INT NOT NULL, -- position in the witness stack, 0 = bottom
    data                BYTEA NOT NULL,
    PRIMARY KEY (txid, vin_index, item_index),
    FOREIGN KEY (txid, vin_index) REFERENCES mempool_inputs (txid, vin_index) ON DELETE CASCADE,
    CONSTRAINT mempool_input_witness_item_index_nonnegative CHECK (item_index >= 0)
);

-- ── mempool_outputs: one row per vout, byte-for-byte, never re-classified ──
-- script_type/witness_version/witness_program are copied from
-- internal/script.Classify's already-reviewed output (via
-- internal/decode.DecodeTransaction) — this schema does not classify
-- scripts itself, mirroring confirmed transaction_outputs.
CREATE TABLE mempool_outputs (
    txid                TEXT NOT NULL REFERENCES mempool_transactions (txid) ON DELETE CASCADE,
    vout_index          INT NOT NULL,
    value_satoshis      BIGINT NOT NULL,
    script_pubkey       BYTEA NOT NULL,
    script_type         TEXT NOT NULL,
    witness_version     INT,
    witness_program     BYTEA,
    PRIMARY KEY (txid, vout_index),
    CONSTRAINT mempool_outputs_vout_index_nonnegative CHECK (vout_index >= 0),
    CONSTRAINT mempool_outputs_value_nonnegative CHECK (value_satoshis >= 0),
    CONSTRAINT mempool_outputs_script_type_valid CHECK (script_type IN (
        'p2pk', 'p2pkh', 'p2sh', 'p2wpkh', 'p2wsh', 'p2tr', 'p2qpk',
        'nulldata', 'multisig', 'unknown_witness', 'unknown'
    )),
    CONSTRAINT mempool_outputs_witness_version_range CHECK (
        witness_version IS NULL OR (witness_version >= 0 AND witness_version <= 16)
    ),
    -- Identical structural (never consensus) shape to
    -- transaction_outputs_witness_metadata_consistency in
    -- 0001_initial.up.sql — see that constraint's comment for why every
    -- branch is written to avoid Postgres's NULL-passes-CHECK loophole.
    CONSTRAINT mempool_outputs_witness_metadata_consistency CHECK (
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
                witness_version IS NOT NULL AND witness_version > 0
                AND witness_program IS NOT NULL
                AND octet_length(witness_program) BETWEEN 2 AND 40
                AND NOT (witness_version = 1 AND octet_length(witness_program) = 32)
                AND NOT (witness_version = 2 AND octet_length(witness_program) = 32)
            WHEN 'unknown' THEN
                (witness_version IS NULL AND witness_program IS NULL)
                OR (
                    witness_version IS NOT NULL AND witness_version = 0
                    AND witness_program IS NOT NULL
                    AND octet_length(witness_program) BETWEEN 2 AND 40
                    AND octet_length(witness_program) NOT IN (20, 32)
                )
            ELSE
                witness_version IS NULL AND witness_program IS NULL
        END
    )
);
CREATE INDEX mempool_outputs_script_type_idx ON mempool_outputs (script_type);

-- ── mempool_output_addresses: BALANCE-ACCOUNTING destinations only ──────
-- At most one row per output, same PRIMARY KEY shape as confirmed
-- output_addresses, for the same reason: a second address for the same
-- output would double-count it.
CREATE TABLE mempool_output_addresses (
    txid                TEXT NOT NULL,
    vout_index          INT NOT NULL,
    address             TEXT NOT NULL,
    PRIMARY KEY (txid, vout_index),
    FOREIGN KEY (txid, vout_index) REFERENCES mempool_outputs (txid, vout_index) ON DELETE CASCADE
);
CREATE INDEX mempool_output_addresses_address_idx ON mempool_output_addresses (address);

CREATE OR REPLACE FUNCTION mempool_output_addresses_reject_multisig() RETURNS trigger AS $$
DECLARE
    st TEXT;
BEGIN
    SELECT script_type INTO st FROM mempool_outputs WHERE txid = NEW.txid AND vout_index = NEW.vout_index;
    IF st = 'multisig' THEN
        RAISE EXCEPTION 'mempool_output_addresses: output %:% is script_type=multisig; multisig outputs belong in mempool_output_participants, not mempool_output_addresses', NEW.txid, NEW.vout_index;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER mempool_output_addresses_reject_multisig_trigger
    BEFORE INSERT OR UPDATE ON mempool_output_addresses
    FOR EACH ROW EXECUTE FUNCTION mempool_output_addresses_reject_multisig();

-- ── mempool_output_participants: MULTISIG co-signer identities only ─────
CREATE TABLE mempool_output_participants (
    txid                TEXT NOT NULL,
    vout_index          INT NOT NULL,
    address             TEXT NOT NULL,
    pubkey              BYTEA NOT NULL,
    PRIMARY KEY (txid, vout_index, address),
    FOREIGN KEY (txid, vout_index) REFERENCES mempool_outputs (txid, vout_index) ON DELETE CASCADE
);
CREATE INDEX mempool_output_participants_address_idx ON mempool_output_participants (address);

CREATE OR REPLACE FUNCTION mempool_output_participants_require_multisig() RETURNS trigger AS $$
DECLARE
    st TEXT;
BEGIN
    SELECT script_type INTO st FROM mempool_outputs WHERE txid = NEW.txid AND vout_index = NEW.vout_index;
    IF st IS DISTINCT FROM 'multisig' THEN
        RAISE EXCEPTION 'mempool_output_participants: output %:% is script_type=%, not multisig', NEW.txid, NEW.vout_index, st;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER mempool_output_participants_require_multisig_trigger
    BEFORE INSERT OR UPDATE ON mempool_output_participants
    FOR EACH ROW EXECUTE FUNCTION mempool_output_participants_require_multisig();

-- ── mempool_dependencies: in-mempool parent relationships ───────────────
-- Both columns are foreign keys into mempool_transactions(txid): a
-- dependency can only ever reference another transaction that is part of
-- the SAME candidate snapshot, never a dangling/external reference. Core's
-- verbose mempool "depends" list is the source (see internal/mempool).
CREATE TABLE mempool_dependencies (
    txid                TEXT NOT NULL REFERENCES mempool_transactions (txid) ON DELETE CASCADE,
    depends_on_txid     TEXT NOT NULL REFERENCES mempool_transactions (txid) ON DELETE CASCADE,
    PRIMARY KEY (txid, depends_on_txid),
    CONSTRAINT mempool_dependencies_not_self CHECK (txid <> depends_on_txid)
);
CREATE INDEX mempool_dependencies_depends_on_idx ON mempool_dependencies (depends_on_txid);
