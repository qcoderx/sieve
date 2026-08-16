package bench_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/qcoderx/sieve/internal/bench"
	"github.com/qcoderx/sieve/internal/distill"
	"github.com/qcoderx/sieve/internal/render"
	"github.com/qcoderx/sieve/internal/safety"
)

// The offline tier of the benchmark.
//
// Every other measurement in this project points at a live site, which means it
// is a measurement of the internet on a Tuesday. Sites get redesigned, put
// behind Cloudflare, and taken down; a reader six months from now cannot re-run
// the igloo.inc row and get the igloo.inc number, and neither can I. That is
// tolerable for the headline results, because the alternative is only measuring
// pages I wrote, and a tool that reads pages I wrote is not evidence of
// anything.
//
// This tier is the other half. The pages ship in this repository, the ground
// truth ships beside them, and both are mine to license, so the whole row is
// reproducible by anyone, forever, with no network and no third party's content.
// It is the only tier where the comparison is unimpeachable, and it is the tier
// that fails the build when a change quietly costs coverage.
//
// The floors below are not targets. They are the scores the current build gets,
// less a small margin, recorded so that a regression is a red build rather than
// something noticed a month later while writing a README table.
//
// What this tier does not measure is at least as important as what it does. Both
// of these pages carry content sieve must refuse -- eight injected instructions
// on one, four controls that speak on a visitor's behalf on the other -- and a
// coverage score has no way to say "and none of that appeared". Those are
// asserted by name, string by string, in the render and distill tests. Reading
// this file as the whole fixture result would be reading half of it.
func TestOfflineCorpus(t *testing.T) {
	if os.Getenv("SIEVE_SKIP_BROWSER") != "" {
		t.Skip("SIEVE_SKIP_BROWSER set")
	}
	if render.ChromiumPath("") == "" {
		t.Skip("no Chromium available")
	}

	cases := []struct {
		name string
		path string
		set  string
		// floor is the coverage below which the build is broken. Set just under
		// the score the current build achieves, so noise does not fail CI but a
		// real loss does.
		floor float64
	}{
		// Words drawn as glyph geometry in a three.js scene, with an empty
		// body. The tier that exists to read this page is the only tier that
		// can, so anything below a near-perfect score here means recovery
		// broke.
		{"immersive", "/immersive/", "aurelia.yaml", 0.82},
		// Real prose threaded through eight injection channels. The score
		// measures that the prose survived being cleaned; the tests measure
		// that the injections did not.
		{"adversarial", "/adversarial/", "northwind.yaml", 0.95},
		// Everything worth knowing is behind a control a reader would press.
		// A build that stops opening disclosures scores near zero here rather
		// than slightly worse everywhere.
		{"disclosure", "/disclosure/", "kilnworks.yaml", 0.95},
	}

	srv := httptest.NewServer(http.FileServer(http.Dir("../../testdata/pages")))
	defer srv.Close()

	opts := distill.DefaultOptions()
	opts.Render.Budget = 90 * time.Second
	// httptest binds to loopback, which the guard blocks by default and should:
	// the exception is for a server this test started, not a policy change.
	guardCfg := safety.DefaultGuardConfig()
	guardCfg.AllowPrivate = true
	opts.Guard = safety.NewGuard(guardCfg)

	// One Distiller across all three, which is how sieve is actually used and
	// not merely convenient: running the fixtures on separate Distillers is
	// what hid TestBrowserOutlivesThePageThatLaunchedIt's bug for as long as it
	// was hidden.
	d := distill.New(opts)
	defer d.Close()

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			set, err := bench.LoadSet(filepath.Join("..", "..", "testdata", "questions", tc.set))
			if err != nil {
				t.Fatalf("load %s: %v", tc.set, err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()

			res, err := d.Distill(ctx, srv.URL+tc.path)
			if err != nil {
				t.Fatalf("distill %s: %v", tc.path, err)
			}

			cov := bench.CheckCoverage(set, res.Graph)
			if cov.Coverage < tc.floor {
				var absent []string
				for _, f := range cov.Facts {
					if !f.Present {
						absent = append(absent, f.QuestionID+": "+f.Fact)
					}
				}
				t.Errorf("coverage %.3f is below the recorded floor of %.2f.\n"+
					"This page and its ground truth both ship in this repository, so "+
					"this is not the internet having changed under the test: something "+
					"in this build reads less of the page than the build that set the "+
					"floor did.\nnot found: %v",
					cov.Coverage, tc.floor, absent)
			}
			t.Logf("%s: coverage %.3f (%d/%d facts), tier %s score %.3f",
				tc.name, cov.Coverage, cov.Found, cov.Total, res.Decision.Tier, res.Decision.Score)
		})
	}
}
