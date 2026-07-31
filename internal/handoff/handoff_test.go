package handoff

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/amitray007/brewfast/internal/brew"
)

// TestHelperProcess is not a real test. It is re-executed as the fake `brew`
// child by the process-group integration tests (the standard Go pattern:
// os.Args[0] is the test binary, gated on an env var).
//
// It optionally IGNORES the trapped signals (SIGINT/SIGTERM/SIGHUP) so that the
// integration tests can deliver a real signal directly to the child's OWN
// process group and still prove the child survives to completion. Ignoring the
// signal is the child's contract for "a stray interrupt to my group must not
// wedge me mid-transaction"; the parent-side guarantee (that a TTY interrupt to
// brewfast's group never even reaches this separate group) is proven by the
// process-group split under test.
//
// It prints a "started" marker so the parent can synchronize before delivering
// a signal, sleeps for a duration, writes a completion marker, then exits with a
// configurable code — the marker's existence proves it was not killed mid-run.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("BREWFAST_HELPER_PROCESS") != "1" {
		return
	}

	if os.Getenv("BREWFAST_HELPER_IGNORE_SIGNALS") == "1" {
		signal.Ignore(syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	}

	sleepMS, _ := strconv.Atoi(os.Getenv("BREWFAST_HELPER_SLEEP_MS"))
	code, _ := strconv.Atoi(os.Getenv("BREWFAST_HELPER_EXIT"))
	marker := os.Getenv("BREWFAST_HELPER_MARKER")

	os.Stdout.WriteString("started\n")
	os.Stdout.Sync()

	time.Sleep(time.Duration(sleepMS) * time.Millisecond)

	if marker != "" {
		_ = os.WriteFile(marker, []byte("completed"), 0o644)
	}
	os.Exit(code)
}

type helperOpts struct {
	sleepMS  int
	exitCode int
	marker   string
	// ignoreSignals makes the child ignore trapped signals — used by the
	// group-delivery tests that signal the child's own process group.
	ignoreSignals bool
}

// helperCmdFactory returns a newCmd factory that re-execs this test binary as
// the fake brew child (TestHelperProcess) in its OWN process group (Setpgid),
// mirroring production realBrewCmd. startedCh is signalled when the child prints
// its "started" line.
func helperCmdFactory(o helperOpts, startedCh chan struct{}) func(string, ...string) *exec.Cmd {
	return func(_ string, _ ...string) *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess")
		env := append(os.Environ(),
			"BREWFAST_HELPER_PROCESS=1",
			"BREWFAST_HELPER_SLEEP_MS="+strconv.Itoa(o.sleepMS),
			"BREWFAST_HELPER_EXIT="+strconv.Itoa(o.exitCode),
			"BREWFAST_HELPER_MARKER="+o.marker,
		)
		if o.ignoreSignals {
			env = append(env, "BREWFAST_HELPER_IGNORE_SIGNALS=1")
		}
		cmd.Env = env
		// Own process group — the load-bearing property under test.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

		// StdoutPipe hands ownership of the write end to exec, which closes it
		// safely after Start — avoiding the fd race a hand-rolled os.Pipe closed
		// from a goroutine would create.
		pr, err := cmd.StdoutPipe()
		if err != nil {
			panic("handoff test: StdoutPipe: " + err.Error())
		}
		go func() {
			buf := make([]byte, 64)
			for {
				n, rerr := pr.Read(buf)
				if n > 0 && strings.Contains(string(buf[:n]), "started") {
					select {
					case startedCh <- struct{}{}:
					default:
					}
				}
				if rerr != nil {
					return
				}
			}
		}()
		return cmd
	}
}

func waitStarted(t *testing.T, ch chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for fake child to report started")
	}
}

// signalChildGroup delivers sig to the child's OWN process group (negative
// pgid). Because the child was started with Setpgid, its pgid equals its pid.
// This is exactly a TTY-style group delivery, aimed precisely at the child's
// isolated group so it never touches the test runner's group.
func signalChildGroup(t *testing.T, cmd *exec.Cmd, sig syscall.Signal) {
	t.Helper()
	pid := cmd.Process.Pid
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		t.Fatalf("getpgid(%d): %v", pid, err)
	}
	if pgid == syscall.Getpgrp() {
		t.Fatalf("child shares the test's process group (pgid=%d) — Setpgid isolation failed", pgid)
	}
	if err := syscall.Kill(-pgid, sig); err != nil {
		t.Fatalf("kill(-%d, %v): %v", pgid, sig, err)
	}
}

