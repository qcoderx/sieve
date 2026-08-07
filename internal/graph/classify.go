package graph

import (
	"strings"
	"unicode/utf8"

	"github.com/qcoderx/sieve/internal/capture"
)

// candidate is a reassembled run on its way to becoming a block.
type candidate struct {
	Text  string
	Path  string
	Block string
	Tag   string
	Role  string
	Href  string
	BBox  capture.Box

	Checkpoint    int
	SemanticLevel int
	Style         StyleInfo
	Region        Region
	Fixed         bool
	EverVisible   bool

	// landmark is the nearest landmark ancestor the page declared, if any.
	landmark string
	// repeats counts identical runs elsewhere on the page.
	repeats int
	// mediaRef links an image block back to its media entry.
	mediaRef string

	// InvisibleColor marks text whose colour matched what was behind it.
	InvisibleColor bool

	// Declared marks a run kept on the page's own statement that it would be
	// revealed, rather than on sieve having seen it revealed. It is never
	// treated as verified and it is always reported in the audit.
	Declared bool

	// Keep is false for candidates that never became visible to anyone.
	Keep       bool
	DropReason string
	Confidence float64

	// SourceKind and Verified describe where the text came from and whether a
	// recovery from pixels was corroborated. Text from the DOM is neither
	// recovered nor in need of verification.
	SourceKind Source
	Verified   Verification

	Type  BlockType
	Level int
}

// minVisibleOpacity is shared with the capture layer rather than redeclared.
// The retention audit divides what this package emitted by what the capture
// counted as observed, and two thresholds that drifted apart would make that
// ratio meaningless.
//
// Scroll-reveal animations start at 0 and finish at 1, so the test is always
// against the maximum opacity ever observed, never against one checkpoint.
const minVisibleOpacity = capture.MinVisibleOpacity

// edgeBand is how close to the top or bottom of the viewport a pinned run has
// to sit before it can be read as furniture, as a fraction of viewport height.
// A sticky bar is a strip; 18% of a 900px viewport is 162px, which comfortably
// covers a tall header with a logo and still excludes anything occupying the
// body of the screen.
//
// maxChromeRunLen is the other half of that test. Navigation is labels and
// content is sentences, so a run long enough to be prose is not furniture no
// matter where it is pinned.
const (
	edgeBand        = 0.18
	maxChromeRunLen = 60
)

// classify decides, for each reassembled run, which part of the page it belongs
// to and whether it is content at all.
//
// The decision is not "is this in <main>". Most design-led sites have no
// landmarks whatsoever. It comes from combining what evidence the page does
// give: where the run sits, whether it is pinned to the viewport, whether it
// ever became visible, how it is set in type, and how it repeats.
func classify(groups []*group, m *capture.Merged) []*candidate {
	cands := make([]*candidate, 0, len(groups))
	for _, g := range groups {
		for _, r := range g.Runs {
			c := candidateFrom(r)
			if c != nil {
				cands = append(cands, c)
			}
		}
	}

	assignRegions(cands, m)
	markRepeats(cands)

	ts := buildTypeScale(cands)
	for _, c := range cands {
		c.Level = ts.level(c)
		c.Type = blockType(c, ts)
		if c.Type != TypeHeading {
			// A level on a paragraph is not merely useless, it is misleading:
			// it says "this was considered a heading and then wasn't", and a
			// consumer that reads level without checking type builds the wrong
			// outline.
			c.Level = 0
		}
		c.Confidence = confidenceOf(c, ts)
	}
	return cands
}

