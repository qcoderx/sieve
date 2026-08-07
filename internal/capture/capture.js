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
  var LANDMARK_ROLE = {
    navigation: "nav", banner: "header", contentinfo: "footer",
    main: "main", complementary: "aside", form: "form", search: "form",
    dialog: "dialog", alertdialog: "dialog", menu: "nav", menubar: "nav",
  };

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

  // Below this the text is not readable by a human at any size. It is a much
  // lower bar than the WCAG 4.5 accessibility threshold on purpose: plenty of
  // legitimate design is low-contrast, and only text that is effectively
  // invisible should be treated as a hiding technique.
  var INVISIBLE_CONTRAST = 1.2;

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
   * @param vis       false when an ancestor set visibility:hidden
   * @param fixed     true when el or an ancestor is position:fixed/sticky
   * @param bg        nearest opaque ancestor background colour
   * @param ctx       shared frame context {win, doc, sx, sy, vw, vh, col, disc}
   */
  function walk(el, path, blockPath, opacity, landmark, href, depth, off, vis, fixed, bg, ctx) {
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

    if (LANDMARK_TAG[tag]) landmark = LANDMARK_TAG[tag];
    var roleAttr = el.getAttribute ? el.getAttribute("role") : null;
    var role = roleAttr ? roleAttr.trim().toLowerCase() : "";
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
      descendChildren(sr, path + "/#shadow", myBlock, op, landmark, href, depth + 1, off, visible, fixed, bg, ctx);
    }

    if (tag === "IFRAME" || tag === "FRAME") {
      descendFrame(el, path, myBlock, op, landmark, href, depth, off, visible, fixed, bg, ctx);
      return;
    }

    descendChildren(el, path, myBlock, op, landmark, href, depth + 1, off, visible, fixed, bg, ctx);
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
  function emitRun(el, path, blockPath, text, textNode, env, ctx, pad) {
    if (!text || isIconGlyph(text)) return;

    var rect, lineTop;
    if (textNode) {
      var rng = ctx.doc.createRange();
      try {
        rng.selectNodeContents(textNode);
      } catch (e) {
        return;
      }
      rect = rng.getBoundingClientRect();
      lineTop = rect.top;
      var rrs = rng.getClientRects();
      if (rrs && rrs.length) lineTop = rrs[0].top;
      rng.detach && rng.detach();
    } else {
      rect = el.getBoundingClientRect();
      lineTop = rect.top;
      try {
        var rs = el.getClientRects();
        if (rs && rs.length) lineTop = rs[0].top;
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
      bb: [round2(docX), round2(docY), round2(rect.width), round2(rect.height)],
      lt: Math.round(env.fixed ? lineTop : lineTop + env.off.y + ctx.sy),
      d: env.depth,
    });
  }

  function descendChildren(parent, path, blockPath, op, landmark, href, depth, off, vis, fixed, bg, ctx) {
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
      walk(c, path + "/" + seg(lower, idx), blockPath, op, landmark, href, depth, off, vis, fixed, bg, ctx);
    }
  }

  function descendFrame(el, path, blockPath, op, landmark, href, depth, off, vis, fixed, bg, ctx) {
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
    };
    try {
      inner.sx = win.scrollX || 0;
      inner.sy = win.scrollY || 0;
    } catch (e) {}
    var innerOff = {
      x: off.x + ctx.sx + r.left - inner.sx,
      y: off.y + ctx.sy + r.top - inner.sy,
    };
    walk(doc.documentElement, path + "/#doc", blockPath, op, landmark, href, depth + 1, innerOff, vis, fixed, bg, inner);
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

  function introspectScene() {
    var names = [];
    var texts = [];
    var seen = new Set();
    var count = 0;

    function visit(obj, depth) {
      if (!obj || depth > 24 || count > 4000) return;
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
        // Text geometry carries the actual words when a headline is rendered
        // as 3D type.
        if (obj.geometry && obj.geometry.parameters && typeof obj.geometry.parameters.text === "string") {
          texts.push(obj.geometry.parameters.text);
        }
        var ch = obj.children;
        if (ch && ch.length) {
          for (var i = 0; i < ch.length && i < 2000; i++) visit(ch[i], depth + 1);
        }
      } catch (e) {}
    }

    var roots = [];
    try {
      // The devtools hook is the reliable way in when a page uses three.js.
      var hook = window.__THREE_DEVTOOLS__;
      if (hook && hook.scenes) {
        for (var i = 0; i < hook.scenes.length; i++) roots.push(hook.scenes[i]);
      }
    } catch (e) {}
    if (!roots.length) {
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
    for (var r = 0; r < roots.length && r < 8; r++) visit(roots[r], 0);
    if (!names.length && !texts.length) return undefined;
    return { n: names.slice(0, 400), t: texts.slice(0, 200) };
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

  function capture(checkpoint, budget, latentBudget) {
    var col = new Collector(budget || 40000, latentBudget || 12000);
    var de = document.documentElement;
    var vw = window.innerWidth || (de && de.clientWidth) || 0;
    var vh = window.innerHeight || (de && de.clientHeight) || 0;
    var disc = buildDisclosureIndex(document);
    var ctx = {
      win: window,
      doc: document,
      sx: window.scrollX || window.pageXOffset || 0,
      sy: window.scrollY || window.pageYOffset || 0,
      vw: vw,
      vh: vh,
      col: col,
      disc: disc,
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
      walk(de, "html", "html", 1, "", "", 0, { x: 0, y: 0 }, true, false, rootBG, ctx);
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

  window.__sieve = {
    capture: function (n, budget, latentBudget) {
      return JSON.stringify(capture(n, budget, latentBudget));
    },

    // corpus, scene and libs are called once each, not per checkpoint.
    corpus: function () {
      return collectCorpus(document);
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
    settle: function (frames, timeoutMs) {
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
        var pts = [];
        for (var i = 1; i <= 4; i++) {
          for (var j = 1; j <= 4; j++) {
            pts.push([(vw * i) / 5, (vh * j) / 5]);
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
        function pendingTransitions() {
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

        function tick() {
          var cur = sig();
          var pending = pendingTransitions();
          if (cur === last && pending === 0) stable++;
          else {
            stable = 0;
            last = cur;
          }
          var el = now() - t0;
          if (stable >= frames) {
            done = true;
            resolve({ settled: true, ms: Math.round(el), pending: pending });
            return;
          }
          if (el >= timeoutMs) {
            done = true;
            resolve({ settled: false, ms: Math.round(el), pending: pending });
            return;
          }
          schedule();
        }
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
