-- Reverses 0006_addresses_richlist_index.up.sql only. Drops ONLY the
-- rich-list index; does not touch the addresses table itself or any table
-- from 0001-0005.
DROP INDEX addresses_richlist_positive_balance_idx;
