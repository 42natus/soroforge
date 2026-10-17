package deploy

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/soroworks/soroforge/internal/stellar"
	"github.com/soroworks/soroforge/internal/store"
)

// UploadRequest asks for a contract's bytecode to be installed on-chain
// without creating or upgrading an instance.
type UploadRequest struct {
	Alias   string `json:"alias"`
	Network string `json:"network,omitempty"`
	Notes   string `json:"notes,omitempty"`
	DryRun  bool   `json:"dry_run,omitempty"`
}

// UploadResult describes a completed or simulated upload.
type UploadResult struct {
	Alias    string `json:"alias"`
	Network  string `json:"network"`
	WasmHash string `json:"wasm_hash"`
	Deployer string `json:"deployer_pubkey"`

	UploadTxHash string `json:"upload_tx_hash,omitempty"`
	Ledger       uint32 `json:"ledger,omitempty"`

	// MinResourceFee is the resource fee simulation determined for the upload,
	// in stroops. It is reported so a dry run can show what the transaction
	// would cost without submitting it.
	MinResourceFee int64 `json:"min_resource_fee,omitempty"`

	DryRun bool `json:"dry_run"`

	// UploadEnvelopeXDR is the assembled envelope. On a dry run it is unsigned
	// and is the deliverable: a governance proposal can carry it as-is.
	UploadEnvelopeXDR string `json:"upload_envelope_xdr,omitempty"`

	// UploadSkipped reports that the bytecode was already on-chain, so nothing
	// was uploaded and nothing was recorded.
	UploadSkipped bool `json:"upload_skipped,omitempty"`

	// Deployment is the recorded history entry, absent on a dry run or when
	// the upload was skipped.
	Deployment *store.Deployment `json:"deployment,omitempty"`
}

// Upload installs a contract's bytecode on-chain, addressed by its WASM hash,
// without deploying or upgrading anything.
//
// This is the first half of a governance-driven upgrade: upload the new
// bytecode, put its hash into a proposal, and let a separate party call the
// contract's upgrade function later. Uploading is content-addressed — the
// ledger entry is keyed by the hash — so this leaves no contract behind and
// changes nothing that already exists.
func (s *Service) Upload(ctx context.Context, req UploadRequest) (*UploadResult, error) {
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
	wasmHashHex := stellar.HashHex(stellar.WasmHash(wasm))

	log := s.log.With(
		"alias", r.Alias,
		"network", r.NetworkName,
		"wasm_hash", wasmHashHex,
		"dry_run", req.DryRun,
	)

	result := &UploadResult{
		Alias:    r.Alias,
		Network:  r.NetworkName,
		WasmHash: wasmHashHex,
		Deployer: signer.Address(),
		DryRun:   req.DryRun,
	}

	upload, skipped, err := s.uploadBytecode(ctx, r, signer, wasm, req.DryRun, log)
	if err != nil {
		return nil, err
	}
	if skipped {
		result.UploadSkipped = true
		// Nothing was installed, so there is nothing to record. A history entry
		// claiming an upload that did not happen would be worse than no entry.
		return result, nil
	}

	result.UploadTxHash = upload.Hash
	result.UploadEnvelopeXDR = upload.EnvelopeXDR
	result.Ledger = upload.Ledger
	result.MinResourceFee = upload.Simulation.MinResourceFee

	if req.DryRun {
		log.Info("dry run complete, nothing submitted or recorded")
		return result, nil
	}

	// The bytecode is on-chain now, so it is recorded — but only the bytecode.
	// No contract instance exists yet, so the contracts table is left alone and
	// the record carries no contract id.
	deployment, err := s.store.RecordDeployment(ctx, store.Deployment{
		Alias:          r.Alias,
		Network:        r.NetworkName,
		WasmHash:       wasmHashHex,
		Action:         store.ActionUpload,
		DeployerPubkey: signer.Address(),
		TxHash:         upload.Hash,
		Ledger:         upload.Ledger,
		Notes:          req.Notes,
		CIRunURL:       ciRunURL(),
	})
	if err != nil {
		return nil, fmt.Errorf(
			"wasm %s is uploaded on %s but recording it failed: %w",
			wasmHashHex, r.NetworkName, err)
	}
	result.Deployment = &deployment

	return result, nil
}

// uploadBytecode makes a WASM artifact available on-chain, skipping the upload
// when the exact bytecode is already there.
//
// Uploading is content-addressed, so an identical upload is a no-op that still
// costs a transaction and a fee. Deploy and Upgrade share this so all three
// paths make the same presence check, log the same lines, and produce the same
// errors.
func (s *Service) uploadBytecode(
	ctx context.Context,
	r resolved,
	signer stellar.Signer,
	wasm []byte,
	dryRun bool,
	log *slog.Logger,
) (txResult, bool, error) {
	uploaded, err := stellar.WasmUploaded(ctx, r.Client, stellar.WasmHash(wasm))
	if err != nil {
		return txResult{}, false, fmt.Errorf("check whether wasm is already uploaded: %w", err)
	}
	if uploaded {
		log.Info("wasm already on-chain, skipping upload")
		return txResult{}, true, nil
	}

	upload, err := s.runTransaction(ctx, r, stellar.UploadWasmOp(wasm, signer.Address()), dryRun)
	if err != nil {
		return txResult{}, false, fmt.Errorf("upload wasm: %w", err)
	}
	log.Info("wasm uploaded", "tx_hash", upload.Hash)
	return upload, false, nil
}