func candidateFrom(r *run) *candidate {
	text := strings.TrimSpace(r.Text)
	if text == "" {
		return nil
	}
	lead := r.Lead
	c := &candidate{
		Text:       text,
		Path:       lead.Path,
		Block:      r.Block,
		Tag:        lead.Tag,
		Role:       lead.Role,
		Href:       lead.Href,
		BBox:       r.BBox,
		Checkpoint: lead.Checkpoint,
		landmark:   lead.Landmark,
		Fixed:      r.Fixed,
		// EverVisible means the run was inside the viewport at some checkpoint.
		// A run can be legitimately content and never satisfy this if the sweep
		// was cut short, so it lowers confidence rather than dropping the run.
		EverVisible: r.EverVisible,
		Style: StyleInfo{
			FontSize:   lead.FontSize,
			Weight:     lead.Weight,
			Tracking:   lead.Tracking,
			Uppercase:  isUppercase(text, lead.Transform),
			Italic:     lead.Italic,
			Family:     lead.Family,
			MaxOpacity: r.MaxOpacity,
		},
		InvisibleColor: lead.InvisibleColor,
		SourceKind:     SourceDOM,
		Keep:           true,
	}
	c.SemanticLevel = semanticLevel(c.Tag, c.Role, 0)

	// Content that never became even faintly visible at any point in the sweep
	// was not shown to anybody, so it is not content. This is the rendering-
	// grounded defence the project leads with: it is decided by what the browser
	// reported, not by guessing from class names, and a markup-based extractor
	// ingests all of it verbatim.
	if r.MaxOpacity < minVisibleOpacity {
		// A run the page declared it would reveal is kept, and labelled.
		//
		// The opacity rule stays exactly as strict about what it will vouch
		// for: this run is not marked as seen, it is marked as declared. What
		// changes is that a scroll-driven site no longer loses its entire
		// argument because the sweep could not afford to stop at the precise
		// offset that fades each section in. On pear.no that is the difference
		// between an artifact with the terms, the selectivity and the
		// application in it and one without them.
		//
		// The distinction is the page's own: `transition: opacity` is written
		// by an author who intends the text to be read. Text hidden in order to
		// stay hidden -- the injection case this filter exists for -- carries no
		// such declaration and is still dropped here.
		if r.Revealable {
			c.Declared = true
		} else {
			c.Keep = false
			c.DropReason = "never reached visible opacity"
		}
	}
	// Text the same colour as its background renders at full opacity and is
	// still unreadable, which is exactly how the opacity defence is bypassed.
	// It is excluded from the content tier for the same reason.
	if lead.InvisibleColor {
		c.Keep = false
		c.DropReason = "text colour matches background"
	}
	return c
}

// assignRegions places each run in a part of the page.
func assignRegions(cands []*candidate, m *capture.Merged) {
	docH := m.DocHeight
	vh := m.ViewportH
	if vh <= 0 {
		vh = 900
	}

	for _, c := range cands {
		// The page's own landmarks are the best evidence there is, when present.
		switch leadLandmark(c) {
		case "nav":
			c.Region = RegionNav
			continue
		case "header":
			c.Region = RegionHeader
			continue
		case "footer":
			c.Region = RegionFooter
			continue
		case "aside":
			c.Region = RegionAside
			continue
		case "form":
			c.Region = RegionForm
			continue
		case "dialog":
			c.Region = RegionDialog
			continue
		}

		// A run pinned to the viewport is usually beside the content rather than
		// in it, and that is the signal that keeps a sticky header out of the
		// middle of chapter four. But "pinned" alone is not enough, because
		// pinned sections are a whole genre of site: the page holds a panel
		// still while the scroll drives an animation, swaps in the next panel,
		// and holds that. Every word of the argument is pinned.
		//
		// Splitting the viewport in half and calling the top header and the
		// bottom footer classifies such a page as pure chrome. pear.no is built
		// this way, and that rule filed all thirty-four blocks of its extracted
		// body copy as furniture: an artifact reporting no content for a page
		// whose text had been captured perfectly.
		//
		// Furniture has two properties this does not. It hugs an edge of the
		// viewport -- that is what makes it furniture rather than page -- and it
		// is short, because navigation is labels and content is sentences.
		// Requiring both leaves the pinned-section genre in the reading order
		// while still catching sticky bars, which is the case that mattered.
		if c.Fixed {
			atTop := c.BBox.Bottom() <= vh*edgeBand
			atBottom := c.BBox.Y() >= vh*(1-edgeBand)
			brief := utf8.RuneCountInString(c.Text) <= maxChromeRunLen
			switch {
			case brief && atTop:
				c.Region = RegionHeader
				continue
			case brief && atBottom:
				c.Region = RegionFooter
				continue
			}
			// Pinned, but neither brief nor at an edge: this is a held section,
			// so it falls through to the ordinary tests and keeps its place.
		}

		// Without landmarks, position is what is left. Text in the first screen
		// that is short, small and linked is a menu; text in the last screen
		// that is short and linked is a footer.
		short := utf8.RuneCountInString(c.Text) <= 40
		if docH > 0 && c.BBox.Bottom() > docH-vh*0.75 && short && c.Href != "" {
			c.Region = RegionFooter
			continue
		}
		if c.BBox.Y() < vh*0.12 && short && c.Href != "" {
			c.Region = RegionNav
			continue
		}
		c.Region = RegionMain
	}
}

func leadLandmark(c *candidate) string {
	return c.landmark
}

