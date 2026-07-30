package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mtzanidakis/ypotitlo/internal/charset"
	"github.com/mtzanidakis/ypotitlo/internal/config"
	"github.com/mtzanidakis/ypotitlo/internal/lang"
	"github.com/mtzanidakis/ypotitlo/internal/llm"
	"github.com/mtzanidakis/ypotitlo/internal/srt"
	"github.com/mtzanidakis/ypotitlo/internal/translate"
)

// stdinPath is the conventional sentinel for "read from stdin" / "write to
// stdout". It is a legitimate value for -i and -o and must survive the check
// that rejects flag-looking values.
const stdinPath = "-"

type translateFlags struct {
	in, out       string
	sourceLang    string
	targetLang    string
	charsetName   string
	outCharset    string
	model         string
	baseURL       string
	apiKey        string
	configPath    string
	concurrency   int
	batchSize     int
	budget        float64
	timeout       time.Duration
	dryRun        bool
	force         bool
	quiet         bool
	verbose       bool
	keepIndices   bool
	noBrief       bool
	bom           bool
	crlf          bool
	bomGiven      bool
	lineEndGiven  bool
	budgetGiven   bool
	batchGiven    bool
	concurGiven   bool
	modelGiven    bool
	baseURLGiven  bool
	targetLangSet bool
}

func cmdTranslate(ctx context.Context, e env, args []string) error {
	fs := newFlagSet("translate")
	var f translateFlags

	fs.StringVar(&f.in, "i", "", "input subtitle file, or - for stdin")
	fs.StringVar(&f.sourceLang, "il", "", "source language (default: detect)")
	fs.StringVar(&f.targetLang, "ol", "", "target language")
	fs.StringVar(&f.out, "o", "", "output file, or - for stdout")
	fs.StringVar(&f.charsetName, "charset", "", "input character set (default: detect)")
	fs.StringVar(&f.outCharset, "output-charset", "", "output character set (default: utf-8)")
	fs.StringVar(&f.model, "m", "", "model id")
	fs.StringVar(&f.baseURL, "base-url", "", "API base URL")
	fs.StringVar(&f.apiKey, "api-key", "", "API key")
	fs.StringVar(&f.configPath, "config", "", "configuration file path")
	fs.IntVar(&f.concurrency, "j", 0, "concurrent requests")
	fs.IntVar(&f.batchSize, "b", 0, "cues per request")
	fs.Float64Var(&f.budget, "budget", 0, "spend ceiling for this run, in USD")
	fs.DurationVar(&f.timeout, "timeout", 30*time.Minute, "overall deadline")
	fs.BoolVar(&f.dryRun, "n", false, "report what would happen and make no API calls")
	fs.BoolVar(&f.force, "f", false, "overwrite an existing output file")
	fs.BoolVar(&f.quiet, "q", false, "suppress progress")
	fs.BoolVar(&f.verbose, "v", false, "explain what was detected and why")
	fs.BoolVar(&f.keepIndices, "keep-indices", false, "keep the input's cue numbering")
	fs.BoolVar(&f.noBrief, "no-brief", false, "skip the whole-file consistency pass")
	fs.BoolVar(&f.bom, "bom", false, "write a UTF-8 BOM (default: as the input had)")
	fs.BoolVar(&f.crlf, "crlf", false, "write CRLF line endings (default: as the input had)")

	if err := parseFlags(fs, args, "i", "o"); err != nil {
		if errors.Is(err, flagErrHelp) {
			translateUsage(e.Stdout)
			return nil
		}
		return err
	}

	// Presence, not value: a plain bool cannot tell -bom=false from an absent
	// -bom, so a config file saying true would be overridden on every run.
	f.bomGiven = wasGiven(fs, "bom")
	f.lineEndGiven = wasGiven(fs, "crlf")
	f.budgetGiven = wasGiven(fs, "budget")
	f.batchGiven = wasGiven(fs, "b")
	f.concurGiven = wasGiven(fs, "j")
	f.modelGiven = wasGiven(fs, "m")
	f.baseURLGiven = wasGiven(fs, "base-url")
	f.targetLangSet = wasGiven(fs, "ol")

	if f.in == "" {
		return usagef("-i is required (use -i - to read stdin)")
	}
	return runTranslate(ctx, e, f)
}

