// Command ypotitlo translates subtitle files into another language via an LLM.
//
// Subcommands are dispatched from os.Args[1] (no cobra). The stdlib flag
// package is used deliberately: it looks flag names up by exact match, so
// multi-character single-dash flags such as -il and -ol work. cobra/pflag
// applies POSIX shorthand clustering and would silently read -il as -i l.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	// Not deferred: os.Exit does not run deferred functions, so stop() is
	// called explicitly before exiting.
	code := run(ctx, environ())
	stop()
	os.Exit(code)
}
