// Package accel performs an accelerated cask download: it drives aria2's
// parallel-connection transfer straight to brew's canonical cache filename
// (no download-then-copy — R25/KTD-7), then verifies the placed file against
// the cask's declared SHA-256. A checksum mismatch is fatal and unrecoverable
// even under --fallback (R6); a missing expected checksum is surfaced as a
// distinct sentinel so the caller (U4) can decide posture rather than passing
// silently.
package accel

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// sha256HexRE matches exactly 64 hex digits — the shape of a lower/upper-hex
// SHA-256 digest. An ExpectedSHA that is non-empty but does not match this can
// never equal a real digest, so it is treated as "no usable checksum" rather
// than an unconditional mismatch.
var sha256HexRE = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// isValidSHA reports whether s (after trimming) is a well-formed 64-hex
// SHA-256 digest. Empty or malformed values are not valid — there is nothing
// to verify against.
func isValidSHA(s string) bool {
	return sha256HexRE.MatchString(strings.TrimSpace(s))
}

// ErrChecksumMismatch is a FATAL sentinel: the downloaded file's SHA-256 did
// not match the cask's declared value. The file and its .aria2 sidecar have
// been discarded. Callers MUST NOT recover from this — not even under
// --fallback (R6). A mismatch means the bytes on disk are not the asset the
// cask promised, and handing them to brew (or plain-brew) would install an
// unverified artifact.
var ErrChecksumMismatch = errors.New("checksum mismatch")

// ErrNoChecksum is a distinct (non-fatal) sentinel: no expected checksum was
// provided, so verification could not be performed. The caller decides the
// posture (stop by default, proceed under an explicit flag) — accel never
// silently treats an unverifiable download as verified.
var ErrNoChecksum = errors.New("no expected checksum")

// ErrInsecureURL is returned when the asset url is not https. A downgrade or
// plain-http asset is rejected up front, before any download is attempted, so
// brewfast never fetches an asset over an unencrypted, tamperable channel.
var ErrInsecureURL = errors.New("asset url is not https")

// downloadFunc runs the actual byte transfer, writing the asset to
// filepath.Join(dir, name). interactive selects aria2's progress presentation
// (a live in-place readout for a TTY, a quiet periodic summary otherwise). It is
// an indirection so tests can inject a fake that writes known bytes without
// invoking real aria2 or touching the network.
type downloadFunc func(dir, name, rawurl string, interactive bool) error

// Downloader accelerates a single cask asset download and verifies it.
//
// The zero value is ready to use and drives real aria2c. Tests may override
// Download with a fake transfer to exercise the sidecar-cleanup, verify, and
// discard-on-mismatch paths without aria2 or a network.
type Downloader struct {
	// Download performs the transfer to dir/name. If nil, aria2Download (the
	// real aria2c invocation) is used.
	Download downloadFunc
}

// Params describes one accelerated download.
type Params struct {
	// URL is the asset url. Must be https or the transfer is rejected.
	URL string
	// CachePath is brew's canonical cache path (from brew.CachePath). aria2
	// writes DIRECTLY to this exact name — dir and filename are derived from
	// it — so no duplicate asset is left beside the canonical file (R25).
	CachePath string
	// ExpectedSHA is the cask's declared lower-hex SHA-256. Empty yields
	// ErrNoChecksum (posture is the caller's decision).
	ExpectedSHA string
	// Interactive is true when brewfast is attached to a terminal, so aria2
	// should show its live updating readout; false (CI/piped) selects a quiet
	// periodic summary instead of an in-place readout that just spams the log.
	Interactive bool
}

// Fetch runs the accelerated download-then-verify for a single asset:
//
//  1. Reject a non-https url before any transfer (ErrInsecureURL).
//  2. Split CachePath into dir + canonical filename and run the transfer
//     writing directly to that name.
//  3. On success, delete the <name>.aria2 control sidecar if present.
//  4. Verify the file's SHA-256 against ExpectedSHA.
//  5. On mismatch, delete BOTH the file and its sidecar, return the fatal
//     ErrChecksumMismatch.
//
// The placed file's mtime is left at write time and never back-dated, so
// brew's freshness gate reuses it on handoff (KTD-2b). An empty ExpectedSHA
// returns ErrNoChecksum after a successful transfer without deleting the file.
func (d Downloader) Fetch(p Params) error {
	if err := requireHTTPS(p.URL); err != nil {
		return err
	}

	dir, name := filepath.Split(p.CachePath)
	// filepath.Split leaves a trailing separator on dir; Clean normalizes it
	// (and turns "" — a bare filename with no dir — into ".").
	dir = filepath.Clean(dir)
	if name == "" {
		return fmt.Errorf("cache path %q has no filename component", p.CachePath)
	}

	filePath := filepath.Join(dir, name)
	sidecar := filePath + ".aria2"

	// When there is no verifiable checksum (empty or malformed ExpectedSHA),
	// verify cannot catch a stale or planted file. aria2's -c would happily
	// reuse a pre-existing full-size file at the canonical path (or splice onto
	// a stale partial) and we would install those unverified bytes. Remove any
	// pre-existing canonical file AND its .aria2 sidecar first so aria2 does a
	// clean full download. When a checksum IS present, keep -c resume behavior:
	// verifyChecksum catches any bad splice.
	if !isValidSHA(p.ExpectedSHA) {
		removeIfPresent(filePath)
		removeIfPresent(sidecar)
	}

	transfer := d.Download
	if transfer == nil {
		transfer = aria2Download
	}
	if err := transfer(dir, name, p.URL, p.Interactive); err != nil {
		return fmt.Errorf("aria2 download of %q: %w", p.URL, err)
	}

	// A completed transfer should not leave a control sidecar; remove it if
	// aria2 left one behind (e.g. a resumed/interrupted run).
	removeIfPresent(sidecar)

	if err := verifyChecksum(filePath, p.ExpectedSHA); err != nil {
		if errors.Is(err, ErrChecksumMismatch) {
			// The bytes are wrong: discard the file and any sidecar so a later
			// handoff can never reuse a corrupt/tampered asset.
			removeIfPresent(filePath)
			removeIfPresent(sidecar)
		}
		return err
	}
	return nil
}