func runTranslate(ctx context.Context, e env, f translateFlags) error {
	o := configOptions(e, f.configPath)
	cfg, _, err := config.Load(o)
	if err != nil {
		return err
	}
	applyFlagOverrides(&cfg, f)

	target, err := resolveTarget(cfg, f)
	if err != nil {
		return err
	}

	// ---- read and decode -------------------------------------------------
	raw, err := readInput(e, f.in)
	if err != nil {
		return err
	}
	dec, err := charset.Decode(raw, f.charsetName)
	if err != nil {
		return &parseError{err}
	}
	for _, w := range dec.Warnings {
		warnf(e, "%s", w)
	}
	if f.verbose {
		outf(e.Stderr, "input encoding: %s\n", dec.Encoding)
	}

	file, err := srt.ParseBytes(dec.Text)
	if err != nil {
		return &parseError{err}
	}
	for _, w := range file.Warnings {
		warnf(e, "%s", w)
	}

	// ---- decide where the output goes, and refuse to clobber -------------
	outPath, err := resolveOutputPath(f, target)
	if err != nil {
		return err
	}
	if err := guardOutput(f, outPath); err != nil {
		return err
	}

	// ---- an empty file is a no-op, not an error and not a call -----------
	if len(file.Cues) == 0 {
		warnf(e, "%s contains no cues; writing it through unchanged", displayPath(f.in))
		if f.dryRun {
			outf(e.Stderr, "dry run: nothing to translate\n")
			return nil
		}
		return writeOutput(e, file, f, outPath)
	}

	provider, keySource, err := buildProvider(cfg, f, e)
	if err != nil && !f.dryRun {
		return err
	}

	// The status line is drawn only for an interactive run: a pipe, a log file
	// or -q gets clean output instead of a stream of carriage returns.
	ui := newProgressUI(e.Stderr, !f.quiet && !f.dryRun && isTerminal(e.Stderr), e.Now)
	defer ui.Stop()

	// Warnings go through the UI so they never land on top of the spinner.
	warn := func(format string, a ...any) {
		ui.Suspend(func() { warnf(e, format, a...) })
	}

	opts := translate.Options{
		Provider:    provider,
		Model:       cfg.Model,
		Target:      target,
		BatchSize:   cfg.BatchSize,
		Concurrency: cfg.Concurrency,
		Brief:       !f.noBrief,
		Rand:        rand.New(rand.NewSource(e.Now().UnixNano())),
		Warn:        warn,
		Phase:       ui.Phase,
		Progress:    ui.Progress,
	}

	// The overall deadline covers language detection too. It was created after
	// it before, leaving one provider call bounded only by the HTTP client's own
	// timeout times its retries — outside -timeout, outside the watchdog and
	// outside the call fuse.
	ctx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()

	// ---- source language --------------------------------------------------
	ui.Start()
	ui.Phase("detecting language")
	source, provenance, err := resolveSource(ctx, e, file.Cues, f, opts)
	if err != nil {
		return err
	}
	opts.Source = source

	// One line naming both ends of the translation, so a mistaken target is
	// obvious at a glance rather than only after the file is written. It always
	// prints: the target is worth confirming even when the source could not be
	// worked out, and especially when the target came from the config file
	// rather than the command line.
	// Skipped for a dry run, whose report already states both ends in its table.
	if !f.dryRun {
		ui.Suspend(func() {
			outf(e.Stderr, "translating %s -> %s\n", describeSource(source, provenance), target.English)
		})
	}
	if !source.Zero() && source.Code == target.Code && !f.force {
		return usagef("source and target are both %s; pass -f to translate anyway", target.Code)
	}

	if f.dryRun {
		return reportDryRun(e, file, f, cfg, source, target, outPath)
	}

	// ---- translate --------------------------------------------------------
	res, err := translate.Run(ctx, file.Cues, opts)
	if err != nil {
		// A failed run has usually already spent something. Reporting the error
		// without the accounting leaves no way to tell a run that failed on its
		// first call from one that failed after forty.
		failed := ui.Elapsed()
		ui.Stop()
		writeSpend(e, res.Stats, failed, f)
		return classifyRunError(err, keySource)
	}
	file.Cues = res.Cues

	elapsed := ui.Elapsed()
	ui.Stop()

	if err := writeOutput(e, file, f, outPath); err != nil {
		return err
	}

	writeFooter(e, res.Stats, outPath, f, elapsed)
	return nil
}

