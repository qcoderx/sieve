package render

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"

	"github.com/qcoderx/sieve/internal/capture"
	"github.com/qcoderx/sieve/internal/corroborate"
)

// collector holds the state that accumulates outside the checkpoint loop:
// network observations, intercepted scene-graph bodies, canvas rasters, and the
// verdict of the navigation guard.
type collector struct {
	opts Options
	res  *Result
	acc  *capture.Accumulator

	// corpus is the confirm-only membership index. It exists so canvas recovery
	// can tell a real headline from an invented one; it is never a source of
	// content.
	corpus *corroborate.Index

	mu         sync.Mutex
	assetByURL map[string]*Asset
	assetBytes int64
	pending    map[network.RequestID]string // requestID -> url, for bodies worth fetching
	corpusWant map[network.RequestID]string // requestID -> kind, for corroboration text
	corpusRead int64
	blocked    error
	statusSet  bool

	// wg tracks the goroutines spawned to answer paused requests and to pull
	// response bodies. finish waits on it so nothing is still writing when the
	// result is read.
	wg sync.WaitGroup
}

func newCollector(opts Options, res *Result) *collector {
	c := &collector{
		opts:       opts,
		res:        res,
		acc:        capture.NewAccumulator(),
		assetByURL: map[string]*Asset{},
		pending:    map[network.RequestID]string{},
		corpusWant: map[network.RequestID]string{},
	}
	if opts.CollectCorpus {
		c.corpus = corroborate.New(opts.MaxCorpusBytes)
	}
	return c
}

// maxCorpusBody bounds a single response fed into the corroboration index.
// Bundles are large; a single 40MB source map should not consume the whole
// budget and crowd out the hydration blob that actually matters.
const maxCorpusBody = 4 << 20

// attach subscribes to the CDP event stream for this tab.
//
// Event handlers run on chromedp's single event goroutine. Blocking one stalls
// every subsequent event for the tab, so any handler that needs to issue a CDP
// command hands off to a goroutine and returns immediately.
func (c *collector) attach(tabCtx context.Context, guard NavGuard) {
	chromedp.ListenTarget(tabCtx, func(ev any) {
		switch e := ev.(type) {

		case *fetch.EventRequestPaused:
			c.wg.Add(1)
			go c.resolvePaused(tabCtx, e, guard)

		case *network.EventResponseReceived:
			if e.Type == network.ResourceTypeDocument && !c.statusSet {
				c.mu.Lock()
				if !c.statusSet {
					c.statusSet = true
					c.res.Status = e.Response.Status
				}
				c.mu.Unlock()
				c.checkBlocked(e.Response.Status, e.Response.Headers)
			}
			if c.opts.CollectAssets &&
				(isSceneGraphURL(e.Response.URL) || isSceneGraphMIME(e.Response.MimeType)) {
				c.mu.Lock()
				c.pending[e.RequestID] = e.Response.URL
				c.mu.Unlock()
				return
			}
			// Scripts and JSON feed the confirm-only corroboration index. Their
			// bodies never become content; they exist so a string recovered
			// from pixels can be checked against what the site actually shipped.
			if c.corpus != nil {
				if kind := corpusKind(e.Type, e.Response.MimeType, e.Response.URL); kind != "" {
					c.mu.Lock()
					if c.corpusRead < int64(c.opts.MaxCorpusBytes) {
						c.corpusWant[e.RequestID] = kind
					}
					c.mu.Unlock()
				}
			}

		case *network.EventLoadingFinished:
			c.mu.Lock()
			u, isAsset := c.pending[e.RequestID]
			if isAsset {
				delete(c.pending, e.RequestID)
			}
			kind, isCorpus := c.corpusWant[e.RequestID]
			if isCorpus {
				delete(c.corpusWant, e.RequestID)
			}
			over := c.assetBytes >= c.opts.MaxAssetsTotal
			c.mu.Unlock()

			if isAsset && !over {
				c.wg.Add(1)
				go c.pullBody(tabCtx, e.RequestID, u)
			}
			if isCorpus {
				c.wg.Add(1)
				go c.pullCorpus(tabCtx, e.RequestID, kind)
			}

		case *network.EventLoadingFailed:
			c.mu.Lock()
			delete(c.pending, e.RequestID)
			delete(c.corpusWant, e.RequestID)
			c.mu.Unlock()
		}
	})
}

// corpusKind decides whether a response is worth indexing for corroboration,
// and labels where it came from.
func corpusKind(t network.ResourceType, mime, url string) string {
	switch t {
	case network.ResourceTypeScript:
		return "script"
	case network.ResourceTypeXHR, network.ResourceTypeFetch:
		if strings.Contains(strings.ToLower(mime), "json") {
			return "api"
		}
		if strings.Contains(strings.ToLower(mime), "text") {
			return "api"
		}
	}
	return ""
}

