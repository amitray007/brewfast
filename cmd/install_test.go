package cmd

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/amitray007/brewfast/internal/accel"
	"github.com/amitray007/brewfast/internal/brew"
	"github.com/amitray007/brewfast/internal/resolve"
)

// fakeDeps returns a deps wired with sensible, all-succeeding fakes plus call
// recorders. Individual tests override the fields they care about. The stdout is
// a plain buffer (not a *os.File), so isTTY is false and the success line is
// suppressed — tests assert on stderr and on the recorded calls instead.
type recorder struct {
	fetchCalled       bool
	handoffCalled     bool
	installAria2Calls int
	fetchParams       accel.Params
	handoffOp         brew.Operation
}

func fakeDeps(rec *recorder, cask *brew.Cask) (deps, *bytes.Buffer, *bytes.Buffer) {
	var out, errBuf bytes.Buffer
	d := deps{
		caskInfo:    func(string) (*brew.Cask, error) { return cask, nil },
		cachePath:   func(string) (string, error) { return "/tmp/cache/abc--" + cask.Token + ".dmg", nil },
		isInstalled: func(string) bool { return true }, // aria2 present, cask installed
		installAria2: func() error {
			rec.installAria2Calls++
			return nil
		},
		isSlowHost: func(string) bool { return true },
		resolve: func(name string, _ resolve.Options) (string, error) {
			return name, nil
		},
		fetch: func(p accel.Params) error {
			rec.fetchCalled = true
			rec.fetchParams = p
			return nil
		},
		handoff: func(_ context.Context, op brew.Operation, _ string) error {
			rec.handoffCalled = true
			rec.handoffOp = op
			return nil
		},
		stdout: &out,
		stderr: &errBuf,
	}
	return d, &out, &errBuf
}

func sampleCask() *brew.Cask {
	return &brew.Cask{
		Token:   "orpheus-nightly",
		Version: "1.2.3",
		URL:     "https://github.com/o/r/releases/download/v1/app.dmg",
		SHA256:  "deadbeef",
	}
}

func TestVersionFlag(t *testing.T) {
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--version"})
	if err := root.Execute(); err != nil {
		t.Fatalf("--version returned error: %v", err)
	}
	if !strings.Contains(out.String(), version) {
		t.Fatalf("--version output %q does not contain version %q", out.String(), version)
	}
}

func TestHelpMentionsBrewfast(t *testing.T) {
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("--help returned error: %v", err)
	}
	if !strings.Contains(out.String(), "brewfast") {
		t.Fatalf("--help output does not mention brewfast: %q", out.String())
	}
}

