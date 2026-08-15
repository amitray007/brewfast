// Package brew is the single adapter wrapping every `brew` interaction brewfast
// needs: reading a cask's url/sha256/version/token, resolving the canonical
// download cache path, listing near-name search candidates, detecting tool
// presence, and running the install/upgrade handoff.
//
// This package is the foundation seam other units build against. Its exported
// API is intended to be stable and clean. It contains no business logic — it is
// a thin, well-typed adapter over the `brew` CLI.
//
// Security posture: every cask name that reaches an exec call is first validated
// against Homebrew's token grammar (see ValidateName), and every invocation uses
// explicit argument slices with a `--` terminator before any user-derived value.
// brewfast never builds a shell string, so there is no shell-injection surface,
// and a leading-dash name can never be parsed as a flag by brew or a downstream
// tool.
package brew

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// brewCmdTimeout bounds every network-touching brew invocation (info/search/
// cache resolution). `brew info --json=v2` and `brew search` refresh taps and
// thus touch the network; without a ceiling a hung upstream would block the
// whole install pipeline forever. 30s is generous for a healthy tap refresh yet
// fails fast when brew is wedged.
const brewCmdTimeout = 30 * time.Second

// ErrCaskNotFound is returned when a queried cask does not exist. Callers can
// test for it with errors.Is.
var ErrCaskNotFound = errors.New("cask not found")

// ErrInvalidName is the sentinel wrapped by every ValidateName rejection.
// Callers can test for it with errors.Is; the wrapped error carries a
// human-readable reason.
var ErrInvalidName = errors.New("invalid cask name")

// Cask holds the fields brewfast reads from `brew info --json=v2 --cask`.
// Only the fields brewfast uses are modeled; the JSON has many more.
type Cask struct {
	Token   string
	Version string
	URL     string
	SHA256  string
}

// nameGrammar matches Homebrew's cask-token grammar: lowercase alphanumerics
// plus `-`, `@`, `+`, `.`. Anchored so the whole string must conform. A leading
// dash is therefore rejected, which stops a name like `--version` from being
// parsed as a flag by a downstream tool.
var nameGrammar = regexp.MustCompile(`^[a-z0-9@+.-]+$`)

// ValidateName checks name against Homebrew's cask-token grammar and returns a
// clear error (wrapping ErrInvalidName) for anything that does not conform. It
// MUST be called before name is used in any exec invocation. It rejects the
// empty string, uppercase, whitespace, shell metacharacters, and — critically —
// a leading dash.
func ValidateName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: name is empty", ErrInvalidName)
	}
	if strings.HasPrefix(name, "-") {
		return fmt.Errorf("%w: %q starts with a dash and would be read as a flag", ErrInvalidName, name)
	}
	if !nameGrammar.MatchString(name) {
		return fmt.Errorf("%w: %q contains characters outside Homebrew's token grammar (lowercase alphanumerics, - @ + .)", ErrInvalidName, name)
	}
	return nil
}

// brewInfoV2 mirrors the fields brewfast reads from Homebrew's shared
// `brew info --json=v2` response envelope. Cask metadata drives acceleration;
// formula metadata is intentionally limited to exact-match detection.
type brewInfoV2 struct {
	Casks []struct {
		Token   string `json:"token"`
		Version string `json:"version"`
		URL     string `json:"url"`
		SHA256  string `json:"sha256"`
	} `json:"casks"`
	Formulae []struct {
		Name string `json:"name"`
	} `json:"formulae"`
}

// parseCaskInfo is the pure JSON-parsing core of CaskInfo, split out so it is
// unit-testable without a real `brew` on the machine. It returns ErrCaskNotFound
// when the document contains no casks.
func parseCaskInfo(data []byte) (*Cask, error) {
	var doc brewInfoV2
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parsing brew info json: %w", err)
	}
	if len(doc.Casks) == 0 {
		return nil, ErrCaskNotFound
	}
	c := doc.Casks[0]
	return &Cask{
		Token:   c.Token,
		Version: c.Version,
		URL:     c.URL,
		SHA256:  c.SHA256,
	}, nil
}

