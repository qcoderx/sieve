// Package distill is the orchestrator: it takes a URL and returns an artifact,
// choosing how much work to do along the way.
//
// The shape of a run is always the same, whatever tier answers:
//
//	fetch  ->  score  ->  [render]  ->  [sweep]  ->  [recover]  ->  graph  ->  emit
//
// Every rung is optional except the first and the last two, and the decision to
// climb is recorded in the artifact rather than left implicit. A caller can
// always see which tier answered and why.
package distill

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/qcoderx/sieve/internal/canvas"
	"github.com/qcoderx/sieve/internal/capture"
	"github.com/qcoderx/sieve/internal/escalate"
	"github.com/qcoderx/sieve/internal/fetch"
	"github.com/qcoderx/sieve/internal/graph"
	"github.com/qcoderx/sieve/internal/render"
	"github.com/qcoderx/sieve/internal/safety"
	"github.com/qcoderx/sieve/internal/static"
)

// Options configures a distillation.
type Options struct {
	// MaxTier caps the ladder. Setting it to TierFetch forbids the browser
	// entirely, which is what a caller with no Chromium wants.
	MaxTier escalate.Tier
	// MinTier forces at least this much work, for a caller who already knows
	// the page is heavy.
	MinTier escalate.Tier
	// Thresholds are pinned escalation constants.
	Thresholds escalate.Thresholds
	// Memory provides per-domain hysteresis. Optional but strongly recommended:
	// without it the tier decision is recomputed from scratch every run and a
	// page near the threshold will waver.
	Memory *escalate.Memory

	// Render configures the browser, when one is used.
	Render render.Options
	// Fetch configures tier 0.
	Fetch fetch.Options

	// Robots enforces robots.txt. A nil cache means no enforcement, which is
	// only appropriate for a site the caller owns.
	Robots *safety.RobotsCache
	// Limiter enforces politeness towards a single site.
	Limiter *safety.Limiter
	// Guard vets every URL.
	Guard *safety.Guard

	// Canvas configures recovery. Vision is off unless explicitly enabled.
	Canvas canvas.Options

	// Private marks the run as using an authenticated or user-profile session,
	// which makes the artifact ineligible for any shared cache.
	Private bool

	// Now is injectable so golden tests are not time-dependent.
	Now time.Time
	// Generator identifies the build.
	Generator string
	// Logf receives progress lines.
	Logf func(format string, args ...any)

	// OnProgress is called as stages complete, so a caller can emit a manifest
	// as soon as one exists rather than after the whole sweep. Time to first
	// useful token is the metric that matters; thirty seconds of silence and
	// thirty seconds with usable output at two seconds are different products.
	OnProgress func(Progress)
}

// Progress reports a stage boundary.
type Progress struct {
	Stage   string
	Tier    escalate.Tier
	Message string
	Elapsed time.Duration
	// Partial carries a graph good enough to answer some questions, when one
	// exists. It is always marked incomplete.
	Partial *graph.Graph
}

// DefaultOptions returns a usable configuration.
func DefaultOptions() Options {
	return Options{
		MaxTier:    escalate.TierRecover,
		MinTier:    escalate.TierFetch,
		Thresholds: escalate.DefaultThresholds(),
		Render:     render.DefaultOptions(),
		Fetch:      fetch.DefaultOptions(),
		Canvas:     canvas.DefaultOptions(),
		Generator:  "sieve/" + render.Version,
	}
}

// Result is a completed distillation.
type Result struct {
	Graph *graph.Graph
	// Freshness is what to store so the next run can skip work.
	Freshness fetch.Freshness
	// Decision records the tier and its reasoning.
	Decision escalate.Decision
	// Timing breaks down where the wall clock went.
	Timing map[string]time.Duration

	// Capture is the deduplicated observation the graph was built from. It is
	// retained so a snapshot can be recorded: storing the artifact would only
	// prove what the graph produced last time, whereas storing the capture lets
	// the whole graph stage be re-run against new code.
	Capture *capture.Merged
	// StaticHTML is the served document.
	StaticHTML string
	// Scene and Libraries are the page-level probes, kept for the same reason.
	Scene     *capture.SceneIntrospection
	Libraries []string
	Status    int64
}

// Distiller runs distillations, reusing a browser across them.
type Distiller struct {
	opts    Options
	fetcher *fetch.Client
	browser *render.Browser
	memory  *escalate.Memory
}

