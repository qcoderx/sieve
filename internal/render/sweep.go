package render

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"github.com/qcoderx/sieve/internal/capture"
	"github.com/qcoderx/sieve/internal/corroborate"
)

// NavGuard vets a URL the browser is about to navigate to. It is called for the
// initial navigation and again for every redirect, because a URL that passed
// once says nothing about where it points after three hops.
//
// Returning a non-nil error aborts that navigation. The safety package supplies
// the real implementation; render only defines the shape so the two packages
// stay independent.
type NavGuard func(u *url.URL) error

// Result is everything one page yielded.
type Result struct {
	RequestedURL string
	FinalURL     string
	Status       int64
	Merged       *capture.Merged

	// Assets holds response bodies for scene-graph formats, kept so canvas
	// recovery can read them without a second fetch.
	Assets []Asset
	// CanvasShots maps a canvas node path to a PNG cropped to that canvas at
	// the checkpoint where it filled the most of the viewport.
	CanvasShots map[string]*CanvasShot
	// Corpus is the confirm-only membership index over the text the site
	// shipped. It is never a source of content; see the corroborate package.
	Corpus *corroborate.Index
	// Scene is what walking the live 3D scene graph produced, if anything.
	Scene *capture.SceneIntrospection
	// Libraries are the animation, scroll and 3D libraries detected in the page.
	Libraries []string
	// EntryGate names an interstitial the visitor must dismiss before the site
	// begins -- the "click to enter" screen. It is empty when there is none.
	//
	// It matters because the alternative is silence. hatom.com sits behind one,
	// and the artifact reported nine blocks with no indication that the page had
	// not started: indistinguishable, to a reader, from a site that simply has
	// nothing on it.
	EntryGate string

	// Trace is everything needed to reproduce this render.
	Trace Trace

	Timing Timing
	// ReachedBottom reports that the sweep saw the end of the document rather
	// than running out of budget on the way there.
	ReachedBottom bool
	// Notes records anything that limited the sweep, so the artifact can be
	// honest about its own coverage instead of silently reporting a partial
	// page as complete.
	Notes []string
	// Blocked records that the site refused this client, and how it was
	// detected. Partial and honest beats empty.
	Blocked       bool
	BlockedReason string
}

// Trace is the complete set of inputs that determined a render's output.
//
// A trace missing any of these is not replayable, and an artifact whose trace
// is not replayable cannot support a bug report: the maintainer would have to
// reproduce against the live site, on their own machine, with their own
// Chromium, which is precisely the situation that makes browser-dependent
// projects expensive to maintain.
type Trace struct {
	SieveVersion   string   `json:"sieve_version"`
	CaptureHash    string   `json:"capture_script_sha256"`
	Chromium       string   `json:"chromium"`
	UserAgent      string   `json:"user_agent"`
	ViewportW      int      `json:"viewport_w"`
	ViewportH      int      `json:"viewport_h"`
	DeviceScale    float64  `json:"device_scale"`
	Locale         string   `json:"locale"`
	Timezone       string   `json:"timezone"`
	ReducedMotion  bool     `json:"reduced_motion"`
	StepRatio      float64  `json:"step_ratio"`
	SettleFrames   int      `json:"settle_frames"`
	SettleTimeout  string   `json:"settle_timeout"`
	FirstSettle    string   `json:"first_settle"`
	SettleFloor    string   `json:"settle_floor"`
	SweepBudget    string   `json:"sweep_budget"`
	Passes         int      `json:"passes"`
	MaxCheckpoints int      `json:"max_checkpoints"`
	StableCheckpts int      `json:"stable_checkpoints"`
	Flags          []string `json:"flags"`
	BlockHostsHash string   `json:"block_hosts_sha256"`
}

// Asset is an intercepted response body.
type Asset struct {
	URL  string
	MIME string
	Body []byte
}

// CanvasShot is a rasterised canvas region awaiting recovery.
type CanvasShot struct {
	Path       string
	PNG        []byte
	Share      float64
	Checkpoint int
	// Uniform is set when the crop is a single flat colour, which means there
	// is nothing in it for a vision model to describe and the expensive path
	// should be skipped.
	Uniform bool
}

// Timing breaks the wall clock down so a slow site can be diagnosed without
// re-running with a profiler attached.
type Timing struct {
	Navigate    time.Duration `json:"navigate_ms"`
	FirstSettle time.Duration `json:"first_settle_ms"`
	Sweep       time.Duration `json:"sweep_ms"`
	Total       time.Duration `json:"total_ms"`
	Checkpoints int           `json:"checkpoints"`
	Passes      int           `json:"passes"`
	SettleWaits int           `json:"settle_waits"`
	SettleMiss  int           `json:"settle_timeouts"`
}

type settleResult struct {
	Settled bool `json:"settled"`
	MS      int  `json:"ms"`
}

