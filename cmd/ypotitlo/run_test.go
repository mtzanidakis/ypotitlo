package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testEnv builds an env whose stdio is captured and whose environment is a
// fixed map, so no test reads the real environment or the real home directory.
func testEnv(t *testing.T, args []string, environ map[string]string) (env, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	var out, errb bytes.Buffer
	return env{
		Stdout: &out,
		Stderr: &errb,
		Stdin:  strings.NewReader(""),
		Args:   args,
		Getenv: func(k string) string { return environ[k] },
		Now:    func() time.Time { return time.Unix(0, 0).UTC() },
	}, &out, &errb
}

func TestRunDispatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		args     []string
		want     int
		inStdout string
		inStderr string
	}{
		{name: "no arguments", args: nil, want: exitUsage, inStderr: "Usage:"},
		{name: "help", args: []string{"-h"}, want: exitOK, inStdout: "Usage:"},
		{name: "help word", args: []string{"help"}, want: exitOK, inStdout: "Commands:"},
		{name: "version", args: []string{"version"}, want: exitOK, inStdout: "dev"},
		{name: "version flag", args: []string{"--version"}, want: exitOK, inStdout: "dev"},
		{name: "unknown command", args: []string{"frobnicate"}, want: exitUsage, inStderr: `unknown command "frobnicate"`},
		{name: "version rejects arguments", args: []string{"version", "x"}, want: exitUsage, inStderr: "takes no arguments"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e, out, errb := testEnv(t, tt.args, nil)
			if got := run(context.Background(), e); got != tt.want {
				t.Errorf("exit = %d, want %d (stderr: %s)", got, tt.want, errb)
			}
			if tt.inStdout != "" && !strings.Contains(out.String(), tt.inStdout) {
				t.Errorf("stdout = %q, want it to contain %q", out, tt.inStdout)
			}
			if tt.inStderr != "" && !strings.Contains(errb.String(), tt.inStderr) {
				t.Errorf("stderr = %q, want it to contain %q", errb, tt.inStderr)
			}
		})
	}
}

// TestFlagValidation covers the two stdlib flag behaviours that are silent and
// wrong for this tool: a missing value swallowing the next flag, and parsing
// stopping at the first non-flag argument.
func TestFlagValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name:    "omitted value swallows the next flag",
			args:    []string{"-config", "-json"},
			wantErr: "looks like a flag",
		},
		{
			name:    "stray positional argument",
			args:    []string{"models.txt"},
			wantErr: "unexpected argument",
		},
		{
			name:    "unknown flag",
			args:    []string{"-nope", "x"},
			wantErr: "not defined",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fs := newFlagSet("test")
			fs.String("config", "", "")
			fs.Bool("json", false, "")

			err := parseFlags(fs, tt.args)
			if err == nil {
				t.Fatalf("parseFlags(%q) = nil, want an error", tt.args)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestFlagValidationAcceptsStdinSentinel(t *testing.T) {
	t.Parallel()

	fs := newFlagSet("test")
	in := fs.String("i", "", "")
	if err := parseFlags(fs, []string{"-i", "-"}, "i"); err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if *in != "-" {
		t.Errorf("i = %q, want %q", *in, "-")
	}
}

// TestMultiCharSingleDashFlags is the reason this tool uses the stdlib flag
// package: cobra/pflag reads -il as the clustered shorthand -i l.
func TestMultiCharSingleDashFlags(t *testing.T) {
	t.Parallel()

	for _, args := range [][]string{
		{"-i", "m.srt", "-il", "en", "-ol", "el"},
		{"-i=m.srt", "-il=en", "-ol=el"},
		{"--i", "m.srt", "--il", "en", "--ol", "el"},
	} {
		fs := newFlagSet("translate")
		in := fs.String("i", "", "")
		il := fs.String("il", "", "")
		ol := fs.String("ol", "", "")
		if err := parseFlags(fs, args); err != nil {
			t.Fatalf("parseFlags(%q): %v", args, err)
		}
		if *in != "m.srt" || *il != "en" || *ol != "el" {
			t.Errorf("parseFlags(%q) = i:%q il:%q ol:%q, want m.srt/en/el", args, *in, *il, *ol)
		}
	}
}

func TestConfigSetAndShow(t *testing.T) {
	t.Parallel()

	cfg := filepath.Join(t.TempDir(), "config.toml")

	e, out, errb := testEnv(t, []string{"config-set", "-config", cfg, "model", "deepseek-v4-pro"}, nil)
	if got := run(context.Background(), e); got != exitOK {
		t.Fatalf("config-set exit = %d (stderr: %s)", got, errb)
	}
	if !strings.Contains(out.String(), "model set") {
		t.Errorf("stdout = %q", out)
	}

	info, err := os.Stat(cfg)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("config mode = %o, want 600", perm)
	}

	// A target language given by name must be stored canonically, so that
	// -ol greek and -ol el name the same output file.
	e, _, errb = testEnv(t, []string{"config-set", "-config", cfg, "target_language", "greek"}, nil)
	if got := run(context.Background(), e); got != exitOK {
		t.Fatalf("config-set target_language exit = %d (stderr: %s)", got, errb)
	}

	e, out, errb = testEnv(t, []string{"config-show", "-config", cfg}, nil)
	if got := run(context.Background(), e); got != exitOK {
		t.Fatalf("config-show exit = %d (stderr: %s)", got, errb)
	}
	for _, want := range []string{"deepseek-v4-pro", "target_language", "el", "SOURCE"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("config-show output missing %q:\n%s", want, out)
		}
	}
}

