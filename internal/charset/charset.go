// Package charset decodes subtitle files of unknown encoding into UTF-8 and
// encodes UTF-8 back out again.
//
// Subtitle files in the wild carry no encoding declaration, so the encoding has
// to be guessed from the bytes. Decode applies, in this exact order:
//
//  1. BOM stripping — always first, before any encoding decision. A -charset
//     override must not short-circuit this: a UTF-8 BOM left in front of a
//     windows-1253 decode turns the first index line into the three mojibake
//     characters of the BOM followed by "1", which no longer matches ^\d+$ and
//     so silently becomes subtitle text.
//  2. the caller's override, if any.
//  3. BOM-less UTF-32, then BOM-less UTF-16 by zero-byte parity (not ratio).
//  4. tolerant UTF-8: a file that is 99% well-formed is UTF-8 with a few bad
//     bytes, not a legacy file.
//  5. the Greek ladder, windows-1253 vs ISO-8859-7.
//  6. windows-1252 as a last resort, with a warning.
//
// Every fallback is reported through Result.Warnings rather than being applied
// silently; the caller is expected to print them.
package charset

import (
	"bytes"
	"fmt"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/transform"
)

// Result is the outcome of decoding a byte slice.
type Result struct {
	// Text is the decoded input as UTF-8, with any BOM removed.
	//
	// Text may alias the input slice when the input was already valid UTF-8.
	Text []byte

	// Encoding is the canonical name of the encoding that was actually used,
	// for example "windows-1253". It is never empty.
	Encoding string

	// HadBOM holds the BOM bytes found at the start of the input, or nil if
	// there was none. It is the bytes rather than a bool so that a writer can
	// reproduce the exact BOM it was given.
	HadBOM []byte

	// Warnings lists everything the caller should be told: guesses, conflicts,
	// and bytes that could not be decoded. Empty when the decode was clean.
	Warnings []string
}

// utf8Replacement is U+FFFD encoded as UTF-8.
var utf8Replacement = []byte{0xEF, 0xBF, 0xBD}

// Decode converts b to UTF-8.
//
// override is the user's -charset value; the empty string means "detect". An
// override is honoured for everything except the BOM: if b starts with a BOM
// that contradicts the override, the BOM wins and a warning is recorded, since
// a BOM is evidence and an override is only an assertion.
//
// An unknown override name is an error even when a BOM makes it moot, so that a
// typo is never silently ignored.
func Decode(b []byte, override string) (Result, error) {
	var overrideEnc encoding.Encoding
	var overrideName string
	if override != "" {
		var err error
		overrideEnc, overrideName, err = resolve(override)
		if err != nil {
			return Result{}, err
		}
	}

	if len(b) == 0 {
		// Nothing to detect. Report the declared encoding if there was one, so
		// that an empty file does not look like it changed encoding.
		name := nameUTF8
		if overrideName != "" {
			name = overrideName
		}
		return Result{Text: b, Encoding: name}, nil
	}

	// Step 1: the BOM, always first.
	bom, bomName := detectBOM(b)
	body := b[len(bom):]

	// The BOM is copied rather than aliased: a writer holds on to it to
	// reproduce the input byte for byte, outliving the input slice.
	res := Result{HadBOM: bytes.Clone(bom)}

	switch {
	case bomName != "":
		if overrideName != "" && overrideName != bomName {
			res.warnf("input starts with a %s BOM but charset %s was given; the BOM wins",
				bomName, overrideName)
		}
		enc, _, err := resolve(bomName)
		if err != nil { // unreachable: every BOM name is in the table
			return Result{}, err
		}
		res.decode(enc, bomName, body)

	// Step 2: the override.
	case overrideName != "":
		res.decode(overrideEnc, overrideName, body)

	default:
		res.detect(body)
	}

	return res, nil
}

