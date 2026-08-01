package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
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
		isInstalled: func(string) bool { return true }, // aria2 present
		// Default: the cask is NOT installed, so the happy-path tests below still
		// accelerate + reinstall-from-cache (the up-to-date short-circuit only
		// triggers when a test overrides this to report the current version).
		installedVersion: func(string) (string, bool, error) { return "", false, nil },
		installAria2: func() error {
			rec.installAria2Calls++
			return nil
		},
		isSlowHost: func(string) bool { return true },
		resolve: func(name string, _ resolve.Options) (string, *brew.Cask, error) {
			return name, cask, nil
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
	d.isInstalled = func(tool string) bool { return tool != "aria2c" }

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
	d.isInstalled = func(tool string) bool { return tool != "aria2c" }
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
	// Every accelerated handoff uses reinstall-from-cache, which covers both
	// first install and upgrade.
	if rec.handoffOp != brew.OpReinstall {
		t.Fatalf("expected reinstall op, got %q", rec.handoffOp)
	}
}

// Not-yet-installed cask → fetch + reinstall-from-cache op.
func TestNotInstalled_UsesReinstall(t *testing.T) {
	rec := &recorder{}
	d, _, _ := fakeDeps(rec, sampleCask())
	d.installedVersion = func(string) (string, bool, error) { return "", false, nil } // not installed

	if err := runInstall(context.Background(), d, postureFlags{}, "orpheus-nightly"); err != nil {
		t.Fatalf("happy path should succeed, got: %v", err)
	}
	if !rec.fetchCalled {
		t.Fatal("expected fetch for a not-installed cask")
	}
	if !rec.handoffCalled {
		t.Fatal("expected handoff for a not-installed cask")
	}
	if rec.handoffOp != brew.OpReinstall {
		t.Fatalf("expected reinstall op for a not-installed cask, got %q", rec.handoffOp)
	}
}

// KEY REGRESSION: installed AND at the current version, no --reinstall → NO-OP.
// The whole accelerate+handoff pipeline is skipped: fetch is NOT called, handoff
// is NOT called, and stdout says the cask is already up to date. This is the fix
// for `brew reinstall` needlessly tearing down and re-laying an up-to-date app.
func TestAlreadyCurrent_NoOp(t *testing.T) {
	rec := &recorder{}
	cask := sampleCask()
	d, out, _ := fakeDeps(rec, cask)
	d.installedVersion = func(string) (string, bool, error) { return cask.Version, true, nil }

	err := runInstall(context.Background(), d, postureFlags{}, "orpheus-nightly")
	if err != nil {
		t.Fatalf("an already-current cask must be a clean no-op, got: %v", err)
	}
	if rec.fetchCalled {
		t.Fatal("no download must happen when already up to date")
	}
	if rec.handoffCalled {
		t.Fatal("no handoff must happen when already up to date")
	}
	if !strings.Contains(out.String(), "up to date") {
		t.Fatalf("expected an 'up to date' message on stdout, got: %q", out.String())
	}
	if !strings.Contains(out.String(), cask.Version) {
		t.Fatalf("up-to-date message must name the version, got: %q", out.String())
	}
}

// Already current BUT --reinstall passed → proceed and reinstall from the freshly
// accelerated cache (fetch + handoff with OpReinstall).
func TestAlreadyCurrent_Reinstall_Proceeds(t *testing.T) {
	rec := &recorder{}
	cask := sampleCask()
	d, _, _ := fakeDeps(rec, cask)
	d.installedVersion = func(string) (string, bool, error) { return cask.Version, true, nil }

	err := runInstall(context.Background(), d, postureFlags{reinstall: true}, "orpheus-nightly")
	if err != nil {
		t.Fatalf("--reinstall should proceed even when current, got: %v", err)
	}
	if !rec.fetchCalled {
		t.Fatal("expected fetch under --reinstall")
	}
	if !rec.handoffCalled {
		t.Fatal("expected handoff under --reinstall")
	}
	if rec.handoffOp != brew.OpReinstall {
		t.Fatalf("--reinstall must force OpReinstall, got %q", rec.handoffOp)
	}
}

// Installed but OUTDATED → accelerate and hand off with OpUpgrade.
func TestOutdated_UsesUpgrade(t *testing.T) {
	rec := &recorder{}
	cask := sampleCask() // Version "1.2.3"
	d, _, _ := fakeDeps(rec, cask)
	d.installedVersion = func(string) (string, bool, error) { return "1.0.0", true, nil }

	err := runInstall(context.Background(), d, postureFlags{}, "orpheus-nightly")
	if err != nil {
		t.Fatalf("outdated cask should accelerate + upgrade, got: %v", err)
	}
	if !rec.fetchCalled {
		t.Fatal("expected fetch for an outdated cask")
	}
	if !rec.handoffCalled {
		t.Fatal("expected handoff for an outdated cask")
	}
	if rec.handoffOp != brew.OpUpgrade {
		t.Fatalf("an outdated cask must hand off with OpUpgrade, got %q", rec.handoffOp)
	}
}

