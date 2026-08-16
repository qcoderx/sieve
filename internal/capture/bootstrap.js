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

  // Announce ourselves to three.js.
  //
  // three.js checks for __THREE_DEVTOOLS__ inside the Scene and WebGLRenderer
  // constructors and, when it exists, dispatches an "observe" event carrying
  // the object. That is the only dependable way into a scene built inside a
  // bundled ES module, which puts nothing on window at all: without it the
  // scene walk has to guess by scanning globals, and on a modern build there
  // is nothing there to find.
  //
  // igloo.inc is the case in point. Its entire site -- every paragraph -- is
  // drawn as MSDF glyph geometry inside the scene, and the page serves an
  // empty <body>. Scanning globals found nothing, so the artifact reported a
  // page with no words on it while a reader could see several hundred.
  //
  // This has to be installed before any page script runs, which is what this
  // file is for. It is inert: an object with a dispatchEvent that records
  // what it is handed. Nothing is read back until the sweep asks.
  try {
    if (!window.__THREE_DEVTOOLS__) {
      var scenes = [];
      window.__THREE_DEVTOOLS__ = {
        scenes: scenes,
        // Counts every dispatch, not just the ones carrying a scene.
        //
        // three.js touches this hook from the WebGLRenderer constructor as
        // well as from Scene, so a non-zero count means three.js is on the
        // page and running -- which is knowable well before any scene has
        // finished being built. That distinction is the difference between
        // waiting for a scene that is coming and waiting on a page that has a
        // canvas for some entirely unrelated reason, and the second is most
        // pages with a canvas on them.
        observed: 0,
        dispatchEvent: function (e) {
          try {
            window.__THREE_DEVTOOLS__.observed++;
            var d = e && e.detail;
            if (d && d.isScene && scenes.indexOf(d) < 0 && scenes.length < 64) {
              scenes.push(d);
            }
          } catch (err) {}
        },
        addEventListener: function () {},
        removeEventListener: function () {},
      };
    }
  } catch (e) {}

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
