-- Phase 2H.2a: immutable block supply rollup foundation.
--
-- This migration is SCHEMA ONLY. It never writes a data row itself — a
-- fresh database gets this table empty, exactly like block_accounting was
-- empty until Store.ApplyBlock or backfill-accounting wrote to it. Filling
-- it for an already-indexed chain is the separate, explicit
-- `qoge-explorer backfill-supply-rollup` operator command (see
-- internal/store/supply_rollup_backfill.go) — never hidden inside a
-- migration (see docs/ARCHITECTURE.md §27).
--
-- Design invariants (see docs/ARCHITECTURE.md §27 for full rationale):
--
--   1. One immutable row per block hash, not per canonical height. A row
--      means "the cumulative monetary state from genesis through THIS
--      block along this block's immutable ancestry" — a fact about the
--      block's own immutable body and parentage, exactly like
--      block_accounting's per-block facts. It never changes when the block
--      is later orphaned by a reorg; only blocks.canonical does. There is
--      deliberately NO canonical flag here — canonical selection remains
--      exclusively blocks.canonical / sync_state.indexed_block_hash, and a
--      public reader follows that pointer to find which row is "the"
--      current answer. This table is never UPDATEd after insertion.
--   2. No mutable global counter. This is not a eIquidus-style running
--      total incremented on ApplyBlock and decremented on RollbackTo — see
--      docs/ARCHITECTURE.md §2. Reorg safety instead comes from the rows
--      themselves being branch-addressed and immutable: RollbackTo moves
--      which hash sync_state points at; it never deletes, modifies, or
--      decrements a block_supply_rollup row (internal/store/reorg.go is
--      unchanged by this migration).
--   3. Reward identity (cumulative equivalent of
--      block_accounting_reward_identity): every satoshi of cumulative
--      maximum-available reward is accounted for as either paid out or
--      left unclaimed, summed over the whole canonical prefix.
--   4. UTXO identity: the current UTXO-set value is provably
--      cumulative-coinbase-output minus cumulative-fees minus
--      cumulative-Core-excluded-output (see
--      internal/store/supply_rollup.go's doc comment for the telescoping
--      argument this identity rests on) — this is how
--      cumulative_utxo_set_value_satoshis is actually COMPUTED (by
--      subtraction from the other three cumulative columns), not summed
--      independently, so the identity holds by construction; the CHECK
--      constraint below is a second, independent, DB-native guard.
CREATE TABLE block_supply_rollup (
    block_hash                              TEXT PRIMARY KEY REFERENCES blocks (hash),

    -- excluded_output_satoshis: this block's OWN (non-cumulative) total
    -- value encoded in transaction outputs Core's coins view never adds at
    -- creation — every output value at height 0 (genesis), or any output
    -- at any height whose scriptPubKey is Core's IsUnspendable (OP_RETURN,
    -- or larger than MAX_SCRIPT_SIZE). Mirrors exactly the exclusion
    -- applyOutput already applies when deciding whether to create a
    -- utxo_state row (internal/store/apply.go) — see
    -- internal/store/supply_rollup.go's localExcludedOutputValue.
    excluded_output_satoshis                BIGINT NOT NULL,

    cumulative_subsidy_satoshis             BIGINT NOT NULL,
    cumulative_fee_satoshis                 BIGINT NOT NULL,
    cumulative_coinbase_output_satoshis     BIGINT NOT NULL,
    cumulative_unclaimed_reward_satoshis    BIGINT NOT NULL,
    cumulative_excluded_output_satoshis     BIGINT NOT NULL,
    cumulative_utxo_set_value_satoshis      BIGINT NOT NULL,

    indexed_at                              TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT block_supply_rollup_excluded_nonnegative
        CHECK (excluded_output_satoshis >= 0),
    CONSTRAINT block_supply_rollup_cumulative_subsidy_nonnegative
        CHECK (cumulative_subsidy_satoshis >= 0),
    CONSTRAINT block_supply_rollup_cumulative_fee_nonnegative
        CHECK (cumulative_fee_satoshis >= 0),
    CONSTRAINT block_supply_rollup_cumulative_coinbase_output_nonnegative
        CHECK (cumulative_coinbase_output_satoshis >= 0),
    CONSTRAINT block_supply_rollup_cumulative_unclaimed_reward_nonnegative
        CHECK (cumulative_unclaimed_reward_satoshis >= 0),
    CONSTRAINT block_supply_rollup_cumulative_excluded_output_nonnegative
        CHECK (cumulative_excluded_output_satoshis >= 0),
    CONSTRAINT block_supply_rollup_cumulative_utxo_set_value_nonnegative
        CHECK (cumulative_utxo_set_value_satoshis >= 0),

    -- Cumulative equivalent of block_accounting_reward_identity, summed
    -- over the whole canonical prefix rather than one block.
    CONSTRAINT block_supply_rollup_reward_identity CHECK (
        cumulative_coinbase_output_satoshis + cumulative_unclaimed_reward_satoshis
        = cumulative_subsidy_satoshis + cumulative_fee_satoshis
    ),

    -- UTXO algebra (item 11): current UTXO-set value + cumulative fees +
    -- cumulative Core-excluded output value = cumulative coinbase output
    -- value. Valid only for a complete-from-genesis rollup, which is
    -- exactly what a row in this table always is (internal/store never
    -- inserts a partial/non-genesis-rooted row — see supply_rollup.go).
    CONSTRAINT block_supply_rollup_utxo_identity CHECK (
        cumulative_utxo_set_value_satoshis + cumulative_fee_satoshis + cumulative_excluded_output_satoshis
        = cumulative_coinbase_output_satoshis
    )
);
