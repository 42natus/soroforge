-- SoroForge deployment history.
--
-- Two tables: `contracts` is current state (one row per contract per network),
-- `deployments` is the append-only log of how it got there.

CREATE TABLE IF NOT EXISTS contracts (
    id                BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    alias             TEXT        NOT NULL,
    network           TEXT        NOT NULL,
    contract_id       TEXT        NOT NULL,
    current_wasm_hash TEXT        NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- An alias identifies a contract only within a network: "counter" on
    -- testnet and "counter" on mainnet are independent deployments.
    CONSTRAINT contracts_network_alias_key UNIQUE (network, alias)
);

CREATE TABLE IF NOT EXISTS deployments (
    id              BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    alias           TEXT        NOT NULL,
    network         TEXT        NOT NULL,
    contract_id     TEXT        NOT NULL,
    wasm_hash       TEXT        NOT NULL,
    action          TEXT        NOT NULL,
    deployer_pubkey TEXT        NOT NULL,
    tx_hash         TEXT,
    ledger          BIGINT,
    notes           TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

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
