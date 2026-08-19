package main

import (
	"runtime"
	"strings"
	"testing"
)

// A version string is the first thing asked for in a bug report, so it has to
// identify the build unambiguously — not just a tag, which a fork or an
// unreleased branch would share.
func TestVersionString(t *testing.T) {
	got := versionString()

	for _, want := range []string{
		"pocket-ap",
		version,      // the tag, stamped at release
		commit,       // which commit — a tag alone cannot identify a fork's build
		runtime.GOOS, // wrong-arch downloads are a real support question
		runtime.GOARCH,
		runtime.Version(),
	} {
		if !strings.Contains(got, want) {
			t.Errorf("versionString() = %q, missing %q", got, want)
		}
	}
}

// An unstamped build must say so. If `go build` output claimed a release
// version, a colleague's bug report would point at the wrong code.
func TestVersionDefaultsAreHonest(t *testing.T) {
	if version != "dev" {
		t.Errorf("version default = %q, want %q — an unstamped build must not look like a release", version, "dev")
	}
	if commit != "none" || date != "unknown" {
		t.Errorf("commit/date defaults = %q/%q, want none/unknown", commit, date)
	}
}
