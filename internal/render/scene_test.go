package render_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestSceneTextIsRead covers a page whose words exist only inside a 3D scene.
//
// igloo.inc serves an empty <body>, draws every paragraph of its site as MSDF
// glyph geometry, and never attaches a canvas element to the document. sieve
// reported it as a page with no words on it -- and, worse, said so
// confidently, because every DOM-shaped question it knew how to ask returned
// nothing and nothing contradicted that.
//
// Two things had to be true to fix it. bootstrap.js must install
// __THREE_DEVTOOLS__ before any page script runs, because a scene built inside
// a bundled ES module puts nothing on window and cannot otherwise be found;
// and the walk must read the string the site handed its text renderer, which
// every implementation stores somewhere slightly different.
func TestSceneTextIsRead(t *testing.T) {
	if os.Getenv("SIEVE_SKIP_BROWSER") != "" {
		t.Skip("SIEVE_SKIP_BROWSER set")
	}
	srv := serveFixtures(t)
	b := newBrowser(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	res, err := b.Sweep(ctx, srv.URL+"/webglscene/", nil)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Scene == nil {
		t.Fatal("no scene found: the devtools hook is not installed before page script, " +
			"so a scene built inside a module is invisible")
	}
	if len(res.Scene.Runs) == 0 {
		t.Fatalf("scene found (%d names) but no text runs read from it", len(res.Scene.Names))
	}

	var all strings.Builder
	for _, r := range res.Scene.Runs {
		all.WriteString(r.Text)
		all.WriteByte('\n')
	}
	got := all.String()

	// The words themselves, including a full paragraph: a page like this is
	// worth nothing if only the headings come back.
	for _, want := range []string{
		"Summary",
		"Kestrel Works builds measurement instruments",
		"unusable in a glove",
		"Manifesto",
		"survive a decade outdoors",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("scene text is missing %q; got:\n%s", want, got)
		}
	}
}

// TestSceneWalkIsCheapWithoutAScene guards the cost of asking.
//
// The walk runs on every rendered page now, because a page can hold a whole
// scene without ever attaching a canvas -- the condition it used to be gated
// on. That is only defensible if a page with no scene pays nothing: the hook
// is empty, the walk returns immediately, and the global scan that used to be
// the fallback is not attempted unless a canvas is there to justify it.
func TestSceneWalkIsCheapWithoutAScene(t *testing.T) {
	if os.Getenv("SIEVE_SKIP_BROWSER") != "" {
		t.Skip("SIEVE_SKIP_BROWSER set")
	}
	srv := serveFixtures(t)
	b := newBrowser(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	res, err := b.Sweep(ctx, srv.URL+"/gates/plain-page/", nil)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Scene != nil {
		t.Errorf("an ordinary page reported a 3D scene: %d names, %d runs",
			len(res.Scene.Names), len(res.Scene.Runs))
	}
}