// New builds a distiller. The browser is not started here: a caller that only
// ever hits tier 0 should never pay for one.
func New(opts Options) *Distiller {
	if opts.Thresholds == (escalate.Thresholds{}) {
		opts.Thresholds = escalate.DefaultThresholds()
	}
	if opts.MaxTier == "" {
		opts.MaxTier = escalate.TierRecover
	}
	if opts.Generator == "" {
		opts.Generator = "sieve/" + render.Version
	}
	mem := opts.Memory
	if mem == nil {
		mem = escalate.NewMemory()
	}
	fo := opts.Fetch
	if fo.Guard == nil {
		fo.Guard = opts.Guard
	}
	return &Distiller{opts: opts, fetcher: fetch.New(fo), memory: mem}
}

// Close releases the browser, if one was started.
func (d *Distiller) Close() {
	if d.browser != nil {
		d.browser.Close()
		d.browser = nil
	}
}

// Memory exposes the escalation memory so a caller can persist it.
func (d *Distiller) Memory() *escalate.Memory { return d.memory }

func (d *Distiller) logf(format string, args ...any) {
	if d.opts.Logf != nil {
		d.opts.Logf(format, args...)
	}
}

func (d *Distiller) progress(p Progress) {
	if d.opts.OnProgress != nil {
		d.opts.OnProgress(p)
	}
}

