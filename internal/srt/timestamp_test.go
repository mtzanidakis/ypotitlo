package srt

import (
	"strings"
	"testing"
	"time"
)

func TestParseTimeToken(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want time.Duration
		warn string
	}{
		// Rule 2: every separator is a colon.
		{name: "HH:MM:SS is not MM:SS:mmm", in: "0:01:30", want: 90 * time.Second},
		{name: "two digit hours, no milliseconds", in: "00:01:35", want: 95 * time.Second},
		{name: "HH:MM:SS:mmm", in: "00:01:36:500", want: 96500 * time.Millisecond},
		{name: "MM:SS", in: "02:00", want: 2 * time.Minute},
		{name: "M:S", in: "2:5", want: 2*time.Minute + 5*time.Second},

		// Rule 1: a comma or a dot marks the millisecond boundary.
		{name: "comma", in: "00:00:01,000", want: time.Second},
		{name: "dot", in: "00:00:01.500", want: 1500 * time.Millisecond},
		{name: "one millisecond digit is a decimal fraction", in: "00:00:03,5", want: 3500 * time.Millisecond},
		{name: "two millisecond digits are a decimal fraction", in: "00:00:04,05", want: 4050 * time.Millisecond},
		{
			name: "more than three millisecond digits truncate",
			in:   "00:00:05,12345",
			want: 5123 * time.Millisecond,
			warn: "truncated to three digits",
		},
		{name: "no hours", in: "01:30,250", want: 90250 * time.Millisecond},
		{name: "seconds only", in: "1,5", want: 1500 * time.Millisecond},

		// Hours are unbounded, minutes and seconds are not but are kept.
		{name: "three digit hours", in: "100:00:00,000", want: 100 * time.Hour},
		{name: "past twenty-four hours", in: "25:30:00,500", want: 25*time.Hour + 30*time.Minute + 500*time.Millisecond},
		{name: "minutes out of range", in: "00:90:00,000", want: 90 * time.Minute, warn: "minutes value 90"},
		{name: "seconds out of range", in: "00:00:75,000", want: 75 * time.Second, warn: "seconds value 75"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, warns, err := parseTimeToken(tc.in)
			if err != nil {
				t.Fatalf("parseTimeToken(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("parseTimeToken(%q) = %v, want %v", tc.in, got, tc.want)
			}
			switch {
			case tc.warn == "" && len(warns) != 0:
				t.Errorf("unexpected warnings %q", warns)
			case tc.warn != "" && (len(warns) != 1 || !strings.Contains(warns[0], tc.warn)):
				t.Errorf("warnings = %q, want one containing %q", warns, tc.warn)
			}
		})
	}
}

func TestParseTimeTokenErrors(t *testing.T) {
	t.Parallel()

	for _, in := range []string{
		"",
		"abc",
		"00:00:0a,000",
		"00::01,000",
		"00:00:01,",
		",500",
		"1:2:3:4:5",
		"00,00,000",     // two decimal separators
		"00,00:000",     // decimal separator not before the last field
		"00:000:00,000", // three digit minutes
		"00:00:000,000", // three digit seconds
		"12345678:00:00,000",
		"90",   // a bare number is not a timestamp
		"-1:0", // no signs
		"1 2",
		"1:2:3:4,5", // too many fields before the milliseconds
	} {
		if _, _, err := parseTimeToken(in); err == nil {
			t.Errorf("parseTimeToken(%q) = nil error, want one", in)
		}
	}
}

