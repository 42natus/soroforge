// Package deploy orchestrates SoroForge's contract lifecycle operations.
//
// This is where the pieces meet: configuration says what to deploy and where,
// the stellar package assembles and submits transactions, and the store records
// what happened. Nothing in here talks to a network or a database directly —
// both arrive as interfaces, which is what makes the whole flow testable
// against fakes.
//
// The ordering rule throughout is that SoroForge records only what it has
// confirmed on-chain. A history that claims a deploy that never landed is worse
// than no history at all, so every write happens after confirmation, never
// before.
package deploy

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/soroworks/soroforge/internal/catalog"
	"github.com/soroworks/soroforge/internal/config"
	"github.com/soroworks/soroforge/internal/stellar"
	"github.com/soroworks/soroforge/internal/store"
	"github.com/stellar/go-stellar-sdk/txnbuild"
)

// ClientFactory creates a Stellar client for a named network.
//
// It is a function rather than a prebuilt client because one process serves
// several networks — the HTTP API can be asked to deploy to testnet and mainnet
// in consecutive requests — and because it is the seam tests replace to return
// a fake.
type ClientFactory func(networkName string, network config.Network) (stellar.Client, error)

// DefaultClientFactory connects to the network's configured RPC endpoint.
func DefaultClientFactory(_ string, network config.Network) (stellar.Client, error) {
	return stellar.NewRPCClient(stellar.RPCOptions{
		URL:               network.RPCURL,
		NetworkPassphrase: network.Passphrase,
	})
}

// Service performs deploys, upgrades, and status checks.
type Service struct {
	cfg     *config.Config
	store   store.Store
	signer  stellar.Signer
	clients ClientFactory
	log     *slog.Logger

	// baseFee is the per-operation fee in stroops. The resource fee that
	// simulation determines is added on top of it.
	baseFee int64

	// confirmTimeout bounds how long to wait for a submitted transaction to
	// reach a terminal state.
	confirmTimeout time.Duration

	// catalog publishes confirmed contracts to a network's sorovault_url.
	catalog catalog.Registrar

	// webhooks delivers notifications to endpoints configured on a network.
	webhooks WebhookSender
}

// Options configures a Service. Config, Store, and Signer are required.
type Options struct {
	Config *config.Config
	Store  store.Store

	// Signer may be nil for a read-only Service; Deploy and Upgrade then fail
	// with a clear message rather than a nil dereference. List, History, and
	// Status work without one.
	Signer stellar.Signer

	// Clients defaults to DefaultClientFactory.
	Clients ClientFactory

	// Log defaults to slog.Default().
	Log *slog.Logger

	// BaseFee defaults to txnbuild.MinBaseFee.
	BaseFee int64

	// ConfirmTimeout defaults to DefaultConfirmTimeout.
	ConfirmTimeout time.Duration

	// Catalog registers confirmed deploys and upgrades with the network's
	// sorovault_url, when one is configured. Defaults to a SoroVault HTTP
	// client; tests replace it.
	Catalog catalog.Registrar

	// Webhooks delivers notifications after confirmed deploys and upgrades.
	// Defaults to NewHTTPWebhookSender(nil).
	Webhooks WebhookSender
}

// DefaultConfirmTimeout bounds waiting for a transaction to be included.
//
// Stellar closes ledgers roughly every 5 seconds, so a transaction that has not
// landed in two minutes is not going to; waiting longer just delays the error.
// This also serves as the backstop for the SDK's PollTransaction, which
// otherwise polls until its context ends.
const DefaultConfirmTimeout = 2 * time.Minute

