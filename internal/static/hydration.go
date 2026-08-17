package static

import (
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Hydration payloads: the server-rendered state a framework ships so the client
// can rebuild the page without asking for it again.
//
// This is a narrow channel and it is deliberately not "read the scripts". Only
// <script type="application/json"> is considered, which is a container the HTML
// specification says is data and not code, carrying one of a small set of ids a
// framework writes by name. Nothing here executes, nothing is inferred from
// JavaScript source, and a page that ships its state as executable assignment --
// window.__NUXT__ = {...} -- is not read, because telling data from code in a
// script body means parsing JavaScript and being wrong sometimes.
//
// The case for reading it at all is that this is the page's own text. The
// framework hydrates the visible document from exactly these strings; they are
// what a visitor ends up reading. hatom.com is the example that forced it: the
// site spends its whole loading budget on an intro sequence and its render
// never completes, so sieve returned an interstitial and nothing else, while
// every heading and paragraph of the site sat in a __NUXT_DATA__ block in the
// served bytes.
//
// It is used as a recovery channel and not a content channel. A page that
// renders is read from what rendered; this only runs when the artifact would
// otherwise be a shell. Without that gate a working page would carry its copy
// twice, once as rendered text and once as payload, and the payload copy has no
// position, no ordering and no styling to place it by.

// hydrationContainers are the script ids worth opening, each of which is a
// documented, typed JSON payload rather than a convention someone noticed.
var hydrationContainers = map[string]bool{
	"__NUXT_DATA__": true, // Nuxt 3
	"__NEXT_DATA__": true, // Next.js
}

const (
	// A string has to be at least this long to be prose rather than a label,
	// a key or an icon name.
	minHydrationRunes = 12
	// And no longer than this, which is well past a paragraph and stops a
	// minified blob or an inlined stylesheet arriving as one run.
	maxHydrationRunes = 600
	// Caps on the whole harvest, so a large payload cannot become the artifact.
	maxHydrationRuns  = 400
	maxHydrationChars = 40000
)

// HydrationLink is a labelled destination found in a hydration payload.
type HydrationLink struct {
	Text string
	Href string
}

// maxHydrationLinks caps the link harvest for the same reason the text harvest
// is capped: a payload holds every route a site knows about.
const maxHydrationLinks = 120

// HydrationText returns the prose-looking strings a framework shipped as
// server-rendered state, in document order, deduplicated.
func HydrationText(root *html.Node) []string {
	var out []string
	seen := map[string]bool{}
	chars := 0

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.DataAtom == atom.Script {
			if strings.EqualFold(strings.TrimSpace(attr(n, "type")), "application/json") &&
				hydrationContainers[strings.TrimSpace(attr(n, "id"))] {
				var body strings.Builder
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					if c.Type == html.TextNode {
						body.WriteString(c.Data)
					}
				}
				collectHydration(body.String(), &out, seen, &chars)
			}
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	return out
}

// collectHydration parses one payload and walks it for strings.
func collectHydration(body string, out *[]string, seen map[string]bool, chars *int) {
	body = strings.TrimSpace(body)
	if body == "" {
		return
	}
	var doc any
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		// A payload that will not parse is not a payload. Guessing at it with
		// a regexp is how config values and tracking ids end up in an artifact.
		return
	}

	var visit func(any, int)
	visit = func(v any, depth int) {
		if depth > 32 || len(*out) >= maxHydrationRuns || *chars >= maxHydrationChars {
			return
		}
		switch t := v.(type) {
		case string:
			s := strings.TrimSpace(t)
			if !looksLikeHydrationProse(s) {
				return
			}
			key := strings.ToLower(strings.Join(strings.Fields(s), " "))
			if seen[key] {
				return
			}
			seen[key] = true
			*out = append(*out, s)
			*chars += len(s)
		case []any:
			for _, e := range t {
				visit(e, depth+1)
			}
		case map[string]any:
			for _, e := range t {
				visit(e, depth+1)
			}
		}
	}
	visit(doc, 0)
}

