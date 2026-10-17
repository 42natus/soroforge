package deploy

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/soroworks/soroforge/internal/config"
	"github.com/soroworks/soroforge/internal/stellar"
	"github.com/soroworks/soroforge/internal/store"
)

// InvokeRequest asks for a function on a tracked contract to be called.
type InvokeRequest struct {
	Alias     string   `json:"alias"`
	Network   string   `json:"network,omitempty"`
	Function  string   `json:"function"`
	Args      []string `json:"args,omitempty"`
	Notes     string   `json:"notes,omitempty"`
	DryRun    bool     `json:"dry_run,omitempty"`
	GitCommit string   `json:"git_commit,omitempty"`
}

// InvokeResult describes a completed or simulated invocation.
type InvokeResult struct {
	Alias       string            `json:"alias"`
	Network     string            `json:"network"`
	ContractID  string            `json:"contract_id"`
	Function    string            `json:"function"`
	Args        []string          `json:"args"`
	Deployer    string            `json:"deployer_pubkey"`
	TxHash      string            `json:"tx_hash,omitempty"`
	Ledger      uint32            `json:"ledger,omitempty"`
	DryRun      bool              `json:"dry_run"`
	EnvelopeXDR string            `json:"envelope_xdr,omitempty"`
	Deployment  *store.Deployment `json:"deployment,omitempty"`
}

// Invoke executes an administrative or general function call on a deployed,
// tracked contract using the deployer key.
func (s *Service) Invoke(ctx context.Context, req InvokeRequest) (*InvokeResult, error) {
	if strings.TrimSpace(req.Function) == "" {
		return nil, fmt.Errorf("invoke: function name is required")
	}

	r, err := s.resolve(req.Network, req.Alias)
	if err != nil {
		return nil, err
	}
	signer, err := s.requireSigner()
	if err != nil {
		return nil, err
	}

	tracked, err := s.store.GetContract(ctx, r.NetworkName, r.Alias)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf(
				"contract %s is not tracked on %s; deploy it first with `soroforge deploy %s`",
				r.Alias, r.NetworkName, r.Alias)
		}
		return nil, err
	}

	parsedArgs, err := config.ParseCLIArgs(req.Args)
	if err != nil {
		return nil, fmt.Errorf("parse arguments for %s: %w", r.Alias, err)
	}

	scVals, err := config.ArgsToScVals(parsedArgs)
	if err != nil {
		return nil, fmt.Errorf("convert arguments for %s: %w", r.Alias, err)
	}

	log := s.log.With(
		"alias", r.Alias,
		"network", r.NetworkName,
		"contract_id", tracked.ContractID,
		"function", req.Function,
		"dry_run", req.DryRun,
	)

	result := &InvokeResult{
		Alias:      r.Alias,
		Network:    r.NetworkName,
		ContractID: tracked.ContractID,
		Function:   req.Function,
		Args:       req.Args,
		Deployer:   signer.Address(),
		DryRun:     req.DryRun,
	}

	invokeOp, err := stellar.InvokeOp(tracked.ContractID, req.Function, scVals, signer.Address())
	if err != nil {
		return nil, fmt.Errorf("build invoke op: %w", err)
	}

	txRes, err := s.runTransaction(ctx, r, invokeOp, req.DryRun)
	if err != nil {
		return nil, fmt.Errorf("invoke %s on %s: %w", req.Function, tracked.ContractID, err)
	}

	result.TxHash = txRes.Hash
	result.Ledger = txRes.Ledger
	result.EnvelopeXDR = txRes.EnvelopeXDR

	if req.DryRun {
		log.Info("dry run complete, nothing submitted or recorded")
		return result, nil
	}

	log.Info("function invoked",
		"contract_id", tracked.ContractID,
		"function", req.Function,
		"tx_hash", txRes.Hash)

	gitCommit, dirty := gitMetadata(s.cfg.Dir(), req.GitCommit)

	deployment, err := s.store.RecordDeployment(ctx, store.Deployment{
		Alias:          r.Alias,
		Network:        r.NetworkName,
		ContractID:     tracked.ContractID,
		WasmHash:       tracked.CurrentWasmHash,
		Action:         store.ActionInvoke,
		DeployerPubkey: signer.Address(),
		TxHash:         txRes.Hash,
		Ledger:         txRes.Ledger,
		Notes:          req.Notes,
		GitCommit:      gitCommit,
		Dirty:          dirty,
		FeeCharged:     txRes.FeeCharged,
		CIRunURL:       ciRunURL(),
		FnName:         req.Function,
		FnArgs:         strings.Join(req.Args, " "),
	})
	if err != nil {
		return nil, fmt.Errorf(
			"invoked %s on %s (%s) but recording it failed: %w",
			req.Function, r.Alias, tracked.ContractID, err)
	}
	result.Deployment = &deployment

	return result, nil
}
