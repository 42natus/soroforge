-- Add ci_run_url, fn_name, and fn_args to deployments table, and widen
-- the action check constraint to allow 'invoke'.
ALTER TABLE deployments
    DROP CONSTRAINT deployments_action_check;

ALTER TABLE deployments
    ADD CONSTRAINT deployments_action_check CHECK (action IN ('deploy', 'upgrade', 'upload', 'invoke'));

ALTER TABLE deployments
    ADD COLUMN ci_run_url TEXT,
    ADD COLUMN fn_name    TEXT,
    ADD COLUMN fn_args    TEXT;