// New builds a Service.
func New(opts Options) (*Service, error) {
	if opts.Config == nil {
		return nil, fmt.Errorf("deploy: config is required")
	}
	if opts.Store == nil {
		return nil, fmt.Errorf("deploy: store is required")
	}

	clients := opts.Clients
	if clients == nil {
		clients = DefaultClientFactory
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	baseFee := opts.BaseFee
	if baseFee < txnbuild.MinBaseFee {
		baseFee = txnbuild.MinBaseFee
	}
	confirmTimeout := opts.ConfirmTimeout
	if confirmTimeout <= 0 {
		confirmTimeout = DefaultConfirmTimeout
	}
	registrar := opts.Catalog
	if registrar == nil {
		registrar = catalog.NewSoroVault(nil)
	}
	webhooks := opts.Webhooks
	if webhooks == nil {
		webhooks = NewHTTPWebhookSender(nil)
	}

	return &Service{
		cfg:            opts.Config,
		store:          opts.Store,
		signer:         opts.Signer,
		clients:        clients,
		log:            log,
		baseFee:        baseFee,
		confirmTimeout: confirmTimeout,
		catalog:        registrar,
		webhooks:       webhooks,
	}, nil
}

// CatalogStatus reports what happened when a confirmed contract was
// published to the network's interface registry.
type CatalogStatus struct {
	// Registry is the sorovault_url the contract was sent to.
	Registry string `json:"registry"`
	// OK is false when registration failed. The deploy itself still
	// succeeded; Error says why the catalog step did not.
	OK    bool            `json:"ok"`
	Error string          `json:"error,omitempty"`
	Entry *catalog.Result `json:"entry,omitempty"`
}

// publish registers a confirmed contract with the network's SoroVault, if
// one is configured. It never returns an error: the contract is already live
// and recorded, so a failure here is reported in the result and logged, and
// `sorovault add` or a later deploy can catch it up.
func (s *Service) publish(ctx context.Context, r resolved, contractID string, log *slog.Logger) *CatalogStatus {
	if r.Network.SoroVaultURL == "" {
		return nil
	}

	status := &CatalogStatus{Registry: r.Network.SoroVaultURL}
	entry, err := s.catalog.Register(ctx, r.Network.SoroVaultURL, contractID)
	if err != nil {
		status.Error = err.Error()
		log.Warn("contract is live but could not be registered with sorovault",
			"contract_id", contractID, "registry", r.Network.SoroVaultURL, "error", err)
		return status
	}

	status.OK = true
	status.Entry = entry
	log.Info("contract registered with sorovault",
		"contract_id", contractID, "url", entry.URL, "functions", entry.Functions)
	return status
}

// notifyWebhooks delivers webhook notifications to every endpoint configured on
// the network. Like publish, it is strictly best-effort: delivery failures are
// logged and recorded in the returned statuses, but never fail the deploy.
func (s *Service) notifyWebhooks(ctx context.Context, network config.Network, payload WebhookPayload, log *slog.Logger) []WebhookStatus {
	if len(network.Webhooks) == 0 {
		return nil
	}

	var results []WebhookStatus
	for _, wh := range network.Webhooks {
		status := WebhookStatus{URL: wh.URL}
		if err := s.webhooks.Send(ctx, wh, payload); err != nil {
			status.OK = false
			status.Error = err.Error()
			log.Warn("webhook delivery failed", "url", wh.URL, "error", err)
		} else {
			status.OK = true
			log.Info("webhook delivered", "url", wh.URL)
		}
		results = append(results, status)
	}
	return results
}

// Config exposes the loaded configuration, for callers that need to render
// network or contract names.
func (s *Service) Config() *config.Config { return s.cfg }

// resolve looks up the network and contract a request refers to and connects a
// client. Every operation starts here, so an unknown alias or network fails the
// same way regardless of entry point.
func (s *Service) resolve(networkName, alias string) (resolved, error) {
	name, network, err := s.cfg.Network(networkName)
	if err != nil {
		return resolved{}, err
	}
	contract, err := s.cfg.Contract(alias)
	if err != nil {
		return resolved{}, err
	}
	client, err := s.clients(name, network)
	if err != nil {
		return resolved{}, fmt.Errorf("connect to %s: %w", name, err)
	}
	return resolved{
		NetworkName: name,
		Network:     network,
		Alias:       alias,
		Contract:    contract,
		Client:      client,
	}, nil
}

type resolved struct {
	NetworkName string
	Network     config.Network
	Alias       string
	Contract    config.Contract
	Client      stellar.Client
}

// requireSigner reports a usable signer or explains what is missing.
func (s *Service) requireSigner() (stellar.Signer, error) {
	if s.signer == nil {
		return nil, fmt.Errorf(
			"no signing key configured: set %s or %s (see README security notes)",
			config.EnvSecretKey, config.EnvKeystorePath)
	}
	return s.signer, nil
}

// txResult describes one submitted transaction.
type txResult struct {
	Hash       string
	Ledger     uint32
	FeeCharged *int64

	// Simulation is retained so callers can read the host function's return
	// value — for a create, that is the new contract's address.
	Simulation stellar.SimulateResult

	// EnvelopeXDR is the assembled, signed envelope. On a dry run it is the
	// assembled but unsigned envelope, and it is the whole point of the run.
	EnvelopeXDR string
}

// runTransaction executes the full Soroban submission cycle for one operation:
// load the account, build, simulate, assemble, and — unless this is a dry run —
// sign, submit, and wait for confirmation.
//
// The simulate-then-assemble step is mandatory rather than an optimisation. A
// Soroban transaction must declare the exact ledger entries it touches and the
// resources it consumes, and simulation is the only way to discover them.
func (s *Service) runTransaction(
	ctx context.Context,
	r resolved,
	op *txnbuild.InvokeHostFunction,
	dryRun bool,
) (txResult, error) {
	signer, err := s.requireSigner()
	if err != nil {
		return txResult{}, err
	}

	source, err := r.Client.LoadAccount(ctx, signer.Address())
	if err != nil {
		return txResult{}, err
	}

	tx, err := stellar.Build(stellar.BuildParams{
		Source:    source,
		Operation: op,
		BaseFee:   s.baseFee,
	})
	if err != nil {
		return txResult{}, err
	}

	unsignedXDR, err := tx.Base64()
	if err != nil {
		return txResult{}, fmt.Errorf("encode transaction: %w", err)
	}

	sim, err := r.Client.Simulate(ctx, unsignedXDR)
	if err != nil {
		return txResult{}, err
	}

	assembled, err := stellar.Assemble(tx, sim)
	if err != nil {
		return txResult{}, err
	}

	if dryRun {
		envelope, err := assembled.Base64()
		if err != nil {
			return txResult{}, fmt.Errorf("encode assembled transaction: %w", err)
		}
		// Stop before signing. Nothing is sent and nothing is recorded; the
		// envelope is returned for inspection.
		return txResult{Simulation: sim, EnvelopeXDR: envelope}, nil
	}

	signed, err := signer.Sign(r.Client.NetworkPassphrase(), assembled)
	if err != nil {
		return txResult{}, err
	}
	signedXDR, err := signed.Base64()
	if err != nil {
		return txResult{}, fmt.Errorf("encode signed transaction: %w", err)
	}

	sent, err := r.Client.Send(ctx, signedXDR)
	if err != nil {
		return txResult{}, err
	}
	if err := checkSendStatus(sent); err != nil {
		return txResult{}, err
	}

	confirmed, err := s.confirm(ctx, r.Client, sent.Hash)
	if err != nil {
		return txResult{}, err
	}

	return txResult{
		Hash:        confirmed.Hash,
		Ledger:      confirmed.Ledger,
		FeeCharged:  confirmed.FeeCharged,
		Simulation:  sim,
		EnvelopeXDR: signedXDR,
	}, nil
}

// checkSendStatus turns a non-PENDING submission into a useful error.
func checkSendStatus(sent stellar.SendResult) error {
	switch sent.Status {
	case stellar.SendStatusPending, stellar.SendStatusDuplicate:
		// DUPLICATE means this exact transaction was already submitted, which
		// is fine — polling still finds its result.
		return nil
	case stellar.SendStatusTryAgainLater:
		return fmt.Errorf("RPC is busy and asked to try again later (transaction %s was not queued)", sent.Hash)
	case stellar.SendStatusError:
		if sent.ErrorResultXDR != "" {
			return fmt.Errorf("transaction rejected by the network (result XDR: %s)", sent.ErrorResultXDR)
		}
		return fmt.Errorf("transaction rejected by the network")
	default:
		return fmt.Errorf("unexpected submission status %q", sent.Status)
	}
}

// confirm waits for a submitted transaction to reach a terminal state.
//
// It applies confirmTimeout even when the caller's context has no deadline,
// because the SDK's polling relies entirely on the context to stop.
func (s *Service) confirm(ctx context.Context, client stellar.Client, hash string) (stellar.TxResult, error) {
	pollCtx, cancel := context.WithTimeout(ctx, s.confirmTimeout)
	defer cancel()

	result, err := client.AwaitTransaction(pollCtx, hash)
	if err != nil {
		return stellar.TxResult{}, fmt.Errorf("waiting for transaction %s: %w", hash, err)
	}

	// A FAILED transaction is a successful poll: the RPC answered, the answer
	// was "it failed". Checking status explicitly is required.
	if !result.Succeeded() {
		return stellar.TxResult{}, fmt.Errorf(
			"transaction %s was included in ledger %d but failed (status %s)",
			hash, result.Ledger, result.Status)
	}
	return result, nil
}
