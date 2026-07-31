package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/amitray007/brewfast/internal/brew"
)

// checkFakes builds a checkDeps with the given cask/error and a slow-host
// predicate. A nil download/handoff surface is structurally impossible here:
// checkDeps has no fetch field, so these tests prove `check` cannot install by
// construction — there is nothing to assert-not-called because nothing is wired.
func checkFakes(cask *brew.Cask, caskErr error, slow bool) checkDeps {
	return checkDeps{
		caskInfo:   func(string) (*brew.Cask, error) { return cask, caskErr },
		isSlowHost: func(string) bool { return slow },
	}
}

// TestCheck_Accelerable: slow GitHub host + checksum present → accelerate-able.
func TestCheck_Accelerable(t *testing.T) {
	if got := classifyCheck("https://github.com/o/r/releases/download/v1/a.dmg", "abc123", func(string) bool { return true }); got != categoryAccelerable {
		t.Fatalf("classify: want accelerable, got %v", got)
	}

	var out bytes.Buffer
	d := checkFakes(&brew.Cask{Token: "orpheus-nightly", URL: "https://github.com/o/r/releases/download/v1/a.dmg", SHA256: "abc123"}, nil, true)
	if err := runCheck(&out, d, "orpheus-nightly"); err != nil {
		t.Fatalf("runCheck: %v", err)
	}
	if !strings.Contains(out.String(), "can accelerate") {
		t.Fatalf("expected accelerate-able report, got: %q", out.String())
	}
}

// TestCheck_AlreadyFast: non-slow host → already-fast, reassuring framing.
func TestCheck_AlreadyFast(t *testing.T) {
	if got := classifyCheck("https://dl.example.com/a.dmg", "abc123", func(string) bool { return false }); got != categoryAlreadyFast {
		t.Fatalf("classify: want already-fast, got %v", got)
	}

	var out bytes.Buffer
	d := checkFakes(&brew.Cask{Token: "fastcask", URL: "https://dl.example.com/a.dmg", SHA256: "abc123"}, nil, false)
	if err := runCheck(&out, d, "fastcask"); err != nil {
		t.Fatalf("runCheck: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "already CDN-fast") || !strings.Contains(s, "not a failure") {
		t.Fatalf("expected already-fast reassuring report, got: %q", s)
	}
}

// TestCheck_NoChecksum: slow host but empty sha256 → checksum-caveat category.
func TestCheck_NoChecksum(t *testing.T) {
	if got := classifyCheck("https://github.com/o/r/releases/download/v1/a.dmg", "", func(string) bool { return true }); got != categoryNoChecksum {
		t.Fatalf("classify: want no-checksum, got %v", got)
	}

	var out bytes.Buffer
	d := checkFakes(&brew.Cask{Token: "nosum", URL: "https://github.com/o/r/releases/download/v1/a.dmg", SHA256: ""}, nil, true)
	if err := runCheck(&out, d, "nosum"); err != nil {
		t.Fatalf("runCheck: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "no checksum") || !strings.Contains(s, "--no-verify") {
		t.Fatalf("expected checksum-caveat report naming --no-verify, got: %q", s)
	}
}

// TestCheck_NotFound: unknown cask → clear non-zero not-found error, and the
// host classifier is never consulted (nothing to classify).
func TestCheck_NotFound(t *testing.T) {
	var out bytes.Buffer
	slowCalled := false
	d := checkDeps{
		caskInfo:   func(string) (*brew.Cask, error) { return nil, brew.ErrCaskNotFound },
		isSlowHost: func(string) bool { slowCalled = true; return true },
	}

	err := runCheck(&out, d, "ghostcask")
	if err == nil {
		t.Fatal("expected a not-found error")
	}
	if code := exitCodeFor(err); code == 0 {
		t.Fatalf("not-found should carry a non-zero exit code, got %d", code)
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected a clear not-found message, got: %v", err)
	}
	if slowCalled {
		t.Fatal("host classification must not run when the cask does not resolve")
	}
	if out.String() != "" {
		t.Fatalf("no report should be printed for an unknown cask, got: %q", out.String())
	}
}

// TestCheckCmdRegistered verifies the check subcommand is wired into root.
func TestCheckCmdRegistered(t *testing.T) {
	root := newRootCmd()
	var found bool
	for _, c := range root.Commands() {
		if c.Name() == "check" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("check subcommand not registered on root")
	}
}
