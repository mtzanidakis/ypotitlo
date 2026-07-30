package lang

import (
	"strings"
	"sync"
)

// name holds the two human-readable renderings of a language: the English one
// (used in error messages and in the LLM prompt) and the endonym (used when
// telling the model which language to write in — models follow "γράψε στα
// Ελληνικά" more reliably than "write in Greek").
type name struct {
	English string
	Native  string
}

// names maps a canonical BCP 47 tag string to its display names.
//
// This table exists instead of golang.org/x/text/language/display, which
// carries 2.35 MB of CLDR tables for every locale pair. We only ever need
// two strings per language, in one direction, for the ~70 languages that
// subtitles are actually published in.
//
// The keys double as the canonical Code returned by Resolve, which is why
// they are the ISO 639-1 two-letter form wherever one exists: it is what
// makes -ol el, -ol ell, -ol gre and -ol greek all write the same file.
// Keys with a script or region subtag are only present where the
// distinction changes the translation (zh-Hans/zh-Hant, pt/pt-BR,
// sr/sr-Latn, es/es-419, fr/fr-CA).
var names = map[string]name{
	"af":      {"Afrikaans", "Afrikaans"},
	"ar":      {"Arabic", "العربية"},
	"az":      {"Azerbaijani", "Azərbaycan dili"},
	"be":      {"Belarusian", "Беларуская"},
	"bg":      {"Bulgarian", "Български"},
	"bn":      {"Bengali", "বাংলা"},
	"bs":      {"Bosnian", "Bosanski"},
	"ca":      {"Catalan", "Català"},
	"cs":      {"Czech", "Čeština"},
	"cy":      {"Welsh", "Cymraeg"},
	"da":      {"Danish", "Dansk"},
	"de":      {"German", "Deutsch"},
	"el":      {"Greek", "Ελληνικά"},
	"en":      {"English", "English"},
	"eo":      {"Esperanto", "Esperanto"},
	"es":      {"Spanish", "Español"},
	"es-419":  {"Latin American Spanish", "Español latinoamericano"},
	"et":      {"Estonian", "Eesti"},
	"eu":      {"Basque", "Euskara"},
	"fa":      {"Persian", "فارسی"},
	"fi":      {"Finnish", "Suomi"},
	"fil":     {"Filipino", "Filipino"},
	"fr":      {"French", "Français"},
	"fr-CA":   {"Canadian French", "Français canadien"},
	"ga":      {"Irish", "Gaeilge"},
	"gl":      {"Galician", "Galego"},
	"he":      {"Hebrew", "עברית"},
	"hi":      {"Hindi", "हिन्दी"},
	"hr":      {"Croatian", "Hrvatski"},
	"hu":      {"Hungarian", "Magyar"},
	"hy":      {"Armenian", "Հայերեն"},
	"id":      {"Indonesian", "Bahasa Indonesia"},
	"is":      {"Icelandic", "Íslenska"},
	"it":      {"Italian", "Italiano"},
	"ja":      {"Japanese", "日本語"},
	"ka":      {"Georgian", "ქართული"},
	"kk":      {"Kazakh", "Қазақша"},
	"km":      {"Khmer", "ខ្មែរ"},
	"ko":      {"Korean", "한국어"},
	"lt":      {"Lithuanian", "Lietuvių"},
	"lv":      {"Latvian", "Latviešu"},
	"mk":      {"Macedonian", "Македонски"},
	"ml":      {"Malayalam", "മലയാളം"},
	"mn":      {"Mongolian", "Монгол"},
	"ms":      {"Malay", "Bahasa Melayu"},
	"mt":      {"Maltese", "Malti"},
	"my":      {"Burmese", "မြန်မာ"},
	"nb":      {"Norwegian Bokmål", "Norsk bokmål"},
	"ne":      {"Nepali", "नेपाली"},
	"nl":      {"Dutch", "Nederlands"},
	"nn":      {"Norwegian Nynorsk", "Norsk nynorsk"},
	"no":      {"Norwegian", "Norsk"},
	"pl":      {"Polish", "Polski"},
	"pt":      {"Portuguese", "Português"},
	"pt-BR":   {"Brazilian Portuguese", "Português brasileiro"},
	"ro":      {"Romanian", "Română"},
	"ru":      {"Russian", "Русский"},
	"si":      {"Sinhala", "සිංහල"},
	"sk":      {"Slovak", "Slovenčina"},
	"sl":      {"Slovenian", "Slovenščina"},
	"sq":      {"Albanian", "Shqip"},
	"sr":      {"Serbian", "Српски"},
	"sr-Latn": {"Serbian (Latin)", "Srpski"},
	"sv":      {"Swedish", "Svenska"},
	"sw":      {"Swahili", "Kiswahili"},
	"ta":      {"Tamil", "தமிழ்"},
	"te":      {"Telugu", "తెలుగు"},
	"th":      {"Thai", "ไทย"},
	"tr":      {"Turkish", "Türkçe"},
	"uk":      {"Ukrainian", "Українська"},
	"ur":      {"Urdu", "اردو"},
	"vi":      {"Vietnamese", "Tiếng Việt"},
	"zh-Hans": {"Chinese (Simplified)", "简体中文"},
	"zh-Hant": {"Chinese (Traditional)", "繁體中文"},
}

