package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"text/tabwriter"

	"github.com/mtzanidakis/ypotitlo/internal/config"
	"github.com/mtzanidakis/ypotitlo/internal/llm"
)

func cmdListModels(ctx context.Context, e env, args []string) error {
	fs := newFlagSet("list-models")
	path := fs.String("config", "", "configuration file path")
	baseURL := fs.String("base-url", "", "API base URL (overrides the config)")
	asJSON := fs.Bool("json", false, "emit JSON instead of a table")
	if err := parseFlags(fs, args); err != nil {
		if errors.Is(err, flagErrHelp) {
			usageBlock(e.Stdout, "List the models the configured endpoint offers.\n\nThis needs no API key, so it doubles as a check that the endpoint is\nreachable before anything is configured.",
				[]flagSection{{Title: "Flags", Flags: []flagDoc{
					{"-base-url URL", "API base URL (overrides the config)"},
					{"-config PATH", "configuration file path"},
					{"-json", "emit JSON instead of a table"},
				}}},
				[]string{
					"ypotitlo list-models",
					"ypotitlo list-models -base-url https://opencode.ai/zen/v1",
				})
			return nil
		}
		return err
	}

	o := configOptions(e, *path)
	cfg, _, err := config.Load(o)
	if err != nil {
		return err
	}
	if *baseURL != "" {
		cfg.BaseURL = *baseURL
	}

	// A missing key is not fatal here: the endpoint answers /models
	// unauthenticated, which is what makes this a useful first command to run.
	key, keySource, _ := config.ResolveAPIKey(o, cfg, "")

	client := llm.NewOpenCodeGo(llm.OpenCodeGoConfig{
		APIKey:    key,
		KeySource: keySource,
		BaseURL:   cfg.BaseURL,
		Now:       e.Now,
	})

	models, err := client.Models(ctx)
	if err != nil {
		return fmt.Errorf("listing models from %s: %w", cfg.BaseURL, err)
	}

	if *asJSON {
		enc := json.NewEncoder(e.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(models)
	}

	outf(e.Stdout, "endpoint: %s\n\n", cfg.BaseURL)
	writeModels(e, models, cfg.Model)
	return nil
}

func writeModels(e env, models []llm.Model, current string) {
	tw := tabwriter.NewWriter(e.Stdout, 0, 0, 2, ' ', 0)
	rowf(tw, "\tMODEL\tINPUT $/1M\tOUTPUT $/1M\n")

	unknown := 0
	for _, m := range models {
		marker := " "
		if m.ID == current {
			marker = "*"
		}
		in, out := "unknown", "unknown"
		if m.Price != nil {
			in = fmt.Sprintf("%.2f", m.Price.InputPer1M)
			out = fmt.Sprintf("%.2f", m.Price.OutputPer1M)
		} else {
			unknown++
		}
		rowf(tw, "%s\t%s\t%s\t%s\n", marker, m.ID, in, out)
	}
	_ = tw.Flush()

	outf(e.Stdout, "\n%d models", len(models))
	if current != "" {
		outf(e.Stdout, ", * marks the configured one")
	}
	outf(e.Stdout, "\n")
	if unknown > 0 {
		// Said plainly rather than shown as 0.00: an unpriced model makes the
		// spend ceiling unenforceable for that run, and the user should know.
		outf(e.Stdout, "%d have no published price; runs using them cannot be costed or capped\n", unknown)
	}
}
