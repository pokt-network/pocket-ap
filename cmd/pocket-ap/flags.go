package main

import (
	"flag"
	"fmt"
	"io"
	"strings"
)

// printFlags is flag.PrintDefaults with two dashes.
//
// Go's own printer writes "-config string", and both forms have always worked —
// the flag package treats one dash and two identically. The docs, `pocketd
// --network=beta` two lines away in the same README, and every CLI this
// project's users reach for from npm all spell a long flag with two. Printing
// one while every example around it printed two was the only inconsistency
// anyone could actually see.
//
// Single-dash invocations keep working. Nothing here changes parsing; this is
// the help text alone.
func printFlags(fs *flag.FlagSet, out io.Writer) {
	fs.VisitAll(func(f *flag.Flag) {
		name, usage := flag.UnquoteUsage(f)
		line := "  --" + f.Name
		if name != "" {
			line += " " + name
		}
		// Four spaces rather than a tab, so the wrapped usage lines below sit
		// where they look like they belong regardless of tab width.
		fmt.Fprintln(out, line)
		fmt.Fprintln(out, "    \t"+strings.ReplaceAll(usage, "\n", "\n    \t"))
		// A zero value is the absence of the flag, and saying "default: empty"
		// for it is noise. Anything else is worth stating.
		if f.DefValue != "" && f.DefValue != "false" && f.DefValue != "0" {
			fmt.Fprintf(out, "    \t(default %q)\n", f.DefValue)
		}
	})
}
