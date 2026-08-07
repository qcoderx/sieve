package render

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strings"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/input"
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

	// Trace is everything needed to reproduce this render.
	Trace Trace

	Timing Timing
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
	SettleWaits int           `json:"settle_waits"`
	SettleMiss  int           `json:"settle_timeouts"`
}

type settleResult struct {
	Settled bool `json:"settled"`
	MS      int  `json:"ms"`
}

type stepResult struct {
	Before float64 `json:"before"`
	After  float64 `json:"after"`
	Moved  float64 `json:"moved"`
	Height float64 `json:"height"`
	VH     float64 `json:"vh"`
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

	if b.opts.Budget > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, b.opts.Budget)
		defer cancel()
	}

	tabCtx, tabCancel := chromedp.NewContext(b.baseCtx)
	defer tabCancel()

	// Tie the tab to the caller's deadline without losing chromedp's target
	// bookkeeping, which lives on tabCtx's values.
	go func() {
		select {
		case <-ctx.Done():
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

	navStart := time.Now()
	navCtx := tabCtx
	if b.opts.NavTimeout > 0 {
		var cancel context.CancelFunc
		navCtx, cancel = context.WithTimeout(tabCtx, b.opts.NavTimeout)
		defer cancel()
	}
	if err := chromedp.Run(navCtx, chromedp.Navigate(rawURL)); err != nil {
		if blocked := col.blockedErr(); blocked != nil {
			return nil, blocked
		}
		return nil, fmt.Errorf("navigate %s: %w", rawURL, err)
	}
	res.Timing.Navigate = time.Since(navStart)

	// Install the extraction script, disable native smooth scrolling, then wait
	// for the entrance animations to finish before the first capture. Capturing
	// a hero mid-fade records it at the opacity it happened to have, and every
	// downstream signal reads from that number.
	settleStart := time.Now()
	var firstSettle settleResult
	if err := chromedp.Run(tabCtx,
		chromedp.Evaluate(capture.Script, nil),
		chromedp.Evaluate(`window.__sieve.unsmooth()`, nil),
		chromedp.Evaluate(b.settleExpr(), &firstSettle, awaitPromise),
	); err != nil {
		return nil, fmt.Errorf("prepare page: %w", err)
	}
	res.Timing.FirstSettle = time.Since(settleStart)
	res.Timing.SettleWaits++
	if !firstSettle.Settled {
		res.Timing.SettleMiss++
		res.note("page never stopped animating within the settle timeout; capture may include mid-animation state")
	}

	if err := chromedp.Run(tabCtx, chromedp.Evaluate(`location.href`, &res.FinalURL)); err != nil {
		res.FinalURL = rawURL
	}

	b.probePage(tabCtx, res, col)

	sweepStart := time.Now()
	if err := b.runSweep(tabCtx, res, col); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return nil, err
	}
	res.Timing.Sweep = time.Since(sweepStart)

	// The scene walk runs last, after every animation has settled and any
	// lazily constructed geometry exists.
	b.introspectScene(tabCtx, res)

	col.finish(tabCtx)
	res.Merged = col.acc.Result()
	res.Assets = col.assets()
	res.Corpus = col.corpus
	res.Trace = b.trace()
	res.Timing.Total = time.Since(start)
	res.Timing.Checkpoints = res.Merged.Checkpoints

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

func motionValue(reduced bool) string {
	if reduced {
		return "reduce"
	}
	return "no-preference"
}

// probePage runs the one-shot in-page probes: which libraries are present, and
// the confirm-only corpus of text the site shipped inline.
func (b *Browser) probePage(ctx context.Context, res *Result, col *collector) {
	specs, err := LibrarySpecs()
	if err != nil {
		res.note("library fingerprints unavailable: " + err.Error())
	} else {
		payload, _ := json.Marshal(specs)
		var raw string
		if err := chromedp.Run(ctx, chromedp.Evaluate(
			fmt.Sprintf(`window.__sieve.libs(%s)`, payload), &raw)); err == nil {
			_ = json.Unmarshal([]byte(raw), &res.Libraries)
		}
	}

	if !b.opts.CollectCorpus || col.corpus == nil {
		return
	}
	var inline string
	if err := chromedp.Run(ctx, chromedp.Evaluate(`window.__sieve.corpus()`, &inline)); err == nil {
		col.corpus.AddText("inline", inline)
	}
	// The page's own rendered text belongs in the index too: a canvas headline
	// that also appears as a visually-hidden caption is confirmed by the page
	// itself, which is the strongest corroboration available.
	var pageText string
	if err := chromedp.Run(ctx, chromedp.Evaluate(
		`(document.body&&document.body.innerText||'').slice(0,1000000)`, &pageText)); err == nil {
		col.corpus.AddText("page", pageText)
	}
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
	return fmt.Sprintf(`window.__sieve.settle(%d, %d)`,
		b.opts.SettleFrames, b.opts.SettleTimeout.Milliseconds())
}

// runSweep walks the viewport down the page.
func (b *Browser) runSweep(ctx context.Context, res *Result, col *collector) error {
	o := &b.opts
	stepPx := float64(o.ViewportH) * o.StepRatio
	if stepPx < 100 {
		stepPx = 100
	}

	var travelled float64
	stalls := 0
	atBottom := false

	for cp := 0; cp < o.MaxCheckpoints; cp++ {
		if err := ctx.Err(); err != nil {
			res.note("time budget exhausted mid-sweep; the lower part of the page was not swept")
			return nil
		}

		snap, err := b.captureCheckpoint(ctx, cp)
		if err != nil {
			if cp == 0 {
				return fmt.Errorf("capture checkpoint 0: %w", err)
			}
			res.note("capture failed at checkpoint " + fmt.Sprint(cp) + ": " + err.Error())
			return nil
		}
		fresh := col.acc.Add(snap)
		o.logf("checkpoint %d  y=%.0f  nodes=%d (+%d)  actions=%d",
			cp, snap.ScrollY, len(snap.Nodes), fresh, len(snap.Actions))

		col.considerCanvases(ctx, snap, cp)

		atBottom = snap.ScrollY+snap.ViewportH >= snap.DocHeight-2

		// Termination. The stability rule is the primary one, but firing it
		// while the page is still scrolling and still growing would truncate a
		// site that happens to have a tall empty section between two blocks of
		// content. So stability only ends the sweep once there is nowhere left
		// to go: the document bottom is reached, or scrolling has stopped
		// having any effect.
		if col.acc.StableFor(o.StableCheckpoints) && (atBottom || stalls >= 2) {
			o.logf("sweep complete: %d checkpoints with no new content", o.StableCheckpoints)
			break
		}
		if atBottom && col.acc.StableFor(1) {
			o.logf("sweep complete: reached document bottom")
			break
		}
		if travelled >= o.MaxScrollPx {
			res.note(fmt.Sprintf("scroll budget of %.0fpx exhausted; the page continued below this point", o.MaxScrollPx))
			break
		}

		moved, err := b.advance(ctx, stepPx)
		if err != nil {
			res.note("scroll failed: " + err.Error())
			break
		}
		if moved < stepPx*0.2 {
			stalls++
		} else {
			stalls = 0
		}
		travelled += math.Max(moved, 0)

		var sr settleResult
		if err := chromedp.Run(ctx, chromedp.Evaluate(b.settleExpr(), &sr, awaitPromise)); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			res.note("settle wait failed after scroll: " + err.Error())
		}
		res.Timing.SettleWaits++
		if !sr.Settled {
			res.Timing.SettleMiss++
		}

		if cp == o.MaxCheckpoints-1 && !atBottom {
			res.note(fmt.Sprintf("checkpoint cap of %d reached before the document bottom", o.MaxCheckpoints))
		}
	}
	return nil
}

