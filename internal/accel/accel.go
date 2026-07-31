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
	"strings"
)

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
// filepath.Join(dir, name). It is an indirection so tests can inject a fake
// that writes known bytes without invoking real aria2 or touching the network.
type downloadFunc func(dir, name, rawurl string) error

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

	transfer := d.Download
	if transfer == nil {
		transfer = aria2Download
	}
	if err := transfer(dir, name, p.URL); err != nil {
		return fmt.Errorf("aria2 download of %q: %w", p.URL, err)
	}

	filePath := filepath.Join(dir, name)
	sidecar := filePath + ".aria2"

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
//   - ErrNoChecksum if expectedSHA is empty (nothing to verify against),
//   - ErrChecksumMismatch if the computed digest differs,
//   - nil on a match.
//
// It is pure w.r.t. package state — a separately testable function over a real
// file — so the verify decision can be covered without any download.
func verifyChecksum(path, expectedSHA string) error {
	if strings.TrimSpace(expectedSHA) == "" {
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
func aria2Download(dir, name, rawurl string) error {
	cmd := exec.Command(
		"aria2c",
		"--check-certificate=true",
		"-x16", "-s16", "-k1M", "-c",
		"-o", name,
		"-d", dir,
		"--", rawurl,
	)
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
