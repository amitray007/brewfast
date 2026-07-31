package cmd

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// fixedNow is a deterministic reference time used to drive the staleness gate in
// findStuckCasks tests without ever calling time.Now().
var fixedNow = time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)

// chtimes is a test helper that stamps a fixed mtime on a path, failing on error.
func chtimes(t *testing.T, path string, mtime time.Time) {
	t.Helper()
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

// mkdirAll is a test helper that fails the test on error.
func mkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

// writeFile is a test helper that writes content and fails on error.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	mkdirAll(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestFindStuckCasks_Detected builds a stuck fixture: a token with a version
// directory but NO receipt at token/.metadata and no staged artifact, whose
// version dir is STALE (mtime well before now) → flagged. The staleness is what
// distinguishes a wedged transaction from a live in-progress install.
func TestFindStuckCasks_Detected(t *testing.T) {
	root := t.TempDir()
	// <root>/orpheus-nightly/0.5.9/ exists, empty (no artifact).
	versionDir := filepath.Join(root, "orpheus-nightly", "0.5.9")
	mkdirAll(t, versionDir)
	// No <token>/.metadata/INSTALL_RECEIPT.json.
	// Stamp the version dir well before now so it reads as stale, not in-flight.
	chtimes(t, versionDir, fixedNow.Add(-time.Hour))

	stuck, err := findStuckCasks(root, fixedNow)
	if err != nil {
		t.Fatalf("findStuckCasks: %v", err)
	}
	if !reflect.DeepEqual(stuck, []string{"orpheus-nightly"}) {
		t.Fatalf("want [orpheus-nightly], got %v", stuck)
	}
}

// TestFindStuckCasks_LiveInstallNotFlagged verifies the staleness gate: a token
// matching the raw stuck signature (version dir present, no receipt, no artifact)
// but with a RECENTLY-modified version dir is treated as a live in-progress
// install and is NOT flagged — the false-positive the gate exists to prevent.
func TestFindStuckCasks_LiveInstallNotFlagged(t *testing.T) {
	root := t.TempDir()
	versionDir := filepath.Join(root, "orpheus-nightly", "0.5.9")
	mkdirAll(t, versionDir)
	// Version dir touched just now (within stuckThreshold): an install in progress.
	chtimes(t, versionDir, fixedNow.Add(-time.Minute))

	stuck, err := findStuckCasks(root, fixedNow)
	if err != nil {
		t.Fatalf("findStuckCasks: %v", err)
	}
	if len(stuck) != 0 {
		t.Fatalf("live in-progress install should not be flagged, got %v", stuck)
	}
}

// TestFindStuckCasks_Healthy verifies that a receipt at the TOKEN level under
// .metadata/ (the correct signature) prevents a flag — guarding against the
// bug of looking for the receipt inside the version dir, which would flag every
// healthy cask.
func TestFindStuckCasks_Healthy(t *testing.T) {
	root := t.TempDir()
	mkdirAll(t, filepath.Join(root, "orpheus", "0.5.0"))
	// Receipt present at <token>/.metadata/INSTALL_RECEIPT.json → completed install.
	writeFile(t, filepath.Join(root, "orpheus", ".metadata", "INSTALL_RECEIPT.json"), "{}")

	stuck, err := findStuckCasks(root, fixedNow)
	if err != nil {
		t.Fatalf("findStuckCasks: %v", err)
	}
	if len(stuck) != 0 {
		t.Fatalf("healthy cask should not be flagged, got %v", stuck)
	}
}

// TestFindStuckCasks_ArtifactPresent verifies a version dir holding a real
// (non-empty) artifact is not flagged even without a receipt — the install
// staged bytes, so it is not the empty-wedge signature.
func TestFindStuckCasks_ArtifactPresent(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "widget", "1.0", "Widget.app.txt"), "payload")

	stuck, err := findStuckCasks(root, fixedNow)
	if err != nil {
		t.Fatalf("findStuckCasks: %v", err)
	}
	if len(stuck) != 0 {
		t.Fatalf("cask with staged artifact should not be flagged, got %v", stuck)
	}
}

// TestFindStuckCasks_NoVersionDir verifies a token with only a .metadata dir and
// no version directory is not flagged (nothing was staged).
func TestFindStuckCasks_NoVersionDir(t *testing.T) {
	root := t.TempDir()
	mkdirAll(t, filepath.Join(root, "lonely", ".metadata"))

	stuck, err := findStuckCasks(root, fixedNow)
	if err != nil {
		t.Fatalf("findStuckCasks: %v", err)
	}
	if len(stuck) != 0 {
		t.Fatalf("token without version dir should not be flagged, got %v", stuck)
	}
}