// detect runs the detection ladder over b, which has already had any BOM
// removed.
func (r *Result) detect(b []byte) {
	// Step 3: BOM-less UTF-32 first, then BOM-less UTF-16. UTF-32 has to come
	// first because UTF-32LE text also looks like UTF-16LE with a lot of NULs.
	if name, ok := detectUTF32(b); ok {
		enc, _, err := resolve(name)
		if err == nil {
			r.decode(enc, name, b)
			return
		}
	}
	if name, ok := detectUTF16(b); ok {
		enc, _, err := resolve(name)
		if err == nil {
			r.decode(enc, name, b)
			return
		}
	}

	// Step 4: UTF-8, tolerantly. A file with no high bytes at all is ASCII,
	// which is UTF-8; anything at least 99% well-formed is a UTF-8 file with a
	// few corrupt bytes, and throwing it into the legacy path over one stray
	// byte would mojibake the whole thing.
	if isASCII(b) {
		r.Text, r.Encoding = b, nameUTF8
		return
	}
	if valid, multibyte := utf8Stats(b); multibyte && valid*100 >= utf8Threshold*len(b) {
		r.decode(nil, nameUTF8, b)
		return
	}

	// Step 5: the Greek ladder, but only if the bytes look Greek at all.
	if looksGreek(b) {
		name, tie := greekLadder(b)
		if enc, _, err := resolve(name); err == nil {
			if tie {
				r.warnf("cannot tell %s from %s here; assuming %s (override with -charset %s)",
					nameWindows1253, nameISO88597, name, nameISO88597)
			}
			r.decode(enc, name, b)
			return
		}
	}

	// Step 6: give up, guess, and say so.
	enc, _, err := resolve(nameWindows1252)
	if err != nil { // unreachable: windows-1252 is always available
		r.Text, r.Encoding = b, nameUTF8
		return
	}
	r.warnf("could not detect the encoding; assuming %s (override with -charset NAME)",
		nameWindows1252)
	r.decode(enc, nameWindows1252, b)
}

// decode runs b through enc and records the result. A nil enc means UTF-8,
// which needs no transformation, only validation.
func (r *Result) decode(enc encoding.Encoding, name string, b []byte) {
	r.Encoding = name

	if enc == nil || name == nameUTF8 {
		text, bad := sanitizeUTF8(b)
		r.Text = text
		if len(bad) > 0 {
			r.warnf("input is UTF-8 with %s at %s; replaced with U+FFFD",
				plural(len(bad), "invalid byte", "invalid bytes"), formatOffsets(bad))
		}
		return
	}

	text, _, err := transform.Bytes(enc.NewDecoder(), b)
	if err != nil {
		// Only the multi-byte decoders can fail, and only on truncated input.
		// Keep what was decoded rather than failing the whole file.
		r.warnf("%s: %v; input may be truncated", name, err)
	}
	r.Text = text

	// charmap decoders never report an error: an undefined byte silently
	// becomes U+FFFD. The only way to notice is to count them afterwards.
	n := bytes.Count(text, utf8Replacement)
	if n == 0 {
		return
	}
	if offsets := replacementOffsets(enc, b); len(offsets) > 0 {
		r.warnf("%s: %s no character (%s); replaced with U+FFFD",
			name, plural(len(offsets), "byte has", "bytes have"), formatByteOffsets(b, offsets))
		return
	}
	r.warnf("%s: %s not be decoded; replaced with U+FFFD",
		name, plural(n, "character could", "characters could"))
}

func (r *Result) warnf(format string, a ...any) {
	r.Warnings = append(r.Warnings, fmt.Sprintf(format, a...))
}

// sanitizeUTF8 returns b with every byte that is not part of a well-formed
// UTF-8 sequence replaced by U+FFFD, plus the offsets of those bytes. A U+FFFD
// that was already encoded in the input is left alone and not reported.
func sanitizeUTF8(b []byte) ([]byte, []int) {
	if utf8.Valid(b) {
		return b, nil
	}

	out := make([]byte, 0, len(b)+len(utf8Replacement))
	var bad []int
	for i := 0; i < len(b); {
		r, size := utf8.DecodeRune(b[i:])
		if r == utf8.RuneError && size <= 1 {
			bad = append(bad, i)
			out = append(out, utf8Replacement...)
			i++
			continue
		}
		out = append(out, b[i:i+size]...)
		i += size
	}
	return out, bad
}

// replacementOffsets reports the offsets in b of bytes that enc maps to U+FFFD.
// It only works for single-byte encodings, where one byte is one rune; for
// anything else it returns nil and the caller falls back to a count.
func replacementOffsets(enc encoding.Encoding, b []byte) []int {
	if !isSingleByte(enc) {
		return nil
	}

	var undefined [256]bool
	dec := enc.NewDecoder()
	for i := range undefined {
		out, _, err := transform.Bytes(dec, []byte{byte(i)})
		undefined[i] = err != nil || bytes.Equal(out, utf8Replacement)
	}

	var offsets []int
	for i, c := range b {
		if undefined[c] {
			offsets = append(offsets, i)
		}
	}
	return offsets
}

func isSingleByte(enc encoding.Encoding) bool {
	switch enc.(type) {
	case *charmap.Charmap, *singleByte:
		return true
	default:
		return false
	}
}
