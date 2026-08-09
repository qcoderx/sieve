package graph

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/qcoderx/sieve/internal/capture"
	"github.com/qcoderx/sieve/internal/textnorm"
	"github.com/qcoderx/sieve/internal/tokens"
)

// SchemaVersion is bumped when the artifact shape changes in a way consumers
// must notice.
const SchemaVersion = "1.0"

// Input is everything the builder needs. It is deliberately not the render
// package's Result: the graph must be buildable from a recorded capture with no
// browser present, which is what makes golden-file tests and offline bug
// reproduction possible.
type Input struct {
	RequestedURL string
	FinalURL     string
	Merged       *capture.Merged
	Notes        []string
	// OriginalBytes is the transfer size of the undistilled page, used for the
	// stats that justify the project.
	OriginalBytes int64
	// OriginalText is the raw page text an unaided agent would have had to
	// read, used to estimate the token cost that was avoided.
	OriginalText string
	// ReachedBottom reports whether the sweep saw the end of the document.
	ReachedBottom bool
	// EntryGate names an interstitial standing between the visitor and the
	// site, when one was detected.
	EntryGate string
	// Now is injectable so golden tests are not time-dependent.
	Now time.Time
	// Generator identifies the build, e.g. "sieve/0.1.0".
	Generator string
	// Provenance carries the render-level facts through unchanged.
	Provenance Provenance
}

// Build turns a capture into a content graph.
//
// The stages run in a fixed order because each depends on the last: text must
// be normalised before anything hashes it, reassembled before type sizes mean
// anything, type sizes must be known before headings can be inferred, and
// headings must exist before the document can be cut into sections.
func Build(in Input) (*Graph, error) {
	if in.Merged == nil {
		return nil, fmt.Errorf("graph: nil capture")
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}

	groups := reassemble(in.Merged.Nodes)
	cands := classify(groups, in.Merged)

	kept := make([]*candidate, 0, len(cands))
	dropped := 0
	dropStats := map[string]*DropCount{}
	for _, c := range cands {
		if c.Keep {
			kept = append(kept, c)
			continue
		}
		dropped++
		d, ok := dropStats[c.DropReason]
		if !ok {
			d = &DropCount{Reason: c.DropReason}
			dropStats[c.DropReason] = d
		}
		d.Runs++
		d.Chars += utf8.RuneCountInString(c.Text)
	}

	pageWidth := in.Merged.ViewportW
	if pageWidth <= 0 {
		pageWidth = 1440
	}

	base := baseURL(in.FinalURL, in.RequestedURL)
	mediaAll := makeMedia(in.Merged, base)
	kept = append(kept, mediaCandidates(in.Merged, mediaAll)...)

	// Chrome is ordered separately from content. Mixing a pinned navigation
	// into the same geometric solve as the article would let it cut the article
	// in half, and the navigation's own order is simply the order it was
	// written in.
	var flow, chrome []*candidate
	for _, c := range kept {
		if c.Region.IsChrome() {
			chrome = append(chrome, c)
		} else {
			flow = append(flow, c)
		}
	}

	ord := orderBoxes(flow, pageWidth)
	ordered := make([]*candidate, 0, len(kept))
	for _, i := range ord.Order {
		ordered = append(ordered, flow[i])
	}
	sort.SliceStable(chrome, func(i, j int) bool {
		a, b := chrome[i], chrome[j]
		if a.Region != b.Region {
			return a.Region < b.Region
		}
		return comparePath(a.Path, b.Path) < 0
	})
	ordered = append(ordered, chrome...)

	g := &Graph{
		SchemaVersion: SchemaVersion,
		URL:           in.RequestedURL,
		FinalURL:      in.FinalURL,
		DistilledAt:   now.UTC(),
		Generator:     in.Generator,
		Title:         textnorm.CleanString(pickTitle(in.Merged, ordered)),
		Description:   textnorm.CleanString(in.Merged.Meta.Description),
		Lang:          in.Merged.Meta.Lang,
		Provenance:    in.Provenance,
	}
	g.Provenance.NormalizerVersion = textnorm.Version

	g.Blocks = makeBlocks(collapseAdjacentRepeats(ordered))
	normaliseLevels(g.Blocks)
	g.Sections = makeSections(g.Blocks)
	g.Actions, g.Links = makeActions(in.Merged, base)
	g.Blocks = pruneNonContent(g.Blocks, g.Actions)
	g.MediaAll = mediaAll
	g.Structured = ParseJSONLD(in.Merged.Meta.JSONLD)
	g.FAQ = ParseFAQ(in.Merged.Meta.JSONLD)
	g.Latent = makeLatent(in.Merged.Latent)
	g.Gaps = makeGaps(in.Merged, g.Latent)
	if in.EntryGate != "" {
		// An entry gate goes at the head of the gap list because it does not
		// hide one section, it holds back the whole site. A reader who is told
		// only that the page was thin will conclude the page is thin.
		g.Gaps = append([]Gap{{
			Label: in.EntryGate,
			Kind:  "entry-gate",
			Reason: "the site is behind an entry screen that a visitor must dismiss before it begins; " +
				"sieve does not click through interstitials, so everything past it is absent from this artifact",
		}}, g.Gaps...)
	}
	g.Summary = makeSummary(g)

	contentChars, chromeCount, emitted := 0, 0, 0
	for _, b := range g.Blocks {
		// Only text that was actually rendered counts toward retention. An
		// image's alt text and anything recovered from pixels were never on
		// screen, so including them would compare the emitted total against a
		// denominator that never contained them.
		if b.Source == SourceDOM && b.Type != TypeImage {
			emitted += utf8.RuneCountInString(b.Text)
		}
		if b.Region.IsChrome() {
			chromeCount++
		} else {
			contentChars += len(b.Text)
		}
	}
	latentChars := 0
	for _, l := range g.Latent {
		latentChars += utf8.RuneCountInString(l.Text)
	}

	g.Stats = Stats{
		OriginalBytes:  in.OriginalBytes,
		OriginalTokens: tokens.EstimateHTML(in.OriginalText),
		Checkpoints:    in.Merged.Checkpoints,
		RawNodes:       len(in.Merged.Nodes),
		ContentNodes:   len(g.Blocks) - chromeCount,
		ChromeNodes:    chromeCount,
		LatentNodes:    len(g.Latent),
		DroppedNodes:   dropped,
		LatentTokens:   tokens.EstimateChars(latentChars),
	}

	g.Audit = buildAudit(in, g, ord, flow, emitted)
	for _, d := range dropStats {
		g.Audit.Dropped = append(g.Audit.Dropped, *d)
	}
	sort.SliceStable(g.Audit.Dropped, func(i, j int) bool {
		return g.Audit.Dropped[i].Runs > g.Audit.Dropped[j].Runs
	})

	// The hash covers the normalised semantic graph, not the serialised output.
	// Timestamps and timings change on every run and would make an unchanged
	// site look different every time, defeating the point of a content-addressed
	// cache; so would a rewritten asset URL or a whitespace change.
	g.ContentHash = semanticHash(g)
	g.Stats.ArtifactTokens = tokens.Estimate(payloadText(g))
	return g, nil
}

