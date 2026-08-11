package graph_test

import (
	"strings"
	"testing"
)

// TestInlineProseStaysInOrder covers the worst class of bug this tool can have:
// text that is present, plausible, and in the wrong place.
//
// A paragraph containing an inline link arrives at classification as several
// runs, and every rule there judges a run on its own -- how long it is, whether
// it links somewhere, where it sits. So the prose stayed in the reading order
// while the line carrying the link was taken for footer furniture and appended
// after the content. The sentence "...and see Appendix E for information on
// editions." was cut in half, with the conclusion emitted several paragraphs
// later, and nothing in the artifact said it had been moved.
//
// A reader cannot detect that. Missing text announces itself; reordered text
// reads perfectly and is wrong.
func TestInlineProseStaysInOrder(t *testing.T) {
	g := buildFixture(t, "inlineprose/")

	// The reading order must follow the page down. Every rule that files a run
	// as furniture takes it out of the flow entirely, so a fragment of body
	// prose landing out of sequence shows up here as a jump backwards.
	var prev float64 = -1
	for _, b := range g.Blocks {
		if b.Region.IsChrome() {
			continue
		}
		if y := b.BBox[1]; y+1 < prev {
			t.Errorf("block %q is at y=%.0f, after a block at y=%.0f: "+
				"body text was reordered", b.Text, y, prev)
		} else if y > prev {
			prev = y
		}
	}

	// The specific sentence that was being broken, in the specific way.
	var joined []string
	for _, b := range g.Blocks {
		if !b.Region.IsChrome() {
			joined = append(joined, b.Text)
		}
	}
	all := strings.Join(joined, " ")
	before := strings.Index(all, "See the Installation")
	after := strings.Index(all, "Appendix E for information")
	switch {
	case before < 0 || after < 0:
		t.Fatalf("fixture text missing from the artifact:\n%s", all)
	case after < before:
		t.Errorf("the end of the sentence was emitted before its beginning")
	case after-before > 200:
		t.Errorf("the two halves of one sentence are %d characters apart; "+
			"the paragraph was split and its conclusion filed elsewhere", after-before)
	}
}
