package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"
)

// Process exit codes. 2 is reserved for usage errors because the stdlib flag
// package hardcodes it.
const (
	exitOK       = 0
	exitError    = 1
	exitUsage    = 2
	exitParse    = 3
	exitAuth     = 4
	exitBudget   = 5
	exitCanceled = 130
)

// env is the process environment, injected so that every subcommand is
// testable without touching the real stdio, argv, environment or clock.
type env struct {
	Stdout io.Writer
	Stderr io.Writer
	Stdin  io.Reader
	Args   []string // argv without the program name
	Getenv func(string) string
	Now    func() time.Time
}

func environ() env {
	return env{
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		Stdin:  os.Stdin,
		Args:   os.Args[1:],
		Getenv: os.Getenv,
		Now:    time.Now,
	}
}

// outf writes to a CLI output stream. Write errors on stdout/stderr are not
// actionable — there is nowhere left to report them — so they are dropped
// here once rather than ignored at each of the dozens of call sites.
func outf(w io.Writer, format string, a ...any) {
	_, _ = fmt.Fprintf(w, format, a...)
}

// rowf writes one tabwriter row. Same reasoning as outf: a tabwriter over
// stdout has nowhere useful to report a write failure.
func rowf(tw *tabwriter.Writer, format string, a ...any) {
	_, _ = fmt.Fprintf(tw, format, a...)
}

// usageError marks an error as a misuse of the command line rather than a
// runtime failure, so run can map it to exitUsage.
type usageError struct{ err error }

func (e *usageError) Error() string { return e.err.Error() }
func (e *usageError) Unwrap() error { return e.err }

func usagef(format string, a ...any) error {
	return &usageError{fmt.Errorf(format, a...)}
}

// codedError carries an explicit exit code, so that a shell loop over a
// subtitle library can distinguish a rejected key from an odd input file
// without scraping stderr.
type codedError struct {
	code int
	err  error
}

func (e *codedError) Error() string { return e.err.Error() }
func (e *codedError) Unwrap() error { return e.err }

// parseError marks a malformed input file, as opposed to a runtime failure.
type parseError struct{ err error }

func (e *parseError) Error() string { return e.err.Error() }
func (e *parseError) Unwrap() error { return e.err }

type command struct {
	run  func(ctx context.Context, e env, args []string) error
	desc string
}

func commands() map[string]command {
	return map[string]command{
		"translate":    {cmdTranslate, "Translate a subtitle file into another language"},
		"list-models":  {cmdListModels, "List the models the endpoint offers"},
		"config-show":  {cmdConfigShow, "Show the effective config and where each value came from"},
		"config-set":   {cmdConfigSet, "Set a configuration value"},
		"config-unset": {cmdConfigUnset, "Remove a configuration value"},
		"upgrade":      {cmdUpgrade, "Replace this binary with the newest release"},
		"version":      {cmdVersion, "Print the ypotitlo version"},
	}
}

// order is the order subcommands are listed in the usage text; it is
// deliberately by workflow rather than alphabetical.
var order = []string{"translate", "list-models", "config-show", "config-set", "config-unset", "upgrade", "version"}

func run(ctx context.Context, e env) int {
	if len(e.Args) == 0 {
		printUsage(e.Stderr)
		return exitUsage
	}

	name := e.Args[0]
	switch name {
	case "-h", "--help", "help":
		printUsage(e.Stdout)
		return exitOK
	case "--version":
		name = "version"
	}

	c, ok := commands()[name]
	if !ok {
		outf(e.Stderr, "ypotitlo: unknown command %q\n\n", name)
		printUsage(e.Stderr)
		return exitUsage
	}

	return exitCode(ctx, e, c.run(ctx, e, e.Args[1:]))
}

func exitCode(ctx context.Context, e env, err error) int {
	if err == nil {
		return exitOK
	}
	// A cancelled context outranks whatever error the cancellation produced.
	if ctx.Err() != nil {
		outf(e.Stderr, "ypotitlo: cancelled; no output written\n")
		return exitCanceled
	}

	outf(e.Stderr, "ypotitlo: %v\n", err)

	var ue *usageError
	if errors.As(err, &ue) {
		return exitUsage
	}
	var pe *parseError
	if errors.As(err, &pe) {
		return exitParse
	}
	var ce *codedError
	if errors.As(err, &ce) {
		return ce.code
	}
	return exitError
}

func printUsage(w io.Writer) {
	var b strings.Builder
	b.WriteString("ypotitlo translates subtitle files into another language via an LLM.\n\n")
	b.WriteString("Usage:\n  ypotitlo <command> [flags]\n\nCommands:\n")

	cmds := commands()
	for _, name := range order {
		if c, ok := cmds[name]; ok {
			fmt.Fprintf(&b, "  %-13s %s\n", name, c.desc)
		}
	}
	b.WriteString("\nRun 'ypotitlo <command> -h' for the flags of a command.\n")
	outf(w, "%s", b.String())
}

func cmdVersion(_ context.Context, e env, args []string) error {
	if len(args) > 0 {
		return usagef("version takes no arguments, got %q", args[0])
	}
	outf(e.Stdout, "%s\n", resolveVersion())
	return nil
}
