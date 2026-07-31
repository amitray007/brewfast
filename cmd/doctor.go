package cmd

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/amitray007/brewfast/internal/brew"
	"github.com/spf13/cobra"
)

// hashPrefixRe matches brew's canonical download prefix: a 64-hex-char content
// hash followed by "--". Files carrying this prefix are brew's own canonical
// cache entries and must never be swept as orphans (R25).
var hashPrefixRe = regexp.MustCompile(`^[0-9a-f]{64}--`)

// stuckThreshold is the minimum age of a staged version directory before doctor
// will classify a receipt-less, artifact-less cask as *stuck* rather than an
// install in progress. During a normal install brew stages the version dir and
// only later writes the receipt / copies the app, so all three "stuck" signals
// are transiently true. Requiring the version dir to be older than this gate
// keeps doctor from false-positiving (and offering a destructive recovery for)
// a healthy in-flight install.
const stuckThreshold = 10 * time.Minute

// newDoctorCmd builds the `brewfast doctor` subcommand: a read-only diagnostics
// pass that reports environment health, detects stuck cask transactions (R24),
// and lists orphaned cache files (R25). Every potential mutation is *offered*
// (printed as a recommended command), never run — honoring the read-only-by-
// default posture.
func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Report environment health and detect stuck cask transactions",
		Long: "doctor checks that brewfast's dependencies are in place, detects a cask\n" +
			"transaction wedged by an interrupted install (a version directory present\n" +
			"without its install receipt), and lists orphaned download files left in\n" +
			"brew's cache. It never mutates anything: recoveries are printed as commands\n" +
			"you can run yourself.",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDoctor(cmd.OutOrStdout())
		},
	}
}

// runDoctor drives the three doctor sections against the real machine. The
// exec-dependent parts (locating brew's prefix/cache, tool presence) are thin;
// the tested logic lives in findStuckCasks and findOrphans.
func runDoctor(out io.Writer) error {
	reportHealth(out)

	prefix := brewPrefix()
	if prefix == "" {
		fmt.Fprintln(out, "\nSkipping transaction/cache checks: could not resolve `brew --prefix`.")
		return nil
	}

	reportStuck(out, filepath.Join(prefix, "Caskroom"), time.Now())
	reportOrphans(out, filepath.Join(brewCache(), "downloads"))
	return nil
}

// reportHealth prints the ✓/✗ health lines: brew present, aria2 present, stdin a
// TTY, and (best-effort) the tap reachable.
func reportHealth(out io.Writer) {
	fmt.Fprintln(out, "Health")
	line(out, brew.IsInstalled("brew"), "brew present", "brew not found on PATH — install Homebrew")
	line(out, brew.IsInstalled("aria2"), "aria2 present", "aria2 not found — brewfast installs it on first use")
	line(out, isTTY(os.Stdin), "stdin is a TTY (interactive prompts available)", "stdin is not a TTY (running non-interactively)")
	line(out, tapReachable(), "tap reachable", "tap not reachable (network or brew unavailable) — best-effort check")
}

// reportStuck runs the R24 detection and offers the mechanical recovery. now is
// injected (the caller passes time.Now()) so the staleness gate is deterministic
// under test.
func reportStuck(out io.Writer, caskroomDir string, now time.Time) {
	fmt.Fprintln(out, "\nStuck transactions")
	stuck, err := findStuckCasks(caskroomDir, now)
	if err != nil {
		fmt.Fprintf(out, "  ✗ could not scan %s: %v\n", caskroomDir, err)
		return
	}
	if len(stuck) == 0 {
		fmt.Fprintln(out, "  ✓ no stuck cask transactions detected")
		return
	}
	fmt.Fprintf(out, "  ✗ %d stuck cask transaction(s) detected:\n", len(stuck))
	for _, tok := range stuck {
		fmt.Fprintf(out, "      • %s\n", tok)
	}
	fmt.Fprintln(out, "  Recommended recovery (review, then run yourself — brewfast will NOT do this for you):")
	fmt.Fprintln(out, "  First try the non-destructive reinstall (reinstalls from cache, keeps nothing to lose):")
	for _, tok := range stuck {
		fmt.Fprintf(out, "      brew reinstall --cask %s\n", tok)
	}
	fmt.Fprintln(out, "  Only if that does NOT clear it, remove the wedged version directory and force a reinstall:")
	for _, tok := range stuck {
		fmt.Fprintf(out, "      rm -rf %q && brew reinstall --force --cask %s\n",
			filepath.Join(caskroomDir, tok), tok)
	}
}

