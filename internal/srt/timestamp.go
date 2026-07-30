package srt

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// errNoArrow reports that a line contains no "-->" separator at all, which is
// the common case: most lines of an SRT file are text.
var errNoArrow = errors.New("no --> separator")

// maxHourDigits bounds the hour field. Hours are otherwise unbounded (a
// concatenated box set legitimately runs past 24h and three-digit hours are
// attested), but an unbounded digit run would overflow time.Duration.
const maxHourDigits = 7

// splitArrow splits a line on the first "-->" separator. The separator is
// matched as -{2,}> because "--->" and longer dashes occur in the wild.
func splitArrow(line string) (left, right string, ok bool) {
	for i := 0; i < len(line); i++ {
		if line[i] != '-' {
			continue
		}
		j := i
		for j < len(line) && line[j] == '-' {
			j++
		}
		if j-i >= 2 && j < len(line) && line[j] == '>' {
			return line[:i], line[j+1:], true
		}
		i = j - 1
	}
	return "", "", false
}

// hasArrow reports whether the line contains an arrow separator, regardless of
// whether the surrounding fields parse as timings.
func hasArrow(line string) bool {
	_, _, ok := splitArrow(line)
	return ok
}

// parseTimeLine parses a cue timing line such as
//
//	00:00:01,000 --> 00:00:02,000  X1:040 X2:600 Y1:050 Y2:100
//
// Whitespace around the arrow is free and anything after the end timing (the
// legacy SubRip on-screen coordinates, for one) is discarded. The returned
// warnings are unprefixed; the caller adds the line number, because a line that
// turns out to be cue text must not produce warnings.
func parseTimeLine(line string) (start, end time.Duration, warns []string, err error) {
	left, right, ok := splitArrow(line)
	if !ok {
		return 0, 0, nil, errNoArrow
	}

	lt := strings.TrimSpace(left)
	rt := strings.TrimSpace(right)
	// Trailing junk after the end timing: keep the first whitespace-
	// delimited field only.
	if i := strings.IndexAny(rt, " \t"); i >= 0 {
		rt = rt[:i]
	}
	if lt == "" || rt == "" {
		return 0, 0, nil, fmt.Errorf("missing timing on %s side of -->", sideName(lt))
	}

	start, sw, err := parseTimeToken(lt)
	if err != nil {
		return 0, 0, nil, err
	}
	end, ew, err := parseTimeToken(rt)
	if err != nil {
		return 0, 0, nil, err
	}
	return start, end, append(sw, ew...), nil
}

func sideName(left string) string {
	if left == "" {
		return "left"
	}
	return "right"
}

// parseTimeToken parses a single timing field. It tokenises first and then
// disambiguates on the alphabet of separators used; see the package comment for
// why a single regexp is not good enough.
func parseTimeToken(tok string) (time.Duration, []string, error) {
	fields, seps, err := tokenizeTime(tok)
	if err != nil {
		return 0, nil, err
	}

	timeFields, msField, err := splitMilliseconds(tok, fields, seps)
	if err != nil {
		return 0, nil, err
	}

	var warns []string
	var h, m, s int
	// Right to left: SS, then MM, then HH. Absent means zero.
	for i, name := range []string{"seconds", "minutes"} {
		p := len(timeFields) - 1 - i
		if p < 0 {
			break
		}
		f := timeFields[p]
		if len(f) > 2 {
			return 0, nil, fmt.Errorf("timestamp %q: %s field %q has more than two digits", tok, name, f)
		}
		v, convErr := strconv.Atoi(f)
		if convErr != nil {
			return 0, nil, fmt.Errorf("timestamp %q: %s field %q: %w", tok, name, f, convErr)
		}
		if v >= 60 {
			warns = append(warns, fmt.Sprintf("timestamp %q: %s value %d is out of range", tok, name, v))
		}
		if i == 0 {
			s = v
		} else {
			m = v
		}
	}
	if p := len(timeFields) - 3; p >= 0 {
		f := timeFields[p]
		if len(f) > maxHourDigits {
			return 0, nil, fmt.Errorf("timestamp %q: hours field %q is too large", tok, f)
		}
		v, convErr := strconv.Atoi(f)
		if convErr != nil {
			return 0, nil, fmt.Errorf("timestamp %q: hours field %q: %w", tok, f, convErr)
		}
		h = v
	}

	ms, msWarn := parseMilliseconds(tok, msField)
	if msWarn != "" {
		warns = append(warns, msWarn)
	}

	d := time.Duration(h)*time.Hour +
		time.Duration(m)*time.Minute +
		time.Duration(s)*time.Second +
		time.Duration(ms)*time.Millisecond
	return d, warns, nil
}

