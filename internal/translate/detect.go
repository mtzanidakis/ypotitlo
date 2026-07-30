package translate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/mtzanidakis/ypotitlo/internal/lang"
	"github.com/mtzanidakis/ypotitlo/internal/llm"
	"github.com/mtzanidakis/ypotitlo/internal/srt"
)

// Language detection is deterministic first and asks the model last, in that
// order and for that reason.
//
// The tempting design — one cheap call, "what language is this?" — is wrong on
// three counts. It costs money and latency on every run for something the
// filename usually states outright. Modern models do not need the source
// language to translate; two of the three reference implementations never ask
// for it. And the obvious sample, "the first fifteen cues", is the worst
// possible one: the head of a subtitle file is release credits, a scene-group
// tag and the opening titles, none of which is dialogue and some of which is in
// a different language from the film.
//
// So: the filename first (free, and usually right), then the script (one
// unicode.Is call per rune, and it settles Greek, Cyrillic, Arabic, Hebrew, CJK
// and half a dozen others outright), then function-word frequencies for the
// Latin alphabet, and only then a model call — sampled from the middle of the
// file, and never for a file too small to sample.

// ErrUndetermined is returned when nothing could identify the language.
var ErrUndetermined = errors.New("translate: could not determine the source language")

// detection provenance strings, returned to the caller so the CLI can print
// where the answer came from. "detected Greek" invites no scrutiny; "detected
// Greek (from the filename)" tells a user with a mislabelled file exactly which
// of their assumptions is wrong.
const (
	FromFilename  = "filename"
	FromScript    = "script"
	FromStopwords = "common words"
	FromModel     = "model"
)

// minModelCues is the number of substantial cues below which the model is not
// asked at all. A file with fewer is a sample or a fragment, and a guess from
// three lines of dialogue is not worth a call.
const minModelCues = 15

// minModelWords is how many words a cue needs to count towards minModelCues.
const minModelWords = 3

// DetectLanguage identifies the language of cues, returning the language and
// the provenance of that answer.
//
// It makes at most one provider call, and none at all for an empty file, for a
// file whose name already names its language, for a non-Latin script, or when
// o.Provider is nil.
func DetectLanguage(ctx context.Context, cues []srt.Cue, filename string, o Options) (lang.Lang, string, error) {
	if l, ok := langFromFilename(filename); ok {
		return l, FromFilename, nil
	}

	text := sampleText(cues)
	if strings.TrimSpace(text) == "" {
		// An empty file has no language and must not cost a call.
		return lang.Lang{}, "", ErrUndetermined
	}

	if code, ok := scriptLanguage(text); ok {
		if l, err := lang.Resolve(code); err == nil {
			return l, FromScript, nil
		}
	}

	if code, ok := stopwordLanguage(text); ok {
		if l, err := lang.Resolve(code); err == nil {
			return l, FromStopwords, nil
		}
	}

	if o.Provider == nil || o.Model == "" {
		return lang.Lang{}, "", ErrUndetermined
	}
	sample, n := modelSample(cues)
	if n < minModelCues {
		return lang.Lang{}, "", fmt.Errorf("%w: only %d cues of %d+ words to go on", ErrUndetermined, n, minModelWords)
	}
	return askModel(ctx, sample, o)
}

// langFromFilename reads the language out of movie.en.srt or movie.eng.sdh.srt.
//
// The marker peeling lives in internal/lang so that detection and output-path
// derivation cannot disagree about whether a segment such as "sdh" (which is a
// real language tag, Southern Kurdish) names a language or a track type.
func langFromFilename(path string) (lang.Lang, bool) {
	return lang.FromFilename(path)
}

// sampleText concatenates cue text for the deterministic tests. It walks the
// whole file: script and word statistics are cheap and more text is strictly
// better.
func sampleText(cues []srt.Cue) string {
	var sb strings.Builder
	for _, c := range cues {
		for _, line := range c.Lines {
			sb.WriteString(line)
			sb.WriteByte('\n')
		}
		if sb.Len() > 200_000 {
			break
		}
	}
	return sb.String()
}

// modelSample takes cues from the middle of the file.
//
// The head is credits and titles and the tail is the closing song. The middle
// is dialogue. One real file in the author's test set has twelve cues at the
// head that are all the same sentence, a transcription artefact; sampling the
// head would have detected the language of a hallucination.
func modelSample(cues []srt.Cue) (string, int) {
	var good []string
	for _, c := range cues {
		s := strings.TrimSpace(strings.Join(c.Lines, " "))
		if s == "" || len(strings.Fields(s)) < minModelWords {
			continue
		}
		good = append(good, s)
	}
	n := len(good)
	if n == 0 {
		return "", 0
	}
	const want = 20
	start := max(0, n/2-want/2)
	end := min(n, start+want)
	return strings.Join(good[start:end], "\n"), n
}

