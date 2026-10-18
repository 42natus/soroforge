-- Mirrors postgres/000003_deployment_metadata.up.sql. SQLite adds one column
-- per ALTER TABLE statement; plain column additions need no table rebuild.
ALTER TABLE deployments ADD COLUMN git_commit TEXT;
ALTER TABLE deployments ADD COLUMN dirty BOOLEAN;
ALTER TABLE deployments ADD COLUMN fee_charged INTEGER;
