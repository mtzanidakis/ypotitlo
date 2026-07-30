package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// stubEnv returns a Getenv that reads from a map, so no test ever depends on
// the real environment or on HOME existing.
func stubEnv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestDir(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		goos string
		env  map[string]string
		want string
		err  bool
	}{
		{
			name: "linux without xdg",
			goos: "linux",
			env:  map[string]string{"HOME": "/home/u"},
			want: "/home/u/.config/ypotitlo",
		},
		{
			name: "linux with xdg",
			goos: "linux",
			env:  map[string]string{"HOME": "/home/u", "XDG_CONFIG_HOME": "/cfg"},
			want: "/cfg/ypotitlo",
		},
		{
			// os.UserConfigDir would answer ~/Library/Application Support
			// here and ignore XDG entirely, which is why Dir does not use it.
			name: "darwin without xdg",
			goos: "darwin",
			env:  map[string]string{"HOME": "/Users/u"},
			want: "/Users/u/.config/ypotitlo",
		},
		{
			name: "darwin with xdg",
			goos: "darwin",
			env:  map[string]string{"HOME": "/Users/u", "XDG_CONFIG_HOME": "/cfg"},
			want: "/cfg/ypotitlo",
		},
		{
			// The XDG spec says a relative value must be ignored, not
			// resolved against the working directory.
			name: "relative xdg ignored",
			goos: "linux",
			env:  map[string]string{"HOME": "/home/u", "XDG_CONFIG_HOME": "relative"},
			want: "/home/u/.config/ypotitlo",
		},
		{
			name: "empty xdg ignored",
			goos: "linux",
			env:  map[string]string{"HOME": "/home/u", "XDG_CONFIG_HOME": ""},
			want: "/home/u/.config/ypotitlo",
		},
		{
			name: "plan9",
			goos: "plan9",
			env:  map[string]string{"home": "/usr/u"},
			want: "/usr/u/lib/ypotitlo",
		},
		{
			name: "no home",
			goos: "linux",
			env:  map[string]string{},
			err:  true,
		},
		{
			name: "no appdata",
			goos: "windows",
			env:  map[string]string{},
			err:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := Dir(Options{GOOS: tc.goos, Getenv: stubEnv(tc.env)})
			if tc.err {
				if !errors.Is(err, ErrNoHome) {
					t.Fatalf("Dir() error = %v, want ErrNoHome", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Dir(): %v", err)
			}
			if got != filepath.FromSlash(tc.want) {
				t.Errorf("Dir() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFilePath(t *testing.T) {
	t.Parallel()

	o := Options{GOOS: "linux", Getenv: stubEnv(map[string]string{"XDG_CONFIG_HOME": "/cfg"})}
	got, err := FilePath(o)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.FromSlash("/cfg/ypotitlo/config.toml"); got != want {
		t.Errorf("FilePath() = %q, want %q", got, want)
	}

	o.Path = "/explicit/other.toml"
	got, err = FilePath(o)
	if err != nil {
		t.Fatal(err)
	}
	if got != "/explicit/other.toml" {
		t.Errorf("FilePath() with Path = %q, want the explicit path", got)
	}
}

func TestLoadNoFile(t *testing.T) {
	t.Parallel()

	o := Options{Path: filepath.Join(t.TempDir(), "config.toml")}
	cfg, srcs, err := Load(o)
	if err != nil {
		t.Fatalf("Load with no file: %v", err)
	}
	if cfg != Defaults() {
		t.Errorf("Load() = %+v, want %+v", cfg, Defaults())
	}
	if cfg.BaseURL != DefaultBaseURL || cfg.BatchSize != 20 || cfg.Concurrency != 2 ||
		cfg.LineEndings != "auto" || cfg.MaxSpendUSD != 1.0 {
		t.Errorf("defaults drifted: %+v", cfg)
	}
	if cfg.OutputBOM != nil {
		t.Errorf("OutputBOM = %v, want nil (tri-state: absent means preserve)", *cfg.OutputBOM)
	}
	if len(srcs) != len(Keys()) {
		t.Fatalf("got %d sources, want %d", len(srcs), len(Keys()))
	}
	for _, s := range srcs {
		if s.From != FromDefault {
			t.Errorf("source %s from %q, want %q", s.Key, s.From, FromDefault)
		}
	}
}

func TestLoadMissingDirectory(t *testing.T) {
	t.Parallel()

	// A whole missing tree, not just a missing file: the first run of the
	// tool has no ~/.config/ypotitlo at all.
	o := Options{Path: filepath.Join(t.TempDir(), "nope", "deeper", "config.toml")}
	cfg, _, err := Load(o)
	if err != nil {
		t.Fatalf("Load with no directory: %v", err)
	}
	if cfg != Defaults() {
		t.Errorf("Load() = %+v, want defaults", cfg)
	}
}

func TestLoadPartialFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, "model = 'qwen'\nconcurrency = 8\n")

	cfg, srcs, err := Load(Options{Path: path})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Model != "qwen" || cfg.Concurrency != 8 {
		t.Errorf("file values not applied: %+v", cfg)
	}
	if cfg.BaseURL != DefaultBaseURL || cfg.BatchSize != DefaultBatchSize {
		t.Errorf("absent keys did not fall back to defaults: %+v", cfg)
	}

	from := map[string]string{}
	shadowed := map[string]int{}
	for _, s := range srcs {
		from[s.Key] = s.From
		shadowed[s.Key] = len(s.Shadowed)
	}
	if from["model"] != FromConfig || from["concurrency"] != FromConfig {
		t.Errorf("keys present in the file are not reported as %q: %v", FromConfig, from)
	}
	if from["base_url"] != FromDefault {
		t.Errorf("absent key reported as %q, want %q", from["base_url"], FromDefault)
	}
	// concurrency = 8 overrides the default 2, so the default is shadowed.
	if shadowed["concurrency"] != 1 {
		t.Errorf("concurrency shadowed = %d, want 1", shadowed["concurrency"])
	}
	if shadowed["base_url"] != 0 {
		t.Errorf("base_url shadowed = %d, want 0", shadowed["base_url"])
	}
}

// TestLoadDistinguishesZeroFromAbsent is the reason Load decodes the file
// twice. Probing the struct for zero values cannot tell an explicit
// "batch_size = 0" from an absent key, and those must produce opposite
// outcomes: a validation error versus the default.
func TestLoadDistinguishesZeroFromAbsent(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, "batch_size = 0\n")

	if _, _, err := Load(Options{Path: path}); err == nil {
		t.Fatal("Load accepted batch_size = 0, want a validation error")
	}

	writeFile(t, path, "model = 'x'\n")
	cfg, _, err := Load(Options{Path: path})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.BatchSize != DefaultBatchSize {
		t.Errorf("BatchSize = %d, want the default %d", cfg.BatchSize, DefaultBatchSize)
	}
}

func TestLoadEmptyAPIKeyIsPresentButEmpty(t *testing.T) {
	t.Parallel()

	// config-set rewrites the whole file, so api_key = "" is what an unset
	// key looks like on disk. It must not be treated as a usable value.
	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, "api_key = ''\n")

	cfg, srcs, err := Load(Options{Path: path})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.APIKey != "" {
		t.Errorf("APIKey = %q, want empty", cfg.APIKey)
	}
	for _, s := range srcs {
		if s.Key == "api_key" && !s.Secret {
			t.Error("api_key source is not marked Secret")
		}
	}
}

func TestLoadErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
		want string
	}{
		{"syntax", "model = \n", "parse"},
		{"wrong type", "batch_size = 'twenty'\n", "parse"},
		{"batch_size too small", "batch_size = 0\n", "batch_size"},
		{"batch_size too large", "batch_size = 201\n", "batch_size"},
		{"concurrency too large", "concurrency = 65\n", "concurrency"},
		{"bad line endings", "line_endings = 'cr'\n", "line_endings"},
		{"bad base url", "base_url = 'ftp://x/'\n", "base_url"},
		{"empty base url", "base_url = ''\n", "base_url"},
		{"bad target language", "target_language = 'klingon'\n", "target_language"},
		{"zero spend", "max_spend_usd = 0.0\n", "max_spend_usd"},
		{"negative spend", "max_spend_usd = -1.0\n", "max_spend_usd"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "config.toml")
			writeFile(t, path, tc.body)

			_, _, err := Load(Options{Path: path})
			if err == nil {
				t.Fatalf("Load(%q) = nil error, want one", tc.body)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Load(%q) error = %q, want it to mention %q", tc.body, err, tc.want)
			}
			if !strings.Contains(err.Error(), path) {
				t.Errorf("Load(%q) error = %q, want it to name the file", tc.body, err)
			}
		})
	}
}

