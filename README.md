# sieve

**Make a heavy website readable by an agent.**

sieve takes the URL of a site built for human eyes — WebGL heroes, scroll-driven
reveals, pinned sections, headlines shattered into one element per character —
and republishes it as a structured artifact that costs a fraction of the tokens
and time to parse.

The name is what it does. The content stays. The rest is what you remove.

```
$ sieve distill https://linear.app

Linear – The system for product development

  tier         fetch (score 0.120)
  content      217 blocks in 5 sections
  hidden       2 block(s) quarantined, not in the payload

  tokens       472961 → 1732  (99.6% fewer)
  bytes        1.2 MB → 12 KB

  audit
    retention  100.0% of observed text reached the graph
    order      high (geometry basis, 93% method agreement)
    headings   low
```

---

## The problem

Modern brand and portfolio sites are built for human eyes. The initial HTML
response contains almost no content, content that does exist appears only after
specific scroll positions, DOM order does not match reading order because
pinning and transforms rearrange the layout, and some content exists only as
pixels inside a canvas.

An agent sent to such a page either fails to answer questions about it, or burns
a large token budget on markup and inline scripts to extract a small amount of
meaning. The site is not broken. It is illegible to a class of visitor that now
matters.

---

## What makes sieve different

### It escalates, so it is not overkill for a blog

Most extraction tools pick a side: parse the markup and fail on the hard tier, or
always render and overpay on the easy one. sieve does neither.

| Tier | What it does | Typical time |
|---|---|---|
| `fetch` | Plain HTTP GET, static extraction, boilerplate removal | Under a second |
| `render` | Headless load, wait for settle, one full capture | A few seconds |
| `sweep` | Full checkpoint sweep with deduplication and geometric ordering | Tens of seconds |
| `recover` | Canvas recovery on top of the sweep | Plus vision, if enabled |

The decision is scored from the served bytes — text volume, text-to-markup
ratio, structural richness, script weight, hydration blobs, canvas elements —
and **every artifact records which tier answered and why**. Thresholds are pinned
constants, and a domain that has ever escalated stays escalated, so a page near
the line does not get judged differently on different days.

### It audits itself

An artifact that reports its own uncertainty is a categorically different object
from one that does not.

- **Graph retention** — what share of the text the capture observed survived
  into the emitted graph. Narrowly named on purpose: it measures the graph
  stage, not the sweep. True coverage is a benchmark number, measured against
  hand-written ground truth.
- **Order agreement** — geometry and first-appearance are two independent
  orderings computed from entirely different evidence. Where they disagree is
  where reading order goes wrong, and the comparison is nearly free.
- **Heading separation** — a clean gap between type sizes means the heading
  inference had something real to work with; an overlapping continuum means it
  guessed.

Confidence is reported as `high` / `medium` / `low`, not as a decimal. A wrong
number is worse than no number, because people trust numbers more than prose.

### It keeps hidden content without trusting it

Tab panels and accordion bodies are overwhelmingly already in the DOM as
`display:none`. Discarding them is the largest single source of missing content
in a scroll-only extractor — and keeping them naively means ingesting exactly
the material a visibility filter exists to exclude.

So hidden text goes into a **latent tier**: its own top-level key, its own
retrieval tool, a trust marker in every format, excluded from the headline token
count, and labelled with the control that would reveal it. An artifact can say
*"there is a section behind a tab labelled Pricing"* instead of silently
omitting it.

### It cannot invent content

Vision is **off by default**, and with it off the artifact structurally cannot
contain invented text. With it on, anything recovered from pixels is
cross-checked against the text the site actually shipped: found → `confirmed`,
not found → `speculative` and excluded from every default payload.

The payload is used **only to confirm, never as a source**. Hydration blobs
routinely carry draft copy, other locales and unpublished records, and emitting
any of it would stop being extraction and start being republishing someone's
back office.

### It is reproducible

Every artifact carries a trace: viewport, device scale, locale, timezone,
Chromium build, the full flag set, and a hash of the extraction script. Locale
and timezone are pinned rather than inherited. A/B testing platforms are blocked,
because a distiller that lets a split test decide which headline it captured has
no claim to determinism.

`sieve distill --snapshot` records the capture so the whole graph stage can be
replayed offline, months later, on a different machine:

```
$ sieve replay bug-report.sieve
```

