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

	// NavTimeout bounds the initial navigation.
	NavTimeout time.Duration
	// SettleTimeout bounds the wait for animation settle after load and after
	// each scroll step. A page with a permanently looping animation never
	// settles, so this is the value that actually ends the wait on such sites.
	SettleTimeout time.Duration
	// SettleFrames is how many consecutive animation frames must show no
	// layout change before the page counts as settled.
	SettleFrames int
	// Budget bounds the whole sweep for one page, settle waits included.
	Budget time.Duration

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

		NavTimeout:    45 * time.Second,
		SettleTimeout: 6 * time.Second,
		SettleFrames:  6,
		Budget:        150 * time.Second,

		// 0.75 of a viewport per step. Scroll-triggered reveals commonly fire
		// when an element crosses 70-80% of viewport height, so a full-viewport
		// step can land past the trigger and past the settle in one move,
		// capturing the element mid-animation and never again.
		StepRatio:         0.75,
		MaxCheckpoints:    64,
		StableCheckpoints: 3,
		MaxScrollPx:       120000,
		NodeBudget:        40000,
		LatentBudget:      12000,

		CollectCorpus:  true,
		MaxCorpusBytes: 8 << 20,

		// Pinned, not inherited. Locale and timezone change what a site serves,
		// and a distiller whose output depends on which machine ran it has no
		// business claiming determinism.
		Locale:   "en-US",
		Timezone: "UTC",

		BlockHosts:      DefaultBlockHosts(),
		CaptureCanvas:   true,
		CanvasShareGate: 0.25,

		CollectAssets:  true,
		MaxAssetBytes:  32 << 20,
		MaxAssetsTotal: 128 << 20,
	}
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
