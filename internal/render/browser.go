// Package render drives a headless Chromium session: it loads a page, waits for
// it to stop moving, sweeps the viewport through the full scroll height, and
// hands back a deduplicated capture of everything that was ever on screen.
package render

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/chromedp"
)

// Version identifies the build. It is stamped into every trace so an artifact
// can be tied back to the code that produced it.
var Version = "0.1.0-dev"

// Browser owns one Chromium process. Reuse it across pages: process startup is
// 200-600ms, which dominates the cost of a small page.
type Browser struct {
	allocCtx    context.Context
	allocCancel context.CancelFunc
	baseCtx     context.Context
	baseCancel  context.CancelFunc
	opts        Options

	// chromiumVersion and flags are recorded for the trace. A rendering change
	// between Chromium builds silently changes extraction output, so an
	// artifact that does not say which build produced it cannot be compared
	// with one that does.
	chromiumVersion string
	flags           []string

	mu     sync.Mutex
	closed bool
}

// ErrChromiumNotFound is returned when no browser binary could be located.
// It carries instructions rather than a stack trace, because a missing
// Chromium is the single most common first-run failure.
var ErrChromiumNotFound = errors.New(
	"no Chromium or Chrome binary found\n" +
		"  sieve needs a Chromium-based browser to render pages.\n" +
		"  Install Google Chrome, Chromium or Microsoft Edge, or point sieve at one:\n" +
		"    sieve distill <url> --chrome /path/to/chrome\n" +
		"    SIEVE_CHROME=/path/to/chrome sieve distill <url>")

