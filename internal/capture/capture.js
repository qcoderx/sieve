// capture.js installs window.__sieve in the page. The scroll sweep calls
// capture(n) once per checkpoint; the other entry points are used once each.
//
// The whole extraction is one top-down walk of the composed tree. Inherited
// facts -- effective opacity, effective background colour, the enclosing
// landmark, the enclosing link, the nearest block-level ancestor -- are carried
// *down* as arguments rather than recovered by walking *up* from each node.
// That turns an O(nodes x depth) extraction into an O(nodes) one, which is the
// difference between a 60ms and a 900ms checkpoint on a page with 40,000
// elements.
//
// The walk reads layout but never writes to the DOM, so the browser computes
// layout once for the whole pass instead of thrashing.
//
// The result is returned as a JSON string. Letting CDP serialise a deep object
// graph by value is dramatically slower than stringifying in-page and parsing
// once in Go.
(function () {
  if (window.__sieve) return;

  // The opacity a run must reach to count as legible. Defined here because both
  // the walk and the sweep read it, and two thresholds that drifted apart would
  // make the audit compare different populations.
  var MIN_VISIBLE_OPACITY = 0.12;

  var WS = /\s+/g;
  var URLISH = /^(https?:|mailto:|tel:|data:)/i;

  // Elements that never carry readable content, or whose text is machinery.
  var SKIP = {
    SCRIPT: 1, STYLE: 1, NOSCRIPT: 1, TEMPLATE: 1, HEAD: 1, META: 1, LINK: 1,
    TITLE: 1, BASE: 1, PARAM: 1, TRACK: 1, SOURCE: 1, MAP: 1, AREA: 1,
    DEFS: 1, CLIPPATH: 1, LINEARGRADIENT: 1, RADIALGRADIENT: 1, FILTER: 1,
  };

  // Computed `display` values that start a new run of text.
  //
  // The nearest such ancestor is the grouping key for split-text reassembly,
  // so the set is deliberately narrower than "establishes a block formatting
  // context". inline-block is excluded: text-splitting plugins wrap every word
  // and every character in an inline-block, and treating those as boundaries
  // would leave the reassembly with nothing to reassemble.
  var BLOCKISH = {
    block: 1, "flow-root": 1, flex: 1, grid: 1, "list-item": 1,
    table: 1, "table-cell": 1, "table-row": 1, "table-caption": 1,
  };

  var LANDMARK_TAG = {
    NAV: "nav", HEADER: "header", FOOTER: "footer", MAIN: "main",
    ASIDE: "aside", FORM: "form", DIALOG: "dialog",
  };

  // Sectioning elements scope <header> and <footer>.
  //
  // This is the HTML rule and it is load-bearing here. A <header> inside an
  // <article> or <section> is that section's own heading area -- ordinary
  // content -- and only a <header> whose nearest sectioning ancestor is the
  // body is the page banner. The ARIA spec says the same thing: the implicit
  // banner and contentinfo roles apply only when the element is not nested in
  // sectioning content.
  //
  // Treating every <header> as page chrome is catastrophic on a well-structured
  // modern site: pear.no wraps each of its sections in <header>/<footer>, and
  // the result was an artifact reporting zero content blocks for a page whose
  // body copy had been extracted perfectly and then filed as furniture.
  var SECTIONING = { ARTICLE: 1, SECTION: 1, ASIDE: 1, NAV: 1, MAIN: 1 };
  var LANDMARK_ROLE = {
    navigation: "nav", banner: "header", contentinfo: "footer",
    main: "main", complementary: "aside", form: "form", search: "form",
    dialog: "dialog", alertdialog: "dialog", menu: "nav", menubar: "nav",
  };

  // countPageLandmarks counts the <header> and <footer> elements that sit
  // outside sectioning content, which are the only ones that could be the
  // page's banner and contentinfo.
  //
  // The walk needs the footer total up front: it discovers footers in document
  // order and cannot know which is the last one until it has passed them all,
  // but it has to decide each node's region as it reaches it.
  function countPageLandmarks(doc) {
    var out = { header: 0, footer: 0 };
    var nested = ":not(article *):not(section *):not(aside *):not(nav *):not(main *)";
    try {
      out.header = doc.querySelectorAll("header" + nested).length;
      out.footer = doc.querySelectorAll("footer" + nested).length;
    } catch (e) {
      // A selector engine that rejects the :not() chain leaves the totals at
      // zero, which makes the last-footer test match the first footer only.
    }
    return out;
  }

  var FIELD_TAGS = { INPUT: 1, TEXTAREA: 1, SELECT: 1 };

  // Accessibility metadata is attacker-controlled text on an otherwise visible
  // element, so it bypasses every visibility defence by design. Capping it
  // bounds the channel; the cap being hit is itself recorded.
  var META_TEXT_CAP = 300;

  function norm(s) {
    return s ? s.replace(WS, " ").trim() : "";
  }

  function capText(s) {
    if (!s) return "";
    s = norm(s);
    if (s.length <= META_TEXT_CAP) return s;
    return s.slice(0, META_TEXT_CAP);
  }

  function num(v, dflt) {
    var f = parseFloat(v);
    return isFinite(f) ? f : dflt;
  }

  function weightOf(w) {
    if (w === "normal") return 400;
    if (w === "bold") return 700;
    if (w === "lighter") return 300;
    if (w === "bolder") return 700;
    var n = parseInt(w, 10);
    return isFinite(n) ? n : 400;
  }

  function familyOf(f) {
    if (!f) return "";
    var i = f.indexOf(",");
    var first = i < 0 ? f : f.slice(0, i);
    return first.replace(/^\s*["']?|["']?\s*$/g, "");
  }

  // Icon fonts render glyphs from the Unicode private use areas. Their text
  // content is noise that would otherwise be classified as a heading because
  // icon glyphs are set large.
  function isIconGlyph(s) {
    if (s.length > 3) return false;
    for (var i = 0; i < s.length; i++) {
      var c = s.charCodeAt(i);
      if (c >= 0xe000 && c <= 0xf8ff) return true;
      if (c >= 0xf0000) return true;
    }
    return false;
  }

  // A path segment is the tag plus its index among same-tag siblings. Computing
  // it during the downward walk costs one counter per parent; recovering it
  // afterwards would cost a sibling scan per node.
  function seg(tag, idx) {
    return idx === 0 ? tag : tag + "[" + idx + "]";
  }

  // ---------------------------------------------------------------------------
  // Colour: the white-on-white channel
  //
  // Opacity and `visibility` are the two ways to hide text that the capture
  // already sees. Setting the text colour to the background colour is a third,
  // and it defeats both -- the element is fully opaque, fully visible, and
  // completely unreadable. Effective background is carried down the walk the
  // same way opacity is, so the check costs one comparison per text run.
  // ---------------------------------------------------------------------------

  function parseColor(c) {
    if (!c) return null;
    var m = c.match(/^rgba?\(([^)]+)\)$/);
    if (!m) return null;
    var p = m[1].split(",");
    if (p.length < 3) return null;
    var a = p.length > 3 ? parseFloat(p[3]) : 1;
    return {
      r: parseFloat(p[0]) || 0,
      g: parseFloat(p[1]) || 0,
      b: parseFloat(p[2]) || 0,
      a: isFinite(a) ? a : 1,
    };
  }

  function relLuminance(c) {
    function ch(v) {
      v /= 255;
      return v <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4);
    }
    return 0.2126 * ch(c.r) + 0.7152 * ch(c.g) + 0.0722 * ch(c.b);
  }

  // contrastRatio implements the WCAG formula. A ratio of 1 is identical
  // colours; 21 is black on white. Real body copy is rarely below 3.
  function contrastRatio(fg, bg) {
    if (!fg || !bg) return 21;
    // Composite a translucent foreground over the background first, or an
    // rgba(0,0,0,0.02) text colour would look like high contrast black.
    if (fg.a < 1) {
      fg = {
        r: fg.r * fg.a + bg.r * (1 - fg.a),
        g: fg.g * fg.a + bg.g * (1 - fg.a),
        b: fg.b * fg.a + bg.b * (1 - fg.a),
        a: 1,
      };
    }
    var l1 = relLuminance(fg);
    var l2 = relLuminance(bg);
    if (l1 < l2) {
      var t = l1;
      l1 = l2;
      l2 = t;
    }
    return (l1 + 0.05) / (l2 + 0.05);
  }

  // The threshold for "this text is not rendered to a human at all".
  //
  // It is far below the WCAG 4.5 accessibility bar on purpose. This check is
  // not an accessibility audit; it exists to catch one hiding technique, where
  // the text colour is set to the background colour. Anything above roughly
  // 1.0 is legible to someone.
  //
  // The number matters more than it looks. A dark-themed site setting
  // rgb(29,28,25) on rgb(11,10,9) scores 1.17: genuinely poor contrast, and
  // genuinely rendered text that a reader can see. A threshold of 1.2 excluded
  // every content run on such a page and produced an empty artifact -- turning
  // a defence against hidden text into a defence against dark mode. 1.08 keeps
  // the margin needed to catch near-identical colours without reaching real
  // low-contrast design.
  var INVISIBLE_CONTRAST = 1.08;

  function accessibleName(el, own) {
    var n = el.getAttribute && el.getAttribute("aria-label");
    if (n) return capText(n);
    var lb = el.getAttribute && el.getAttribute("aria-labelledby");
    if (lb) {
      var parts = [];
      var ids = lb.split(/\s+/);
      for (var i = 0; i < ids.length; i++) {
        var r = el.ownerDocument.getElementById(ids[i]);
        if (r) parts.push(norm(r.textContent));
      }
      var joined = capText(parts.join(" "));
      if (joined) return joined;
    }
    if (own) return own;
    var t = norm(el.textContent || "");
    if (t) return capText(t);
    var ti = el.getAttribute && el.getAttribute("title");
    if (ti) return capText(ti);
    // A link or button whose only child is an image borrows the image's alt.
    var img = el.querySelector && el.querySelector("img[alt]");
    if (img) {
      var a = capText(img.getAttribute("alt"));
      if (a) return a;
    }
    return "";
  }

  function labelForField(el) {
    var doc = el.ownerDocument;
    var id = el.getAttribute("id");
    if (id) {
      try {
        var lab = doc.querySelector('label[for="' + CSS.escape(id) + '"]');
        if (lab) {
          var t = capText(lab.textContent);
          if (t) return t;
        }
      } catch (e) {}
    }
    var p = el.parentElement;
    for (var i = 0; p && i < 3; i++, p = p.parentElement) {
      if (p.tagName === "LABEL") {
        var lt = capText(p.textContent);
        if (lt) return lt;
      }
    }
    var al = el.getAttribute("aria-label");
    if (al) return capText(al);
    var ph = el.getAttribute("placeholder");
    if (ph) return capText(ph);
    return "";
  }

  // ---------------------------------------------------------------------------
  // Disclosure map
  //
  // Tab panels and accordion bodies are overwhelmingly already in the DOM as
  // display:none rather than absent from it, and the widgets that hide them
  // announce themselves through ARIA. Indexing the controls once per capture
  // lets every hidden subtree be labelled with the control that reveals it, so
  // the artifact can say "this section sits behind a tab labelled Pricing"
  // instead of silently omitting it.
  // ---------------------------------------------------------------------------

  function buildDisclosureIndex(doc) {
    var byTargetID = Object.create(null);
    var controls = [];
    var els;
    try {
      els = doc.querySelectorAll("[aria-controls],[aria-expanded],details>summary");
    } catch (e) {
      return { byTargetID: byTargetID, controls: controls };
    }
    for (var i = 0; i < els.length && i < 2000; i++) {
      var el = els[i];
      var kind = "disclosure";
      var role = (el.getAttribute("role") || "").toLowerCase();
      if (role === "tab") kind = "tab";
      else if (el.tagName === "SUMMARY") kind = "details";
      else if (el.getAttribute("aria-expanded") !== null) kind = "disclosure";
      else if (el.getAttribute("aria-controls")) kind = "control";

      var expandedAttr = el.getAttribute("aria-expanded");
      var expanded = expandedAttr === null ? null : expandedAttr === "true";
      if (el.tagName === "SUMMARY" && el.parentElement) {
        expanded = el.parentElement.open === true;
      }

      var rec = {
        l: capText(accessibleName(el, "")),
        k: kind,
        e: expanded,
        s: (role === "tab" && el.getAttribute("aria-selected") === "true") || undefined,
      };
      controls.push(rec);

      var target = el.getAttribute("aria-controls");
      if (target) {
        var ids = target.split(/\s+/);
        for (var j = 0; j < ids.length; j++) {
          if (ids[j]) byTargetID[ids[j]] = rec;
        }
      }
      if (el.tagName === "SUMMARY" && el.parentElement) {
        el.parentElement.__sieveCtl = rec;
      }
    }
    return { byTargetID: byTargetID, controls: controls };
  }

  // controlFor finds the widget that would reveal a hidden element: an
  // aria-controls pointing at its id, a <details> ancestor, or the nearest
  // ancestor that declares aria-expanded="false".
  function controlFor(el, ctx) {
    var idx = ctx.disc;
    if (!idx) return null;
    var node = el;
    for (var i = 0; node && i < 12; i++, node = node.parentElement) {
      var id = node.getAttribute && node.getAttribute("id");
      if (id && idx.byTargetID[id]) return idx.byTargetID[id];
      if (node.__sieveCtl) return node.__sieveCtl;
      if (node.tagName === "DETAILS" && node.open === false) {
        var s = node.querySelector("summary");
        return { l: s ? capText(s.textContent) : "", k: "details", e: false };
      }
      if (node.getAttribute && node.getAttribute("aria-expanded") === "false") {
        return { l: capText(accessibleName(node, "")), k: "disclosure", e: false };
      }
    }
    return null;
  }

  function collectMeta(doc, win) {
    var og = {};
    var ld = [];
    try {
      var metas = doc.head ? doc.head.querySelectorAll("meta") : [];
      var desc = "";
      for (var i = 0; i < metas.length; i++) {
        var m = metas[i];
        var prop = m.getAttribute("property") || m.getAttribute("name") || "";
        var c = m.getAttribute("content");
        if (!c) continue;
        var lp = prop.toLowerCase();
        if (lp === "description") desc = capText(c);
        else if (lp.indexOf("og:") === 0 || lp.indexOf("twitter:") === 0) {
          og[lp] = capText(c);
        }
      }
      var scripts = doc.querySelectorAll('script[type="application/ld+json"]');
      for (var j = 0; j < scripts.length && j < 20; j++) {
        var txt = scripts[j].textContent || "";
        // Structured data never renders, so it is a pure metadata channel: an
        // attacker can put a megabyte of anything here and no visitor would
        // see it. It is carried through only so Go can pull a whitelisted set
        // of schema.org fields out of it; the raw text is never emitted.
        if (txt.length > 0 && txt.length < 262144) ld.push(txt);
      }
      var can = doc.querySelector('link[rel="canonical"]');
      return {
        ti: norm(doc.title || ""),
        lg: (doc.documentElement && doc.documentElement.lang) || "",
        de: desc,
        ca: can ? can.href : "",
        u: win.location.href,
        og: og,
        ld: ld,
      };
    } catch (e) {
      return { ti: "", u: "", og: og, ld: ld };
    }
  }

  // ---------------------------------------------------------------------------

  function Collector(budget, latentBudget) {
    this.nodes = [];
    this.latent = [];
    this.actions = [];
    this.media = [];
    this.canvases = [];
    this.budget = budget;
    this.latentBudget = latentBudget;
    this.truncated = false;
    this.latentTruncated = false;
    this.frames = 0;
    this.framesBlocked = 0;
    this.visibleChars = 0;
    this.seenAction = new Set();
    this.seenMedia = new Set();
  }

  Collector.prototype.full = function () {
    if (this.nodes.length >= this.budget) {
      this.truncated = true;
      return true;
    }
    return false;
  };

  /**
   * walk descends one element, emitting whatever it carries and recursing.
   *
   * @param el        element to visit
   * @param path      structural path of el
   * @param blockPath path of the nearest block-level ancestor (including el)
   * @param opacity   product of every ancestor opacity, el's included
   * @param landmark  nearest landmark ancestor descriptor
   * @param href      nearest ancestor link target
   * @param depth     depth in the composed tree
   * @param off       {x,y} offset from this document's origin to the top document's
   * @param reveal    true when this element or an ancestor declares a
   *                  transition or animation that would make it visible
   * @param vis       false when an ancestor set visibility:hidden
   * @param fixed     true when el or an ancestor is position:fixed/sticky
   * @param bg        nearest opaque ancestor background colour
   * @param ctx       shared frame context {win, doc, sx, sy, vw, vh, col, disc}
   */
  function walk(el, path, blockPath, opacity, landmark, href, depth, off, vis, fixed, bg, sectioned, reveal, ctx) {
    var col = ctx.col;
    if (col.full()) return;

    var tag = el.tagName;
    if (!tag) return;
    if (SKIP[tag] === 1) return;

    var cs;
    try {
      cs = ctx.win.getComputedStyle(el);
    } catch (e) {
      return;
    }
    if (!cs) return;

    if (LANDMARK_TAG[tag]) {
      if (tag === "HEADER" || tag === "FOOTER") {
        // A document has at most one banner and one contentinfo.
        //
        // Two rules narrow it. First, <header> and <footer> inside sectioning
        // content are that section's own heading and trailing matter, never
        // page furniture -- that is the HTML rule. Second, when a page uses
        // them repeatedly at top level (a very common way to wrap sections),
        // only the first header and the last footer can be the page's, and the
        // rest are content.
        //
        // Without the second rule pear.no, which wraps nineteen sections in
        // <header>, had its entire body filed as chrome: thirty-four blocks of
        // correctly extracted copy, and an artifact reporting no content at
        // all. It is the difference between a working extraction and an empty
        // one, on any site that structures its page this way.
        var key = tag === "HEADER" ? "header" : "footer";
        var idx = ctx.lmCount[key] || 0;
        ctx.lmCount[key] = idx + 1;
        var isPageLevel =
          !sectioned &&
          (key === "header" ? idx === 0 : idx === (ctx.lmTotals[key] || 1) - 1);
        if (isPageLevel) landmark = key;
      } else {
        landmark = LANDMARK_TAG[tag];
      }
    }
    if (SECTIONING[tag] === 1) sectioned = true;

    var roleAttr = el.getAttribute ? el.getAttribute("role") : null;
    var role = roleAttr ? roleAttr.trim().toLowerCase() : "";
    // An explicit role is the author stating intent outright and overrides the
    // structural inference either way.
    if (role && LANDMARK_ROLE[role]) landmark = LANDMARK_ROLE[role];

    var display = cs.display;
    // display:none removes the subtree from layout entirely. It is not
    // discarded: tab panels and accordion bodies live here, and throwing them
    // away is the single largest source of missing content in a scroll-only
    // extractor. It descends into a cheaper walk that collects text and
    // structure but no geometry, since none exists.
    if (display === "none") {
      var ctl = controlFor(el, ctx);
      walkLatent(el, path, blockPath, landmark, href, depth,
        "display-none", ctl, ctx);
      return;
    }

    var op = opacity * num(cs.opacity, 1);
    var visible = vis && cs.visibility !== "hidden" && cs.visibility !== "collapse";

    // A fixed or sticky element does not live at a document coordinate: it
    // follows the viewport, so naively adding scrollY would place the same
    // header at a different Y at every checkpoint and scatter one navigation
    // across the whole reading order. Such nodes are recorded in viewport
    // coordinates and flagged, and the classifier treats the flag as strong
    // evidence of chrome.
    var pos = cs.position;
    if (pos === "fixed" || pos === "sticky") fixed = true;

    // Carry the nearest opaque background down so a text run can be checked
    // against what is actually behind it without walking back up.
    var ownBG = parseColor(cs.backgroundColor);
    if (ownBG && ownBG.a >= 0.95) bg = ownBG;

    // Inherited downward: a section that fades itself in carries every run
    // inside it into view with it.
    if (!reveal && revealDeclared(cs)) reveal = true;

    if (tag === "A") {
      var h = el.href;
      if (h && URLISH.test(h)) href = h;
    }

    var myBlock = BLOCKISH[display] === 1 ? path : blockPath;

    // -- own text ------------------------------------------------------------
    //
    // "Own text" means the element's direct child text nodes, not its subtree.
    // That granularity is what makes the node count proportional to the amount
    // of text on the page instead of to the size of the DOM.
    var kids = el.childNodes;
    var textKids = null;
    var hasElementChild = false;
    for (var i = 0; i < kids.length; i++) {
      var k = kids[i];
      if (k.nodeType === 3) {
        if (k.data && k.data.length) {
          if (textKids === null) textKids = [];
          textKids.push(k);
        }
      } else if (k.nodeType === 1) {
        hasElementChild = true;
      }
    }

    var env = {
      tag: tag, role: role, landmark: landmark, href: href, cs: cs,
      op: op, visible: visible, fixed: fixed, depth: depth, off: off, bg: bg,
      reveal: reveal,
    };

    if (textKids === null && !hasElementChild) {
      // Leaf elements with no text may still render text through ::before or
      // ::after. Only leaves are probed, because that is where authored pseudo
      // content lives and the probe costs an extra style resolution.
      var pc = pseudoText(ctx.win, el);
      if (pc) emitRun(el, path, myBlock, pc, null, env, ctx, 0);
    } else if (textKids !== null) {
      if (!hasElementChild) {
        // Pure text element: the element's own rectangle is the text's
        // rectangle, and reading it costs one call.
        var joined = "";
        for (var j = 0; j < textKids.length; j++) joined += textKids[j].data;
        emitRun(el, path, myBlock, norm(joined), null, env, ctx, 0);
      } else {
        // Mixed content, as in "<p>Words <a>link</a> more words</p>". The
        // element's rectangle spans the whole paragraph including the link, so
        // using it would place two text fragments at the same coordinates and
        // the reassembly could not order them. Measuring each text node with a
        // Range gives each fragment its true position, at the cost of one
        // Range per fragment -- paid only on elements that actually mix.
        for (var m = 0; m < textKids.length; m++) {
          var raw = textKids[m].data;
          var t2 = norm(raw);
          if (!t2) continue;
          // Whether the fragment was flanked by whitespace has to be recorded
          // here, because normalising strips it and the reassembly cannot
          // recover it from geometry: an inline link sits flush against the
          // text before it, so the gap between their boxes is zero whether or
          // not a space was written there.
          var pad = 0;
          if (/^\s/.test(raw)) pad |= 1;
          if (/\s$/.test(raw)) pad |= 2;
          emitRun(el, path + "/#t[" + m + "]", myBlock, t2, textKids[m], env, ctx, pad);
        }
      }
    }

    // -- actions, media, canvas ---------------------------------------------
    // Actions take their label from the whole subtree, not from own text: the
    // readable name of "<a><span>Send</span> <span>enquiry</span></a>" is both
    // spans, and a link wrapping only an image borrows that image's alt.
    collectAction(el, tag, role, path, landmark, "", off, fixed, ctx);
    collectMedia(el, tag, path, cs, off, fixed, ctx);
    if (tag === "CANVAS") collectCanvas(el, path, off, fixed, ctx);

    // -- descend -------------------------------------------------------------

    // Shadow roots compose into the rendered tree but are invisible to a plain
    // childNodes walk, and they are where component-framework sites keep most
    // of their text.
    var sr = el.shadowRoot;
    if (sr) {
      descendChildren(sr, path + "/#shadow", myBlock, op, landmark, href, depth + 1, off, visible, fixed, bg, sectioned, reveal, ctx);
    }

    if (tag === "IFRAME" || tag === "FRAME") {
      descendFrame(el, path, myBlock, op, landmark, href, depth, off, visible, fixed, bg, sectioned, reveal, ctx);
      return;
    }

    descendChildren(el, path, myBlock, op, landmark, href, depth + 1, off, visible, fixed, bg, sectioned, reveal, ctx);
  }

  // walkLatent collects a subtree that is not rendered at all.
  //
  // It is deliberately much cheaper than walk(): a display:none subtree has no
  // layout, so there is nothing to measure, no styles worth resolving, and no
  // visibility to compute. It collects text, structure and the disclosure
  // control that would reveal it, and nothing else.
  //
  // Everything it produces is quarantined. It is the exact material the
  // visibility filter exists to exclude, kept because a tab panel is content a
  // reader can reach, and kept separate because a hidden element is also how a
  // page hides an instruction aimed at whatever agent reads it.
  function walkLatent(root, rootPath, blockPath, landmark, href, depth, reason, control, ctx) {
    var col = ctx.col;
    var stack = [[root, rootPath, blockPath, depth]];

    while (stack.length) {
      if (col.latent.length >= col.latentBudget) {
        col.latentTruncated = true;
        return;
      }
      var frame = stack.pop();
      var el = frame[0], path = frame[1], bp = frame[2], d = frame[3];
      var tag = el.tagName;
      if (!tag || SKIP[tag] === 1) continue;

      if (LANDMARK_TAG[tag]) landmark = LANDMARK_TAG[tag];

      var own = "";
      var kids = el.childNodes;
      for (var i = 0; i < kids.length; i++) {
        if (kids[i].nodeType === 3 && kids[i].data) own += kids[i].data;
      }
      own = norm(own);
      if (own && !isIconGlyph(own)) {
        var roleAttr = el.getAttribute ? el.getAttribute("role") : null;
        col.latent.push({
          p: path,
          bp: bp,
          t: tag.toLowerCase(),
          x: own,
          r: roleAttr ? roleAttr.trim().toLowerCase() : undefined,
          lm: landmark || undefined,
          h: href || undefined,
          why: reason,
          cl: control && control.l ? control.l : undefined,
          ck: control ? control.k : undefined,
          d: d,
        });
      }

      var sr = el.shadowRoot;
      if (sr) pushChildren(stack, sr, path + "/#shadow", bp, d + 1);
      pushChildren(stack, el, path, bp, d + 1);
    }
  }

  function pushChildren(stack, parent, path, bp, depth) {
    var kids = parent.children;
    if (!kids) return;
    var counts = Object.create(null);
    var frames = [];
    for (var i = 0; i < kids.length; i++) {
      var c = kids[i];
      var t = c.tagName;
      if (!t) continue;
      var lower = t.toLowerCase();
      var idx = counts[lower] === undefined ? 0 : counts[lower];
      counts[lower] = idx + 1;
      frames.push([c, path + "/" + seg(lower, idx), bp, depth]);
    }
    // Pushed in reverse so the stack pops in document order, which keeps the
    // latent tier's ordering deterministic.
    for (var j = frames.length - 1; j >= 0; j--) stack.push(frames[j]);
  }

  // emitRun records one run of text. `textNode` is non-null only for fragments
  // of a mixed-content element, where geometry must come from a Range rather
  // than from the element box.
  // ---------------------------------------------------------------------------
  // Waiting to be revealed, versus hidden
  //
  // A run at opacity zero is excluded from the content tier, and that rule is
  // the project's central defence: what a visitor never saw is not content, and
  // the decision rests on what the browser reported rather than on a guess
  // about class names.
  //
  // It also throws away most of a scroll-driven site. On pear.no the entire
  // argument -- the terms, the selectivity, the application -- sits at opacity
  // zero until its section scrubs into view, and a sweep that cannot afford to
  // stop at exactly the right scroll offset never sees any of it. The text is
  // real, authored, and read by every human who scrolls the page. Losing it is
  // sieve's failure, not the page's.
  //
  // The two cases are distinguishable, and the page itself makes the
  // distinction: an element that is *going* to appear says so in its computed
  // style. `transition: opacity .8s` and a running `animation` are declarations
  // of intent to become visible, written by the author for the browser. Text
  // hidden to stay hidden carries no such declaration -- an off-screen
  // instruction aimed at an agent is not politely faded in.
  //
  // So the flag records the declaration, and nothing more. It never promotes
  // anything on its own; it gives the graph a fact to weigh, and everything it
  // marks is still reported as unverified and counted in the audit.
  function revealDeclared(cs) {
    if (!cs) return false;
    var tp = cs.transitionProperty;
    if (tp && tp !== "none") {
      if (tp.indexOf("opacity") >= 0 || tp.indexOf("transform") >= 0 ||
          tp.indexOf("all") >= 0 || tp.indexOf("visibility") >= 0) {
        // A zero-length transition is a declaration in name only.
        var td = cs.transitionDuration || "";
        if (td && !/^(0s|0ms)(,\s*(0s|0ms))*$/.test(td.replace(/\s/g, ""))) return true;
      }
    }
    if (cs.animationName && cs.animationName !== "none") return true;
    return false;
  }

  function emitRun(el, path, blockPath, text, textNode, env, ctx, pad) {
    if (!text || isIconGlyph(text)) return;

    // Where the run begins, as distinct from how far it extends.
    //
    // A run that wraps gets a bounding box that is the union of its lines, and
    // the union's left edge is the left margin -- because some later line
    // starts there -- not the point on the first line where the words actually
    // begin. Ordering fragments by that left edge puts a wrapped run before
    // everything that precedes it on its own first line, which tears an inline
    // link out of the sentence around it and re-emits it afterwards.
    //
    // The first client rect is the first line box, and its left edge is the
    // real starting point. The union stays as the box, because it is the
    // honest extent and everything else downstream wants that.
    var rect, lineTop, lineLeft;
    if (textNode) {
      var rng = ctx.doc.createRange();
      try {
        rng.selectNodeContents(textNode);
      } catch (e) {
        return;
      }
      rect = rng.getBoundingClientRect();
      lineTop = rect.top;
      lineLeft = rect.left;
      var rrs = rng.getClientRects();
      if (rrs && rrs.length) {
        lineTop = rrs[0].top;
        lineLeft = rrs[0].left;
      }
      rng.detach && rng.detach();
    } else {
      rect = el.getBoundingClientRect();
      lineTop = rect.top;
      lineLeft = rect.left;
      try {
        var rs = el.getClientRects();
        if (rs && rs.length) {
          lineTop = rs[0].top;
          lineLeft = rs[0].left;
        }
      } catch (e) {}
    }
    if (rect.width <= 0 && rect.height <= 0) return;

    var cs = env.cs;
    var inView =
      env.visible &&
      rect.bottom > 0 &&
      rect.top < ctx.vh &&
      rect.right > 0 &&
      rect.left < ctx.vw;

    // Text the same colour as what is behind it renders at full opacity and is
    // still unreadable. Flagged rather than dropped: low-contrast design is
    // legitimate and common, so the graph decides what to do with the signal.
    var invisible = false;
    if (env.bg) {
      invisible = contrastRatio(parseColor(cs.color), env.bg) < INVISIBLE_CONTRAST;
    }

    var nameAttr = el.getAttribute ? el.getAttribute("aria-label") : null;
    var aria = nameAttr ? capText(nameAttr) : "";
    var docX = env.fixed ? rect.left : rect.left + env.off.x + ctx.sx;
    var docY = env.fixed ? rect.top : rect.top + env.off.y + ctx.sy;

    // The running total of visible text is the denominator for the graph's
    // retention audit: how much of what the browser showed survived into the
    // artifact.
    if (inView && env.op > 0.12 && !invisible) ctx.col.visibleChars += text.length;

    ctx.col.nodes.push({
      p: path,
      bp: blockPath,
      t: env.tag.toLowerCase(),
      x: text,
      r: env.role || undefined,
      al: aria && aria !== text ? aria : undefined,
      lm: env.landmark || undefined,
      h: env.href || undefined,
      fs: num(cs.fontSize, 16),
      fw: weightOf(cs.fontWeight),
      ls: cs.letterSpacing === "normal" ? 0 : num(cs.letterSpacing, 0),
      lh: cs.lineHeight === "normal" ? 0 : num(cs.lineHeight, 0),
      ff: familyOf(cs.fontFamily),
      tt: cs.textTransform === "none" ? undefined : cs.textTransform,
      c: cs.color,
      it: cs.fontStyle === "italic" || cs.fontStyle === "oblique" || undefined,
      o: env.op,
      v: inView,
      iv: invisible || undefined,
      pd: pad || undefined,
      fx: env.fixed || undefined,
      // Only recorded where it matters. On a legible run the page's intent to
      // reveal it is history; on an illegible one it is the whole question.
      rv: (env.op <= MIN_VISIBLE_OPACITY && env.reveal) || undefined,
      bb: [round2(docX), round2(docY), round2(rect.width), round2(rect.height)],
      lt: Math.round(env.fixed ? lineTop : lineTop + env.off.y + ctx.sy),
      lx: Math.round(env.fixed ? lineLeft : lineLeft + env.off.x + ctx.sx),
      d: env.depth,
    });
  }

  function descendChildren(parent, path, blockPath, op, landmark, href, depth, off, vis, fixed, bg, sectioned, reveal, ctx) {
    var kids = parent.children;
    if (!kids) return;
    var counts = Object.create(null);
    for (var i = 0; i < kids.length; i++) {
      var c = kids[i];
      var t = c.tagName;
      if (!t) continue;
      var lower = t.toLowerCase();
      var idx = counts[lower] === undefined ? 0 : counts[lower];
      counts[lower] = idx + 1;
      if (ctx.col.full()) return;
      walk(c, path + "/" + seg(lower, idx), blockPath, op, landmark, href, depth, off, vis, fixed, bg, sectioned, reveal, ctx);
    }
  }

  function descendFrame(el, path, blockPath, op, landmark, href, depth, off, vis, fixed, bg, sectioned, reveal, ctx) {
    var doc;
    try {
      doc = el.contentDocument;
    } catch (e) {
      doc = null;
    }
    if (!doc || !doc.documentElement) {
      // Cross-origin. Recording the count matters: it is the honest way to say
      // "there is content here that was not read" rather than silently
      // reporting a page as fully covered.
      ctx.col.framesBlocked++;
      return;
    }
    ctx.col.frames++;
    var r = el.getBoundingClientRect();
    var win = el.contentWindow;
    var inner = {
      win: win,
      doc: doc,
      sx: 0,
      sy: 0,
      vw: r.width,
      vh: r.height,
      col: ctx.col,
      disc: ctx.disc,
      lmCount: ctx.lmCount,
      lmTotals: ctx.lmTotals,
    };
    try {
      inner.sx = win.scrollX || 0;
      inner.sy = win.scrollY || 0;
    } catch (e) {}
    var innerOff = {
      x: off.x + ctx.sx + r.left - inner.sx,
      y: off.y + ctx.sy + r.top - inner.sy,
    };
    walk(doc.documentElement, path + "/#doc", blockPath, op, landmark, href, depth + 1, innerOff, vis, fixed, bg, sectioned, reveal, inner);
  }

  function pseudoText(win, el) {
    var out = "";
    for (var i = 0; i < 2; i++) {
      var which = i === 0 ? "::before" : "::after";
      var s;
      try {
        s = win.getComputedStyle(el, which);
      } catch (e) {
        continue;
      }
      if (!s) continue;
      var c = s.content;
      if (!c || c === "none" || c === "normal" || c === '""' || c === "''") continue;
      if (c.charAt(0) !== '"' && c.charAt(0) !== "'") continue; // url(), counters, attr()
      var v = norm(c.slice(1, -1));
      if (!v || isIconGlyph(v)) continue;
      out = out ? out + " " + v : v;
    }
    return out;
  }

  function docBox(r, off, fixed, ctx) {
    return [
      round2(fixed ? r.left : r.left + off.x + ctx.sx),
      round2(fixed ? r.top : r.top + off.y + ctx.sy),
      round2(r.width),
      round2(r.height),
    ];
  }

  function collectAction(el, tag, role, path, landmark, own, off, fixed, ctx) {
    var col = ctx.col;
    var kind = "";
    if (tag === "A" && el.href && URLISH.test(el.href)) kind = "link";
    else if (tag === "BUTTON") kind = "button";
    else if (tag === "FORM") kind = "form";
    else if (tag === "INPUT") {
      var it = (el.type || "").toLowerCase();
      if (it === "submit" || it === "button" || it === "reset" || it === "image") kind = "button";
    } else if (role === "button" || role === "link") {
      kind = role;
    }
    if (!kind) return;
    if (col.seenAction.has(path)) return;
    col.seenAction.add(path);

    var r = el.getBoundingClientRect();
    var bb = docBox(r, off, fixed, ctx);

    if (kind === "form") {
      col.actions.push({
        p: path,
        k: "form",
        l: accessibleName(el, "") || formHeading(el),
        h: el.action || "",
        m: (el.method || "get").toUpperCase(),
        f: formFields(el),
        bb: bb,
        lm: landmark || undefined,
      });
      return;
    }

    var label = accessibleName(el, own);
    if (kind === "button" && tag === "INPUT") {
      label = capText(el.value || el.getAttribute("alt") || "") || label;
    }
    col.actions.push({
      p: path,
      k: kind,
      l: label,
      h: kind === "link" ? el.href : el.getAttribute("formaction") || "",
      bb: bb,
      lm: landmark || undefined,
      dis: el.disabled === true || el.getAttribute("aria-disabled") === "true" || undefined,
    });
  }

  // formHeading gives an unlabelled form a name from the nearest preceding
  // heading, which is how a human reading the page would name it.
  function formHeading(form) {
    var p = form;
    for (var up = 0; p && up < 4; up++, p = p.parentElement) {
      var h = p.querySelector ? p.querySelector("h1,h2,h3,h4,[role=heading]") : null;
      if (h) {
        var t = capText(h.textContent);
        if (t) return t;
      }
    }
    return "";
  }

  function formFields(form) {
    var out = [];
    var els;
    try {
      els = form.querySelectorAll("input,select,textarea");
    } catch (e) {
      return out;
    }
    for (var i = 0; i < els.length && i < 100; i++) {
      var f = els[i];
      if (!FIELD_TAGS[f.tagName]) continue;
      var type = f.tagName === "INPUT" ? (f.type || "text").toLowerCase() : f.tagName.toLowerCase();
      if (type === "submit" || type === "button" || type === "reset" || type === "image") continue;
      // A hidden field is machinery (CSRF tokens, campaign ids), not something
      // a visitor fills in.
      if (type === "hidden") continue;
      var rec = {
        n: f.name || f.id || "",
        t: type,
        l: labelForField(f),
        r: f.required === true || f.getAttribute("aria-required") === "true" || undefined,
      };
      var pat = f.getAttribute("pattern");
      if (pat) rec.pt = pat;
      if (f.tagName === "SELECT") {
        var opts = [];
        for (var j = 0; j < f.options.length && j < 60; j++) {
          var ot = norm(f.options[j].textContent);
          if (ot) opts.push(ot);
        }
        if (opts.length) rec.o = opts;
      }
      out.push(rec);
    }
    return out;
  }

  function collectMedia(el, tag, path, cs, off, fixed, ctx) {
    var col = ctx.col;
    var kind = "";
    var src = "";
    var alt = "";

    if (tag === "IMG") {
      kind = "image";
      src = el.currentSrc || el.src || "";
      alt = el.getAttribute("alt");
    } else if (tag === "VIDEO") {
      kind = "video";
      src = el.currentSrc || el.src || "";
      if (!src) {
        var s = el.querySelector("source[src]");
        if (s) src = s.src;
      }
      alt = el.getAttribute("aria-label");
    } else if (tag === "MODEL-VIEWER") {
      kind = "model";
      src = el.getAttribute("src") || "";
      alt = el.getAttribute("alt");
    } else if (tag === "SVG" || tag === "svg") {
      return; // inline SVG text is already captured as nodes
    } else {
      // A CSS background image on a large element is a hero image in every way
      // that matters to a reader, and carries no alt text at all, so it is
      // worth surfacing as media that vision can describe on request.
      var bi = cs.backgroundImage;
      if (bi && bi !== "none" && bi.indexOf("url(") === 0) {
        var r0 = el.getBoundingClientRect();
        var area = r0.width * r0.height;
        if (area > 0.06 * ctx.vw * ctx.vh) {
          kind = "image";
          src = bi.slice(4, bi.length - 1).replace(/^["']|["']$/g, "");
          alt = el.getAttribute("aria-label");
        }
      }
    }
    if (!kind || !src) return;
    if (src.indexOf("data:") === 0 && src.length > 512) src = src.slice(0, 64) + "...";
    var key = path + "|" + src;
    if (col.seenMedia.has(key)) return;
    col.seenMedia.add(key);

    var rect = el.getBoundingClientRect();
    var decorative =
      el.getAttribute("role") === "presentation" ||
      el.getAttribute("role") === "none" ||
      (tag === "IMG" && el.getAttribute("alt") === "");

    var altText = alt ? capText(alt) : "";
    col.media.push({
      p: path,
      k: kind,
      s: src,
      a: altText || undefined,
      // A capped alt is recorded as capped so a consumer can tell truncation
      // from brevity.
      ac: alt && norm(alt).length > META_TEXT_CAP ? true : undefined,
      ti: el.getAttribute("title") ? capText(el.getAttribute("title")) : undefined,
      cp: figcaption(el),
      bb: docBox(rect, off, fixed, ctx),
      dec: decorative || undefined,
    });
  }

  function figcaption(el) {
    var p = el.parentElement;
    for (var i = 0; p && i < 3; i++, p = p.parentElement) {
      if (p.tagName === "FIGURE") {
        var c = p.querySelector("figcaption");
        if (c) return capText(c.textContent) || undefined;
        return undefined;
      }
    }
    return undefined;
  }

  function collectCanvas(el, path, off, fixed, ctx) {
    var col = ctx.col;
    var r = el.getBoundingClientRect();
    var area = r.width * r.height;
    if (area <= 0) return;
    var share = area / (ctx.vw * ctx.vh);
    var cx = "";
    try {
      if (window.__sieveCanvasCtx) cx = window.__sieveCanvasCtx(el) || "";
    } catch (e) {}

    // The accessibility fallback. A canvas element's child content is what a
    // screen reader is given, and a well-built WebGL site puts a real
    // description there. It is the cheapest canvas recovery there is, it is
    // authored by the site rather than inferred, and most tools ignore it.
    var fallback = "";
    try {
      fallback = capText(el.textContent || "");
    } catch (e) {}

    col.canvases.push({
      p: path,
      bb: docBox(r, off, fixed, ctx),
      // Viewport rectangle is what a screenshot clip needs; the document box is
      // where the canvas sits in the reading order.
      vb: [round2(r.left), round2(r.top), round2(r.width), round2(r.height)],
      vs: round4(share),
      cx: cx || undefined,
      l: capText(el.getAttribute("aria-label") || el.getAttribute("title") || "") || undefined,
      fb: fallback || undefined,
    });
  }

  function round2(v) {
    return Math.round(v * 100) / 100;
  }
  function round4(v) {
    return Math.round(v * 10000) / 10000;
  }

  // ---------------------------------------------------------------------------
  // Corroboration corpus
  //
  // The text on a WebGL brand site almost always exists as text before it
  // becomes pixels: a hydration blob, an inline JSON constant, a CMS payload.
  //
  // That text is NEVER a source of content. It routinely contains draft copy,
  // other languages, unpublished fields and adjacent records, and emitting it
  // would turn extraction into republishing someone's back office. It is
  // collected for exactly one purpose: to confirm that a string recovered from
  // pixels really does appear in what the site shipped. Confirmation promotes a
  // guess to verified; absence leaves it speculative. Nothing here is ever
  // emitted, and the corpus is discarded once recovery has run.
  // ---------------------------------------------------------------------------

  var CORPUS_CAP = 2 << 20;

  function collectCorpus(doc) {
    var out = [];
    var size = 0;
    function add(s) {
      if (!s || size >= CORPUS_CAP) return;
      if (s.length > CORPUS_CAP - size) s = s.slice(0, CORPUS_CAP - size);
      out.push(s);
      size += s.length;
    }
    try {
      var scripts = doc.querySelectorAll("script");
      for (var i = 0; i < scripts.length && size < CORPUS_CAP; i++) {
        var sc = scripts[i];
        var type = (sc.getAttribute("type") || "").toLowerCase();
        // Inline JSON payloads and hydration blobs. External bundles are
        // harvested on the Go side from intercepted responses.
        if (sc.src) continue;
        if (type && type.indexOf("json") < 0 && type !== "" && type.indexOf("javascript") < 0) continue;
        add(sc.textContent || "");
      }
      // Framework hydration globals that are not inline script text.
      var globals = ["__NEXT_DATA__", "__NUXT__", "__remixContext", "__APOLLO_STATE__", "__INITIAL_STATE__"];
      for (var g = 0; g < globals.length; g++) {
        try {
          var v = window[globals[g]];
          if (v) add(typeof v === "string" ? v : JSON.stringify(v));
        } catch (e) {}
      }
    } catch (e) {}
    return out.join("\n");
  }

  // ---------------------------------------------------------------------------
  // Runtime scene-graph introspection
  //
  // Parsing a .glb file only works when the site loaded one. A scene built
  // procedurally in JavaScript never produces an asset to intercept, and its
  // object names live only in memory. Walking the live scene after settle
  // catches those, and costs nothing when there is no scene to walk.
  // ---------------------------------------------------------------------------

  // sceneTextOf reads the words a 3D text object was built from.
  //
  // Text drawn into WebGL is glyph geometry by the time it reaches the screen,
  // but the library that laid it out keeps the string it was given, and every
  // implementation keeps it somewhere slightly different. TextGeometry puts it
  // on geometry.parameters; troika exposes a .text property; the MSDF renderer
  // igloo.inc uses hangs an _options object off the mesh. Reading the handful
  // of places they actually use costs nothing and is exact -- these are the
  // site's own strings, not a guess about pixels.
  function sceneTextOf(o) {
    var cands = [];
    try {
      if (o._options && typeof o._options.text === "string") cands.push(o._options.text);
      if (o.options && typeof o.options.text === "string") cands.push(o.options.text);
      if (typeof o.text === "string") cands.push(o.text);
      if (o.geometry && o.geometry.parameters &&
        typeof o.geometry.parameters.text === "string") {
        cands.push(o.geometry.parameters.text);
      }
    } catch (e) {}
    for (var i = 0; i < cands.length; i++) {
      var t = norm(cands[i]);
      if (t && t.length > 1) return t.slice(0, 4000);
    }
    return "";
  }

  function introspectScene() {
    var names = [];
    var texts = [];
    var runs = [];
    var seen = new Set();
    var seenText = new Set();
    var seenObj = new Set();
    var count = 0;

    function visit(obj, depth) {
      if (!obj || depth > 24 || count > 8000) return;
      if (seenObj.has(obj)) return;
      seenObj.add(obj);
      count++;
      try {
        if (obj.name && typeof obj.name === "string" && !seen.has(obj.name)) {
          seen.add(obj.name);
          names.push(obj.name);
        }
        if (obj.userData && typeof obj.userData === "object") {
          for (var k in obj.userData) {
            var v = obj.userData[k];
            if (typeof v === "string" && v.length > 3 && v.length < 400) texts.push(v);
          }
        }
        // The words themselves, in the order the scene was assembled, which is
        // the order the author wrote them and the best evidence of reading
        // order available on a surface that has no layout to consult.
        var t = sceneTextOf(obj);
        if (t && !seenText.has(t) && runs.length < 400) {
          seenText.add(t);
          texts.push(t);
          runs.push({ x: t, n: (obj.name && String(obj.name).slice(0, 60)) || undefined });
        }
        var ch = obj.children;
        if (ch && ch.length) {
          for (var i = 0; i < ch.length && i < 2000; i++) visit(ch[i], depth + 1);
        }
      } catch (e) {}
    }

    var roots = [];
    try {
      // The devtools hook is the reliable way in when a page uses three.js,
      // and bootstrap.js installs it before any page script runs precisely so
      // that this is available. See the note there.
      var hook = window.__THREE_DEVTOOLS__;
      if (hook && hook.scenes) {
        for (var i = 0; i < hook.scenes.length; i++) roots.push(hook.scenes[i]);
      }
    } catch (e) {}
    // Scanning the global scope means touching up to eight hundred properties
    // of window, any of which may be a getter with side effects. That is worth
    // doing on a page that has a canvas -- there is plainly something to find --
    // and not worth doing on a page that has neither a canvas nor a scene, which
    // is almost every page. The hook above is what makes this affordable: a
    // scene announces itself, so the scan is only a fallback for older builds.
    var worthScanning = false;
    try {
      worthScanning = !!document.querySelector("canvas");
    } catch (e) {}
    if (!roots.length && worthScanning) {
      // Otherwise scan the global scope shallowly for something scene-shaped.
      try {
        var keys = Object.keys(window);
        for (var j = 0; j < keys.length && j < 800; j++) {
          var v;
          try {
            v = window[keys[j]];
          } catch (e) {
            continue;
          }
          if (!v || typeof v !== "object") continue;
          if (v.isScene === true || (v.type === "Scene" && typeof v.traverse === "function")) {
            roots.push(v);
          } else if (v.scene && (v.scene.isScene === true || v.scene.type === "Scene")) {
            roots.push(v.scene);
          }
        }
      } catch (e) {}
    }
    // A page can register many scenes -- igloo.inc announces seventeen -- and
    // the text is spread across them, so the cap is on work done rather than
    // on how many roots are considered.
    for (var r = 0; r < roots.length && r < 64; r++) visit(roots[r], 0);
    if (!names.length && !texts.length) return undefined;
    return { n: names.slice(0, 400), t: texts.slice(0, 200), r: runs };
  }

  // ---------------------------------------------------------------------------
  // Library fingerprints
  //
  // Which animation and scroll libraries a page loaded is the strongest single
  // predictor of whether a cheap fetch will be enough. The list lives in Go as
  // versioned data; this only reports which of the supplied globals exist, so
  // adding a detector never requires touching this file.
  // ---------------------------------------------------------------------------

  function detectLibraries(specs) {
    var found = [];
    for (var i = 0; i < specs.length; i++) {
      var s = specs[i];
      try {
        var ok = false;
        if (s.g) {
          var parts = s.g.split(".");
          var cur = window;
          ok = true;
          for (var p = 0; p < parts.length; p++) {
            if (cur == null || typeof cur !== "object" && typeof cur !== "function") {
              ok = false;
              break;
            }
            cur = cur[parts[p]];
            if (cur === undefined) {
              ok = false;
              break;
            }
          }
        }
        if (!ok && s.s) {
          ok = !!document.querySelector(s.s);
        }
        if (ok) found.push(s.n);
      } catch (e) {}
    }
    return found;
  }

  // ---------------------------------------------------------------------------

  // The disclosure map and the page-landmark totals are whole-document
  // querySelectorAll scans, and both were being rebuilt at every checkpoint.
  // On a forty-checkpoint sweep that is forty scans of the entire DOM -- with
  // the landmark query carrying a five-clause :not() chain that the selector
  // engine cannot index -- to recompute two answers that had not changed since
  // the page loaded.
  //
  // They are memoised against the structural mutation counter, which ticks only
  // when an element is inserted or removed. A page that mounts a new section
  // mid-sweep rebuilds them exactly once; a page that merely animates never
  // rebuilds them at all.
  var docIndexCache = { key: -1, disc: null, lmTotals: null };

  function documentIndexes() {
    var key = window.__sieveStructMutations;
    if (key === undefined) key = -2; // bootstrap absent: rebuild every time
    if (docIndexCache.key === key && docIndexCache.disc) return docIndexCache;
    docIndexCache = {
      key: key,
      disc: buildDisclosureIndex(document),
      lmTotals: countPageLandmarks(document),
    };
    return docIndexCache;
  }

  function capture(checkpoint, budget, latentBudget) {
    var col = new Collector(budget || 40000, latentBudget || 12000);
    var de = document.documentElement;
    var vw = window.innerWidth || (de && de.clientWidth) || 0;
    var vh = window.innerHeight || (de && de.clientHeight) || 0;
    var idx = documentIndexes();
    var disc = idx.disc;
    var ctx = {
      win: window,
      doc: document,
      sx: window.scrollX || window.pageXOffset || 0,
      sy: window.scrollY || window.pageYOffset || 0,
      vw: vw,
      vh: vh,
      col: col,
      disc: disc,
      lmCount: {},
      lmTotals: idx.lmTotals,
    };
    if (de) {
      // The page's own base background is the reference for the contrast check.
      var rootBG = null;
      try {
        rootBG = parseColor(window.getComputedStyle(document.body || de).backgroundColor);
        if (!rootBG || rootBG.a < 0.95) rootBG = { r: 255, g: 255, b: 255, a: 1 };
      } catch (e) {
        rootBG = { r: 255, g: 255, b: 255, a: 1 };
      }
      walk(de, "html", "html", 1, "", "", 0, { x: 0, y: 0 }, true, false, rootBG, false, false, ctx);
    }
    var body = document.body;
    var dh = Math.max(
      de ? de.scrollHeight : 0,
      de ? de.offsetHeight : 0,
      body ? body.scrollHeight : 0,
      body ? body.offsetHeight : 0
    );
    return {
      n: checkpoint,
      sy: ctx.sy,
      dh: dh,
      vw: vw,
      vh: vh,
      nodes: col.nodes,
      latent: col.latent,
      actions: col.actions,
      media: col.media,
      canvases: col.canvases,
      disc: disc.controls,
      vc: col.visibleChars,
      meta: checkpoint === 0 ? collectMeta(document, window) : undefined,
      fr: col.frames,
      frx: col.framesBlocked,
      tr: col.truncated || undefined,
      ltr: col.latentTruncated || undefined,
    };
  }

  // ---------------------------------------------------------------------------
  // Entry gates
  //
  // A "click to enter" screen is a full-viewport overlay with a single control
  // on it, and behind it the site has not started: no scroll, no reveals, no
  // content. Sieve had no way to say so. hatom.com produced nine blocks and an
  // artifact that read exactly like a site with nothing on it, when in fact the
  // page was waiting to be let in.
  //
  // Detecting it does not mean clicking it. Dismissing an interstitial is an
  // interaction, and the bounded-interaction question is settled elsewhere in
  // this project by only opening what announces itself as openable. What a
  // detector buys is the difference between a thin artifact and a thin artifact
  // that says why it is thin -- which is the whole of the honesty claim.
  // ---------------------------------------------------------------------------

  // What sieve will press, and what it will never press.
  //
  // ENTER_WORDS is the front door: the control a site puts between a visitor
  // and itself that carries no meaning beyond "begin". Pressing it asserts
  // nothing on the visitor's behalf -- it is the same gesture as following a
  // link, and every human who reads the site makes it.
  //
  // "Enable sound" belongs here, which is not obvious. A browser will not play
  // audio until someone has touched the page, so a site built around sound has
  // no choice but to ask, and asking is the only thing standing between the
  // reader and the content: hatom.com finishes loading, prints "Click to enable
  // sound", and waits there indefinitely. Pressing it makes no noise, because
  // Chromium is launched muted -- see the --mute-audio flag in browser.go --
  // so what the press actually buys is the gesture, not the audio.
  //
  // REFUSE_WORDS is the line. An age gate, a cookie banner, a consent wall and
  // a purchase button all look like entry gates and none of them is one: each
  // asks the visitor to *say* something -- that they are of age, that they
  // accept terms, that they want to buy -- and a tool has no standing to say it
  // for them. Those are declared as gaps instead, which is what the artifact
  // did for every gate before this and still does for these.
  //
  // The refusal list is checked first and wins ties. When the two overlap --
  // "Enter site (I am over 18)" -- the answer is no.
  // The leading character class, rather than \s*, is what lets an ornamented
  // label through. Half of these controls are typeset as "→ Enter site" or
  // "— begin —", and a pattern anchored to the first character never matches
  // one, though it plainly says the same thing to a person. Only leading
  // punctuation is skipped; the word itself still has to start the label, so
  // "we will never enter into a contract" is still not a front door.
  var ENTER_WORDS = /^[^A-Za-z0-9]*(click|tap|press|push)?\s*(here\s*)?(to\s+)?(enter|start|begin|continue|explore|discover|launch|skip|play|view\s+site|go\b|enable\s+(sound|audio)|sound\s+on|with\s+sound|unmute|step\s+inside|come\s+in|dive\s+in|let'?s\s+go)\b/i;

  var REFUSE_WORDS = /\b(18|21)\+?\b|over\s*(18|21)|of\s+legal\s+age|\bi\s*am\b|i'?m\s+over|accept|agree|consent|cookie|privacy|terms|gdpr|allow\s+all|sign\s*in|sign\s*up|log\s*in|register|subscribe|newsletter|\bbuy\b|purchase|checkout|add\s+to\s+(cart|bag)|\bpay\b|\border\b|donate|delete|remove|submit|\bsend\b|download|install/i;

  // ENTER_INVITE is the page saying, in words, what it wants done.
  //
  // Some entry screens have no findable control at all: the words are painted
  // on a decorative layer with pointer-events disabled, and the handler listens
  // on the window for a click anywhere. hatom.com prints CLICK TO ENTER exactly
  // that way. When a page asks in plain language and there is nothing to aim
  // at, the middle of it is the honest place to press -- and the deny list is
  // still applied to the surface before anything is touched.
  var ENTER_INVITE = /\b(click|tap|press)\s+(anywhere\s+)?(here\s+)?to\s+(enter|start|begin|continue|explore|play)\b|\benter\s+site\b|\bclick\s+anywhere\b/i;

  // KEY_INVITE is the same request made of the keyboard.
  //
  // Some entry screens listen only for a key and ignore the mouse entirely,
  // and they say so: "press enter to continue", "hit space to begin".
  // quadricodes.tech offers both -- "SKIP INTRO OR PRESS ENTER" -- and plenty
  // of sites offer only the second, which a tool that can only click reads as
  // a door that would not open.
  var KEY_INVITE = /\b(press|hit|tap)\s+(the\s+)?(enter|return|space(bar)?|any\s+key)\b/i;

  // The maximum this will ever do to a page: two presses, and only while the
  // page is still refusing to show anything.
  var MAX_ENTRY_CLICKS = 2;

  // looksLikeLoader reports that the page is still assembling itself rather
  // than waiting for a visitor. hatom.com says "Loading 5 phases" for longer
  // than the load allowance, and pressing something during that is both futile
  // and the wrong response: what it needs is patience.
  function looksLikeLoader(text) {
    if (/loading|please wait|preparing|initialis|initializ/i.test(text)) return true;
    // A bare percentage counts only on a page that is showing almost nothing
    // else. A progress figure is a loader; "98% of our clients renew" is copy,
    // and treating every percentage as progress made pear.no wait out rounds it
    // did not need and then run short of budget for the read itself.
    if (text.length < 120 && /\d{1,3}\s*%/.test(text)) return true;
    // A page whose entire visible text contains not one letter and not one
    // digit is not showing content. It is showing a progress indicator drawn
    // out of punctuation, and igloo.inc counts itself in with a ten-character
    // ASCII bar -- "++==------", then "=++==-----", then "==++==----" -- which
    // sieve read twenty-four frames of and called the page. The letters are
    // tested by Unicode class rather than A-Z, or every CJK page short enough
    // would be mistaken for a spinner.
    return text.length > 0 && text.length < 120 && !/[\p{L}\p{N}]/u.test(text);
  }

  // findEntryControl locates the front door, and refuses to find anything else.
  //
  // It returns viewport coordinates rather than clicking, so the press can be
  // dispatched as a real input event from outside the page. A great many
  // gates listen for a trusted event and ignore element.click().
  //
  // Everything about the search is deny-first. A control is a candidate only if
  // its accessible name reads as "begin" and does not read as a claim about the
  // visitor, a transaction, or an account; it must be visible, of a size a
  // person could hit, and not inside a form. Anything ambiguous is left alone
  // and declared as a gap, which is what happened to every gate before this.
  function findEntryControl() {
    var vw = window.innerWidth || 1;
    var vh = window.innerHeight || 1;
    // Two searches, because half of these controls are not controls.
    //
    // A site built with a framework attaches its listener in JavaScript, so the
    // thing a visitor presses is very often a plain div with no role, no href
    // and no onclick attribute -- invisible to any selector that looks for
    // semantics. hatom.com's "Click to enable sound" is exactly that, and a
    // tag-first search finds nothing on the page at all.
    //
    // So the second pass matches on the words instead. That is not a loosening
    // of what will be pressed: the allow list and the deny list are applied to
    // the label either way, and everything below still has to be visible, hit
    // testable, of a size a person could aim at, outside a form and not a link
    // that leaves the page. What changes is only where sieve is willing to look
    // for it.
    var els;
    try {
      els = document.querySelectorAll(
        "button,[role=button],a,[onclick],[class*='enter' i],[class*='skip' i]");
      if (els.length === 0 || !hasEnterLabel(els)) {
        els = document.querySelectorAll("body *");
      }
    } catch (e) {
      return null;
    }

    var best = null;
    for (var i = 0; i < els.length && i < 3000; i++) {
      var el = els[i];

      // Never inside a form: those controls submit something.
      if (el.closest && el.closest("form")) continue;

      // Never a link that leaves this page. An in-page anchor is fine, and is
      // how a good many intros are built.
      if (el.tagName === "A") {
        var href = el.getAttribute("href") || "";
        if (href && href.charAt(0) !== "#" && !/^javascript:/i.test(href)) continue;
      }
      if (el.disabled === true || el.getAttribute("aria-disabled") === "true") continue;

      var label = norm(el.innerText || el.textContent ||
        el.getAttribute("aria-label") || el.getAttribute("title") || "");
      if (!label || label.length > 40) continue;

      // The refusal is checked first and wins ties.
      if (REFUSE_WORDS.test(label)) continue;
      if (!ENTER_WORDS.test(label)) continue;

      // Prefer the innermost element that carries the whole label: if a child
      // says exactly the same thing, the child is the control and this is
      // merely its wrapper.
      //
      // This replaced a cap on how many children a candidate could have, which
      // was the same idea done crudely and which excluded the commonest way an
      // entry control is built. Every site that animates its entry text splits
      // the label into one element per letter -- GSAP's SplitText and its
      // imitators all do this -- so the button a person sees reading "ENTER"
      // has five children, none of which says "ENTER". A child count rejected
      // exactly the controls most worth finding; asking whether anything
      // inside repeats the label rejects only true wrappers.
      if (!ownsLabel(el, label)) continue;

      var cs;
      try {
        cs = getComputedStyle(el);
      } catch (e) {
        continue;
      }
      if (cs.display === "none" || cs.visibility === "hidden") continue;
      if (parseFloat(cs.opacity) < 0.1) continue;
      // pointer-events:none is not a reason to skip it -- it is a reason the
      // hit test below would fail. Such an element is painted over a handler
      // that is listening somewhere else, usually the window, and a click at
      // its coordinates passes straight through to exactly that handler. This
      // is how a "CLICK TO ENTER" caption is normally built.
      var throughOnly = cs.pointerEvents === "none";

      var r = el.getBoundingClientRect();
      if (r.width < 8 || r.height < 8) continue;
      if (r.bottom <= 0 || r.top >= vh || r.right <= 0 || r.left >= vw) continue;

      var x = r.left + r.width / 2;
      var y = r.top + r.height / 2;

      // Whatever is actually on top at that point has to be the control or a
      // part of it, or the press would land somewhere else entirely.
      if (!throughOnly) {
        var hit = null;
        try {
          hit = document.elementFromPoint(x, y);
        } catch (e) {}
        if (hit && hit !== el && !el.contains(hit)) continue;
      }

      // Prefer a real button, then the largest target: on a page with both a
      // "skip" and an "enter" the bigger one is the primary way in.
      var score = (el.tagName === "BUTTON" ? 1e6 : 0) + r.width * r.height;
      if (!best || score > best.score) {
        best = { label: label, x: Math.round(x), y: Math.round(y), score: score,
          tag: el.tagName.toLowerCase() };
      }
    }
    if (!best) return null;
    return { label: best.label, x: best.x, y: best.y, tag: best.tag };
  }

  // ownsLabel reports that no child of this element repeats the whole label,
  // which is what distinguishes a control from an ancestor containing one.
  function ownsLabel(el, label) {
    var kids = el.children || [];
    for (var i = 0; i < kids.length && i < 40; i++) {
      if (norm(kids[i].innerText || kids[i].textContent || "") === label) return false;
    }
    return true;
  }

  // opaqueSurface reports whether an element paints over what is behind it,
  // rather than being one of the many invisible full-screen boxes a modern
  // page is full of -- scroll proxies, gesture catchers, focus traps.
  function opaqueSurface(cs) {
    if (cs.backdropFilter && cs.backdropFilter !== "none") return true;
    if (cs.backgroundImage && cs.backgroundImage !== "none") return true;
    var bg = cs.backgroundColor || "";
    if (!bg || bg === "transparent") return false;
    var m = bg.match(/^rgba?\(([^)]+)\)/);
    if (!m) return false;
    var parts = m[1].split(",");
    if (parts.length < 4) return true;
    return parseFloat(parts[3]) >= 0.85;
  }

  // findCover locates a layer painted over the whole viewport.
  //
  // This is the signal that judging a gate by how much text the page reports
  // cannot see. A splash screen laid over a fully rendered page leaves
  // document.body.innerText returning the entire site -- every word of it
  // present, none of it visible -- so a text threshold concludes "not gated"
  // about a page a visitor cannot read a syllable of. Geometry is the honest
  // test: something opaque is covering everything.
  //
  // What it deliberately does not do is treat every full-screen layer as a
  // door. The element must actually paint (an invisible overlay hides nothing)
  // and the decision to press still needs a reason beyond the cover itself --
  // words that invite a press, a control that reads as an entrance, or real
  // content measurably hidden underneath.
  function findCover() {
    var vw = window.innerWidth || 1;
    var vh = window.innerHeight || 1;
    var area = vw * vh;
    var els;
    try {
      els = document.querySelectorAll("body *");
    } catch (e) {
      return null;
    }
    var best = null;
    var bestZ = -1e9;
    for (var i = 0; i < els.length && i < 4000; i++) {
      var el = els[i];

      // Geometry first, style second. getComputedStyle forces a style recalc
      // and this runs on every look at the page, several times a second, for
      // as long as a loading screen is up -- so asking it about every element
      // of a large document would slow down the very page being waited for.
      // A bounding rect is cheap and rejects all but a handful of candidates.
      var r = el.getBoundingClientRect();
      if (r.width * r.height < area * 0.9) continue;
      if (r.top > 2 || r.left > 2 || r.bottom < vh - 2 || r.right < vw - 2) continue;

      var cs;
      try {
        cs = getComputedStyle(el);
      } catch (e) {
        continue;
      }
      if (cs.position !== "fixed" && cs.position !== "absolute") continue;
      if (cs.display === "none" || cs.visibility === "hidden") continue;
      if (parseFloat(cs.opacity) < 0.85) continue;
      if (!opaqueSurface(cs)) continue;

      var z = parseInt(cs.zIndex, 10);
      if (isNaN(z)) z = 0;
      if (!best || z >= bestZ) {
        best = el;
        bestZ = z;
      }
    }
    if (!best) return null;

    // How much of the page's prose is underneath rather than on the cover
    // itself. Real text hidden by the layer is the strongest evidence that
    // this is a door and not simply a dark background.
    var hidden = 0;
    try {
      var all = norm((document.body && document.body.innerText) || "").length;
      var mine = norm(best.innerText || best.textContent || "").length;
      hidden = all - mine;
      if (hidden < 0) hidden = 0;
    } catch (e) {}

    return {
      tag: best.tagName ? best.tagName.toLowerCase() : "",
      label: norm(best.innerText || best.textContent || "").slice(0, 60),
      hidden: hidden,
      x: Math.round(vw / 2),
      y: Math.round(vh / 2),
    };
  }

  // centreTarget describes whatever is sitting in the middle of the viewport.
  //
  // Some entry screens have no control to find: the loader finishes and the
  // page waits for a gesture anywhere, which is how a site that wants to start
  // audio must behave, because browsers will not play sound until a visitor has
  // touched the page. hatom.com stops at "LOADING 5 PHASES 100 100 HEADPHONES
  // RECOMMENDED" and waits exactly like that.
  function centreTarget() {
    var vw = window.innerWidth || 1;
    var vh = window.innerHeight || 1;
    var el = null;
    try {
      el = document.elementFromPoint(vw / 2, vh / 2);
    } catch (e) {}
    if (!el) return null;

    // Walk up a little to find whatever carries the meaning, and refuse the
    // moment anything on the way says this is not a front door.
    var node = el;
    var label = "";
    for (var i = 0; node && i < 6; i++, node = node.parentElement) {
      if (node.closest && node.closest("form")) return null;
      if (node.tagName === "A") {
        var href = node.getAttribute("href") || "";
        if (href && href.charAt(0) !== "#" && !/^javascript:/i.test(href)) return null;
      }
      var t = norm(node.innerText || node.textContent || "");
      if (t && t.length <= 60 && !label) label = t;
      if (t && REFUSE_WORDS.test(t)) return null;
    }
    return {
      tag: el.tagName ? el.tagName.toLowerCase() : "",
      label: label,
      x: Math.round(vw / 2),
      y: Math.round(vh / 2),
    };
  }

  // hasEnterLabel reports whether any of these already carries a front-door
  // label, so the wider search is only run when the narrow one came up empty.
  function hasEnterLabel(els) {
    for (var i = 0; i < els.length && i < 500; i++) {
      var t = norm(els[i].innerText || els[i].textContent || "");
      if (t && t.length <= 40 && !REFUSE_WORDS.test(t) && ENTER_WORDS.test(t)) return true;
    }
    return false;
  }

  // gateState describes what is standing between sieve and the page.
  function gateState() {
    var text = "";
    try {
      text = norm((document.body && document.body.innerText) || "").slice(0, 400);
    } catch (e) {}
    var ctl = findEntryControl();
    return {
      text: text,
      chars: text.length,
      loading: looksLikeLoader(text),
      invites: ENTER_INVITE.test(text) && !REFUSE_WORDS.test(text),
      keys: KEY_INVITE.test(text) && !REFUSE_WORDS.test(text),
      control: ctl,
      cover: findCover(),
      centre: ctl ? null : centreTarget(),
      refused: ctl ? null : refusedGateLabel(),
    };
  }

  // refusedGateLabel names a gate sieve will not press, so the artifact can say
  // that the page is behind one rather than merely appearing empty.
  function refusedGateLabel() {
    var els;
    try {
      els = document.querySelectorAll("button,[role=button],a");
    } catch (e) {
      return null;
    }
    for (var i = 0; i < els.length && i < 300; i++) {
      var label = norm(els[i].innerText || els[i].textContent || "");
      if (label && label.length <= 40 && REFUSE_WORDS.test(label) &&
        /enter|continue|proceed|yes|agree|accept/i.test(label)) {
        return label;
      }
    }
    return null;
  }

  function detectEntryGate() {
    var vw = window.innerWidth || 1;
    var vh = window.innerHeight || 1;
    var de = document.documentElement;

    // A gate holds the page still. If the document already scrolls, whatever is
    // on screen is a hero and not an interstitial -- and the scan below is a
    // whole-document selector followed by a style resolution per candidate,
    // which is far too much to spend proving a negative on every page that
    // obviously has no gate.
    var scrollable = de && de.scrollHeight > de.clientHeight + 8;
    if (scrollable) return "";

    var best = "";
    var els;
    try {
      els = document.querySelectorAll(
        "body > *, body > * > *, [class*='enter' i], [id*='enter' i], [class*='intro' i], [class*='splash' i], [class*='gate' i]"
      );
    } catch (e) {
      return "";
    }
    for (var i = 0; i < els.length && i < 400; i++) {
      var el = els[i];
      var cs;
      try {
        cs = getComputedStyle(el);
      } catch (e) {
        continue;
      }
      if (cs.display === "none" || cs.visibility === "hidden") continue;
      if (parseFloat(cs.opacity) < 0.5) continue;
      var pos = cs.position;
      if (pos !== "fixed" && pos !== "absolute" && el.parentElement !== document.body) continue;

      var r = el.getBoundingClientRect();
      // It has to cover the viewport, or it is not standing in the way.
      if (r.width < vw * 0.9 || r.height < vh * 0.9) continue;
      if (r.top > vh * 0.1 || r.left > vw * 0.1) continue;

      // The overlay must be nearly empty, and what it does contain must read as
      // an invitation. A cookie banner covering the viewport is not an entry
      // gate, and neither is a full-screen menu.
      var txt = norm(el.innerText || el.textContent || "");
      if (txt.length > 200) continue;
      var controls;
      try {
        controls = el.querySelectorAll("button,a,[role=button],[onclick]");
      } catch (e) {
        controls = [];
      }
      for (var c = 0; c < controls.length && c < 12; c++) {
        var label = norm(controls[c].innerText || controls[c].textContent ||
          controls[c].getAttribute("aria-label") || "");
        if (label && label.length <= 40 && ENTER_WORDS.test(label)) {
          if (!best || label.length < best.length) best = label;
        }
      }
      // Some gates put the words on the overlay itself with no button at all.
      if (!best && txt && txt.length <= 40 && ENTER_WORDS.test(txt)) best = txt;
    }
    return best;
  }

  // ---------------------------------------------------------------------------
  // The sweep, driven from inside the page
  //
  // The scroll sweep used to live in Go: capture, decode, decide, scroll, wait,
  // repeat, with a CDP round trip for each of those and the whole extraction
  // re-serialised at every stop. On a page needing forty checkpoints that is a
  // hundred and sixty message round trips and forty full copies of a payload
  // that is ninety-five per cent identical to the last one -- and, worse, a
  // settle wait that could only be a fixed timeout because the process deciding
  // it was on the far side of a socket from the frames it was waiting for.
  //
  // Running the loop here collapses all of that into one call. The settle wait
  // becomes what it should always have been -- a count of animation frames,
  // measured where the frames actually happen -- and each checkpoint ships only
  // what it added, so the transfer is proportional to the page's content rather
  // than to the number of times we looked at it.
  //
  // Everything the Go side decided is still decided, with the same rules and
  // the same reasons. It is decided here because here is where the evidence is.
  // ---------------------------------------------------------------------------

  var UNREVEALED_THRESHOLD = 0.6;

  // ---------------------------------------------------------------------------
  // WebGL throttling
  //
  // Chromium is launched with SwiftShader, which rasterises WebGL on the CPU.
  // That is a deliberate choice -- the headless GPU path is the least
  // predictable part of the browser across machines, and identical output on
  // every machine is a stated goal -- but on a full-viewport 3D hero it means
  // every frame costs hundreds of milliseconds of main-thread time. Those are
  // the same milliseconds the sweep needs: a settle wait asked for 200ms comes
  // back after 700 because the thread was busy shading a scene, and pear.no
  // manages three checkpoints where an ordinary page manages forty.
  //
  // Sieve does not read those pixels. Canvas recovery works from the live scene
  // graph, the accessibility fallback and intercepted assets; rasterisation is
  // off unless OCR or a vision model is configured to look at the result. So
  // the drawing buffer is shrunk to something the CPU can fill instantly.
  //
  // The *drawing buffer* specifically, not the element. A canvas has two sizes:
  // the CSS box it occupies, which is what layout and therefore every text
  // position on the page depends on, and the pixel grid it renders into, which
  // nothing outside the canvas can observe. Changing the second leaves the first
  // untouched, so the page laid out for the reader is the page that is measured.
  // ---------------------------------------------------------------------------

  var THROTTLE_PX = 32;

  // pauseVideos stops video playback for the duration of the sweep.
  //
  // A headless Chromium decodes video on the CPU, on the same thread the sweep
  // needs, and a design-led site commonly autoplays two or three full-bleed
  // loops. pear.no runs a hero film and a footer loop; between them and the
  // WebGL they cost most of a frame, and the sweep waits a frame at every
  // checkpoint.
  //
  // Pausing changes no text and no layout: a video element occupies the same
  // box whether or not it is advancing, and what sieve records about it -- the
  // source, the accessible name, the caption -- is markup, not pixels. It is
  // also reversible and never observed, since nothing downstream reads a
  // playback position.
  function pauseVideos() {
    var n = 0;
    try {
      var vids = document.getElementsByTagName("video");
      for (var i = 0; i < vids.length && i < 40; i++) {
        try {
          if (!vids[i].paused) {
            vids[i].pause();
            n++;
          }
        } catch (e) {}
      }
    } catch (e) {}
    return n;
  }

  function throttleCanvases() {
    var n = 0;
    var els;
    try {
      els = document.getElementsByTagName("canvas");
    } catch (e) {
      return 0;
    }
    for (var i = 0; i < els.length && i < 40; i++) {
      var c = els[i];
      try {
        var kind = window.__sieveCanvasCtx ? window.__sieveCanvasCtx(c) : "";
        // 2D canvases are cheap and are sometimes used for text the page then
        // measures. Only the software-rasterised 3D contexts are worth this.
        if (kind.indexOf("webgl") < 0 && kind.indexOf("experimental-webgl") < 0) continue;
        if (c.width <= THROTTLE_PX && c.height <= THROTTLE_PX) continue;
        if (c.width * c.height < 4096) continue;
        c.width = THROTTLE_PX;
        c.height = THROTTLE_PX;
        n++;
      } catch (e) {}
    }
    return n;
  }

  function nowMs() {
    return typeof performance !== "undefined" && performance.now
      ? performance.now()
      : Date.now();
  }

  function documentHeight() {
    var de = document.documentElement;
    var b = document.body;
    return Math.max(
      de ? de.scrollHeight : 0,
      de ? de.offsetHeight : 0,
      b ? b.scrollHeight : 0,
      b ? b.offsetHeight : 0
    );
  }

  // pickProbes chooses elements whose movement reports how far the content
  // travelled.
  //
  // window.scrollY is the obvious measure and it is wrong on exactly the pages
  // that need measuring. A virtual scroller cancels native scrolling and
  // translates a wrapper instead, so scrollY stays at zero for the life of the
  // page while the content moves past the viewport perfectly well. Watching
  // where a handful of real elements actually are catches both cases with one
  // number, and it is what lets the sweep tell "this advance achieved nothing"
  // apart from "this page does not use the scrollbar".
  function pickProbes() {
    var out = [];
    var body = document.body;
    if (!body) return out;
    var kids = body.children;
    for (var i = 0; i < kids.length && out.length < 24; i++) {
      var el = kids[i];
      if (!el.tagName || SKIP[el.tagName] === 1) continue;
      out.push(el);
      // One level deeper as well: a great many sites put everything inside a
      // single wrapper, and a probe set of one element is a probe set that a
      // lone animation can fool.
      var gk = el.children;
      for (var j = 0; j < gk.length && out.length < 24; j++) {
        if (gk[j].tagName && SKIP[gk[j].tagName] !== 1) out.push(gk[j]);
      }
    }
    return out;
  }

  function readPos(probes) {
    var tops = new Array(probes.length);
    for (var i = 0; i < probes.length; i++) {
      try {
        tops[i] = probes[i].getBoundingClientRect().top;
      } catch (e) {
        tops[i] = 0;
      }
    }
    return { y: window.scrollY || window.pageYOffset || 0, tops: tops };
  }

  // progressBetween reports how far the content moved between two readings.
  //
  // The probe deltas are reduced with an upper quartile rather than a maximum.
  // A maximum would call a single element sliding in from the right "progress"
  // and let a stuck page sweep forever; a median would report zero on a page
  // where only the one scrolling wrapper moves. The quartile survives both.
  function progressBetween(a, b) {
    var scrolled = b.y - a.y;
    var deltas = [];
    for (var i = 0; i < a.tops.length && i < b.tops.length; i++) {
      deltas.push(a.tops[i] - b.tops[i]);
    }
    var moved = 0;
    if (deltas.length) {
      deltas.sort(function (x, y) {
        return y - x;
      });
      moved = deltas[Math.floor(deltas.length * 0.25)];
    }
    return Math.max(scrolled, moved, 0);
  }

  // findScrollContainer locates a scrollable element standing in for the
  // document. A page whose body is fixed at 100vh with the real content inside
  // an overflow:auto panel scrolls perfectly well -- just not via the window,
  // which is the only thing the sweep used to try.
  function findScrollContainer() {
    var vw = window.innerWidth || 1;
    var vh = window.innerHeight || 1;
    var best = null;
    var bestArea = 0;
    var els;
    try {
      els = document.querySelectorAll("body *");
    } catch (e) {
      return null;
    }
    for (var i = 0; i < els.length && i < 4000; i++) {
      var el = els[i];
      if (el.scrollHeight <= el.clientHeight + 8) continue;
      var cs;
      try {
        cs = getComputedStyle(el);
      } catch (e) {
        continue;
      }
      var oy = cs.overflowY;
      if (oy !== "auto" && oy !== "scroll" && oy !== "overlay") continue;
      var r = el.getBoundingClientRect();
      var area = r.width * r.height;
      if (area < 0.25 * vw * vh) continue;
      if (area > bestArea) {
        bestArea = area;
        best = el;
      }
    }
    return best;
  }

  // driveLibrary asks a recognised smooth-scroll library to move, using its own
  // API. This is both the fastest and the most faithful way to advance such a
  // page: it is the same call the library's own anchor links make.
  function driveLibrary(delta) {
    try {
      var lenis = window.lenis || window.__lenis || (window.Lenis && window.Lenis.instance);
      if (lenis && typeof lenis.scrollTo === "function") {
        var cur = typeof lenis.animatedScroll === "number" ? lenis.animatedScroll
          : typeof lenis.scroll === "number" ? lenis.scroll : window.scrollY || 0;
        lenis.scrollTo(cur + delta, { immediate: true, force: true });
        return "lenis";
      }
    } catch (e) {}
    try {
      var loco = window.locomotive || window.locoScroll || window.scroller;
      if (loco && typeof loco.scrollTo === "function") {
        var ly = (loco.scroll && loco.scroll.instance && loco.scroll.instance.scroll &&
          loco.scroll.instance.scroll.y) || 0;
        loco.scrollTo(ly + delta, { duration: 0, disableLerp: true });
        return "locomotive";
      }
    } catch (e) {}
    try {
      var ss = window.ScrollSmoother && window.ScrollSmoother.get && window.ScrollSmoother.get();
      if (ss && typeof ss.scrollTop === "function") {
        ss.scrollTop(ss.scrollTop() + delta);
        return "scrollsmoother";
      }
    } catch (e) {}
    return "";
  }

  // dispatchWheel sends real wheel events from inside the page.
  //
  // These used to be sent over CDP as synthesised input, one round trip each,
  // with the page's handler billed to the sweep synchronously -- organimo.com
  // spent forty-six seconds on five of them. Dispatched here they cost
  // microseconds. A scroll hijacker cannot tell the difference: it reads
  // deltaY off the event, and the one thing an untrusted event cannot do --
  // trigger the browser's own scrolling -- is the one thing such a library
  // cancels anyway.
  function dispatchWheel(delta) {
    var x = (window.innerWidth || 1000) / 2;
    var y = (window.innerHeight || 800) / 2;
    var target = null;
    try {
      target = document.elementFromPoint(x, y);
    } catch (e) {}
    if (!target) target = document.body || document.documentElement;
    if (!target) return false;
    // Several ticks rather than one: a library that clamps per-event delta
    // would swallow a single large one whole.
    var ticks = 3;
    var per = delta / ticks;
    for (var i = 0; i < ticks; i++) {
      try {
        target.dispatchEvent(
          new WheelEvent("wheel", {
            deltaY: per, deltaX: 0, deltaMode: 0,
            clientX: x, clientY: y,
            bubbles: true, cancelable: true, composed: true,
          })
        );
      } catch (e) {
        return false;
      }
    }
    return true;
  }

  function scrollWindowBy(delta) {
    var before = window.scrollY || window.pageYOffset || 0;
    try {
      window.scrollTo({ top: before + delta, behavior: "instant" });
    } catch (e) {
      window.scrollTo(0, before + delta);
    }
  }

  // advance moves the page, trying the mechanisms in order of cost and
  // fidelity. It does not report how far it got: on a smooth-scroll library the
  // movement happens over the following frames, so the only honest measurement
  // is taken by the caller after the settle wait.
  function advance(delta, st) {
    var moved = false;
    if (!st.virtual) {
      var y0 = window.scrollY || window.pageYOffset || 0;
      scrollWindowBy(delta);
      if (Math.abs((window.scrollY || window.pageYOffset || 0) - y0) > 1) {
        st.mode = "window";
        return;
      }
    }
    if (!st.container && !st.containerChecked) {
      st.containerChecked = true;
      st.container = findScrollContainer();
    }
    if (st.container) {
      var c0 = st.container.scrollTop;
      st.container.scrollTop = c0 + delta;
      if (Math.abs(st.container.scrollTop - c0) > 1) {
        st.mode = "container";
        return;
      }
    }
    var lib = driveLibrary(delta);
    if (lib) {
      st.mode = lib;
      moved = true;
    }
    // The wheel goes out even when a library API answered: several of these
    // libraries only commit a programmatic target on the next input, and a
    // wheel event costs nothing here.
    if (dispatchWheel(delta) && !moved) st.mode = "wheel";
  }

  // ---------------------------------------------------------------------------
  // Where the unread text is
  //
  // A capture sees the whole document, not just the viewport: every text run in
  // the page is measured, including the ones sitting at opacity zero waiting to
  // be scrolled to. Each of those carries its document position. So after the
  // very first checkpoint the sweep already knows, precisely, where the words it
  // has not been able to read are.
  //
  // Stepping down the page in fixed increments throws that away. It spends
  // checkpoints on empty stretches, lands in the gaps between narrow reveal
  // bands, and on a tall document runs out of budget somewhere in the middle --
  // pear.no has perhaps ten sections and a linear sweep needs forty stops to be
  // sure of covering them, at six hundred milliseconds each.
  //
  // Visiting the positions instead turns "cover the document" into "go to the
  // ten places that have something to show", which is both far fewer stops and
  // a better aim: each visit lands the text in the middle of the viewport,
  // comfortably past whatever threshold its reveal is watching for.
  // ---------------------------------------------------------------------------

  // TARGET_BUCKET groups nearby runs into one visit, as a fraction of viewport
  // height. Everything within one bucket is revealed by the same stop.
  var TARGET_BUCKET = 0.5;

  // noteTargets records where this checkpoint saw text it could not read, and
  // reports how much of that text is pinned.
  //
  // Pinned text is the case targeting cannot serve. A section held with
  // position:fixed or sticky has no document coordinate to travel to -- its box
  // is measured against the viewport, so it reports the same place at every
  // scroll offset. Such pages reveal by scrubbing an animation against scroll
  // distance rather than by moving content past the viewport, and the only way
  // to find their content is to sample the scroll range.
  function noteTargets(snap, targets, vh) {
    var bucket = Math.max(120, vh * TARGET_BUCKET);
    noteTargets.pinned = 0;
    noteTargets.free = 0;
    for (var i = 0; i < snap.nodes.length; i++) {
      var n = snap.nodes[i];
      if (n.o <= MIN_VISIBLE_OPACITY && n.x && n.x.length >= 4) {
        if (n.fx) noteTargets.pinned += n.x.length;
        else noteTargets.free += n.x.length;
      }
      if (n.fx) continue;
      if (n.o > MIN_VISIBLE_OPACITY) continue;
      if (!n.x || n.x.length < 4) continue;
      var y = n.bb[1];
      if (!isFinite(y) || y < 0) continue;
      var key = Math.round(y / bucket);
      var cur = targets.get(key);
      if (cur === undefined) targets.set(key, { y: y, chars: n.x.length });
      else cur.chars += n.x.length;
    }
  }

  // nextTarget picks the nearest unvisited place worth going to, preferring the
  // next one below the current position so the sweep still travels in reading
  // order where it can.
  function nextTarget(targets, visited, curTop, vh) {
    var bucket = Math.max(120, vh * TARGET_BUCKET);
    var bestBelow = null;
    var bestAny = null;
    targets.forEach(function (t, key) {
      if (visited.has(key)) return;
      if (bestAny === null || t.y < bestAny.y) bestAny = { key: key, y: t.y };
      if (t.y >= curTop + vh * 0.25) {
        if (bestBelow === null || t.y < bestBelow.y) bestBelow = { key: key, y: t.y };
      }
    });
    var pick = bestBelow || bestAny;
    if (!pick) return null;
    // Land the text just above the middle of the viewport: past any reasonable
    // reveal threshold, and far enough from the edges that a band either side of
    // it comes along for free.
    return { key: pick.key, top: Math.max(0, pick.y - vh * 0.45), bucket: bucket };
  }

  // markVisited retires every bucket the viewport now covers, not only the one
  // aimed at: a stop reveals a whole screen, and re-visiting the rest of that
  // screen one bucket at a time is the fixed-step sweep by another name.
  function markVisited(targets, visited, curTop, vh) {
    var bucket = Math.max(120, vh * TARGET_BUCKET);
    var lo = curTop - vh * 0.1;
    var hi = curTop + vh * 1.1;
    targets.forEach(function (t, key) {
      if (t.y >= lo && t.y <= hi) visited.add(key);
    });
  }

  // shareUnrevealed reports what fraction of the text captured at this
  // checkpoint is in the document but not currently legible. Measured in
  // characters, so a headline shattered into forty single-character spans does
  // not outvote a paragraph.
  function shareUnrevealed(snap) {
    var total = 0;
    var hidden = 0;
    // Returned alongside the share so the caller can tell "most of this page is
    // waiting to be revealed" from "there are four words here and one of them
    // is faded". The virtual-scroller override rests on that distinction: on a
    // page with almost no text, a high unrevealed share is arithmetic on a
    // sample of three, and acting on it kept igloo.inc -- which has two blocks
    // and no scroll -- dispatching wheel events for sixty-three checkpoints.
    shareUnrevealed.chars = 0;
    shareUnrevealed.visible = 0;
    for (var i = 0; i < snap.nodes.length; i++) {
      var n = snap.nodes[i];
      var c = n.x.length;
      total += c;
      if (n.o <= MIN_VISIBLE_OPACITY) hidden += c;
      else if (n.v) shareUnrevealed.visible += c;
    }
    shareUnrevealed.chars = total;
    return total === 0 ? 0 : hidden / total;
  }

  // MIN_UNREVEALED_CHARS is how much text must be present before the share of it
  // that is unrevealed means anything.
  var MIN_UNREVEALED_CHARS = 200;

  // reduce strips a capture down to what this checkpoint actually added.
  //
  // The merge on the Go side is idempotent, so sending a run it already holds
  // is harmless -- and it is also ninety-five per cent of the payload. What it
  // must not lose is a *better* sighting: a run first seen mid-fade at opacity
  // 0.1 and later fully revealed has to be sent again, because the second
  // sighting is the one carrying the geometry and the opacity the graph will
  // use. So a record is re-sent exactly when it improves on the last one sent,
  // and dropped when it does not.
  function reduce(snap, seen) {
    var fresh = 0;
    var nodes = [];
    for (var i = 0; i < snap.nodes.length; i++) {
      var n = snap.nodes[i];
      var k = n.p + " " + n.x;
      var score = n.o + (n.v ? 2 : 0);
      var prev = seen.nodes.get(k);
      if (prev === undefined) {
        seen.nodes.set(k, score);
        nodes.push(n);
        fresh++;
      } else if (score > prev + 0.02) {
        seen.nodes.set(k, score);
        nodes.push(n);
      }
    }
    snap.nodes = nodes;

    var latent = [];
    for (var j = 0; j < snap.latent.length; j++) {
      var l = snap.latent[j];
      var lk = l.p + " " + l.x;
      if (seen.latent.has(lk)) continue;
      seen.latent.add(lk);
      latent.push(l);
    }
    snap.latent = latent;

    var actions = [];
    for (var a = 0; a < snap.actions.length; a++) {
      var act = snap.actions[a];
      var ap = seen.actions.get(act.p);
      var asig = (act.l || "") + " " + (act.h || "");
      if (ap === asig) continue;
      seen.actions.set(act.p, asig);
      actions.push(act);
    }
    snap.actions = actions;

    var media = [];
    for (var m = 0; m < snap.media.length; m++) {
      var md = snap.media[m];
      var mk = md.s || md.p;
      var marea = md.bb[2] * md.bb[3];
      var mprev = seen.media.get(mk);
      if (mprev !== undefined && marea <= mprev * 1.2) continue;
      seen.media.set(mk, Math.max(marea, mprev || 0));
      media.push(md);
    }
    snap.media = media;

    var canvases = [];
    for (var c = 0; c < snap.canvases.length; c++) {
      var cv = snap.canvases[c];
      var cprev = seen.canvases.get(cv.p);
      if (cprev !== undefined && cv.vs <= cprev) continue;
      seen.canvases.set(cv.p, cv.vs);
      canvases.push(cv);
    }
    snap.canvases = canvases;

    var disc = [];
    for (var d = 0; d < snap.disc.length; d++) {
      var dc = snap.disc[d];
      var dk = dc.k + " " + (dc.l || "");
      if (!dc.l || seen.disc.has(dk)) continue;
      seen.disc.add(dk);
      disc.push(dc);
    }
    snap.disc = disc;

    return fresh;
  }

  // sweep walks the viewport down the document and returns every checkpoint's
  // contribution in one reply.
  function sweep(cfg) {
    cfg = cfg || {};
    var budgetMs = cfg.budgetMs || 6000;
    var maxCheckpoints = cfg.maxCheckpoints || 200;
    var stableK = cfg.stableCheckpoints || 3;
    var settleFrames = cfg.settleFrames || 3;
    var settleMaxMs = cfg.settleMaxMs || 260;
    var settleMinMs = cfg.settleMinMs || 60;
    var nodeBudget = cfg.nodeBudget || 40000;
    var latentBudget = cfg.latentBudget || 12000;
    var maxScrollPx = cfg.maxScrollPx || 120000;
    var stepRatio = cfg.stepRatio || 0.75;
    var maxPasses = cfg.passes || 2;
    // How long a reveal is given to happen on a page where nothing has been
    // legible yet. Web fades run 300-800ms; this is the low end of useful.
    var revealFloorMs = cfg.revealFloorMs || 450;
    var throttleGL = cfg.throttleGL !== false;

    var t0 = nowMs();
    var vh = window.innerHeight || 800;
    var minStep = Math.max(120, vh * 0.18);
    var maxStep = vh * 0.95;
    var step = Math.min(maxStep, Math.max(minStep, vh * stepRatio));

    var st = {
      // Located lazily. The scan resolves styles across the whole document, and
      // on the great majority of pages the window scrolls perfectly well and the
      // answer is never needed.
      container: null,
      containerChecked: false,
      mode: "window",
      virtual: false,
    };
    var seen = {
      nodes: new Map(), latent: new Set(), actions: new Map(),
      media: new Map(), canvases: new Map(), disc: new Set(),
    };

    var snaps = [];
    var notes = [];
    var probes = pickProbes();

    // The sweep publishes as it goes.
    //
    // Everything it has seen is returned in one reply at the end, which is what
    // makes it cheap -- and what made it fragile: a call cancelled one
    // checkpoint before it finished returned nothing at all, and a page that
    // had been swept perfectly well for three seconds produced an empty
    // artifact. The accumulated state lives in a global as well, so the driver
    // can come back for it when its own deadline arrives first.
    window.__sieveSweepOut = {
      snaps: snaps, notes: notes, checkpoints: 0, passes: 0,
      reachedBottom: false, mode: st.mode, virtual: false,
      refinements: 0, step: 0, captureMs: 0, settleMs: 0, totalMs: 0,
      partial: true,
    };
    var stalls = 0;
    var stableRun = 0;
    var denseMisses = 0;
    var refinements = 0;
    var travelled = 0;
    var atBottom = false;
    var reachedBottom = false;
    var captureTotal = 0;
    var settleTotal = 0;
    var captureAvg = 30;
    var cp = 0;
    var pass = 0;
    var settleMisses = 0;
    var everSawVisible = false;
    var scrollTotal = 0;
    var settleWorst = 0;
    var captureWorst = 0;

    // Once, before the first checkpoint.
    var throttled = throttleGL ? throttleCanvases() : 0;
    var pausedVideos = throttleGL ? pauseVideos() : 0;

    // Where text is known to be waiting, and where the sweep has already been.
    var targets = new Map();
    var visited = new Set();
    var targetedStops = 0;
    var pinnedChars = 0;
    var freeChars = 0;

    function note(s) {
      if (notes.indexOf(s) < 0) notes.push(s);
    }

    function publish() {
      var out = window.__sieveSweepOut;
      out.checkpoints = cp;
      out.passes = pass;
      out.reachedBottom = reachedBottom;
      out.mode = st.mode;
      out.virtual = st.virtual;
      out.refinements = refinements;
      out.step = Math.round(step);
      out.throttledCanvases = throttled;
      out.targetedStops = targetedStops;
      out.targetsFound = targets.size;
      out.sawVisible = everSawVisible;
      out.pausedVideos = pausedVideos;
      out.pinnedChars = pinnedChars;
      out.freeChars = freeChars;
      out.scrollMs = Math.round(scrollTotal);
      out.settleWorstMs = Math.round(settleWorst);
      out.captureWorstMs = Math.round(captureWorst);
      out.captureMs = Math.round(captureTotal);
      out.settleMs = Math.round(settleTotal);
      out.totalMs = Math.round(nowMs() - t0);
      return out;
    }

    function done(why) {
      var out = publish();
      out.partial = false;
      out.stopReason = why || "unspecified";
      return JSON.stringify({
        snaps: snaps,
        notes: notes,
        checkpoints: cp,
        passes: pass,
        reachedBottom: reachedBottom,
        mode: st.mode,
        virtual: st.virtual,
        refinements: refinements,
        stopReason: window.__sieveSweepOut.stopReason || "",
        throttledCanvases: throttled,
        targetedStops: targetedStops,
        targetsFound: targets.size,
        sawVisible: everSawVisible,
        pausedVideos: pausedVideos,
        pinnedChars: pinnedChars,
        freeChars: freeChars,
        scrollMs: Math.round(scrollTotal),
        settleWorstMs: Math.round(settleWorst),
        captureWorstMs: Math.round(captureWorst),
        step: Math.round(step),
        captureMs: Math.round(captureTotal),
        settleMs: Math.round(settleTotal),
        totalMs: Math.round(nowMs() - t0),
      });
    }

    // rewind puts the viewport back at the top of the document, and says
    // whether it managed it.
    function rewind() {
      // A virtual scroller has no position to restore: the sweep drove it with
      // wheel events and there is no reliable way to wind those back. Claiming
      // otherwise would start a second pass halfway down the page and report
      // the top of the document as swept twice when it was swept once.
      if (st.virtual) return false;
      try {
        window.scrollTo({ top: 0, behavior: "instant" });
      } catch (e) {
        window.scrollTo(0, 0);
      }
      if (st.container) st.container.scrollTop = 0;
      driveLibrary(-(documentHeight() + 10000));
      var y = window.scrollY || window.pageYOffset || 0;
      var cy = st.container ? st.container.scrollTop : 0;
      return y < vh * 0.5 && cy < vh * 0.5;
    }

    // endPass decides whether the document is worth walking again.
    //
    // Almost every scroll reveal on the web fires once and stays fired: an
    // IntersectionObserver that unobserves, a GSAP `from` that has played, a
    // class added and never removed. The first pass is therefore as much a
    // trigger as a capture -- it puts the whole page into its revealed state --
    // and a second pass over an already-revealed page sees at full opacity what
    // the first pass could only catch mid-fade.
    //
    // That is a far better use of a second of budget than dwelling a second
    // longer at each checkpoint of a single pass, because dwelling only helps
    // the section being dwelt on, and the second pass helps all of them. It
    // costs almost nothing to send: everything it re-observes is already known,
    // so only genuine improvements cross the wire.
    // How much readable text has been seen so far, in characters.
    //
    // Counting runs instead was the wrong measure: a loading screen with seven
    // language links and a progress number has plenty of runs and nothing to
    // say, so hatom.com passed the "did we find anything" test while showing
    // its pre-loader and the sweep finished satisfied.
    function textSeen() {
      var n = 0;
      seen.nodes.forEach(function (_, k) {
        var i = k.indexOf(" ");
        n += i < 0 ? 0 : k.length - i - 1;
      });
      return n;
    }

    // emptyEnough is the point below which a page has told us nothing.
    //
    // A sweep that ends having seen a handful of runs has either found a page
    // with nothing on it or looked before the page was ready, and from inside
    // the loop those are indistinguishable. They are not equally likely: a site
    // worth distilling almost always has words, and the cost of looking again
    // is one settle wait against an artifact that says the page was empty.
    var EMPTY_ENOUGH = 240;
    var reLooks = 0;

    function endPass() {
      var remaining = budgetMs - (nowMs() - t0);

      // Before concluding that a page is empty, look again.
      if (textSeen() < EMPTY_ENOUGH && reLooks < 3 && remaining > 1200) {
        reLooks++;
        // Only the stability counter is cleared. Clearing the position as well
        // would send the sweep back down a document it has already covered,
        // which on a page that genuinely has nothing is a great deal of work to
        // confirm it a second time: igloo.inc turned two checkpoints into
        // twenty-eight that way. Waiting where we stand and looking once more
        // is the whole of what is wanted.
        stableRun = 0;
        note("this page had produced almost no text when the sweep first reached " +
          "the end of it, so the sweep waited and looked again");
        return window.__sieve
          .settle(settleFrames, Math.min(900, remaining / 3))
          .then(iterate);
      }

      pass++;
      if (pass < maxPasses && reachedBottom && remaining > budgetMs * 0.25 && rewind()) {
        stalls = 0;
        stableRun = 0;
        denseMisses = 0;
        atBottom = false;
        reachedBottom = false;
        step = Math.min(maxStep, Math.max(minStep, vh * stepRatio));
        return window.__sieve
          .settle(settleFrames, Math.min(settleMaxMs, 200))
          .then(iterate);
      }
      return done("stable");
    }

    // One iteration. Written as a promise chain rather than a loop because the
    // settle wait is asynchronous and the whole point of this rewrite is that
    // waiting happens on the frame clock.
    function iterate() {
      if (cp >= maxCheckpoints) {
        note("checkpoint cap of " + maxCheckpoints + " reached before the document bottom");
        return done("checkpoint-cap");
      }
      var elapsed = nowMs() - t0;
      // cp 0 is exempt. A budget that has already run out must still yield one
      // capture: an artifact built from a single checkpoint is thin, and an
      // artifact built from none is empty, and those are very different
      // failures to hand somebody.
      if (elapsed >= budgetMs && cp > 0) {
        if (!atBottom && pass === 0) {
          note("time budget exhausted mid-sweep; the lower part of the page was not swept");
        }
        return done("budget");
      }

      var tc = nowMs();
      var snap = capture(cp, nodeBudget, latentBudget);
      var dc = nowMs() - tc;
      captureTotal += dc;
      if (dc > captureWorst) captureWorst = dc;
      captureAvg = cp === 0 ? dc : captureAvg * 0.7 + dc * 0.3;

      publish();

      // Targets are read from the full capture, before it is reduced to a
      // delta: the runs that matter here are the ones this checkpoint could not
      // read, and most of those were already known and would be stripped.
      if (st.mode === "window" && !st.virtual) {
        noteTargets(snap, targets, vh);
        markVisited(targets, visited, snap.sy, vh);
        pinnedChars = Math.max(pinnedChars, noteTargets.pinned);
        freeChars = Math.max(freeChars, noteTargets.free);
      }

      // Applied once, at the start, and never again.
      //
      // It was re-applied every few checkpoints, on the theory that a page
      // which rebuilds its drawing buffer would otherwise undo it. What that
      // actually did was reallocate a WebGL surface underneath a running
      // renderer over and over, and some pages respond by rebuilding their
      // whole scene -- seconds of main-thread work each time, during which
      // nothing in this loop can run, including the checks that would have
      // ended it. pear.no returned fewer blocks with a twenty-second budget
      // than with a ten-second one, which is not a thing a page can do to
      // itself. One application is the whole of the benefit; the repeats were
      // all cost.
      var lowShare = shareUnrevealed(snap);
      var sampledChars = shareUnrevealed.chars;
      if (shareUnrevealed.visible > 0) everSawVisible = true;
      var docH = st.container ? st.container.scrollHeight : snap.dh;
      var pos = st.container ? st.container.scrollTop : snap.sy;
      var fresh = reduce(snap, seen);
      snaps.push(snap);
      cp++;

      // On the first pass progress means new content. On a later pass every run
      // is already known, so the measure of progress is whether this checkpoint
      // saw anything *better* than what is held -- which is exactly what a
      // second pass exists to find.
      // A budget check at the top of the loop only ever notices an overrun
      // that has already happened. Predicting the next capture from the cost of
      // the last one is what actually keeps the sweep inside its allowance --
      // and staying inside it is what lets the reply be delivered at all.
      // Priced from the observed rate, not from the capture alone: on a page
      // rendering at two frames a second a checkpoint costs six hundred
      // milliseconds, and predicting the next one at twenty guarantees the
      // overrun this check exists to prevent.
      var nextCost = cp > 0 ? Math.max(captureAvg, (nowMs() - t0) / cp) : captureAvg;
      if (cp > 1 && nowMs() - t0 + nextCost >= budgetMs) {
        if (!atBottom && pass === 0) {
          note("time budget exhausted mid-sweep; the lower part of the page was not swept");
        }
        return done("budget-predicted");
      }

      var progressed = pass === 0 ? fresh : snap.nodes.length;
      if (progressed > 0) {
        stalls = 0;
        stableRun = 0;
      } else {
        stableRun++;
      }

      // Sample more finely when text is present but not being revealed. A
      // scroll-scrubbed pinned section fades its content in and out within a
      // band of scroll positions sometimes two hundred pixels wide, and a
      // three-quarter-viewport step lands in the gaps between those bands.
      // Refining doubles the number of checkpoints needed to cover what is
      // left, so it is only worth doing while there is room for them.
      var canAffordFiner =
        (nowMs() - t0) / Math.max(1, cp) * 2 * Math.max(1, (docH - (pos + vh)) / step) <
        (budgetMs - (nowMs() - t0));
      if (lowShare >= UNREVEALED_THRESHOLD && sampledChars >= MIN_UNREVEALED_CHARS &&
          fresh > 0 && canAffordFiner) {
        denseMisses++;
        if (denseMisses >= 2 && step > minStep) {
          step = Math.max(minStep, step / 2);
          denseMisses = 0;
          refinements++;
        }
      } else {
        denseMisses = 0;
      }

      atBottom = pos + vh >= docH - 2;

      // A document claiming to be one viewport tall, while most of the text
      // captured inside it is still at zero opacity, is not a short page: it is
      // a virtual scroller, and believing its height would end the sweep before
      // the content had started.
      if (atBottom && lowShare >= UNREVEALED_THRESHOLD &&
          sampledChars >= MIN_UNREVEALED_CHARS && stalls < 2) {
        st.virtual = true;
        atBottom = false;
      }

      // Unvisited places outrank every stopping rule below. "Nothing new at
      // this stop" is not evidence about a section the sweep has not reached.
      var pending = st.mode === "window" && !st.virtual &&
        nextTarget(targets, visited, snap.sy, vh) !== null;

      if (!pending && stableRun >= stableK && (atBottom || stalls >= 2)) {
        reachedBottom = reachedBottom || atBottom;
        return endPass();
      }
      if (!pending && atBottom && stableRun >= 1) {
        reachedBottom = true;
        return endPass();
      }
      if (travelled >= maxScrollPx) {
        note("scroll budget of " + Math.round(maxScrollPx) +
          "px exhausted; the page continued below this point");
        return done("scroll-budget");
      }

      // Plan the rest of the sweep against the time that is left. Covering the
      // whole document coarsely beats covering a third of it finely: a section
      // never reached contributes nothing at any sampling rate.
      var remainingMs = budgetMs - (nowMs() - t0);
      var remainingPx = Math.max(0, docH - (pos + vh));

      // What a checkpoint actually costs, measured, not assumed.
      //
      // The planner used to price a checkpoint at capture time plus the settle
      // floor. On a page rendering at two frames a second that is wrong by an
      // order of magnitude -- the settle cannot return before the page produces
      // a frame, whatever budget it was given -- so the sweep believed it could
      // afford forty checkpoints, planned a fine step, and ran out of time a
      // fifth of the way down. Pricing from the observed rate makes the plan
      // match the page.
      var observedPerCp = cp > 0 ? (nowMs() - t0) / cp : captureAvg + settleMinMs;
      var perCp = Math.max(captureAvg + settleMinMs, observedPerCp);
      var affordable = Math.max(1, Math.floor(remainingMs / perCp));
      var wantCp = remainingPx > 0 ? Math.ceil(remainingPx / step) : 1;

      // Coverage first. A section the viewport never reaches contributes
      // nothing at any sampling rate, so when the document cannot be covered at
      // the current step the step widens -- even if the unrevealed-text rule
      // asked for a finer one. Those two rules were in direct conflict and the
      // finer one was winning: pear.no halved its step on a budget that could
      // not cover the page at the original one, and swept the top fifth of the
      // site twice as carefully as it needed to.
      // On a pinned page the one-viewport clamp is meaningless.
      //
      // The clamp exists so that a step never jumps over a screen of content.
      // That reasoning holds where content is laid out along the scroll axis:
      // scroll a viewport, a viewport of new material arrives. A page that pins
      // its sections and scrubs them against scroll distance lays nothing out
      // along that axis -- the same pinned box is on screen for thousands of
      // pixels while its contents cross-fade -- so a bigger step does not skip
      // content, it advances the animation further. Holding pear.no to 855px
      // steps meant covering four thousand pixels of a twenty thousand pixel
      // scrub and calling it a sweep.
      var pinnedDominant = pinnedChars > freeChars && pinnedChars > 200;
      // Loosened, not removed. A pinned page can be stepped further than one
      // viewport because nothing is laid out along the scroll axis, but a step
      // of twenty thousand pixels sails past the sections near the top that do
      // render early -- pear.no lost its hero and its three pillars that way,
      // which are the part of the page only the browser can supply.
      var cap = pinnedDominant ? Math.min(maxStep * 3, vh * 3) : maxStep;
      if (wantCp > affordable && remainingPx > 0) {
        var wider = Math.min(cap, remainingPx / affordable);
        if (wider > step) {
          step = wider;
          denseMisses = 0;
        }
        wantCp = Math.ceil(remainingPx / step);
      }
      var allowance = remainingMs / Math.max(1, Math.min(wantCp, affordable));
      var settleBudget = Math.max(settleMinMs,
        Math.min(settleMaxMs, allowance - captureAvg));
      // A page that has refused to settle three times running will refuse
      // again. Paying full price for an answer it has already declined to give
      // spends on stillness the budget that should be spent on covering the
      // document.
      if (settleMisses >= 3) settleBudget = settleMinMs;

      // Seeing nothing is a reason to slow down, not to hurry.
      //
      // The rule above exists for a page with a permanent animation, which will
      // never report itself still however long we wait; cutting the wait there
      // buys checkpoints for free. But "never settles" and "never reveals" are
      // different conditions, and on a page that fades every run in they occur
      // together -- so the shortcut fires, the wait collapses to its floor, and
      // the sweep starts capturing faster than the page can animate.
      //
      // A portfolio was swept fifty-seven times at sixty-five milliseconds a
      // stop and observed not one character above the visible-opacity floor.
      // Every run was dropped, the audit reported nought per cent retention,
      // and the remedy was not more stops but slower ones: the same page read
      // correctly at eighteen.
      //
      // So when text is being captured and none of it has ever been legible,
      // the wait is raised instead. It is bounded by the budget, so a page that
      // genuinely has nothing to reveal costs a handful of checkpoints to find
      // that out rather than the whole sweep.
      if (!everSawVisible && cp >= 2 && sampledChars > 0) {
        var patient = Math.min(revealFloorMs, Math.max(settleMaxMs, remainingMs / 4));
        if (patient > settleBudget) settleBudget = patient;
      }

      var tScroll = nowMs();
      var before = readPos(probes);

      // Go to the next place that has something to show, when there is one.
      //
      // The fixed step remains the fallback, and it is what drives every page
      // whose position cannot be trusted: a virtual scroller, a container
      // scroll, a document that reports a height it does not have. Everywhere
      // else, the sweep now travels between the places it has seen unread text
      // rather than marching past them.
      var tgt = null;
      if (st.mode === "window" && !st.virtual) {
        tgt = nextTarget(targets, visited, snap.sy, vh);
      }
      if (tgt !== null && Math.abs(tgt.top - snap.sy) > vh * 0.15) {
        visited.add(tgt.key);
        targetedStops++;
        try {
          window.scrollTo({ top: tgt.top, behavior: "instant" });
        } catch (e) {
          window.scrollTo(0, tgt.top);
        }
        st.mode = "window";
      } else {
        if (tgt !== null) visited.add(tgt.key);
        advance(step, st);
      }
      scrollTotal += nowMs() - tScroll;

      var ts = nowMs();
      return window.__sieve.settle(settleFrames, settleBudget).then(function (sr) {
        var dSettle = nowMs() - ts;
        settleTotal += dSettle;
        if (dSettle > settleWorst) settleWorst = dSettle;
        if (sr && sr.settled) settleMisses = 0;
        else settleMisses++;
        var after = readPos(probes);
        var moved = progressBetween(before, after);
        travelled += moved;
        // A targeted jump is progress by definition: it went somewhere it had
        // not been. Judging it against the fixed step would count a deliberate
        // short hop to the next section as a stall and end the sweep early.
        if (tgt !== null || moved >= step * 0.2) stalls = 0;
        else stalls++;
        // A page that will not move under the window scrollbar is worth
        // re-examining once: the container that does scroll may only have been
        // mounted after the first paint.
        if (stalls === 2 && !st.container) {
          st.containerChecked = true;
          st.container = findScrollContainer();
        }
        return iterate();
      });
    }

    return Promise.resolve().then(iterate);
  }

  window.__sieve = {
    capture: function (n, budget, latentBudget) {
      return JSON.stringify(capture(n, budget, latentBudget));
    },

    // sweep runs the whole checkpoint loop in the page and returns every
    // checkpoint's contribution in a single reply.
    sweep: function (cfg) {
      return sweep(cfg);
    },

    // sweepResult hands back whatever the sweep has accumulated so far. It is
    // the recovery path for a driver whose own deadline arrived first: the
    // checkpoints already taken are real observations and there is no reason to
    // throw them away because the last one did not fit.
    sweepResult: function () {
      return JSON.stringify(window.__sieveSweepOut || null);
    },

    // gate reports what is standing between sieve and the page: a loader, a
    // control it may press, or one it will not.
    gate: function () {
      try {
        return JSON.stringify(gateState());
      } catch (e) {
        return "null";
      }
    },

    // probe answers, in one call, everything the sweep needs to know about the
    // page before it starts. These were three separate evaluations returning
    // three short strings, which is three round trips for one round trip's
    // worth of information.
    probe: function (specs) {
      var gate = "";
      try {
        gate = detectEntryGate();
      } catch (e) {}
      return JSON.stringify({
        url: window.location.href,
        libs: detectLibraries(specs || []),
        gate: gate,
      });
    },

    // corpusText and scene are called once each, after the sweep, and only on a
    // page that has a canvas worth recovering.
    corpusText: function () {
      return JSON.stringify({
        inline: collectCorpus(document),
        page: (document.body && document.body.innerText || "").slice(0, 1000000),
      });
    },
    scene: function () {
      return JSON.stringify(introspectScene() || null);
    },
    libs: function (specs) {
      return JSON.stringify(detectLibraries(specs || []));
    },

    // settle resolves once the page has shown no layout change for
    // `frames` consecutive animation frames, or once `timeoutMs` elapses.
    //
    // It runs in the page rather than as a Go polling loop for two reasons.
    // A CDP round trip is 1-3ms, so polling from outside samples at ~300Hz at
    // best and burns a message per sample; requestAnimationFrame samples
    // exactly at the rate the compositor actually paints. And a scroll-driven
    // animation is defined against frames, so frames are the honest unit.
    //
    // The signature deliberately samples hit-tested elements on a grid rather
    // than measuring every element. Reading 16 rects per frame is affordable;
    // reading 40,000 is not, and the grid catches precisely the transforms
    // that are visible, which is the only kind that matters.
    // settle takes two budgets, because it answers two questions.
    //
    // readyTimeoutMs bounds the wait for the document to finish loading, which
    // is the site's time and can legitimately be long. timeoutMs bounds the wait
    // for it to stop moving *after* that, which is sieve's time and should not
    // be. Charging both to one clock meant a page that loads promptly and then
    // animates forever -- suzanne3d.com, and most of this corpus -- burned the
    // entire loading allowance proving it would never be still: nineteen and a
    // half seconds to reach a page that had been ready in two.
    settle: function (frames, timeoutMs, readyTimeoutMs) {
      if (!readyTimeoutMs) readyTimeoutMs = timeoutMs;
      return new Promise(function (resolve) {
        var t0 =
          typeof performance !== "undefined" && performance.now
            ? performance.now()
            : Date.now();
        var now = function () {
          return typeof performance !== "undefined" && performance.now
            ? performance.now()
            : Date.now();
        };
        var vw = window.innerWidth || 1;
        var vh = window.innerHeight || 1;
        // Nine sample points, not sixteen.
        //
        // Every point costs a hit test, a rectangle and a style resolution, and
        // the whole signature is recomputed on every frame of every settle wait
        // on every checkpoint. Nine points on a three-by-three grid catch the
        // same transforms -- a reveal that misses all nine is not a reveal
        // anyone would see -- for a little over half the per-frame cost.
        var pts = [];
        for (var i = 1; i <= 3; i++) {
          for (var j = 1; j <= 3; j++) {
            pts.push([(vw * i) / 4, (vh * j) / 4]);
          }
        }
        var de = document.documentElement;
        var last = null;
        var stable = 0;
        var done = false;

        // A headless tab that is not being composited starves
        // requestAnimationFrame indefinitely. The sweep brings the tab to the
        // front to avoid that, but a starved rAF must never be able to hang the
        // whole distillation, so every tick is also armed with a timer. The
        // first of the two to fire advances the loop and disarms the other.
        function schedule() {
          var fired = false;
          var go = function () {
            if (fired || done) return;
            fired = true;
            tick();
          };
          requestAnimationFrame(go);
          setTimeout(go, 24);
        }

        function sig() {
          var parts = [
            de ? de.scrollHeight : 0,
            document.getElementsByTagName("*").length,
            window.__sieveMutations || 0,
          ];
          for (var k = 0; k < pts.length; k++) {
            var el;
            try {
              el = document.elementFromPoint(pts[k][0], pts[k][1]);
            } catch (e) {
              el = null;
            }
            if (!el) {
              parts.push(-1);
              continue;
            }
            var r = el.getBoundingClientRect();
            parts.push(
              (Math.round(r.left) << 1) ^
                (Math.round(r.top) * 31) ^
                (Math.round(r.width) * 131) ^
                (Math.round(r.height) * 313)
            );
            // Opacity has to be in the signature, not just geometry.
            //
            // The most common reveal on the web is a pure opacity fade, and a
            // fade moves nothing: a geometry-only stability check declares the
            // page settled while text is still at 0.4, the capture records that
            // opacity, and the block is filed as low confidence for the rest of
            // its life. Two style resolutions per sampled point is a cheap
            // price for not mis-reading every fade-in on the web.
            try {
              parts.push(Math.round(parseFloat(getComputedStyle(el).opacity) * 20));
            } catch (e) {
              parts.push(-2);
            }
          }
          return parts.join(",");
        }

        // pendingTransitions counts animations that are still going to finish.
        //
        // Infinite animations -- a rotating hero, a looping marquee -- are
        // excluded deliberately: waiting for them would mean waiting for the
        // timeout on every site that has one. What matters is the finite
        // transitions that are still mid-flight, because those are the reveals.
        // On a short wait the animation gate cannot pay for itself.
        //
        // getAnimations() walks every animation on the page, and its only job
        // here is to hold off settling during the first forty per cent of the
        // window. On a two-hundred-millisecond wait that window is eighty
        // milliseconds, which on an animation-heavy site is less than the walk
        // itself costs -- so the gate spends more time than it guards.
        var gateWorthIt = timeoutMs >= 500;

        function pendingTransitions() {
          if (!gateWorthIt) return 0;
          if (typeof document.getAnimations !== "function") return 0;
          var n = 0;
          try {
            var anims = document.getAnimations();
            for (var i = 0; i < anims.length && i < 400; i++) {
              var a = anims[i];
              if (a.playState !== "running") continue;
              var t = a.effect && a.effect.getTiming ? a.effect.getTiming() : null;
              if (!t) continue;
              var dur = t.duration;
              if (dur === "auto" || !isFinite(dur)) continue;
              if (t.iterations === Infinity) continue;
              n++;
            }
          } catch (e) {}
          return n;
        }

        var ticks = 0;
        var lastPending = 0;
        var readyAt = -1;

        function tick() {
          var cur = sig();
          var el = now() - t0;

          // A document that has not finished loading is not still, it is
          // incomplete. This gate is what lets one call answer both "has the
          // page arrived" and "has it stopped moving": without it the settle
          // run immediately after a navigation commits finds an empty document
          // perfectly stable and reports a blank page as settled.
          //
          // "interactive" is not enough on its own. A client-rendered page
          // reaches it with an empty body and its scripts still to run, and
          // igloo.inc -- whose entire content is drawn by JavaScript after load
          // -- was declared settled after forty-six milliseconds and swept to
          // completion before it had written anything. But waiting for
          // "complete" unconditionally would hand the page's slowest
          // third-party image a veto over the whole budget, so the requirement
          // relaxes to "interactive" once a decent share of the window has gone.
          var ready = document.readyState === "complete" ||
            (document.readyState === "interactive" && el > readyTimeoutMs * 0.5);
          if (!ready) {
            stable = 0;
            last = null;
            if (el >= readyTimeoutMs) {
              done = true;
              resolve({ settled: false, ms: Math.round(el), pending: lastPending });
              return;
            }
            schedule();
            return;
          }
          // The stillness clock starts when the document does, not when the
          // call did.
          if (readyAt < 0) readyAt = el;
          var still = el - readyAt;

          // document.getAnimations() walks every animation on the page, which
          // on an animation-heavy site is the most expensive thing in this
          // loop. Sampling it every third frame keeps it a useful gate without
          // letting it dominate the frame budget it is supposed to be
          // measuring.
          if (gateWorthIt && ticks % 3 === 0) lastPending = pendingTransitions();
          ticks++;

          // Pending animations block settling only early on.
          //
          // A scroll-scrubbed animation is "running" for as long as it is
          // attached, whatever the scroll position, so on a scrub-driven site
          // this gate never clears. Requiring it for the whole window made
          // every checkpoint wait four to seven seconds for a page that was
          // visually still the entire time -- which on a tall document is the
          // difference between sweeping all of it and sweeping a third.
          //
          // The geometry-and-opacity signature already catches a fade in
          // progress, so after the early window it can carry the decision
          // alone. The gate still does its job where it matters: a transition
          // that starts the moment we scroll gets its chance to finish.
          var gateWindow = timeoutMs * 0.4;
          var blocked = lastPending > 0 && still < gateWindow;

          if (cur === last && !blocked) stable++;
          else {
            stable = 0;
            last = cur;
          }
          if (stable >= frames) {
            done = true;
            resolve({ settled: true, ms: Math.round(el), pending: lastPending });
            return;
          }
          if (still >= timeoutMs) {
            done = true;
            resolve({ settled: false, ms: Math.round(el), pending: lastPending });
            return;
          }
          schedule();
        }

        // The timeout is armed once, up front, rather than being checked at the
        // top of each tick.
        //
        // Checking it inside the loop means the wait can only end on a tick
        // boundary, so it overruns by however long one tick takes -- and a tick
        // measures sixteen hit-tested rectangles and, periodically, every
        // running animation on the page. On an animation-heavy site that is
        // tens of milliseconds; on pear.no a one-second first settle came back
        // after three. When every checkpoint carries a settle wait, an overrun
        // that scales with how busy the page is defeats the entire budget.
        //
        // A timer cannot overrun. The tick check stays as well, because it is
        // what reports the elapsed time accurately.
        var deadline = setTimeout(function () {
          if (done) return;
          done = true;
          resolve({ settled: false, ms: Math.round(now() - t0), pending: lastPending });
        }, readyTimeoutMs + timeoutMs);

        var innerResolve = resolve;
        resolve = function (v) {
          clearTimeout(deadline);
          innerResolve(v);
        };

        // A throw inside tick() happens in a requestAnimationFrame callback,
        // where nothing catches it: the promise simply never resolves and the
        // sweep hangs until its budget expires. That is a far worse failure
        // than a bad settle, and it has happened -- so the loop is wrapped and
        // an error resolves rather than disappearing.
        var guardedTick = tick;
        tick = function () {
          try {
            guardedTick();
          } catch (e) {
            if (!done) {
              done = true;
              resolve({ settled: false, ms: Math.round(now() - t0), error: String(e) });
            }
          }
        };
        schedule();
      });
    },

    // step advances the scroll position and reports what actually happened.
    //
    // The report matters because a page using a scroll-hijacking library
    // (Lenis, Locomotive, GSAP ScrollSmoother) may ignore scrollTo entirely
    // and keep window.scrollY pinned at zero while translating content. When
    // `moved` comes back near zero the sweep escalates to dispatching real
    // wheel events, which those libraries do listen to.
    step: function (delta) {
      var de = document.documentElement;
      var before = window.scrollY || window.pageYOffset || 0;
      var height = Math.max(
        de ? de.scrollHeight : 0,
        document.body ? document.body.scrollHeight : 0
      );
      try {
        window.scrollTo({ top: before + delta, behavior: "instant" });
      } catch (e) {
        window.scrollTo(0, before + delta);
      }
      var after = window.scrollY || window.pageYOffset || 0;
      return JSON.stringify({
        before: before,
        after: after,
        moved: after - before,
        height: height,
        vh: window.innerHeight || 0,
      });
    },

    // Native smooth scrolling turns every step into an animation the sweep
    // then has to wait out. Disabling it is not a change to page content.
    unsmooth: function () {
      try {
        var s = document.createElement("style");
        s.setAttribute("data-sieve", "1");
        s.textContent =
          "html,body{scroll-behavior:auto !important;}" +
          "*{scroll-behavior:auto !important;}";
        (document.head || document.documentElement).appendChild(s);
      } catch (e) {}
      return true;
    },
  };
})();
