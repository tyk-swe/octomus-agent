package redact

import (
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Part is text from a capture, optionally cut inside its first or last line.
// Prefix is included only when text remains after trimming the cut lines.
type Part struct {
	Text             string
	Prefix           string
	CutStart, CutEnd bool
}

// Parts scrubs the concatenation after trimming capture cuts, retaining the part
// boundaries. A secret spanning parts is replaced once, in the part where it starts.
func Parts(parts ...Part) []string {
	values := environmentSecrets()
	texts := make([]string, len(parts))
	for i, part := range parts {
		if text, _ := partSlice(part, values); text != "" {
			texts[i] = part.Prefix + text
		}
	}
	return scrubParts(texts, values)
}

func partSlice(part Part, values []string) (text string, start int) {
	text = part.Text
	if part.CutStart {
		text = cutFragment(text, TailLineCut, values)
		start = len(part.Text) - len(text)
	}
	if part.CutEnd {
		text = cutFragment(text, HeadLineCut, values)
	}
	return text, start
}

// Streams returns first's parts, a separating newline, and second's parts after
// capture-cut normalization and joint redaction. Both adjacent and separated
// views are matched before replacement. Discarded capture fragments still inform
// redaction of retained bytes, without changing which cut lines are displayed.
func Streams(first, second []Part) []string {
	values := environmentSecrets()
	parts := append(slices.Clone(first), Part{Text: "\n"})
	parts = append(parts, second...)
	texts, raw := make([]string, len(parts)), make([]string, len(parts))
	// Each retained slice maps original capture bytes to their display offsets.
	type keptSlice struct{ rawStart, rawEnd, textStart int }
	var kept []keptSlice
	var rawOffset, textOffset, rawBoundary, textBoundary int
	for i, part := range parts {
		if i == len(first) {
			rawBoundary, textBoundary = rawOffset, textOffset
		}
		if part.Text != "" {
			raw[i] = part.Prefix + part.Text
		}
		text, start := partSlice(part, values)
		if text != "" {
			texts[i] = part.Prefix + text
			// Anchor projected replacements in retained capture bytes, never in
			// display-only prefixes or the separator that callers may omit.
			if i != len(first) {
				from := rawOffset + len(part.Prefix) + start
				kept = append(kept, keptSlice{from, from + len(text), textOffset + len(part.Prefix)})
			}
		}
		rawOffset += len(raw[i])
		textOffset += len(texts[i])
	}
	input := strings.Join(texts, "")
	spans := streamSpans(input, textBoundary, values)
	original := strings.Join(raw, "")
	if original == input {
		return replaceParts(texts, input, spans)
	}
	for _, span := range streamSpans(original, rawBoundary, values) {
		from, to := -1, 0
		for _, slice := range kept {
			start, end := max(span[0], slice.rawStart), min(span[1], slice.rawEnd)
			if start < end {
				if from < 0 {
					from = slice.textStart + start - slice.rawStart
				}
				to = slice.textStart + end - slice.rawStart
			}
		}
		if from >= 0 {
			spans = append(spans, [2]int{from, to})
		}
	}
	return replaceParts(texts, input, mergeSpans(spans))
}

func streamSpans(input string, boundary int, values []string) [][2]int {
	spans := secretSpans(input, values)
	adjacent := input[:boundary] + input[boundary+1:]
	for _, span := range secretSpans(adjacent, values) {
		// Map back across the display-only newline. A spanning match includes
		// that newline; a match starting in the second stream leaves it alone.
		if span[0] >= boundary {
			span[0]++
		}
		if span[1] > boundary {
			span[1]++
		}
		spans = append(spans, span)
	}
	return mergeSpans(spans)
}

type FragmentKind uint8

const (
	HeadLineCut FragmentKind = iota
	HeadWordCut
	TailLineCut
	TailTwoWordsCut
)

func Fragment(input string, kind FragmentKind) string {
	values := environmentSecrets()
	return scrub(cutFragment(input, kind, values), values)
}

func cutFragment(text string, kind FragmentKind, values []string) string {
	switch kind {
	case HeadLineCut:
		if i := strings.LastIndexByte(text, '\n'); i >= 0 {
			return trimCutSecretEnd(text[:i], values)
		}
		if i := strings.LastIndexFunc(text, unicode.IsSpace); i >= 0 {
			return trimCutSecretEnd(text[:i], values)
		}
		return ""
	case HeadWordCut:
		scrubbed, before := scrub(text, values), ""
		if i := strings.LastIndexFunc(scrubbed, unicode.IsSpace); i >= 0 {
			before = scrubbed[:i]
		}
		return trimCutSecretEnd(before, values)
	case TailLineCut:
		return tailLineStart(text, values)
	case TailTwoWordsCut:
		return tailTwoWordsStart(scrub(text, values), values)
	}
	return ""
}

func afterWord(text string) string {
	if i := strings.IndexFunc(text, unicode.IsSpace); i >= 0 {
		return text[i:]
	}
	return ""
}

func tailTwoWordsStart(text string, values []string) string {
	var rest string
	if dropped, after, found := strings.Cut(text, "\n"); found {
		rest = after
		if mayEndBearerPrefix(strings.TrimRightFunc(dropped, unicode.IsSpace)) {
			rest = afterWord(strings.TrimLeftFunc(rest, unicode.IsSpace))
		}
	} else {
		rest = afterWord(strings.TrimLeftFunc(afterWord(text), unicode.IsSpace))
	}
	return trimCutSecretStart(strings.TrimLeftFunc(rest, unicode.IsSpace), values)
}

const escapeIntermediates = " !\"#$%&'()*+,-./"

var cutEscapeKey = regexp.MustCompile(`(?i)^[a-z]sk-[a-z0-9_-]{10}`)

func mayEndBearerPrefix(text string) bool {
	const prefix = "bearer"
	n := min(len(text), len(prefix))
	return (n == len(text) || n == len(prefix)) && strings.EqualFold(text[len(text)-n:], prefix[len(prefix)-n:])
}

func tailLineStart(text string, values []string) string {
	dropped, rest, found := strings.Cut(text, "\n")
	if !found {
		i := strings.IndexFunc(text, unicode.IsSpace)
		if i < 0 {
			return ""
		}
		dropped, rest = text[:i], text[i:]
	}
	run := 0
	for {
		rest = strings.TrimLeftFunc(rest, unicode.IsSpace)
		at := len(text) - len(rest)
		if at >= run {
			run = at + len(rest) - len(strings.TrimLeft(rest, escapeIntermediates))
		}
		from := -1
		if key := cutEscapeKey.FindStringIndex(text[run:]); key != nil {
			from = run - at + key[1]
		} else if mayEndBearerPrefix(strings.TrimRightFunc(dropped, unicode.IsSpace)) {
			from = 0
		} else if trimmed := trimCutSecretStart(rest, values); len(trimmed) < len(rest) {
			from = len(rest) - len(trimmed)
		}
		if from < 0 {
			return rest
		}
		i := strings.IndexFunc(rest[from:], unicode.IsSpace)
		if i < 0 {
			return ""
		}
		dropped, rest = rest[:from+i], rest[from+i:]
	}
}

func trimCutSecretEnd(text string, values []string) string {
	cut := 0
	for _, value := range values {
		for i, r := range value {
			if i > cut && unicode.IsSpace(r) && strings.HasSuffix(text, value[:i]) {
				cut = i
			}
		}
	}
	return text[:len(text)-cut]
}

func trimCutSecretStart(text string, values []string) string {
	cut := 0
	for _, value := range values {
		for i, r := range value {
			if !unicode.IsSpace(r) {
				continue
			}
			if rest := value[i+utf8.RuneLen(r):]; len(rest) > cut && strings.HasPrefix(text, rest) {
				cut = len(rest)
			}
		}
	}
	return text[cut:]
}