// Distill produces an artifact for one URL.
func (d *Distiller) Distill(ctx context.Context, rawURL string) (*Result, error) {
	start := time.Now()
	timing := map[string]time.Duration{}

	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse URL: %w", err)
	}
	if u.Scheme == "" {
		u.Scheme = "https"
		rawURL = u.String()
	}

	if d.opts.Guard != nil {
		if err := d.opts.Guard.Check(u); err != nil {
			return nil, err
		}
	}
	// robots.txt is consulted before anything is fetched, not after. Asking
	// permission after taking the page is not asking permission.
	if d.opts.Robots != nil {
		if err := d.opts.Robots.Allowed(ctx, u); err != nil {
			return nil, err
		}
		if r, rerr := d.opts.Robots.Get(ctx, u); rerr == nil && r.CrawlDelay > 0 && d.opts.Limiter != nil {
			d.opts.Limiter.SetDelay(u.Hostname(), r.CrawlDelay)
		}
	}
	if d.opts.Limiter != nil {
		release, lerr := d.opts.Limiter.Acquire(ctx, u)
		if lerr != nil {
			return nil, lerr
		}
		defer release()
	}

	// --- Tier 0 -------------------------------------------------------------
	t0 := time.Now()
	resp, err := d.fetcher.Get(ctx, rawURL, nil)
	if err != nil {
		return nil, err
	}
	timing["fetch"] = time.Since(t0)
	d.progress(Progress{Stage: "fetch", Tier: escalate.TierFetch,
		Message: fmt.Sprintf("HTTP %d, %d bytes", resp.Status, len(resp.Body)), Elapsed: time.Since(start)})

	if !resp.IsHTML() && !resp.Blocked {
		return nil, fmt.Errorf("%s served %q, which is not an HTML document",
			resp.FinalURL, resp.ContentType)
	}

	staticRes, err := static.Extract(resp.FinalURL, strings.NewReader(string(resp.Body)), len(resp.Body))
	if err != nil {
		return nil, fmt.Errorf("static extraction: %w", err)
	}
	timing["static"] = time.Since(t0)

	// --- Decide -------------------------------------------------------------
	decision := escalate.Score(staticRes.Signals, 0, "", d.opts.Thresholds)
	decision = d.memory.Apply(u.Hostname(), decision)
	decision = clampTier(decision, d.opts.MinTier, d.opts.MaxTier)
	d.logf("tier=%s score=%.3f pinned=%v — %s",
		decision.Tier, decision.Score, decision.Pinned, decision.Reason)

	freshness := fetch.Freshness{
		ETag:         resp.ETag,
		LastModified: resp.LastModified,
		RawHash:      fetch.HashBytes(resp.Body),
		TextHash:     fetch.StaticTextHash(staticRes),
		CheckedAt:    time.Now().UTC(),
	}

	prov := graph.Provenance{
		Tier:          string(decision.Tier),
		TierReason:    decision.Reason,
		TierScore:     decision.Score,
		TierPinned:    decision.Pinned,
		Blocked:       resp.Blocked,
		BlockedReason: resp.BlockedReason,
		Private:       d.opts.Private,
	}

	// A refusal degrades rather than failing. A labelled partial artifact from
	// the served HTML is more useful than an error, and pretending the page was
	// empty would be worse than both.
	if resp.Blocked {
		d.logf("site refused this client (%s); staying at tier 0 and labelling the artifact", resp.BlockedReason)
		prov.Tier = string(escalate.TierFetch)
		g, gerr := d.buildGraph(rawURL, resp, staticRes.Merged, staticRes, prov,
			[]string{"the site refused this client (" + resp.BlockedReason + "); this artifact was built from the served HTML alone and is likely incomplete"},
			false)
		if gerr != nil {
			return nil, gerr
		}
		timing["total"] = time.Since(start)
		return &Result{
			Graph: g, Freshness: freshness, Decision: decision, Timing: timing,
			Capture: staticRes.Merged, StaticHTML: staticRes.RawHTML,
			Status: int64(resp.Status),
		}, nil
	}

	// --- Tier 0 answers -----------------------------------------------------
	if decision.Tier == escalate.TierFetch {
		g, gerr := d.buildGraph(rawURL, resp, staticRes.Merged, staticRes, prov, nil, true)
		if gerr != nil {
			return nil, gerr
		}
		timing["total"] = time.Since(start)
		d.progress(Progress{Stage: "done", Tier: escalate.TierFetch, Elapsed: time.Since(start)})
		return &Result{Graph: g, Freshness: freshness, Decision: decision, Timing: timing}, nil
	}

	// --- Tiers 1-3 ----------------------------------------------------------
	if err := d.ensureBrowser(ctx); err != nil {
		// No browser available. Rather than failing, fall back to what tier 0
		// produced and say plainly that the artifact is the cheap version.
		d.logf("browser unavailable (%v); falling back to tier 0", err)
		prov.Tier = string(escalate.TierFetch)
		prov.TierReason = fmt.Sprintf("escalation to %q was chosen but no browser was available: %v", decision.Tier, err)
		g, gerr := d.buildGraph(rawURL, resp, staticRes.Merged, staticRes, prov,
			[]string{"a browser was required for this page but none was available; this artifact was built from the served HTML alone"},
			false)
		if gerr != nil {
			return nil, gerr
		}
		timing["total"] = time.Since(start)
		return &Result{
			Graph: g, Freshness: freshness, Decision: decision, Timing: timing,
			Capture: staticRes.Merged, StaticHTML: staticRes.RawHTML,
			Status: int64(resp.Status),
		}, nil
	}

	tr := time.Now()
	sweepOpts := d.opts.Render
	if decision.Tier == escalate.TierRender {
		// One capture after settle, no sweep. Enough for a client-rendered page
		// whose content is all present once JavaScript has run.
		sweepOpts.MaxCheckpoints = 1
		sweepOpts.StableCheckpoints = 1
	}
	d.browser.SetOptions(sweepOpts)

	res, err := d.browser.Sweep(ctx, rawURL, d.guardFunc())
	if err != nil {
		return nil, err
	}
	timing["render"] = time.Since(tr)
	d.progress(Progress{Stage: "render", Tier: decision.Tier,
		Message: fmt.Sprintf("%d checkpoints", res.Merged.Checkpoints), Elapsed: time.Since(start)})

	// The browser has now told us things the served HTML could not: which
	// libraries are loaded, and how much of the viewport a canvas covers. Score
	// again with that evidence and remember the outcome for this domain.
	if w, name := render.LibraryWeight(res.Libraries); w > 0 {
		post := escalate.Score(staticRes.Signals, w, name, d.opts.Thresholds)
		if post.Tier.Rank() > decision.Tier.Rank() {
			d.logf("post-render evidence raises the tier to %s (%s)", post.Tier, post.Reason)
		}
		d.memory.Note(u.Hostname(), post.Tier)
		prov.Libraries = res.Libraries
	}
	prov.Trace = res.Trace
	prov.Blocked = prov.Blocked || res.Blocked
	if res.BlockedReason != "" {
		prov.BlockedReason = res.BlockedReason
	}
	if res.Corpus != nil {
		prov.CorpusBytes = res.Corpus.Size()
		prov.CorpusSaturated = res.Corpus.Saturated()
	}

	notes := res.Notes
	for _, n := range render.LibraryNotes(res.Libraries) {
		notes = append(notes, n)
	}

	// --- Tier 3: canvas recovery -------------------------------------------
	var recovered []canvas.Recovery
	if decision.Tier == escalate.TierRecover && len(res.Merged.Canvases) > 0 {
		tc := time.Now()
		rec := canvas.NewRecoverer(d.opts.Canvas)
		recovered, err = rec.Recover(ctx, canvas.Input{
			Canvases: res.Merged.Canvases,
			Assets:   assetBodies(res.Assets),
			Shots:    shotBytes(res.CanvasShots),
			Scene:    res.Scene,
			Corpus:   res.Corpus,
		})
		if err != nil {
			notes = append(notes, "canvas recovery failed: "+err.Error())
		}
		timing["recover"] = time.Since(tc)
		d.progress(Progress{Stage: "recover", Tier: decision.Tier,
			Message: fmt.Sprintf("%d canvas region(s)", len(recovered)), Elapsed: time.Since(start)})
	}

	prov.Tier = string(decision.Tier)
	g, err := d.buildGraph(rawURL, resp, res.Merged, staticRes, prov, notes, reachedBottom(res))
	if err != nil {
		return nil, err
	}
	appendRecoveries(g, recovered)

	timing["total"] = time.Since(start)
	d.progress(Progress{Stage: "done", Tier: decision.Tier, Elapsed: time.Since(start), Partial: g})
	return &Result{
		Graph: g, Freshness: freshness, Decision: decision, Timing: timing,
		Capture: res.Merged, StaticHTML: staticRes.RawHTML,
		Scene: res.Scene, Libraries: res.Libraries, Status: res.Status,
	}, nil
}

