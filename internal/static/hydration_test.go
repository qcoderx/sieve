package static

import (
	"os"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

func parseFixture(t *testing.T, path string) *html.Node {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := html.Parse(strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

// TestHydrationTakesProseAndLeavesMachinery is the test that decides whether
// this channel is worth having at all.
//
// A framework's state payload is mostly not prose. It holds routes, asset URLs,
// build hashes, class names, component names, timestamps, feature flags and
// i18n keys, and an artifact that swallowed them would read like a config file
// and cost tokens to say nothing. The value of the channel is entirely in the
// filtering, so the filtering is what is asserted.
func TestHydrationTakesProseAndLeavesMachinery(t *testing.T) {
	doc := parseFixture(t, "../../testdata/pages/hydrated/index.html")
	got := HydrationText(doc)
	all := strings.Join(got, "\n")

	for _, want := range []string{
		"The firing schedule for the coming year",
		"Two firings a year, in March and October",
		"six-hour shifts through the night",
		"opening it early cracks the glaze",
		"bisque firing must be finished",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("prose missing from the harvest: %q\nThis is the page's own text, and "+
				"on a page whose render never completes it is the only text there is.", want)
		}
	}

	// Everything a payload carries that is not for reading.
	for _, unwanted := range []string{
		"9f2c1ab77e4d3f80c5a6b1e2d4f8a0c3", // build hash
		"KilnScheduleHero",                 // component name
		"_hero_1x2y3_11",                   // css module
		"2026-08-17T09:41:22",              // timestamp
		"cdn.example.org",                  // asset host
		"/en/preview/",                     // route
		"nav.home",                         // i18n key
		"enableAudio",                      // feature flag
	} {
		if strings.Contains(all, unwanted) {
			t.Errorf("machinery reached the harvest: %q\nA payload is mostly this, and an "+
				"artifact that carries it reads like a config file and spends tokens saying "+
				"nothing.", unwanted)
		}
	}
}

// TestHydrationPairsLabelsWithDestinations covers the one piece of structure
// this reads, and the reason it is read as a link rather than as a word.
//
// "Ethereum" on its own is indistinguishable from a component name. "Ethereum"
// immediately followed by https://ethereum.foundation/ is a link with a label,
// which is a thing a page shows and a reader clicks.
func TestHydrationPairsLabelsWithDestinations(t *testing.T) {
	doc := parseFixture(t, "../../testdata/pages/hydrated/index.html")
	links := HydrationLinks(doc)

	want := map[string]string{
		"Ethereum": "https://ethereum.foundation/",
		"Solana":   "https://solana.com/",
	}
	got := map[string]string{}
	for _, l := range links {
		got[l.Text] = l.Href
	}
	for label, href := range want {
		if got[label] != href {
			t.Errorf("link %q: got %q, want %q", label, got[label], href)
		}
	}

	// The asset URLs in the payload have no label in front of them and must
	// not acquire one from whatever happens to be adjacent.
	for _, l := range links {
		if strings.Contains(l.Href, "cdn.example.org") {
			t.Errorf("an asset URL became a link: %q -> %q", l.Text, l.Href)
		}
	}
}

// TestHydrationIgnoresUntypedAndUnknownContainers keeps the channel narrow.
//
// Only <script type="application/json"> with a known framework id is read.
// Widening this to script bodies in general would mean telling data from code
// by inspection, which is how a tracking payload or an inline configuration
// object ends up quoted in an artifact as though the page had said it.
func TestHydrationIgnoresUntypedAndUnknownContainers(t *testing.T) {
	cases := []struct{ name, doc string }{
		{"executable assignment", `<script>window.__NUXT__ = {"heading":"A sentence long enough to pass the filter"}</script>`},
		{"no type attribute", `<script id="__NUXT_DATA__">["A sentence long enough to pass the filter here"]</script>`},
		{"unknown id", `<script type="application/json" id="__SOMETHING_ELSE__">["A sentence long enough to pass the filter"]</script>`},
		{"no id", `<script type="application/json">["A sentence long enough to pass the filter here"]</script>`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, err := html.Parse(strings.NewReader("<!doctype html><body>" + tc.doc))
			if err != nil {
				t.Fatal(err)
			}
			if got := HydrationText(root); len(got) != 0 {
				t.Errorf("read %d run(s) from a container that is not a typed, named "+
					"hydration payload: %q", len(got), got)
			}
		})
	}
}
