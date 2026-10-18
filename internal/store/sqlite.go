package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	// The pure-Go SQLite driver. Registered as "sqlite"; CGO_ENABLED=0 builds
	// keep working, which is the whole reason SoroForge's SQLite backend uses
	// this rather than the mattn/cgo one.
	_ "modernc.org/sqlite"
)

// SQLite is a Store backed by a single SQLite file, selected with
// DATABASE_URL=sqlite:///path/to/file.db.
//
// It exists for the single-machine and CI-cache cases where provisioning
// Postgres is more machinery than the deployment history is worth: one file,
// no server, no Docker. It passes the same conformance suite as Postgres and
// MemoryStore, so every command behaves identically against it.
//
// Timestamps are stored as fixed-width UTC text (see sqliteTimeFormat) because
// SQLite has no timestamp type and sorts TEXT lexicographically; variable
// width fractional seconds would misorder history.
type SQLite struct {
	db *sql.DB
}

var _ Store = (*SQLite)(nil)

// sqliteTimeFormat is a fixed-layout RFC3339 with exactly six fractional
// digits and a literal Z: microsecond precision, matching Postgres'
// timestamptz, so a snapshot exported from either backend re-imports into
// the other without precision surprises, and string ordering equals time
// ordering inside SQLite.
const sqliteTimeFormat = "2006-01-02T15:04:05.000000Z"

// defaultSQLitePragmas bound a file-backed store: writers wait instead of
// failing instantly, and WAL keeps readers (list, history, status) from
// blocking the occasional writer.
const defaultSQLitePragmas = "_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"

// NewSQLite opens the SQLite file named by a sqlite:// DATABASE_URL and
// verifies it answers.
//
// The schema is not created here: `soroforge migrate up` owns that, exactly as
// for Postgres, so opening never mutates a file behind the operator's back.
func NewSQLite(ctx context.Context, databaseURL string) (*SQLite, error) {
	dsn, err := sqliteDSN(databaseURL)
	if err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open sqlite: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: sqlite file %s: %w", redactedSQLitePath(databaseURL), err)
	}

	return &SQLite{db: db}, nil
}

// sqliteDSN turns sqlite:///path[/to]/file.db[?_pragma=...] into the file
// path plus pragmas the driver understands. An explicit query in the URL
// wins over the defaults, so operators can tune busy_timeout or disable WAL.
func sqliteDSN(databaseURL string) (string, error) {
	if databaseURL == "" {
		return "", fmt.Errorf("store: database URL is required")
	}
	if !IsSQLiteURL(databaseURL) {
		return "", fmt.Errorf("store: expected a %s URL, got a different scheme", SQLiteURLPrefix)
	}

	rest := databaseURL[len(SQLiteURLPrefix):]
	path, query := rest, ""
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path, query = path[:i], path[i+1:]
	}
	if path == "" {
		return "", fmt.Errorf("store: sqlite URL names no file (want %s/path/to/file.db)", strings.TrimSuffix(SQLiteURLPrefix, "/"))
	}
	if query == "" {
		query = defaultSQLitePragmas
	}
	return path + "?" + query, nil
}

// redactedSQLitePath keeps error messages free of the query string, which can
// carry pragmas — the path itself is not a secret, it is on the command line.
func redactedSQLitePath(databaseURL string) string {
	if i := strings.IndexByte(databaseURL, '?'); i >= 0 {
		return databaseURL[:i]
	}
	return databaseURL
}

// Close releases the database handle.
func (s *SQLite) Close() error { return s.db.Close() }

// DB exposes the underlying handle for tests that need to inspect the schema
// directly — the SQLite counterpart of Postgres.Pool().
func (s *SQLite) DB() *sql.DB { return s.db }

// formatSQLiteTime renders a timestamp for storage. Every write path supplies
// an explicit, non-zero time (RecordDeployment stamps now(), import rejects
// missing timestamps), so nothing here needs a zero-time fallback.
func formatSQLiteTime(t time.Time) string {
	return t.UTC().Format(sqliteTimeFormat)
}

