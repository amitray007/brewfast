package brew

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// A small but realistic slice of `brew info --json=v2 --cask orpheus-nightly`.
// Extra top-level keys and cask fields are included to prove the parser ignores
// what it does not model.
const caskInfoFixture = `{
  "formulae": [],
  "casks": [
    {
      "token": "orpheus-nightly",
      "full_token": "amitray007/tap/orpheus-nightly",
      "name": ["Orpheus Nightly"],
      "version": "0.6.0-nightly.20260731",
      "url": "https://github.com/amitray007/orpheus/releases/download/nightly-20260731/Orpheus-0.6.0.dmg",
      "sha256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
      "desc": "A nightly build",
      "artifacts": [{"app": ["Orpheus.app"]}]
    }
  ]
}`

const caskInfoNotFoundFixture = `{"formulae": [], "casks": []}`

const formulaInfoFixture = `{
  "formulae": [
    {
      "name": "brewfast",
      "full_name": "amitray007/tap/brewfast"
    }
  ],
  "casks": []
}`

const formulaInfoNotFoundFixture = `{"formulae": [], "casks": []}`

func TestParseCaskInfo(t *testing.T) {
	c, err := parseCaskInfo([]byte(caskInfoFixture))
	if err != nil {
		t.Fatalf("parseCaskInfo returned error: %v", err)
	}
	want := &Cask{
		Token:   "orpheus-nightly",
		Version: "0.6.0-nightly.20260731",
		URL:     "https://github.com/amitray007/orpheus/releases/download/nightly-20260731/Orpheus-0.6.0.dmg",
		SHA256:  "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
	}
	if !reflect.DeepEqual(c, want) {
		t.Errorf("parseCaskInfo() = %+v, want %+v", c, want)
	}
}

func TestParseCaskInfo_NotFound(t *testing.T) {
	_, err := parseCaskInfo([]byte(caskInfoNotFoundFixture))
	if !errors.Is(err, ErrCaskNotFound) {
		t.Errorf("parseCaskInfo(empty casks) error = %v, want ErrCaskNotFound", err)
	}
}

func TestParseCaskInfo_Malformed(t *testing.T) {
	if _, err := parseCaskInfo([]byte("{not json")); err == nil {
		t.Error("parseCaskInfo(malformed) = nil error, want a parse error")
	}
}

func TestParseFormulaExists(t *testing.T) {
	exists, err := parseFormulaExists([]byte(formulaInfoFixture))
	if err != nil {
		t.Fatalf("parseFormulaExists returned error: %v", err)
	}
	if !exists {
		t.Fatal("parseFormulaExists(existing formula) = false, want true")
	}
}

func TestParseFormulaExists_NotFound(t *testing.T) {
	exists, err := parseFormulaExists([]byte(formulaInfoNotFoundFixture))
	if err != nil {
		t.Fatalf("parseFormulaExists returned error: %v", err)
	}
	if exists {
		t.Fatal("parseFormulaExists(empty formulae) = true, want false")
	}
}

func TestParseFormulaExists_Malformed(t *testing.T) {
	if _, err := parseFormulaExists([]byte("{not json")); err == nil {
		t.Error("parseFormulaExists(malformed) = nil error, want a parse error")
	}
}

func installFakeBrew(t *testing.T, scriptBody string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "brew")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+scriptBody+"\n"), 0o755); err != nil {
		t.Fatalf("write fake brew: %v", err)
	}
	t.Setenv("PATH", dir)
}

func TestFormulaExists_UnknownFormulaExitIsMiss(t *testing.T) {
	installFakeBrew(t, "exit 1")

	exists, err := FormulaExists("not-a-formula")
	if err != nil {
		t.Fatalf("unknown formula should be a clean miss, got: %v", err)
	}
	if exists {
		t.Fatal("unknown formula exit reported an exact formula match")
	}
}