// TestRun_GroupSignalDoesNotKillChild is the incident-critical test (the RED
// test from the execution note). It delivers SIGINT to the CHILD'S OWN PROCESS
// GROUP mid-run — the same delivery mode a TTY uses on Ctrl-C — and asserts the
// child still runs to completion and Run returns success.
//
// A PID-only variant of this test would pass even without Setpgid; delivering to
// the process GROUP is what exercises the real TTY path and proves the isolation
// that closes the incident's root cause.
func TestRun_GroupSignalDoesNotKillChild(t *testing.T) {
	requireUnix(t)
	testGroupSignalSurvives(t, syscall.SIGINT, brew.OpReinstall)
}

// TestRun_SIGHUPDoesNotKillChild repeats the group-delivery proof for SIGHUP —
// terminal close, a likely real-world wedge vector.
func TestRun_SIGHUPDoesNotKillChild(t *testing.T) {
	requireUnix(t)
	testGroupSignalSurvives(t, syscall.SIGHUP, brew.OpUpgrade)
}

func testGroupSignalSurvives(t *testing.T, sig syscall.Signal, op brew.Operation) {
	t.Helper()
	marker := t.TempDir() + "/completed"
	started := make(chan struct{}, 1)

	cmdCh := make(chan *exec.Cmd, 1)
	h := &Handoff{
		newCmd: helperCmdFactory(helperOpts{sleepMS: 600, marker: marker, ignoreSignals: true}, started),
		// Real brewfast signal handling: swallow first interrupt, keep waiting.
		out:        &bytes.Buffer{},
		afterStart: func(cmd *exec.Cmd) { cmdCh <- cmd },
	}

	errCh := make(chan error, 1)
	go func() { errCh <- h.Run(context.Background(), op, "orpheus-nightly") }()

	cmd := <-cmdCh
	waitStarted(t, started)

	signalChildGroup(t, cmd, sig)

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run returned error after group %v; child should have completed: %v", sig, err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("Run did not return; child may have been killed or hung")
	}

	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("child did not complete (marker missing): %v", err)
	}
}

// TestRun_PIDSignalToBrewfastSwallowed exercises brewfast's own handler path:
// a signal delivered to brewfast's PID (this process) during the handoff is
// swallowed on first receipt, the child completes, and Run returns success. The
// notice is printed to stderr.
func TestRun_PIDSignalToBrewfastSwallowed(t *testing.T) {
	requireUnix(t)

	marker := t.TempDir() + "/completed"
	started := make(chan struct{}, 1)
	var out bytes.Buffer

	h := &Handoff{
		newCmd: helperCmdFactory(helperOpts{sleepMS: 400, marker: marker, ignoreSignals: true}, started),
		out:    &out,
	}

	errCh := make(chan error, 1)
	go func() { errCh <- h.Run(context.Background(), brew.OpReinstall, "orpheus-nightly") }()

	waitStarted(t, started)

	// Deliver SIGINT to brewfast itself (this test process). Run installed a
	// real signal.Notify handler, so this reaches the swallow path.
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGINT); err != nil {
		t.Fatalf("delivering SIGINT to brewfast PID: %v", err)
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run returned error after PID SIGINT; expected swallow+complete: %v", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("Run did not return after PID SIGINT")
	}

	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("child did not complete after PID SIGINT (marker missing): %v", err)
	}
	if !strings.Contains(out.String(), firstInterruptNotice) {
		t.Fatalf("expected first-interrupt notice on stderr, got %q", out.String())
	}
}

