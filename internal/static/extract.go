// Package static extracts content from served HTML without a browser.
//
// This is tier 0 of the escalation ladder, and it is what stops sieve from
// being "an expensive tool that is overkill for a blog". A plain GET plus this
// package answers a documentation page in under a second. The browser is
// reserved for pages that actually need it, and the decision is recorded in the
// artifact so a caller can always see which tier answered.
//
// The extraction here is deliberately conventional: readability-style scoring
// over the DOM, boilerplate removal, and the same block vocabulary the rendered
// path produces. What matters is not that it is clever but that its output
// slots into the same graph, so the two tiers cannot drift into producing
// different shapes for the same page.
package static

import (
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	"github.com/qcoderx/sieve/internal/capture"
)

// Result is a static extraction, shaped so the graph builder can consume it
// exactly as it consumes a rendered capture.
type Result struct {
	Merged *capture.Merged
	// Signals feed the escalation scorer.
	Signals Signals
	// RawHTML is retained for the change-detection ladder and for the token
	// comparison that justifies the project.
	RawHTML string
}

// Signals are the measurements the escalation scorer reads.
//
// They are all cheap and all computed from the served bytes, which is the whole
// point: the decision about whether to spend thirty seconds on a browser has to
// cost far less than thirty seconds.
type Signals struct {
	// HTMLBytes is the size of the served document.
	HTMLBytes int
	// TextChars is how much readable text static extraction found.
	TextChars int
	// MarkupChars is how much text a naive tag-strip finds in the same bytes.
	//
	// The two together say whether static extraction is getting the page or
	// only part of it. TextChars alone cannot: three thousand characters reads
	// as a substantial page and the escalator was scoring it that way, while a
	// tag-strip of the same document found nine thousand. cuberto.com lost
	// every one of its service descriptions that way -- the escalator saw
	// "substantial text served statically", stayed at tier 0, and returned 25
	// of 34 ground-truth facts where the browser returns all 34.
	//
	// This is a crude count on purpose. It includes text a visitor never sees,
	// which is exactly why it is a floor for suspicion rather than a target:
	// the question it answers is "is there plainly more here than we extracted",
	// and only a large shortfall is worth a browser.
	MarkupChars int
	// TextRatio is TextChars over HTMLBytes. A documentation page is text with
	// a little markup around it; an application shell is markup with no text.
	TextRatio float64
	// Headings, Paragraphs and Landmarks measure structural richness. A page
	// that arrives with a real outline probably does not need rendering.
	Headings   int
	Paragraphs int
	Landmarks  int
	// Links and Images are counted because a page with many links and no prose
	// is usually an index, which static extraction handles fine.
	Links  int
	Images int
	// CanvasElements and CanvasWithSize hint at a WebGL-driven page before any
	// browser has run.
	CanvasElements int
	// ScriptBytes is how much JavaScript the page ships. A megabyte of bundle
	// with two kilobytes of text is the signature of a client-rendered site.
	ScriptBytes int
	// NoScriptWarning is set when the page ships a "you need JavaScript"
	// message, which is an explicit statement that static extraction will fail.
	NoScriptWarning bool
	// HydrationBlob is set when a framework payload is present, which means the
	// served HTML may be a shell.
	HydrationBlob bool
	// Title and Description come from the head.
	Title       string
	Description string

	// ExternalStylesheets and ComplexHidingRules record how much of the page's
	// visibility logic the static tier could not evaluate.
	//
	// This is the honest counterpart to the CSS scan: an external stylesheet is
	// not fetched, and a selector with a combinator is not resolved. Reporting
	// the counts lets the escalation scorer treat a page whose hiding logic is
	// out of reach as one worth rendering, instead of quietly assuming
	// everything it could not analyse was visible.
	ExternalStylesheets int
	ComplexHidingRules  int

	// FoldedControls and FoldedChars count the disclosures the served document
	// ships shut, and how much text they hold.
	//
	// This is the one shortfall tier 0 can see coming and can never repair. A
	// closed <details>, a panel with aria-hidden="true", an unselected tab: the
	// text is in the bytes, so MarkupChars counts it and no shortfall is
	// detected, but a reader gets it by pressing something and tier 0 has
	// nothing to press with. The page then scores as a well-served static
	// document while a third of what it says stays folded.
	//
	// Without this signal the disclosure prober was mostly unreachable in
	// practice. It only ran on pages that had already escalated for some other
	// reason, so a small, fast, entirely static page whose pricing sits behind
	// a tab -- which describes a great many real pages -- was answered from
	// tier 0 with the pricing missing and no indication anything was.
	FoldedControls int
	FoldedChars    int

	// TextRuns and ShortRuns detect split text: markup where an animation
	// library has shattered a heading into one element per character or per
	// word so each can be tweened separately.
	//
	// This is the one thing tier 0 cannot repair. Putting the pieces back
	// together needs the rendered line box and the computed style -- which
	// fragments shared a line, in what order they sat on it -- and the served
	// HTML has neither, so tier 0 emits the fragments in document order. When
	// the library also reorders the source for its effect, the result is not
	// merely ugly, it is wrong: organimo.com yields "Liitless m", "Te real h"
	// and a heading spelled out as "e / v / er / n / eed."
	//
	// Every other escalation signal here asks whether the text is present.
	// This one asks whether it is legible, which is a different question and
	// the only one that catches a page that serves all of its copy and none of
	// it readably.
	TextRuns  int
	ShortRuns int
}

