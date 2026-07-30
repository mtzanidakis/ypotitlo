package charset

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/encoding/unicode/utf32"
	"golang.org/x/text/transform"
)

// Fixtures are Greek subtitle text built programmatically in the test rather
// than committed as binaries. The byte values every assertion depends on were
// checked against the Python cp1253 and iso8859_7 codecs; the ones that carry
// the detection logic are asserted explicitly below so the fixtures cannot rot
// into something that passes for the wrong reason.

// srtElision is the misdetection case: ’ used for elision, which is 0xA2 in
// ISO-8859-7 and Ά in windows-1253. Counting bytes gets this backwards.
const srtElision = "1\n" +
	"00:00:01,000 --> 00:00:03,500\n" +
	"Σ’ αυτό το σπίτι μεγάλωσα.\n" +
	"Δεν το αφήνω σε κανέναν.\n" +
	"\n" +
	"2\n" +
	"00:00:04,000 --> 00:00:06,500\n" +
	"Απ’ την πίσω πόρτα θα φύγεις.\n" +
	"Μη μου το ξαναπείς αυτό.\n"

// srtDash has the em dash and ellipsis of professional typesetting, which live
// in 0x80-0x9F and are C1 controls in ISO-8859-7.
const srtDash = "1\n" +
	"00:00:01,000 --> 00:00:03,500\n" +
	"— Δεν ξέρω τι να πω…\n" +
	"— Πρέπει να φύγουμε τώρα.\n" +
	"\n" +
	"2\n" +
	"00:00:04,000 --> 00:00:06,500\n" +
	"— Πού πας;\n" +
	"— Στο λιμάνι, να προλάβω.\n"

// srtEuro contains €, which is 0xA4 in ISO-8859-7 and ¤ in windows-1253.
const srtEuro = "1\n" +
	"00:00:01,000 --> 00:00:03,500\n" +
	"Κοστίζει είκοσι ευρώ το κιλό.\n" +
	"Θα σου δώσω πενήντα €.\n" +
	"\n" +
	"2\n" +
	"00:00:04,000 --> 00:00:06,500\n" +
	"Δεν έχω τόσα πολλά επάνω μου.\n" +
	"Θα σε πληρώσω αύριο.\n"

// srtRegistered contains ®, which is 0xAE in windows-1253 and undefined in
// ISO-8859-7.
const srtRegistered = "1\n" +
	"00:00:01,000 --> 00:00:03,500\n" +
	"Το Acme® παρουσιάζει.\n" +
	"Μια ταινία για τη θάλασσα.\n" +
	"\n" +
	"2\n" +
	"00:00:04,000 --> 00:00:06,500\n" +
	"Γυρίστηκε στην Κρήτη.\n" +
	"Το καλοκαίρι του ογδόντα.\n"

// srtPlain encodes identically in both Greek encodings: no C1 bytes, no €, no
// ®, no apostrophe. It is a genuine tie.
const srtPlain = "1\n" +
	"00:00:01,000 --> 00:00:03,500\n" +
	"Καλημέρα σας, τι κάνετε;\n" +
	"Είμαι πολύ καλά, ευχαριστώ.\n" +
	"\n" +
	"2\n" +
	"00:00:04,000 --> 00:00:06,500\n" +
	"Πάμε μια βόλτα στην παραλία;\n" +
	"Με μεγάλη μου χαρά.\n"

// srtASCII is an ordinary English subtitle: no high bytes at all.
const srtASCII = "1\n" +
	"00:00:01,000 --> 00:00:03,500\n" +
	"I don't know what to say.\n" +
	"We have to leave now.\n"

func mustEncode(t *testing.T, enc encoding.Encoding, s string) []byte {
	t.Helper()
	b, _, err := transform.Bytes(enc.NewEncoder(), []byte(s))
	if err != nil {
		t.Fatalf("encoding fixture: %v", err)
	}
	return b
}

func containsWarning(warnings []string, substr string) bool {
	for _, w := range warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}

