package stellar

import (
	"context"
	"fmt"
	"sync"

	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/txnbuild"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// This file provides in-memory stand-ins for the network. They live in the
// package rather than a _test.go file because the deploy, api, and cmd packages
// all need them, and because a contributor adding a feature should be able to
// test it without provisioning an RPC endpoint.
//
// MockClient records what it was asked to do and returns what it was told to
// return. It does not simulate Soroban — it is a test double, not an emulator —
// so tests assert on the transactions SoroForge builds, which is the part
// SoroForge is responsible for.

// MockClient is a Client that answers from canned data.
type MockClient struct {
	mu sync.Mutex

	// Passphrase is returned by NetworkPassphrase.
	Passphrase string

	// Sequence is the sequence number LoadAccount reports.
	Sequence int64

	// LedgerSeq is returned by LatestLedger.
	LedgerSeq uint32

	// LatestLedgerErr, if set, makes LatestLedger fail — the shape a network
	// health check uses to exercise an unreachable endpoint.
	LatestLedgerErr error

	// ReportPassphrase is returned by ReportNetworkPassphrase. Empty means
	// reuse Passphrase, which models an endpoint serving its configured
	// network; set it to a different value to model a URL pointing at the
	// wrong network.
	ReportPassphrase string

	// ReportPassphraseErr, if set, makes ReportNetworkPassphrase fail,
	// modelling an endpoint without getNetwork support.
	ReportPassphraseErr error

	// SimulateFunc overrides simulation entirely. When nil, Simulate returns
	// SimulateResponse.
	SimulateFunc func(ctx context.Context, txXDR string) (SimulateResult, error)

	// SimulateResponse is returned by Simulate when SimulateFunc is nil.
	SimulateResponse SimulateResult

	// SendFunc overrides submission. When nil, Send reports PENDING with
	// SendHash.
	SendFunc func(ctx context.Context, signedTxXDR string) (SendResult, error)

	// SendHash is the transaction hash reported by the default Send.
	SendHash string

	// AwaitResponse is returned by AwaitTransaction. Its zero value is not
	// usable; NewMockClient sets a SUCCESS default.
	AwaitResponse TxResult
	AwaitErr      error

	// Entries maps a base64 LedgerKey to the base64 LedgerEntryData returned
	// for it. Keys absent from the map are reported as not found, which is how
	// tests express "this contract is not deployed" or "this wasm is not
	// uploaded".
	Entries map[string]string

	// LoadAccountErr, if set, makes LoadAccount fail.
	LoadAccountErr error

	// Recorded calls, for assertions.
	Simulated []string // unsigned envelopes passed to Simulate
	Sent      []string // signed envelopes passed to Send
	Awaited   []string // hashes passed to AwaitTransaction
}

var _ Client = (*MockClient)(nil)
var _ NetworkReporter = (*MockClient)(nil)

// NewMockClient returns a client with workable defaults: testnet passphrase, a
// successful confirmation, and no ledger entries.
func NewMockClient() *MockClient {
	return &MockClient{
		Passphrase: "Test SDF Network ; September 2015",
		Sequence:   1,
		LedgerSeq:  1000,
		SendHash:   "0000000000000000000000000000000000000000000000000000000000000001",
		AwaitResponse: TxResult{
			Status: TxStatusSuccess,
			Hash:   "0000000000000000000000000000000000000000000000000000000000000001",
			Ledger: 1000,
		},
		Entries: map[string]string{},
	}
}

// NetworkPassphrase implements Client.
func (m *MockClient) NetworkPassphrase() string { return m.Passphrase }

// LoadAccount implements Client.
func (m *MockClient) LoadAccount(_ context.Context, address string) (txnbuild.Account, error) {
	if m.LoadAccountErr != nil {
		return nil, m.LoadAccountErr
	}
	return &txnbuild.SimpleAccount{AccountID: address, Sequence: m.Sequence}, nil
}

// Simulate implements Client.
func (m *MockClient) Simulate(ctx context.Context, txXDR string) (SimulateResult, error) {
	m.mu.Lock()
	m.Simulated = append(m.Simulated, txXDR)
	m.mu.Unlock()

	if m.SimulateFunc != nil {
		return m.SimulateFunc(ctx, txXDR)
	}
	return m.SimulateResponse, nil
}

// Send implements Client.
func (m *MockClient) Send(ctx context.Context, signedTxXDR string) (SendResult, error) {
	m.mu.Lock()
	m.Sent = append(m.Sent, signedTxXDR)
	m.mu.Unlock()

	if m.SendFunc != nil {
		return m.SendFunc(ctx, signedTxXDR)
	}
	return SendResult{Hash: m.SendHash, Status: SendStatusPending, LatestLedger: m.LedgerSeq}, nil
}

// AwaitTransaction implements Client.
func (m *MockClient) AwaitTransaction(_ context.Context, hash string) (TxResult, error) {
	m.mu.Lock()
	m.Awaited = append(m.Awaited, hash)
	m.mu.Unlock()

	if m.AwaitErr != nil {
		return TxResult{}, m.AwaitErr
	}
	return m.AwaitResponse, nil
}

// LedgerEntries implements Client. Keys with no entry are omitted from the
// result, matching how the RPC reports a missing entry.
func (m *MockClient) LedgerEntries(_ context.Context, keysB64 []string) ([]LedgerEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var out []LedgerEntry
	for _, k := range keysB64 {
		if data, ok := m.Entries[k]; ok {
			out = append(out, LedgerEntry{KeyXDR: k, DataXDR: data, LastModifiedLedger: m.LedgerSeq})
		}
	}
	return out, nil
}

// LatestLedger implements Client.
func (m *MockClient) LatestLedger(_ context.Context) (uint32, error) {
	if m.LatestLedgerErr != nil {
		return 0, m.LatestLedgerErr
	}
	return m.LedgerSeq, nil
}

// ReportNetworkPassphrase implements NetworkReporter. It returns
// ReportPassphrase when set, otherwise the configured Passphrase, so that a
// default mock reports a matching network; ReportPassphraseErr stands in for
// an endpoint whose getNetwork fails or is unsupported.
func (m *MockClient) ReportNetworkPassphrase(_ context.Context) (string, error) {
	if m.ReportPassphraseErr != nil {
		return "", m.ReportPassphraseErr
	}
	if m.ReportPassphrase != "" {
		return m.ReportPassphrase, nil
	}
	return m.Passphrase, nil
}

// SendCount reports how many envelopes were submitted. Dry-run tests assert
// this is zero.
func (m *MockClient) SendCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.Sent)
}

