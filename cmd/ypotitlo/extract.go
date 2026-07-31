package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"unicode"

	"github.com/mtzanidakis/ypotitlo/internal/lang"
	"github.com/mtzanidakis/ypotitlo/internal/mkv"
	"github.com/mtzanidakis/ypotitlo/internal/srt"
)

type extractFlags struct {
	in           string
	out          string
	sourceLang   string
	list         bool
	wantForced   bool
	wantSDH      bool
	force        bool
	quiet        bool
	bom          bool
	crlf         bool
	bomGiven     bool
	lineEndGiven bool
}

func cmdExtract(_ context.Context, e env, args []string) error {
	fs := newFlagSet("extract")
	var f extractFlags

	fs.StringVar(&f.in, "i", "", "input video file")
	fs.StringVar(&f.sourceLang, "il", "", "language of the track to extract")
	fs.BoolVar(&f.wantForced, "forced", false, "take the forced track of that language")
	fs.BoolVar(&f.wantSDH, "sdh", false, "take the hearing-impaired track of that language")
	fs.StringVar(&f.out, "o", "", "output file, or - for stdout")
	fs.BoolVar(&f.list, "list", false, "list the subtitle tracks and stop")
	fs.BoolVar(&f.force, "f", false, "overwrite an existing output file")
	fs.BoolVar(&f.quiet, "q", false, "suppress the summary")
	fs.BoolVar(&f.bom, "bom", false, "write a UTF-8 BOM")
	fs.BoolVar(&f.crlf, "crlf", false, "write CRLF line endings (default: as the cues had)")

	// "-" is allowed through for -i so that the check below can explain why
	// extract cannot read stdin. Left out of the list, the generic guard
	// would answer "did you omit its value?", which is the wrong advice for
	// a user who meant it.
	if err := parseFlags(fs, args, "i", "o"); err != nil {
		if errors.Is(err, flagErrHelp) {
			extractUsage(e.Stdout)
			return nil
		}
		return err
	}
	f.bomGiven = wasGiven(fs, "bom")
	f.lineEndGiven = wasGiven(fs, "crlf")

	if f.in == "" {
		return usagef("-i is required")
	}
	// A subtitle track is interleaved through the whole container, so
	// reading one means seeking through it. A pipe cannot do that, and
	// buffering a gigabyte of film to work around it would be absurd.
	if f.in == stdinPath {
		return usagef("extract needs a seekable file and cannot read stdin")
	}
	return runExtract(e, f)
}

func runExtract(e env, f extractFlags) error {
	file, err := os.Open(f.in)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()

	m, err := mkv.NewReader(file)
	if err != nil {
		if errors.Is(err, mkv.ErrNotMatroska) {
			return usagef("%s is %v; extract reads Matroska containers (.mkv, .mka, .webm)", displayPath(f.in), err)
		}
		return &parseError{fmt.Errorf("reading %s: %w", displayPath(f.in), err)}
	}
	for _, w := range m.Warnings() {
		warnf(e, "%s", w)
	}

	subs := m.SubtitleTracks()
	if f.list {
		listTracks(e, f.in, subs)
		return nil
	}

	// An -il this tool cannot resolve is not refused: it falls back to
	// matching the container's tag as written, which is the only way to name
	// a track tagged "zxx" or with a typo the muxer baked in. A genuine
	// mistyped language then fails on "no subtitle track for ...", which
	// names the languages that are there.
	want, _ := lang.Resolve(f.sourceLang)
	track, notes, err := pickTrack(subs, want, strings.TrimSpace(f.sourceLang), f.wantForced, f.wantSDH)
	if err != nil {
		return err
	}
	for _, n := range notes {
		warnf(e, "%s", n)
	}

	res, err := m.Cues(track.Number)
	if err != nil {
		return &parseError{fmt.Errorf("reading track %d of %s: %w", track.Number, displayPath(f.in), err)}
	}
	for _, w := range res.Warnings {
		warnf(e, "%s", w)
	}

	// Resolving the language is best-effort by design. A track tagged with
	// something this tool does not translate is still worth extracting; it
	// just loses the language segment of the derived filename.
	trackLang, langNote := resolveTrackLang(track)
	outPath, err := resolveExtractPath(f, track, trackLang)
	if err != nil {
		return err
	}
	if err := guardOutput(f.in, outPath, f.force); err != nil {
		return err
	}

	if len(res.Cues) == 0 {
		return fmt.Errorf("track %d of %s contains no cues", track.Number, displayPath(f.in))
	}

	out, warnings := buildSRT(res.Cues)
	body, writeWarnings, err := srt.WriteBytes(out, extractWriteOptions(f))
	if err != nil {
		return err
	}
	for _, w := range append(warnings, writeWarnings...) {
		warnf(e, "%s", w)
	}
	if err := writeFileAtomic(e, outPath, body); err != nil {
		return err
	}

	if !f.quiet {
		outf(e.Stderr, "track %d: %s, %s%s\n", track.Number, describeTrackLang(trackLang, track), track.CodecName(), langNote)
		outf(e.Stderr, "wrote %s (%d cues)\n", displayPath(outPath), len(res.Cues))
	}
	return nil
}

