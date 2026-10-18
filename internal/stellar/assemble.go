package stellar

import (
	"fmt"

	"github.com/stellar/go-stellar-sdk/txnbuild"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// DefaultTimeout is how long an assembled transaction stays valid. Soroban
// transactions are simulated against current ledger state, so a long window
// mostly widens the gap in which that state can change underneath them.
const DefaultTimeout = 300

// BuildParams are the inputs for building an unsigned Soroban transaction.
type BuildParams struct {
	// Source is the account paying the fee and supplying the sequence number.
	Source txnbuild.Account

	// Operation is the host function invocation to run.
	Operation *txnbuild.InvokeHostFunction

	// BaseFee is the per-operation fee in stroops. The resource fee determined
	// by simulation is added on top of it during assembly.
	BaseFee int64

	// TimeoutSeconds overrides DefaultTimeout when non-zero.
	TimeoutSeconds int64
}

// Build produces the unsigned transaction to simulate.
//
// The result carries no Soroban resource data yet: the footprint and fees are
// what simulation exists to discover. Feed the result through Simulate and then
// Assemble before signing.
func Build(p BuildParams) (*txnbuild.Transaction, error) {
	if p.Source == nil {
		return nil, fmt.Errorf("build transaction: no source account")
	}
	if p.Operation == nil {
		return nil, fmt.Errorf("build transaction: no operation")
	}

	baseFee := p.BaseFee
	if baseFee < txnbuild.MinBaseFee {
		baseFee = txnbuild.MinBaseFee
	}
	timeout := p.TimeoutSeconds
	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	tx, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
		SourceAccount:        p.Source,
		IncrementSequenceNum: true,
		Operations:           []txnbuild.Operation{p.Operation},
		BaseFee:              baseFee,
		Preconditions:        txnbuild.Preconditions{TimeBounds: txnbuild.NewTimeout(timeout)},
	})
	if err != nil {
		return nil, fmt.Errorf("build transaction: %w", err)
	}
	return tx, nil
}

// Assemble folds a simulation result back into a transaction, producing the
// envelope that is actually submitted.
//
// The Go SDK has no equivalent of the JavaScript SDK's assembleTransaction, so
// this is done by hand. Three things come back from simulation and have to be
// applied:
//
//   - SorobanTransactionData — the footprint (which ledger entries the
//     transaction reads and writes), resource limits, and the resource fee.
//   - Authorization entries — recorded during simulation and required at
//     submission for anything that needs authorization.
//   - The resource fee itself, which must be added to the transaction fee.
//
// The last one is handled for us: txnbuild.NewTransaction computes
// `BaseFee × numOperations + SorobanData.ResourceFee`, so setting the operation's
// Ext and rebuilding is enough. That is also why this function rebuilds rather
// than mutating — the fee is fixed at construction time.
func Assemble(tx *txnbuild.Transaction, sim SimulateResult) (*txnbuild.Transaction, error) {
	if tx == nil {
		return nil, fmt.Errorf("assemble: nil transaction")
	}
	if sim.Error != "" {
		return nil, fmt.Errorf("simulation failed: %s", sim.Error)
	}
	if sim.RestoreRequired {
		// Submitting anyway would burn a fee on a transaction that cannot
		// succeed, so this stops with an explanation instead.
		return nil, fmt.Errorf(
			"simulation reports archived ledger entries that must be restored first; " +
				"run a RestoreFootprint transaction (e.g. `stellar contract restore`) and retry")
	}
	if sim.TransactionDataXDR == "" {
		return nil, fmt.Errorf("simulation returned no transaction data; cannot determine the footprint")
	}

	var sorobanData xdr.SorobanTransactionData
	if err := xdr.SafeUnmarshalBase64(sim.TransactionDataXDR, &sorobanData); err != nil {
		return nil, fmt.Errorf("decode simulated transaction data: %w", err)
	}

	ops := tx.Operations()
	if len(ops) != 1 {
		return nil, fmt.Errorf("assemble: expected exactly 1 operation, got %d", len(ops))
	}
	invoke, ok := ops[0].(*txnbuild.InvokeHostFunction)
	if !ok {
		return nil, fmt.Errorf("assemble: expected an InvokeHostFunction operation, got %T", ops[0])
	}

	auth, err := decodeAuth(sim)
	if err != nil {
		return nil, err
	}

	assembled := &txnbuild.InvokeHostFunction{
		HostFunction:  invoke.HostFunction,
		Auth:          auth,
		SourceAccount: invoke.SourceAccount,
		Ext: xdr.TransactionExt{
			V:           1,
			SorobanData: &sorobanData,
		},
	}

	source := tx.SourceAccount()
	// tx already consumed a sequence number when it was built; reuse that exact
	// number rather than incrementing again, so the assembled transaction is
	// the same transaction rather than the next one.
	out, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
		SourceAccount:        &txnbuild.SimpleAccount{AccountID: source.AccountID, Sequence: source.Sequence},
		IncrementSequenceNum: false,
		Operations:           []txnbuild.Operation{assembled},
		BaseFee:              tx.BaseFee(),
		Preconditions:        txnbuild.Preconditions{TimeBounds: tx.Timebounds()},
		Memo:                 tx.Memo(),
	})
	if err != nil {
		return nil, fmt.Errorf("assemble transaction: %w", err)
	}
	return out, nil
}

// decodeAuth decodes the authorization entries recorded during simulation.
//
// Simulation reports one result per host function, and Soroban allows only one
// host function per transaction, so more than one result means the response did
// not describe the transaction we sent.
func decodeAuth(sim SimulateResult) ([]xdr.SorobanAuthorizationEntry, error) {
	if len(sim.Results) == 0 {
		return nil, nil
	}
	if len(sim.Results) > 1 {
		return nil, fmt.Errorf(
			"simulation returned %d host function results but a transaction may contain only 1",
			len(sim.Results))
	}

	encoded := sim.Results[0].AuthXDR
	if len(encoded) == 0 {
		return nil, nil
	}

	auth := make([]xdr.SorobanAuthorizationEntry, 0, len(encoded))
	for i, e := range encoded {
		var entry xdr.SorobanAuthorizationEntry
		if err := xdr.SafeUnmarshalBase64(e, &entry); err != nil {
			return nil, fmt.Errorf("decode simulated auth entry %d: %w", i, err)
		}
		auth = append(auth, entry)
	}
	return auth, nil
}

// ResourceFee reports the resource fee a simulation result implies, read from
// the authoritative SorobanTransactionData rather than the advisory
// MinResourceFee field. Used for dry-run output.
func ResourceFee(sim SimulateResult) (int64, error) {
	if sim.TransactionDataXDR == "" {
		return 0, fmt.Errorf("simulation returned no transaction data")
	}
	var data xdr.SorobanTransactionData
	if err := xdr.SafeUnmarshalBase64(sim.TransactionDataXDR, &data); err != nil {
		return 0, fmt.Errorf("decode simulated transaction data: %w", err)
	}
	return int64(data.ResourceFee), nil
}

// StroopsPerXLM is the number of stroops in one lumen: 10^7.
const StroopsPerXLM int64 = 10_000_000

// StroopsToXLM renders a stroop amount in XLM with all seven decimal places, so
// that fees smaller than a tenth of a lumen do not print as zero.
func StroopsToXLM(stroops int64) string {
	sign := ""
	if stroops < 0 {
		sign = "-"
		stroops = -stroops
	}
	return fmt.Sprintf("%s%d.%07d", sign, stroops/StroopsPerXLM, stroops%StroopsPerXLM)
}