// Fix 1: --force on a non-slow host → accelerates (implies --any-host).
func TestForce_NonSlowHost_Accelerates(t *testing.T) {
	rec := &recorder{}
	d, _, _ := fakeDeps(rec, sampleCask())
	d.isSlowHost = func(string) bool { return false }

	err := runInstall(context.Background(), d, postureFlags{force: true}, "orpheus-nightly")
	if err != nil {
		t.Fatalf("--force should accelerate on a non-slow host, got: %v", err)
	}
	if !rec.fetchCalled {
		t.Fatal("expected fetch to be called on the --force override path")
	}
	if !rec.handoffCalled {
		t.Fatal("expected handoff after a successful accelerated fetch under --force")
	}
}

// Fix 1: --force on a no-checksum cask → proceeds (implies --no-verify).
func TestForce_NoChecksum_Proceeds(t *testing.T) {
	rec := &recorder{}
	d, _, errBuf := fakeDeps(rec, sampleCask())
	d.fetch = func(p accel.Params) error {
		rec.fetchCalled = true
		return accel.ErrNoChecksum
	}

	err := runInstall(context.Background(), d, postureFlags{force: true}, "orpheus-nightly")
	if err != nil {
		t.Fatalf("--force should tolerate a missing checksum and proceed, got: %v", err)
	}
	if !rec.handoffCalled {
		t.Fatal("expected handoff to proceed under --force on a no-checksum cask")
	}
	// A missing checksum genuinely was not verified, so the warning IS expected.
	if !strings.Contains(errBuf.String(), "WARNING") {
		t.Fatalf("expected the unverified warning on the no-checksum --force path, got: %q", errBuf.String())
	}
}

// Fix 1: --force must NOT rescue a genuine checksum mismatch — still fatal.
func TestForce_ChecksumMismatch_StillFatal(t *testing.T) {
	rec := &recorder{}
	d, _, _ := fakeDeps(rec, sampleCask())
	d.fetch = func(p accel.Params) error {
		rec.fetchCalled = true
		return accel.ErrChecksumMismatch
	}

	err := runInstall(context.Background(), d, postureFlags{force: true}, "orpheus-nightly")
	if err == nil {
		t.Fatal("checksum mismatch must be fatal even under --force, got nil")
	}
	if !errors.Is(err, accel.ErrChecksumMismatch) {
		t.Fatalf("expected the error to wrap ErrChecksumMismatch, got: %v", err)
	}
	if exitCodeFor(err) == 0 {
		t.Fatalf("checksum mismatch must be non-zero exit under --force")
	}
	if rec.handoffCalled {
		t.Fatal("no handoff should ever happen after a checksum mismatch")
	}
}

// Fix 2: --no-verify on a cask WITH a matching checksum (fetch returns nil) →
// bytes WERE verified, so no unverified warning and no "(installed unverified)"
// note. The success line is suppressed here (buffer stdout is non-TTY), so we
// assert the warning's absence on stderr and that the note never appears.
func TestNoVerify_ChecksumMatched_NotUnverified(t *testing.T) {
	rec := &recorder{}
	d, out, errBuf := fakeDeps(rec, sampleCask())
	// Default fetch returns nil → checksum matched.

	err := runInstall(context.Background(), d, postureFlags{noVerify: true}, "orpheus-nightly")
	if err != nil {
		t.Fatalf("--no-verify with a matching checksum should succeed, got: %v", err)
	}
	if !rec.handoffCalled {
		t.Fatal("expected handoff on a successful verified install")
	}
	if strings.Contains(errBuf.String(), "WARNING") || strings.Contains(errBuf.String(), "NOT checked") {
		t.Fatalf("bytes WERE verified; no unverified warning must be printed, got: %q", errBuf.String())
	}
	if strings.Contains(out.String(), "installed unverified") {
		t.Fatalf("success note must not claim unverified when the checksum matched, got: %q", out.String())
	}
}

