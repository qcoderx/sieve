package graph

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// semanticLevel maps an HTML heading tag to its level, or 0.
func semanticLevel(tag, role string, ariaLevel int) int {
	switch tag {
	case "h1":
		return 1
	case "h2":
		return 2
	case "h3":
		return 3
	case "h4":
		return 4
	case "h5":
		return 5
	case "h6":
		return 6
	}
	if role == "heading" {
		if ariaLevel >= 1 && ariaLevel <= 6 {
			return ariaLevel
		}
		return 2
	}
	return 0
}

// typeScale is the ladder of heading sizes found on a page.
type typeScale struct {
	// body is the font size of running prose, the reference every other size is
	// judged against.
	body float64
	// steps are distinct heading sizes, largest first. Index 0 is level 1.
	steps []float64
	// semantic is true when the page used real heading tags, in which case
	// those are trusted over anything inferred.
	semantic bool
}

// buildTypeScale works out what counts as body text and what counts as a
// heading on this particular page.
//
// Absolute sizes are useless as a signal: 24px is a subheading on a marketing
// site and body copy on an editorial one. What is stable is the relationship
// between sizes within one document, so the scale is always relative to that
// page's own body size.
func buildTypeScale(cands []*candidate) typeScale {
	var ts typeScale

	// Body size is the size at which this page sets prose. Weighting by
	// character count rather than counting blocks is what makes it robust:
	// a page with forty short navigation links and six long paragraphs would
	// otherwise decide its body size from the navigation.
	weight := map[float64]int{}
	for _, c := range cands {
		if c.Region.IsChrome() {
			continue
		}
		n := utf8.RuneCountInString(c.Text)
		if n < 40 {
			continue // too short to be prose
		}
		weight[roundTo(c.Style.FontSize, 0.5)] += n
	}
	if len(weight) == 0 {
		// No prose at all. Fall back to weighting every block.
		for _, c := range cands {
			weight[roundTo(c.Style.FontSize, 0.5)] += utf8.RuneCountInString(c.Text)
		}
	}
	ts.body = weightedMedian(weight)
	if ts.body <= 0 {
		ts.body = 16
	}

	// Collect the sizes at which this page sets things that look like headings.
	sizeSet := map[float64]int{}
	for _, c := range cands {
		if c.Region.IsChrome() {
			continue
		}
		if c.SemanticLevel > 0 {
			ts.semantic = true
		}
		if !looksLikeHeading(c, ts.body) {
			continue
		}
		sizeSet[roundTo(c.Style.FontSize, 0.5)]++
	}
	sizes := make([]float64, 0, len(sizeSet))
	for s := range sizeSet {
		sizes = append(sizes, s)
	}
	sort.Sort(sort.Reverse(sort.Float64Slice(sizes)))

	// Collapse sizes that are within 8% of each other into one rung. Design
	// systems and responsive clamps produce 47.9px and 48px for what the author
	// thinks of as one size, and those must not become two heading levels.
	for _, s := range sizes {
		if len(ts.steps) > 0 && s > 0 && ts.steps[len(ts.steps)-1]/s < 1.08 {
			continue
		}
		ts.steps = append(ts.steps, s)
		if len(ts.steps) == 6 {
			break
		}
	}
	return ts
}

// looksLikeHeading is the gate for entering the type scale at all. It is
// deliberately generous, because the scale only decides relative level; the
// decision that something *is* a heading is made in classify.
func looksLikeHeading(c *candidate, body float64) bool {
	if c.SemanticLevel > 0 {
		return true
	}
	n := utf8.RuneCountInString(c.Text)
	if n == 0 || n > 180 {
		return false
	}
	if c.Style.FontSize >= body*1.15 {
		return true
	}
	// An all-caps line, set small and widely tracked, is the other common way
	// to write a heading on a design-led site -- the eyebrow or kicker above a
	// title. It is smaller than body text, so a size test alone never sees it.
	if c.Style.Uppercase && c.Style.Tracking >= 0.04*c.Style.FontSize && n <= 60 {
		return true
	}
	if c.Style.Weight >= 600 && c.Style.FontSize >= body*1.02 && n <= 90 {
		return true
	}
	return false
}

