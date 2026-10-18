package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// Snapshot is the versioned JSON document `soroforge history export` writes
// and `soroforge history import` reads. It is the whole deployment history —
// every contract's current state and every recorded deploy, upgrade, and
// upload — portable between databases, environments, and CI caches.
//
// # Format
//
// The document carries an explicit schema version so a reader never has to
// guess what it is parsing:
//
//	{
//	  "version": 1,
//	  "generated_at": "2026-02-01T09:00:00Z",
//	  "contracts": [
//	    {"id": 1, "alias": "counter", "network": "testnet",
//	     "contract_id": "C2W...", "current_wasm_hash": "a1b2...",
//	     "created_at": "...", "updated_at": "..."}
//	  ],
//	  "deployments": [
//	    {"id": 2, "alias": "counter", "network": "testnet",
//	     "contract_id": "C2W...", "wasm_hash": "a1b2...", "action": "deploy",
//	     "deployer_pubkey": "GABC...", "tx_hash": "...", "ledger": 123,
//	     "notes": "v1.0.0", "created_at": "..."}
//	  ]
//	}
//
// Version 1 rules, which import enforces:
//
//   - ids and timestamps are the original ones. Import never re-stamps: a
//     record that says it landed in ledger 4500 on 2026-01-02 still says
//     that after a round trip, and still carries the same id, so notes,
//     tickets, or scripts that reference ids keep working.
//   - contract ids and deployment ids must be non-zero and unique within the
//     document.
//   - every record must satisfy the same Validate() rules the live store
//     enforces, so a hand-edited file fails at parse time with a readable
//     message rather than half-applied.
//
// A future version may add fields; readers of version 1 reject unknown fields
// loudly instead of ignoring them, because a file written by a newer tool is
// not this tool's format to guess at.
type Snapshot struct {
	// Version is the snapshot schema version. This build writes and reads
	// SnapshotVersion; anything else in a file is a loud error.
	Version int `json:"version"`

	// GeneratedAt is when the export ran. It is metadata, not history: two
	// exports of an unchanged database differ only here.
	GeneratedAt time.Time `json:"generated_at"`

	Contracts   []SnapshotContract `json:"contracts"`
	Deployments []Deployment       `json:"deployments"`
}

// SnapshotVersion is the format version this build writes and reads.
const SnapshotVersion = 1

