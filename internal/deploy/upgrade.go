package deploy

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/soroworks/soroforge/internal/stellar"
	"github.com/soroworks/soroforge/internal/store"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// UpgradeRequest asks for a tracked contract's bytecode to be replaced.
type UpgradeRequest struct {
	Alias     string `json:"alias"`
	Network   string `json:"network,omitempty"`
	Notes     string `json:"notes,omitempty"`
	DryRun    bool   `json:"dry_run,omitempty"`
	GitCommit string `json:"git_commit,omitempty"`
	WasmHash  string `json:"wasm_hash,omitempty"`
}

// UpgradeResult describes a completed or simulated upgrade.
type UpgradeResult struct {
	Alias      string `json:"alias"`
	Network    string `json:"network"`
	ContractID string `json:"contract_id"`

	// PreviousWasmHash and WasmHash are the version transition this upgrade
	// performs. Recording both is what makes the history answer "what changed".
	PreviousWasmHash string `json:"previous_wasm_hash"`
	WasmHash         string `json:"wasm_hash"`

	Deployer string `json:"deployer_pubkey"`

	// UpgradeFunc is the contract entrypoint that was invoked.
	UpgradeFunc string `json:"upgrade_fn"`

	UploadTxHash  string `json:"upload_tx_hash,omitempty"`
	UpgradeTxHash string `json:"upgrade_tx_hash,omitempty"`
	Ledger        uint32 `json:"ledger,omitempty"`

	DryRun bool `json:"dry_run"`

	UploadEnvelopeXDR  string `json:"upload_envelope_xdr,omitempty"`
	UpgradeEnvelopeXDR string `json:"upgrade_envelope_xdr,omitempty"`

	UploadSkipped bool `json:"upload_skipped,omitempty"`

	// NoChange reports that the new bytecode is identical to what is already
	// live, so no upgrade was performed.
	NoChange bool `json:"no_change,omitempty"`

	Deployment *store.Deployment `json:"deployment,omitempty"`

	// Catalog reports registration of the new interface version with the
	// network's sorovault_url. Absent on a dry run, on a no-change upgrade,
	// or when no registry is configured.
	Catalog *CatalogStatus `json:"catalog,omitempty"`

	// Webhooks reports the outcome of delivering notifications to configured
	// webhook endpoints. Absent on a dry run, on a no-change upgrade, or when
	// no webhooks are configured.
	Webhooks []WebhookStatus `json:"webhooks,omitempty"`
}

func (s *Service) resolveUpgradeHash(ctx context.Context, r resolved, input string) (xdr.Hash, string, error) {
	input = strings.TrimSpace(strings.ToLower(input))
	if len(input) == 64 {
		h, err := stellar.ParseHashHex(input)
		if err != nil {
			return h, "", err
		}
		return h, input, nil
	}

	history, err := s.store.History(ctx, r.NetworkName, r.Alias)
	if err != nil {
		return xdr.Hash{}, "", fmt.Errorf("lookup history for %s/%s: %w", r.NetworkName, r.Alias, err)
	}

	seen := make(map[string]struct{})
	var matches []string
	for _, d := range history {
		h := strings.ToLower(d.WasmHash)
		if strings.HasPrefix(h, input) {
			if _, ok := seen[h]; !ok {
				seen[h] = struct{}{}
				matches = append(matches, h)
			}
		}
	}

	if len(matches) == 0 {
		return xdr.Hash{}, "", fmt.Errorf("no wasm hash starting with %q found in history for %s on %s", input, r.Alias, r.NetworkName)
	}
	if len(matches) > 1 {
		return xdr.Hash{}, "", fmt.Errorf("ambiguous wasm hash prefix %q for %s on %s (matches: %s)", input, r.Alias, r.NetworkName, strings.Join(matches, ", "))
	}

	target := matches[0]
	h, err := stellar.ParseHashHex(target)
	if err != nil {
		return h, "", err
	}
	return h, target, nil
}