// pickTrack decides which track to extract, given the language asked for.
//
// The language is the selector rather than the track number because a track
// number is a fact about the container, not about the subtitle: it differs
// between two rips of the same film, it differs from the stream index every
// other tool prints, and nobody knows it without looking it up first. A
// language is what the user actually has an opinion about.
//
// The returned notes describe any choice that had to be made. Choosing is not
// refused — the whole point of selecting by language is that the track number
// need never come up — but a choice that is never mentioned is the kind of
// silence this tool exists to avoid.
func pickTrack(subs []mkv.Track, want lang.Lang, raw string, forced, sdh bool) (mkv.Track, []string, error) {
	if len(subs) == 0 {
		return mkv.Track{}, nil, fmt.Errorf("the file has no subtitle tracks")
	}

	// Matching happens over every subtitle track, not just the readable
	// ones, so that asking for a language carried only by a bitmap track
	// gets the explanation rather than "no such language".
	candidates := subs
	if raw != "" {
		candidates = nil
		for _, t := range subs {
			if matchesLanguage(t, want, raw) {
				candidates = append(candidates, t)
			}
		}
		if len(candidates) == 0 {
			choices := languageChoices(subs)
			if len(choices) > maxListedChoices {
				return mkv.Track{}, nil, usagef("no subtitle track for %q; this file has %d other languages, and -list shows them", raw, len(choices))
			}
			return mkv.Track{}, nil, usagef("no subtitle track for %q; this file has %s", raw, strings.Join(choices, ", "))
		}
	}

	// -forced and -sdh are filters, not preferences: asking for a track the
	// file does not have is a mistake worth reporting, not something to
	// quietly satisfy with the nearest other track.
	if forced || sdh {
		var marked []mkv.Track
		for _, t := range candidates {
			if (!forced || trackIsForced(t)) && (!sdh || trackIsSDH(t)) {
				marked = append(marked, t)
			}
		}
		if len(marked) == 0 {
			forLang := ""
			if raw != "" {
				forLang = fmt.Sprintf(" for %q", raw)
			}
			if len(subs) > maxListedChoices {
				return mkv.Track{}, nil, usagef("no %s subtitle track%s; -list shows the %d tracks this file has",
					describeMarkers(forced, sdh), forLang, len(subs))
			}
			return mkv.Track{}, nil, usagef("no %s subtitle track%s; this file has %s",
				describeMarkers(forced, sdh), forLang, describeTracks(subs))
		}
		candidates = marked
	}

	var text []mkv.Track
	for _, t := range candidates {
		if t.Text() {
			text = append(text, t)
		}
	}
	if len(text) == 0 {
		return mkv.Track{}, nil, unsupportedCodec(bestOf(candidates))
	}
	if len(text) == 1 {
		return chose(text[0])
	}

	// Several tracks are still in play. Which language each one is in is the
	// one thing this command cannot work out on its own, so that is the only
	// case left worth refusing.
	if raw == "" && len(distinctLanguages(text)) > 1 {
		choices := languageChoices(text)
		if len(choices) > maxListedChoices {
			return mkv.Track{}, nil, usagef("the file has subtitles in %d languages; run -list to see them, then say which with -il", len(choices))
		}
		return mkv.Track{}, nil, usagef("the file has subtitles in %s; say which with -il", strings.Join(choices, ", "))
	}

	chosen := bestOf(text)
	return chose(chosen, fmt.Sprintf("%d tracks match; extracting %s and leaving %s",
		len(text), trackSummary(chosen), describeOthers(text, chosen)))
}

