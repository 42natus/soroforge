package deploy_test

import (
	"fmt"
	"testing"

	"github.com/soroworks/soroforge/internal/config"
	"github.com/soroworks/soroforge/internal/deploy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The passphrases are the ones the deploy fixtures use for testnet and mainnet
// (see deploy_test.go and catalog_test.go), so a regression here shows up the
// same way the fixture config would present it.
const (
	testnetPassphrase = "Test SDF Network ; September 2015"
	mainnetPassphrase = "Public Global Stellar Network ; September 2015"
)

// TestDefaultClientFactoryWiresTheNetworkPassphrase pins config.Network's two
// fields to stellar.RPCOptions' two fields. Swapping rpc_url and passphrase
// would sign transactions for the wrong network, and nothing downstream
// notices until the submission is rejected — so each network's built client
// must report the passphrase configured for that network, not the other one's.
func TestDefaultClientFactoryWiresTheNetworkPassphrase(t *testing.T) {
	tests := map[string]struct {
		network config.Network
		want    string
	}{
		"testnet": {
			network: config.Network{
				RPCURL:     "https://soroban-testnet.example.org",
				Passphrase: testnetPassphrase,
			},
			want: testnetPassphrase,
		},
		"mainnet": {
			network: config.Network{
				RPCURL:     "https://soroban-mainnet.example.org",
				Passphrase: mainnetPassphrase,
			},
			want: mainnetPassphrase,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			client, err := deploy.DefaultClientFactory(name, tc.network)
			require.NoError(t, err, "building a client must not touch the network")
			assert.Equal(t, tc.want, client.NetworkPassphrase())
		})
	}
}

// TestDefaultClientFactoryKeepsNetworksApart builds clients for two networks
// back to back — the sequence a server serves when consecutive requests deploy
// to testnet and then mainnet — and proves neither client picked up the other
// network's passphrase.
func TestDefaultClientFactoryKeepsNetworksApart(t *testing.T) {
	testnet := config.Network{RPCURL: "https://rpc.example.org", Passphrase: testnetPassphrase}
	mainnet := config.Network{RPCURL: "https://mainnet.example.org", Passphrase: mainnetPassphrase}

	testnetClient, err := deploy.DefaultClientFactory("testnet", testnet)
	require.NoError(t, err)
	mainnetClient, err := deploy.DefaultClientFactory("mainnet", mainnet)
	require.NoError(t, err)

	assert.Equal(t, testnetPassphrase, testnetClient.NetworkPassphrase())
	assert.Equal(t, mainnetPassphrase, mainnetClient.NetworkPassphrase())
	assert.NotEqual(t, testnetClient.NetworkPassphrase(), mainnetClient.NetworkPassphrase(),
		"the two networks' passphrases must never be swapped")
}

// TestDefaultClientFactoryRejectsInvalidRPCURLs proves a bad endpoint fails at
// construction with the offending value named, rather than handing back a
// client that errors opaquely on its first request.
func TestDefaultClientFactoryRejectsInvalidRPCURLs(t *testing.T) {
	tests := map[string]string{
		"not a URL at all":  "not a url",
		"missing scheme":    "://missing-scheme",
		"non-http scheme":   "ftp://example.org",
		"empty string":      "",
		"http with no host": "https://",
	}

	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			client, err := deploy.DefaultClientFactory("testnet", config.Network{
				RPCURL:     raw,
				Passphrase: testnetPassphrase,
			})

			require.Error(t, err)
			assert.Nil(t, client, "an invalid URL must produce an error, not a client")
			assert.Contains(t, err.Error(), fmt.Sprintf("%q", raw),
				"the error must name the bad URL so it can be found in the config")
		})
	}
}