// Launch starts a browser process configured for extraction rather than for
// browsing: no GPU rasterisation surprises, no first-run dialogs, no background
// networking, and name resolution for tracker hosts pointed at nothing.
func Launch(ctx context.Context, opts Options) (*Browser, error) {
	path := opts.ChromePath
	if path == "" {
		path = os.Getenv("SIEVE_CHROME")
	}
	if path == "" {
		path = findChromium()
	}
	if path == "" {
		return nil, ErrChromiumNotFound
	}
	if _, err := os.Stat(path); err != nil {
		if _, lerr := exec.LookPath(path); lerr != nil {
			return nil, fmt.Errorf("browser %q is not executable: %w", path, err)
		}
	}

	var fs flagSet
	fs.add(chromedp.ExecPath(path), "--exec-path="+path)
	fs.add(chromedp.NoFirstRun, "--no-first-run")
	fs.add(chromedp.NoDefaultBrowserCheck, "--no-default-browser-check")
	fs.set("disable-background-networking", true)
	fs.set("disable-background-timer-throttling", true)
	fs.set("disable-backgrounding-occluded-windows", true)
	fs.set("disable-renderer-backgrounding", true)
	fs.set("disable-client-side-phishing-detection", true)
	fs.set("disable-default-apps", true)
	fs.set("disable-sync", true)
	fs.set("disable-domain-reliability", true)
	fs.set("disable-component-update", true)
	fs.set("disable-features", "Translate,BackForwardCache,AcceptCHFrame,MediaRouter,OptimizationHints,InterestFeedContentSuggestions")
	fs.set("metrics-recording-only", true)
	fs.set("no-service-autorun", true)
	fs.set("password-store", "basic")
	fs.set("use-mock-keychain", true)
	fs.set("mute-audio", true)
	fs.set("hide-scrollbars", true)
	// An animation that pauses when the tab is not focused would make the
	// sweep see a frozen page.
	fs.set("autoplay-policy", "no-user-gesture-required")
	fs.set("lang", strings.SplitN(opts.AcceptLanguage, ",", 2)[0])

	if opts.Headless {
		fs.set("headless", "new")
	}
	if opts.NoSandbox {
		fs.add(chromedp.NoSandbox, "--no-sandbox")
	}
	if opts.Proxy != "" {
		fs.set("proxy-server", opts.Proxy)
	}
	if rules := hostResolverRules(opts.BlockHosts); rules != "" {
		// Refusing to resolve a tracker is both faster and quieter than
		// intercepting its request after the connection is already open.
		//
		// The rule string itself is not recorded: it is thousands of characters
		// of blocklist. The trace carries a hash of the host list instead, which
		// is what actually needs to match between two runs.
		fs.opts = append(fs.opts, chromedp.Flag("host-resolver-rules", rules))
		fs.desc = append(fs.desc, "--host-resolver-rules=<blocklist>")
	}
	// Software rendering is not a fallback here, it is the correct choice: a
	// headless GPU path is the least predictable part of Chromium across
	// machines, and identical extraction output on every machine is a stated
	// goal. WebGL still works, it is just rasterised by SwiftShader.
	//
	// --in-process-gpu deliberately absent. Combined with SwiftShader it stops
	// Chromium producing frames for any tab beyond the first, which starves
	// requestAnimationFrame and therefore IntersectionObserver -- the mechanism
	// almost every scroll-reveal animation is built on. A sweep under that
	// combination scrolls past content that never becomes visible and reports a
	// rich page as nearly empty. See TestFrameProduction.
	fs.set("use-gl", "swiftshader")
	fs.set("enable-unsafe-swiftshader", true)
	if runtime.GOOS == "linux" {
		fs.set("disable-dev-shm-usage", true)
	}

	allocCtx, allocCancel := chromedp.NewExecAllocator(ctx, fs.opts...)

	ctxOpts := []chromedp.ContextOption{}
	if opts.Logf != nil {
		ctxOpts = append(ctxOpts, chromedp.WithLogf(func(s string, a ...any) {
			opts.Logf("chrome: "+s, a...)
		}))
	}
	baseCtx, baseCancel := chromedp.NewContext(allocCtx, ctxOpts...)

	// Force the process to start now so a bad binary fails here with a clear
	// error rather than midway through the first page.
	if err := chromedp.Run(baseCtx); err != nil {
		baseCancel()
		allocCancel()
		return nil, fmt.Errorf("start browser %q: %w", path, err)
	}

	b := &Browser{
		allocCtx:    allocCtx,
		allocCancel: allocCancel,
		baseCtx:     baseCtx,
		baseCancel:  baseCancel,
		opts:        opts,
		flags:       fs.flags(),
	}
	if v, err := browserVersion(baseCtx); err == nil {
		b.chromiumVersion = v
	}
	opts.logf("browser started: %s (%s)", path, b.chromiumVersion)
	return b, nil
}

// browserVersion asks the browser what it is. Pinning matters here: an upgrade
// that changes rendering silently changes extraction output, and an artifact
// that does not record the build cannot be compared against one produced by a
// different build.
func browserVersion(ctx context.Context) (string, error) {
	var version string
	err := chromedp.Run(ctx, chromedp.ActionFunc(func(c context.Context) error {
		// The first return is the CDP protocol version ("1.3"), not the browser
		// build. Recording that would make every trace claim the same Chromium,
		// which is exactly the field a rendering regression needs to compare.
		_, product, revision, _, _, err := browser.GetVersion().Do(c)
		if err != nil {
			return err
		}
		version = product
		if revision != "" {
			version += " (" + revision + ")"
		}
		return nil
	}))
	return version, err
}

// flagSet accumulates browser options and, in parallel, a readable record of
// what they were.
//
// chromedp's option type is an opaque function with no way to read back what it
// set, so the record has to be built as the flags are. It is worth the small
// duplication: "which flags were set" is the first question every rendering bug
// asks, and the --in-process-gpu frame-starvation incident was exactly that
// question asked with no way to answer it from an artifact.
type flagSet struct {
	opts []chromedp.ExecAllocatorOption
	desc []string
}

