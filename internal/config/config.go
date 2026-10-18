// Package config loads and validates SoroForge project configuration from a
// soroforge.yaml file plus environment variables.
//
// The YAML file describes what is stable and shareable — the networks a team
// deploys to and the contracts it manages — and belongs in version control.
// Secrets and per-machine settings come from the environment and must never be
// written to the YAML file. See Env for the recognised variables.
package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// DefaultFilename is the config file looked up when no path is supplied.
const DefaultFilename = "soroforge.yaml"

// DefaultUpgradeFunc is the contract function SoroForge invokes to perform an
// upgrade when a contract does not override it.
//
// This is a contract-level convention rather than a protocol rule: the Soroban
// docs suggest an `upgrade(new_wasm_hash: BytesN<32>)` entrypoint that calls
// env.deployer().update_current_contract_wasm(). Contracts that name it
// differently can set `upgrade_fn` on the contract entry.
const DefaultUpgradeFunc = "upgrade"

// Config is a parsed and validated soroforge.yaml, merged with environment
// settings. Construct it with Load rather than by hand so that validation and
// defaulting always run.
type Config struct {
	// Version is the config schema version. Only version 1 exists today; it is
	// declared explicitly so the format can evolve without guessing.
	Version int `yaml:"version"`

	// DefaultNetwork is the network used when a command omits --network. It
	// must name an entry in Networks.
	DefaultNetwork string `yaml:"default_network"`

	// Networks maps a short name (testnet, mainnet, staging, ...) to its RPC
	// endpoint and passphrase. Every stored record is namespaced by this name.
	Networks map[string]Network `yaml:"networks"`

	// Contracts maps a contract alias to its build artifact and deploy
	// settings. The alias is how the contract is referred to on the command
	// line and in the deployment history.
	Contracts map[string]Contract `yaml:"contracts"`

	// Env holds settings sourced from environment variables. It is populated
	// by Load and is never read from the YAML file.
	Env Env `yaml:"-"`

	// path is the file this config was loaded from, used to resolve relative
	// WASM paths against the project directory rather than the process CWD.
	path string
}

// Network is a Stellar network SoroForge can deploy to.
//
// The passphrase is required and not inferred from the name: it is mixed into
// every transaction signature, so a wrong value produces a transaction that is
// invalid on the intended network — and a silently defaulted one could route a
// mainnet deploy at a testnet key. Making it explicit keeps that decision
// visible in review.
type Network struct {
	// RPCURL is the Stellar RPC endpoint, e.g. https://soroban-testnet.stellar.org.
	RPCURL string `yaml:"rpc_url"`

	// Passphrase is the network passphrase, e.g. "Test SDF Network ; September 2015".
	Passphrase string `yaml:"passphrase"`

	// SoroVaultURL, when set, is a SoroVault registry serving this network.
	// Every confirmed deploy and upgrade is registered there, so the
	// contract's interface is discoverable as soon as it is live. Optional.
	SoroVaultURL string `yaml:"sorovault_url"`

	// Webhooks lists endpoints notified after each confirmed deploy or upgrade.
	// Optional.
	Webhooks []Webhook `yaml:"webhooks"`
}

// Webhook configures a notification target for confirmed contract events.
type Webhook struct {
	URL    string `yaml:"url"`
	Secret string `yaml:"secret"`
}

// UnmarshalYAML supports both a bare URL string and an object with url and secret.
func (w *Webhook) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		w.URL = node.Value
		return nil
	}
	if node.Kind == yaml.MappingNode {
		type rawWebhook Webhook
		var raw rawWebhook
		if err := node.Decode(&raw); err != nil {
			return err
		}
		*w = Webhook(raw)
		return nil
	}
	return fmt.Errorf("webhook: expected a URL string or {url, secret} mapping")
}