---

## Install

```sh
go install github.com/qcoderx/sieve/cmd/sieve@latest
```

Chromium is the one non-trivial dependency, and only tiers above `fetch` need
it. sieve finds Chrome, Chromium or Edge automatically; point it somewhere else
with `--chrome` or `SIEVE_CHROME`.

```sh
sieve doctor      # check the environment before you need it
```

---

## Use

### Command line

```sh
sieve distill https://example.com --out ./artifacts
sieve distill https://example.com --min-tier sweep      # force the browser
sieve distill https://example.com --max-tier fetch      # forbid it
sieve distill https://example.com --snapshot ./traces   # record for replay

sieve doctor https://example.com   # why did it choose that tier?
sieve replay ./traces/example.com.sieve
sieve serve ./artifacts
```

### As an MCP server

This is the primary interface. The CLI is for humans and CI.

```sh
claude mcp add sieve -- sieve mcp
```

Or a hosted instance:

```sh
claude mcp add --transport http sieve https://sieve.example.com/mcp
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
payload:

| Tool | Returns |
|---|---|
| `distill` | A manifest: title, summary, sections with sizes, counts. Never the body |
| `status` | Progress, for jobs still rendering |
| `get_content` | One section or specific blocks, capped, with a cursor |
| `search_content` | Block ids and short snippets |
| `list_actions` | Links, buttons, and form field schemas |
| `get_hidden_content` | The latent tier, with a stronger warning attached |
| `describe_media` | What is known about one image |

Output defaults to JSON rather than Markdown. Markdown is the friendlier
artifact on disk, but tool output lands unmediated in a context window and
Markdown has no structural marking a model reliably treats as data.

### Benchmark

```sh
sieve bench ./artifacts/example.com --questions questions.yaml
sieve bench https://example.com --stability
```

The same model, the same prompt and the same budget answer each question twice —
once from the raw page, once from the artifact — and both are graded against
hand-written ground truth. Token counts come from the API's own accounting, so
they are measurements rather than estimates.

Success criteria for v1: **90%** fewer tokens, **20 points** more accurate,
coverage **≥ 0.90**, fidelity **≥ 0.98**. Fidelity gates the release, because a
distiller that invents content is worse than no distiller.

---

## What it does not do

- **Not a general-purpose crawler.** Single site, bounded depth, one domain per
  job.
- **Not a visual clone.** Layout, animation and art direction are explicitly
  discarded.
- **Not a bypass.** sieve obeys `robots.txt` and `crawl-delay`, sends an
  identifying user agent with a contact URL, caps concurrency per domain, and
  enforces a minimum interval that cannot be configured to zero. It does not
  fingerprint-spoof, does not solve challenges, and does not authenticate. A
  site that blocks it stays unread, and the artifact says so.
- **Not immune to prompt injection.** It closes the hidden-element channel
  completely, which markup-based extractors do not. That is a precise claim and
  not a general one. See [THREAT_MODEL.md](THREAT_MODEL.md).

---

## Honest limits

Three things remain genuinely real, and are worth stating rather than hiding:

1. **Sites that block and do not wish to be read stay unread.** That is correct
   behaviour, not a defect.
2. **The long tail of browser and animation-library quirks does not end.**
   Secondary tabs never compositing and therefore starving
   `requestAnimationFrame`; `--in-process-gpu` with SwiftShader killing frame
   production outright — neither appears in any documentation. `sieve doctor`
   probes for both.
3. **Untrusted-content handling is a permanent posture, not a solved problem.**
   New channels will appear.

Two more, on the numbers:

- Confidence scores are uncalibrated until the benchmark corpus is large enough
  to tune them, which is why they ship as coarse buckets.
- Graph retention is not coverage, and is named narrowly so it cannot be read as
  one.

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
testdata/             fixture pages, golden artifacts
```

---

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). The two things worth knowing up front:

- **Library detectors live in a data file.** `internal/render/fingerprints.json`
  takes a pull request with a fixture; adding one needs no Go and no release.
- **Golden files catch what tests cannot.** Extraction regressions are silent.
  `go test ./internal/graph -run TestGolden -update` accepts a change — read the
  diff before you do.

## Licence

Apache 2.0. It carries an explicit patent grant, which matters for a tool other
companies may embed in their own agent stacks.
