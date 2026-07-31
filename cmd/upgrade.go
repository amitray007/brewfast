package cmd

import (
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/amitray007/brewfast/internal/brew"
	"github.com/spf13/cobra"
)

// upgradeDeps holds the injected collaborators for the self-update flow. Both
// fields default (in realUpgradeDeps) to the real behavior, so runUpgrade can be
// exercised with fakes — no brew and no network required.
type upgradeDeps struct {
	// isInstalled reports whether a tool is on PATH (used to detect brew absence).
	isInstalled func(string) bool
	// runBrew runs `brew` with the given args and returns its combined output so
	// the already-current case can be detected from what brew printed.
	runBrew func(args ...string) (output string, err error)
}

// realUpgradeDeps wires the flow to real brew via os/exec.
func realUpgradeDeps() upgradeDeps {
	return upgradeDeps{
		isInstalled: brew.IsInstalled,
		runBrew: func(args ...string) (string, error) {
			out, err := exec.Command("brew", args...).CombinedOutput()
			return string(out), err
		},
	}
}

// newUpgradeCmd builds the `brewfast upgrade` subcommand: it self-updates the
// brewfast binary through its Homebrew tap by running `brew update` then
// `brew upgrade brewfast`, reporting the already-current case cleanly rather
// than as an error (KTD-5, R15).
func newUpgradeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "upgrade",
		Short: "Update brewfast itself via its Homebrew tap",
		Long: "upgrade self-updates the brewfast tool through the tap it was installed\n" +
			"from: it runs `brew update` then `brew upgrade brewfast`. When brewfast is\n" +
			"already on the latest version this is reported as success, not an error.",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUpgrade(cmd.OutOrStdout(), realUpgradeDeps())
		},
	}
}

// runUpgrade drives the self-update. It returns a non-nil error only for a real
// failure (brew absent, or `brew upgrade` failing for a reason other than
// "already current"); the already-current case returns nil after reporting it.
func runUpgrade(out io.Writer, d upgradeDeps) error {
	if !d.isInstalled("brew") {
		return fmt.Errorf("brew not found on PATH — brewfast self-updates through Homebrew; install Homebrew first (https://brew.sh)")
	}

	// `brew update` refreshes the tap. A transient update failure should not block
	// the upgrade attempt itself, so its error is reported but not fatal — the
	// subsequent `brew upgrade` is the operation that matters.
	if updateOut, err := d.runBrew("update"); err != nil {
		fmt.Fprintf(out, "brewfast: `brew update` reported a problem (continuing anyway): %v\n%s", err, updateOut)
	}

	upgradeOut, err := d.runBrew("upgrade", "brewfast")
	if isAlreadyCurrent(upgradeOut) {
		fmt.Fprintln(out, "brewfast: already up-to-date; nothing to upgrade.")
		return nil
	}
	if err != nil {
		return fmt.Errorf("brewfast: `brew upgrade brewfast` failed: %v\n%s", err, upgradeOut)
	}

	if trimmed := strings.TrimSpace(upgradeOut); trimmed != "" {
		fmt.Fprintln(out, trimmed)
	}
	fmt.Fprintln(out, "brewfast: upgraded to the latest version.")
	return nil
}

// alreadyCurrentMarkers are the substrings brew prints when a formula is already
// on the latest version. brew's exact phrasing varies across versions, so we
// match on any of the known markers, case-insensitively.
var alreadyCurrentMarkers = []string{
	"already installed",
	"already up-to-date",
	"already up to date",
	"is up-to-date",
	"is up to date",
	"no available upgrade", // "... no available upgrade..." fallback wording
}

// isAlreadyCurrent is a pure classifier over brew's output: it reports whether
// brew indicated brewfast is already on the latest version. Kept pure so the
// three upgrade scenarios (current / upgraded / absent) are testable without
// running real brew.
func isAlreadyCurrent(brewOutput string) bool {
	lower := strings.ToLower(brewOutput)
	for _, m := range alreadyCurrentMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}
