package distill_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/qcoderx/sieve/internal/distill"
	"github.com/qcoderx/sieve/internal/render"
	"github.com/qcoderx/sieve/internal/safety"
)

// TestHydrationOnlyRecoversAPageThatSaidNothing is the safety property of the
// hydration channel, and it is the one worth having a test for.
//
// Reading a framework's state payload is a recovery path. A recovery path that
// also runs when nothing needs recovering is not a recovery path: it is a
// second source of text with no position, no ordering and no way for a reader
// to tell which half of the artifact came from where. It would also put strings
// into an artifact that no visitor ever saw, on pages where sieve otherwise
// only reports what rendered.
//
// So both directions are asserted. The shell must gain its content, and the
// page that rendered must not gain a sentence that exists only in its payload.
func TestHydrationOnlyRecoversAPageThatSaidNothing(t *testing.T) {
	if os.Getenv("SIEVE_SKIP_BROWSER") != "" {
		t.Skip("SIEVE_SKIP_BROWSER set")
	}
	if render.ChromiumPath("") == "" {
		t.Skip("no Chromium available")
	}

	srv := httptest.NewServer(http.FileServer(http.Dir("../../testdata/pages")))
	defer srv.Close()

	opts := distill.DefaultOptions()
	opts.Render.Budget = 60 * time.Second
	guardCfg := safety.DefaultGuardConfig()
	guardCfg.AllowPrivate = true
	opts.Guard = safety.NewGuard(guardCfg)

	d := distill.New(opts)
	defer d.Close()

	text := func(path string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		res, err := d.Distill(ctx, srv.URL+path)
		if err != nil {
			t.Fatalf("distill %s: %v", path, err)
		}
		var b strings.Builder
		for _, blk := range res.Graph.Blocks {
			b.WriteString(blk.Text)
			b.WriteByte('\n')
		}
		return b.String()
	}

	t.Run("a shell is recovered from its payload", func(t *testing.T) {
		got := text("/hydrated/")
		for _, want := range []string{
			"firing schedule for the coming year",
			"six-hour shifts through the night",
			"bisque firing must be finished",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("missing %q.\nThis page renders nothing at all, so its payload is "+
					"the only place its words exist. Without this the artifact is an empty "+
					"mount point.", want)
			}
		}
	})

	t.Run("a page that rendered keeps its payload out", func(t *testing.T) {
		got := text("/hydratedfull/")
		if !strings.Contains(got, "six-hour shifts through the night") {
			t.Fatalf("the page's own rendered text is missing; this test is not measuring "+
				"what it thinks it is")
		}
		if strings.Contains(got, "PAYLOAD_ONLY_NEVER_DISPLAYED") {
			t.Errorf("a sentence that exists only in the state payload reached the artifact "+
				"of a page that rendered.\nThe channel is meant to open only when there is "+
				"nothing to recover; opening it here puts text no visitor ever saw next to "+
				"text that was on screen, with nothing distinguishing them.")
		}
	})
}
