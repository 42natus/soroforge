-- Reverses 000002_upload_action.up.sql.
--
-- Narrowing the constraint re-validates existing rows, so this fails loudly if
-- any 'upload' rows are present rather than deleting history to make itself
-- work — dropping those rows is the operator's call, not a migration's.
ALTER TABLE deployments
    DROP CONSTRAINT deployments_action_check;

ALTER TABLE deployments
    ADD CONSTRAINT deployments_action_check CHECK (action IN ('deploy', 'upgrade'));
