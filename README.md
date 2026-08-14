# sieve

**Browser-grade page reading that does not eat your context window.**

---

## Two bets

sieve exists because of two bets about where the web is going.

**The first is that there will be more AI agents on the internet than humans.**
Not eventually — soon, and then overwhelmingly. Every research task, price
check, documentation lookup and comparison that a person used to do by opening
six tabs is becoming a request made by something that never sees a tab at all.

**The second is that the web will fill with AI-generated websites, and that this
will make genuinely elegant, human-made sites more valuable, not less.** When
anyone can generate a competent page in a minute, competence stops being worth
anything. What survives is craft: the WebGL hero that took a studio three weeks,
the scroll-driven narrative, the typography that was drawn rather than chosen.
Sites built for human eyes are about to get *more* ambitious, not less, because
that is the only ground left where a human can still win.

Those two bets point in opposite directions, and the gap between them is the
problem.

---

## Agents do not want what humans want

A person visiting a beautiful site wants the experience. The reveal as they
scroll. The type shattering into individual characters and reassembling. The
canvas that responds to the cursor. That is not decoration — for a brand, it is
most of the point.

An agent wants none of it. It does not love the animation. It does not need the
art direction. It cannot see the layout, and every byte spent describing that
layout is a byte spent not answering the question it was sent to answer.

Worse: the techniques that make a site beautiful are the same ones that make it
illegible to an agent. Content that only exists after a scroll position. DOM
order that does not match reading order because pinning and transforms
rearranged it. Headlines split into one element per character so they can be
animated. Text drawn as geometry inside a 3D scene, present nowhere in the
document. An empty `<body>` that fills itself in with JavaScript.

The site is not broken. It is illegible to a class of visitor that now matters.

**sieve sits between them.** The site keeps its craft for the humans it was
built for. The agent gets the content and nothing else — no markup, no
boilerplate, no duplication, no layout. Less time, far fewer tokens, and an
answer that is actually grounded in the page.

The name is what it does. The content stays. The rest is what you remove.

```
$ sieve distill https://stripe.com

  tier         fetch (score 0.270)
               substantial text served statically; rich structure served
  content      134 blocks in 42 sections
  actions      152 (1 form)
  hidden       14 block(s) quarantined, not in the payload

  tokens       243168 → 5009  (97.9% fewer)
  bytes        631 KB → 18 KB

  audit
    retention  100.0% of observed text reached the graph
    order      high (geometry basis, 93% method agreement)
    headings   low

  timing
    total      4.255s
```

---

## What it costs to read a page

Measured, not asserted. Reproduce any row in about thirty seconds with
`sieve bench <url> --tokens`, which calls no model and needs no API key.

| Page | Served HTML | What an agent receives | |
|---|---|---|---|
| stripe.com | 240,611 | 2,406 | **100×** |
| en.wikipedia.org (Go) | 249,121 | 2,680 | **93×** |
| hatom.com | 92,222 | 1,187 | 78× |
| organimo.com | 36,782 | 1,311 | 28× |
| pear.no | 18,725 | 2,015 | 9× |
| news.ycombinator.com | 11,979 | 3,014 | 4× |

The spread is the interesting part, and it is why sieve does not claim a single
number. A `distill` call returns a **manifest** — what the page contains, its
sections and what each would cost — and the manifest is close to flat in the
size of the document, because it names sections rather than carrying them. So
the margin tracks page size. A link-dense front page barely benefits. A large
content page benefits enormously.

**On a page of a few hundred tokens the manifest costs more than the page, and
sieve says so and tells you to just fetch it.** A tool that claims to help
everywhere is not measuring.

The MCP tool definitions cost **~1,737 tokens**, paid once per session rather
than per page. That number is a test, not an aspiration: it fails the build if
it grows.

---

## Read the outcome first

The worst failure in this category is an agent that cannot tell reading failed.