// applyFlagOverrides folds the command line onto the loaded config. Only flags
// actually given win, so an unset flag never silently replaces a stored value.
func applyFlagOverrides(cfg *config.Config, f translateFlags) {
	if f.modelGiven {
		cfg.Model = f.model
	}
	if f.baseURLGiven {
		cfg.BaseURL = f.baseURL
	}
	if f.batchGiven {
		cfg.BatchSize = f.batchSize
	}
	if f.concurGiven {
		cfg.Concurrency = f.concurrency
	}
	if f.budgetGiven {
		cfg.MaxSpendUSD = f.budget
	}
}

func resolveTarget(cfg config.Config, f translateFlags) (lang.Lang, error) {
	name := f.targetLang
	if !f.targetLangSet {
		name = cfg.TargetLanguage
	}
	if strings.TrimSpace(name) == "" {
		return lang.Lang{}, usagef("-ol is required (or set target_language in the config)")
	}
	l, err := lang.Resolve(name)
	if err != nil {
		return lang.Lang{}, usagef("%v", err)
	}
	return l, nil
}

func resolveOutputPath(f translateFlags, target lang.Lang) (string, error) {
	if f.out != "" {
		return f.out, nil
	}
	if f.in == stdinPath {
		return "", usagef("-o is required when reading from stdin: there is no filename to derive one from")
	}
	out, err := lang.DeriveOutputPath(f.in, target)
	if err != nil {
		return "", usagef("%v", err)
	}
	return out, nil
}

// guardOutput is the last line of defence against destroying the input. The
// derivation rule can legitimately produce the input's own name -- translating
// movie.el.srt into Greek is the obvious case -- and -o can be pointed anywhere.
func guardOutput(f translateFlags, outPath string) error {
	// Writing to stdout can clobber nothing.
	if outPath == stdinPath {
		return nil
	}
	// The same-file check needs two real paths. Reading stdin skips only that
	// check — it must not also disable the clobber check below, which is what an
	// earlier combined guard did: "-i - -o existing.srt" overwrote silently.
	if f.in != stdinPath {
		same, err := lang.SameFile(f.in, outPath)
		if err != nil {
			return err
		}
		if same {
			return usagef("the output path %q is the input file; pass -o to write somewhere else", outPath)
		}
	}
	if _, err := os.Stat(outPath); err == nil && !f.force {
		return usagef("%q already exists; pass -f to overwrite", outPath)
	}
	// Finding out the directory is unwritable after the model calls are paid for
	// is a bad way to learn it.
	return checkWritable(filepath.Dir(outPath))
}