// collapseAdjacentRepeats removes a sequence of runs that repeats the sequence
// immediately before it in reading order.
//
// Carousels, marquees and infinite-scroll tracks work by holding two or more
// copies of their items in the DOM so the loop has somewhere to go. Both copies
// are genuinely on screen at different moments, so neither is hidden and neither
// can be dropped for invisibility -- the artifact simply gets the list twice.
// Four of six sites in a spot check carried it, up to thirty-three repeats each,
// and it reads as though the page says everything twice.
//
// It has to match sequences, not single runs. A cloned track repeats its whole
// list -- HTML, CSS, JavaScript, React, HTML, CSS, JavaScript, React -- and no
// item there is ever immediately followed by itself, so a rule that only looks
// one block back sees nothing wrong and every item survives twice.
//
// Adjacency is what makes this safe. Text that recurs across a page -- a
// repeated call to action, a price appearing in a table and again in a summary
// -- is separated by other content and is left alone. Only an immediate echo is
// removed, which is the shape a cloned track always has and the shape ordinary
// prose never does.
func collapseAdjacentRepeats(cands []*candidate) []*candidate {
	if len(cands) < 2 {
		return cands
	}
	keys := make([]string, len(cands))
	for i, c := range cands {
		keys[i] = dedupeKey(c.Text)
	}

	drop := make([]bool, len(cands))
	for i := 0; i < len(cands); i++ {
		if drop[i] || keys[i] == "" {
			continue
		}
		// Longest repeat first, so a cloned list of eight items collapses as one
		// sequence rather than leaving seven survivors after the first item is
		// matched on its own.
		for n := maxRepeatRun; n >= 1; n-- {
			if i+2*n > len(cands) {
				continue
			}
			same := true
			for k := 0; k < n; k++ {
				if keys[i+k] == "" || keys[i+k] != keys[i+n+k] {
					same = false
					break
				}
			}
			if !same {
				continue
			}
			for k := 0; k < n; k++ {
				drop[i+n+k] = true
			}
			i += n - 1
			break
		}
	}

	out := make([]*candidate, 0, len(cands))
	for i, c := range cands {
		if !drop[i] {
			out = append(out, c)
		}
	}
	return out
}