// aliases are extra spellings that people actually type. They exist because
// the canonical English names in names are either parenthesised
// ("Chinese (Simplified)") or unidiomatic as a command-line argument.
var aliases = map[string]string{
	"simplified chinese":   "zh-Hans",
	"chinese simplified":   "zh-Hans",
	"mandarin":             "zh-Hans",
	"traditional chinese":  "zh-Hant",
	"chinese traditional":  "zh-Hant",
	"brazilian":            "pt-BR",
	"brazilian portuguese": "pt-BR",
	"portuguese (brazil)":  "pt-BR",
	"castilian":            "es",
	"latin american":       "es-419",
	"canadian french":      "fr-CA",
	"french (canada)":      "fr-CA",
	"bokmal":               "nb",
	"nynorsk":              "nn",
	"serbian latin":        "sr-Latn",
	"serbian (latin)":      "sr-Latn",
	"farsi":                "fa",
	"tagalog":              "fil",
	"flemish":              "nl",
	"greek (modern)":       "el",
	"modern greek":         "el",
}

// ambiguousNames are spellings that name a macrolanguage whose subtitle
// forms are not mutually intelligible in writing. Accepting them would pick
// a script for the user; we make them ask instead.
var ambiguousNames = map[string]string{
	"chinese":  "zh",
	"zhongwen": "zh",
	"中文":       "zh",
}

// ambiguousBases is the tag-side counterpart of ambiguousNames: a bare "zh"
// parses perfectly well, it just does not say which script to write.
var ambiguousBases = map[string]bool{"zh": true}

var (
	byNameOnce sync.Once
	byName     map[string]string
)

// nameIndex builds the name → tag reverse index. It is what makes -ol greek
// work at all: language.Parse only ever sees subtags, and rejects every
// language name outright.
func nameIndex() map[string]string {
	byNameOnce.Do(func() {
		byName = make(map[string]string, len(names)*2+len(aliases))
		for tag, n := range names {
			byName[foldName(n.English)] = tag
			byName[foldName(n.Native)] = tag
		}
		// Aliases are applied last so that a hand-written alias always wins
		// over a generated one.
		for alias, tag := range aliases {
			byName[foldName(alias)] = tag
		}
	})
	return byName
}

// foldName normalises a language name for lookup: case-folded, with runs of
// whitespace and underscores collapsed to a single space. It deliberately
// keeps parentheses so that "Chinese (Simplified)" stays addressable.
func foldName(s string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(strings.TrimSpace(s)), func(r rune) bool {
		return r == ' ' || r == '\t' || r == '_'
	}), " ")
}
