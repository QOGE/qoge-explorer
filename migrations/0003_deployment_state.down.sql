-- Reverse of 0003_deployment_state.up.sql.
--
-- Removes only this migration's own addition. chain_deployments belongs
-- to migration 0001 and is NOT dropped here — after this migration rolls
-- back, the schema must be identical to schema v2 (0002's end state) plus
-- the untouched chain_deployments table from 0001.

DROP TABLE IF EXISTS deployment_state;