func TestDecodeFixtureBytesAreWhatWeThink(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		enc   encoding.Encoding
		text  string
		want  []byte
		avoid []byte
	}{
		{
			name: "elision apostrophe is 0xA2 in iso-8859-7",
			enc:  charmap.ISO8859_7,
			text: srtElision,
			want: []byte{0xA2},
			// Nothing in the ladder's earlier, higher-priority rules, or the
			// apostrophe rule would never be reached.
			avoid: []byte{0x80, 0x97, 0xA4, 0xA5, 0xAA, 0xAE, 0xB5, 0xB6},
		},
		{
			name: "em dash is 0x97 and ellipsis 0x85 in windows-1253",
			enc:  charmap.Windows1253,
			text: srtDash,
			want: []byte{0x97, 0x85},
		},
		{
			name:  "euro sign is 0xA4 in iso-8859-7",
			enc:   charmap.ISO8859_7,
			text:  srtEuro,
			want:  []byte{0xA4},
			avoid: []byte{0x97, 0x85, 0xAE},
		},
		{
			name:  "registered sign is 0xAE in windows-1253",
			enc:   charmap.Windows1253,
			text:  srtRegistered,
			want:  []byte{0xAE},
			avoid: []byte{0x97, 0x85, 0xA1, 0xA2, 0xA4, 0xA5, 0xAA, 0xB5, 0xB6},
		},
		{
			name:  "the tie fixture has no discriminating byte at all",
			enc:   charmap.Windows1253,
			text:  srtPlain,
			avoid: []byte{0x80, 0x85, 0x92, 0x97, 0xA1, 0xA2, 0xA4, 0xA5, 0xAA, 0xAE, 0xB5, 0xB6},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			b := mustEncode(t, tt.enc, tt.text)
			for _, c := range tt.want {
				if !bytes.Contains(b, []byte{c}) {
					t.Errorf("fixture does not contain %#02x", c)
				}
			}
			for _, c := range tt.avoid {
				if bytes.Contains(b, []byte{c}) {
					t.Errorf("fixture unexpectedly contains %#02x", c)
				}
			}
		})
	}

	// The tie fixture must encode to the same bytes in both Greek encodings,
	// which is what makes it undecidable.
	win := mustEncode(t, charmap.Windows1253, srtPlain)
	iso := mustEncode(t, charmap.ISO8859_7, srtPlain)
	if !bytes.Equal(win, iso) {
		t.Error("srtPlain differs between windows-1253 and iso-8859-7; it is not a tie")
	}
}

func TestDecodeDetectsGreek(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		enc     encoding.Encoding
		text    string
		want    string
		wantTie bool
	}{
		{
			name: "C1 bytes mean windows-1253",
			enc:  charmap.Windows1253,
			text: srtDash,
			want: nameWindows1253,
		},
		{
			name: "euro sign means iso-8859-7",
			enc:  charmap.ISO8859_7,
			text: srtEuro,
			want: nameISO88597,
		},
		{
			name: "registered sign means windows-1253",
			enc:  charmap.Windows1253,
			text: srtRegistered,
			want: nameWindows1253,
		},
		{
			name: "elision apostrophe means iso-8859-7, not windows-1253",
			enc:  charmap.ISO8859_7,
			text: srtElision,
			want: nameISO88597,
		},
		{
			name: "the same text in windows-1253 uses 0x92 and is detected as such",
			enc:  charmap.Windows1253,
			text: srtElision,
			want: nameWindows1253,
		},
		{
			name:    "no evidence either way falls to windows-1253 with a warning",
			enc:     charmap.Windows1253,
			text:    srtPlain,
			want:    nameWindows1253,
			wantTie: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			in := mustEncode(t, tt.enc, tt.text)
			res, err := Decode(in, "")
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if res.Encoding != tt.want {
				t.Errorf("Encoding = %q, want %q (warnings: %v)", res.Encoding, tt.want, res.Warnings)
			}
			if got := string(res.Text); got != tt.text {
				t.Errorf("Text = %q, want %q", got, tt.text)
			}
			if res.HadBOM != nil {
				t.Errorf("HadBOM = % X, want nil", res.HadBOM)
			}
			gotTie := containsWarning(res.Warnings, "cannot tell")
			if gotTie != tt.wantTie {
				t.Errorf("tie warning = %v, want %v (warnings: %v)", gotTie, tt.wantTie, res.Warnings)
			}
		})
	}
}