// maxShatteredRun is the length, in runes, at or below which a text run looks
// like a fragment rather than a phrase. Two covers per-character splitting and
// the diacritic pairs it produces, without catching ordinary short words.
const maxShatteredRun = 2

// SplitTextRatio is the share of text runs short enough to be fragments.
func (s Signals) SplitTextRatio() float64 {
	if s.TextRuns == 0 {
		return 0
	}
	return float64(s.ShortRuns) / float64(s.TextRuns)
}

// blockTags are the elements whose text becomes a block.
var blockTags = map[atom.Atom]capture.Node{}

// skipTags never contain readable content.
var skipTags = map[atom.Atom]bool{
	atom.Script: true, atom.Style: true, atom.Noscript: true,
	atom.Template: true, atom.Head: true, atom.Link: true, atom.Meta: true,
	atom.Svg: true, atom.Iframe: true, atom.Object: true, atom.Embed: true,
}

// boilerplateClasses are the class and id substrings that mark page furniture.
// This is a heuristic and it is applied only to decide region, never to drop
// content outright: a "sidebar" class on the main article is unusual but it
// happens, and silently discarding it would be worse than mislabelling it.
var boilerplateHints = []string{
	"nav", "menu", "header", "footer", "sidebar", "breadcrumb", "cookie",
	"banner", "advert", "promo", "social", "share", "subscribe", "newsletter",
	"related", "recommend", "comment", "pagination", "skip-link",
}

