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
	core = restoreSpeakerDash(p.core, core)

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

// restoreSpeakerDash re-attaches a leading dash the model dropped.
//
// A dash at the start of a subtitle line marks a change of speaker within one
// cue. It is structure, not words, and losing it merges two people's dialogue
// into one voice for a viewer. Observed once in a 656-cue film: the model
// translated both lines of a two-speaker cue correctly and returned only the
// first dash.
//
// Restoring it is safe precisely because it is not content — the prompt asks
// for the dash to be kept, this only repairs the cases where that was ignored,
// and a line that already has one is left alone.
func restoreSpeakerDash(src, translated string) string {
	dash, ok := leadingDash(src)
	if !ok {
		return translated
	}
	if _, already := leadingDash(translated); already {
		return translated
	}
	return dash + strings.TrimLeft(translated, " \t")
}

// leadingDash returns the speaker-dash prefix of s ("- " or "-"), if any.
func leadingDash(s string) (string, bool) {
	t := strings.TrimLeft(s, " \t")
	if !strings.HasPrefix(t, "-") {
		return "", false
	}
	// A lone "-" or "--" is punctuation or an em-dash substitute, not a marker.
	rest := t[1:]
	if strings.HasPrefix(rest, "-") || strings.TrimSpace(rest) == "" {
		return "", false
	}
	if strings.HasPrefix(rest, " ") {
		return "- ", true
	}
	return "-", true
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

// stripTags removes recognised markup from s, leaving the words.
func stripTags(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '<' {
			// scanTag reports the index of the closing '>', so the loop's own
			// increment is what steps past it.
			if _, end, ok := scanTag(s, i); ok {
				i = end
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return strings.TrimSpace(b.String())
}

// reconcileTags salvages a translation whose markup does not match its source.
//
// Rejecting the whole cue was the original behaviour and it is the wrong trade:
// a viewer reading an English line because the model emitted one italic pair too
// many has lost far more than one reading it in Greek without the italics. The
// text is what was asked for; the markup is decoration.
//
// So the words are kept and the markup is rebuilt from the source. When every
// source line is wrapped in one tag pair — which is what subtitle italics almost
// always are — the same wrapping is put back. Anything more intricate is left
// plain, because guessing where a tag belongs inside a reordered sentence is how
// markup ends up in the middle of a word.
func reconcileTags(src, dst []string) (out []string, wrapped bool) {
	out = make([]string, len(dst))
	for i, line := range dst {
		out[i] = stripTags(line)
	}
	open, close, ok := commonWrap(src)
	if !ok {
		return out, false
	}
	for i, line := range out {
		if line != "" {
			out[i] = open + line + close
		}
	}
	return out, true
}

// commonWrap reports the single tag pair enclosing every non-empty source line,
// if there is one.
func commonWrap(src []string) (open, close string, ok bool) {
	for _, line := range src {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		o, end, good := scanTag(line, 0)
		if !good || strings.HasPrefix(o, "/") {
			return "", "", false
		}
		closing := "</" + o + ">"
		if !strings.HasSuffix(line, closing) {
			return "", "", false
		}
		// The wrapping must be the only markup on the line.
		inner := line[end+1 : len(line)-len(closing)]
		if strings.Contains(inner, "<") {
			return "", "", false
		}
		got := line[:end+1]
		if open == "" {
			open, close = got, closing
		} else if open != got {
			return "", "", false
		}
	}
	return open, close, open != ""
}
