package render_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestRestlessPageStillGetsSwept sweeps a page that never stops animating.
//
// sieve waits for stillness before each capture, which is right: capturing
// mid-animation reads text that is still fading in. But a page with a permanent
// animation can never answer that request, so every wait ends in a timeout, and
// with a fixed wait per stop a few stops consume the whole sweep. pear.no spent
// 3.9 seconds of a 5.6 second budget on a single settle and reached three
// checkpoints of a document needing far more.
//
// WHAT THIS TEST DOES NOT DO, stated plainly because it would otherwise be
// assumed: it is not a regression guard for the fix that followed. That fix
// bounds any one wait to a quarter of what remains. This test passes with the
// bound removed, and it passed with it removed after two rounds of hardening
// the fixture -- lengthening the document to forty sections, mutating layout on
// every frame so stillness is genuinely unreachable, tightening the budget, and
// asserting on checkpoint count rather than content. The synthetic page settles
// far faster than pear.no does, and I could not make it reproduce the
// pathology. The fix rests on measurements against real pages instead: pear.no
// went from three checkpoints to five and coverage 0.733 to 0.756 at an
// unchanged budget, igloo.inc from 23 blocks to 29, with stripe, github,
// news.ycombinator and organimo unchanged.
//
// What it does cover is the weaker property, which is still worth holding: a
// page that never holds still is swept rather than stalled on. It would catch
// the catastrophic version of this going wrong -- and that version is real, not
// hypothetical. The first attempt at the fix applied the bound unconditionally,
// which fires before a fade completes on a page that reveals content that way;
// the sweep then observes nothing and pear.no fell from 44 blocks to 6. Hence
// the everSawVisible condition: "never settles" and "never reveals" look
// identical from inside the sweep and need opposite responses.
func TestRestlessPageStillGetsSwept(t *testing.T) {
	if os.Getenv("SIEVE_SKIP_BROWSER") != "" {
		t.Skip("SIEVE_SKIP_BROWSER set")
	}
	srv := serveFixtures(t)
	b := newBrowser(t)

	// A budget in the region where the bug bit: enough for a handful of stops,
	// not enough to be generous about any one of them.
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	res, err := b.Sweep(ctx, srv.URL+"/restless/", nil)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}

	got := allText(res.Merged)

	// The number of stops is the measurement that matters. Content alone is a
	// weak proxy: a short document is swept fully whatever the settle costs,
	// which is why the first version of this test passed with the fix removed.
	if res.Timing.Checkpoints < 4 {
		t.Errorf("the sweep managed %d checkpoint(s) in its budget; a page that "+
			"never holds still is spending the whole sweep waiting for a stillness "+
			"that cannot arrive (settle timeouts: %d)",
			res.Timing.Checkpoints, res.Timing.SettleMiss)
	}

	// The first section is visible from the start, so seeing only that one
	// means the sweep never travelled -- the failure this guards against.
	if !strings.Contains(got, "kilns were fired in 1923") {
		t.Fatal("the first section was not captured at all; the sweep saw nothing")
	}

	deeper := []string{
		"glaze recipe uses feldspar",
		"workshop moved to Bergen",
		"thrown by hand",
		"studio closes each August",
	}
	var missed []string
	for _, want := range deeper {
		if !strings.Contains(got, want) {
			missed = append(missed, want)
		}
	}
	// Not all six: the point is that the sweep travels down a page that never
	// holds still, not that it always reaches the bottom of one.
	if len(missed) > 1 {
		t.Errorf("the sweep reached %d of %d sections below the fold; missing: %s\n"+
			"a permanently animating page is spending its whole budget waiting "+
			"for a stillness that never comes",
			len(deeper)-len(missed), len(deeper), strings.Join(missed, "; "))
	}
}
