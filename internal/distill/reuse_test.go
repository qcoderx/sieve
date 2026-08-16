package distill_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/qcoderx/sieve/internal/distill"
	"github.com/qcoderx/sieve/internal/escalate"
	"github.com/qcoderx/sieve/internal/render"
	"github.com/qcoderx/sieve/internal/safety"
)

// TestBrowserOutlivesThePageThatLaunchedIt covers a failure that every test
// written before it was structurally unable to see.
//
// A Distiller is meant to be reused: `sieve site` walks a documentation site
// through one of them, and the MCP server keeps one for the life of the session
// precisely so that the second request does not pay for another Chromium. The
// browser is therefore owned by the Distiller and ended by Close.
//
// It was being launched on the context of whichever page happened to need it
// first. That page's context is cancelled the moment its distillation returns,
// which took the browser down while d.browser stayed non-nil. Every later page
// opened a tab on a dead context, and the "the browser could not load this page"
// path did exactly what it should: it fell back to the served HTML and carried
// on. The artifact was smaller and said tier 0, which is a legitimate thing for
// an artifact to say, so nothing anywhere reported a problem.
//
// The shape of the bug is why it survived: one page in isolation always works,
// and every test distilled one page. It needs a second page on the same
// Distiller to appear at all.
func TestBrowserOutlivesThePageThatLaunchedIt(t *testing.T) {
	if os.Getenv("SIEVE_SKIP_BROWSER") != "" {
		t.Skip("SIEVE_SKIP_BROWSER set")
	}
	if render.ChromiumPath("") == "" {
		t.Skip("no Chromium available")
	}

	srv := httptest.NewServer(http.FileServer(http.Dir("../../testdata/pages")))
	defer srv.Close()

	opts := distill.DefaultOptions()
	opts.Render.Budget = 90 * time.Second
	guardCfg := safety.DefaultGuardConfig()
	guardCfg.AllowPrivate = true
	opts.Guard = safety.NewGuard(guardCfg)

	d := distill.New(opts)
	defer d.Close()

	// Each page gets its own context and cancels it, which is what a caller
	// walking a site does and what the MCP server does per request.
	var firstNodes int
	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		res, err := d.Distill(ctx, srv.URL+"/disclosure/")
		cancel()
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}

		if res.Decision.Tier.Rank() < escalate.TierRender.Rank() {
			t.Fatalf("run %d came back at tier %q, having scored %.3f.\n"+
				"The score chose a browser and the run did not use one, which means the "+
				"browser this Distiller is holding is unusable and the fallback to the "+
				"served HTML hid it. A Distiller that stops rendering after its first "+
				"page renders the first page of a site and none of the rest.",
				i, res.Decision.Tier, res.Decision.Score)
		}
		if i == 0 {
			firstNodes = res.Graph.Stats.ContentNodes
			continue
		}
		if res.Graph.Stats.ContentNodes < firstNodes {
			t.Errorf("run %d produced %d blocks where run 0 produced %d; "+
				"the same page through the same Distiller got quieter",
				i, res.Graph.Stats.ContentNodes, firstNodes)
		}
	}
}
