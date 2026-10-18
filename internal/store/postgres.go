package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Postgres is the production Store, backed by pgx v5.
type Postgres struct {
	pool *pgxpool.Pool
}

var _ Store = (*Postgres)(nil)

// connectTimeout bounds the initial connection so a wrong DATABASE_URL fails
// promptly instead of hanging a CI job.
const connectTimeout = 10 * time.Second

// NewPostgres connects to Postgres and verifies the connection.
//
// It pings eagerly: a pool is lazy by default, and discovering a bad connection
// string only after a deploy transaction has been submitted on-chain would be a
// bad time to find out the history cannot be written.
func NewPostgres(ctx context.Context, databaseURL string) (*Postgres, error) {
	if databaseURL == "" {
		return nil, fmt.Errorf("store: database URL is required")
	}

	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		// pgx echoes the connection string in parse errors, and that string
		// normally carries a password. Report the failure without it.
		return nil, fmt.Errorf("store: could not parse %s (check its format)", "DATABASE_URL")
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: connect: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}

	return &Postgres{pool: pool}, nil
}

// Close releases the connection pool.
func (p *Postgres) Close() error {
	p.pool.Close()
	return nil
}

// Pool exposes the underlying pool for migrations and integration tests.
func (p *Postgres) Pool() *pgxpool.Pool { return p.pool }

const upsertContractSQL = `
INSERT INTO contracts (alias, network, contract_id, current_wasm_hash)
VALUES ($1, $2, $3, $4)
ON CONFLICT (network, alias) DO UPDATE
SET contract_id       = EXCLUDED.contract_id,
    current_wasm_hash = EXCLUDED.current_wasm_hash,
    updated_at        = now()
RETURNING id, alias, network, contract_id, current_wasm_hash, created_at, updated_at`

// UpsertContract implements Store.
func (p *Postgres) UpsertContract(ctx context.Context, c Contract) (Contract, error) {
	if err := c.Validate(); err != nil {
		return Contract{}, err
	}
	row := p.pool.QueryRow(ctx, upsertContractSQL, c.Alias, c.Network, c.ContractID, c.CurrentWasmHash)
	out, err := scanContract(row)
	if err != nil {
		return Contract{}, fmt.Errorf("upsert contract %s/%s: %w", c.Network, c.Alias, err)
	}
	return out, nil
}

const getContractSQL = `
SELECT id, alias, network, contract_id, current_wasm_hash, created_at, updated_at
FROM contracts
WHERE network = $1 AND alias = $2`

// GetContract implements Store.
func (p *Postgres) GetContract(ctx context.Context, network, alias string) (Contract, error) {
	row := p.pool.QueryRow(ctx, getContractSQL, network, alias)
	c, err := scanContract(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Contract{}, fmt.Errorf("contract %s on %s: %w", alias, network, ErrNotFound)
	}
	if err != nil {
		return Contract{}, fmt.Errorf("get contract %s/%s: %w", network, alias, err)
	}
	return c, nil
}

const listContractsSQL = `
SELECT id, alias, network, contract_id, current_wasm_hash, created_at, updated_at
FROM contracts
WHERE ($1 = '' OR network = $1)
ORDER BY network, alias`

const listContractsPageSQL = listContractsSQL + `
LIMIT $2 OFFSET $3`

