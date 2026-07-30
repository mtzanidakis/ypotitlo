package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/mtzanidakis/ypotitlo/internal/srt"
	"github.com/mtzanidakis/ypotitlo/internal/translate"
)

// briefCache is a translation brief parked next to a partly translated file.
//
// Pass 0 costs about a minute and fifteen thousand tokens, and it describes the
// film rather than the attempt — so recomputing it when resuming buys nothing.
// The file is written only when a run fails with work worth keeping, and removed
// as soon as one finishes, so it exists exactly while it is useful.
type briefCache struct {
	Target string           `json:"target"`
	Digest string           `json:"digest"`
	Brief  *translate.Brief `json:"brief"`
}

// briefCachePath is a hidden sibling of the output.
func briefCachePath(outPath string) string {
	dir, base := filepath.Split(outPath)
	return filepath.Join(dir, "."+base+".brief")
}

// briefDigest fingerprints the source dialogue, so a cache written for one
// subtitle is never applied to another that happens to share a filename.
func briefDigest(cues []srt.Cue) string {
	h := sha256.New()
	for _, c := range cues {
		for _, l := range c.Lines {
			_, _ = h.Write([]byte(l))
			_, _ = h.Write([]byte{'\n'})
		}
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// saveBrief parks a brief for a later resume. Failing to write one is not worth
// reporting as an error: the next run simply computes it again.
func saveBrief(outPath, target string, cues []srt.Cue, b *translate.Brief) error {
	if b == nil {
		return nil
	}
	body, err := json.Marshal(briefCache{Target: target, Digest: briefDigest(cues), Brief: b})
	if err != nil {
		return err
	}
	path := briefCachePath(outPath)
	f, name, err := createTemp(filepath.Dir(path), 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(name) }()
	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// loadBrief returns a cached brief when one matches this subtitle and target.
// Anything else — missing, unreadable, stale, for another film — means there is
// no cache, not an error: the brief is an optimisation and recomputing it is
// always correct.
func loadBrief(outPath, target string, cues []srt.Cue) *translate.Brief {
	body, err := os.ReadFile(briefCachePath(outPath))
	if err != nil {
		return nil
	}
	var c briefCache
	if err := json.Unmarshal(body, &c); err != nil {
		return nil
	}
	if !strings.EqualFold(c.Target, target) || c.Digest != briefDigest(cues) {
		return nil
	}
	return c.Brief
}

// dropBrief removes the cache once it is no longer needed. A failure here is
// not worth surfacing: the file is an optimisation, and a stale one is ignored
// by loadBrief anyway.
func dropBrief(outPath string) {
	_ = os.Remove(briefCachePath(outPath))
}