// Contract is a contract SoroForge manages.
type Contract struct {
	// Wasm is the path to the compiled contract artifact, relative to the
	// config file's directory unless absolute.
	Wasm string `yaml:"wasm"`

	// ConstructorArgs are passed to the contract's constructor at
	// instantiation. Empty is valid and common.
	ConstructorArgs []Arg `yaml:"constructor_args"`

	// InitFunc is the contract entrypoint invoked immediately after
	// instantiation to initialize contracts that lack constructors.
	InitFunc string `yaml:"init_fn"`

	// InitArgs are passed to the initializer entrypoint named by InitFunc.
	InitArgs []Arg `yaml:"init_args"`

	// UpgradeFunc overrides the contract function invoked by `soroforge
	// upgrade`. Defaults to DefaultUpgradeFunc.
	UpgradeFunc string `yaml:"upgrade_fn"`

	// Salt is an optional 32-byte hex value that makes the derived contract ID
	// deterministic. When empty a random salt is generated per deploy, so
	// deploying the same alias twice yields two distinct contracts.
	Salt string `yaml:"salt"`
}

// UpgradeFuncOrDefault returns the configured upgrade entrypoint, falling back
// to DefaultUpgradeFunc.
func (c Contract) UpgradeFuncOrDefault() string {
	if c.UpgradeFunc == "" {
		return DefaultUpgradeFunc
	}
	return c.UpgradeFunc
}

// Load reads the config at path, overlays environment variables, validates the
// result, and returns it. An empty path resolves to DefaultFilename in the
// current directory, or to $SOROFORGE_CONFIG when that is set.
func Load(path string) (*Config, error) {
	resolved, err := resolvePath(path)
	if err != nil {
		return nil, err
	}

	raw, err := os.ReadFile(resolved)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", resolved, err)
	}

	cfg, err := parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse config %s: %w", resolved, err)
	}
	cfg.path = resolved
	cfg.Env = LoadEnv()

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", resolved, err)
	}
	return cfg, nil
}

// parse decodes YAML into a Config without touching the filesystem or
// environment. Kept separate from Load so tests can exercise decoding and
// validation on in-memory documents.
func parse(raw []byte) (*Config, error) {
	var cfg Config
	// KnownFields makes typos in the config a loud error instead of a silently
	// ignored key — a misspelled `passphrase` should not deploy to the wrong
	// network.
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func resolvePath(path string) (string, error) {
	if path == "" {
		path = os.Getenv(EnvConfigPath)
	}
	if path == "" {
		path = DefaultFilename
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve config path %q: %w", path, err)
	}
	return abs, nil
}

// Path returns the absolute path this config was loaded from, or "" if it was
// not loaded from a file.
func (c *Config) Path() string { return c.path }

// Dir returns the directory containing the config file, used as the base for
// relative WASM paths.
func (c *Config) Dir() string {
	if c.path == "" {
		return "."
	}
	return filepath.Dir(c.path)
}

// Network looks up a network by name. An empty name selects DefaultNetwork.
func (c *Config) Network(name string) (string, Network, error) {
	if name == "" {
		name = c.DefaultNetwork
	}
	net, ok := c.Networks[name]
	if !ok {
		return "", Network{}, fmt.Errorf("unknown network %q (configured: %s)", name, joinKeys(c.Networks))
	}
	return name, net, nil
}

// Contract looks up a contract by alias.
func (c *Config) Contract(alias string) (Contract, error) {
	contract, ok := c.Contracts[alias]
	if !ok {
		return Contract{}, fmt.Errorf("unknown contract %q (configured: %s)", alias, joinKeys(c.Contracts))
	}
	return contract, nil
}

// WasmPath returns the absolute path to a contract's WASM artifact, resolving
// relative paths against the config file's directory so that `soroforge deploy`
// behaves identically regardless of the shell's working directory.
func (c *Config) WasmPath(contract Contract) string {
	if filepath.IsAbs(contract.Wasm) {
		return contract.Wasm
	}
	return filepath.Join(c.Dir(), contract.Wasm)
}
