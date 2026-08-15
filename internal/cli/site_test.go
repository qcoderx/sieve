package cli

import (
	"net/url"
	"testing"

	"github.com/qcoderx/sieve/internal/graph"
)

func TestSameSiteLinks(t *testing.T) {
	root, _ := url.Parse("https://kubernetes.io")
	g := &graph.Graph{Links: []graph.Link{
		{Href: "https://kubernetes.io/", Region: graph.RegionNav},
		{Href: "https://kubernetes.io/blog/", Region: graph.RegionNav},
		{Href: "https://kubernetes.io/docs/concepts/secret/", Region: graph.RegionMain},
		{Href: "https://kubernetes.io/docs/home/#top", Region: graph.RegionMain},
		{Href: "https://kubernetes.io/search/?q=pods", Region: graph.RegionMain},
		{Href: "https://cncf.io/", Region: graph.RegionMain},
		{Href: "mailto:someone@kubernetes.io", Region: graph.RegionFooter},
	}}

	all := sameSiteLinks(g, root, "")
	if len(all) == 0 {
		t.Fatal("no links followed at all")
	}
	for _, u := range all {
		if got, _ := url.Parse(u); got.Host != "kubernetes.io" {
			t.Errorf("followed an off-site link: %s", u)
		}
		if got, _ := url.Parse(u); got.RawQuery != "" {
			t.Errorf("followed a query string, which is a filter and not a page: %s", u)
		}
	}

	// The filter is the part that decides whether "read the docs" works.
	docs := sameSiteLinks(g, root, "/docs")
	if len(docs) == 0 {
		t.Fatalf("--include /docs matched nothing, though the page carries /docs links.\n"+
			"unfiltered: %v", all)
	}
	for _, u := range docs {
		if got, _ := url.Parse(u); got.Path == "" || got.Path[:5] != "/docs" {
			t.Errorf("filter let through %s, which is not under /docs", u)
		}
	}
}

// TestIncludeIgnoresSlashes guards a bug that made --include silently match
// nothing on Windows.
//
// Git Bash rewrites any argument beginning with a slash into a filesystem path,
// so `--include /docs` reached sieve as "C:/Program Files/Git/docs" and matched
// no link on any site. Nothing failed and nothing was logged: the crawl simply
// read one page and stopped, which looks exactly like a site with no internal
// links. A flag that works in one shell and fails silently in another is a bug
// in the flag, not in the shell.
func TestIncludeIgnoresSlashes(t *testing.T) {
	root, _ := url.Parse("https://kubernetes.io")
	g := &graph.Graph{Links: []graph.Link{
		{Href: "https://kubernetes.io/docs/concepts/secret/", Region: graph.RegionMain},
		{Href: "https://kubernetes.io/blog/", Region: graph.RegionNav},
	}}

	for _, form := range []string{"docs", "/docs", "docs/", "/docs/", "DOCS"} {
		got := sameSiteLinks(g, root, form)
		if len(got) != 1 {
			t.Errorf("--include %q matched %d links, want 1; the same intent written "+
				"a different way must not change the result", form, len(got))
		}
	}
}

// TestCanonicalCollapsesTheSamePage: a site links to its own pages with and
// without a trailing slash and with fragments, and reading one page four times
// is the crawl spending its budget on nothing.
func TestCanonicalCollapsesTheSamePage(t *testing.T) {
	forms := []string{
		"https://example.com/docs/intro",
		"https://example.com/docs/intro/",
		"https://example.com/docs/intro#install",
	}
	seen := map[string]bool{}
	for _, f := range forms {
		u, err := url.Parse(f)
		if err != nil {
			t.Fatal(err)
		}
		u.Fragment = ""
		seen[canonical(u)] = true
	}
	if len(seen) != 1 {
		t.Errorf("three spellings of one page produced %d entries: %v", len(seen), seen)
	}
}