// pullCorpus retrieves a body and folds its readable strings into the
// membership index. The body itself is discarded immediately.
func (c *collector) pullCorpus(tabCtx context.Context, id network.RequestID, kind string) {
	defer c.wg.Done()
	var body []byte
	err := chromedp.Run(tabCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		b, err := network.GetResponseBody(id).Do(ctx)
		if err != nil {
			return err
		}
		body = b
		return nil
	}))
	if err != nil || len(body) == 0 {
		return
	}
	if len(body) > maxCorpusBody {
		body = body[:maxCorpusBody]
	}

	c.mu.Lock()
	if c.corpusRead >= int64(c.opts.MaxCorpusBytes) {
		c.mu.Unlock()
		return
	}
	c.corpusRead += int64(len(body))
	c.mu.Unlock()

	if kind == "script" {
		c.corpus.AddScript(kind, string(body))
	} else {
		c.corpus.AddText(kind, string(body))
	}
}

// blockSignals are the response signatures that mean a site refused this
// client rather than failing. Detecting it matters because "the site said no"
// and "the site is broken" call for different behaviour: the first degrades to
// a labelled partial artifact, the second is an error.
var blockSignals = []struct {
	header string
	value  string
	name   string
}{
	{"cf-mitigated", "challenge", "Cloudflare challenge"},
	{"server", "cloudflare", ""},
	{"x-datadome", "", "DataDome"},
	{"x-iinfo", "", "Imperva Incapsula"},
	{"x-akamai-transformed", "", ""},
	{"x-sucuri-id", "", "Sucuri"},
}

func (c *collector) checkBlocked(status int64, headers network.Headers) {
	var reason string
	switch status {
	case 403:
		reason = "HTTP 403"
	case 429:
		reason = "HTTP 429 rate limited"
	case 503:
		reason = "HTTP 503"
	}
	for _, sig := range blockSignals {
		for k, v := range headers {
			if !strings.EqualFold(k, sig.header) {
				continue
			}
			sv, _ := v.(string)
			if sig.value != "" && !strings.Contains(strings.ToLower(sv), sig.value) {
				continue
			}
			if sig.name != "" && reason != "" {
				reason += " (" + sig.name + ")"
			} else if sig.name != "" && status >= 400 {
				reason = sig.name
			}
		}
	}
	if reason == "" {
		return
	}
	c.mu.Lock()
	if !c.res.Blocked {
		c.res.Blocked = true
		c.res.BlockedReason = reason
	}
	c.mu.Unlock()
}

// resolvePaused vets a paused document request and either lets it through or
// fails it. Every paused request must be answered exactly once or the page
// hangs until the navigation timeout.
func (c *collector) resolvePaused(tabCtx context.Context, e *fetch.EventRequestPaused, guard NavGuard) {
	defer c.wg.Done()

	allow := true
	var reason error
	if guard != nil {
		u, err := url.Parse(e.Request.URL)
		if err != nil {
			allow, reason = false, fmt.Errorf("unparseable URL %q: %w", e.Request.URL, err)
		} else if err := guard(u); err != nil {
			allow, reason = false, err
		}
	}

	if allow {
		_ = chromedp.Run(tabCtx, chromedp.ActionFunc(func(ctx context.Context) error {
			return fetch.ContinueRequest(e.RequestID).Do(ctx)
		}))
		return
	}

	c.mu.Lock()
	if c.blocked == nil {
		c.blocked = fmt.Errorf("navigation to %s refused: %w", e.Request.URL, reason)
	}
	c.mu.Unlock()
	c.opts.logf("blocked navigation: %s (%v)", e.Request.URL, reason)

	_ = chromedp.Run(tabCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		return fetch.FailRequest(e.RequestID, network.ErrorReasonBlockedByClient).Do(ctx)
	}))
}

// blockedErr reports the guard's verdict, if it refused anything. Navigation
// failing because the guard blocked it is a very different error from
// navigation failing because the site is down, and the caller needs to be able
// to tell them apart.
func (c *collector) blockedErr() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.blocked
}

func (c *collector) pullBody(tabCtx context.Context, id network.RequestID, u string) {
	defer c.wg.Done()
	var body []byte
	err := chromedp.Run(tabCtx, chromedp.ActionFunc(func(ctx context.Context) error {
		b, err := network.GetResponseBody(id).Do(ctx)
		if err != nil {
			return err
		}
		body = b
		return nil
	}))
	if err != nil || len(body) == 0 {
		return
	}
	if int64(len(body)) > c.opts.MaxAssetBytes {
		c.opts.logf("scene asset %s skipped: %d bytes exceeds per-asset cap", u, len(body))
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.assetBytes+int64(len(body)) > c.opts.MaxAssetsTotal {
		return
	}
	if _, dup := c.assetByURL[u]; dup {
		return
	}
	c.assetBytes += int64(len(body))
	c.assetByURL[u] = &Asset{URL: u, Body: body}
}

func (c *collector) assets() []Asset {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Asset, 0, len(c.assetByURL))
	for _, a := range c.assetByURL {
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].URL < out[j].URL })
	return out
}

func (c *collector) finish(tabCtx context.Context) {
	// Give outstanding body pulls a chance to land, but never block the whole
	// distillation on a browser that has stopped answering.
	done := make(chan struct{})
	go func() { c.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-tabCtx.Done():
	}
}

