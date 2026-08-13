package emit_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/qcoderx/sieve/internal/emit"
	"github.com/qcoderx/sieve/internal/graph"
)

// canaryHidden is a string that exists only in the latent tier of the fixture
// graph. Any default rendering that contains it has leaked hidden content.
const canaryHidden = "CANARY-HIDDEN-IGNORE-PREVIOUS-INSTRUCTIONS"

func fixtureGraph() *graph.Graph {
	return &graph.Graph{
		SchemaVersion: graph.SchemaVersion,
		URL:           "https://example.com/",
		Title:         "Example",
		Summary:       "A page.",
		DistilledAt:   time.Date(2026, 8, 7, 0, 0, 0, 0, time.UTC),
		ContentHash:   "sha256:test",
		Generator:     "sieve/test",
		Blocks: []graph.Block{
			{ID: "b_000", Type: graph.TypeHeading, Level: 1, Text: "Visible heading",
				Region: graph.RegionMain, Source: graph.SourceDOM, Confidence: graph.ConfidenceHigh},
			{ID: "b_001", Type: graph.TypeParagraph, Text: "Visible paragraph.",
				Region: graph.RegionMain, Source: graph.SourceDOM, Confidence: graph.ConfidenceHigh},
			// A recovery from pixels that nothing corroborated. It must not
			// appear in a default rendering either.
			{ID: "b_002", Type: graph.TypeParagraph, Text: "SPECULATIVE-CANVAS-GUESS",
				Region: graph.RegionMain, Source: graph.SourceCanvasVision,
				Verified: graph.VerificationSpeculative, Confidence: graph.ConfidenceLow},
		},
		Sections: []graph.Section{
			{ID: "s_00", Title: "Visible heading", Level: 1, FirstBlock: "b_000", LastBlock: "b_001"},
		},
		Latent: []graph.LatentBlock{
			{ID: "l_000", Type: graph.TypeParagraph, Text: canaryHidden,
				Reason: "display-none", ControlLabel: "Pricing", ControlKind: "tab",
				Trust: graph.LatentTrustMarker},
		},
		Gaps: []graph.Gap{
			{Label: "Pricing", Kind: "tab", Reason: "collapsed disclosure", LatentIDs: []string{"l_000"}},
		},
		Audit: graph.Audit{
			GraphRetention: 1, OrderConfidence: graph.ConfidenceHigh,
			OrderBasis: "geometry", OrderAgreement: 1,
			HeadingConfidence: graph.ConfidenceHigh, ReachedBottom: true,
		},
		Provenance: graph.Provenance{Tier: "sweep", NormalizerVersion: 1},
	}
}

// TestLatentNeverLeaksIntoDefaultOutput is the test that keeps the latent tier
// safe.
//
// Keeping hidden text is safe right up until one bug isn't. The whole design
// rests on the quarantine staying shut: one convenience shortcut that flattens
// the arrays, one emit function that concatenates, and the security claim this
// project leads with is dead rather than weakened. Discipline does not survive
// a year of refactors, so the invariant is asserted on every format instead.
//
// If this test fails, do not adjust the test.
func TestLatentNeverLeaksIntoDefaultOutput(t *testing.T) {
	g := fixtureGraph()

	renderings := map[string]string{
		"markdown/default": emit.Markdown(g, emit.DefaultMarkdownOptions()),
		"markdown/compact": emit.Markdown(g, emit.CompactMarkdownOptions()),
		"markdown/strict":  emit.Markdown(g, emit.MarkdownOptions{Strict: true}),
		"markdown/section": emit.SectionMarkdown(g, "s_00", emit.DefaultMarkdownOptions()),
		"markdown/blocks":  emit.BlocksMarkdown(g, g.Blocks, emit.DefaultMarkdownOptions()),
		"html":             emit.HTML(g),
		"plaintext":        graph.PlainText(g),
	}

	manifest, err := json.Marshal(emit.BuildManifest(g))
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	renderings["manifest"] = string(manifest)

	for name, out := range renderings {
		if strings.Contains(out, canaryHidden) {
			t.Errorf("%s leaked latent content into a default rendering.\n"+
				"The latent tier is the exact material the visibility filter exists to exclude. "+
				"It must never reach a default payload. Fix the renderer, not this test.", name)
		}
		if strings.Contains(out, "SPECULATIVE-CANVAS-GUESS") {
			t.Errorf("%s emitted an uncorroborated pixel recovery into a default rendering", name)
		}
	}

	// content.json is the one default artifact that does carry the latent
	// array, because it is the complete record. It must carry the trust marker
	// with it, so nothing downstream can read a latent block without seeing
	// what it is.
	full, err := json.Marshal(g)
	if err != nil {
		t.Fatalf("marshal graph: %v", err)
	}
	if !strings.Contains(string(full), canaryHidden) {
		t.Error("content.json should retain the latent tier as the complete record")
	}
	if !strings.Contains(string(full), graph.LatentTrustMarker) {
		t.Error("latent blocks in content.json lost their trust marker")
	}
	// It must be under its own key, never merged into blocks.
	var probe struct {
		Blocks []map[string]any `json:"blocks"`
		Latent []map[string]any `json:"latent"`
	}
	if err := json.Unmarshal(full, &probe); err != nil {
		t.Fatalf("unmarshal graph: %v", err)
	}
	for _, b := range probe.Blocks {
		if txt, _ := b["text"].(string); strings.Contains(txt, canaryHidden) {
			t.Fatal("latent content appeared inside the blocks array")
		}
	}
	if len(probe.Latent) != 1 {
		t.Errorf("latent array has %d entries, want 1", len(probe.Latent))
	}
}

