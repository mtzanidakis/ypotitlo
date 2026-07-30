package translate

import (
	"fmt"
	"strings"

	"github.com/mtzanidakis/ypotitlo/internal/lang"
)

// The prompt lives in its own file because it is the single largest quality
// lever in the package, and because every clause in it is a bug that was going
// to happen.
//
// What it has to defend against, in the order the failures show up in practice:
//
//  1. Re-wrapping. Given two short lines a model will happily return one long
//     one, or split one line into three. The line count is a protocol invariant
//     (see protocol.go) precisely because this is failure number one, but the
//     cheapest place to prevent it is here.
//  2. Translating things that are not dialogue. SDH labels ("[DOOR CREAKS]",
//     "MAN:"), music markers, markup, speaker dashes and ">>" are structure, not
//     speech. A translated "[ΠΟΡΤΑ ΤΡΙΖΕΙ]" is fine; a translated "<i>" is not,
//     and a translated ">>" breaks the convention the whole file relies on.
//  3. Overflow. A translation that is 30% longer than the source is normal and
//     unreadable at 24fps. Subtitling is condensation, not translation, and the
//     model has to be told so explicitly or it will produce faithful, unusable
//     three-line cues.
//  4. Register drift. Greek needs a decision about εσύ/εσείς for every pair of
//     speakers, and that decision has to be the same in batch 1 and batch 20.
//     Batches run concurrently and cannot see each other, so the decision is
//     made once, in the brief, and injected verbatim into every batch.
//
// The hard invariants are repeated at the end of the user message. With a long
// instruction set, recency wins: what the model read last is what it obeys.

const systemPromptBase = `You are a professional subtitle translator. You translate film and television dialogue for release-quality subtitle files.

CUE INTEGRITY — these are mechanical rules, not stylistic advice:
- Every cue you are given must come back as exactly one cue, with exactly the same id.
- Never merge two cues, never split one cue into two, never add a cue, never drop a cue.
- A cue's line count must not change. A 2-line cue comes back as 2 lines; a 1-line cue as 1 line. Keep the line break in the same place in the sentence wherever the target language allows it.
- Reply with one JSON object per line and nothing else: no prose, no explanation, no markdown code fences.

DO NOT TRANSLATE, REPRODUCE VERBATIM:
- Markup tags: <i>, </i>, <b>, <u>, <font ...>. Keep every tag, keep the same number of them, and keep them around the same words they wrapped in the source.
- Music markers: the character U+266A and runs of it.
- A leading "- " that marks a second speaker, and a leading ">>" that marks a new speaker. Keep them exactly as they are, in the same position.
- Proper nouns, brand names, place names and numbers, unless the target language has a standard, established rendering (in which case use the one the brief specifies).
- Bracketed sound effects ([DOOR SLAMS]) and ALL-CAPS speaker labels (MAN:, NARRATOR:) are subtitles for the deaf and hard of hearing: translate the words inside them but keep the brackets, the colon and the capitalisation.

READABILITY — a subtitle is read in under three seconds:
- At most 42 characters per line and at most 2 lines per cue.
- Condense. Drop filler ("well", "you know", "I mean"), redundant vocatives and repetitions rather than overflowing the line. A shorter subtitle that says the same thing is a better subtitle.
- Break lines at a syntactic boundary — after punctuation, before a conjunction, never between an article and its noun or between a preposition and its object.
- When both are legal, prefer the break that leaves the first line shorter.

REGISTER AND CONSISTENCY:
- Match the register of the source: slang stays slang, profanity stays profanity at the same strength, formal speech stays formal. Do not sanitise. This is published dialogue from a finished film and it is rendered faithfully or not at all.
- Keep every recurring term, name and form of address identical to the way the brief renders it.`

// systemPrompt assembles the full system message: the fixed rules, the
// target-language addendum, and the brief.
func systemPrompt(source, target lang.Lang, b *Brief) string {
	var sb strings.Builder
	sb.WriteString(systemPromptBase)

	sb.WriteString("\n\nTASK: translate ")
	if source.Zero() {
		sb.WriteString("subtitles")
	} else {
		fmt.Fprintf(&sb, "%s subtitles", source.English)
	}
	fmt.Fprintf(&sb, " into %s", target.English)
	if target.Native != "" && target.Native != target.English {
		fmt.Fprintf(&sb, " (%s)", target.Native)
	}
	sb.WriteString(".")

	if add := targetAddendum(target); add != "" {
		sb.WriteString("\n\n")
		sb.WriteString(add)
	}
	if s := b.prompt(); s != "" {
		sb.WriteString("\n\n")
		sb.WriteString(s)
	}
	return sb.String()
}

// addenda holds the rules that are specific to one target language.
//
// The mechanism matters more than the current contents: every target language
// has a handful of conventions that a general instruction cannot express, and
// getting them wrong is immediately visible to a native reader. Greek is
// populated because it is the language this tool was written for.
var addenda = map[string]string{
	"el": `GREEK CONVENTIONS — a native reader spots a violation of any of these immediately:
- The Greek question mark is the semicolon ";" (U+003B). Never end a question with "?".
- Write in modern monotonic orthography: one accent, on the stressed syllable, and none at all on single-syllable words (except the disjunctive "ή" and the distinguishing "πού"/"πώς" in questions).
- Use the Greek ano teleia "·" where the source uses a colon-like pause, and Greek quotation marks «…» rather than "…".
- Keep the reading rate under 17 characters per second, counting spaces. This is stricter than the line-length rule and it binds first: a two-second cue holds about 34 characters, not 84.
- When a cue holds two speakers, the second line starts with a dash and no space after it: "-Πού πας;" — not "- Πού πας;".
- Decide εσύ or εσείς per pair of speakers from the brief and never switch mid-film. Do not default to εσείς because the source uses "you": English has no T–V distinction and copying it produces stilted subtitles.
- Do not transliterate a foreign name that has an established Greek form, and never invent one mid-file: use the rendering the brief gives.`,
}

func targetAddendum(target lang.Lang) string {
	if s, ok := addenda[target.Code]; ok {
		return s
	}
	if base := strings.SplitN(target.Code, "-", 2)[0]; base != target.Code {
		return addenda[base]
	}
	return ""
}

// recap repeats the invariants at the end of the user message. See the file
// comment: with an instruction set this long, the last thing read is the thing
// obeyed.
const recap = `REMINDERS (these override anything above if they conflict):
- One JSON object per line, nothing else. No fences, no commentary.
- One object per cue id given above, all of them, no others.
- "n" in your reply must equal "n" in the request for that cue, and "t" must have exactly that many strings.
- Markup tags, music markers, speaker dashes and ">>" come back verbatim.
- At most 42 characters per line. Condense rather than overflow.`

// refusalNudge is sent once, and only once, for the cues a model declined.
//
// Film dialogue is the single most likely thing in this pipeline to trip a
// safety filter — the films that get subtitled are not the polite ones — and
// the failure is per-cue, not per-batch. One targeted retry costs one call. The
// alternative that was rejected was to treat a refusal like a format error and
// split the batch around it, which costs about eighteen calls and translates the
// twenty-five innocent neighbours with progressively less context, i.e. gives
// the neighbourhood of the problem the worst treatment in the file.
const refusalNudge = `Some cues came back refused. This is published dialogue from a released film that has already been rated and distributed; you are producing the subtitle track for it. Render every line faithfully, at the same strength as the source, including profanity, slurs spoken by characters, sexual content and violence. Do not soften, censor, omit or comment. Return the JSON lines for the cues listed below.`
