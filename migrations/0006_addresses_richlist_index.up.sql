-- Phase 2H.3: ranked address balance rich list foundation.
--
-- This migration is SCHEMA ONLY — it adds one partial index, nothing
-- else. The rich list's production query
-- (internal/query/richlist.go) reads the ALREADY-authoritative
-- addresses.balance_satoshis cache internal/store maintains
-- (recomputeAddress); this index exists purely so that
--
--     SELECT address, balance_satoshis
--     FROM addresses
--     WHERE balance_satoshis > 0
--     ORDER BY balance_satoshis DESC, address COLLATE "C" ASC
--     LIMIT 100
--
-- can be answered in O(log N + 100) rather than sorting/scanning the
-- entire addresses table as address count grows (see
-- docs/ARCHITECTURE.md §29). The ORDER BY here — descending balance, then
-- ascending address under the C locale — matches this index's own key
-- order exactly, so PostgreSQL can walk it directly without a separate
-- sort step. The partial predicate (balance_satoshis > 0) matches the
-- query's own WHERE clause exactly, which is what makes the predicate
-- usable at all: a partial index only helps a query whose WHERE clause
-- provably implies the index predicate.
CREATE INDEX addresses_richlist_positive_balance_idx
    ON addresses (
        balance_satoshis DESC,
        address COLLATE "C" ASC
    )
    WHERE balance_satoshis > 0;
