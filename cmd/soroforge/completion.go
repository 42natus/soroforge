package main

import (
	"log/slog"
	"sort"

	"github.com/soroworks/soroforge/internal/config"
	"github.com/spf13/cobra"
)

// Shell completion for aliases and networks.
//
// Two rules govern everything in this file, because a completion runs on
// every keystroke in an interactive shell:
//
//   - It touches nothing remote. No RPC calls, no database: only
//     soroforge.yaml, loaded through the same path rules as normal commands
//     (the --config flag, then $SOROFORGE_CONFIG, then ./soroforge.yaml).
//     A slow or unreachable dependency must never stall the shell.
//
//   - It fails quietly. A missing or invalid config produces no suggestions
//     and no error output — the user still gets their prompt. An error
//     printed mid-tab-completion is noise that makes people disable
//     completions entirely.

// completionConfig loads soroforge.yaml for completion, returning nil when it
// cannot be loaded. Callers treat nil as "no suggestions".
func completionConfig() *config.Config {
	cfg, err := config.Load(flags.configPath)
	if err != nil {
		return nil
	}
	return cfg
}

// aliasCompletion suggests the contract aliases configured in soroforge.yaml
// for commands whose positional argument is an alias. Cobra's shell code does
// the prefix filtering; we return everything in sorted order.
func aliasCompletion(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	cfg := completionConfig()
	if cfg == nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	names := make([]string, 0, len(cfg.Contracts))
	for alias := range cfg.Contracts {
		names = append(names, alias)
	}
	sort.Strings(names)
	return names, cobra.ShellCompDirectiveNoFileComp
}

// networkFlagCompletion suggests the network names configured in
// soroforge.yaml as values for --network.
func networkFlagCompletion(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	cfg := completionConfig()
	if cfg == nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	names := make([]string, 0, len(cfg.Networks))
	for name := range cfg.Networks {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, cobra.ShellCompDirectiveNoFileComp
}

// attachAliasCompletion wires alias completion onto a command whose first
// positional argument is a contract alias.
func attachAliasCompletion(cmd *cobra.Command) {
	cmd.ValidArgsFunction = aliasCompletion
}

// registerNetworkFlagCompletion binds --network value completion to the root
// command's persistent flag. The cobra registry keys on the flag object
// itself, which subcommands share through inheritance, so one registration
// covers every command.
func registerNetworkFlagCompletion(root *cobra.Command) {
	// A duplicate registration would fail; there is exactly one call site
	// (newRootCmd), so this cannot error in practice — but check rather
	// than panic on a future refactor.
	if err := root.RegisterFlagCompletionFunc("network", networkFlagCompletion); err != nil {
		// There is exactly one call site, so a failure here is a programming
		// error, not a runtime condition; warn rather than crash the CLI.
		slog.Warn("could not register --network completion", "error", err)
	}
}