func (d *Distiller) buildGraph(rawURL string, resp *fetch.Response, merged *capture.Merged,
	staticRes *static.Result, prov graph.Provenance, notes []string, reachedBottom bool) (*graph.Graph, error) {

	return graph.Build(graph.Input{
		RequestedURL:  rawURL,
		FinalURL:      resp.FinalURL,
		Merged:        merged,
		Notes:         notes,
		OriginalBytes: int64(len(resp.Body)),
		OriginalText:  staticRes.RawHTML,
		ReachedBottom: reachedBottom,
		Now:           d.opts.Now,
		Generator:     d.opts.Generator,
		Provenance:    prov,
	})
}

func (d *Distiller) ensureBrowser(ctx context.Context) error {
	if d.browser != nil {
		return nil
	}
	b, err := render.Launch(ctx, d.opts.Render)
	if err != nil {
		return err
	}
	d.browser = b
	return nil
}

func (d *Distiller) guardFunc() render.NavGuard {
	if d.opts.Guard == nil {
		return nil
	}
	return func(u *url.URL) error { return d.opts.Guard.Check(u) }
}

func clampTier(d escalate.Decision, min, max escalate.Tier) escalate.Decision {
	if min != "" && d.Tier.Rank() < min.Rank() {
		d.Reason = fmt.Sprintf("raised to %q by configuration (page scored %.3f)", min, d.Score)
		d.Tier = min
	}
	if max != "" && d.Tier.Rank() > max.Rank() {
		d.Reason = fmt.Sprintf("capped at %q by configuration (page scored %.3f, which would have chosen %q)",
			max, d.Score, d.Tier)
		d.Tier = max
	}
	return d
}

func reachedBottom(res *render.Result) bool {
	for _, n := range res.Notes {
		if strings.Contains(n, "budget") || strings.Contains(n, "cap of") {
			return false
		}
	}
	return true
}

func assetBodies(assets []render.Asset) map[string][]byte {
	out := make(map[string][]byte, len(assets))
	for _, a := range assets {
		out[a.URL] = a.Body
	}
	return out
}

func shotBytes(shots map[string]*render.CanvasShot) map[string]canvas.Shot {
	out := make(map[string]canvas.Shot, len(shots))
	for k, v := range shots {
		if v == nil {
			continue
		}
		out[k] = canvas.Shot{PNG: v.PNG, Uniform: v.Uniform, Share: v.Share}
	}
	return out
}

// appendRecoveries folds canvas recoveries into the graph.
//
// They are appended rather than woven into the reading order because a canvas
// has a position but its recovered text does not: the words came from a scene
// graph or a caption, not from a rectangle on the page.
func appendRecoveries(g *graph.Graph, recs []canvas.Recovery) {
	if len(recs) == 0 {
		return
	}
	unrecovered := 0
	for _, r := range recs {
		if r.Text == "" {
			unrecovered++
			continue
		}
		verified := graph.VerificationSpeculative
		if r.Confirmed {
			verified = graph.VerificationConfirmed
		}
		// An exact recovery needs no corroboration: a canvas's own
		// accessibility fallback and a scene graph's node names are text the
		// site authored, not a guess about pixels.
		if r.Source == graph.SourceCanvasFallback || r.Source == graph.SourceCanvasScene {
			verified = graph.VerificationNone
		}
		g.Blocks = append(g.Blocks, graph.Block{
			ID:         fmt.Sprintf("b_c%02d", len(g.Blocks)),
			Type:       graph.TypeParagraph,
			Text:       r.Text,
			Order:      len(g.Blocks),
			Source:     r.Source,
			Score:      r.Score,
			Confidence: graph.Bucket(r.Score),
			Verified:   verified,
			Region:     graph.RegionMain,
			BBox:       r.BBox,
		})
	}
	g.Stats.ContentNodes = 0
	for _, b := range g.Blocks {
		if !b.Region.IsChrome() {
			g.Stats.ContentNodes++
		}
	}
	g.Audit.CanvasesUnrecovered = unrecovered
}
