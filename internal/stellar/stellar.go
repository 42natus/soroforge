// Package stellar wraps the Stellar RPC and transaction-assembly work SoroForge
// needs, behind interfaces that can be mocked.
//
// The boundary is deliberately narrow and string-shaped: Client speaks
// base64-encoded XDR envelopes rather than SDK structs, so a fake needs no XDR
// plumbing to stand in for a live network. Everything that interprets XDR —
// building operations, assembling simulated transactions, deriving contract IDs
// — is a pure function in this package and is tested without any network at all.
//
// Verified against github.com/stellar/go-stellar-sdk v0.7.1. The concrete
// implementation lives in rpc.go and is the only file that should need to
// change when the SDK moves.
package stellar

import (
	"context"

	"github.com/stellar/go-stellar-sdk/txnbuild"
)

// Client is the subset of Stellar RPC that SoroForge uses.
//
// Method names follow the RPC methods they wrap (simulateTransaction,
// sendTransaction, getTransaction, getLedgerEntries) so that behaviour can be
// traced back to the published API.
type Client interface {
	// NetworkPassphrase returns the passphrase this client signs for. It comes
	// from configuration rather than the network, so that a misconfigured
	// passphrase fails locally instead of after a signed submission.
	NetworkPassphrase() string

	// LoadAccount fetches an account's current sequence number.
	LoadAccount(ctx context.Context, address string) (txnbuild.Account, error)

	// Simulate runs simulateTransaction against an unsigned envelope.
	Simulate(ctx context.Context, txXDR string) (SimulateResult, error)

	// Send submits a signed envelope via sendTransaction. It returns as soon
	// as the RPC accepts the transaction; use AwaitTransaction for the outcome.
	Send(ctx context.Context, signedTxXDR string) (SendResult, error)

	// AwaitTransaction polls getTransaction until the transaction reaches a
	// terminal state or ctx is done.
	AwaitTransaction(ctx context.Context, hash string) (TxResult, error)

	// LedgerEntries reads ledger entries by base64 LedgerKey.
	LedgerEntries(ctx context.Context, keysB64 []string) ([]LedgerEntry, error)

	// LatestLedger returns the most recently closed ledger sequence. It doubles
	// as a connectivity check for `soroforge status`.
	LatestLedger(ctx context.Context) (uint32, error)
}

// NetworkReporter is implemented by clients whose endpoint can report which
// Stellar network it actually serves. `soroforge networks` uses it to verify
// that a configured network's passphrase matches what the endpoint answers,
// so a misspelled rpc_url cannot silently point a deploy at the wrong chain.
//
// It is a separate interface rather than a method on Client because the
// check is optional: an endpoint that does not implement getNetwork still
// answers every operation Client requires.
type NetworkReporter interface {
	// ReportNetworkPassphrase returns the network passphrase the endpoint
	// reports serving, via the RPC's getNetwork method.
	ReportNetworkPassphrase(ctx context.Context) (string, error)
}

// Signer produces signatures for assembled transactions.
//
// Implementations hold key material and must never log, serialise, or otherwise
// expose it; only Address is safe to record.
//
// TODO(extension): SoroForge intentionally ships one implementation, reading a
// provided key (see KeypairSigner). Hardware wallets, remote signing services,
// and multisig collection are natural additions and only need to satisfy this
// interface — no orchestration code should need to change.
type Signer interface {
	// Address returns the signer's public G... address.
	Address() string

	// Sign returns a signed copy of tx for the given network passphrase.
	Sign(networkPassphrase string, tx *txnbuild.Transaction) (*txnbuild.Transaction, error)
}

// SimulateResult is the part of a simulateTransaction response SoroForge acts on.
//
// The XDR stays base64-encoded here so that the Client boundary carries no SDK
// types; Assemble decodes it.
type SimulateResult struct {
	// Error is the simulation's own error message. It is returned in a
	// successful RPC response, so callers must check it explicitly — a nil
	// error from Simulate does not mean the transaction would succeed.
	Error string

	// TransactionDataXDR is a base64 SorobanTransactionData carrying the
	// footprint, resource limits, and resource fee the transaction needs.
	TransactionDataXDR string

	// MinResourceFee is the resource fee in stroops, reported separately for
	// diagnostics; the authoritative value is inside TransactionDataXDR.
	MinResourceFee int64

	// Results holds one entry per host function invoked. Soroban permits a
	// single InvokeHostFunction operation per transaction, so this has at most
	// one element.
	Results []HostFunctionResult

	// RestoreRequired reports that archived ledger entries must be restored
	// before this transaction can succeed. SoroForge surfaces this rather than
	// silently submitting a doomed transaction.
	RestoreRequired bool

	// RestoreTransactionDataXDR is the SorobanTransactionData for the required
	// RestoreFootprint transaction, when RestoreRequired is set.
	RestoreTransactionDataXDR string

	// LatestLedger is the ledger the simulation ran against.
	LatestLedger uint32
}

// HostFunctionResult is a single host function's simulated outcome.
type HostFunctionResult struct {
	// ReturnValueXDR is a base64 ScVal. For contract creation it is the new
	// contract's address, which is how SoroForge learns the contract ID.
	ReturnValueXDR string

	// AuthXDR holds base64 SorobanAuthorizationEntry values recorded during
	// simulation, which must be attached to the transaction before submission.
	AuthXDR []string
}

// SendResult is a sendTransaction response.
type SendResult struct {
	// Hash is the hex transaction hash, usable with AwaitTransaction.
	Hash string

	// Status is one of PENDING, DUPLICATE, TRY_AGAIN_LATER, or ERROR.
	Status string

	// ErrorResultXDR is a base64 TransactionResult, present only when Status
	// is ERROR.
	ErrorResultXDR string

	LatestLedger uint32
}

// Transaction submission statuses returned by sendTransaction.
const (
	SendStatusPending       = "PENDING"
	SendStatusDuplicate     = "DUPLICATE"
	SendStatusTryAgainLater = "TRY_AGAIN_LATER"
	SendStatusError         = "ERROR"
)

// TxResult is a getTransaction response.
type TxResult struct {
	// Status is one of TxStatusSuccess, TxStatusFailed, or TxStatusNotFound.
	Status string

	Hash string

	// Ledger is the ledger that included the transaction, 0 if not included.
	Ledger uint32

	// ResultMetaXDR is a base64 TransactionMeta, the source of a contract's
	// return value once the transaction is on-chain.
	ResultMetaXDR string

	// ResultXDR is a base64 TransactionResult.
	ResultXDR string

	// FeeCharged is the actual transaction fee from the confirmed result XDR.
	FeeCharged *int64
}

// Transaction statuses returned by getTransaction.
const (
	TxStatusSuccess  = "SUCCESS"
	TxStatusFailed   = "FAILED"
	TxStatusNotFound = "NOT_FOUND"
)

// Succeeded reports whether the transaction was included and executed cleanly.
func (r TxResult) Succeeded() bool { return r.Status == TxStatusSuccess }

// LedgerEntry is one getLedgerEntries result.
type LedgerEntry struct {
	// KeyXDR is the base64 LedgerKey that was requested.
	KeyXDR string

	// DataXDR is the base64 LedgerEntryData found for that key.
	DataXDR string

	LastModifiedLedger uint32
}
