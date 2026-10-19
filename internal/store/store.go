// Package store persists SoroForge's deployment history.
//
// The history is the point of the tool: it answers "which WASM hash is live on
// which network, and who put it there" long after the terminal that ran the
// deploy is gone. Two tables back that — contracts holds current state, and
// deployments is an append-only log of every deploy, upgrade, and upload.
//
// Everything is namespaced by network. A contract alias means nothing on its
// own; "counter on testnet" and "counter on mainnet" are separate records with
// separate lifecycles, and no query in this package crosses that boundary
// implicitly.
package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// ErrNotFound is returned when a contract has no record on the given network.
// Callers distinguish "SoroForge has never deployed this" from a real failure
// with errors.Is.
var ErrNotFound = errors.New("not found")

// Action is the kind of change a deployment record describes.
type Action string

const (
	// ActionDeploy is the first instantiation of a contract on a network.
	ActionDeploy Action = "deploy"

	// ActionUpgrade replaces the bytecode of an already-deployed contract,
	// keeping its address.
	ActionUpgrade Action = "upgrade"

	// ActionUpload installs bytecode on-chain without deploying or upgrading:
	// the first half of a governance-driven upgrade, where the hash goes into
	// a proposal and a separate party later calls the contract's upgrade
	// function. No contract exists yet, so the record has no contract id.
	ActionUpload Action = "upload"

	// ActionInvoke is an administrative function call against a deployed contract.
	ActionInvoke Action = "invoke"
)

// Valid reports whether a is a recognised action. The database enforces this
// too; checking here produces a better error than a constraint violation.
func (a Action) Valid() bool {
	return a == ActionDeploy || a == ActionUpgrade || a == ActionUpload || a == ActionInvoke
}

func (a Action) String() string { return string(a) }

