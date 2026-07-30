package lang

import (
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/text/language"
)

// markerSet is the set of dot-separated filename segments that describe the
// *kind* of subtitle track rather than its language.
//
// Peeling these off before any language test is not cosmetic, it prevents
// data loss. "sdh" parses as a well-formed language tag (Southern Kurdish),
// "hi" as Hindi and "cs" as Czech, so a naive "replace the trailing language
// segment" turns movie.sdh.srt into movie.el.srt — which is the filename the
// *normal* translation of movie.srt already occupies. Translating both a
// regular and an SDH track would silently leave one of them.
var markerSet = map[string]bool{
	"forced":   true,
	"sdh":      true,
	"hi":       true,
	"cc":       true,
	"hoh":      true,
	"hearing":  true,
	"impaired": true,
	"full":     true,
	"sub":      true,
	"subs":     true,
	"srt":      true,
	"ass":      true,
	"ssa":      true,
	"vtt":      true,
	"default":  true,
	"native":   true,
	"dub":      true,
	"dubbed":   true,
}

// subtitleExts are the extensions we recognise as "this is the file type
// suffix" as opposed to "this is part of the name". Anything else — the
// ".2024" of movie.2024, say — is treated as part of the stem and .srt is
// appended, rather than producing movie.el.2024.
var subtitleExts = map[string]bool{
	".srt":  true,
	".ass":  true,
	".ssa":  true,
	".vtt":  true,
	".sub":  true,
	".sbv":  true,
	".smi":  true,
	".ttml": true,
	".dfxp": true,
}

// defaultExt is used when the input has no recognisable subtitle extension.
const defaultExt = ".srt"

// DeriveOutputPath computes the path to write the translation of in to, by
// replacing the language segment of the filename with target's canonical
// code — movie.en.srt with -ol greek becomes movie.el.srt.
//
// Track markers keep their place after the code, so movie.eng.sdh.srt
// becomes movie.el.sdh.srt and movie.en.forced.srt becomes
// movie.el.forced.srt. When there is no language segment to replace the code
// is appended: movie.2024.srt becomes movie.2024.el.srt.
//
// The result is not guaranteed to differ from in — translating movie.el.srt
// into Greek derives movie.el.srt — so callers must check SameFile before
// writing.
func DeriveOutputPath(in string, target Lang) (string, error) {
	if target.Zero() {
		return "", fmt.Errorf("derive output path for %q: no target language", in)
	}
	if in == "" {
		return "", fmt.Errorf("derive output path: empty input path")
	}
	if in == "-" {
		return "", fmt.Errorf("derive output path: cannot derive an output path from stdin; pass -o")
	}

	// filepath.Split, not Dir+Base: it keeps the directory prefix verbatim,
	// so "./movie.srt" stays "./movie.el.srt" instead of losing the "./".
	dir, base := filepath.Split(in)
	if base == "" {
		return "", fmt.Errorf("derive output path: %q is a directory", in)
	}

	// filepath.Ext is already directory-aware — Ext("/a.b/movie") is "" — so
	// a dot in a parent directory needs no special handling.
	ext := filepath.Ext(base)
	stem := base
	if subtitleExts[strings.ToLower(ext)] {
		stem = base[:len(base)-len(ext)]
	} else {
		ext = defaultExt
	}
	if stem == "" {
		return "", fmt.Errorf("derive output path: %q has no filename before the extension", in)
	}

	segs := strings.Split(stem, ".")

	// Peel trailing markers first, remembering their order.
	var markers []string
	for len(segs) > 1 && markerSet[strings.ToLower(segs[len(segs)-1])] {
		markers = append([]string{segs[len(segs)-1]}, markers...)
		segs = segs[:len(segs)-1]
	}

	// Only now is it safe to ask whether the trailing segment is a language.
	// The len > 1 guard keeps "en.srt" from becoming ".el.srt".
	if len(segs) > 1 && resolvesAsLanguage(segs[len(segs)-1]) {
		segs = segs[:len(segs)-1]
	}

	segs = append(segs, target.Code)
	segs = append(segs, markers...)

	return dir + strings.Join(segs, ".") + ext, nil
}

// FromFilename reads the language out of a subtitle filename: movie.en.srt or
// movie.eng.sdh.srt both yield English.
//
// It exists so that language *detection* and output-path *derivation* share one
// marker table. Two copies of that table would drift, and a segment classified
// as a marker here but as a language there is exactly how movie.en.sdh.srt ends
// up translated from Southern Kurdish.
func FromFilename(path string) (Lang, bool) {
	if path == "" || path == "-" {
		return Lang{}, false
	}

	base := filepath.Base(path)
	if ext := filepath.Ext(base); ext != "" {
		base = base[:len(base)-len(ext)]
	}

	segs := strings.Split(base, ".")
	for len(segs) > 1 && markerSet[strings.ToLower(segs[len(segs)-1])] {
		segs = segs[:len(segs)-1]
	}
	if len(segs) < 2 {
		return Lang{}, false
	}

	seg := segs[len(segs)-1]
	if !resolvesAsLanguage(seg) {
		return Lang{}, false
	}
	l, err := Resolve(seg)
	if err != nil {
		return Lang{}, false
	}
	return l, true
}

// resolvesAsLanguage reports whether a filename segment is a language code.
//
// All four conditions are load-bearing. The length bound rejects words;
// markerSet rejects track descriptors that happen to parse; language.Parse
// rejects arbitrary text; and the names lookup rejects "sub", "srt", "ass"
// and "ssa", which are well-formed but unassigned ISO 639-3 subtags and so
// parse without error.
func resolvesAsLanguage(seg string) bool {
	if n := len(seg); n < 2 || n > 3 {
		return false
	}
	if markerSet[strings.ToLower(seg)] {
		return false
	}
	tag, err := language.Parse(seg)
	if err != nil {
		return false
	}
	parts := strings.Split(tag.String(), "-")
	for i := len(parts); i > 0; i-- {
		if known(strings.Join(parts[:i], "-")) {
			return true
		}
	}
	// A bare "zh" segment is a language even though we refuse it as a
	// target, so it should still be replaced rather than kept.
	base, _ := tag.Base()
	return ambiguousBases[base.String()]
}
