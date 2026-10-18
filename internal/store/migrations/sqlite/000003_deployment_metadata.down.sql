-- Reverses 000003_deployment_metadata.up.sql.
ALTER TABLE deployments DROP COLUMN fee_charged;
ALTER TABLE deployments DROP COLUMN dirty;
ALTER TABLE deployments DROP COLUMN git_commit;
