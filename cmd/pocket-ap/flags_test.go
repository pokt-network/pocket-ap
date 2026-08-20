package main

import (
	"bytes"
	"flag"
	"strings"
	"testing"
)

// The help text switched to two dashes; PARSING did not change, and must not.
// Every single-dash invocation in every README, script and shell history has to
// keep working, so this pins all three spellings to the same result.
func TestFlagFormsAreInterchangeable(t *testing.T) {
	for _, argv := range [][]string{
		{"-config", "x.yaml", "-X", "GET"},
		{"--config", "x.yaml", "--X", "GET"},
		{"-config=x.yaml", "-X=GET"},
		{"--config=x.yaml", "--X=GET"},
	} {
		t.Run(strings.Join(argv, " "), func(t *testing.T) {
			fs := flag.NewFlagSet("t", flag.ContinueOnError)
			fs.SetOutput(&bytes.Buffer{})
			cfg := fs.String("config", "", "")
			method := fs.String("X", "", "")
			if err := fs.Parse(argv); err != nil {
				t.Fatalf("parse %q: %v", argv, err)
			}
			if *cfg != "x.yaml" || *method != "GET" {
				t.Errorf("parse %q gave config=%q X=%q, want x.yaml/GET", argv, *cfg, *method)
			}
		})
	}
}

// The point of the custom printer: what help prints is what the docs show.
func TestPrintFlagsUsesTwoDashes(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.String("config", "", "path to config YAML")
	fs.Bool("v", false, "verbose")

	var out bytes.Buffer
	printFlags(fs, &out)
	got := out.String()

	if !strings.Contains(got, "--config string") {
		t.Errorf("help does not print --config with two dashes:\n%s", got)
	}
	if !strings.Contains(got, "--v") {
		t.Errorf("help does not print --v with two dashes:\n%s", got)
	}
	// A single-dash rendering anywhere means PrintDefaults leaked back in.
	for _, line := range strings.Split(got, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "-") && !strings.HasPrefix(trimmed, "--") {
			t.Errorf("help line renders one dash: %q", line)
		}
	}
}

// A default worth stating is stated; an empty or false default is the absence of
// the flag and saying so is noise.
func TestPrintFlagsOmitsZeroDefaults(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.String("path", "/", "request path")
	fs.String("config", "", "config path")
	fs.Bool("v", false, "verbose")

	var out bytes.Buffer
	printFlags(fs, &out)
	got := out.String()

	if !strings.Contains(got, `(default "/")`) {
		t.Errorf("help omits the meaningful default for --path:\n%s", got)
	}
	if strings.Contains(got, `(default "")`) || strings.Contains(got, `(default "false")`) {
		t.Errorf("help states a zero default, which is just the flag being absent:\n%s", got)
	}
}
