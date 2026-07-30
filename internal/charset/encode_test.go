package charset

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/transform"
)

func TestEncodeDefaultsToUTF8WithoutBOM(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		encoding string
	}{
		{"empty name", ""},
		{"utf-8", "utf-8"},
		{"UTF8", "UTF8"},
		{"padded", "  utf-8  "},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			out, err := Encode([]byte(srtElision), tt.encoding, false)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			if string(out) != srtElision {
				t.Errorf("Encode = %q, want %q", out, srtElision)
			}
			if bytes.HasPrefix(out, []byte{0xEF, 0xBB, 0xBF}) {
				t.Error("output has a BOM but none was asked for")
			}
		})
	}
}

func TestEncodeBOM(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		encoding string
		bom      bool
		wantBOM  []byte
	}{
		{"utf-8", "utf-8", true, []byte{0xEF, 0xBB, 0xBF}},
		// The name carries the intent on its own, for callers who pass the
		// user's -output-charset value straight through.
		{"utf-8-bom name", "utf-8-bom", false, []byte{0xEF, 0xBB, 0xBF}},
		{"utf-16le", "utf-16le", true, []byte{0xFF, 0xFE}},
		{"utf-16be", "utf-16be", true, []byte{0xFE, 0xFF}},
		{"utf-32le", "utf-32le", true, []byte{0xFF, 0xFE, 0x00, 0x00}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			out, err := Encode([]byte(srtPlain), tt.encoding, tt.bom)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			if !bytes.HasPrefix(out, tt.wantBOM) {
				t.Fatalf("output starts % X, want a % X BOM", out[:min(len(out), 4)], tt.wantBOM)
			}

			// Round-trip: what came out must decode back to what went in, BOM
			// and all.
			res, err := Decode(out, "")
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if got := string(res.Text); got != srtPlain {
				t.Errorf("round trip = %q, want %q", got, srtPlain)
			}
			if !bytes.Equal(res.HadBOM, tt.wantBOM) {
				t.Errorf("HadBOM = % X, want % X", res.HadBOM, tt.wantBOM)
			}
		})
	}
}

func TestEncodeRejectsBOMForNonUnicode(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"windows-1253", "iso-8859-7", "cp737"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if _, err := Encode([]byte(srtPlain), name, true); err == nil {
				t.Fatalf("Encode(%q, bom=true) succeeded, want an error", name)
			}
		})
	}
}

func TestEncodeToLegacyRoundTrips(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		enc  encoding.Encoding
	}{
		{"windows-1253", charmap.Windows1253},
		{"iso-8859-7", charmap.ISO8859_7},
		{"cp737", cp737},
		{"cp869", cp869},
		{"macgreek", macGreek},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			out, err := Encode([]byte(srtPlain), tt.name, false)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			want := mustEncode(t, tt.enc, srtPlain)
			if !bytes.Equal(out, want) {
				t.Fatalf("Encode = % X, want % X", out, want)
			}

			res, err := Decode(out, tt.name)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if got := string(res.Text); got != srtPlain {
				t.Errorf("round trip = %q, want %q", got, srtPlain)
			}
		})
	}
}

func TestEncodeReportsUnsupportedRunes(t *testing.T) {
	t.Parallel()

	// charmap.ISO8859_7's encoder returns an error and an empty buffer for the
	// em dash, the ellipsis and the curly quotes an LLM emits constantly. The
	// pre-scan turns that into a message naming every one of them.
	text := "— Δεν ξέρω τι να πω…\n“Ποτέ”, είπε.\n"

	_, err := Encode([]byte(text), "iso-8859-7", false)
	if err == nil {
		t.Fatal("Encode succeeded, want an error")
	}

	var unsupported *UnsupportedRunesError
	if !errors.As(err, &unsupported) {
		t.Fatalf("error is %T, want *UnsupportedRunesError", err)
	}
	if unsupported.Encoding != "iso-8859-7" {
		t.Errorf("Encoding = %q, want iso-8859-7", unsupported.Encoding)
	}

	want := []rune{'—', '…', '“', '”'}
	if len(unsupported.Runes) != len(want) {
		t.Fatalf("Runes = %q, want %q", unsupported.Runes, want)
	}
	for i, r := range want {
		if unsupported.Runes[i] != r {
			t.Errorf("Runes[%d] = %q, want %q", i, unsupported.Runes[i], r)
		}
	}
	if len(unsupported.Offsets) != len(unsupported.Runes) {
		t.Fatalf("Offsets = %v, want one per rune", unsupported.Offsets)
	}
	if unsupported.Offsets[0] != 0 {
		t.Errorf("Offsets[0] = %d, want 0", unsupported.Offsets[0])
	}
	for _, want := range []string{"U+2014", "U+2026", "iso-8859-7"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}

	// The same text is fine in windows-1253, which is the whole reason the
	// ladder bothers to tell the two apart.
	if _, err := Encode([]byte(text), "windows-1253", false); err != nil {
		t.Errorf("Encode to windows-1253: %v", err)
	}
}

