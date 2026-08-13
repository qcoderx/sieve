// Package corroborate holds the confirm-only index used to decide whether a
// string recovered from pixels was really something the site shipped.
//
// # Why this is confirm-only, and why that is not a limitation
//
// The text on a WebGL brand site almost always exists as text before it becomes
// pixels: a hydration blob, a CMS payload, a string table in the bundle. That
// makes those payloads an enormously tempting source of content, and using them
// as one would be a mistake.
//
// A hydration blob routinely carries draft copy, other locales, unpublished
// fields, internal identifiers, and adjacent records that were never on the
// page. Emitting any of it stops being extraction and becomes republishing
// someone's back office -- which the project's ethics position rules out, and
// which no amount of filtering makes safe, because the filter would have to
// know which records the site intended to publish and it cannot.
//
// There is a second, quieter reason. Fidelity is defined as the share of
// artifact statements verifiable in the source. Content that only ever existed
// in a data feed fails that check while being technically true, so a distiller
// that mines feeds scores worse on its own headline metric.
//
// So the rule is absolute and it is enforced by this package's shape rather
// than by discipline: an Index answers one question -- "does this string appear
// in what the site shipped?" -- and offers no way to enumerate its contents.
// Confirmation promotes a guess from speculative to verified. Absence leaves it
// speculative. Nothing here can become a block.
package corroborate

import (
	"strings"
	"sync"
	"unicode"
)

// DefaultCap bounds the corpus. Large enough for a hydration blob and the
// string tables of a typical bundle; small enough that a hostile response
// cannot exhaust memory.
const DefaultCap = 8 << 20

// Index answers membership questions about the text a site shipped.
//
// It deliberately exposes no iteration, no listing, and no way to read a stored
// string back out. The only thing a caller can learn is whether a string it
// already has appears somewhere in the payload.
type Index struct {
	mu    sync.RWMutex
	buf   strings.Builder
	cap   int
	full  bool
	// sources counts what fed the index, for the artifact's provenance record.
	sources map[string]int
}

// New builds an empty index.
func New(capBytes int) *Index {
	if capBytes <= 0 {
		capBytes = DefaultCap
	}
	return &Index{cap: capBytes, sources: map[string]int{}}
}

