package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/qcoderx/sieve/internal/graph"
)

// TestTokenReportDoesNotFlatterItself covers the two ways this report could
// mislead, both of which it did during development.
//
// It is the one claim in this project a sceptical reader can check casually --
// one page fetch, no API key -- which makes it the one most worth being exact
// about. A table that overstates on a small page is worth less than no table.
func TestTokenReportDoesNotFlatterItself(t *testing.T) {
	t.Run("an increase is not reported as a reduction", func(t *testing.T) {
		// A tiny page: describing it costs more than reading it. True, and it
		// must read as true rather than as "0.4x smaller", which is how this
		// printed before.
		var b bytes.Buffer
		printTokenReport(&b, tokenReport{
			URL: "https://example.com", RawPage: 193, Manifest: 508,
			Artifact: 64, Sections: 1, ToolSurface: 1737,
			Outcome: graph.Outcome{Status: graph.StatusOK},
		})
		out := b.String()
		if strings.Contains(out, "0.4x smaller") {
			t.Error("an increase was printed as a reduction")
		}
		if !strings.Contains(out, "larger") {
			t.Error("the manifest costing more than the page was not stated")
		}
		// And it must say plainly that sieve is the wrong tool here.
		if !strings.Contains(out, "nothing to offer here") {
			t.Error("a page too small to be worth distilling was not called out")
		}
	})

	t.Run("a failed read does not advertise a ratio as a reading saving", func(t *testing.T) {
		var b bytes.Buffer
		printTokenReport(&b, tokenReport{
			URL: "https://hatom.com", RawPage: 92222, Manifest: 1187,
			Artifact: 433, Sections: 1, ToolSurface: 1737,
			Outcome: graph.Outcome{Status: graph.StatusChallenge,
				Evidence: []string{"an entry screen was not passed"}},
		})
		out := b.String()
		// The ratio is real, but it compares a page against an artifact
		// describing the screen in front of it, and the reader has to be told.
		if !strings.Contains(out, "challenge") {
			t.Error("a non-ok outcome was not surfaced beside the figures")
		}
		if !strings.Contains(out, "not the page") {
			t.Error("the figures were presented without saying what they describe")
		}
	})
}

// TestTokenReportUsesTheArtifactsOwnCount: the served-page figure must be the
// one the manifest publishes.
//
// Recomputing it from a separately carried copy of the HTML produced a report
// saying zero for a page whose own manifest said 193. Two numbers for one
// quantity is the recurring failure in this project, and a benchmark is the
// worst place for it.
func TestTokenReportUsesTheArtifactsOwnCount(t *testing.T) {
	g := &graph.Graph{URL: "https://example.com"}
	g.Stats.OriginalTokens = 193
	g.Blocks = append(g.Blocks, graph.Block{
		ID: "b_000", Type: graph.TypeParagraph, Text: "Example Domain.", Source: graph.SourceDOM,
	})
	g.Recount()

	// A deliberately empty rawHTML: if the report consults it rather than the
	// graph, the figure collapses to zero, which is the bug.
	r := measureTokens(g, "")
	if r.RawPage != 193 {
		t.Errorf("raw page = %d, want 193 (the figure the artifact publishes)", r.RawPage)
	}
}
