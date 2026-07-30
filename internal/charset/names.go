package charset

import (
	"fmt"
	"sort"
	"strings"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/htmlindex"
	"golang.org/x/text/encoding/ianaindex"
	"golang.org/x/text/encoding/unicode/utf32"
)

// Canonical names of the encodings this package refers to by name.
const (
	nameUTF8        = "utf-8"
	nameUTF16LE     = "utf-16le"
	nameUTF16BE     = "utf-16be"
	nameUTF32LE     = "utf-32le"
	nameUTF32BE     = "utf-32be"
	nameWindows1252 = "windows-1252"
	nameWindows1253 = "windows-1253"
	nameISO88597    = "iso-8859-7"
	nameCP737       = "cp737"
	nameCP869       = "cp869"
	nameMacGreek    = "macgreek"
)

// extra holds the encodings the x/text indexes cannot supply.
//
// htmlindex is the WHATWG whitelist, which rejects cp737, cp869 and macgreek
// outright — precisely the encodings old Greek subtitle rips use, so the escape
// hatch has a hole in it exactly where it is needed. ianaindex knows the name
// "cp869" but has no implementation and returns (nil, nil) for it, which is why
// resolve nil-checks the encoding and not only the error. The UTF-32 family is
// in the same position.
var extra = map[string]struct {
	enc  encoding.Encoding
	name string
}{
	"cp737":          {cp737, nameCP737},
	"ibm737":         {cp737, nameCP737},
	"xibm737":        {cp737, nameCP737},
	"oem737":         {cp737, nameCP737},
	"msdos737":       {cp737, nameCP737},
	"greek737":       {cp737, nameCP737},
	"cp869":          {cp869, nameCP869},
	"ibm869":         {cp869, nameCP869},
	"xibm869":        {cp869, nameCP869},
	"oem869":         {cp869, nameCP869},
	"msdos869":       {cp869, nameCP869},
	"greek869":       {cp869, nameCP869},
	"869":            {cp869, nameCP869},
	"csibm869":       {cp869, nameCP869},
	"macgreek":       {macGreek, nameMacGreek},
	"xmacgreek":      {macGreek, nameMacGreek},
	"macintoshgreek": {macGreek, nameMacGreek},
	"utf32le":        {utf32.UTF32(utf32.LittleEndian, utf32.IgnoreBOM), nameUTF32LE},
	"utf32be":        {utf32.UTF32(utf32.BigEndian, utf32.IgnoreBOM), nameUTF32BE},
	"utf32":          {utf32.UTF32(utf32.BigEndian, utf32.ExpectBOM), nameUTF32BE},
	"ucs4":           {utf32.UTF32(utf32.BigEndian, utf32.ExpectBOM), nameUTF32BE},
}

// supported is the list of canonical names reported by SupportedNames. Common
// aliases (cp1253, latin1, greek, iso8859-7, x-mac-greek, ...) also resolve;
// listing every alias of every encoding would bury the useful names.
var supported = []string{
	"big5", "cp737", "cp869", "euc-jp", "euc-kr", "gb18030", "gbk", "ibm866",
	"iso-2022-jp", "iso-8859-2", "iso-8859-3", "iso-8859-4", "iso-8859-5",
	"iso-8859-6", "iso-8859-7", "iso-8859-8", "iso-8859-8-i", "iso-8859-10",
	"iso-8859-13", "iso-8859-14", "iso-8859-15", "iso-8859-16", "koi8-r",
	"koi8-u", "macgreek", "macintosh", "shift_jis", "utf-8", "utf-16be",
	"utf-16le", "utf-32be", "utf-32le", "windows-874", "windows-1250",
	"windows-1251", "windows-1252", "windows-1253", "windows-1254",
	"windows-1255", "windows-1256", "windows-1257", "windows-1258",
	"x-mac-cyrillic",
}

// SupportedNames returns the canonical charset names accepted by Decode's
// override and by Encode, sorted. Aliases of these names are accepted too.
func SupportedNames() []string {
	out := make([]string, len(supported))
	copy(out, supported)
	sort.Strings(out)
	return out
}

// UnknownCharsetError is returned when a charset name cannot be resolved.
type UnknownCharsetError struct {
	Name string
}

func (e *UnknownCharsetError) Error() string {
	return fmt.Sprintf("unknown charset %q; supported: %s (common aliases such as "+
		"cp1253, latin1, greek or x-mac-greek are accepted too)",
		e.Name, strings.Join(SupportedNames(), ", "))
}

// resolve maps a user-supplied charset name to an encoding and its canonical
// name.
//
// The lookup order is htmlindex (WHATWG labels: cp1253, latin1, greek, ...),
// then the IANA and MIME indexes, then the hand-written table for what x/text
// leaves out. Both ianaindex lookups can succeed with a nil encoding, meaning
// "registered but not implemented"; that is a miss, not a hit.
func resolve(name string) (encoding.Encoding, string, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return nil, "", &UnknownCharsetError{Name: name}
	}

	if enc, err := htmlindex.Get(trimmed); err == nil && enc != nil {
		return enc, canonicalName(enc, trimmed), nil
	}
	for _, idx := range []*ianaindex.Index{ianaindex.IANA, ianaindex.MIME} {
		if enc, err := idx.Encoding(trimmed); err == nil && enc != nil {
			return enc, canonicalName(enc, trimmed), nil
		}
	}
	if e, ok := extra[normalize(trimmed)]; ok {
		return e.enc, e.name, nil
	}
	return nil, "", &UnknownCharsetError{Name: name}
}

// normalize reduces a charset name to lowercase alphanumerics so that cp-737,
// CP_737 and cp737 all match the same table entry.
func normalize(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// canonicalName returns the name to report for enc, preferring the WHATWG name
// (lowercase, and the one users are most likely to recognise) over the IANA
// one. fallback is used only for encodings no index can name.
func canonicalName(enc encoding.Encoding, fallback string) string {
	if sb, ok := enc.(*singleByte); ok {
		return sb.name
	}
	if name, err := htmlindex.Name(enc); err == nil && name != "" {
		return name
	}
	if name, err := ianaindex.MIME.Name(enc); err == nil && name != "" {
		return strings.ToLower(name)
	}
	if name, err := ianaindex.IANA.Name(enc); err == nil && name != "" {
		return strings.ToLower(name)
	}
	return strings.ToLower(fallback)
}

// isUnicode reports whether the named encoding is a Unicode encoding form, and
// so can represent every rune and can legitimately carry a BOM.
func isUnicode(name string) bool {
	switch name {
	case nameUTF8, nameUTF16LE, nameUTF16BE, nameUTF32LE, nameUTF32BE:
		return true
	default:
		return false
	}
}
