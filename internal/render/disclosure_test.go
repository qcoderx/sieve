package render_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestDisclosuresOpenWhatIsFoldedAway covers the one interaction sieve performs
// beyond the front door, and the line it will not cross.
//
// A tab panel, an accordion body and a details element hold content the page is
// already carrying. Pressing them asserts nothing, submits nothing and asks the
// server for nothing; it is the same category as scrolling, and every visitor
// does it without thinking. Refusing to meant an artifact could report "there is
// a section behind a tab labelled Pricing" and say nothing about what was in it.
//
// The refusals are the feature. An age gate, a consent banner and a purchase
// button present as identical controls and each asks the visitor to say
// something on their own behalf, which no tool has standing to say for them. A
// link to another document is navigation rather than disclosure: following it
// means describing a different page. A submit button can change something on a
// server, whatever its label happens to read.
func TestDisclosuresOpenWhatIsFoldedAway(t *testing.T) {
	if os.Getenv("SIEVE_SKIP_BROWSER") != "" {
		t.Skip("SIEVE_SKIP_BROWSER set")
	}
	srv := serveFixtures(t)
	b := newBrowser(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	res, err := b.Sweep(ctx, srv.URL+"/disclosure/", nil)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	got := allText(res.Merged)

	// What a visitor would see after opening the page's own controls.
	for _, want := range []string{"REVEAL_DETAILS", "REVEAL_ACCORDION", "REVEAL_TAB"} {
		if !strings.Contains(got, want) {
			t.Errorf("%s was not revealed; a control the page offers was left shut, "+
				"so the artifact describes less of the page than a reader sees", want)
		}
	}

	// The line. Each of these is a control that looks exactly like the ones
	// above and is not one.
	for _, forbidden := range []string{"REFUSED_AGE", "REFUSED_CONSENT", "REFUSED_PURCHASE"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("%s was opened. That control asks the visitor to assert something "+
				"on their own behalf, which sieve has no standing to do for them", forbidden)
		}
	}

	// Navigation is not disclosure: the sweep must still be describing the page
	// it was given.
	if res.FinalURL != "" && !strings.Contains(res.FinalURL, "/disclosure/") {
		t.Errorf("ended up at %q; a press followed a link and the artifact now "+
			"describes a different document", res.FinalURL)
	}

	// And it should say what it opened, so a reader can tell content that was on
	// screen from content that had to be revealed.
	if len(res.OpenedDisclosures) == 0 {
		t.Error("nothing was recorded as opened, so the artifact cannot distinguish " +
			"revealed content from content that was already visible")
	}
	for _, label := range res.OpenedDisclosures {
		low := strings.ToLower(label)
		for _, bad := range []string{"over 18", "cookie", "cart"} {
			if strings.Contains(low, bad) {
				t.Errorf("recorded pressing %q, which is on the refusal list", label)
			}
		}
	}
}