// TestFindStuckCasks_MissingRoot returns no error and no casks when the Caskroom
// does not exist (nothing installed).
func TestFindStuckCasks_MissingRoot(t *testing.T) {
	stuck, err := findStuckCasks(filepath.Join(t.TempDir(), "does-not-exist"), fixedNow)
	if err != nil {
		t.Fatalf("missing root should not error: %v", err)
	}
	if len(stuck) != 0 {
		t.Fatalf("missing root should yield no stuck casks, got %v", stuck)
	}
}

// TestFindOrphans is the AE9 orphan-sweep scenario: a downloads dir with a
// hashed canonical file, a brew-style symlink, a genuine stray regular file, and
// a .aria2 sidecar → returns ONLY the stray file and the sidecar; leaves the
// symlink and the hashed canonical file out. Asserts the symlink is excluded
// specifically.
func TestFindOrphans(t *testing.T) {
	dir := t.TempDir()

	hash := "0000000000000000000000000000000000000000000000000000000000000000"
	// (a) brew's canonical hashed download — must be excluded.
	writeFile(t, filepath.Join(dir, hash+"--orpheus--0.5.9.dmg"), "canonical")
	// (b) a genuine stray regular file (no hash prefix) — orphan.
	writeFile(t, filepath.Join(dir, "orpheus-nightly-leftover.dmg"), "stray")
	// (c) a stray .aria2 control sidecar — orphan.
	writeFile(t, filepath.Join(dir, "orpheus-nightly-leftover.dmg.aria2"), "ctl")
	// (d) brew's version-named symlink pointing at the canonical file — must be
	//     excluded (deleting it removes brew state).
	linkName := "orpheus--0.5.9.dmg"
	if err := os.Symlink(filepath.Join(dir, hash+"--orpheus--0.5.9.dmg"), filepath.Join(dir, linkName)); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	// Sanity: confirm the fixture symlink really is a symlink via Lstat.
	li, err := os.Lstat(filepath.Join(dir, linkName))
	if err != nil {
		t.Fatalf("lstat symlink: %v", err)
	}
	if li.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("fixture %s is not a symlink; test cannot prove exclusion", linkName)
	}

	orphans, err := findOrphans(dir, map[string]bool{})
	if err != nil {
		t.Fatalf("findOrphans: %v", err)
	}

	want := []string{"orpheus-nightly-leftover.dmg", "orpheus-nightly-leftover.dmg.aria2"}
	if !reflect.DeepEqual(orphans, want) {
		t.Fatalf("want %v, got %v", want, orphans)
	}

	// Explicit: the symlink must not appear among orphans.
	for _, o := range orphans {
		if o == linkName {
			t.Fatalf("brew symlink %s was wrongly listed as an orphan", linkName)
		}
		if o == hash+"--orpheus--0.5.9.dmg" {
			t.Fatalf("hashed canonical file was wrongly listed as an orphan")
		}
	}
}

// TestFindOrphans_KnownCanonicalExcluded verifies a non-hashed file that is the
// current canonical name for a known cask is left alone.
func TestFindOrphans_KnownCanonicalExcluded(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "tool-1.2.3.pkg"), "known")
	writeFile(t, filepath.Join(dir, "random-junk.dmg"), "junk")

	orphans, err := findOrphans(dir, map[string]bool{"tool-1.2.3.pkg": true})
	if err != nil {
		t.Fatalf("findOrphans: %v", err)
	}
	want := []string{"random-junk.dmg"}
	if !reflect.DeepEqual(orphans, want) {
		t.Fatalf("want %v, got %v", want, orphans)
	}
}

// TestFindOrphans_MissingDir returns no error and no orphans when the downloads
// dir does not exist.
func TestFindOrphans_MissingDir(t *testing.T) {
	orphans, err := findOrphans(filepath.Join(t.TempDir(), "nope"), map[string]bool{})
	if err != nil {
		t.Fatalf("missing dir should not error: %v", err)
	}
	if len(orphans) != 0 {
		t.Fatalf("missing dir should yield no orphans, got %v", orphans)
	}
}

// TestDoctorCmdRegistered verifies the doctor subcommand is wired into the root
// tree via the single AddCommand line in root.go.
func TestDoctorCmdRegistered(t *testing.T) {
	root := newRootCmd()
	var found bool
	for _, c := range root.Commands() {
		if c.Name() == "doctor" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("doctor subcommand not registered on root")
	}
}
