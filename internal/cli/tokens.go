package cli

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/qcoderx/sieve/internal/distill"
	"github.com/qcoderx/sieve/internal/safety"

	"github.com/qcoderx/sieve/internal/emit"
	"github.com/qcoderx/sieve/internal/graph"
	"github.com/qcoderx/sieve/internal/mcpserver"
	"github.com/qcoderx/sieve/internal/tokens"
)

// tokenReport measures what an agent is actually handed for one page read.
//
// The accuracy numbers in this project need a model, a question set and a
// grader, and nobody is going to reproduce them casually. This costs one page
// fetch and no API key, which makes it the only claim here that a sceptical
// reader can check in the time it takes to read the claim.
//
// It measures sieve against an unaided fetch of the same URL, which is the
// comparison sieve controls. It deliberately does not print a figure for any
// other tool: numbers for Playwright MCP or Chrome DevTools MCP belong to
// whoever runs them, and quoting someone else's blog post beside your own
// measurement in the same table invites the reader to assume both were measured
// the same way.
type tokenReport struct {
	URL string `json:"url"`
	// RawPage is what an agent gets from a plain fetch: the served HTML, which
	// is what it must read if it has no browser and no distiller.
	RawPage int `json:"raw_page_tokens"`
	// Manifest is what a distill call actually returns. This is the number that
	// matters, and it is roughly flat in the size of the page: the manifest
	// describes the document and names its sections rather than carrying them.
	Manifest int `json:"manifest_tokens"`
	// Artifact is the whole distilled page, if a caller asks for all of it.
	Artifact int `json:"artifact_tokens"`
	// LargestSection is the most a single get_content call would cost.
	LargestSection int `json:"largest_section_tokens"`
	// ToolSurface is the definition cost, paid once per session rather than per
	// page. It belongs in the same report because it is the number the
	// ecosystem is actually rationing on.
	ToolSurface int `json:"tool_surface_tokens"`

	Outcome  graph.Outcome `json:"outcome"`
	Sections int           `json:"sections"`
}

func measureTokens(g *graph.Graph, rawHTML string) tokenReport {
	man := emit.BuildManifest(g)
	manJSON, _ := json.Marshal(man)

	full := emit.CompactMarkdownOptions()
	full.Actions, full.Navigation, full.Structured, full.Gaps, full.Notes = true, true, true, true, true

	// The graph already counted the served page, and the manifest publishes that
	// count. Recomputing it here from a separately-carried copy of the HTML
	// produced a report saying zero for a page whose own manifest said 193 --
	// two numbers for one quantity, which is the failure this project keeps
	// finding in itself. Use the artifact's figure and fall back only if it is
	// genuinely absent.
	raw := g.Stats.OriginalTokens
	if raw == 0 {
		raw = tokens.EstimateHTML(rawHTML)
	}

	r := tokenReport{
		URL:         g.URL,
		RawPage:     raw,
		Manifest:    tokens.Estimate(string(manJSON)),
		Artifact:    tokens.Estimate(emit.Markdown(g, full)),
		ToolSurface: mcpserver.SurfaceTokens(),
		Outcome:     g.Outcome,
		Sections:    len(g.Sections),
	}
	for _, s := range g.Sections {
		if s.Tokens > r.LargestSection {
			r.LargestSection = s.Tokens
		}
	}
	return r
}

func printTokenReport(w io.Writer, r tokenReport) {
	// A ratio below one is an increase, and saying "0.4x smaller" for one is the
	// kind of phrasing that makes a reader stop believing the rest of the table.
	// On a page of two hundred tokens the manifest genuinely costs more than the
	// page; that is a true and unremarkable fact about small pages, and it
	// should read like one.
	ratio := func(n int) string {
		if n <= 0 || r.RawPage <= 0 {
			return ""
		}
		f := float64(r.RawPage) / float64(n)
		if f < 1 {
			return fmt.Sprintf("%.1fx larger", 1/f)
		}
		return fmt.Sprintf("%.1fx smaller", f)
	}

	fmt.Fprintf(w, "\ntokens: %s\n", r.URL)
	if r.Outcome.Status != graph.StatusOK {
		fmt.Fprintf(w, "  outcome %s -- these figures describe whatever answered, "+
			"not the page\n", r.Outcome.Status)
	}
	fmt.Fprintf(w, "\n  per page read, what the agent receives\n")
	fmt.Fprintf(w, "    unaided fetch of the served HTML   %8d\n", r.RawPage)
	fmt.Fprintf(w, "    sieve distill (manifest)           %8d   %s\n", r.Manifest, ratio(r.Manifest))
	fmt.Fprintf(w, "    one section via get_content        %8d   (largest of %d)\n",
		r.LargestSection, r.Sections)
	fmt.Fprintf(w, "    the whole artifact, if asked for   %8d   %s\n", r.Artifact, ratio(r.Artifact))

	fmt.Fprintf(w, "\n  once per session, not per page\n")
	fmt.Fprintf(w, "    sieve MCP tool definitions         %8d\n", r.ToolSurface)

	fmt.Fprintf(w, "\n  The manifest is what a distill call returns, and it is close to flat in\n")
	fmt.Fprintf(w, "  the size of the page: it names the sections and what each would cost\n")
	fmt.Fprintf(w, "  rather than carrying them. A page ten times larger does not produce a\n")
	fmt.Fprintf(w, "  manifest ten times larger, which is where the margin comes from.\n")
	if r.RawPage > 0 && r.RawPage < r.Manifest {
		fmt.Fprintf(w, "\n  This page is small enough that describing it costs more than reading\n")
		fmt.Fprintf(w, "  it. That is the expected result below a few thousand tokens, and the\n")
		fmt.Fprintf(w, "  honest reading is that sieve has nothing to offer here: fetch it.\n")
	}
	if r.RawPage < r.Artifact {
		fmt.Fprintf(w, "\n  This page served less than the artifact contains, so there was nothing\n")
		fmt.Fprintf(w, "  to reduce: its text is rendered some other way and an unaided fetch\n")
		fmt.Fprintf(w, "  gets none of it. Reduction is the wrong question here; the comparison\n")
		fmt.Fprintf(w, "  that matters on such a page is whether anything was readable at all.\n")
	}
	fmt.Fprintln(w)
}

// runTokens distills a URL and reports the per-read cost.
func runTokens(target string, common commonFlags, out string, stdout, stderr io.Writer) int {
	opts := distill.DefaultOptions()
	opts.Guard = common.guard()
	opts.Limiter = common.limiter()
	opts.Memory = loadMemory(common.memoryPath)
	opts.Robots = safety.NewRobotsCache(nil)
	opts.Render.ChromePath = common.chrome
	if common.verbose {
		opts.Logf = func(f string, a ...any) { fmt.Fprintf(stderr, "  "+f+"\n", a...) }
	}

	d := distill.New(opts)
	defer d.Close()

	ctx, cancel := withTimeout(common.timeout * 3)
	defer cancel()

	fmt.Fprintf(stderr, "distilling %s…\n", target)
	res, err := d.Distill(ctx, target)
	if err != nil {
		return fail(stderr, err)
	}

	rep := measureTokens(res.Graph, res.StaticHTML)
	printTokenReport(stdout, rep)
	if out != "" {
		if err := writeJSON(out, rep); err != nil {
			return fail(stderr, err)
		}
	}
	return 0
}
