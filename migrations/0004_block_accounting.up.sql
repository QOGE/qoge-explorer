-- Phase 2H.1: immutable block monetary accounting foundation.
--
-- Design invariants (see docs/ARCHITECTURE.md §6/§26 for full rationale):
--
--   1. Per-block, not cumulative. A block's monetary facts — the subsidy
--      its height entitled it to, the fees its transactions paid, what its
--      coinbase transaction actually paid out, and what it left unclaimed —
--      are immutable properties of THAT block, exactly like its hash or
--      merkle root. They never change when a reorg later demotes the block
--      off the canonical chain; only blocks.canonical does. This mirrors
--      the same "immutable body vs. canonical derived state" split already
--      used throughout 0001_initial.up.sql (transaction_outputs vs.
--      utxo_state) — see docs/ARCHITECTURE.md §3b. There is deliberately
--      no cumulative supply/fee/coinbase counter here: an additive global
--      total is exactly the eIquidus-style design this project avoids (see
--      docs/ARCHITECTURE.md §2), and would require a parent-accounting
--      dependency this migration's schema does not have.
--   2. No canonical duplication. blocks.canonical is already the single
--      source of truth for which chain a block belongs to; block_accounting
--      carries no canonical flag of its own, so there is nothing here that
--      can ever drift out of sync with it. A reorg's ONLY effect on this
--      table is via cascading deletes when an orphaned block itself is
--      never deleted — see RollbackTo (internal/store/reorg.go), which
--      marks blocks.canonical = false and never deletes the row, so this
--      table's block_hash FK is never touched by a reorg either. Orphaned
--      blocks' accounting rows remain as audit history.
--   3. Coinbase overclaim is rejected upstream, not here. Core's
--      ConnectBlock only rejects coinbase_output_total > subsidy + fees
--      (src/validation.cpp: `if (block.vtx[0]->GetValueOut() > blockReward)
--      ... "bad-cb-amount"`) — underclaiming is valid chain state. Because
--      internal/accounting.SubsidySchedule.ComputeBlockFacts already
--      rejects the overclaim direction in Go before this table is ever
--      written to (using the network-specific schedule the writing Store
--      was constructed with — see docs/ARCHITECTURE.md §26 "Network-aware
--      subsidy schedule"), the identity
--      below only needs to hold, not additionally bound
--      unclaimed_reward_satoshis by anything other than >= 0.
CREATE TABLE block_accounting (
    block_hash                  TEXT PRIMARY KEY REFERENCES blocks (hash),
    subsidy_satoshis             BIGINT NOT NULL,
    fee_satoshis                 BIGINT NOT NULL,
    coinbase_output_satoshis     BIGINT NOT NULL,
    unclaimed_reward_satoshis    BIGINT NOT NULL,
    indexed_at                   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT block_accounting_subsidy_nonnegative CHECK (subsidy_satoshis >= 0),
    CONSTRAINT block_accounting_fee_nonnegative CHECK (fee_satoshis >= 0),
    CONSTRAINT block_accounting_coinbase_output_nonnegative CHECK (coinbase_output_satoshis >= 0),
    CONSTRAINT block_accounting_unclaimed_reward_nonnegative CHECK (unclaimed_reward_satoshis >= 0),
    -- Per-block monetary identity: coinbase_output + unclaimed == subsidy +
    -- fees (the "maximum available block reward" from
    -- docs/ARCHITECTURE.md §6), i.e. every satoshi of that block's maximum
    -- available reward is accounted for as either paid out or left
    -- unclaimed. This expression is safe from the "overflow-prone SQL"
    -- concern the design explicitly considered: PostgreSQL's bigint (int8)
    -- addition RAISES "bigint out of range" on overflow rather than
    -- silently wrapping (unlike C/Go's raw int64 +), and real QOGE
    -- quantities are many orders of magnitude below int64's range in any
    -- case — internal/accounting's Go layer performs its own checked
    -- arithmetic (checkedAdd) before a row ever reaches this INSERT, so
    -- this CHECK is a second, independent, DB-native guard rather than the
    -- only line of defense.
    CONSTRAINT block_accounting_reward_identity CHECK (
        coinbase_output_satoshis + unclaimed_reward_satoshis = subsidy_satoshis + fee_satoshis
    )
);
