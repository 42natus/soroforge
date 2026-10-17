package stellar

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/stellar/go-stellar-sdk/clients/rpcclient"
	protocol "github.com/stellar/go-stellar-sdk/protocols/rpc"
	"github.com/stellar/go-stellar-sdk/txnbuild"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// This file is the only place SoroForge touches the Stellar RPC SDK. Everything
// else in the codebase works against the Client interface in stellar.go, so an
// SDK upgrade that changes response shapes should be contained here.
//
// Verified against github.com/stellar/go-stellar-sdk v0.7.1:
//   rpcclient.NewClient(url string, httpClient *http.Client) *Client
//   SimulateTransaction(ctx, protocol.SimulateTransactionRequest) (protocol.SimulateTransactionResponse, error)
//   SendTransaction(ctx, protocol.SendTransactionRequest)         (protocol.SendTransactionResponse, error)
//   PollTransaction(ctx, txHash string)                           (protocol.GetTransactionResponse, error)
//   GetLedgerEntries(ctx, protocol.GetLedgerEntriesRequest)       (protocol.GetLedgerEntriesResponse, error)
//   GetLatestLedger(ctx)                                          (protocol.GetLatestLedgerResponse, error)
//   LoadAccount(ctx, address string)                              (txnbuild.Account, error)

// DefaultHTTPTimeout bounds a single RPC call. Transaction confirmation is not
// a single call — AwaitTransaction polls — so this only needs to cover one
// request/response round trip.
const DefaultHTTPTimeout = 30 * time.Second

// RPCClient is the live Client implementation, backed by a Stellar RPC endpoint.
type RPCClient struct {
	rpc        *rpcclient.Client
	passphrase string
}

// Compile-time check that the live implementation satisfies the interface every
// other package depends on.
var _ Client = (*RPCClient)(nil)

// RPCOptions configures a live client.
type RPCOptions struct {
	// URL is the Stellar RPC endpoint.
	URL string

	// NetworkPassphrase is the passphrase transactions are signed for. It is
	// taken from configuration, not from the server, so a mismatch between the
	// configured network and the endpoint cannot silently produce signatures
	// for the wrong network.
	NetworkPassphrase string

	// HTTPClient overrides the default HTTP client. Optional.
	HTTPClient *http.Client
}

// NewRPCClient connects to a Stellar RPC endpoint.
func NewRPCClient(opts RPCOptions) (*RPCClient, error) {
	if opts.URL == "" {
		return nil, fmt.Errorf("stellar: RPC URL is required")
	}
	if opts.NetworkPassphrase == "" {
		return nil, fmt.Errorf("stellar: network passphrase is required")
	}

	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: DefaultHTTPTimeout}
	}

	return &RPCClient{
		rpc:        rpcclient.NewClient(opts.URL, httpClient),
		passphrase: opts.NetworkPassphrase,
	}, nil
}

// Close releases the underlying client's resources.
func (c *RPCClient) Close() error { return c.rpc.Close() }

// NetworkPassphrase implements Client.
func (c *RPCClient) NetworkPassphrase() string { return c.passphrase }

// LoadAccount implements Client.
func (c *RPCClient) LoadAccount(ctx context.Context, address string) (txnbuild.Account, error) {
	account, err := c.rpc.LoadAccount(ctx, address)
	if err != nil {
		return nil, fmt.Errorf("load account %s: %w", address, err)
	}
	return account, nil
}

