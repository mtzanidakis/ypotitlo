package translate

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/mtzanidakis/ypotitlo/internal/llm"
	"github.com/mtzanidakis/ypotitlo/internal/srt"
)

// textCues turns lines of dialogue into cues, one line per cue.
func textCues(lines ...string) []srt.Cue {
	out := make([]srt.Cue, len(lines))
	for i, l := range lines {
		out[i] = cue(strconv.Itoa(i+1), i*2000, i*2000+1800, l)
	}
	return out
}

// repeatCues makes n cues of the same sentence, which is enough to satisfy the
// "enough substantial cues" test without saying anything a stopword list knows.
func repeatCues(n int, line string) []srt.Cue {
	lines := make([]string, n)
	for i := range lines {
		lines[i] = line
	}
	return textCues(lines...)
}

func TestDetectLanguageFromFilename(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		filename string
		want     string
		ok       bool
	}{
		{"two letter code", "movie.en.srt", "en", true},
		{"three letter code", "movie.eng.srt", "en", true},
		{"bibliographic code", "movie.gre.srt", "el", true},
		{"marker after the code", "movie.eng.sdh.srt", "en", true},
		{"two markers", "film.es.forced.srt", "es", true},
		{"full path", "/media/films/Sirat (2025)/Sirat.1080p.en.srt", "en", true},
		{"uppercase", "movie.EN.srt", "en", true},
		// "sdh" parses as Southern Kurdish and "cc" as Atsam. Reading either as
		// the source language would poison every prompt in the run.
		{"sdh alone is not a language", "movie.sdh.srt", "", false},
		{"cc alone is not a language", "movie.cc.srt", "", false},
		{"no language segment", "movie.srt", "", false},
		{"year is not a language", "movie.2024.srt", "", false},
		{"bare name", "movie", "", false},
		{"stdin", "-", "", false},
		{"empty", "", "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, ok := langFromFilename(tc.filename)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v (got %v)", ok, tc.ok, got)
			}
			if ok && got.Code != tc.want {
				t.Errorf("code = %q, want %q", got.Code, tc.want)
			}
		})
	}
}

// The filename wins outright, before any text is looked at and before any call.
func TestDetectLanguagePrefersTheFilename(t *testing.T) {
	t.Parallel()

	p := echoing(prefix("EL:"))
	var warns []string
	got, from, err := DetectLanguage(context.Background(),
		textCues("Καλημέρα σε όλους", "Πώς είσαι σήμερα;"), "movie.en.srt", opts(p, &warns))
	if err != nil {
		t.Fatalf("DetectLanguage: %v", err)
	}
	if got.Code != "en" || from != FromFilename {
		t.Errorf("got %q from %q, want en from %q", got.Code, from, FromFilename)
	}
	if p.count() != 0 {
		t.Errorf("provider calls = %d, want 0", p.count())
	}
}

func TestDetectLanguageFromScript(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		text []string
		want string
	}{
		{"greek", []string{"Καλημέρα σε όλους.", "Πώς είσαι σήμερα;", "Δεν ξέρω τι να πω."}, "el"},
		{"russian", []string{"Что ты здесь делаешь?", "Я не знаю, что сказать.", "Это было давно."}, "ru"},
		{"ukrainian", []string{"Що ти тут робиш?", "Я не знаю, що сказати.", "Це було давно, її брат."}, "uk"},
		{"serbian", []string{"Шта радиш овде?", "Не знам шта да кажем, ђаво.", "Њега нема."}, "sr"},
		{"bulgarian", []string{"Какво правиш тук?", "Не знам какво да кажа.", "Аз съм тъжен и ъгълът е тъмен."}, "bg"},
		{"arabic", []string{"ماذا تفعل هنا؟", "لا أعرف ماذا أقول.", "كان ذلك منذ زمن بعيد."}, "ar"},
		{"persian", []string{"چه کار می\u200cکنی؟", "نمی\u200cدانم چه بگویم.", "خیلی وقت پیش بود."}, "fa"},
		{"hebrew", []string{"מה אתה עושה כאן?", "אני לא יודע מה לומר.", "זה היה מזמן."}, "he"},
		{"japanese", []string{"何をしているの？", "わからない。", "昔のことだ。"}, "ja"},
		{"korean", []string{"여기서 뭐 하는 거야?", "모르겠어요.", "오래전 일이야."}, "ko"},
		{"simplified chinese", []string{"你在这里做什么？", "我不知道说什么。", "这是很久以前的事了。"}, "zh-Hans"},
		{"traditional chinese", []string{"你在這裡做什麼？", "我不知道說什麼。", "這是很久以前的事了。"}, "zh-Hant"},
		{"thai", []string{"คุณทำอะไรอยู่ที่นี่", "ฉันไม่รู้จะพูดอะไร"}, "th"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p := echoing(prefix("x"))
			var warns []string
			got, from, err := DetectLanguage(context.Background(), textCues(tc.text...), "movie.srt", opts(p, &warns))
			if err != nil {
				t.Fatalf("DetectLanguage: %v", err)
			}
			if got.Code != tc.want {
				t.Errorf("code = %q, want %q", got.Code, tc.want)
			}
			if from != FromScript {
				t.Errorf("provenance = %q, want %q", from, FromScript)
			}
			if p.count() != 0 {
				t.Errorf("provider calls = %d, want 0: the script settles this for free", p.count())
			}
		})
	}
}

