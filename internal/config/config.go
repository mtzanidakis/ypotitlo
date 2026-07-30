// Package config loads, saves and explains ypotitlo's configuration file.
//
// The precedence for every setting is flag > env > config file > default,
// and the point of the Source type is that the tool can *show* which of
// those won. The expected support question is "why is it using the wrong
// model" or "why do I get a 401", and the answer is nearly always that an
// environment variable is shadowing the file — a dump of the file alone
// never answers it.
//
// Every filesystem lookup goes through Options so that the test suite runs
// with HOME=/nonexistent and still touches nothing outside t.TempDir().
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/pelletier/go-toml/v2"
)

// appName is the directory name under the user's config directory.
const appName = "ypotitlo"

// FileName is the base name of the configuration file.
const FileName = "config.toml"

// Defaults for the settings that have one. base_url points at the Zen
// open-weight pool; the Claude/GPT/Gemini models live under a different
// base path, which is exactly why this has to be configurable.
const (
	DefaultBaseURL     = "https://opencode.ai/zen/go/v1"
	DefaultBatchSize   = 20
	DefaultConcurrency = 2
	DefaultLineEndings = "auto"
	DefaultMaxSpendUSD = 1.0
)

// Environment variables consulted for the API key, in precedence order.
const (
	EnvAPIKey    = "OPENCODE_API_KEY"
	EnvZenAPIKey = "OPENCODE_ZEN_API_KEY"
)

// Config is the on-disk configuration. The comment tags are not decoration:
// config-set rewrites the whole file from this struct, so they are what
// makes the generated file self-documenting.
type Config struct {
	BaseURL string `toml:"base_url" comment:"OpenAI-compatible API base URL. The default pool carries only\nopen-weight models; Claude, GPT and Gemini live under https://opencode.ai/zen/v1."`
	Model   string `toml:"model" comment:"Model id, as reported by 'ypotitlo list-models'."`
	APIKey  string `toml:"api_key" comment:"API key. Leave empty to fall back to $OPENCODE_API_KEY or to\n~/.local/share/opencode/auth.json."`

	TargetLanguage string `toml:"target_language" comment:"Default target language for 'translate' when -ol is omitted.\nAccepts a code (el, ell, gre) or a name (greek)."`
	LineEndings    string `toml:"line_endings" comment:"Line endings of the output file: lf, crlf, or auto (preserve the input's)."`

	BatchSize   int `toml:"batch_size" comment:"Cues per request (1-200). Larger batches drift out of alignment\nsuperlinearly; 20-25 is the sweet spot."`
	Concurrency int `toml:"concurrency" comment:"Parallel requests (1-64)."`

	// OutputBOM is a pointer because it is tri-state: unset means "preserve
	// whatever the input had", which is a different answer from false.
	OutputBOM *bool `toml:"output_bom,omitempty" comment:"Write a UTF-8 BOM. Omit the key entirely to preserve the input's."`

	MaxSpendUSD float64 `toml:"max_spend_usd" comment:"Hard budget ceiling per run, in USD. The run aborts before the\nrequest that would cross it."`
}

// Defaults returns the configuration used when no file exists.
func Defaults() Config {
	return Config{
		BaseURL:     DefaultBaseURL,
		BatchSize:   DefaultBatchSize,
		Concurrency: DefaultConcurrency,
		LineEndings: DefaultLineEndings,
		MaxSpendUSD: DefaultMaxSpendUSD,
	}
}

// Options are the injected seams. The zero value means "behave like the real
// process": real environment, real GOOS, real config directory.
type Options struct {
	// Path overrides the configuration file location entirely. Empty means
	// Dir(o)/config.toml.
	Path string

	// GOOS overrides runtime.GOOS, so the darwin branch of Dir is testable
	// from Linux.
	GOOS string

	// Getenv overrides os.Getenv.
	Getenv func(string) string

	// AuthPath overrides the opencode credentials file location. Empty
	// means $XDG_DATA_HOME/opencode/auth.json.
	AuthPath string
}

func (o Options) getenv() func(string) string {
	if o.Getenv != nil {
		return o.Getenv
	}
	return os.Getenv
}

func (o Options) goos() string {
	if o.GOOS != "" {
		return o.GOOS
	}
	return runtime.GOOS
}