func TestSetCreatesDirAndFileWithTightPermissions(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "ypotitlo")
	path := filepath.Join(dir, "config.toml")
	o := Options{Path: path}

	if err := Set(o, "model", "qwen/qwen3-coder"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	di, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("directory mode = %04o, want 0700", perm)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat file: %v", err)
	}
	// Explicitly chmod'd after creation: the mode passed to file creation is
	// masked by the umask, and this file can hold an API key.
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("file mode = %04o, want 0600", perm)
	}

	// No temp file left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != FileName {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("directory contains %v, want just %s", names, FileName)
	}
}

func TestSetRoundTrip(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "cfg", "config.toml")
	o := Options{Path: path}

	steps := []struct{ key, value string }{
		{"base_url", "https://opencode.ai/zen/v1"},
		{"model", "anthropic/claude-sonnet-4"},
		{"api_key", "sk-test-abcdef1234"},
		{"target_language", "greek"},
		{"line_endings", "CRLF"},
		{"batch_size", "25"},
		{"concurrency", "4"},
		{"output_bom", "true"},
		{"max_spend_usd", "2.5"},
	}
	for _, s := range steps {
		if err := Set(o, s.key, s.value); err != nil {
			t.Fatalf("Set(%s, %s): %v", s.key, s.value, err)
		}
	}

	cfg, srcs, err := Load(o)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := Config{
		BaseURL:        "https://opencode.ai/zen/v1",
		Model:          "anthropic/claude-sonnet-4",
		APIKey:         "sk-test-abcdef1234",
		TargetLanguage: "el", // canonicalised on the way in
		LineEndings:    "crlf",
		BatchSize:      25,
		Concurrency:    4,
		MaxSpendUSD:    2.5,
	}
	got := cfg
	got.OutputBOM = nil
	if got != want {
		t.Errorf("round trip gave %+v, want %+v", got, want)
	}
	if cfg.OutputBOM == nil || !*cfg.OutputBOM {
		t.Errorf("OutputBOM = %v, want true", cfg.OutputBOM)
	}
	for _, s := range srcs {
		if s.From != FromConfig {
			t.Errorf("after setting every key, %s came from %q", s.Key, s.From)
		}
	}

	// The regenerated file documents itself.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{"# ypotitlo configuration", "# OpenAI-compatible API base URL"} {
		if !strings.Contains(text, want) {
			t.Errorf("generated file is missing %q:\n%s", want, text)
		}
	}
	for _, key := range Keys() {
		if !strings.Contains(text, key+" =") {
			t.Errorf("generated file is missing key %q:\n%s", key, text)
		}
	}
}