func TestEncodeReplaceUnsupportedIsOptIn(t *testing.T) {
	t.Parallel()

	text := "Δεν ξέρω…\n"

	if _, err := EncodeWith([]byte(text), "iso-8859-7", EncodeOptions{}); err == nil {
		t.Fatal("EncodeWith without ReplaceUnsupported succeeded, want an error")
	}

	res, err := EncodeWith([]byte(text), "iso-8859-7", EncodeOptions{ReplaceUnsupported: true})
	if err != nil {
		t.Fatalf("EncodeWith: %v", err)
	}
	if want := []rune{'…'}; len(res.Replaced) != 1 || res.Replaced[0] != want[0] {
		t.Errorf("Replaced = %q, want %q", res.Replaced, want)
	}
	if len(res.Warnings) == 0 {
		t.Error("replacing characters produced no warning")
	}
	if !bytes.Contains(res.Bytes, []byte{encoding.ASCIISub}) {
		t.Errorf("output % X has no substitute character", res.Bytes)
	}

	// Everything else must survive intact.
	back, _, err := transform.Bytes(charmap.ISO8859_7.NewDecoder(), res.Bytes)
	if err != nil {
		t.Fatalf("decoding back: %v", err)
	}
	if want := "Δεν ξέρω" + string(rune(encoding.ASCIISub)) + "\n"; string(back) != want {
		t.Errorf("round trip = %q, want %q", back, want)
	}
}

func TestEncodeUnknownName(t *testing.T) {
	t.Parallel()

	_, err := Encode([]byte(srtPlain), "klingon", false)
	if err == nil {
		t.Fatal("Encode succeeded, want an error")
	}
	var unknown *UnknownCharsetError
	if !errors.As(err, &unknown) {
		t.Fatalf("error is %T, want *UnknownCharsetError", err)
	}
}

func TestEncodeEmpty(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"", "utf-8", "windows-1253"} {
		t.Run("name="+name, func(t *testing.T) {
			t.Parallel()

			out, err := Encode(nil, name, false)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			if len(out) != 0 {
				t.Errorf("Encode = % X, want empty", out)
			}
		})
	}
}

func TestPlural(t *testing.T) {
	t.Parallel()

	tests := []struct {
		n    int
		want string
	}{
		{0, "0 bytes"},
		{1, "1 byte"},
		{2, "2 bytes"},
	}
	for _, tt := range tests {
		if got := plural(tt.n, "byte", "bytes"); got != tt.want {
			t.Errorf("plural(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}

func TestFormatOffsetsCaps(t *testing.T) {
	t.Parallel()

	offsets := make([]int, 25)
	for i := range offsets {
		offsets[i] = i
	}
	got := formatOffsets(offsets)
	if !strings.HasSuffix(got, "and 15 more") {
		t.Errorf("formatOffsets = %q, want it capped", got)
	}

	b := make([]byte, 25)
	got = formatByteOffsets(b, offsets)
	if !strings.HasSuffix(got, "and 15 more") {
		t.Errorf("formatByteOffsets = %q, want it capped", got)
	}
	if !strings.HasPrefix(got, "offset 0: 0x00") {
		t.Errorf("formatByteOffsets = %q, want it to name the byte value", got)
	}
}
