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
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

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
	memory  *escalate.Memory

	// mu guards browser and closed, which the speculative launch goroutine
	// writes and the distillation reads.
	mu      sync.Mutex
	browser *render.Browser
	closed  bool
	// warming tracks the speculative launch so Close can never return while a
	// browser is still being born. Without that wait, a run cancelled during
	// startup leaves a Chromium process with nothing holding its context --
	// which is how a machine ends up with a hundred and sixty of them.
	warming sync.WaitGroup
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
	// A redirect budget belongs to a page, not to a process.
	//
	// Sharing one guard across every distillation counts every hop every page
	// has ever taken, so a long-lived server works for a handful of URLs and
	// then refuses everything: the MCP server answered four pages and rejected
	// the next hundred and eighty with "more than 8 redirects", including pages
	// that redirect exactly once. The CLI never showed it, because there each
	// run is a new process with a new guard.
	//
	// Taking a copy here rather than resetting the caller's is what makes it
	// safe under concurrency -- the MCP server runs jobs in parallel, and
	// clearing a shared counter mid-chain would spend another page's budget.
	opts.Guard = opts.Guard.ForPage()
	fo := opts.Fetch
	if fo.Guard == nil {
		fo.Guard = opts.Guard
	}
	return &Distiller{opts: opts, fetcher: fetch.New(fo), memory: mem}
}