func TestSetPreservesOtherKeys(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.toml")
	o := Options{Path: path}

	if err := Set(o, "model", "a"); err != nil {
		t.Fatal(err)
	}
	if err := Set(o, "concurrency", "7"); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := Load(o)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model != "a" || cfg.Concurrency != 7 {
		t.Errorf("second Set lost the first: %+v", cfg)
	}
}

func TestSetRepairsAnInvalidFile(t *testing.T) {
	t.Parallel()

	// Load rejects this file; config-set must still be able to fix it,
	// otherwise the only way out is a text editor.
	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, "batch_size = 5000\n")

	if _, _, err := Load(Options{Path: path}); err == nil {
		t.Fatal("Load accepted batch_size = 5000")
	}
	if err := Set(Options{Path: path}, "batch_size", "20"); err != nil {
		t.Fatalf("Set on an invalid file: %v", err)
	}
	cfg, _, err := Load(Options{Path: path})
	if err != nil {
		t.Fatalf("Load after repair: %v", err)
	}
	if cfg.BatchSize != 20 {
		t.Errorf("BatchSize = %d, want 20", cfg.BatchSize)
	}
}

func TestSetUnknownKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		key     string
		suggest string
	}{
		{"batchsize", "batch_size"},
		{"batch-size", "batch_size"},
		{"batch_sze", "batch_size"},
		{"modle", "model"},
		{"apikey", "api_key"},
		{"api-key", "api_key"},
		{"baseurl", "base_url"},
		{"concurency", "concurrency"},
	}

	for _, tc := range tests {
		t.Run(tc.key, func(t *testing.T) {
			t.Parallel()

			err := Set(Options{Path: filepath.Join(t.TempDir(), "config.toml")}, tc.key, "x")
			if !errors.Is(err, ErrUnknownKey) {
				t.Fatalf("Set(%q) error = %v, want ErrUnknownKey", tc.key, err)
			}
			if !strings.Contains(err.Error(), tc.suggest) {
				t.Errorf("Set(%q) error = %q, want it to suggest %q", tc.key, err, tc.suggest)
			}
			// The full list is always there, suggestion or not.
			for _, k := range Keys() {
				if !strings.Contains(err.Error(), k) {
					t.Errorf("Set(%q) error does not list known key %q: %q", tc.key, k, err)
				}
			}
		})
	}
}

