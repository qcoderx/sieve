package graph_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/qcoderx/sieve/internal/graph"
	"github.com/qcoderx/sieve/internal/render"
)

func buildFixture(t *testing.T, page string) *graph.Graph {
	t.Helper()
	if render.ChromiumPath("") == "" {
		t.Skip("no Chromium available")
	}
	srv := httptest.NewServer(http.FileServer(http.Dir("../../testdata/pages")))
	t.Cleanup(srv.Close)

	opts := render.DefaultOptions()
	opts.Budget = 90 * time.Second
	b, err := render.Launch(context.Background(), opts)
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	t.Cleanup(b.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	res, err := b.Sweep(ctx, srv.URL+"/"+page, nil)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	g, err := graph.Build(graph.Input{
		RequestedURL:  res.RequestedURL,
		FinalURL:      res.FinalURL,
		Merged:        res.Merged,
		Notes:         res.Notes,
		ReachedBottom: true,
		Now:           time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC),
		Generator:     "sieve/test",
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return g
}

func TestBuildImmersive(t *testing.T) {
	g := buildFixture(t, "immersive/")

	t.Logf("title=%q", g.Title)
	t.Logf("summary=%q", g.Summary)
	t.Logf("order: basis=%s confidence=%s agreement=%.2f",
		g.Audit.OrderBasis, g.Audit.OrderConfidence, g.Audit.OrderAgreement)
	t.Logf("audit: retention=%.3f (%d/%d chars) headingSep=%.2f/%s",
		g.Audit.GraphRetention, g.Audit.EmittedChars, g.Audit.ObservedChars,
		g.Audit.HeadingSeparation, g.Audit.HeadingConfidence)
	t.Logf("stats: raw=%d content=%d chrome=%d latent=%d dropped=%d artifactTokens=%d",
		g.Stats.RawNodes, g.Stats.ContentNodes, g.Stats.ChromeNodes,
		g.Stats.LatentNodes, g.Stats.DroppedNodes, g.Stats.ArtifactTokens)
	for _, l := range g.Latent {
		t.Logf("latent %s [%s behind %q] %s", l.ID, l.Reason, l.ControlLabel, short(l.Text))
	}
	for _, gp := range g.Gaps {
		t.Logf("gap %q (%s): %s", gp.Label, gp.Kind, gp.Reason)
	}
	for _, s := range g.Sections {
		t.Logf("section %s L%d %q blocks=%d chars=%d", s.ID, s.Level, s.Title, s.BlockCount, s.Chars)
	}
	for _, b := range g.Blocks {
		lvl := ""
		if b.Level > 0 {
			lvl = strings.Repeat("#", b.Level) + " "
		}
		t.Logf("%s [%s/%s conf=%s fs=%.0f w=%d] %s%s",
			b.ID, b.Type, b.Region, b.Confidence, b.Style.FontSize, b.Style.Weight, lvl, short(b.Text))
	}

	// Split text must come back as one heading, not thirty single-character
	// ones. This is the whole point of the reassembly stage.
	if !hasBlock(g, func(b graph.Block) bool {
		return b.Type == graph.TypeHeading && b.Text == "Furniture that outlives its owner"
	}) {
		t.Error("split headline was not reassembled into a single heading")
	}
	for _, b := range g.Blocks {
		if len([]rune(b.Text)) == 1 {
			t.Errorf("single-character block survived reassembly: %s %q", b.ID, b.Text)
		}
	}

	// A div styled as a headline must be recognised as a heading. There is no
	// tag to read it from; only the typography says so.
	if !hasBlock(g, func(b graph.Block) bool {
		return b.Type == graph.TypeHeading && b.Text == "The studio"
	}) {
		t.Error("styled div was not recognised as a heading")
	}

	// The headline is set at 76px and the section titles at 44px, so the
	// headline must outrank them.
	var headlineLvl, studioLvl int
	for _, b := range g.Blocks {
		switch b.Text {
		case "Furniture that outlives its owner":
			headlineLvl = b.Level
		case "The studio":
			studioLvl = b.Level
		}
	}
	if headlineLvl == 0 || studioLvl == 0 || headlineLvl >= studioLvl {
		t.Errorf("heading hierarchy wrong: headline=L%d studio=L%d", headlineLvl, studioLvl)
	}

	// The pinned navigation must be chrome, not content.
	for _, b := range g.Blocks {
		if b.Text == "Materials" && b.Href != "" && !b.Region.IsChrome() {
			t.Errorf("pinned nav link classified as %s, expected chrome", b.Region)
		}
	}

	// Two-column prose must not interleave. In the source, the left column ends
	// with "keep everything else moving" and the right begins with "Every piece
	// is made to order".
	order := map[string]int{}
	for _, b := range g.Blocks {
		for _, frag := range []string{"converted bakery", "everything else moving", "made to order", "veneer over particle board"} {
			if strings.Contains(b.Text, frag) {
				order[frag] = b.Order
			}
		}
	}
	if len(order) == 4 {
		if !(order["converted bakery"] < order["everything else moving"] &&
			order["everything else moving"] < order["made to order"] &&
			order["made to order"] < order["veneer over particle board"]) {
			t.Errorf("two-column reading order interleaved: %v", order)
		}
	} else {
		t.Errorf("column test fragments missing: %v", order)
	}

	// Shadow DOM content must be present and correctly typed.
	if !hasBlock(g, func(b graph.Block) bool {
		return strings.Contains(b.Text, "quietly confident furniture")
	}) {
		t.Error("shadow DOM quote missing from graph")
	}

	// The form and its schema.
	var form *graph.Action
	for i := range g.Actions {
		if g.Actions[i].Type == "form" {
			form = &g.Actions[i]
		}
	}
	if form == nil {
		t.Fatal("no form action")
	}
	if form.Method != "POST" {
		t.Errorf("form method = %q", form.Method)
	}
	if len(form.Fields) != 3 {
		t.Errorf("form has %d fields, want 3 (hidden field should be excluded)", len(form.Fields))
	}

	// Image with alt and caption becomes a block in the flow.
	if !hasBlock(g, func(b graph.Block) bool {
		return b.Type == graph.TypeImage && strings.Contains(b.Text, "quarter-sawn oak grain")
	}) {
		t.Error("described image did not become a block in the reading order")
	}
	// The figcaption is rendered text and is already its own block; repeating
	// it inside the image block would charge for the same sentence twice.
	captions := 0
	for _, b := range g.Blocks {
		if strings.Contains(b.Text, "medullary ray fleck") {
			captions++
		}
	}
	if captions != 1 {
		t.Errorf("figcaption text appears in %d blocks, want 1", captions)
	}

	// Mixed inline content must keep its word boundaries.
	for _, b := range g.Blocks {
		if strings.Contains(b.Text, "hello@example.com") &&
			!strings.Contains(b.Text, "Lisboa. hello@example.com") {
			t.Errorf("word boundary lost around inline link: %q", b.Text)
		}
	}

	// A lede paragraph set larger than body copy is still a paragraph.
	for _, b := range g.Blocks {
		if strings.HasPrefix(b.Text, "We make chairs") && b.Type != graph.TypeParagraph {
			t.Errorf("lede paragraph classified as %s", b.Type)
		}
		if strings.Contains(b.Text, "quietly confident") && b.Type != graph.TypeQuote {
			t.Errorf("blockquote classified as %s", b.Type)
		}
	}

	// Navigation must not generate headings and pollute the outline.
	for _, b := range g.Blocks {
		if b.Region.IsChrome() && b.Type == graph.TypeHeading {
			t.Errorf("chrome block became a heading: %s %q", b.ID, b.Text)
		}
	}

	// Determinism: the content hash must not depend on wall-clock time.
	if g.ContentHash == "" || !strings.HasPrefix(g.ContentHash, "sha256:") {
		t.Errorf("content hash = %q", g.ContentHash)
	}
}

func hasBlock(g *graph.Graph, pred func(graph.Block) bool) bool {
	for _, b := range g.Blocks {
		if pred(b) {
			return true
		}
	}
	return false
}

func short(s string) string {
	if len(s) <= 90 {
		return s
	}
	return s[:90] + "…"
}
