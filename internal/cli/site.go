package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/qcoderx/sieve/internal/distill"
	"github.com/qcoderx/sieve/internal/emit"
	"github.com/qcoderx/sieve/internal/graph"
	"github.com/qcoderx/sieve/internal/safety"
)

// Reading a documentation site is the highest-frequency web-reading task an
// agent has, and doing it one URL at a time is how a context window is spent.
//
// The unit of work is wrong. An agent looking for how to configure something
// does not know which of forty pages holds it, so it fetches the landing page,
// guesses, fetches another, guesses again. Each fetch is a full page. What it
// needs first is the shape of the site: which pages exist, what each is called,
// and what each would cost -- then one page, chosen.
//
// So this produces a site manifest: every page read, its title, its outcome and
// its token cost, in a few hundred tokens. The pages themselves sit beside it
// and are opened only when wanted.
//
// It is deliberately not a crawler. One origin, bounded depth, bounded count,
// robots obeyed, links taken from the page's own navigation. A tool that
// wandered would be a different thing with a different set of obligations.

// sitePage is one page in the manifest.
type sitePage struct {
	URL     string       `json:"url"`
	Path    string       `json:"path"`
	Title   string       `json:"title"`
	Outcome graph.Status `json:"outcome"`
	Tokens  int          `json:"est_tokens"`
	// Sections is how many parts the page has, so a caller can tell a landing
	// page from a reference document without opening either.
	Sections int    `json:"sections"`
	Depth    int    `json:"depth"`
	Error    string `json:"error,omitempty"`
}

// siteManifest is what an agent reads instead of the site.
type siteManifest struct {
	Root        string     `json:"root"`
	GeneratedAt time.Time  `json:"generated_at"`
	Pages       []sitePage `json:"pages"`
	// TotalTokens is what opening every page would cost, so the choice not to
	// is an informed one.
	TotalTokens int    `json:"est_total_tokens"`
	Guidance    string `json:"guidance"`
}

const siteGuidance = "Each page was read separately. Open the ones you need by path; " +
	"est_tokens says what each costs and est_total_tokens what all of them would. " +
	"A page whose outcome is not \"ok\" did not read: say so rather than treating it as empty."

func runSite(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("site", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		common   commonFlags
		out      string
		maxPages int
		depth    int
		include  string
		quiet    bool
	)
	common.register(fs)
	fs.StringVar(&out, "out", "./artifacts", "directory to write the artifacts into")
	fs.IntVar(&maxPages, "max-pages", 20, "how many pages to read, including the root")
	fs.IntVar(&depth, "depth", 1,
		"how far to follow links. 1 means the root and the pages it links to")
	fs.StringVar(&include, "include", "",
		"only follow links whose path contains this, e.g. docs or /docs/concepts.\n"+
			"Leading and trailing slashes are ignored, which also sidesteps Git Bash\n"+
			"on Windows rewriting a leading slash into a filesystem path")
	fs.BoolVar(&quiet, "quiet", false, "print only the site manifest path")

	fs.Usage = func() {
		fmt.Fprint(stderr, `Usage: sieve site <url> [flags]

Reads a site across pages and writes one manifest naming every page, its title
and what it would cost to open. Same origin only, bounded, obeys robots.txt.

Flags:
`)
		fs.PrintDefaults()
	}
	positional, err := parseArgs(fs, args)
	if err != nil {
		return 2
	}
	if len(positional) != 1 {
		fs.Usage()
		return 2
	}
	root := positional[0]

	rootURL, uerr := url.Parse(root)
	if uerr != nil || rootURL.Host == "" {
		return fail(stderr, fmt.Errorf("%q is not a URL sieve can read", root))
	}
	if maxPages < 1 {
		maxPages = 1
	}

	opts := distill.DefaultOptions()
	opts.Guard = common.guard()
	opts.Limiter = common.limiter()
	opts.Memory = loadMemory(common.memoryPath)
	opts.Robots = safety.NewRobotsCache(nil)
	opts.Render.ChromePath = common.chrome
	opts.Render.ScaleTo(common.timeout)
	opts.Render.LoadBudget = common.loadTimeout
	if common.verbose {
		opts.Logf = func(f string, a ...any) { fmt.Fprintf(stderr, "  "+f+"\n", a...) }
	}

	d := distill.New(opts)
	defer d.Close()

	// One browser for the whole site. Chromium costs a few hundred milliseconds
	// to start and the point of reading twenty pages together is to pay that
	// once.
	man := siteManifest{Root: root, GeneratedAt: time.Now().UTC(), Guidance: siteGuidance}
	seen := map[string]bool{canonical(rootURL): true}
	type queued struct {
		u     string
		depth int
	}
	queue := []queued{{root, 0}}

	for len(queue) > 0 && len(man.Pages) < maxPages {
		cur := queue[0]
		queue = queue[1:]

		if !quiet {
			fmt.Fprintf(stderr, "  [%2d/%2d] %s\n", len(man.Pages)+1, maxPages, cur.u)
		}
		// Per page, not for the whole run: one slow page must not consume the
		// budget of the nineteen after it.
		pageCtx, cancel := withTimeout(
			fetchAllowance(common.loadTimeout) + common.loadTimeout + common.timeout + teardownReserve)
		res, derr := d.Distill(pageCtx, cur.u)
		cancel()

		if derr != nil {
			man.Pages = append(man.Pages, sitePage{
				URL: cur.u, Depth: cur.depth, Error: derr.Error(),
			})
			continue
		}

		dir := artifactDir(out, cur.u)
		art, aerr := emit.Build(res.Graph)
		if aerr == nil {
			aerr = art.Write(dir)
		}
		if aerr != nil {
			man.Pages = append(man.Pages, sitePage{
				URL: cur.u, Depth: cur.depth, Error: aerr.Error(),
			})
			continue
		}

		rel, rerr := filepath.Rel(out, dir)
		if rerr != nil {
			rel = dir
		}
		man.Pages = append(man.Pages, sitePage{
			URL:      cur.u,
			Path:     filepath.ToSlash(rel),
			Title:    res.Graph.Title,
			Outcome:  res.Graph.Outcome.Status,
			Tokens:   res.Graph.Stats.ArtifactTokens,
			Sections: len(res.Graph.Sections),
			Depth:    cur.depth,
		})
		man.TotalTokens += res.Graph.Stats.ArtifactTokens

		if cur.depth >= depth {
			continue
		}
		candidates := sameSiteLinks(res.Graph, rootURL, include)
		if common.verbose {
			fmt.Fprintf(stderr, "  include=%q  %d link(s) on the page, %d worth following\n",
				include, len(res.Graph.Links), len(candidates))
		}
		for _, next := range candidates {
			if len(seen)+len(queue) >= maxPages*4 {
				break // do not build a queue far larger than will ever be read
			}
			if seen[next] {
				continue
			}
			seen[next] = true
			queue = append(queue, queued{next, cur.depth + 1})
		}
	}

	// Stable order: shallowest first, then by URL, so two runs of the same site
	// produce the same manifest.
	sort.SliceStable(man.Pages, func(i, j int) bool {
		if man.Pages[i].Depth != man.Pages[j].Depth {
			return man.Pages[i].Depth < man.Pages[j].Depth
		}
		return man.Pages[i].URL < man.Pages[j].URL
	})

	if err := os.MkdirAll(out, 0o755); err != nil {
		return fail(stderr, err)
	}
	path := filepath.Join(out, "site.json")
	blob, merr := json.MarshalIndent(man, "", "  ")
	if merr != nil {
		return fail(stderr, merr)
	}
	if werr := os.WriteFile(path, blob, 0o644); werr != nil {
		return fail(stderr, werr)
	}

	if quiet {
		fmt.Fprintln(stdout, path)
		return 0
	}
	printSite(stdout, path, man)
	return 0
}

