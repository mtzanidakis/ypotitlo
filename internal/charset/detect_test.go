package charset

import (
	"strings"
	"testing"

	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/encoding/unicode/utf32"
)

func TestGreekLadderOrder(t *testing.T) {
	t.Parallel()

	// Each case pits a lower rule against a higher one to prove the ordering,
	// not just the individual rules.
	tests := []struct {
		name    string
		in      []byte
		want    string
		wantTie bool
	}{
		{
			name: "rule 1 beats rule 2",
			in:   []byte{0x97, 0xC1, 0xB6, 0xE1},
			want: nameWindows1253,
		},
		{
			name: "rule 2 beats rule 3",
			in:   []byte{0xB6, 0xC1, 0xAE, 0xE1},
			want: nameISO88597,
		},
		{
			name: "rule 2 fires on the euro sign",
			in:   []byte{0xA4, 0xC1, 0xE1},
			want: nameISO88597,
		},
		{
			name: "rule 2 fires on 0xAA, undefined in windows-1253",
			in:   []byte{0xAA, 0xC1, 0xE1},
			want: nameISO88597,
		},
		{
			name: "rule 3 beats rule 4",
			in:   []byte{0xAE, 0xC1, 0xA2, 0x20, 0xE1},
			want: nameWindows1253,
		},
		{
			name: "rule 4 reads an elision apostrophe as iso-8859-7",
			// Σ ’ space α: a Greek letter, the mark, then a break.
			in:   []byte{0xD3, 0xA2, 0x20, 0xE1, 0xF5, 0xF4, 0xFC},
			want: nameISO88597,
		},
		{
			name: "rule 4 reads a word-initial accented capital as windows-1253",
			// space Ά λ: start of a word followed by a lowercase Greek letter.
			in:   []byte{0x20, 0xA2, 0xEB, 0xEB, 0xEF, 0xF2},
			want: nameWindows1253,
		},
		{
			name: "rule 4 takes the majority over the file",
			in: []byte{
				0x20, 0xA2, 0xEB, 0xEB, 0xEF, 0xF2, // one vote for windows-1253
				0xD3, 0xA2, 0x20, 0xE1, // and two for iso-8859-7
				0xF0, 0xA2, 0x20, 0xF4,
			},
			want: nameISO88597,
		},
		{
			name:    "one vote each is a tie",
			in:      []byte{0x20, 0xA2, 0xEB, 0x20, 0xD3, 0xA2, 0x20, 0xE1},
			want:    nameWindows1253,
			wantTie: true,
		},
		{
			name:    "no evidence at all is a tie",
			in:      []byte{0xCA, 0xE1, 0xEB, 0xE7, 0xEC, 0xDD, 0xF1, 0xE1},
			want:    nameWindows1253,
			wantTie: true,
		},
		{
			name: "an apostrophe at the very end of the file still counts",
			in:   []byte{0xD3, 0xA2},
			want: nameISO88597,
		},
		{
			name: "punctuation after the mark is a break too",
			// Σ ’ full stop.
			in:   []byte{0xD3, 0xA2, '.'},
			want: nameISO88597,
		},
		{
			name:    "a Latin letter after the mark is neither reading",
			in:      []byte{0xD3, 0xA2, 'x', 0xE1, 0xF5},
			want:    nameWindows1253,
			wantTie: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, tie := greekLadder(tt.in)
			if got != tt.want {
				t.Errorf("greekLadder = %q, want %q", got, tt.want)
			}
			if tie != tt.wantTie {
				t.Errorf("tie = %v, want %v", tie, tt.wantTie)
			}
		})
	}
}