// A Greek file peppered with Latin release tags and English proper nouns is
// still Greek. The share threshold is well below a half for exactly this.
func TestDetectLanguageScriptWithLatinNoise(t *testing.T) {
	t.Parallel()

	cues := textCues(
		"Ripped by WORLD-GROUP 1080p WEBRip x264",
		"Καλημέρα, τι κάνεις σήμερα;",
		"Ο John Smith ήρθε από το Los Angeles.",
		"Δεν ξέρω τι να σου πω για αυτό.",
	)
	var warns []string
	got, from, err := DetectLanguage(context.Background(), cues, "movie.srt", opts(nil, &warns))
	if err != nil {
		t.Fatalf("DetectLanguage: %v", err)
	}
	if got.Code != "el" || from != FromScript {
		t.Errorf("got %q from %q, want el from script", got.Code, from)
	}
}

func TestDetectLanguageFromStopwords(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		text string
		want string
	}{
		{"english", "What are you doing here? I don't know what to say about all of this. The man with the gun was not there when they came for you, and that is all I have to tell you about it. I told them that you were not here and they did not believe me at all.", "en"},
		{"spanish", "¿Qué haces aquí? No sé qué decir de todo esto. El hombre con la pistola no estaba allí cuando vinieron por ti, y eso es todo lo que tengo para decirte de esto. Les dije que no estabas aquí y no me creyeron para nada.", "es"},
		{"french", "Que fais-tu ici ? Je ne sais pas quoi dire de tout ça. L'homme avec le pistolet n'était pas là quand ils sont venus pour toi, et c'est tout ce que j'ai à te dire. Je leur ai dit que tu n'étais pas là et ils ne m'ont pas cru.", "fr"},
		{"german", "Was machst du hier? Ich weiß nicht, was ich dazu sagen soll. Der Mann mit der Waffe war nicht da, als sie für dich kamen, und das ist alles, was ich dir sagen kann. Ich habe ihnen gesagt, dass du nicht hier bist und sie haben mir nicht geglaubt.", "de"},
		{"italian", "Che cosa fai qui? Non so cosa dire di tutto questo. L'uomo con la pistola non era lì quando sono venuti per te, e questo è tutto quello che ho da dirti. Ho detto loro che non eri qui e non mi hanno creduto per niente.", "it"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p := echoing(prefix("x"))
			var warns []string
			got, from, err := DetectLanguage(context.Background(),
				textCues(strings.Split(tc.text, ". ")...), "movie.srt", opts(p, &warns))
			if err != nil {
				t.Fatalf("DetectLanguage: %v", err)
			}
			if got.Code != tc.want || from != FromStopwords {
				t.Errorf("got %q from %q, want %q from %q", got.Code, from, tc.want, FromStopwords)
			}
			if p.count() != 0 {
				t.Errorf("provider calls = %d, want 0", p.count())
			}
		})
	}
}

func TestStopwordLanguageRefusesToGuess(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		text string
	}{
		{"too short", "What are you doing"},
		{"no function words", strings.Repeat("zxqv wrtp mnbk ", 30)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got, ok := stopwordLanguage(tc.text); ok {
				t.Errorf("guessed %q from %q", got, tc.text)
			}
		})
	}
}

// The empty file is the one case that must never reach the model.
func TestDetectLanguageEmptyFileMakesNoCalls(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cues []srt.Cue
	}{
		{"nil", nil},
		{"no cues", []srt.Cue{}},
		{"cues with no text", []srt.Cue{cue("1", 0, 1000), cue("2", 1000, 2000, "  ")}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p := echoing(prefix("x"))
			var warns []string
			_, from, err := DetectLanguage(context.Background(), tc.cues, "movie.srt", opts(p, &warns))
			if !errors.Is(err, ErrUndetermined) {
				t.Fatalf("err = %v, want ErrUndetermined", err)
			}
			if from != "" {
				t.Errorf("provenance = %q, want empty", from)
			}
			if p.count() != 0 {
				t.Errorf("provider calls = %d, want 0", p.count())
			}
		})
	}
}

