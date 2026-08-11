-- Reverse of 0002_mempool_cache.up.sql, in strict dependency order.

DROP TABLE IF EXISTS mempool_dependencies;

DROP TRIGGER IF EXISTS mempool_output_participants_require_multisig_trigger ON mempool_output_participants;
DROP FUNCTION IF EXISTS mempool_output_participants_require_multisig();
DROP TABLE IF EXISTS mempool_output_participants;

DROP TRIGGER IF EXISTS mempool_output_addresses_reject_multisig_trigger ON mempool_output_addresses;
DROP FUNCTION IF EXISTS mempool_output_addresses_reject_multisig();
DROP TABLE IF EXISTS mempool_output_addresses;

DROP TABLE IF EXISTS mempool_outputs;
DROP TABLE IF EXISTS mempool_input_witness;
DROP TABLE IF EXISTS mempool_inputs;
DROP TABLE IF EXISTS mempool_transactions;
DROP TABLE IF EXISTS mempool_state;