func parseSQLiteTime(raw string) (time.Time, error) {
	t, err := time.Parse(sqliteTimeFormat, raw)
	if err != nil {
		// Tolerate a second-less timestamp from a hand-edited file; the
		// store always writes full precision.
		t, err2 := time.Parse(time.RFC3339, raw)
		if err2 != nil {
			return time.Time{}, fmt.Errorf("store: unparsable timestamp %q: %w", raw, err)
		}
		return t, nil
	}
	return t, nil
}

const sqliteUpsertContract = `
INSERT INTO contracts (alias, network, contract_id, current_wasm_hash, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (network, alias) DO UPDATE
SET contract_id       = excluded.contract_id,
    current_wasm_hash = excluded.current_wasm_hash,
    updated_at        = excluded.updated_at
RETURNING id, alias, network, contract_id, current_wasm_hash, created_at, updated_at`

// UpsertContract implements Store.
func (s *SQLite) UpsertContract(ctx context.Context, c Contract) (Contract, error) {
	if err := c.Validate(); err != nil {
		return Contract{}, err
	}
	now := formatSQLiteTime(time.Now().UTC())
	row := s.db.QueryRowContext(ctx, sqliteUpsertContract,
		c.Alias, c.Network, c.ContractID, c.CurrentWasmHash, now, now)
	out, err := scanSQLiteContract(row)
	if err != nil {
		return Contract{}, fmt.Errorf("upsert contract %s/%s: %w", c.Network, c.Alias, err)
	}
	return out, nil
}

const sqliteGetContract = `
SELECT id, alias, network, contract_id, current_wasm_hash, created_at, updated_at
FROM contracts
WHERE network = ? AND alias = ?`

// GetContract implements Store.
func (s *SQLite) GetContract(ctx context.Context, network, alias string) (Contract, error) {
	row := s.db.QueryRowContext(ctx, sqliteGetContract, network, alias)
	c, err := scanSQLiteContract(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Contract{}, fmt.Errorf("contract %s on %s: %w", alias, network, ErrNotFound)
	}
	if err != nil {
		return Contract{}, fmt.Errorf("get contract %s/%s: %w", network, alias, err)
	}
	return c, nil
}

const sqliteListContracts = `
SELECT id, alias, network, contract_id, current_wasm_hash, created_at, updated_at
FROM contracts
WHERE (? = '' OR network = ?)
ORDER BY network, alias`