// maxRepeatRun is the longest cloned sequence recognised. A track long enough to
// exceed it is collapsed in pieces, which is still an improvement.
const maxRepeatRun = 24

// pruneNonContent removes blocks that carry no reading value.
//
// Two kinds turned up across a hundred-site sweep, in more than half of them.
//
// The first is punctuation on its own. A pager renders its arrows as separate
// runs and docs.python.org contributed three blocks reading "«", "|" and "»".
// A run with no letter and no digit in it is a separator, not a sentence.
//
// The second is a navigation label that is already in the actions list. go.dev
// emitted "learn more" four times and "Tour", "Docs" and "Blog" once each as
// content, when every one of them is a link whose label and destination are
// recorded properly a few fields away. Repeating them as prose is what made
// forty-four of a hundred sites look like they were full of duplicates: they
// were full of menus.
//
// Only a block that is the whole of a link's text is removed, and only a short
// one. A paragraph containing a link is a paragraph, and its block is the
// prose, not the link.
func pruneNonContent(blocks []Block, actions []Action) []Block {
	labels := make(map[string]bool, len(actions))
	for _, a := range actions {
		if l := dedupeKey(a.Label); l != "" {
			labels[l] = true
		}
	}
	// A short line that appears many times on one page is a template label.
	//
	// Card and list layouts repeat their furniture once per item, and none of it
	// is a link, so the rule above never sees it: npr.org emitted "hide caption"
	// and "toggle caption" fifty-eight times each, esa.int "views" and "likes"
	// twenty-seven times, airbnb.com "guest favorite" twenty-two. It is real
	// interface text and it is worth knowing about once; repeated it is most of
	// the artifact.
	//
	// The first occurrence is kept, so nothing disappears from the page's
	// vocabulary -- what goes is the repetition. Length is the guard: a
	// paragraph that happens to recur is quoted prose and stays, and the cap is
	// well below any sentence.
	repeats := map[string]int{}
	for _, b := range blocks {
		if utf8.RuneCountInString(b.Text) <= maxTemplateLabelRunes {
			repeats[dedupeKey(b.Text)]++
		}
	}
	kept := map[string]int{}

	out := blocks[:0]
	for _, b := range blocks {
		if b.Type != TypeImage && !hasLexicalContent(b.Text) {
			continue
		}
		// A single character is never a block. It is a fragment the reassembly
		// could not place -- resend.com produced blocks reading "t", "b" and "s"
		// -- and no artifact is improved by carrying it.
		if b.Type != TypeImage && utf8.RuneCountInString(b.Text) < 2 {
			continue
		}
		if b.Href != "" && utf8.RuneCountInString(b.Text) <= maxNavLabelRunes &&
			labels[dedupeKey(b.Text)] {
			continue
		}
		if k := dedupeKey(b.Text); b.Type != TypeTable &&
			repeats[k] >= minTemplateRepeats {
			kept[k]++
			if kept[k] > 1 {
				continue
			}
		}
		out = append(out, b)
	}
	// IDs are positional, so they are reassigned rather than left with holes.
	for i := range out {
		out[i].ID = blockID(i)
		out[i].Order = i
	}
	return out
}

// maxNavLabelRunes is the length below which a standalone link is a menu item
// rather than a sentence that happens to be linked.
const maxNavLabelRunes = 40

// maxTemplateLabelRunes and minTemplateRepeats define the template-label rule:
// short, and repeated often enough that it is plainly furniture rather than
// coincidence.
const maxTemplateLabelRunes = 40

// Three is the threshold because three is what the corpus shows. Card layouts
// that repeat a label twice are ambiguous; at three the pattern is a template --
// "video", "models", "company", "4.85", "apartment in Accra" -- and every
// remaining duplicate finding in a hundred-site sweep sat at exactly three.
const minTemplateRepeats = 3

