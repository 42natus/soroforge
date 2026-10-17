ALTER TABLE deployments
    ADD COLUMN git_commit TEXT,
    ADD COLUMN dirty BOOLEAN,
    ADD COLUMN fee_charged BIGINT;