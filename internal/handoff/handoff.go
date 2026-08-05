// Package handoff runs the `brew` install/upgrade child so a single interrupt
// to brewfast never leaves brew mid-transaction (R23, KTD-6, flow F6).
//
// The incident this package exists to prevent: a `brew` process killed
// mid-transaction leaving a wedged cask. The root cause is subtle. A terminal
// Ctrl-C is delivered by the TTY to the entire foreground *process group*, so a
// `brew` child that shares brewfast's process group is killed by the kernel
// directly — brewfast's own signal handler never gets a say. Trapping the
// signal in brewfast is necessary but NOT sufficient on its own.
//
// The mechanism that actually works (KTD-6):
//
//  1. Start `brew` with SysProcAttr{Setpgid: true} so it lives in its OWN
//     process group. Now a TTY-delivered SIGINT reaches only brewfast, not the
//     brew child.
//  2. Trap SIGINT/SIGTERM/SIGHUP in brewfast. On the FIRST signal, do NOT
//     forward any kill to the child and do NOT exit — print a notice and keep
//     waiting on the child to complete.
//  3. On a SECOND signal, stop swallowing and allow default termination.
//  4. After brew completes, propagate its exit status.
//
// SIGHUP is included deliberately: closing the terminal is a real-world wedge
// vector (the originating incident class), not just an interactive Ctrl-C.
//
// Step 1 has a consequence that must be paid for explicitly: a process in a
// background group that READS the terminal is stopped by SIGTTIN. Left
// unaddressed, brew's interactive prompts ("Do you want to proceed with the
// upgrade? [y/n]") print and then hang forever. So the handoff also transfers
// terminal foreground ownership to brew's group for the duration of the run and
// restores it afterwards — see terminal.go. While brew holds the terminal, a
// Ctrl-C goes to brew (which aborts its own prompt cleanly) rather than to
// brewfast; the guard above still covers every signal delivered to brewfast
// itself.
//
// Full daemon-style detachment (surviving SIGKILL of brewfast) is out of scope
// and deferred; SIGKILL and a deliberate second interrupt may still terminate
// the child at the OS level. That is documented, not silently swallowed.
package handoff

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/amitray007/brewfast/internal/brew"
)

// trappedSignals are the signals brewfast guards against during a handoff.
// SIGINT: interactive Ctrl-C. SIGTERM: a timeout / `kill`. SIGHUP: terminal
// close — a likely real-world wedge vector.
var trappedSignals = []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP}

// firstInterruptNotice is printed to stderr on the first trapped signal, so the
// user understands why Ctrl-C did not stop the install and how to force it.
const firstInterruptNotice = "brew is installing; not interrupting (press again to force, may corrupt)"

// Handoff supervises a single `brew` child through to completion, guarding it
// against interrupts. The zero value is ready to use and runs the real `brew`
// binary; tests override newCmd, notify, and out to exercise the signal and
// process-group logic without invoking real brew.
type Handoff struct {
	// newCmd builds the child command. It defaults (when nil) to a real `brew`
	// invocation with its own process group (Setpgid). Tests inject a factory
	// that builds a fake long-running child — also with its own process group —
	// so a group-delivered signal can be shown NOT to kill it.
	newCmd func(name string, args ...string) *exec.Cmd

	// notify installs a signal handler on ch. It defaults (when nil) to
	// signal.Notify. Tests inject a no-op and drive ch directly.
	notify func(ch chan<- os.Signal, sig ...os.Signal)

	// stop uninstalls the signal handler. It defaults (when nil) to
	// signal.Stop.
	stop func(ch chan<- os.Signal)

	// out receives the first-interrupt notice. Defaults (when nil) to
	// os.Stderr. Tests capture it to assert the swallow path was taken.
	out io.Writer

	// afterStart, if non-nil, is called with the started child command
	// immediately after a successful Start (before Wait). It is a test seam that
	// exposes the child's PID/process-group so a test can deliver a real signal
	// to the child's own group. Production leaves it nil.
	afterStart func(cmd *exec.Cmd)
}

// New returns a Handoff configured to run the real `brew` binary with the
// production signal handling. Callers who need to inject test doubles construct
// a Handoff literal directly (all fields are within-package).
func New() *Handoff {
	return &Handoff{}
}

