package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAuthFilePath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		o    Options
		want string
	}{
		{
			name: "explicit override",
			o:    Options{AuthPath: "/tmp/auth.json"},
			want: "/tmp/auth.json",
		},
		{
			name: "xdg data home",
			o:    Options{Getenv: stubEnv(map[string]string{"XDG_DATA_HOME": "/data", "HOME": "/home/u"})},
			want: "/data/opencode/auth.json",
		},
		{
			name: "home fallback",
			o:    Options{Getenv: stubEnv(map[string]string{"HOME": "/home/u"})},
			want: "/home/u/.local/share/opencode/auth.json",
		},
		{
			name: "relative xdg ignored",
			o:    Options{Getenv: stubEnv(map[string]string{"XDG_DATA_HOME": "rel", "HOME": "/home/u"})},
			want: "/home/u/.local/share/opencode/auth.json",
		},
		{
			// HOME=/nonexistent is not the same as no HOME; with neither, we
			// simply have nowhere to look and say so.
			name: "no home at all",
			o:    Options{Getenv: stubEnv(map[string]string{})},
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := AuthFilePath(tc.o)
			want := tc.want
			if want != "" {
				want = filepath.FromSlash(want)
			}
			if got != want {
				t.Errorf("AuthFilePath() = %q, want %q", got, want)
			}
		})
	}
}

// TestResolveAPIKeyPrecedence walks the whole ladder from the top down,
// removing one source at a time, so that every rung is exercised with all
// the lower ones still present.
func TestResolveAPIKeyPrecedence(t *testing.T) {
	t.Parallel()

	authPath := filepath.Join(t.TempDir(), "auth.json")
	writeFile(t, authPath, `{"opencode-go":{"type":"api","key":"key-auth"}}`)

	fullEnv := map[string]string{
		EnvAPIKey:    "key-env1",
		EnvZenAPIKey: "key-env2",
	}
	cfg := Config{APIKey: "key-config"}

	tests := []struct {
		name       string
		flag       string
		env        map[string]string
		cfg        Config
		wantKey    string
		wantSource string
	}{
		{
			name:       "flag wins",
			flag:       "key-flag",
			env:        fullEnv,
			cfg:        cfg,
			wantKey:    "key-flag",
			wantSource: FromFlag,
		},
		{
			name:       "primary env beats zen env",
			env:        fullEnv,
			cfg:        cfg,
			wantKey:    "key-env1",
			wantSource: "env " + EnvAPIKey,
		},
		{
			name:       "zen env beats config",
			env:        map[string]string{EnvZenAPIKey: "key-env2"},
			cfg:        cfg,
			wantKey:    "key-env2",
			wantSource: "env " + EnvZenAPIKey,
		},
		{
			name:       "config beats auth.json",
			env:        map[string]string{},
			cfg:        cfg,
			wantKey:    "key-config",
			wantSource: FromConfig,
		},
		{
			name:       "auth.json last",
			env:        map[string]string{},
			cfg:        Config{},
			wantKey:    "key-auth",
			wantSource: "auth.json (opencode-go)",
		},
		{
			// The regenerated config file always contains api_key = "", so
			// an empty value must not short-circuit the fallback.
			name:       "empty config api_key does not block auth.json",
			env:        map[string]string{},
			cfg:        Config{APIKey: ""},
			wantKey:    "key-auth",
			wantSource: "auth.json (opencode-go)",
		},
		{
			name:       "whitespace-only config api_key does not block auth.json",
			env:        map[string]string{},
			cfg:        Config{APIKey: "   "},
			wantKey:    "key-auth",
			wantSource: "auth.json (opencode-go)",
		},
		{
			name:       "empty env var is skipped",
			env:        map[string]string{EnvAPIKey: "", EnvZenAPIKey: "key-env2"},
			cfg:        Config{},
			wantKey:    "key-env2",
			wantSource: "env " + EnvZenAPIKey,
		},
		{
			name:       "empty flag is skipped",
			flag:       "  ",
			env:        map[string]string{},
			cfg:        cfg,
			wantKey:    "key-config",
			wantSource: FromConfig,
		},
		{
			name:       "values are trimmed",
			env:        map[string]string{EnvAPIKey: " key-env1\n"},
			cfg:        Config{},
			wantKey:    "key-env1",
			wantSource: "env " + EnvAPIKey,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			o := Options{Getenv: stubEnv(tc.env), AuthPath: authPath}
			key, source, err := ResolveAPIKey(o, tc.cfg, tc.flag)
			if err != nil {
				t.Fatalf("ResolveAPIKey: %v", err)
			}
			if key != tc.wantKey {
				t.Errorf("key = %q, want %q", key, tc.wantKey)
			}
			if source != tc.wantSource {
				t.Errorf("source = %q, want %q", source, tc.wantSource)
			}
		})
	}
}

