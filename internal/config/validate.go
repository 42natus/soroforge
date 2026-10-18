package config

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/stellar/go-stellar-sdk/txnbuild"
)

// SupportedVersion is the only soroforge.yaml schema version understood today.
const SupportedVersion = 1

// Validate checks that the config is internally consistent and usable.
//
// It reports every problem it finds rather than stopping at the first, because
// fixing a config one error per run is a miserable way to start with a tool.
// Filesystem and network checks are deliberately excluded; `soroforge config
// check` performs those environment-dependent checks on demand.
func (c *Config) Validate() error {
	var problems []string

	if c.Version != SupportedVersion {
		problems = append(problems, fmt.Sprintf(
			"version: got %d, supported version is %d", c.Version, SupportedVersion))
	}

	if len(c.Networks) == 0 {
		problems = append(problems, "networks: at least one network must be configured")
	}
	for _, name := range sortedKeys(c.Networks) {
		problems = append(problems, validateNetwork(name, c.Networks[name])...)
	}

	switch {
	case c.DefaultNetwork == "":
		problems = append(problems, "default_network: must be set")
	case len(c.Networks) > 0:
		if _, ok := c.Networks[c.DefaultNetwork]; !ok {
			problems = append(problems, fmt.Sprintf(
				"default_network: %q is not one of the configured networks (%s)",
				c.DefaultNetwork, joinKeys(c.Networks)))
		}
	}

	if len(c.Contracts) == 0 {
		problems = append(problems, "contracts: at least one contract must be configured")
	}
	for _, alias := range sortedKeys(c.Contracts) {
		problems = append(problems, validateContract(alias, c.Contracts[alias], c.Networks)...)
	}

	if len(problems) == 0 {
		return nil
	}
	return errors.New("\n  - " + strings.Join(problems, "\n  - "))
}

// checkHTTPURL returns a problem description when raw is not an absolute
// http(s) URL, or "" when it is.
func checkHTTPURL(field, raw string) string {
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return fmt.Sprintf("%s: %v", field, err)
	case u.Scheme != "http" && u.Scheme != "https":
		return fmt.Sprintf("%s: scheme must be http or https, got %q", field, u.Scheme)
	case u.Host == "":
		return field + ": missing host"
	}
	return ""
}

func validateNetwork(name string, n Network) []string {
	var problems []string
	prefix := fmt.Sprintf("networks.%s", name)

	if name == "" {
		return []string{"networks: network names must not be empty"}
	}

	if strings.TrimSpace(n.RPCURL) == "" {
		problems = append(problems, prefix+".rpc_url: must be set")
	} else if p := checkHTTPURL(prefix+".rpc_url", n.RPCURL); p != "" {
		problems = append(problems, p)
	}

	if n.SoroVaultURL != "" {
		if p := checkHTTPURL(prefix+".sorovault_url", n.SoroVaultURL); p != "" {
			problems = append(problems, p)
		}
	}

	if n.ExplorerURL != "" {
		if p := checkHTTPURL(prefix+".explorer_url", n.ExplorerURL); p != "" {
			problems = append(problems, p)
		}
	}

	for i, wh := range n.Webhooks {
		field := fmt.Sprintf("%s.webhooks[%d].url", prefix, i)
		if strings.TrimSpace(wh.URL) == "" {
			problems = append(problems, field+": must be set")
		} else if p := checkHTTPURL(field, wh.URL); p != "" {
			problems = append(problems, p)
		}
	}

	// A blank passphrase would produce signatures valid on no network at all,
	// and the failure surfaces as an opaque tx_bad_auth at submission time.
	if strings.TrimSpace(n.Passphrase) == "" {
		problems = append(problems, prefix+".passphrase: must be set (it is mixed into every signature)")
	}

	return problems
}

func validateContract(alias string, c Contract, networks map[string]Network) []string {
	var problems []string
	prefix := fmt.Sprintf("contracts.%s", alias)

	if alias == "" {
		return []string{"contracts: contract aliases must not be empty"}
	}

	hasWasm := strings.TrimSpace(c.Wasm) != ""
	hasAsset := strings.TrimSpace(c.Asset) != ""

	switch {
	case !hasWasm && !hasAsset:
		problems = append(problems, prefix+".wasm: must be set")
	case hasWasm && hasAsset:
		problems = append(problems, prefix+": wasm and asset are mutually exclusive")
	case hasAsset:
		if err := validateAsset(c.Asset); err != nil {
			problems = append(problems, fmt.Sprintf("%s.asset: %v", prefix, err))
		}
		if len(c.ConstructorArgs) > 0 {
			problems = append(problems, prefix+": Stellar Asset Contracts do not take constructor args")
		}
		if c.Salt != "" {
			problems = append(problems, prefix+": Stellar Asset Contracts derive their ID from the asset; salt cannot be used")
		}
		if strings.TrimSpace(c.InitFunc) != "" || len(c.InitArgs) > 0 {
			problems = append(problems, prefix+": Stellar Asset Contracts are not initialized; init_fn and init_args cannot be used")
		}
	case hasWasm:
		for i, arg := range c.ConstructorArgs {
			if _, err := arg.ToScVal(); err != nil {
				problems = append(problems, fmt.Sprintf("%s.constructor_args[%d]: %v", prefix, i, err))
			}
		}

		if strings.TrimSpace(c.InitFunc) != "" && len(c.ConstructorArgs) > 0 {
			problems = append(problems, fmt.Sprintf("%s: cannot configure both constructor_args and init_fn", prefix))
		}

		for i, arg := range c.InitArgs {
			if _, err := arg.ToScVal(); err != nil {
				problems = append(problems, fmt.Sprintf("%s.init_args[%d]: %v", prefix, i, err))
			}
		}

		if len(c.InitArgs) > 0 && strings.TrimSpace(c.InitFunc) == "" {
			problems = append(problems, prefix+".init_args: init_fn must be set when init_args are configured")
		}

		if c.Salt != "" {
			if _, err := ParseSalt(c.Salt); err != nil {
				problems = append(problems, fmt.Sprintf("%s.salt: %v", prefix, err))
			}
		}
	}

	// Overrides are validated in their own context, with the full path
	// (`contracts.token.networks.mainnet.salt`) so a bad override cannot be
	// mistaken for the contract-level setting it replaces.
	for _, network := range sortedKeys(c.Networks) {
		problems = append(problems, validateContractOverride(prefix, network, c.Networks[network], networks)...)
	}

	return problems
}

func validateContractOverride(prefix, network string, o ContractOverride, networks map[string]Network) []string {
	var problems []string
	overridePrefix := fmt.Sprintf("%s.networks.%s", prefix, network)

	if _, ok := networks[network]; !ok {
		problems = append(problems, fmt.Sprintf(
			"%s: unknown network (configured: %s)", overridePrefix, joinKeys(networks)))
	}

	for i, arg := range o.ConstructorArgs {
		if _, err := arg.ToScVal(); err != nil {
			problems = append(problems, fmt.Sprintf("%s.constructor_args[%d]: %v", overridePrefix, i, err))
		}
	}

	if o.Salt != "" {
		if _, err := ParseSalt(o.Salt); err != nil {
			problems = append(problems, fmt.Sprintf("%s.salt: %v", overridePrefix, err))
		}
	}

	return problems
}

func validateAsset(s string) error {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return errors.New("must not be empty")
	}
	_, err := txnbuild.ParseAssetString(trimmed)
	return err
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func joinKeys[V any](m map[string]V) string {
	if len(m) == 0 {
		return "none"
	}
	return strings.Join(sortedKeys(m), ", ")
}
