package render

import (
	"os"
	"runtime"
	"time"
)

// Options configures a render session. The zero value is not usable; start from
// DefaultOptions and adjust.
type Options struct {
	// ChromePath is an explicit browser binary. Empty means auto-detect.
	ChromePath string
	// Headless runs without a visible window. Turning it off is a debugging aid.
	Headless bool
	// NoSandbox is required inside most containers. It weakens the browser's
	// own isolation, so it is off unless asked for.
	NoSandbox bool

	ViewportW   int
	ViewportH   int
	DeviceScale float64
	UserAgent   string
	// AcceptLanguage is sent on every request and also drives the page's
	// navigator.languages, which some sites use to pick content.
	AcceptLanguage string

	// NavTimeout bounds the initial navigation. Exceeding it is not fatal: what
	// matters is whether a document arrived, not whether every straggling image
	// finished.
	NavTimeout time.Duration
	// FirstSettle bounds the wait for entrance animations to finish before the
	// first capture. It is larger than SettleTimeout because a page gets one
	// chance to finish loading and settling, and capturing a hero mid-fade
	// records it at whatever opacity it happened to have.
	FirstSettle time.Duration
	// SettleTimeout bounds the wait for animation settle after a scroll step. A
	// page with a permanently looping animation never settles, so this is the
	// value that actually ends the wait on such sites -- which is why it is
	// small. The sweep takes many cheap looks rather than a few expensive ones.
	SettleTimeout time.Duration
	// SettleFloor is the shortest that wait may be compressed to when the sweep
	// is rationing its remaining time across the rest of the document.
	SettleFloor time.Duration
	// RevealFloor is the settle wait used when text is being captured and none
	// of it has ever been legible. On such a page the sweep is outrunning the
	// animation, and the remedy is fewer, slower stops rather than more.
	RevealFloor time.Duration
	// SettleFrames is how many consecutive animation frames must show no
	// layout change before the page counts as settled.
	SettleFrames int
	// LoadBudget bounds how long sieve will wait for a page to arrive and stop
	// moving, before it starts reading.
	//
	// It is deliberately separate from Budget, and it is not deducted from it.
	// Charging a site's own loading time to the extraction meant that a page
	// with an intro film or a preloader -- exactly the class of page this tool
	// exists for -- handed the sweep whatever was left, and what was left was
	// often one capture of a loading screen. A budget for reading a page should
	// start when there is a page to read.
	LoadBudget time.Duration
	// SweepBudget bounds the in-page checkpoint loop. The loop rations itself
	// against this: it plans its step size and its per-checkpoint settle wait
	// so that the document is covered within it.
	SweepBudget time.Duration
	// Budget bounds the whole render for one page, navigation included.
	Budget time.Duration
	// Passes is how many times the document is walked.
	//
	// Two is the useful default and the reason is that almost every scroll
	// reveal on the web fires once and stays fired. The first pass is as much a
	// trigger as a capture; the second sees at full opacity what the first
	// could only catch mid-fade, and costs almost nothing to transmit because
	// everything it re-observes is already known. It is a far better use of a
	// second than dwelling a second longer at every checkpoint of one pass,
	// because dwelling only helps the section being dwelt on.
	Passes int

	// StepRatio is the fraction of a viewport height advanced per checkpoint.
	// Below 1.0 so that content revealed at the boundary of two viewports is
	// seen fully at least once.
	StepRatio float64
	// MaxCheckpoints caps the sweep on pages that grow as you scroll.
	MaxCheckpoints int
	// StableCheckpoints is K: stop once this many consecutive checkpoints have
	// added no new unique nodes.
	StableCheckpoints int
	// MaxScrollPx caps total distance travelled, which is the guard against an
	// infinite-scroll feed.
	MaxScrollPx float64
	// NodeBudget caps nodes captured per checkpoint.
	NodeBudget int
	// LatentBudget caps hidden-content nodes per checkpoint. It is separate
	// from NodeBudget so that a page with an enormous hidden menu tree cannot
	// crowd out the content the reader can actually see.
	LatentBudget int

	// CollectCorpus retains inline JSON, hydration blobs and script string
	// literals as a confirm-only index for canvas recovery. It is never a
	// source of content -- see the corroborate package for why that rule is
	// absolute.
	CollectCorpus bool
	// MaxCorpusBytes bounds that index.
	MaxCorpusBytes int

	// ReducedMotion emulates prefers-reduced-motion. Running a second pass with
	// it on and comparing the two is the cheapest available check on whether
	// the reveal machinery was understood correctly.
	ReducedMotion bool

	// Locale and Timezone are pinned rather than inherited from the host, so
	// that the same URL rendered on two machines produces the same artifact.
	Locale   string
	Timezone string

	// BlockHosts are host patterns resolved to nothing by the browser, which
	// removes analytics and ad traffic before a connection is opened rather
	// than after. Wildcards are allowed: "*.doubleclick.net".
	BlockHosts []string
	// BlockURLPatterns are additional URLPattern-syntax rules applied by the
	// browser's network layer, for path-level blocking that a host rule cannot
	// express.
	BlockURLPatterns []string

	// CaptureCanvas enables viewport screenshots at checkpoints where a canvas
	// covers at least CanvasShareGate of the viewport. Screenshots are cropped
	// to the canvas and kept only for canvas recovery.
	CaptureCanvas bool
	// CanvasShareGate is the fraction of viewport area a canvas must cover
	// before it is worth rasterising.
	CanvasShareGate float64

	// CollectAssets keeps response bodies for scene-graph formats so canvas
	// recovery can read node names out of them without refetching.
	CollectAssets bool
	// MaxAssetBytes and MaxAssetsTotal bound that collection.
	MaxAssetBytes  int64
	MaxAssetsTotal int64

	// Proxy routes browser traffic through an HTTP proxy.
	Proxy string

	// Logf receives progress lines. Nil discards them.
	Logf func(format string, args ...any)
}