// Upgrade replaces a tracked contract's bytecode, keeping its address.
//
// Soroban has no protocol-level upgrade operation. Upgrading is something a
// contract does to itself: it exposes an entrypoint that calls
// env.deployer().update_current_contract_wasm(new_hash), and that entrypoint
// decides who is allowed to call it. SoroForge therefore uploads the new
// bytecode and then invokes that function — which means a contract that does
// not expose one cannot be upgraded, and this will report the failure the
// contract returns.
//
// The entrypoint name defaults to "upgrade" and is configurable per contract
// via `upgrade_fn`.
func (s *Service) Upgrade(ctx context.Context, req UpgradeRequest) (*UpgradeResult, error) {
	r, err := s.resolve(req.Network, req.Alias)
	if err != nil {
		return nil, err
	}
	signer, err := s.requireSigner()
	if err != nil {
		return nil, err
	}

	// Upgrading requires knowing which contract to upgrade, and that is exactly
	// what the tracked history is for. An untracked contract is a deploy, not
	// an upgrade.
	tracked, err := s.store.GetContract(ctx, r.NetworkName, r.Alias)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf(
				"contract %s is not tracked on %s; deploy it first with `soroforge deploy %s`",
				r.Alias, r.NetworkName, r.Alias)
		}
		return nil, err
	}

	var (
		wasmHash    xdr.Hash
		wasmHashHex string
		wasm        []byte
	)

	if req.WasmHash != "" {
		targetHash, targetHex, err := s.resolveUpgradeHash(ctx, r, req.WasmHash)
		if err != nil {
			return nil, err
		}
		wasmHash = targetHash
		wasmHashHex = targetHex

		uploaded, err := stellar.WasmUploaded(ctx, r.Client, wasmHash)
		if err != nil {
			return nil, fmt.Errorf("check whether wasm hash is on-chain: %w", err)
		}
		if !uploaded {
			return nil, fmt.Errorf("wasm hash %s does not exist on-chain on %s", wasmHashHex, r.NetworkName)
		}
	} else {
		wasm, err = s.readWasm(r.Contract)
		if err != nil {
			return nil, err
		}
		wasmHash = stellar.WasmHash(wasm)
		wasmHashHex = stellar.HashHex(wasmHash)
	}

	upgradeFunc := r.Contract.UpgradeFuncOrDefault()

	log := s.log.With(
		"alias", r.Alias,
		"network", r.NetworkName,
		"contract_id", tracked.ContractID,
		"wasm_hash", wasmHashHex,
		"dry_run", req.DryRun,
	)

	result := &UpgradeResult{
		Alias:            r.Alias,
		Network:          r.NetworkName,
		ContractID:       tracked.ContractID,
		PreviousWasmHash: tracked.CurrentWasmHash,
		WasmHash:         wasmHashHex,
		Deployer:         signer.Address(),
		UpgradeFunc:      upgradeFunc,
		DryRun:           req.DryRun,
	}

	// Compare against the ledger rather than the database. If someone upgraded
	// this contract outside SoroForge, the database is stale and the honest
	// answer to "is this a no-op?" comes from the chain.
	liveHash, err := stellar.OnChainWasmHash(ctx, r.Client, tracked.ContractID)
	if err != nil {
		return nil, fmt.Errorf("read current on-chain wasm hash: %w", err)
	}
	result.PreviousWasmHash = stellar.HashHex(liveHash)

	if liveHash == wasmHash {
		log.Info("contract already runs this wasm hash, nothing to upgrade")
		result.NoChange = true
		return result, nil
	}

	// Step 1: make the new bytecode available on-chain.
	if req.WasmHash != "" {
		result.UploadSkipped = true
	} else {
		upload, skipped, err := s.uploadBytecode(ctx, r, signer, wasm, req.DryRun, log)
		if err != nil {
			return nil, err
		}
		if skipped {
			result.UploadSkipped = true
		} else {
			result.UploadTxHash = upload.Hash
			result.UploadEnvelopeXDR = upload.EnvelopeXDR
		}
	}

	// Step 2: ask the contract to upgrade itself.
	//
	// On a dry run where the upload was skipped above, this simulation runs
	// against bytecode that is genuinely on-chain and is therefore accurate.
	// Where the upload was itself dry-run, the new bytecode does not exist yet
	// and the contract's own validation may reject the hash — the assembled
	// envelope is still produced, but a simulation error here is expected and
	// is reported as such.
	invokeOp, err := stellar.InvokeOp(
		tracked.ContractID,
		upgradeFunc,
		[]xdr.ScVal{stellar.UpgradeArg(wasmHash)},
		signer.Address(),
	)
	if err != nil {
		return nil, err
	}

	upgrade, err := s.runTransaction(ctx, r, invokeOp, req.DryRun)
	if err != nil {
		return nil, fmt.Errorf("invoke %s on %s: %w", upgradeFunc, tracked.ContractID, err)
	}
	result.UpgradeTxHash = upgrade.Hash
	result.UpgradeEnvelopeXDR = upgrade.EnvelopeXDR
	result.Ledger = upgrade.Ledger

	if req.DryRun {
		log.Info("dry run complete, nothing submitted or recorded")
		return result, nil
	}

	log.Info("contract upgraded",
		"from_wasm_hash", result.PreviousWasmHash,
		"to_wasm_hash", wasmHashHex,
		"tx_hash", upgrade.Hash)

	gitCommit, dirty := gitMetadata(s.cfg.Dir(), req.GitCommit)

	deployment, err := s.record(ctx, store.Deployment{
		Alias:          r.Alias,
		Network:        r.NetworkName,
		ContractID:     tracked.ContractID,
		WasmHash:       wasmHashHex,
		Action:         store.ActionUpgrade,
		DeployerPubkey: signer.Address(),
		TxHash:         upgrade.Hash,
		Ledger:         upgrade.Ledger,
		Notes:          req.Notes,
		GitCommit:      gitCommit,
		Dirty:          dirty,
		FeeCharged:     upgrade.FeeCharged,
		CIRunURL:       ciRunURL(),
	})
	if err != nil {
		return nil, err
	}
	result.Deployment = deployment
	result.Catalog = s.publish(ctx, r, tracked.ContractID, log)
	result.Webhooks = s.notifyWebhooks(ctx, r.Network, WebhookPayload{
		Alias:          r.Alias,
		Network:        r.NetworkName,
		ContractID:     tracked.ContractID,
		Contract:       tracked.ContractID,
		WasmHash:       wasmHashHex,
		Action:         store.ActionUpgrade,
		DeployerPubkey: signer.Address(),
		TxHash:         upgrade.Hash,
		Tx:             upgrade.Hash,
		Ledger:         upgrade.Ledger,
		Notes:          req.Notes,
	}, log)

	return result, nil
}
