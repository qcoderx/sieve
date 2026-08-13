package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/qcoderx/sieve/internal/canvas"
	"github.com/qcoderx/sieve/internal/distill"
	"github.com/qcoderx/sieve/internal/emit"
	"github.com/qcoderx/sieve/internal/graph"
	"github.com/qcoderx/sieve/internal/render"
	"github.com/qcoderx/sieve/internal/safety"
	"github.com/qcoderx/sieve/internal/snapshot"
)

// teardownReserve is the wall-clock margin allowed on top of the load and read
// budgets for emitting the artifact and releasing Chromium. Shutting the browser
// down is the larger half and is easy to forget: it costs a few hundred
// milliseconds whatever the page was.
const teardownReserve = 900 * time.Millisecond

// fetchAllowance mirrors the distiller's tier-0 budget so the command's overall
// deadline can account for it. Kept in step with Distiller.fetchBudget.
func fetchAllowance(loadTimeout time.Duration) time.Duration {
	b := loadTimeout / 3
	if b < 5*time.Second {
		b = 5 * time.Second
	}
	if b > 8*time.Second {
		b = 8 * time.Second
	}
	return b
}

// minComparableTokens is the amount of readable text the served page must have
// carried before a reduction percentage means anything.
const minComparableTokens = 200

func runDistill(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("distill", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		common        commonFlags
		out           string
		snapshotPath  string
		vision        bool
		visionModel   string
		visionCalls   int
		reducedMotion bool
		private       bool
		quiet         bool
		headed        bool
	)
	common.register(fs)
	fs.StringVar(&out, "out", "./artifacts", "directory to write the artifact into")
	fs.StringVar(&snapshotPath, "snapshot", "",
		"also record a replayable snapshot to this path (a directory, or a file ending in .sieve)")
	fs.BoolVar(&vision, "vision", false,
		"allow a vision model to describe canvas regions that yielded nothing else.\n"+
			"Off by default: with vision disabled the artifact structurally cannot contain invented text")
	fs.StringVar(&visionModel, "vision-model", "", "model to use for canvas descriptions")
	fs.IntVar(&visionCalls, "vision-max-calls", 4, "hard ceiling on vision calls per page")
	fs.BoolVar(&reducedMotion, "reduced-motion", false,
		"render with prefers-reduced-motion, which suppresses most scroll animation")
	fs.BoolVar(&private, "private", false,
		"mark the artifact private: never eligible for a shared cache, and snapshots are refused")
	fs.BoolVar(&quiet, "quiet", false, "print only the artifact path")
	fs.BoolVar(&headed, "headed", false,
		"run the browser with a visible window.\n"+
			"A diagnostic: some sites behave differently when they can tell they are\n"+
			"being rendered headlessly, and watching one is the fastest way to find out")

	fs.Usage = func() {
		fmt.Fprint(stderr, "Usage: sieve distill <url> [flags]\n\nFlags:\n")
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
	target := positional[0]

	w, h, verr := common.viewportSize()
	if verr != nil {
		return fail(stderr, verr)
	}
	minTier, maxTier, terr := common.tiers()
	if terr != nil {
		return fail(stderr, terr)
	}

	opts := distill.DefaultOptions()
	opts.MinTier, opts.MaxTier = minTier, maxTier
	if opts.MaxTier == "" {
		opts.MaxTier = distill.DefaultOptions().MaxTier
	}
	opts.Guard = common.guard()
	opts.Limiter = common.limiter()
	opts.Logf = common.logf(stderr)
	opts.Private = private
	opts.Memory = loadMemory(common.memoryPath)

	opts.Render.ViewportW, opts.Render.ViewportH = w, h
	opts.Render.ChromePath = common.chrome
	opts.Render.ReducedMotion = reducedMotion
	opts.Render.Headless = !headed
	opts.Render.Logf = opts.Logf
	// Every render sub-budget follows the timeout the user actually asked for.
	opts.Render.ScaleTo(common.timeout)
	opts.Render.LoadBudget = common.loadTimeout

	opts.Canvas = canvas.DefaultOptions()
	opts.Canvas.EnableVision = vision
	opts.Canvas.MaxVisionCalls = visionCalls
	opts.Canvas.Logf = opts.Logf
	if visionModel != "" {
		opts.Canvas.VisionModel = visionModel
	}
	// Rasterising a canvas is only worth the compositor wait when something is
	// going to look at the pixels.
	opts.Render.CaptureCanvas = vision || opts.Canvas.OCR != nil

	rpath := robotsPath(common.memoryPath)
	if common.obeyRobots {
		opts.Robots = loadRobots(rpath)
	}

	if !quiet {
		fmt.Fprintf(stderr, "sieve %s — distilling %s\n", render.Version, target)
	}

	// The wall clock has to cover every stage that can actually run, or the
	// stages eat each other.
	//
	// It covered the load wait and the read, but not tier 0 -- so a host that
	// is slow to the fetch client spent its fetch budget failing, the browser
	// then began a full, fresh load allowance on top of that, and the overall
	// deadline expired before a single checkpoint was taken. casper.com and
	// womp.com both burned ten seconds on a fetch timeout, waited the whole
	// twenty for the page, and returned nothing with four hundred milliseconds
	// left on the clock.
	//
	// Tier 0 runs before the page is loaded, so its budget belongs in the total
	// alongside the others rather than inside one of them.
	ctx, cancel := withTimeout(
		fetchAllowance(common.loadTimeout) + common.loadTimeout + common.timeout + teardownReserve)
	defer cancel()

	// One distiller, built once, closed once.
	//
	// There were two: one created here and immediately shadowed by a second
	// carrying the progress callback. `defer d.Close()` binds its receiver at
	// the point the defer runs, so the deferred close belonged to the distiller
	// that never did any work, and the one that owned the browser was released
	// only on the success path. Every error return -- an unreachable host, a
	// refusal, a timeout -- leaked a Chromium. That is the hundred and sixty
	// stray processes, and they were slow enough to look like extraction bugs.
	if !quiet {
		opts.OnProgress = func(p distill.Progress) {
			fmt.Fprintf(stderr, "  [%6s] %-8s %s\n",
				p.Elapsed.Round(time.Millisecond), p.Stage, p.Message)
		}
	}
	d := distill.New(opts)
	defer d.Close()

	res, err := d.Distill(ctx, target)
	if err != nil {
		if isBlocked(err) {
			fmt.Fprintf(stderr, "\nsieve: %v\n", err)
			fmt.Fprint(stderr, "\nThis is sieve declining to proceed, not a failure to try. "+
				"sieve respects a site's stated boundaries rather than working around them.\n")
			return 3
		}
		if containsErr(err, safety.ErrUnreachable) {
			fmt.Fprintf(stderr, "\nsieve: %v\n", err)
			fmt.Fprint(stderr, "\nThe host could not be reached, which is not a refusal: "+
				"nothing about this site was declined. Check the name and the resolver, then retry.\n")
			return 4
		}
		return fail(stderr, err)
	}
	saveMemory(common.memoryPath, d.Memory())
	saveRobots(rpath, opts.Robots)

	art, err := emit.Build(res.Graph)
	if err != nil {
		return fail(stderr, err)
	}
	dir := artifactDir(out, target)
	if err := art.Write(dir); err != nil {
		return fail(stderr, err)
	}

	if snapshotPath != "" {
		if err := writeSnapshot(snapshotPath, target, res, private); err != nil {
			fmt.Fprintf(stderr, "sieve: could not record snapshot: %v\n", err)
		} else if !quiet {
			fmt.Fprintf(stderr, "  snapshot: %s\n", snapshotResolved(snapshotPath, target))
		}
	}

	// The browser has nothing left to do, and shutting it down is not free:
	// Chromium takes the better part of a second to exit. Releasing it here,
	// rather than leaving it to the deferred Close after the summary is printed,
	// takes that second off the wall clock the user is measuring.
	tClose := time.Now()
	d.Close()
	if common.verbose {
		fmt.Fprintf(stderr, "  browser released in %v\n", time.Since(tClose).Round(time.Millisecond))
	}

	if quiet {
		fmt.Fprintln(stdout, dir)
		return 0
	}
	printSummary(stdout, dir, res)
	return 0
}

func printSummary(w io.Writer, dir string, res *distill.Result) {
	g := res.Graph
	fmt.Fprintf(w, "\n%s\n", g.Title)
	fmt.Fprintf(w, "%s\n\n", dir)

	fmt.Fprintf(w, "  tier         %s (score %.3f%s)\n",
		g.Provenance.Tier, g.Provenance.TierScore, pinnedSuffix(g.Provenance.TierPinned))
	fmt.Fprintf(w, "               %s\n", g.Provenance.TierReason)
	fmt.Fprintf(w, "  content      %d blocks in %d sections\n", g.Stats.ContentNodes, len(g.Sections))
	fmt.Fprintf(w, "  actions      %d (%d form%s)\n", len(g.Actions), countForms(g), plural(countForms(g)))
	if len(g.Latent) > 0 {
		fmt.Fprintf(w, "  hidden       %d block(s) quarantined, not in the payload\n", len(g.Latent))
	}
	if len(g.Gaps) > 0 {
		fmt.Fprintf(w, "  gaps         %d disclosure control(s) declared but not opened\n", len(g.Gaps))
	}

	// A percentage is only meaningful when there was something to compare
	// against. A page that serves a shell -- or one whose fetch failed -- gives
	// a denominator of almost nothing, and the artifact then either reports a
	// saving it did not make or an increase that reads like a fault. Saying what
	// happened is more use than a number that cannot be true.
	fmt.Fprintf(w, "\n  tokens       %d → %d", g.Stats.OriginalTokens, g.Stats.ArtifactTokens)
	switch {
	case g.Stats.OriginalTokens < minComparableTokens:
		fmt.Fprint(w, "  (the served page carried no readable text, so there is nothing to compare)")
	case g.Stats.ArtifactTokens < g.Stats.OriginalTokens:
		saved := 100 * (1 - float64(g.Stats.ArtifactTokens)/float64(g.Stats.OriginalTokens))
		fmt.Fprintf(w, "  (%.1f%% fewer)", saved)
	default:
		fmt.Fprint(w, "  (larger than the served page, which carried little readable text)")
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  bytes        %s → %s\n",
		humanBytes(g.Stats.OriginalBytes), humanBytes(g.Stats.ArtifactBytes))

	fmt.Fprintf(w, "\n  audit\n")
	fmt.Fprintf(w, "    retention  %.1f%% of observed text reached the graph\n", g.Audit.GraphRetention*100)
	fmt.Fprintf(w, "    order      %s (%s basis, %.0f%% method agreement)\n",
		g.Audit.OrderConfidence, g.Audit.OrderBasis, g.Audit.OrderAgreement*100)
	fmt.Fprintf(w, "    headings   %s\n", g.Audit.HeadingConfidence)
	for _, d := range g.Audit.Dropped {
		fmt.Fprintf(w, "    excluded   %d run(s), %d chars — %s\n", d.Runs, d.Chars, d.Reason)
	}
	if !g.Audit.ReachedBottom {
		fmt.Fprintf(w, "    !          the sweep did not reach the bottom of the document\n")
	}
	for _, n := range g.Audit.Notes {
		fmt.Fprintf(w, "    note       %s\n", n)
	}

	// An artifact with no content is the most alarming thing this tool can
	// produce, so it never goes out without an explanation attached.
	if g.Stats.ContentNodes == 0 {
		fmt.Fprintf(w, "\n  This page yielded no readable content. %s\n", emptyDiagnosis(g))
	}

	if len(res.Timing) > 0 {
		fmt.Fprintf(w, "\n  timing\n")
		for _, k := range sortedKeys(res.Timing) {
			fmt.Fprintf(w, "    %-10s %v\n", k, res.Timing[k].Round(time.Millisecond))
		}
	}
	fmt.Fprintln(w)
}

// emptyDiagnosis says why a page produced nothing, in the terms a user can act
// on. "No content" with no explanation is indistinguishable from a broken tool.
func emptyDiagnosis(g *graph.Graph) string {
	switch {
	case g.Provenance.Blocked:
		return "The site refused this client (" + g.Provenance.BlockedReason +
			"). sieve respects that rather than working around it."
	case len(g.Audit.Dropped) == 1 && g.Audit.Dropped[0].Reason == graph.DropNonLexical:
		// Everything the browser rendered was punctuation. That is not a page
		// sieve mishandled; it is a page with no words in it. igloo.inc serves
		// an empty <body>, boots a WebGL scene into a bare div, and shows an
		// animated ASCII bar while it does -- so the only text that ever
		// existed was the loading bar, and the site itself is geometry.
		return "Everything the browser rendered was punctuation — a loading indicator " +
			"or separators — and no run of it contained a letter or a digit. This page " +
			"draws its content into a canvas or a WebGL surface rather than writing it " +
			"into the document, so there is no text on it to extract. A vision pass " +
			"(-vision) is the only thing that would describe it."
	case len(g.Audit.Dropped) > 0:
		d := g.Audit.Dropped[0]
		return fmt.Sprintf("Every run that was captured was excluded: %s. "+
			"If that reason looks wrong for this page, it is a bug worth reporting with a snapshot.", d.Reason)
	case len(g.Latent) > 0:
		return fmt.Sprintf("All %d run(s) of text on it were hidden from visitors and are quarantined; "+
			"retrieve them with the hidden-content tool if you need them.", len(g.Latent))
	case g.Stats.RawNodes == 0:
		return "The browser reported no text at all. The page's words are most likely drawn " +
			"into a canvas, or held behind an interaction such as a click-to-enter screen."
	default:
		return "Its text was captured but none of it became content. This is worth reporting with a snapshot."
	}
}

func pinnedSuffix(pinned bool) string {
	if pinned {
		return ", pinned by an earlier run"
	}
	return ""
}

func countForms(g *graph.Graph) int {
	n := 0
	for _, a := range g.Actions {
		if a.Type == "form" {
			n++
		}
	}
	return n
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// artifactDir derives a stable directory name from a URL, so re-distilling the
// same page overwrites rather than accumulating.
func artifactDir(root, rawURL string) string {
	name := rawURL
	for _, p := range []string{"https://", "http://"} {
		name = trimPrefix(name, p)
	}
	name = sanitise(name)
	if name == "" {
		name = "artifact"
	}
	if len(name) > 100 {
		name = name[:100]
	}
	return filepath.Join(root, name)
}

func trimPrefix(s, p string) string {
	if len(s) >= len(p) && s[:len(p)] == p {
		return s[len(p):]
	}
	return s
}

func sanitise(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			out = append(out, r)
		default:
			out = append(out, '-')
		}
	}
	// Collapse runs of separators so a query string does not produce a name of
	// forty consecutive dashes.
	collapsed := make([]rune, 0, len(out))
	prevDash := false
	for _, r := range out {
		if r == '-' {
			if prevDash {
				continue
			}
			prevDash = true
		} else {
			prevDash = false
		}
		collapsed = append(collapsed, r)
	}
	return string(collapsed)
}

func snapshotResolved(path, target string) string {
	if fi, err := os.Stat(path); err == nil && fi.IsDir() {
		return snapshot.DefaultPath(path, target)
	}
	if filepath.Ext(path) == "" {
		return snapshot.DefaultPath(path, target)
	}
	return path
}

func writeSnapshot(path, target string, res *distill.Result, private bool) error {
	s := &snapshot.Snapshot{
		RequestedURL: target,
		FinalURL:     res.Graph.FinalURL,
		Status:       res.Status,
		Trace:        res.Graph.Provenance.Trace,
		Merged:       res.Capture,
		Scene:        res.Scene,
		Libraries:    res.Libraries,
		StaticHTML:   res.StaticHTML,
		Notes:        res.Graph.Audit.Notes,
		Tier:         res.Graph.Provenance.Tier,
	}
	return snapshot.Write(snapshotResolved(path, target), s, snapshot.WriteOptions{
		Private:     private,
		IncludeHTML: true,
	})
}

func isBlocked(err error) bool {
	return err != nil && (containsErr(err, safety.ErrBlocked))
}

func containsErr(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
