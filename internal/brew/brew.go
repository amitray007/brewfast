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
)

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

// caskInfoV2 mirrors the relevant slice of the `brew info --json=v2` document:
// a top-level object with a "casks" array.
type caskInfoV2 struct {
	Casks []struct {
		Token   string `json:"token"`
		Version string `json:"version"`
		URL     string `json:"url"`
		SHA256  string `json:"sha256"`
	} `json:"casks"`
}

// parseCaskInfo is the pure JSON-parsing core of CaskInfo, split out so it is
// unit-testable without a real `brew` on the machine. It returns ErrCaskNotFound
// when the document contains no casks.
func parseCaskInfo(data []byte) (*Cask, error) {
	var doc caskInfoV2
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

// parseSearchOutput is the pure parser for `brew search` stdout, split out for
// testing. brew search prints section markers (e.g. "==> Casks") and, for
// tap-qualified results, "owner/tap/<name>" lines. It also prints "No formulae
// or casks found" to STDERR while exiting 0, so an empty parsed set here means
// "no candidates" regardless of the process exit code.
//
// Candidate selection is deliberately inclusive (U5 refines): names under a
// "==> Casks" section and every tap-qualified "owner/tap/name" line are
// returned; a "==> Formulae" section is skipped. Lines before any section
// marker are treated as casks too, since a cask-only search prints bare names
// with no header.
func parseSearchOutput(stdout string) []string {
	const (
		sectionNone = iota
		sectionCasks
		sectionFormulae
	)
	// Start in "casks" so bare cask names printed with no header are captured.
	section := sectionCasks

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
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "==>") {
			header := strings.ToLower(line)
			switch {
			case strings.Contains(header, "cask"):
				section = sectionCasks
			case strings.Contains(header, "formula"):
				section = sectionFormulae
			default:
				section = sectionNone
			}
			continue
		}
		// A tap-qualified "owner/tap/name" line is a cask candidate regardless
		// of the current section.
		if strings.Count(line, "/") >= 2 {
			add(line)
			continue
		}
		if section == sectionCasks {
			add(line)
		}
	}
	return candidates
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
	cmd := exec.Command("brew", "info", "--json=v2", "--cask", "--", name)
	out, err := cmd.Output()
	if err != nil {
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

// CachePath runs `brew --cache --cask -- <name>` and returns the absolute,
// canonical cache path (trimmed of surrounding whitespace). The name is
// validated before any exec.
func CachePath(name string) (string, error) {
	if err := ValidateName(name); err != nil {
		return "", err
	}
	cmd := exec.Command("brew", "--cache", "--cask", "--", name)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("resolving cache path for %q: %w", name, err)
	}
	path := strings.TrimSpace(string(out))
	if path == "" {
		return "", fmt.Errorf("brew returned an empty cache path for %q", name)
	}
	return path, nil
}

// SearchCandidates runs `brew search -- <name>` and returns cask candidates
// parsed from stdout. Because `brew search` exits 0 even on no match (printing
// its "No formulae or casks found" notice to stderr), an empty parsed set is
// treated as zero candidates rather than an error; a genuine exec failure is
// still surfaced. The name is validated before any exec.
func SearchCandidates(name string) ([]string, error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	cmd := exec.Command("brew", "search", "--", name)
	out, err := cmd.Output()
	if err != nil {
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

// IsInstalled reports whether tool (e.g. "brew", "aria2") is resolvable on PATH.
func IsInstalled(tool string) bool {
	_, err := exec.LookPath(tool)
	return err == nil
}