// DefaultOptions returns settings tuned for design-heavy marketing and
// portfolio sites, which is the class of page this tool exists for.
func DefaultOptions() Options {
	return Options{
		Headless:    true,
		NoSandbox:   runtime.GOOS == "linux" && os.Geteuid() == 0,
		ViewportW:   1440,
		ViewportH:   900,
		DeviceScale: 1,
		// An honest, identifying user agent with a contact URL, as the ethics
		// section requires. Sites that wish to refuse this traffic are given
		// everything they need to do so.
		UserAgent:      "Mozilla/5.0 (compatible; sieve/0.1; +https://github.com/qcoderx/sieve)",
		AcceptLanguage: "en-US,en;q=0.9",

		// The timings below are built around a ten-second ceiling for a whole
		// page, and they are aggressive on purpose.
		//
		// The old defaults -- a six-second settle wait at every checkpoint, a
		// forty-five second navigation, a hundred and fifty second budget --
		// were sized for a loop that could only wait by timeout, because it was
		// driven from outside the browser. A settle wait measured in animation
		// frames, inside the page, answers in tens of milliseconds when the page
		// is still and stops waiting almost immediately when it never will be.
		// That is what makes these numbers affordable rather than reckless: the
		// sweep is not doing less looking, it is doing far more of it, faster.
		NavTimeout:    6 * time.Second,
		FirstSettle:   1200 * time.Millisecond,
		SettleTimeout: 260 * time.Millisecond,
		SettleFloor:   60 * time.Millisecond,
		RevealFloor:   450 * time.Millisecond,
		SettleFrames:  2,
		LoadBudget:    20 * time.Second,
		SweepBudget:   5 * time.Second,
		Budget:        10 * time.Second,
		Passes:        2,

		// 0.75 of a viewport per step. Scroll-triggered reveals commonly fire
		// when an element crosses 70-80% of viewport height, so a full-viewport
		// step can land past the trigger and past the settle in one move,
		// capturing the element mid-animation and never again. The sweep widens
		// this by itself on a document too tall to cover otherwise, and narrows
		// it on a page revealing content in narrow bands.
		StepRatio: 0.75,
		// The cap is generous because checkpoints are now cheap: it exists to
		// stop a pathological page looping forever, not to ration work. The time
		// budget is the real bound.
		MaxCheckpoints:    400,
		StableCheckpoints: 3,
		MaxScrollPx:       120000,
		NodeBudget:        40000,
		LatentBudget:      12000,

		CollectCorpus:  true,
		MaxCorpusBytes: 4 << 20,

		// Pinned, not inherited. Locale and timezone change what a site serves,
		// and a distiller whose output depends on which machine ran it has no
		// business claiming determinism.
		Locale:   "en-US",
		Timezone: "UTC",

		BlockHosts: DefaultBlockHosts(),
		// Off unless something can read the pixels.
		//
		// A canvas screenshot is only ever consumed by OCR or by a vision model,
		// and both are off by default -- vision deliberately so, because with it
		// disabled the artifact structurally cannot contain invented text. That
		// left the default configuration waiting on the compositor, decoding,
		// cropping and re-encoding PNGs at checkpoint after checkpoint, and then
		// discarding every one of them unread. The distiller turns this on when
		// it has a recogniser to hand it to.
		CaptureCanvas:   false,
		CanvasShareGate: 0.25,

		CollectAssets:  true,
		MaxAssetBytes:  32 << 20,
		MaxAssetsTotal: 128 << 20,
	}
}

