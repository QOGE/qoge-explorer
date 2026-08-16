-- Phase 2H.4a: address balance distribution rollup foundation.
--
-- This migration adds two tables — an eight-row fixed bucket rollup and a
-- singleton anchor — and nothing else. It is SCHEMA ONLY: both bucket rows
-- and the anchor are seeded at their zero/uninitialized values below; a
-- fresh database's live indexing keeps them correct incrementally
-- (internal/store/distribution.go), and an already-indexed database
-- upgraded onto this migration needs the separate, explicit
-- `qoge-explorer backfill-address-distribution` operator command — never
-- hidden inside a migration (see docs/ARCHITECTURE.md §30, mirroring §27's
-- own "never hidden inside a migration" policy for block_supply_rollup).
--
-- This is an ADDRESS balance distribution — never a holder/wallet/entity
-- distribution (see docs/ARCHITECTURE.md §30). No percentages, no
-- concentration statistics, no public read surface: this phase is
-- foundation only.
--
-- ── address_balance_distribution: eight fixed buckets ──────────────────
--
-- One row per fixed, predefined balance bucket, keyed by a stable
-- bucket_id (never a surrogate/serial key — the bucket_id IS the stable
-- identity a future public reader keys off of). Boundaries are exact
-- integer satoshis (1 QOGE = 100,000,000 satoshis) — never a float, and
-- never chain.Amount.WholeQOGE(), which truncates and is deliberately
-- lossy for classification (see internal/store/distribution.go). Each
-- bucket's [min_balance_satoshis, max_balance_satoshis] is INCLUSIVE on
-- both ends (max_balance_satoshis NULL only for the open-ended top
-- bucket), and the eight ranges partition the positive-balance space with
-- no gap and no overlap.
--
-- The bucket_id CHECK constraint below fixes the exact eight legal values;
-- combined with the primary key (no duplicate bucket_id), the immutability
-- trigger (no DELETE, no changing a row's own bucket_id/min/max after
-- seeding), and the fact that this migration seeds all eight and nothing
-- ever inserts a ninth, the table can never contain anything other than
-- exactly these eight rows for the lifetime of the database.
CREATE TABLE address_balance_distribution (
    bucket_id               TEXT PRIMARY KEY
        CHECK (bucket_id IN ('lt_1', '1_10', '10_100', '100_1k', '1k_10k', '10k_100k', '100k_1m', 'gte_1m')),
    min_balance_satoshis     BIGINT NOT NULL,
    max_balance_satoshis     BIGINT, -- NULL only for the open-ended top bucket (gte_1m)
    address_count            BIGINT NOT NULL DEFAULT 0,
    balance_satoshis         BIGINT NOT NULL DEFAULT 0,
    CONSTRAINT address_balance_distribution_min_positive CHECK (min_balance_satoshis >= 1),
    CONSTRAINT address_balance_distribution_bounds_order CHECK (max_balance_satoshis IS NULL OR max_balance_satoshis >= min_balance_satoshis),
    CONSTRAINT address_balance_distribution_count_nonnegative CHECK (address_count >= 0),
    CONSTRAINT address_balance_distribution_balance_nonnegative CHECK (balance_satoshis >= 0)
);

-- Bucket boundaries, in satoshis (1 QOGE = 100,000,000 satoshis):
--   lt_1:     [1,                     99,999,999]            1 sat .. <1 QOGE
--   1_10:     [100,000,000,           999,999,999]           1 QOGE .. <10 QOGE
--   10_100:   [1,000,000,000,         9,999,999,999]         10 QOGE .. <100 QOGE
--   100_1k:   [10,000,000,000,        99,999,999,999]        100 QOGE .. <1,000 QOGE
--   1k_10k:   [100,000,000,000,       999,999,999,999]       1,000 QOGE .. <10,000 QOGE
--   10k_100k: [1,000,000,000,000,     9,999,999,999,999]     10,000 QOGE .. <100,000 QOGE
--   100k_1m:  [10,000,000,000,000,    99,999,999,999,999]    100,000 QOGE .. <1,000,000 QOGE
--   gte_1m:   [100,000,000,000,000,   NULL]                  >= 1,000,000 QOGE
INSERT INTO address_balance_distribution (bucket_id, min_balance_satoshis, max_balance_satoshis) VALUES
    ('lt_1',      1,                   99999999),
    ('1_10',      100000000,           999999999),
    ('10_100',    1000000000,          9999999999),
    ('100_1k',    10000000000,         99999999999),
    ('1k_10k',    100000000000,        999999999999),
    ('10k_100k',  1000000000000,       9999999999999),
    ('100k_1m',   10000000000000,      99999999999999),
    ('gte_1m',    100000000000000,     NULL);