// Simulate implements Client.
func (c *RPCClient) Simulate(ctx context.Context, txXDR string) (SimulateResult, error) {
	resp, err := c.rpc.SimulateTransaction(ctx, protocol.SimulateTransactionRequest{
		Transaction: txXDR,
	})
	if err != nil {
		return SimulateResult{}, fmt.Errorf("simulateTransaction: %w", err)
	}

	out := SimulateResult{
		Error:              resp.Error,
		TransactionDataXDR: resp.TransactionDataXDR,
		MinResourceFee:     resp.MinResourceFee,
		LatestLedger:       resp.LatestLedger,
	}

	if resp.RestorePreamble != nil && resp.RestorePreamble.TransactionDataXDR != "" {
		out.RestoreRequired = true
		out.RestoreTransactionDataXDR = resp.RestorePreamble.TransactionDataXDR
	}

	for _, r := range resp.Results {
		result := HostFunctionResult{}
		if r.ReturnValueXDR != nil {
			result.ReturnValueXDR = *r.ReturnValueXDR
		}
		if r.AuthXDR != nil {
			result.AuthXDR = *r.AuthXDR
		}
		out.Results = append(out.Results, result)
	}

	return out, nil
}

// Send implements Client.
func (c *RPCClient) Send(ctx context.Context, signedTxXDR string) (SendResult, error) {
	resp, err := c.rpc.SendTransaction(ctx, protocol.SendTransactionRequest{
		Transaction: signedTxXDR,
	})
	if err != nil {
		return SendResult{}, fmt.Errorf("sendTransaction: %w", err)
	}
	return SendResult{
		Hash:           resp.Hash,
		Status:         resp.Status,
		ErrorResultXDR: resp.ErrorResultXDR,
		LatestLedger:   resp.LatestLedger,
	}, nil
}

// AwaitTransaction implements Client.
//
// It delegates to the SDK's PollTransaction, which retries getTransaction with
// exponential backoff until the transaction reaches a terminal state. Two
// behaviours are worth knowing:
//
//   - It returns a nil error for a FAILED transaction, because failure is a
//     valid terminal state. Callers must check Status, not just err.
//   - It disables its own elapsed-time limit and relies entirely on the
//     context, so ctx must carry a deadline or this will poll indefinitely.
//     Service.confirm supplies one.
func (c *RPCClient) AwaitTransaction(ctx context.Context, hash string) (TxResult, error) {
	resp, err := c.rpc.PollTransaction(ctx, hash)
	if err != nil {
		return TxResult{}, fmt.Errorf("getTransaction %s: %w", hash, err)
	}
	feeCharged := feeChargedFromResultXDR(resp.ResultXDR)
	return TxResult{
		Status:        resp.Status,
		Hash:          resp.TransactionHash,
		Ledger:        resp.Ledger,
		ResultMetaXDR: resp.ResultMetaXDR,
		ResultXDR:     resp.ResultXDR,
		FeeCharged:    feeCharged,
	}, nil
}

func feeChargedFromResultXDR(resultXDR string) *int64 {
	if resultXDR == "" {
		return nil
	}
	var result xdr.TransactionResult
	if err := xdr.SafeUnmarshalBase64(resultXDR, &result); err != nil {
		return nil
	}
	fee := int64(result.FeeCharged)
	return &fee
}

// LedgerEntries implements Client.
func (c *RPCClient) LedgerEntries(ctx context.Context, keysB64 []string) ([]LedgerEntry, error) {
	if len(keysB64) == 0 {
		return nil, nil
	}
	resp, err := c.rpc.GetLedgerEntries(ctx, protocol.GetLedgerEntriesRequest{Keys: keysB64})
	if err != nil {
		return nil, fmt.Errorf("getLedgerEntries: %w", err)
	}

	entries := make([]LedgerEntry, 0, len(resp.Entries))
	for _, e := range resp.Entries {
		entries = append(entries, LedgerEntry{
			KeyXDR:             e.KeyXDR,
			DataXDR:            e.DataXDR,
			LastModifiedLedger: e.LastModifiedLedger,
		})
	}
	return entries, nil
}

// LatestLedger implements Client.
func (c *RPCClient) LatestLedger(ctx context.Context) (uint32, error) {
	resp, err := c.rpc.GetLatestLedger(ctx)
	if err != nil {
		return 0, fmt.Errorf("getLatestLedger: %w", err)
	}
	return resp.Sequence, nil
}