func (f *flagSet) set(name string, value any) {
	f.opts = append(f.opts, chromedp.Flag(name, value))
	switch v := value.(type) {
	case bool:
		if v {
			f.desc = append(f.desc, "--"+name)
		}
	default:
		f.desc = append(f.desc, fmt.Sprintf("--%s=%v", name, value))
	}
}

func (f *flagSet) add(o chromedp.ExecAllocatorOption, describedAs string) {
	f.opts = append(f.opts, o)
	if describedAs != "" {
		f.desc = append(f.desc, describedAs)
	}
}

func (f *flagSet) flags() []string {
	out := append([]string(nil), f.desc...)
	sort.Strings(out)
	return out
}

// SetOptions adjusts per-sweep settings on a running browser.
//
// The flags a Chromium process was launched with cannot change, so only the
// sweep-level knobs are honoured -- checkpoint counts, budgets, gates. This is
// what lets one warm browser serve both a single post-settle capture and a full
// sweep without paying process startup twice.
func (b *Browser) SetOptions(o Options) {
	b.mu.Lock()
	defer b.mu.Unlock()
	launch := b.opts
	// Preserve everything that was fixed at launch.
	//
	// Chromium's own flags cannot change after the process starts, so only the
	// sweep-level knobs are honoured.
	o.ChromePath = launch.ChromePath
	o.Headless = launch.Headless
	o.NoSandbox = launch.NoSandbox
	o.Proxy = launch.Proxy
	o.BlockHosts = launch.BlockHosts
	o.AcceptLanguage = launch.AcceptLanguage
	o.UserAgent = launch.UserAgent
	if o.Logf == nil {
		o.Logf = launch.Logf
	}
	b.opts = o
}

// Close shuts the browser down. It is safe to call more than once.
func (b *Browser) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	b.baseCancel()
	b.allocCancel()
}

// hostResolverRules turns a host blocklist into Chromium's resolver rule
// syntax. `~NOTFOUND` makes the lookup fail, which the page sees as an
// unreachable host: the same outcome as a blocked request, reached before any
// socket is opened.
func hostResolverRules(hosts []string) string {
	if len(hosts) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, h := range hosts {
		h = strings.TrimSpace(h)
		if h == "" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString("MAP ")
		sb.WriteString(h)
		sb.WriteString(" ~NOTFOUND")
	}
	return sb.String()
}

// findChromium looks for a usable browser in the places one is normally
// installed, preferring stable Chrome, then Chromium, then Edge.
func findChromium() string {
	var candidates []string
	switch runtime.GOOS {
	case "windows":
		var roots []string
		for _, env := range []string{"PROGRAMFILES", "PROGRAMFILES(X86)", "LOCALAPPDATA"} {
			if v := os.Getenv(env); v != "" {
				roots = append(roots, v)
			}
		}
		rel := []string{
			`Google\Chrome\Application\chrome.exe`,
			`Chromium\Application\chrome.exe`,
			`Microsoft\Edge\Application\msedge.exe`,
		}
		for _, r := range rel {
			for _, root := range roots {
				candidates = append(candidates, filepath.Join(root, r))
			}
		}
	case "darwin":
		candidates = []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
			filepath.Join(os.Getenv("HOME"), "Applications/Google Chrome.app/Contents/MacOS/Google Chrome"),
		}
	default:
		candidates = []string{
			"/usr/bin/google-chrome",
			"/usr/bin/google-chrome-stable",
			"/usr/bin/chromium",
			"/usr/bin/chromium-browser",
			"/snap/bin/chromium",
			"/usr/bin/microsoft-edge",
		}
	}
	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			return c
		}
	}
	// Fall back to whatever is on PATH.
	for _, name := range []string{"google-chrome", "chromium", "chromium-browser", "chrome", "msedge"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

// ChromiumPath reports the browser sieve would use, for `sieve doctor`.
func ChromiumPath(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if v := os.Getenv("SIEVE_CHROME"); v != "" {
		return v
	}
	return findChromium()
}
