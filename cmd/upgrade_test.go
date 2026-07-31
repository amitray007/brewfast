package cmd

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// TestUpgrade_AlreadyCurrent: when brew reports brewfast is already installed at
// the latest version, upgrade reports current and returns no error.
func TestUpgrade_AlreadyCurrent(t *testing.T) {
	var out bytes.Buffer
	var upgradeArgs []string
	d := upgradeDeps{
		isInstalled: func(string) bool { return true },
		runBrew: func(args ...string) (string, error) {
			if len(args) > 0 && args[0] == "upgrade" {
				upgradeArgs = args
				// brew succeeds (nil error) and reports brewfast already current.
				// This is the genuine already-current case: a nil error gates the
				// up-to-date report so a real failure is never masked (see
				// TestUpgrade_AlreadyInstalledButFailed).
				return "Warning: brewfast 1.2.3 already installed", nil
			}
			return "", nil
		},
	}

	if err := runUpgrade(&out, d); err != nil {
		t.Fatalf("already-current should not error, got: %v", err)
	}
	if !strings.Contains(out.String(), "up-to-date") {
		t.Fatalf("expected an up-to-date report, got: %q", out.String())
	}
	// It still had to invoke `brew upgrade brewfast`.
	if len(upgradeArgs) != 2 || upgradeArgs[0] != "upgrade" || upgradeArgs[1] != "brewfast" {
		t.Fatalf("expected `brew upgrade brewfast`, got: %v", upgradeArgs)
	}
}

// TestUpgrade_UpgradeAvailable: a real upgrade succeeds — upgrade invokes
// `brew upgrade brewfast` and reports success.
func TestUpgrade_UpgradeAvailable(t *testing.T) {
	var out bytes.Buffer
	var sawUpdate, sawUpgrade bool
	d := upgradeDeps{
		isInstalled: func(string) bool { return true },
		runBrew: func(args ...string) (string, error) {
			switch {
			case len(args) == 1 && args[0] == "update":
				sawUpdate = true
				return "Updated 1 tap.", nil
			case len(args) == 2 && args[0] == "upgrade" && args[1] == "brewfast":
				sawUpgrade = true
				return "==> Upgrading brewfast\n  1.2.3 -> 1.3.0", nil
			}
			t.Fatalf("unexpected brew args: %v", args)
			return "", nil
		},
	}

	if err := runUpgrade(&out, d); err != nil {
		t.Fatalf("upgrade should succeed, got: %v", err)
	}
	if !sawUpdate {
		t.Fatal("expected `brew update` to be invoked")
	}
	if !sawUpgrade {
		t.Fatal("expected `brew upgrade brewfast` to be invoked")
	}
	if !strings.Contains(out.String(), "upgraded to the latest version") {
		t.Fatalf("expected an upgraded report, got: %q", out.String())
	}
}

// TestUpgrade_BrewAbsent: brew missing → a clear error, and brew is never run.
func TestUpgrade_BrewAbsent(t *testing.T) {
	var out bytes.Buffer
	d := upgradeDeps{
		isInstalled: func(string) bool { return false },
		runBrew: func(args ...string) (string, error) {
			t.Fatalf("runBrew must not be called when brew is absent; got %v", args)
			return "", nil
		},
	}

	err := runUpgrade(&out, d)
	if err == nil {
		t.Fatal("expected an error when brew is absent")
	}
	if !strings.Contains(err.Error(), "brew not found") {
		t.Fatalf("expected a clear brew-absent error, got: %v", err)
	}
}

// TestUpgrade_RealFailure: a genuine `brew upgrade` failure (output not matching
// the already-current markers) surfaces as an error.
func TestUpgrade_RealFailure(t *testing.T) {
	var out bytes.Buffer
	d := upgradeDeps{
		isInstalled: func(string) bool { return true },
		runBrew: func(args ...string) (string, error) {
			if len(args) > 0 && args[0] == "upgrade" {
				return "Error: network is unreachable", errors.New("exit status 1")
			}
			return "", nil
		},
	}

	err := runUpgrade(&out, d)
	if err == nil {
		t.Fatal("expected a real upgrade failure to error")
	}
	if !strings.Contains(err.Error(), "brew upgrade brewfast` failed") {
		t.Fatalf("expected the failure error to name the brew command, got: %v", err)
	}
}

// TestUpgrade_AlreadyInstalledButFailed: a real `brew upgrade` failure whose
// output happens to contain an already-current marker ("already installed") must
// surface as a failure, not be masked as up-to-date with a clean exit (Fix 3).
func TestUpgrade_AlreadyInstalledButFailed(t *testing.T) {
	var out bytes.Buffer
	d := upgradeDeps{
		isInstalled: func(string) bool { return true },
		runBrew: func(args ...string) (string, error) {
			if len(args) > 0 && args[0] == "upgrade" {
				// Output contains an already-current marker, but brew failed.
				return "Error: brewfast 1.2.3 already installed but the keg is broken", errors.New("exit status 1")
			}
			return "", nil
		},
	}

	err := runUpgrade(&out, d)
	if err == nil {
		t.Fatal("a failed upgrade must surface as an error even when its output contains an already-current marker")
	}
	if !strings.Contains(err.Error(), "brew upgrade brewfast` failed") {
		t.Fatalf("expected the failure error to name the brew command, got: %v", err)
	}
	if strings.Contains(out.String(), "up-to-date") {
		t.Fatalf("a real failure must not be reported as up-to-date, got: %q", out.String())
	}
}

// TestIsAlreadyCurrent exercises the pure classifier over brew's known phrasings.
func TestIsAlreadyCurrent(t *testing.T) {
	current := []string{
		"Warning: brewfast 1.2.3 already installed",
		"brewfast is up-to-date",
		"brewfast is up to date",
		"Already up-to-date.",
		"No available upgrade for brewfast",
	}
	for _, s := range current {
		if !isAlreadyCurrent(s) {
			t.Errorf("expected %q to be classified already-current", s)
		}
	}
	notCurrent := []string{
		"==> Upgrading brewfast\n  1.2.3 -> 1.3.0",
		"Error: network is unreachable",
		"",
	}
	for _, s := range notCurrent {
		if isAlreadyCurrent(s) {
			t.Errorf("expected %q to NOT be classified already-current", s)
		}
	}
}

// TestUpgradeCmdRegistered verifies the upgrade subcommand is wired into root.
func TestUpgradeCmdRegistered(t *testing.T) {
	root := newRootCmd()
	var found bool
	for _, c := range root.Commands() {
		if c.Name() == "upgrade" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("upgrade subcommand not registered on root")
	}
}