// looksLikeHydrationProse keeps sentences and headings and rejects the rest of
// what a state payload contains: routes, ids, class names, hashes, timestamps,
// asset URLs, i18n keys and configuration flags.
//
// The test is deliberately conservative. A payload is mostly machinery, and the
// cost of admitting machinery is an artifact that reads like a config file;
// the cost of rejecting a real sentence is one missing line in a recovery path
// that only runs when the alternative was nothing at all.
func looksLikeHydrationProse(s string) bool {
	n := utf8.RuneCountInString(s)
	if n < minHydrationRunes || n > maxHydrationRunes {
		return false
	}
	// A URL, a path, a selector or a data URI.
	switch {
	case strings.HasPrefix(s, "http://"), strings.HasPrefix(s, "https://"),
		strings.HasPrefix(s, "//"), strings.HasPrefix(s, "/"),
		strings.HasPrefix(s, "#"), strings.HasPrefix(s, "."),
		strings.HasPrefix(s, "data:"), strings.HasPrefix(s, "{"),
		strings.HasPrefix(s, "<"):
		return false
	}
	// Prose has spaces between words. One long token is an identifier.
	if !strings.Contains(strings.TrimSpace(s), " ") {
		return false
	}
	// And is mostly letters. This is what rejects hashes, version strings,
	// timestamps and anything with a dense run of punctuation in it.
	letters, total := 0, 0
	for _, r := range s {
		total++
		if unicode.IsLetter(r) || unicode.IsSpace(r) {
			letters++
		}
	}
	if total == 0 || float64(letters)/float64(total) < 0.75 {
		return false
	}
	// An i18n key: dots or underscores joining word fragments, no sentence
	// punctuation anywhere.
	if strings.Count(s, "_") > 2 || strings.Count(s, ".") > 3 {
		return false
	}
	return true
}

// HydrationLinks returns the labelled destinations a payload carries.
//
// Frameworks serialise a link as its label and its href, and in a flat payload
// array those land next to each other: "Ethereum" followed by
// "https://ethereum.foundation/". That adjacency is the whole rule, and it is
// deliberately the only one -- resolving Nuxt's index-reference format properly
// would mean reimplementing devalue, and guessing at structure any less
// literally than this is how a config value becomes a link in an artifact.
//
// A label that fails this pairing is simply not harvested. It is not turned
// into prose, because a bare capitalised word with no sentence around it is
// exactly what a component name, a route name and an icon name all look like.
func HydrationLinks(root *html.Node) []HydrationLink {
	var out []HydrationLink
	seen := map[string]bool{}

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.DataAtom == atom.Script {
			if strings.EqualFold(strings.TrimSpace(attr(n, "type")), "application/json") &&
				hydrationContainers[strings.TrimSpace(attr(n, "id"))] {
				var body strings.Builder
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					if c.Type == html.TextNode {
						body.WriteString(c.Data)
					}
				}
				collectHydrationLinks(body.String(), &out, seen)
			}
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	return out
}

func collectHydrationLinks(body string, out *[]HydrationLink, seen map[string]bool) {
	body = strings.TrimSpace(body)
	if body == "" {
		return
	}
	var doc any
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		return
	}
	// Every array, at any depth. Nuxt happens to flatten its whole payload into
	// one top-level array, and scanning only that worked on the site this was
	// written against and on nothing else: a payload that nests its link list
	// one level down -- which is the ordinary shape -- yielded nothing at all.
	var visit func(any, int)
	visit = func(v any, depth int) {
		if depth > 32 || len(*out) >= maxHydrationLinks {
			return
		}
		switch t := v.(type) {
		case []any:
			for i := 0; i+1 < len(t); i++ {
				label, ok1 := t[i].(string)
				href, ok2 := t[i+1].(string)
				if !ok1 || !ok2 {
					continue
				}
				label = strings.TrimSpace(label)
				href = strings.TrimSpace(href)
				if !isHydrationHref(href) || !isHydrationLabel(label) || seen[href] {
					continue
				}
				seen[href] = true
				*out = append(*out, HydrationLink{Text: label, Href: href})
			}
			for _, e := range t {
				visit(e, depth+1)
			}
		case map[string]any:
			for _, e := range t {
				visit(e, depth+1)
			}
		}
	}
	visit(doc, 0)
}

func isHydrationHref(s string) bool {
	if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
		return false
	}
	return utf8.RuneCountInString(s) <= 300
}

// isHydrationLabel accepts what a person would recognise as the words on a
// link, and nothing that looks like a machine's name for something.
func isHydrationLabel(s string) bool {
	n := utf8.RuneCountInString(s)
	if n < 2 || n > 80 {
		return false
	}
	if strings.HasPrefix(s, "http") || strings.ContainsAny(s, "{}[]<>|\\") {
		return false
	}
	letters := 0
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsSpace(r) || r == '&' || r == '-' || r == '\'' {
			letters++
		}
	}
	return float64(letters)/float64(n) >= 0.85
}