// Too little dialogue to be worth a call.
func TestDetectLanguageTooFewCuesForTheModel(t *testing.T) {
	t.Parallel()

	p := echoing(prefix("x"))
	var warns []string
	_, _, err := DetectLanguage(context.Background(),
		repeatCues(minModelCues-1, "zxqv wrtp mnbk"), "movie.srt", opts(p, &warns))
	if !errors.Is(err, ErrUndetermined) {
		t.Fatalf("err = %v, want ErrUndetermined", err)
	}
	if p.count() != 0 {
		t.Errorf("provider calls = %d, want 0", p.count())
	}
}

func TestDetectLanguageNoProviderIsNotAnError(t *testing.T) {
	t.Parallel()

	var warns []string
	o := opts(nil, &warns)
	_, _, err := DetectLanguage(context.Background(), repeatCues(40, "zxqv wrtp mnbk"), "movie.srt", o)
	if !errors.Is(err, ErrUndetermined) {
		t.Fatalf("err = %v, want ErrUndetermined", err)
	}
}

// The model is the last resort, it is asked once, and it is asked about the
// middle of the file.
func TestDetectLanguageFromModel(t *testing.T) {
	t.Parallel()

	var cues []srt.Cue
	for i := range 40 {
		text := fmt.Sprintf("zxqv wrtp mnbk %d", i)
		if i < 5 {
			text = "OPENING CREDITS RELEASE GROUP"
		}
		cues = append(cues, cue(strconv.Itoa(i+1), i*2000, i*2000+1500, text))
	}

	p := &fakeProvider{fn: func(_ llm.Request, _ int) (llm.Response, error) {
		return llm.Response{Content: `{"code":"fr"}`}, nil
	}}
	var warns []string
	got, from, err := DetectLanguage(context.Background(), cues, "movie.srt", opts(p, &warns))
	if err != nil {
		t.Fatalf("DetectLanguage: %v", err)
	}
	if got.Code != "fr" || from != FromModel {
		t.Errorf("got %q from %q, want fr from %q", got.Code, from, FromModel)
	}
	if p.count() != 1 {
		t.Fatalf("provider calls = %d, want 1", p.count())
	}

	req := p.requests()[0]
	msg := userMessage(req)
	if strings.Contains(msg, "OPENING CREDITS") {
		t.Error("the sample was taken from the head of the file, which is credits, not dialogue")
	}
	if !strings.Contains(msg, "zxqv wrtp mnbk 20") {
		t.Errorf("the sample was not taken from the middle:\n%s", msg)
	}
	if req.Stage != "detect" {
		t.Errorf("stage = %q, want detect", req.Stage)
	}
}

func TestDetectLanguageModelAnswersNonsense(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{fn: func(_ llm.Request, _ int) (llm.Response, error) {
		return llm.Response{Content: `{"code":"klingon"}`}, nil
	}}
	var warns []string
	_, _, err := DetectLanguage(context.Background(), repeatCues(40, "zxqv wrtp mnbk"), "movie.srt", opts(p, &warns))
	if !errors.Is(err, ErrUndetermined) {
		t.Fatalf("err = %v, want ErrUndetermined", err)
	}
}

func TestModelSampleTakesTheMiddle(t *testing.T) {
	t.Parallel()

	var cues []srt.Cue
	for i := range 100 {
		cues = append(cues, cue(strconv.Itoa(i+1), i*1000, i*1000+900, fmt.Sprintf("cue number %d here", i)))
	}
	sample, n := modelSample(cues)
	if n != 100 {
		t.Errorf("counted %d substantial cues, want 100", n)
	}
	lines := strings.Split(sample, "\n")
	if len(lines) != 20 {
		t.Fatalf("sample has %d lines, want 20", len(lines))
	}
	if !strings.HasPrefix(lines[0], "cue number 40 ") {
		t.Errorf("sample starts at %q, want the middle", lines[0])
	}
}

func TestScriptLanguageIgnoresPunctuationOnlyText(t *testing.T) {
	t.Parallel()

	if got, ok := scriptLanguage("... --- ??? 123 !!!"); ok {
		t.Errorf("scriptLanguage = %q from text with no letters", got)
	}
}