// chose is the single exit for a successful selection, so that a marker read
// out of a track's name is reported however the track was arrived at.
func chose(t mkv.Track, notes ...string) (mkv.Track, []string, error) {
	return t, append(notes, inferredMarkerNotes(t)...), nil
}

// Track names that mean "forced" and "hearing impaired".
//
// These are matched against a track's title, which is prose — "English
// (Hearing Impaired)", "Signs & Songs" — so they are matched as whole words.
// Substring matching would read "hi" out of "This" and mark half a library
// hearing-impaired. They are a separate table from internal/lang's markerSet,
// which classifies dot-separated filename segments: "hi" is a safe marker there
// and an unsafe one here, and merging the two would import that risk.
var (
	forcedNameWords = map[string]bool{"forced": true, "signs": true}
	sdhNameWords    = map[string]bool{"sdh": true, "cc": true, "hoh": true, "hearing": true, "impaired": true}
)

// trackIsForced and trackIsSDH report the track's kind from either source.
//
// The flag alone is not enough. FlagHearingImpaired was added to Matroska long
// after the files people own were muxed, and a great many rips record the fact
// only in the track's title. Reading the title as well is what makes -sdh find
// the track, and what puts the .sdh marker in the output filename so that it
// does not collide with the ordinary one.
func trackIsForced(t mkv.Track) bool { return t.Forced || nameSays(t.Name, forcedNameWords) }
func trackIsSDH(t mkv.Track) bool    { return t.HearingImpaired || nameSays(t.Name, sdhNameWords) }

func nameSays(name string, words map[string]bool) bool {
	for _, w := range strings.FieldsFunc(strings.ToLower(name), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if words[w] {
			return true
		}
	}
	return false
}

// inferredMarkerNotes reports a marker that came from the track's name rather
// than from its flag, because that inference decides the output filename.
func inferredMarkerNotes(t mkv.Track) []string {
	var out []string
	if !t.Forced && trackIsForced(t) {
		out = append(out, fmt.Sprintf("track %d carries no forced flag, but its name %q says it is forced; the output is named accordingly", t.Number, t.Name))
	}
	if !t.HearingImpaired && trackIsSDH(t) {
		out = append(out, fmt.Sprintf("track %d carries no hearing-impaired flag, but its name %q says it is; the output is named accordingly", t.Number, t.Name))
	}
	return out
}

// describeMarkers names the kind of track that was asked for, for an error
// that has to repeat the request back.
func describeMarkers(forced, sdh bool) string {
	switch {
	case forced && sdh:
		return "forced hearing-impaired"
	case forced:
		return "forced"
	default:
		return "hearing-impaired"
	}
}

// describeTracks lists the tracks in full, for an error where the flags are the
// point and a bare language list would not explain the refusal.
func describeTracks(tracks []mkv.Track) string {
	out := make([]string, 0, len(tracks))
	for _, t := range tracks {
		out = append(out, trackSummary(t))
	}
	return strings.Join(out, ", ")
}

// matchesLanguage reports whether a track carries the language asked for.
//
// Both forms are compared. The resolved code is what makes -il greek, -il el
// and -il ell one request; the raw tag is the only way to name a track whose
// tag this tool cannot resolve at all — "zxx", "qaa", a typo the muxer baked in
// — but which is nonetheless the track the user is pointing at.
func matchesLanguage(t mkv.Track, want lang.Lang, raw string) bool {
	if l, _ := resolveTrackLang(t); !l.Zero() && l.Code == want.Code {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(t.Language), strings.TrimSpace(raw))
}

// bestOf picks one track out of several, the same way every time.
//
// A forced track is signs and captions only — thirty cues where the full track
// has fifteen hundred — so it is never what somebody meant to translate, even
// in a file that marks it default. An SDH track is a whole subtitle and is
// merely the less likely of the two. After that the file's own default wins,
// and the track number settles the rest so that the same file always yields the
// same subtitle.
func bestOf(tracks []mkv.Track) mkv.Track {
	sorted := slices.Clone(tracks)
	slices.SortStableFunc(sorted, func(a, b mkv.Track) int {
		if r := selectionRank(a) - selectionRank(b); r != 0 {
			return r
		}
		return cmp.Compare(a.Number, b.Number)
	})
	return sorted[0]
}