// TestRun_NotifyInstalledBeforeChildStarts is the regression test for the P1
// ordering defect: cmd.Start() must NOT run before the signal handler is
// installed. If Start ran first, a trapped signal delivered in that window would
// hit brewfast's default disposition — brewfast dies with no notice and, because
// the child runs in its own process group (Setpgid), the child is orphaned and
// keeps installing unsupervised.
//
// Delivering a signal precisely inside that window is not deterministically
// timeable, so the invariant is asserted structurally: using the notify and
// afterStart seams, record the order of events and assert Notify happened before
// the child was observably started. The buffered channel already makes moving
// Notify earlier safe (a signal between Notify and the select is retained), so
// ordering is both necessary and sufficient for the guarantee.
func TestRun_NotifyInstalledBeforeChildStarts(t *testing.T) {
	requireUnix(t)

	var (
		mu           sync.Mutex
		order        []string
		notifyCalled bool
	)
	record := func(event string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, event)
	}

	started := make(chan struct{}, 1)
	h := &Handoff{
		newCmd: helperCmdFactory(helperOpts{sleepMS: 30, exitCode: 0}, started),
		notify: func(ch chan<- os.Signal, sig ...os.Signal) {
			mu.Lock()
			notifyCalled = true
			mu.Unlock()
			record("notify")
			// Delegate to the real handler so the installed trap is genuine —
			// this is the true production seam, not a no-op.
			signal.Notify(ch, sig...)
		},
		stop: func(ch chan<- os.Signal) { signal.Stop(ch) },
		out:  &bytes.Buffer{},
		afterStart: func(cmd *exec.Cmd) {
			mu.Lock()
			seen := notifyCalled
			mu.Unlock()
			if !seen {
				t.Errorf("child started before signal handler was installed: a trapped signal in this window would orphan the child")
			}
			record("start")
		},
	}

	if err := h.Run(context.Background(), brew.OpReinstall, "orpheus-nightly"); err != nil {
		t.Fatalf("clean run returned error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != "notify" || order[1] != "start" {
		t.Fatalf("expected notify to precede child start; got order %v", order)
	}
}

// TestRun_CleanExitPropagatesStatus proves a clean handoff (no signal) forwards
// the child's exit status: success stays success, and a non-zero child exit is
// returned as an *exec.ExitError with the code preserved.
func TestRun_CleanExitPropagatesStatus(t *testing.T) {
	requireUnix(t)

	t.Run("success", func(t *testing.T) {
		started := make(chan struct{}, 1)
		h := &Handoff{
			newCmd: helperCmdFactory(helperOpts{sleepMS: 30, exitCode: 0}, started),
			notify: func(ch chan<- os.Signal, _ ...os.Signal) {},
			stop:   func(ch chan<- os.Signal) {},
			out:    &bytes.Buffer{},
		}
		err := h.Run(context.Background(), brew.OpReinstall, "orpheus-nightly")
		if err != nil {
			t.Fatalf("expected nil error on clean success, got %v", err)
		}
		if code := ExitCode(err); code != 0 {
			t.Fatalf("expected exit code 0, got %d", code)
		}
	})

	t.Run("nonzero", func(t *testing.T) {
		started := make(chan struct{}, 1)
		h := &Handoff{
			newCmd: helperCmdFactory(helperOpts{sleepMS: 30, exitCode: 7}, started),
			notify: func(ch chan<- os.Signal, _ ...os.Signal) {},
			stop:   func(ch chan<- os.Signal) {},
			out:    &bytes.Buffer{},
		}
		err := h.Run(context.Background(), brew.OpReinstall, "orpheus-nightly")
		if err == nil {
			t.Fatal("expected error for non-zero child exit, got nil")
		}
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("expected *exec.ExitError, got %T: %v", err, err)
		}
		if code := ExitCode(err); code != 7 {
			t.Fatalf("expected exit code 7 propagated, got %d", code)
		}
	})
}

// TestRun_RejectsInvalidName ensures a name that fails brew's grammar never
// reaches an exec — a leading-dash name (which could be read as a flag) is
// rejected up front.
func TestRun_RejectsInvalidName(t *testing.T) {
	called := false
	h := &Handoff{
		newCmd: func(string, ...string) *exec.Cmd { called = true; return exec.Command("true") },
		notify: func(ch chan<- os.Signal, _ ...os.Signal) {},
		stop:   func(ch chan<- os.Signal) {},
		out:    &bytes.Buffer{},
	}
	err := h.Run(context.Background(), brew.OpReinstall, "--version")
	if err == nil {
		t.Fatal("expected invalid-name error, got nil")
	}
	if !errors.Is(err, brew.ErrInvalidName) {
		t.Fatalf("expected ErrInvalidName, got %v", err)
	}
	if called {
		t.Fatal("newCmd was invoked for an invalid name; validation must precede exec")
	}
}