func TestConfigShowRedactsAndAttributesTheKey(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.toml")
	const key = "sk-secret-value-1234"

	// An auth.json that does not exist must not be mistaken for a key source.
	environ := map[string]string{"OPENCODE_API_KEY": key, "XDG_DATA_HOME": dir}

	e, out, errb := testEnv(t, []string{"config-show", "-config", cfg}, environ)
	if got := run(context.Background(), e); got != exitOK {
		t.Fatalf("config-show exit = %d (stderr: %s)", got, errb)
	}
	if strings.Contains(out.String(), key) {
		t.Errorf("config-show leaked the API key:\n%s", out)
	}
	if !strings.Contains(out.String(), "1234") {
		t.Errorf("config-show should show the last four characters:\n%s", out)
	}
	if !strings.Contains(out.String(), "env OPENCODE_API_KEY") {
		t.Errorf("config-show should name the environment variable it used:\n%s", out)
	}
}

func TestConfigSetRejectsBadValues(t *testing.T) {
	t.Parallel()

	cfg := filepath.Join(t.TempDir(), "config.toml")

	tests := []struct {
		name  string
		args  []string
		inErr string
	}{
		{"unknown key", []string{"nope", "1"}, "unknown"},
		{"non-numeric batch size", []string{"batch_size", "abc"}, "batch_size"},
		{"out-of-range concurrency", []string{"concurrency", "999"}, "concurrency"},
		{"bad line endings", []string{"line_endings", "windows"}, "line_endings"},
		{"missing value", []string{"model"}, "KEY and VALUE"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			args := append([]string{"config-set", "-config", cfg}, tt.args...)
			e, _, errb := testEnv(t, args, nil)
			if got := run(context.Background(), e); got == exitOK {
				t.Fatalf("config-set %q succeeded, want failure", tt.args)
			}
			if !strings.Contains(errb.String(), tt.inErr) {
				t.Errorf("stderr = %q, want it to contain %q", errb, tt.inErr)
			}
		})
	}
}

func TestConfigSetSecretFromStdin(t *testing.T) {
	t.Parallel()

	cfg := filepath.Join(t.TempDir(), "config.toml")
	e, _, errb := testEnv(t, []string{"config-set", "-config", cfg, "api_key", "-"}, nil)
	e.Stdin = strings.NewReader("sk-from-stdin-9999\n")

	if got := run(context.Background(), e); got != exitOK {
		t.Fatalf("config-set exit = %d (stderr: %s)", got, errb)
	}
	// Reading from stdin is the way to keep a key out of the shell history, so
	// it must not also emit the warning that the argv path does.
	if strings.Contains(errb.String(), "shell history") {
		t.Errorf("stdin path should not warn about shell history: %s", errb)
	}

	b, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(b), "sk-from-stdin-9999") {
		t.Errorf("key not stored:\n%s", b)
	}
	// The trailing newline from a here-string or a pipe must not become part
	// of the credential.
	if strings.Contains(string(b), "sk-from-stdin-9999\\n") {
		t.Errorf("trailing newline was stored with the key:\n%s", b)
	}
}

func TestConfigSetWarnsWhenSecretComesFromArgv(t *testing.T) {
	t.Parallel()

	cfg := filepath.Join(t.TempDir(), "config.toml")
	e, _, errb := testEnv(t, []string{"config-set", "-config", cfg, "api_key", "sk-on-the-command-line"}, nil)

	if got := run(context.Background(), e); got != exitOK {
		t.Fatalf("config-set exit = %d (stderr: %s)", got, errb)
	}
	if !strings.Contains(errb.String(), "shell history") {
		t.Errorf("stderr = %q, want a shell-history warning", errb)
	}
}

func TestConfigUnset(t *testing.T) {
	t.Parallel()

	cfg := filepath.Join(t.TempDir(), "config.toml")

	e, _, errb := testEnv(t, []string{"config-set", "-config", cfg, "model", "kimi-k3"}, nil)
	if got := run(context.Background(), e); got != exitOK {
		t.Fatalf("config-set exit = %d (stderr: %s)", got, errb)
	}

	e, _, errb = testEnv(t, []string{"config-unset", "-config", cfg, "model"}, nil)
	if got := run(context.Background(), e); got != exitOK {
		t.Fatalf("config-unset exit = %d (stderr: %s)", got, errb)
	}

	e, out, errb := testEnv(t, []string{"config-show", "-config", cfg}, nil)
	if got := run(context.Background(), e); got != exitOK {
		t.Fatalf("config-show exit = %d (stderr: %s)", got, errb)
	}
	if strings.Contains(out.String(), "kimi-k3") {
		t.Errorf("model survived config-unset:\n%s", out)
	}
}

// TestCancelledContextReportsNoOutputWritten pins the promise the tool makes
// on Ctrl-C: it never leaves a half-translated file behind.
func TestCancelledContextReportsNoOutputWritten(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	e, _, errb := testEnv(t, nil, nil)
	if got := exitCode(ctx, e, context.Canceled); got != exitCanceled {
		t.Errorf("exit = %d, want %d", got, exitCanceled)
	}
	if !strings.Contains(errb.String(), "no output written") {
		t.Errorf("stderr = %q", errb)
	}
}