func TestFormulaExists_BrewStartFailureIsError(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	exists, err := FormulaExists("brewfast")
	if err == nil {
		t.Fatal("a brew start failure must not be reported as a formula miss")
	}
	if exists {
		t.Fatal("a brew start failure reported an exact formula match")
	}
	if !strings.Contains(err.Error(), "running brew formula info") {
		t.Fatalf("start failure needs formula lookup context, got: %v", err)
	}
}

// A `brew search --cask orpheus`-style fixture. Because SearchCandidates scopes
// the search with --cask, every line brew prints is a cask; the parser's job is
// to reduce tap-qualified references to bare tokens and de-duplicate, NOT to
// classify formulae vs casks.
const searchFixture = `morpheus
orpheus
orpheus-nightly
amitray007/tap/orpheus-beta
`

func TestParseSearchOutput(t *testing.T) {
	got := parseSearchOutput(searchFixture)
	// Tap-qualified cask lines are reduced to their bare token.
	want := []string{"morpheus", "orpheus", "orpheus-nightly", "orpheus-beta"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseSearchOutput() = %v, want %v", got, want)
	}
}

// TestParseSearchOutput_PipedHeaderlessOutput is the regression for the bug
// where `brewfast t3` offered formulae (qt3d, ropebwt3, mt32emu) as cask
// candidates.
//
// brew prints its "==> Formulae"/"==> Casks" headers ONLY when stdout is a
// terminal. Read through a pipe — which is always how brewfast reads it — the
// groups arrive as bare names separated by a blank line, exactly as reproduced
// below. The old parser started in "casks" and tracked those absent headers, so
// it labeled every formula a cask.
//
// The fix scopes the search itself with --cask so brew never emits formulae at
// all. This asserts the parser is faithful to that scoped output and, critically,
// that a blank-line group separator is not treated as a section change.
func TestParseSearchOutput_PipedHeaderlessOutput(t *testing.T) {
	// Real `brew search --cask -- t3` stdout captured through a pipe.
	fixture := "convert3dgui\ndust3d\nfont-3270\nt3-code\ntheblankclub/tap/t3code-alpha\n"
	got := parseSearchOutput(fixture)
	want := []string{"convert3dgui", "dust3d", "font-3270", "t3-code", "t3code-alpha"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseSearchOutput(piped --cask output) = %v, want %v", got, want)
	}

	// A blank line separates groups in brew's piped layout; it must never be
	// read as a section marker that changes how later names are classified.
	sep := parseSearchOutput("alpha\n\nbeta\n")
	if !reflect.DeepEqual(sep, []string{"alpha", "beta"}) {
		t.Errorf("a blank separator line must not drop or reclassify names; got %v", sep)
	}
}

// TestParseSearchOutput_HeadersTolerated ensures output captured from a terminal
// (which does carry headers) still parses to the same names — the markers are
// skipped as noise rather than used for classification.
func TestParseSearchOutput_HeadersTolerated(t *testing.T) {
	got := parseSearchOutput("==> Casks\norpheus\norpheus-nightly\n")
	want := []string{"orpheus", "orpheus-nightly"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseSearchOutput(with headers) = %v, want %v", got, want)
	}
}

func TestParseSearchOutput_NoMatch(t *testing.T) {
	// On no match, brew prints its notice to STDERR and stdout is empty.
	if got := parseSearchOutput(""); len(got) != 0 {
		t.Errorf("parseSearchOutput(\"\") = %v, want zero candidates", got)
	}
}

func TestParseSearchOutput_CaskOnlyNoHeader(t *testing.T) {
	// A cask-only search prints bare names with no header line.
	got := parseSearchOutput("orpheus\norpheus-nightly\n")
	want := []string{"orpheus", "orpheus-nightly"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseSearchOutput(no header) = %v, want %v", got, want)
	}
}