// ScaleTo fits the render's time budget inside the wall clock the caller is
// willing to spend on a whole page, holding back a margin for the fetch, the
// graph and the emit.
//
// The sub-budgets move together and stay in proportion, because they are not
// independent: a navigation allowance larger than the sweep budget guarantees a
// page that loads slowly is never swept, and a first-settle wait that is a large
// fraction of the total guarantees the same. Scaling one number is also the only
// way `--timeout` can mean what it says. Before this, `--timeout 10m` left the
// sweep stopping at its own hardcoded two and a half minutes, and `--timeout 10s`
// left it cheerfully planning for a hundred and fifty.
func (o *Options) ScaleTo(total time.Duration) {
	if total <= 0 {
		return
	}
	// The margin is everything that is not rendering but still happens on the
	// clock the user is measuring: the graph build, the emit, and -- the largest
	// single item, and the one easiest to forget -- shutting Chromium down,
	// which takes the better part of a second whatever the page was.
	//
	// It is a fraction rather than a constant so that a very short budget does
	// not spend all of itself on rendering and leave nothing to write with, with
	// a floor so that a generous budget still reserves enough to finish.
	reserve := total / 5
	if reserve < 1400*time.Millisecond {
		reserve = 1400 * time.Millisecond
	}
	render := total - reserve
	if render < time.Second {
		render = total
	}
	o.Budget = render

	// Navigation and the first settle are the fixed cost of having a page at
	// all; the sweep gets what is left.
	// The load allowance is generous and independent: waiting for a slow site is
	// not work sieve is doing, and cutting it short only guarantees a thin read.
	o.NavTimeout = scaleDur(render, 6, 10, 2*time.Second, 45*time.Second)
	o.FirstSettle = scaleDur(render, 3, 25, 400*time.Millisecond, 6*time.Second)
	o.SweepBudget = scaleDur(render, 3, 4, 800*time.Millisecond, 10*time.Minute)
	o.SettleTimeout = scaleDur(render, 1, 40, 120*time.Millisecond, 2*time.Second)
	o.SettleFloor = scaleDur(render, 1, 160, 40*time.Millisecond, 600*time.Millisecond)
	o.RevealFloor = scaleDur(render, 1, 18, 250*time.Millisecond, 1200*time.Millisecond)
}

// scaleDur takes num/den of d and clamps it.
func scaleDur(d time.Duration, num, den int64, min, max time.Duration) time.Duration {
	v := time.Duration(int64(d) * num / den)
	if v < min {
		v = min
	}
	if v > max {
		v = max
	}
	return v
}

func (o *Options) logf(format string, args ...any) {
	if o.Logf != nil {
		o.Logf(format, args...)
	}
}

// DefaultBlockHosts is the analytics, advertising and session-replay traffic
// that contributes nothing to a page's content and a great deal to its load
// time. Blocking happens at name resolution, so the cost is zero per request
// rather than one interception round trip per request.
//
// Session replay tools in particular (FullStory, Hotjar, Clarity) both slow the
// page and record the sweep, so declining to talk to them is the courteous
// default as well as the fast one.
func DefaultBlockHosts() []string {
	return []string{
		"*.google-analytics.com",
		"google-analytics.com",
		"*.googletagmanager.com",
		"googletagmanager.com",
		"*.doubleclick.net",
		"doubleclick.net",
		"*.googlesyndication.com",
		"*.googleadservices.com",
		"*.facebook.net",
		"connect.facebook.net",
		"*.hotjar.com",
		"*.hotjar.io",
		"*.fullstory.com",
		"*.clarity.ms",
		"*.mixpanel.com",
		"api.segment.io",
		"cdn.segment.com",
		"*.amplitude.com",
		"*.intercom.io",
		"*.intercomcdn.com",
		"*.crisp.chat",
		"*.drift.com",
		"*.hs-analytics.net",
		"*.hs-scripts.com",
		"*.newrelic.com",
		"*.nr-data.net",
		"*.sentry.io",
		"*.bugsnag.com",
		"*.optimizely.com",
		"*.criteo.com",
		"*.taboola.com",
		"*.outbrain.com",
		"*.adroll.com",
		"ct.pinterest.com",
		"analytics.tiktok.com",
		"*.ads-twitter.com",
		"static.ads-twitter.com",
		"px.ads.linkedin.com",
		"*.scorecardresearch.com",
		"*.quantserve.com",
		"*.onetrust.com",
		"*.cookielaw.org",
		"*.usercentrics.eu",
		"*.cookiebot.com",

		// A/B testing and personalisation platforms are blocked for a different
		// reason from the trackers above. They do not merely slow the page --
		// they change what it says, at random, per visit. A distiller that
		// claims the same input yields the same artifact cannot let a split
		// test decide which headline it captured. Blocking them yields the
		// control variant, deterministically.
		//
		// Only client-side *visual* testing tools are listed. Those are built
		// to fail open: their whole anti-flicker design is that the original
		// markup shows when the script does not arrive. Server-side feature
		// flag SDKs (LaunchDarkly, Statsig, Split) are deliberately absent --
		// applications hard-depend on them and blocking one can hang the page
		// rather than yield a default.
		"*.visualwebsiteoptimizer.com",
		"*.vwo.com",
		"*.abtasty.com",
		"*.dynamicyield.com",
		"*.monetate.net",
		"*.kameleoon.eu",
		"*.convertexperiments.com",
		"*.omappapi.com",
		"*.adobedtm.com",
		"*.omtrdc.net",
	}
}