// Extract parses served HTML into the same shape a rendered sweep produces.
func Extract(pageURL string, body io.Reader, sizeHint int) (*Result, error) {
	raw, err := io.ReadAll(body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	doc, err := html.Parse(strings.NewReader(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("parse HTML: %w", err)
	}

	ex := &extractor{
		url:     pageURL,
		acc:     capture.NewAccumulator(),
		signals: Signals{HTMLBytes: len(raw)},
		seen:    map[string]bool{},
		hiding:  newHidingRules(),
		lmSeen:  map[atom.Atom]int{},
		lmTotal: countPageLandmarks(doc),
	}
	// Stylesheets are scanned before the walk, because visibility has to be
	// known at the moment an element is visited. Almost nobody hides content
	// with an inline style; they use a class, and without this pass the static
	// tier would ingest exactly the material the rendered tiers quarantine.
	ex.scanStyles(doc)
	ex.walk(doc, "html", "html", "", "", 0, false, 0)

	snap := &capture.Snapshot{
		Checkpoint:   0,
		ViewportW:    1440,
		ViewportH:    900,
		DocHeight:    float64(ex.lineNo+1) * 40,
		Nodes:        ex.nodes,
		Latent:       ex.latent,
		Actions:      ex.actions,
		MediaItems:   ex.media,
		VisibleChars: ex.signals.TextChars,
		Meta: &capture.Meta{
			Title:       ex.signals.Title,
			Description: ex.signals.Description,
			Lang:        ex.lang,
			URL:         pageURL,
			OpenGraph:   ex.openGraph,
			JSONLD:      ex.jsonLD,
		},
	}
	ex.acc.Add(snap)

	if ex.signals.HTMLBytes > 0 {
		ex.signals.TextRatio = float64(ex.signals.TextChars) / float64(ex.signals.HTMLBytes)
	}
	ex.signals.MarkupChars = markupTextChars(string(raw))

	return &Result{
		Merged:  ex.acc.Result(),
		Signals: ex.signals,
		RawHTML: string(raw),
	}, nil
}

type extractor struct {
	url       string
	acc       *capture.Accumulator
	nodes     []capture.Node
	latent    []capture.LatentNode
	actions   []capture.Action
	media     []capture.Media
	signals   Signals
	lang      string
	openGraph map[string]string
	jsonLD    []string
	seen      map[string]bool
	order     int
	hiding    *hidingRules

	// line, lineX and lineBlock lay out the synthetic geometry: one line per
	// block-level container, with fragments placed along it in document order.
	lineNo int
	lineW  map[int]float64

	// lmSeen and lmTotal resolve which <header> and <footer> are the page's own,
	// matching the rendered walk exactly. Tier 0 and tier 2 have to agree about
	// what counts as chrome or the artifact changes shape purely with the tier
	// that answered.
	lmSeen  map[atom.Atom]int
	lmTotal map[atom.Atom]int
}

// countPageLandmarks counts <header> and <footer> elements outside sectioning
// content, which the walk needs up front: it cannot know which footer is the
// last until it has passed them all, but must classify each as it arrives.
func countPageLandmarks(root *html.Node) map[atom.Atom]int {
	out := map[atom.Atom]int{}
	var walk func(*html.Node, bool)
	walk = func(n *html.Node, sectioned bool) {
		if n.Type == html.ElementNode {
			if !sectioned && (n.DataAtom == atom.Header || n.DataAtom == atom.Footer) {
				out[n.DataAtom]++
			}
			if sectioningTags[n.DataAtom] {
				sectioned = true
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, sectioned)
		}
	}
	walk(root, false)
	return out
}

// pageLandmark reports whether this <header> or <footer> is the document's
// banner or contentinfo. There is at most one of each: the first page-level
// header and the last page-level footer. Every other one wraps a section and is
// content, which is what keeps a page built out of nineteen <header> elements
// from being filed away as nineteen banners and no content at all.
func (e *extractor) pageLandmark(a atom.Atom) bool {
	idx := e.lmSeen[a]
	e.lmSeen[a] = idx + 1
	if a == atom.Header {
		return idx == 0
	}
	total := e.lmTotal[a]
	if total == 0 {
		total = 1
	}
	return idx == total-1
}

// scanStyles walks the document for <style> blocks and folds them into the
// hiding-rule set.
func (e *extractor) scanStyles(root *html.Node) {
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.DataAtom == atom.Style {
			var sb strings.Builder
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				if c.Type == html.TextNode {
					sb.WriteString(c.Data)
				}
			}
			e.hiding.parseStylesheet(sb.String())
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)

	// An external stylesheet is not fetched: doing so would turn tier 0 into a
	// multi-request operation and it would still be an approximation. Its
	// existence is recorded instead, so the artifact can say the static
	// visibility analysis was partial.
	e.signals.ExternalStylesheets = countExternalStylesheets(root)
	e.signals.ComplexHidingRules = e.hiding.complex
	e.signals.FoldedControls, e.signals.FoldedChars = countFolded(root)
}

// countFolded measures the text the served document ships behind a shut control.
//
// The three shapes here are the same three the render tier will actually press,
// deliberately: counting a fourth kind would promise a browser something it will
// not open, and the point of the signal is to predict what escalating would
// recover. Anything a visitor must assert -- an age gate, a consent banner, a
// purchase -- is not in this list on either side.
//
// A folded region's descendants are not walked again once it has been counted,
// so an accordion inside a closed tab is charged once rather than twice.
func countFolded(root *html.Node) (controls, chars int) {
	// Panels named by a collapsed control, resolved before the walk so a
	// control that appears after its panel in document order still counts.
	collapsed := map[string]bool{}
	var findControls func(*html.Node)
	findControls = func(n *html.Node) {
		if n.Type == html.ElementNode && strings.EqualFold(attr(n, "aria-expanded"), "false") {
			// A control the prober will refuse to press names content that is
			// never going to be revealed, and counting it argues for a browser
			// on the strength of a cookie banner. The two lists have to agree
			// or the signal promises something the tier below it will decline
			// to do, which on the open web means arguing for a browser on
			// nearly every page, since a consent control is the single most
			// common collapsed thing there is.
			if !refusedLabel(textOfNode(n)) {
				for _, id := range strings.Fields(attr(n, "aria-controls")) {
					collapsed[id] = true
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			findControls(c)
		}
	}
	findControls(root)

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			folded := false
			switch {
			// A <details> without the open attribute. Presence, not value:
			// `open` is a boolean attribute, so the markup a page actually
			// ships for an expanded section is <details open>, whose value is
			// the empty string and reads as absent to anything comparing.
			case n.DataAtom == atom.Details && !hasAttr(n, "open"):
				folded = true
			// A panel the page has hidden from assistive technology, which is
			// how an unselected tab is nearly always marked.
			case strings.EqualFold(attr(n, "aria-hidden"), "true"):
				folded = true
			// A region named by a control reporting itself collapsed.
			case collapsed[attr(n, "id")]:
				folded = true
			}
			if folded {
				if c := foldedProse(n); c > 0 {
					controls++
					chars += c
				}
				return
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	return controls, chars
}

// minFoldedRegion is the least a single shut region can hold and still count.
// Roughly two sentences: below that a region is a label, a badge or a menu item,
// and a browser spent to open it buys nothing.
const minFoldedRegion = 120

// refuseWords mirrors REFUSE_WORDS in capture.js. The two are separate because
// one runs in a browser and one runs over served bytes, and they must stay in
// step: this side decides whether to argue for a browser, that side decides
// whether to press. Disagreement means spending a browser to be refused.
var refuseWords = regexp.MustCompile(`(?i)\b(18|21)\+?\b|over\s*(18|21)|of\s+legal\s+age|\bi\s*am\b|i'?m\s+over|accept|agree|consent|cookie|privacy|terms|gdpr|allow\s+all|sign\s*in|sign\s*up|log\s*in|register|subscribe|newsletter|\bbuy\b|purchase|checkout|add\s+to\s+(cart|bag)|\bpay\b|\border\b|donate|delete|remove|submit|\bsend\b|download|install`)

func refusedLabel(s string) bool {
	return refuseWords.MatchString(s)
}

// textOfNode returns a node's text, for reading a control's label.
func textOfNode(n *html.Node) string {
	var sb strings.Builder
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.TextNode {
			sb.WriteString(node.Data)
			sb.WriteByte(' ')
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return sb.String()
}

// foldedProse returns the text in a shut region, or zero if the region is
// navigation rather than reading matter.
//
// This distinction is the whole difference between a useful signal and one that
// sends a quarter of the web to a browser. Measured across two hundred sites,
// counting every shut region escalated 24.7% of them, and the reason was not
// that a quarter of the web folds its prose away: it was closed dropdown menus,
// off-screen mobile navigation and decorative panels marked aria-hidden. Those
// pages have hundreds of "shut controls" holding ten characters each, and
// opening every one of them buys a second copy of a menu that was already in
// the artifact.
//
// A folded menu is almost entirely link text. A folded specification, FAQ or
// price list is almost entirely not. That ratio separates them cleanly where a
// character count cannot: mlb.com folds 3,323 characters across 329 regions and
// none of it is prose, while a single collapsed pricing panel holds a few
// hundred characters with no links in it at all.
func foldedProse(n *html.Node) int {
	// A <nav> is furniture by declaration, and so is anything with a menu role.
	if n.DataAtom == atom.Nav {
		return 0
	}
	switch strings.ToLower(attr(n, "role")) {
	case "navigation", "menu", "menubar", "toolbar":
		return 0
	}

	total := markupTextChars(renderNode(n))
	if total == 0 {
		return 0
	}
	linked := 0
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode && (node.DataAtom == atom.A || node.DataAtom == atom.Button) {
			linked += markupTextChars(renderNode(node))
			return
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)

	// Over half of it being link and button labels means this is something to
	// navigate with, not something to read.
	if linked*2 >= total {
		return 0
	}

	// And a region has to hold enough to be worth a browser on its own.
	//
	// The link ratio catches menus; this catches everything else that folds a
	// line at a time. On the corpus the two populations barely overlap: pages
	// that fold reading matter hold between 133 and 7,854 characters per shut
	// region, and pages whose shut regions are decoration hold 7 to 60. mlb.com
	// folds 2,039 characters across 287 regions, which is seven characters each
	// and not a fact among them.
	if total < minFoldedRegion {
		return 0
	}
	return total
}

// renderNode serialises a subtree so its text can be counted with the same
// tag-stripping used for the document as a whole. Counting the two differently
// is how a shortfall gets compared against a number that was never measuring
// the same thing.
func renderNode(n *html.Node) string {
	var sb strings.Builder
	if err := html.Render(&sb, n); err != nil {
		return ""
	}
	return sb.String()
}

func countExternalStylesheets(root *html.Node) int {
	n := 0
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode && node.DataAtom == atom.Link {
			if strings.Contains(strings.ToLower(attr(node, "rel")), "stylesheet") {
				n++
			}
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	return n
}

// walk descends the parsed tree, carrying the same inherited facts the rendered
// walk does so that the two produce comparable output.
func (e *extractor) walk(n *html.Node, path, blockPath, landmark, href string, depth int, sectioned bool, parentLine int) {
	switch n.Type {
	case html.ElementNode:
		if skipTags[n.DataAtom] {
			switch n.DataAtom {
			case atom.Script:
				e.collectScript(n)
			case atom.Head:
				e.collectHead(n)
			case atom.Noscript:
				// A "you need JavaScript" message is the page stating outright
				// that static extraction will not work. It is the single most
				// direct escalation signal available, and it can appear in the
				// head or the body, so it is checked here rather than in either.
				e.checkNoScript(n)
			}
			return
		}
		if lm := landmarkOf(n, sectioned); lm != "" {
			if n.DataAtom == atom.Header || n.DataAtom == atom.Footer {
				if e.pageLandmark(n.DataAtom) {
					landmark = lm
					e.signals.Landmarks++
				}
			} else {
				landmark = lm
				e.signals.Landmarks++
			}
		}
		if sectioningTags[n.DataAtom] {
			sectioned = true
		}
		if n.DataAtom == atom.Html {
			e.lang = attr(n, "lang")
		}
		if n.DataAtom == atom.A {
			if h := attr(n, "href"); h != "" {
				href = h
				e.collectLink(n, path, h, landmark)
			}
		}
		if n.DataAtom == atom.Canvas {
			e.signals.CanvasElements++
		}
		if n.DataAtom == atom.Img {
			e.collectImage(n, path)
		}
		if n.DataAtom == atom.Form {
			e.collectForm(n, path, landmark)
		}
		// Hidden content goes to the latent tier, exactly as it does in the
		// rendered path. A static extractor that silently ingested hidden text
		// would have a different security posture from the rendered one, which
		// is precisely the kind of divergence between tiers that makes a tiered
		// tool untrustworthy.
		if reason := e.hiddenReason(n); reason != "" {
			// Structured data is harvested from hidden subtrees too, because
			// "hidden" is not a property structured data can have.
			//
			// A <script type="application/ld+json"> renders nowhere: inside a
			// visible section and inside a collapsed one it is equally invisible
			// to every visitor, and which of the two an author chose is an
			// accident of where the section's markup happened to be written.
			// pear.no puts its entire FAQPage block inside the FAQ section it
			// describes, and that section is aria-hidden until it scrolls into
			// view -- so the walk turned back at the door and the artifact lost
			// five questions and answers that were sitting in the served bytes.
			//
			// This does not widen the content channel. Nothing here becomes a
			// block; it goes to the same whitelist, with the same caps, as
			// structured data found anywhere else.
			e.harvestStructured(n, 0)
			e.collectLatent(n, path, blockPath, landmark, href, depth, reason)
			return
		}

		if isBlockish(n.DataAtom) {
			blockPath = path
		}

	case html.TextNode:
		return
	}

	// Children and this element's own text, interleaved in document order.
	//
	// The element's text used to be emitted in one piece before any recursion,
	// which put every fragment of a paragraph ahead of the link that belongs in
	// the middle of it. Order is the whole product at this tier -- there is no
	// geometry to fall back on, and source order is the only evidence of
	// reading order there is -- so the walk has to preserve it exactly.
	// Which synthetic line this element's text belongs on.
	//
	// One line is one element's own text flow: the words it holds directly,
	// plus any inline children standing between them. That is exactly the span
	// the reassembler should be able to join back into a sentence, and exactly
	// the span a browser would lay out as continuous text.
	//
	// An element with no text of its own is only a container, and its children
	// each start fresh. The distinction matters: danluu.com lists posts as
	// <li><d>08/26</d><a>How do programming languages…</a></li>, where the <li>
	// holds no words itself, and putting a bare container's children on one
	// line welded the date onto the title as "08/26How do programming…" -- a
	// missing space invents a word that is not on the page.
	line := parentLine
	if line == 0 && ownsText(n) {
		e.lineNo++
		line = e.lineNo
	}

	counts := map[string]int{}
	var sb strings.Builder
	flush := func() {
		e.emitFragment(n, sb.String(), path, blockPath, landmark, href, depth, line)
		sb.Reset()
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		switch {
		case c.Type == html.TextNode:
			sb.WriteString(c.Data)
			continue
		case c.Type != html.ElementNode:
			continue
		case c.DataAtom == atom.Br:
			// A <br> between two text children is a word boundary, and
			// concatenating across it welds the words together: "African soul."
			// and "European cut." on the two lines of a heading come back as
			// "African soul.European cut."
			sb.WriteByte(' ')
			continue
		case skipTags[c.DataAtom]:
			// Contributes no text, so it is not a boundary in the prose.
		default:
			flush()
		}
		tag := c.Data
		idx := counts[tag]
		counts[tag] = idx + 1
		seg := tag
		if idx > 0 {
			seg = fmt.Sprintf("%s[%d]", tag, idx)
		}
		// An inline child continues this element's line; a block-level one
		// begins its own, as it would on screen.
		childLine := 0
		if !isBlockish(c.DataAtom) {
			childLine = line
		}
		e.walk(c, path+"/"+seg, blockPath, landmark, href, depth+1, sectioned, childLine)
	}
	flush()
}

// ownsText reports whether an element holds any words of its own, as opposed to
// only holding other elements.
func ownsText(n *html.Node) bool {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.TextNode && strings.TrimSpace(c.Data) != "" {
			return true
		}
	}
	return false
}

// emitFragment records one stretch of text with its surrounding whitespace.
func (e *extractor) emitFragment(n *html.Node, raw, path, blockPath, landmark, href string, depth, line int) {
	text := normalizeSpace(raw)
	if text == "" {
		return
	}
	sb := strings.Builder{}
	sb.WriteString(raw)

	// Whitespace that surrounded this fragment in the source, recorded the way
	// the rendered walk records it. Where an inline link abuts the text beside
	// it, the reassembler has no geometry to consult at this tier and this is
	// the only thing that can tell "Lisboa. hello@" from "Lisboa.hello@".
	pad := 0
	if raw := sb.String(); raw != "" {
		if isASCIISpace(raw[0]) {
			pad |= 1
		}
		if isASCIISpace(raw[len(raw)-1]) {
			pad |= 2
		}
	}

	size, weight := typographyFor(n.DataAtom)
	switch n.DataAtom {
	case atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6:
		e.signals.Headings++
	case atom.P:
		e.signals.Paragraphs++
	}
	runes := utf8.RuneCountInString(text)
	e.signals.TextChars += runes
	e.signals.TextRuns++
	if runes <= maxShatteredRun {
		e.signals.ShortRuns++
	}
	// Static extraction has no geometry. Positions are synthesised from
	// document order so the ordering pass has a consistent, monotonic input:
	// on a served document, source order *is* reading order, which is exactly
	// the assumption the rendered path exists to stop relying on.
	//
	// One line per block-level container, and fragments laid out along it in
	// the order the document has them. A paragraph broken by an inline link is
	// three fragments of one sentence, and putting them on one synthetic line
	// is what lets the reassembler join them back into that sentence -- the
	// same code path, and the same result, as when the browser reports three
	// boxes on one rendered line.
	//
	// Giving each fragment its own line instead made every one of them a
	// separate block, so a paragraph arrived as rubble and the link inside it
	// as a stray line of its own.
	if line == 0 {
		e.lineNo++
		line = e.lineNo
	}
	if e.lineW == nil {
		e.lineW = map[int]float64{}
	}
	y := float64(line) * 40
	x := e.lineW[line]
	// Advance by the fragment's own width at a nominal character width, so the
	// gap between two fragments is zero and joinRun's contiguity test sees them
	// as adjacent. Whether a space belongs between them is then decided by the
	// recorded padding, which is the only real evidence at this tier.
	e.lineW[line] += float64(runes)*7 + 1

	e.order++
	e.nodes = append(e.nodes, capture.Node{
		Path: path, Block: blockPath, Tag: strings.ToLower(n.Data), Text: text,
		Role: attr(n, "role"), Landmark: landmark, Href: href,
		FontSize: size, Weight: weight, Family: "static",
		Opacity: 1, Visible: true,
		Pad:      pad,
		BBox:     capture.Box{x, y, float64(runes)*7 + 1, 32},
		LineTop:  y,
		LineLeft: x,
		Depth:    depth,
	})
}

func isASCIISpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\f'
}

// hiddenReason reports why an element is not rendered, or empty.
func (e *extractor) hiddenReason(n *html.Node) string {
	if hasAttr(n, "hidden") {
		return "hidden-attribute"
	}
	if strings.EqualFold(attr(n, "aria-hidden"), "true") {
		return "aria-hidden"
	}
	style := strings.ToLower(strings.ReplaceAll(attr(n, "style"), " ", ""))
	switch {
	case strings.Contains(style, "display:none"):
		return "display-none"
	case strings.Contains(style, "visibility:hidden"):
		return "visibility-hidden"
	case strings.Contains(style, "opacity:0;"), strings.HasSuffix(style, "opacity:0"):
		return "opacity-zero"
	}
	if n.DataAtom == atom.Details && !hasAttr(n, "open") {
		return "collapsed-details"
	}
	return e.hiding.match(n.Data, attr(n, "id"), attr(n, "class"))
}

// harvestStructured walks a subtree the main walk is about to skip, looking
// only for JSON-LD. It reads nothing else.
func (e *extractor) harvestStructured(n *html.Node, depth int) {
	if depth > 12 {
		return
	}
	if n.Type == html.ElementNode && n.DataAtom == atom.Script &&
		strings.ToLower(attr(n, "type")) == "application/ld+json" {
		var sb strings.Builder
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.TextNode {
				sb.WriteString(c.Data)
			}
		}
		if body := sb.String(); len(body) > 0 && len(body) < 262144 {
			e.jsonLD = append(e.jsonLD, body)
		}
		return
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		e.harvestStructured(c, depth+1)
	}
}

func (e *extractor) collectLatent(n *html.Node, path, blockPath, landmark, href string, depth int, reason string) {
	var sb strings.Builder
	collectText(n, &sb)
	text := normalizeSpace(sb.String())
	if text == "" {
		return
	}
	e.latent = append(e.latent, capture.LatentNode{
		Path: path, Block: blockPath, Tag: strings.ToLower(n.Data), Text: text,
		Landmark: landmark, Href: href, Reason: reason,
		ControlLabel: disclosureLabel(n), Depth: depth,
	})
}

func (e *extractor) collectLink(n *html.Node, path, href, landmark string) {
	var sb strings.Builder
	collectText(n, &sb)
	label := normalizeSpace(sb.String())
	if label == "" {
		label = normalizeSpace(attr(n, "aria-label"))
	}
	e.signals.Links++
	e.actions = append(e.actions, capture.Action{
		Path: path, Kind: "link", Label: label, Href: href, Landmark: landmark,
	})
}

func (e *extractor) collectImage(n *html.Node, path string) {
	src := attr(n, "src")
	if src == "" {
		src = attr(n, "data-src")
	}
	if src == "" {
		return
	}
	e.signals.Images++
	alt := attr(n, "alt")
	e.media = append(e.media, capture.Media{
		Path: path, Kind: "image", Src: src,
		Alt:        normalizeSpace(alt),
		Decorative: alt == "" && hasAttr(n, "alt"),
	})
}

func (e *extractor) collectForm(n *html.Node, path, landmark string) {
	var fields []capture.Field
	var walkFields func(*html.Node)
	walkFields = func(c *html.Node) {
		if c.Type == html.ElementNode {
			switch c.DataAtom {
			case atom.Input, atom.Select, atom.Textarea:
				t := strings.ToLower(attr(c, "type"))
				if c.DataAtom != atom.Input {
					t = strings.ToLower(c.Data)
				} else if t == "" {
					t = "text"
				}
				switch t {
				case "hidden", "submit", "button", "reset", "image":
				default:
					fields = append(fields, capture.Field{
						Name:     firstNonEmpty(attr(c, "name"), attr(c, "id")),
						Type:     t,
						Label:    normalizeSpace(firstNonEmpty(attr(c, "aria-label"), attr(c, "placeholder"))),
						Required: hasAttr(c, "required"),
						Pattern:  attr(c, "pattern"),
					})
				}
			}
		}
		for k := c.FirstChild; k != nil; k = k.NextSibling {
			walkFields(k)
		}
	}
	walkFields(n)

	method := strings.ToUpper(attr(n, "method"))
	if method == "" {
		method = "GET"
	}
	e.actions = append(e.actions, capture.Action{
		Path: path, Kind: "form", Label: normalizeSpace(attr(n, "aria-label")),
		Href: attr(n, "action"), Method: method, Fields: fields, Landmark: landmark,
	})
}

func (e *extractor) collectScript(n *html.Node) {
	var sb strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.TextNode {
			sb.WriteString(c.Data)
		}
	}
	body := sb.String()
	e.signals.ScriptBytes += len(body)

	t := strings.ToLower(attr(n, "type"))
	id := attr(n, "id")
	switch {
	case t == "application/ld+json":
		if len(body) > 0 && len(body) < 262144 {
			e.jsonLD = append(e.jsonLD, body)
		}
	case id == "__NEXT_DATA__" || strings.Contains(body, "__NUXT__") ||
		strings.Contains(body, "__remixContext") || strings.Contains(body, "__APOLLO_STATE__"):
		e.signals.HydrationBlob = true
	}
	if attr(n, "src") != "" {
		// An external bundle's size is not visible from here, but its presence
		// is. Counting references is a weaker signal than counting bytes, so it
		// contributes a nominal amount rather than pretending to measure.
		e.signals.ScriptBytes += 2048
	}
}

// checkNoScript looks for the page telling us it needs a browser.
func (e *extractor) checkNoScript(n *html.Node) {
	// The children are gathered directly rather than through collectText:
	// collectText refuses any node on the skip list, and <noscript> is on it,
	// so passing the element itself yields nothing at all.
	var sb strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		collectText(c, &sb)
		sb.WriteByte(' ')
	}
	low := strings.ToLower(sb.String())
	if !strings.Contains(low, "javascript") && !strings.Contains(low, "js") {
		return
	}
	for _, verb := range []string{"enable", "require", "need", "turn on", "activate", "support"} {
		if strings.Contains(low, verb) {
			e.signals.NoScriptWarning = true
			return
		}
	}
}

func (e *extractor) collectHead(n *html.Node) {
	if e.openGraph == nil {
		e.openGraph = map[string]string{}
	}
	var walkHead func(*html.Node)
	walkHead = func(c *html.Node) {
		if c.Type == html.ElementNode {
			switch c.DataAtom {
			case atom.Title:
				var sb strings.Builder
				collectText(c, &sb)
				e.signals.Title = normalizeSpace(sb.String())
			case atom.Meta:
				name := strings.ToLower(firstNonEmpty(attr(c, "name"), attr(c, "property")))
				content := attr(c, "content")
				if content == "" {
					break
				}
				switch {
				case name == "description":
					e.signals.Description = normalizeSpace(content)
				case strings.HasPrefix(name, "og:"), strings.HasPrefix(name, "twitter:"):
					e.openGraph[name] = normalizeSpace(content)
				}
			case atom.Script:
				e.collectScript(c)
			case atom.Noscript:
				e.checkNoScript(c)
			}
		}
		for k := c.FirstChild; k != nil; k = k.NextSibling {
			walkHead(k)
		}
	}
	walkHead(n)
}

// --- helpers ---------------------------------------------------------------

// sectioningTags scope <header> and <footer>.
//
// A <header> inside an <article> or <section> is that section's own heading
// area -- ordinary content -- and only one whose nearest sectioning ancestor is
// the body is the page banner. Treating every <header> as chrome zeroes out the
// content of any site that structures its sections properly.
var sectioningTags = map[atom.Atom]bool{
	atom.Article: true, atom.Section: true, atom.Aside: true,
	atom.Nav: true, atom.Main: true,
}

func landmarkOf(n *html.Node, sectioned bool) string {
	switch n.DataAtom {
	case atom.Nav:
		return "nav"
	case atom.Header:
		if sectioned {
			return ""
		}
		return "header"
	case atom.Footer:
		if sectioned {
			return ""
		}
		return "footer"
	case atom.Main:
		return "main"
	case atom.Aside:
		return "aside"
	case atom.Form:
		return "form"
	case atom.Dialog:
		return "dialog"
	}
	switch strings.ToLower(attr(n, "role")) {
	case "navigation", "menu", "menubar":
		return "nav"
	case "banner":
		return "header"
	case "contentinfo":
		return "footer"
	case "main":
		return "main"
	case "complementary":
		return "aside"
	case "search", "form":
		return "form"
	case "dialog", "alertdialog":
		return "dialog"
	}
	// Fall back to class and id hints, which is how the great majority of the
	// web still marks its furniture.
	joined := strings.ToLower(attr(n, "class") + " " + attr(n, "id"))
	if joined == " " {
		return ""
	}
	for _, hint := range boilerplateHints {
		if !strings.Contains(joined, hint) {
			continue
		}
		switch hint {
		case "nav", "menu", "breadcrumb", "pagination", "skip-link":
			return "nav"
		case "header", "banner":
			return "header"
		case "footer":
			return "footer"
		default:
			return "aside"
		}
	}
	return ""
}

func disclosureLabel(n *html.Node) string {
	if n.DataAtom == atom.Details {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.ElementNode && c.DataAtom == atom.Summary {
				var sb strings.Builder
				collectText(c, &sb)
				return normalizeSpace(sb.String())
			}
		}
	}
	return normalizeSpace(attr(n, "aria-label"))
}

