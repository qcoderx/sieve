package render_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// marker appears only in the content behind each gate. Its presence in the
// captured text is the whole assertion: the door opened, or it did not.
const marker = "THE STUDIO IS OPEN"

// TestEntryGates walks the archetypes of the "click something before you may
// read this" pattern.
//
// Each one is a real shape taken from a site that sieve got wrong. They are
// fixtures rather than live URLs because the interesting cases are timing
// dependent -- a loader that goes still before it opens, a press that takes a
// second to have an effect -- and a test that has to reproduce those over the
// network is a test nobody trusts.
//
// The refusals matter as much as the entrances. Half of what looks like a
// front door is a question addressed to the visitor -- their age, their
// consent, their account -- and sieve has no standing to answer one. Those
// cases assert that the content stayed shut and that the artifact says so.
func TestEntryGates(t *testing.T) {
	if os.Getenv("SIEVE_SKIP_BROWSER") != "" {
		t.Skip("SIEVE_SKIP_BROWSER set")
	}

	cases := []struct {
		dir string
		// open is whether the content behind the gate should be reached.
		open bool
		// press is whether sieve should have pressed anything at all. A page
		// that is not gated must come back untouched.
		press bool
		why   string
	}{
		{"split-letters", true, true,
			"the label is one span per letter, which is how every animated entry control is built"},
		{"overlay-over-content", true, true,
			"the page is fully rendered underneath an opaque splash, so its character count says 'not gated' about a page nothing is visible on"},
		{"press-to-enter", true, true,
			"the words are 'PRESS TO ENTER' and the listener is on the window, so there is nothing to aim at but the middle"},
		{"arrow-prefix", true, true,
			"the label is ornamented, '→ Enter site', and a pattern anchored to the first character misses it"},
		{"loader-then-gate", true, true,
			"the loader reaches a hundred per cent and then goes still for a while before the door appears"},
		{"div-listener", true, true,
			"a bare div with a listener attached in script: no button, no role, no href"},
		{"sound-gate", true, true,
			"a site built around audio must ask before it may play any, and the browser here is muted anyway"},
		{"trusted-only", true, true,
			"opens for a real input event and ignores element.click()"},
		{"key-to-enter", true, true,
			"listens for a key and ignores the mouse entirely, and says so in words"},

		{"age-gate", false, false,
			"asks the visitor to state their age; sieve does not answer for a visitor"},
		{"signin-wall", false, false,
			"asks for an account that belongs to nobody"},

		{"cookie-banner", true, false,
			"a strip at the bottom of a readable page is not a door, and nothing should be pressed"},
		{"plain-page", true, false,
			"an ordinary page that happens to contain the word 'Explore'"},
	}

	srv := serveFixtures(t)
	b := newBrowser(t)

	for _, tc := range cases {
		t.Run(tc.dir, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			res, err := b.Sweep(ctx, srv.URL+"/gates/"+tc.dir+"/", nil)
			if err != nil {
				t.Fatalf("sweep: %v", err)
			}
			got := strings.Contains(strings.ToUpper(allText(res.Merged)), marker)

			switch {
			case tc.open && !got:
				t.Errorf("content behind the gate was never reached (%s)\n"+
					"pressed: %q\ndeclared gate: %q", tc.why, res.EnteredGate, res.EntryGate)
			case !tc.open && got:
				t.Errorf("content was reached through a gate sieve must not open (%s)\n"+
					"pressed: %q", tc.why, res.EnteredGate)
			}

			if tc.press && res.EnteredGate == "" {
				t.Errorf("nothing was pressed, and the artifact does not record an entrance (%s)", tc.why)
			}
			if !tc.press && res.EnteredGate != "" {
				t.Errorf("sieve pressed %q on a page it had no business touching (%s)",
					res.EnteredGate, tc.why)
			}
			// A gate that stays shut has to be named, or the artifact is
			// indistinguishable from one describing a site with nothing on it.
			if !tc.open && res.EntryGate == "" {
				t.Errorf("the gate was refused but never declared; a reader cannot tell "+
					"this page from an empty one (%s)", tc.why)
			}
		})
	}
}
