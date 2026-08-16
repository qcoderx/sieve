# Changelog

## 0.2.0

The artifact format is unchanged: schema is still 1.0 and everything in
[docs/ARTIFACT.md](docs/ARTIFACT.md) still holds. What changed is how much of a
page reaches it, and how honestly the artifact reports what it left out.

### Fixed

- **A Distiller stopped rendering after its first page.** The browser was
  launched on the context of whichever page happened to need it first, so that
  page's cancellation took Chromium down while the handle stayed non-nil. Every
  later page opened a tab on a dead context and fell back to the served HTML,
  which is a supported outcome and looks like a decision rather than a failure.
  In practice this was `sieve site` reading the first page of a documentation
  site and none of the rest, and the MCP server doing the same after its first
  request. One page in isolation always worked, which is why nothing caught it.
- **Reference documentation lost every term it defines.** A short linked run is
  usually a menu item, and on an API reference it is the name of the thing being
  documented. All five libcurl options on curl's cookie page were removed and
  their definitions kept, leaving fluent prose about unnamed options. `<code>`
  inside a link now outranks the guess, as do `mailto:` and `tel:` links, which
  were taking contact addresses with them.
- **Text folded behind a control could not get a page a browser.** Nothing in
  the ladder could see that a page was holding content shut, so the disclosure
  prober only ran on pages that had escalated for some other reason. It now
  measures the prose behind closed disclosures, ignoring navigation, decorative
  panels and anything the prober would refuse to press — 9.8% of a
  two-hundred-site corpus newly escalates, against 24.7% for a naive count.
- **Words in a 3D scene were read before the scene existed.** The sweep's settle
  loop watches DOM text, so a page whose content is glyph geometry looks
  perfectly still from the moment it loads: the loop declares the page settled
  and the scene is walked before three.js has built a single text object. The
  scene was read once, so losing that race was permanent. `igloo.inc` — the case
  this project exists for — returned an empty artifact on two runs in five. A
  scene that is present and empty is now given time to fill, bounded by elapsed
  time rather than a number of attempts. Six runs of six since, 40 of 40
  ground-truth facts each time. A page with no scene, or one whose scene is
  already built, still returns on the first read and pays nothing.
- Static extraction no longer treats an already-open `<details>` as closed.

### Added

- **An offline benchmark tier.** Four fixtures ship here with their ground
  truth, graded with no network in CI, with floors set just under the measured
  score. Every other measurement in this project points at a live site and is
  therefore a measurement of the internet on a Tuesday.
- **Thirty question sets**, ten each in three bands: hard, medium, and the easy
  band where sieve must simply not lose. Ground truth for every set is read from
  the page's own source rather than from a sieve artifact.
- **A Claude Code plugin**, with the skill, the WebFetch hook and the MCP server
  in one install, plus prebuilt binaries and an npm package so trying it does
  not require a Go toolchain.
- `sieve site` for reading across the pages of one documentation site, and
  `sieve hook` for reading the page a WebFetch could not.

### Known issues

- **Content that fades in can be missed, about one run in six.** A page that
  reveals sections on scroll with a short opacity transition can be
  photographed mid-fade, below the visible-opacity threshold, and the section
  is then dropped. The `immersive` fixture reproduces it at 30 of 45 facts
  instead of 39. The offline corpus retries a row once and says so in its
  output when it does, which keeps the build meaningful without pretending the
  race is fixed.
- **Pages whose text is entirely non-DOM are slow.** On `igloo.inc` the sweep
  spends up to twelve seconds in settle waits being patient about text that
  cannot arrive, because every word is geometry. The guard that would cut those
  waits is conditional on having seen text, which never happens there. Reading
  the page is correct and takes 15 to 40 seconds.

### Changed

- **The artifact now reports every exclusion it makes.** Three of the prune's
  four rules removed text and recorded nothing, so the artifact's account of
  itself said nothing had happened — on one page, 1,836 characters of silence.
  All four now give a reason written for somebody wondering where a fact went.