// reportOrphans runs the R25 sweep and offers removal.
func reportOrphans(out io.Writer, downloadsDir string) {
	fmt.Fprintln(out, "\nOrphaned cache files")
	known := knownCanonicalNames()
	orphans, err := findOrphans(downloadsDir, known)
	if err != nil {
		fmt.Fprintf(out, "  ✗ could not scan %s: %v\n", downloadsDir, err)
		return
	}
	if len(orphans) == 0 {
		fmt.Fprintln(out, "  ✓ no orphaned download files detected")
		return
	}
	fmt.Fprintf(out, "  ✗ %d orphaned download file(s) detected:\n", len(orphans))
	for _, name := range orphans {
		fmt.Fprintf(out, "      • %s\n", name)
	}
	fmt.Fprintln(out, "  Recommended cleanup (review, then run yourself — brewfast will NOT do this for you):")
	for _, name := range orphans {
		fmt.Fprintf(out, "      rm %q\n", filepath.Join(downloadsDir, name))
	}
}

// findStuckCasks walks a Caskroom root and returns the tokens whose transaction
// is stuck per R24: a `<token>/<version>/` version directory exists AND
// `<token>/.metadata/INSTALL_RECEIPT.json` is ABSENT AND the installed artifact
// is missing/empty AND the version directory has not been modified within
// stuckThreshold of now. The receipt lives at the TOKEN level under `.metadata/`,
// not inside the version dir — checking the wrong location would flag every
// healthy cask. The staleness gate matters because those first three signals are
// ALSO transiently true during a normal in-progress install (brew stages the
// version dir before writing the receipt / copying the app); a recently-touched
// version dir is presumed live, not wedged. now is injected so the gate is
// deterministic under test. This is a pure function over the passed-in root so it
// can be tested with a temp fixture and no real brew.
func findStuckCasks(caskroomDir string, now time.Time) ([]string, error) {
	entries, err := os.ReadDir(caskroomDir)
	if err != nil {
		if os.IsNotExist(err) {
			// No Caskroom means nothing installed — not an error, just no stuck casks.
			return nil, nil
		}
		return nil, err
	}

	var stuck []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		token := e.Name()
		tokenDir := filepath.Join(caskroomDir, token)

		versionDir := hasVersionDir(tokenDir)
		if versionDir == "" {
			// No version directory: nothing was ever staged here — not stuck.
			continue
		}

		// Receipt at TOKEN level under .metadata/ — its presence means a completed
		// install, so NOT stuck.
		receipt := filepath.Join(tokenDir, ".metadata", "INSTALL_RECEIPT.json")
		if fileExists(receipt) {
			continue
		}

		// Version dir present, no receipt, and the staged artifact is missing/empty.
		// But this is also exactly what a normal in-progress install looks like, so
		// only classify as stuck once the version dir has gone stale: a recently
		// modified dir is presumed to be a live install, not a wedged one.
		if artifactMissingOrEmpty(versionDir) && versionDirStale(versionDir, now) {
			stuck = append(stuck, token)
		}
	}
	sort.Strings(stuck)
	return stuck, nil
}

// hasVersionDir returns the path of the first real version subdirectory of a
// token directory (any subdirectory other than the `.metadata` sidecar), or ""
// if none exists.
func hasVersionDir(tokenDir string) string {
	entries, err := os.ReadDir(tokenDir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == ".metadata" {
			continue
		}
		return filepath.Join(tokenDir, e.Name())
	}
	return ""
}

// versionDirStale reports whether versionDir was last modified more than
// stuckThreshold before now. A dir we cannot stat is treated as stale (its
// mtime is unknowable, and the artifact-empty signal already fired). A
// recently-modified dir is presumed to be an install in progress, not wedged.
func versionDirStale(versionDir string, now time.Time) bool {
	info, err := os.Stat(versionDir)
	if err != nil {
		return true
	}
	return now.Sub(info.ModTime()) > stuckThreshold
}

