-- Mirrors postgres/000005_deployment_constructor_args.up.sql: records the
-- typed constructor arguments a deploy actually used, as JSON. SQLite has no
-- ADD COLUMN IF NOT EXISTS; the migration runner applies this exactly once.
ALTER TABLE deployments ADD COLUMN constructor_args TEXT;
