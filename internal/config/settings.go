package config

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/mtzanidakis/ypotitlo/internal/lang"
)

// setting is one configuration key, described once and reused by Load
// (defaults and provenance), Set, Unset, Validate and config-show. Keeping
// the four operations next to each other is what stops the settings table
// and the struct from drifting apart.
type setting struct {
	// Name is the TOML key. It must match the struct tag in Config.
	Name string

	// Get formats the effective value for display.
	Get func(Config) string

	// Set parses, validates and assigns a value given as a string.
	Set func(*Config, string) error

	// Reset restores the default, or clears the value if there is none.
	Reset func(*Config)

	// Check validates an already-decoded value.
	Check func(Config) error

	// Secret marks a value that must be redacted before display.
	Secret bool
}

// LineEndingValues are the accepted values of line_endings.
var LineEndingValues = []string{"lf", "crlf", "auto"}

// Range limits. batch_size is bounded above because alignment failures grow
// superlinearly with batch size; concurrency because the service's rate
// limits are undocumented and a large -j is how you find them.
const (
	MinBatchSize   = 1
	MaxBatchSize   = 200
	MinConcurrency = 1
	MaxConcurrency = 64
)

// settings is in the display order used by config-show, which follows the
// struct declaration order rather than being alphabetical.
var settings = []setting{
	{
		Name: "base_url",
		Get:  func(c Config) string { return c.BaseURL },
		Set: func(c *Config, v string) error {
			v = strings.TrimSpace(v)
			if err := checkBaseURL(v); err != nil {
				return err
			}
			c.BaseURL = v
			return nil
		},
		Reset: func(c *Config) { c.BaseURL = DefaultBaseURL },
		Check: func(c Config) error { return checkBaseURL(c.BaseURL) },
	},
	{
		Name: "model",
		Get:  func(c Config) string { return c.Model },
		Set: func(c *Config, v string) error {
			v = strings.TrimSpace(v)
			if v == "" {
				return errors.New("must not be empty; use 'config-unset model' to clear it")
			}
			if strings.ContainsAny(v, " \t\n") {
				return fmt.Errorf("must not contain whitespace, got %q", v)
			}
			c.Model = v
			return nil
		},
		Reset: func(c *Config) { c.Model = "" },
	},
	{
		Name:   "api_key",
		Secret: true,
		Get:    func(c Config) string { return c.APIKey },
		Set: func(c *Config, v string) error {
			v = strings.TrimSpace(v)
			if v == "" {
				return errors.New("must not be empty; use 'config-unset api_key' to clear it")
			}
			// A pasted key that still carries a newline or a "Bearer "
			// prefix produces a 401 that looks like a bad key.
			if strings.ContainsAny(v, " \t\r\n") {
				return errors.New("must not contain whitespace; paste the key alone, without a \"Bearer \" prefix")
			}
			c.APIKey = v
			return nil
		},
		Reset: func(c *Config) { c.APIKey = "" },
	},
	{
		Name: "target_language",
		Get:  func(c Config) string { return c.TargetLanguage },
		Set: func(c *Config, v string) error {
			v = strings.TrimSpace(v)
			if v == "" {
				return errors.New("must not be empty; use 'config-unset target_language' to clear it")
			}
			l, err := lang.Resolve(v)
			if err != nil {
				return err
			}
			// Stored canonically so the file does not depend on how the
			// user happened to spell it.
			c.TargetLanguage = l.Code
			return nil
		},
		Reset: func(c *Config) { c.TargetLanguage = "" },
		Check: func(c Config) error {
			if c.TargetLanguage == "" {
				return nil
			}
			_, err := lang.Resolve(c.TargetLanguage)
			return err
		},
	},
	{
		Name: "line_endings",
		Get:  func(c Config) string { return c.LineEndings },
		Set: func(c *Config, v string) error {
			v = strings.ToLower(strings.TrimSpace(v))
			if err := checkLineEndings(v); err != nil {
				return err
			}
			c.LineEndings = v
			return nil
		},
		Reset: func(c *Config) { c.LineEndings = DefaultLineEndings },
		Check: func(c Config) error { return checkLineEndings(c.LineEndings) },
	},
	{
		Name: "batch_size",
		Get:  func(c Config) string { return strconv.Itoa(c.BatchSize) },
		Set: func(c *Config, v string) error {
			n, err := parseInt(v)
			if err != nil {
				return err
			}
			if err := checkRange(n, MinBatchSize, MaxBatchSize); err != nil {
				return err
			}
			c.BatchSize = n
			return nil
		},
		Reset: func(c *Config) { c.BatchSize = DefaultBatchSize },
		Check: func(c Config) error { return checkRange(c.BatchSize, MinBatchSize, MaxBatchSize) },
	},
	{
		Name: "concurrency",
		Get:  func(c Config) string { return strconv.Itoa(c.Concurrency) },
		Set: func(c *Config, v string) error {
			n, err := parseInt(v)
			if err != nil {
				return err
			}
			if err := checkRange(n, MinConcurrency, MaxConcurrency); err != nil {
				return err
			}
			c.Concurrency = n
			return nil
		},
		Reset: func(c *Config) { c.Concurrency = DefaultConcurrency },
		Check: func(c Config) error { return checkRange(c.Concurrency, MinConcurrency, MaxConcurrency) },
	},
	{
		Name: "output_bom",
		Get: func(c Config) string {
			if c.OutputBOM == nil {
				return ""
			}
			return strconv.FormatBool(*c.OutputBOM)
		},
		Set: func(c *Config, v string) error {
			b, err := strconv.ParseBool(strings.TrimSpace(v))
			if err != nil {
				return fmt.Errorf("must be true or false, got %q", v)
			}
			c.OutputBOM = &b
			return nil
		},
		// Cleared rather than set to false: absent means "preserve the
		// input's BOM", which is not the same answer as false.
		Reset: func(c *Config) { c.OutputBOM = nil },
	},
	{
		Name: "max_spend_usd",
		Get:  func(c Config) string { return formatFloat(c.MaxSpendUSD) },
		Set: func(c *Config, v string) error {
			f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
			if err != nil {
				return fmt.Errorf("must be a number, got %q", v)
			}
			if err := checkSpend(f); err != nil {
				return err
			}
			c.MaxSpendUSD = f
			return nil
		},
		Reset: func(c *Config) { c.MaxSpendUSD = DefaultMaxSpendUSD },
		Check: func(c Config) error { return checkSpend(c.MaxSpendUSD) },
	},
}