// checkWritable confirms a file can be created in dir.
func checkWritable(dir string) error {
	probe, err := os.CreateTemp(dir, ".ypotitlo-probe-*")
	if err != nil {
		return fmt.Errorf("cannot write to %s: %w", dir, err)
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	return nil
}

// createTemp makes a uniquely named file in dir with mode applied at creation.
//
// os.CreateTemp is not used because it hardcodes 0600, which would then need a
// chmod to correct — and chmod is the thing to avoid here. CIFS and NFS derive
// file modes from mount options and refuse it, so the call either errors or does
// nothing, and a subtitle directory usually lives on one; warning about that on
// every run would be pure noise. Setting the mode at creation gets the right
// result on a local filesystem and is silently governed by the mount elsewhere.
func createTemp(dir string, mode os.FileMode) (*os.File, string, error) {
	for range 10 {
		name := filepath.Join(dir, fmt.Sprintf(".ypotitlo-%d-%d.tmp", os.Getpid(), time.Now().UnixNano()))
		f, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, mode)
		if errors.Is(err, os.ErrExist) {
			continue // vanishingly unlikely; try another name
		}
		if err != nil {
			return nil, "", err
		}
		return f, name, nil
	}
	return nil, "", fmt.Errorf("could not find an unused temporary name in %s", dir)
}

// outputMode is the mode for a new output file: the existing file's when
// replacing one, otherwise 0644 less the process umask.
func outputMode(outPath string) os.FileMode {
	if fi, err := os.Stat(outPath); err == nil {
		return fi.Mode().Perm()
	}
	return 0o644 &^ os.FileMode(umask())
}

func resolveSource(ctx context.Context, e env, cues []srt.Cue, f translateFlags, opts translate.Options) (lang.Lang, string, error) {
	if f.sourceLang != "" {
		l, err := lang.Resolve(f.sourceLang)
		if err != nil {
			return lang.Lang{}, "", usagef("%v", err)
		}
		return l, "given", nil
	}

	l, provenance, err := translate.DetectLanguage(ctx, cues, f.in, opts)
	if err != nil {
		// Not knowing the source is survivable: the models translate without
		// being told, and a wrong guess is worse than no guess.
		if f.verbose {
			outf(e.Stderr, "source language: undetermined (%v)\n", err)
		}
		return lang.Lang{}, "", nil
	}
	return l, provenance, nil
}

func buildProvider(cfg config.Config, f translateFlags, e env) (llm.Provider, string, error) {
	o := configOptions(e, f.configPath)
	key, keySource, err := config.ResolveAPIKey(o, cfg, f.apiKey)
	if err != nil {
		return nil, "", fmt.Errorf("%w; set one with 'ypotitlo config-set api_key -' or $%s", err, config.EnvAPIKey)
	}
	if cfg.Model == "" {
		return nil, keySource, usagef("no model configured; pick one from 'ypotitlo list-models' and set it with 'ypotitlo config-set model NAME'")
	}

	var budget *llm.BudgetGuard
	if cfg.MaxSpendUSD > 0 {
		budget = llm.NewBudgetGuard(cfg.MaxSpendUSD, e.Now, time.UTC)
	}
	return llm.NewOpenCodeGo(llm.OpenCodeGoConfig{
		APIKey:    key,
		KeySource: keySource,
		BaseURL:   cfg.BaseURL,
		Budget:    budget,
		Now:       e.Now,
	}), keySource, nil
}

// classifyRunError maps the failures that deserve their own exit code, so that
// a shell loop over a subtitle library can tell "this file was odd" from "your
// key is wrong" and "you are out of credit" without scraping stderr.
func classifyRunError(err error, keySource string) error {
	switch {
	case errors.Is(err, llm.ErrAuth):
		if keySource != "" {
			return &codedError{exitAuth, fmt.Errorf("%w (key came from %s)", err, keySource)}
		}
		return &codedError{exitAuth, err}
	case errors.Is(err, llm.ErrCreditExhausted):
		return &codedError{exitAuth, err}
	case errors.Is(err, llm.ErrBudgetExceeded):
		return &codedError{exitBudget, fmt.Errorf("%w; raise it with -budget or 'ypotitlo config-set max_spend_usd'", err)}
	case errors.Is(err, translate.ErrStalled):
		return &codedError{exitError, fmt.Errorf("%w; the connection was open but silent, so nothing was written", err)}
	case errors.Is(err, translate.ErrProviderUnreachable):
		return &codedError{exitError, fmt.Errorf("%w; check your network and try again", err)}
	case errors.Is(err, translate.ErrMostlyUntranslated):
		return &codedError{exitError, fmt.Errorf("%w; nothing was written", err)}
	}
	return err
}

