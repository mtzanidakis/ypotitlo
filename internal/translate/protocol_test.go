package translate

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/mtzanidakis/ypotitlo/internal/llm"
)

func TestParseJSONL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		reply       string
		wantIDs     []int
		wantSkipped int
		check       func(t *testing.T, got map[int]entry)
	}{
		{
			name:    "plain",
			reply:   `{"i":1,"n":1,"t":["a"],"s":"ok"}` + "\n" + `{"i":2,"n":1,"t":["b"],"s":"ok"}`,
			wantIDs: []int{1, 2},
		},
		{
			name:    "markdown fence",
			reply:   "```json\n" + `{"i":1,"n":1,"t":["a"],"s":"ok"}` + "\n```",
			wantIDs: []int{1},
			// The two fence lines are not JSON objects, so they are skipped.
			wantSkipped: 2,
		},
		{
			name:        "preamble and chatter",
			reply:       "Here you go:\n" + `{"i":1,"n":1,"t":["a"],"s":"ok"}` + "\nHope that helps!",
			wantIDs:     []int{1},
			wantSkipped: 2,
		},
		{
			name:    "crlf",
			reply:   `{"i":1,"n":1,"t":["a"],"s":"ok"}` + "\r\n" + `{"i":2,"n":1,"t":["b"],"s":"ok"}` + "\r\n",
			wantIDs: []int{1, 2},
		},
		{
			name:        "one malformed object costs one cue",
			reply:       `{"i":1,"n":1,"t":["a"],"s":"ok"}` + "\n" + `{"i":2,"n":1,"t":["b"` + "\n" + `{"i":3,"n":1,"t":["c"],"s":"ok"}`,
			wantIDs:     []int{1, 3},
			wantSkipped: 1,
		},
		{
			name:        "object without an id",
			reply:       `{"note":"translating now"}` + "\n" + `{"i":1,"n":1,"t":["a"],"s":"ok"}`,
			wantIDs:     []int{1},
			wantSkipped: 1,
		},
		{
			name:        "zero and negative ids are not cues",
			reply:       `{"i":0,"n":1,"t":["a"],"s":"ok"}` + "\n" + `{"i":-3,"n":1,"t":["b"],"s":"ok"}`,
			wantIDs:     nil,
			wantSkipped: 2,
		},
		{
			name:    "duplicate id keeps the first",
			reply:   `{"i":1,"n":1,"t":["first"],"s":"ok"}` + "\n" + `{"i":1,"n":1,"t":["second"],"s":"ok"}`,
			wantIDs: []int{1},
			check: func(t *testing.T, got map[int]entry) {
				if got[1].T[0] != "first" {
					t.Errorf("kept %q, want the first", got[1].T[0])
				}
			},
		},
		{
			name:    "t as a bare string",
			reply:   `{"i":1,"n":1,"t":"one line","s":"ok"}`,
			wantIDs: []int{1},
			check: func(t *testing.T, got map[int]entry) {
				if !slices.Equal([]string(got[1].T), []string{"one line"}) {
					t.Errorf("t = %q", got[1].T)
				}
			},
		},
		{
			name:    "t as a string with newlines",
			reply:   `{"i":1,"n":2,"t":"first\nsecond","s":"ok"}`,
			wantIDs: []int{1},
			check: func(t *testing.T, got map[int]entry) {
				if !slices.Equal([]string(got[1].T), []string{"first", "second"}) {
					t.Errorf("t = %q", got[1].T)
				}
			},
		},
		{
			name:    "a JSON array is not JSON lines",
			reply:   `[{"i":1,"n":1,"t":["a"],"s":"ok"}]`,
			wantIDs: nil, wantSkipped: 1,
		},
		{
			name:    "empty reply",
			reply:   "\n\n   \n",
			wantIDs: nil,
		},
		{
			name:    "missing s is treated as ok",
			reply:   `{"i":1,"n":1,"t":["a"]}`,
			wantIDs: []int{1},
			check: func(t *testing.T, got map[int]entry) {
				if got[1].refused() {
					t.Error("an entry with no status must not read as refused")
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, skipped := parseJSONL(tc.reply)
			ids := make([]int, 0, len(got))
			for id := range got {
				ids = append(ids, id)
			}
			slices.Sort(ids)
			if !slices.Equal(ids, tc.wantIDs) {
				t.Errorf("ids = %v, want %v", ids, tc.wantIDs)
			}
			if skipped != tc.wantSkipped {
				t.Errorf("skipped = %d, want %d", skipped, tc.wantSkipped)
			}
			if tc.check != nil {
				tc.check(t, got)
			}
		})
	}
}

func TestEntryStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		e       entry
		refused bool
		blank   bool
	}{
		{"ok", entry{I: 1, T: jsonl{"a"}, S: "ok"}, false, false},
		{"refused", entry{I: 1, S: "refused"}, true, true},
		{"refused uppercase", entry{I: 1, S: "REFUSED"}, true, true},
		{"refusal", entry{I: 1, S: " refusal "}, true, true},
		{"empty strings", entry{I: 1, T: jsonl{"", "  "}, S: "ok"}, false, true},
		{"one empty line of two", entry{I: 1, T: jsonl{"", "b"}, S: "ok"}, false, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := tc.e.refused(); got != tc.refused {
				t.Errorf("refused = %v, want %v", got, tc.refused)
			}
			if got := tc.e.blank(); got != tc.blank {
				t.Errorf("blank = %v, want %v", got, tc.blank)
			}
		})
	}
}

func TestLooksRefusal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		reply string
		want  bool
	}{
		{"apology", "I'm sorry, but I can't translate this content.", true},
		{"cannot", "I cannot assist with that request.", true},
		{"policy", "As an AI, I must decline.", true},
		{"ordinary failure", "Error: upstream timeout", false},
		{"empty", "", false},
		// A refusal marker buried in a long reply is dialogue, not a refusal;
		// looksRefusal is only consulted when nothing parsed at all.
		{"late marker", strings.Repeat("x", 900) + "i'm sorry", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := looksRefusal(tc.reply); got != tc.want {
				t.Errorf("looksRefusal = %v, want %v", got, tc.want)
			}
		})
	}
}

// The renumbering is per batch, so the model never sees a large, gapped or
// duplicated SRT index.
func TestBatchLocalIDs(t *testing.T) {
	t.Parallel()

	var warns []string
	r := newRunner(opts(nil, &warns))
	cues := makeCues(40)
	cues[10].Index = "0"
	cues[11].Index = "9999"

	prep := r.prepare(cues, batchRange{start: 10, end: 20})
	for i, p := range prep {
		if p.id != i+1 {
			t.Errorf("cue %d got id %d, want %d", p.idx, p.id, i+1)
		}
		if p.idx != 10+i {
			t.Errorf("prepared[%d].idx = %d, want %d", i, p.idx, 10+i)
		}
	}
}

// An id the request did not ask for is dropped. The model is shown context
// cues it must not emit, and this is what makes "must not" enforceable.
func TestUnrequestedIDsAreIgnored(t *testing.T) {
	t.Parallel()

	in := makeCues(3)
	p := &fakeProvider{fn: func(req llm.Request, _ int) (llm.Response, error) {
		body := reply(req, prefix("EL:"))
		return llm.Response{Content: body + jsonLine(99, 1, []string{"context cue"}, "ok") + "\n"}, nil
	}}
	var warns []string
	res, err := Run(context.Background(), in, opts(p, &warns))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertShape(t, in, res.Cues)
	for i, c := range res.Cues {
		if want := fmt.Sprintf("EL:line %d", i+1); c.Lines[0] != want {
			t.Errorf("cue %d: %q, want %q", i, c.Lines[0], want)
		}
	}
}

// A "t" that is neither a string nor a list of strings costs that one line.
func TestUnusableTFieldSkipsTheLine(t *testing.T) {
	t.Parallel()

	got, skipped := parseJSONL(`{"i":1,"n":1,"t":42,"s":"ok"}` + "\n" + `{"i":2,"n":1,"t":["b"],"s":"ok"}`)
	if len(got) != 1 || skipped != 1 {
		t.Fatalf("entries = %v, skipped = %d, want one of each", got, skipped)
	}
	if _, ok := got[2]; !ok {
		t.Error("the usable entry was dropped along with the broken one")
	}
}
