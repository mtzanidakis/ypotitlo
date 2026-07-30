package charset

import "unicode/utf8"

const (
	// sampleSize is how much of the input the byte-statistics detectors look
	// at. Subtitle files are homogeneous; the first few KB decide it.
	sampleSize = 4096

	// utf8Threshold is the percentage of bytes that must belong to well-formed
	// UTF-8 sequences for the input to be treated as UTF-8 with damage rather
	// than as a legacy encoding.
	utf8Threshold = 99

	// minUTF16Pairs is the smallest sample the UTF-16 parity test will judge.
	// Below it the counts are too small to mean anything, and the ratios below
	// would happily classify a two-byte file as UTF-16.
	minUTF16Pairs = 16

	// greekByteShare is the percentage of all bytes that must fall in the Greek
	// letter range before the windows-1253/ISO-8859-7 ladder is entered. Greek
	// text runs around 50%; Western European text with the odd accented letter
	// runs a few percent, so the two do not overlap.
	greekByteShare = 15

	// greekHighShare is the percentage of the non-ASCII bytes that must fall in
	// the Greek letter range. It rejects files that are mostly some other
	// high-byte alphabet with a Greek-looking tail.
	greekHighShare = 60
)

// sample returns the leading window of b the statistical detectors work on.
func sample(b []byte) []byte {
	if len(b) > sampleSize {
		return b[:sampleSize]
	}
	return b
}

// isASCII reports whether b contains no byte above 0x7F. Such a file is ASCII,
// which is UTF-8, and needs no detection at all.
func isASCII(b []byte) bool {
	for _, c := range b {
		if c >= 0x80 {
			return false
		}
	}
	return true
}

// utf8Stats reports how many bytes of b belong to well-formed UTF-8 sequences
// and whether at least one of those sequences is multi-byte.
//
// The multi-byte requirement matters: without it every ASCII file with one
// stray high byte would score 99.9% and be called UTF-8, when the stray byte is
// the only evidence there is and it says the opposite.
func utf8Stats(b []byte) (validBytes int, multibyte bool) {
	for i := 0; i < len(b); {
		r, size := utf8.DecodeRune(b[i:])
		if r == utf8.RuneError && size <= 1 {
			i++
			continue
		}
		validBytes += size
		if size > 1 {
			multibyte = true
		}
		i += size
	}
	return validBytes, multibyte
}

// detectUTF32 reports BOM-less UTF-32 by looking for the two always-zero bytes
// of every code unit. It runs before the UTF-16 test because UTF-32LE text is
// also full of zero bytes at odd offsets and would otherwise be read as
// UTF-16LE, turning every character into a pair of them.
func detectUTF32(b []byte) (string, bool) {
	s := sample(b)
	s = s[:len(s)-len(s)%4]
	groups := len(s) / 4
	if groups < minUTF16Pairs/2 || len(b)%4 != 0 {
		return "", false
	}

	var le, be int
	for i := 0; i < len(s); i += 4 {
		// Every code point below U+10000 leaves the top two bytes zero.
		if s[i+2] == 0 && s[i+3] == 0 {
			le++
		}
		if s[i] == 0 && s[i+1] == 0 {
			be++
		}
	}

	switch {
	case le*10 >= groups*9 && be*10 < groups*9:
		return nameUTF32LE, true
	case be*10 >= groups*9 && le*10 < groups*9:
		return nameUTF32BE, true
	default:
		return "", false
	}
}

// detectUTF16 reports BOM-less UTF-16 by separating zero bytes by parity rather
// than by counting them.
//
// A plain ratio test does not work for the language this tool exists to
// produce: Greek is U+03xx, so the high byte of a Greek code unit is 0x03 and
// not 0x00, and a real Greek UTF-16LE subtitle with two lines per cue measures
// only about 25-28% zero bytes — under any sensible ratio threshold. What is
// invariant is that in UTF-16 every zero byte lands on the same parity: the
// same sample gives 226 zeros at odd offsets and 0 at even ones. A legacy
// single-byte file has no zero bytes at all, so the test cannot fire on one.
func detectUTF16(b []byte) (string, bool) {
	s := sample(b)
	s = s[:len(s)-len(s)%2]
	pairs := len(s) / 2
	if pairs < minUTF16Pairs {
		return "", false
	}

	var even, odd int
	for i := 0; i < len(s); i += 2 {
		if s[i] == 0 {
			even++
		}
		if s[i+1] == 0 {
			odd++
		}
	}

	hi, lo := even, odd
	if odd > even {
		hi, lo = odd, even
	}
	if hi < pairs/10 || hi == 0 || lo > hi/20 {
		return "", false
	}
	if odd > even {
		return nameUTF16LE, true
	}
	return nameUTF16BE, true
}

