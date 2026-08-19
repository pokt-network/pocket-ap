package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The LICENSE is referenced from three places that cannot see it: the goreleaser
// archive ships it by name, the brew formula and the nfpm packages both declare
// "MIT", and the README claims it. Nothing would notice if the file went missing
// or changed licence — the build would keep passing and the packages would keep
// claiming MIT.
func TestLicenseIsMITAndPresent(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "LICENSE"))
	if err != nil {
		t.Fatalf("LICENSE missing: %v — goreleaser ships it by name and both package formats declare MIT", err)
	}
	text := string(raw)

	if !strings.HasPrefix(text, "MIT License") {
		t.Errorf("LICENSE does not start with %q — the brew formula and nfpm packages both declare MIT", "MIT License")
	}
	// The clause that makes it MIT rather than something merely MIT-shaped.
	for _, want := range []string{
		"Permission is hereby granted, free of charge",
		"WITHOUT WARRANTY OF ANY KIND",
		"Copyright (c)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("LICENSE is missing %q", want)
		}
	}
}

// Everything that declares a licence must declare the same one.
func TestLicenseDeclarationsAgree(t *testing.T) {
	root := filepath.Join("..", "..")
	for _, f := range []string{".goreleaser.yaml", "README.md"} {
		raw, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if !strings.Contains(string(raw), "MIT") {
			t.Errorf("%s does not mention MIT — the declarations have drifted from LICENSE", f)
		}
	}
}
