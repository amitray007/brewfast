package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/amitray007/brewfast/internal/accel"
	"github.com/amitray007/brewfast/internal/brew"
	"github.com/amitray007/brewfast/internal/handoff"
	"github.com/amitray007/brewfast/internal/host"
	"github.com/amitray007/brewfast/internal/resolve"
	"github.com/spf13/cobra"
)

// deps is the set of injected collaborators the install pipeline calls. Every
// field is a function value defaulting (in realDeps) to the real package
// behavior, so runInstall can be exercised end-to-end with fakes — no brew,
// aria2, or network required.
type deps struct {
	caskInfo     func(string) (*brew.Cask, error)
	cachePath    func(string) (string, error)
	isInstalled  func(string) bool
	installAria2 func() error
	isSlowHost   func(string) bool
	resolve      func(string, resolve.Options) (string, *brew.Cask, error)
	fetch        func(accel.Params) error
	handoff      func(ctx context.Context, op brew.Operation, name string) error

	stdout io.Writer
	stderr io.Writer
}

// exitError carries an explicit process exit code alongside a message, so the
// pipeline can signal "stop non-zero" vs "stop clean" without the caller having
// to re-classify the error. Execute unwraps it via exitCodeFor.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

// stopf builds a non-zero exit error with a formatted message.
func stopf(code int, format string, a ...any) *exitError {
	return &exitError{code: code, err: fmt.Errorf(format, a...)}
}

// cleanExit is a sentinel exitError with code 0: stop the run with no error
// output (e.g. the user deliberately cancelled the picker).
var cleanExit = &exitError{code: 0, err: errors.New("")}

// exitCodeFor returns the process exit code an error implies: the carried code
// for an *exitError, brew's own code for a handoff error, else 1.
func exitCodeFor(err error) int {
	if err == nil {
		return 0
	}
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.code
	}
	// A handoff (brew child) error carries brew's exit status.
	return handoff.ExitCode(err)
}

// realDeps wires the pipeline to the real foundation packages, capturing the
// command's stdout/stderr so tests of the cobra layer can still redirect I/O.
func realDeps(cmd *cobra.Command) deps {
	return deps{
		caskInfo:    brew.CaskInfo,
		cachePath:   brew.CachePath,
		isInstalled: brew.IsInstalled,
		installAria2: func() error {
			c := exec.Command("brew", "install", "aria2")
			c.Stdout = os.Stderr
			c.Stderr = os.Stderr
			c.Stdin = os.Stdin
			return c.Run()
		},
		isSlowHost: host.IsSlowHost,
		resolve:    resolve.Resolve,
		fetch:      accel.Downloader{}.Fetch,
		handoff: func(ctx context.Context, op brew.Operation, name string) error {
			return handoff.New().Run(ctx, op, name)
		},
		stdout: cmd.OutOrStdout(),
		stderr: cmd.ErrOrStderr(),
	}
}

