package accel

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// sha256Hex returns the lower-hex SHA-256 of b, for building known-good and
// known-bad expectations in the tests below.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// writeFile is a helper that writes content to dir/name, used both to seed
// fixtures and as the body of fake downloaders.
func writeFile(t *testing.T, dir, name string, content []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatalf("seeding %q: %v", p, err)
	}
	return p
}

func TestVerifyChecksum_Match(t *testing.T) {
	dir := t.TempDir()
	content := []byte("the quick brown fox")
	p := writeFile(t, dir, "asset.dmg", content)

	if err := verifyChecksum(p, sha256Hex(content)); err != nil {
		t.Errorf("verifyChecksum(good) = %v, want nil", err)
	}
	// Case-insensitive match on the expected hex.
	if err := verifyChecksum(p, hexUpper(sha256Hex(content))); err != nil {
		t.Errorf("verifyChecksum(good, uppercased) = %v, want nil", err)
	}
}

func TestVerifyChecksum_Mismatch(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "asset.dmg", []byte("real bytes"))

	err := verifyChecksum(p, sha256Hex([]byte("different bytes")))
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Errorf("verifyChecksum(bad) = %v, want ErrChecksumMismatch", err)
	}
}

func TestVerifyChecksum_Empty(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "asset.dmg", []byte("bytes"))

	if err := verifyChecksum(p, ""); !errors.Is(err, ErrNoChecksum) {
		t.Errorf("verifyChecksum(empty) = %v, want ErrNoChecksum", err)
	}
	if err := verifyChecksum(p, "   "); !errors.Is(err, ErrNoChecksum) {
		t.Errorf("verifyChecksum(whitespace) = %v, want ErrNoChecksum", err)
	}
}

// TestVerifyChecksum_Malformed covers Fix 1: a non-empty but malformed
// expected value (not 64-hex) can never equal a real digest. It must surface
// as ErrNoChecksum (unverifiable, posture is the caller's) rather than a
// permanent, unrecoverable ErrChecksumMismatch that would block a genuine cask.
func TestVerifyChecksum_Malformed(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "asset.dmg", []byte("bytes"))

	malformed := []string{
		"abc123",                         // far too short
		"not-hex-at-all",                 // non-hex characters
		sha256Hex([]byte("x"))[:63],      // 63 hex digits — one short
		sha256Hex([]byte("x")) + "0",     // 65 hex digits — one long
		"g" + sha256Hex([]byte("x"))[1:], // 64 chars but a non-hex digit
	}
	for _, exp := range malformed {
		err := verifyChecksum(p, exp)
		if !errors.Is(err, ErrNoChecksum) {
			t.Errorf("verifyChecksum(malformed %q) = %v, want ErrNoChecksum", exp, err)
		}
		if errors.Is(err, ErrChecksumMismatch) {
			t.Errorf("verifyChecksum(malformed %q) returned ErrChecksumMismatch; malformed must not be a fatal mismatch", exp)
		}
	}
}

// TestFetch_NoChecksumRemovesStaleFile covers Fix 2: when ExpectedSHA is empty
// (or malformed) there is no verify to catch a stale/planted file at the
// canonical path. Fetch must remove any pre-existing canonical file and its
// .aria2 sidecar BEFORE the transfer so aria2 -c cannot reuse unverifiable
// bytes. The fake download records whether the canonical file still existed at
// the moment it was invoked.
func TestFetch_NoChecksumRemovesStaleFile(t *testing.T) {
	dir := t.TempDir()
	name := "stale--4.0.dmg"
	cachePath := filepath.Join(dir, name)

	// Seed a pre-existing full-size "stale/planted" file and a leftover sidecar
	// at the canonical path before the transfer runs.
	writeFile(t, dir, name, []byte("stale planted bytes"))
	writeFile(t, dir, name+".aria2", []byte("stale control"))

	existedAtCall := true
	sidecarAtCall := true
	d := Downloader{
		Download: func(gotDir, gotName, _ string) error {
			if _, statErr := os.Stat(filepath.Join(gotDir, gotName)); errors.Is(statErr, os.ErrNotExist) {
				existedAtCall = false
			}
			if _, statErr := os.Stat(filepath.Join(gotDir, gotName+".aria2")); errors.Is(statErr, os.ErrNotExist) {
				sidecarAtCall = false
			}
			writeFile(t, gotDir, gotName, []byte("freshly fetched bytes"))
			return nil
		},
	}

	err := d.Fetch(Params{
		URL:         "https://dl.example.com/app.dmg",
		CachePath:   cachePath,
		ExpectedSHA: "",
	})
	if !errors.Is(err, ErrNoChecksum) {
		t.Fatalf("Fetch(no checksum) = %v, want ErrNoChecksum", err)
	}
	if existedAtCall {
		t.Error("stale canonical file still existed when transfer was invoked; want removed before transfer")
	}
	if sidecarAtCall {
		t.Error("stale .aria2 sidecar still existed when transfer was invoked; want removed before transfer")
	}
}

