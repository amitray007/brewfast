package resolve

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/amitray007/brewfast/internal/brew"
)

// fakeSelector records whether it was invoked and returns a scripted result. A
// non-nil err is returned in place of a choice (used to simulate a cancel).
type fakeSelector struct {
	called      bool
	gotPrompt   string
	gotOptions  []string
	returnValue string
	returnErr   error
}

func (f *fakeSelector) Pick(prompt string, options []string) (string, error) {
	f.called = true
	f.gotPrompt = prompt
	f.gotOptions = options
	if f.returnErr != nil {
		return "", f.returnErr
	}
	return f.returnValue, nil
}

// ttyPtr is a helper for the IsTTY override.
func ttyPtr(b bool) *bool { return &b }

// baseOpts builds an Options wired to fakes so no real brew/terminal is touched.
// lookupExact and search must be provided per-test.
func baseOpts(
	sel Selector,
	isTTY bool,
	noInput bool,
	lookup func(string) (*brew.Cask, error),
	search func(string) ([]string, error),
) (Options, *bytes.Buffer) {
	var stderr bytes.Buffer
	return Options{
		NoInput:     noInput,
		IsTTY:       ttyPtr(isTTY),
		Stderr:      &stderr,
		Selector:    sel,
		lookupExact: lookup,
		search:      search,
	}, &stderr
}

// TestExactMatchReturnsImmediately covers AE5: an exact name returns immediately,
// the candidate search is never called, and the selector is never invoked.
func TestExactMatchReturnsImmediately(t *testing.T) {
	sel := &fakeSelector{}
	searchCalled := false
	lookup := func(name string) (*brew.Cask, error) {
		return &brew.Cask{Token: name}, nil
	}
	search := func(name string) ([]string, error) {
		searchCalled = true
		return nil, nil
	}
	opts, _ := baseOpts(sel, true /*TTY*/, false, lookup, search)

	got, gotCask, err := Resolve("orpheus-nightly", opts)
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
	if got != "orpheus-nightly" {
		t.Fatalf("got %q, want %q", got, "orpheus-nightly")
	}
	// On the exact-match path the fetched cask is handed back so the caller can
	// skip a second brew info.
	if gotCask == nil || gotCask.Token != "orpheus-nightly" {
		t.Fatalf("exact match should return the fetched cask; got %v", gotCask)
	}
	if searchCalled {
		t.Error("search was called on an exact match; it must not be")
	}
	if sel.called {
		t.Error("selector was invoked on an exact match; it must not be")
	}
}

// TestInexactTTYInvokesSelector covers AE5: inexact + TTY + not NoInput → the
// selector is invoked with the candidates and its choice is returned.
func TestInexactTTYInvokesSelector(t *testing.T) {
	sel := &fakeSelector{returnValue: "orpheus-nightly"}
	lookup := func(name string) (*brew.Cask, error) {
		return nil, brew.ErrCaskNotFound
	}
	search := func(name string) ([]string, error) {
		return []string{"morpheus", "orpheus", "orpheus-nightly"}, nil
	}
	opts, _ := baseOpts(sel, true /*TTY*/, false, lookup, search)

	got, gotCask, err := Resolve("orpheus", opts)
	if err != nil {
		t.Fatalf("Resolve returned error: %v", err)
	}
	if got != "orpheus-nightly" {
		t.Fatalf("got %q, want %q", got, "orpheus-nightly")
	}
	// The picker path does not fetch the chosen cask's definition, so the
	// returned cask is nil and the caller fetches it.
	if gotCask != nil {
		t.Fatalf("picker path should return a nil cask; got %v", gotCask)
	}
	if !sel.called {
		t.Fatal("selector was not invoked on the interactive path")
	}
	// "orpheus" (the queried name itself) is filtered out — we never propose the
	// exact typed name back as a suggestion — so the picker sees the near-matches.
	wantOpts := []string{"morpheus", "orpheus-nightly"}
	if strings.Join(sel.gotOptions, ",") != strings.Join(wantOpts, ",") {
		t.Errorf("selector got options %v, want %v", sel.gotOptions, wantOpts)
	}
}

