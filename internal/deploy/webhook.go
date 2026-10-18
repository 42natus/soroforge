package deploy

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/soroworks/soroforge/internal/config"
	"github.com/soroworks/soroforge/internal/store"
)

// DefaultWebhookTimeout bounds a single webhook delivery attempt.
const DefaultWebhookTimeout = 10 * time.Second

// WebhookStatus reports the outcome of delivering a webhook to one URL.
type WebhookStatus struct {
	URL   string `json:"url"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// WebhookPayload is the JSON payload sent to configured webhook endpoints after
// a confirmed deploy or upgrade.
type WebhookPayload struct {
	Alias          string       `json:"alias"`
	Network        string       `json:"network"`
	ContractID     string       `json:"contract_id"`
	Contract       string       `json:"contract,omitempty"`
	WasmHash       string       `json:"wasm_hash"`
	Action         store.Action `json:"action"`
	DeployerPubkey string       `json:"deployer_pubkey"`
	TxHash         string       `json:"tx_hash,omitempty"`
	Tx             string       `json:"tx,omitempty"`
	Ledger         uint32       `json:"ledger,omitempty"`
	Notes          string       `json:"notes,omitempty"`
}

// WebhookSender delivers webhooks.
type WebhookSender interface {
	Send(ctx context.Context, webhook config.Webhook, payload WebhookPayload) error
}

// HTTPWebhookSender delivers webhooks via HTTP POST.
type HTTPWebhookSender struct {
	client *http.Client
}

var _ WebhookSender = (*HTTPWebhookSender)(nil)

// NewHTTPWebhookSender returns a WebhookSender using client, or an http.Client with
// DefaultWebhookTimeout when client is nil.
func NewHTTPWebhookSender(client *http.Client) *HTTPWebhookSender {
	if client == nil {
		client = &http.Client{Timeout: DefaultWebhookTimeout}
	}
	return &HTTPWebhookSender{client: client}
}

// Send delivers a JSON payload to a webhook URL, optionally signing with HMAC-SHA256.
func (s *HTTPWebhookSender) Send(ctx context.Context, wh config.Webhook, payload WebhookPayload) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal webhook payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, wh.URL, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("create webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "SoroForge-Webhook")

	if wh.Secret != "" {
		mac := hmac.New(sha256.New, []byte(wh.Secret))
		mac.Write(raw)
		sig := hex.EncodeToString(mac.Sum(nil))
		req.Header.Set("X-SoroForge-Signature", "sha256="+sig)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("deliver webhook: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned status %s", resp.Status)
	}
	return nil
}