A bot challenge, a login wall and an unhydrated single-page shell all arrive as
a valid HTTP 200 carrying valid HTML and no content. Nothing in the response
says anything went wrong. An agent handed one of those either reports the site
as empty or invents something to fill the silence.

So every artifact carries a machine-readable verdict, before the content:

```json
"outcome": {
  "status": "blocked",
  "evidence": ["the server answered HTTP 403"],
  "http_status": 403,
  "body_excerpt": "Access denied by policy WEB-31."
}
```

| status | meaning |
|---|---|
| `ok` | content was extracted normally |
| `blocked` | the site refused: 4xx/5xx, robots.txt, or a rate limit |
| `challenge` | a bot-protection or entry screen answered instead |
| `auth_required` | a login wall stands in front of the content |
| `spa_shell` | an unhydrated shell that never filled in |
| `empty_after_render` | the page rendered and genuinely has no text |
| `partial` | content was extracted, but a tier fell back |

`evidence` is never empty for anything but `ok`: a verdict you cannot check is
a verdict you have to trust. Across a 334-site corpus, 76.6% came back `ok`,
8.3% `blocked`, 7.1% `challenge` — and every one of the rest said so rather
than returning a plausible-looking empty page.

---

## How it works

### It escalates, so it is not overkill for a blog

Most extraction tools pick a side: parse the markup and fail on the hard tier,
or always render and overpay on the easy one. sieve does neither.

| Tier | What it does | Typical time |
|---|---|---|
| `fetch` | Plain HTTP GET, static extraction, boilerplate removal | Under a second |
| `render` | Headless load, wait for settle, one full capture | A few seconds |
| `sweep` | Checkpoint sweep with deduplication and geometric ordering | Tens of seconds |
| `recover` | Canvas and 3D-scene recovery on top of the sweep | Plus vision, if enabled |

The decision is scored from the served bytes — text volume, text-to-markup
ratio, structural richness, script weight, hydration blobs, canvas elements —
and **every artifact records which tier answered and why**. A domain that has
ever escalated stays escalated, so a page near the line is not judged
differently on different days.

Across 334 sites: median **4 seconds**, ninetieth percentile **19 seconds**.
Most of the web is cheap, and sieve only spends where it must.

### It reads what the document does not contain

- **Text drawn in a 3D scene.** igloo.inc serves an empty `<body>` and draws
  every word as glyph geometry inside three.js. sieve installs the devtools
  hook before any page script runs and walks the live scene graph.
- **Words split for animation.** Sites split headlines into one element per
  character. Reassembly uses rendered line boxes, because document order alone
  misspells the page.
- **Entry screens.** Click-to-enter interstitials are pressed through, with
  positive evidence required first so a normal page is never clicked at random.
- **Collapsed panels.** Tab and accordion bodies are usually already in the DOM.
  Discarding them is the largest single source of missing content in a
  scroll-only extractor.

### It audits itself

An artifact that reports its own uncertainty is a categorically different
object from one that does not.

- **Graph retention** — what share of the observed text survived into the graph.
  Narrowly named on purpose: it measures the graph stage, not the sweep.
- **Order agreement** — geometry and first-appearance are two independent
  orderings from entirely different evidence. Where they disagree is where
  reading order goes wrong.
- **Heading separation** — a clean gap between type sizes means the inference
  had something real to work with; an overlapping continuum means it guessed.

Confidence ships as `high` / `medium` / `low`, not as a decimal. A wrong number
is worse than no number, because people trust numbers more than prose.

### It cannot invent content

**No model touches the extraction path.** Token reduction comes from removing
markup, boilerplate and duplication — never from a model paraphrasing. That is
a claim summarising pipelines structurally cannot make.

Vision is off by default, and with it off the artifact structurally cannot
contain invented text. With it on, anything recovered from pixels is
cross-checked against text the site actually shipped: found → `confirmed`, not
found → `speculative` and excluded from every default payload. The site's own
data payload is used **only to confirm, never as a source** — hydration blobs
routinely carry draft copy, other locales and unpublished records.

