-- Widens the action constraint for `soroforge upload`, which installs
-- bytecode on-chain without creating or upgrading a contract instance.
--
-- SQLite has no ALTER TABLE ... DROP/ADD CONSTRAINT, so widening a CHECK means
-- rebuilding the table: create the new shape, copy the rows across (ids
-- included, so foreign expectations and history ordering survive), drop the
-- old table, and rename. The rename also carries the rows' indexes back, so
-- they are recreated explicitly. Wrapped in a transaction by the migration
-- runner, so a failure mid-way leaves the original table intact.

CREATE TABLE deployments_rebuild (
    id              INTEGER     PRIMARY KEY AUTOINCREMENT,
    alias           TEXT        NOT NULL,
    network         TEXT        NOT NULL,
    contract_id     TEXT        NOT NULL,
    wasm_hash       TEXT        NOT NULL,
    action          TEXT        NOT NULL,
    deployer_pubkey TEXT        NOT NULL,
    tx_hash         TEXT,
    ledger          INTEGER,
    notes           TEXT,
    created_at      TEXT        NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),

    CONSTRAINT deployments_action_check CHECK (action IN ('deploy', 'upgrade', 'upload'))
);

INSERT INTO deployments_rebuild (
    id, alias, network, contract_id, wasm_hash, action,
    deployer_pubkey, tx_hash, ledger, notes, created_at
)
SELECT
    id, alias, network, contract_id, wasm_hash, action,
    deployer_pubkey, tx_hash, ledger, notes, created_at
FROM deployments;

DROP TABLE deployments;

ALTER TABLE deployments_rebuild RENAME TO deployments;

CREATE INDEX deployments_network_contract_id_idx
    ON deployments (network, contract_id);

CREATE INDEX deployments_network_alias_created_at_idx
    ON deployments (network, alias, created_at DESC);

-- Keep the sequence ahead of the copied rows so the next insert cannot
-- collide with an existing id.
UPDATE sqlite_sequence
SET seq = (SELECT MAX(id) FROM deployments)
WHERE name = 'deployments' AND (SELECT MAX(id) FROM deployments) >
    (SELECT seq FROM sqlite_sequence WHERE name = 'deployments');