// SnapshotContract is Contract as it appears in a snapshot. store.Contract
// hides its row id from JSON (`json:"-"`) because the API should not leak
// database internals; a history file is precisely the place ids belong, so
// the snapshot gets its own shape rather than a tag change with blast radius
// into every API response.
type SnapshotContract struct {
	ID              int64     `json:"id"`
	Alias           string    `json:"alias"`
	Network         string    `json:"network"`
	ContractID      string    `json:"contract_id"`
	CurrentWasmHash string    `json:"current_wasm_hash"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// ToContract converts the record back to the live store shape.
func (sc SnapshotContract) ToContract() Contract {
	return Contract{
		ID:              sc.ID,
		Alias:           sc.Alias,
		Network:         sc.Network,
		ContractID:      sc.ContractID,
		CurrentWasmHash: sc.CurrentWasmHash,
		CreatedAt:       sc.CreatedAt,
		UpdatedAt:       sc.UpdatedAt,
	}
}

func contractToSnapshot(c Contract) SnapshotContract {
	return SnapshotContract{
		ID:              c.ID,
		Alias:           c.Alias,
		Network:         c.Network,
		ContractID:      c.ContractID,
		CurrentWasmHash: c.CurrentWasmHash,
		CreatedAt:       c.CreatedAt,
		UpdatedAt:       c.UpdatedAt,
	}
}

// BuildSnapshot reads the entire history from a store. Contracts come back in
// (network, alias) order and deployments in id order, so exports are stable
// and diffable.
func BuildSnapshot(ctx context.Context, s Store) (*Snapshot, error) {
	contracts, err := s.ListContracts(ctx, "")
	if err != nil {
		return nil, err
	}
	deployments, err := s.ListAllDeployments(ctx)
	if err != nil {
		return nil, err
	}

	snap := &Snapshot{
		Version:     SnapshotVersion,
		GeneratedAt: time.Now().UTC(),
		Contracts:   make([]SnapshotContract, 0, len(contracts)),
		Deployments: deployments,
	}
	if snap.Deployments == nil {
		snap.Deployments = []Deployment{}
	}
	for _, c := range contracts {
		snap.Contracts = append(snap.Contracts, contractToSnapshot(c))
	}
	return snap, nil
}

// Marshal renders the snapshot as indented JSON.
func (snap *Snapshot) Marshal() ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(snap); err != nil {
		return nil, fmt.Errorf("history: encode snapshot: %w", err)
	}
	return buf.Bytes(), nil
}

// ParseSnapshot decodes and validates a history file.
func ParseSnapshot(data []byte) (*Snapshot, error) {
	var snap Snapshot
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&snap); err != nil {
		return nil, fmt.Errorf("history: parse file: %w", err)
	}
	if err := dec.Decode(&json.RawMessage{}); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("history: file contains more than one JSON document")
	}

	if snap.Version != SnapshotVersion {
		return nil, fmt.Errorf(
			"history: unsupported format version %d (this build reads version %d)",
			snap.Version, SnapshotVersion)
	}

	seenContractID := map[int64]bool{}
	for i, c := range snap.Contracts {
		if err := c.ToContract().Validate(); err != nil {
			return nil, fmt.Errorf("history: contracts[%d]: %w", i, err)
		}
		if c.ID <= 0 {
			return nil, fmt.Errorf("history: contracts[%d] (%s/%s): id must be positive; version 1 files carry original ids", i, c.Network, c.Alias)
		}
		if seenContractID[c.ID] {
			return nil, fmt.Errorf("history: contracts[%d]: duplicate contract id %d", i, c.ID)
		}
		seenContractID[c.ID] = true
		if c.CreatedAt.IsZero() || c.UpdatedAt.IsZero() {
			return nil, fmt.Errorf("history: contracts[%d] (%s/%s): created_at and updated_at are required; import preserves timestamps and does not re-stamp", i, c.Network, c.Alias)
		}
	}

	seenDeploymentID := map[int64]bool{}
	for i, d := range snap.Deployments {
		if err := d.Validate(); err != nil {
			return nil, fmt.Errorf("history: deployments[%d]: %w", i, err)
		}
		if d.ID <= 0 {
			return nil, fmt.Errorf("history: deployments[%d] (%s/%s): id must be positive; version 1 files carry original ids", i, d.Network, d.Alias)
		}
		if seenDeploymentID[d.ID] {
			return nil, fmt.Errorf("history: deployments[%d]: duplicate deployment id %d", i, d.ID)
		}
		seenDeploymentID[d.ID] = true
		if d.CreatedAt.IsZero() {
			return nil, fmt.Errorf("history: deployments[%d] (%s/%s): created_at is required; import preserves timestamps and does not re-stamp", i, d.Network, d.Alias)
		}
	}

	if snap.Contracts == nil {
		snap.Contracts = []SnapshotContract{}
	}
	if snap.Deployments == nil {
		snap.Deployments = []Deployment{}
	}
	return &snap, nil
}

// ImportOptions tunes Import.
type ImportOptions struct {
	// Merge permits a non-empty target database and skips records that
	// exactly duplicate what is already stored. Without it, Import refuses a
	// database that already holds history, so a mistaken import cannot pile
	// a second copy of the world onto the first.
	Merge bool
}

// ImportReport tells the caller what Import actually did, so the CLI can say
// "imported 4, skipped 12" rather than leaving --merge's effect a mystery.
type ImportReport struct {
	ContractsImported   int
	DeploymentsImported int
	ContractsSkipped    int
	DeploymentsSkipped  int
}

// snapshotImporter is implemented by every Store that can insert records
// verbatim, preserving ids and timestamps. MemoryStore, Postgres, and SQLite
// do; anything else fails with a clear error rather than a silent re-stamp.
type snapshotImporter interface {
	importSnapshot(ctx context.Context, contracts []Contract, deployments []Deployment) error
}

// Import loads a snapshot into a store, preserving ids and timestamps.
//
// On an empty database every record is inserted as it appears in the file.
// With Merge, records identical to ones already present are skipped —
// re-running the same import is a no-op, and importing an environment's
// history into a slightly-different one adds only what was missing. Any
// *conflict* (same id, different content; same (network, alias), different
// id) fails the import instead of guessing, because both cases mean the
// file and the database disagree about identity, and silently picking one
// would corrupt the history the tool exists to protect.
func Import(ctx context.Context, s Store, snap *Snapshot, opts ImportOptions) (*ImportReport, error) {
	importer, ok := s.(snapshotImporter)
	if !ok {
		return nil, fmt.Errorf("history: store implementation %T cannot import records without re-stamping ids and timestamps", s)
	}

	existingContracts, err := s.ListContracts(ctx, "")
	if err != nil {
		return nil, err
	}
	existingDeployments, err := s.ListAllDeployments(ctx)
	if err != nil {
		return nil, err
	}

	if !opts.Merge && (len(existingContracts) > 0 || len(existingDeployments) > 0) {
		return nil, fmt.Errorf(
			"history: database is not empty (%d contract(s), %d deployment(s)); "+
				"import into a fresh database or pass --merge to combine histories",
			len(existingContracts), len(existingDeployments))
	}

	byContractID := make(map[int64]Contract, len(existingContracts))
	byContractKey := make(map[string]int64, len(existingContracts))
	for _, c := range existingContracts {
		byContractID[c.ID] = c
		byContractKey[c.Network+"\x00"+c.Alias] = c.ID
	}
	byDeploymentID := make(map[int64]Deployment, len(existingDeployments))
	for _, d := range existingDeployments {
		byDeploymentID[d.ID] = d
	}

	contracts := make([]Contract, 0, len(snap.Contracts))
	skippedContracts := 0
	for _, sc := range snap.Contracts {
		c := sc.ToContract()
		if existing, dup := byContractID[c.ID]; dup {
			if contractsEqual(existing, c) {
				skippedContracts++
				continue
			}
			return nil, fmt.Errorf(
				"history: contract id %d already exists with different content (%s/%s in database, %s/%s in file); "+
					"the file and the database disagree about identities",
				c.ID, existing.Network, existing.Alias, c.Network, c.Alias)
		}
		if existingID, taken := byContractKey[c.Network+"\x00"+c.Alias]; taken {
			return nil, fmt.Errorf(
				"history: %s on %s is already tracked with id %d, but the file records it with id %d; "+
					"refusing to renumber a contract",
				c.Alias, c.Network, existingID, c.ID)
		}
		contracts = append(contracts, c)
	}

	deployments := make([]Deployment, 0, len(snap.Deployments))
	skippedDeployments := 0
	for _, d := range snap.Deployments {
		if existing, dup := byDeploymentID[d.ID]; dup {
			if deploymentsEqual(existing, d) {
				skippedDeployments++
				continue
			}
			return nil, fmt.Errorf(
				"history: deployment id %d already exists with different content (%s/%s %s in database, %s/%s %s in file); "+
					"the file and the database disagree about identities",
				d.ID, existing.Network, existing.Alias, existing.Action, d.Network, d.Alias, d.Action)
		}
		deployments = append(deployments, d)
	}

	report := &ImportReport{
		ContractsImported:   len(contracts),
		DeploymentsImported: len(deployments),
		ContractsSkipped:    skippedContracts,
		DeploymentsSkipped:  skippedDeployments,
	}

	if len(contracts) == 0 && len(deployments) == 0 {
		return report, nil
	}
	if err := importer.importSnapshot(ctx, contracts, deployments); err != nil {
		return nil, err
	}
	return report, nil
}

// contractsEqual compares persisted contract content, ignoring timestamps'
// sub-microsecond noise: Postgres stores microseconds, so a round trip
// through it can legally drop nanoseconds an in-memory store kept.
func contractsEqual(a, b Contract) bool {
	return a.Alias == b.Alias &&
		a.Network == b.Network &&
		a.ContractID == b.ContractID &&
		a.CurrentWasmHash == b.CurrentWasmHash &&
		a.CreatedAt.Round(time.Microsecond).Equal(b.CreatedAt.Round(time.Microsecond)) &&
		a.UpdatedAt.Round(time.Microsecond).Equal(b.UpdatedAt.Round(time.Microsecond))
}

func deploymentsEqual(a, b Deployment) bool {
	return a.Alias == b.Alias &&
		a.Network == b.Network &&
		a.ContractID == b.ContractID &&
		a.WasmHash == b.WasmHash &&
		a.Action == b.Action &&
		a.DeployerPubkey == b.DeployerPubkey &&
		a.TxHash == b.TxHash &&
		a.Ledger == b.Ledger &&
		a.Notes == b.Notes &&
		a.CreatedAt.Round(time.Microsecond).Equal(b.CreatedAt.Round(time.Microsecond))
}
