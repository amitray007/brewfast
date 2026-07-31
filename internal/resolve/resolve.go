// Package resolve turns a possibly-inexact cask name into a single chosen cask.
//
// An exact cask match always proceeds without prompting, in any environment
// (R4). When the given name has no exact match, resolve gathers near-name
// candidates from brew and branches on the environment:
//
//   - In an interactive terminal (and not forced non-interactive), it presents a
//     Clack-style huh Select picker over the candidates (R12).
//   - With no TTY, or when --yes/--no-input forced it, it prints the candidates
//     to stderr and returns ErrNoInteractiveResolve so the caller exits non-zero.
//     This path NEVER blocks or prompts, so it can never hang a CI job (R13/R14).
//
// The interactive selection sits behind the Selector interface and TTY detection
// is injectable, so the whole routing logic is testable without a real terminal.
// The two brew calls (exact lookup, candidate search) are likewise function
// fields defaulting to the real brew adapter, keeping tests fully offline.
package resolve

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/amitray007/brewfast/internal/brew"
	"github.com/charmbracelet/huh"
	"golang.org/x/term"
)

// ErrNoInteractiveResolve is returned when a name has no exact match and no
// interactive picker can be shown — either there is no TTY or the run was forced
// non-interactive via --yes/--no-input. The caller should exit non-zero. The
// candidate list has already been printed to the configured Stderr. Test for it
// with errors.Is.
var ErrNoInteractiveResolve = errors.New("cannot resolve inexact name without an interactive terminal")

// ErrCancelled is returned when the user aborts the interactive picker (e.g.
// Ctrl-C or Esc). It is distinct from ErrNoInteractiveResolve so the caller can
// exit cleanly on a deliberate cancel. Test for it with errors.Is.
var ErrCancelled = errors.New("selection cancelled")

// ErrNoCandidates is returned when a name has no exact match and the candidate
// search yields nothing to choose from. Test for it with errors.Is.
var ErrNoCandidates = errors.New("no matching cask found")

// Selector performs the interactive choice over a set of candidate cask names.
// The default implementation (HuhSelector) uses a huh Select; tests inject a
// fake to exercise the picker branch without a real terminal. An implementation
// MUST return an error wrapping (or equal to) ErrCancelled when the user aborts.
type Selector interface {
	// Pick shows prompt over options and returns the chosen option. On user
	// abort it returns an error satisfying errors.Is(err, ErrCancelled).
	Pick(prompt string, options []string) (string, error)
}

// Options configures a Resolve call. The zero value is usable: it targets the
// real brew adapter, auto-detects the TTY on os.Stdin, prints candidates to
// os.Stderr, and uses the huh-backed selector.
type Options struct {
	// NoInput forces the non-interactive path even when a TTY is present, taking
	// the R13 print-and-exit branch instead of the picker (from --yes/--no-input).
	NoInput bool

	// Stdin is the file used for TTY detection when IsTTY is nil. Defaults to
	// os.Stdin.
	Stdin *os.File

	// IsTTY, when non-nil, overrides TTY detection entirely (for tests). When
	// nil, resolve detects a terminal on Stdin.
	IsTTY *bool

	// Stderr is where the candidate list is printed on the non-interactive
	// branch. Defaults to os.Stderr.
	Stderr io.Writer

	// Selector performs the interactive pick. Defaults to a HuhSelector.
	Selector Selector

	// lookupExact resolves an exact cask match. Defaults to brew.CaskInfo. A nil
	// return with a nil error is treated as "not found".
	lookupExact func(name string) (*brew.Cask, error)

	// search gathers near-name candidates. Defaults to brew.SearchCandidates.
	search func(name string) ([]string, error)
}

