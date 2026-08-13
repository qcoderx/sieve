package graph

import "testing"

// TestSectionIDsSurviveTheDocumentChanging is the regression guard for ids that
// were only stable while nothing above them moved.
//
// The ids are handed to agents over MCP and used to fetch content by name, so
// an agent may read a manifest, do something else, and come back for a section
// later -- possibly against a freshly distilled artifact. Positional ids made
// that silently wrong: two distillations of pear.no minutes apart produced the
// same twenty-one sections with seventeen ids pointing at different content.
func TestSectionIDsSurviveTheDocumentChanging(t *testing.T) {
	build := func(headings ...string) map[string]string {
		var blocks []Block
		n := 0
		add := func(typ BlockType, level int, text string) {
			blocks = append(blocks, Block{
				ID: blockID(n), Type: typ, Level: level, Text: text, Source: SourceDOM,
			})
			n++
		}
		for _, h := range headings {
			add(TypeHeading, 1, h)
			add(TypeParagraph, 0, "Body text belonging to "+h+".")
		}
		byID := map[string]string{}
		for _, s := range makeSections(blocks) {
			byID[s.ID] = s.Title
		}
		return byID
	}

	// The same three sections, once on their own and once after a section has
	// been inserted above them -- which is what a page growing, or a sweep
	// reaching further, does to every id below the change.
	before := build("Materials", "Shipping", "Returns")
	after := build("About us", "Materials", "Shipping", "Returns")

	for id, title := range before {
		got, ok := after[id]
		if !ok {
			t.Errorf("section %q (%s) lost its id when a section was added above it",
				title, id)
			continue
		}
		if got != title {
			t.Errorf("id %s meant %q and now means %q; an agent fetching it a "+
				"second time is silently given a different part of the page",
				id, title, got)
		}
	}

	// A heading that genuinely differs must not collide with one that does not.
	if len(build("Materials")) != 1 {
		t.Fatal("fixture built the wrong number of sections")
	}
	one := build("Materials")
	two := build("Materials Used")
	for id := range one {
		if _, clash := two[id]; clash {
			t.Errorf("distinct headings produced the same id %s", id)
		}
	}
}

// TestRepeatedHeadingsAreDistinguished: pages really do use one heading twice,
// and both sections still need their own name.
func TestRepeatedHeadingsAreDistinguished(t *testing.T) {
	var blocks []Block
	n := 0
	for i := 0; i < 3; i++ {
		blocks = append(blocks,
			Block{ID: blockID(n), Type: TypeHeading, Level: 2, Text: "Full disclosure", Source: SourceDOM},
			Block{ID: blockID(n + 1), Type: TypeParagraph, Text: "Paragraph.", Source: SourceDOM})
		n += 2
	}
	secs := makeSections(blocks)
	if len(secs) != 3 {
		t.Fatalf("built %d sections, want 3", len(secs))
	}
	seen := map[string]bool{}
	for _, s := range secs {
		if seen[s.ID] {
			t.Errorf("duplicate id %s: two sections cannot be addressed separately", s.ID)
		}
		seen[s.ID] = true
	}
}
