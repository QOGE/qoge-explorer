-- Reverses 0001_initial.up.sql in strict dependency order.

DROP TABLE IF EXISTS chain_deployments;
DROP TABLE IF EXISTS addresses;

DROP TRIGGER IF EXISTS utxo_state_derive_heights_trigger ON utxo_state;
DROP TABLE IF EXISTS utxo_state;
DROP FUNCTION IF EXISTS utxo_state_derive_heights();

DROP TRIGGER IF EXISTS output_participants_require_multisig_trigger ON output_participants;
DROP TABLE IF EXISTS output_participants;
DROP FUNCTION IF EXISTS output_participants_require_multisig();

DROP TRIGGER IF EXISTS output_addresses_reject_multisig_trigger ON output_addresses;
DROP TABLE IF EXISTS output_addresses;
DROP FUNCTION IF EXISTS output_addresses_reject_multisig();

DROP TABLE IF EXISTS transaction_outputs;
DROP TABLE IF EXISTS transaction_input_witness;
DROP TABLE IF EXISTS transaction_inputs;

DROP TRIGGER IF EXISTS block_transactions_set_height_trigger ON block_transactions;
DROP TABLE IF EXISTS block_transactions;
DROP FUNCTION IF EXISTS block_transactions_set_height();

DROP TABLE IF EXISTS transaction_variants;

DROP TABLE IF EXISTS transactions;
DROP TABLE IF EXISTS blocks;

DROP TRIGGER IF EXISTS sync_state_validate_checkpoint_trigger ON sync_state;
DROP TABLE IF EXISTS sync_state;
DROP FUNCTION IF EXISTS sync_state_validate_checkpoint();