// parseFormulaExists reports whether brew's structured response contains an
// exact formula. It stays separate from parseCaskInfo because formula and cask
// metadata have different installation semantics and must not share a model by
// accident.
func parseFormulaExists(data []byte) (bool, error) {
	var doc brewInfoV2
	if err := json.Unmarshal(data, &doc); err != nil {
		return false, fmt.Errorf("parsing brew formula info json: %w", err)
	}
	return len(doc.Formulae) > 0, nil
}

// parseSearchOutput is the pure parser for `brew search --cask` stdout, split
// out for testing. Because the search is scoped to casks at the brew level,
// EVERY name in stdout is a cask and no section routing is needed here. brew
// also prints "No formulae or casks found" to STDERR while exiting 0, so an
// empty parsed set here means "no candidates" regardless of the process exit
// code.
//
// Section markers must NOT be used to classify names. brew only prints "==>
// Formulae"/"==> Casks" headers when stdout is a TERMINAL; when brewfast reads
// the output through a pipe (always, since it uses cmd.Output), brew emits bare
// names with the two groups separated only by a blank line. A parser that
// tracked sections therefore mistook every formula for a cask on the real,
// piped path — the fix is to scope the search itself with --cask (see
// SearchCandidates) rather than to guess from layout that is not there.
//
// Headers are still tolerated and skipped, so output captured from a terminal
// parses identically. Tap-qualified "owner/tap/name" lines are reduced to their
// bare token so they pass cask-name validation and refer to the cask correctly
// downstream.
func parseSearchOutput(stdout string) []string {
	var candidates []string
	seen := make(map[string]bool)
	add := func(name string) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		candidates = append(candidates, name)
	}

	sc := bufio.NewScanner(strings.NewReader(stdout))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "==>") {
			continue
		}
		if strings.Count(line, "/") >= 2 {
			add(tapToken(line))
			continue
		}
		add(line)
	}
	return candidates
}

// tapToken reduces a tap-qualified "owner/tap/name" reference to its bare
// "name" token. A non-tap-qualified string is returned unchanged.
func tapToken(s string) string {
	if i := strings.LastIndex(s, "/"); i >= 0 {
		return s[i+1:]
	}
	return s
}

// Runner is the handoff surface: it runs a command (e.g. `brew reinstall`) so
// U6 can wrap it with signal handling and its own process group. Keeping the
// actual invocation construction in this package (see HandoffArgs) while running
// it through this interface lets U6 substitute a signal-guarded implementation
// without duplicating the argument-building logic.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) error
}

// ExecRunner is the default Runner. It shells out with os/exec and wires the
// child's stdio to the current process. It performs no signal handling; U6
// supplies that by wrapping the same construction.
type ExecRunner struct {
	// Stdout, Stderr, Stdin, if non-nil, are wired to the child process.
	Stdout, Stderr interface{ Write([]byte) (int, error) }
	Stdin          interface{ Read([]byte) (int, error) }
}

// Run executes name with args under ctx. It does no name validation of its own —
// callers building brew invocations via HandoffArgs/InfoArgs/etc. have already
// validated the user-derived cask name.
func (r ExecRunner) Run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	if r.Stdout != nil {
		cmd.Stdout = r.Stdout
	}
	if r.Stderr != nil {
		cmd.Stderr = r.Stderr
	}
	if r.Stdin != nil {
		cmd.Stdin = r.Stdin
	}
	return cmd.Run()
}

// Operation selects the brew subcommand for a handoff.
type Operation string

const (
	// OpReinstall maps to `brew reinstall --cask`.
	OpReinstall Operation = "reinstall"
	// OpUpgrade maps to `brew upgrade --cask`.
	OpUpgrade Operation = "upgrade"
)

// HandoffArgs builds the explicit argument slice for a brew cask handoff, with a
// `--` terminator before the user-derived name. name must have passed
// ValidateName. The returned slice is the args to pass to a Runner alongside the
// "brew" binary name — e.g. runner.Run(ctx, "brew", HandoffArgs(op, name)...).
//
// Building the args here (rather than in U6) keeps the brew invocation shape in
// the one adapter that owns brew knowledge, while U6 owns only how the process
// is supervised.
func HandoffArgs(op Operation, name string) []string {
	return []string{string(op), "--cask", "--", name}
}