func TestRequireHTTPS(t *testing.T) {
	good := []string{
		"https://github.com/o/r/releases/download/v1/app.dmg",
		"https://dl.example.com/app.dmg",
	}
	for _, u := range good {
		if err := requireHTTPS(u); err != nil {
			t.Errorf("requireHTTPS(%q) = %v, want nil", u, err)
		}
	}

	bad := []string{
		"http://github.com/o/r/releases/download/v1/app.dmg",
		"ftp://example.com/app.dmg",
		"://bad",
		"",
	}
	for _, u := range bad {
		if err := requireHTTPS(u); !errors.Is(err, ErrInsecureURL) {
			t.Errorf("requireHTTPS(%q) = %v, want ErrInsecureURL", u, err)
		}
	}
}

// TestFetch_MatchingHash covers AE1: a successful accelerated download whose
// bytes match the declared checksum lands exactly at the canonical path with no
// sidecar and no sibling duplicate.
func TestFetch_MatchingHash(t *testing.T) {
	dir := t.TempDir()
	content := []byte("orpheus dmg payload")
	name := "orpheus--0.6.0.dmg"
	cachePath := filepath.Join(dir, name)

	d := Downloader{
		Download: func(gotDir, gotName, gotURL string) error {
			if gotDir != dir {
				t.Errorf("download dir = %q, want %q", gotDir, dir)
			}
			if gotName != name {
				t.Errorf("download name = %q, want %q", gotName, name)
			}
			writeFile(t, gotDir, gotName, content)
			return nil
		},
	}

	err := d.Fetch(Params{
		URL:         "https://github.com/o/r/releases/download/v1/orpheus.dmg",
		CachePath:   cachePath,
		ExpectedSHA: sha256Hex(content),
	})
	if err != nil {
		t.Fatalf("Fetch(matching) = %v, want nil", err)
	}

	// File is at the exact canonical path.
	if _, statErr := os.Stat(cachePath); statErr != nil {
		t.Errorf("canonical file missing: %v", statErr)
	}
	// No .aria2 sidecar left.
	if _, statErr := os.Stat(cachePath + ".aria2"); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("sidecar present after success, want removed (stat err = %v)", statErr)
	}
	// Exactly one file in the dir — no sibling duplicate.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("dir has %d entries %v, want exactly 1 (the canonical file)", len(entries), names)
	}
}

// TestFetch_SidecarRemovedOnSuccess covers AE1/R25: a .aria2 control sidecar
// left by aria2 is cleaned up on a successful, verified transfer.
func TestFetch_SidecarRemovedOnSuccess(t *testing.T) {
	dir := t.TempDir()
	content := []byte("payload with leftover sidecar")
	name := "app--1.0.dmg"
	cachePath := filepath.Join(dir, name)

	d := Downloader{
		Download: func(gotDir, gotName, _ string) error {
			writeFile(t, gotDir, gotName, content)
			// Simulate aria2 leaving its control file behind.
			writeFile(t, gotDir, gotName+".aria2", []byte("control"))
			return nil
		},
	}

	if err := d.Fetch(Params{
		URL:         "https://dl.example.com/app.dmg",
		CachePath:   cachePath,
		ExpectedSHA: sha256Hex(content),
	}); err != nil {
		t.Fatalf("Fetch = %v, want nil", err)
	}

	if _, statErr := os.Stat(cachePath + ".aria2"); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("sidecar not removed on success (stat err = %v)", statErr)
	}
	if _, statErr := os.Stat(cachePath); statErr != nil {
		t.Errorf("canonical file should remain: %v", statErr)
	}
}