// AE2: non-slow host, no flags → non-zero, message names --any-host/--fallback,
// nothing fetched or handed off.
func TestNonSlowHost_NoFlags_Stops(t *testing.T) {
	rec := &recorder{}
	d, _, errBuf := fakeDeps(rec, sampleCask())
	d.isSlowHost = func(string) bool { return false }

	err := runInstall(context.Background(), d, postureFlags{}, "orpheus-nightly")
	if err == nil {
		t.Fatal("expected a stop error, got nil")
	}
	if exitCodeFor(err) == 0 {
		t.Fatalf("expected non-zero exit, got 0 for err: %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "--any-host") || !strings.Contains(msg, "--fallback") {
		t.Fatalf("stop message must name --any-host and --fallback, got: %q", msg)
	}
	if rec.fetchCalled || rec.handoffCalled {
		t.Fatalf("nothing should be fetched/handed off; fetch=%v handoff=%v", rec.fetchCalled, rec.handoffCalled)
	}
	_ = errBuf
}

// AE6: non-slow host + --fallback → plain brew handoff, fetch not called.
func TestNonSlowHost_Fallback_HandsOff(t *testing.T) {
	rec := &recorder{}
	d, _, _ := fakeDeps(rec, sampleCask())
	d.isSlowHost = func(string) bool { return false }

	err := runInstall(context.Background(), d, postureFlags{fallback: true}, "orpheus-nightly")
	if err != nil {
		t.Fatalf("fallback handoff should succeed, got: %v", err)
	}
	if !rec.handoffCalled {
		t.Fatal("expected plain-brew handoff to be invoked")
	}
	if rec.fetchCalled {
		t.Fatal("fetch must NOT be called on the fallback path")
	}
}

// --any-host on a non-slow host → accelerate (fetch IS called).
func TestNonSlowHost_AnyHost_Accelerates(t *testing.T) {
	rec := &recorder{}
	d, _, _ := fakeDeps(rec, sampleCask())
	d.isSlowHost = func(string) bool { return false }

	err := runInstall(context.Background(), d, postureFlags{anyHost: true}, "orpheus-nightly")
	if err != nil {
		t.Fatalf("--any-host accelerate path should succeed, got: %v", err)
	}
	if !rec.fetchCalled {
		t.Fatal("expected fetch to be called on the --any-host override path")
	}
	if !rec.handoffCalled {
		t.Fatal("expected handoff after a successful accelerated fetch")
	}
}

// AE4 (default half): no checksum, no flags → stop naming --no-verify/--fallback.
func TestNoChecksum_NoFlags_Stops(t *testing.T) {
	rec := &recorder{}
	d, _, _ := fakeDeps(rec, sampleCask())
	d.fetch = func(p accel.Params) error {
		rec.fetchCalled = true
		return accel.ErrNoChecksum
	}

	err := runInstall(context.Background(), d, postureFlags{}, "orpheus-nightly")
	if err == nil {
		t.Fatal("expected a stop error for a no-checksum cask, got nil")
	}
	if exitCodeFor(err) == 0 {
		t.Fatalf("expected non-zero exit, got 0")
	}
	msg := err.Error()
	if !strings.Contains(msg, "--no-verify") || !strings.Contains(msg, "--fallback") {
		t.Fatalf("no-checksum stop must name --no-verify and --fallback, got: %q", msg)
	}
	if rec.handoffCalled {
		t.Fatal("no handoff should happen on the no-checksum default stop")
	}
}

// AE4 (override half): no checksum + --no-verify → proceeds AND warns.
func TestNoChecksum_NoVerify_ProceedsWithWarning(t *testing.T) {
	rec := &recorder{}
	d, _, errBuf := fakeDeps(rec, sampleCask())
	d.fetch = func(p accel.Params) error {
		rec.fetchCalled = true
		return accel.ErrNoChecksum
	}

	err := runInstall(context.Background(), d, postureFlags{noVerify: true}, "orpheus-nightly")
	if err != nil {
		t.Fatalf("--no-verify should proceed, got: %v", err)
	}
	if !rec.handoffCalled {
		t.Fatal("expected handoff to proceed under --no-verify")
	}
	warn := errBuf.String()
	if !strings.Contains(warn, "WARNING") || !strings.Contains(warn, "orpheus-nightly") {
		t.Fatalf("expected an unmissable unverified warning naming the cask, got: %q", warn)
	}
	if !strings.Contains(warn, sampleCask().URL) {
		t.Fatalf("unverified warning must name the url, got: %q", warn)
	}
}

// AE3 / F5: checksum mismatch + --fallback → STILL fatal (fallback must not rescue it).
func TestChecksumMismatch_Fallback_StillFatal(t *testing.T) {
	rec := &recorder{}
	d, _, _ := fakeDeps(rec, sampleCask())
	d.fetch = func(p accel.Params) error {
		rec.fetchCalled = true
		return accel.ErrChecksumMismatch
	}

	err := runInstall(context.Background(), d, postureFlags{fallback: true}, "orpheus-nightly")
	if err == nil {
		t.Fatal("checksum mismatch must be fatal even under --fallback, got nil")
	}
	if !errors.Is(err, accel.ErrChecksumMismatch) {
		t.Fatalf("expected the error to wrap ErrChecksumMismatch, got: %v", err)
	}
	if exitCodeFor(err) == 0 {
		t.Fatalf("checksum mismatch must be non-zero exit")
	}
	if rec.handoffCalled {
		t.Fatal("no handoff should ever happen after a checksum mismatch")
	}
}

// AE7: aria2 absent → installAria2 invoked with a notice; then proceeds.
func TestAria2Absent_Installs(t *testing.T) {
	rec := &recorder{}
	d, _, errBuf := fakeDeps(rec, sampleCask())
	// aria2 missing, but cask reports installed for op selection.
	d.isInstalled = func(tool string) bool { return tool != "aria2" }

	err := runInstall(context.Background(), d, postureFlags{}, "orpheus-nightly")
	if err != nil {
		t.Fatalf("expected success after auto-installing aria2, got: %v", err)
	}
	if rec.installAria2Calls != 1 {
		t.Fatalf("expected installAria2 to be called once, got %d", rec.installAria2Calls)
	}
	if !strings.Contains(errBuf.String(), "aria2") {
		t.Fatalf("expected a one-line aria2 install notice, got: %q", errBuf.String())
	}
}

// aria2 install failure → clear error naming the manual command.
func TestAria2InstallFailure_ClearError(t *testing.T) {
	rec := &recorder{}
	d, _, _ := fakeDeps(rec, sampleCask())
	d.isInstalled = func(tool string) bool { return tool != "aria2" }
	d.installAria2 = func() error {
		rec.installAria2Calls++
		return errors.New("brew exploded")
	}

	err := runInstall(context.Background(), d, postureFlags{}, "orpheus-nightly")
	if err == nil {
		t.Fatal("expected an error when aria2 install fails, got nil")
	}
	if !strings.Contains(err.Error(), "brew install aria2") {
		t.Fatalf("aria2 failure error must name the manual command, got: %q", err.Error())
	}
	if rec.fetchCalled {
		t.Fatal("must not proceed to fetch when aria2 install failed")
	}
}

// Happy path: slow host, valid checksum → fetch then handoff, no error.
func TestHappyPath_Accelerates(t *testing.T) {
	rec := &recorder{}
	d, _, _ := fakeDeps(rec, sampleCask())

	err := runInstall(context.Background(), d, postureFlags{}, "orpheus-nightly")
	if err != nil {
		t.Fatalf("happy path should succeed, got: %v", err)
	}
	if !rec.fetchCalled {
		t.Fatal("expected fetch on the happy path")
	}
	if !rec.handoffCalled {
		t.Fatal("expected handoff on the happy path")
	}
	if rec.fetchParams.ExpectedSHA != "deadbeef" {
		t.Fatalf("fetch should receive the cask sha, got %q", rec.fetchParams.ExpectedSHA)
	}
	// Cask reported installed → upgrade op.
	if rec.handoffOp != brew.OpUpgrade {
		t.Fatalf("expected upgrade op for an installed cask, got %q", rec.handoffOp)
	}
}

// Not-yet-installed cask → reinstall-from-cache op.
func TestNotInstalled_UsesReinstall(t *testing.T) {
	rec := &recorder{}
	d, _, _ := fakeDeps(rec, sampleCask())
	d.isInstalled = func(tool string) bool { return tool == "aria2" } // aria2 present, cask not

	if err := runInstall(context.Background(), d, postureFlags{}, "orpheus-nightly"); err != nil {
		t.Fatalf("happy path should succeed, got: %v", err)
	}
	if rec.handoffOp != brew.OpReinstall {
		t.Fatalf("expected reinstall op for a not-installed cask, got %q", rec.handoffOp)
	}
}

// Resolve cancel → clean (zero) exit, nothing fetched.
func TestResolveCancelled_CleanExit(t *testing.T) {
	rec := &recorder{}
	d, _, _ := fakeDeps(rec, sampleCask())
	d.resolve = func(string, resolve.Options) (string, error) {
		return "", resolve.ErrCancelled
	}

	err := runInstall(context.Background(), d, postureFlags{}, "orpheus")
	if exitCodeFor(err) != 0 {
		t.Fatalf("a deliberate cancel must be a clean (0) exit, got code %d (err %v)", exitCodeFor(err), err)
	}
	if rec.fetchCalled || rec.handoffCalled {
		t.Fatal("nothing should run after a cancel")
	}
}

// Non-interactive resolve failure → non-zero exit.
func TestResolveNoInteractive_NonZero(t *testing.T) {
	rec := &recorder{}
	d, _, _ := fakeDeps(rec, sampleCask())
	d.resolve = func(string, resolve.Options) (string, error) {
		return "", resolve.ErrNoInteractiveResolve
	}

	err := runInstall(context.Background(), d, postureFlags{noInput: true}, "orpheus")
	if exitCodeFor(err) == 0 {
		t.Fatal("no-interactive resolve must exit non-zero")
	}
	if rec.fetchCalled {
		t.Fatal("nothing should be fetched when resolution fails")
	}
}

// Cache path moving between download and handoff → abort (KTD-2b).
func TestCachePathMoves_Aborts(t *testing.T) {
	rec := &recorder{}
	d, _, _ := fakeDeps(rec, sampleCask())
	calls := 0
	d.cachePath = func(string) (string, error) {
		calls++
		if calls == 1 {
			return "/tmp/cache/old.dmg", nil
		}
		return "/tmp/cache/new.dmg", nil // moved before handoff
	}

	err := runInstall(context.Background(), d, postureFlags{}, "orpheus-nightly")
	if err == nil {
		t.Fatal("expected an abort when the cache path moved before handoff")
	}
	if rec.handoffCalled {
		t.Fatal("must not hand off when the canonical cache path moved")
	}
}