// ErrNoHome is returned when there is no home directory to derive a config
// path from. It is a distinct error so the CLI can suggest -config or
// XDG_CONFIG_HOME instead of printing something about os.UserConfigDir.
var ErrNoHome = errors.New("cannot locate a home directory")

// Dir returns the directory holding the configuration file.
//
// $XDG_CONFIG_HOME wins everywhere when it is set to an absolute path. On
// darwin we then use $HOME/.config rather than os.UserConfigDir, which
// returns ~/Library/Application Support and ignores XDG entirely — a CLI
// that a user configures by hand belongs in ~/.config on every Unix.
func Dir(o Options) (string, error) {
	getenv := o.getenv()

	// The XDG spec says a relative path must be ignored, not resolved.
	if v := getenv("XDG_CONFIG_HOME"); filepath.IsAbs(v) {
		return filepath.Join(v, appName), nil
	}

	switch o.goos() {
	case "windows":
		v := getenv("AppData")
		if v == "" {
			return "", fmt.Errorf("%w: %%AppData%% is not set", ErrNoHome)
		}
		return filepath.Join(v, appName), nil
	case "plan9":
		v := getenv("home")
		if v == "" {
			return "", fmt.Errorf("%w: $home is not set", ErrNoHome)
		}
		return filepath.Join(v, "lib", appName), nil
	default:
		// darwin included, deliberately.
		v := getenv("HOME")
		if v == "" {
			return "", fmt.Errorf("%w: $HOME is not set", ErrNoHome)
		}
		return filepath.Join(v, ".config", appName), nil
	}
}

// FilePath returns the path of the configuration file.
func FilePath(o Options) (string, error) {
	if o.Path != "" {
		return o.Path, nil
	}
	dir, err := Dir(o)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, FileName), nil
}

// Provenance strings for Source.From.
const (
	FromFlag     = "flag"
	FromConfig   = "config"
	FromAuthFile = "auth.json"
	FromDefault  = "default"
)

// FromEnv formats the provenance string for an environment variable.
func FromEnv(name string) string { return "env " + name }

// Shadow is a value that lost to a higher-precedence one.
type Shadow struct {
	From  string
	Value string
}

// Source records where one effective setting came from, together with the
// lower-precedence values it beat. config-show prints it as an effective
// config with a SOURCE column and a "shadowed by" note.
type Source struct {
	Key      string
	Value    string
	From     string
	Shadowed []Shadow

	// Secret marks values the CLI must pass through Redact before display.
	Secret bool
}

// Load reads the configuration file, filling in defaults for absent keys.
//
// A missing file is not an error: it yields pure defaults. A file that
// exists but contains an out-of-range value is, because silently clamping
// batch_size back to 20 is how a user ends up debugging the wrong thing.
func Load(o Options) (Config, []Source, error) {
	cfg, present, path, err := load(o)
	if err != nil {
		return Config{}, nil, err
	}
	if err := Validate(cfg); err != nil {
		return Config{}, nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, sourcesOf(cfg, present), nil
}

// load reads and merges without validating, so that config-set can still
// repair a file that Load would reject.
func load(o Options) (Config, map[string]bool, string, error) {
	path, err := FilePath(o)
	if err != nil {
		return Config{}, nil, "", err
	}

	data, err := os.ReadFile(path) //nolint:gosec // the path is the user's own config file
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return Defaults(), map[string]bool{}, path, nil
	case err != nil:
		return Config{}, nil, path, fmt.Errorf("read %s: %w", path, err)
	}

	// Decoded twice on purpose: the struct gives typed values, the map says
	// which keys the file actually mentions. Zero-value probing cannot tell
	// "max_spend_usd = 0.0" from "absent", and those mean opposite things.
	var raw map[string]any
	if err := toml.Unmarshal(data, &raw); err != nil {
		return Config{}, nil, path, fmt.Errorf("parse %s: %w", path, err)
	}
	var cfg Config
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return Config{}, nil, path, fmt.Errorf("parse %s: %w", path, err)
	}

	present := make(map[string]bool, len(raw))
	for k := range raw {
		present[strings.ToLower(k)] = true
	}
	for _, s := range settings {
		if !present[s.Name] {
			s.Reset(&cfg)
		}
	}
	return cfg, present, path, nil
}

