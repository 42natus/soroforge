package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/soroworks/soroforge/internal/config"
)

// A deploy or upgrade against a protected network spends real XLM and changes
// a live contract, and it can differ from a testnet run by nothing but a flag
// value. This file is the guard: on a protected network the mutating commands
// print what will happen and require the operator to type the network name.
//
// The prompt must fail closed. Without a terminal on stdin — CI, a pipe, a
// redirected file — the command refuses rather than hanging on a prompt no one
// can answer, and only an exact typed match of the network name is accepted.

// stdinIsTerminal reports whether stdin is a character device. The mirror of
// stderrIsTerminal in output.go: pipes, files, and closed descriptors are not
// confirmation sources.
func stdinIsTerminal() bool {
	info, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// confirmProtected guards one mutating command against w, reading the typed
// confirmation from in.
//
// It returns nil — proceeds without asking — when the network is not
// protected, when dryRun is set (a dry run submits nothing, so there is
// nothing to confirm), or when yes is set, which is the explicit CI opt-out.
// Otherwise it prints what will happen and requires the exact network name;
// anything else, including an unreadable or empty stdin, is a refusal.
func confirmProtected(
	w io.Writer,
	in io.Reader,
	tty bool,
	networkName string,
	network config.Network,
	action, target string,
	yes, dryRun bool,
) error {
	if !network.Protected || dryRun {
		return nil
	}

	if yes {
		// Recorded on stderr so --json on stdout stays parseable, the same
		// rule the progress printer follows.
		fmt.Fprintf(w, "confirmation skipped with --yes: %s %s on protected network %s\n",
			action, target, networkName)
		return nil
	}

	if !tty {
		return fmt.Errorf(
			"refusing to %s %s on protected network %q: stdin is not a terminal, so the "+
				"required confirmation cannot be answered. Run this interactively, or pass "+
				"--yes once you have verified the target network in your configuration",
			action, target, networkName)
	}

	fmt.Fprintf(w, "About to %s %s on %q — a protected network.\n", action, target, networkName)
	fmt.Fprintf(w, "  Passphrase  %s\n", network.Passphrase)
	fmt.Fprintf(w, "This submits real transactions and spends real XLM.\n")
	fmt.Fprintf(w, "Type the network name (%s) to continue: ", networkName)

	// The scanner takes the last line even without a trailing newline, so a
	// typed answer is not silently dropped by an EOF right after it; an empty
	// or closed stdin yields the empty string and is refused below. Piped
	// input never reaches here at all: without a terminal the command has
	// already refused.
	scanner := bufio.NewScanner(in)
	var typed string
	if scanner.Scan() {
		typed = strings.TrimSpace(scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintln(w)
		return fmt.Errorf("confirmation aborted before an answer was read: %w", err)
	}
	if typed != networkName {
		fmt.Fprintln(w)
		return fmt.Errorf(
			"confirmation rejected on protected network %q: expected the network name to be "+
				"typed exactly, aborting with nothing submitted", networkName)
	}
	fmt.Fprintln(w)
	return nil
}
