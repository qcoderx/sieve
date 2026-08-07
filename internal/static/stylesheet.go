package static

import (
	"strings"
)

// hidingRules is what a bounded scan of the page's stylesheets found.
//
// # Why this exists at all
//
// The rendered tiers decide visibility by asking the browser. The static tier
// has no browser, so without this it can only see inline `style="display:none"`
// and the `hidden` attribute -- and almost nobody hides content that way. The
// overwhelmingly common form is a class.
//
// That gap matters more than it looks. sieve's central claim is that hidden
// text never reaches a default payload, and a claim that holds at tier 2 but
// not at tier 0 is not a claim: it becomes a property of which rung happened to
// answer, which is precisely the inconsistency that makes a tiered tool
// untrustworthy.
//
// So this is a deliberately small CSS scanner. It does not implement the
// cascade, specificity, media queries or inheritance. It finds rules whose
// declarations hide content and records their simple selectors, which covers
// the real cases (`.sr-only`, `.hidden`, `[hidden]`, `.visually-hidden`, a
// utility class from a framework) at a fraction of the cost of being correct in
// general. What it cannot resolve, it says so about rather than assuming
// visible.
type hidingRules struct {
	classes map[string]string // class name -> reason
	ids     map[string]string // id -> reason
	tags    map[string]string // tag name -> reason

	// complex counts rules that hide content through selectors this scanner
	// does not evaluate -- descendant combinators, pseudo-classes, attribute
	// matches beyond the simple forms. Their existence is reported so the
	// artifact can say its static visibility analysis was incomplete rather
	// than implying it was thorough.
	complex int
}

func newHidingRules() *hidingRules {
	return &hidingRules{
		classes: map[string]string{},
		ids:     map[string]string{},
		tags:    map[string]string{},
	}
}

func (h *hidingRules) empty() bool {
	return len(h.classes) == 0 && len(h.ids) == 0 && len(h.tags) == 0
}

// parseStylesheet folds one <style> block into the rule set.
func (h *hidingRules) parseStylesheet(css string) {
	css = stripCSSComments(css)

	// At-rules are skipped wholesale. A rule inside @media only applies at some
	// viewport, and treating it as unconditional would quarantine content that
	// is plainly visible on a desktop -- a false positive that loses real text,
	// which is worse than the false negative it prevents.
	for _, rule := range splitRules(css) {
		brace := strings.IndexByte(rule, '{')
		if brace < 0 {
			continue
		}
		selectors := strings.TrimSpace(rule[:brace])
		decls := rule[brace+1:]
		if selectors == "" || strings.HasPrefix(selectors, "@") {
			continue
		}
		reason := hidingReason(decls)
		if reason == "" {
			continue
		}
		for _, sel := range strings.Split(selectors, ",") {
			h.addSelector(strings.TrimSpace(sel), reason)
		}
	}
}

// hidingReason reports why a declaration block hides content, or empty.
func hidingReason(decls string) string {
	d := strings.ToLower(strings.ReplaceAll(decls, " ", ""))
	d = strings.ReplaceAll(d, "\t", "")
	d = strings.ReplaceAll(d, "\n", "")

	switch {
	case strings.Contains(d, "display:none"):
		return "display-none"
	case strings.Contains(d, "visibility:hidden"), strings.Contains(d, "visibility:collapse"):
		return "visibility-hidden"
	case strings.Contains(d, "opacity:0;"), strings.HasSuffix(d, "opacity:0"),
		strings.Contains(d, "opacity:0!"), strings.Contains(d, "opacity:0.0"):
		// A zero-opacity rule is ambiguous in a way display:none is not: it is
		// also how every scroll-reveal animation starts. The rendered tiers
		// resolve that by watching the opacity change; the static tier cannot,
		// so it treats the text as hidden and lets the escalation ladder
		// promote the page if there is any doubt.
		return "opacity-zero"
	}

	// Text the same colour as its own background is the third hiding
	// technique, and it defeats both of the others' defences. It is only
	// detectable here when one rule sets both.
	if fg, bg := declValue(d, "color"), declValue(d, "background-color"); fg != "" && fg == bg {
		return "colour-matches-background"
	}
	if fg, bg := declValue(d, "color"), declValue(d, "background"); fg != "" && bg != "" && strings.HasPrefix(bg, fg) {
		return "colour-matches-background"
	}
	return ""
}

func declValue(decls, prop string) string {
	i := strings.Index(decls, prop+":")
	if i < 0 {
		return ""
	}
	// Guard against matching "background-color" when asked for "color".
	if i > 0 && (isCSSNameByte(decls[i-1])) {
		rest := decls[i+1:]
		if j := strings.Index(rest, prop+":"); j >= 0 {
			i = i + 1 + j
		} else {
			return ""
		}
	}
	v := decls[i+len(prop)+1:]
	if j := strings.IndexByte(v, ';'); j >= 0 {
		v = v[:j]
	}
	return strings.TrimSuffix(strings.TrimSpace(v), "!important")
}

func isCSSNameByte(c byte) bool {
	return c == '-' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// addSelector records a selector if it is one of the simple forms this scanner
// evaluates, and counts it as complex otherwise.
func (h *hidingRules) addSelector(sel, reason string) {
	if sel == "" {
		return
	}
	// Anything with a combinator, a pseudo, or a compound beyond one token is
	// out of scope. Guessing at those would produce false positives, and a
	// false positive here quarantines visible content.
	if strings.ContainsAny(sel, " >+~:()[]*") {
		h.complex++
		return
	}
	switch sel[0] {
	case '.':
		if name := sel[1:]; name != "" {
			h.classes[name] = reason
		}
	case '#':
		if name := sel[1:]; name != "" {
			h.ids[name] = reason
		}
	default:
		// A bare tag selector that hides everything of that type. Rare but real
		// (`template`, a custom element before definition).
		h.tags[strings.ToLower(sel)] = reason
	}
}

// match reports why an element is hidden by the stylesheet, or empty.
func (h *hidingRules) match(tag, id, class string) string {
	if h.empty() {
		return ""
	}
	if id != "" {
		if r, ok := h.ids[id]; ok {
			return r
		}
	}
	if class != "" {
		for _, c := range strings.Fields(class) {
			if r, ok := h.classes[c]; ok {
				return r
			}
		}
	}
	if r, ok := h.tags[strings.ToLower(tag)]; ok {
		return r
	}
	return ""
}

// stripCSSComments removes /* ... */ so a commented-out rule cannot be read as
// live, and so a comment containing a brace cannot desynchronise the split.
func stripCSSComments(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if i+1 < len(s) && s[i] == '/' && s[i+1] == '*' {
			j := strings.Index(s[i+2:], "*/")
			if j < 0 {
				break
			}
			i += 2 + j + 2
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// splitRules cuts a stylesheet into rules, tracking nesting so that the body of
// an at-rule is not mistaken for a top-level rule.
func splitRules(css string) []string {
	var out []string
	depth := 0
	start := 0
	for i := 0; i < len(css); i++ {
		switch css[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth <= 0 {
				depth = 0
				out = append(out, css[start:i])
				start = i + 1
			}
		}
		if len(out) > 4000 {
			// A stylesheet with more rules than this is machine-generated and
			// the interesting ones are near the top. Remote input is untrusted;
			// an unbounded scan is a denial-of-service surface.
			return out
		}
	}
	return out
}