func selectionRank(t mkv.Track) int {
	rank := 0
	if trackIsForced(t) {
		rank += 4
	}
	if trackIsSDH(t) {
		rank += 2
	}
	if !t.Default {
		rank++
	}
	return rank
}

// distinctLanguages counts the languages present, by the tag as written: two
// tracks tagged "eng" are one language even when one of them is forced.
func distinctLanguages(tracks []mkv.Track) map[string]bool {
	out := make(map[string]bool, len(tracks))
	for _, t := range tracks {
		out[strings.ToLower(t.Language)] = true
	}
	return out
}

// maxListedChoices caps how much an error spells out before it defers to -list.
//
// A streaming rip carries forty subtitle tracks, and an error that prints all
// forty is one the reader scrolls past rather than reads. The cap sits above an
// ordinary disc rip, so the common case still gets the whole list — which is
// the useful part, because it can be pasted straight into -il.
const maxListedChoices = 8

// languageChoices lists the -il values that would select something, deduplicated
// and in file order.
//
// A tag this tool cannot resolve is still offered, spelled as the file spells
// it: -il und does select an untagged track, and an entry that named the tag
// without saying how to ask for it would be a dead end.
func languageChoices(tracks []mkv.Track) []string {
	var seen []string
	for _, t := range tracks {
		name, code := t.Language, t.Language
		if l, _ := resolveTrackLang(t); !l.Zero() {
			name, code = l.English, l.Code
		}
		s := fmt.Sprintf("%s (-il %s)", name, code)
		if name == code {
			s = fmt.Sprintf("-il %s", code)
		}
		if !slices.Contains(seen, s) {
			seen = append(seen, s)
		}
	}
	return seen
}

// describeOthers names the tracks that were not chosen, so that a wrong choice
// is visible and correctable rather than merely wrong.
func describeOthers(tracks []mkv.Track, chosen mkv.Track) string {
	var out []string
	for _, t := range tracks {
		if t.Number != chosen.Number {
			out = append(out, trackSummary(t))
		}
	}
	return strings.Join(out, ", ")
}

// unsupportedCodec explains what would be needed to read a track this tool
// cannot, rather than saying only that it cannot.
func unsupportedCodec(t mkv.Track) error {
	switch t.CodecID {
	case mkv.CodecASS, mkv.CodecSSA:
		return fmt.Errorf("track %d is %s, which carries styling this tool would have to throw away to make an SRT; extract it with mkvextract instead", t.Number, t.CodecName())
	case mkv.CodecPGS, mkv.CodecVobSub, mkv.CodecDVB:
		return fmt.Errorf("track %d is %s: the subtitles are images, not text, and turning them into an SRT needs OCR", t.Number, t.CodecName())
	default:
		return fmt.Errorf("track %d is %s, which this tool cannot read as text", t.Number, t.CodecName())
	}
}

func listTracks(e env, in string, subs []mkv.Track) {
	if len(subs) == 0 {
		outf(e.Stdout, "%s has no subtitle tracks\n", displayPath(in))
		return
	}

	tw := tabwriter.NewWriter(e.Stdout, 0, 0, 2, ' ', 0)
	rowf(tw, "TRACK\tLANGUAGE\tCODEC\tFLAGS\tNAME\n")
	for _, t := range subs {
		lg, _ := resolveTrackLang(t)
		language := t.Language
		if !lg.Zero() {
			language = lg.Code
		}
		if t.LanguageIsDefault {
			// Not silently shown as "en": the file said nothing and
			// Matroska's default is what filled it in. Every second rip
			// on a NAS is tagged this way by accident.
			language += " (assumed)"
		}
		rowf(tw, "%d\t%s\t%s\t%s\t%s\n", t.Number, language, t.CodecName(), orDash(trackFlags(t)), orDash(t.Name))
	}
	_ = tw.Flush()

	outf(e.Stdout, "\nExtract one with 'ypotitlo extract -i %s -il LANGUAGE'.\n", shellQuote(in))
	if inferredMarkers(subs) {
		outf(e.Stdout, "A ? means the flag is not set and the track's name is what says so.\n")
	}
}

