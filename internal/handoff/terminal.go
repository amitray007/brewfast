//go:build unix

package handoff

import (
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"
)

// termOwner transfers controlling-terminal ownership to the brew child's process
// group for the duration of the handoff, and restores it afterwards.
//
// Why this is required (the bug it fixes):
//
// realBrewCmd starts brew with Setpgid so a TTY Ctrl-C cannot kill it
// mid-transaction. That leaves brew in a process group which is NOT the
// terminal's foreground group. POSIX says a process in a BACKGROUND process
// group that reads from the controlling terminal is sent SIGTTIN, whose default
// action is to STOP it. So brew would print its prompt (writes are allowed —
// TOSTOP is off by default) and then suspend the instant it tried to read the
// answer:
//
//	==> Do you want to proceed with the upgrade? [y/n]
//	<brew stopped by SIGTTIN; the user's "y" goes to the shell instead>
//
// Wiring cmd.Stdin = os.Stdin is necessary but NOT sufficient: owning the fd is
// a different thing from being the terminal's foreground process group. This
// type supplies the missing half via tcsetpgrp(3).
//
// Interaction with the interrupt guard: while brew is the foreground group, a
// Ctrl-C is delivered by the TTY to BREW, not to brewfast. That is the correct
// trade-off — a user answering an interactive prompt must be able to abort at
// that prompt, and brew handles SIGINT at its own prompts cleanly (Ask.confirm?
// treats Ctrl-C as "abort"). brewfast's swallow-the-first-interrupt guard still
// covers every signal routed to brewfast itself (SIGTERM, SIGHUP, and any
// SIGINT arriving once the terminal has been handed back).
type termOwner struct {
	tty  *os.File
	prev int
	// active reports whether ownership was actually transferred, so restore is a
	// no-op when acquisition failed or there was no terminal at all.
	active bool
}

// acquireTerminal makes pgid the foreground process group of the controlling
// terminal. It is a no-op (returning an inactive termOwner, never an error) when
// there is no controlling terminal — the non-interactive CI/piped case, where
// there is no prompt to answer and nothing to hand over.
//
// SIGTTOU must be ignored across the tcsetpgrp call: a process in a background
// group calling tcsetpgrp is itself sent SIGTTOU, which would stop brewfast —
// trading brew's hang for brewfast's. Ignoring it makes the call succeed
// regardless of which group brewfast is in.
func acquireTerminal(pgid int) *termOwner {
	t := &termOwner{}

	// Open the controlling terminal directly rather than trusting fd 0: stdin may
	// be redirected while a terminal is still present (`brewfast x < /dev/null`),
	// and vice versa.
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return t // No controlling terminal: nothing to transfer.
	}

	signal.Ignore(syscall.SIGTTOU)

	prev, err := unix.IoctlGetInt(int(tty.Fd()), unix.TIOCGPGRP)
	if err != nil {
		signal.Reset(syscall.SIGTTOU)
		tty.Close()
		return t
	}

	if err := unix.IoctlSetPointerInt(int(tty.Fd()), unix.TIOCSPGRP, pgid); err != nil {
		// Could not hand over the terminal. Leave ownership untouched: brew may
		// still stop if it prompts, but that is strictly no worse than before and
		// far better than aborting an install that probably will not prompt.
		signal.Reset(syscall.SIGTTOU)
		tty.Close()
		return t
	}

	t.tty, t.prev, t.active = tty, prev, true
	return t
}

// restore returns terminal ownership to the group that held it before the
// handoff. It is safe to call on an inactive termOwner and safe to call twice.
//
// This MUST run before brewfast exits: leaving a dead child's process group as
// the terminal's foreground group leaves the user's shell in the background,
// where its own reads would raise SIGTTIN — a wedged terminal.
func (t *termOwner) restore() {
	if !t.active {
		return
	}
	t.active = false
	// SIGTTOU is still ignored here, which is what allows this call to succeed
	// now that brewfast is itself a background group.
	_ = unix.IoctlSetPointerInt(int(t.tty.Fd()), unix.TIOCSPGRP, t.prev)
	signal.Reset(syscall.SIGTTOU)
	t.tty.Close()
}