// hasLexicalContent reports whether a run contains anything readable at all.
func hasLexicalContent(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

// buildAudit assembles the artifact's account of its own reliability.
func buildAudit(in Input, g *Graph, ord orderResult, flow []*candidate, emittedChars int) Audit {
	a := Audit{
		ObservedChars:   in.Merged.ObservedVisibleChars,
		EmittedChars:    emittedChars,
		OrderScore:      ord.Confidence,
		OrderConfidence: Bucket(ord.Confidence),
		OrderBasis:      ord.Basis,
		OrderAgreement:  ord.Agreement,
		ReachedBottom:   in.ReachedBottom,
		FramesBlocked:   in.Merged.FramesBlocked,
		Notes:           in.Notes,
	}
	if a.ObservedChars > 0 {
		r := float64(a.EmittedChars) / float64(a.ObservedChars)
		if r > 1 {
			// Reassembly inserts spaces between fragments, so the emitted count
			// can legitimately exceed the observed one by a little. Clamping
			// keeps the figure honest rather than letting it read as 104%.
			r = 1
		}
		a.GraphRetention = roundTo(r, 0.001)
	}
	a.HeadingSeparation = roundTo(headingSeparation(g.Blocks), 0.01)
	a.HeadingConfidence = Bucket(a.HeadingSeparation)
	return a
}

// headingSeparation measures how cleanly the document's heading sizes separate
// into distinct levels.
//
// A page whose headings are 76px, 44px and 22px has unmistakable levels. A page
// whose headings run 30, 29, 28, 27 has a continuum, and any level assigned
// from it is a guess. Reporting which situation the artifact is in costs one
// pass over the blocks and tells a consumer exactly how much to trust the
// outline.
func headingSeparation(blocks []Block) float64 {
	var sizes []float64
	for _, b := range blocks {
		if b.Type == TypeHeading && !b.Region.IsChrome() && b.Style.FontSize > 0 {
			sizes = append(sizes, b.Style.FontSize)
		}
	}
	if len(sizes) < 2 {
		// Nothing to confuse: one heading size cannot be ambiguous.
		return 1
	}
	sort.Float64s(sizes)
	// Deduplicate to the distinct rungs actually used.
	var rungs []float64
	for _, s := range sizes {
		if len(rungs) == 0 || s/rungs[len(rungs)-1] > 1.08 {
			rungs = append(rungs, s)
		}
	}
	if len(rungs) < 2 {
		return 1
	}
	// The separation of the tightest adjacent pair is what limits confidence:
	// one ambiguous boundary is enough to misplace a heading.
	worst := 10.0
	for i := 1; i < len(rungs); i++ {
		if ratio := rungs[i] / rungs[i-1]; ratio < worst {
			worst = ratio
		}
	}
	// A 1.4x step is unambiguous; 1.08x is the merge threshold and is not.
	score := (worst - 1.08) / (1.4 - 1.08)
	if score > 1 {
		score = 1
	}
	if score < 0 {
		score = 0
	}
	return score
}

// verificationOf downgrades a block sieve never actually saw rendered.
func verificationOf(c *candidate) Verification {
	if c.Declared {
		return VerificationSpeculative
	}
	return c.Verified
}

func makeBlocks(cands []*candidate) []Block {
	blocks := make([]Block, 0, len(cands))
	for i, c := range cands {
		clean := textnorm.Clean(c.Text)
		if clean.Text == "" {
			continue
		}
		var flags []string
		if clean.HadBidi {
			flags = append(flags, "bidi-control-removed")
		}
		if clean.HadInvisible {
			flags = append(flags, "invisible-characters-removed")
		}
		if c.InvisibleColor {
			flags = append(flags, "text-colour-matches-background")
		}
		// Said plainly on the block itself, in every format, because a consumer
		// deciding whether to quote this text needs to know that sieve did not
		// watch it appear -- it read the page's promise that it would.
		if c.Declared {
			flags = append(flags, "declared-reveal-not-observed")
		}
		blocks = append(blocks, Block{
			ID:         blockID(len(blocks)),
			Type:       c.Type,
			Level:      c.Level,
			Text:       clean.Text,
			Order:      i,
			Source:     c.SourceKind,
			Score:      roundTo(c.Confidence, 0.01),
			Confidence: Bucket(c.Confidence),
			Verified:   verificationOf(c),
			Checkpoint: c.Checkpoint,
			BBox:       [4]float64{c.BBox[0], c.BBox[1], c.BBox[2], c.BBox[3]},
			Region:     c.Region,
			Href:       c.Href,
			MediaID:    c.mediaRef,
			Style:      c.Style,
			Flags:      flags,
		})
	}
	return blocks
}

// makeLatent builds the quarantine tier.
func makeLatent(nodes []capture.LatentNode) []LatentBlock {
	out := make([]LatentBlock, 0, len(nodes))
	seen := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		clean := textnorm.Clean(n.Text)
		if clean.Text == "" {
			continue
		}
		// Hidden duplicates of the same string are common: a responsive
		// template ships the same menu three times.
		key := strings.ToLower(clean.Text) + "\x00" + n.ControlLabel
		if seen[key] {
			continue
		}
		seen[key] = true

		var flags []string
		if clean.HadBidi {
			flags = append(flags, "bidi-control-removed")
		}
		if clean.HadInvisible {
			flags = append(flags, "invisible-characters-removed")
		}

		t := TypeParagraph
		lvl := 0
		switch n.Tag {
		case "h1", "h2", "h3", "h4", "h5", "h6":
			t = TypeHeading
			lvl = int(n.Tag[1] - '0')
		case "li", "dt", "dd":
			t = TypeListItem
		case "blockquote", "q":
			t = TypeQuote
		case "td", "th":
			t = TypeTable
		case "label", "legend", "figcaption":
			t = TypeLabel
		}

		out = append(out, LatentBlock{
			ID:           fmt.Sprintf("l_%03d", len(out)),
			Type:         t,
			Level:        lvl,
			Text:         clean.Text,
			Reason:       n.Reason,
			ControlLabel: n.ControlLabel,
			ControlKind:  n.ControlKind,
			Region:       regionOfLandmark(n.Landmark),
			Href:         n.Href,
			Trust:        LatentTrustMarker,
			Flags:        flags,
		})
	}
	return out
}

