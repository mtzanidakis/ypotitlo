package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/mtzanidakis/ypotitlo/internal/config"
)

// configOptions builds the config seams from the injected environment, so the
// subcommands below never reach for the real home directory in tests.
func configOptions(e env, path string) config.Options {
	return config.Options{Path: path, Getenv: e.Getenv}
}

func cmdConfigShow(_ context.Context, e env, args []string) error {
	fs := newFlagSet("config-show")
	path := fs.String("config", "", "configuration file path")
	raw := fs.Bool("file", false, "show the file's contents instead of the effective config")
	if err := parseFlags(fs, args); err != nil {
		if errors.Is(err, flagErrHelp) {
			usageBlock(e.Stdout, "Show the effective configuration and where each value came from.",
				[]flagSection{{Title: "Flags", Flags: []flagDoc{
					{"-config PATH", "configuration file path"},
					{"-file", "show the raw file instead of the effective config"},
				}}},
				[]string{"ypotitlo config-show", "ypotitlo config-show -file"})
			return nil
		}
		return err
	}

	o := configOptions(e, *path)
	file, err := config.FilePath(o)
	if err != nil {
		return err
	}

	if *raw {
		return showRawFile(e, file)
	}

	cfg, sources, err := config.Load(o)
	if err != nil {
		return err
	}

	// The API key is resolved separately: it can come from the environment or
	// from opencode's own credentials file, neither of which Load can see.
	key, keySource, keyErr := config.ResolveAPIKey(o, cfg, "")
	sources = withAPIKey(sources, key, keySource, keyErr)

	if _, err := os.Stat(file); err != nil {
		outf(e.Stdout, "config file: %s (not present)\n\n", file)
	} else {
		outf(e.Stdout, "config file: %s\n\n", file)
	}
	writeSources(e.Stdout, sources)
	return nil
}

func showRawFile(e env, file string) error {
	b, err := os.ReadFile(file)
	if errors.Is(err, os.ErrNotExist) {
		outf(e.Stdout, "config file: %s (not present)\n", file)
		return nil
	}
	if err != nil {
		return err
	}
	outf(e.Stdout, "config file: %s\n\n", file)
	// Redact any api_key line so that piping the output somewhere public is
	// not a way to leak the key.
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) != "api_key" {
			outf(e.Stdout, "%s\n", line)
			continue
		}
		// go-toml quotes with apostrophes, not double quotes; trimming only
		// the latter turns an unset key into a two-character "secret".
		secret := strings.Trim(strings.TrimSpace(v), `'"`)
		outf(e.Stdout, "%s= %s\n", k, quotedSecret(secret))
	}
	return nil
}

func quotedSecret(s string) string {
	if s == "" {
		return "''"
	}
	return config.Redact(s)
}

// withAPIKey folds the separately-resolved API key into the source list so the
// table shows one coherent picture. The raw key is stored, not a redacted one:
// the Source is marked Secret and writeSources redacts on the way out, so
// redacting here too would render the marker twice.
func withAPIKey(sources []config.Source, key, from string, err error) []config.Source {
	if err != nil {
		return config.Override(sources, "api_key", "", "default")
	}
	return config.Override(sources, "api_key", key, from)
}

func writeSources(w io.Writer, sources []config.Source) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	rowf(tw, "SETTING\tVALUE\tSOURCE\n")

	var notes []string
	for _, s := range sources {
		value := s.Value
		if s.Secret {
			value = config.Redact(value)
		}
		if value == "" {
			value = "(unset)"
		}
		rowf(tw, "%s\t%s\t%s\n", s.Key, value, s.From)

		for _, sh := range s.Shadowed {
			// An empty lower-precedence value lost to nothing. Reporting
			// "api_key is also set in config" when the file holds api_key = ''
			// sends the reader to look at a key that is not there.
			if sh.Value == "" {
				continue
			}
			notes = append(notes, fmt.Sprintf("%s is also set in %s, shadowed by %s", s.Key, sh.From, s.From))
		}
	}
	_ = tw.Flush()

	if len(notes) > 0 {
		outf(w, "\n")
		for _, n := range notes {
			outf(w, "note: %s\n", n)
		}
	}
}

func cmdConfigSet(_ context.Context, e env, args []string) error {
	fs := newFlagSet("config-set")
	path := fs.String("config", "", "configuration file path")
	if err := parseFlagsN(fs, args, 2); err != nil {
		if errors.Is(err, flagErrHelp) {
			usageBlock(e.Stdout, "Usage: ypotitlo config-set KEY VALUE\n\nSet a configuration value. Pass - as VALUE to read it from stdin,\nwhich keeps secrets such as api_key out of the shell history.",
				[]flagSection{{Title: "Keys", Flags: keyDocs()}},
				[]string{"ypotitlo config-set model deepseek-v4-pro", "ypotitlo config-set api_key -"})
			return nil
		}
		if fs.NArg() != 2 {
			return usagef("config-set takes exactly KEY and VALUE (known keys: %s)", strings.Join(config.Keys(), ", "))
		}
		return err
	}
	key, value := fs.Arg(0), fs.Arg(1)

	if value == "-" {
		b, err := io.ReadAll(e.Stdin)
		if err != nil {
			return fmt.Errorf("reading value from stdin: %w", err)
		}
		value = strings.TrimRight(string(b), "\r\n")
	} else if isSecretKey(key) {
		outf(e.Stderr, "warning: %s was given on the command line and will be stored in your shell history; use '%s -' to read it from stdin instead\n", key, key)
	}

	if err := config.Set(configOptions(e, *path), key, value); err != nil {
		if errors.Is(err, config.ErrUnknownKey) {
			return usagef("%v", err)
		}
		return err
	}
	outf(e.Stdout, "%s set\n", key)
	return nil
}

func cmdConfigUnset(_ context.Context, e env, args []string) error {
	fs := newFlagSet("config-unset")
	path := fs.String("config", "", "configuration file path")
	if err := parseFlagsN(fs, args, 1); err != nil {
		if errors.Is(err, flagErrHelp) {
			usageBlock(e.Stdout, "Usage: ypotitlo config-unset KEY\n\nRemove a configuration value, restoring its default.",
				[]flagSection{{Title: "Keys", Flags: keyDocs()}},
				[]string{"ypotitlo config-unset api_key"})
			return nil
		}
		if fs.NArg() != 1 {
			return usagef("config-unset takes exactly one KEY (known keys: %s)", strings.Join(config.Keys(), ", "))
		}
		return err
	}

	key := fs.Arg(0)
	if err := config.Unset(configOptions(e, *path), key); err != nil {
		if errors.Is(err, config.ErrUnknownKey) {
			return usagef("%v", err)
		}
		return err
	}
	outf(e.Stdout, "%s unset\n", key)
	return nil
}

func isSecretKey(key string) bool { return key == "api_key" }

func keyDocs() []flagDoc {
	docs := make([]flagDoc, 0, len(config.Keys()))
	for _, k := range config.Keys() {
		docs = append(docs, flagDoc{Name: k})
	}
	return docs
}