// advance moves the viewport down by delta and reports how far it actually got.
//
// Two mechanisms, in order of cost. window.scrollTo is one evaluation and works
// on any page using native scrolling. When it does not move the page -- which
// is what a scroll-hijacking library looks like from outside -- real wheel
// events are dispatched instead, because those libraries are built to listen
// for exactly that.
func (b *Browser) advance(ctx context.Context, delta float64) (float64, error) {
	var raw string
	if err := chromedp.Run(ctx, chromedp.Evaluate(
		fmt.Sprintf(`window.__sieve.step(%f)`, delta), &raw)); err != nil {
		return 0, err
	}
	var sr stepResult
	if err := json.Unmarshal([]byte(raw), &sr); err != nil {
		return 0, err
	}
	if sr.Moved >= delta*0.2 {
		return sr.Moved, nil
	}
	// Already at the bottom is not a stall.
	if sr.After+sr.VH >= sr.Height-2 {
		return sr.Moved, nil
	}

	x := float64(b.opts.ViewportW) / 2
	y := float64(b.opts.ViewportH) / 2
	// Split into several wheel ticks: a library that clamps per-event delta
	// would swallow one large one.
	const ticks = 5
	per := delta / ticks
	for i := 0; i < ticks; i++ {
		if err := chromedp.Run(ctx, chromedp.ActionFunc(func(c context.Context) error {
			return input.DispatchMouseEvent(input.MouseWheel, x, y).
				WithDeltaX(0).WithDeltaY(per).Do(c)
		})); err != nil {
			return sr.Moved, nil // wheel is best-effort; stability will end the sweep
		}
	}
	var after float64
	if err := chromedp.Run(ctx, chromedp.Evaluate(`window.scrollY||0`, &after)); err != nil {
		return sr.Moved, nil
	}
	if d := after - sr.Before; d > sr.Moved {
		return d, nil
	}
	return sr.Moved, nil
}

func (b *Browser) captureCheckpoint(ctx context.Context, cp int) (*capture.Snapshot, error) {
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
