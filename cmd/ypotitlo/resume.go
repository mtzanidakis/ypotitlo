package main

import (
	"fmt"
	"os"
	"slices"

	"github.com/mtzanidakis/ypotitlo/internal/charset"
	"github.com/mtzanidakis/ypotitlo/internal/srt"
)

// resumePlan is what is left to do when continuing a partly translated file.
type resumePlan struct {
	existing *srt.File // the previous output, to be filled in and rewritten
	missing  []srt.Cue // the cues still in the source language
	indices  []int     // where each missing cue sits in the file
}

// planResume reads a previous output and works out which cues never got
// translated.
//
// It needs no state file because the guarantee the translator already makes is
// enough: the output has the same cues in the same order with the same timings
// as its input, and a cue that could not be translated keeps its original text.
// So a cue whose text still equals the source is one that never made it.
//
// The known imprecision, worth stating because it never goes away: a line that
// legitimately translates to itself — a number, a name, "OK", a ♪ marker — is
// indistinguishable from an untranslated one and will be re-sent on every
// resume. That costs a little and changes nothing, but it does mean a resume
// never reports zero remaining.
func planResume(inPath, outPath, charsetName string, in *srt.File) (*resumePlan, error) {
	raw, err := os.ReadFile(outPath)
	if err != nil {
		return nil, fmt.Errorf("reading the previous output: %w", err)
	}
	dec, err := charset.Decode(raw, charsetName)
	if err != nil {
		return nil, &parseError{fmt.Errorf("%s: %w", outPath, err)}
	}
	existing, err := srt.ParseBytes(dec.Text)
	if err != nil {
		return nil, &parseError{fmt.Errorf("%s: %w", outPath, err)}
	}

	// Refuse to guess if the two files are not the same subtitle. Merging by
	// position into a file that has been re-cut would silently put translated
	// lines against the wrong timings, which is worse than any error.
	if len(existing.Cues) != len(in.Cues) {
		return nil, usagef("%q has %d cues and %q has %d; they are not the same subtitle",
			outPath, len(existing.Cues), inPath, len(in.Cues))
	}
	for i := range in.Cues {
		if existing.Cues[i].Start != in.Cues[i].Start || existing.Cues[i].End != in.Cues[i].End {
			return nil, usagef("%q and %q disagree on the timing of cue %d; they are not the same subtitle",
				outPath, inPath, i+1)
		}
	}

	plan := &resumePlan{existing: existing}
	for i := range in.Cues {
		if slices.Equal(existing.Cues[i].Lines, in.Cues[i].Lines) {
			plan.missing = append(plan.missing, in.Cues[i])
			plan.indices = append(plan.indices, i)
		}
	}
	return plan, nil
}

// merge folds freshly translated cues back into their places.
func (p *resumePlan) merge(translated []srt.Cue) {
	for i, idx := range p.indices {
		if i < len(translated) {
			p.existing.Cues[idx].Lines = translated[i].Lines
		}
	}
}