func TestSetUnknownKeyNoWildSuggestion(t *testing.T) {
	t.Parallel()

	err := Set(Options{Path: filepath.Join(t.TempDir(), "config.toml")}, "colour", "blue")
	if !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("error = %v, want ErrUnknownKey", err)
	}
	if strings.Contains(err.Error(), "did you mean") {
		t.Errorf("unrelated key produced a suggestion: %q", err)
	}
}

func TestSetDoesNotCreateAFileOnRejection(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "config.toml")
	if err := Set(Options{Path: path}, "batch_size", "0"); err == nil {
		t.Fatal("Set accepted batch_size = 0")
	}
	if _, err := os.Stat(filepath.Dir(path)); err == nil {
		t.Error("a rejected Set created the configuration directory")
	}
}

func TestSetValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		key   string
		value string
		want  string
	}{
		{"batch size zero", "batch_size", "0", "between 1 and 200"},
		{"batch size negative", "batch_size", "-1", "between 1 and 200"},
		{"batch size too big", "batch_size", "201", "between 1 and 200"},
		{"batch size not a number", "batch_size", "twenty", "integer"},
		{"batch size float", "batch_size", "20.5", "integer"},
		{"batch size empty", "batch_size", "", "integer"},
		{"concurrency zero", "concurrency", "0", "between 1 and 64"},
		{"concurrency too big", "concurrency", "65", "between 1 and 64"},
		{"concurrency not a number", "concurrency", "many", "integer"},
		{"line endings unknown", "line_endings", "cr", "lf, crlf, auto"},
		{"line endings empty", "line_endings", "", "lf, crlf, auto"},
		{"base url not a url", "base_url", "::/nope", "URL"},
		{"base url wrong scheme", "base_url", "ftp://example.com", "http or https"},
		{"base url no host", "base_url", "https://", "host"},
		{"base url bare host", "base_url", "example.com", "http or https"},
		{"base url empty", "base_url", "", "empty"},
		{"model empty", "model", "", "empty"},
		{"model with space", "model", "a model", "whitespace"},
		{"api key empty", "api_key", "", "empty"},
		{"api key with bearer", "api_key", "Bearer sk-x", "whitespace"},
		{"target language unknown", "target_language", "klingon", "unknown language"},
		{"target language ambiguous", "target_language", "zh", "zh-Hans"},
		{"target language empty", "target_language", "", "empty"},
		{"output bom not a bool", "output_bom", "yes please", "true or false"},
		{"max spend not a number", "max_spend_usd", "cheap", "number"},
		{"max spend zero", "max_spend_usd", "0", "greater than 0"},
		{"max spend negative", "max_spend_usd", "-0.5", "greater than 0"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "config.toml")
			err := Set(Options{Path: path}, tc.key, tc.value)
			if err == nil {
				t.Fatalf("Set(%s, %q) = nil error, want one", tc.key, tc.value)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Set(%s, %q) error = %q, want it to mention %q", tc.key, tc.value, err, tc.want)
			}
			if _, statErr := os.Stat(path); statErr == nil {
				t.Errorf("Set(%s, %q) wrote a file despite failing", tc.key, tc.value)
			}
		})
	}
}

