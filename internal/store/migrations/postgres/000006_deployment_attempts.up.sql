-- Record failed deploy and upgrade attempts.
--
-- The deployments table holds only confirmed, on-chain history; that rule is
-- what makes `status` and drift detection trustworthy, and it must not bend.
-- Failures still carry real information — a transaction that was included and
-- then failed, or an upgrade the contract rejected — so they get their own
-- append-only table instead of vanishing into a log line.
--
-- `error` may contain RPC responses but never key material: every write path
-- redacts secret-seed-shaped text before storing it.

CREATE TABLE IF NOT EXISTS deployment_attempts (
    id         BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    alias      TEXT        NOT NULL,
    network    TEXT        NOT NULL,
    action     TEXT        NOT NULL,
    stage      TEXT        NOT NULL,
    error      TEXT        NOT NULL,
    tx_hash    TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Only mutating actions are attempted this way, and the stage vocabulary
    -- mirrors the submission cycle: simulate, then submit, then confirm.
    CONSTRAINT deployment_attempts_action_check CHECK (action IN ('deploy', 'upgrade', 'upload', 'invoke')),
    CONSTRAINT deployment_attempts_stage_check  CHECK (stage IN ('simulate', 'submit', 'confirm'))
);

-- `soroforge history --failures` reads newest-first for one contract on one
-- network, the same shape the deployments history index serves.
CREATE INDEX IF NOT EXISTS deployment_attempts_network_alias_created_at_idx
    ON deployment_attempts (network, alias, created_at DESC);
