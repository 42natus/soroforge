-- Reverses 000002_upload_action.up.sql by rebuilding deployments with the
-- narrower CHECK.
--
-- Re-creating the constraint re-validates existing rows, so this fails loudly
-- if any 'upload' rows are present rather than deleting history to make
-- itself work — dropping those rows is the operator's call, not a
-- migration's.

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

    CONSTRAINT deployments_action_check CHECK (action IN ('deploy', 'upgrade'))
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

UPDATE sqlite_sequence
SET seq = (SELECT MAX(id) FROM deployments)
WHERE name = 'deployments' AND (SELECT MAX(id) FROM deployments) >
    (SELECT seq FROM sqlite_sequence WHERE name = 'deployments');