// sweepConfig is what the in-page sweep is told. Every number here was
// previously a Go-side loop variable; moving the loop into the page moved the
// knobs with it, and they stay Options fields so a caller tunes one thing.
type sweepConfig struct {
	BudgetMS          int64   `json:"budgetMs"`
	MaxCheckpoints    int     `json:"maxCheckpoints"`
	StableCheckpoints int     `json:"stableCheckpoints"`
	SettleFrames      int     `json:"settleFrames"`
	SettleMaxMS       int64   `json:"settleMaxMs"`
	SettleMinMS       int64   `json:"settleMinMs"`
	NodeBudget        int     `json:"nodeBudget"`
	LatentBudget      int     `json:"latentBudget"`
	MaxScrollPx       float64 `json:"maxScrollPx"`
	StepRatio         float64 `json:"stepRatio"`
	Passes            int     `json:"passes"`
	ThrottleGL        bool    `json:"throttleGL"`
}

// sweepReply is one sweep's entire output: every checkpoint's contribution,
// already reduced in the page to what that checkpoint added.
type sweepReply struct {
	Snapshots      []capture.Snapshot `json:"snaps"`
	Notes          []string           `json:"notes"`
	Checkpoints    int                `json:"checkpoints"`
	Passes         int                `json:"passes"`
	ReachedBottom  bool               `json:"reachedBottom"`
	Mode           string             `json:"mode"`
	StopReason     string             `json:"stopReason"`
	Virtual        bool               `json:"virtual"`
	Refinements    int                `json:"refinements"`
	Throttled      int                `json:"throttledCanvases"`
	TargetedStops  int                `json:"targetedStops"`
	TargetsFound   int                `json:"targetsFound"`
	PausedVideos   int                `json:"pausedVideos"`
	PinnedChars    int                `json:"pinnedChars"`
	FreeChars      int                `json:"freeChars"`
	ScrollMS       int                `json:"scrollMs"`
	SettleWorstMS  int                `json:"settleWorstMs"`
	CaptureWorstMS int                `json:"captureWorstMs"`
	Step           int                `json:"step"`
	Partial        bool               `json:"partial"`
	CaptureMS      int                `json:"captureMs"`
	SettleMS       int                `json:"settleMs"`
	TotalMS        int                `json:"totalMs"`
}

// awaitPromise makes chromedp resolve the promise an expression returns instead
// of handing back a Promise object.
func awaitPromise(p *runtime.EvaluateParams) *runtime.EvaluateParams {
	return p.WithAwaitPromise(true)
}