// TestFetch_MismatchDiscards covers AE3/R6: a checksum mismatch removes BOTH
// the file and its sidecar and returns the fatal ErrChecksumMismatch.
func TestFetch_MismatchDiscards(t *testing.T) {
	dir := t.TempDir()
	name := "bad--2.0.dmg"
	cachePath := filepath.Join(dir, name)

	d := Downloader{
		Download: func(gotDir, gotName, _ string) error {
			writeFile(t, gotDir, gotName, []byte("corrupt payload"))
			writeFile(t, gotDir, gotName+".aria2", []byte("control"))
			return nil
		},
	}

	err := d.Fetch(Params{
		URL:         "https://dl.example.com/app.dmg",
		CachePath:   cachePath,
		ExpectedSHA: sha256Hex([]byte("the expected payload")),
	})
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("Fetch(mismatch) = %v, want ErrChecksumMismatch", err)
	}

	if _, statErr := os.Stat(cachePath); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("file not discarded on mismatch (stat err = %v)", statErr)
	}
	if _, statErr := os.Stat(cachePath + ".aria2"); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("sidecar not discarded on mismatch (stat err = %v)", statErr)
	}
}

// TestFetch_EmptyChecksum covers AE10: an empty expected checksum is surfaced
// as ErrNoChecksum, never a silent pass. The file is left in place for the
// caller to decide posture.
func TestFetch_EmptyChecksum(t *testing.T) {
	dir := t.TempDir()
	content := []byte("unverifiable payload")
	name := "nocsum--3.0.dmg"
	cachePath := filepath.Join(dir, name)

	d := Downloader{
		Download: func(gotDir, gotName, _ string) error {
			writeFile(t, gotDir, gotName, content)
			return nil
		},
	}

	err := d.Fetch(Params{
		URL:         "https://dl.example.com/app.dmg",
		CachePath:   cachePath,
		ExpectedSHA: "",
	})
	if !errors.Is(err, ErrNoChecksum) {
		t.Fatalf("Fetch(no checksum) = %v, want ErrNoChecksum", err)
	}
	// The downloaded file is NOT discarded on ErrNoChecksum — posture is U4's call.
	if _, statErr := os.Stat(cachePath); statErr != nil {
		t.Errorf("file should remain on ErrNoChecksum: %v", statErr)
	}
}

// TestFetch_NonHTTPS covers the TLS-enforcement gate: an http url is rejected
// before any transfer, and the injected downloader is never called.
func TestFetch_NonHTTPS(t *testing.T) {
	dir := t.TempDir()
	called := false

	d := Downloader{
		Download: func(_, _, _ string) error {
			called = true
			return nil
		},
	}

	err := d.Fetch(Params{
		URL:         "http://github.com/o/r/releases/download/v1/app.dmg",
		CachePath:   filepath.Join(dir, "app--1.0.dmg"),
		ExpectedSHA: sha256Hex([]byte("whatever")),
	})
	if !errors.Is(err, ErrInsecureURL) {
		t.Fatalf("Fetch(http) = %v, want ErrInsecureURL", err)
	}
	if called {
		t.Error("downloader was invoked for a non-https url; want rejected before any download")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("dir not empty after rejected download: %d entries", len(entries))
	}
}

// hexUpper returns an ASCII-uppercased copy of s, to prove verifyChecksum's
// comparison is case-insensitive.
func hexUpper(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			b[i] = c - ('a' - 'A')
		}
	}
	return string(b)
}