// realBrewCmd builds the production child: `brew <handoff args>` in its own
// process group, with stdio wired to the current process so the user sees
// brew's live output and can still answer any brew prompt.
//
// Setpgid is THE load-bearing line: without it, brew shares brewfast's process
// group and a TTY Ctrl-C kills brew directly, defeating every bit of the signal
// handling below.
func realBrewCmd(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

func (h *Handoff) cmdFactory() func(name string, args ...string) *exec.Cmd {
	if h.newCmd != nil {
		return h.newCmd
	}
	return realBrewCmd
}

func (h *Handoff) notifyFunc() func(ch chan<- os.Signal, sig ...os.Signal) {
	if h.notify != nil {
		return h.notify
	}
	return signal.Notify
}

func (h *Handoff) stopFunc() func(ch chan<- os.Signal) {
	if h.stop != nil {
		return h.stop
	}
	return signal.Stop
}

func (h *Handoff) writer() io.Writer {
	if h.out != nil {
		return h.out
	}
	return os.Stderr
}

// Run performs the guarded handoff: it starts `brew <op> --cask -- <name>` in
// its own process group, traps interrupts (swallowing the first, allowing
// default behaviour on the second), waits for brew to finish, and returns
// brew's exit status.
//
// The returned error is nil on a clean (exit 0) brew run. A non-zero brew exit
// is returned as the *exec.ExitError from Wait, so callers can propagate brew's
// exit code (see ExitCode).
//
// ctx is accepted for signature parity with brew.Runner and future use, but is
// deliberately NOT wired to cancel-kill the child: the entire purpose of this
// package is that an interrupt must not abort brew mid-transaction, so Run never
// uses exec.CommandContext's cancel-on-done path. Cancelling ctx does not kill
// brew; only a second deliberate interrupt (or SIGKILL) can.
func (h *Handoff) Run(ctx context.Context, op brew.Operation, name string) error {
	_ = ctx // intentionally not used to cancel the child; see doc above.
	if err := brew.ValidateName(name); err != nil {
		return err
	}
	args := brew.HandoffArgs(op, name)

	cmd := h.cmdFactory()("brew", args...)

	// Install the signal handler BEFORE starting the child. If Notify ran after
	// Start, a SIGINT/SIGTERM/SIGHUP delivered in that window would hit
	// brewfast's DEFAULT disposition: brewfast would die without a notice, and
	// because the child runs in its own process group (Setpgid), it would be
	// orphaned and keep installing unsupervised — the exact incident this package
	// exists to prevent. Trapping first guarantees no trapped signal ever has a
	// window under the default disposition.
	//
	// Buffered so a signal arriving between Notify and our select — including one
	// that lands during Start, before supervise begins — is not lost.
	sigCh := make(chan os.Signal, 4)
	h.notifyFunc()(sigCh, trappedSignals...)
	defer h.stopFunc()(sigCh)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting brew handoff: %w", err)
	}

	// Hand the controlling terminal to brew's process group. Setpgid above put
	// brew in a BACKGROUND group, and a background process that reads the
	// terminal is stopped by SIGTTIN — so without this, any interactive brew
	// prompt ("Do you want to proceed with the upgrade? [y/n]") would print and
	// then hang forever with the user's keystrokes going to the shell instead.
	// See terminal.go for the full rationale and the Ctrl-C trade-off.
	//
	// Restored on every exit path (including the forced-abort one) so the user's
	// shell is never left as a background group on a terminal it cannot read.
	term := acquireTerminal(cmd.Process.Pid)
	defer term.restore()

	if h.afterStart != nil {
		h.afterStart(cmd)
	}

	waitErr := supervise(cmd.Wait, sigCh, h.writer(), term.restore)
	return waitErr
}

// supervise waits for the child (via wait) while guarding against interrupts on
// sigCh. It is the pure-ish heart of the package, split out from Run so it can
// be unit-tested with a fake wait func and a hand-driven signal channel — no
// real process required.
//
// Behaviour:
//   - It runs wait in a goroutine and selects between the child finishing and a
//     signal arriving.
//   - On the FIRST signal, it does NOT return and does NOT kill the child: it
//     prints the notice to out and keeps waiting. This is the swallow that keeps
//     brew alive through a single Ctrl-C.
//   - On a SECOND signal, it stops swallowing. It re-raises the signal to the
//     default disposition so the process terminates as it normally would — i.e.
//     brewfast stops guarding and lets the user force the abort.
//   - When wait returns first, supervise returns wait's error (nil on success,
//     the *exec.ExitError carrying a non-zero code otherwise).
//
// beforeForce, if non-nil, runs immediately before the forced-abort path
// terminates the process. forceDefault kills brewfast via a signal, which does
// NOT unwind the stack or run deferred functions — so terminal ownership must be
// handed back here explicitly, or the user's shell is left as a background group
// on a terminal it cannot read from (a wedged shell).
func supervise(wait func() error, sigCh <-chan os.Signal, out io.Writer, beforeForce func()) error {
	done := make(chan error, 1)
	go func() { done <- wait() }()

	swallowed := false
	for {
		select {
		case err := <-done:
			return err
		case sig := <-sigCh:
			if !swallowed {
				// First interrupt: guard the transaction. Do not kill, do not
				// return — keep waiting on the child.
				swallowed = true
				fmt.Fprintln(out, firstInterruptNotice)
				continue
			}
			// Second interrupt: stop swallowing and allow default termination.
			// Restore the default disposition and re-deliver the signal to
			// ourselves so the process dies as it normally would.
			if beforeForce != nil {
				beforeForce()
			}
			forceDefault(sig)
			// If forceDefault could not terminate us (non-unix / unusual
			// signal), fall through and keep waiting on the child rather than
			// silently returning success.
		}
	}
}

// forceDefault stops guarding signal sig: it resets sig to its default
// disposition and re-raises it against brewfast's own process, so the second
// interrupt terminates brewfast exactly as an unguarded Ctrl-C would have. This
// is the "press again to force" path.
func forceDefault(sig os.Signal) {
	s, ok := sig.(syscall.Signal)
	if !ok {
		return
	}
	// Reset to default handling, then re-raise against our own process.
	signal.Reset(sig)
	_ = syscall.Kill(syscall.Getpid(), s)
}

// ExitCode extracts the process exit code from an error returned by Run. It
// returns 0 for a nil error, the child's exit code for an *exec.ExitError, and
// 1 for any other (non-exit) error so callers always have a usable exit status
// to propagate.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return 1
}
