package render

import (
	"context"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// FrameProbe reports whether the browser is actually producing frames.
type FrameProbe struct {
	// Ticks is how many animation frames fired within the probe window.
	Ticks int `json:"ticks"`
	// IO reports whether an IntersectionObserver callback was delivered.
	IO bool `json:"intersection_observer"`
	// Elapsed is how long the probe took.
	Elapsed time.Duration `json:"elapsed"`
}

// ChromiumVersion reports the browser build, for diagnostics and traces.
func (b *Browser) ChromiumVersion() string { return b.chromiumVersion }

// buildProbePage constructs the probe DOM in the page rather than navigating to
// a data: URL.
//
// A data: URL would be simpler and it is a trap: everything after the first '#'
// is parsed as a fragment, so any CSS containing an id selector or a hex colour
// silently truncates the document. The probe then finds no element, reports that
// IntersectionObserver is broken, and sends the user chasing a rendering bug
// that does not exist.
const buildProbePage = `(function(){
  document.body.innerHTML = '';
  var pad = document.createElement('div');
  pad.style.height = '250vh';
  var target = document.createElement('div');
  target.id = 'sieve-probe-target';
  target.style.height = '80px';
  target.style.background = 'rgb(51,51,51)';
  document.body.style.margin = '0';
  document.body.appendChild(pad);
  document.body.appendChild(target);
  window.scrollTo(0, 0);
  return true;
})()`

// ProbeFrameProduction verifies the least obvious precondition in the whole
// renderer.
//
// A headless tab that is not being composited never runs the rendering steps.
// requestAnimationFrame stops firing, and so does IntersectionObserver, which
// is what nearly every scroll-reveal animation uses to decide when to show
// content. Under that condition a sweep completes quickly, reports no errors,
// and produces an artifact containing the hero and nothing else -- the exact
// failure this project exists to prevent, arriving silently.
//
// The probe runs on a *secondary* tab on purpose. That is where the failure
// occurs: the first tab a browser opens composites fine, so a check that used
// it would pass on a machine where every real sweep is starved.
func (b *Browser) ProbeFrameProduction(ctx context.Context) (FrameProbe, error) {
	var out FrameProbe
	start := time.Now()

	tabCtx, cancel := chromedp.NewContext(b.baseCtx)
	defer cancel()
	if err := chromedp.Run(tabCtx); err != nil {
		return out, err
	}

	await := func(p *runtime.EvaluateParams) *runtime.EvaluateParams {
		return p.WithAwaitPromise(true)
	}

	pctx, pcancel := context.WithTimeout(tabCtx, 30*time.Second)
	defer pcancel()

	err := chromedp.Run(pctx,
		chromedp.ActionFunc(func(c context.Context) error {
			// Activating the tab is the fix for one of the two known causes, so
			// the probe applies it: what is being tested is whether frames flow
			// under the conditions a real sweep runs in.
			_ = page.BringToFront().Do(c)
			return nil
		}),
		chromedp.Navigate("about:blank"),
		chromedp.Evaluate(buildProbePage, nil),
		chromedp.Evaluate(rafProbeJS, &out.Ticks, await),
		chromedp.Evaluate(ioProbeJS, &out.IO, await),
	)
	out.Elapsed = time.Since(start)
	return out, err
}

// rafProbeJS counts animation frames, resolving with -1 if none arrive.
const rafProbeJS = `new Promise(function(r){
  var n = 0;
  var done = false;
  function tick(){ if(done) return; n++; if(n < 6) requestAnimationFrame(tick); else { done = true; r(n); } }
  requestAnimationFrame(tick);
  setTimeout(function(){ if(!done){ done = true; r(n); } }, 4000);
})`

// ioProbeJS scrolls an off-screen element into view and reports whether the
// observer fired. This is the signal that actually matters: rAF ticking but
// IntersectionObserver never delivering would still lose every reveal.
const ioProbeJS = `new Promise(function(r){
  var el = document.getElementById('sieve-probe-target');
  if(!el){ r(false); return; }
  var done = false;
  var io = new IntersectionObserver(function(es){
    if(done) return;
    for(var i=0;i<es.length;i++){
      if(es[i].isIntersecting){ done = true; io.disconnect(); r(true); return; }
    }
  });
  io.observe(el);
  // Scroll only after the observer is attached and has had a frame to deliver
  // its initial not-intersecting callback. Scrolling first can let the element
  // already be in view when observation starts, which some engines report in
  // the initial callback and others do not -- and a probe that depends on which
  // is a probe that reports a working browser as broken.
  requestAnimationFrame(function(){
    el.scrollIntoView({block: 'center'});
  });
  setTimeout(function(){ if(!done){ done = true; r(false); } }, 5000);
})`
