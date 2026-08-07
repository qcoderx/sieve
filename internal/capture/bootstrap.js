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
  try {
    window.__sieveMutations = 0;
    var mo = new MutationObserver(function (recs) {
      window.__sieveMutations += recs.length;
    });
    var start = function () {
      try {
        mo.observe(document.documentElement, {
          childList: true,
          subtree: true,
          attributes: true,
          characterData: true,
        });
      } catch (e) {}
    };
    if (document.documentElement) start();
    else document.addEventListener("readystatechange", start, { once: true });
  } catch (e) {}
})();