func TestSetAccepts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		key, value, want string
	}{
		{"line_endings", "LF", "lf"},
		{"line_endings", "  crlf  ", "crlf"},
		{"line_endings", "auto", "auto"},
		{"batch_size", " 200 ", "200"},
		{"batch_size", "1", "1"},
		{"concurrency", "64", "64"},
		{"base_url", "http://localhost:8080/v1", "http://localhost:8080/v1"},
		{"target_language", "GREEK", "el"},
		{"target_language", "gre", "el"},
		{"target_language", "zh-Hant", "zh-Hant"},
		{"output_bom", "false", "false"},
		{"output_bom", "1", "true"},
		{"max_spend_usd", "0.01", "0.01"},
		{"max_spend_usd", "100", "100"},
		// A key pasted from `config-set api_key -` arrives with the
		// terminating newline still attached.
		{"api_key", "sk-abcdef1234\n", "sk-abcdef1234"},
	}

	for _, tc := range tests {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			t.Parallel()

			o := Options{Path: filepath.Join(t.TempDir(), "config.toml")}
			if err := Set(o, tc.key, tc.value); err != nil {
				t.Fatalf("Set(%s, %q): %v", tc.key, tc.value, err)
			}
			cfg, srcs, err := Load(o)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			for _, s := range srcs {
				if s.Key == tc.key && s.Value != tc.want {
					t.Errorf("Set(%s, %q) stored %q, want %q", tc.key, tc.value, s.Value, tc.want)
				}
			}
			_ = cfg
		})
	}
}

func TestSetKeyIsCaseInsensitive(t *testing.T) {
	t.Parallel()

	o := Options{Path: filepath.Join(t.TempDir(), "config.toml")}
	if err := Set(o, "  Batch_Size ", "30"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	cfg, _, err := Load(o)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BatchSize != 30 {
		t.Errorf("BatchSize = %d, want 30", cfg.BatchSize)
	}
}

func TestUnset(t *testing.T) {
	t.Parallel()

	o := Options{Path: filepath.Join(t.TempDir(), "config.toml")}
	for _, s := range []struct{ k, v string }{
		{"model", "qwen"},
		{"batch_size", "50"},
		{"base_url", "http://localhost:1/v1"},
		{"output_bom", "true"},
		{"target_language", "el"},
	} {
		if err := Set(o, s.k, s.v); err != nil {
			t.Fatal(err)
		}
	}

	for _, key := range []string{"model", "batch_size", "base_url", "output_bom", "target_language"} {
		if err := Unset(o, key); err != nil {
			t.Fatalf("Unset(%s): %v", key, err)
		}
	}

	cfg, _, err := Load(o)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Model != "" || cfg.TargetLanguage != "" {
		t.Errorf("keys without a default were not cleared: %+v", cfg)
	}
	if cfg.BatchSize != DefaultBatchSize || cfg.BaseURL != DefaultBaseURL {
		t.Errorf("keys with a default were not restored: %+v", cfg)
	}
	// Tri-state: unset means "preserve the input's BOM", not false.
	if cfg.OutputBOM != nil {
		t.Errorf("OutputBOM = %v, want nil after Unset", *cfg.OutputBOM)
	}
}

func TestUnsetUnknownKey(t *testing.T) {
	t.Parallel()

	err := Unset(Options{Path: filepath.Join(t.TempDir(), "config.toml")}, "batchsize")
	if !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("Unset error = %v, want ErrUnknownKey", err)
	}
	if !strings.Contains(err.Error(), "batch_size") {
		t.Errorf("Unset error = %q, want a suggestion", err)
	}
}

