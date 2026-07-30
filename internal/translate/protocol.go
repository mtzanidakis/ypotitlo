package translate

import (
	"encoding/json"
	"fmt"
	"strings"
)

// The wire format is JSON Lines: the reply is expected to be one JSON object
// per line, and it is parsed by walking the reply line by line and unmarshalling
// each line on its own.
//
//	{"i":12,"n":2,"t":["πρώτη γραμμή","δεύτερη γραμμή"],"s":"ok"}
//
// Everything about that sentence is load-bearing:
//
//   - "line by line" is what makes a markdown fence, a "Here you go:" preamble,
//     a closing "Let me know if..." , CRLF line endings and blank lines all
//     harmless, without a single regexp: none of them parses as a JSON object,
//     so all of them are skipped.
//   - "each line on its own" is what caps the blast radius of a malformed
//     object at one cue instead of one batch.
//   - the id is explicit, so nothing is matched by position. The alternative —
//     delimiters plus positional matching, which is what one of the reference
//     projects does — de-synchronises the entire rest of the file the first time
//     the model emits an empty translation, with no error anywhere.
//
// Ids are renumbered 1..N per batch so the model never sees a large, gapped or
// duplicated SRT index and never has a reason to "correct" one.

// entry is one line of the reply.
type entry struct {
	I int    `json:"i"`
	N int    `json:"n"`
	T jsonl  `json:"t"`
	S string `json:"s"`
}

// refused reports whether the model declined this cue.
func (e entry) refused() bool {
	s := strings.ToLower(strings.TrimSpace(e.S))
	return s == "refused" || s == "refusal" || s == "refuse"
}

// blank reports whether the entry carries no usable text.
func (e entry) blank() bool {
	for _, s := range e.T {
		if strings.TrimSpace(s) != "" {
			return false
		}
	}
	return true
}

// jsonl is a list of translated lines that also accepts the two shapes a model
// reaches for when it drifts: a bare string, and a bare string containing
// embedded newlines. Both carry the full translation, so rejecting them would
// throw away a cue over punctuation in the envelope.
type jsonl []string

func (l *jsonl) UnmarshalJSON(b []byte) error {
	var arr []string
	if err := json.Unmarshal(b, &arr); err == nil {
		*l = arr
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*l = strings.Split(s, "\n")
		return nil
	}
	return fmt.Errorf("translate: field t is neither a string nor an array of strings")
}

// parseJSONL scans a reply and returns the entries it could understand, keyed
// by id, plus the number of non-blank lines it had to skip.
//
// The skip count is not cosmetic: zero entries and a high skip count is a
// format violation worth one strict retry, whereas zero entries and zero skips
// means the model said nothing at all.
func parseJSONL(reply string) (map[int]entry, int) {
	got := make(map[int]entry)
	skipped := 0
	for _, raw := range strings.Split(reply, "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if line == "" {
			continue
		}
		var e entry
		if err := json.Unmarshal([]byte(line), &e); err != nil || e.I <= 0 {
			skipped++
			continue
		}
		// First writer wins. A duplicated id is a model error either way, and
		// preferring the first keeps the result independent of how many copies
		// arrived.
		if _, dup := got[e.I]; !dup {
			got[e.I] = e
		}
	}
	return got, skipped
}

// refusalMarkers are the openings of a plain-prose refusal, checked only when a
// reply produced no parseable entry at all. A cue whose translation legitimately
// contains "I'm sorry" is never examined by this function.
var refusalMarkers = []string{
	"i can't", "i cannot", "i can not", "i won't", "i will not",
	"i'm sorry", "i am sorry", "i'm not able", "i am unable",
	"cannot assist", "can't assist", "can't help with", "cannot help with",
	"i must decline", "as an ai",
}

func looksRefusal(reply string) bool {
	s := strings.ToLower(reply)
	if len(s) > 600 {
		s = s[:600]
	}
	for _, m := range refusalMarkers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}
