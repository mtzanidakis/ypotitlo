package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
)

// flagErrHelp is re-exported so subcommands can spot -h without importing flag.
var flagErrHelp = flag.ErrHelp

// newFlagSet builds a FlagSet that never writes or exits on its own, so main
// owns every byte of output and every exit code.
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	return fs
}

// parseFlags parses args and applies the checks the stdlib does not.
//
// Two stdlib behaviours are actively dangerous for this tool and both are
// silent. "-i -ol el" parses as i="-ol" with no error, so a forgotten value
// swallows the next flag; and parsing stops at the first non-flag argument, so
// "translate movie.srt -ol el" succeeds with every flag left empty. Both would
// surface much later as a confusing failure, or worse, as the wrong file.
//
// stdinOK names the flags for which a bare "-" is a legitimate value.
func parseFlags(fs *flag.FlagSet, args []string, stdinOK ...string) error {
	if err := parseFlagsN(fs, args, 0, stdinOK...); err != nil {
		return err
	}
	return nil
}

// parseFlagsN is parseFlags for subcommands that take positional arguments.
// The stdlib stops parsing at the first non-flag argument, so those arguments
// are exactly fs.Args() and need no hand-rolled scan.
func parseFlagsN(fs *flag.FlagSet, args []string, want int, stdinOK ...string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return usagef("%v", err)
	}
	if fs.NArg() != want {
		if want == 0 {
			return usagef("unexpected argument %q; flags must be given as -name value", fs.Arg(0))
		}
		return usagef("expected %d argument(s), got %d", want, fs.NArg())
	}

	allowDash := make(map[string]bool, len(stdinOK))
	for _, n := range stdinOK {
		allowDash[n] = true
	}

	var err error
	fs.Visit(func(f *flag.Flag) {
		if err != nil {
			return
		}
		v := f.Value.String()
		if !strings.HasPrefix(v, "-") {
			return
		}
		if v == "-" && allowDash[f.Name] {
			return
		}
		err = usagef("-%s was given %q, which looks like a flag; did you omit its value?", f.Name, v)
	})
	return err
}

// wasGiven reports whether the flag appeared on the command line.
//
// This is the only way to tell "-bom=false" from an absent -bom: reading the
// value cannot, and a flag whose zero value silently beats the config file
// would override a stored setting on every single run.
func wasGiven(fs *flag.FlagSet, name string) bool {
	seen := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			seen = true
		}
	})
	return seen
}

// usageBlock renders a hand-written help text. The stdlib's generated help
// sorts alphabetically, which puts -i and -il adjacent and near-identical, and
// it cannot mark a flag required or show an example.
func usageBlock(w io.Writer, synopsis string, sections []flagSection, examples []string) {
	var b strings.Builder
	b.WriteString(synopsis)
	b.WriteString("\n")
	for _, s := range sections {
		fmt.Fprintf(&b, "\n%s:\n", s.Title)
		for _, f := range s.Flags {
			fmt.Fprintf(&b, "  %-18s %s\n", f.Name, f.Desc)
		}
	}
	if len(examples) > 0 {
		b.WriteString("\nExamples:\n")
		for _, e := range examples {
			fmt.Fprintf(&b, "  %s\n", e)
		}
	}
	outf(w, "%s", b.String())
}

type flagSection struct {
	Title string
	Flags []flagDoc
}

type flagDoc struct{ Name, Desc string }