// requireHTTPS returns ErrInsecureURL unless rawurl parses and has scheme
// https. It is pure and performs no I/O.
func requireHTTPS(rawurl string) error {
	u, err := url.Parse(rawurl)
	if err != nil {
		return fmt.Errorf("%w: %q is not a parseable url", ErrInsecureURL, rawurl)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("%w: scheme %q for %q", ErrInsecureURL, u.Scheme, rawurl)
	}
	return nil
}

// verifyChecksum streams the file at path through SHA-256 and compares it to
// expectedSHA (lower-hex, case-insensitive). It returns:
//
//   - ErrNoChecksum if expectedSHA is empty OR malformed (not 64-hex): in
//     either case there is nothing verifiable to compare against, so the
//     posture is the caller's to decide rather than an unconditional (and
//     unrecoverable) mismatch,
//   - ErrChecksumMismatch if a well-formed digest differs,
//   - nil on a match.
//
// It is pure w.r.t. package state — a separately testable function over a real
// file — so the verify decision can be covered without any download.
func verifyChecksum(path, expectedSHA string) error {
	if strings.TrimSpace(expectedSHA) == "" {
		return ErrNoChecksum
	}
	// A non-empty but malformed expected value (not 64-hex) can never equal a
	// real digest — comparing would yield an unconditional, unrecoverable
	// ErrChecksumMismatch and a genuine cask could never install. Surface it as
	// unverifiable instead and let the caller's --no-verify posture decide.
	if !isValidSHA(expectedSHA) {
		return ErrNoChecksum
	}

	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening %q for checksum: %w", path, err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("hashing %q: %w", path, err)
	}
	got := hex.EncodeToString(h.Sum(nil))

	if !strings.EqualFold(got, strings.TrimSpace(expectedSHA)) {
		return fmt.Errorf("%w: got %s, want %s", ErrChecksumMismatch, got, expectedSHA)
	}
	return nil
}

// aria2Download is the real transfer: it invokes aria2c with 16 parallel
// connections writing directly to dir/name. `--` terminates option parsing
// before the url so a dash-leading url can never be read as a flag.
func aria2Download(dir, name, rawurl string, interactive bool) error {
	args := []string{
		"--check-certificate=true",
		// Stall guard: abort a connection that makes no meaningful progress
		// (slow-loris server) rather than hanging brewfast forever under CI or
		// unattended use.
		"--lowest-speed-limit=1K",
		"--timeout=60",
		"-x16", "-s16", "-k1M", "-c",
	}
	// Progress presentation. aria2's default output is a noisy multi-line dump;
	// these flags collapse it to a single clean, updating readout line showing
	// overall progress, aggregate speed, ETA, and the live connection count.
	if interactive {
		// Interactive terminal: ONLY the live one-line readout that updates in
		// place (overall progress, aggregate speed, ETA, connection count).
		// --summary-interval=0 turns off aria2's periodic multi-line summary
		// block so the single readout line is all the user sees.
		args = append(args,
			"--console-log-level=warn",
			"--summary-interval=0",
			"--show-console-readout=true",
			"--human-readable=true",
		)
	} else {
		// No TTY (CI, piped, scripted): the in-place readout is meaningless and
		// just spams the log. Drop it entirely and print a single summary line
		// per 30s so a long download still shows liveness without noise.
		args = append(args,
			"--console-log-level=warn",
			"--summary-interval=30",
			"--show-console-readout=false",
			"--human-readable=true",
		)
	}
	args = append(args, "-o", name, "-d", dir, "--", rawurl)

	cmd := exec.Command("aria2c", args...)
	cmd.Stdout = os.Stderr // progress goes to stderr; keep stdout clean.
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("running aria2c: %w", err)
	}
	return nil
}

// removeIfPresent deletes path, best-effort. Cleanup of a sidecar or partial
// must never mask the primary result (a verify outcome), so every error —
// including not-exist — is intentionally ignored.
func removeIfPresent(path string) {
	_ = os.Remove(path)
}
