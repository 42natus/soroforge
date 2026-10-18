-- Revert migration 000004: drop columns fn_args, fn_name, and ci_run_url,
-- and narrow the action check constraint back to ('deploy', 'upgrade', 'upload').
ALTER TABLE deployments
    DROP COLUMN fn_args,
    DROP COLUMN fn_name,
    DROP COLUMN ci_run_url;

ALTER TABLE deployments
    DROP CONSTRAINT deployments_action_check;

ALTER TABLE deployments
    ADD CONSTRAINT deployments_action_check CHECK (action IN ('deploy', 'upgrade', 'upload'));
