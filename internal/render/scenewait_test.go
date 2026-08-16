package render

import (
	"testing"
	"time"
)

// TestSceneWalkPatienceHoldsTogether guards four constants whose relationships
// are not visible from any one of them.
//
// The scene walk read once, and that was wrong in two different ways, both
// silent. On roughly one run in four igloo.inc had not registered its scene
// when the walk arrived, and the artifact reported an empty site. On another
// run the scene existed but was still filling, and the walk took 23 of its 29
// text objects six milliseconds in and reported them as the whole page.
//
// The partial read is the worse failure and the reason a simple "wait until
// non-empty" fix was not enough. An empty artifact is obviously wrong and gets
// investigated. An artifact missing a fifth of a site reads exactly like a
// complete one, and the only thing that would have caught it is a question set
// asking about the missing fifth.
//
// What has to hold:
//
//   - the context given to the walk must outlast the walk's own budget, or the
//     deadline check ends it early and the patience is decorative;
//   - waiting for a scene to appear must be shorter than waiting for one to
//     settle, because the first is spent on pages that turn out to have no
//     scene at all and the second only on pages that certainly do;
//   - a beat fine enough that several readings fit inside the budget, since
//     agreement across readings is the whole completion signal;
//   - at least two readings must agree, because one reading is what produced
//     the partial read.
func TestSceneWalkPatienceHoldsTogether(t *testing.T) {
	if sceneFloor <= sceneSettleWait {
		t.Errorf("sceneFloor is %v and sceneSettleWait is %v.\n"+
			"The context handed to the walk must outlast the budget the walk spends, "+
			"or the deadline check ends the loop early and the extra patience buys "+
			"nothing.", sceneFloor, sceneSettleWait)
	}

	if sceneAnnounceWait >= sceneSettleWait {
		t.Errorf("sceneAnnounceWait is %v against a settle budget of %v.\n"+
			"Waiting for a scene to appear is spent on pages that may have none; "+
			"waiting for one to settle is spent only on pages that certainly do. "+
			"The speculative wait should be the shorter of the two.",
			sceneAnnounceWait, sceneSettleWait)
	}

	// Measured, not chosen: three quarters of a second was the budget that let
	// igloo.inc fail outright on two runs in five.
	const observedTooShort = 750 * time.Millisecond
	if sceneSettleWait <= observedTooShort {
		t.Errorf("sceneSettleWait is %v, at or below the %v measured failing on "+
			"igloo.inc two runs in five", sceneSettleWait, observedTooShort)
	}

	if sceneStableReads < 2 {
		t.Errorf("sceneStableReads is %d.\nOne reading is exactly what produced the "+
			"partial read: the scene was six milliseconds old and still filling, and "+
			"the first answer was taken as the last.", sceneStableReads)
	}

	// Enough beats inside the budget that "the count stopped moving" is a real
	// observation rather than two samples of the same instant.
	if beats := int(sceneSettleWait / sceneRetryWait); beats < 4*sceneStableReads {
		t.Errorf("a %v budget at %v per beat is %d readings, too few to establish "+
			"that a scene stopped growing when %d must agree",
			sceneSettleWait, sceneRetryWait, beats, sceneStableReads)
	}
}