// CaskInfo runs `brew info --json=v2 --cask -- <name>` and parses the result
// into a Cask. A name that does not exist yields ErrCaskNotFound. The name is
// validated before any exec.
func CaskInfo(name string) (*Cask, error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), brewCmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "brew", "info", "--json=v2", "--cask", "--", name)
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("brew info for %q timed out after %s", name, brewCmdTimeout)
		}
		// brew exits non-zero for an unknown cask; disambiguate from a real
		// failure by trying to parse whatever it emitted. An unknown cask
		// prints no valid casks document, so parseCaskInfo returns
		// ErrCaskNotFound and we surface a not-found rather than an exec error.
		if len(out) > 0 {
			if c, perr := parseCaskInfo(out); perr == nil {
				return c, nil
			}
		}
		return nil, fmt.Errorf("%w: %q (brew info failed: %v)", ErrCaskNotFound, name, err)
	}
	return parseCaskInfo(out)
}

// FormulaExists reports whether name is an exact Homebrew formula. It exists
// solely so cask resolution can identify the user's input correctly before
// offering fuzzy cask candidates; it does not opt formulae into acceleration.
func FormulaExists(name string) (bool, error) {
	if err := ValidateName(name); err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), brewCmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "brew", "info", "--json=v2", "--formula", "--", name)
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return false, fmt.Errorf("brew formula info for %q timed out after %s", name, brewCmdTimeout)
		}
		// An unknown formula exits non-zero and emits no JSON. If brew did emit a
		// structured response, honor it. Otherwise only an actual process exit is
		// a clean miss; a failure to start brew must stop resolution rather than
		// silently falling through to fuzzy cask suggestions.
		if len(out) > 0 {
			return parseFormulaExists(out)
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return false, nil
		}
		return false, fmt.Errorf("running brew formula info for %q: %w", name, err)
	}
	return parseFormulaExists(out)
}

// CachePath runs `brew --cache --cask -- <name>` and returns the absolute,
// canonical cache path (trimmed of surrounding whitespace). The name is
// validated before any exec.
func CachePath(name string) (string, error) {
	if err := ValidateName(name); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), brewCmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "brew", "--cache", "--cask", "--", name)
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("brew --cache for %q timed out after %s", name, brewCmdTimeout)
		}
		return "", fmt.Errorf("resolving cache path for %q: %w", name, err)
	}
	path := strings.TrimSpace(string(out))
	if path == "" {
		return "", fmt.Errorf("brew returned an empty cache path for %q", name)
	}
	return path, nil
}

// SearchCandidates runs `brew search --cask -- <name>` and returns cask
// candidates parsed from stdout. Because `brew search` exits 0 even on no match
// (printing its "No formulae or casks found" notice to stderr), an empty parsed
// set is treated as zero candidates rather than an error; a genuine exec failure
// is still surfaced. The name is validated before any exec.
//
// The --cask scope is load-bearing, not cosmetic: brew omits its "==>
// Formulae"/"==> Casks" headers whenever stdout is not a terminal, which is
// always the case here. Without --cask, an unscoped search returns formulae and
// casks as one undifferentiated list and every formula would be offered as a
// cask candidate (e.g. `brewfast t3` suggesting qt3d and ropebwt3). Letting brew
// do the filtering keeps the answer correct regardless of how it formats output.
func SearchCandidates(name string) ([]string, error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), brewCmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "brew", "search", "--cask", "--", name)
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("brew search for %q timed out after %s", name, brewCmdTimeout)
		}
		// A no-match still exits 0, so a non-zero exit is a real failure — but
		// parse whatever stdout we got first: if brew printed candidates before
		// a nonzero exit, honor them.
		if cands := parseSearchOutput(string(out)); len(cands) > 0 {
			return cands, nil
		}
		return nil, fmt.Errorf("searching for %q: %w", name, err)
	}
	return parseSearchOutput(string(out)), nil
}

