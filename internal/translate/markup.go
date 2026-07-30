package translate

import (
	"strings"
	"unicode"
)

// This file owns everything that must survive a round trip through a language
// model without being understood by it.
//
// Two kinds of markup appear in .srt text and they are handled in opposite
// ways, deliberately:
//
//   - HTML-ish styling (<i>, <b>, <u>, <font color="...">) is sent to the model
//     raw. It is ordinary HTML, it is thoroughly in-distribution, and models
//     reproduce it around translated text without being asked twice. The
//     alternative that was considered and rejected — substituting private-use
//     codepoints (U+E000+n) — fails for exactly the reason it looks clever:
//     nothing in any training corpus contains U+E000, so the model has no prior
//     for echoing it back, and every emphasised cue would fail validation and
//     land in the untranslated bucket. A systematic hole in precisely the lines
//     that carry emphasis.
//
//   - ASS override blocks ({\an8}, {\pos(120,400)}, {\i1}) are stripped before
//     the call and re-attached afterwards. They carry no linguistic content
//     whatsoever, so involving the model can only lose.
//
// Validation of the HTML side compares the multiset of tags in the source
// against the multiset in the reply. A mismatch means the cue is kept in its
// original language rather than emitted with mangled markup.

// knownTags is the closed set of tag names recognised as markup.
//
// The set is closed on purpose. A blanket <[^>]*> match would treat the
// dialogue line "if x < 10 > 3" as a tag and silently eat half of it, and
// subtitle files really do contain bare angle brackets — the srt package goes
// out of its way not to escape them.
var knownTags = map[string]bool{
	"i": true, "b": true, "u": true, "s": true,
	"em": true, "strong": true, "font": true, "br": true,
	"ruby": true, "rt": true, "rp": true,
}

// assBlock is one ASS override block together with the byte offset it occupied
// in the text once every block had been removed.
type assBlock struct {
	off  int
	text string
}