func TestLooksGreek(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   []byte
		want bool
	}{
		{
			name: "greek subtitle",
			in:   mustEncode(t, charmap.Windows1253, srtPlain),
			want: true,
		},
		{
			name: "french subtitle",
			in:   mustEncode(t, charmap.Windows1252, "Je ne sais pas quoi dire, chérie. Il faut partir tout de suite, mon frère."),
			want: false,
		},
		{
			name: "pure ascii",
			in:   []byte(srtASCII),
			want: false,
		},
		{
			name: "empty",
			in:   []byte{},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := looksGreek(tt.in); got != tt.want {
				t.Errorf("looksGreek = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDetectUTF16(t *testing.T) {
	t.Parallel()

	le := unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM)
	be := unicode.UTF16(unicode.BigEndian, unicode.IgnoreBOM)

	tests := []struct {
		name string
		in   []byte
		want string
		ok   bool
	}{
		{"greek utf-16le", mustEncode(t, le, srtPlain), nameUTF16LE, true},
		{"greek utf-16be", mustEncode(t, be, srtPlain), nameUTF16BE, true},
		{"ascii utf-16le", mustEncode(t, le, srtASCII), nameUTF16LE, true},
		{"windows-1253", mustEncode(t, charmap.Windows1253, srtPlain), "", false},
		{"utf-8", []byte(srtPlain), "", false},
		{"ascii", []byte(srtASCII), "", false},
		{"empty", nil, "", false},
		{"too short to judge", []byte{'a', 0, 'b', 0}, "", false},
		{
			// The zero bytes are on both parities, so this is not UTF-16
			// whatever the overall ratio says.
			name: "zero bytes on both parities",
			in:   []byte(strings.Repeat("a\x00\x00b", 40)),
		},
		{"utf-32le is not utf-16", mustEncode(t, utf32.UTF32(utf32.LittleEndian, utf32.IgnoreBOM), srtPlain), "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := detectUTF16(tt.in)
			if ok != tt.ok || got != tt.want {
				t.Errorf("detectUTF16 = (%q, %v), want (%q, %v)", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestDetectUTF32(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   []byte
		want string
		ok   bool
	}{
		{"utf-32le", mustEncode(t, utf32.UTF32(utf32.LittleEndian, utf32.IgnoreBOM), srtPlain), nameUTF32LE, true},
		{"utf-32be", mustEncode(t, utf32.UTF32(utf32.BigEndian, utf32.IgnoreBOM), srtPlain), nameUTF32BE, true},
		{"utf-16le", mustEncode(t, unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM), srtPlain), "", false},
		{"utf-8", []byte(srtPlain), "", false},
		{"empty", nil, "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := detectUTF32(tt.in)
			if ok != tt.ok || got != tt.want {
				t.Errorf("detectUTF32 = (%q, %v), want (%q, %v)", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestUTF8Stats(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		in            string
		wantValid     int
		wantMultibyte bool
	}{
		{"ascii", "abc", 3, false},
		{"greek", "αβγ", 6, true},
		{"empty", "", 0, false},
		{"one bad byte among greek", "αβ\x93γ", 6, true},
		{"all bad", "\x93\x94\x95", 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			valid, multibyte := utf8Stats([]byte(tt.in))
			if valid != tt.wantValid || multibyte != tt.wantMultibyte {
				t.Errorf("utf8Stats = (%d, %v), want (%d, %v)",
					valid, multibyte, tt.wantValid, tt.wantMultibyte)
			}
		})
	}
}

func TestSanitizeUTF8(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		want    string
		wantBad []int
	}{
		{name: "clean", in: "αβγ", want: "αβγ"},
		{name: "one bad byte", in: "α\x93β", want: "α�β", wantBad: []int{2}},
		{
			name: "existing replacement character is left alone",
			in:   "α�β",
			want: "α�β",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, bad := sanitizeUTF8([]byte(tt.in))
			if string(got) != tt.want {
				t.Errorf("sanitizeUTF8 = %q, want %q", got, tt.want)
			}
			if len(bad) != len(tt.wantBad) {
				t.Fatalf("bad offsets = %v, want %v", bad, tt.wantBad)
			}
			for i := range bad {
				if bad[i] != tt.wantBad[i] {
					t.Errorf("bad[%d] = %d, want %d", i, bad[i], tt.wantBad[i])
				}
			}
		})
	}
}

func TestIsASCII(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want bool
	}{
		{"", true},
		{srtASCII, true},
		{"\x00\x7f", true},
		{"\x80", false},
		{srtPlain, false},
	}
	for _, tt := range tests {
		if got := isASCII([]byte(tt.in)); got != tt.want {
			t.Errorf("isASCII(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}