// makeGaps lists the disclosure controls whose content was not opened, so an
// agent that needs what is behind one can go and get it another way rather than
// concluding the page had nothing to say.
func makeGaps(m *capture.Merged, latent []LatentBlock) []Gap {
	byLabel := map[string][]string{}
	for _, l := range latent {
		if l.ControlLabel != "" {
			byLabel[l.ControlLabel] = append(byLabel[l.ControlLabel], l.ID)
		}
	}

	var out []Gap
	for _, d := range m.Disclosures {
		// A control that is already expanded revealed its content into the
		// normal capture; there is no gap.
		if d.Expanded != nil && *d.Expanded {
			continue
		}
		if d.Selected {
			continue
		}
		ids := byLabel[d.Label]
		reason := "collapsed disclosure; content was captured into the latent tier"
		if len(ids) == 0 {
			reason = "collapsed disclosure; its content is not present in the document and was not retrieved"
		}
		out = append(out, Gap{
			Label:     d.Label,
			Kind:      d.Kind,
			Reason:    reason,
			LatentIDs: ids,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
}

func blockID(i int) string   { return fmt.Sprintf("b_%03d", i) }
func actionID(i int) string  { return fmt.Sprintf("a_%03d", i) }
func mediaID(i int) string   { return fmt.Sprintf("m_%03d", i) }
func sectionID(i int) string { return fmt.Sprintf("s_%02d", i) }

// makeSections cuts the document at headings.
//
// Sections are what make the artifact usable from an agent's context window: a
// tool that can return "the Materials section" is useful, one that can only
// return the whole document has moved the token cost rather than removed it.
func makeSections(blocks []Block) []Section {
	var secs []Section
	cur := -1

	open := func(title string, level int, first string) {
		secs = append(secs, Section{
			ID: sectionID(len(secs)), Title: title, Level: level,
			FirstBlock: first, LastBlock: first,
		})
		cur = len(secs) - 1
	}

	for i := range blocks {
		b := &blocks[i]
		if b.Region.IsChrome() {
			continue
		}
		if b.Type == TypeHeading {
			open(b.Text, b.Level, b.ID)
		} else if cur < 0 {
			// Content before the first heading still belongs somewhere.
			open("", 0, b.ID)
		}
		b.SectionID = secs[cur].ID
		secs[cur].LastBlock = b.ID
		secs[cur].BlockCount++
		secs[cur].Chars += utf8.RuneCountInString(b.Text)
	}

	for i := range secs {
		if secs[i].Title == "" {
			secs[i].Title = "(introduction)"
		}
		secs[i].Tokens = tokens.EstimateChars(secs[i].Chars)
	}
	return secs
}

func makeActions(m *capture.Merged, base *url.URL) ([]Action, []Link) {
	actions := make([]Action, 0, len(m.Actions))
	links := make([]Link, 0, len(m.Actions))
	seenLink := make(map[string]bool, len(m.Actions))

	for _, a := range m.Actions {
		href := absolutise(a.Href, base)
		region := regionOfLandmark(a.Landmark)
		internal := isInternal(href, base)
		label := textnorm.CleanString(a.Label)

		switch a.Kind {
		case "link":
			if href == "" {
				continue
			}
			key := href + "\x00" + strings.ToLower(label)
			if !seenLink[key] {
				seenLink[key] = true
				links = append(links, Link{Href: href, Text: label, Internal: internal, Region: region})
			}
			actions = append(actions, Action{
				ID: actionID(len(actions)), Type: "link", Label: label,
				Href: href, Region: region, Internal: internal, Disabled: a.Disabled,
			})
		case "form":
			fields := make([]Field, 0, len(a.Fields))
			for _, f := range a.Fields {
				fields = append(fields, Field{
					Name: f.Name, Type: f.Type, Label: textnorm.CleanString(f.Label),
					Required: f.Required, Options: f.Options, Pattern: f.Pattern,
				})
			}
			method := strings.ToUpper(a.Method)
			if method == "" {
				method = "GET"
			}
			actions = append(actions, Action{
				ID: actionID(len(actions)), Type: "form", Label: label,
				Href: href, Method: method, Fields: fields, Region: region,
				Internal: internal,
			})
		case "button":
			if label == "" {
				continue
			}
			actions = append(actions, Action{
				ID: actionID(len(actions)), Type: "button", Label: label,
				Href: href, Region: region, Disabled: a.Disabled,
			})
		}
	}
	sort.SliceStable(links, func(i, j int) bool { return links[i].Href < links[j].Href })
	return actions, links
}

func makeMedia(m *capture.Merged, base *url.URL) []Media {
	out := make([]Media, 0, len(m.Media))
	for _, md := range m.Media {
		// An image the page itself marks as presentational carries no meaning
		// and describing it would be inventing content.
		if md.Decorative && md.Alt == "" && md.Caption == "" {
			continue
		}
		src := absolutise(md.Src, base)
		source := "none"
		switch {
		case md.Caption != "":
			source = "caption"
		case md.Alt != "":
			source = "alt"
		case md.Title != "":
			source = "title"
		}
		alt := md.Alt
		if alt == "" {
			alt = md.Title
		}
		var flags []string
		if md.AltCapped {
			flags = append(flags, "alt-text-truncated-at-metadata-cap")
		}
		out = append(out, Media{
			ID: mediaID(len(out)), Type: md.Kind, Src: src,
			Alt:     textnorm.CleanString(alt),
			Caption: textnorm.CleanString(md.Caption),
			Source:  source, Confidence: ConfidenceHigh,
			Width: md.BBox.W(), Height: md.BBox.H(),
			Flags: flags,
		})
	}
	return out
}

// mediaCandidates turns described media into blocks that take part in the
// reading order, rather than being listed separately at the end.
//
// An image described only in an appendix loses the thing that made it
// meaningful: which paragraph it sat next to. Feeding media through the same
// geometric ordering as text puts the photograph back between the two
// paragraphs it was between.
func mediaCandidates(m *capture.Merged, media []Media) []*candidate {
	byID := make(map[string]*Media, len(media))
	for i := range media {
		byID[media[i].Src] = &media[i]
	}
	out := make([]*candidate, 0, len(media))
	for _, raw := range m.Media {
		md := byID[absolutiseKey(raw.Src, media)]
		if md == nil || (md.Alt == "" && md.Caption == "") {
			continue
		}
		// A figcaption is rendered text and was already captured as a block in
		// its own right. Repeating it inside the image block would state the
		// same sentence twice and charge for it twice, so the image block
		// carries only what the caption does not already say: the alt text,
		// which is not rendered anywhere.
		text := md.Alt
		if text == "" {
			text = md.Caption
		}
		if text == "" {
			continue
		}
		// Only images that say something join the reading order.
		//
		// A page of avatars with alt="lena", alt="didier" produces a block per
		// face, and a reader gets a column of first names where the prose
		// should be. Those are labels, not descriptions, and they cost tokens
		// in an artifact whose whole premise is not spending them. They stay in
		// the media array, addressable by describe_media, just not in the flow.
		if !imageCarriesMeaning(md, raw.BBox) {
			continue
		}
		out = append(out, &candidate{
			Text: text, Path: raw.Path, Block: raw.Path,
			Tag: "img", BBox: raw.BBox, Region: RegionMain,
			Type: TypeImage, mediaRef: md.ID, Keep: true,
			SourceKind:  SourceDOM,
			EverVisible: true, Confidence: 0.95,
			Style: StyleInfo{MaxOpacity: 1},
		})
	}
	return out
}

// imageCarriesMeaning decides whether an image's description belongs in the
// reading order.
//
// The test is on the description, not the image: a caption is authored prose
// and always counts; alt text counts when it reads like a description rather
// than a label. A large image is given the benefit of the doubt, because a hero
// with a terse alt is still a hero.
func imageCarriesMeaning(md *Media, box capture.Box) bool {
	if md.Caption != "" {
		return true
	}
	alt := strings.TrimSpace(md.Alt)
	if alt == "" {
		return false
	}
	words := len(strings.Fields(alt))
	if words >= 3 || utf8.RuneCountInString(alt) >= 25 {
		return true
	}
	// A hero image: big enough that a reader would notice its absence.
	const heroArea = 240 * 240
	return box.Area() >= heroArea
}

// absolutiseKey finds the resolved src for a raw capture src by position,
// since makeMedia rewrote relative URLs into absolute ones.
func absolutiseKey(rawSrc string, media []Media) string {
	for i := range media {
		if strings.HasSuffix(media[i].Src, strings.TrimPrefix(rawSrc, "./")) || media[i].Src == rawSrc {
			return media[i].Src
		}
	}
	return rawSrc
}

func makeSummary(g *Graph) string {
	// The summary is built from the page's own words, never generated. A
	// distiller that writes prose about a site has started inventing content,
	// and the fidelity metric exists precisely to catch that.
	if g.Description != "" {
		return g.Description
	}
	var parts []string
	budget := 320
	for _, b := range g.Blocks {
		if b.Region.IsChrome() || b.Type != TypeParagraph {
			continue
		}
		if utf8.RuneCountInString(b.Text) < 40 {
			continue
		}
		parts = append(parts, b.Text)
		budget -= utf8.RuneCountInString(b.Text)
		if budget <= 0 {
			break
		}
	}
	s := strings.Join(parts, " ")
	if s == "" {
		// Fall back to the first heading, which at least says what the page is.
		for _, b := range g.Blocks {
			if b.Type == TypeHeading && !b.Region.IsChrome() {
				return b.Text
			}
		}
		return g.Title
	}
	out, _ := textnorm.Truncate(s, 400)
	return out
}

func pickTitle(m *capture.Merged, cands []*candidate) string {
	if t := strings.TrimSpace(m.Meta.Title); t != "" {
		return t
	}
	if t := m.Meta.OpenGraph["og:title"]; t != "" {
		return t
	}
	for _, c := range cands {
		if c.Type == TypeHeading && c.Level <= 2 && !c.Region.IsChrome() {
			return c.Text
		}
	}
	return ""
}

// PlainText renders exactly what a caller receives by default.
//
// It is the basis for the headline token estimate, so it must match the default
// payload precisely: latent content is excluded by construction, and
// uncorroborated pixel recoveries are excluded because no default rendering
// emits them either. A token count that included material no caller receives
// would misstate the project's central claim in its own favour.
func PlainText(g *Graph) string {
	var sb strings.Builder
	sb.WriteString(g.Title)
	sb.WriteByte('\n')
	for _, b := range g.Blocks {
		if b.Region.IsChrome() || b.Verified == VerificationSpeculative {
			continue
		}
		sb.WriteString(b.Text)
		sb.WriteByte('\n')
	}
	return sb.String()
}

// payloadText is everything an artifact actually puts in front of a reader.
//
// The token figure in the stats used to be PlainText, which is the prose blocks
// and nothing else. That is the right measure of the content and the wrong
// measure of the cost: the emitted document also carries the description, the
// links, the buttons, the media descriptions, the questions and answers and the
// whitelisted facts, and a reader pays for all of it.
//
// On a page whose prose did not survive extraction the gap becomes absurd --
// five hundred characters of block text reported against an eight-kilobyte file
// -- and the artifact announced a ninety-nine per cent saving it had not made.
// Counting what is emitted keeps the headline honest in both directions.
func payloadText(g *Graph) string {
	var sb strings.Builder
	sb.WriteString(g.Title)
	sb.WriteByte('\n')
	sb.WriteString(g.Summary)
	sb.WriteByte('\n')
	for _, b := range g.Blocks {
		sb.WriteString(b.Text)
		sb.WriteByte('\n')
	}
	for _, a := range g.Actions {
		sb.WriteString(a.Label)
		sb.WriteByte(' ')
		sb.WriteString(a.Href)
		sb.WriteByte('\n')
	}
	for _, m := range g.MediaAll {
		sb.WriteString(m.Alt)
		sb.WriteByte(' ')
		sb.WriteString(m.Caption)
		sb.WriteByte('\n')
	}
	for _, qa := range g.FAQ {
		sb.WriteString(qa.Question)
		sb.WriteByte('\n')
		sb.WriteString(qa.Answer)
		sb.WriteByte('\n')
	}
	for _, f := range g.Structured {
		sb.WriteString(f.Field)
		sb.WriteByte(' ')
		sb.WriteString(f.Value)
		sb.WriteByte('\n')
	}
	for _, gap := range g.Gaps {
		sb.WriteString(gap.Label)
		sb.WriteByte(' ')
		sb.WriteString(gap.Reason)
		sb.WriteByte('\n')
	}
	return sb.String()
}

// semanticHash covers the meaning of the artifact and nothing else.
//
// Hashing the serialised JSON would be simpler and wrong: it would change when
// a build-id in an asset URL rotated, when a timestamp advanced, or when the
// encoder's field order shifted, and every one of those would evict a cache
// entry for a page that had not changed. Hashing a canonical form of the
// semantic graph means the hash moves when the page's meaning moves.
//
// The normalizer version is an input. Changing how text is cleaned changes what
// every artifact says, so it must invalidate caches -- deliberately, and
// visibly, rather than as a mystery.
func semanticHash(g *Graph) string {
	h := sha256.New()
	write := func(parts ...string) {
		for _, p := range parts {
			h.Write([]byte(p))
			h.Write([]byte{0x1f})
		}
		h.Write([]byte{0x1e})
	}

	write("schema", SchemaVersion)
	write("normalizer", strconv.Itoa(textnorm.Version))
	write("url", canonicalURL(g.FinalURL, g.URL))
	write("title", g.Title)

	for _, b := range g.Blocks {
		write("block", string(b.Type), strconv.Itoa(b.Level), string(b.Region),
			string(b.Source), b.Text)
	}
	for _, a := range g.Actions {
		write("action", a.Type, a.Label, a.Href, a.Method)
		for _, f := range a.Fields {
			write("field", f.Name, f.Type, strconv.FormatBool(f.Required))
		}
	}
	for _, m := range g.MediaAll {
		// Media source URLs carry content hashes and build ids that rotate on
		// every deploy without the image changing, so only the description is
		// hashed.
		write("media", m.Type, m.Alt, m.Caption)
	}
	for _, l := range g.Links {
		write("link", l.Href, l.Text)
	}
	for _, s := range g.Structured {
		write("fact", s.Type, s.Field, s.Value)
	}
	// Latent content is part of the artifact's meaning even though it is not
	// part of the default payload: a page that gains a hidden pricing tab has
	// changed.
	for _, l := range g.Latent {
		write("latent", string(l.Type), l.ControlLabel, l.Text)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// canonicalURL strips the parts of a URL that do not select content, so two
// requests for the same page hash the same.
func canonicalURL(final, requested string) string {
	s := final
	if s == "" {
		s = requested
	}
	u, err := url.Parse(s)
	if err != nil {
		return s
	}
	u.Fragment = ""
	// Campaign parameters change per visitor and never change the page.
	q := u.Query()
	for _, k := range []string{
		"utm_source", "utm_medium", "utm_campaign", "utm_term", "utm_content",
		"gclid", "fbclid", "msclkid", "mc_cid", "mc_eid", "ref", "_ga",
	} {
		q.Del(k)
	}
	u.RawQuery = q.Encode()
	u.Host = strings.ToLower(u.Host)
	if u.Path == "" {
		u.Path = "/"
	}
	return u.String()
}

func regionOfLandmark(l string) Region {
	switch l {
	case "nav":
		return RegionNav
	case "header":
		return RegionHeader
	case "footer":
		return RegionFooter
	case "aside":
		return RegionAside
	case "form":
		return RegionForm
	case "dialog":
		return RegionDialog
	}
	return RegionMain
}

func baseURL(final, requested string) *url.URL {
	for _, s := range []string{final, requested} {
		if s == "" {
			continue
		}
		if u, err := url.Parse(s); err == nil && u.Host != "" {
			return u
		}
	}
	return nil
}

func absolutise(href string, base *url.URL) string {
	if href == "" || base == nil {
		return href
	}
	u, err := url.Parse(href)
	if err != nil {
		return href
	}
	return base.ResolveReference(u).String()
}

func isInternal(href string, base *url.URL) bool {
	if base == nil || href == "" {
		return false
	}
	u, err := url.Parse(href)
	if err != nil {
		return false
	}
	if u.Host == "" {
		return true
	}
	return sameSite(u.Host, base.Host)
}

// sameSite compares hosts by their last two labels, so www.example.com and
// example.com count as one site while example.com and example.org do not. This
// is a deliberate simplification: a full public-suffix list would be more
// correct for domains like co.uk, at the cost of a dependency and a data file
// that goes stale.
func sameSite(a, b string) bool {
	a, b = strings.ToLower(hostOnly(a)), strings.ToLower(hostOnly(b))
	if a == b {
		return true
	}
	return lastLabels(a, 2) == lastLabels(b, 2)
}

func hostOnly(h string) string {
	if i := strings.LastIndexByte(h, ':'); i > 0 && !strings.Contains(h[i:], "]") {
		return h[:i]
	}
	return h
}

func lastLabels(h string, n int) string {
	parts := strings.Split(h, ".")
	if len(parts) <= n {
		return h
	}
	return strings.Join(parts[len(parts)-n:], ".")
}
