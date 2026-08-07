package render_test

import (
	"context"
	"testing"
	"time"
)

// TestFrameProduction is a regression guard for the least obvious failure mode
// in the whole renderer.
//
// A headless Chromium tab that is not being composited never runs the rendering
// steps. requestAnimationFrame stops firing, and so does IntersectionObserver,
// which is what nearly every scroll-reveal animation on a modern site uses to
// decide when to show content. Under that condition a sweep completes quickly,
// reports no errors, and produces an artifact containing the hero and nothing
// else -- the exact failure this project exists to prevent, arriving silently.
//
// Two things caused it: secondary tabs are not activated by default, and
// --in-process-gpu combined with SwiftShader stops frame production for every
// tab after the first. Both are fixed in Launch and Sweep. This test fails if
// either regresses, by checking that content which only exists after a
// scroll-triggered reveal is present.
func TestFrameProduction(t *testing.T) {
	srv := serveFixtures(t)
	b := newBrowser(t)

	// Sweep twice on the same browser. The second sweep runs in a tab that is
	// not the browser's first, which is where frame starvation shows up.
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		res, err := b.Sweep(ctx, srv.URL+"/immersive/", nil)
		cancel()
		if err != nil {
			t.Fatalf("sweep %d: %v", i, err)
		}
		if res.Merged.Checkpoints < 3 {
			t.Errorf("sweep %d: only %d checkpoints; the sweep did not descend the page",
				i, res.Merged.Checkpoints)
		}
		found := false
		for _, n := range res.Merged.Nodes {
			// This paragraph starts at opacity 0 and is only revealed by an
			// IntersectionObserver callback, which requires frames.
			if n.Text != "" && n.MaxOpacity > 0.9 &&
				containsFold(n.Text, "nine weeks") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("sweep %d: scroll-revealed content never reached full opacity; "+
				"frame production is starved", i)
		}
	}
}

func containsFold(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexFold(haystack, needle) >= 0
}

func indexFold(s, sub string) int {
	n := len(sub)
	if n == 0 {
		return 0
	}
	for i := 0; i+n <= len(s); i++ {
		ok := true
		for j := 0; j < n; j++ {
			a, b := s[i+j], sub[j]
			if 'A' <= a && a <= 'Z' {
				a += 32
			}
			if 'A' <= b && b <= 'Z' {
				b += 32
			}
			if a != b {
				ok = false
				break
			}
		}
		if ok {
			return i
		}
	}
	return -1
}