// Sweep renders one page and returns the deduplicated capture.
//
// The shape of the work is: navigate, wait for the page to stop moving, then
// walk the viewport down the document taking a full extraction at each stop,
// folding each one into a running deduplicated set. It ends when the set stops
// growing, when the document ends, or when a budget runs out, whichever comes
// first.
func (b *Browser) Sweep(ctx context.Context, rawURL string, guard NavGuard) (*Result, error) {
	start := time.Now()
	res := &Result{RequestedURL: rawURL, CanvasShots: map[string]*CanvasShot{}}

	// The render budget bounds the stages; the caller's context is what may
	// tear the tab down.
	//
	// These were the same context, and that is why a sweep that overran by a
	// checkpoint lost every checkpoint it had taken: the budget expiring killed
	// the tab, and with it any chance of asking the page what it had seen. A
	// budget is a decision to stop working, not a reason to destroy the evidence.
	callerCtx := ctx
	if b.opts.Budget > 0 {
		// The render budget is measured from here, so on its own it can end
		// after the caller's deadline has already passed -- which is not a
		// budget at all. When tier 0 has spent four seconds failing, a render
		// that still believes it has its full allowance plans a first settle
		// and a sweep for time that does not exist, and the tab is destroyed
		// underneath it before either finishes. Every stage then reports a
		// deadline it was never going to meet, and the artifact is empty.
		limit := time.Now().Add(b.opts.Budget)
		if dl, ok := ctx.Deadline(); ok && dl.Before(limit) {
			limit = dl
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, limit)
		defer cancel()
	}
	// The page deadline is carried by hand from here on.
	//
	// Everything below runs on tabCtx, which is derived from the browser's base
	// context and deliberately has no deadline of its own -- cancelling it would
	// tear the tab down rather than stop one operation. That means a stage
	// asking "how long have I got?" of its own context gets no answer, and the
	// sweep, asking exactly that, silently planned for its full allowance no
	// matter how much of the page budget navigation had already spent.
	deadline, hasDeadline := ctx.Deadline()

	tabCtx, tabCancel := chromedp.NewContext(b.baseCtx)
	defer tabCancel()

	// Tie the tab to the caller's deadline without losing chromedp's target
	// bookkeeping, which lives on tabCtx's values.
	go func() {
		select {
		case <-callerCtx.Done():
			tabCancel()
		case <-tabCtx.Done():
		}
	}()

	// The first Run on a chromedp context allocates the target and starts its
	// message pump, whose lifetime is bound to the context passed in. Doing it
	// explicitly here means the per-phase timeout contexts below can expire
	// without taking the tab down with them.
	if err := chromedp.Run(tabCtx); err != nil {
		return nil, fmt.Errorf("open tab: %w", err)
	}

	col := newCollector(b.opts, res)
	col.attach(tabCtx, guard)

	vw, vh := int64(b.opts.ViewportW), int64(b.opts.ViewportH)
	if err := chromedp.Run(tabCtx,
		// A headless tab that is not the active one is never composited, and an
		// uncomposited tab starves requestAnimationFrame -- which in turn
		// starves IntersectionObserver, the mechanism nearly every
		// scroll-reveal animation is built on. Without this, a sweep of a
		// reveal-driven site would scroll past content that never becomes
		// visible, and would report the page as almost empty.
		chromedp.ActionFunc(func(c context.Context) error {
			if err := page.BringToFront().Do(c); err != nil {
				res.note("could not activate tab: " + err.Error())
			}
			return nil
		}),
		// Installed before any page script so the canvas context hook is in
		// place the first time the page calls getContext.
		chromedp.ActionFunc(func(c context.Context) error {
			_, err := page.AddScriptToEvaluateOnNewDocument(capture.Bootstrap).Do(c)
			return err
		}),
		// The extraction script goes in at document start too.
		//
		// It used to be evaluated after DOMContentLoaded, and that is the worst
		// possible moment: the main thread is then busy running the page's own
		// boot, so a ninety-kilobyte script has to queue behind it and be
		// compiled in the middle of it. On pear.no that single injection took
		// three seconds of a ten-second budget -- more than the settle it was
		// preparing for, and more than the sweep it was preparing.
		//
		// At document start it is compiled while there is nothing to compete
		// with, and it is idempotent, so a page that navigates itself gets it
		// again for free. It only defines functions; nothing runs until asked.
		chromedp.ActionFunc(func(c context.Context) error {
			_, err := page.AddScriptToEvaluateOnNewDocument(capture.Script).Do(c)
			return err
		}),
		page.Enable(),
		network.Enable(),
		network.SetCacheDisabled(false),
		network.SetExtraHTTPHeaders(network.Headers{
			"Accept-Language": b.opts.AcceptLanguage,
		}),
		emulation.SetUserAgentOverride(b.opts.UserAgent).
			WithAcceptLanguage(b.opts.AcceptLanguage),
		emulation.SetDeviceMetricsOverride(vw, vh, b.opts.DeviceScale, false),
		chromedp.ActionFunc(func(c context.Context) error {
			// Timezone is pinned rather than inherited. A site that renders
			// dates, opening hours or countdowns produces different text in
			// different zones, and an artifact that changes depending on where
			// the machine sits is not reproducible.
			if b.opts.Timezone != "" {
				if err := emulation.SetTimezoneOverride(b.opts.Timezone).Do(c); err != nil {
					res.note("could not pin timezone: " + err.Error())
				}
			}
			if b.opts.Locale != "" {
				if err := emulation.SetLocaleOverride().WithLocale(b.opts.Locale).Do(c); err != nil {
					res.note("could not pin locale: " + err.Error())
				}
			}
			// The reduced-motion pass exists to be compared against the normal
			// one: agreement between them is evidence the reveal machinery was
			// understood, and divergence is a flag worth raising.
			features := []*emulation.MediaFeature{
				{Name: "prefers-color-scheme", Value: "light"},
				{Name: "prefers-reduced-motion", Value: motionValue(b.opts.ReducedMotion)},
			}
			if err := emulation.SetEmulatedMedia().WithFeatures(features).Do(c); err != nil {
				res.note("could not pin media features: " + err.Error())
			}
			return nil
		}),
		chromedp.ActionFunc(func(c context.Context) error {
			if len(b.opts.BlockURLPatterns) == 0 {
				return nil
			}
			pats := make([]*network.BlockPattern, 0, len(b.opts.BlockURLPatterns))
			for _, p := range b.opts.BlockURLPatterns {
				pats = append(pats, &network.BlockPattern{URLPattern: p, Block: true})
			}
			// A malformed pattern must not sink the whole render.
			if err := network.SetBlockedURLs().WithURLPatterns(pats).Do(c); err != nil {
				res.note("url block patterns rejected by browser: " + err.Error())
			}
			return nil
		}),
		chromedp.ActionFunc(func(c context.Context) error {
			if guard == nil {
				return nil
			}
			// Interception is scoped to document loads only. Those are the
			// requests that can move the browser to a new origin -- the main
			// navigation, every redirect hop, and every iframe, all of which
			// Chromium reports as Document -- and they number in the single
			// digits per page, so the guard costs almost nothing. Pausing
			// every image and font would add a round trip to each of several
			// hundred requests for no security gain.
			return fetch.Enable().WithPatterns([]*fetch.RequestPattern{
				{URLPattern: "*", RequestStage: fetch.RequestStageRequest, ResourceType: network.ResourceTypeDocument},
			}).Do(c)
		}),
	); err != nil {
		return nil, fmt.Errorf("configure tab: %w", err)
	}

	b.opts.logf("tab configured after %v", time.Since(start).Round(time.Millisecond))

	navStart := time.Now()
	navCtx := tabCtx
	if b.opts.NavTimeout > 0 {
		var cancel context.CancelFunc
		navCtx, cancel = context.WithTimeout(tabCtx, b.opts.NavTimeout)
		defer cancel()
	}
	// The sweep waits for the document, not for the last image.
	//
	// chromedp.Navigate returns on the load event, which means every image,
	// font, video poster and third-party script has finished -- including the
	// ones nobody is waiting for and the ones that never arrive. On a
	// design-heavy site that is routinely several seconds after the document is
	// complete and interactive, and on pear.no it was most of the page's entire
	// budget spent before a single checkpoint had been taken.
	//
	// DOMContentLoaded is the honest signal that there is a page to extract
	// from. Anything still in flight after it either lands during the settle
	// wait, or is a straggler whose absence changes no text on the page.
	if err := chromedp.Run(navCtx, chromedp.ActionFunc(func(c context.Context) error {
		_, _, errText, _, nerr := page.Navigate(rawURL).Do(c)
		if nerr != nil {
			return nerr
		}
		if errText != "" {
			return fmt.Errorf("%s", errText)
		}
		return nil
	})); err != nil {
		if blocked := col.blockedErr(); blocked != nil {
			return nil, blocked
		}
		// A navigation command that outran its deadline has not necessarily
		// failed: Page.navigate returns when the navigation commits, and a slow
		// first byte pushes that past the allowance while the load proceeds
		// perfectly well behind it. Treating that as fatal turned a slow server
		// into no artifact at all -- and then spent the rest of the budget
		// starting a second render that had even less time than the first.
		//
		// So the deadline hands over to the readiness wait below, which is
		// bounded anyway and which asks the only question that matters: is there
		// a document here. A genuine failure -- an unresolvable host, a refused
		// connection -- comes back as an error text rather than a timeout, and
		// is still fatal.
		if !errors.Is(err, context.DeadlineExceeded) && navCtx.Err() == nil {
			return nil, fmt.Errorf("navigate %s: %w", rawURL, err)
		}
		res.note("the navigation had not committed when its share of the budget ran out; " +
			"whatever had arrived was swept")
	}

	res.Timing.Navigate = time.Since(navStart)
	b.opts.logf("navigation committed in %v", res.Timing.Navigate.Round(time.Millisecond))

	// Install the extraction script, disable native smooth scrolling, then wait
	// for the entrance animations to finish before the first capture. Capturing
	// a hero mid-fade records it at the opacity it happened to have, and every
	// downstream signal reads from that number.
	// Waiting for the document and waiting for it to stop moving are one wait,
	// and it gets a third of what is left -- no more.
	//
	// The wait is worth having: it is what stops a client-rendered page being
	// swept before it has written anything. But on a page whose boot saturates
	// the main thread it returns nothing at all, because the evaluation that
	// would report readiness cannot run either. pear.no spends three and a half
	// seconds there and comes back with no measurement, having learned nothing
	// and spent half the budget. Capping it means the sweep starts on a page
	// that is still settling -- which is what the sweep is for. It takes many
	// looks, keeps the best sighting of every run, and looks again when a page
	// comes back nearly empty.
	//
	// A third rather than a sixth: sweeping a page that is still playing its
	// intro film produces checkpoints full of the word "loading", and the
	// content that only the browser can supply -- the parts of the page that are
	// not in the served bytes at all -- is exactly what is lost by starting too
	// early. The served half of a page like pear.no is now recovered from the
	// HTML regardless, so the render can afford to wait for the half that is
	// only its to give.
	//
	// They were two: an event wait for DOMContentLoaded, then a settle wait
	// starting from scratch afterwards. Run in series that is the page's load
	// time plus the settle budget, when what is actually wanted is "tell me when
	// this page is both loaded and still" -- and the settle loop, which gates
	// itself on readyState, answers exactly that question in one call.
	readyBudget := b.opts.NavTimeout + b.opts.FirstSettle
	if hasDeadline {
		// Never more than a share of what is left. A page whose boot blocks the
		// main thread for five seconds will happily take the whole budget to
		// declare itself ready, and then there is nothing left to read it with:
		// pear.no settled at 4.9s of an 8s render and returned an empty
		// artifact. Waiting less means capturing a page mid-boot, which is worth
		// saying; capturing nothing is not worth anything.
		avail := time.Until(deadline) - sweepReserve
		if share := avail / 3; share < readyBudget {
			readyBudget = share
		}
	}
	if readyBudget < 300*time.Millisecond {
		readyBudget = 300 * time.Millisecond
	}

	settleStart := time.Now()
	var firstSettle settleResult
	readyCtx, cancelReady := context.WithTimeout(tabCtx, readyBudget+replyGrace)
	err := chromedp.Run(readyCtx,
		chromedp.Evaluate(`window.__sieve.unsmooth()`, nil),
		chromedp.Evaluate(b.settleExprMS(readyBudget), &firstSettle, awaitPromise),
	)
	cancelReady()
	if err != nil {
		if blocked := col.blockedErr(); blocked != nil {
			return nil, blocked
		}
		if !errors.Is(err, context.DeadlineExceeded) && readyCtx.Err() == nil {
			return nil, fmt.Errorf("prepare page: %w", err)
		}
		res.note("the page had not stopped loading when its share of the budget ran out; " +
			"it was swept as it stood")
	}
	res.Timing.FirstSettle = time.Since(settleStart)
	b.opts.logf("first settle took %v (page measured %dms, settled=%v)",
		res.Timing.FirstSettle.Round(time.Millisecond), firstSettle.MS, firstSettle.Settled)
	res.Timing.SettleWaits++
	if !firstSettle.Settled {
		res.Timing.SettleMiss++
		res.note("page never stopped animating within the settle timeout; capture may include mid-animation state")
	}

	// One capture before the sweep, unconditionally.
	//
	// The sweep reports everything at the end, which is what makes it cheap, and
	// it has a recovery path for being cut off -- but that path also has to run
	// in the page, and a page whose main thread is saturated can refuse both.
	// The result was an artifact with nothing in it for a site that was sitting
	// right there on screen. This costs one extraction, around twenty
	// milliseconds, and it means the floor is "what the first screen showed"
	// rather than "nothing at all".
	tOpen := time.Now()
	openCtx := tabCtx
	if hasDeadline {
		// Bounded, like everything else. This runs on a page whose main thread
		// may be saturated -- that is precisely when it is most valuable and
		// most likely to be slow -- and an unbounded wait here spends the
		// sweep's budget before the sweep is asked for its budget.
		limit := time.Until(deadline) / 3
		if limit > 1500*time.Millisecond {
			limit = 1500 * time.Millisecond
		}
		if limit < 300*time.Millisecond {
			limit = 300 * time.Millisecond
		}
		var cancelOpen context.CancelFunc
		openCtx, cancelOpen = context.WithTimeout(tabCtx, limit)
		defer cancelOpen()
	}
	if snap, cerr := b.captureOnce(openCtx, 0); cerr == nil {
		col.acc.Add(snap)
		b.opts.logf("opening capture took %v", time.Since(tOpen).Round(time.Millisecond))
	} else {
		b.opts.logf("opening capture failed after %v: %v",
			time.Since(tOpen).Round(time.Millisecond), cerr)
	}

	sweepStart := time.Now()
	if err := b.runSweep(tabCtx, res, col, deadline, hasDeadline); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return nil, err
	}
	res.Timing.Sweep = time.Since(sweepStart)

	// Last resort: if nothing at all was captured, look once more.
	//
	// Every stage above is bounded, and on a page whose boot saturates the main
	// thread for several seconds every one of those bounds can expire in turn --
	// the readiness wait, the opening capture, the sweep, its recovery drain --
	// leaving an artifact that says the site is empty about a site that is
	// simply slow. Emptiness is the one outcome worth spending the last of the
	// budget to avoid, because it is the only one a reader cannot tell apart
	// from the truth.
	if col.acc.NodeCount() == 0 {
		last := 1500 * time.Millisecond
		if hasDeadline {
			last = time.Until(deadline) - 300*time.Millisecond
		}
		if last > 200*time.Millisecond {
			lastCtx, cancelLast := context.WithTimeout(tabCtx, last)
			if snap, cerr := b.captureOnce(lastCtx, 0); cerr == nil && len(snap.Nodes) > 0 {
				col.acc.Add(snap)
				res.note("every timed stage of this render expired before it could report; " +
					"the artifact below is a single capture taken at the end of the budget")
				b.opts.logf("last-resort capture recovered %d node(s)", len(snap.Nodes))
			}
			cancelLast()
		}
	}

	res.Merged = col.acc.Result()

	// The probes run after the sweep, not before it.
	//
	// Nothing in the sweep depends on them: the library fingerprints feed the
	// post-render re-score and the entry gate is read at graph build. Asking for
	// them first meant queueing a round trip behind whatever the page was doing
	// on its main thread during boot -- three quarters of a second on pear.no --
	// and spending it before a single checkpoint had been taken.
	tProbe := time.Now()
	probeCtx := tabCtx
	if hasDeadline {
		var cancelProbe context.CancelFunc
		probeCtx, cancelProbe = context.WithDeadline(tabCtx, deadline)
		defer cancelProbe()
	}
	b.probePage(probeCtx, res)
	b.opts.logf("page probes took %v", time.Since(tProbe).Round(time.Millisecond))
	if res.FinalURL == "" {
		res.FinalURL = rawURL
	}

	// Everything from here on serves canvas recovery, and a page with no canvas
	// has nothing for it to recover.
	//
	// This used to run unconditionally, and the corpus was the expensive part:
	// a Network.getResponseBody for every script the page loaded, pulled while
	// the page was still being swept, which on a bundle-heavy site is tens of
	// megabytes crossing the CDP boundary to build an index that would then be
	// thrown away unread. It is a membership oracle for confirming text
	// recovered from pixels -- when there are no pixels to recover from, it is
	// pure cost.
	//
	// Deferring it also takes it off the critical path: the bodies are still
	// retained by the browser after the sweep, so nothing is lost by asking for
	// them once we know they are wanted.
	postCtx := tabCtx
	if hasDeadline {
		var cancelPost context.CancelFunc
		postCtx, cancelPost = context.WithDeadline(tabCtx, deadline)
		defer cancelPost()
	}

	if len(res.Merged.Canvases) > 0 {
		// The scene walk runs after every animation has settled and any lazily
		// constructed geometry exists.
		tScene := time.Now()
		b.introspectScene(postCtx, res)
		b.opts.logf("scene introspection took %v", time.Since(tScene).Round(time.Millisecond))

		tCorpus := time.Now()
		col.drainBodies(postCtx)
		b.collectCorpus(postCtx, col)
		b.opts.logf("corroboration corpus took %v", time.Since(tCorpus).Round(time.Millisecond))

		tShots := time.Now()
		col.rasteriseCanvases(postCtx, res.Merged.Canvases)
		if n := len(res.CanvasShots); n > 0 {
			b.opts.logf("rasterised %d canvas region(s) in %v", n,
				time.Since(tShots).Round(time.Millisecond))
		}
	}

	col.finish(postCtx)
	res.Assets = col.assets()
	res.Corpus = col.corpus
	res.Trace = b.trace()
	res.Timing.Total = time.Since(start)
	if res.Timing.Checkpoints == 0 {
		res.Timing.Checkpoints = res.Merged.Checkpoints
	}

	if res.Merged.FramesBlocked > 0 {
		res.note(fmt.Sprintf("%d cross-origin iframe(s) could not be read; their content is absent from this artifact", res.Merged.FramesBlocked))
	}
	if res.Merged.Truncated {
		res.note("per-checkpoint node budget was reached; some deeply nested content may be missing")
	}
	if res.Merged.LatentTruncated {
		res.note("hidden-content budget was reached; the latent tier is itself incomplete")
	}
	if len(res.Merged.Latent) > 0 {
		res.note(fmt.Sprintf("%d run(s) of text exist in the document but were never rendered; they are quarantined in the latent tier and excluded from the content payload", len(res.Merged.Latent)))
	}
	return res, nil
}