// updateTimeout bounds `brew update`. A tap refresh does real network I/O across
// every tapped repository, so it needs more headroom than the read-only info and
// search calls, but it must still fail fast rather than stall an install behind a
// wedged remote.
const updateTimeout = 90 * time.Second

// Update runs `brew update --quiet` to refresh tap metadata, so a subsequent
// CaskInfo reads the CURRENT cask definition rather than whatever version was
// last fetched onto this machine.
//
// This matters because brewfast reads a cask's version, url, and sha256 straight
// from the local tap checkout. Homebrew normally hides tap staleness by
// auto-updating inside `brew install`, but brewfast decides whether an install is
// even needed BEFORE it hands off — and then pins HOMEBREW_NO_AUTO_UPDATE=1 for
// the child. Without an explicit refresh, a machine whose tap is behind reports
// the stale version as the latest and brewfast reports "already up to date" for a
// cask that has a newer release upstream.
//
// A refresh failure is returned to the caller but is NOT fatal by design: callers
// treat it as a soft warning and continue against the local metadata, so a
// network blip degrades brewfast to its previous behavior instead of blocking an
// install that would otherwise work.
func Update() error {
	ctx, cancel := context.WithTimeout(context.Background(), updateTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "brew", "update", "--quiet")
	// brew refuses to auto-update inside its own update; keep the child quiet and
	// non-interactive so it can never block on a prompt.
	cmd.Env = append(cmd.Environ(), "HOMEBREW_NO_AUTO_UPDATE=1", "HOMEBREW_NO_INTERACTIVE=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("brew update timed out after %s", updateTimeout)
		}
		return fmt.Errorf("brew update failed: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// IsInstalled reports whether tool (e.g. "brew", "aria2") is resolvable on PATH.
func IsInstalled(tool string) bool {
	_, err := exec.LookPath(tool)
	return err == nil
}

// parseInstalledVersion is the pure parser for `brew list --cask --versions`
// stdout, split out for testing. When the cask is installed, brew prints a
// single line of the form "<name> <version>" (a token may report more than one
// installed version; the first is taken). When it is not installed, brew prints
// nothing (and exits 1). An empty/blank input therefore parses to ("", false):
// not installed. The second whitespace-delimited field is the version.
func parseInstalledVersion(stdout string) (string, bool) {
	line := strings.TrimSpace(stdout)
	if line == "" {
		return "", false
	}
	fields := strings.Fields(line)
	if len(fields) < 2 {
		// A token with no version field is not a usable "installed" signal.
		return "", false
	}
	return fields[1], true
}

// InstalledVersion runs `brew list --cask --versions -- <name>` and reports the
// currently-installed version of the cask, if any. When the cask is installed it
// returns (version, true, nil); when it is not installed it returns ("", false,
// nil) — brew exits 1 with empty output for an uninstalled cask, which is a
// normal "not installed" answer, not an error. A genuine exec failure (e.g. brew
// missing, or a timeout) is returned as a real error. The name is validated
// before any exec.
func InstalledVersion(name string) (string, bool, error) {
	if err := ValidateName(name); err != nil {
		return "", false, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), brewCmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "brew", "list", "--cask", "--versions", "--", name)
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", false, fmt.Errorf("brew list for %q timed out after %s", name, brewCmdTimeout)
		}
		// brew exits non-zero for a cask that is not installed, printing nothing
		// to stdout. Treat that (a clean exit-status error with no output) as a
		// definitive "not installed" answer rather than a failure. Anything that
		// produced parseable output is honored below.
		if v, ok := parseInstalledVersion(string(out)); ok {
			return v, true, nil
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			// Ran, exited non-zero, produced no installed line → not installed.
			return "", false, nil
		}
		// Could not run brew at all (not found, permission, etc.) → real error.
		return "", false, fmt.Errorf("running brew list for %q: %w", name, err)
	}
	v, ok := parseInstalledVersion(string(out))
	return v, ok, nil
}