func TestDecodeUndefinedByteWarns(t *testing.T) {
	t.Parallel()

	// 0x81 is undefined in windows-1253. charmap decoders never report an
	// error for it: it silently becomes U+FFFD, so the only way to notice is
	// to look for the replacement character afterwards.
	in := mustEncode(t, charmap.Windows1253, srtDash)
	offset := len(in) / 2
	damaged := make([]byte, 0, len(in)+1)
	damaged = append(damaged, in[:offset]...)
	damaged = append(damaged, 0x81)
	damaged = append(damaged, in[offset:]...)

	res, err := Decode(damaged, "")
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if res.Encoding != nameWindows1253 {
		t.Fatalf("Encoding = %q, want %q", res.Encoding, nameWindows1253)
	}
	if !bytes.Contains(res.Text, utf8Replacement) {
		t.Error("decoded text has no U+FFFD")
	}
	if !containsWarning(res.Warnings, "no character") {
		t.Errorf("warnings = %v, want one about an undecodable byte", res.Warnings)
	}
	if !containsWarning(res.Warnings, "0x81") {
		t.Errorf("warnings = %v, want the offending byte value", res.Warnings)
	}
}

func TestDecodeUTF16WithoutBOM(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		enc  encoding.Encoding
		want string
	}{
		{"little endian", unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM), nameUTF16LE},
		{"big endian", unicode.UTF16(unicode.BigEndian, unicode.IgnoreBOM), nameUTF16BE},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Two lines per cue: a one-line-per-cue sample has enough ASCII
			// padding to pass a naive ratio test by luck.
			in := mustEncode(t, tt.enc, srtPlain)

			// The whole point of parity: this file is well under any sensible
			// "mostly zero bytes" threshold, because Greek is U+03xx, so the
			// high byte is 0x03 and not 0x00.
			s := sample(in)
			if pct := bytes.Count(s, []byte{0}) * 100 / len(s); pct >= 30 {
				t.Fatalf("fixture is %d%% zero bytes; it no longer exercises the sub-threshold case", pct)
			}

			res, err := Decode(in, "")
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if res.Encoding != tt.want {
				t.Errorf("Encoding = %q, want %q", res.Encoding, tt.want)
			}
			if got := string(res.Text); got != srtPlain {
				t.Errorf("Text = %q, want %q", got, srtPlain)
			}
			if len(res.Warnings) != 0 {
				t.Errorf("Warnings = %v, want none", res.Warnings)
			}
		})
	}
}

func TestDecodeUTF32WithoutBOM(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		enc  encoding.Encoding
		want string
	}{
		{"little endian", utf32.UTF32(utf32.LittleEndian, utf32.IgnoreBOM), nameUTF32LE},
		{"big endian", utf32.UTF32(utf32.BigEndian, utf32.IgnoreBOM), nameUTF32BE},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			in := mustEncode(t, tt.enc, srtPlain)
			res, err := Decode(in, "")
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if res.Encoding != tt.want {
				t.Fatalf("Encoding = %q, want %q", res.Encoding, tt.want)
			}
			if got := string(res.Text); got != srtPlain {
				t.Errorf("Text = %q, want %q", got, srtPlain)
			}
		})
	}
}

func TestDecodeUTF8(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		in          []byte
		wantText    string
		wantWarning string
	}{
		{
			name:     "pure ascii",
			in:       []byte(srtASCII),
			wantText: srtASCII,
		},
		{
			name:     "greek utf-8",
			in:       []byte(srtElision),
			wantText: srtElision,
		},
		{
			name:     "empty",
			in:       nil,
			wantText: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			res, err := Decode(tt.in, "")
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if res.Encoding != nameUTF8 {
				t.Errorf("Encoding = %q, want %q", res.Encoding, nameUTF8)
			}
			if got := string(res.Text); got != tt.wantText {
				t.Errorf("Text = %q, want %q", got, tt.wantText)
			}
			if len(res.Warnings) != 0 {
				t.Errorf("Warnings = %v, want none", res.Warnings)
			}
		})
	}
}

