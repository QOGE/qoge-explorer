-- Reverses 0007_address_balance_distribution.up.sql only. Does not touch
-- any table from 0001/0002/0003/0004/0005/0006 (in particular, addresses
-- and its rows are completely untouched).
DROP TABLE address_balance_distribution_state;
DROP TABLE address_balance_distribution;
