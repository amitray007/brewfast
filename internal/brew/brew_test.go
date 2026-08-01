package brew

import (
	"context"
	"errors"
	"reflect"
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

// A `brew search orpheus`-style fixture: a Casks section plus a tap-qualified
// line, and a Formulae section that must be excluded.
const searchFixture = `==> Formulae
morph
==> Casks
morpheus
orpheus
orpheus-nightly
amitray007/tap/orpheus-beta
`

func TestParseSearchOutput(t *testing.T) {
	got := parseSearchOutput(searchFixture)
	want := []string{"morpheus", "orpheus", "orpheus-nightly", "amitray007/tap/orpheus-beta"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseSearchOutput() = %v, want %v", got, want)
	}
}

func TestParseSearchOutput_NoMatch(t *testing.T) {
	// On no match, brew prints its notice to STDERR and stdout is empty.
	if got := parseSearchOutput(""); len(got) != 0 {
		t.Errorf("parseSearchOutput(\"\") = %v, want zero candidates", got)
	}
}

func TestParseSearchOutput_CaskOnlyNoHeader(t *testing.T) {
	// A cask-only search can print bare names with no header line.
	got := parseSearchOutput("orpheus\norpheus-nightly\n")
	want := []string{"orpheus", "orpheus-nightly"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseSearchOutput(no header) = %v, want %v", got, want)
	}
}

func TestParseSearchOutput_FormulaeOnlyExcluded(t *testing.T) {
	// Formulae-only output yields no cask candidates.
	got := parseSearchOutput("==> Formulae\nwget\ncurl\n")
	if len(got) != 0 {
		t.Errorf("parseSearchOutput(formulae only) = %v, want zero candidates", got)
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