// sourcesOf builds the provenance list for a freshly loaded config, in the
// declaration order of the settings table so config-show is stable.
func sourcesOf(cfg Config, present map[string]bool) []Source {
	def := Defaults()
	out := make([]Source, 0, len(settings))
	for _, s := range settings {
		src := Source{Key: s.Name, Value: s.Get(cfg), From: FromDefault, Secret: s.Secret}
		// A shadowed default is only worth reporting when there was a
		// default to shadow; "model" has none, so saying it overrode the
		// empty string is noise.
		if present[s.Name] {
			src.From = FromConfig
			if d := s.Get(def); d != "" && d != src.Value {
				src.Shadowed = []Shadow{{From: FromDefault, Value: d}}
			}
		}
		out = append(out, src)
	}
	return out
}

// Override records a higher-precedence value for key, pushing whatever was
// there into the shadowed list. It is how the CLI folds flags and
// environment variables into the provenance table that Load returns.
func Override(srcs []Source, key, value, from string) []Source {
	for i := range srcs {
		if srcs[i].Key != key {
			continue
		}
		prev := Shadow{From: srcs[i].From, Value: srcs[i].Value}
		srcs[i].Shadowed = append([]Shadow{prev}, srcs[i].Shadowed...)
		srcs[i].From = from
		srcs[i].Value = value
		return srcs
	}
	return append(srcs, Source{Key: key, Value: value, From: from})
}

// Redact renders a secret for display: the last four characters and the
// length, which is enough to tell two keys apart or to spot a truncated
// paste, and not enough to use.
func Redact(s string) string {
	if s == "" {
		return ""
	}
	n := utf8.RuneCountInString(s)
	if n <= 4 {
		return fmt.Sprintf("%s (%d chars)", strings.Repeat("*", n), n)
	}
	r := []rune(s)
	return fmt.Sprintf("...%s (%d chars)", string(r[n-4:]), n)
}

// header is prepended to every generated file. go-toml has no notion of a
// document-level comment, and the file is otherwise a wall of settings with
// no clue where it came from.
const header = "# ypotitlo configuration.\n" +
	"# Rewritten in full by 'ypotitlo config-set'; comments below are generated.\n\n"

// Set writes a single key to the configuration file, creating it if needed.
//
// The whole struct is re-marshalled rather than patched, so the file always
// carries the current set of keys and their explanatory comments.
func Set(o Options, key, value string) error {
	s, err := lookup(key)
	if err != nil {
		return err
	}

	// Deliberately the unvalidated load: a file with an out-of-range value
	// must still be repairable with config-set.
	cfg, _, path, err := load(o)
	if err != nil {
		return err
	}
	if err := s.Set(&cfg, value); err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	return write(path, cfg)
}

// Unset restores a key to its default, which for keys without one means
// removing the value.
func Unset(o Options, key string) error {
	s, err := lookup(key)
	if err != nil {
		return err
	}
	cfg, _, path, err := load(o)
	if err != nil {
		return err
	}
	s.Reset(&cfg)
	return write(path, cfg)
}

// write atomically replaces the configuration file.
//
// The temp file is created in the destination directory so the rename stays
// within one filesystem, and its mode is set explicitly afterwards: the mode
// passed to file creation is masked by the process umask, and a config file
// that may hold an API key must not be world-readable because the user
// happens to run with umask 022.
func write(path string, cfg Config) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	data, err := toml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}

	tmp, err := os.CreateTemp(dir, "."+FileName+".*")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	// A no-op once the rename below has succeeded; on any earlier failure
	// it is what keeps stray .config.toml.* files out of the directory.
	defer func() { _ = os.Remove(tmpName) }()

	if err := fill(tmp, header, data); err != nil {
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return fmt.Errorf("chmod %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename %s to %s: %w", tmpName, path, err)
	}
	return nil
}

// fill writes a header and a body to f, flushes them to disk and closes f
// exactly once, whichever step fails first.
func fill(f *os.File, head string, body []byte) error {
	err := func() error {
		if _, err := f.WriteString(head); err != nil {
			return err
		}
		if _, err := f.Write(body); err != nil {
			return err
		}
		return f.Sync()
	}()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// formatFloat renders a float without a trailing ".0" for whole numbers
// beyond two decimals, so max_spend_usd shows as "1" rather than "1.000000".
func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}
