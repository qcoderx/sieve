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
	}
	// Stylesheets are scanned before the walk, because visibility has to be
	// known at the moment an element is visited. Almost nobody hides content
	// with an inline style; they use a class, and without this pass the static
	// tier would ingest exactly the material the rendered tiers quarantine.
	ex.scanStyles(doc)
	ex.walk(doc, "html", "html", "", "", 0)

	snap := &capture.Snapshot{
		Checkpoint:   0,
		ViewportW:    1440,
		ViewportH:    900,
		DocHeight:    float64(ex.order) * 40,
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
func (e *extractor) walk(n *html.Node, path, blockPath, landmark, href string, depth int) {
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
		if lm := landmarkOf(n); lm != "" {
			landmark = lm
			e.signals.Landmarks++
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
			e.collectLatent(n, path, blockPath, landmark, href, depth, reason)
			return
		}

		if isBlockish(n.DataAtom) {
			blockPath = path
		}
		e.emitOwnText(n, path, blockPath, landmark, href, depth)

	case html.TextNode:
		return
	}

	counts := map[string]int{}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type != html.ElementNode {
			continue
		}
		tag := c.Data
		idx := counts[tag]
		counts[tag] = idx + 1
		seg := tag
		if idx > 0 {
			seg = fmt.Sprintf("%s[%d]", tag, idx)
		}
		e.walk(c, path+"/"+seg, blockPath, landmark, href, depth+1)
	}
}

// emitOwnText records the element's direct text children, matching the rendered
// walk's granularity.
func (e *extractor) emitOwnText(n *html.Node, path, blockPath, landmark, href string, depth int) {
	var sb strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.TextNode {
			sb.WriteString(c.Data)
		}
	}
	text := normalizeSpace(sb.String())
	if text == "" {
		return
	}

	size, weight := typographyFor(n.DataAtom)
	switch n.DataAtom {
	case atom.H1, atom.H2, atom.H3, atom.H4, atom.H5, atom.H6:
		e.signals.Headings++
	case atom.P:
		e.signals.Paragraphs++
	}
	e.signals.TextChars += utf8.RuneCountInString(text)
	e.order++

	// Static extraction has no geometry. Positions are synthesised from
	// document order so the ordering pass has a consistent, monotonic input:
	// on a served document, source order *is* reading order, which is exactly
	// the assumption the rendered path exists to stop relying on.
	y := float64(e.order) * 40
	e.nodes = append(e.nodes, capture.Node{
		Path: path, Block: blockPath, Tag: strings.ToLower(n.Data), Text: text,
		Role: attr(n, "role"), Landmark: landmark, Href: href,
		FontSize: size, Weight: weight, Family: "static",
		Opacity: 1, Visible: true,
		BBox:    capture.Box{0, y, 800, 32},
		LineTop: y, Depth: depth,
	})
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

func landmarkOf(n *html.Node) string {
	switch n.DataAtom {
	case atom.Nav:
		return "nav"
	case atom.Header:
		return "header"
	case atom.Footer:
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
