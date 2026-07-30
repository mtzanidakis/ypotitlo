package charset

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/transform"
)

// EncodeOptions controls Encode's behaviour beyond the choice of encoding.
type EncodeOptions struct {
	// BOM prepends a byte order mark. Only Unicode encodings can carry one.
	BOM bool

	// ReplaceUnsupported swaps every rune the target encoding cannot represent
	// for its substitute character instead of failing. Off by default: silently
	// turning an em dash into SUB is data loss, and the caller has to ask for
	// it.
	ReplaceUnsupported bool
}

// EncodeResult is the outcome of encoding UTF-8 text.
type EncodeResult struct {
	// Bytes is the encoded text, including any BOM.
	Bytes []byte

	// Replaced lists the distinct runes that were substituted, in order of
	// first appearance. Non-empty only with EncodeOptions.ReplaceUnsupported.
	Replaced []rune

	// Warnings lists anything the caller should be told.
	Warnings []string
}

// UnsupportedRunesError reports runes that the target encoding has no character
// for.
//
// This is why the default output encoding is UTF-8: charmap.ISO8859_7's encoder
// returns an error and an empty buffer for —, … and curly quotes, all of which
// an LLM emits constantly, so encoding a translated subtitle to ISO-8859-7
// without checking first produces an empty file.
type UnsupportedRunesError struct {
	Encoding string
	Runes    []rune
	Offsets  []int
}

func (e *UnsupportedRunesError) Error() string {
	const maxListed = 8

	parts := make([]string, 0, maxListed)
	for i, r := range e.Runes {
		if i == maxListed {
			parts = append(parts, fmt.Sprintf("and %d more", len(e.Runes)-maxListed))
			break
		}
		parts = append(parts, fmt.Sprintf("%q (U+%04X) at byte %d", r, r, e.Offsets[i]))
	}
	return fmt.Sprintf("cannot encode %s as %s: %s",
		plural(len(e.Runes), "character", "distinct characters"),
		e.Encoding, strings.Join(parts, ", "))
}

// Encode converts UTF-8 text to the named encoding.
//
// An empty name means UTF-8. The default for this tool is always UTF-8 without
// a BOM, whatever the input was; anything else is a deliberate concession to a
// legacy player.
//
// For a non-UTF-8 target the text is scanned first and every rune the encoding
// cannot represent is reported at once, as an *UnsupportedRunesError, rather
// than the caller discovering a truncated or empty buffer.
func Encode(text []byte, name string, bom bool) ([]byte, error) {
	res, err := EncodeWith(text, name, EncodeOptions{BOM: bom})
	return res.Bytes, err
}

// EncodeWith is Encode with the extra knobs of EncodeOptions.
func EncodeWith(text []byte, name string, opts EncodeOptions) (EncodeResult, error) {
	canonical := nameUTF8
	var enc encoding.Encoding

	if trimmed := strings.TrimSpace(name); trimmed != "" {
		// "utf-8-bom" is not a charset name but it is what a user who wants a
		// BOM will type, so accept it rather than sending them to a flag.
		if normalize(trimmed) == "utf8bom" {
			opts.BOM = true
		} else {
			var err error
			enc, canonical, err = resolve(trimmed)
			if err != nil {
				return EncodeResult{}, err
			}
		}
	}

	var res EncodeResult
	if opts.BOM && !isUnicode(canonical) {
		return EncodeResult{}, fmt.Errorf("%s output cannot carry a byte order mark", canonical)
	}

	body := text
	if canonical != nameUTF8 {
		var err error
		body, res.Replaced, err = encodeTo(enc, canonical, text, opts.ReplaceUnsupported)
		if err != nil {
			return EncodeResult{}, err
		}
		if len(res.Replaced) > 0 {
			res.Warnings = append(res.Warnings, fmt.Sprintf(
				"%s: %s replaced with %q because %s has no character for them",
				canonical, plural(len(res.Replaced), "character was", "distinct characters were"),
				encoding.ASCIISub, canonical))
		}
	}

	bom := []byte(nil)
	if opts.BOM {
		bom = BOMFor(canonical)
	}
	if len(bom) == 0 {
		res.Bytes = body
		return res, nil
	}

	withBOM := make([]byte, 0, len(bom)+len(body))
	withBOM = append(withBOM, bom...)
	withBOM = append(withBOM, body...)
	res.Bytes = withBOM
	return res, nil
}

// encodeTo runs text through enc, either failing on the first unsupported rune
// after collecting every one of them, or replacing them if asked.
func encodeTo(enc encoding.Encoding, name string, text []byte, replace bool) ([]byte, []rune, error) {
	if !replace {
		if runes, offsets := unsupportedRunes(enc, text); len(runes) > 0 {
			return nil, nil, &UnsupportedRunesError{Encoding: name, Runes: runes, Offsets: offsets}
		}
		out, _, err := transform.Bytes(enc.NewEncoder(), text)
		if err != nil {
			return nil, nil, fmt.Errorf("encoding to %s: %w", name, err)
		}
		return out, nil, nil
	}

	runes, _ := unsupportedRunes(enc, text)
	out, _, err := transform.Bytes(encoding.ReplaceUnsupported(enc.NewEncoder()), text)
	if err != nil {
		return nil, nil, fmt.Errorf("encoding to %s: %w", name, err)
	}
	return out, runes, nil
}

// unsupportedRunes returns the distinct runes of text that enc cannot
// represent, in order of first appearance, together with the byte offset of
// that first appearance.
//
// Every rune is probed at most once; a subtitle file has a small alphabet, so
// the cache does nearly all the work.
func unsupportedRunes(enc encoding.Encoding, text []byte) ([]rune, []int) {
	var (
		runes   []rune
		offsets []int
		seen    = make(map[rune]bool)
		encoder = enc.NewEncoder()
		buf     [utf8.UTFMax]byte
	)
	for i, r := range string(text) {
		if r < utf8.RuneSelf {
			continue
		}
		if _, cached := seen[r]; cached {
			continue // already probed, and reported if it needed reporting
		}
		n := utf8.EncodeRune(buf[:], r)
		_, _, err := transform.Bytes(encoder, buf[:n])
		seen[r] = err == nil
		if err != nil {
			runes = append(runes, r)
			offsets = append(offsets, i)
		}
	}
	return runes, offsets
}

// plural renders a count with the right noun: "1 byte", "3 bytes".
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// formatOffsets renders a list of byte offsets, capped so that a badly damaged
// file does not produce a warning longer than the file.
func formatOffsets(offsets []int) string {
	const maxListed = 10

	var b strings.Builder
	for i, off := range offsets {
		if i == maxListed {
			fmt.Fprintf(&b, ", and %d more", len(offsets)-maxListed)
			break
		}
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%d", off)
	}
	return b.String()
}

// formatByteOffsets is formatOffsets with the offending byte value included,
// which is what a user needs to work out what the file really is.
func formatByteOffsets(b []byte, offsets []int) string {
	const maxListed = 10

	var sb strings.Builder
	for i, off := range offsets {
		if i == maxListed {
			fmt.Fprintf(&sb, ", and %d more", len(offsets)-maxListed)
			break
		}
		if i > 0 {
			sb.WriteString(", ")
		}
		fmt.Fprintf(&sb, "offset %d: %#02x", off, b[off])
	}
	return sb.String()
}