// askModel is the last resort: one call, one word back.
func askModel(ctx context.Context, sample string, o Options) (lang.Lang, string, error) {
	req := llm.Request{
		Stage:           "detect",
		Model:           o.Model,
		MaxTokens:       64,
		Temperature:     ptr(0.0),
		ReasoningEffort: reasoningEffort,
		Schema: &llm.JSONSchema{
			Name: "language",
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"code": map[string]any{"type": "string"},
				},
				"required": []any{"code"},
			},
		},
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: "You identify the language of a text. Answer only with the JSON object."},
			{Role: llm.RoleUser, Content: "What language are these subtitle lines written in? Reply with its ISO 639-1 code (or 639-3 when there is no two-letter code) in the field \"code\".\n\n" + sample},
		},
	}

	out, _, err := llm.CompleteJSON[struct {
		Code string `json:"code"`
	}](ctx, o.Provider, req)
	if err != nil {
		return lang.Lang{}, "", fmt.Errorf("%w: %w", ErrUndetermined, err)
	}
	l, err := lang.Resolve(strings.TrimSpace(out.Code))
	if err != nil {
		return lang.Lang{}, "", fmt.Errorf("%w: model answered %q: %w", ErrUndetermined, out.Code, err)
	}
	return l, FromModel, nil
}

// scriptRanges maps a script to the language it settles, for the scripts where
// the script alone is the answer.
var scriptRanges = []struct {
	name  string
	table *unicode.RangeTable
	code  string
}{
	{"Greek", unicode.Greek, "el"},
	{"Hebrew", unicode.Hebrew, "he"},
	{"Thai", unicode.Thai, "th"},
	{"Armenian", unicode.Armenian, "hy"},
	{"Georgian", unicode.Georgian, "ka"},
	{"Hangul", unicode.Hangul, "ko"},
	{"Hiragana", unicode.Hiragana, "ja"},
	{"Katakana", unicode.Katakana, "ja"},
	{"Devanagari", unicode.Devanagari, "hi"},
	{"Bengali", unicode.Bengali, "bn"},
	{"Tamil", unicode.Tamil, "ta"},
	{"Telugu", unicode.Telugu, "te"},
	{"Malayalam", unicode.Malayalam, "ml"},
	{"Sinhala", unicode.Sinhala, "si"},
	{"Khmer", unicode.Khmer, "km"},
	{"Myanmar", unicode.Myanmar, "my"},
	// Cyrillic, Arabic and Han name a family, not a language, and are refined
	// below by letters that only one member of the family uses.
	{"Cyrillic", unicode.Cyrillic, ""},
	{"Arabic", unicode.Arabic, ""},
	{"Han", unicode.Han, ""},
}

// scriptShare is the fraction of letters one script must hold to decide the
// question. It is well below a half because subtitle files are full of Latin
// proper nouns, timestamps, song titles and release tags whatever the language.
const scriptShare = 0.30

// scriptLanguage identifies the language from the writing system alone.
func scriptLanguage(text string) (string, bool) {
	counts := make(map[string]int, len(scriptRanges))
	letters := 0
	for _, r := range text {
		if !unicode.IsLetter(r) {
			continue
		}
		letters++
		for _, s := range scriptRanges {
			if unicode.Is(s.table, r) {
				counts[s.name]++
				break
			}
		}
	}
	if letters == 0 {
		return "", false
	}

	for _, s := range scriptRanges {
		if float64(counts[s.name])/float64(letters) < scriptShare {
			continue
		}
		switch s.name {
		case "Cyrillic":
			return refineCyrillic(text), true
		case "Arabic":
			return refineArabic(text), true
		case "Han":
			// Hangul and the kana are checked before Han, and Japanese text
			// always carries kana, so reaching here means Chinese.
			return refineHan(text), true
		default:
			return s.code, true
		}
	}
	return "", false
}

// refineCyrillic tells the Cyrillic-writing languages apart by the letters that
// only one of them has. Answering "Russian" for every Cyrillic file would be
// right most of the time and insulting the rest of it.
func refineCyrillic(text string) string {
	switch {
	case containsAny(text, "ђћџљњЂЋЏЉЊ"):
		return "sr"
	case containsAny(text, "їєґЇЄҐ"):
		return "uk"
	case containsAny(text, "ѓќѕЃЌЅ"):
		return "mk"
	case containsAny(text, "ўЎ"):
		return "be"
	case countAny(text, "ъЪ") > countAny(text, "ыЫэЭ"):
		// Bulgarian uses ъ as an ordinary vowel and has no ы or э at all;
		// Russian has both and uses ъ only as a rare hard sign.
		return "bg"
	default:
		return "ru"
	}
}

// refineArabic separates the Arabic script's three big subtitle languages.
func refineArabic(text string) string {
	switch {
	case containsAny(text, "ٹڈڑںے"):
		return "ur"
	case containsAny(text, "پچژگ"):
		return "fa"
	default:
		return "ar"
	}
}

// refineHan picks a Chinese script. lang.Resolve refuses a bare "zh" — it does
// not say whether the audience reads simplified or traditional characters — so
// something has to be chosen here, and the characters that were simplified are
// the evidence for choosing it.
func refineHan(text string) string {
	simplified := countAny(text, "个们这来说时国见东车马门风飞长书语实")
	traditional := countAny(text, "個們這來說時國見東車馬門風飛長書語實")
	if traditional > simplified {
		return "zh-Hant"
	}
	return "zh-Hans"
}

func containsAny(text, set string) bool {
	return strings.ContainsAny(text, set)
}

func countAny(text, set string) int {
	n := 0
	for _, r := range text {
		if strings.ContainsRune(set, r) {
			n++
		}
	}
	return n
}
