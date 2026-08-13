package graph

import (
	"strings"

	"github.com/qcoderx/sieve/internal/capture"
	"github.com/qcoderx/sieve/internal/textnorm"
)

// AdoptServedText folds text from the served HTML into a rendered artifact,
// but only where the render itself has proved that text of that kind is shown
// to visitors.
//
// # Why this exists
//
// A scroll-driven page can hold its content in two places at once, and sieve
// sees both. pear.no ships its whole argument -- the terms, the selectivity,
// the application, the FAQ -- in the served HTML, inside sections marked
// aria-hidden with opacity zero until they scrub into view. It builds a
// different DOM after hydration, mounting those sections only as the viewport
// reaches them. So the static tier holds the sections the browser never travels
// far enough to see, and the browser holds the hero and the pillars that are
// not in the served bytes at all. Each tier is missing exactly what the other
// has, and sieve used to keep one and discard the other.
//
// # Why it is not a hole in the visibility defence
//
// The rule that hidden text is not content is what makes this tool safe to
// point at the open web, and it is not being relaxed. What is added is a
// second, independent observation standing behind the first.
//
// The promotion needs proof, and the proof is the render: if text that the
// served HTML marked hidden was afterwards seen on screen by the browser, then
// on this page that marking is a reveal state and not a concealment. Pages
// prove that about themselves, one at a time, in the same run. A page that
// hides text and never shows any of it proves nothing, and nothing is adopted
// from it -- which is precisely the injection case, where an attacker's hidden
// block has no visible counterpart anywhere on the page.
//
// Everything adopted is marked: speculative verification, an explicit flag, and
// a line in the audit. It is offered as what it is -- text the site served and
// sieve did not personally watch appear -- and never as observed content.
func AdoptServedText(g *Graph, served []capture.LatentNode) (adopted int, proof int) {
	if g == nil || len(served) == 0 {
		return 0, 0
	}

	// Two independent witnesses that this page shows what it serves hidden.
	//
	// The first is the render: text the served HTML marked hidden, afterwards
	// seen on screen.
	//
	// The second is the page's own structured data. A site that publishes a
	// passage as schema.org FAQPage content is telling search engines and
	// assistants to quote it -- the opposite of concealing it -- and when the
	// same words appear in a section the markup marks hidden, that marking is
	// plainly a reveal state. This witness costs nothing and needs no browser,
	// so a page that documents itself well can be read correctly at tier 0.
	seen := make(map[string]bool, len(g.Blocks)+len(g.FAQ))
	for i := range g.Blocks {
		if g.Blocks[i].Region.IsChrome() {
			continue
		}
		for _, k := range adoptionKeys(g.Blocks[i].Text) {
			seen[k] = true
		}
	}
	for _, qa := range g.FAQ {
		for _, k := range adoptionKeys(qa.Question) {
			seen[k] = true
		}
		for _, k := range adoptionKeys(qa.Answer) {
			seen[k] = true
		}
	}
	for _, f := range g.Structured {
		for _, k := range adoptionKeys(f.Value) {
			seen[k] = true
		}
	}

	type candidateRun struct {
		text   string
		reason string
	}
	var pending []candidateRun
	dup := map[string]bool{}

	for i := range served {
		n := &served[i]
		// Only the reasons that describe a page's own reveal machinery. A
		// display:none subtree is a different thing -- a closed menu, a template,
		// an off-screen instruction -- and stays where it is.
		if n.Reason != "aria-hidden" && n.Reason != "opacity-zero" {
			continue
		}
		text := textnorm.CleanString(n.Text)
		if len([]rune(text)) < minAdoptedRunes {
			continue
		}
		// Proof is counted in matched fragments, not in matched runs.
		//
		// A served-hidden section arrives as one long run holding a whole FAQ,
		// and the same words arrive from the witnesses as a dozen separate
		// questions and answers. Counting the run once records a single
		// coincidence where the evidence is actually a dozen sentences agreeing,
		// and the threshold then never clears on the pages that most deserve it.
		hits := 0
		for _, k := range adoptionKeys(text) {
			if seen[k] {
				hits++
			}
		}
		if hits > 0 {
			proof += hits
			// Already represented in the artifact by the witness that matched it.
			continue
		}
		key := strings.ToLower(text)
		if dup[key] {
			continue
		}
		dup[key] = true
		pending = append(pending, candidateRun{text: text, reason: n.Reason})
	}

	// One coincidence is not proof. Two independent runs of served-hidden text
	// turning up on screen is a page telling us how it works.
	if proof < minAdoptionProof || len(pending) == 0 {
		return 0, proof
	}

	for _, c := range pending {
		g.Blocks = append(g.Blocks, Block{
			ID:         blockID(len(g.Blocks)),
			Type:       TypeParagraph,
			Text:       c.text,
			Order:      len(g.Blocks),
			Source:     SourceDOM,
			Score:      adoptedConfidence,
			Confidence: Bucket(adoptedConfidence),
			Verified:   VerificationSpeculative,
			Region:     RegionMain,
			Flags:      []string{"served-html-not-observed-rendered"},
		})
		adopted++
	}

	g.Stats.ContentNodes = 0
	for i := range g.Blocks {
		if !g.Blocks[i].Region.IsChrome() {
			g.Stats.ContentNodes++
		}
	}
	return adopted, proof
}

// minAdoptedRunes keeps single words and stray labels out. Adoption is for
// prose the render could not reach, not for every fragment in the document.
const minAdoptedRunes = 24

// minAdoptionProof is how many sentence-length fragments of served-hidden text
// must have been independently witnessed before the rest of it is believed.
const minAdoptionProof = 3

// adoptedConfidence is deliberately below anything the render vouches for.
const adoptedConfidence = 0.45

// adoptionKeys reduces a run to comparable fragments.
//
// The two tiers rarely produce byte-identical strings for the same words: the
// served copy is one long run of a whole section, and the rendered copy is a
// heading and a paragraph reassembled from animated spans. Matching on
// sentence-length fragments finds the overlap that matching on whole runs
// misses.
func adoptionKeys(s string) []string {
	s = strings.ToLower(textnorm.CleanString(s))
	if s == "" {
		return nil
	}
	var out []string
	for _, part := range strings.FieldsFunc(s, func(r rune) bool {
		return r == '.' || r == '!' || r == '?' || r == '·' || r == '\n'
	}) {
		f := strings.Join(strings.Fields(part), " ")
		if len([]rune(f)) >= minAdoptedRunes {
			out = append(out, f)
		}
	}
	return out
}