// stripASS removes ASS override blocks from s and returns the remaining text
// plus the removed blocks.
//
// Only "{\" opens a block. A brace that is not followed by a backslash is
// dialogue — "{laughing}" is a real thing subtitlers write — and is left alone.
func stripASS(s string) (string, []assBlock) {
	if !strings.Contains(s, `{\`) {
		return s, nil
	}
	var (
		b      strings.Builder
		blocks []assBlock
	)
	for i := 0; i < len(s); {
		if s[i] == '{' && i+1 < len(s) && s[i+1] == '\\' {
			if end := strings.IndexByte(s[i:], '}'); end >= 0 {
				blocks = append(blocks, assBlock{off: b.Len(), text: s[i : i+end+1]})
				i += end + 1
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String(), blocks
}

// lineParts is one source line decomposed into the pieces that are sent to the
// model (core) and the pieces that are re-attached to whatever comes back.
//
// The whitespace fields exist because leading spaces are load-bearing in real
// subtitle files — they are how some releases indent a second speaker — and no
// model preserves them. Capturing and re-attaching them is only sound because
// the number of lines is a validated protocol invariant: without that, line i
// of the reply would not be line i of the source and the whitespace would be
// re-attached to the wrong text.
type lineParts struct {
	leadWS  string
	trailWS string
	lead    []string // ASS blocks before the text
	mid     []string // ASS blocks inside the text (see rebuild)
	trail   []string // ASS blocks after the text
	core    string   // what the model sees; "" when the line has no text
}

// splitLine decomposes one source line.
func splitLine(s string) lineParts {
	stripped, blocks := stripASS(s)

	trimmed := strings.TrimLeftFunc(stripped, unicode.IsSpace)
	lead := stripped[:len(stripped)-len(trimmed)]
	core := strings.TrimRightFunc(trimmed, unicode.IsSpace)
	trail := stripped[len(lead)+len(core):]

	p := lineParts{leadWS: lead, trailWS: trail, core: core}
	coreEnd := len(lead) + len(core)
	for _, b := range blocks {
		switch {
		case b.off <= len(lead):
			p.lead = append(p.lead, b.text)
		case b.off >= coreEnd:
			p.trail = append(p.trail, b.text)
		default:
			p.mid = append(p.mid, b.text)
		}
	}
	return p
}

// rebuild puts a translated core back into its original wrapping.
//
// Leading and trailing blocks land exactly where they were. A block that sat
// *inside* the text has no faithful position once the words around it have
// changed length and order, so it is appended after the text: dropping it would
// lose data, and guessing a character offset in a different language would put
// an override in the middle of a word. prepare warns whenever this happens.
func (p lineParts) rebuild(core string) string {
	var b strings.Builder
	b.WriteString(p.leadWS)
	for _, t := range p.lead {
		b.WriteString(t)
	}
	b.WriteString(core)
	for _, t := range p.mid {
		b.WriteString(t)
	}
	for _, t := range p.trail {
		b.WriteString(t)
	}
	b.WriteString(p.trailWS)
	return b.String()
}

// tagMultiset counts markup tags in s, keyed by lowercased name with a leading
// "/" for a closing tag. Attributes are deliberately not part of the key: a
// model that rewrites <font color="#FFFFFF"> as <font color="#ffffff"> has not
// damaged anything, and failing the cue over it would cost a translation.
func tagMultiset(s string) map[string]int {
	var counts map[string]int
	for i := 0; i < len(s); i++ {
		if s[i] != '<' {
			continue
		}
		key, end, ok := scanTag(s, i)
		if !ok {
			continue
		}
		if counts == nil {
			counts = make(map[string]int, 4)
		}
		counts[key]++
		i = end
	}
	return counts
}

// scanTag decides whether the '<' at i opens a markup tag, and if so returns
// its multiset key and the index of the closing '>'.
func scanTag(s string, i int) (key string, end int, ok bool) {
	j := i + 1
	closing := false
	if j < len(s) && s[j] == '/' {
		closing = true
		j++
	}
	start := j
	for j < len(s) && isNameByte(s[j]) {
		j++
	}
	name := strings.ToLower(s[start:j])
	if name == "" || !knownTags[name] || j >= len(s) {
		return "", 0, false
	}

	switch {
	case s[j] == '>':
		end = j
	case s[j] == '/' && j+1 < len(s) && s[j+1] == '>':
		end = j + 1
	case s[j] == ' ' || s[j] == '\t':
		k := strings.IndexByte(s[j:], '>')
		if k < 0 {
			return "", 0, false
		}
		end = j + k
	default:
		return "", 0, false
	}

	if closing {
		name = "/" + name
	}
	return name, end, true
}

func isNameByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// sameTags reports whether two sets of lines carry the same markup.
func sameTags(src, dst []string) bool {
	a := tagMultiset(strings.Join(src, "\n"))
	b := tagMultiset(strings.Join(dst, "\n"))
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// balancedSplit re-splits text into exactly n lines on word boundaries.
//
// It is the recovery path for the one protocol violation that cannot be
// repaired by asking again cheaply: the model returned a different number of
// lines than the cue had. Indexing the reply blindly would panic or drop text;
// re-wrapping ourselves keeps the cue and costs nothing.
//
// Each cut is placed as close as possible to an equal share of the remaining
// characters, and ties resolve towards a *shorter* first line, which is the
// standard subtitling convention (a bottom-heavy break reads faster and leaves
// the eye lower on the frame, closer to the picture).
func balancedSplit(text string, n int) []string {
	if n <= 1 {
		return []string{strings.TrimSpace(text)}
	}
	words := strings.Fields(text)
	out := make([]string, 0, n)
	for n > 1 {
		// Leave at least one word for each remaining line.
		if len(words) <= n-1 {
			out = append(out, "")
			n--
			continue
		}
		k := bestCut(words, n)
		out = append(out, strings.Join(words[:k], " "))
		words = words[k:]
		n--
	}
	return append(out, strings.Join(words, " "))
}

// bestCut picks how many of words belong on the current line when n lines are
// still to be produced.
func bestCut(words []string, n int) int {
	total := 0
	for _, w := range words {
		total += len([]rune(w)) + 1
	}
	target := total / n

	best, bestCost, run := 1, -1, 0
	for k := 1; k <= len(words)-(n-1); k++ {
		run += len([]rune(words[k-1])) + 1
		cost := run - target
		if cost < 0 {
			cost = -cost
		}
		// Strictly-less keeps the smallest k on a tie, i.e. the shorter line.
		if bestCost < 0 || cost < bestCost {
			best, bestCost = k, cost
		}
	}
	return best
}