// inferredMarkers reports whether any listed track owes a marker to its name,
// which is the only case where the "?" in the flags column needs explaining.
func inferredMarkers(tracks []mkv.Track) bool {
	for _, t := range tracks {
		if (!t.Forced && trackIsForced(t)) || (!t.HearingImpaired && trackIsSDH(t)) {
			return true
		}
	}
	return false
}

// trackFlags renders the track's kind as a compact list.
//
// A marker read out of the track's name rather than off its flag is suffixed
// with "?", so that a listing predicts the filename an extraction will produce
// and says which of the two it came from.
func trackFlags(t mkv.Track) string {
	var out []string
	if t.Default {
		out = append(out, "default")
	}
	if t.Forced {
		out = append(out, "forced")
	} else if trackIsForced(t) {
		out = append(out, "forced?")
	}
	if t.HearingImpaired {
		out = append(out, "sdh")
	} else if trackIsSDH(t) {
		out = append(out, "sdh?")
	}
	return strings.Join(out, ",")
}

// trackSummary names a track well enough to tell it apart from its siblings,
// which is what a message about a choice between them has to do.
func trackSummary(t mkv.Track) string {
	var parts []string
	if t.Language != "" {
		parts = append(parts, t.Language)
	}
	if flags := trackFlags(t); flags != "" {
		parts = append(parts, flags)
	}
	if t.Name != "" {
		parts = append(parts, fmt.Sprintf("%q", t.Name))
	}
	s := fmt.Sprintf("track %d", t.Number)
	if len(parts) > 0 {
		s += " (" + strings.Join(parts, ", ") + ")"
	}
	return s
}

// resolveTrackLang turns the track's tag into a Lang, and returns a note
// explaining any doubt about it.
//
// A failure is not an error: "und", "mul" and a misspelt tag are all things
// real files contain, and none of them is a reason to refuse an extraction. The
// note is what keeps the doubt visible.
func resolveTrackLang(t mkv.Track) (lang.Lang, string) {
	l, err := lang.Resolve(t.Language)
	switch {
	case err != nil:
		return lang.Lang{}, fmt.Sprintf(" (the file tags it %q, which is not a language this tool knows)", t.Language)
	case t.LanguageIsDefault:
		return l, " (assumed: the file tags no language, and Matroska's default is English)"
	default:
		return l, ""
	}
}

func describeTrackLang(l lang.Lang, t mkv.Track) string {
	if l.Zero() {
		return fmt.Sprintf("language %q", t.Language)
	}
	return l.English
}

// resolveExtractPath decides where the SRT goes.
//
// The track's flags become filename markers, so the forced and the full track
// of one language land on different files instead of one overwriting the other.
func resolveExtractPath(f extractFlags, t mkv.Track, l lang.Lang) (string, error) {
	if f.out != "" {
		return f.out, nil
	}
	var markers []string
	if trackIsForced(t) {
		markers = append(markers, "forced")
	}
	if trackIsSDH(t) {
		markers = append(markers, "sdh")
	}
	out, err := lang.SidecarPath(f.in, l, markers...)
	if err != nil {
		return "", usagef("%v", err)
	}
	return out, nil
}

func extractWriteOptions(f extractFlags) srt.WriteOptions {
	opts := srt.WriteOptions{}
	if f.bomGiven {
		opts.BOM = &f.bom
	}
	if f.lineEndGiven {
		le := srt.LF
		if f.crlf {
			le = srt.CRLF
		}
		opts.LineEnding = &le
	}
	return opts
}

// buildSRT turns container cues into a subtitle file.
//
// This is the one place where the block payload stops being opaque, and it does
// exactly one thing to it: split it into lines. That is unavoidable — an SRT
// cue *is* a list of lines — but nothing else happens. No trimming, no escaping,
// no markup handling. A single trailing terminator is dropped because it is a
// muxer artefact rather than an empty last line, and because the writer would
// collapse it into the cue separator anyway.
//
// The line ending is taken from the cues rather than fixed at LF, which is the
// same rule internal/srt applies to a file it parsed. It matters: a great many
// muxers store cue text with CRLF, and writing LF between the cues while
// copying CRLF inside them produces a mixed-ending file. That is what ffmpeg's
// stream copy emits, and every later tool has to make the choice again.
func buildSRT(cues []mkv.Cue) (*srt.File, []string) {
	eol, warnings := cueLineEnding(cues)
	out := &srt.File{
		Cues:       make([]srt.Cue, 0, len(cues)),
		LineEnding: eol,
		// The blank line plus terminator that ends a conventional SRT.
		TrailingNewlines: 2,
	}
	for _, c := range cues {
		out.Cues = append(out.Cues, srt.Cue{
			Start: c.Start,
			End:   c.End,
			Lines: splitCueLines(c.Text),
		})
	}
	return out, warnings
}