func isBlockish(a atom.Atom) bool {
	switch a {
	case atom.Div, atom.P, atom.Section, atom.Article, atom.Main, atom.Aside,
		atom.Header, atom.Footer, atom.Nav, atom.Li, atom.Dd, atom.Dt,
		atom.Blockquote, atom.Pre, atom.Td, atom.Th, atom.Tr, atom.Figure,
		atom.Figcaption, atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6,
		atom.Ul, atom.Ol, atom.Dl, atom.Table, atom.Form, atom.Fieldset,
		atom.Address, atom.Hgroup, atom.Details, atom.Summary:
		return true
	}
	return false
}

// typographyFor synthesises the type scale a browser would have applied.
//
// The rendered path infers heading level from measured typography; the static
// path has no measurements, so it supplies the default stylesheet's sizes. That
// keeps one heading-inference implementation rather than two, and a page with
// real h1/h2 tags lands on the same levels either way.
func typographyFor(a atom.Atom) (size float64, weight int) {
	switch a {
	case atom.H1:
		return 32, 700
	case atom.H2:
		return 24, 700
	case atom.H3:
		return 18.72, 700
	case atom.H4:
		return 16, 700
	case atom.H5:
		return 13.28, 700
	case atom.H6:
		return 10.72, 700
	case atom.Strong, atom.B:
		return 16, 700
	case atom.Small, atom.Figcaption, atom.Cite:
		return 13, 400
	case atom.Code, atom.Pre, atom.Kbd, atom.Samp:
		return 15, 400
	}
	return 16, 400
}