func TestDecodeUTF8WithOneInvalidByte(t *testing.T) {
	t.Parallel()

	// A single stray byte must not throw the whole file into the legacy path:
	// that is the difference between one U+FFFD and a mojibake file.
	in := []byte(srtElision)
	offset := strings.Index(srtElision, "Δεν")
	damaged := make([]byte, 0, len(in)+1)
	damaged = append(damaged, in[:offset]...)
	damaged = append(damaged, 0x93) // a bare continuation byte
	damaged = append(damaged, in[offset:]...)

	res, err := Decode(damaged, "")
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if res.Encoding != nameUTF8 {
		t.Fatalf("Encoding = %q, want %q (warnings: %v)", res.Encoding, nameUTF8, res.Warnings)
	}
	want := srtElision[:offset] + string(utf8Replacement) + srtElision[offset:]
	if got := string(res.Text); got != want {
		t.Errorf("Text = %q, want %q", got, want)
	}
	if !containsWarning(res.Warnings, "invalid byte") {
		t.Errorf("Warnings = %v, want one about an invalid byte", res.Warnings)
	}
	if !containsWarning(res.Warnings, "1 invalid byte") {
		t.Errorf("Warnings = %v, want the count", res.Warnings)
	}
}

func TestDecodeFallsBackToWindows1252(t *testing.T) {
	t.Parallel()

	// French: high bytes, but nowhere near enough of them in the Greek range
	// for the Greek ladder to be entitled to an opinion.
	in := mustEncode(t, charmap.Windows1252, "1\n"+
		"00:00:01,000 --> 00:00:03,500\n"+
		"Je ne sais pas quoi dire, chérie.\n"+
		"Il faut partir tout de suite.\n")

	res, err := Decode(in, "")
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if res.Encoding != nameWindows1252 {
		t.Errorf("Encoding = %q, want %q", res.Encoding, nameWindows1252)
	}
	if !containsWarning(res.Warnings, "could not detect") {
		t.Errorf("Warnings = %v, want one naming the guess", res.Warnings)
	}
	if !containsWarning(res.Warnings, "-charset") {
		t.Errorf("Warnings = %v, want a hint about -charset", res.Warnings)
	}
}

func TestDecodeBOM(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		enc  encoding.Encoding
		bom  []byte
		want string
	}{
		{"utf-8", encoding.Nop, []byte{0xEF, 0xBB, 0xBF}, nameUTF8},
		{"utf-16le", unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM), []byte{0xFF, 0xFE}, nameUTF16LE},
		{"utf-16be", unicode.UTF16(unicode.BigEndian, unicode.IgnoreBOM), []byte{0xFE, 0xFF}, nameUTF16BE},
		{"utf-32le", utf32.UTF32(utf32.LittleEndian, utf32.IgnoreBOM), []byte{0xFF, 0xFE, 0x00, 0x00}, nameUTF32LE},
		{"utf-32be", utf32.UTF32(utf32.BigEndian, utf32.IgnoreBOM), []byte{0x00, 0x00, 0xFE, 0xFF}, nameUTF32BE},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			body := mustEncode(t, tt.enc, srtPlain)
			in := append(append([]byte(nil), tt.bom...), body...)

			res, err := Decode(in, "")
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if res.Encoding != tt.want {
				t.Errorf("Encoding = %q, want %q", res.Encoding, tt.want)
			}
			if !bytes.Equal(res.HadBOM, tt.bom) {
				t.Errorf("HadBOM = % X, want % X", res.HadBOM, tt.bom)
			}
			if got := string(res.Text); got != srtPlain {
				t.Errorf("Text = %q, want %q", got, srtPlain)
			}
			if bytes.HasPrefix(res.Text, tt.bom) {
				t.Error("BOM survived into the decoded text")
			}
		})
	}
}

