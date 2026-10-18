package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/soroworks/soroforge/internal/config"
	"github.com/soroworks/soroforge/internal/deploy"
	"github.com/spf13/cobra"
)

const wasmMagic = "\x00asm"

func newConfigCheckCmd() *cobra.Command {
	var offline bool
	checkCmd := &cobra.Command{
		Use:   "check",
		Short: "Validate configuration, WASM artifacts, and RPC networks",
		Long: `Check the loaded configuration and every configured WASM artifact.

By default, each network's RPC endpoint is queried and its reported network
passphrase is compared with the configured passphrase. Use --offline to skip
the network checks.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(flags.configPath)
			if err != nil {
				return err
			}
			if err := checkConfig(cmd.Context(), cfg, offline, deploy.DefaultClientFactory); err != nil {
				return err
			}
			if offline {
				fmt.Fprintln(out(), "Configuration and WASM artifacts are valid (offline; RPC checks skipped).")
			} else {
				fmt.Fprintln(out(), "Configuration, WASM artifacts, and RPC networks are valid.")
			}
			return nil
		},
	}
	checkCmd.Flags().BoolVar(&offline, "offline", false, "skip RPC network checks")
	return checkCmd
}

func checkConfig(
	ctx context.Context,
	cfg *config.Config,
	offline bool,
	factory deploy.ClientFactory,
) error {
	var problems []string

	contracts := make([]string, 0, len(cfg.Contracts))
	for alias := range cfg.Contracts {
		contracts = append(contracts, alias)
	}
	sort.Strings(contracts)
	for _, alias := range contracts {
		contract := cfg.Contracts[alias]
		path := cfg.WasmPath(contract)
		wasm, err := os.ReadFile(path)
		if err != nil {
			problems = append(problems, fmt.Sprintf("contracts.%s.wasm: read %s: %v", alias, path, err))
			continue
		}
		if len(wasm) < len(wasmMagic) || string(wasm[:len(wasmMagic)]) != wasmMagic {
			problems = append(problems, fmt.Sprintf(
				"contracts.%s.wasm: %s does not start with the WebAssembly magic bytes", alias, path))
		}
	}

	if !offline {
		if factory == nil {
			factory = deploy.DefaultClientFactory
		}
		networks := make([]string, 0, len(cfg.Networks))
		for name := range cfg.Networks {
			networks = append(networks, name)
		}
		sort.Strings(networks)
		for _, name := range networks {
			network := cfg.Networks[name]
			client, err := factory(name, network)
			if err != nil {
				problems = append(problems, fmt.Sprintf("networks.%s.rpc_url: connect: %v", name, err))
				continue
			}
			if client == nil {
				problems = append(problems, fmt.Sprintf("networks.%s.rpc_url: client factory returned nil", name))
				continue
			}

			passphrase, err := client.NetworkInfo(ctx)
			if err != nil {
				problems = append(problems, fmt.Sprintf("networks.%s.rpc_url: get network info: %v", name, err))
			} else if passphrase != network.Passphrase {
				problems = append(problems, fmt.Sprintf(
					"networks.%s.passphrase: RPC reports %q, configured value is %q",
					name, passphrase, network.Passphrase))
			}

			if closer, ok := client.(io.Closer); ok {
				if err := closer.Close(); err != nil {
					problems = append(problems, fmt.Sprintf("networks.%s.rpc_url: close client: %v", name, err))
				}
			}
		}
	}

	if len(problems) == 0 {
		return nil
	}
	return errors.New("configuration check failed:\n  - " + strings.Join(problems, "\n  - "))
}
