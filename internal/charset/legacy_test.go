package charset

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/transform"
)

// The byte sequences below come from the Python cp737, cp869 and mac_greek
// codecs. x/text has no charmap for any of the three, so these tables are the
// only reference the package has and they are pinned against a second
// implementation rather than against themselves.
func TestLegacyTablesMatchReference(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		enc  *singleByte
		text string
		want []byte
	}{
		{
			name: "cp737 greeting",
			enc:  cp737,
			text: "Καλημέρα σας",
			want: []byte{0x89, 0x98, 0xA2, 0x9E, 0xA3, 0xE2, 0xA8, 0x98, 0x20, 0xA9, 0x98, 0xAA},
		},
		{
			name: "cp737 accented capitals",
			enc:  cp737,
			text: "Άλφα Βήτα Γάμμα",
			want: []byte{0xEA, 0xA2, 0xAD, 0x98, 0x20, 0x81, 0xE3, 0xAB, 0x98, 0x20, 0x82, 0xE1, 0xA3, 0xA3, 0x98},
		},
		{
			name: "cp869 greeting",
			enc:  cp869,
			text: "Καλημέρα σας",
			want: []byte{0xB5, 0xD6, 0xE5, 0xE1, 0xE6, 0x9D, 0xEB, 0xD6, 0x20, 0xEC, 0xD6, 0xED},
		},
		{
			name: "cp869 accented capitals",
			enc:  cp869,
			text: "Άλφα Βήτα Γάμμα",
			want: []byte{0x86, 0xE5, 0xF3, 0xD6, 0x20, 0xA5, 0x9E, 0xEE, 0xD6, 0x20, 0xA6, 0x9B, 0xE6, 0xE6, 0xD6},
		},
		{
			name: "macgreek greeting",
			enc:  macGreek,
			text: "Καλημέρα σας",
			want: []byte{0xBA, 0xE1, 0xEC, 0xE8, 0xED, 0xDB, 0xF2, 0xE1, 0x20, 0xF3, 0xE1, 0xF7},
		},
		{
			name: "macgreek sentence with omega",
			enc:  macGreek,
			text: "Ώρα να φύγουμε τώρα!",
			want: []byte{
				0xDF, 0xF2, 0xE1, 0x20, 0xEE, 0xE1, 0x20, 0xE6, 0xE0, 0xE7, 0xEF, 0xF9,
				0xED, 0xE5, 0x20, 0xF4, 0xF1, 0xF2, 0xE1, 0x21,
			},
		},
		{
			name: "cp737 sentence with omega",
			enc:  cp737,
			text: "Ώρα να φύγουμε τώρα!",
			want: []byte{
				0xF0, 0xA8, 0x98, 0x20, 0xA4, 0x98, 0x20, 0xAD, 0xE7, 0x9A, 0xA6, 0xAC,
				0xA3, 0x9C, 0x20, 0xAB, 0xE9, 0xA8, 0x98, 0x21,
			},
		},
		{
			name: "cp869 sentence with omega",
			enc:  cp869,
			text: "Ώρα να φύγουμε τώρα!",
			want: []byte{
				0x98, 0xEB, 0xD6, 0x20, 0xE7, 0xD6, 0x20, 0xF3, 0xA3, 0xD8, 0xE9, 0xF2,
				0xE6, 0xDE, 0x20, 0xEE, 0xFD, 0xEB, 0xD6, 0x21,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, _, err := transform.Bytes(tt.enc.NewEncoder(), []byte(tt.text))
			if err != nil {
				t.Fatalf("encoding: %v", err)
			}
			if !bytes.Equal(got, tt.want) {
				t.Errorf("encode = % X, want % X", got, tt.want)
			}

			back, _, err := transform.Bytes(tt.enc.NewDecoder(), tt.want)
			if err != nil {
				t.Fatalf("decoding: %v", err)
			}
			if string(back) != tt.text {
				t.Errorf("decode = %q, want %q", back, tt.text)
			}
		})
	}
}

func TestLegacyUndefinedBytes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		enc  *singleByte
		want []byte
	}{
		{"cp737 has no gaps", cp737, nil},
		{"cp869 gaps", cp869, []byte{0x80, 0x81, 0x82, 0x83, 0x84, 0x85, 0x87, 0x93, 0x94}},
		{"macgreek has no gaps", macGreek, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var got []byte
			for i := 0x80; i < 0x100; i++ {
				out, _, err := transform.Bytes(tt.enc.NewDecoder(), []byte{byte(i)})
				if err != nil {
					t.Fatalf("decoding %#02x: %v", i, err)
				}
				if bytes.Equal(out, utf8Replacement) {
					got = append(got, byte(i))
				}
			}
			if !bytes.Equal(got, tt.want) {
				t.Errorf("undefined bytes = % X, want % X", got, tt.want)
			}
		})
	}
}