// level returns the heading level for a block, or 0 if it is not a heading.
func (ts typeScale) level(c *candidate) int {
	// An explicit heading tag is the author stating the level outright. No
	// inference beats that, and overriding it is how extraction tools produce
	// documents whose outline disagrees with the page's own accessibility tree.
	if c.SemanticLevel > 0 {
		return c.SemanticLevel
	}
	// Navigation and footers are set in their own type scale, unrelated to the
	// document's. Inferring headings there turns every menu item into a section
	// title and shatters the outline.
	if c.Region.IsChrome() {
		return 0
	}
	if !looksLikeHeading(c, ts.body) {
		return 0
	}
	size := roundTo(c.Style.FontSize, 0.5)
	for i, s := range ts.steps {
		if size >= s*0.96 {
			lv := i + 1
			// A small, tracked, all-caps kicker sits above a title visually but
			// below it in the outline. Placing it at the same level as the
			// large heading it introduces would produce two h1s per section.
			if c.Style.Uppercase && size < ts.body*1.15 && lv < 6 {
				lv++
			}
			return lv
		}
	}
	return len(ts.steps) + 1
}

// normaliseLevels repairs an outline that skips levels.
//
// A page whose only headings are 76px and 18px yields levels 1 and 2 from the
// scale, which is right. But a page whose largest heading is a 40px section
// title with no page title above it produces a document that starts at level 2
// or 3, and Markdown consumers reasonably expect the outline to start at 1 and
// not to jump. This walks the sequence and compresses gaps without ever
// reordering anything.
func normaliseLevels(blocks []Block) {
	var seen []int
	for _, b := range blocks {
		if b.Type == TypeHeading && b.Level > 0 && !b.Region.IsChrome() {
			seen = append(seen, b.Level)
		}
	}
	if len(seen) == 0 {
		return
	}
	uniq := map[int]bool{}
	for _, l := range seen {
		uniq[l] = true
	}
	levels := make([]int, 0, len(uniq))
	for l := range uniq {
		levels = append(levels, l)
	}
	sort.Ints(levels)
	remap := make(map[int]int, len(levels))
	for i, l := range levels {
		remap[l] = i + 1
	}
	for i := range blocks {
		if blocks[i].Type == TypeHeading && blocks[i].Level > 0 {
			if nl, ok := remap[blocks[i].Level]; ok {
				blocks[i].Level = nl
			}
		}
	}
}

// isUppercase reports whether a string is rendered in capitals, either because
// it was written that way or because text-transform made it so.
func isUppercase(s, transform string) bool {
	if transform == "uppercase" {
		return true
	}
	letters, upper := 0, 0
	for _, r := range s {
		if !unicode.IsLetter(r) {
			continue
		}
		letters++
		if unicode.IsUpper(r) {
			upper++
		}
	}
	// Requiring a few letters stops "OK" and "3D" from being read as styling.
	return letters >= 3 && upper == letters
}

// closesLikeProse reports whether a run of text ends the way a sentence does.
//
// A question mark or exclamation mark is not evidence: "Ready to begin?" and
// "New!" are ordinary headings. A full stop, comma, semicolon or ellipsis is,
// because headings essentially never carry one.
func closesLikeProse(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	if strings.HasSuffix(s, "...") || strings.HasSuffix(s, "…") {
		return true
	}
	switch s[len(s)-1] {
	case '.', ';', ',':
		// An abbreviation or an initial is not the end of a sentence.
		return !endsWithInitial(s)
	}
	return false
}

// endsWithInitial spots "Inc." and "J. R. R." style endings, where the full
// stop belongs to an abbreviation rather than to a sentence.
func endsWithInitial(s string) bool {
	if len(s) < 2 || s[len(s)-1] != '.' {
		return false
	}
	body := s[:len(s)-1]
	i := strings.LastIndexAny(body, " \t")
	last := body[i+1:]
	return utf8.RuneCountInString(last) <= 2
}

func roundTo(v, step float64) float64 {
	if step <= 0 {
		return v
	}
	return float64(int(v/step+0.5)) * step
}

// weightedMedian returns the key at which cumulative weight crosses half.
func weightedMedian(w map[float64]int) float64 {
	if len(w) == 0 {
		return 0
	}
	keys := make([]float64, 0, len(w))
	total := 0
	for k, v := range w {
		keys = append(keys, k)
		total += v
	}
	sort.Float64s(keys)
	half := total / 2
	run := 0
	for _, k := range keys {
		run += w[k]
		if run >= half {
			return k
		}
	}
	return keys[len(keys)-1]
}

// RoundTo is roundTo, exported for callers that recompute audit figures after
// Build has returned -- the scene walk appends text no DOM walk could have
// produced, and the retention ratio has to be redone when it does.
func RoundTo(v, step float64) float64 { return roundTo(v, step) }
