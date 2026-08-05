//go:build darwin || linux

package handoff

import (
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
)

// TestTerminalPromptRegression is the regression test for the interactive-prompt
// hang: brew printed "Do you want to proceed with the upgrade? [y/n]" and then
// froze, because Setpgid left it in a background process group and a background
// read from the terminal raises SIGTTIN (default action: stop the process).
//
// The test builds the exact conditions with a real PTY:
//   - a child in its OWN process group (as realBrewCmd creates), which
//   - reads a byte from the controlling terminal (as brew's prompt does).
//
// With acquireTerminal the child reads the byte and exits 0. WITHOUT it the
// child is stopped by SIGTTIN and this test times out — which is precisely the
// reported bug.
func TestTerminalPromptRegression(t *testing.T) {
	if os.Getenv("BREWFAST_TTY_CHILD") == "1" {
		ttyChildMain()
		return
	}

	ptmx, slave, err := pty.Open()
	if err != nil {
		t.Skipf("cannot allocate a PTY in this environment: %v", err)
	}
	defer ptmx.Close()
	defer slave.Close()

	// The child must have the pty slave as its controlling terminal, which
	// requires it to be a session leader (Setsid) with Setctty. This models the
	// user's real shell session; brewfast itself inherits one from the terminal.
	cmd := exec.Command(os.Args[0], "-test.run=TestTerminalPromptRegression")
	cmd.Env = append(os.Environ(), "BREWFAST_TTY_CHILD=1")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}

	if err := cmd.Start(); err != nil {
		t.Fatalf("starting pty session leader: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	// Stream pty output, and press "y" only once the reader says it is about to
	// read. Writing earlier races the reader's startup: the byte would be
	// consumed from the line discipline before the read is posted, and the test
	// would hang for a reason unrelated to what it is meant to prove.
	out := make(chan string, 1)
	ready := make(chan struct{})
	go func() {
		buf := make([]byte, 4096)
		var sb strings.Builder
		signalled := false
		for {
			n, rerr := ptmx.Read(buf)
			if n > 0 {
				sb.Write(buf[:n])
				if !signalled && strings.Contains(sb.String(), readerReadyMarker) {
					signalled = true
					close(ready)
				}
			}
			if rerr != nil {
				break
			}
		}
		out <- sb.String()
	}()

	go func() {
		select {
		case <-ready:
		case <-time.After(10 * time.Second):
		}
		// A newline is required because this reader uses a plain (canonical-mode)
		// read, unlike brew's raw-mode getch: canonical mode delivers nothing to
		// the reader until a line terminator arrives. The SIGTTIN behaviour under
		// test is independent of the line discipline.
		_, _ = ptmx.Write([]byte("y\n"))
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("child session failed: %v", err)
		}
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		var diag string
		select {
		case diag = <-out:
		case <-time.After(2 * time.Second):
		}
		t.Fatalf("timed out: the brew child never consumed the prompt answer "+
			"(SIGTTIN stop — the interactive-prompt hang has regressed)\npty output:\n%s", diag)
	}

	select {
	case got := <-out:
		if !strings.Contains(got, "CHILD-READ-OK") {
			t.Fatalf("child did not report reading the answer; pty output:\n%s", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no output captured from pty")
	}
}

// ttyChildMain runs inside the PTY session leader. It plays brewfast's role:
// start a grandchild in its OWN process group that reads the terminal, hand it
// the terminal via acquireTerminal, and verify the read succeeds.
func ttyChildMain() {
	// The grandchild models brew: own process group + reads the terminal.
	gc := exec.Command(os.Args[0], "-test.run=TestTerminalReaderHelper")
	gc.Env = append(os.Environ(), "BREWFAST_TTY_READER=1")
	gc.Stdin, gc.Stdout, gc.Stderr = os.Stdin, os.Stdout, os.Stderr
	gc.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := gc.Start(); err != nil {
		os.Stdout.WriteString("START-FAIL\n")
		os.Exit(1)
	}

	// Backstop: if the terminal handover regresses, the grandchild is stopped by
	// SIGTTIN and would linger after the parent test times out and kills this
	// process. Bound it so a failing run cannot leak a stopped process.
	go func() {
		time.Sleep(30 * time.Second)
		_ = gc.Process.Kill()
		os.Exit(1)
	}()

	// The line under test.
	term := acquireTerminal(gc.Process.Pid)
	defer term.restore()

	if err := gc.Wait(); err != nil {
		os.Stdout.WriteString("CHILD-READ-FAIL\n")
		os.Exit(1)
	}
	os.Exit(0)
}

// TestTerminalReaderHelper is not a real test: it is the grandchild that models
// brew's prompt read (a single byte from the controlling terminal, as
// Homebrew's Ask.confirm? does via $stdin.getch).
func TestTerminalReaderHelper(t *testing.T) {
	if os.Getenv("BREWFAST_TTY_READER") != "1" {
		return
	}
	// Bound the read so this helper can never wedge a CI run on its own.
	go func() {
		time.Sleep(25 * time.Second)
		os.Exit(1)
	}()

	// Announce readiness so the parent presses the key only once this read is
	// imminent — and crucially, AFTER the process group is already set up, so a
	// missing terminal handover still manifests as the SIGTTIN stop under test.
	os.Stdout.WriteString(readerReadyMarker + "\n")

	buf := make([]byte, 1)
	n, err := os.Stdin.Read(buf)
	if err != nil || n != 1 {
		os.Stdout.WriteString("READ-ERR:" + errString(err) + "\n")
		os.Exit(1)
	}
	os.Stdout.WriteString("CHILD-READ-OK:" + string(buf[:n]) + "\n")
	os.Exit(0)
}

// readerReadyMarker is printed by the reader immediately before it reads, so
// the parent can press the key without racing the reader's startup.
const readerReadyMarker = "READER-READY"

func errString(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}
