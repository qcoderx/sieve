// Package textnorm cleans recovered text once, at the graph boundary.
//
// Doing it here rather than per format is the same reasoning that makes every
// output render from a single graph: a rule applied in the Markdown writer and
// forgotten in the JSON writer is not a rule, it is a coincidence. Every
// emitted format inherits whatever happens in this package, and nothing
// downstream is permitted to re-clean text.
//
// # What this defends against
//
// Text extracted from a third-party page goes straight into a model's context.
// Unicode offers several ways to make a string read one way to a human and
// another way to a machine, and none of them are visible in a diff:
//
//   - Bidirectional overrides can reverse the displayed order of a run, so
//     that text reading "delete nothing" on screen is "gnihton eteled" in the
//     buffer -- or the reverse, which is the direction that matters here.
//   - Zero-width characters split a word so it survives a human read and a
//     substring search but reaches the model as separate tokens, which is how
//     a filtered phrase gets past a filter.
//   - Tag characters (U+E0000 block) are invisible everywhere and were used
//     for exactly this in the wild.
//
// None of these appear in legitimate rendered prose in a way that survives
// removal, so removal is safe and the absence of them is worth guaranteeing.
//
// # Why the version number exists
//
// Changing what this package does changes every artifact's content hash, which
// invalidates every cache everywhere. That has to be a decision, not an
// accident, so the version is part of the hash input: a consumer can see that
// the normalizer changed rather than inferring it from a hash that moved for
// no visible reason.
package textnorm

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Version identifies the normalization rules. It is an input to every content
// hash, so bumping it deliberately invalidates caches.
const Version = 1

// Result reports what normalization had to remove. A caller that wants to
// know whether a page was playing games can look at this rather than diffing.
type Result struct {
	Text string
	// Removed counts control characters stripped.
	Removed int
	// HadBidi is true when directional override characters were present. This
	// is worth surfacing: unlike a stray zero-width space, a bidi override in
	// body copy is almost never accidental.
	HadBidi bool
	// HadInvisible is true when zero-width or tag characters were present.
	HadInvisible bool
}

// Clean normalizes a run of text.
func Clean(s string) Result {
	if s == "" {
		return Result{}
	}
	// Fast path: the overwhelming majority of runs contain nothing to strip and
	// need no allocation at all.
	if !needsWork(s) {
		return Result{Text: s}
	}

	var b strings.Builder
	b.Grow(len(s))
	r := Result{}
	lastSpace := false
	for _, c := range s {
		switch {
		case isBidiControl(c):
			r.HadBidi = true
			r.Removed++
			continue
		case isInvisible(c):
			r.HadInvisible = true
			r.Removed++
			continue
		case c == '�':
			// A replacement character means the bytes were already broken;
			// carrying it forward just puts a black diamond in the artifact.
			r.Removed++
			continue
		case c != '\n' && c != '\t' && unicode.Is(unicode.Cf, c):
			// Any other format character: soft hyphens, joiners, interlinear
			// annotation marks. None of them carry meaning once the text is out
			// of its original layout.
			r.Removed++
			continue
		}
		// Collapse whitespace runs, including the exotic spaces that survive a
		// naive TrimSpace.
		if unicode.IsSpace(c) {
			if !lastSpace {
				b.WriteByte(' ')
				lastSpace = true
			}
			continue
		}
		lastSpace = false
		b.WriteRune(c)
	}
	r.Text = strings.TrimSpace(b.String())
	return r
}

// CleanString is the convenience form for callers that do not need the report.
func CleanString(s string) string { return Clean(s).Text }

// needsWork reports whether a string contains anything Clean would change.
// It is a scan with no allocation, and it returns false for nearly every run
// on a real page.
func needsWork(s string) bool {
	prevSpace := false
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			// ASCII fast path.
			if c == ' ' {
				if prevSpace {
					return true
				}
				prevSpace = true
			} else if c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f' {
				return true
			} else {
				prevSpace = false
			}
			if c < 0x20 && c != '\t' && c != '\n' && c != '\r' {
				return true
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError || isBidiControl(r) || isInvisible(r) ||
			unicode.Is(unicode.Cf, r) || unicode.IsSpace(r) {
			return true
		}
		prevSpace = false
		i += size
	}
	// Leading or trailing whitespace still needs trimming.
	return len(s) > 0 && (s[0] == ' ' || s[len(s)-1] == ' ')
}

// isBidiControl covers the explicit directional formatting characters. These
// change the *displayed order* of the characters around them, which means the
// text a human proofreads and the text a model receives can differ.
func isBidiControl(r rune) bool {
	switch r {
	case 0x200E, 0x200F, // LRM, RLM
		0x202A, 0x202B, 0x202C, 0x202D, 0x202E, // LRE, RLE, PDF, LRO, RLO
		0x2066, 0x2067, 0x2068, 0x2069: // LRI, RLI, FSI, PDI
		return true
	}
	return false
}

// isInvisible covers characters that occupy no space and render as nothing.
func isInvisible(r rune) bool {
	switch {
	case r == 0x00AD: // soft hyphen
		return true
	case r >= 0x200B && r <= 0x200D: // ZWSP, ZWNJ, ZWJ
		return true
	case r == 0x2060 || r == 0xFEFF: // word joiner, BOM
		return true
	case r >= 0x2061 && r <= 0x2064: // invisible maths operators
		return true
	case r >= 0xFFF9 && r <= 0xFFFB: // interlinear annotation
		return true
	case r >= 0xE0000 && r <= 0xE007F: // tag characters
		return true
	case r >= 0xE0100 && r <= 0xE01EF: // variation selectors supplement
		return true
	}
	return false
}

// Truncate cuts a string to n runes without splitting one, appending an
// ellipsis when it cut. Metadata channels are capped rather than trusted, and
// a cap that splits a rune produces mojibake in every consumer.
func Truncate(s string, n int) (string, bool) {
	if n <= 0 {
		return "", s != ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s, false
	}
	count := 0
	for i := range s {
		if count == n {
			return strings.TrimSpace(s[:i]) + "…", true
		}
		count++
	}
	return s, false
}