// Keys returns the configuration keys in display order.
func Keys() []string {
	out := make([]string, 0, len(settings))
	for _, s := range settings {
		out = append(out, s.Name)
	}
	return out
}

// ErrUnknownKey is returned by Set and Unset for a key that does not exist.
var ErrUnknownKey = errors.New("unknown configuration key")

func lookup(key string) (setting, error) {
	k := strings.ToLower(strings.TrimSpace(key))
	for _, s := range settings {
		if s.Name == k {
			return s, nil
		}
	}

	var suggestion string
	if guess, ok := nearest(k); ok {
		suggestion = fmt.Sprintf("; did you mean %q?", guess)
	}
	return setting{}, fmt.Errorf("%w %q%s\nknown keys: %s",
		ErrUnknownKey, key, suggestion, strings.Join(Keys(), ", "))
}

// nearest returns the closest known key by edit distance, if it is close
// enough to be a plausible typo rather than a different word entirely.
func nearest(key string) (string, bool) {
	best, bestDist := "", math.MaxInt
	for _, name := range Keys() {
		if d := levenshtein(key, name); d < bestDist {
			best, bestDist = name, d
		}
	}
	// A third of the key length, with a floor of 2 so that short keys still
	// get a suggestion and long ones do not match anything vaguely similar.
	limit := max(2, len(best)/3)
	if bestDist > limit {
		return "", false
	}
	return best, true
}

// levenshtein is the ordinary edit distance, two rows at a time.
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	if len(ra) == 0 {
		return len(rb)
	}
	prev := make([]int, len(rb)+1)
	curr := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		curr[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			curr[j] = min(min(curr[j-1]+1, prev[j]+1), prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[len(rb)]
}

// Validate checks every setting of an already-decoded configuration.
func Validate(c Config) error {
	for _, s := range settings {
		if s.Check == nil {
			continue
		}
		if err := s.Check(c); err != nil {
			return fmt.Errorf("%s: %w", s.Name, err)
		}
	}
	return nil
}

func checkBaseURL(v string) error {
	if v == "" {
		return errors.New("must not be empty")
	}
	u, err := url.Parse(v)
	if err != nil {
		return fmt.Errorf("must be a URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("must be an http or https URL, got %q", v)
	}
	if u.Host == "" {
		return fmt.Errorf("must include a host, got %q", v)
	}
	return nil
}

func checkLineEndings(v string) error {
	if slices.Contains(LineEndingValues, v) {
		return nil
	}
	return fmt.Errorf("must be one of %s, got %q", strings.Join(LineEndingValues, ", "), v)
}

func checkRange(n, lo, hi int) error {
	if n < lo || n > hi {
		return fmt.Errorf("must be between %d and %d, got %d", lo, hi, n)
	}
	return nil
}

func checkSpend(f float64) error {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return fmt.Errorf("must be a finite number, got %v", f)
	}
	if f <= 0 {
		return fmt.Errorf("must be greater than 0, got %s; a zero ceiling aborts every run", formatFloat(f))
	}
	return nil
}

func parseInt(v string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return 0, fmt.Errorf("must be an integer, got %q", v)
	}
	return n, nil
}
