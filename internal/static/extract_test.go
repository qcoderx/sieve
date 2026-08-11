package static

import (
	"sort"
	"strings"
	"testing"

	"github.com/qcoderx/sieve/internal/capture"
)

const docPage = `<!doctype html>
<html lang="en">
<head>
  <title>Quarter-sawn oak — Northwind</title>
  <meta name="description" content="How we prepare oak.">
  <script type="application/ld+json">{"@type":"Article","headline":"Quarter-sawn oak"}</script>
</head>
<body>
  <nav class="site-nav"><a href="/">Home</a><a href="/notes">Notes</a></nav>
  <main>
    <h1>Quarter-sawn oak</h1>
    <p>Quarter-sawn boards are cut radially from the log. The medullary rays
       show as fleck across the face.</p>
    <h2>Drying</h2>
    <p>We air-dry for four years before anything is worked.</p>
    <ul><li>Air-dried, not kiln-dried</li><li>Stacked with stickers</li></ul>
    <img src="/grain.png" alt="Ray fleck on a quarter-sawn board">
    <form action="/enquiry" method="post">
      <input name="email" type="email" required>
      <input name="csrf" type="hidden" value="x">
      <button type="submit">Send</button>
    </form>
  </main>
  <div style="display:none">Hidden panel copy that was never rendered.</div>
  <details><summary>Delivery</summary><p>UK only.</p></details>
  <footer>Northwind, Leeds.</footer>
</body>
</html>`

func extract(t *testing.T, html string) *Result {
	t.Helper()
	r, err := Extract("https://example.com/oak", strings.NewReader(html), len(html))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	return r
}

func TestExtractsContentAndStructure(t *testing.T) {
	r := extract(t, docPage)

	all := allText(r)
	for _, want := range []string{
		"Quarter-sawn boards are cut radially",
		"air-dry for four years",
		"Air-dried, not kiln-dried",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing content: %q", want)
		}
	}

	if r.Merged.Meta.Title != "Quarter-sawn oak — Northwind" {
		t.Errorf("title = %q", r.Merged.Meta.Title)
	}
	if len(r.Merged.Meta.JSONLD) != 1 {
		t.Errorf("expected one JSON-LD blob, got %d", len(r.Merged.Meta.JSONLD))
	}
	if r.Signals.Headings != 2 {
		t.Errorf("headings = %d, want 2", r.Signals.Headings)
	}
}

// TestHiddenContentIsQuarantinedNotIngested is the important one.
//
// The static tier must have the same security posture as the rendered tier. If
// tier 0 silently ingested display:none text while tier 2 quarantined it, then
// the project's central claim would hold or not hold depending on which rung
// happened to answer -- which is exactly the divergence between tiers that
// makes a tiered tool untrustworthy.
func TestHiddenContentIsQuarantinedNotIngested(t *testing.T) {
	r := extract(t, docPage)

	if strings.Contains(allText(r), "Hidden panel copy") {
		t.Error("static extraction ingested display:none content into the content tier")
	}

	var latent []string
	for _, l := range r.Merged.Latent {
		latent = append(latent, l.Text)
	}
	joined := strings.Join(latent, " | ")
	if !strings.Contains(joined, "Hidden panel copy") {
		t.Errorf("hidden content was discarded rather than quarantined; latent tier holds: %v", latent)
	}
	// A closed <details> is the same case: real content behind one click.
	if !strings.Contains(joined, "UK only") {
		t.Errorf("a closed <details> was not captured into the latent tier; latent tier holds: %v", latent)
	}
}

func TestFormSchemaExcludesHiddenFields(t *testing.T) {
	r := extract(t, docPage)

	var form *struct {
		method string
		names  []string
	}
	for _, a := range r.Merged.Actions {
		if a.Kind != "form" {
			continue
		}
		f := struct {
			method string
			names  []string
		}{method: a.Method}
		for _, fl := range a.Fields {
			f.names = append(f.names, fl.Name)
		}
		form = &f
	}
	if form == nil {
		t.Fatal("no form captured")
	}
	if form.method != "POST" {
		t.Errorf("method = %q", form.method)
	}
	for _, n := range form.names {
		if n == "csrf" {
			t.Error("a hidden field leaked into the form schema; it is machinery, not something a visitor fills in")
		}
	}
	if len(form.names) != 1 || form.names[0] != "email" {
		t.Errorf("fields = %v, want [email]", form.names)
	}
}

func TestSignalsSeparateShellFromDocument(t *testing.T) {
	doc := extract(t, docPage)
	if doc.Signals.TextChars < 100 {
		t.Errorf("a real document should yield substantial text, got %d chars", doc.Signals.TextChars)
	}
	if doc.Signals.TextRatio <= 0 {
		t.Error("text ratio was not computed")
	}

	shell := extract(t, `<!doctype html><html><head><title>App</title>
	  <noscript>You need to enable JavaScript to run this app.</noscript></head>
	  <body><div id="root"></div><script src="/bundle.js"></script></body></html>`)
	if shell.Signals.TextChars > 60 {
		t.Errorf("an application shell should yield almost no text, got %d chars", shell.Signals.TextChars)
	}
	if !shell.Signals.NoScriptWarning {
		t.Error("the page said it requires JavaScript and that was not detected")
	}
}

func TestNavigationIsClassifiedAsChrome(t *testing.T) {
	r := extract(t, docPage)
	for _, n := range r.Merged.Nodes {
		if n.Text == "Home" && n.Landmark != "nav" {
			t.Errorf("a navigation link was not marked as nav, got landmark %q", n.Landmark)
		}
	}
}

func allText(r *Result) string {
	var sb strings.Builder
	for _, n := range r.Merged.Nodes {
		sb.WriteString(n.Text)
		sb.WriteByte('\n')
	}
	return sb.String()
}

// TestInlineElementsKeepTheirPlace covers a silent corruption of prose.
//
// The extractor gathered an element's direct text children into one string and
// emitted it before recursing, so a paragraph broken by an inline link had the
// hole welded shut and the link's own words emitted somewhere else entirely.
// "Several community <a>translations</a> are also available." came out as
// "Several community are also available." -- fluent, grammatical, and not what
// the page says. An agent quoting that is quoting something nobody wrote.
//
// The test reads the fragments back in laid-out order, which is what the
// reassembler does, and asks whether the sentence survived.
func TestInlineElementsKeepTheirPlace(t *testing.T) {
	const page = `<!doctype html><html><body>
<p>Several community <a href="/t">translations</a> are also available.</p>
<p>This version assumes <code>edition = "2024"</code> in the <em>Cargo.toml</em> file of all projects.</p>
</body></html>`
	res := extract(t, page)

	byBlock := map[string][]capture.Node{}
	var order []string
	for _, n := range res.Merged.Nodes {
		if _, seen := byBlock[n.Block]; !seen {
			order = append(order, n.Block)
		}
		byBlock[n.Block] = append(byBlock[n.Block], n)
	}

	var got []string
	for _, blk := range order {
		ns := byBlock[blk]
		sort.SliceStable(ns, func(i, j int) bool { return ns[i].BBox.X() < ns[j].BBox.X() })
		var parts []string
		for _, n := range ns {
			parts = append(parts, n.Text)
		}
		got = append(got, strings.Join(parts, " "))
	}

	want := []string{
		"Several community translations are also available.",
		`This version assumes edition = "2024" in the Cargo.toml file of all projects.`,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d paragraphs, want %d:\n%q", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("paragraph %d reassembled as:\n  %q\nwant:\n  %q", i, got[i], want[i])
		}
	}
}
