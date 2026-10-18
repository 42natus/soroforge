package deploy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/soroworks/soroforge/internal/config"
	"github.com/soroworks/soroforge/internal/stellar"
	"github.com/soroworks/soroforge/internal/store"
	"github.com/stellar/go-stellar-sdk/txnbuild"
)

func gitMetadata(dir, override string) (string, bool) {
	commit := override
	if commit == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
		cmd.Dir = dir
		if output, err := cmd.Output(); err == nil {
			commit = strings.TrimSpace(string(output))
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "status", "--porcelain")
	cmd.Dir = dir
	output, err := cmd.Output()
	return commit, err == nil && len(strings.TrimSpace(string(output))) > 0
}

// DeployRequest asks for a contract to be deployed.
type DeployRequest struct {
	// Alias names a contract in soroforge.yaml.
	Alias string `json:"alias"`

	// Network names a network in soroforge.yaml. Empty uses default_network.
	Network string `json:"network,omitempty"`

	// Notes is free-form context recorded with the deployment.
	Notes string `json:"notes,omitempty"`

	// DryRun assembles and simulates the transactions but submits nothing and
	// records nothing.
	DryRun bool `json:"dry_run,omitempty"`

	// GitCommit overrides automatic commit detection, for detached CI checkouts.
	GitCommit string `json:"git_commit,omitempty"`
}

// DeployResult describes a completed or simulated deploy.
type DeployResult struct {
	Alias      string `json:"alias"`
	Network    string `json:"network"`
	ContractID string `json:"contract_id"`
	WasmHash   string `json:"wasm_hash"`
	Deployer   string `json:"deployer_pubkey"`

	// UploadTxHash and CreateTxHash are the two transactions a deploy performs.
	// Both are empty on a dry run.
	UploadTxHash string `json:"upload_tx_hash,omitempty"`
	CreateTxHash string `json:"create_tx_hash,omitempty"`

	// InitTxHash is the transaction hash of the initializer invocation, when
	// init_fn is configured. Empty on a dry run or when no initializer is used.
	InitTxHash string `json:"init_tx_hash,omitempty"`

	Ledger uint32 `json:"ledger,omitempty"`

	// DryRun reports whether anything was actually submitted.
	DryRun bool `json:"dry_run"`

	// UploadEnvelopeXDR and CreateEnvelopeXDR are the assembled transaction
	// envelopes. On a dry run these are the deliverable: they can be inspected,
	// diffed, or submitted by another tool.
	UploadEnvelopeXDR string `json:"upload_envelope_xdr,omitempty"`
	CreateEnvelopeXDR string `json:"create_envelope_xdr,omitempty"`

	// InitEnvelopeXDR is the assembled initializer transaction envelope.
	InitEnvelopeXDR string `json:"init_envelope_xdr,omitempty"`

	// UploadSkipped reports that the bytecode was already on-chain.
	UploadSkipped bool `json:"upload_skipped,omitempty"`

	// Deployment is the recorded history entry, absent on a dry run.
	Deployment *store.Deployment `json:"deployment,omitempty"`

	// Catalog reports registration with the network's sorovault_url. Absent
	// on a dry run or when no registry is configured.
	Catalog *CatalogStatus `json:"catalog,omitempty"`

	// Webhooks reports the outcome of delivering notifications to configured
	// webhook endpoints. Absent on a dry run or when no webhooks are configured.
	Webhooks []WebhookStatus `json:"webhooks,omitempty"`
}