// AddText folds in plain text: inline JSON, hydration blobs, the page's own
// rendered text.
func (ix *Index) AddText(kind, s string) {
	if s == "" {
		return
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	ix.appendLocked(kind, normalize(s))
}

// AddScript folds in a JavaScript bundle by extracting its string literals.
//
// Storing whole bundles would be tens of megabytes of minified code, almost all
// of it operators and identifiers. The literals are the part that can contain a
// headline, and they are a small fraction of the bytes.
func (ix *Index) AddScript(kind, src string) {
	if src == "" {
		return
	}
	lits := extractStringLiterals(src)
	if len(lits) == 0 {
		return
	}
	ix.mu.Lock()
	defer ix.mu.Unlock()
	for _, l := range lits {
		ix.appendLocked(kind, normalize(l))
		if ix.full {
			return
		}
	}
}

func (ix *Index) appendLocked(kind, s string) {
	if s == "" || ix.full {
		return
	}
	remaining := ix.cap - ix.buf.Len()
	if remaining <= 1 {
		ix.full = true
		return
	}
	if len(s) > remaining-1 {
		s = s[:remaining-1]
		ix.full = true
	}
	ix.buf.WriteString(s)
	ix.buf.WriteByte('\n')
	ix.sources[kind]++
}

// Contains reports whether a candidate string appears in the shipped payload.
//
// The comparison is on a normalised form -- lowercased, whitespace collapsed,
// punctuation folded -- because a headline set in a 3D scene and the same
// headline in a JSON field routinely differ in casing, curly quotes, and
// non-breaking spaces while being unmistakably the same sentence.
func (ix *Index) Contains(candidate string) bool {
	c := normalize(candidate)
	// Very short strings match by accident. "Home" appears in every bundle
	// ever written, so confirming on it would promote noise to verified.
	if len(c) < 12 {
		return false
	}
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return strings.Contains(ix.buf.String(), c)
}

// ContainsAny reports whether any substantial fragment of the candidate appears.
// A vision model rarely reproduces a headline verbatim; it commonly gets a
// clause right and the rest approximate. Matching on the longest clause is the
// difference between confirming most real recoveries and confirming none.
func (ix *Index) ContainsAny(candidate string) (string, bool) {
	if ix.Contains(candidate) {
		return candidate, true
	}
	for _, frag := range clauses(candidate) {
		if ix.Contains(frag) {
			return frag, true
		}
	}
	return "", false
}

// Size reports how many bytes are indexed, for the artifact's provenance
// record. It reveals nothing about the contents.
func (ix *Index) Size() int {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return ix.buf.Len()
}

// Sources reports how many fragments came from each kind of input.
func (ix *Index) Sources() map[string]int {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	out := make(map[string]int, len(ix.sources))
	for k, v := range ix.sources {
		out[k] = v
	}
	return out
}

// Saturated reports that the cap was reached, which means a negative result is
// weaker evidence than it would otherwise be.
func (ix *Index) Saturated() bool {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return ix.full
}

// normalize folds the differences that do not change meaning: case, whitespace
// runs, curly quotation marks, non-breaking spaces, and the various dashes.
func normalize(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	lastSpace := true
	for _, r := range s {
		switch r {
		case '‘', '’', '‚', '‛', '′':
			r = '\''
		case '“', '”', '„', '″':
			r = '"'
		case '‐', '‑', '‒', '–', '—', '―', '−':
			r = '-'
		case ' ', ' ', ' ', ' ', ' ':
			r = ' '
		case '…':
			b.WriteString("...")
			lastSpace = false
			continue
		}
		if unicode.IsSpace(r) {
			if !lastSpace {
				b.WriteByte(' ')
				lastSpace = true
			}
			continue
		}
		lastSpace = false
		b.WriteRune(unicode.ToLower(r))
	}
	return strings.TrimSpace(b.String())
}

// clauses splits a candidate into the fragments worth testing separately.
func clauses(s string) []string {
	parts := strings.FieldsFunc(s, func(r rune) bool {
		return r == '.' || r == ',' || r == ';' || r == ':' || r == '\n' ||
			r == '!' || r == '?' || r == '—' || r == '–' || r == '|'
	})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if len(p) >= 16 {
			out = append(out, p)
		}
	}
	// Longest first: a longer confirmed fragment is stronger evidence.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && len(out[j]) > len(out[j-1]); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// extractStringLiterals pulls quoted strings out of JavaScript source.
//
// This is a scanner, not a parser. It handles the three quote forms and escape
// sequences, and it deliberately does not try to understand regular expression
// literals or template interpolation: the cost of a rare mis-scan is a spurious
// entry in a membership index, which can only ever cause a recovered string to
// be marked verified slightly too readily. A full JS parser would be orders of
// magnitude slower for no meaningful gain in that trade.
func extractStringLiterals(src string) []string {
	const minLiteral = 8
	var out []string
	i := 0
	n := len(src)
	for i < n {
		c := src[i]
		if c != '"' && c != '\'' && c != '`' {
			// Skip line and block comments so their prose does not enter the
			// index as if it were shipped content.
			if c == '/' && i+1 < n {
				if src[i+1] == '/' {
					for i < n && src[i] != '\n' {
						i++
					}
					continue
				}
				if src[i+1] == '*' {
					i += 2
					for i+1 < n && !(src[i] == '*' && src[i+1] == '/') {
						i++
					}
					i += 2
					continue
				}
			}
			i++
			continue
		}
		quote := c
		i++
		start := i
		var sb strings.Builder
		for i < n {
			ch := src[i]
			if ch == '\\' {
				i += 2
				continue
			}
			if ch == quote {
				break
			}
			if ch == '\n' && quote != '`' {
				// An unterminated ordinary string: not a literal, bail out.
				break
			}
			i++
		}
		if i <= n && i > start {
			lit := src[start:min(i, n)]
			if len(lit) >= minLiteral && looksLikeProse(lit) {
				sb.WriteString(lit)
				out = append(out, sb.String())
			}
		}
		i++
	}
	return out
}

// looksLikeProse rejects the literals that dominate a bundle and can never be a
// headline: CSS selectors, class-name soup, base64, hex, file paths.
func looksLikeProse(s string) bool {
	if len(s) > 4096 {
		return false
	}
	letters, spaces, other := 0, 0, 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
			letters++
		case c == ' ':
			spaces++
		case c >= 0x80:
			// Non-ASCII is likely real language in another script.
			letters++
		default:
			other++
		}
	}
	if letters == 0 {
		return false
	}
	// A phrase has spaces; an identifier or a hash does not.
	if spaces == 0 {
		return false
	}
	return letters > other
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
