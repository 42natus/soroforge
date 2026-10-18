package main

import (
	"fmt"
	"io"
	"sort"
	"text/tabwriter"

	"github.com/soroworks/soroforge/internal/config"
	"github.com/spf13/cobra"
)

// newConfigCmd groups commands that inspect SoroForge's configuration.
func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect and validate the configuration SoroForge resolves",
		Args:  cobra.NoArgs,
	}

	cmd.AddCommand(newConfigShowCmd(), newConfigCheckCmd())
	return cmd
}

// resolvedConfig is the resolved configuration as `config show` renders it:
// what soroforge.yaml, the environment, and the global flags together produce.
//
// It is a separate view rather than config.Config so that the JSON shape is
// explicit and stable, and so that Env's secret-bearing struct is never
// marshalled directly. The environment section comes from Env.Redacted.
type resolvedConfig struct {
	Path           string                      `json:"config_path"`
	DefaultNetwork string                      `json:"default_network"`
	Networks       map[string]resolvedNetwork  `json:"networks"`
	Contracts      map[string]resolvedContract `json:"contracts"`
	Environment    map[string]string           `json:"environment"`
}

type resolvedNetwork struct {
	RPCURL       string `json:"rpc_url"`
	Passphrase   string `json:"passphrase"`
	SoroVaultURL string `json:"sorovault_url,omitempty"`
}

type resolvedContract struct {
	// Wasm is the path as written in soroforge.yaml.
	Wasm string `json:"wasm"`

	// ResolvedWasm is the absolute path SoroForge would read, with relative
	// paths resolved against the config file's directory.
	ResolvedWasm string `json:"resolved_wasm"`

	UpgradeFunc string `json:"upgrade_fn"`

	// Salt is omitted when unset: an absent salt means a random one per
	// deploy, which `config show` reports by omission.
	Salt string `json:"salt,omitempty"`
}

func newConfigShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Print the configuration SoroForge resolved",
		Long: `Print the configuration SoroForge resolved for this project.

The output is the merge of soroforge.yaml, SOROFORGE_* environment variables,
and the global flags, so it answers "where would a deploy actually go?" without
deploying anything. Contract WASM paths are shown resolved to absolute paths,
and secrets are reported only as (set) or (unset) — never by value.

Unlike most commands, this needs neither Postgres nor a signing key.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(flags.configPath)
			if err != nil {
				return err
			}
			return printResolvedConfig(cfg)
		},
	}
}

// buildResolvedConfig converts a loaded Config into its printable view.
func buildResolvedConfig(cfg *config.Config) resolvedConfig {
	view := resolvedConfig{
		Path:           cfg.Path(),
		DefaultNetwork: cfg.DefaultNetwork,
		Networks:       make(map[string]resolvedNetwork, len(cfg.Networks)),
		Contracts:      make(map[string]resolvedContract, len(cfg.Contracts)),
		Environment:    cfg.Env.Redacted(),
	}

	for name, network := range cfg.Networks {
		view.Networks[name] = resolvedNetwork{
			RPCURL:       network.RPCURL,
			Passphrase:   network.Passphrase,
			SoroVaultURL: network.SoroVaultURL,
		}
	}
	for alias, contract := range cfg.Contracts {
		view.Contracts[alias] = resolvedContract{
			Wasm:         contract.Wasm,
			ResolvedWasm: cfg.WasmPath(contract),
			UpgradeFunc:  contract.UpgradeFuncOrDefault(),
			Salt:         contract.Salt,
		}
	}

	return view
}

func printResolvedConfig(cfg *config.Config) error {
	view := buildResolvedConfig(cfg)

	if flags.jsonOutput {
		return printJSON(view)
	}
	return printResolvedConfigText(out(), view)
}

func printResolvedConfigText(w io.Writer, view resolvedConfig) error {
	fmt.Fprintf(w, "Configuration %s\n\n", view.Path)
	fmt.Fprintf(w, "Default network   %s\n", view.DefaultNetwork)

	fmt.Fprintf(w, "\nNetworks (%d):\n", len(view.Networks))
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  NAME\tRPC URL\tSOROVAULT")
	for _, name := range sortedMapKeys(view.Networks) {
		n := view.Networks[name]
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", name, n.RPCURL, orDash(n.SoroVaultURL))
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	fmt.Fprintf(w, "\nContracts (%d):\n", len(view.Contracts))
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  ALIAS\tWASM\tUPGRADE FN")
	for _, alias := range sortedMapKeys(view.Contracts) {
		c := view.Contracts[alias]
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", alias, c.ResolvedWasm, c.UpgradeFunc)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	fmt.Fprintf(w, "\nEnvironment:\n")
	tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, name := range sortedMapKeys(view.Environment) {
		fmt.Fprintf(tw, "  %s\t%s\n", name, orUnset(view.Environment[name]))
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	return nil
}

func sortedMapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// orUnset renders an empty environment value, such as an unset keystore path,
// the same way Redacted renders an unset secret.
func orUnset(s string) string {
	if s == "" {
		return "(unset)"
	}
	return s
}