func TestOverride(t *testing.T) {
	t.Parallel()

	_, srcs, err := Load(Options{Path: filepath.Join(t.TempDir(), "config.toml")})
	if err != nil {
		t.Fatal(err)
	}
	srcs = Override(srcs, "model", "cfg-model", FromConfig)
	srcs = Override(srcs, "model", "env-model", FromEnv("OPENCODE_MODEL"))

	var got Source
	for _, s := range srcs {
		if s.Key == "model" {
			got = s
		}
	}
	if got.Value != "env-model" || got.From != "env OPENCODE_MODEL" {
		t.Fatalf("model source = %+v, want the env value", got)
	}
	// Most recently shadowed first, so config-show can print "shadowed by"
	// in precedence order.
	if len(got.Shadowed) != 2 {
		t.Fatalf("shadowed = %+v, want two entries", got.Shadowed)
	}
	if got.Shadowed[0].From != FromConfig || got.Shadowed[0].Value != "cfg-model" {
		t.Errorf("shadowed[0] = %+v, want the config value", got.Shadowed[0])
	}
	if got.Shadowed[1].From != FromDefault {
		t.Errorf("shadowed[1] = %+v, want the default", got.Shadowed[1])
	}

	srcs = Override(srcs, "brand_new", "v", FromFlag)
	if srcs[len(srcs)-1].Key != "brand_new" {
		t.Error("Override did not append an unknown key")
	}
}

func TestRedact(t *testing.T) {
	t.Parallel()

	tests := []struct{ in, want string }{
		{"", ""},
		{"a", "* (1 chars)"},
		{"abcd", "**** (4 chars)"},
		{"sk-abcdef123456", "...3456 (15 chars)"},
		{"κλειδί", "...ειδί (6 chars)"},
	}
	for _, tc := range tests {
		if got := Redact(tc.in); got != tc.want {
			t.Errorf("Redact(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := Redact("sk-secret-value"); strings.Contains(got, "secret") {
		t.Errorf("Redact leaked the key: %q", got)
	}
}

func TestKeysMatchStructTags(t *testing.T) {
	t.Parallel()

	// The settings table and the struct tags are two lists of the same
	// thing; a key in one and not the other silently stops round-tripping.
	tags := map[string]bool{}
	rt := reflect.TypeOf(Config{})
	for i := range rt.NumField() {
		f := rt.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("toml"), ",")
		if name == "" {
			t.Errorf("Config.%s has no toml tag", f.Name)
			continue
		}
		tags[name] = true
		if f.Tag.Get("comment") == "" {
			t.Errorf("Config.%s has no comment tag; the generated file would not document it", f.Name)
		}
	}

	for _, key := range Keys() {
		if !tags[key] {
			t.Errorf("settings key %q has no matching toml tag in Config", key)
		}
		delete(tags, key)
	}
	for name := range tags {
		t.Errorf("Config field %q is not in the settings table, so config-set cannot reach it", name)
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