// --- Unit tests of the swallow-first-then-default logic (no real process) ---

// TestSupervise_FirstSignalSwallowed asserts the pure supervise loop does NOT
// return (does not abort) on the first signal while the child is still running,
// and that it prints the interrupt notice exactly once.
func TestSupervise_FirstSignalSwallowed(t *testing.T) {
	sigCh := make(chan os.Signal, 4)
	release := make(chan struct{})
	out := &syncBuffer{}

	wait := func() error { <-release; return nil }

	done := make(chan error, 1)
	go func() { done <- supervise(wait, sigCh, out) }()

	sigCh <- syscall.SIGINT

	select {
	case err := <-done:
		t.Fatalf("supervise returned on first signal (should swallow); err=%v", err)
	case <-time.After(200 * time.Millisecond):
		// Good: still waiting on the child.
	}

	if got := out.String(); !strings.Contains(got, firstInterruptNotice) {
		t.Fatalf("expected first-interrupt notice, got %q", got)
	}

	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected nil after child completes, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("supervise did not return after child completed")
	}
}

// TestSupervise_SecondSignalStopsSwallowing asserts the second signal takes the
// default-termination path rather than swallowing again. To keep the test
// binary alive, SIGINT is ignored for the duration so forceDefault's re-raise is
// a no-op; the assertion is that the notice is printed exactly once (only the
// first signal is swallowed) and supervise still returns when the child ends.
func TestSupervise_SecondSignalStopsSwallowing(t *testing.T) {
	requireUnix(t)

	signal.Ignore(syscall.SIGINT)
	defer signal.Reset(syscall.SIGINT)

	sigCh := make(chan os.Signal, 4)
	release := make(chan struct{})
	out := &syncBuffer{}

	wait := func() error { <-release; return nil }

	done := make(chan error, 1)
	go func() { done <- supervise(wait, sigCh, out) }()

	sigCh <- syscall.SIGINT // first: swallowed
	time.Sleep(50 * time.Millisecond)
	if n := strings.Count(out.String(), firstInterruptNotice); n != 1 {
		t.Fatalf("expected exactly one notice after first signal, got %d (%q)", n, out.String())
	}

	sigCh <- syscall.SIGINT // second: default path (re-raise ignored, so no-op)
	time.Sleep(50 * time.Millisecond)
	if n := strings.Count(out.String(), firstInterruptNotice); n != 1 {
		t.Fatalf("second signal must not print the notice again; notices=%d", n)
	}

	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("supervise did not return after child completed post-second-signal")
	}
}

// TestExitCode covers the exit-status extraction helper for the three shapes
// U4 will propagate.
func TestExitCode(t *testing.T) {
	if got := ExitCode(nil); got != 0 {
		t.Fatalf("nil error → want 0, got %d", got)
	}
	if got := ExitCode(errors.New("boom")); got != 1 {
		t.Fatalf("non-exit error → want 1, got %d", got)
	}
	// A real *exec.ExitError with a known code.
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess")
	cmd.Env = append(os.Environ(),
		"BREWFAST_HELPER_PROCESS=1",
		"BREWFAST_HELPER_SLEEP_MS=0",
		"BREWFAST_HELPER_EXIT=5",
	)
	err := cmd.Run()
	if got := ExitCode(err); got != 5 {
		t.Fatalf("exec.ExitError → want 5, got %d (err=%v)", got, err)
	}
}

func requireUnix(t *testing.T) {
	t.Helper()
	switch runtime.GOOS {
	case "windows", "js", "plan9":
		t.Skipf("signal/process-group test not applicable on %s", runtime.GOOS)
	}
}

// syncBuffer is a goroutine-safe io.Writer used by the supervise unit tests,
// where supervise writes the notice from its own goroutine while the test reads
// it — access must be synchronized to be race-free under `go test -race`.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
