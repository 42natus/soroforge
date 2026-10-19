-- Mirrors migrations/postgres/000006_deployment_attempts.up.sql table for
-- table, column for column; the parity test asserts the two dialects agree.
-- Dialect choices are the ones established in 000001_init: identity becomes
-- INTEGER PRIMARY KEY AUTOINCREMENT, timestamps are fixed-width UTC text so
-- lexicographic ordering equals time ordering.
--
-- Failed deploy and upgrade attempts live here so the deployments table can
-- keep holding only confirmed history. `error` may contain RPC responses but
-- never key material — the write path redacts before storing.

CREATE TABLE IF NOT EXISTS deployment_attempts (
    id         INTEGER     PRIMARY KEY AUTOINCREMENT,
    alias      TEXT        NOT NULL,
    network    TEXT        NOT NULL,
    action     TEXT        NOT NULL,
    stage      TEXT        NOT NULL,
    error      TEXT        NOT NULL,
    tx_hash    TEXT,
    created_at TEXT        NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),

    CONSTRAINT deployment_attempts_action_check CHECK (action IN ('deploy', 'upgrade', 'upload', 'invoke')),
    CONSTRAINT deployment_attempts_stage_check  CHECK (stage IN ('simulate', 'submit', 'confirm'))
);

-- `soroforge history --failures` reads newest-first for one contract on one
-- network, the same shape the deployments history index serves.
CREATE INDEX IF NOT EXISTS deployment_attempts_network_alias_created_at_idx
    ON deployment_attempts (network, alias, created_at DESC);