// SimulateCount reports how many transactions were simulated.
func (m *MockClient) SimulateCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.Simulated)
}

// SetContractInstance makes contractID resolve on-chain to wasmHash, so that
// drift detection and upgrade checks have something to read.
func (m *MockClient) SetContractInstance(contractID string, wasmHash xdr.Hash) error {
	key, err := InstanceLedgerKey(contractID)
	if err != nil {
		return err
	}
	encodedKey, err := xdr.MarshalBase64(key)
	if err != nil {
		return err
	}

	hash := wasmHash
	entry := xdr.LedgerEntryData{
		Type: xdr.LedgerEntryTypeContractData,
		ContractData: &xdr.ContractDataEntry{
			Contract:   mustContractAddress(contractID),
			Key:        xdr.ScVal{Type: xdr.ScValTypeScvLedgerKeyContractInstance},
			Durability: xdr.ContractDataDurabilityPersistent,
			Val: xdr.ScVal{
				Type: xdr.ScValTypeScvContractInstance,
				Instance: &xdr.ScContractInstance{
					Executable: xdr.ContractExecutable{
						Type:     xdr.ContractExecutableTypeContractExecutableWasm,
						WasmHash: &hash,
					},
				},
			},
		},
	}
	encodedEntry, err := xdr.MarshalBase64(entry)
	if err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.Entries[encodedKey] = encodedEntry
	return nil
}

// SetWasmUploaded marks bytecode as already present on-chain, so that upload
// steps are skipped.
func (m *MockClient) SetWasmUploaded(wasmHash xdr.Hash) error {
	encodedKey, err := xdr.MarshalBase64(CodeLedgerKey(wasmHash))
	if err != nil {
		return err
	}
	entry := xdr.LedgerEntryData{
		Type: xdr.LedgerEntryTypeContractCode,
		ContractCode: &xdr.ContractCodeEntry{
			Hash: wasmHash,
			Code: []byte{0x00},
		},
	}
	encodedEntry, err := xdr.MarshalBase64(entry)
	if err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.Entries[encodedKey] = encodedEntry
	return nil
}

func mustContractAddress(contractID string) xdr.ScAddress {
	addr, err := ContractAddress(contractID)
	if err != nil {
		panic(fmt.Sprintf("stellar: invalid contract id in mock setup: %v", err))
	}
	return addr
}

// MockSigner signs with a fixed keypair.
//
// It uses a real keypair rather than returning a fake signature, so tests
// exercise the genuine signing path and the resulting envelope is a valid,
// verifiable transaction.
type MockSigner struct {
	full *keypair.Full

	mu sync.Mutex
	// Signed counts Sign calls, so dry-run tests can assert nothing was signed.
	Signed int

	// SignErr, if set, makes Sign fail.
	SignErr error
}

var _ Signer = (*MockSigner)(nil)

// NewMockSigner returns a signer backed by a freshly generated keypair.
func NewMockSigner() *MockSigner {
	return &MockSigner{full: keypair.MustRandom()}
}

// NewMockSignerFromSeed returns a signer for a specific seed, for tests that
// need a stable address.
func NewMockSignerFromSeed(seed string) (*MockSigner, error) {
	full, err := keypair.ParseFull(seed)
	if err != nil {
		return nil, err
	}
	return &MockSigner{full: full}, nil
}

// Address implements Signer.
func (s *MockSigner) Address() string { return s.full.Address() }

// Sign implements Signer.
func (s *MockSigner) Sign(networkPassphrase string, tx *txnbuild.Transaction) (*txnbuild.Transaction, error) {
	s.mu.Lock()
	s.Signed++
	s.mu.Unlock()

	if s.SignErr != nil {
		return nil, s.SignErr
	}
	return tx.Sign(networkPassphrase, s.full)
}

// SignCount reports how many times Sign was called.
func (s *MockSigner) SignCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Signed
}
