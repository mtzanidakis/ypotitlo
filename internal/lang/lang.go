// Package lang resolves the language codes and names a user may type on the
// command line, and derives the output filename from the input one.
//
// It wraps golang.org/x/text/language, which already knows that gre, ger and
// fre are ISO 639-2/B synonyms of el, de and fr — the twenty-odd languages
// where the bibliographic and terminological codes disagree are almost
// exactly the European languages that get subtitled. What it cannot do is
// parse a language *name*, so this package adds a small hand-written table
// for that direction (see names.go).
package lang

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"golang.org/x/text/language"
)

// Lang is a resolved target or source language.
//
// Code is the canonical short form: the ISO 639-1 two-letter code when the
// language has one, otherwise the three-letter code, optionally followed by
// the script or region subtag when that distinction matters for translation
// (zh-Hans, pt-BR, sr-Latn). It is the form written into output filenames,
// which is what makes the tool idempotent: -ol el, -ol ell, -ol gre,
// -ol Greek and -ol el-GR all produce the same Code and therefore the same
// output path.
type Lang struct {
	Tag     language.Tag
	Code    string
	English string
	Native  string
}

// String renders the language the way it should appear in CLI output.
func (l Lang) String() string {
	if l.Code == "" {
		return "unknown"
	}
	if l.English == "" {
		return l.Code
	}
	return fmt.Sprintf("%s (%s)", l.English, l.Code)
}

// Zero reports whether l is the zero Lang.
func (l Lang) Zero() bool { return l.Code == "" }

// ErrAmbiguous is returned for input that names a macrolanguage whose
// written forms differ enough that guessing one would silently produce an
// unreadable subtitle file.
var ErrAmbiguous = errors.New("ambiguous language")

// ErrUnknown is returned for input that is neither a parseable tag nor a
// known language name.
var ErrUnknown = errors.New("unknown language")

// Resolve turns user input into a Lang. It accepts ISO 639-1 and 639-2 codes
// in either case ("el", "ell", "gre", "EL"), full BCP 47 tags ("el-GR",
// "zh-Hant", "pt-BR"), and English or native language names ("greek",
// "Greek", "Ελληνικά").
func Resolve(s string) (Lang, error) {
	q := strings.TrimSpace(s)
	if q == "" {
		return Lang{}, fmt.Errorf("%w: empty language", ErrUnknown)
	}

	// Tags first. language.Parse handles the ISO 639-2 B/T split for free,
	// which is the whole reason x/text is a dependency.
	if tag, err := language.Parse(q); err == nil {
		return fromTag(tag, q)
	}

	folded := foldName(q)
	if base, ok := ambiguousNames[folded]; ok {
		return Lang{}, ambiguityError(base, q)
	}
	if key, ok := nameIndex()[folded]; ok {
		return fromKey(key), nil
	}

	return Lang{}, fmt.Errorf("%w %q; use an ISO 639-1/639-2 code (el, ell, gre) or an English name (greek)", ErrUnknown, s)
}

// fromTag maps a parsed tag onto a key of the names table, dropping subtags
// from the right until something matches. That is what collapses el-GR to
// el while leaving pt-BR and zh-Hant intact.
func fromTag(tag language.Tag, input string) (Lang, error) {
	parts := strings.Split(tag.String(), "-")
	for i := len(parts); i > 0; i-- {
		if key := strings.Join(parts[:i], "-"); known(key) {
			return fromKey(key), nil
		}
	}

	base, _ := tag.Base()
	if ambiguousBases[base.String()] {
		return Lang{}, ambiguityError(base.String(), input)
	}
	return Lang{}, fmt.Errorf("%w %q: %s is a valid language tag but is not one of the %d subtitle languages ypotitlo knows",
		ErrUnknown, input, tag, len(names))
}

func known(key string) bool {
	_, ok := names[key]
	return ok
}

// fromKey builds a Lang from a names key. The keys are all valid tags, so
// MustParse cannot panic; TestNamesKeysParse pins that down.
func fromKey(key string) Lang {
	n := names[key]
	return Lang{
		Tag:     language.MustParse(key),
		Code:    key,
		English: n.English,
		Native:  n.Native,
	}
}

// ambiguityError reports the concrete alternatives rather than the abstract
// problem: "zh" alone does not say whether the audience reads Simplified or
// Traditional characters, and the two are not interchangeable.
func ambiguityError(base, input string) error {
	var alts []string
	for key := range names {
		if strings.HasPrefix(key, base+"-") {
			alts = append(alts, fmt.Sprintf("%s (%s)", key, names[key].English))
		}
	}
	sort.Strings(alts)
	return fmt.Errorf("%w %q: pick one of %s", ErrAmbiguous, input, strings.Join(alts, " or "))
}

// SameFile reports whether a and b are the same file on disk.
//
// String comparison is not enough and the difference is destructive: the
// tool refuses to write over its own input, and "./movie.el.srt" versus
// "movie.el.srt", a symlink, or a hard link all compare unequal as strings
// while naming the same inode. A path that does not exist is not the same
// file as anything, which is the normal case for an output path.
func SameFile(a, b string) (bool, error) {
	fa, err := os.Stat(a)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("stat %s: %w", a, err)
	}
	fb, err := os.Stat(b)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("stat %s: %w", b, err)
	}
	return os.SameFile(fa, fb), nil
}