func readInput(e env, path string) ([]byte, error) {
	if path == stdinPath {
		return io.ReadAll(e.Stdin)
	}
	return os.ReadFile(path)
}

// writeOutput renders the file and puts it in place atomically: a temporary
// file in the same directory, then a rename. A crash or a signal therefore
// leaves either the old file or the new one, never a half-translated one.
func writeOutput(e env, file *srt.File, f translateFlags, outPath string) error {
	opts := srt.WriteOptions{KeepIndices: f.keepIndices}
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

	body, warnings, err := srt.WriteBytes(file, opts)
	if err != nil {
		return err
	}
	for _, w := range warnings {
		warnf(e, "%s", w)
	}

	if f.outCharset != "" && f.outCharset != "utf-8" {
		body, err = charset.Encode(body, f.outCharset, f.bomGiven && f.bom)
		if err != nil {
			return err
		}
	}

	if outPath == stdinPath {
		_, err := e.Stdout.Write(body)
		return err
	}

	dir := filepath.Dir(outPath)
	tmp, tmpName, err := createTemp(dir, outputMode(outPath))
	if err != nil {
		return fmt.Errorf("creating a temporary file in %s: %w", dir, err)
	}
	defer func() { _ = os.Remove(tmpName) }() // no-op once renamed

	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return err
	}
	// Flush to the device before the rename. Without this the rename can be
	// committed while the data blocks are not, which on a power loss or a NAS
	// reconnect replaces a good file with a truncated one — the opposite of what
	// the atomic write is for. A sync failure is worth reporting but not worth
	// discarding a finished translation over.
	if err := tmp.Sync(); err != nil {
		warnf(e, "could not flush %s to disk: %v", filepath.Base(tmpName), err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, outPath)
}

func reportDryRun(e env, file *srt.File, f translateFlags, cfg config.Config, source, target lang.Lang, outPath string) error {
	batches := (len(file.Cues) + cfg.BatchSize - 1) / cfg.BatchSize
	calls := batches
	if !f.noBrief {
		calls++
	}

	src := "undetermined"
	if !source.Zero() {
		src = source.English
	}

	outf(e.Stdout, "input          %s (%d cues)\n", displayPath(f.in), len(file.Cues))
	outf(e.Stdout, "output         %s\n", displayPath(outPath))
	outf(e.Stdout, "translating    %s -> %s\n", src, target.English)
	outf(e.Stdout, "model          %s at %s\n", orNone(cfg.Model), cfg.BaseURL)
	outf(e.Stdout, "plan           %d batches of %d, %d concurrent, ~%d calls\n", batches, cfg.BatchSize, cfg.Concurrency, calls)
	outf(e.Stdout, "budget         $%.2f\n", cfg.MaxSpendUSD)
	outf(e.Stdout, "\ndry run: no requests were made and nothing was written\n")
	return nil
}

func writeFooter(e env, s translate.Stats, outPath string, f translateFlags, elapsed time.Duration) {
	if f.quiet {
		return
	}
	cost := fmt.Sprintf("~$%.4f", s.CostUSD)
	if s.UnknownCost > 0 {
		cost = fmt.Sprintf("%s + %d calls of unknown cost", cost, s.UnknownCost)
	}
	outf(e.Stderr, "wrote %s in %s\n", displayPath(outPath), clock(elapsed))
	outf(e.Stderr, "%d calls · %d in / %d out tokens · %s · %d retries · %d cues untranslated\n",
		s.Calls, s.PromptTokens, s.CompletionTokens, cost, s.Retries, s.Untranslated)
	if s.Untranslated > 0 {
		outf(e.Stderr, "%d cues kept their original text; search the output for them before publishing\n", s.Untranslated)
	}
}