// ListContracts implements Store.
func (s *SQLite) ListContracts(ctx context.Context, network string) ([]Contract, error) {
	rows, err := s.db.QueryContext(ctx, sqliteListContracts, network, network)
	if err != nil {
		return nil, fmt.Errorf("list contracts: %w", err)
	}
	defer rows.Close()

	contracts := []Contract{}
	for rows.Next() {
		c, err := scanSQLiteContract(rows)
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

// ListContractsPage implements Store with the same filter and ordering as
// ListContracts.
func (s *SQLite) ListContractsPage(ctx context.Context, network string, limit, offset int) ([]Contract, error) {
	if err := validatePage(limit, offset); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, sqliteListContracts+"\nLIMIT ? OFFSET ?", network, network, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("list contracts page: %w", err)
	}
	defer rows.Close()

	contracts := []Contract{}
	for rows.Next() {
		c, err := scanSQLiteContract(rows)
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

const sqliteRecordDeployment = `
INSERT INTO deployments (
    alias, network, contract_id, wasm_hash, action, deployer_pubkey,
    tx_hash, ledger, git_commit, dirty, fee_charged, notes,
    ci_run_url, fn_name, fn_args, created_at
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING id, alias, network, contract_id, wasm_hash, action,
          deployer_pubkey, tx_hash, ledger, git_commit, dirty, fee_charged,
          notes, ci_run_url, fn_name, fn_args, created_at`

// RecordDeployment implements Store.
func (s *SQLite) RecordDeployment(ctx context.Context, d Deployment) (Deployment, error) {
	if err := d.Validate(); err != nil {
		return Deployment{}, err
	}
	row := s.db.QueryRowContext(ctx, sqliteRecordDeployment,
		d.Alias, d.Network, d.ContractID, d.WasmHash, string(d.Action),
		d.DeployerPubkey, nullableString(d.TxHash), nullableInt64(int64(d.Ledger)),
		nullableString(d.GitCommit), nullableDirty(d), nullableFee(d.FeeCharged), nullableString(d.Notes),
		nullableString(d.CIRunURL), nullableString(d.FnName), nullableString(d.FnArgs),
		formatSQLiteTime(time.Now().UTC()))

	out, err := scanSQLiteDeployment(row)
	if err != nil {
		return Deployment{}, fmt.Errorf("record deployment for %s/%s: %w", d.Network, d.Alias, err)
	}
	return out, nil
}

const sqliteHistory = `
SELECT id, alias, network, contract_id, wasm_hash, action,
       deployer_pubkey, tx_hash, ledger, git_commit, dirty, fee_charged,
       notes, ci_run_url, fn_name, fn_args, created_at
FROM deployments
WHERE network = ? AND alias = ?
ORDER BY created_at DESC, id DESC`

const sqliteGetDeployment = `
SELECT id, alias, network, contract_id, wasm_hash, action,
       deployer_pubkey, tx_hash, ledger, git_commit, dirty, fee_charged,
       notes, ci_run_url, fn_name, fn_args, created_at
FROM deployments
WHERE id = ?`

// GetDeployment implements Store.
func (s *SQLite) GetDeployment(ctx context.Context, id int64) (Deployment, error) {
	d, err := scanSQLiteDeployment(s.db.QueryRowContext(ctx, sqliteGetDeployment, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Deployment{}, fmt.Errorf("deployment %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return Deployment{}, fmt.Errorf("get deployment %d: %w", id, err)
	}
	return d, nil
}

// History implements Store. Ordered by timestamp then id, so records sharing
// a clock tick keep insertion order — the same guarantee Postgres gives.
func (s *SQLite) History(ctx context.Context, network, alias string) ([]Deployment, error) {
	rows, err := s.db.QueryContext(ctx, sqliteHistory, network, alias)
	if err != nil {
		return nil, fmt.Errorf("history for %s/%s: %w", network, alias, err)
	}
	defer rows.Close()

	deployments := []Deployment{}
	for rows.Next() {
		d, err := scanSQLiteDeployment(rows)
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

// HistoryPage implements Store, in the same newest-first order as History so
// pages concatenate to the full log.
func (s *SQLite) HistoryPage(ctx context.Context, network, alias string, limit, offset int) ([]Deployment, error) {
	if err := validatePage(limit, offset); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, sqliteHistory+"\nLIMIT ? OFFSET ?", network, alias, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("history page for %s/%s: %w", network, alias, err)
	}
	defer rows.Close()

	deployments := []Deployment{}
	for rows.Next() {
		d, err := scanSQLiteDeployment(rows)
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

const sqliteListAllDeployments = `
SELECT id, alias, network, contract_id, wasm_hash, action,
       deployer_pubkey, tx_hash, ledger, git_commit, dirty, fee_charged,
       notes, ci_run_url, fn_name, fn_args, created_at
FROM deployments
ORDER BY id`

// ListAllDeployments implements Store.
func (s *SQLite) ListAllDeployments(ctx context.Context) ([]Deployment, error) {
	rows, err := s.db.QueryContext(ctx, sqliteListAllDeployments)
	if err != nil {
		return nil, fmt.Errorf("list deployments: %w", err)
	}
	defer rows.Close()

	deployments := []Deployment{}
	for rows.Next() {
		d, err := scanSQLiteDeployment(rows)
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

// scanSQLite* mirror their pgx counterparts but read timestamps as text,
// which is how SQLite stores them.

type rowScanner interface {
	Scan(dest ...any) error
}

func scanSQLiteContract(s rowScanner) (Contract, error) {
	var (
		c                Contract
		createdAt, updAt string
	)
	if err := s.Scan(&c.ID, &c.Alias, &c.Network, &c.ContractID, &c.CurrentWasmHash, &createdAt, &updAt); err != nil {
		return Contract{}, err
	}
	var err error
	if c.CreatedAt, err = parseSQLiteTime(createdAt); err != nil {
		return Contract{}, err
	}
	if c.UpdatedAt, err = parseSQLiteTime(updAt); err != nil {
		return Contract{}, err
	}
	return c, nil
}

func scanSQLiteDeployment(s rowScanner) (Deployment, error) {
	var (
		d             Deployment
		action        string
		createdAt     string
		txHash, notes sql.NullString
		ledger        sql.NullInt64
		gitCommit     sql.NullString
		dirty         sql.NullBool
		fee           sql.NullInt64
		ciRunURL      sql.NullString
		fnName        sql.NullString
		fnArgs        sql.NullString
	)
	if err := s.Scan(&d.ID, &d.Alias, &d.Network, &d.ContractID, &d.WasmHash, &action,
		&d.DeployerPubkey, &txHash, &ledger, &gitCommit, &dirty, &fee,
		&notes, &ciRunURL, &fnName, &fnArgs, &createdAt); err != nil {
		return Deployment{}, err
	}

	d.Action = Action(action)
	d.GitCommit = gitCommit.String
	d.Dirty = dirty.Valid && dirty.Bool
	if fee.Valid {
		d.FeeCharged = &fee.Int64
	}
	d.CIRunURL = ciRunURL.String
	d.FnName = fnName.String
	d.FnArgs = fnArgs.String
	if txHash.Valid {
		d.TxHash = txHash.String
	}
	if ledger.Valid {
		d.Ledger = uint32(ledger.Int64)
	}
	if notes.Valid {
		d.Notes = notes.String
	}
	var err error
	if d.CreatedAt, err = parseSQLiteTime(createdAt); err != nil {
		return Deployment{}, err
	}
	return d, nil
}

func nullableString(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func nullableInt64(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

// nullableFee keeps a zero fee distinct from an unknown one: only a nil
// pointer is NULL, matching the Postgres store.
func nullableFee(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

// nullableDirty records the dirty flag only alongside a commit, as the
// Postgres store does; without a commit there is no working tree to describe.
func nullableDirty(d Deployment) any {
	if d.GitCommit == "" {
		return nil
	}
	return d.Dirty
}

// importSnapshot implements snapshotImporter for SQLite.
//
// Explicit ids insert cleanly: a plain INTEGER PRIMARY KEY is the rowid, and
// SQLite advances its autoincrement counter past any id supplied on insert,
// so the next RecordDeployment cannot collide — no setval equivalent needed.
// The whole import runs in one transaction: half a history is worse than none.
func (s *SQLite) importSnapshot(ctx context.Context, contracts []Contract, deployments []Deployment) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite import: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, c := range contracts {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO contracts (id, alias, network, contract_id, current_wasm_hash, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`,
			c.ID, c.Alias, c.Network, c.ContractID, c.CurrentWasmHash,
			formatSQLiteTime(c.CreatedAt), formatSQLiteTime(c.UpdatedAt)); err != nil {
			return fmt.Errorf("sqlite import contract %s/%s: %w", c.Network, c.Alias, err)
		}
	}

	for _, d := range deployments {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO deployments (id, alias, network, contract_id, wasm_hash, action,
                         deployer_pubkey, tx_hash, ledger, git_commit, dirty, fee_charged,
                         notes, ci_run_url, fn_name, fn_args, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			d.ID, d.Alias, d.Network, d.ContractID, d.WasmHash, string(d.Action),
			d.DeployerPubkey, nullableString(d.TxHash), nullableInt64(int64(d.Ledger)),
			nullableString(d.GitCommit), nullableDirty(d), nullableFee(d.FeeCharged), nullableString(d.Notes),
			nullableString(d.CIRunURL), nullableString(d.FnName), nullableString(d.FnArgs),
			formatSQLiteTime(d.CreatedAt)); err != nil {
			return fmt.Errorf("sqlite import deployment %d: %w", d.ID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite import: commit: %w", err)
	}
	return nil
}