func collectText(n *html.Node, sb *strings.Builder) {
	if n.Type == html.TextNode {
		sb.WriteString(n.Data)
		return
	}
	if n.Type == html.ElementNode && skipTags[n.DataAtom] {
		return
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		collectText(c, sb)
		sb.WriteByte(' ')
	}
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return a.Val
		}
	}
	return ""
}

func hasAttr(n *html.Node, key string) bool {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return true
		}
	}
	return false
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

func normalizeSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// IsShell reports that the served document is an application shell: markup with
// a mount point and no content in it.
//
// This is the condition behind the most deceptive failure in the category. A
// React or Next.js page that has not hydrated answers with a valid 200 and
// valid HTML carrying no text, so nothing in the response says the read failed,
// and an agent handed it either reports the site as empty or invents something.
// Naming the condition here keeps the threshold beside the signals it reads
// rather than copied into every caller that wants to ask.
//
// A hydration payload is corroborating rather than sufficient: plenty of
// server-rendered pages ship one alongside their content, so it only counts
// when the text is missing too.
func (s Signals) IsShell() bool {
	const shellChars = 100 // MinStaticChars/4, the escalator's "this is a shell" line
	if s.TextChars >= shellChars {
		return false
	}
	return s.HydrationBlob || s.ScriptBytes > 0 || s.CanvasElements > 0
}

