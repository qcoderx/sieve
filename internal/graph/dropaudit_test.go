package graph

import (
	"strings"
	"testing"
)

// TestEveryExclusionIsRecorded is the test that would have caught three content
// losses before a question set did.
//
// The prune has four rules. For a long time only one of them wrote anything to
// the audit, so the other three removed text and the artifact's own account of
// itself said nothing had happened. On one Stripe documentation page that was
// 117 runs and 1,836 characters reported as no exclusion at all.
//
// The consequences were not theoretical. Every term in curl's libcurl
// reference, the only contact address on basement.studio, and the option names
// on any API page shaped like them were all the same rule firing, and all three
// were invisible. They were found by writing question sets and noticing the
// answers were missing -- a filter that catches roughly what somebody happens
// to ask about.
//
// An artifact that says what it withheld can be checked by anyone. One that
// withholds silently has to be caught by a person asking the right question.
func TestEveryExclusionIsRecorded(t *testing.T) {
	actions := []Action{{Type: "link", Label: "Pricing", Href: "/pricing"}}

	blocks := []Block{
		{ID: "b0", Type: TypeParagraph, Text: "A paragraph with real words in it that nothing should touch."},
		// Non-lexical.
		{ID: "b1", Type: TypeParagraph, Text: "—— ·· ——"},
		// A single character.
		{ID: "b2", Type: TypeParagraph, Text: "t"},
		// A short linked run matching a collected link label.
		{ID: "b3", Type: TypeParagraph, Text: "Pricing", Href: "/pricing"},
		// A short line repeated often enough to be template furniture. The
		// first is kept and the rest are recorded.
		{ID: "b4", Type: TypeParagraph, Text: "hide caption"},
		{ID: "b5", Type: TypeParagraph, Text: "hide caption"},
		{ID: "b6", Type: TypeParagraph, Text: "hide caption"},
	}

	stats := map[string]*DropCount{}
	out := pruneNonContent(blocks, actions, stats)

	// The paragraph, and the first "hide caption". Everything else is furniture
	// of one kind or another, including "Pricing", which is a menu item here
	// because a link with that label exists elsewhere on the page.
	if len(out) != 2 {
		var got []string
		for _, b := range out {
			got = append(got, b.Text)
		}
		t.Fatalf("kept %d blocks, want 2 (the paragraph and one 'hide caption'): %q",
			len(out), got)
	}

	for _, want := range []string{
		DropNonLexical, DropSingleChar, DropNavLabel, DropTemplateLabel,
	} {
		d, ok := stats[want]
		if !ok {
			t.Errorf("nothing recorded for %q.\nThe rule still removes the text; it just "+
				"no longer says so, which is the exact condition that hid three content "+
				"losses until somebody wrote a question about them.", want)
			continue
		}
		if d.Runs == 0 || d.Chars == 0 {
			t.Errorf("%q recorded %d run(s) and %d char(s); both should be non-zero",
				want, d.Runs, d.Chars)
		}
	}
}

// TestPunctuationPageStillReportsOneReason protects a message that depends on
// the order of the prune's tests rather than on anything it says out loud.
//
// The CLI has a carefully written diagnosis for a page whose entire rendered
// output was punctuation -- a loading bar in front of a WebGL scene -- and it
// fires only when the audit holds exactly one reason. Recording the other three
// exclusions could have broken it by adding a second, which would have replaced
// a specific, actionable explanation with a generic one.
//
// It does not, because non-lexical runs are tested first and a run of
// punctuation never reaches the single-character or nav-label tests. That is
// load-bearing and invisible, so it is asserted here.
func TestPunctuationPageStillReportsOneReason(t *testing.T) {
	blocks := []Block{
		{ID: "b0", Type: TypeParagraph, Text: "++==------"},
		{ID: "b1", Type: TypeParagraph, Text: "=++==-----"},
		{ID: "b2", Type: TypeParagraph, Text: "-"},
		{ID: "b3", Type: TypeParagraph, Text: "·"},
	}

	stats := map[string]*DropCount{}
	if out := pruneNonContent(blocks, nil, stats); len(out) != 0 {
		t.Fatalf("kept %d blocks, want none: a page of punctuation has no content", len(out))
	}
	if len(stats) != 1 {
		var got []string
		for r := range stats {
			got = append(got, r)
		}
		t.Fatalf("audit holds %d reasons, want exactly 1.\nThe CLI's canvas diagnosis "+
			"is conditional on there being one, and with two it falls back to a generic "+
			"message about the largest.\ngot: %q", len(stats), got)
	}
	if _, ok := stats[DropNonLexical]; !ok {
		t.Errorf("the one reason should be %q", DropNonLexical)
	}
	if !strings.Contains(DropNonLexical, "letter or digit") {
		t.Errorf("the CLI matches this constant by identity, not text, but the text is "+
			"shown to users: %q", DropNonLexical)
	}
}
