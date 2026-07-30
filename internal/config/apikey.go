package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// authProviders are the keys ypotitlo looks for inside opencode's auth.json,
// in order. The Go SDK writes "opencode-go"; the TUI and the Zen provider
// have been seen writing the other two.
var authProviders = []string{"opencode-go", "opencode", "opencode-zen"}

// authEntry is one provider's stored credentials. opencode stores both API
// keys and OAuth logins in the same file and distinguishes them only by
// "type"; an oauth entry has no "key" field at all, so decoding it without
// checking the type yields an empty token and a 401 that looks like a wrong
// key rather than a wrong login method.
type authEntry struct {
	Type string `json:"type"`
	Key  string `json:"key"`
}

// ErrNoAPIKey is returned when none of the four sources produced a key.
var ErrNoAPIKey = errors.New("no API key found")

// AuthFilePath returns the path of opencode's credentials file.
func AuthFilePath(o Options) string {
	if o.AuthPath != "" {
		return o.AuthPath
	}
	getenv := o.getenv()
	if v := getenv("XDG_DATA_HOME"); filepath.IsAbs(v) {
		return filepath.Join(v, "opencode", "auth.json")
	}
	home := getenv("HOME")
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".local", "share", "opencode", "auth.json")
}

// ResolveAPIKey applies the key precedence ladder: flag, then
// $OPENCODE_API_KEY, then $OPENCODE_ZEN_API_KEY, then the config file, then
// opencode's auth.json.
//
// The returned source string is what config-show displays and what the
// authentication error names, so that a 401 says which of the five places
// the bad key came from.
//
// An empty api_key in the config file is not a value: the file is rewritten
// in full by config-set, so api_key = "" is what an *unset* key looks like
// on disk, and it must not stop the auth.json fallback.
func ResolveAPIKey(o Options, cfg Config, flagVal string) (key, source string, err error) {
	if k := strings.TrimSpace(flagVal); k != "" {
		return k, FromFlag, nil
	}

	getenv := o.getenv()
	for _, name := range []string{EnvAPIKey, EnvZenAPIKey} {
		if k := strings.TrimSpace(getenv(name)); k != "" {
			return k, FromEnv(name), nil
		}
	}

	if k := strings.TrimSpace(cfg.APIKey); k != "" {
		return k, FromConfig, nil
	}

	path := AuthFilePath(o)
	k, provider, notes, err := readAuthFile(path)
	if err != nil {
		return "", "", err
	}
	if k != "" {
		return k, fmt.Sprintf("%s (%s)", FromAuthFile, provider), nil
	}

	return "", "", noKeyError(path, notes)
}

// readAuthFile looks for a usable API key in opencode's auth.json. notes
// carries human-readable reasons for every entry that was found but skipped,
// so the final error can say "there is an oauth login here, that is not the
// same thing as an API key".
func readAuthFile(path string) (key, provider string, notes []string, err error) {
	if path == "" {
		return "", "", nil, nil
	}

	data, err := os.ReadFile(path) //nolint:gosec // the path is the user's own credentials file
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", "", nil, nil
	case err != nil:
		return "", "", nil, fmt.Errorf("read %s: %w", path, err)
	}

	// RawMessage rather than authEntry: a single provider whose value is not
	// an object (opencode has changed this file's shape before) must not
	// make the whole file undecodable.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return "", "", nil, fmt.Errorf("parse %s: %w", path, err)
	}

	for _, name := range authProviders {
		msg, ok := raw[name]
		if !ok {
			continue
		}
		var e authEntry
		if err := json.Unmarshal(msg, &e); err != nil {
			notes = append(notes, fmt.Sprintf("entry %q is not in the expected form", name))
			continue
		}
		switch {
		case e.Type != "" && e.Type != "api":
			notes = append(notes, fmt.Sprintf("entry %q is an %s login, not an api key", name, e.Type))
		case strings.TrimSpace(e.Key) == "":
			notes = append(notes, fmt.Sprintf("entry %q has an empty key", name))
		default:
			return strings.TrimSpace(e.Key), name, notes, nil
		}
	}

	if len(raw) > 0 && len(notes) == 0 {
		notes = append(notes, fmt.Sprintf("no entry for %s", strings.Join(authProviders, ", ")))
	}
	return "", "", notes, nil
}

// noKeyError spells out every place that was consulted. With five possible
// sources, "unauthorized" on its own is an unactionable bug report.
func noKeyError(authPath string, notes []string) error {
	var b strings.Builder
	b.WriteString("looked in: -api-key flag, $")
	b.WriteString(EnvAPIKey)
	b.WriteString(", $")
	b.WriteString(EnvZenAPIKey)
	b.WriteString(", config api_key")
	if authPath != "" {
		b.WriteString(", ")
		b.WriteString(authPath)
	}
	for _, n := range notes {
		b.WriteString("\n  ")
		b.WriteString(authPath)
		b.WriteString(": ")
		b.WriteString(n)
	}
	b.WriteString("\nset one with 'ypotitlo config-set api_key -' or export $")
	b.WriteString(EnvAPIKey)
	return fmt.Errorf("%w; %s", ErrNoAPIKey, b.String())
}