// TestLegacyDecodeReportsUndefinedByte checks the end-to-end path: a legacy
// decoder never errors on an undefined byte, so the warning has to come from
// counting U+FFFD after the fact.
func TestLegacyDecodeReportsUndefinedByte(t *testing.T) {
	t.Parallel()

	in, _, err := transform.Bytes(cp869.NewEncoder(), []byte("Καλημέρα σας"))
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	damaged := append([]byte{0x80}, in...) // 0x80 is a gap in cp869

	res, decErr := Decode(damaged, "cp869")
	if decErr != nil {
		t.Fatalf("Decode: %v", decErr)
	}
	if res.Encoding != nameCP869 {
		t.Errorf("Encoding = %q, want %q", res.Encoding, nameCP869)
	}
	if !bytes.HasPrefix(res.Text, utf8Replacement) {
		t.Errorf("Text = %q, want it to start with U+FFFD", res.Text)
	}
	if !containsWarning(res.Warnings, "offset 0: 0x80") {
		t.Errorf("Warnings = %v, want the offset and byte value", res.Warnings)
	}
}

func TestLegacyEncoderRejectsUnsupportedRune(t *testing.T) {
	t.Parallel()

	// cp737 is a DOS code page: it has box drawing but no em dash.
	_, _, err := transform.Bytes(cp737.NewEncoder(), []byte("—"))
	if err == nil {
		t.Fatal("encoding an em dash to cp737 succeeded, want an error")
	}
	rep, ok := err.(interface{ Replacement() byte })
	if !ok {
		t.Fatalf("error %v (%T) does not carry a replacement byte, so "+
			"encoding.ReplaceUnsupported cannot handle it", err, err)
	}
	if got := rep.Replacement(); got != encoding.ASCIISub {
		t.Errorf("Replacement = %#02x, want %#02x", got, encoding.ASCIISub)
	}
	if err.Error() == "" {
		t.Error("the error has no message")
	}

	// And the structural detection really does work end to end: x/text's
	// ReplaceUnsupported only looks for the Replacement method, so an encoding
	// defined outside x/text can take part.
	res, encErr := EncodeWith([]byte("Δεν ξέρω —"), "cp737", EncodeOptions{ReplaceUnsupported: true})
	if encErr != nil {
		t.Fatalf("EncodeWith: %v", encErr)
	}
	if len(res.Replaced) != 1 || res.Replaced[0] != '—' {
		t.Errorf("Replaced = %q, want [—]", res.Replaced)
	}
	if !bytes.Contains(res.Bytes, []byte{encoding.ASCIISub}) {
		t.Errorf("output % X has no substitute character", res.Bytes)
	}
}

func TestLegacyEncoderRejectsInvalidUTF8(t *testing.T) {
	t.Parallel()

	_, _, err := transform.Bytes(cp737.NewEncoder(), []byte{0xE1, 0x93})
	if err == nil {
		t.Fatal("encoding invalid UTF-8 succeeded, want an error")
	}
}

// TestLegacyTransformerStreams runs the transformers through a reader with a
// deliberately tiny buffer, which is the only way the ErrShortDst and
// ErrShortSrc paths are exercised.
func TestLegacyTransformerStreams(t *testing.T) {
	t.Parallel()

	text := strings.Repeat("Καλημέρα σας, τι κάνετε; ", 40)

	for _, enc := range []*singleByte{cp737, cp869, macGreek} {
		t.Run(enc.String(), func(t *testing.T) {
			t.Parallel()

			encoded, err := io.ReadAll(transform.NewReader(
				strings.NewReader(text), enc.NewEncoder()))
			if err != nil {
				t.Fatalf("streaming encode: %v", err)
			}

			decoded, err := io.ReadAll(transform.NewReader(
				bytes.NewReader(encoded), enc.NewDecoder()))
			if err != nil {
				t.Fatalf("streaming decode: %v", err)
			}
			if string(decoded) != text {
				t.Errorf("round trip differs from the original")
			}
		})
	}
}

func TestLegacyDecoderPassesASCIIThrough(t *testing.T) {
	t.Parallel()

	for _, enc := range []*singleByte{cp737, cp869, macGreek} {
		t.Run(enc.String(), func(t *testing.T) {
			t.Parallel()

			var ascii []byte
			for i := 0; i < 0x80; i++ {
				ascii = append(ascii, byte(i))
			}
			out, _, err := transform.Bytes(enc.NewDecoder(), ascii)
			if err != nil {
				t.Fatalf("decoding: %v", err)
			}
			if string(out) != string(ascii) {
				t.Errorf("ASCII did not survive %s", enc)
			}
		})
	}
}

// TestLegacyTablesAreInvertible guards the generated tables against a
// copy-and-paste slip: every defined byte must decode to a rune that encodes
// back to the same byte.
func TestLegacyTablesAreInvertible(t *testing.T) {
	t.Parallel()

	for _, enc := range []*singleByte{cp737, cp869, macGreek} {
		t.Run(enc.String(), func(t *testing.T) {
			t.Parallel()

			for i, r := range enc.dec {
				if r == utf8.RuneError {
					continue
				}
				b := byte(0x80 + i)
				if got, ok := enc.enc[r]; !ok || got != b {
					t.Errorf("%#02x decodes to %q which encodes back to %#02x", b, r, got)
				}
			}
		})
	}
}
