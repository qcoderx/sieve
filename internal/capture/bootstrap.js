// bootstrap.js is installed with Page.addScriptToEvaluateOnNewDocument, so it
// runs before any page script on every document and every frame.
//
// Its only job is to observe things that cannot be recovered after the fact.
// A canvas does not report which rendering context it handed out, so the
// decision of whether a canvas is a WebGL scene worth recovering or a 2D
// scratch buffer has to be recorded at the moment getContext is called.
//
// The hook is strictly additive: it records and delegates. It must never change
// what the page receives, or the capture stops describing the real page.
(function () {
  if (window.__sieveBoot) return;
  window.__sieveBoot = true;

  try {
    var ctxOf = new WeakMap();
    var proto = HTMLCanvasElement.prototype;
    var real = proto.getContext;
    if (typeof real === "function") {
      proto.getContext = function (type) {
        var out = real.apply(this, arguments);
        try {
          if (out && !ctxOf.has(this)) ctxOf.set(this, String(type));
        } catch (e) {}
        return out;
      };
      window.__sieveCanvasCtx = function (el) {
        try {
          return ctxOf.get(el) || "";
        } catch (e) {
          return "";
        }
      };
    }
  } catch (e) {}

  // Record whether the document ever mutated after load, which the sweep uses
  // as a cheap secondary settle signal alongside layout stability.
  //
  // Structural mutations are counted separately from all mutations, and the
  // distinction is what makes the sweep affordable. An animated page fires
  // thousands of attribute mutations per second as transforms are rewritten,
  // so the total is useless as a cache key -- it never stops moving. But the
  // per-capture indexes that cost real time (the disclosure map, the
  // page-landmark totals) depend only on which elements exist, and that
  // changes rarely. Counting element insertions and removals on their own
  // gives those caches a key that is stable across an entire scroll sweep and
  // still invalidates the moment a framework mounts a new section.
  try {
    window.__sieveMutations = 0;
    window.__sieveStructMutations = 0;
    var mo = new MutationObserver(function (recs) {
      window.__sieveMutations += recs.length;
      for (var i = 0; i < recs.length; i++) {
        var r = recs[i];
        if (r.addedNodes.length || r.removedNodes.length) {
          window.__sieveStructMutations++;
        }
      }
    });
    var start = function () {
      try {
        // childList only.
        //
        // Observing attributes and character data as well meant that on a
        // scroll-animated site -- the exact class of page this tool exists for
        // -- the observer callback ran on every frame for every element whose
        // transform had been rewritten, charging the page's own animation loop
        // for sieve's bookkeeping. The counter it feeds is a settle hint, and
        // the settle signature already samples geometry and opacity directly,
        // which is a better signal than "something somewhere changed" and costs
        // the page nothing.
        mo.observe(document.documentElement, {
          childList: true,
          subtree: true,
        });
      } catch (e) {}
    };
    if (document.documentElement) start();
    else document.addEventListener("readystatechange", start, { once: true });
  } catch (e) {}
})();
