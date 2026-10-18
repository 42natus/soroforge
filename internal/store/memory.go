package store

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// MemoryStore is an in-memory Store.
//
// It exists so that `go test ./...` never needs a database: the deploy
// orchestration, the CLI, and the HTTP API are all exercised against this
// rather than Postgres. It is shipped in the package (not a _test.go file)
// because those other packages need it too, and because it makes `soroforge`
// runnable for a quick local try without provisioning anything.
//
// It is safe for concurrent use, but it is not durable and is not intended for
// production use.
type MemoryStore struct {
	mu sync.RWMutex

	contracts   map[string]Contract // keyed by network\x00alias
	deployments []Deployment
	nextID      int64

	// now is overridable so tests can produce deterministic timestamps.
	now func() time.Time
}

var _ Store = (*MemoryStore)(nil)

// NewMemoryStore returns an empty in-memory store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		contracts: make(map[string]Contract),
		nextID:    1,
		now:       time.Now,
	}
}

// SetClock overrides the store's time source, for deterministic tests.
func (m *MemoryStore) SetClock(now func() time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.now = now
}

func contractKey(network, alias string) string {
	// A NUL separator cannot appear in either component, so keys cannot collide
	// the way "a"+"b" and "ab" would with an empty separator.
	return network + "\x00" + alias
}

// UpsertContract implements Store.
func (m *MemoryStore) UpsertContract(ctx context.Context, c Contract) (Contract, error) {
	if err := ctx.Err(); err != nil {
		return Contract{}, err
	}
	if err := c.Validate(); err != nil {
		return Contract{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	key := contractKey(c.Network, c.Alias)
	now := m.now()

	if existing, ok := m.contracts[key]; ok {
		existing.ContractID = c.ContractID
		existing.CurrentWasmHash = c.CurrentWasmHash
		existing.UpdatedAt = now
		m.contracts[key] = existing
		return existing, nil
	}

	c.ID = m.nextID
	m.nextID++
	c.CreatedAt = now
	c.UpdatedAt = now
	m.contracts[key] = c
	return c, nil
}

// GetContract implements Store.
func (m *MemoryStore) GetContract(ctx context.Context, network, alias string) (Contract, error) {
	if err := ctx.Err(); err != nil {
		return Contract{}, err
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	c, ok := m.contracts[contractKey(network, alias)]
	if !ok {
		return Contract{}, fmt.Errorf("contract %s on %s: %w", alias, network, ErrNotFound)
	}
	return c, nil
}

// ListContracts implements Store.
func (m *MemoryStore) ListContracts(ctx context.Context, network string) ([]Contract, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	out := []Contract{}
	for _, c := range m.contracts {
		if network == "" || c.Network == network {
			out = append(out, c)
		}
	}
	// Matches the Postgres ORDER BY, so callers see the same ordering from
	// either implementation.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Network != out[j].Network {
			return out[i].Network < out[j].Network
		}
		return out[i].Alias < out[j].Alias
	})
	return out, nil
}

// ListContractsPage implements Store.
func (m *MemoryStore) ListContractsPage(ctx context.Context, network string, limit, offset int) ([]Contract, error) {
	if err := validatePage(limit, offset); err != nil {
		return nil, err
	}
	contracts, err := m.ListContracts(ctx, network)
	if err != nil {
		return nil, err
	}
	if offset >= len(contracts) {
		return []Contract{}, nil
	}
	end := len(contracts)
	if limit < len(contracts)-offset {
		end = offset + limit
	}
	return contracts[offset:end], nil
}

// RecordDeployment implements Store.
func (m *MemoryStore) RecordDeployment(ctx context.Context, d Deployment) (Deployment, error) {
	if err := ctx.Err(); err != nil {
		return Deployment{}, err
	}
	if err := d.Validate(); err != nil {
		return Deployment{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	d.ID = m.nextID
	m.nextID++
	d.CreatedAt = m.now()

	if d.FeeCharged != nil {
		fee := *d.FeeCharged
		d.FeeCharged = &fee
	}

	m.deployments = append(m.deployments, d)
	return d, nil
}

// History implements Store.
func (m *MemoryStore) History(ctx context.Context, network, alias string) ([]Deployment, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	out := []Deployment{}
	for _, d := range m.deployments {
		if d.Network == network && d.Alias == alias {
			out = append(out, d)
		}
	}
	// Newest first, breaking ties by ID so records sharing a timestamp keep
	// insertion order — the same guarantee the Postgres query makes.
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	return out, nil
}

// ListAllDeployments implements Store. Returned in id order, matching the
// Postgres and SQLite ORDER BY, so exports are byte-identical across backends.
func (m *MemoryStore) ListAllDeployments(ctx context.Context) ([]Deployment, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]Deployment, len(m.deployments))
	copy(out, m.deployments)
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// importSnapshot implements snapshotImporter: inserts records verbatim,
// preserving the ids and timestamps an import carries rather than re-stamping
// them, and advances the id counter past everything imported so subsequent
// RecordDeployment calls cannot collide.
func (m *MemoryStore) importSnapshot(ctx context.Context, contracts []Contract, deployments []Deployment) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	next := m.nextID
	for _, c := range contracts {
		m.contracts[contractKey(c.Network, c.Alias)] = c
		if c.ID >= next {
			next = c.ID + 1
		}
	}
	for _, d := range deployments {
		m.deployments = append(m.deployments, d)
		if d.ID >= next {
			next = d.ID + 1
		}
	}
	m.nextID = next
	return nil
}

// HistoryPage implements Store.
func (m *MemoryStore) HistoryPage(ctx context.Context, network, alias string, limit, offset int) ([]Deployment, error) {
	if err := validatePage(limit, offset); err != nil {
		return nil, err
	}
	history, err := m.History(ctx, network, alias)
	if err != nil {
		return nil, err
	}
	if offset >= len(history) {
		return []Deployment{}, nil
	}
	end := len(history)
	if limit < len(history)-offset {
		end = offset + limit
	}
	return history[offset:end], nil
}

// Close implements Store. There is nothing to release.
func (m *MemoryStore) Close() error { return nil }

// GetDeployment implements Store.
func (m *MemoryStore) GetDeployment(ctx context.Context, id int64) (Deployment, error) {
	if err := ctx.Err(); err != nil {
		return Deployment{}, err
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, d := range m.deployments {
		if d.ID == id {
			return d, nil
		}
	}
	return Deployment{}, fmt.Errorf("deployment %d: %w", id, ErrNotFound)
}
