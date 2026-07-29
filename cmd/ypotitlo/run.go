package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
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

// usageError marks an error as a misuse of the command line rather than a
// runtime failure, so run can map it to exitUsage.
type usageError struct{ err error }

func (e usageError) Error() string { return e.err.Error() }
func (e usageError) Unwrap() error { return e.err }

func usagef(format string, a ...any) error {
	return usageError{fmt.Errorf(format, a...)}
}

type command struct {
	run  func(ctx context.Context, e env, args []string) error
	desc string
}

func commands() map[string]command {
	return map[string]command{
		"version": {cmdVersion, "Print the ypotitlo version"},
	}
}

// order is the order subcommands are listed in the usage text; it is
// deliberately by workflow rather than alphabetical.
var order = []string{"translate", "list-models", "config-show", "config-set", "config-unset", "version"}

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

	cmds := commands()
	c, ok := cmds[name]
	if !ok {
		fmt.Fprintf(e.Stderr, "ypotitlo: unknown command %q\n\n", name)
		printUsage(e.Stderr)
		return exitUsage
	}

	err := c.run(ctx, e, e.Args[1:])
	return exitCode(ctx, e, err)
}

func exitCode(ctx context.Context, e env, err error) int {
	if err == nil {
		return exitOK
	}
	if ctx.Err() != nil {
		fmt.Fprintln(e.Stderr, "ypotitlo: cancelled; no output written")
		return exitCanceled
	}

	var ue usageError
	if ok := asUsageError(err, &ue); ok {
		fmt.Fprintf(e.Stderr, "ypotitlo: %v\n", err)
		return exitUsage
	}
	fmt.Fprintf(e.Stderr, "ypotitlo: %v\n", err)
	return exitError
}

func asUsageError(err error, target *usageError) bool {
	if ue, ok := err.(usageError); ok {
		*target = ue
		return true
	}
	return false
}

func printUsage(w io.Writer) {
	var b strings.Builder
	b.WriteString("ypotitlo translates subtitle files into another language via an LLM.\n\n")
	b.WriteString("Usage:\n  ypotitlo <command> [flags]\n\nCommands:\n")

	cmds := commands()
	for _, name := range order {
		c, ok := cmds[name]
		if !ok {
			continue // not implemented yet
		}
		fmt.Fprintf(&b, "  %-13s %s\n", name, c.desc)
	}
	b.WriteString("\nRun 'ypotitlo <command> -h' for the flags of a command.\n")
	fmt.Fprint(w, b.String())
}

func cmdVersion(_ context.Context, e env, args []string) error {
	if len(args) > 0 {
		return usagef("version takes no arguments, got %q", args[0])
	}
	fmt.Fprintln(e.Stdout, version)
	return nil
}
