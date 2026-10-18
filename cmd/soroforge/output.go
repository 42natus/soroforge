package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/soroworks/soroforge/internal/deploy"
	"github.com/soroworks/soroforge/internal/netcheck"
	"github.com/soroworks/soroforge/internal/store"
)

// Output goes to stdout; logs go to stderr. That split is what lets
// `soroforge list --json | jq` work while logging is still on.
//
// Nothing printed here includes key material. Results carry the deployer's
// public address and never the seed that produced it.

func out() io.Writer { return os.Stdout }

// printJSON writes an indented JSON document.
func printJSON(v any) error {
	enc := json.NewEncoder(out())
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// shortHash abbreviates a 64-character hash for table output. The full value is
// always available with --json, so the table optimises for scanning.
func shortHash(hash string) string {
	const shown = 12
	if len(hash) <= shown {
		return hash
	}
	return hash[:shown] + "…"
}

func printDeployResult(r *deploy.DeployResult) error {
	if flags.jsonOutput {
		return printJSON(r)
	}

	w := out()
	if r.DryRun {
		fmt.Fprintf(w, "Dry run — nothing was submitted and nothing was recorded.\n\n")
	} else {
		fmt.Fprintf(w, "Deployed %s to %s.\n\n", r.Alias, r.Network)
	}

	fmt.Fprintf(w, "  Contract ID   %s\n", r.ContractID)
	if r.ContractURL != "" {
		fmt.Fprintf(w, "  Explorer      %s\n", r.ContractURL)
	}
	fmt.Fprintf(w, "  WASM hash     %s\n", r.WasmHash)
	fmt.Fprintf(w, "  Network       %s\n", r.Network)
	fmt.Fprintf(w, "  Deployer      %s\n", r.Deployer)
	if r.ConstructorArgs != "" {
		fmt.Fprintf(w, "  Constructor   %s\n", r.ConstructorArgs)
	}
	printWarnings(w, r.Warnings)

	if r.UploadSkipped {
		fmt.Fprintf(w, "  Upload        skipped (this bytecode is already on-chain)\n")
	} else if r.UploadTxHash != "" {
		fmt.Fprintf(w, "  Upload tx     %s\n", r.UploadTxHash)
		if r.UploadTxURL != "" {
			fmt.Fprintf(w, "  Upload URL    %s\n", r.UploadTxURL)
		}
	}
	if r.CreateTxHash != "" {
		fmt.Fprintf(w, "  Create tx     %s\n", r.CreateTxHash)
		if r.CreateTxURL != "" {
			fmt.Fprintf(w, "  Create URL    %s\n", r.CreateTxURL)
		}
	}
	if r.InitTxHash != "" {
		fmt.Fprintf(w, "  Init tx       %s\n", r.InitTxHash)
	}
	if r.Ledger != 0 {
		fmt.Fprintf(w, "  Ledger        %d\n", r.Ledger)
	}
	if r.Deployment != nil && r.Deployment.CIRunURL != "" {
		fmt.Fprintf(w, "  CI run        %s\n", r.Deployment.CIRunURL)
	}
	printCatalog(w, r.Catalog)
	printWebhooks(w, r.Webhooks)

	if r.DryRun {
		printEstimatedFees(w, r.EstimatedFees)

		fmt.Fprintf(w, "\nAssembled transaction envelopes (unsigned):\n")
		if r.UploadEnvelopeXDR != "" {
			fmt.Fprintf(w, "\n  upload:\n  %s\n", r.UploadEnvelopeXDR)
		}
		if r.CreateEnvelopeXDR != "" {
			fmt.Fprintf(w, "\n  create:\n  %s\n", r.CreateEnvelopeXDR)
		}
		if r.InitEnvelopeXDR != "" {
			fmt.Fprintf(w, "\n  init:\n  %s\n", r.InitEnvelopeXDR)
		}
		fmt.Fprintf(w, "\nRe-run without --dry-run to submit.\n")
	}

	return nil
}

func printUpgradeResult(r *deploy.UpgradeResult) error {
	if flags.jsonOutput {
		return printJSON(r)
	}

	w := out()

	if r.NoChange {
		fmt.Fprintf(w, "%s on %s already runs this WASM hash — nothing to do.\n\n",
			r.Alias, r.Network)
		fmt.Fprintf(w, "  Contract ID   %s\n", r.ContractID)
		if r.ContractURL != "" {
			fmt.Fprintf(w, "  Explorer      %s\n", r.ContractURL)
		}
		fmt.Fprintf(w, "  WASM hash     %s\n", r.WasmHash)
		return nil
	}

	if r.DryRun {
		fmt.Fprintf(w, "Dry run — nothing was submitted and nothing was recorded.\n\n")
	} else {
		fmt.Fprintf(w, "Upgraded %s on %s.\n\n", r.Alias, r.Network)
	}

	fmt.Fprintf(w, "  Contract ID   %s\n", r.ContractID)
	if r.ContractURL != "" {
		fmt.Fprintf(w, "  Explorer      %s\n", r.ContractURL)
	}
	fmt.Fprintf(w, "  From          %s\n", r.PreviousWasmHash)
	fmt.Fprintf(w, "  To            %s\n", r.WasmHash)
	fmt.Fprintf(w, "  Entrypoint    %s\n", r.UpgradeFunc)
	fmt.Fprintf(w, "  Deployer      %s\n", r.Deployer)
	printWarnings(w, r.Warnings)

	if r.UploadSkipped {
		fmt.Fprintf(w, "  Upload        skipped (this bytecode is already on-chain)\n")
	} else if r.UploadTxHash != "" {
		fmt.Fprintf(w, "  Upload tx     %s\n", r.UploadTxHash)
		if r.UploadTxURL != "" {
			fmt.Fprintf(w, "  Upload URL    %s\n", r.UploadTxURL)
		}
	}
	if r.UpgradeTxHash != "" {
		fmt.Fprintf(w, "  Upgrade tx    %s\n", r.UpgradeTxHash)
		if r.UpgradeTxURL != "" {
			fmt.Fprintf(w, "  Upgrade URL   %s\n", r.UpgradeTxURL)
		}
	}
	if r.Ledger != 0 {
		fmt.Fprintf(w, "  Ledger        %d\n", r.Ledger)
	}
	if r.Deployment != nil && r.Deployment.CIRunURL != "" {
		fmt.Fprintf(w, "  CI run        %s\n", r.Deployment.CIRunURL)
	}
	printCatalog(w, r.Catalog)
	printWebhooks(w, r.Webhooks)

	if r.DryRun {
		printEstimatedFees(w, r.EstimatedFees)

		if r.UpgradeEnvelopeXDR != "" {
			fmt.Fprintf(w, "\nAssembled upgrade envelope (unsigned):\n\n  %s\n", r.UpgradeEnvelopeXDR)
			fmt.Fprintf(w, "\nRe-run without --dry-run to submit.\n")
		}
	}

	return nil
}

// printEstimatedFees renders a dry run's per-transaction and total cost. The
// figures come from simulation, so they are labelled as estimates rather than
// promises: the network can charge differently by the time a real deploy runs.
func printEstimatedFees(w io.Writer, f *deploy.DryRunFees) {
	if f == nil {
		return
	}

	fmt.Fprintf(w, "\nEstimated fees (from simulation; the actual fee may differ):\n")
	for _, row := range []struct {
		label string
		fee   *deploy.FeeEstimate
	}{
		{"upload", f.Upload},
		{"create", f.Create},
		{"upgrade", f.Upgrade},
		{"init", f.Init},
	} {
		if row.fee == nil {
			continue
		}
		fmt.Fprintf(w, "  %-8s %d stroops (%s XLM)\n", row.label, row.fee.TotalStroops, row.fee.TotalXLM)
	}
	fmt.Fprintf(w, "  %-8s %d stroops (%s XLM)\n", "total", f.Total.TotalStroops, f.Total.TotalXLM)
}

// stderrIsTerminal reports whether stderr is a character device. Only then is
// in-place progress output safe; pipes, files, and CI logs are not.
func stderrIsTerminal() bool {
	info, err := os.Stderr.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// progressFor returns a progress printer when progress should be shown: always
// with --progress, and otherwise only when stderr is a terminal. It returns nil
// when progress is off, which leaves the service silent.
func progressFor(force bool) deploy.ProgressFunc {
	interactive := stderrIsTerminal()
	if !force && !interactive {
		return nil
	}
	return progressPrinter(os.Stderr, interactive)
}

// progressPrinter renders deploy progress to w. On a terminal each event
// rewrites a single line; elsewhere each event is its own line, which is what
// a CI log wants.
func progressPrinter(w io.Writer, interactive bool) deploy.ProgressFunc {
	if !interactive {
		return func(ev deploy.ProgressEvent) {
			fmt.Fprintln(w, progressLine(ev))
		}
	}
	return func(ev deploy.ProgressEvent) {
		line := progressLine(ev)
		if ev.Phase == deploy.ProgressConfirmed {
			fmt.Fprintf(w, "\r\033[K%s\n", line)
			return
		}
		fmt.Fprintf(w, "\r\033[K%s", line)
	}
}

// progressLine renders one event as a single line of text.
func progressLine(ev deploy.ProgressEvent) string {
	prefix := fmt.Sprintf("%s on %s: %s", ev.Alias, ev.Network, ev.Step)
	switch ev.Phase {
	case deploy.ProgressSimulating:
		return prefix + ": simulating…"
	case deploy.ProgressSubmitted:
		return fmt.Sprintf("%s: submitted (%s)", prefix, shortHash(ev.Hash))
	case deploy.ProgressWaiting:
		return fmt.Sprintf("%s: waiting for ledger %d…", prefix, ev.Ledger)
	case deploy.ProgressConfirmed:
		return fmt.Sprintf("%s: confirmed in ledger %d", prefix, ev.Ledger)
	default:
		return prefix + ": " + ev.Phase
	}
}

func printUploadResult(r *deploy.UploadResult) error {
	if flags.jsonOutput {
		return printJSON(r)
	}

	w := out()

	if r.UploadSkipped {
		fmt.Fprintf(w, "%s's bytecode is already on-chain on %s — nothing to upload.\n\n",
			r.Alias, r.Network)
		fmt.Fprintf(w, "  WASM hash     %s\n", r.WasmHash)
		fmt.Fprintf(w, "  Deployer      %s\n", r.Deployer)
		return nil
	}

	if r.DryRun {
		fmt.Fprintf(w, "Dry run — nothing was submitted and nothing was recorded.\n\n")
	} else {
		fmt.Fprintf(w, "Uploaded %s's bytecode to %s.\n\n", r.Alias, r.Network)
	}

	fmt.Fprintf(w, "  WASM hash     %s\n", r.WasmHash)
	fmt.Fprintf(w, "  Network       %s\n", r.Network)
	fmt.Fprintf(w, "  Deployer      %s\n", r.Deployer)
	if r.MinResourceFee != 0 {
		fmt.Fprintf(w, "  Resource fee  %d stroops\n", r.MinResourceFee)
	}
	if r.UploadTxHash != "" {
		fmt.Fprintf(w, "  Upload tx     %s\n", r.UploadTxHash)
	}
	if r.Ledger != 0 {
		fmt.Fprintf(w, "  Ledger        %d\n", r.Ledger)
	}
	if r.Deployment != nil && r.Deployment.CIRunURL != "" {
		fmt.Fprintf(w, "  CI run        %s\n", r.Deployment.CIRunURL)
	}

	if r.DryRun && r.UploadEnvelopeXDR != "" {
		fmt.Fprintf(w, "\nAssembled upload envelope (unsigned):\n\n  %s\n", r.UploadEnvelopeXDR)
		fmt.Fprintf(w, "\nRe-run without --dry-run to submit.\n")
	}

	return nil
}

func printInvokeResult(r *deploy.InvokeResult) error {
	if flags.jsonOutput {
		return printJSON(r)
	}

	w := out()

	if r.DryRun {
		fmt.Fprintf(w, "Dry run — nothing was submitted and nothing was recorded.\n\n")
	} else {
		fmt.Fprintf(w, "Invoked %s on %s (%s).\n\n", r.Function, r.Alias, r.Network)
	}

	fmt.Fprintf(w, "  Contract ID   %s\n", r.ContractID)
	fmt.Fprintf(w, "  Function      %s\n", r.Function)
	if len(r.Args) > 0 {
		fmt.Fprintf(w, "  Arguments     %s\n", strings.Join(r.Args, " "))
	}
	fmt.Fprintf(w, "  Deployer      %s\n", r.Deployer)
	if r.TxHash != "" {
		fmt.Fprintf(w, "  Tx hash       %s\n", r.TxHash)
	}
	if r.Ledger != 0 {
		fmt.Fprintf(w, "  Ledger        %d\n", r.Ledger)
	}
	if r.Deployment != nil && r.Deployment.CIRunURL != "" {
		fmt.Fprintf(w, "  CI run        %s\n", r.Deployment.CIRunURL)
	}

	if r.DryRun && r.EnvelopeXDR != "" {
		fmt.Fprintf(w, "\nAssembled invocation envelope (unsigned):\n\n  %s\n", r.EnvelopeXDR)
		fmt.Fprintf(w, "\nRe-run without --dry-run to submit.\n")
	}

	return nil
}

// printWarnings reports non-fatal artifact problems found before upload. They
// go to stdout with the result because they are part of it; --json carries the
// same text in the result's warnings field.
func printWarnings(w io.Writer, warnings []string) {
	for _, warning := range warnings {
		fmt.Fprintf(w, "  Warning       %s\n", warning)
	}
}

// printCatalog reports SoroVault registration. A failure is shown as a
// warning, not an error: the deploy itself succeeded and is recorded.
func printCatalog(w io.Writer, c *deploy.CatalogStatus) {
	if c == nil {
		return
	}
	if !c.OK {
		fmt.Fprintf(w, "  SoroVault     not registered: %s\n", c.Error)
		fmt.Fprintf(w, "                (the deploy succeeded; run `sorovault add` to catch up)\n")
		return
	}
	fmt.Fprintf(w, "  SoroVault     %s (%d functions)\n", c.Entry.URL, c.Entry.Functions)
}

// printDeployAll reports one batch deploy. It always prints every result —
// including a failure — because the summary is how the caller learns what
// succeeded before the run stopped.
func printDeployAll(results []deploy.DeployAllResult) error {
	if flags.jsonOutput {
		return printJSON(results)
	}

	w := out()
	if len(results) == 0 {
		fmt.Fprintln(w, "No contracts were deployed.")
		return nil
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ALIAS\tSTATUS\tCONTRACT ID\tWASM HASH\tDETAIL")
	for _, r := range results {
		contractID, wasmHash, detail := "-", "-", ""
		if r.Result != nil {
			contractID = r.Result.ContractID
			wasmHash = shortHash(r.Result.WasmHash)
			if r.Result.DryRun {
				detail = "nothing submitted"
			} else if r.Result.UploadSkipped {
				detail = "upload skipped"
			}
		}
		switch r.Status {
		case deploy.DeployAllSkipped:
			detail = "already tracked"
		case deploy.DeployAllFailed:
			detail = r.Error
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.Alias, r.Status, contractID, wasmHash, detail)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	counts := map[string]int{}
	for _, r := range results {
		counts[r.Status]++
	}
	fmt.Fprintf(w, "\n%d deployed, %d simulated, %d skipped, %d failed.\n",
		counts[deploy.DeployAllDeployed],
		counts[deploy.DeployAllDryRun],
		counts[deploy.DeployAllSkipped],
		counts[deploy.DeployAllFailed])
	if last := results[len(results)-1]; last.Status == deploy.DeployAllFailed {
		fmt.Fprintf(w, "Stopped at %s; nothing after it was attempted.\n", last.Alias)
	}
	return nil
}

func printWebhooks(w io.Writer, webhooks []deploy.WebhookStatus) {
	for _, wh := range webhooks {
		if !wh.OK {
			fmt.Fprintf(w, "  Webhook       failed (%s): %s\n", wh.URL, wh.Error)
		} else {
			fmt.Fprintf(w, "  Webhook       delivered to %s\n", wh.URL)
		}
	}
}

func printContracts(contracts []store.Contract) error {
	if flags.jsonOutput {
		return printJSON(map[string]any{"contracts": contracts})
	}

	if len(contracts) == 0 {
		fmt.Fprintln(out(), "No contracts tracked yet. Run `soroforge deploy <alias>` to add one.")
		return nil
	}

	tw := tabwriter.NewWriter(out(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NETWORK\tALIAS\tCONTRACT ID\tWASM HASH\tDEPLOYED")
	for _, c := range contracts {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			c.Network, c.Alias, c.ContractID, shortHash(c.CurrentWasmHash), formatTime(c.UpdatedAt))
	}
	return tw.Flush()
}

func printHistory(alias, network string, deployments []store.Deployment) error {
	if flags.jsonOutput {
		return printJSON(map[string]any{
			"alias":       alias,
			"network":     network,
			"deployments": deployments,
		})
	}

	if len(deployments) == 0 {
		fmt.Fprintf(out(), "No history for %s on %s.\n", alias, network)
		return nil
	}

	fmt.Fprintf(out(), "History for %s on %s (newest first):\n\n", alias, network)

	hasExplorerLinks := false
	for _, d := range deployments {
		if d.ContractURL != "" || d.TxURL != "" {
			hasExplorerLinks = true
			break
		}
	}
	tw := tabwriter.NewWriter(out(), 0, 0, 2, ' ', 0)
	header := "WHEN\tACTION\tWASM HASH\tDEPLOYER\tLEDGER\tFEE (STROOPS)\tFEE (XLM)\tCOMMIT\tTX\tCI RUN"
	if hasExplorerLinks {
		header += "\tCONTRACT URL\tTX URL"
	}
	fmt.Fprintln(tw, header+"\tNOTES")
	for _, d := range deployments {
		feeStroops, feeXLM := "-", "-"
		if d.FeeCharged != nil {
			feeStroops = fmt.Sprintf("%d", *d.FeeCharged)
			feeXLM = fmt.Sprintf("%d.%07d", *d.FeeCharged/10_000_000, *d.FeeCharged%10_000_000)
		}
		commit := "-"
		if d.GitCommit != "" {
			commit = shortHash(d.GitCommit)
			if d.Dirty {
				commit += "*"
			}
		}
		ciRun := "-"
		if d.CIRunURL != "" {
			ciRun = d.CIRunURL
		}
		notes := d.Notes
		if d.Action == store.ActionInvoke && d.FnName != "" {
			inv := d.FnName
			if d.FnArgs != "" {
				inv += "(" + d.FnArgs + ")"
			} else {
				inv += "()"
			}
			if notes != "" {
				notes = inv + " - " + notes
			} else {
				notes = inv
			}
		}
		cols := []string{
			formatTime(d.CreatedAt),
			string(d.Action),
			shortHash(d.WasmHash),
			shortHash(d.DeployerPubkey),
			formatLedger(d.Ledger),
			feeStroops,
			feeXLM,
			commit,
			shortHash(d.TxHash),
			ciRun,
		}
		if hasExplorerLinks {
			cols = append(cols, d.ContractURL, d.TxURL)
		}
		cols = append(cols, truncate(notes, 40))
		fmt.Fprintln(tw, strings.Join(cols, "\t"))
	}
	return tw.Flush()
}

func printStatus(r *deploy.StatusResult) error {
	if flags.jsonOutput {
		return printJSON(r)
	}

	w := out()
	fmt.Fprintf(w, "%s\n\n", r.Detail)

	fmt.Fprintf(w, "  Alias         %s\n", r.Alias)
	fmt.Fprintf(w, "  Network       %s\n", r.Network)
	if r.ContractID != "" {
		fmt.Fprintf(w, "  Contract ID   %s\n", r.ContractID)
	}
	fmt.Fprintf(w, "  State         %s\n", r.State)
	if r.ExpectedWasmHash != "" {
		fmt.Fprintf(w, "  Recorded      %s\n", r.ExpectedWasmHash)
	}
	if r.ActualWasmHash != "" {
		fmt.Fprintf(w, "  On-chain      %s\n", r.ActualWasmHash)
	}

	return nil
}

func printNetworkStatus(r *deploy.NetworkStatus) error {
	if flags.jsonOutput {
		return printJSON(r)
	}

	w := out()
	if len(r.Contracts) == 0 {
		fmt.Fprintf(w, "No contracts tracked on %s.\n", r.Network)
		return nil
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ALIAS\tSTATE\tCONTRACT ID\tRECORDED\tON-CHAIN")
	for _, c := range r.Contracts {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			c.Alias, c.State, c.ContractID, shortHash(c.ExpectedWasmHash), shortHash(c.ActualWasmHash))
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	synced := 0
	for _, c := range r.Contracts {
		if c.InSync() {
			synced++
		}
	}
	fmt.Fprintf(w, "\n%d of %d contract(s) on %s in sync.\n", synced, len(r.Contracts), r.Network)
	return nil
}

// printNetworkResults renders the `soroforge networks` report. The RPC URL is
// always the redacted one from the probe result — the raw URL can carry
// provider credentials in userinfo or the query string, and nothing here may
// leak them, including in the --json payload a user is encouraged to paste.
func printNetworkResults(results []netcheck.Result) error {
	if flags.jsonOutput {
		return printJSON(map[string]any{
			"networks": results,
			"all_ok":   !netcheck.AnyFailure(results),
		})
	}

	w := out()
	if len(results) == 0 {
		fmt.Fprintln(w, "No networks are configured.")
		return nil
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NETWORK\tRESPONDED\tLATENCY\tLATEST LEDGER\tPASSPHRASE\tRPC")
	for _, r := range results {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			r.Network,
			yesNo(r.Responded),
			formatLatency(r.Latency),
			formatLedger(r.LatestLedger),
			passphraseVerdict(r),
			r.RedactedURL,
		)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	for _, r := range results {
		if !r.OK {
			fmt.Fprintf(w, "\n%s: %s\n", r.Network, r.Error)
		}
	}

	healthy := 0
	for _, r := range results {
		if r.OK {
			healthy++
		}
	}
	fmt.Fprintf(w, "\n%d of %d network(s) healthy.\n", healthy, len(results))
	return nil
}

func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

// passphraseVerdict distinguishes "the endpoint confirmed the network" from
// "the endpoint never answered getNetwork": an unverified passphrase is not a
// mismatch, it is unknown, and the report must not imply otherwise.
func passphraseVerdict(r netcheck.Result) string {
	switch {
	case !r.PassphraseVerified:
		return "not verified"
	case r.PassphraseMatch:
		return "match"
	default:
		return "MISMATCH"
	}
}

func formatLatency(d time.Duration) string {
	if d <= 0 {
		return "-"
	}
	return d.Round(time.Millisecond).String()
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format("2006-01-02 15:04 UTC")
}

func formatLedger(ledger uint32) string {
	if ledger == 0 {
		return "-"
	}
	return fmt.Sprintf("%d", ledger)
}

func truncate(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if s == "" {
		return "-"
	}
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}
