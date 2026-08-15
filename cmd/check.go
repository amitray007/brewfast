package cmd

import (
	"errors"
	"fmt"
	"io"

	"github.com/amitray007/brewfast/internal/brew"
	"github.com/amitray007/brewfast/internal/host"
	"github.com/spf13/cobra"
)

// checkDeps holds the injected collaborators for the dry-run applicability
// report. Both fields default (in realCheckDeps) to the real foundation
// packages. Note there is deliberately NO fetch/handoff/download dependency
// here: `check` is read-only by construction (R27) — it cannot install or
// download because the machinery to do so is not even wired in.
type checkDeps struct {
	caskInfo   func(string) (*brew.Cask, error)
	isSlowHost func(string) bool
	// update refreshes tap metadata so the report describes the CURRENT cask
	// definition. It refreshes brew's own metadata checkout; it still installs and
	// downloads nothing, so `check` remains read-only with respect to the user's
	// installed software (R27).
	update func() error
	// stderr receives the soft warning when a metadata refresh fails.
	stderr io.Writer
}

// realCheckDeps wires the report to the real brew adapter and host classifier.
func realCheckDeps(stderr io.Writer) checkDeps {
	return checkDeps{
		caskInfo:   brew.CaskInfo,
		isSlowHost: host.IsSlowHost,
		update:     brew.Update,
		stderr:     stderr,
	}
}

// checkCategory is the classification of a cask's acceleration applicability. It
// is the pure result of (url, sha256) so the branch logic is testable without
// running brew or touching the network.
type checkCategory int

const (
	// categoryAccelerable: slow GitHub-asset host AND a checksum is present.
	categoryAccelerable checkCategory = iota
	// categoryAlreadyFast: the download host is already CDN-fast.
	categoryAlreadyFast
	// categoryNoChecksum: slow host, but the cask declares no checksum.
	categoryNoChecksum
)

// classifyCheck is the pure classifier: given a cask's url and sha256, decide
// which applicability category applies. The already-fast host wins first (R19
// framing: nothing to accelerate regardless of checksum); on a slow host, a
// missing checksum downgrades to the checksum-caveat category.
func classifyCheck(url, sha256 string, isSlowHost func(string) bool) checkCategory {
	if !isSlowHost(url) {
		return categoryAlreadyFast
	}
	if sha256 == "" {
		return categoryNoChecksum
	}
	return categoryAccelerable
}

// newCheckCmd builds the `brewfast check <cask>` subcommand: a read-only report
// of whether brewfast can accelerate a cask, without installing or downloading
// anything (R27).
func newCheckCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "check <cask>",
		Short: "Report whether brewfast can accelerate a cask (no install)",
		Long: "check inspects a cask and reports whether brewfast can accelerate its\n" +
			"download — a throttled GitHub release asset with a checksum — or whether it\n" +
			"is already CDN-fast (nothing to accelerate) or declares no checksum. It is\n" +
			"strictly read-only: it never downloads or installs anything.",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCheck(cmd.OutOrStdout(), realCheckDeps(cmd.ErrOrStderr()), args[0])
		},
	}
}

// runCheck resolves the cask (exact), classifies it, and prints the report. A
// not-found cask returns a clear non-zero error; every applicable path
// (accelerable / already-fast / no-checksum) is a success (nil error) since
// none is a failure — merely information about applicability.
func runCheck(out io.Writer, d checkDeps, name string) error {
	// Refresh tap metadata first so the report reflects the current cask, not a
	// stale local checkout. Non-fatal: on failure warn and report against local
	// data rather than failing a read-only query.
	if d.update != nil {
		if err := d.update(); err != nil && d.stderr != nil {
			fmt.Fprintf(d.stderr, "brewfast: could not refresh brew metadata (%v); reporting local cask data, which may be out of date.\n", err)
		}
	}

	cask, err := d.caskInfo(name)
	if err != nil {
		if errors.Is(err, brew.ErrCaskNotFound) {
			return stopf(1, "cask %q not found", name)
		}
		return stopf(1, "reading cask info for %q: %v", name, err)
	}

	switch classifyCheck(cask.URL, cask.SHA256, d.isSlowHost) {
	case categoryAccelerable:
		fmt.Fprintf(out,
			"✓ brewfast can accelerate %s.\n"+
				"  Its download is a throttled GitHub release asset (%s) and a checksum is\n"+
				"  present, so brewfast can fetch it in parallel and verify it.\n\n"+
				"  run:  brewfast %s\n",
			cask.Token, cask.URL, cask.Token)
	case categoryAlreadyFast:
		fmt.Fprintf(out,
			"%s is already CDN-fast; brewfast won't help here.\n"+
				"  Its download comes from %s, which is not a throttled GitHub release\n"+
				"  asset — so there is nothing to accelerate. This is expected, not a failure.\n\n"+
				"  install with plain brew:     brew install --cask %s\n"+
				"  force acceleration anyway:   brewfast %s --any-host\n",
			cask.Token, cask.URL, cask.Token, cask.Token)
	case categoryNoChecksum:
		fmt.Fprintf(out,
			"%s is served from a throttled GitHub release asset, but declares no checksum.\n"+
				"  brewfast could download it in parallel, but cannot verify the result, so by\n"+
				"  default it will not install an unverified file. This is expected, not a failure.\n\n"+
				"  accept the risk and accelerate: brewfast %s --no-verify\n",
			cask.Token, cask.Token)
	}
	return nil
}