-- Bucket definitions (bucket_id, min, max) are fixed for the lifetime of
-- the database; only address_count/balance_satoshis are ever mutated, by
-- internal/store's incremental maintenance and the backfill command. This
-- trigger makes both "a row was deleted" and "a row's own definition
-- changed" structurally impossible from this point on, mirroring this
-- schema's existing defense-in-depth culture (e.g.
-- sync_state_validate_checkpoint, output_addresses_reject_multisig).
CREATE OR REPLACE FUNCTION address_balance_distribution_immutable_bounds() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'address_balance_distribution: rows are fixed and may never be deleted (bucket %)', OLD.bucket_id;
    END IF;
    IF NEW.bucket_id <> OLD.bucket_id
       OR NEW.min_balance_satoshis <> OLD.min_balance_satoshis
       OR NEW.max_balance_satoshis IS DISTINCT FROM OLD.max_balance_satoshis THEN
        RAISE EXCEPTION 'address_balance_distribution: bucket_id/min_balance_satoshis/max_balance_satoshis are fixed and may never change (bucket %)', OLD.bucket_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER address_balance_distribution_immutable_bounds_trigger
    BEFORE UPDATE OR DELETE ON address_balance_distribution
    FOR EACH ROW EXECUTE FUNCTION address_balance_distribution_immutable_bounds();

-- ── address_balance_distribution_state: singleton anchor ────────────────
--
-- Tracks which canonical state the eight bucket rows above currently
-- represent — structurally identical to sync_state (same bootstrap
-- representation: height=-1/hash=NULL for "never built", same hash-format
-- and bootstrap-consistency CHECKs, same "derive height from blocks,
-- require canonical" trigger below). Unlike block_supply_rollup's
-- per-block cumulative anchor (§27), this table has exactly ONE row and it
-- must equal sync_state's own checkpoint EXACTLY at every committed
-- canonical state (docs/ARCHITECTURE.md §30's "core invariant") — there is
-- no "arbitrary bootstrap: no lineage" case here, because the distribution
-- is a snapshot of addresses.balance_satoshis, not a genesis-rooted
-- cumulative total, so it is well-defined starting from ANY store bootstrap
-- height.
CREATE TABLE address_balance_distribution_state (
    name                TEXT PRIMARY KEY,
    indexed_height      BIGINT NOT NULL DEFAULT -1,
    indexed_block_hash  TEXT,
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT address_balance_distribution_state_height_floor CHECK (indexed_height >= -1),
    CONSTRAINT address_balance_distribution_state_hash_format CHECK (
        indexed_block_hash IS NULL OR indexed_block_hash ~ '^[0-9a-f]{64}$'
    ),
    CONSTRAINT address_balance_distribution_state_bootstrap_consistency CHECK (
        (indexed_height = -1 AND indexed_block_hash IS NULL) OR
        (indexed_height >= 0 AND indexed_block_hash IS NOT NULL)
    )
);
INSERT INTO address_balance_distribution_state (name, indexed_height, indexed_block_hash) VALUES ('main', -1, NULL);

-- Mirrors sync_state_validate_checkpoint (migrations/0001_initial.up.sql)
-- exactly: indexed_height is mechanically derived from blocks.height, never
-- trusted from the caller, and a non-NULL indexed_block_hash must reference
-- an existing, currently-canonical block.
CREATE OR REPLACE FUNCTION address_balance_distribution_state_validate_checkpoint() RETURNS trigger AS $$
DECLARE
    blk_height    BIGINT;
    blk_canonical BOOLEAN;
BEGIN
    IF NEW.indexed_block_hash IS NULL THEN
        IF NEW.indexed_height <> -1 THEN
            RAISE EXCEPTION 'address_balance_distribution_state: indexed_block_hash is NULL but indexed_height=% (must be -1 for the uninitialized/bootstrap checkpoint)', NEW.indexed_height;
        END IF;
        RETURN NEW;
    END IF;

    SELECT height, canonical INTO blk_height, blk_canonical
    FROM blocks WHERE hash = NEW.indexed_block_hash;

    IF blk_height IS NULL THEN
        RAISE EXCEPTION 'address_balance_distribution_state: no block found for indexed_block_hash %', NEW.indexed_block_hash;
    END IF;
    IF NOT blk_canonical THEN
        RAISE EXCEPTION 'address_balance_distribution_state: block % is not canonical; the durable checkpoint must reference the canonical chain', NEW.indexed_block_hash;
    END IF;

    NEW.indexed_height := blk_height;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER address_balance_distribution_state_validate_checkpoint_trigger
    BEFORE INSERT OR UPDATE ON address_balance_distribution_state
    FOR EACH ROW EXECUTE FUNCTION address_balance_distribution_state_validate_checkpoint();
