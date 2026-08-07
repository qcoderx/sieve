package render_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/qcoderx/sieve/internal/capture"
	"github.com/qcoderx/sieve/internal/render"
)

// serveFixtures serves testdata/pages over HTTP. Loading fixtures from disk via
// file:// would work, but the sweep needs to exercise the same network path a
// real distillation takes, redirects and status codes included.
func serveFixtures(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.FileServer(http.Dir("../../testdata/pages")))
	t.Cleanup(srv.Close)
	return srv
}

func newBrowser(t *testing.T) *render.Browser {
	t.Helper()
	if render.ChromiumPath("") == "" {
		t.Skip("no Chromium available")
	}
	opts := render.DefaultOptions()
	opts.Budget = 90 * time.Second
	if os.Getenv("SIEVE_TEST_LOG") != "" {
		opts.Logf = t.Logf
	}
	b, err := render.Launch(context.Background(), opts)
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	t.Cleanup(b.Close)
	return b
}

func TestSweepImmersiveFixture(t *testing.T) {
	if os.Getenv("SIEVE_SKIP_BROWSER") != "" {
		t.Skip("SIEVE_SKIP_BROWSER set")
	}
	srv := serveFixtures(t)
	b := newBrowser(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	res, err := b.Sweep(ctx, srv.URL+"/immersive/", nil)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	m := res.Merged
	t.Logf("checkpoints=%d nodes=%d actions=%d media=%d canvases=%d timing=%+v",
		m.Checkpoints, len(m.Nodes), len(m.Actions), len(m.Media), len(m.Canvases), res.Timing)
	t.Logf("new per checkpoint: %v", m.NewPerCheckpoint)
	for _, n := range res.Notes {
		t.Logf("note: %s", n)
	}

	if m.Meta.Title != "Aurelia Atelier — Hand-finished furniture" {
		t.Errorf("title = %q", m.Meta.Title)
	}
	if m.Checkpoints < 3 {
		t.Errorf("expected the sweep to take several checkpoints, got %d", m.Checkpoints)
	}

	all := allText(m)

	// Content that only exists after a scroll-triggered reveal.
	for _, want := range []string{
		"founded in 1974",
		"nine weeks",
		"Portuguese cork",
		"medullary ray fleck",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("scroll-revealed content missing: %q", want)
		}
	}

	// Content that only exists inside a shadow root.
	if !strings.Contains(all, "quietly confident furniture") {
		t.Error("shadow DOM content missing")
	}

	// The display:none duplicate navigation must not have been captured. It is
	// only detectable by counting: the visible nav has the same link text.
	navCount := 0
	for _, n := range m.Nodes {
		if n.Text == "Materials" && n.Tag == "a" {
			navCount++
		}
	}
	if navCount != 1 {
		t.Errorf("expected exactly one 'Materials' link node, got %d (display:none subtree leaked?)", navCount)
	}

	// A pinned navigation must be flagged, not scattered down the document.
	fixed := 0
	for _, n := range m.Nodes {
		if n.Fixed {
			fixed++
		}
	}
	if fixed == 0 {
		t.Error("pinned navigation was not flagged as fixed")
	}

	// Dedup: every node must be unique by path+text, and the paragraphs that
	// were visible for many checkpoints must appear exactly once.
	seen := map[string]int{}
	for _, n := range m.Nodes {
		seen[n.Path+"\x00"+n.Text]++
	}
	for k, v := range seen {
		if v > 1 {
			t.Errorf("duplicate node (%d copies): %q", v, k)
		}
	}

	// Actions.
	var form *string
	hasSend := false
	for i := range m.Actions {
		a := &m.Actions[i]
		if a.Kind == "form" {
			form = &a.Href
			if len(a.Fields) == 0 {
				t.Error("form captured with no fields")
			}
			names := map[string]bool{}
			for _, f := range a.Fields {
				names[f.Name] = true
				if f.Name == "email" && !f.Required {
					t.Error("email field should be required")
				}
			}
			if names["csrf"] {
				t.Error("hidden field leaked into form schema")
			}
			for _, want := range []string{"email", "piece", "notes"} {
				if !names[want] {
					t.Errorf("form field %q missing", want)
				}
			}
		}
		if a.Kind == "button" && strings.Contains(a.Label, "Send enquiry") {
			hasSend = true
		}
	}
	if form == nil {
		t.Error("contact form not captured")
	} else if !strings.HasSuffix(*form, "/enquiry") {
		t.Errorf("form action = %q", *form)
	}
	if !hasSend {
		t.Error("submit button not captured")
	}

	// Media, including the figcaption a plain img scrape would miss.
	foundImg := false
	for _, md := range m.Media {
		if strings.Contains(md.Src, "grain.png") {
			foundImg = true
			if md.Alt == "" {
				t.Error("image alt not captured")
			}
			if !strings.Contains(md.Caption, "medullary") {
				t.Errorf("figcaption not captured: %q", md.Caption)
			}
		}
	}
	if !foundImg {
		t.Error("image not captured")
	}

	// Canvas, with a screenshot taken because it fills the hero.
	if len(m.Canvases) == 0 {
		t.Error("canvas not captured")
	} else if m.Canvases[0].Context != "2d" {
		t.Errorf("canvas context = %q, want 2d (bootstrap hook not installed?)", m.Canvases[0].Context)
	}
	if len(res.CanvasShots) == 0 {
		t.Error("no canvas screenshot taken despite a full-bleed canvas")
	}
	for _, s := range res.CanvasShots {
		if s.Uniform {
			t.Error("painted canvas reported as uniform")
		}
		if len(s.PNG) == 0 {
			t.Error("empty canvas PNG")
		}
	}
}

func allText(m *capture.Merged) string {
	var sb strings.Builder
	for _, n := range m.Nodes {
		sb.WriteString(n.Text)
		sb.WriteByte('\n')
	}
	return sb.String()
}