// Resolve cancel → clean (zero) exit, nothing fetched.
func TestResolveCancelled_CleanExit(t *testing.T) {
	rec := &recorder{}
	d, _, _ := fakeDeps(rec, sampleCask())
	d.resolve = func(string, resolve.Options) (string, *brew.Cask, error) {
		return "", nil, resolve.ErrCancelled
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
	d.resolve = func(string, resolve.Options) (string, *brew.Cask, error) {
		return "", nil, resolve.ErrNoInteractiveResolve
	}

	err := runInstall(context.Background(), d, postureFlags{noInput: true}, "orpheus")
	if exitCodeFor(err) == 0 {
		t.Fatal("no-interactive resolve must exit non-zero")
	}
	if rec.fetchCalled {
		t.Fatal("nothing should be fetched when resolution fails")
	}
}

// Change 1: aria2 progress presentation follows the TTY. runInstall sets
// accel.Params.Interactive = !quiet && isTTY(stderr). The default fakeDeps wires
// stderr to a *bytes.Buffer (not a *os.File), so isTTY(stderr) is false and the
// recorded fetch params must carry Interactive=false — the non-TTY path. With
// --quiet it is false regardless.
//
// Note: the Interactive=true branch requires a real terminal on stderr, which a
// unit test cannot fake (isTTY only returns true for an *os.File whose Stat
// reports os.ModeCharDevice). We therefore assert only the two deterministically
// reachable cases — non-TTY and --quiet — which is the testable half. The
// TTY-true wiring is exercised via the isTTY unit and by manual/integration use.
func TestFetch_InteractiveFlag_FollowsTTY(t *testing.T) {
	// Non-TTY stderr (a buffer) → Interactive must be false.
	rec := &recorder{}
	d, _, _ := fakeDeps(rec, sampleCask())

	if err := runInstall(context.Background(), d, postureFlags{}, "orpheus-nightly"); err != nil {
		t.Fatalf("happy path should succeed, got: %v", err)
	}
	if !rec.fetchCalled {
		t.Fatal("expected fetch to be called")
	}
	if rec.fetchParams.Interactive {
		t.Fatalf("Interactive must be false when stderr is not a TTY, got true")
	}

	// --quiet → Interactive must be false regardless of the stderr kind.
	recQ := &recorder{}
	dQ, _, _ := fakeDeps(recQ, sampleCask())

	if err := runInstall(context.Background(), dQ, postureFlags{quiet: true}, "orpheus-nightly"); err != nil {
		t.Fatalf("--quiet happy path should succeed, got: %v", err)
	}
	if !recQ.fetchCalled {
		t.Fatal("expected fetch to be called under --quiet")
	}
	if recQ.fetchParams.Interactive {
		t.Fatalf("Interactive must be false under --quiet, got true")
	}
}

// Change 2 (the important one): with no TTY on stdin, runInstall pins
// HOMEBREW_NO_INTERACTIVE=1 for the brew child so brew fails fast instead of
// hanging on a prompt nobody can answer. The default fakeDeps leaves
// stdinIsTTY=false (its zero value), so a successful handoff must set the env.
//
// Env isolation: runInstall calls os.Setenv directly (not t.Setenv), which
// mutates global process env and would leak across tests. t.Setenv here
// establishes a known "" baseline AND registers automatic restore of the
// original value at test end, so the mutation runInstall makes cannot leak.
func TestHandoff_NonInteractiveEnv_WhenNoTTY(t *testing.T) {
	t.Setenv("HOMEBREW_NO_INTERACTIVE", "") // known baseline + auto-restore after the test

	rec := &recorder{}
	d, _, _ := fakeDeps(rec, sampleCask())
	// Default fakeDeps: not installed, slow host, valid fetch, stdinIsTTY=false.
	if d.stdinIsTTY {
		t.Fatal("precondition: default fakeDeps must have stdinIsTTY=false")
	}

	if err := runInstall(context.Background(), d, postureFlags{}, "orpheus-nightly"); err != nil {
		t.Fatalf("happy path should succeed, got: %v", err)
	}
	if !rec.handoffCalled {
		t.Fatal("expected a handoff so the env pin is exercised")
	}
	if got := os.Getenv("HOMEBREW_NO_INTERACTIVE"); got != "1" {
		t.Fatalf("HOMEBREW_NO_INTERACTIVE = %q, want \"1\" when stdin is not a TTY", got)
	}
}

// Change 2 (interactive half): with a TTY on stdin, brewfast must NOT force
// non-interactive — a cask that legitimately needs a login password can still
// prompt. Assert the env is left unset (still the "" baseline).
//
// Env isolation: same as above — t.Setenv gives a clean "" baseline and auto
// restores the original HOMEBREW_NO_INTERACTIVE when the test ends.
func TestHandoff_Interactive_LeavesEnvUnset_WhenTTY(t *testing.T) {
	t.Setenv("HOMEBREW_NO_INTERACTIVE", "") // known baseline + auto-restore after the test

	rec := &recorder{}
	d, _, _ := fakeDeps(rec, sampleCask())
	d.stdinIsTTY = true // pretend a terminal is present on stdin

	if err := runInstall(context.Background(), d, postureFlags{}, "orpheus-nightly"); err != nil {
		t.Fatalf("happy path should succeed, got: %v", err)
	}
	if !rec.handoffCalled {
		t.Fatal("expected a handoff to run")
	}
	if got := os.Getenv("HOMEBREW_NO_INTERACTIVE"); got != "" {
		t.Fatalf("HOMEBREW_NO_INTERACTIVE = %q, want \"\" (unset) when a TTY is present", got)
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