// TestInexactNonTTYPrintsAndErrors covers AE5: inexact + no TTY → candidates are
// printed, ErrNoInteractiveResolve is returned, and the selector is never called
// (asserting no hang / no prompt).
func TestInexactNonTTYPrintsAndErrors(t *testing.T) {
	sel := &fakeSelector{}
	lookup := func(name string) (*brew.Cask, error) {
		return nil, brew.ErrCaskNotFound
	}
	search := func(name string) ([]string, error) {
		return []string{"morpheus", "orpheus-nightly"}, nil
	}
	opts, stderr := baseOpts(sel, false /*no TTY*/, false, lookup, search)

	_, _, err := Resolve("orpheus", opts)
	if !errors.Is(err, ErrNoInteractiveResolve) {
		t.Fatalf("got err %v, want ErrNoInteractiveResolve", err)
	}
	if sel.called {
		t.Error("selector was invoked on the non-interactive path; it must never be (would hang CI)")
	}
	out := stderr.String()
	for _, want := range []string{"morpheus", "orpheus-nightly"} {
		if !strings.Contains(out, want) {
			t.Errorf("candidate %q not printed to stderr; got:\n%s", want, out)
		}
	}
}

// TestNoInputInTTYTakesNonInteractiveBranch covers AE5/R14: --no-input in a TTY
// still takes the print-and-exit branch; the selector is never called.
func TestNoInputInTTYTakesNonInteractiveBranch(t *testing.T) {
	sel := &fakeSelector{}
	lookup := func(name string) (*brew.Cask, error) {
		return nil, brew.ErrCaskNotFound
	}
	search := func(name string) ([]string, error) {
		return []string{"orpheus-nightly"}, nil
	}
	opts, _ := baseOpts(sel, true /*TTY*/, true /*NoInput*/, lookup, search)

	_, _, err := Resolve("orpheus", opts)
	if !errors.Is(err, ErrNoInteractiveResolve) {
		t.Fatalf("got err %v, want ErrNoInteractiveResolve", err)
	}
	if sel.called {
		t.Error("selector was invoked despite NoInput; --no-input must force the non-interactive branch")
	}
}

// TestZeroCandidatesReturnsNoMatch covers AE5: no exact match and no candidates
// → a clear no-match error, and the selector is never invoked.
func TestZeroCandidatesReturnsNoMatch(t *testing.T) {
	sel := &fakeSelector{}
	lookup := func(name string) (*brew.Cask, error) {
		return nil, brew.ErrCaskNotFound
	}
	search := func(name string) ([]string, error) {
		return nil, nil
	}
	opts, _ := baseOpts(sel, true /*TTY*/, false, lookup, search)

	_, _, err := Resolve("zzznotacask", opts)
	if !errors.Is(err, ErrNoCandidates) {
		t.Fatalf("got err %v, want ErrNoCandidates", err)
	}
	if sel.called {
		t.Error("selector was invoked with zero candidates; it must not be")
	}
}

// TestSelectorCancelReturnsCancelled covers AE5: the picker cancel maps to the
// distinct ErrCancelled sentinel.
func TestSelectorCancelReturnsCancelled(t *testing.T) {
	sel := &fakeSelector{returnErr: ErrCancelled}
	lookup := func(name string) (*brew.Cask, error) {
		return nil, brew.ErrCaskNotFound
	}
	search := func(name string) ([]string, error) {
		return []string{"orpheus", "orpheus-nightly"}, nil
	}
	opts, _ := baseOpts(sel, true /*TTY*/, false, lookup, search)

	_, _, err := Resolve("orpheus", opts)
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("got err %v, want ErrCancelled", err)
	}
}

// TestCandidatesFilterDropsSelfAndDupes verifies the near-name list drops the
// queried name itself and de-duplicates while preserving order.
func TestCandidatesFilterDropsSelfAndDupes(t *testing.T) {
	got := filterCaskCandidates([]string{"orpheus", "orpheus", "", "morpheus"}, "orpheus")
	want := []string{"morpheus"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestLookupErrorSurfaced verifies a non-not-found lookup error is surfaced
// rather than swallowed, and does not fall through to candidate gathering.
func TestLookupErrorSurfaced(t *testing.T) {
	sel := &fakeSelector{}
	sentinel := errors.New("brew exploded")
	searchCalled := false
	lookup := func(name string) (*brew.Cask, error) {
		return nil, sentinel
	}
	search := func(name string) ([]string, error) {
		searchCalled = true
		return nil, nil
	}
	opts, _ := baseOpts(sel, true, false, lookup, search)

	_, _, err := Resolve("orpheus", opts)
	if !errors.Is(err, sentinel) {
		t.Fatalf("got err %v, want it to wrap the lookup sentinel", err)
	}
	if searchCalled {
		t.Error("search ran after a hard lookup error; it must not")
	}
}
