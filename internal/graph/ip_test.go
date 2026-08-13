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
	//
	// The anchors are single words rather than phrases, because where the lines
	// break depends on the font the machine happens to have and a phrase that
	// is contiguous under one family is split under another. What must hold on
	// every machine is the order of the words and the fact that nothing from a
	// different paragraph gets in between them.
	var joined []string
	for _, b := range g.Blocks {
		if !b.Region.IsChrome() {
			joined = append(joined, b.Text)
		}
	}
	all := strings.Join(joined, " ")

	for _, want := range []string{"Installation", "Appendix E", "trailing clause"} {
		if !strings.Contains(all, want) {
			t.Fatalf("%q is missing from the artifact entirely:\n%s", want, all)
		}
	}

	seeIdx := strings.Index(all, "See the")
	installIdx := strings.Index(all, "Installation")
	appendixIdx := strings.Index(all, "Appendix E")
	trailingIdx := strings.Index(all, "trailing clause")

	// Within the sentence: the link text sits where the author put it, between
	// the words on either side of it.
	if installIdx < seeIdx {
		t.Errorf("the link text was emitted before the words that introduce it")
	}
	if appendixIdx < installIdx {
		t.Errorf("the second link was emitted before the first")
	}
	// Across paragraphs: the next paragraph must not appear inside this one.
	// This is the shape of the original bug -- the end of the sentence was
	// filed as furniture and re-emitted after the paragraph that follows it.
	if trailingIdx < appendixIdx {
		t.Errorf("the following paragraph was emitted inside this one; "+
			"the sentence was split and its conclusion filed elsewhere:\n%s", all)
	}
}