- The MCP server applies the timeouts it accepts. It registered `-timeout` and
  `-load-timeout` and then discarded them, which is why pages that worked on the
  command line failed over MCP.
- The manifest no longer carries five keys that always read zero.
- A hung run is killed and reported with its own exit code, and orphaned
  browsers are reaped rather than left running.

## 0.1.0

The first release with a frozen artifact format. Everything in
[docs/ARTIFACT.md](docs/ARTIFACT.md) is a contract from here: within schema 1.x,
fields are added and never removed or repurposed.

Validated against a 334-site corpus: 311 clean exits, median 4 seconds,
ninetieth percentile 19.

### What sieve does

Reads a web page the way a browser does and returns text an agent can use. It
escalates — a plain fetch answers most pages in under a second, and the browser,
the scrolling sweep and canvas recovery are used only when a cheaper read comes
back thin. Every artifact records which rung answered and why.

It reads pages that serve an empty body and build themselves in JavaScript, text
drawn as geometry inside a 3D scene, and text that sites split one letter per
element for animation. It presses through entry screens. It notices collapsed
panels and says what it could not open.

No model touches the extraction path. Token reduction comes from removing
markup, boilerplate and duplication — never from paraphrasing.

### Format

- **An outcome on every artifact.** `ok`, `blocked`, `challenge`,
  `auth_required`, `spa_shell`, `empty_after_render` or `partial`, each with the
  evidence that produced it, the HTTP status, and on an error the beginning of
  the response body. A bot challenge, a login wall and an unhydrated shell all
  arrive as a valid 200 carrying valid HTML and no content; without this field
  an agent reports the site as empty or invents something to fill the gap.
- **Section ids derived from heading text**, so they survive the page changing.
  They are handed to agents over MCP and used to fetch content by name; when they
  were positional, two distillations of one page minutes apart produced the same
  twenty-one sections with seventeen ids pointing at different content.
- Hidden text is quarantined in a latent tier that no default rendering
  includes, reachable only by a caller that asks for it by name.

### Measurements

Reproducible with `sieve bench <url> --tokens`, which needs no API key:

| page | served HTML | manifest | |
|---|---|---|---|
| stripe.com | 240,611 | 2,406 | 100× |
| en.wikipedia.org (Go) | 249,121 | 2,680 | 93× |
| organimo.com | 36,782 | 1,311 | 28× |
| news.ycombinator.com | 11,979 | 3,014 | 4× |

The margin tracks page size, because the manifest names sections and their costs
rather than carrying them. A link-dense front page barely benefits; a large
content page benefits enormously. On a page of a few hundred tokens the manifest
costs more than the page, and sieve says so.

MCP tool definitions cost ~1,737 tokens per session, paid once rather than per
page.

### Reliability

- **Bounded browser shutdown.** Cancelling the allocator waits for Chromium to
  exit with no bound, and a browser that has stopped answering never provides
  one. One corpus run reached that line with a complete artifact already on disk
  and did not return to its caller for another fourteen minutes. Shutdown now
  has an eight-second deadline, after which the process tree is killed.
- **A watchdog.** Past its budget plus a grace, sieve prints the stage it last
  reached, kills the browser tree, and exits 5 — distinct from a failed run (1),
  a usage error (2), a refusal by policy (3) and an unreachable host (4).
- Snapshots record the tier, so a replayed artifact reproduces the original
  byte for byte rather than losing the field a reader consults first.

### Known limits

- Text painted as a texture rather than drawn as geometry cannot be read.
  `hatom.com` is committed as a failing question set with the ruled-out causes
  written down.
- One page at a time. No multi-page or whole-site mode.
- Under heavy machine load, large pages can time out and produce an empty
  artifact. They are labelled `empty_after_render` or `spa_shell` rather than
  reported as success, but the content is missing all the same.
