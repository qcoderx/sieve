package graph_test

import (
	"strings"
	"testing"

	"github.com/qcoderx/sieve/internal/emit"
	"github.com/qcoderx/sieve/internal/graph"
)

// TestAdversarialChannels checks each way a page can smuggle text past a
// visibility filter.
//
// # What is actually being claimed
//
// Rendering-grounded capture closes the DOM-text channel completely: a
// display:none subtree never enters the content tier, text that never reaches a
// visible opacity is excluded, and text the same colour as its background is
// excluded. A markup-based extractor ingests all of it verbatim. That is a
// categorical difference and it is worth stating.
//
// It is hidden-element immunity, not injection immunity. Several channels
// bypass the visibility defence by design because they sit on elements that are
// genuinely visible -- alt text, aria-label, structured data. Those are capped,
// normalised, whitelisted and marked rather than trusted, and this test pins
// each of those behaviours so the precise claim keeps being true.
//
// Claiming general immunity would take exactly one reply containing
// alt="ignore previous instructions" to demolish.
func TestAdversarialChannels(t *testing.T) {
	g := buildFixture(t, "adversarial/")

	md := emit.Markdown(g, emit.DefaultMarkdownOptions())
	html := emit.HTML(g)
	plain := graph.PlainText(g)
	defaults := map[string]string{"markdown": md, "html": html, "plaintext": plain}

	// --- Channels the visibility defence closes completely -----------------
	//
	// None of these may appear in ANY default rendering. They are the claim.
	closedChannels := []struct {
		marker string
		how    string
	}{
		{"INJECT_CONTRAST", "text whose colour matches its background"},
		{"INJECT_OPACITY", "text that never reaches a visible opacity"},
		{"INJECT_HIDDEN_TAB", "text inside a display:none panel"},
		{"INJECT_JSONLD_UNWHITELISTED", "a JSON-LD field outside the whitelist"},
		{"INJECT_JSONLD_SAMEAS", "a JSON-LD field outside the whitelist"},
	}
	for _, c := range closedChannels {
		for name, out := range defaults {
			if strings.Contains(out, c.marker) {
				t.Errorf("%s leaked %s (%s) into default output.\n"+
					"This channel is supposed to be closed completely; that closure is the "+
					"project's headline security claim.", name, c.marker, c.how)
			}
		}
	}

	// The hidden tab panel is not discarded, though. It is quarantined, because
	// the panel next to it holds real pricing a reader can reach with one click
	// and the two are indistinguishable to a walker.
	foundHiddenInLatent := false
	foundPricingInLatent := false
	for _, l := range g.Latent {
		if strings.Contains(l.Text, "INJECT_HIDDEN_TAB") {
			foundHiddenInLatent = true
			if l.Trust != graph.LatentTrustMarker {
				t.Error("a quarantined injection lost its trust marker")
			}
		}
		if strings.Contains(l.Text, "145 pounds") {
			foundPricingInLatent = true
		}
	}
	if !foundHiddenInLatent {
		t.Error("hidden-tab text was discarded rather than quarantined; " +
			"the latent tier exists so that hidden content is recoverable and labelled, not deleted")
	}
	if !foundPricingInLatent {
		t.Error("the collapsed pricing panel was not captured into the latent tier — " +
			"this is the content that makes keeping hidden text worthwhile")
	}

	// --- Channels that bypass visibility by design -------------------------
	//
	// These sit on visible elements. They are bounded and marked, not excluded.
	t.Run("metadata channels are capped and marked", func(t *testing.T) {
		for _, m := range g.MediaAll {
			if len([]rune(m.Alt)) > 320 {
				t.Errorf("alt text was not capped: %d runes", len([]rune(m.Alt)))
			}
		}
		// Strict mode is the minimal-trust surface: it drops them entirely.
		strict := emit.Markdown(g, emit.MarkdownOptions{Strict: true})
		for _, marker := range []string{"INJECT_ALT", "INJECT_ARIA"} {
			if strings.Contains(strict, marker) {
				t.Errorf("strict mode retained %s; it exists precisely to drop metadata channels", marker)
			}
		}
	})

	// --- Unicode normalisation ---------------------------------------------
	t.Run("control characters are stripped at the graph boundary", func(t *testing.T) {
		for _, b := range g.Blocks {
			for _, r := range b.Text {
				switch r {
				case 0x202A, 0x202B, 0x202C, 0x202D, 0x202E, 0x2066, 0x2067, 0x2068, 0x2069:
					t.Errorf("block %s retained a bidi override: %q", b.ID, b.Text)
				case 0x200B, 0x200C, 0x200D, 0x2060, 0xFEFF:
					t.Errorf("block %s retained a zero-width character: %q", b.ID, b.Text)
				case 0x200E, 0x200F:
					t.Errorf("block %s retained a directional mark: %q", b.ID, b.Text)
				}
			}
		}
		// The visible words survive; only the invisible machinery is removed.
		if !containsBlock(g, "Tuesday to Saturday") {
			t.Error("normalisation removed legitimate text along with the control characters")
		}
	})

	// --- Markdown structure injection --------------------------------------
	t.Run("markdown structure cannot be forged", func(t *testing.T) {
		for _, line := range strings.Split(md, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "# INJECT") || strings.HasPrefix(trimmed, "- INJECT") {
				t.Errorf("a page forged Markdown structure: %q", trimmed)
			}
		}
		// The text itself is still present -- escaped, not censored. Removing
		// it would be lying about what the page said.
		if !strings.Contains(md, "INJECT_MARKDOWN_HEADING") {
			t.Error("escaping removed the text instead of neutralising its structure")
		}
	})

	// --- The gap declaration ------------------------------------------------
	t.Run("collapsed controls are declared", func(t *testing.T) {
		var labels []string
		for _, gap := range g.Gaps {
			labels = append(labels, gap.Label)
		}
		joined := strings.Join(labels, ", ")
		if !strings.Contains(joined, "Pricing") {
			t.Errorf("the collapsed Pricing tab was not declared as a gap; found: %v", labels)
		}
		if !strings.Contains(md, "Content not shown on this page") {
			t.Error("default Markdown did not declare the gaps section")
		}
	})

	// --- Real content still arrives ----------------------------------------
	//
	// A filter that removes everything passes every test above and is useless.
	t.Run("legitimate content survives", func(t *testing.T) {
		for _, want := range []string{
			"wheel-thrown and wood-fired",
			"anagama kiln is fired twice a year",
			"first Saturday of each month",
			"Kirkstall Road",
		} {
			if !containsBlock(g, want) {
				t.Errorf("legitimate content missing: %q", want)
			}
		}
		// Whitelisted structured data does come through.
		var facts []string
		for _, f := range g.Structured {
			facts = append(facts, f.Field+"="+f.Value)
		}
		joined := strings.Join(facts, " ")
		if !strings.Contains(joined, "1998") {
			t.Errorf("whitelisted structured data was dropped: %v", facts)
		}
	})
}

func containsBlock(g *graph.Graph, want string) bool {
	for _, b := range g.Blocks {
		if strings.Contains(b.Text, want) {
			return true
		}
	}
	return false
}