// Close releases the browser, if one was started.
//
// It waits for any speculative launch still in flight first. A browser that is
// half-started when the caller gives up is still a process, and the goroutine
// that would have adopted it is the only thing holding its handle.
func (d *Distiller) Close() {
	d.mu.Lock()
	d.closed = true
	d.mu.Unlock()

	// A launch that never finishes must not hold the process open.
	//
	// Close runs after the run's context has expired, so nothing here is
	// protected by it, and Wait on a WaitGroup has no deadline of its own. A
	// half-started browser is still a process and the goroutine holding its
	// handle is the only thing that can release it -- but waiting forever for
	// that goroutine turns a leaked process into a hung command, which is
	// worse. Wait for a bounded while, then proceed and let the operating
	// system reap what is left.
	if !waitTimeout(&d.warming, warmupDrainTimeout) {
		d.logf("a speculative browser launch had not finished after %v; "+
			"closing anyway and leaving it to be reaped", warmupDrainTimeout)
	}

	d.mu.Lock()
	b := d.browser
	d.browser = nil
	d.mu.Unlock()
	if b != nil {
		b.Close()
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

	tGuard := time.Now()
	if d.opts.Guard != nil {
		// One page, one budget. The guard is this distiller's own -- see New --
		// so clearing it here cannot disturb a page being fetched elsewhere,
		// and a distiller reused for a second page starts that page's redirect
		// chain from zero rather than from wherever the last one ended.
		d.opts.Guard.Reset()
		if err := d.opts.Guard.Check(u); err != nil {
			return nil, err
		}
	}
	timing["guard"] = time.Since(tGuard)

	// What the ladder expects before it has looked: the memory's verdict on this
	// domain, raised by anything the caller has already insisted on.
	expected := d.memory.Predicted(u.Hostname())
	if d.opts.MinTier.Rank() > expected.Rank() {
		expected = d.opts.MinTier
	}
	if d.opts.MaxTier != "" && expected.Rank() > d.opts.MaxTier.Rank() {
		expected = d.opts.MaxTier
	}

	// The browser process starts here, before robots.txt is consulted.
	//
	// Launching Chromium contacts nobody -- it is a local process start, and it
	// navigates nowhere until it is told to. So it can overlap the one part of
	// the run that must come first and cannot be hurried: asking the site
	// whether we may read it at all. That question costs a round trip to a cold
	// host, better than a second on most of this corpus, and it was a second
	// spent doing nothing else.
	var warm *warmup
	var spec *speculation
	if expected.Rank() >= escalate.TierRender.Rank() {
		warm = d.prewarm(ctx)
	}

	// robots.txt is consulted before anything is fetched, not after. Asking
	// permission after taking the page is not asking permission.
	//
	// It is also, on a slow host, one of the largest single costs in the run:
	// igloo.inc spent four seconds of a ten-second budget here, before a byte of
	// the page had been requested. So the wait is bounded. A site that cannot
	// answer within its share of the budget has not refused -- it has not
	// answered -- and the distinction is recorded in the artifact rather than
	// resolved silently in either direction.
	var robotsNote string
	var blockedAtFetch string
	if d.opts.Robots != nil {
		rctx, rcancel := context.WithTimeout(ctx, d.robotsBudget())
		err := d.opts.Robots.Allowed(rctx, u)
		switch {
		case err == nil:
			if r, rerr := d.opts.Robots.Get(rctx, u); rerr == nil && r.CrawlDelay > 0 && d.opts.Limiter != nil {
				d.opts.Limiter.SetDelay(u.Hostname(), r.CrawlDelay)
			}
		case errors.Is(err, safety.ErrBlocked):
			// An answer. It is a refusal, and it ends the run.
			rcancel()
			return nil, err
		case rctx.Err() != nil:
			// No answer inside the budget. Proceeding is the same thing every
			// crawler does when robots.txt does not load, and saying so is what
			// keeps it honest.
			robotsNote = "this site's robots.txt did not answer within the time allowed, so it could " +
				"not be consulted; this artifact was produced without it and may cover paths the site " +
				"would have excluded"
			d.logf("robots.txt did not answer within %v; proceeding and declaring it", d.robotsBudget())
		default:
			rcancel()
			return nil, err
		}
		rcancel()
	}
	timing["robots"] = time.Since(tGuard) - timing["guard"]

	if d.opts.Limiter != nil {
		release, lerr := d.opts.Limiter.Acquire(ctx, u)
		if lerr != nil {
			return nil, lerr
		}
		defer release()
	}

	// The browser starts warming while tier 0 is still in flight.
	//
	// Process startup is 200-600ms and it used to sit squarely on the critical
	// path: the fetch finished, the score was computed, and only then did a
	// Chromium boot begin -- despite the fact that nothing about the boot
	// depends on anything the fetch returns. Starting it speculatively costs a
	// process on the pages that turn out not to need one and removes it from the
	// clock entirely on the pages that do.
	//
	// It is skipped where a browser is already known to be unnecessary or
	// forbidden: a warm browser held for a caller capped at tier 0 is a process
	// nobody will ever use.
	// The speculative render starts once robots.txt has allowed it, and not
	// before: navigating is fetching, and asking permission afterwards is not
	// asking permission.
	if expected.Rank() >= escalate.TierRender.Rank() {
		d.logf("a browser is expected for this page (%q); starting the render alongside the fetch", expected)
		spec = d.speculate(ctx, rawURL, expected, warm)
		defer func() {
			if spec != nil {
				spec.cancel()
			}
		}()
	}

	// --- Tier 0 -------------------------------------------------------------
	//
	// Tier 0 gets a share of the budget, not all of it. It is an optimisation:
	// it answers cheaply when it can and tells the ladder how hard the page is,
	// and a host that will not answer quickly has already failed at both jobs.
	// Left unbounded it spent the entire ten seconds on hatom.com waiting for a
	// connection that never came, and the browser -- which has its own network
	// stack and routinely reaches hosts this client cannot -- never got a turn.
	t0 := time.Now()
	fetchCtx, fetchCancel := context.WithTimeout(ctx, d.fetchBudget())
	resp, err := d.fetcher.Get(fetchCtx, rawURL, nil)
	fetchCancel()

	// A tier-0 network failure is not the end of the run.
	//
	// Tier 0 is an optimisation: it exists to answer cheaply when it can and to
	// tell the ladder how hard the page is. The browser has an entirely separate
	// network stack -- its own TLS, its own HTTP/2, its own connection reuse --
	// and routinely reaches a host that a bare Go client cannot, which is
	// precisely the case observed on hatom.com, where the fetch died in the TLS
	// handshake. Aborting there means the tool refuses a page it is fully
	// capable of reading, and it does so at the one stage that was only ever
	// meant to save time.
	//
	// A policy refusal is different in kind and still ends the run: that is a
	// decision, not a failure, and the browser must not be used to go around it.
	var fetchFailure string
	if err != nil {
		if errors.Is(err, safety.ErrBlocked) || d.opts.MaxTier == escalate.TierFetch {
			return nil, err
		}
		d.logf("tier 0 fetch failed (%v); handing the page to the browser", err)
		fetchFailure = err.Error()
		resp = &fetch.Response{FinalURL: rawURL, ContentType: "text/html"}
	}
	timing["fetch"] = time.Since(t0)
	if fetchFailure == "" {
		d.progress(Progress{Stage: "fetch", Tier: escalate.TierFetch,
			Message: fmt.Sprintf("HTTP %d, %d bytes", resp.Status, len(resp.Body)), Elapsed: time.Since(start)})
	} else {
		d.progress(Progress{Stage: "fetch", Tier: escalate.TierFetch,
			Message: "failed, escalating to the browser", Elapsed: time.Since(start)})
	}

	// A response that is not HTML is a reason to let the browser try, not a
	// reason to stop.
	//
	// patagonia.com answers this client with a bare 404 and an empty body while
	// serving the site perfectly well to a browser. Whatever the merits of that,
	// the fact sieve has is "the cheap path did not return a document", and it
	// already knows what to do with that: the browser has its own TLS, its own
	// HTTP/2 and its own fingerprint, and it routinely gets a page this client
	// cannot. Ending the run here refuses a page sieve is capable of reading, at
	// the one stage that exists only to save time.
	//
	// A genuine non-HTML resource -- a PDF, an image -- simply yields nothing in
	// the browser either, and the artifact says so.
	if fetchFailure == "" && !resp.IsHTML() && !resp.Blocked {
		if d.opts.MaxTier == escalate.TierFetch {
			return nil, fmt.Errorf("%s served %q, which is not an HTML document",
				resp.FinalURL, resp.ContentType)
		}
		d.logf("tier 0 received %q (HTTP %d), which is not a document; handing the page to the browser",
			resp.ContentType, resp.Status)
		fetchFailure = fmt.Sprintf("the server answered this client with HTTP %d and no HTML document",
			resp.Status)
		resp.Body = nil
	}

	staticRes, err := static.Extract(resp.FinalURL, strings.NewReader(string(resp.Body)), len(resp.Body))
	if err != nil {
		return nil, fmt.Errorf("static extraction: %w", err)
	}
	timing["static"] = time.Since(t0)

	// --- Decide -------------------------------------------------------------
	decision := escalate.Score(staticRes.Signals, 0, "", d.opts.Thresholds)
	// The memory records what a page needed, never what the network did.
	//
	// A failed fetch leaves zero bytes to score, and zero bytes score as the
	// heaviest page on the web: no text, no structure, nothing served. Feeding
	// that to the memory pins the domain to a browser for good on the strength
	// of one dropped connection -- which is exactly what happened to a static
	// site that answers at tier 0 in under a second, after a single timeout.
	// Hysteresis only ratchets upward, so there is no way back from a wrong
	// entry, which makes writing one on no evidence particularly expensive.
	if fetchFailure == "" {
		decision = d.memory.Apply(u.Hostname(), decision)
	}
	decision = clampTier(decision, d.opts.MinTier, d.opts.MaxTier)
	if fetchFailure != "" {
		// The score was computed from an empty document, so it describes nothing.
		// The browser is the only remaining source, and saying so is more honest
		// than a number derived from zero bytes.
		decision = clampTier(escalate.Decision{
			Tier:   escalate.TierSweep,
			Reason: "the direct fetch failed, so the served HTML could not be scored; the browser is the only available source",
		}, escalate.TierRender, d.opts.MaxTier)
	}
	d.logf("tier=%s score=%.3f pinned=%v — %s",
		decision.Tier, decision.Score, decision.Pinned, decision.Reason)

	// Freshness is a statement about the served bytes. With no bytes there is
	// nothing to be fresh against, and a hash of the empty string would compare
	// equal on the next run and report a changed page as unchanged.
	var freshness fetch.Freshness
	if fetchFailure == "" {
		freshness = fetch.Freshness{
			ETag:         resp.ETag,
			LastModified: resp.LastModified,
			RawHash:      fetch.HashBytes(resp.Body),
			TextHash:     fetch.StaticTextHash(staticRes),
			CheckedAt:    time.Now().UTC(),
		}
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
	// A refusal of *this* client is not necessarily a refusal of the tool.
	//
	// moma.org, ox.ac.uk and vanguardngr.com all answer the Go client with 403
	// on their apex host while serving the identical user agent 200 on their www
	// host, reproducibly. Freezing at tier 0 there emits the CDN's error page as
	// the artifact -- zero blocks for a site that was refusing nothing -- and it
	// does not honour anything, because the site never expressed a preference
	// that a second client would violate.
	//
	// So the browser is allowed one attempt. It is not a disguise: it carries the
	// same identifying user agent and contact URL, obeys the same robots.txt, and
	// is the tool's other ordinary client. If it is refused as well, that is two
	// independent refusals and the artifact says so and stops. Nothing here
	// retries a refusal, spoofs a fingerprint, or asks twice with the same
	// client.
	if resp.Blocked && decision.Tier.Rank() >= escalate.TierRender.Rank() {
		d.logf("tier 0 was refused (%s); the browser is a different client and gets one attempt",
			resp.BlockedReason)
		blockedAtFetch = resp.BlockedReason
		resp.Blocked = false
		prov.Blocked = false
		prov.BlockedReason = ""
	}

	if resp.Blocked {
		d.logf("site refused this client (%s); staying at tier 0 and labelling the artifact", resp.BlockedReason)
		prov.Tier = string(escalate.TierFetch)
		g, gerr := d.buildGraph(rawURL, resp, staticRes.Merged, staticRes, prov,
			[]string{"the site refused this client (" + resp.BlockedReason + "); this artifact was built from the served HTML alone and is likely incomplete"},
			false, "")
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
		var n0 []string
		if robotsNote != "" {
			n0 = append(n0, robotsNote)
		}
		g, gerr := d.buildGraph(rawURL, resp, staticRes.Merged, staticRes, prov, n0, true, "")
		if gerr != nil {
			return nil, gerr
		}
		// Tier 0 adopts too, on the evidence it has.
		//
		// No browser has run here, so the only witness is the page's own
		// structured data -- but that is a good witness, and a site that
		// publishes its FAQ as schema.org while hiding the rendered copy until
		// it scrolls into view can be read completely without a browser at all.
		d.adoptServed(g, staticRes)
		timing["total"] = time.Since(start)
		d.progress(Progress{Stage: "done", Tier: escalate.TierFetch, Elapsed: time.Since(start)})
		return &Result{Graph: g, Freshness: freshness, Decision: decision, Timing: timing}, nil
	}

	// --- Tiers 1-3 ----------------------------------------------------------
	if err := d.ensureBrowser(ctx, warm); err != nil {
		if fetchFailure != "" {
			// Both sources are gone: the fetch failed and there is no browser to
			// take over. Falling back to tier 0 here would emit an artifact built
			// from zero bytes, which says the page is empty when the truth is
			// that it was never read.
			return nil, fmt.Errorf("fetch failed (%s) and no browser was available to retry: %w", fetchFailure, err)
		}
		// No browser available. Rather than failing, fall back to what tier 0
		// produced and say plainly that the artifact is the cheap version.
		d.logf("browser unavailable (%v); falling back to tier 0", err)
		prov.Tier = string(escalate.TierFetch)
		prov.TierReason = fmt.Sprintf("escalation to %q was chosen but no browser was available: %v", decision.Tier, err)
		prov.TierFellBack = true
		g, gerr := d.buildGraph(rawURL, resp, staticRes.Merged, staticRes, prov,
			[]string{"a browser was required for this page but none was available; this artifact was built from the served HTML alone"},
			false, "")
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
	res, serr, claimed := spec.claim(ctx, decision.Tier)
	if claimed {
		d.logf("the speculative render answered; its navigation and page boot cost nothing")
	} else {
		if serr != nil {
			d.logf("the speculative render did not complete (%v); rendering normally", serr)
		}
		d.mu.Lock()
		b := d.browser
		d.mu.Unlock()
		b.SetOptions(d.sweepOptions(decision.Tier))
		var err error
		res, err = b.Sweep(ctx, rawURL, d.guardFunc())
		if err != nil {
			// The browser could not reach a page that tier 0 already has.
			//
			// This happens for reasons that are entirely the network's: an
			// expired or mismatched certificate, a reset connection, an HTTP/2
			// protocol error. Four sites in a hundred hit one of them --
			// internetarchive.org presents a certificate for archive.org, muji
			// resets the connection, uniqlo breaks the HTTP/2 framing -- and
			// every one of them was returned to the caller as a failed run,
			// discarding served HTML that was sitting in memory and was
			// perfectly readable.
			//
			// A refusal still ends the run: that is a decision rather than a
			// failure, and the browser must not be used to go around it.
			// A status code is an answer, not a failure to answer.
			//
			// httpbin.org/status/401 sets fetchFailure -- the server sent no HTML
			// with it -- and the browser then fails too, with
			// ERR_INVALID_AUTH_CREDENTIALS. Both conditions being true returned
			// the run as an error, so the caller learned nothing except that
			// something went wrong, when what had actually happened was
			// completely determined and worth saying: the page is behind a login.
			// A 403 escaped this only because it trips the refusal detector
			// earlier and never sets fetchFailure, which made the handling of two
			// almost identical cases completely different.
			//
			// So a known error status produces an artifact describing it. A
			// robots refusal still ends the run: that is a decision not to read
			// the page, and the browser must not be used to go around it.
			if errors.Is(err, safety.ErrBlocked) || (fetchFailure != "" && resp.Status < 400) {
				return nil, err
			}
			d.logf("the browser could not load this page (%v); falling back to the served HTML", err)
			bprov := prov
			bprov.Tier = string(escalate.TierFetch)
			bprov.TierReason = fmt.Sprintf("escalated to %q, but the browser could not load the page "+
				"(%v), so the served HTML was used instead", decision.Tier, err)
			bprov.TierFellBack = true
			bg, berr := d.buildGraph(rawURL, resp, staticRes.Merged, staticRes, bprov,
				append(notesFor(robotsNote),
					"the browser was unable to load this page ("+err.Error()+"); this artifact was "+
						"built from the served HTML alone, so anything this page renders with "+
						"JavaScript is absent from it"),
				false, "")
			if berr != nil {
				return nil, err
			}
			d.adoptServed(bg, staticRes)
			decision.Tier = escalate.TierFetch
			decision.Reason = bprov.TierReason
			timing["render"] = time.Since(tr)
			timing["total"] = time.Since(start)
			d.progress(Progress{Stage: "done", Tier: decision.Tier, Elapsed: time.Since(start)})
			return &Result{
				Graph: bg, Freshness: freshness, Decision: decision, Timing: timing,
				Capture: staticRes.Merged, StaticHTML: staticRes.RawHTML,
				Status: int64(resp.Status),
			}, nil
		}
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
		if fetchFailure == "" {
			d.memory.Note(u.Hostname(), post.Tier)
		}
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
	if blockedAtFetch != "" {
		if res.Blocked {
			notes = append(notes, "both the direct fetch and the browser were refused by this site ("+
				blockedAtFetch+"); this artifact is whatever the refusal page contained")
		} else {
			notes = append(notes, "the direct fetch of this page was refused ("+blockedAtFetch+
				"), but the browser was served normally; this artifact rests on the rendered page")
		}
	}
	if robotsNote != "" {
		notes = append(notes, robotsNote)
	}
	for _, n := range render.LibraryNotes(res.Libraries) {
		notes = append(notes, n)
	}
	// Say what was folded away and had to be opened. A reader weighing a claim
	// should know whether it was on screen or behind a tab, and the gap list
	// only ever named what stayed shut.
	if n := len(res.OpenedDisclosures); n > 0 {
		shown := res.OpenedDisclosures
		if len(shown) > 6 {
			shown = shown[:6]
		}
		notes = append(notes, fmt.Sprintf("%d disclosure control(s) on this page were "+
			"opened to read what they hold, including %s; each reveals content the page "+
			"already carried and none submits anything",
			n, strconv.Quote(strings.Join(shown, ", "))))
	}
	if res.EntryGate != "" && res.EnteredGate == "" {
		notes = append(notes, "this site is behind an entry screen labelled "+
			strconv.Quote(res.EntryGate)+"; the page past it never loaded, so this artifact "+
			"describes the interstitial and not the site")
	}
	if fetchFailure != "" {
		notes = append(notes, "the direct HTTP fetch of this page failed ("+fetchFailure+
			"); the browser reached it instead, so this artifact rests on the rendered page alone "+
			"and the served-HTML comparison is unavailable")
	}
	// What the browser was actually served.
	//
	// The status was collected and never looked at, so a page the server
	// answered with an error was distilled as though it were the page asked
	// for: patagonia.com returns 404 to this client at both tiers, and the
	// artifact was one block reading "Not found" with a full-marks retention
	// score and nothing to say that the site had refused. An artifact that
	// describes an error page must say it is describing an error page --
	// otherwise an agent reads "Not found" as the content of patagonia.com.
	if res.Status >= 400 {
		prov.Blocked = true
		if prov.BlockedReason == "" {
			prov.BlockedReason = fmt.Sprintf("HTTP %d", res.Status)
		}
		notes = append(notes, fmt.Sprintf("the browser was served HTTP %d for this URL, so what "+
			"follows is the error page the site returned and not the page that was asked for",
			res.Status))
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
	g, err := d.buildGraph(rawURL, resp, res.Merged, staticRes, prov, notes, res.ReachedBottom, res.EntryGate)
	if err != nil {
		return nil, err
	}
	appendRecoveries(g, recovered)
	appendSceneRuns(g, res.Scene)

	// The served HTML is not thrown away because a browser ran.
	//
	// On a page that mounts its sections as the viewport reaches them, the two
	// tiers hold different halves of the site: the render has what loads early,
	// the served bytes have what the sweep never travelled far enough to reach.
	// Keeping only one of them was losing content sieve had already fetched.
	if fetchFailure == "" {
		d.adoptServed(g, staticRes)
	}

	// Working harder must never return less.
	//
	// Escalation assumes the browser sees at least what the served HTML did.
	// That holds until a page cannot be driven: organimo.com cancels native
	// scrolling and ignores synthetic wheel events, so the sweep captures its
	// whole document and finds ninety per cent of it still at zero opacity,
	// waiting for a gesture that never lands. The rendered artifact is four
	// blocks. The served HTML has forty-six -- imperfect, because the page
	// splits its words across elements, but overwhelmingly more of the page.
	//
	// Shipping the smaller one because it came from the more expensive tier is
	// indefensible, so the two are compared and the fuller one wins. The
	// substitution is not silent: the tier is corrected to what actually
	// answered, and the artifact says why.
	//
	// This costs no safety. The fallback is the ordinary tier-0 artifact, built
	// by the same graph pipeline under the same visibility rules, and tier 0
	// answering is already a supported outcome for most of the web.
	// A render that produced nothing is not a render.
	//
	// Every stage of the browser path is bounded, and on a bad second all of
	// them can expire in turn, leaving an artifact that says the site is empty
	// about a site whose HTML is sitting in memory a few lines away. The static
	// extraction is always available and always costs nothing at this point, so
	// it is the floor: escalating can leave the artifact unchanged, but it can
	// never leave it worse than not having escalated.
	if fetchFailure == "" && g.Stats.ContentNodes == 0 {
		fprov := prov
		fprov.Tier = string(escalate.TierFetch)
		fprov.TierReason = fmt.Sprintf("escalated to %q, but the render produced no content at all "+
			"within its budget, so the served HTML was used instead", decision.Tier)
		fprov.TierFellBack = true
		if fg, ferr := d.buildGraph(rawURL, resp, staticRes.Merged, staticRes, fprov,
			append(notes, "the browser was given this page and returned nothing readable inside its "+
				"time budget; this artifact was built from the served HTML instead"),
			false, res.EntryGate); ferr == nil && fg.Stats.ContentNodes > 0 {
			d.adoptServed(fg, staticRes)
			d.logf("the render came back empty; falling back to the served HTML (%d block(s))",
				fg.Stats.ContentNodes)
			g = fg
			decision.Tier = escalate.TierFetch
			decision.Reason = fprov.TierReason
		}
	}

	if fetchFailure == "" && shouldPreferStatic(g, staticRes) {
		d.logf("the rendered sweep yielded less text than the served HTML; falling back to tier 0")
		sprov := prov
		sprov.Tier = string(escalate.TierFetch)
		sprov.TierReason = fmt.Sprintf("escalated to %q, but the browser could not drive this page: "+
			"the rendered capture yielded less readable text than the served HTML, so the tier-0 "+
			"extraction was used instead", decision.Tier)
		sprov.TierFellBack = true
		sg, serr := d.buildGraph(rawURL, resp, staticRes.Merged, staticRes, sprov,
			append(notes, "this page was rendered in a browser, but scrolling it revealed almost nothing: "+
				"most of its text stayed at zero opacity. The artifact below was built from the served HTML "+
				"instead, which contains more of the page. Text this page splits across elements for animation "+
				"may appear fragmented, because reassembling it needs the rendered layout that could not be obtained"),
			false, res.EntryGate)
		if serr != nil {
			return nil, serr
		}
		g = sg
		decision.Tier = escalate.TierFetch
		decision.Reason = sprov.TierReason
	}

	timing["total"] = time.Since(start)
	d.progress(Progress{Stage: "done", Tier: decision.Tier, Elapsed: time.Since(start), Partial: g})
	return &Result{
		Graph: g, Freshness: freshness, Decision: decision, Timing: timing,
		Capture: res.Merged, StaticHTML: staticRes.RawHTML,
		Scene: res.Scene, Libraries: res.Libraries, Status: res.Status,
	}, nil
}

// staticAdvantage is how much more text the served HTML must carry before it
// displaces a rendered capture.
//
// The margin is wide on purpose. Rendering legitimately drops text that the
// served HTML contains -- that is the whole point of judging by what a browser
// showed rather than by what a document said -- so a rendered artifact being
// somewhat smaller is the system working. Only a collapse, the browser
// returning a small fraction of what was served, means the page was never
// driven at all.
const staticAdvantage = 3.0

// minStaticFallbackChars stops the comparison firing on pages with nothing to
// compare. Three times almost nothing is still almost nothing.
const minStaticFallbackChars = 400

func shouldPreferStatic(g *graph.Graph, staticRes *static.Result) bool {
	staticChars := staticRes.Signals.TextChars
	if staticChars < minStaticFallbackChars {
		return false
	}
	rendered := 0
	for _, b := range g.ContentBlocks() {
		rendered += utf8.RuneCountInString(b.Text)
	}
	return float64(staticChars) > float64(rendered)*staticAdvantage
}

func (d *Distiller) buildGraph(rawURL string, resp *fetch.Response, merged *capture.Merged,
	staticRes *static.Result, prov graph.Provenance, notes []string, reachedBottom bool,
	entryGate string) (*graph.Graph, error) {

	// Every return path in Distill funnels through here, which is why the
	// outcome is assembled here rather than at each of them. Deciding it at the
	// call sites is how the fetch path came to report a 403 as an empty page:
	// the render path marked HTTP errors and the tier-0 path returned before
	// reaching that code, so the same condition was labelled in one place and
	// silently dropped in the other.
	out := graph.OutcomeInput{
		HTTPStatus:     resp.Status,
		Blocked:        prov.Blocked,
		BlockedReason:  prov.BlockedReason,
		EntryGate:      entryGate,
		Rendered:       prov.Tier != string(escalate.TierFetch),
		ShellHTML:      staticRes.Signals.IsShell(),
		SweepTruncated: !reachedBottom,
		TierFellBack:   prov.TierFellBack,
		TierReason:     prov.TierReason,
	}
	// The body is only carried on an error, where it is the one place a proxy
	// or policy filter explains itself.
	if resp.Status >= 400 {
		out.Body = string(resp.Body)
	}

	return graph.Build(graph.Input{
		RequestedURL:  rawURL,
		FinalURL:      resp.FinalURL,
		Merged:        merged,
		Notes:         notes,
		OriginalBytes: int64(len(resp.Body)),
		OriginalText:  staticRes.RawHTML,
		ReachedBottom: reachedBottom,
		EntryGate:     entryGate,
		Now:           d.opts.Now,
		Generator:     d.opts.Generator,
		Provenance:    prov,
		Outcome:       out,
	})
}

// warmup is a speculative browser launch that more than one caller may wait on.
type warmup struct {
	ch   <-chan error
	once sync.Once
	err  error
}

// wait blocks until the launch has finished. It is safe to call repeatedly and
// from more than one goroutine, which matters because both the speculative
// sweep and the ordinary path need the same browser.
func (w *warmup) wait(ctx context.Context) error {
	if w == nil {
		return nil
	}
	w.once.Do(func() {
		select {
		case err := <-w.ch:
			w.err = err
		case <-ctx.Done():
			w.err = ctx.Err()
		}
	})
	return w.err
}

// speculation is a render started before the page has been scored.
type speculation struct {
	tier   escalate.Tier
	res    *render.Result
	err    error
	done   chan struct{}
	cancel context.CancelFunc
}

// speculate starts rendering while tier 0 is still in flight.
//
// The browser's navigation and the page's own boot are the two largest fixed
// costs in an escalated run, and neither of them depends on anything the fetch
// returns. On pear.no they are six seconds that the ladder spends waiting
// before it has permission to start waiting.
//
// It runs only where the escalation memory already says this domain needs a
// browser. That restraint is the whole of the ethics here: a speculative render
// of a page that turns out to be answerable at tier 0 is a page load nobody
// asked for, and doing that to every site on the chance it might be heavy is
// exactly the always-render behaviour the ladder exists to avoid. Waiting for
// the memory to say so costs the first visit to a heavy site and nothing after
// it -- which is the same bargain the hysteresis already makes.
func (d *Distiller) speculate(ctx context.Context, rawURL string, tier escalate.Tier, warm *warmup) *speculation {
	sctx, cancel := context.WithCancel(ctx)
	s := &speculation{tier: tier, done: make(chan struct{}), cancel: cancel}
	go func() {
		defer close(s.done)
		if err := d.ensureBrowser(sctx, warm); err != nil {
			s.err = err
			return
		}
		d.mu.Lock()
		b := d.browser
		d.mu.Unlock()
		if b == nil {
			s.err = errors.New("no browser")
			return
		}
		b.SetOptions(d.sweepOptions(tier))
		s.res, s.err = b.Sweep(sctx, rawURL, d.guardFunc())
	}()
	return s
}

// claim takes the speculative render if it was started for the tier that was
// actually chosen, and abandons it otherwise.
func (s *speculation) claim(ctx context.Context, tier escalate.Tier) (*render.Result, error, bool) {
	if s == nil {
		return nil, nil, false
	}
	if s.tier != tier {
		s.cancel()
		return nil, nil, false
	}
	// Bounded by the caller's context, not by hope.
	//
	// This was a bare receive on a channel closed by a goroutine running
	// Sweep. Sweep takes a context and honours it everywhere it looks, but
	// "everywhere it looks" is the whole assumption: a CDP call that never
	// returns, or a browser that never finishes starting, leaves the goroutine
	// alive, the channel open, and this line waiting for as long as the process
	// is allowed to live. One pear.no run sat here for twenty minutes against a
	// thirty-eight second deadline.
	//
	// Cancelling the speculation on the way out matters as much as returning:
	// abandoning it without cancelling leaves a browser running for a render
	// nobody will read.
	select {
	case <-s.done:
	case <-ctx.Done():
		s.cancel()
		return nil, ctx.Err(), false
	}
	if s.err != nil {
		// A speculative render that failed says nothing about whether an
		// ordinary one would: it may simply have been cancelled. The caller
		// falls back to doing the work itself.
		return nil, s.err, false
	}
	return s.res, nil, true
}

// fetchBudget is how long tier 0 gets before the browser takes over.
//
// It follows the load allowance, not the reading budget. Tier 0 is a page load
// like any other -- the same bytes over the same network as the browser would
// pull -- so pricing it against the time set aside for *reading* a page was
// simply the wrong comparison, and it was miserly: two-fifths of the reading
// budget is a few seconds, and a large marketing site behind a slow first byte
// exceeds that routinely.
//
// The cost of getting this wrong is not a slow tier 0, it is no tier 0 at all.
// A fetch that times out is recorded as a failure, which forbids the static
// fallback later, so a page whose HTML was perfectly readable ends up resting
// entirely on a render that may itself run out of time. Four documentation and
// marketing sites in a hundred-site sweep -- react.dev, nuxt.com, posthog.com
// and womp.com -- came back completely empty that way, each having spent the
// whole budget failing twice at something one of the two would have done.
func (d *Distiller) fetchBudget() time.Duration {
	b := d.opts.Render.LoadBudget / 3
	if b < 5*time.Second {
		b = 5 * time.Second
	}
	if b > 8*time.Second {
		// Tier 0 is the cheap path. A host that has not answered a plain GET in
		// eight seconds is one the browser is going to have to handle anyway,
		// and every further second spent waiting is taken from the stage that
		// will actually produce the artifact.
		b = 8 * time.Second
	}
	if to := d.opts.Fetch.Timeout; to > 0 && to < b {
		b = to
	}
	return b
}

// robotsBudget is how long the run will wait for robots.txt. It follows the
// page budget so that a caller who asked for more time gives the site more time
// to answer.
func (d *Distiller) robotsBudget() time.Duration {
	b := d.opts.Render.Budget / 5
	if b < 1500*time.Millisecond {
		b = 1500 * time.Millisecond
	}
	if b > 10*time.Second {
		b = 10 * time.Second
	}
	return b
}

// notesFor turns an optional robots note into a note slice.
func notesFor(robotsNote string) []string {
	if robotsNote == "" {
		return nil
	}
	return []string{robotsNote}
}

// adoptServed folds corroborated served-HTML text into a graph and records what
// it did. See graph.AdoptServedText for the rule and the reasoning.
func (d *Distiller) adoptServed(g *graph.Graph, staticRes *static.Result) {
	if g == nil || staticRes == nil || staticRes.Merged == nil {
		return
	}
	n, proof := graph.AdoptServedText(g, staticRes.Merged.Latent)
	if n == 0 {
		return
	}
	d.logf("adopted %d run(s) from the served HTML on %d corroborating fragment(s)", n, proof)
	g.Audit.Notes = append(g.Audit.Notes, fmt.Sprintf(
		"%d run(s) of text below were served in this page's HTML inside sections it keeps hidden "+
			"until they scroll into view, and sieve did not watch them appear. They are included "+
			"because %d sentence-length fragments of that same hidden text were independently "+
			"witnessed on this page -- on screen during the render, or published by the site as "+
			"its own structured data -- so on this page that marking is a reveal state rather than "+
			"concealment. Each one is marked speculative and flagged.", n, proof))
}

// sweepOptions is the render configuration for one tier.
func (d *Distiller) sweepOptions(tier escalate.Tier) render.Options {
	o := d.opts.Render
	if tier == escalate.TierRender {
		// A capture after settle, and no second pass: enough for a
		// client-rendered page whose content is all present once JavaScript has
		// run.
		//
		// This used to pin MaxCheckpoints to one, which is a different and worse
		// promise. A page is not one screen tall because the served HTML looked
		// sparse, and techsibiti.com was stopped one checkpoint into a document
		// it had seven unspent seconds to finish. Requiring only a single stable
		// checkpoint ends the loop just as quickly on a page that really is one
		// screen, and lets it continue on a page that is not.
		o.StableCheckpoints = 1
		o.Passes = 1
	}
	// A canvas screenshot is only ever read by OCR or by a vision model. With
	// neither configured the rasterisation is decoded, cropped, re-encoded and
	// dropped, so it does not happen.
	o.CaptureCanvas = o.CaptureCanvas &&
		(d.opts.Canvas.EnableVision || d.opts.Canvas.OCR != nil)
	return o
}

// prewarm starts a browser in the background. A nil return means no browser was
// started.
// prewarm is skipped unless a browser is actually expected.
//
// Warming one unconditionally made every tier-0 page pay for a Chromium it never
// touched: process startup on the way in and the better part of a second of
// teardown on the way out, on a page an HTTP GET answered in under a second.
// That is the ladder's entire advantage handed back on the majority of the web.
func (d *Distiller) prewarm(ctx context.Context) *warmup {
	if d.browser != nil || d.opts.MaxTier == escalate.TierFetch {
		return nil
	}
	ch := make(chan error, 1)
	w := &warmup{ch: ch}
	d.warming.Add(1)
	go func() {
		defer d.warming.Done()
		// The launch is deliberately not tied to the caller's context. A
		// cancellation mid-boot would abandon a process that is already running,
		// and the whole point of the handle is to be able to close it.
		b, err := render.Launch(context.WithoutCancel(ctx), d.opts.Render)
		if err != nil {
			ch <- err
			return
		}
		d.mu.Lock()
		if d.closed || d.browser != nil {
			// Either the caller gave up while this was starting, or something
			// else got there first. Either way this process has no owner.
			d.mu.Unlock()
			b.Close()
			ch <- context.Canceled
			return
		}
		d.browser = b
		d.mu.Unlock()
		ch <- nil
	}()
	return w
}

func (d *Distiller) ensureBrowser(ctx context.Context, warm *warmup) error {
	// The launch may already be running. Waiting on it is not a delay: it is the
	// part of the boot that overlapped the fetch being cashed in.
	if err := warm.wait(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	d.mu.Lock()
	have := d.browser != nil
	d.mu.Unlock()
	if have {
		return nil
	}
	b, err := render.Launch(ctx, d.opts.Render)
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.browser = b
	d.mu.Unlock()
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
// appendSceneRuns adds the text a page drew into a 3D scene.
//
// Some sites put their whole body copy on a WebGL surface. igloo.inc serves an
// empty <body>, builds every paragraph as MSDF glyph geometry, and never
// attaches a canvas element to the document -- so every DOM-shaped question
// sieve can ask returns nothing, and the artifact said the page had no words
// on it while a reader could see several hundred.
//
// These are not a guess about pixels. They are the strings the site handed its
// text renderer, read back off the objects it built, which makes them exactly
// as exact as text read out of the DOM and is why they need no corroboration.
// What they lack is layout: a scene has no line boxes, so the order is the
// order the scene was assembled in, and the artifact says so.
//
// Anything the DOM already yielded is skipped. A page that draws a headline in
// 3D and also writes it into the document should not have it twice.
func appendSceneRuns(g *graph.Graph, sc *capture.SceneIntrospection) {
	if sc == nil || len(sc.Runs) == 0 {
		return
	}
	have := make(map[string]bool, len(g.Blocks))
	for _, b := range g.Blocks {
		if k := sceneKey(b.Text); k != "" {
			have[k] = true
		}
	}

	added, sceneChars := 0, 0
	for _, r := range sc.Runs {
		k := sceneKey(r.Text)
		if k == "" || have[k] {
			continue
		}
		have[k] = true
		sceneChars += utf8.RuneCountInString(r.Text)
		g.Blocks = append(g.Blocks, graph.Block{
			ID:         fmt.Sprintf("b_s%02d", added),
			Type:       graph.TypeParagraph,
			Text:       r.Text,
			Order:      len(g.Blocks),
			Source:     graph.SourceCanvasScene,
			Score:      sceneRunScore,
			Confidence: graph.Bucket(sceneRunScore),
			Verified:   graph.VerificationNone,
			Region:     graph.RegionMain,
		})
		added++
	}
	if added == 0 {
		return
	}

	g.Recount()

	// Scene text counts on both sides of the retention ratio.
	//
	// Retention asks how much of what the browser put on screen reached the
	// artifact, and its denominator is the characters the DOM walk saw. Glyph
	// geometry is on screen and is not in the DOM, so a page drawn entirely in
	// WebGL scored zero per cent while every word of it sat in the payload --
	// the audit calling a complete extraction a total loss.
	//
	// Adding the run to both sides says the honest thing: this text was on the
	// page, and it is in the artifact.
	g.Audit.ObservedChars += sceneChars
	g.Audit.EmittedChars += sceneChars
	if g.Audit.ObservedChars > 0 {
		r := float64(g.Audit.EmittedChars) / float64(g.Audit.ObservedChars)
		if r > 1 {
			r = 1
		}
		g.Audit.GraphRetention = graph.RoundTo(r, 0.001)
	}

	g.Audit.Notes = append(g.Audit.Notes, fmt.Sprintf(
		"%d run(s) of text on this page are drawn into a 3D scene rather than written "+
			"into the document; they were read back from the scene's own text objects, so "+
			"the words are exact, but a scene has no line boxes and their order is the "+
			"order the page assembled them in rather than a measured reading order", added))
}

// sceneRunScore is the confidence attached to text read off a scene object. It
// is high because the string is the site's own, and short of certain because
// where it sat on screen is not known.
const sceneRunScore = 0.85

// sceneKey normalises a run for the duplicate check.
func sceneKey(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

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
	g.Recount()
	g.Audit.CanvasesUnrecovered = unrecovered
}

// warmupDrainTimeout bounds how long Close waits for a speculative launch.
const warmupDrainTimeout = 5 * time.Second

// waitTimeout is sync.WaitGroup.Wait with a deadline. It reports whether the
// group finished.
func waitTimeout(wg *sync.WaitGroup, d time.Duration) bool {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
		return false
	}
}