// ListContracts implements Store.
func (p *Postgres) ListContracts(ctx context.Context, network string) ([]Contract, error) {
	rows, err := p.pool.Query(ctx, listContractsSQL, network)
	if err != nil {
		return nil, fmt.Errorf("list contracts: %w", err)
	}
	defer rows.Close()

	contracts := []Contract{}
	for rows.Next() {
		c, err := scanContract(rows)
		if err != nil {
			return nil, fmt.Errorf("list contracts: %w", err)
		}
		contracts = append(contracts, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list contracts: %w", err)
	}
	return contracts, nil
}

// ListContractsPage implements Store.
func (p *Postgres) ListContractsPage(ctx context.Context, network string, limit, offset int) ([]Contract, error) {
	if err := validatePage(limit, offset); err != nil {
		return nil, err
	}
	rows, err := p.pool.Query(ctx, listContractsPageSQL, network, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list contracts page: %w", err)
	}
	defer rows.Close()

	contracts := []Contract{}
	for rows.Next() {
		c, err := scanContract(rows)
		if err != nil {
			return nil, fmt.Errorf("list contracts page: %w", err)
		}
		contracts = append(contracts, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list contracts page: %w", err)
	}
	return contracts, nil
}

const recordDeploymentSQL = `
INSERT INTO deployments (
    alias, network, contract_id, wasm_hash, action, deployer_pubkey, tx_hash, ledger, git_commit, dirty, fee_charged, notes, ci_run_url, fn_name, fn_args
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
RETURNING id, alias, network, contract_id, wasm_hash, action,
		deployer_pubkey, tx_hash, ledger, git_commit, dirty, fee_charged, notes, ci_run_url, fn_name, fn_args, created_at`

// RecordDeployment implements Store.
func (p *Postgres) RecordDeployment(ctx context.Context, d Deployment) (Deployment, error) {
	if err := d.Validate(); err != nil {
		return Deployment{}, err
	}

	// tx_hash and ledger are absent for a dry run and unknown if submission
	// fails after signing; NULL records that honestly rather than storing a
	// misleading empty string or a zero ledger.
	var txHash *string
	if d.TxHash != "" {
		txHash = &d.TxHash
	}
	var ledger *int64
	if d.Ledger != 0 {
		l := int64(d.Ledger)
		ledger = &l
	}
	var notes *string
	if d.Notes != "" {
		notes = &d.Notes
	}

	var feeCharged *int64
	if d.FeeCharged != nil {
		feeCharged = d.FeeCharged
	}
	var gitCommit *string
	if d.GitCommit != "" {
		gitCommit = &d.GitCommit
	}
	var dirty *bool
	if d.GitCommit != "" {
		dirty = &d.Dirty
	}
	var ciRunURL *string
	if d.CIRunURL != "" {
		ciRunURL = &d.CIRunURL
	}
	var fnName *string
	if d.FnName != "" {
		fnName = &d.FnName
	}
	var fnArgs *string
	if d.FnArgs != "" {
		fnArgs = &d.FnArgs
	}

	row := p.pool.QueryRow(ctx, recordDeploymentSQL,
		d.Alias, d.Network, d.ContractID, d.WasmHash, string(d.Action),
		d.DeployerPubkey, txHash, ledger, gitCommit, dirty, feeCharged, notes, ciRunURL, fnName, fnArgs)

	out, err := scanDeployment(row)
	if err != nil {
		return Deployment{}, fmt.Errorf("record deployment for %s/%s: %w", d.Network, d.Alias, err)
	}
	return out, nil
}

const historySQL = `
SELECT id, alias, network, contract_id, wasm_hash, action,
	deployer_pubkey, tx_hash, ledger, git_commit, dirty, fee_charged, notes, ci_run_url, fn_name, fn_args, created_at
FROM deployments
WHERE network = $1 AND alias = $2
ORDER BY created_at DESC, id DESC`

const historyPageSQL = historySQL + `
LIMIT $3 OFFSET $4`

// History implements Store.
//
// It orders by created_at then id so that records written within the same clock
// tick still come back in insertion order — an upgrade must never appear to
// precede the deploy it followed.
func (p *Postgres) History(ctx context.Context, network, alias string) ([]Deployment, error) {
	rows, err := p.pool.Query(ctx, historySQL, network, alias)
	if err != nil {
		return nil, fmt.Errorf("history for %s/%s: %w", network, alias, err)
	}
	defer rows.Close()

	deployments := []Deployment{}
	for rows.Next() {
		d, err := scanDeployment(rows)
		if err != nil {
			return nil, fmt.Errorf("history for %s/%s: %w", network, alias, err)
		}
		deployments = append(deployments, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("history for %s/%s: %w", network, alias, err)
	}
	return deployments, nil
}

const getDeploymentSQL = `
SELECT id, alias, network, contract_id, wasm_hash, action,
	deployer_pubkey, tx_hash, ledger, git_commit, dirty, fee_charged, notes, ci_run_url, fn_name, fn_args, created_at
FROM deployments
WHERE id = $1`

// GetDeployment implements Store.
func (p *Postgres) GetDeployment(ctx context.Context, id int64) (Deployment, error) {
	row := p.pool.QueryRow(ctx, getDeploymentSQL, id)
	d, err := scanDeployment(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Deployment{}, fmt.Errorf("deployment %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return Deployment{}, fmt.Errorf("get deployment %d: %w", id, err)
	}
	return d, nil
}

// HistoryPage implements Store.
func (p *Postgres) HistoryPage(ctx context.Context, network, alias string, limit, offset int) ([]Deployment, error) {
	if err := validatePage(limit, offset); err != nil {
		return nil, err
	}
	rows, err := p.pool.Query(ctx, historyPageSQL, network, alias, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("history page for %s/%s: %w", network, alias, err)
	}
	defer rows.Close()

	deployments := []Deployment{}
	for rows.Next() {
		d, err := scanDeployment(rows)
		if err != nil {
			return nil, fmt.Errorf("history page for %s/%s: %w", network, alias, err)
		}
		deployments = append(deployments, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("history page for %s/%s: %w", network, alias, err)
	}
	return deployments, nil
}

const listAllDeploymentsSQL = `
SELECT id, alias, network, contract_id, wasm_hash, action,
	deployer_pubkey, tx_hash, ledger, git_commit, dirty, fee_charged, notes, ci_run_url, fn_name, fn_args, created_at
FROM deployments
ORDER BY id`

// ListAllDeployments implements Store.
func (p *Postgres) ListAllDeployments(ctx context.Context) ([]Deployment, error) {
	rows, err := p.pool.Query(ctx, listAllDeploymentsSQL)
	if err != nil {
		return nil, fmt.Errorf("list deployments: %w", err)
	}
	defer rows.Close()

	deployments := []Deployment{}
	for rows.Next() {
		d, err := scanDeployment(rows)
		if err != nil {
			return nil, fmt.Errorf("list deployments: %w", err)
		}
		deployments = append(deployments, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list deployments: %w", err)
	}
	return deployments, nil
}

// importSnapshot implements snapshotImporter for Postgres.
//
// The identity columns are GENERATED ALWAYS, so inserting recorded ids needs
// OVERRIDING SYSTEM VALUE, and the backing sequences must then be moved past
// the imported ids — otherwise the next deploy collides on a duplicate key.
// The whole import runs in one transaction: half a history is worse than none.
func (p *Postgres) importSnapshot(ctx context.Context, contracts []Contract, deployments []Deployment) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres import: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, c := range contracts {
		if _, err := tx.Exec(ctx, `
INSERT INTO contracts (id, alias, network, contract_id, current_wasm_hash, created_at, updated_at)
OVERRIDING SYSTEM VALUE
VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			c.ID, c.Alias, c.Network, c.ContractID, c.CurrentWasmHash, c.CreatedAt, c.UpdatedAt); err != nil {
			return fmt.Errorf("postgres import contract %s/%s: %w", c.Network, c.Alias, err)
		}
	}

	for _, d := range deployments {
		var txHash *string
		if d.TxHash != "" {
			txHash = &d.TxHash
		}
		var ledger *int64
		if d.Ledger != 0 {
			l := int64(d.Ledger)
			ledger = &l
		}
		// dirty is only meaningful alongside a commit, as in RecordDeployment.
		var dirty *bool
		if d.GitCommit != "" {
			dirty = &d.Dirty
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO deployments (id, alias, network, contract_id, wasm_hash, action,
                         deployer_pubkey, tx_hash, ledger, git_commit, dirty, fee_charged,
                         notes, ci_run_url, fn_name, fn_args, created_at)
OVERRIDING SYSTEM VALUE
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)`,
			d.ID, d.Alias, d.Network, d.ContractID, d.WasmHash, string(d.Action),
			d.DeployerPubkey, txHash, ledger, optionalText(d.GitCommit), dirty, d.FeeCharged,
			optionalText(d.Notes), optionalText(d.CIRunURL), optionalText(d.FnName), optionalText(d.FnArgs), d.CreatedAt); err != nil {
			return fmt.Errorf("postgres import deployment %d: %w", d.ID, err)
		}
	}

	// setval(..., max(id)) marks the sequence as having just produced that
	// value, so the next identity is max+1. COALESCE keeps an empty table on
	// safe ground (sequence restarts at 1).
	for _, seq := range []struct{ table, column string }{
		{"contracts", "id"},
		{"deployments", "id"},
	} {
		if _, err := tx.Exec(ctx, fmt.Sprintf(
			`SELECT setval(pg_get_serial_sequence('%s', '%s'), (SELECT COALESCE(MAX(id), 1) FROM %s))`,
			seq.table, seq.column, seq.table)); err != nil {
			return fmt.Errorf("postgres import: advance %s sequence: %w", seq.table, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres import: commit: %w", err)
	}
	return nil
}

// optionalText stores an empty optional field as NULL, as RecordDeployment does.
func optionalText(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

// scanner is satisfied by both pgx.Row and pgx.Rows, so the column lists below
// are written once rather than duplicated for single-row and multi-row reads.
type scanner interface {
	Scan(dest ...any) error
}

func scanContract(s scanner) (Contract, error) {
	var c Contract
	err := s.Scan(&c.ID, &c.Alias, &c.Network, &c.ContractID, &c.CurrentWasmHash, &c.CreatedAt, &c.UpdatedAt)
	return c, err
}

func scanDeployment(s scanner) (Deployment, error) {
	var (
		d           Deployment
		action      string
		txHash      *string
		ledger      *int64
		txGitCommit *string
		txDirty     *bool
		txFee       *int64
		notes       *string
		ciRunURL    *string
		fnName      *string
		fnArgs      *string
	)
	err := s.Scan(&d.ID, &d.Alias, &d.Network, &d.ContractID, &d.WasmHash, &action,
		&d.DeployerPubkey, &txHash, &ledger, &txGitCommit, &txDirty, &txFee, &notes, &ciRunURL, &fnName, &fnArgs, &d.CreatedAt)
	if err != nil {
		return Deployment{}, err
	}

	d.Action = Action(action)
	if txHash != nil {
		d.TxHash = *txHash
	}
	if ledger != nil {
		d.Ledger = uint32(*ledger)
	}
	if txGitCommit != nil {
		d.GitCommit = *txGitCommit
	}
	if txDirty != nil {
		d.Dirty = *txDirty
	}
	if txFee != nil {
		d.FeeCharged = txFee
	}
	if notes != nil {
		d.Notes = *notes
	}
	if ciRunURL != nil {
		d.CIRunURL = *ciRunURL
	}
	if fnName != nil {
		d.FnName = *fnName
	}
	if fnArgs != nil {
		d.FnArgs = *fnArgs
	}
	return d, nil
}
