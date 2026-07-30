package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mtzanidakis/ypotitlo/internal/srt"
)

const resumeSource = `1
00:00:01,000 --> 00:00:02,000
Hello there.

2
00:00:03,000 --> 00:00:04,000
How are you?

3
00:00:05,000 --> 00:00:06,000
Fine, thanks.
`

func writeTemp(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func parseString(t *testing.T, body string) *srt.File {
	t.Helper()
	f, err := srt.ParseBytes([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// A cue whose text still matches the source never got translated.
func TestPlanResumeFindsUntranslatedCues(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	in := writeTemp(t, dir, "movie.en.srt", resumeSource)
	out := writeTemp(t, dir, "movie.el.srt", strings.Replace(resumeSource, "How are you?", "Τι κάνεις;", 1))

	plan, err := planResume(in, out, "", parseString(t, resumeSource))
	if err != nil {
		t.Fatalf("planResume: %v", err)
	}
	if len(plan.missing) != 2 {
		t.Fatalf("missing = %d, want 2", len(plan.missing))
	}
	if got := []int{plan.indices[0], plan.indices[1]}; got[0] != 0 || got[1] != 2 {
		t.Errorf("indices = %v, want [0 2]", got)
	}
	if plan.missing[0].Lines[0] != "Hello there." {
		t.Errorf("first missing cue = %q", plan.missing[0].Lines[0])
	}
}

// Merging puts each translation back where it came from and leaves the cues
// that were already done alone.
func TestResumeMergePutsCuesBack(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	in := writeTemp(t, dir, "movie.en.srt", resumeSource)
	out := writeTemp(t, dir, "movie.el.srt", strings.Replace(resumeSource, "How are you?", "Τι κάνεις;", 1))

	plan, err := planResume(in, out, "", parseString(t, resumeSource))
	if err != nil {
		t.Fatal(err)
	}

	translated := make([]srt.Cue, len(plan.missing))
	copy(translated, plan.missing)
	translated[0].Lines = []string{"Γεια σου."}
	translated[1].Lines = []string{"Καλά, ευχαριστώ."}
	plan.merge(translated)

	want := []string{"Γεια σου.", "Τι κάνεις;", "Καλά, ευχαριστώ."}
	for i, w := range want {
		if got := plan.existing.Cues[i].Lines[0]; got != w {
			t.Errorf("cue %d = %q, want %q", i+1, got, w)
		}
	}
	// Timings must survive the merge untouched.
	src := parseString(t, resumeSource)
	for i := range src.Cues {
		if plan.existing.Cues[i].Start != src.Cues[i].Start || plan.existing.Cues[i].End != src.Cues[i].End {
			t.Errorf("cue %d timing changed", i+1)
		}
	}
}

// Merging by position into a differently-cut file would put translations
// against the wrong timings, which is worse than refusing.
func TestPlanResumeRefusesAMismatchedFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	in := writeTemp(t, dir, "movie.en.srt", resumeSource)

	t.Run("different cue count", func(t *testing.T) {
		out := writeTemp(t, dir, "short.el.srt", "1\n00:00:01,000 --> 00:00:02,000\nΓεια.\n")
		_, err := planResume(in, out, "", parseString(t, resumeSource))
		if err == nil || !strings.Contains(err.Error(), "not the same subtitle") {
			t.Errorf("err = %v, want a refusal about the files differing", err)
		}
	})

	t.Run("different timings", func(t *testing.T) {
		shifted := strings.Replace(resumeSource, "00:00:03,000", "00:00:09,000", 1)
		out := writeTemp(t, dir, "shifted.el.srt", shifted)
		_, err := planResume(in, out, "", parseString(t, resumeSource))
		if err == nil || !strings.Contains(err.Error(), "not the same subtitle") {
			t.Errorf("err = %v, want a refusal about the timings", err)
		}
	})
}

// A fully translated file has nothing left to do.
func TestPlanResumeOnACompleteFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	in := writeTemp(t, dir, "movie.en.srt", resumeSource)
	done := strings.NewReplacer(
		"Hello there.", "Γεια σου.",
		"How are you?", "Τι κάνεις;",
		"Fine, thanks.", "Καλά.",
	).Replace(resumeSource)
	out := writeTemp(t, dir, "movie.el.srt", done)

	plan, err := planResume(in, out, "", parseString(t, resumeSource))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.missing) != 0 {
		t.Errorf("missing = %d, want 0 on a finished file", len(plan.missing))
	}
}

// A missing previous output is a clear error, not a crash.
func TestPlanResumeWithoutAPreviousOutput(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	in := writeTemp(t, dir, "movie.en.srt", resumeSource)
	_, err := planResume(in, filepath.Join(dir, "absent.el.srt"), "", parseString(t, resumeSource))
	if err == nil || !strings.Contains(err.Error(), "previous output") {
		t.Errorf("err = %v, want it to name the missing previous output", err)
	}
}