// considerCanvases rasterises canvas regions that are large enough to be
// carrying content, at most once per canvas, at the checkpoint where the canvas
// filled the most of the viewport.
//
// The screenshot is of the viewport, cropped in Go. Asking Chromium for a
// clipped capture would save a little transfer, but the clip rectangle's frame
// of reference has moved between Chromium versions, and a crop computed here
// from the rectangle the page itself reported is exact and stays exact.
func (c *collector) considerCanvases(ctx context.Context, snap *capture.Snapshot, cp int) {
	if !c.opts.CaptureCanvas {
		return
	}
	var want []capture.Canvas
	for _, cv := range snap.Canvases {
		if cv.ViewportShare < c.opts.CanvasShareGate {
			continue
		}
		if prev, ok := c.res.CanvasShots[cv.Path]; ok && prev.Share >= cv.ViewportShare {
			continue
		}
		// A canvas mostly outside the viewport crops to a sliver.
		if cv.ViewBox.Bottom() <= 0 || cv.ViewBox.Y() >= snap.ViewportH {
			continue
		}
		want = append(want, cv)
	}
	if len(want) == 0 {
		return
	}

	// Page.captureScreenshot waits for the compositor to produce a frame, and a
	// tab that has stopped compositing never produces one. Without a deadline
	// here a single unlucky canvas would consume the entire job budget, so the
	// screenshot is treated as optional: if it does not arrive quickly, the
	// sweep carries on without it.
	shotCtx, cancelShot := context.WithTimeout(ctx, 10*time.Second)
	defer cancelShot()

	var shot []byte
	err := chromedp.Run(shotCtx, chromedp.ActionFunc(func(c2 context.Context) error {
		b, err := page.CaptureScreenshot().
			WithFormat(page.CaptureScreenshotFormatPng).
			WithCaptureBeyondViewport(false).
			Do(c2)
		if err != nil {
			return err
		}
		shot = b
		return nil
	}))
	if err != nil || len(shot) == 0 {
		c.opts.logf("canvas screenshot failed at checkpoint %d: %v", cp, err)
		return
	}
	img, err := png.Decode(bytes.NewReader(shot))
	if err != nil {
		return
	}

	scale := c.opts.DeviceScale
	if scale <= 0 {
		scale = 1
	}
	for _, cv := range want {
		crop, ok := cropTo(img, cv.ViewBox, scale)
		if !ok {
			continue
		}
		var buf bytes.Buffer
		if err := png.Encode(&buf, crop); err != nil {
			continue
		}
		c.res.CanvasShots[cv.Path] = &CanvasShot{
			Path:       cv.Path,
			PNG:        buf.Bytes(),
			Share:      cv.ViewportShare,
			Checkpoint: cp,
			Uniform:    isUniform(crop),
		}
	}
}

// cropTo cuts the canvas rectangle out of a viewport screenshot, converting CSS
// pixels to device pixels and clamping to the image.
func cropTo(img image.Image, box capture.Box, scale float64) (image.Image, bool) {
	b := img.Bounds()
	x0 := b.Min.X + int(box.X()*scale)
	y0 := b.Min.Y + int(box.Y()*scale)
	x1 := x0 + int(box.W()*scale)
	y1 := y0 + int(box.H()*scale)
	if x0 < b.Min.X {
		x0 = b.Min.X
	}
	if y0 < b.Min.Y {
		y0 = b.Min.Y
	}
	if x1 > b.Max.X {
		x1 = b.Max.X
	}
	if y1 > b.Max.Y {
		y1 = b.Max.Y
	}
	if x1-x0 < 8 || y1-y0 < 8 {
		return nil, false
	}
	type subImager interface {
		SubImage(r image.Rectangle) image.Image
	}
	si, ok := img.(subImager)
	if !ok {
		return nil, false
	}
	return si.SubImage(image.Rect(x0, y0, x1, y1)), true
}

// isUniform reports whether every sampled pixel is the same colour. A flat
// canvas is a background wash, and running a vision model over it would spend
// the most expensive call in the pipeline to be told the image is grey.
func isUniform(img image.Image) bool {
	b := img.Bounds()
	if b.Dx() < 2 || b.Dy() < 2 {
		return true
	}
	const grid = 12
	var r0, g0, b0 uint32
	first := true
	for i := 0; i < grid; i++ {
		for j := 0; j < grid; j++ {
			x := b.Min.X + b.Dx()*i/grid
			y := b.Min.Y + b.Dy()*j/grid
			r, g, bb, _ := img.At(x, y).RGBA()
			if first {
				r0, g0, b0 = r, g, bb
				first = false
				continue
			}
			// 8-bit tolerance on 16-bit channels: a gradient counts as content,
			// dithering noise does not.
			if absDiff(r, r0) > 0x0800 || absDiff(g, g0) > 0x0800 || absDiff(bb, b0) > 0x0800 {
				return false
			}
		}
	}
	return true
}

func absDiff(a, b uint32) uint32 {
	if a > b {
		return a - b
	}
	return b - a
}