func settledMark(settled bool) string {
	if settled {
		return ""
	}
	return " (never settled)"
}

func motionValue(reduced bool) string {
	if reduced {
		return "reduce"
	}
	return "no-preference"
}

// pageProbe is what one round trip asks the page for before the sweep starts.
type pageProbe struct {
	URL       string   `json:"url"`
	Libraries []string `json:"libs"`
	// Gate names an entry interstitial standing between the visitor and the
	// site: a full-viewport overlay whose only offer is a control that says
	// some variant of "enter". See detectEntryGate in capture.js.
	Gate string `json:"gate"`
}

// probePage asks the page, in one evaluation, for the facts the sweep's
// decisions rest on: where the navigation actually landed, which animation and
// scroll libraries are loaded, and whether an entry gate is holding the content
// back. These were three round trips returning three short strings.
func (b *Browser) probePage(ctx context.Context, res *Result) {
	specs, err := LibrarySpecs()
	if err != nil {
		res.note("library fingerprints unavailable: " + err.Error())
		specs = nil
	}
	payload, _ := json.Marshal(specs)

	var raw string
	if err := chromedp.Run(ctx, chromedp.Evaluate(
		fmt.Sprintf(`window.__sieve.probe(%s)`, payload), &raw)); err != nil {
		return
	}
	var p pageProbe
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return
	}
	res.FinalURL = p.URL
	res.Libraries = p.Libraries
	res.EntryGate = p.Gate
}