// runInstall is the F1/F4/F5 pipeline. It takes injected deps and the parsed
// posture, and returns an error carrying exit intent (see exitError / handoff
// errors). A nil return is a successful accelerated install.
//
// The branch structure mirrors R5–R11 and R19/R26:
//  1. resolve the name,
//  2. read the cask (url + sha256),
//  3. host gate (slow-path? else --any-host / --fallback / stop),
//  4. ensure aria2 (R18),
//  5. fetch + verify (checksum mismatch always fatal; no-checksum posture),
//  6. no-verify warning,
//  7. re-read cache path (KTD-2b) and hand off with HOMEBREW_NO_AUTO_UPDATE=1,
//  8. success line.
func runInstall(ctx context.Context, d deps, flags postureFlags, name string) error {
	if ctx == nil {
		ctx = context.Background()
	}

	// --force is the "attempt the fast path even where the default would refuse"
	// posture: it implies both --any-host (accelerate an already-fast host) and
	// --no-verify (tolerate a *missing* checksum) for the refuse-by-default gates
	// below. It does NOT rescue a genuine checksum MISMATCH — that stays fatal.
	anyHost := flags.anyHost || flags.force
	noVerify := flags.noVerify || flags.force

	// 1. Resolve the (possibly inexact) name. On the exact-match path resolve
	// already fetched the cask definition and hands it back, so we skip a
	// second brew info for the same cask.
	resolved, cask, err := d.resolve(name, resolve.Options{NoInput: flags.noInput})
	if err != nil {
		switch {
		case errors.Is(err, resolve.ErrCancelled):
			// Deliberate cancel: clean exit, no error noise.
			return cleanExit
		case errors.Is(err, resolve.ErrNoInteractiveResolve):
			// Candidates already printed to stderr; exit non-zero without dumping
			// a second message.
			return &exitError{code: 1, err: errors.New("no exact cask match; see candidates above")}
		case errors.Is(err, resolve.ErrNoCandidates):
			return stopf(1, "no cask matches %q, and no near matches were found", name)
		default:
			return stopf(1, "resolving %q: %v", name, err)
		}
	}

	// 2. Read the cask definition (url, sha256) — only when resolve did not
	// already supply it (the picker path returns a nil cask).
	if cask == nil {
		cask, err = d.caskInfo(resolved)
		if err != nil {
			if errors.Is(err, brew.ErrCaskNotFound) {
				return stopf(1, "cask %q not found", resolved)
			}
			return stopf(1, "reading cask info for %q: %v", resolved, err)
		}
	}

	// 3. Host gate.
	slow := d.isSlowHost(cask.URL)
	if !slow {
		switch {
		case flags.fallback:
			// Non-slow host + --fallback: hand off to plain brew (F4 fallback).
			fmt.Fprintf(d.stderr, "brewfast: %s is already CDN-fast; handing off to plain brew (--fallback).\n", resolved)
			return d.handoff(ctx, handoffOp, resolved)
		case anyHost:
			// --any-host (or --force) override: accelerate anyway.
			fmt.Fprintf(d.stderr, "brewfast: %s is not a recognized slow host; accelerating anyway (--any-host).\n", resolved)
		default:
			// Default stop with the R19 first-impression framing.
			return stopf(1, "%s", firstImpressionHost(resolved, cask.URL))
		}
	}

	// 4. Ensure aria2 is present (R18 install half).
	if !d.isInstalled("aria2") {
		fmt.Fprintln(d.stderr, "brewfast: aria2 is required for accelerated downloads; installing it via brew...")
		if err := d.installAria2(); err != nil {
			return stopf(1, "brewfast: could not install aria2 automatically (%v)\n"+
				"install it yourself and retry:\n    brew install aria2", err)
		}
	}

	// 5. Resolve the cache path and fetch + verify.
	cachePath, err := d.cachePath(resolved)
	if err != nil {
		return stopf(1, "resolving brew cache path for %q: %v", resolved, err)
	}

	// Under --no-verify, we accept a cask that declares no checksum. We pass the
	// expected sha through unchanged; a genuine mismatch is ALWAYS fatal
	// (below), --no-verify only tolerates a *missing* checksum.
	fetchErr := d.fetch(accel.Params{
		URL:         cask.URL,
		CachePath:   cachePath,
		ExpectedSHA: cask.SHA256,
	})
	if fetchErr != nil {
		switch {
		case errors.Is(fetchErr, accel.ErrChecksumMismatch):
			// ALWAYS fatal — even under --fallback (R6/F5). Wrap the sentinel so
			// callers can still classify it while carrying a non-zero exit.
			return &exitError{code: 1, err: fmt.Errorf(
				"brewfast: checksum mismatch for %s — the downloaded file does not match the cask's declared sha256; discarded and NOT installed: %w",
				resolved, fetchErr)}
		case errors.Is(fetchErr, accel.ErrInsecureURL):
			return stopf(1, "brewfast: refusing to download %s over a non-https URL (%s)", resolved, cask.URL)
		case errors.Is(fetchErr, accel.ErrNoChecksum):
			switch {
			case noVerify:
				// Proceed unverified: fall through to the no-verify warning + handoff.
			case flags.fallback:
				fmt.Fprintf(d.stderr, "brewfast: %s declares no checksum; handing off to plain brew (--fallback).\n", resolved)
				return d.handoff(ctx, handoffOp, resolved)
			default:
				return stopf(1, "%s", firstImpressionNoChecksum(resolved))
			}
		default:
			return stopf(1, "brewfast: accelerated download of %s failed: %v", resolved, fetchErr)
		}
	}

	// 6. Verification genuinely did not happen only on the no-checksum path that
	// --no-verify/--force tolerated. A successful checksum match (fetchErr == nil)
	// means the bytes WERE verified, so it is never "unverified" — regardless of
	// the flag. Emit the unmissable warning only for the real no-checksum case.
	unverified := errors.Is(fetchErr, accel.ErrNoChecksum)
	if unverified {
		fmt.Fprintf(d.stderr,
			"\n!! WARNING: installing %s WITHOUT checksum verification (--no-verify).\n"+
				"!! url:  %s\n"+
				"!! The downloaded bytes were NOT checked against the cask's sha256.\n\n",
			resolved, cask.URL)
	}

	// 7. Hand off. Re-read the cache path immediately before handoff (KTD-2b) so
	// a tap refresh cannot move the canonical path out from under the placed
	// file, and pin HOMEBREW_NO_AUTO_UPDATE=1 for the child.
	if fresh, err := d.cachePath(resolved); err == nil && fresh != cachePath {
		return stopf(1, "brewfast: brew's cache path for %s moved between download and handoff (%s -> %s); aborting to avoid installing a stale file", resolved, cachePath, fresh)
	}
	os.Setenv("HOMEBREW_NO_AUTO_UPDATE", "1")

	start := time.Now()
	if err := d.handoff(ctx, handoffOp, resolved); err != nil {
		// Propagate brew's exit status verbatim (no extra wrapping message so the
		// exit code maps cleanly).
		return err
	}

	// 8. Concise success line (suppressed under --quiet or a non-TTY stdout).
	if !flags.quiet && isTTY(d.stdout) {
		note := ""
		if unverified {
			note = " (installed unverified)"
		}
		fmt.Fprintf(d.stdout, "brewfast: %s installed via the fast path in %s%s\n",
			resolved, elapsed(start), note)
	}
	return nil
}