### It keeps hidden content without trusting it

Hidden text goes into a **latent tier**: its own key, its own retrieval tool, a
trust marker in every format, excluded from the headline token count, and
labelled with the control that would reveal it. An artifact can say *"there is a
section behind a tab labelled Pricing"* instead of silently omitting it.

No default rendering includes it. That is a security property, not a formatting
choice — hidden text is exactly where a page would place instructions aimed at
an automated reader.

### It is reproducible

Every artifact carries a trace: viewport, device scale, locale, timezone,
Chromium build, the full flag set, and a hash of the extraction script. Locale
and timezone are pinned rather than inherited. A/B testing platforms are
blocked, because a distiller that lets a split test decide which headline it
captured has no claim to determinism.

`--snapshot` records the capture so the graph stage can be replayed offline,
months later, on another machine, byte for byte:

```sh
sieve replay bug-report.sieve
```

### It always ends

A tool an agent drives in a loop cannot hang. Every stage has a deadline, and
past its budget plus a grace a watchdog names the stage it reached, kills the
browser tree, and exits `5` — distinct from a failed run, a usage error, a
refusal by policy and an unreachable host, so a caller can tell "could not read
the page" from "sieve broke".

---

## Install

```sh
go install github.com/qcoderx/sieve/cmd/sieve@latest
```

Chromium is the one non-trivial dependency, and only tiers above `fetch` need
it. sieve finds Chrome, Chromium or Edge automatically; point it elsewhere with
`--chrome` or `SIEVE_CHROME`.

```sh
sieve doctor      # check the environment before you need it
```

---

## Use

### As an MCP server

This is the primary interface. The CLI is for humans and CI.

```sh
claude mcp add sieve -- sieve mcp
```

For Codex, in `~/.codex/config.toml`:

```toml
[mcp_servers.sieve]
command = "sieve"
args = ["mcp"]
```

**The design constraint:** an MCP tool result lands directly in the agent's
context window. If `distill` returned the whole artifact, sieve would have moved
the token cost rather than removed it. So every tool returns the smallest useful
payload.

| Tool | Returns |
|---|---|
| `distill` | A manifest: outcome, title, summary, sections with sizes, counts. Never the body |
| `status` | Progress, for jobs still rendering |
| `get_content` | One section or specific blocks, capped, with a cursor |
| `search_content` | Block ids and short snippets |
| `list_actions` | Links, buttons, and form field schemas |
| `get_hidden_content` | The latent tier, with a stronger warning attached |
| `describe_media` | What is known about one image |

Output defaults to JSON rather than Markdown. Markdown is friendlier on disk,
but tool output lands unmediated in a context window and Markdown has no
structural marking a model reliably treats as data.

### Command line

```sh
sieve distill https://example.com --out ./artifacts
sieve distill https://example.com --min-tier sweep      # force the browser
sieve distill https://example.com --max-tier fetch      # forbid it
sieve distill https://example.com --snapshot ./traces   # record for replay
sieve distill https://example.com --timeout 90s         # slow, scroll-jacked pages

sieve doctor https://example.com    # why did it choose that tier?
sieve replay ./traces/example.com.sieve
sieve serve ./artifacts
```

### What comes out

An artifact directory: `manifest.json` (what the page contains and whether it
could be read), `content.json` (the full graph), `index.md` (what an agent
reads) and `index.html`.

Section ids are derived from heading text, so they survive the page changing and
can be held across calls. Block ids are positional and cannot.

**[docs/ARTIFACT.md](docs/ARTIFACT.md) is the format contract.** Schema 1.x adds
fields and never removes or repurposes them, so it is safe to build against.

### Benchmark

```sh
sieve bench https://example.com --tokens                     # no API key needed
sieve bench ./artifacts/example.com --questions q.yaml        # accuracy
sieve bench ./artifacts/example.com --questions q.yaml --coverage-only
sieve bench https://example.com --stability
```

