package main

import (
	"slices"
	"testing"
)

// Every line here is something a person types. Three of them used to land
// somewhere unhelpful: a bare invocation failed with a message about a file at
// an empty path, and -h/--help reached the serve flagset, which answers "Usage
// of serve:" and two flags — hiding that `call` and `version` exist at all.
func TestDispatch(t *testing.T) {
	for _, tc := range []struct {
		name     string
		argv     []string
		wantCmd  string
		wantArgs []string
	}{
		{"bare invocation asks for help, not serve", nil, "help-exit2", nil},
		{"-h", []string{"-h"}, "help", nil},
		{"-help", []string{"-help"}, "help", nil},
		{"--help", []string{"--help"}, "help", nil},
		{"help verb", []string{"help"}, "help", []string{}},
		{"-version", []string{"-version"}, "version", nil},
		{"--version", []string{"--version"}, "version", nil},
		{"version verb", []string{"version"}, "version", []string{}},
		{"serve verb", []string{"serve", "-config", "x"}, "serve", []string{"-config", "x"}},
		{"call verb", []string{"call", "-d", "{}"}, "call", []string{"-d", "{}"}},
		// The compatibility case the leading-flag rule exists for: a bare flag
		// still means serve, which is how this was invoked before subcommands.
		{"leading flag still means serve", []string{"-config", "x"}, "serve", []string{"-config", "x"}},
		{"long leading flag too", []string{"--config", "x"}, "serve", []string{"--config", "x"}},
		{"unknown verb reaches the default branch", []string{"frobnicate"}, "frobnicate", []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd, args := dispatch(tc.argv)
			if cmd != tc.wantCmd {
				t.Errorf("dispatch(%q) command = %q, want %q", tc.argv, cmd, tc.wantCmd)
			}
			if !slices.Equal(args, tc.wantArgs) {
				t.Errorf("dispatch(%q) args = %q, want %q", tc.argv, args, tc.wantArgs)
			}
		})
	}
}

// The usage text is what all of the above print, so it has to name every
// subcommand the switch in main accepts. A subcommand missing from it is
// undiscoverable — which is the bug this whole file is about.
func TestUsageNamesEverySubcommand(t *testing.T) {
	for _, cmd := range []string{"serve", "call", "version"} {
		if !slices.Contains(splitWords(usage), cmd) {
			t.Errorf("usage does not mention the %q subcommand", cmd)
		}
	}
}

func splitWords(s string) []string {
	var out []string
	word := make([]rune, 0, 16)
	for _, r := range s {
		if r == ' ' || r == '\n' || r == '\t' || r == '"' {
			if len(word) > 0 {
				out = append(out, string(word))
				word = word[:0]
			}
			continue
		}
		word = append(word, r)
	}
	if len(word) > 0 {
		out = append(out, string(word))
	}
	return out
}
