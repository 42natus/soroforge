-- Widens the action constraint for `soroforge upload`, which installs bytecode
-- on-chain without creating or upgrading a contract instance.
ALTER TABLE deployments
    DROP CONSTRAINT deployments_action_check;

ALTER TABLE deployments
    ADD CONSTRAINT deployments_action_check CHECK (action IN ('deploy', 'upgrade', 'upload'));
