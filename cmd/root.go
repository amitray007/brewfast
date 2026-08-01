// Package cmd wires the brewfast command tree with Cobra: the root command, its
// persistent posture flags, --version/--help, and the default install flow that
// runs when a bare cask name is given. Other subcommands (check, doctor,
// upgrade) are added by their own files via rootCmd.AddCommand, so the tree is
// extensible without editing this file.
package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// version is the brewfast version string. It defaults to "dev" and is intended
// to be overridden at build time via -ldflags "-X ...cmd.version=<v>".
var version = "dev"

// SetVersion overrides the reported version. It exists so a build entry point
// (or a test) can set the version without linker flags.
func SetVersion(v string) {
	if v != "" {
		version = v
	}
}

// postureFlags holds the per-run posture selected on the command line. It is a
// plain value carried into the install pipeline (see runInstall) so the flag
// surface and the flow logic stay decoupled and independently testable.
type postureFlags struct {
	fallback  bool
	anyHost   bool
	noVerify  bool
	force     bool
	noInput   bool
	quiet     bool
	reinstall bool
}

// newRootCmd builds the brewfast root command. It is a constructor (rather than
// a package-level singleton) so tests can build a fresh, isolated command tree
// with its own flag state and output buffers.
func newRootCmd() *cobra.Command {
	var flags postureFlags

	root := &cobra.Command{
		Use:   "brewfast <cask>",
		Short: "Accelerate Homebrew cask installs from throttled GitHub release assets",
		Long: "brewfast accelerates the install or upgrade of a Homebrew cask whose download\n" +
			"comes from a throttled GitHub release asset: it fetches the asset with aria2's\n" +
			"parallel connections, verifies the checksum against the cask definition, places\n" +
			"the file in brew's download cache, then hands off to brew for the real install.\n\n" +
			"brew stays the source of truth; brewfast is a pre-fetch optimizer that never\n" +
			"wedges it. Only slow GitHub-asset casks are accelerated — see --any-host and\n" +
			"--fallback for other cases.",
		Version: version,
		// Exactly one positional arg (the cask name) for the default command.
		Args: cobra.ExactArgs(1),
		// Silence Cobra's own error/usage dump; the install flow prints its own
		// specific messages and we control the exit code via Execute.
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			d := realDeps(cmd)
			return runInstall(cmd.Context(), d, flags, args[0])
		},
	}

	// Persistent flags so subcommands added later inherit the same posture
	// surface where relevant.
	pf := root.PersistentFlags()
	pf.BoolVar(&flags.fallback, "fallback", false, "hand off to plain brew on any non-fatal snag instead of stopping")
	pf.BoolVar(&flags.anyHost, "any-host", false, "accelerate downloads from hosts other than recognized slow GitHub-asset hosts")
	pf.BoolVar(&flags.noVerify, "no-verify", false, "skip checksum verification entirely (accepts the risk)")
	pf.BoolVar(&flags.force, "force", false, "attempt the fast path even in cases the default would refuse")
	pf.BoolVar(&flags.noInput, "yes", false, "never prompt; take the non-interactive path even in a terminal")
	pf.BoolVar(&flags.quiet, "quiet", false, "suppress the success/status line for scriptable consumers")
	pf.BoolVar(&flags.reinstall, "reinstall", false, "reinstall from the accelerated cache even if already up to date")

	// --no-input is an alias for --yes.
	pf.BoolVar(&flags.noInput, "no-input", false, "alias for --yes")

	// Match brew/CLI convention: `brewfast --version` prints just the version.
	root.SetVersionTemplate("brewfast {{.Version}}\n")

	root.AddCommand(newDoctorCmd())
	root.AddCommand(newUpgradeCmd())
	root.AddCommand(newCheckCmd())

	return root
}

// Execute builds the root command and runs it, translating any error into a
// process exit code. The install pipeline returns errors carrying exit intent
// (see exitError); anything else exits 1.
func Execute() {
	root := newRootCmd()
	if err := root.Execute(); err != nil {
		// A clean-exit sentinel (e.g. a deliberate picker cancel) exits 0 with no
		// noise; everything else prints and exits with the carried code.
		if code := exitCodeFor(err); code != 0 {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(code)
		}
		os.Exit(0)
	}
}