// markRepeats demotes text that appears many times over.
//
// A phrase that occurs identically in five places is a template, not an
// argument: a "Read more" on every card, a repeated legal line, a menu label
// echoed in the footer. Repetition alone is not proof -- a genuine list can
// repeat a word -- so it lowers confidence and only reclassifies short runs.
func markRepeats(cands []*candidate) {
	counts := make(map[string]int, len(cands))
	for _, c := range cands {
		if utf8.RuneCountInString(c.Text) <= 60 {
			counts[strings.ToLower(c.Text)]++
		}
	}
	for _, c := range cands {
		n := counts[strings.ToLower(c.Text)]
		if n < 3 {
			continue
		}
		c.repeats = n
		if c.Region == RegionMain && c.Href != "" && utf8.RuneCountInString(c.Text) <= 24 && n >= 4 {
			c.Region = RegionNav
		}
	}
}

// blockType decides the shape of a block.
//
// The order of the tests matters. A tag the author chose deliberately -- a
// blockquote, a list item, a caption -- outranks anything inferred from
// typography, because a large italic pull quote is still a quote no matter how
// heading-like it is set. Only a real heading tag outranks the tag test, and
// that case is already folded into Level.
func blockType(c *candidate, ts typeScale) BlockType {
	switch c.Tag {
	case "h1", "h2", "h3", "h4", "h5", "h6":
		return TypeHeading
	case "li", "dt", "dd":
		return TypeListItem
	case "blockquote", "q":
		return TypeQuote
	case "code", "pre", "samp", "kbd":
		return TypeCode
	case "td", "th":
		return TypeTable
	case "label", "legend":
		return TypeLabel
	case "figcaption", "cite", "caption":
		return TypeLabel
	}

	if c.Level > 0 {
		// Prose that happens to be set large is still prose. A run that closes
		// like a sentence is the clearest evidence available that it is one --
		// a lede paragraph set at 22px above 17px body copy trips every size
		// test there is, and the full stop at the end is what gives it away.
		//
		// Question and exclamation marks are excluded: "Ready to begin?" is a
		// perfectly ordinary heading.
		if !closesLikeProse(c.Text) {
			return TypeHeading
		}
	}
	if c.Style.Family != "" && isMonospaceFamily(c.Style.Family) &&
		utf8.RuneCountInString(c.Text) > 12 {
		return TypeCode
	}
	// A large, italic, short run set well above body size is a pull quote in
	// every design system, whatever tag it happens to use.
	if c.Style.Italic && c.Style.FontSize >= ts.body*1.3 &&
		utf8.RuneCountInString(c.Text) <= 300 {
		return TypeQuote
	}
	return TypeParagraph
}

func isMonospaceFamily(f string) bool {
	l := strings.ToLower(f)
	for _, m := range []string{"mono", "courier", "consolas", "menlo", "fira code", "source code"} {
		if strings.Contains(l, m) {
			return true
		}
	}
	return false
}

// confidenceOf scores how sure we are that a block is what we say it is.
//
// This is not decoration. The benchmark grades fidelity, and a consumer that
// cannot tell a certain extraction from a marginal one has no way to weigh what
// it reads. Everything that reduces certainty is subtracted explicitly.
func confidenceOf(c *candidate, ts typeScale) float64 {
	conf := 1.0

	// Text recovered from the DOM is exact; what is uncertain is our reading of
	// its role. Faint text may be a design choice or may be a decorative
	// artefact caught mid-animation.
	if c.Style.MaxOpacity < 0.5 {
		conf -= 0.25
	} else if c.Style.MaxOpacity < 0.9 {
		conf -= 0.08
	}

	// EverVisible is deliberately not a factor here.
	//
	// It records whether a run happened to be inside the viewport at one of the
	// sampled checkpoints, which is a property of where the sweep stopped
	// rather than of the content. A block near the foot of the page can fall
	// between two checkpoints on one run and inside one on the next, purely
	// because a font loaded a pixel differently and changed the document
	// height. Feeding that into confidence made the same page report different
	// confidence on consecutive runs -- caught by the golden corpus, and
	// exactly the kind of wavering that makes a self-audit worthless.
	//
	// Opacity is the real visibility signal and it is deterministic, so it
	// carries this judgement alone.
	// A heading asserted purely from typography is a judgement; one carried by
	// an <h2> is a fact.
	if c.Type == TypeHeading && c.SemanticLevel == 0 {
		conf -= 0.1
		if !ts.semantic {
			// The page has no semantic headings at all, so there is nothing to
			// calibrate the inference against.
			conf -= 0.05
		}
	}
	if c.repeats >= 4 {
		conf -= 0.05
	}
	// A run that is a single character survived reassembly alone, which usually
	// means its neighbours were positioned in a way the line grouping could not
	// follow.
	if utf8.RuneCountInString(c.Text) == 1 {
		conf -= 0.3
	}
	if conf < 0.1 {
		conf = 0.1
	}
	return roundTo(conf, 0.01)
}