// Deploy uploads a contract's bytecode and instantiates it, then records the
// result.
//
// Deploying is two transactions because Soroban permits one host function per
// transaction: first the bytecode is uploaded and becomes addressable by its
// hash, then a contract instance is created from that hash. The instance is
// what gets an address; the bytecode is shared by every contract using it.
func (s *Service) Deploy(ctx context.Context, req DeployRequest) (*DeployResult, error) {
	r, err := s.resolve(req.Network, req.Alias)
	if err != nil {
		return nil, err
	}
	signer, err := s.requireSigner()
	if err != nil {
		return nil, err
	}

	wasm, err := s.readWasm(r.Contract)
	if err != nil {
		return nil, err
	}
	wasmHash := stellar.WasmHash(wasm)
	wasmHashHex := stellar.HashHex(wasmHash)

	log := s.log.With(
		"alias", r.Alias,
		"network", r.NetworkName,
		"wasm_hash", wasmHashHex,
		"dry_run", req.DryRun,
	)
	log.Info("deploying contract", "deployer", signer.Address())

	result := &DeployResult{
		Alias:    r.Alias,
		Network:  r.NetworkName,
		WasmHash: wasmHashHex,
		Deployer: signer.Address(),
		DryRun:   req.DryRun,
	}

	// Step 1: upload the bytecode, unless it is already on-chain.
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

	// Step 2: instantiate a contract from the uploaded bytecode.
	args, err := config.ArgsToScVals(r.Contract.ConstructorArgs)
	if err != nil {
		return nil, fmt.Errorf("constructor args for %s: %w", r.Alias, err)
	}

	salt, err := s.salt(r.Contract)
	if err != nil {
		return nil, err
	}

	createOp, err := stellar.CreateContractOp(wasmHash, signer.Address(), salt, args, signer.Address())
	if err != nil {
		return nil, err
	}

	create, err := s.runTransaction(ctx, r, createOp, req.DryRun)
	if err != nil {
		return nil, fmt.Errorf("create contract: %w", err)
	}
	result.CreateTxHash = create.Hash
	result.CreateEnvelopeXDR = create.EnvelopeXDR
	result.Ledger = create.Ledger

	// The contract's address comes from the host function's return value, which
	// simulation reports — so a dry run knows the address a real deploy would
	// produce.
	contractID, err := stellar.ContractIDFromSimulation(create.Simulation)
	if err != nil {
		return nil, fmt.Errorf("determine contract id: %w", err)
	}
	result.ContractID = contractID

	// Step 3: if an initializer function is configured, prepare its invocation.
	var initOp *txnbuild.InvokeHostFunction
	if r.Contract.InitFunc != "" {
		initArgs, err := config.ArgsToScVals(r.Contract.InitArgs)
		if err != nil {
			return nil, fmt.Errorf("init args for %s: %w", r.Alias, err)
		}
		initOp, err = stellar.InvokeOp(contractID, r.Contract.InitFunc, initArgs, signer.Address())
		if err != nil {
			return nil, fmt.Errorf("build init op: %w", err)
		}
	}

	if req.DryRun {
		if initOp != nil {
			initTx, err := s.runTransaction(ctx, r, initOp, true)
			if err != nil {
				return nil, fmt.Errorf("simulate initializer %s: %w", r.Contract.InitFunc, err)
			}
			result.InitEnvelopeXDR = initTx.EnvelopeXDR
		}
		log.Info("dry run complete, nothing submitted or recorded", "contract_id", contractID)
		return result, nil
	}

	log.Info("contract created", "contract_id", contractID, "tx_hash", create.Hash)

	gitCommit, dirty := gitMetadata(s.cfg.Dir(), req.GitCommit)

	if initOp != nil {
		initTx, err := s.runTransaction(ctx, r, initOp, false)
		if err != nil {
			// The contract is already live on-chain, so record it honestly even though initialization failed.
			failNotes := req.Notes
			if failNotes != "" {
				failNotes += " (uninitialised)"
			} else {
				failNotes = "uninitialised"
			}
			_, _ = s.record(ctx, store.Deployment{
				Alias:          r.Alias,
				Network:        r.NetworkName,
				ContractID:     contractID,
				WasmHash:       wasmHashHex,
				Action:         store.ActionDeploy,
				DeployerPubkey: signer.Address(),
				TxHash:         create.Hash,
				Ledger:         create.Ledger,
				Notes:          failNotes,
				GitCommit:      gitCommit,
				Dirty:          dirty,
				FeeCharged:     create.FeeCharged,
				CIRunURL:       ciRunURL(),
			})
			return result, fmt.Errorf("contract %s is deployed at %s but uninitialised: %w", r.Alias, contractID, err)
		}
		result.InitTxHash = initTx.Hash
		log.Info("contract initialized", "contract_id", contractID, "init_fn", r.Contract.InitFunc, "tx_hash", initTx.Hash)
	}

	// Record only now, after the network has confirmed transactions.
	deployment, err := s.record(ctx, store.Deployment{
		Alias:          r.Alias,
		Network:        r.NetworkName,
		ContractID:     contractID,
		WasmHash:       wasmHashHex,
		Action:         store.ActionDeploy,
		DeployerPubkey: signer.Address(),
		TxHash:         create.Hash,
		Ledger:         create.Ledger,
		Notes:          req.Notes,
		GitCommit:      gitCommit,
		Dirty:          dirty,
		FeeCharged:     create.FeeCharged,
		CIRunURL:       ciRunURL(),
	})
	if err != nil {
		return nil, err
	}
	result.Deployment = deployment
	result.Catalog = s.publish(ctx, r, contractID, log)
	result.Webhooks = s.notifyWebhooks(ctx, r.Network, WebhookPayload{
		Alias:          r.Alias,
		Network:        r.NetworkName,
		ContractID:     contractID,
		Contract:       contractID,
		WasmHash:       wasmHashHex,
		Action:         store.ActionDeploy,
		DeployerPubkey: signer.Address(),
		TxHash:         create.Hash,
		Tx:             create.Hash,
		Ledger:         create.Ledger,
		Notes:          req.Notes,
	}, log)

	return result, nil
}

// record writes a history entry and updates the contract's current state.
//
// If this fails the contract is already live on-chain, so the error says so
// explicitly: the deploy succeeded and only the bookkeeping did not, which is a
// recoverable situation but not one to discover from a bare database error.
func (s *Service) record(ctx context.Context, d store.Deployment) (*store.Deployment, error) {
	recorded, err := s.store.RecordDeployment(ctx, d)
	if err != nil {
		return nil, fmt.Errorf(
			"contract %s is live on %s (%s) but recording it failed: %w",
			d.Alias, d.Network, d.ContractID, err)
	}

	if _, err := s.store.UpsertContract(ctx, store.Contract{
		Alias:           d.Alias,
		Network:         d.Network,
		ContractID:      d.ContractID,
		CurrentWasmHash: d.WasmHash,
	}); err != nil {
		return nil, fmt.Errorf(
			"contract %s is live on %s (%s) and the history entry was written, "+
				"but updating its current state failed: %w",
			d.Alias, d.Network, d.ContractID, err)
	}

	return &recorded, nil
}

// readWasm loads a contract's compiled artifact.
func (s *Service) readWasm(c config.Contract) ([]byte, error) {
	path := s.cfg.WasmPath(c)
	wasm, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("wasm artifact not found at %s (has the contract been built?)", path)
		}
		return nil, fmt.Errorf("read wasm %s: %w", path, err)
	}
	if len(wasm) == 0 {
		return nil, fmt.Errorf("wasm artifact at %s is empty", path)
	}
	return wasm, nil
}

// salt returns the contract's configured salt, or a fresh random one.
func (s *Service) salt(c config.Contract) ([32]byte, error) {
	if c.Salt != "" {
		return config.ParseSalt(c.Salt)
	}
	return stellar.NewSalt()
}