// sameSiteLinks picks the links worth following.
//
// Same origin, http(s) only, no fragments and no query strings: a fragment is
// the same document and a query string is usually a filter, a sort or a session,
// none of which is another page of documentation. Navigation links come first,
// because a documentation site puts its structure there and its body links are
// mostly citations.
func sameSiteLinks(g *graph.Graph, root *url.URL, include string) []string {
	var nav, body []string
	for _, l := range g.Links {
		u, err := url.Parse(l.Href)
		if err != nil || u.Host != root.Host {
			continue
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			continue
		}
		if u.RawQuery != "" {
			continue
		}
		if include != "" && !strings.Contains(pathKey(u.Path), pathKey(include)) {
			continue
		}
		u.Fragment = ""
		c := canonical(u)
		if l.Region.IsChrome() {
			nav = append(nav, c)
		} else {
			body = append(body, c)
		}
	}
	return append(nav, body...)
}

// pathKey normalises a path fragment for comparison.
//
// The slashes are stripped because a caller writing --include /docs and one
// writing --include docs mean the same thing, and because Git Bash on Windows
// silently rewrites any argument beginning with a slash into a filesystem path:
// --include /docs arrived as "C:/Program Files/Git/docs" and matched nothing on
// any site. A flag that works in one shell and fails silently in another is a
// bug in the flag.
func pathKey(s string) string {
	return strings.Trim(strings.ToLower(s), "/")
}

// canonical is the form two links to the same page agree on.
func canonical(u *url.URL) string {
	c := *u
	c.Fragment = ""
	if c.Path == "" {
		c.Path = "/"
	}
	c.Path = strings.TrimSuffix(c.Path, "/")
	if c.Path == "" {
		c.Path = "/"
	}
	return c.String()
}

func printSite(w io.Writer, path string, m siteManifest) {
	fmt.Fprintf(w, "\n%s\n", m.Root)
	fmt.Fprintf(w, "  %d page(s), %d tokens if every one were opened\n\n", len(m.Pages), m.TotalTokens)
	fmt.Fprintf(w, "  %-8s %-9s %s\n", "tokens", "outcome", "page")
	for _, p := range m.Pages {
		if p.Error != "" {
			fmt.Fprintf(w, "  %-8s %-9s %s\n", "-", "failed", p.URL)
			continue
		}
		title := p.Title
		if len(title) > 44 {
			title = title[:43] + "…"
		}
		fmt.Fprintf(w, "  %-8d %-9s %s\n", p.Tokens, p.Outcome, title)
	}
	fmt.Fprintf(w, "\n  manifest %s\n\n", path)
}