The same model, prompt and budget answer each question twice — once from the raw
page, once from the artifact — and both are graded against hand-written ground
truth. Token counts come from the API's own accounting, so they are measurements
rather than estimates. `--regrade` re-grades a sample to measure how far the
grader agrees with itself, which is the error bar on every accuracy figure.

Any provider works, not just Anthropic:

```sh
LLM_BASE_URL=https://api.groq.com/openai/v1 LLM_API_KEY=... sieve bench ...
```

The harness refuses to report a comparison it could not measure. A condition
that answered nothing, or a run where the grader never succeeded, is reported as
unmeasured rather than as a score of zero — because a control that collapses
would otherwise hand the artifact a large apparent win.

---

## What it does not do

- **Not a general-purpose crawler.** Single site, bounded depth, one domain per
  job.
- **Not a visual clone.** Layout, animation and art direction are explicitly
  discarded. That is the point, not a shortcoming.
- **Not a bypass.** sieve obeys `robots.txt` and `crawl-delay`, sends an
  identifying user agent with a contact URL, caps concurrency per domain, and
  enforces a minimum interval that cannot be configured to zero. It does not
  fingerprint-spoof, solve challenges, or authenticate. A site that blocks it
  stays unread, and the artifact says so.
- **Not immune to prompt injection.** It closes the hidden-element channel
  completely, which markup-based extractors do not. That is a precise claim and
  not a general one. See [THREAT_MODEL.md](THREAT_MODEL.md).

---

## Honest limits

- **Text painted as a texture cannot be read.** sieve reads glyph *geometry*
  from a 3D scene. A site that renders its words into a texture instead defeats
  it, and `hatom.com` is committed as a deliberately failing test with the
  ruled-out causes written down.
- **One page at a time.** No multi-page or whole-site mode yet.
- **It is sensitive to machine load.** Under heavy contention a large page can
  time out and produce an empty artifact. It is labelled `empty_after_render`
  rather than reported as success, but the content is missing all the same.
- **Sites that block and do not wish to be read stay unread.** Correct
  behaviour, not a defect.
- **The long tail of browser quirks does not end.** Secondary tabs never
  compositing and starving `requestAnimationFrame`; `--in-process-gpu` with
  SwiftShader killing frame production outright. Neither appears in any
  documentation. `sieve doctor` probes for both.
- **Confidence scores are uncalibrated** until the benchmark corpus is large
  enough to tune them, which is why they ship as coarse buckets. Graph retention
  is not coverage, and is named narrowly so it cannot be read as one.

---

## Repository layout

```
cmd/sieve/            entrypoint, subcommands
internal/capture/     the in-page extraction script and its wire format
internal/render/      Chromium session, scroll sweep, frame-production probe
internal/static/      tier-0 extraction without a browser
internal/escalate/    the tier decision, its thresholds and its memory
internal/graph/       reassembly, classification, ordering, the content graph
internal/canvas/      the five canvas recovery attacks
internal/corroborate/ the confirm-only index that bounds hallucination
internal/textnorm/    versioned normalisation at the graph boundary
internal/emit/        markdown, html, json renderers
internal/mcpserver/   tool handlers and server instructions
internal/bench/       harness, graders, reports
internal/safety/      robots, rate limits, SSRF guards
internal/snapshot/    record and replay
docs/ARTIFACT.md      the format contract
testdata/             fixture pages, golden artifacts, question sets
```

---

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Two things worth knowing up front:

- **Library detectors live in a data file.** `internal/render/fingerprints.json`
  takes a pull request with a fixture; adding one needs no Go and no release.
- **Golden files catch what tests cannot.** Extraction regressions are silent.
  `go test ./internal/graph -run TestGolden -update` accepts a change — read the
  diff before you do.

## Licence

Apache 2.0. It carries an explicit patent grant, which matters for a tool other
companies may embed in their own agent stacks.
