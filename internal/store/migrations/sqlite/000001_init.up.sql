-- SoroForge deployment history (SQLite dialect).
--
-- Mirrors migrations/postgres/000001_init.up.sql table for table, column for
-- column; a test asserts the two dialects stay in parity. Dialect choices:
--
--   * BIGINT GENERATED ALWAYS AS IDENTITY becomes INTEGER PRIMARY KEY
--     AUTOINCREMENT — SQLite's rowid alias, the idiomatic identity column.
--   * TIMESTAMPTZ becomes TEXT holding a fixed-width UTC RFC3339 timestamp
--     with microsecond precision. Fixed width matters: history sorts by
--     created_at, and SQLite compares TEXT lexicographically, so variable-
--     length fractional seconds would misorder rows. Microseconds match
--     Postgres' timestamp resolution, so an exported history re-imports into
--     either backend without silent truncation differences.
--   * now() defaults are strftime(...) in UTC; the Go store supplies explicit
--     timestamps on every write, so the defaults only guard hand-edited files.

CREATE TABLE IF NOT EXISTS contracts (
    id                INTEGER     PRIMARY KEY AUTOINCREMENT,
    alias             TEXT        NOT NULL,
    network           TEXT        NOT NULL,
    contract_id       TEXT        NOT NULL,
    current_wasm_hash TEXT        NOT NULL,
    created_at        TEXT        NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at        TEXT        NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),

    -- An alias identifies a contract only within a network: "counter" on
    -- testnet and "counter" on mainnet are independent deployments.
    CONSTRAINT contracts_network_alias_key UNIQUE (network, alias)
);

CREATE TABLE IF NOT EXISTS deployments (
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

-- Required by the spec and used by every history and drift-detection query.
CREATE INDEX IF NOT EXISTS contracts_network_contract_id_idx
    ON contracts (network, contract_id);

CREATE INDEX IF NOT EXISTS deployments_network_contract_id_idx
    ON deployments (network, contract_id);

-- `soroforge history <alias>` reads newest-first for one contract on one
-- network; this index serves that ordering directly.
CREATE INDEX IF NOT EXISTS deployments_network_alias_created_at_idx
    ON deployments (network, alias, created_at DESC);