// Contract is the current tracked state of one contract on one network.
type Contract struct {
	ID              int64     `json:"-"`
	Alias           string    `json:"alias"`
	Network         string    `json:"network"`
	ContractID      string    `json:"contract_id"`
	CurrentWasmHash string    `json:"current_wasm_hash"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// Deployment is one entry in the append-only history.
//
// Records are never updated or deleted: an upgrade appends a row rather than
// modifying the previous one, so the sequence of rows for a contract is its
// version history.
type Deployment struct {
	ID         int64  `json:"id"`
	Alias      string `json:"alias"`
	Network    string `json:"network"`
	ContractID string `json:"contract_id"`
	WasmHash   string `json:"wasm_hash"`
	Action     Action `json:"action"`

	// DeployerPubkey is the G... address that signed. Only the public address
	// is ever stored — never the seed that produced it.
	DeployerPubkey string `json:"deployer_pubkey"`

	// TxHash and Ledger locate the change on-chain so a record can be audited
	// independently of SoroForge.
	TxHash string `json:"tx_hash,omitempty"`
	Ledger uint32 `json:"ledger,omitempty"`

	// ContractURL and TxURL are explorer links filled in for display when the
	// network configures an explorer. They are derived, never stored.
	ContractURL string `json:"contract_url,omitempty"`
	TxURL       string `json:"tx_url,omitempty"`

	// GitCommit is the git commit hash the deploy/upgrade was recorded from.
	// Empty string when run outside a git repository or when detection fails.
	GitCommit string `json:"git_commit"`

	// Dirty reports whether the working tree had uncommitted changes at the time
	// of the deploy/upgrade. Set to true when inside a git repo with modifications.
	Dirty bool `json:"dirty"`

	// FeeCharged is the fee paid for the transaction, in stroops.
	// Nullable: absent for a dry run, or if the fee cannot be determined.
	FeeCharged *int64 `json:"fee_charged,omitempty"`

	// Notes is free-form operator context, e.g. a ticket or release tag.
	Notes string `json:"notes,omitempty"`

	// ConstructorArgs records the typed constructor arguments a deploy used,
	// as JSON in the same shape as soroforge.yaml. Empty for upgrades, which
	// take none, and for deploys with no arguments.
	ConstructorArgs string `json:"constructor_args,omitempty"`

	// CIRunURL is the CI pipeline run URL when deployed from GitHub Actions or
	// GitLab CI. Outside CI this is empty. It answers "which commit and merge
	// triggered this deploy" for audit purposes.
	CIRunURL string `json:"ci_run_url,omitempty"`

	// FnName is the function invoked on the contract, when action is ActionInvoke.
	FnName string `json:"fn_name,omitempty"`

	// FnArgs is the string representation of invocation arguments, when action is ActionInvoke.
	FnArgs string `json:"fn_args,omitempty"`

	CreatedAt time.Time `json:"created_at"`
}

// AttemptStage names where in the submission cycle a failed attempt died.
type AttemptStage string

const (
	// AttemptSimulate is everything before submission: account load,
	// simulation, assembly, signing, and pre-flight config checks.
	AttemptSimulate AttemptStage = "simulate"

	// AttemptSubmit is the send of the signed transaction.
	AttemptSubmit AttemptStage = "submit"

	// AttemptConfirm is waiting for, and reading the result of, a submitted
	// transaction — an included-but-failed or rejected transaction lands here.
	AttemptConfirm AttemptStage = "confirm"
)

// Valid reports whether st is a recognised stage.
func (st AttemptStage) Valid() bool {
	return st == AttemptSimulate || st == AttemptSubmit || st == AttemptConfirm
}

func (st AttemptStage) String() string { return string(st) }

// Attempt is one failed deploy or upgrade — deliberately not a Deployment.
//
// Successful history stays exactly what it was: the deployments table only
// ever holds confirmed changes, and a failure never appears there, so status
// and drift logic keep trusting every row they read. Failures live in their
// own table because they carry real information — an included transaction that
// failed, an upgrade the contract rejected — that a log line loses.
//
// Error may contain RPC responses, and must never contain key material; every
// write path runs it through SanitizeKeyMaterial first.
type Attempt struct {
	ID      int64        `json:"id"`
	Alias   string       `json:"alias"`
	Network string       `json:"network"`
	Action  Action       `json:"action"`
	Stage   AttemptStage `json:"stage"`

	// Error is the failure the operator saw, redacted of any key material.
	Error string `json:"error"`

	// TxHash is the transaction the attempt sent, when it got that far. Empty
	// for a failure at the simulate stage, where nothing was ever submitted.
	TxHash string `json:"tx_hash,omitempty"`

	CreatedAt time.Time `json:"created_at"`
}

// Validate checks an attempt before it is written.
func (a Attempt) Validate() error {
	switch {
	case a.Alias == "":
		return fmt.Errorf("attempt: alias is required")
	case a.Network == "":
		return fmt.Errorf("attempt: network is required")
	case !a.Action.Valid():
		return fmt.Errorf("attempt: action must be %q, %q, %q, or %q, got %q",
			ActionDeploy, ActionUpgrade, ActionUpload, ActionInvoke, a.Action)
	case !a.Stage.Valid():
		return fmt.Errorf("attempt: stage must be %q, %q, or %q, got %q",
			AttemptSimulate, AttemptSubmit, AttemptConfirm, a.Stage)
	case a.Error == "":
		return fmt.Errorf("attempt: error is required — a failure with no explanation records nothing")
	}
	return nil
}

// secretSeed matches a Stellar secret seed strkey: "S" plus 55 characters of
// the base32 alphabet. Matching is case-insensitive because seeds are written
// in both cases in the wild. Anything of this shape in error text is treated
// as key material regardless of whether it decodes — a false redaction in a
// failure message costs a character count; a leaked seed costs the account.
var secretSeed = regexp.MustCompile(`(?i)\bS[A-Z2-7]{55}\b`)

// SanitizeKeyMaterial redacts anything shaped like a secret seed, so error
// text can be recorded — and later printed by `history --failures` — without
// ever carrying a key.
func SanitizeKeyMaterial(s string) string {
	return secretSeed.ReplaceAllString(s, "[REDACTED-SECRET-KEY]")
}

// Store persists contracts and their deployment history.
//
// It is an interface so that orchestration can be tested without Postgres — see
// MemoryStore — and so that a different backend can be substituted without
// touching the deploy logic.
type Store interface {
	// UpsertContract records or updates a contract's current state, keyed by
	// (network, alias).
	UpsertContract(ctx context.Context, c Contract) (Contract, error)

	// GetContract returns one contract. It returns ErrNotFound if the alias is
	// not tracked on that network.
	GetContract(ctx context.Context, network, alias string) (Contract, error)

	// ListContracts returns tracked contracts. An empty network returns
	// contracts across all networks.
	ListContracts(ctx context.Context, network string) ([]Contract, error)

	// ListContractsPage returns one page using the same network/alias ordering.
	ListContractsPage(ctx context.Context, network string, limit, offset int) ([]Contract, error)

	// RecordDeployment appends a history entry.
	RecordDeployment(ctx context.Context, d Deployment) (Deployment, error)

	// History returns a contract's deployments, newest first.
	History(ctx context.Context, network, alias string) ([]Deployment, error)

	// GetDeployment returns one deployment by its ID. It returns ErrNotFound if
	// no deployment with that ID exists.
	GetDeployment(ctx context.Context, id int64) (Deployment, error)

	// ListAllDeployments returns every recorded deployment across all
	// networks and aliases, in id order. It backs `soroforge history export`,
	// which must capture deployments whose alias has since left the config —
	// History alone cannot enumerate them, because it needs a (network,
	// alias) to start from.
	ListAllDeployments(ctx context.Context) ([]Deployment, error)

	// HistoryPage returns one newest-first page of a contract's deployment log.
	HistoryPage(ctx context.Context, network, alias string, limit, offset int) ([]Deployment, error)

	// RecordAttempt appends a failed deploy/upgrade attempt. It is kept out of
	// the deployments table on purpose: a failure must never surface as a
	// confirmed history entry, so status and drift logic are unaffected.
	RecordAttempt(ctx context.Context, a Attempt) (Attempt, error)

	// Attempts returns a contract's failed attempts, newest first.
	Attempts(ctx context.Context, network, alias string) ([]Attempt, error)

	// Close releases resources held by the store.
	Close() error
}

// Open returns the production Store selected by the DATABASE_URL scheme:
// sqlite://... opens the file-backed SQLite store, anything else is treated
// as a Postgres connection string.
//
// The dispatch lives here so the CLI and any future entry point get one
// answer to "which backend is this URL", consistent with NewMigrator, which
// reads the same scheme to pick a migration set.
func Open(ctx context.Context, databaseURL string) (Store, error) {
	if IsSQLiteURL(databaseURL) {
		return NewSQLite(ctx, databaseURL)
	}
	return NewPostgres(ctx, databaseURL)
}

func validatePage(limit, offset int) error {
	if limit < 1 {
		return fmt.Errorf("page limit must be positive")
	}
	if offset < 0 {
		return fmt.Errorf("page offset must be non-negative")
	}
	return nil
}

// Validate checks a deployment record before it is written, so that bad data is
// rejected with a readable message rather than a constraint violation.
func (d Deployment) Validate() error {
	switch {
	case d.Alias == "":
		return fmt.Errorf("deployment: alias is required")
	case d.Network == "":
		return fmt.Errorf("deployment: network is required")
	// An upload installs bytecode, which is content-addressed by its wasm hash
	// and has no contract address; only a deploy or upgrade names a contract.
	case d.ContractID == "" && d.Action != ActionUpload:
		return fmt.Errorf("deployment: contract_id is required")
	case d.WasmHash == "":
		return fmt.Errorf("deployment: wasm_hash is required")
	case !d.Action.Valid():
		return fmt.Errorf("deployment: action must be %q, %q, %q, or %q, got %q",
			ActionDeploy, ActionUpgrade, ActionUpload, ActionInvoke, d.Action)
	case d.DeployerPubkey == "":
		return fmt.Errorf("deployment: deployer_pubkey is required")
	}
	return nil
}

// Validate checks a contract record before it is written.
func (c Contract) Validate() error {
	switch {
	case c.Alias == "":
		return fmt.Errorf("contract: alias is required")
	case c.Network == "":
		return fmt.Errorf("contract: network is required")
	case c.ContractID == "":
		return fmt.Errorf("contract: contract_id is required")
	case c.CurrentWasmHash == "":
		return fmt.Errorf("contract: current_wasm_hash is required")
	}
	return nil
}
