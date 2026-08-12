-- Phase 2G.1: consensus deployment observer — deployment_state singleton.
--
-- Design invariants (see docs/ARCHITECTURE.md §24 for full rationale):
--
--   1. chain_deployments (migrations/0001_initial.up.sql) remains the
--      per-deployment cache table; this migration does not modify it and
--      does not rewrite 0001. The observer (internal/deployments)
--      atomically replaces its entire content inside the SAME
--      transaction that updates deployment_state, using the same
--      DELETE-then-INSERT full-replacement pattern
--      internal/mempool.Store.ReplaceSnapshot already uses for
--      mempool_transactions (migrations/0002_mempool_cache.up.sql).
--   2. deployment_state distinguishes "never successfully synchronized"
--      from "successfully synchronized" — including a successfully
--      synchronized snapshot containing zero BIP9 deployments, which is
--      real observed state, not "nothing happened" — via an explicit
--      `initialized` flag, exactly mirroring mempool_state's shape and
--      the same CHECK-constraint-enforced consistency.
--   3. Only BIP9 deployments are cached here. Core's getdeploymentinfo
--      also reports "buried" deployments (static historical consensus
--      rules with no ongoing status model); those are decoded just
--      enough to prove they aren't malformed and then intentionally
--      dropped by internal/deployments before anything reaches this
--      schema — chain_deployments.status is the BIP9 status enum
--      (defined/started/locked_in/active/failed) and has no
--      corresponding concept for a buried deployment.
--   4. generation increments once per successfully committed snapshot;
--      there are no incremental counters, and a failed or skipped
--      observation never advances it.
--   5. Every row of one snapshot shares the SAME checked_at observation
--      timestamp (internal/deployments.Candidate.ObservedAt), mirroring
--      deployment_state.observed_at for that generation.

-- ── deployment_state: singleton snapshot state ───────────────────────────
-- Exactly one row ('main'). UNINITIALIZED means "never successfully
-- observed" (initialized=false, generation=0, observed_at=NULL, no Core
-- anchor, deployment_count=0). INITIALIZED means "successfully observed
-- at least once" (initialized=true, generation>=1, observed_at set, a
-- valid Core tip anchor) — including the legitimate "successfully
-- observed and currently zero BIP9 deployments" case.
CREATE TABLE deployment_state (
    name                TEXT PRIMARY KEY,
    initialized         BOOLEAN NOT NULL DEFAULT FALSE,
    generation          BIGINT NOT NULL DEFAULT 0,
    core_tip_height     BIGINT,
    core_tip_hash       TEXT,
    deployment_count    INT NOT NULL DEFAULT 0,
    observed_at         TIMESTAMPTZ,
    CONSTRAINT deployment_state_generation_nonnegative CHECK (generation >= 0),
    CONSTRAINT deployment_state_deployment_count_nonnegative CHECK (deployment_count >= 0),
    CONSTRAINT deployment_state_core_tip_height_nonnegative CHECK (core_tip_height IS NULL OR core_tip_height >= 0),
    CONSTRAINT deployment_state_core_tip_hash_format CHECK (core_tip_hash IS NULL OR core_tip_hash ~ '^[0-9a-f]{64}$'),
    -- Every field below is written so it always evaluates to TRUE or
    -- FALSE, never NULL, for the same reason as
    -- mempool_state_initialized_consistency in
    -- 0002_mempool_cache.up.sql: an ambiguous NULL result would let a
    -- half-initialized row silently pass this CHECK.
    CONSTRAINT deployment_state_initialized_consistency CHECK (
        (
            NOT initialized
            AND generation = 0
            AND observed_at IS NULL
            AND core_tip_height IS NULL
            AND core_tip_hash IS NULL
            AND deployment_count = 0
        ) OR (
            initialized
            AND generation >= 1
            AND observed_at IS NOT NULL
            AND core_tip_height IS NOT NULL
            AND core_tip_hash IS NOT NULL
        )
    )
);
INSERT INTO deployment_state (name) VALUES ('main');
