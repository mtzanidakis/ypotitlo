package translate

import (
	"strconv"
	"testing"
	"time"

	"github.com/mtzanidakis/ypotitlo/internal/srt"
)

// cuesWithGaps builds n cues one second long, with an extra silence before each
// index listed in gaps.
func cuesWithGaps(n int, gap time.Duration, gaps ...int) []srt.Cue {
	set := map[int]bool{}
	for _, g := range gaps {
		set[g] = true
	}
	out := make([]srt.Cue, n)
	var t time.Duration
	for i := range out {
		if set[i] {
			t += gap
		}
		out[i] = srt.Cue{
			Index: strconv.Itoa(i + 1),
			Start: t,
			End:   t + 900*time.Millisecond,
			Lines: []string{"line " + strconv.Itoa(i+1)},
		}
		t += time.Second
	}
	return out
}

func TestPlanBatches(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cues []srt.Cue
		size int
		want []batchRange
	}{
		{
			name: "empty",
			cues: nil, size: 20, want: nil,
		},
		{
			name: "fits in one batch",
			cues: cuesWithGaps(7, 0), size: 20,
			want: []batchRange{{0, 7}},
		},
		{
			// The last cut is pulled back from 20 to 19 so the tail is a legal
			// batch rather than a five-cue stub.
			name: "no scene boundaries falls back to the nominal size",
			cues: cuesWithGaps(25, 0), size: 10,
			want: []batchRange{{0, 10}, {10, 19}, {19, 25}},
		},
		{
			name: "cuts at a scene boundary inside the window",
			// A 5s silence before cue 12, with a nominal size of 10 and a
			// window of [6,14].
			cues: cuesWithGaps(30, 5*time.Second, 12), size: 10,
			want: []batchRange{{0, 12}, {12, 22}, {22, 30}},
		},
		{
			name: "ignores a boundary outside the window",
			// The silence is before cue 3, well below the 6-cue minimum.
			cues: cuesWithGaps(30, 5*time.Second, 3), size: 10,
			want: []batchRange{{0, 10}, {10, 20}, {20, 30}},
		},
		{
			name: "picks the boundary nearest the nominal size",
			cues: cuesWithGaps(40, 5*time.Second, 7, 11, 13), size: 10,
			want: []batchRange{{0, 11}, {11, 21}, {21, 31}, {31, 40}},
		},
		{
			name: "a sub-2s gap is not a boundary",
			cues: cuesWithGaps(30, 1500*time.Millisecond, 12), size: 10,
			want: []batchRange{{0, 10}, {10, 20}, {20, 30}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := planBatches(tc.cues, tc.size)
			if len(got) != len(tc.want) {
				t.Fatalf("batches = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("batch %d = %v, want %v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// Whatever the input, the batches must tile the file exactly and stay inside
// the size window (the final batch may be short only because the file ended).
func TestPlanBatchesInvariants(t *testing.T) {
	t.Parallel()

	sizes := []int{5, 20, 25}
	inputs := map[string][]srt.Cue{
		"flat":              cuesWithGaps(200, 0),
		"scene changes":     cuesWithGaps(200, 3*time.Second, 4, 9, 17, 40, 41, 90, 130, 131, 190),
		"all gaps":          cuesWithGaps(200, 3*time.Second, gapsEvery(200)...),
		"one cue":           cuesWithGaps(1, 0),
		"overlapping cues":  overlapping(50),
		"exactly one batch": cuesWithGaps(20, 0),
	}

	for name, cues := range inputs {
		for _, size := range sizes {
			t.Run(name+"/"+strconv.Itoa(size), func(t *testing.T) {
				t.Parallel()

				got := planBatches(cues, size)
				prev := 0
				for i, br := range got {
					if br.start != prev {
						t.Fatalf("batch %d starts at %d, want %d: batches must tile the file", i, br.start, prev)
					}
					if br.end <= br.start {
						t.Fatalf("batch %d is empty: %v", i, br)
					}
					n := br.end - br.start
					last := i == len(got)-1
					if n > size*14/10 {
						t.Errorf("batch %d holds %d cues, above the 1.4x window for size %d", i, n, size)
					}
					if !last && n < (size*6+9)/10 {
						t.Errorf("batch %d holds %d cues, below the 0.6x window for size %d", i, n, size)
					}
					prev = br.end
				}
				if prev != len(cues) {
					t.Fatalf("batches cover %d of %d cues", prev, len(cues))
				}
			})
		}
	}
}

func gapsEvery(n int) []int {
	out := make([]int, 0, n)
	for i := range n {
		out = append(out, i)
	}
	return out
}

// overlapping builds cues whose timings run backwards, which the srt reader
// preserves rather than repairing. A negative gap is simply not a boundary.
func overlapping(n int) []srt.Cue {
	out := make([]srt.Cue, n)
	for i := range out {
		out[i] = srt.Cue{
			Index: strconv.Itoa(i + 1),
			Start: time.Duration(n-i) * time.Second,
			End:   time.Duration(n-i)*time.Second + 5*time.Second,
			Lines: []string{"x"},
		}
	}
	return out
}

func TestGapBefore(t *testing.T) {
	t.Parallel()

	cues := cuesWithGaps(3, 4*time.Second, 2)
	tests := []struct {
		j    int
		want time.Duration
	}{
		{0, 0},
		{1, 100 * time.Millisecond},
		{2, 4100 * time.Millisecond},
		{3, 0},
	}
	for _, tc := range tests {
		if got := gapBefore(cues, tc.j); got != tc.want {
			t.Errorf("gapBefore(%d) = %v, want %v", tc.j, got, tc.want)
		}
	}
}
