package main

import (
	"context"
	"errors"
	"time"

	"github.com/mtzanidakis/ypotitlo/internal/selfupdate"
)

func cmdUpgrade(ctx context.Context, e env, args []string) error {
	fs := newFlagSet("upgrade")
	dryRun := fs.Bool("n", false, "report what would happen and change nothing")
	repo := fs.String("repo", "", "override the GitHub repository (owner/name)")
	timeout := fs.Duration("timeout", 2*time.Minute, "overall deadline")

	if err := parseFlags(fs, args); err != nil {
		if errors.Is(err, flagErrHelp) {
			usageBlock(e.Stdout,
				"Replace this binary with the newest published release.\n\n"+
					"The archive's checksum is verified against the release's checksums.txt\n"+
					"before anything is replaced, and the swap is atomic: a failure at any\n"+
					"point leaves the installed binary untouched.",
				[]flagSection{{Title: "Flags", Flags: []flagDoc{
					{"-n", "report what would happen and change nothing"},
					{"-timeout D", "overall deadline, e.g. 2m"},
					{"-repo OWNER/NAME", "override the source repository"},
				}}},
				[]string{"ypotitlo upgrade", "ypotitlo upgrade -n"})
			return nil
		}
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	_, err := selfupdate.Run(ctx, e.Stdout, selfupdate.Options{
		CurrentVersion: resolveVersion(),
		Repo:           *repo,
		DryRun:         *dryRun,
	})
	return err
}