// artifactMissingOrEmpty reports whether a version directory has no non-empty
// installed artifact staged in it. An empty version dir, or one containing only
// zero-byte files, counts as a wedged (incomplete) transaction.
func artifactMissingOrEmpty(versionDir string) bool {
	entries, err := os.ReadDir(versionDir)
	if err != nil {
		return true
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		// Any non-empty file, or any subdirectory (e.g. a staged .app bundle),
		// counts as a present artifact.
		if e.IsDir() || info.Size() > 0 {
			return false
		}
	}
	return true
}

// findOrphans classifies entries in a downloads dir into removable orphans per
// R25. It returns, sorted, the names of: (a) regular files whose name lacks the
// `[0-9a-f]{64}--` hash prefix and are not a current canonical name for any
// known cask, plus (b) stray `.aria2` control sidecars. It EXCLUDES symlinks
// entirely — brew maintains `<name>--<version>.<ext>` symlinks, and deleting one
// removes brew state — checked via os.Lstat / ModeSymlink. This is pure over the
// passed-in dir + known-canonical set so it can be tested with a temp fixture.
func findOrphans(downloadsDir string, knownCanonical map[string]bool) ([]string, error) {
	entries, err := os.ReadDir(downloadsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var orphans []string
	for _, e := range entries {
		name := e.Name()

		// Lstat (not Stat) so a symlink is reported as a symlink, not its target.
		info, err := os.Lstat(filepath.Join(downloadsDir, name))
		if err != nil {
			continue
		}
		// Skip symlinks unconditionally: these are brew's version-named links.
		if info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		// Only regular files are sweep candidates (skip dirs, devices, etc.).
		if !info.Mode().IsRegular() {
			continue
		}

		// A stray .aria2 control sidecar is always an orphan.
		if strings.HasSuffix(name, ".aria2") {
			orphans = append(orphans, name)
			continue
		}
		// Brew's own canonical downloads carry the 64-hex hash prefix — leave them.
		if hashPrefixRe.MatchString(name) {
			continue
		}
		// A file that is the current canonical name for a known cask is not stray.
		if knownCanonical[name] {
			continue
		}
		orphans = append(orphans, name)
	}
	sort.Strings(orphans)
	return orphans, nil
}

// --- thin exec-dependent helpers (not unit-tested; the pure functions above are) ---

// brewPrefix resolves `brew --prefix`, returning "" if brew is unavailable.
func brewPrefix() string {
	out, err := exec.Command("brew", "--prefix").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// brewCache resolves `brew --cache`, returning "" if brew is unavailable.
func brewCache() string {
	out, err := exec.Command("brew", "--cache").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// knownCanonicalNames returns the set of current canonical download basenames
// for locally installed casks (best-effort; empty if brew is unavailable). It is
// used only to avoid false-positiving a genuine canonical file in the orphan
// sweep — a miss merely lists an extra candidate the user reviews before acting.
func knownCanonicalNames() map[string]bool {
	known := map[string]bool{}
	out, err := exec.Command("brew", "list", "--cask").Output()
	if err != nil {
		return known
	}
	for _, tok := range strings.Fields(string(out)) {
		p, err := brew.CachePath(tok)
		if err != nil {
			continue
		}
		known[filepath.Base(p)] = true
	}
	return known
}

// tapReachable is a best-effort probe that the brewfast tap responds. It never
// blocks the rest of doctor: any failure is reported as a soft ✗.
func tapReachable() bool {
	if !brew.IsInstalled("brew") {
		return false
	}
	return exec.Command("brew", "tap").Run() == nil
}

// line prints a ✓/✗ health line given a boolean and the two message variants.
func line(out io.Writer, ok bool, okMsg, badMsg string) {
	if ok {
		fmt.Fprintf(out, "  ✓ %s\n", okMsg)
		return
	}
	fmt.Fprintf(out, "  ✗ %s\n", badMsg)
}

// fileExists reports whether path names an existing (regular or dir) entry.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