// scriptish and anyTag are the whole of the naive strip: remove the elements
// whose contents are never prose, then remove every remaining tag.
var (
	// One pattern per element, because Go's regexp is RE2 and has no
	// backreferences. A single `<(script|style)...</\1>` does not compile to
	// what it looks like: the escape is not a backreference, the pattern matches
	// nothing useful, every script body survives, and a page's JavaScript is
	// counted as ninety thousand characters of prose.
	scriptish = []*regexp.Regexp{
		regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`),
		regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`),
		regexp.MustCompile(`(?is)<noscript[^>]*>.*?</noscript>`),
		regexp.MustCompile(`(?is)<template[^>]*>.*?</template>`),
	}
	anyTag  = regexp.MustCompile(`(?s)<[^>]+>`)
	runOfWS = regexp.MustCompile(`\s+`)
)

// markupTextChars counts the text a tag-strip would find.
//
// Deliberately the dumbest possible reading of the document, because its value
// is precisely that it shares none of the judgement the real extractor applies.
// Where the two disagree by a lot, the real extractor is either right and the
// page is full of text no visitor sees, or wrong and content is being dropped.
// Either way it is worth opening a browser to find out.
func markupTextChars(html string) int {
	if html == "" {
		return 0
	}
	for _, re := range scriptish {
		html = re.ReplaceAllString(html, " ")
	}
	t := anyTag.ReplaceAllString(html, " ")
	return utf8.RuneCountInString(strings.TrimSpace(runOfWS.ReplaceAllString(t, " ")))
}
