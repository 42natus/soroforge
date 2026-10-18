package deploy

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/soroworks/soroforge/internal/stellar"
	"github.com/soroworks/soroforge/internal/store"
)

// State is the outcome of comparing SoroForge's records against the ledger.
type State string

const (
	// StateInSync means the on-chain WASM hash matches what SoroForge recorded.
	StateInSync State = "in_sync"

	// StateDrift means the contract is running bytecode SoroForge did not
	// deploy. Someone upgraded it another way, or a deploy was recorded that
	// did not take effect.
	StateDrift State = "drift"

	// StateUntracked means the contract is not in SoroForge's records for this
	// network.
	StateUntracked State = "untracked"

	// StateMissing means SoroForge has a record but the ledger has no such
	// contract — it was never deployed to this network, or its instance entry
	// has expired.
	StateMissing State = "missing"
)

// StatusRequest asks for a drift check.
type StatusRequest struct {
	Alias   string `json:"alias"`
	Network string `json:"network,omitempty"`
}

// StatusResult reports what SoroForge believes against what the ledger says.
type StatusResult struct {
	Alias      string `json:"alias"`
	Network    string `json:"network"`
	ContractID string `json:"contract_id,omitempty"`

	// State summarises the comparison; the two hashes below are the evidence.
	State State `json:"state"`

	// ExpectedWasmHash is what SoroForge recorded as live.
	ExpectedWasmHash string `json:"expected_wasm_hash,omitempty"`

	// ActualWasmHash is what the ledger reports the contract is running.
	ActualWasmHash string `json:"actual_wasm_hash,omitempty"`

	// Detail explains the state in a sentence, for CLI output.
	Detail string `json:"detail"`
}

// InSync reports whether the contract matches its record. Callers use it to
// choose an exit code.
func (r StatusResult) InSync() bool { return r.State == StateInSync }

// Status compares the WASM hash SoroForge recorded against the one the contract
// is actually running.
//
// This is the check that makes the recorded history trustworthy. A database can
// drift from reality — someone deploys by hand, a migration is restored from a
// backup, an upgrade is applied with another tool — and a deployment tracker
// that never verifies itself will confidently report stale information. The
// ledger is the authority here; SoroForge's record is the claim being tested.
func (s *Service) Status(ctx context.Context, req StatusRequest) (*StatusResult, error) {
	r, err := s.resolve(req.Network, req.Alias)
	if err != nil {
		return nil, err
	}

	result := &StatusResult{
		Alias:   r.Alias,
		Network: r.NetworkName,
	}

	tracked, err := s.store.GetContract(ctx, r.NetworkName, r.Alias)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			result.State = StateUntracked
			result.Detail = fmt.Sprintf(
				"%s is not tracked on %s; SoroForge has no record of deploying it",
				r.Alias, r.NetworkName)
			return result, nil
		}
		return nil, err
	}

	return checkTracked(ctx, r.Client, tracked)
}

// NetworkStatus is a drift check over every tracked contract on a network.
type NetworkStatus struct {
	Network string `json:"network"`
	// InSync is true only when every contract is in sync.
	InSync    bool           `json:"in_sync"`
	Contracts []StatusResult `json:"contracts"`
}

// StatusAll drift-checks every contract SoroForge tracks on a network, in
// alias order, so one CI step can gate on all of them.
//
// It works from the records rather than from soroforge.yaml, for two reasons:
// a contract whose alias was since removed from the config is still live and
// still worth checking, and a contract declared in the config but never
// deployed to this network is not drift — it is simply not there yet.
func (s *Service) StatusAll(ctx context.Context, networkName string) (*NetworkStatus, error) {
	name, network, err := s.cfg.Network(networkName)
	if err != nil {
		return nil, err
	}

	tracked, err := s.store.ListContracts(ctx, name)
	if err != nil {
		return nil, err
	}

	result := &NetworkStatus{Network: name, InSync: true, Contracts: []StatusResult{}}
	if len(tracked) == 0 {
		return result, nil
	}

	client, err := s.clients(name, network)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", name, err)
	}

	sort.Slice(tracked, func(i, j int) bool { return tracked[i].Alias < tracked[j].Alias })
	for _, c := range tracked {
		status, err := checkTracked(ctx, client, c)
		if err != nil {
			return nil, fmt.Errorf("checking %s: %w", c.Alias, err)
		}
		result.Contracts = append(result.Contracts, *status)
		if !status.InSync() {
			result.InSync = false
		}
	}
	return result, nil
}

// checkTracked compares one tracked contract against the ledger.
func checkTracked(ctx context.Context, client stellar.Client, tracked store.Contract) (*StatusResult, error) {
	result := &StatusResult{
		Alias:            tracked.Alias,
		Network:          tracked.Network,
		ContractID:       tracked.ContractID,
		ExpectedWasmHash: tracked.CurrentWasmHash,
	}

	liveHash, err := stellar.OnChainWasmHash(ctx, client, tracked.ContractID)
	if err != nil {
		var notFound *stellar.ErrContractNotFound
		if errors.As(err, &notFound) {
			result.State = StateMissing
			result.Detail = fmt.Sprintf(
				"SoroForge records %s as deployed at %s on %s, but that contract has no instance "+
					"on-chain (never deployed to this network, or its instance entry expired)",
				tracked.Alias, tracked.ContractID, tracked.Network)
			return result, nil
		}
		return nil, err
	}

	result.ActualWasmHash = stellar.HashHex(liveHash)

	if result.ActualWasmHash == result.ExpectedWasmHash {
		result.State = StateInSync
		result.Detail = fmt.Sprintf("%s on %s matches its recorded wasm hash", tracked.Alias, tracked.Network)
		return result, nil
	}

	result.State = StateDrift
	result.Detail = fmt.Sprintf(
		"drift: %s on %s is running %s but SoroForge recorded %s; "+
			"the contract was changed outside SoroForge",
		tracked.Alias, tracked.Network, result.ActualWasmHash, result.ExpectedWasmHash)
	return result, nil
}

// List returns tracked contracts. An empty network name returns every network's
// contracts rather than defaulting to one, since `soroforge list` with no flags
// should show everything.
func (s *Service) List(ctx context.Context, network string) ([]store.Contract, error) {
	if network != "" {
		name, _, err := s.cfg.Network(network)
		if err != nil {
			return nil, err
		}
		network = name
	}
	return s.store.ListContracts(ctx, network)
}

// ListPage returns one page of tracked contracts without changing CLI behavior.
func (s *Service) ListPage(ctx context.Context, network string, limit, offset int) ([]store.Contract, error) {
	if network != "" {
		name, _, err := s.cfg.Network(network)
		if err != nil {
			return nil, err
		}
		network = name
	}
	return s.store.ListContractsPage(ctx, network, limit, offset)
}

// History returns a contract's deploy and upgrade log, newest first.
func (s *Service) History(ctx context.Context, network, alias string) ([]store.Deployment, error) {
	name, _, err := s.cfg.Network(network)
	if err != nil {
		return nil, err
	}
	return s.store.History(ctx, name, alias)
}

// GetDeployment returns one deployment record by its ID.
func (s *Service) GetDeployment(ctx context.Context, id int64) (store.Deployment, error) {
	return s.store.GetDeployment(ctx, id)
}

// HistoryPage returns one newest-first page of a contract's deployment log.
func (s *Service) HistoryPage(ctx context.Context, network, alias string, limit, offset int) ([]store.Deployment, error) {
	name, _, err := s.cfg.Network(network)
	if err != nil {
		return nil, err
	}
	return s.store.HistoryPage(ctx, name, alias, limit, offset)
}
