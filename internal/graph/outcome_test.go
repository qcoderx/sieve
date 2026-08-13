package graph

import "testing"

// TestOutcomePrecedence covers the statuses an agent acts on differently.
//
// The order matters as much as the detection. A page that is both refused and
// empty is refused: that is the fact which explains the other one, and the one
// the caller can do something about.
func TestOutcomePrecedence(t *testing.T) {
	cases := []struct {
		name   string
		in     OutcomeInput
		blocks int
		want   Status
	}{
		{"a plain page", OutcomeInput{HTTPStatus: 200, Rendered: true}, 12, StatusOK},

		{"an error status is a refusal", OutcomeInput{HTTPStatus: 403}, 0, StatusBlocked},
		{"a rate limit is a refusal", OutcomeInput{HTTPStatus: 429}, 0, StatusBlocked},
		{"401 is a login wall, not a generic refusal",
			OutcomeInput{HTTPStatus: 401}, 0, StatusAuthRequired},
		{"a proxy demand is a login wall too",
			OutcomeInput{HTTPStatus: 407}, 0, StatusAuthRequired},

		{"robots outranks everything: it is a decision not to read",
			OutcomeInput{HTTPStatus: 200, RobotsRefused: true}, 40, StatusBlocked},

		{"a challenge is not a refusal by policy",
			OutcomeInput{HTTPStatus: 200, Blocked: true, BlockedReason: "Cloudflare challenge"},
			0, StatusChallenge},
		{"an unpassed entry screen means the artifact is the screen",
			OutcomeInput{HTTPStatus: 200, Rendered: true, EntryGate: "CLICK TO ENTER"},
			6, StatusChallenge},

		{"a shell nobody rendered", OutcomeInput{HTTPStatus: 200, ShellHTML: true}, 0, StatusSPAShell},
		{"a shell that rendered to nothing",
			OutcomeInput{HTTPStatus: 200, ShellHTML: true, Rendered: true}, 0, StatusSPAShell},
		{"rendered, and genuinely empty",
			OutcomeInput{HTTPStatus: 200, Rendered: true}, 0, StatusEmptyAfterRender},

		{"a tier that fell back leaves the read partial",
			OutcomeInput{HTTPStatus: 200, TierFellBack: true, TierReason: "browser could not drive this page"},
			30, StatusPartial},
		// A sweep that stops before the bottom is ordinary on a long page and
		// must not, on its own, brand the artifact partial.
		{"a long page is not partial merely for being long",
			OutcomeInput{HTTPStatus: 200, Rendered: true, SweepTruncated: true}, 900, StatusOK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DecideOutcome(tc.in, tc.blocks)
			if got.Status != tc.want {
				t.Errorf("status = %q, want %q (evidence: %v)", got.Status, tc.want, got.Evidence)
			}
			if got.Status != StatusOK && len(got.Evidence) == 0 {
				t.Error("a non-ok verdict was returned with no evidence; a caller cannot check it")
			}
		})
	}
}

// TestErrorBodyIsCarried: on an error the body is where a proxy or policy
// filter says who blocked the request and why, which is the difference between
// an agent understanding a block and working around it.
func TestErrorBodyIsCarried(t *testing.T) {
	body := "Access denied. Your request was filtered by corporate policy WEB-31."
	o := DecideOutcome(OutcomeInput{HTTPStatus: 403, Body: body}, 0)
	if o.BodyExcerpt == "" {
		t.Fatal("the response body was discarded on an error")
	}
	if o.BodyExcerpt != body {
		t.Errorf("excerpt = %q, want the body verbatim", o.BodyExcerpt)
	}

	// A 200 carries no excerpt: the body is the artifact.
	if e := DecideOutcome(OutcomeInput{HTTPStatus: 200, Rendered: true, Body: body}, 3); e.BodyExcerpt != "" {
		t.Errorf("a successful read carried a body excerpt: %q", e.BodyExcerpt)
	}

	// A server answering an error with a whole HTML page cannot spend the
	// caller's context on it.
	long := make([]byte, 4000)
	for i := range long {
		long[i] = 'x'
	}
	if n := len(DecideOutcome(OutcomeInput{HTTPStatus: 500, Body: string(long)}, 0).BodyExcerpt); n > maxBodyExcerpt {
		t.Errorf("excerpt was %d chars, want at most %d", n, maxBodyExcerpt)
	}
}

// TestLateBlocksChangeTheOutcome is the regression guard for the status
// contradicting the artifact it describes.
//
// Canvas recovery and the 3D scene walk append blocks after Build returns.
// Deciding the outcome once inside Build read the count before they existed, so
// igloo.inc -- whose entire site arrives that way -- was labelled spa_shell,
// with the evidence "rendering it produced no text", on an artifact carrying
// twenty-nine paragraphs of that page.
func TestLateBlocksChangeTheOutcome(t *testing.T) {
	g := &Graph{}
	g.outcomeIn = OutcomeInput{HTTPStatus: 200, ShellHTML: true, Rendered: true}
	g.outcomeKnown = true
	g.Recount()
	if g.Outcome.Status != StatusSPAShell {
		t.Fatalf("an empty shell is %q, want %q", g.Outcome.Status, StatusSPAShell)
	}

	// The scene walk delivers the page.
	for i := 0; i < 29; i++ {
		g.Blocks = append(g.Blocks, Block{
			ID: blockID(i), Type: TypeParagraph, Text: "Recovered from the scene.", Source: SourceCanvasScene,
		})
	}
	g.Recount()
	if g.Outcome.Status != StatusOK {
		t.Errorf("with %d blocks the outcome is still %q (%v); the status now contradicts "+
			"the artifact it describes", len(g.Blocks), g.Outcome.Status, g.Outcome.Evidence)
	}
}

// TestLoadedGraphKeepsItsRecordedOutcome: a graph read back from disk has no
// decision inputs, and re-deciding from an empty one would turn a recorded
// refusal into an ok.
func TestLoadedGraphKeepsItsRecordedOutcome(t *testing.T) {
	g := &Graph{Outcome: Outcome{Status: StatusBlocked, HTTPStatus: 403,
		Evidence: []string{"the server answered HTTP 403"}}}
	g.Blocks = append(g.Blocks, Block{ID: "b_000", Type: TypeParagraph, Text: "Forbidden"})
	g.Recount()
	if g.Outcome.Status != StatusBlocked {
		t.Errorf("a loaded artifact's outcome became %q; a recorded refusal must survive "+
			"a recount", g.Outcome.Status)
	}
}