func TestDecodeStripsBOMEvenWithOverride(t *testing.T) {
	t.Parallel()

	// The P0 this ordering exists for: with the override short-circuiting the
	// BOM, the first line comes out as three mojibake characters glued to the
	// index and stops looking like an index at all.
	bom := []byte{0xEF, 0xBB, 0xBF}
	in := append(append([]byte(nil), bom...), []byte(srtElision)...)

	tests := []struct {
		name        string
		override    string
		wantConflct bool
	}{
		{"conflicting override", "cp1253", true},
		{"agreeing override", "utf-8", false},
		{"agreeing override by alias", "UTF8", false},
		{"no override", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			res, err := Decode(in, tt.override)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if res.Encoding != nameUTF8 {
				t.Errorf("Encoding = %q, want %q", res.Encoding, nameUTF8)
			}
			if !bytes.Equal(res.HadBOM, bom) {
				t.Errorf("HadBOM = % X, want % X", res.HadBOM, bom)
			}
			if got := string(res.Text); got != srtElision {
				t.Errorf("Text = %q, want %q", got, srtElision)
			}
			if !strings.HasPrefix(string(res.Text), "1\n") {
				t.Errorf("first line is %q; the index no longer matches ^\\d+$", strings.SplitN(string(res.Text), "\n", 2)[0])
			}
			gotConflict := containsWarning(res.Warnings, "the BOM wins")
			if gotConflict != tt.wantConflct {
				t.Errorf("conflict warning = %v, want %v (warnings: %v)", gotConflict, tt.wantConflct, res.Warnings)
			}
		})
	}
}

func TestDecodeOverride(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		enc      encoding.Encoding
		override string
		want     string
	}{
		{"cp1253 alias", charmap.Windows1253, "cp1253", nameWindows1253},
		{"windows-1253", charmap.Windows1253, "windows-1253", nameWindows1253},
		{"iso-8859-7", charmap.ISO8859_7, "iso-8859-7", nameISO88597},
		{"iso8859-7 without the dash", charmap.ISO8859_7, "iso8859-7", nameISO88597},
		{"greek", charmap.ISO8859_7, "greek", nameISO88597},
		{"uppercase and padded", charmap.Windows1253, "  CP1253  ", nameWindows1253},
		{"cp737", cp737, "cp737", nameCP737},
		{"ibm737", cp737, "ibm737", nameCP737},
		{"cp869", cp869, "cp869", nameCP869},
		{"ibm869", cp869, "IBM869", nameCP869},
		{"macgreek", macGreek, "macgreek", nameMacGreek},
		{"x-mac-greek", macGreek, "x-mac-greek", nameMacGreek},
		{"utf-16le without a BOM", unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM), "utf-16le", nameUTF16LE},
		{"utf-32le without a BOM", utf32.UTF32(utf32.LittleEndian, utf32.IgnoreBOM), "utf-32le", nameUTF32LE},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			in := mustEncode(t, tt.enc, srtPlain)
			res, err := Decode(in, tt.override)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if res.Encoding != tt.want {
				t.Errorf("Encoding = %q, want %q", res.Encoding, tt.want)
			}
			if got := string(res.Text); got != srtPlain {
				t.Errorf("Text = %q, want %q", got, srtPlain)
			}
			if len(res.Warnings) != 0 {
				t.Errorf("Warnings = %v, want none", res.Warnings)
			}
		})
	}
}

func TestDecodeUnknownOverride(t *testing.T) {
	t.Parallel()

	tests := []string{"klingon", "cp1253x", "utf-9", " "}

	for _, name := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := Decode([]byte(srtASCII), name)
			if err == nil {
				t.Fatalf("Decode(%q) succeeded, want an error", name)
			}
			var unknown *UnknownCharsetError
			if !errors.As(err, &unknown) {
				t.Fatalf("error is %T, want *UnknownCharsetError", err)
			}
			for _, want := range SupportedNames() {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error does not list %q: %v", want, err)
				}
			}
		})
	}
}

