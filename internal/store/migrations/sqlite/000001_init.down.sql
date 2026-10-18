-- Reverses 000001_init.up.sql.
--
-- This drops the deployment history. It exists so the migration chain is
-- reversible in development; running it against a real deployment record is a
-- destructive act with no undo.

DROP INDEX IF EXISTS deployments_network_alias_created_at_idx;
DROP INDEX IF EXISTS deployments_network_contract_id_idx;
DROP INDEX IF EXISTS contracts_network_contract_id_idx;

DROP TABLE IF EXISTS deployments;
DROP TABLE IF EXISTS contracts;