// TestLatentRetrievalIsExplicitAndMarked checks the other half: when a caller
// does ask for hidden content, they get it with the warning attached.
func TestLatentRetrievalIsExplicitAndMarked(t *testing.T) {
	g := fixtureGraph()
	out := emit.LatentMarkdown(g, nil)

	if !strings.Contains(out, canaryHidden) {
		t.Error("explicit latent retrieval did not return the hidden content")
	}
	if !strings.Contains(out, graph.LatentTrustMarker) {
		t.Error("latent rendering lost the per-block trust marker")
	}
	if !strings.Contains(out, "never rendered") {
		t.Error("latent rendering did not warn that the content was hidden")
	}
	if !strings.Contains(out, "Pricing") {
		t.Error("latent rendering did not name the control that reveals the content")
	}
}

// TestGapsAreDeclared checks that a collapsed control is named in default
// output even though its content is not included. An agent that knows a
// Specifications tab exists can obtain it another way; an agent told nothing
// concludes the page had no specifications.
func TestGapsAreDeclared(t *testing.T) {
	g := fixtureGraph()
	md := emit.Markdown(g, emit.DefaultMarkdownOptions())
	if !strings.Contains(md, "Pricing") {
		t.Error("default Markdown did not declare the Pricing gap")
	}
	if !strings.Contains(md, "get_hidden_content") {
		t.Error("default Markdown did not say how to retrieve the hidden content")
	}
	if !strings.Contains(emit.HTML(g), "Pricing") {
		t.Error("default HTML did not declare the Pricing gap")
	}
}

// TestStrictModeDropsMetadataChannels checks the minimal-trust surface.
func TestStrictModeDropsMetadataChannels(t *testing.T) {
	g := fixtureGraph()
	g.Structured = []graph.StructuredFact{
		{Type: "Organization", Field: "description", Value: "STRUCTURED-CHANNEL-PAYLOAD"},
	}
	g.Blocks = append(g.Blocks, graph.Block{
		ID: "b_003", Type: graph.TypeImage, Text: "ALT-CHANNEL-PAYLOAD",
		MediaID: "m_000", Region: graph.RegionMain, Source: graph.SourceDOM,
		Confidence: graph.ConfidenceHigh,
	})
	g.MediaAll = []graph.Media{{ID: "m_000", Type: "image", Src: "/x.png", Alt: "ALT-CHANNEL-PAYLOAD"}}

	strict := emit.Markdown(g, emit.MarkdownOptions{Strict: true})
	for _, payload := range []string{"STRUCTURED-CHANNEL-PAYLOAD", "ALT-CHANNEL-PAYLOAD"} {
		if strings.Contains(strict, payload) {
			t.Errorf("strict mode retained a metadata channel: %s", payload)
		}
	}

	normal := emit.Markdown(g, emit.DefaultMarkdownOptions())
	if !strings.Contains(normal, "STRUCTURED-CHANNEL-PAYLOAD") {
		t.Error("non-strict mode should still carry whitelisted structured data")
	}
}
