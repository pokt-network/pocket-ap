package main

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The npm packages exist once per platform goreleaser builds, and the mapping is
// written down three times: goreleaser's goos/goarch, the generator's TARGETS,
// and the shim's PACKAGES table keyed on Node's names. Node and Go disagree on
// two of the five — amd64 is x64, windows is win32 — so the copies cannot be
// diffed by eye, and a target added to one place and not the others produces a
// platform that either has no package or has a package nothing resolves.
func TestNPMTargetsMatchGoreleaser(t *testing.T) {
	repo := filepath.Join("..", "..")

	goreleaserTargets := parseGoreleaserTargets(t, filepath.Join(repo, ".goreleaser.yaml"))
	if len(goreleaserTargets) == 0 {
		t.Fatal("parsed no targets out of .goreleaser.yaml")
	}

	generator := read(t, filepath.Join(repo, "npm", "generate.mjs"))
	// The PACKAGES table alone, not the whole file: every platform name also
	// appears in the comment above the table explaining the Go/Node mismatch, so
	// searching the file found a deleted entry in the prose that described it and
	// passed against a shim that could no longer run on linux/x64.
	shim := sliceBetween(t, read(t, filepath.Join(repo, "npm", "bin", "pocket-ap.js")),
		"const PACKAGES = {", "};")

	// Go name -> Node name, the translation the generator claims to make.
	nodeOS := map[string]string{"darwin": "darwin", "linux": "linux", "windows": "win32"}
	nodeArch := map[string]string{"amd64": "x64", "arm64": "arm64"}

	for _, target := range goreleaserTargets {
		goos, goarch, _ := strings.Cut(target, "/")
		wantEntry := `{ goos: "` + goos + `", goarch: "` + goarch + `"`
		if !strings.Contains(generator, wantEntry) {
			t.Errorf("npm/generate.mjs has no TARGETS entry for %s, which goreleaser builds — that platform would get no npm package", target)
		}
		wantKey := `"` + nodeOS[goos] + " " + nodeArch[goarch] + `"`
		if !strings.Contains(shim, wantKey) {
			t.Errorf("npm/bin/pocket-ap.js has no PACKAGES key %s for %s — the wrapper would refuse to run on a platform we ship", wantKey, target)
		}
	}

	// And the reverse: a package for a platform goreleaser does not build would
	// be published empty.
	for _, m := range regexp.MustCompile(`\{ goos: "(\w+)", goarch: "(\w+)"`).FindAllStringSubmatch(generator, -1) {
		if !slices.Contains(goreleaserTargets, m[1]+"/"+m[2]) {
			t.Errorf("npm/generate.mjs targets %s/%s, which goreleaser does not build", m[1], m[2])
		}
	}
}

// parseGoreleaserTargets expands the builds block's goos × goarch minus its
// ignore list. Hand-rolled rather than pulling in a YAML dependency for one
// test: the block is four lines and this keeps the test tree dependency-free.
func parseGoreleaserTargets(t *testing.T, path string) []string {
	t.Helper()
	body := read(t, path)

	list := func(key string) []string {
		m := regexp.MustCompile(`(?m)^\s+` + key + `: \[([^\]]+)\]`).FindStringSubmatch(body)
		if m == nil {
			t.Fatalf("%s: no %s list found", path, key)
		}
		var out []string
		for _, v := range strings.Split(m[1], ",") {
			out = append(out, strings.TrimSpace(v))
		}
		return out
	}

	ignored := map[string]bool{}
	for _, m := range regexp.MustCompile(`- goos: (\w+)\s+goarch: (\w+)`).FindAllStringSubmatch(body, -1) {
		ignored[m[1]+"/"+m[2]] = true
	}

	var targets []string
	for _, goos := range list("goos") {
		for _, goarch := range list("goarch") {
			if !ignored[goos+"/"+goarch] {
				targets = append(targets, goos+"/"+goarch)
			}
		}
	}
	return targets
}

// sliceBetween returns the text between two markers, failing if either is
// missing — a renamed marker must break the test rather than silently reduce it
// to searching an empty string, which would pass against anything.
func sliceBetween(t *testing.T, body, start, end string) string {
	t.Helper()
	i := strings.Index(body, start)
	if i == -1 {
		t.Fatalf("marker %q not found — has the table been renamed?", start)
	}
	rest := body[i+len(start):]
	j := strings.Index(rest, end)
	if j == -1 {
		t.Fatalf("closing marker %q not found after %q", end, start)
	}
	return rest[:j]
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