func warnf(e env, format string, a ...any) {
	outf(e.Stderr, "warning: "+format+"\n", a...)
}

// describeSource renders the source language with where it came from, or says
// plainly that it is unknown — which is a supported case, not a failure.
func describeSource(source lang.Lang, provenance string) string {
	if source.Zero() {
		return "an undetermined language"
	}
	if provenance == "" {
		return source.English
	}
	return fmt.Sprintf("%s (%s)", source.English, provenance)
}

func displayPath(p string) string {
	if p == stdinPath {
		return "<stdin>"
	}
	return p
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// isTerminal reports whether w is a character device, which is the zero-dependency
// way to keep progress out of a pipe or a log file.
func isTerminal(w io.Writer) bool {
	file, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

func translateUsage(w io.Writer) {
	usageBlock(w,
		"Translate a subtitle file into another language.\n\n"+
			"Usage: ypotitlo translate -i FILE -ol LANGUAGE [flags]\n\n"+
			"Languages are accepted as a code or a name: el, ell, gre, greek and\n"+
			"Greek all mean the same thing. The output filename replaces the input's\n"+
			"language suffix, keeping track markers: movie.eng.sdh.srt becomes\n"+
			"movie.el.sdh.srt.",
		[]flagSection{
			{Title: "Source", Flags: []flagDoc{
				{"-i FILE", "input subtitle file, or - for stdin (required)"},
				{"-il LANG", "source language; omit to detect it"},
				{"-charset NAME", "input character set; omit to detect it"},
			}},
			{Title: "Output", Flags: []flagDoc{
				{"-ol LANG", "target language (required unless target_language is configured)"},
				{"-o FILE", "output file, or - for stdout; omit to derive it from -i"},
				{"-f", "overwrite an existing output file"},
				{"-output-charset", "output character set (default utf-8)"},
				{"-bom / -crlf", "force a BOM or CRLF endings; omit to match the input"},
				{"-keep-indices", "keep the input's cue numbering instead of renumbering"},
			}},
			{Title: "Model", Flags: []flagDoc{
				{"-m ID", "model id (see 'ypotitlo list-models')"},
				{"-base-url URL", "API base URL"},
				{"-api-key KEY", "API key; prefer the config file or the environment"},
				{"-b N", "cues per request"},
				{"-j N", "concurrent requests"},
				{"-budget USD", "spend ceiling for this run"},
				{"-timeout D", "overall deadline, e.g. 30m"},
				{"-no-brief", "skip the whole-file consistency pass"},
			}},
			{Title: "Reporting", Flags: []flagDoc{
				{"-n", "report the plan and make no API calls"},
				{"-v", "explain what was detected and why"},
				{"-q", "suppress progress and the summary"},
			}},
		},
		[]string{
			"ypotitlo translate -i movie.en.srt -ol greek",
			"ypotitlo translate -i movie.srt -il en -ol el -o out.srt",
			"ypotitlo translate -i movie.en.srt -ol el -n",
		})
}

// writeSpend reports what a run cost when it did not produce a file. Nothing is
// written in that case, so the accounting is the only evidence of the attempt.
func writeSpend(e env, s translate.Stats, elapsed time.Duration, f translateFlags) {
	if f.quiet || s.Calls == 0 {
		return
	}
	cost := fmt.Sprintf("~$%.4f", s.CostUSD)
	if s.UnknownCost > 0 {
		cost = fmt.Sprintf("%s + %d calls of unknown cost", cost, s.UnknownCost)
	}
	outf(e.Stderr, "spent %d calls · %d in / %d out tokens · %s · in %s\n",
		s.Calls, s.PromptTokens, s.CompletionTokens, cost, clock(elapsed))
}