// tokenizeTime splits a timing field into digit runs and the separators between
// them. Any other byte is a hard error, which is what keeps ordinary cue text
// from being mistaken for a timing line.
func tokenizeTime(tok string) (fields []string, seps []byte, err error) {
	start := 0
	for i := 0; i < len(tok); i++ {
		c := tok[i]
		switch {
		case c >= '0' && c <= '9':
		case c == ':' || c == ',' || c == '.':
			fields = append(fields, tok[start:i])
			seps = append(seps, c)
			start = i + 1
		default:
			return nil, nil, fmt.Errorf("timestamp %q: unexpected character %q", tok, string(c))
		}
	}
	fields = append(fields, tok[start:])

	for _, f := range fields {
		if f == "" {
			return nil, nil, fmt.Errorf("timestamp %q: empty field", tok)
		}
	}
	return fields, seps, nil
}

// splitMilliseconds decides which of the tokenised fields is the millisecond
// field, using only the separators.
func splitMilliseconds(tok string, fields []string, seps []byte) (timeFields []string, msField string, err error) {
	msSep := -1
	for i, c := range seps {
		if c != ',' && c != '.' {
			continue
		}
		if msSep >= 0 {
			return nil, "", fmt.Errorf("timestamp %q: more than one decimal separator", tok)
		}
		msSep = i
	}

	switch {
	case msSep >= 0:
		// Rule 1: the decimal separator marks the boundary and must be
		// the last separator, so exactly one field follows it.
		if msSep != len(seps)-1 {
			return nil, "", fmt.Errorf("timestamp %q: decimal separator is not before the last field", tok)
		}
		timeFields, msField = fields[:msSep+1], fields[msSep+1]
	case len(fields) == 4:
		// Rule 2: HH:MM:SS:mmm.
		timeFields, msField = fields[:3], fields[3]
	case len(fields) == 3 || len(fields) == 2:
		// Rule 2: HH:MM:SS or MM:SS, both with ms = 0. Never
		// MM:SS:mmm — that reading is not attested and costs 89s a cue
		// when it is wrong.
		timeFields, msField = fields, ""
	default:
		// Rule 3.
		return nil, "", fmt.Errorf("timestamp %q: %d fields is not a timestamp", tok, len(fields))
	}

	if len(timeFields) > 3 {
		return nil, "", fmt.Errorf("timestamp %q: too many fields before the milliseconds", tok)
	}
	return timeFields, msField, nil
}

// parseMilliseconds reads the millisecond field as a decimal fraction: ",5" is
// 500ms and ",05" is 50ms. ffmpeg reads ",5" as 5ms instead; this is a
// documented, deliberate divergence. More than three digits are truncated,
// which is what a decimal fraction implies.
func parseMilliseconds(tok, field string) (ms int, warn string) {
	switch {
	case field == "":
		return 0, ""
	case len(field) > 3:
		warn = fmt.Sprintf("timestamp %q: milliseconds field %q truncated to three digits", tok, field)
		field = field[:3]
	}
	// The field is all digits and at most three of them, so this cannot
	// fail or overflow.
	v, err := strconv.Atoi(field)
	if err != nil {
		return 0, warn
	}
	for i := len(field); i < 3; i++ {
		v *= 10
	}
	return v, warn
}

// formatTime renders a duration as HH:MM:SS,mmm with at least two hour digits.
// Hours beyond 99 simply widen the field. Sub-millisecond precision, which the
// reader never produces, is truncated.
func formatTime(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	ms := d.Milliseconds()
	h := ms / int64(time.Hour/time.Millisecond)
	ms %= int64(time.Hour / time.Millisecond)
	m := ms / int64(time.Minute/time.Millisecond)
	ms %= int64(time.Minute / time.Millisecond)
	s := ms / int64(time.Second/time.Millisecond)
	ms %= int64(time.Second / time.Millisecond)
	return fmt.Sprintf("%02d:%02d:%02d,%03d", h, m, s, ms)
}
