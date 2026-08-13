package emit_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/qcoderx/sieve/internal/emit"
	"github.com/qcoderx/sieve/internal/graph"
)

// docs/ARTIFACT.md is a promise, and a promise nothing checks is a wish.
//
// These tests fail when the artifact format changes. That is their whole
// purpose: within schema 1.x fields are added, never removed or repurposed, and
// a consumer that reads manifest.outcome.status must keep working. If one of
// these fails, either put the field back or bump the schema version and say so
// in the document — but do not quietly delete the line from the test.

// contractGraph is a graph with every part of the format populated, so a field
// disappearing shows up here rather than in someone's integration.
func contractGraph() *graph.Graph {
	g := &graph.Graph{
		SchemaVersion: graph.SchemaVersion,
		URL:           "https://example.com/",
		FinalURL:      "https://example.com/",
		Title:         "Workshop",
		Summary:       "A workshop that has fired kilns since 1923.",
		Lang:          "en",
		Generator:     "sieve/test",
		Outcome: graph.Outcome{
			Status:     graph.StatusPartial,
			Evidence:   []string{"a tier was tried and fell back"},
			HTTPStatus: 200,
		},
		Blocks: []graph.Block{
			{ID: "b_000", Type: graph.TypeHeading, Level: 1, Text: "Materials", Source: graph.SourceDOM},
			{ID: "b_001", Type: graph.TypeParagraph, Text: "Stoneware and porcelain.", Source: graph.SourceDOM},
		},
		Gaps: []graph.Gap{{Label: "Specifications", Kind: "disclosure", Reason: "never opened"}},
	}
	g.Sections = []graph.Section{{
		ID: "s_deadbeef01", Title: "Materials", Level: 1,
		FirstBlock: "b_000", LastBlock: "b_001", BlockCount: 2, Chars: 24, Tokens: 6,
	}}
	g.Provenance.Tier = "sweep"
	g.Provenance.TierReason = "the served HTML is a shell"
	g.Recount()
	return g
}

// TestManifestKeepsItsContractedFields walks the field list in docs/ARTIFACT.md.
func TestManifestKeepsItsContractedFields(t *testing.T) {
	raw, err := json.Marshal(emit.BuildManifest(contractGraph()))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}

	top := []string{
		"schema_version", "outcome", "url", "title", "summary",
		"content_hash", "distilled_at", "sections", "counts", "stats",
		"audit", "provenance", "guidance",
	}
	for _, k := range top {
		if _, ok := m[k]; !ok {
			t.Errorf("manifest lost %q, which docs/ARTIFACT.md promises", k)
		}
	}

	// The outcome is the field a consumer reads before anything else.
	oc, _ := m["outcome"].(map[string]any)
	if oc == nil {
		t.Fatal("manifest.outcome is not an object")
	}
	for _, k := range []string{"status", "evidence", "http_status"} {
		if _, ok := oc[k]; !ok {
			t.Errorf("manifest.outcome lost %q", k)
		}
	}

	sec, _ := m["sections"].([]any)
	if len(sec) == 0 {
		t.Fatal("manifest has no sections")
	}
	s0, _ := sec[0].(map[string]any)
	for _, k := range []string{"id", "title", "level", "blocks", "chars", "est_tokens",
		"first_block", "last_block"} {
		if _, ok := s0[k]; !ok {
			t.Errorf("manifest.sections[] lost %q", k)
		}
	}

	counts, _ := m["counts"].(map[string]any)
	for _, k := range []string{"blocks", "actions", "links", "media", "latent", "est_total_tokens"} {
		if _, ok := counts[k]; !ok {
			t.Errorf("manifest.counts lost %q", k)
		}
	}
}

// TestOutcomeStatusSetIsClosed: the set is documented as closed, and an agent
// switching on it needs that to hold. A new status is a schema change.
func TestOutcomeStatusSetIsClosed(t *testing.T) {
	documented := map[graph.Status]bool{
		graph.StatusOK: true, graph.StatusBlocked: true, graph.StatusChallenge: true,
		graph.StatusAuthRequired: true, graph.StatusSPAShell: true,
		graph.StatusEmptyAfterRender: true, graph.StatusPartial: true,
	}
	if len(documented) != 7 {
		t.Fatalf("the fixture lists %d statuses, want 7", len(documented))
	}
	// Every status the decision function can reach must be one of them.
	inputs := []graph.OutcomeInput{
		{HTTPStatus: 200, Rendered: true},
		{HTTPStatus: 403},
		{HTTPStatus: 401},
		{HTTPStatus: 200, RobotsRefused: true},
		{HTTPStatus: 200, Blocked: true, BlockedReason: "Cloudflare challenge"},
		{HTTPStatus: 200, EntryGate: "ENTER"},
		{HTTPStatus: 200, ShellHTML: true},
		{HTTPStatus: 200, Rendered: true, TierFellBack: true, TierReason: "fell back"},
	}
	for _, in := range inputs {
		for _, blocks := range []int{0, 5} {
			got := graph.DecideOutcome(in, blocks)
			if !documented[got.Status] {
				t.Errorf("DecideOutcome produced %q, which docs/ARTIFACT.md does not list",
					got.Status)
			}
		}
	}
}

// TestSchemaVersionMatchesTheDocument: the document names the version it
// describes, and the two drifting apart is worse than either being wrong.
func TestSchemaVersionMatchesTheDocument(t *testing.T) {
	if graph.SchemaVersion != "1.0" {
		t.Errorf("SchemaVersion is %q but docs/ARTIFACT.md documents 1.0. "+
			"Update the document, its compatibility section, and this test together.",
			graph.SchemaVersion)
	}
}

// TestFrontMatterCarriesTheOutcome: an agent reading index.md rather than the
// JSON must still be able to tell whether the read worked.
func TestFrontMatterCarriesTheOutcome(t *testing.T) {
	md := emit.Markdown(contractGraph(), emit.DefaultMarkdownOptions())
	head, _, _ := strings.Cut(strings.TrimPrefix(md, "---\n"), "\n---")
	for _, want := range []string{"outcome:", "tier:", "graph_retention:"} {
		if !strings.Contains(head, want) {
			t.Errorf("front matter lost %q", want)
		}
	}
	// And a non-ok outcome must be visible in the body, not only the header a
	// reader skips.
	if !strings.Contains(md, "part of the page") {
		t.Error("a partial read is not announced above the content")
	}
}

// TestOKArtifactHasNoBanner: the banner is only worth anything if it is rare.
func TestOKArtifactHasNoBanner(t *testing.T) {
	g := contractGraph()
	g.Outcome = graph.Outcome{Status: graph.StatusOK, HTTPStatus: 200}
	md := emit.Markdown(g, emit.DefaultMarkdownOptions())
	if strings.Contains(md, "This is not the page") {
		t.Error("an ok artifact carries a failure banner")
	}
	if !strings.Contains(md, "outcome: ok") {
		t.Error("an ok artifact does not record its outcome in the front matter")
	}
}