// Byte ranges shared by windows-1253 and ISO-8859-7. The two encodings agree
// from 0xB8 to 0xFE, which is the whole Greek alphabet, and disagree almost
// everywhere below it — that disagreement is what the ladder exploits.
const (
	greekLetterLo = 0xB8 // Έ in both encodings
	greekLetterHi = 0xFE // ώ in both encodings
	greekUpperLo  = 0xC0 // ΐ; letters proper start here
	greekLowerLo  = 0xE0 // ΰ; lowercase from here up
)

// looksGreek reports whether the byte distribution of b is Greek enough for the
// windows-1253/ISO-8859-7 ladder to be meaningful. Without this gate the ladder
// would confidently label a French windows-1252 file as Greek, since é and è
// sit in the Greek letter range too.
func looksGreek(b []byte) bool {
	s := sample(b)
	var greek, high int
	for _, c := range s {
		if c >= 0x80 {
			high++
		}
		if c >= greekLetterLo && c <= greekLetterHi {
			greek++
		}
	}
	if high == 0 {
		return false
	}
	return greek*100 >= greekByteShare*len(s) && greek*100 >= greekHighShare*high
}

// greekLadder decides between windows-1253 and ISO-8859-7 for bytes that are
// already known to be Greek. It returns the chosen canonical name and whether
// the decision was a coin toss.
//
// The rules are mutually exclusive and strictly ordered; each one is a byte
// that is meaningful in one encoding and impossible or absurd in the other.
func greekLadder(b []byte) (name string, tie bool) {
	var (
		hasC1       bool
		hasISOOnly  bool
		has1253Only bool
		hasQuote    bool
	)
	for _, c := range b {
		switch {
		// 1. 0x80-0x9F are C1 control codes in ISO-8859-7 and never occur in
		// text; in windows-1253 they are the em dash, the ellipsis and the
		// curly quotes that professionally typeset subtitles are full of.
		case c >= 0x80 && c <= 0x9F:
			hasC1 = true
		// 2. In ISO-8859-7 these are Ά ΅ € ₯ ͺ; in windows-1253 they are ¶ µ ¤ ¥
		// and, for 0xAA, nothing at all.
		case c == 0xB6 || c == 0xB5 || c == 0xA4 || c == 0xA5 || c == 0xAA:
			hasISOOnly = true
		// 3. 0xAE is ® in windows-1253 and undefined in ISO-8859-7.
		case c == 0xAE:
			has1253Only = true
		case c == 0xA1 || c == 0xA2:
			hasQuote = true
		}
	}

	switch {
	case hasC1:
		return nameWindows1253, false
	case hasISOOnly:
		return nameISO88597, false
	case has1253Only:
		return nameWindows1253, false
	case hasQuote:
		// 4. 0xA1/0xA2 are ‘ and ’ in ISO-8859-7 but ΅ and Ά in windows-1253.
		// Decided by context, never by count: counting was the original bug,
		// because elision (σ’ αυτό, απ’ την) makes 0xA2 ubiquitous in Greek
		// while Ά is all but absent from natural dialogue, so a count always
		// picked the wrong one.
		iso, win := quoteVotes(b)
		switch {
		case iso > win:
			return nameISO88597, false
		case win > iso:
			return nameWindows1253, false
		}
	}
	// 5. Nothing decisive: windows-1253 is the more common encoding by far, but
	// say so.
	return nameWindows1253, true
}

// quoteVotes classifies every 0xA1/0xA2 in b by its neighbours and returns how
// many read as an ISO-8859-7 elision apostrophe and how many as a
// windows-1253 accented capital.
func quoteVotes(b []byte) (iso, win int) {
	for i, c := range b {
		if c != 0xA1 && c != 0xA2 {
			continue
		}
		var prev, next byte
		if i > 0 {
			prev = b[i-1]
		}
		if i+1 < len(b) {
			next = b[i+1]
		}

		switch {
		// A Greek letter, the mark, then a space or punctuation: an elision
		// apostrophe. Nothing in windows-1253 reads that way.
		case prev >= greekUpperLo && prev <= greekLetterHi && isBreak(next):
			iso++
		// Start of line or a space, the mark, then a lowercase Greek letter:
		// a capital starting a word, so windows-1253.
		case (i == 0 || isSpace(prev)) && next >= greekLowerLo && next <= greekLetterHi:
			win++
		}
	}
	return iso, win
}

// isBreak reports whether c ends a word: whitespace, ASCII punctuation, or the
// end of the input (the zero byte the caller substitutes).
func isBreak(c byte) bool {
	if c == 0 || isSpace(c) {
		return true
	}
	return c < 0x80 && !isAlphaNum(c)
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f'
}

func isAlphaNum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}