func TestValidateName(t *testing.T) {
	valid := []string{
		"orpheus",
		"orpheus-nightly",
		"font-fira-code",
		"adoptopenjdk@8",
		"gcc+llvm",
		"visual-studio-code.app",
		"aria2",
	}
	for _, name := range valid {
		if err := ValidateName(name); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", name, err)
		}
	}

	invalid := []string{
		"--version",
		"-d",
		"foo;bar",
		"foo bar",
		"",
		"UPPER",
		"foo/bar", // slash is a tap separator, not valid in a bare token here
		"foo|bar",
		"foo$bar",
		"foo&bar",
		"foo\tbar",
	}
	for _, name := range invalid {
		if err := ValidateName(name); err == nil {
			t.Errorf("ValidateName(%q) = nil, want error", name)
		} else if !errors.Is(err, ErrInvalidName) {
			t.Errorf("ValidateName(%q) error = %v, want wrapping ErrInvalidName", name, err)
		}
	}
}

func TestHandoffArgs(t *testing.T) {
	got := HandoffArgs(OpReinstall, "orpheus-nightly")
	want := []string{"reinstall", "--cask", "--", "orpheus-nightly"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("HandoffArgs(reinstall) = %v, want %v", got, want)
	}
	// The `--` terminator must sit immediately before the user-derived name so a
	// dash-leading name (were one to slip past validation) can never be a flag.
	if got[len(got)-2] != "--" {
		t.Errorf("HandoffArgs: expected `--` immediately before the name, got %v", got)
	}

	gotUp := HandoffArgs(OpUpgrade, "orpheus")
	wantUp := []string{"upgrade", "--cask", "--", "orpheus"}
	if !reflect.DeepEqual(gotUp, wantUp) {
		t.Errorf("HandoffArgs(upgrade) = %v, want %v", gotUp, wantUp)
	}
}

// captureRunner records the last invocation instead of executing it, and
// confirms ExecRunner satisfies the Runner interface at compile time via the
// var below.
type captureRunner struct {
	name string
	args []string
}

func (c *captureRunner) Run(_ context.Context, name string, args ...string) error {
	c.name = name
	c.args = args
	return nil
}

var _ Runner = (*captureRunner)(nil)
var _ Runner = ExecRunner{}

func TestRunnerInterface(t *testing.T) {
	var r Runner = &captureRunner{}
	if err := r.Run(context.Background(), "brew", HandoffArgs(OpUpgrade, "orpheus")...); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	cr := r.(*captureRunner)
	if cr.name != "brew" {
		t.Errorf("Run name = %q, want brew", cr.name)
	}
	want := []string{"upgrade", "--cask", "--", "orpheus"}
	if !reflect.DeepEqual(cr.args, want) {
		t.Errorf("Run args = %v, want %v", cr.args, want)
	}
}

func TestParseInstalledVersion(t *testing.T) {
	// Installed: brew prints "<name> <version>".
	if v, ok := parseInstalledVersion("orpheus-nightly 0.6.0-nightly.20260731\n"); !ok || v != "0.6.0-nightly.20260731" {
		t.Errorf("parseInstalledVersion(installed) = (%q, %v), want (%q, true)", v, ok, "0.6.0-nightly.20260731")
	}
	// Multiple installed versions on one line: take the first version field.
	if v, ok := parseInstalledVersion("orpheus 1.0.0 1.1.0\n"); !ok || v != "1.0.0" {
		t.Errorf("parseInstalledVersion(multi) = (%q, %v), want (%q, true)", v, ok, "1.0.0")
	}
	// Not installed: brew prints nothing.
	if v, ok := parseInstalledVersion(""); ok || v != "" {
		t.Errorf("parseInstalledVersion(empty) = (%q, %v), want (\"\", false)", v, ok)
	}
	if v, ok := parseInstalledVersion("   \n"); ok || v != "" {
		t.Errorf("parseInstalledVersion(blank) = (%q, %v), want (\"\", false)", v, ok)
	}
}

func TestIsInstalled(t *testing.T) {
	// `sh` is on PATH on every POSIX test machine; a random token is not.
	if !IsInstalled("sh") {
		t.Error("IsInstalled(\"sh\") = false, want true")
	}
	if IsInstalled("definitely-not-a-real-binary-xyzzy-42") {
		t.Error("IsInstalled(nonexistent) = true, want false")
	}
}