func TestParseTimeLine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		in               string
		wantStart, wantE time.Duration
	}{
		{
			name:      "the case that silently destroys files",
			in:        "0:01:30 --> 0:01:35",
			wantStart: 90 * time.Second, wantE: 95 * time.Second,
		},
		{
			name:      "canonical",
			in:        "00:00:01,000 --> 00:00:02,000",
			wantStart: time.Second, wantE: 2 * time.Second,
		},
		{
			name:      "no spaces",
			in:        "00:00:01,000-->00:00:02,000",
			wantStart: time.Second, wantE: 2 * time.Second,
		},
		{
			name:      "extra dashes",
			in:        "00:00:01,000 ----> 00:00:02,000",
			wantStart: time.Second, wantE: 2 * time.Second,
		},
		{
			name:      "leading and trailing whitespace",
			in:        "  00:00:01,000   -->   00:00:02,000\t",
			wantStart: time.Second, wantE: 2 * time.Second,
		},
		{
			name:      "legacy coordinates are discarded",
			in:        "00:00:01,000 --> 00:00:02,000  X1:040 X2:600 Y1:050 Y2:100",
			wantStart: time.Second, wantE: 2 * time.Second,
		},
		{
			name:      "mixed forms on the two sides",
			in:        "0:01:30 --> 00:01:35,5",
			wantStart: 90 * time.Second, wantE: 95500 * time.Millisecond,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			start, end, _, err := parseTimeLine(tc.in)
			if err != nil {
				t.Fatalf("parseTimeLine(%q): %v", tc.in, err)
			}
			if start != tc.wantStart || end != tc.wantE {
				t.Errorf("parseTimeLine(%q) = %v, %v, want %v, %v", tc.in, start, end, tc.wantStart, tc.wantE)
			}
		})
	}
}

func TestParseTimeLineErrors(t *testing.T) {
	t.Parallel()

	for _, in := range []string{
		"",
		"just text",
		"00:00:01,000",                      // no arrow
		"00:00:01,000 -> 00:00:02,000",      // a single dash is not the separator
		"--> 00:00:02,000",                  // nothing on the left
		"00:00:01,000 -->",                  // nothing on the right
		"Point A --> Point B",               // the arrow-in-text case
		"see 00:00:01,000 --> 00:00:02,000", // junk on the left is not discarded
		"00:00:01,000 --> nonsense",         // the right side is checked too
	} {
		if _, _, _, err := parseTimeLine(in); err == nil {
			t.Errorf("parseTimeLine(%q) = nil error, want one", in)
		}
	}
}

func TestSplitArrow(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in          string
		left, right string
		ok          bool
	}{
		{in: "a --> b", left: "a ", right: " b", ok: true},
		{in: "a-->b", left: "a", right: "b", ok: true},
		{in: "a ----> b", left: "a ", right: " b", ok: true},
		{in: "a -> b", ok: false},
		{in: "a - - > b", ok: false},
		{in: "no arrow", ok: false},
		{in: "--", ok: false},
		{in: "a --> b --> c", left: "a ", right: " b --> c", ok: true},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()

			left, right, ok := splitArrow(tc.in)
			if ok != tc.ok || left != tc.left || right != tc.right {
				t.Errorf("splitArrow(%q) = %q, %q, %v, want %q, %q, %v",
					tc.in, left, right, ok, tc.left, tc.right, tc.ok)
			}
		})
	}
}

func TestFormatTime(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		in   time.Duration
		want string
	}{
		{0, "00:00:00,000"},
		{time.Second, "00:00:01,000"},
		{90 * time.Second, "00:01:30,000"},
		{5500 * time.Millisecond, "00:00:05,500"},
		{50 * time.Millisecond, "00:00:00,050"},
		{25*time.Hour + 30*time.Minute, "25:30:00,000"},
		{100 * time.Hour, "100:00:00,000"},
		{90 * time.Minute, "01:30:00,000"},
		{-time.Second, "00:00:00,000"},
		{1500*time.Microsecond + time.Second, "00:00:01,001"},
	} {
		if got := formatTime(tc.in); got != tc.want {
			t.Errorf("formatTime(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The millisecond field is a decimal fraction, so ",5" is 500ms. ffmpeg reads
// it as 5ms. The divergence is deliberate and documented; this test pins it.
func TestMillisecondsAreADecimalFraction(t *testing.T) {
	t.Parallel()

	for in, want := range map[string]int{",5": 500, ",05": 50, ",005": 5, ",50": 500, ",500": 500} {
		got, _ := parseMilliseconds("test", strings.TrimPrefix(in, ","))
		if got != want {
			t.Errorf("%q = %dms, want %dms", in, got, want)
		}
	}
}