// Resolve turns name into a chosen cask name, and returns the cask definition
// when it already had to fetch one.
//
// It first attempts an exact match via lookupExact (brew.CaskInfo by default);
// on a hit it returns name and the fetched *brew.Cask immediately without
// gathering candidates or invoking the selector (R4) — callers on the common
// exact-match path can reuse that cask instead of re-running brew info.
// Otherwise it gathers candidates via search (brew.SearchCandidates), filters
// them to cask-looking names, and either shows the picker (TTY and not NoInput)
// or prints the candidates and returns ErrNoInteractiveResolve. On the picker
// path the returned cask is nil (the chosen cask's definition was not fetched
// here). Zero candidates yields ErrNoCandidates. A user abort in the picker
// yields ErrCancelled.
func Resolve(name string, opts Options) (string, *brew.Cask, error) {
	opts = opts.withDefaults()

	// Exact match first — proceed with no candidate gathering, no picker (R4).
	cask, err := opts.lookupExact(name)
	if err != nil && !errors.Is(err, brew.ErrCaskNotFound) {
		return "", nil, fmt.Errorf("looking up cask %q: %w", name, err)
	}
	if err == nil && cask != nil {
		return name, cask, nil
	}

	// No exact match — gather near-name candidates.
	candidates, err := opts.search(name)
	if err != nil {
		return "", nil, fmt.Errorf("searching for candidates for %q: %w", name, err)
	}
	candidates = filterCaskCandidates(candidates, name)
	if len(candidates) == 0 {
		return "", nil, fmt.Errorf("%w: %q", ErrNoCandidates, name)
	}

	// Non-interactive path: print candidates and refuse to prompt (R13/R14).
	// This branch MUST NOT block — it never touches the selector.
	if opts.NoInput || !opts.tty() {
		printCandidates(opts.Stderr, name, candidates)
		return "", nil, fmt.Errorf("%w: %q", ErrNoInteractiveResolve, name)
	}

	// Interactive path: show the Clack-style picker. The chosen cask's
	// definition is not fetched here, so the returned cask is nil and the
	// caller fetches it.
	prompt := fmt.Sprintf("No exact cask %q — did you mean:", name)
	chosen, err := opts.Selector.Pick(prompt, candidates)
	if err != nil {
		if errors.Is(err, ErrCancelled) {
			return "", nil, ErrCancelled
		}
		return "", nil, fmt.Errorf("selecting a cask: %w", err)
	}
	return chosen, nil, nil
}

// withDefaults returns a copy of opts with every unset field filled in with its
// real, production default.
func (o Options) withDefaults() Options {
	if o.Stdin == nil {
		o.Stdin = os.Stdin
	}
	if o.Stderr == nil {
		o.Stderr = os.Stderr
	}
	if o.Selector == nil {
		o.Selector = HuhSelector{}
	}
	if o.lookupExact == nil {
		o.lookupExact = brew.CaskInfo
	}
	if o.search == nil {
		o.search = brew.SearchCandidates
	}
	return o
}

// tty reports whether resolve should treat the environment as interactive. An
// explicit IsTTY override wins; otherwise it detects a terminal on Stdin.
func (o Options) tty() bool {
	if o.IsTTY != nil {
		return *o.IsTTY
	}
	if o.Stdin == nil {
		return false
	}
	return term.IsTerminal(int(o.Stdin.Fd()))
}

// filterCaskCandidates drops obvious non-cask names and de-duplicates while
// preserving order. brew.SearchCandidates already scopes to cask sections and
// tap-qualified lines; this is a light second pass. A tap-qualified
// "owner/tap/token" candidate is kept as-is (it is a valid cask reference brew
// accepts). The name itself is never proposed as its own suggestion.
func filterCaskCandidates(candidates []string, name string) []string {
	out := make([]string, 0, len(candidates))
	seen := make(map[string]bool)
	for _, c := range candidates {
		if c == "" || c == name || seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	return out
}

// printCandidates writes the non-interactive candidate list to w in a stable,
// scriptable form.
func printCandidates(w io.Writer, name string, candidates []string) {
	fmt.Fprintf(w, "No exact cask %q. Candidate matches:\n", name)
	for _, c := range candidates {
		fmt.Fprintf(w, "  %s\n", c)
	}
	fmt.Fprintln(w, "Re-run with an exact name (no interactive terminal available).")
}

// HuhSelector is the default Selector. It renders a Clack-style huh Select —
// framed, arrow-key navigable, cancelable — and maps a user abort to
// ErrCancelled.
type HuhSelector struct{}

// Pick shows a huh Select over options and returns the chosen value. A user
// abort (Ctrl-C / Esc) is returned as ErrCancelled.
func (HuhSelector) Pick(prompt string, options []string) (string, error) {
	var chosen string
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title(prompt).
				Options(huh.NewOptions(options...)...).
				Value(&chosen),
		),
	).WithTheme(huh.ThemeCharm())

	if err := form.Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return "", ErrCancelled
		}
		return "", err
	}
	return chosen, nil
}