// cueLineEnding is the terminator convention the container's cue text uses.
//
// Single-line cues contain no terminator and so contribute nothing; a file made
// entirely of them has no convention to detect and gets LF.
func cueLineEnding(cues []mkv.Cue) (srt.LineEnding, []string) {
	var crlf, lf, cr int
	for _, c := range cues {
		t := c.Text
		for i := 0; i < len(t); i++ {
			switch {
			case t[i] == '\r' && i+1 < len(t) && t[i+1] == '\n':
				crlf++
				i++
			case t[i] == '\r':
				cr++
			case t[i] == '\n':
				lf++
			}
		}
	}

	best, bestN, kinds := srt.LF, 0, 0
	for _, c := range []struct {
		e srt.LineEnding
		n int
	}{{srt.LF, lf}, {srt.CRLF, crlf}, {srt.CR, cr}} {
		if c.n == 0 {
			continue
		}
		kinds++
		if c.n > bestN {
			best, bestN = c.e, c.n
		}
	}
	if kinds > 1 {
		return best, []string{fmt.Sprintf("the container's cues mix line endings; writing everything with %s", best)}
	}
	return best, nil
}

// splitCueLines splits block text on any of the three line terminators, the
// same alphabet internal/srt accepts on input. Lines is non-nil even when
// empty, matching what the parser produces for an empty cue.
func splitCueLines(text string) []string {
	text = strings.TrimSuffix(text, "\n")
	text = strings.TrimSuffix(text, "\r")
	if text == "" {
		return []string{}
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	return strings.Split(text, "\n")
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func extractUsage(w io.Writer) {
	usageBlock(w,
		"Extract an embedded subtitle track from a Matroska container.\n\n"+
			"Usage: ypotitlo extract -i FILE [-il LANGUAGE] [flags]\n\n"+
			"Tracks are chosen by language, in the same spelling translate accepts:\n"+
			"-il el, -il ell, -il gre and -il greek all mean the same thing. With\n"+
			"one text track there is nothing to choose and -il can be left out.\n\n"+
			"Where a language has more than one track, the full one wins. -forced\n"+
			"and -sdh ask for the others by name; both also read the track's title,\n"+
			"since many files record \"SDH\" there and never set the flag.\n\n"+
			"The output filename is derived from the container and the track:\n"+
			"movie.mkv with an English track becomes movie.en.srt, and a forced or\n"+
			"hearing-impaired track keeps that marker — movie.en.forced.srt — so\n"+
			"that two tracks of one language cannot overwrite each other.\n\n"+
			"Only text tracks (S_TEXT/UTF8) can become an SRT. ASS and the bitmap\n"+
			"formats are named and refused rather than silently flattened.",
		[]flagSection{
			{Title: "Source", Flags: []flagDoc{
				{"-i FILE", "input video file (required)"},
				{"-il LANG", "language of the track to extract"},
				{"-forced", "take the forced track of that language"},
				{"-sdh", "take the hearing-impaired track of that language"},
				{"-list", "list the subtitle tracks and stop"},
			}},
			{Title: "Output", Flags: []flagDoc{
				{"-o FILE", "output file, or - for stdout; omit to derive it from -i"},
				{"-f", "overwrite an existing output file"},
				{"-bom / -crlf", "force a BOM or CRLF endings; omit to match the cues"},
				{"-q", "suppress the summary"},
			}},
		},
		[]string{
			"ypotitlo extract -i movie.mkv -list",
			"ypotitlo extract -i movie.mkv",
			"ypotitlo extract -i movie.mkv -il english -o movie.en.srt",
			"ypotitlo extract -i movie.mkv -il en -sdh",
		})
}
