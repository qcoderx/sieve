package graph

import (
	"strings"

	"github.com/qcoderx/sieve/internal/textnorm"
)

// HydrationLink mirrors static.HydrationLink without importing it, so the graph
// package keeps depending on nothing above it.
type HydrationLink struct {
	Text string
	Href string
}

// hydrationConfidence is deliberately below what rendered text carries. These
// strings were shipped by the page and are the ones it hydrates itself from,
// but nothing here watched them appear on screen, nothing knows where on the
// page they belong, and a payload holds strings for states a visitor may never
// reach -- an error message, a tooltip, the label on a button behind a login.
const hydrationConfidence = 0.55

// AdoptHydrationText builds blocks from a framework's server-rendered state.
//
// This runs only when the page produced nothing readable of its own. It is a
// recovery channel in the same sense that canvas OCR is: the answer of last
// resort on a page that would otherwise be reported empty, never a supplement
// to a page that rendered.
//
// The ordering caveat is the honest part. Rendered text has a position, so its
// reading order is measured; these strings have only the order they happen to
// sit in the payload, which is the order the framework serialised them and not
// necessarily the order anyone reads them. That is recorded on the artifact
// rather than smoothed over, because an agent quoting a heading as though it
// introduced the paragraph after it would be wrong in a way it could not detect.
func AdoptHydrationText(g *Graph, payload []string, links []HydrationLink) int {
	if g == nil || len(payload) == 0 {
		return 0
	}

	// Anything already in the artifact by a better route wins. On a page that
	// produced a heading and nothing else, the heading is not repeated here.
	seen := map[string]bool{}
	for i := range g.Blocks {
		seen[hydrationKey(g.Blocks[i].Text)] = true
	}
	for _, f := range g.Structured {
		seen[hydrationKey(f.Value)] = true
	}
	if g.Title != "" {
		seen[hydrationKey(g.Title)] = true
	}
	if g.Description != "" {
		seen[hydrationKey(g.Description)] = true
	}

	adopted := 0
	for _, raw := range payload {
		text := textnorm.CleanString(raw)
		if text == "" {
			continue
		}
		k := hydrationKey(text)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true

		g.Blocks = append(g.Blocks, Block{
			ID:         blockID(len(g.Blocks)),
			Type:       TypeParagraph,
			Text:       text,
			Order:      len(g.Blocks),
			Source:     SourceStatic,
			Score:      hydrationConfidence,
			Confidence: Bucket(hydrationConfidence),
			// Not speculative. That marker exists for text a model guessed at
			// from pixels, and the emitter drops it from the payload for
			// exactly that reason. These strings are verbatim from the served
			// bytes -- the same bytes the framework itself reads them from --
			// so nothing here is invented and withholding them would leave the
			// artifact empty while the words sat in the file.
			//
			// What is uncertain about them is placement, not wording, and that
			// is carried by the flag, the source and the audit note rather than
			// by pretending the text is unreliable.
			Verified:   VerificationNone,
			Region:     RegionMain,
			Flags:      []string{"hydration-payload-not-observed-rendered"},
		})
		adopted++
	}
	// Links go to the links channel, where a destination belongs, rather than
	// being flattened into prose. A bare "Ethereum" in a paragraph says less
	// than a link to ethereum.foundation labelled Ethereum, and the second is
	// what the page actually shows.
	haveHref := map[string]bool{}
	for _, l := range g.Links {
		haveHref[l.Href] = true
	}
	for _, l := range links {
		if haveHref[l.Href] {
			continue
		}
		haveHref[l.Href] = true
		label := textnorm.CleanString(l.Text)
		g.Links = append(g.Links, Link{
			Href:   l.Href,
			Text:   label,
			Region: RegionMain,
		})
		// And as an action, because that is the channel the rendered artifact
		// reads from. A link that reaches content.json and not index.md is
		// invisible to every consumer that reads the page rather than the
		// index, which is most of them.
		g.Actions = append(g.Actions, Action{
			ID:     actionID(len(g.Actions)),
			Type:   "link",
			Label:  label,
			Href:   l.Href,
			Region: RegionMain,
		})
	}

	if adopted == 0 {
		return 0
	}

	g.Stats.ContentNodes = 0
	g.Stats.ChromeNodes = 0
	for i := range g.Blocks {
		if g.Blocks[i].Region.IsChrome() {
			g.Stats.ChromeNodes++
		} else {
			g.Stats.ContentNodes++
		}
	}
	return adopted
}

func hydrationKey(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}
