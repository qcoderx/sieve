package render

import (
	"testing"
	"time"
)

// TestSceneWaitOutlastsARemoteScene guards a constant that was tuned against
// the wrong thing.
//
// The scene walk originally read once. A page whose words are glyph geometry
// looks perfectly still to the sweep's settle loop -- nothing in the document
// changes, ever -- so the loop declares the page settled and the walk can
// arrive before three.js has built a single text object. Losing that race was
// permanent, because there was no second look.
//
// The first fix retried three times at 250ms, which was enough for a fixture
// served from localhost and demonstrably not enough for the real thing:
// igloo.inc returned zero blocks on two runs in five with that budget, and
// igloo.inc is the case the entire project is named for. Six seconds took it to
// six runs out of six, with 40 of 40 ground-truth facts each time.
//
// Two relationships have to hold, and neither is obvious from reading either
// constant on its own:
//
//   - the walk's context must outlast the wait, or the deadline check inside
//     the loop cuts it short and the wait is decorative;
//   - the wait must be long enough for a page loading over a network rather
//     than from a local file server.
//
// If a future change shortens either one, the symptom is not a test failure
// anywhere near here. It is igloo.inc intermittently reporting an empty site.
func TestSceneWaitOutlastsARemoteScene(t *testing.T) {
	if sceneFloor <= sceneEmptyWait {
		t.Errorf("sceneFloor is %v and sceneEmptyWait is %v.\n"+
			"The context handed to the walk must outlast the wait the walk performs, "+
			"or the deadline check ends the loop early and the extra patience buys "+
			"nothing. Raise sceneFloor above sceneEmptyWait.",
			sceneFloor, sceneEmptyWait)
	}

	// Measured, not chosen: three quarters of a second was the budget that let
	// igloo.inc fail outright two runs in five.
	const observedTooShort = 750 * time.Millisecond
	if sceneEmptyWait <= observedTooShort {
		t.Errorf("sceneEmptyWait is %v, which is at or below the %v that was measured "+
			"failing on igloo.inc two runs in five", sceneEmptyWait, observedTooShort)
	}

	// A beat short enough that the wait is a poll rather than one long sleep,
	// so a scene that fills early is picked up promptly.
	if sceneRetryWait <= 0 || sceneRetryWait > sceneEmptyWait/8 {
		t.Errorf("sceneRetryWait is %v against a %v budget: too coarse to notice a "+
			"scene that filled early", sceneRetryWait, sceneEmptyWait)
	}
}