func TestResolveAPIKeyAuthProviders(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		body       string
		wantKey    string
		wantSource string
	}{
		{
			name:       "opencode-go",
			body:       `{"opencode-go":{"type":"api","key":"k1"}}`,
			wantKey:    "k1",
			wantSource: "auth.json (opencode-go)",
		},
		{
			name:       "opencode",
			body:       `{"opencode":{"type":"api","key":"k2"}}`,
			wantKey:    "k2",
			wantSource: "auth.json (opencode)",
		},
		{
			name:       "opencode-zen",
			body:       `{"opencode-zen":{"type":"api","key":"k3"}}`,
			wantKey:    "k3",
			wantSource: "auth.json (opencode-zen)",
		},
		{
			name:       "opencode-go preferred",
			body:       `{"opencode":{"type":"api","key":"k2"},"opencode-go":{"type":"api","key":"k1"}}`,
			wantKey:    "k1",
			wantSource: "auth.json (opencode-go)",
		},
		{
			// An oauth entry has no key at all. Reading it without checking
			// the type sends an empty bearer token and produces a 401 that
			// looks like a wrong key rather than a wrong login method.
			name:       "oauth entry skipped for the next provider",
			body:       `{"opencode-go":{"type":"oauth","access":"a","refresh":"r"},"opencode":{"type":"api","key":"k2"}}`,
			wantKey:    "k2",
			wantSource: "auth.json (opencode)",
		},
		{
			name:       "entry without a type is treated as an api key",
			body:       `{"opencode-go":{"key":"k1"}}`,
			wantKey:    "k1",
			wantSource: "auth.json (opencode-go)",
		},
		{
			name:       "unrelated providers ignored",
			body:       `{"anthropic":{"type":"api","key":"nope"},"opencode-go":{"type":"api","key":"k1"}}`,
			wantKey:    "k1",
			wantSource: "auth.json (opencode-go)",
		},
		{
			name:       "non-object entry does not poison the file",
			body:       `{"broken":"a string","opencode-go":{"type":"api","key":"k1"}}`,
			wantKey:    "k1",
			wantSource: "auth.json (opencode-go)",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			authPath := filepath.Join(t.TempDir(), "auth.json")
			writeFile(t, authPath, tc.body)

			key, source, err := ResolveAPIKey(Options{Getenv: stubEnv(nil), AuthPath: authPath}, Config{}, "")
			if err != nil {
				t.Fatalf("ResolveAPIKey: %v", err)
			}
			if key != tc.wantKey {
				t.Errorf("key = %q, want %q", key, tc.wantKey)
			}
			if source != tc.wantSource {
				t.Errorf("source = %q, want %q", source, tc.wantSource)
			}
		})
	}
}

func TestResolveAPIKeyFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string // "" means: do not create the file
		want []string
	}{
		{
			name: "no file",
			want: []string{"OPENCODE_API_KEY", "OPENCODE_ZEN_API_KEY", "api_key"},
		},
		{
			// The whole point of checking "type": the user *is* logged in,
			// just not in a way that yields a bearer token.
			name: "only an oauth entry",
			body: `{"opencode-go":{"type":"oauth","access":"a","refresh":"r"}}`,
			want: []string{"oauth", "opencode-go", "not an api key"},
		},
		{
			name: "api entry with an empty key",
			body: `{"opencode-go":{"type":"api","key":""}}`,
			want: []string{"empty key"},
		},
		{
			name: "every provider is oauth",
			body: `{"opencode-go":{"type":"oauth"},"opencode":{"type":"oauth"},"opencode-zen":{"type":"oauth"}}`,
			want: []string{"opencode-go", "opencode-zen"},
		},
		{
			name: "no matching provider",
			body: `{"anthropic":{"type":"api","key":"k"}}`,
			want: []string{"no entry for"},
		},
		{
			name: "empty object",
			body: `{}`,
			want: []string{"OPENCODE_API_KEY"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			authPath := filepath.Join(t.TempDir(), "auth.json")
			if tc.body != "" {
				writeFile(t, authPath, tc.body)
			}

			key, source, err := ResolveAPIKey(Options{Getenv: stubEnv(nil), AuthPath: authPath}, Config{}, "")
			if !errors.Is(err, ErrNoAPIKey) {
				t.Fatalf("ResolveAPIKey = (%q, %q, %v), want ErrNoAPIKey", key, source, err)
			}
			if key != "" || source != "" {
				t.Errorf("got key %q source %q on failure, want both empty", key, source)
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not mention %q", err, w)
				}
			}
			if !strings.Contains(err.Error(), authPath) {
				t.Errorf("error %q does not name the auth file", err)
			}
		})
	}
}

func TestResolveAPIKeyMalformedAuthFile(t *testing.T) {
	t.Parallel()

	authPath := filepath.Join(t.TempDir(), "auth.json")
	writeFile(t, authPath, "{not json")

	// A corrupt credentials file is reported rather than silently treated as
	// "no key", because the two need different fixes.
	_, _, err := ResolveAPIKey(Options{Getenv: stubEnv(nil), AuthPath: authPath}, Config{}, "")
	if err == nil {
		t.Fatal("ResolveAPIKey accepted a malformed auth.json")
	}
	if errors.Is(err, ErrNoAPIKey) {
		t.Errorf("malformed file reported as ErrNoAPIKey: %v", err)
	}
	if !strings.Contains(err.Error(), authPath) {
		t.Errorf("error %q does not name the file", err)
	}
}

// TestResolveAPIKeyNoHome is the HOME=/nonexistent case: nothing on disk is
// reachable, and the tool must still fail with a useful message instead of
// panicking or reading someone's real credentials.
func TestResolveAPIKeyNoHome(t *testing.T) {
	t.Parallel()

	_, _, err := ResolveAPIKey(Options{Getenv: stubEnv(nil)}, Config{}, "")
	if !errors.Is(err, ErrNoAPIKey) {
		t.Fatalf("error = %v, want ErrNoAPIKey", err)
	}
	if strings.Contains(err.Error(), ".local/share") {
		t.Errorf("error names an auth path that could not be derived: %q", err)
	}
}

func TestResolveAPIKeyUnreadableAuthFile(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	authPath := filepath.Join(t.TempDir(), "auth.json")
	writeFile(t, authPath, `{"opencode-go":{"type":"api","key":"k"}}`)
	if err := os.Chmod(authPath, 0o000); err != nil {
		t.Fatal(err)
	}

	_, _, err := ResolveAPIKey(Options{Getenv: stubEnv(nil), AuthPath: authPath}, Config{}, "")
	if err == nil {
		t.Fatal("ResolveAPIKey ignored an unreadable auth.json")
	}
	if errors.Is(err, ErrNoAPIKey) {
		t.Errorf("permission error reported as ErrNoAPIKey: %v", err)
	}
}
