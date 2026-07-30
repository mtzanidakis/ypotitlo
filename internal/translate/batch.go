package translate

import (
	"time"

	"github.com/mtzanidakis/ypotitlo/internal/srt"
)

// sceneGap is the silence between two consecutive cues that counts as a scene
// change. Cutting a batch there rather than every N cues means a batch almost
// never straddles a cut in the film, so the model is never asked to translate
// the second half of a conversation it cannot see the start of.
const sceneGap = 2 * time.Second

// contextCues is how many source cues on either side of a batch are shown to
// the model as read-only context.
//
// It has to be *source* cues. Carrying the previous batch's *translation*
// forward would give better consistency in principle and is what a serial
// implementation would do, but it serialises the whole run: batch 5 cannot
// start until batch 4 has come back. Source text is known up front, so ±3
// source cues costs about 10% more input tokens and stays embarrassingly
// parallel. Cross-boundary sentences, which is what the context is actually
// for, are visible either way.
const contextCues = 3

// batchRange is a half-open range of cue indices.
type batchRange struct{ start, end int }

// planBatches groups cues into batches of roughly size cues each, preferring to
// cut at a scene boundary.
//
// The size window is [0.6*size, 1.4*size]: a boundary is only honoured when it
// falls inside it, so one long silence in the middle of a reel cannot produce a
// three-cue batch (which wastes a whole call on three lines) or a hundred-cue
// one (which is where format compliance falls apart — the failure probability
// grows faster than linearly in the number of lines that have to stay aligned,
// and each failure costs a split).
func planBatches(cues []srt.Cue, size int) []batchRange {
	n := len(cues)
	if n == 0 {
		return nil
	}
	if size < 1 {
		size = defaultBatchSize
	}
	minSize := (size*6 + 9) / 10 // ceil(0.6*size)
	maxSize := size * 14 / 10
	if minSize < 1 {
		minSize = 1
	}
	if maxSize < size {
		maxSize = size
	}

	var out []batchRange
	for start := 0; start < n; {
		if n-start <= maxSize {
			out = append(out, batchRange{start, n})
			break
		}
		cut, found := start+size, false
		lo, hi := start+minSize, start+maxSize
		for j := lo; j <= hi; j++ {
			if !isBoundary(cues, j) {
				continue
			}
			if !found || abs(j-(start+size)) < abs(cut-(start+size)) {
				cut, found = j, true
			}
		}
		// Do not leave a stub behind: pull the cut back so the tail is still a
		// legal batch on its own.
		if n-cut < minSize && n-minSize >= lo {
			cut = n - minSize
		}
		out = append(out, batchRange{start, cut})
		start = cut
	}
	return out
}

// gapBefore is the silence between cue j-1 and cue j.
//
// Overlapping or out-of-order cues (both of which the srt reader preserves
// verbatim rather than "fixing") give a negative gap, which simply means "not a
// scene boundary" — exactly the right answer.
func gapBefore(cues []srt.Cue, j int) time.Duration {
	if j <= 0 || j >= len(cues) {
		return 0
	}
	return cues[j].Start - cues[j-1].End
}

func isBoundary(cues []srt.Cue, j int) bool { return gapBefore(cues, j) > sceneGap }

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