// collectCorpus builds the confirm-only membership index.
//
// It runs only when the page has a canvas, because confirming a string
// recovered from pixels is the only thing this index is ever allowed to do. On
// a page with nothing to recover it is pure cost, and on a bundle-heavy site
// that cost is measured in megabytes.
func (b *Browser) collectCorpus(ctx context.Context, col *collector) {
	if !b.opts.CollectCorpus || col.corpus == nil {
		return
	}
	var raw string
	if err := chromedp.Run(ctx, chromedp.Evaluate(`window.__sieve.corpusText()`, &raw)); err != nil {
		return
	}
	var out struct {
		Inline string `json:"inline"`
		Page   string `json:"page"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return
	}
	col.corpus.AddText("inline", out.Inline)
	// The page's own rendered text belongs in the index too: a canvas headline
	// that also appears as a visually-hidden caption is confirmed by the page
	// itself, which is the strongest corroboration available.
	col.corpus.AddText("page", out.Page)
}

// introspectScene walks the live 3D scene graph. Parsing a .glb only works when
// the site loaded one; a scene built procedurally in JavaScript never produces
// an asset to intercept, and its object names exist only in memory.
func (b *Browser) introspectScene(ctx context.Context, res *Result) {
	var raw string
	if err := chromedp.Run(ctx, chromedp.Evaluate(`window.__sieve.scene()`, &raw)); err != nil {
		return
	}
	if raw == "" || raw == "null" {
		return
	}
	var sc capture.SceneIntrospection
	if err := json.Unmarshal([]byte(raw), &sc); err != nil {
		return
	}
	if len(sc.Names) > 0 || len(sc.Texts) > 0 {
		res.Scene = &sc
	}
}

// trace records everything that determined this render's output.
func (b *Browser) trace() Trace {
	return Trace{
		SieveVersion:   Version,
		CaptureHash:    capture.ScriptHash(),
		Chromium:       b.chromiumVersion,
		UserAgent:      b.opts.UserAgent,
		ViewportW:      b.opts.ViewportW,
		ViewportH:      b.opts.ViewportH,
		DeviceScale:    b.opts.DeviceScale,
		Locale:         b.opts.Locale,
		Timezone:       b.opts.Timezone,
		ReducedMotion:  b.opts.ReducedMotion,
		StepRatio:      b.opts.StepRatio,
		SettleFrames:   b.opts.SettleFrames,
		SettleTimeout:  b.opts.SettleTimeout.String(),
		FirstSettle:    b.opts.FirstSettle.String(),
		SettleFloor:    b.opts.SettleFloor.String(),
		SweepBudget:    b.opts.SweepBudget.String(),
		Passes:         b.opts.Passes,
		MaxCheckpoints: b.opts.MaxCheckpoints,
		StableCheckpts: b.opts.StableCheckpoints,
		Flags:          b.flags,
		BlockHostsHash: hashStrings(b.opts.BlockHosts),
	}
}

func hashStrings(ss []string) string {
	h := sha256.New()
	for _, s := range ss {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func (b *Browser) settleExpr() string {
	return b.settleExprMS(b.opts.SettleTimeout)
}

func (b *Browser) settleExprMS(d time.Duration) string {
	return fmt.Sprintf(`window.__sieve.settle(%d, %d)`, b.opts.SettleFrames, d.Milliseconds())
}

// runSweep hands the whole checkpoint loop to the page and folds the reply in.
//
// What used to happen here -- capture, decode, decide, scroll, wait, repeat --
// now happens in capture.js, for one reason: every one of those steps was a CDP
// round trip, and the decisions in the middle of them needed evidence that only
// exists in the page. A settle wait driven from Go can only ever be a timeout,
// because the process choosing when to stop waiting is on the far side of a
// socket from the frames it is waiting for. Driven from the page it is a count
// of animation frames, which is both the honest unit and two orders of
// magnitude cheaper.
//
// Go keeps everything that genuinely belongs to it: the accumulator and its
// dedupe rules, the notes, the audit, and the decision about what to do with
// the result. It just stops being in the inner loop.
func (b *Browser) runSweep(ctx context.Context, res *Result, col *collector,
	deadline time.Time, hasDeadline bool) error {
	o := &b.opts

	passes := o.Passes
	if o.MaxCheckpoints <= 1 {
		// A single post-settle capture is the render tier. There is no second
		// look to take.
		passes = 1
	}
	// The sweep's budget is what is actually left, not what was planned.
	//
	// A static fraction of the page budget is wrong in both directions. If
	// navigation and the first settle were fast there is time going spare that
	// the sweep could have spent covering more of the document; if they were
	// slow, a sweep that still believes it has its full allowance runs past the
	// page deadline and is killed mid-call -- and because the whole loop reports
	// once at the end, being killed mid-call loses every checkpoint it took.
	// That is exactly how a nine-second run on pear.no produced zero blocks.
	//
	// So the budget is recomputed here against the real deadline, with a reserve
	// held back for the work that must still happen afterwards.
	budget := o.SweepBudget
	if hasDeadline {
		if avail := time.Until(deadline) - sweepReserve; avail < budget {
			budget = avail
		}
	}
	if budget < minSweepBudget {
		budget = minSweepBudget
	}
	if hasDeadline {
		o.logf("sweep budget %v (%v left of the page deadline)",
			budget.Round(time.Millisecond), time.Until(deadline).Round(time.Millisecond))
	} else {
		o.logf("sweep budget %v", budget.Round(time.Millisecond))
	}

	cfg := sweepConfig{
		BudgetMS:          budget.Milliseconds(),
		MaxCheckpoints:    o.MaxCheckpoints,
		StableCheckpoints: o.StableCheckpoints,
		SettleFrames:      o.SettleFrames,
		SettleMaxMS:       o.SettleTimeout.Milliseconds(),
		SettleMinMS:       o.SettleFloor.Milliseconds(),
		NodeBudget:        o.NodeBudget,
		LatentBudget:      o.LatentBudget,
		MaxScrollPx:       o.MaxScrollPx,
		StepRatio:         o.StepRatio,
		Passes:            passes,
		// Shrinking a WebGL drawing buffer is only safe while nobody is going
		// to read the pixels out of it.
		ThrottleGL: !o.CaptureCanvas,
	}
	payload, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("encode sweep config: %w", err)
	}

	sweepStart := time.Now()
	sweepCtx, cancelSweep := context.WithTimeout(ctx, budget+replyGrace)
	defer cancelSweep()

	var raw string
	err = chromedp.Run(sweepCtx, chromedp.Evaluate(
		fmt.Sprintf(`window.__sieve.sweep(%s)`, payload), &raw, awaitPromise))
	if err != nil {
		if !errors.Is(err, context.DeadlineExceeded) && sweepCtx.Err() == nil {
			return fmt.Errorf("sweep: %w", err)
		}
		// The sweep outran the driver's patience. Everything it saw is still in
		// the page, so it is asked for rather than discarded.
		o.logf("sweep overran after %v; collecting what it had",
			time.Since(sweepStart).Round(time.Millisecond))
		// ctx here is the tab, which is deliberately still alive. The drain gets
		// whatever remains of the page budget: it is the last chance to keep the
		// checkpoints already taken, and a fixed grace that expires while the
		// page is busy throws all of them away.
		grace := replyGrace
		if hasDeadline {
			if left := time.Until(deadline) - 200*time.Millisecond; left > grace {
				grace = left
			}
		}
		drainCtx, cancelDrain := context.WithTimeout(ctx, grace)
		derr := chromedp.Run(drainCtx, chromedp.Evaluate(`window.__sieve.sweepResult()`, &raw))
		cancelDrain()
		if derr != nil || raw == "" || raw == "null" {
			res.note("the sweep was cut short by the time budget before it could report; " +
				"this artifact rests on the initial capture alone")
			return nil
		}
	}

	var reply sweepReply
	if err := json.Unmarshal([]byte(raw), &reply); err != nil {
		return fmt.Errorf("decode sweep reply (%d bytes): %w", len(raw), err)
	}
	if reply.Partial {
		res.note(fmt.Sprintf("the sweep ran out of time after %d checkpoint(s) and was collected "+
			"where it stood; the page continued below the point it reached", reply.Checkpoints))
	}

	for i := range reply.Snapshots {
		col.acc.Add(&reply.Snapshots[i])
	}
	for _, n := range reply.Notes {
		res.note(n)
	}
	res.ReachedBottom = reply.ReachedBottom
	res.Timing.Checkpoints = reply.Checkpoints
	res.Timing.Passes = reply.Passes

	if reply.Refinements > 0 {
		res.note(fmt.Sprintf("most text on this page was unrevealed at the default sampling rate, "+
			"so the scroll step was refined %d time(s) down to %dpx; the page reveals content in "+
			"narrow scroll bands", reply.Refinements, reply.Step))
	}
	if reply.Throttled > 0 || reply.PausedVideos > 0 {
		o.logf("quietened the page for the sweep: %d canvas drawing buffer(s) shrunk, %d video(s) paused",
			reply.Throttled, reply.PausedVideos)
	}
	if reply.Virtual {
		res.note("this page cancels native scrolling and was driven with wheel events instead; " +
			"its reported document height is not the height of its content")
	}
	if !reply.ReachedBottom {
		res.note("the sweep did not reach the bottom of the document")
	}

	o.logf("sweep: %d checkpoints over %d pass(es) via %s in %dms "+
		"(capture %dms/worst %dms, settle %dms/worst %dms, scroll %dms), "+
		"stop=%s, %d/%d targeted stops, %d pinned / %d free unread chars, "+
		"step %dpx, %d bytes returned, bottom=%v",
		reply.Checkpoints, reply.Passes, reply.Mode, reply.TotalMS,
		reply.CaptureMS, reply.CaptureWorstMS, reply.SettleMS, reply.SettleWorstMS,
		reply.ScrollMS, reply.StopReason, reply.TargetedStops, reply.TargetsFound,
		reply.PinnedChars, reply.FreeChars,
		reply.Step, len(raw), reply.ReachedBottom)
	return nil
}

// sweepReserve is held back from the sweep for everything that still has to
// happen inside the render deadline: the scene walk, the corroboration corpus,
// canvas rasterisation, and the reply itself crossing the CDP boundary.
const sweepReserve = 900 * time.Millisecond

// replyGrace is the extra time allowed for the reply itself: the page has to
// serialise its accumulated checkpoints and the string has to cross the CDP
// boundary, and neither is instant on a large page.
const replyGrace = 700 * time.Millisecond

// minSweepBudget is the floor. Below this the sweep would return a single
// checkpoint, which is still worth having and is what the cp-0 exemption in the
// page guarantees.
const minSweepBudget = 300 * time.Millisecond

// captureOnce takes a single extraction, used for the page probes that need one
// outside the sweep.
func (b *Browser) captureOnce(ctx context.Context, cp int) (*capture.Snapshot, error) {
	var raw string
	expr := fmt.Sprintf(`window.__sieve.capture(%d,%d,%d)`,
		cp, b.opts.NodeBudget, b.opts.LatentBudget)
	if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &raw)); err != nil {
		return nil, err
	}
	var snap capture.Snapshot
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		return nil, fmt.Errorf("decode checkpoint %d (%d bytes): %w", cp, len(raw), err)
	}
	snap.Checkpoint = cp
	return &snap, nil
}

func (r *Result) note(s string) {
	for _, existing := range r.Notes {
		if existing == s {
			return
		}
	}
	r.Notes = append(r.Notes, s)
}

// sceneGraphExt are the response types worth keeping for canvas recovery.
var sceneGraphExt = []string{".glb", ".gltf", ".fbx", ".splinecode", ".usdz"}

func isSceneGraphURL(u string) bool {
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	lower := strings.ToLower(u)
	for _, e := range sceneGraphExt {
		if strings.HasSuffix(lower, e) {
			return true
		}
	}
	return false
}

func isSceneGraphMIME(m string) bool {
	switch strings.ToLower(strings.TrimSpace(m)) {
	case "model/gltf-binary", "model/gltf+json", "model/vnd.usdz+zip":
		return true
	}
	return false
}