// handoffOp is the brew operation used for every accelerated handoff.
// `brew reinstall --cask` installs the cask definition's current version from
// the freshly cached file whether or not the cask is already present, so it
// covers both first install and upgrade. (A cask token is never on $PATH, so a
// LookPath-based installed check would be both wrong and wasted work here.)
const handoffOp = brew.OpReinstall

// firstImpressionHost is the R19 message for the already-fast-host default stop:
// it frames the non-acceleration as expected, not a failure, and names the two
// override flags.
func firstImpressionHost(name, url string) string {
	return fmt.Sprintf(
		"brewfast only accelerates casks served from throttled GitHub release assets.\n"+
			"%s downloads from %s, which is already CDN-fast — so there is nothing to\n"+
			"accelerate here. This is expected, not a failure.\n\n"+
			"  • run plain brew instead:   brewfast %s --fallback\n"+
			"  • force acceleration anyway: brewfast %s --any-host",
		name, url, name, name)
}

// firstImpressionNoChecksum is the R19-style message for the no-checksum default
// stop.
func firstImpressionNoChecksum(name string) string {
	return fmt.Sprintf(
		"%s declares no checksum, so brewfast cannot verify the accelerated download.\n"+
			"By default brewfast will not install an unverified file. This is expected,\n"+
			"not a failure.\n\n"+
			"  • accept the risk and accelerate: brewfast %s --no-verify\n"+
			"  • let plain brew handle it:        brewfast %s --fallback",
		name, name, name)
}

// elapsed renders a download/install duration compactly.
func elapsed(start time.Time) string {
	d := time.Since(start).Round(time.Millisecond)
	if d < time.Second {
		return d.String()
	}
	return d.Round(100 * time.Millisecond).String()
}

// isTTY reports whether w is a terminal (an *os.File on a tty). A non-file
// writer (e.g. a test buffer or a pipe) is treated as non-interactive so the
// success line is suppressed for scriptable consumers.
func isTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
