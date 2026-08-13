// Package tokens estimates how many tokens a piece of text will cost.
//
// The estimate is not a tokenizer. Shipping a real BPE vocabulary would add
// megabytes of data and tie the artifact to one model family, and the numbers
// that matter -- the ones in the benchmark -- come from the API's own usage
// accounting, which is exact. This exists so that a manifest can tell an agent
// roughly what a section will cost before it asks for it, which is a decision
// that only needs to be right to within a few percent.
package tokens

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Estimate approximates the token count of a string.
//
// The model is a blend of two signals that fail in opposite directions.
// Characters-per-token is stable for prose but badly wrong for text full of
// punctuation, markup or IDs. Word count is stable for structured text but
// wrong for long words and for scripts without spaces. Taking the larger of the
// two keeps the estimate from collapsing on either kind of input, and erring
// high is the right direction: an agent that budgets too much reads the
// section, one that budgets too little truncates it.
func Estimate(s string) int {
	if s == "" {
		return 0
	}
	runes := utf8.RuneCountInString(s)

	// Latin prose runs about 3.8 characters per token across current
	// vocabularies. CJK is far denser: roughly one token per character or
	// slightly better, so text is measured in two populations rather than one
	// average that fits neither.
	var wide, narrow int
	var words int
	inWord := false
	for _, r := range s {
		switch {
		case isWide(r):
			wide++
			inWord = false
		case unicode.IsSpace(r):
			if inWord {
				words++
			}
			inWord = false
			narrow++
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			inWord = true
			narrow++
		default:
			// Punctuation and symbols are usually a token each and rarely
			// merge with neighbours.
			if inWord {
				words++
			}
			inWord = false
			narrow++
		}
	}
	if inWord {
		words++
	}

	byChars := float64(narrow)/3.8 + float64(wide)*1.05
	// Roughly one and a third tokens per word covers the way common suffixes
	// and leading spaces are split.
	byWords := float64(words) * 1.33
	if wide > 0 {
		byWords += float64(wide) * 1.05
	}

	est := byChars
	if byWords > est {
		est = byWords
	}
	if est < 1 && runes > 0 {
		est = 1
	}
	return int(est + 0.5)
}

// EstimateChars approximates tokens from a character count alone, for callers
// that have already discarded the text.
func EstimateChars(n int) int {
	if n <= 0 {
		return 0
	}
	return int(float64(n)/3.8 + 0.5)
}

// isWide reports whether a rune belongs to a script that tokenizes at roughly
// one token per character.
func isWide(r rune) bool {
	switch {
	case r >= 0x3040 && r <= 0x30ff: // kana
		return true
	case r >= 0x3400 && r <= 0x4dbf: // CJK ext A
		return true
	case r >= 0x4e00 && r <= 0x9fff: // CJK unified
		return true
	case r >= 0xac00 && r <= 0xd7af: // hangul
		return true
	case r >= 0xf900 && r <= 0xfaff: // CJK compat
		return true
	case r >= 0x20000 && r <= 0x2ebef: // CJK ext B-F
		return true
	}
	return false
}

// EstimateHTML approximates the cost of feeding raw markup to a model, which is
// the number the distilled artifact is measured against.
//
// Markup tokenizes far worse than prose: angle brackets, attribute quoting,
// class-name soup and minified script all fragment heavily. Treating a
// megabyte of HTML as if it were a megabyte of English would understate the
// cost the artifact is avoiding by a wide margin, so raw markup is estimated
// at a denser ratio.
func EstimateHTML(s string) int {
	if s == "" {
		return 0
	}
	// Inline script and style are the densest part of a modern page and the
	// least like prose, so they are counted separately.
	dense := 0
	rest := s
	for {
		i := indexFold(rest, "<script")
		if i < 0 {
			break
		}
		j := indexFold(rest[i:], "</script>")
		if j < 0 {
			break
		}
		dense += j
		rest = rest[i+j:]
	}
	prose := len(s) - dense
	return int(float64(prose)/2.9 + float64(dense)/2.3 + 0.5)
}

func indexFold(s, sub string) int {
	return strings.Index(strings.ToLower(s), sub)
}
