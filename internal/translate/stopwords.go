package translate

import (
	"strings"
	"unicode"
)

// Function-word frequencies are the last deterministic step, and they only
// matter for the Latin alphabet: every other script has already been settled by
// scriptLanguage at the cost of one unicode.Is per rune.
//
// The lists are function words — articles, pronouns, negations, auxiliaries —
// because they are the words a translation cannot avoid, they are the most
// frequent words in any dialogue, and they are the ones that do not leak across
// languages the way nouns do. They are kept to a similar length per language so
// that a longer list is not itself an advantage.
//
// The decision is deliberately refusable. Danish, Norwegian and Swedish share
// most of their function words, and Portuguese and Spanish share many; when the
// margin between the top two is thin, this function declines to answer and the
// question falls through to the model, which is exactly what the model is for.
var stopwords = map[string][]string{
	"en": {"the", "and", "you", "that", "what", "this", "with", "have", "your", "for", "not", "are", "but", "was", "all", "just", "know", "don't", "i'm", "it's", "they", "from", "about", "there"},
	"es": {"que", "de", "la", "el", "no", "en", "es", "un", "por", "con", "para", "los", "las", "se", "te", "mi", "si", "pero", "como", "esto", "esta", "más", "aquí", "nada"},
	"fr": {"que", "de", "le", "la", "les", "des", "une", "est", "pas", "je", "tu", "vous", "nous", "il", "elle", "dans", "pour", "avec", "mais", "tout", "oui", "ça", "moi", "être"},
	"de": {"der", "die", "das", "und", "ich", "ist", "nicht", "ein", "eine", "du", "sie", "wir", "mit", "auf", "für", "aber", "was", "wie", "sich", "den", "dem", "hier", "nur", "noch"},
	"it": {"che", "di", "il", "la", "non", "per", "con", "sono", "una", "mi", "ti", "questo", "questa", "come", "più", "cosa", "sì", "hai", "sei", "siamo", "anche", "ma", "se", "lui"},
	"pt": {"que", "não", "para", "com", "uma", "você", "eu", "isso", "mais", "está", "estou", "são", "tem", "aqui", "quando", "muito", "bem", "ele", "ela", "nós", "mas", "por", "meu", "sim"},
	"nl": {"het", "een", "ik", "je", "niet", "dat", "en", "is", "van", "wat", "voor", "met", "maar", "ze", "hij", "we", "zijn", "heb", "hier", "ook", "naar", "kan", "moet", "wel"},
	"sv": {"det", "är", "jag", "du", "inte", "att", "en", "och", "för", "med", "han", "hon", "vi", "har", "men", "så", "kan", "här", "vad", "om", "till", "på", "vill", "vet"},
	"da": {"det", "er", "jeg", "du", "ikke", "at", "en", "og", "for", "med", "han", "hun", "vi", "har", "men", "så", "kan", "her", "hvad", "om", "til", "på", "vil", "ved"},
	"no": {"det", "er", "jeg", "du", "ikke", "at", "en", "og", "for", "med", "han", "hun", "vi", "har", "men", "så", "kan", "her", "hva", "om", "til", "på", "vil", "vet"},
	"fi": {"ei", "on", "se", "että", "mitä", "minä", "sinä", "hän", "mutta", "niin", "kuin", "tämä", "olen", "voi", "nyt", "vain", "kun", "ole", "sitä", "hyvä", "täällä", "me", "te", "oli"},
	"pl": {"nie", "to", "jest", "że", "się", "na", "co", "tak", "jak", "ale", "mnie", "tego", "jestem", "będzie", "tylko", "może", "dobrze", "jeszcze", "już", "czy", "mam", "był", "moje", "przez"},
	"cs": {"se", "na", "to", "je", "že", "ale", "jak", "jsem", "jsi", "ne", "co", "ten", "tady", "dobře", "bude", "mám", "jsme", "jen", "byl", "když", "tak", "můj", "něco", "už"},
	"tr": {"bir", "bu", "ne", "için", "ben", "sen", "var", "yok", "çok", "daha", "gibi", "ama", "evet", "hayır", "şey", "değil", "olur", "beni", "seni", "onu", "biz", "burada", "nasıl", "kadar"},
	"ro": {"să", "nu", "este", "ce", "cu", "pentru", "mai", "care", "dar", "aici", "bine", "sunt", "tu", "eu", "îmi", "ăsta", "și", "un", "o", "de", "din", "am", "ai", "te"},
	"hu": {"nem", "hogy", "egy", "az", "ez", "és", "de", "van", "csak", "még", "meg", "már", "mit", "ki", "te", "én", "jó", "vagy", "kell", "itt", "mi", "ha", "volt", "lesz"},
	"id": {"yang", "tidak", "saya", "kamu", "ini", "itu", "untuk", "dengan", "ada", "akan", "sudah", "bisa", "kita", "aku", "dia", "apa", "tapi", "dari", "kalau", "harus", "dan", "juga", "mereka", "ke"},
	"vi": {"không", "tôi", "bạn", "của", "là", "được", "có", "người", "một", "này", "đó", "và", "cho", "những", "anh", "chúng", "đã", "sẽ", "ở", "với", "thì", "làm", "gì", "ta"},
}

const (
	// minStopwordTokens is the smallest sample worth counting words in.
	minStopwordTokens = 40
	// minStopwordShare is the fraction of all tokens the winner must match.
	minStopwordShare = 0.06
	// minStopwordMargin is how far ahead of the runner-up the winner must be.
	minStopwordMargin = 1.3
)

// stopwordIndex is the inverted form of stopwords, built once.
var stopwordIndex = func() map[string][]string {
	m := make(map[string][]string, 512)
	for code, words := range stopwords {
		for _, w := range words {
			m[w] = append(m[w], code)
		}
	}
	return m
}()

// stopwordLanguage guesses a Latin-script language from function-word counts,
// or reports that it will not guess.
func stopwordLanguage(text string) (string, bool) {
	tokens := tokenize(text)
	if len(tokens) < minStopwordTokens {
		return "", false
	}

	score := make(map[string]float64, len(stopwords))
	for _, t := range tokens {
		codes := stopwordIndex[t]
		if len(codes) == 0 {
			continue
		}
		// A word shared by several languages is evidence for each of them, but
		// weaker evidence than a word only one language uses. Without this, the
		// languages that share the most vocabulary would win by sharing.
		w := 1 / float64(len(codes))
		for _, c := range codes {
			score[c] += w
		}
	}

	var (
		bestCode     string
		best, runner float64
	)
	for c, s := range score {
		switch {
		case s > best:
			bestCode, runner, best = c, best, s
		case s > runner:
			runner = s
		}
	}

	if bestCode == "" || best/float64(len(tokens)) < minStopwordShare {
		return "", false
	}
	if runner > 0 && best < runner*minStopwordMargin {
		return "", false
	}
	return bestCode, true
}

// tokenize lowercases text and splits it into words, keeping the apostrophes
// that are part of a word ("don't", "l'ai") and dropping everything else.
func tokenize(text string) []string {
	fields := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && r != '\'' && r != '’'
	})
	out := fields[:0]
	for _, f := range fields {
		f = strings.Trim(strings.ReplaceAll(f, "’", "'"), "'")
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}