// TestDecodeUnknownOverrideBeatsBOM documents that a typo is reported even when
// a BOM would have made the override irrelevant.
func TestDecodeUnknownOverrideBeatsBOM(t *testing.T) {
	t.Parallel()

	in := append([]byte{0xEF, 0xBB, 0xBF}, srtASCII...)
	if _, err := Decode(in, "cp1253x"); err == nil {
		t.Fatal("Decode succeeded with an unknown charset name")
	}
}

// TestDecodeTruncatedUTF16 covers the other kind of undecodable input: the
// multi-byte decoders do report an error, and there is no byte offset to give
// because one rune is not one byte, so the warning falls back to a count.
func TestDecodeTruncatedUTF16(t *testing.T) {
	t.Parallel()

	in := mustEncode(t, unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM), srtPlain)

	res, err := Decode(in[:len(in)-1], "")
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if res.Encoding != nameUTF16LE {
		t.Errorf("Encoding = %q, want %q", res.Encoding, nameUTF16LE)
	}
	if !containsWarning(res.Warnings, "could not be decoded") {
		t.Errorf("Warnings = %v, want one about an undecodable character", res.Warnings)
	}
	// Everything up to the damage must survive: a truncated tail is not a
	// reason to lose the file.
	if !strings.HasPrefix(string(res.Text), "1\n00:00:01,000") {
		t.Errorf("Text starts %q, want the file up to the damage", string(res.Text)[:20])
	}
}

// TestDecodeIANAOnlyName covers a name that only the IANA index knows, where
// the canonical name has to come from there rather than from the WHATWG one.
func TestDecodeIANAOnlyName(t *testing.T) {
	t.Parallel()

	res, err := Decode([]byte("plain ascii text"), "437")
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if res.Encoding != "ibm437" {
		t.Errorf("Encoding = %q, want ibm437", res.Encoding)
	}
}

func TestBOMFor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		want []byte
	}{
		{nameUTF8, []byte{0xEF, 0xBB, 0xBF}},
		{nameUTF16LE, []byte{0xFF, 0xFE}},
		{nameUTF16BE, []byte{0xFE, 0xFF}},
		{nameUTF32LE, []byte{0xFF, 0xFE, 0x00, 0x00}},
		{nameUTF32BE, []byte{0x00, 0x00, 0xFE, 0xFF}},
		{nameWindows1253, nil},
		{"nonsense", nil},
	}

	for _, tt := range tests {
		if got := BOMFor(tt.name); !bytes.Equal(got, tt.want) {
			t.Errorf("BOMFor(%q) = % X, want % X", tt.name, got, tt.want)
		}
	}
}

func TestSupportedNamesAllResolve(t *testing.T) {
	t.Parallel()

	names := SupportedNames()
	if len(names) == 0 {
		t.Fatal("SupportedNames is empty")
	}
	for i, name := range names {
		if i > 0 && names[i-1] >= name {
			t.Errorf("SupportedNames is not sorted: %q before %q", names[i-1], name)
		}
		enc, canonical, err := resolve(name)
		if err != nil {
			t.Errorf("resolve(%q): %v", name, err)
			continue
		}
		if enc == nil {
			t.Errorf("resolve(%q) returned a nil encoding", name)
		}
		if canonical != name {
			t.Errorf("resolve(%q) canonical name = %q; the list should hold canonical names", name, canonical)
		}
	}
}

func TestResolveRejectsRegisteredButUnimplemented(t *testing.T) {
	t.Parallel()

	// ianaindex answers (nil, nil) for names it knows but cannot supply, which
	// is why resolve checks the encoding and not only the error. cp869 is one
	// of those; it has to come from the hand-written table instead.
	enc, name, err := resolve("cp869")
	if err != nil {
		t.Fatalf("resolve(cp869): %v", err)
	}
	if enc == nil {
		t.Fatal("resolve(cp869) returned a nil encoding")
	}
	if name != nameCP869 {
		t.Errorf("name = %q, want %q", name, nameCP869)
	}
}
