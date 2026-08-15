-- Reverses 0007_address_balance_distribution.up.sql only. Does not touch
-- any table from 0001/0002/0003/0004/0005/0006 (in particular, addresses
-- and its rows are completely untouched).
--
-- Dropping a table drops its own triggers automatically, but NOT the
-- standalone trigger functions they invoked — those must be dropped
-- explicitly, or a 0007 -> 0006 round trip leaves schema objects behind.
DROP TABLE address_balance_distribution_state;
DROP FUNCTION address_balance_distribution_state_validate_checkpoint();

DROP TABLE address_balance_distribution;
DROP FUNCTION address_balance_distribution_immutable_bounds();
