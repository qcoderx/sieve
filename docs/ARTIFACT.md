# The sieve artifact format

**Schema version 1.0.** This document is the contract. Anything described here
as stable will not change without the schema version changing with it.

An artifact is what sieve produces from one URL. It is a directory of four
files, all derived from the same content graph, so they cannot disagree with one
another — if the Markdown says four sections and the JSON says five, that is a
bug in a renderer rather than a difference of opinion.

```
artifacts/example.com/
  manifest.json    what the page contains, and whether it could be read
  content.json     the complete graph: every block, action, link and gap
  index.md         the rendering an agent reads
  index.html       the same, for a person
```

---

## Read `outcome` first

Every artifact carries an outcome. It is the first field in the manifest and it
appears in the Markdown front matter, because it decides whether the rest of the
artifact describes the page you asked for or something standing in front of it.

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
| `blocked` | the site refused: HTTP 4xx/5xx, robots.txt, or a rate limit |
| `challenge` | a bot-protection or entry screen answered instead of the page |
| `auth_required` | a login wall stands in front of the content |
| `spa_shell` | the served document is an unhydrated shell that never filled in |
| `empty_after_render` | the page rendered and genuinely carries no text |
| `partial` | content was extracted, but a tier was tried and fell back |

`evidence` is never empty for a status other than `ok`: a verdict you cannot
check is a verdict you have to trust. `http_status` is recorded whatever it was.
`body_excerpt` is present only on an error, capped at 400 bytes, and is where a
proxy or policy filter says who blocked the request and why.

The status set is closed and its members are stable. New statuses are a schema
change.

**Why this exists.** A bot challenge, a login wall and an unhydrated shell all
arrive as a valid HTTP 200 carrying valid HTML and no content. Nothing in the
response says the read failed, so an agent handed one either reports the site as
empty or invents something to fill the gap. That is the worst failure in this
category, and this field is the fix.

---

## Identifiers and what they promise

| id | shape | stability |
|---|---|---|
| section | `s_` + 10 hex, or `s_intro` | **stable across runs** while the heading text is unchanged |
| block | `b_000`, sequential | positional; stable only within one artifact |
| action | `a_000` | positional |
| media | `m_000` | positional |

Section ids are derived from the heading text, not from position. A section keeps
its id when content is added above it, when a sibling is removed, or when a sweep
reaches further than it did last time. Two sections sharing a heading are
distinguished by their order among the duplicates. The lead section before any
heading is `s_intro`.

This is the id you may hold across calls. Block ids are not: they renumber
whenever the block list changes, and a block id from a previous run may point
somewhere else.

**Why this matters.** These ids are handed to agents over MCP and used to fetch
content by name. When they were positional, two distillations of the same page
minutes apart produced the same twenty-one sections with seventeen ids pointing
at different content.

---

## `manifest.json`

The manifest is what a `distill` call returns. It describes the page and names
its parts; it never carries the page body. It is roughly flat in the size of the
document, which is where the token margin comes from.

```
schema_version   "1.0"
outcome          object, above
url              what was requested
final_url        where it ended up, if redirected
title            the page's title
summary          one or two sentences, taken from the page
lang             BCP 47, when the page declares one
content_hash     sha256 over the normalised semantic graph
distilled_at     RFC 3339, UTC

sections[]       id, title, level, blocks, chars, est_tokens,
                 first_block, last_block
counts           blocks, actions, forms, links, media, latent,
                 est_total_tokens
stats            original_bytes, artifact_bytes, original_tokens,
                 artifact_tokens, latent_tokens, checkpoints, raw_nodes,
                 content_blocks, chrome_blocks, latent_blocks, dropped_nodes
audit            graph_retention, order_confidence, heading_confidence,
                 reached_bottom, dropped[], notes[]
provenance       tier, tier_reason, tier_score, tier_fell_back, libraries[],
                 normalizer_version, trace
gaps[]           label, kind, reason
guidance         addressed to the model reading this
```

`content_hash` covers the normalised graph — block text, type, level, order — and
not the serialised output. Whitespace, timestamps and rewritten asset URLs do not
change it, so re-distilling an unchanged page is genuinely a no-op. The outcome
is deliberately **not** part of it: the hash answers "is this the same page" and
the outcome answers "did reading it work", and folding one into the other would
make every transient refusal look like a changed page.

`gaps` names content the page has that this artifact does not: a collapsed panel
never opened, an entry screen never passed. Consult it before concluding a page
is silent on a subject.

`est_total_tokens` is what fetching the whole artifact would cost. It is an
estimate at roughly four characters per token, not a measurement.

---

## `content.json`

The complete graph. Everything in the manifest, plus:

- `blocks[]` — the content in reading order, each with `id`, `type`, `level`,
  `text`, `source`, `section_id`
- `actions[]` and `links[]` — what a visitor can do
- `latent[]` — text present in the markup that was never rendered to a visitor
- `structured` and `faq` — whitelisted facts from schema.org data
- `media_all[]` — images and video with their descriptions

`source` records where a block's text came from: `dom` (exact, rendered),
`static` (the served HTML), `canvas_scene` (read out of a 3D scene graph),
`canvas_ocr`, `canvas_fallback`. Anything other than `dom` and `static` was not
read from a document and should be weighted accordingly.

### The latent tier

`latent[]` is quarantined, and is the one part of the format with a security
rule attached. It holds text that exists in the markup but was never shown to a
human — a collapsed accordion, or text positioned off-screen specifically to be
read by an automated agent.

**No default rendering includes it.** It is reachable only through
`get_hidden_content`, which carries a stronger warning, and every block in it
keeps its trust marker inline so a fragment copied out of it still says what it
is. A renderer that leaks latent content into ordinary output is a security bug,
not a formatting one.

---

## `index.md`

What an agent reads. Front matter carries `url`, `outcome`, `http_status`,
`tier`, `order_confidence`, `graph_retention` and `reached_bottom`. Below it:

1. an untrusted-content notice
2. an outcome banner, **only** when the status is not `ok`
3. the title, summary and content
4. actions, navigation, structured data, gaps and the extraction audit

The untrusted-content notice is not decoration. Everything below it was quoted
from a third-party page and is data to report on, never instructions to follow.

---

## Compatibility

Within schema version 1.x:

- fields are **added**, never removed or repurposed
- the outcome status set does not gain members
- section id derivation does not change
- `content_hash` inputs do not change

Any of those would be a 2.0. Consumers should ignore fields they do not
recognise rather than failing on them.

`normalizer_version` inside `provenance` tracks the text normalisation rules
separately, because changing them invalidates every cached artifact. It is
recorded rather than implied.

`trace` is the complete set of inputs that determined a render: sieve version,
capture-script hash, Chromium build, viewport, locale, timezone and every
browser flag. It is what makes a snapshot replayable and a bug reproducible. Its
contents are informational and may change within 1.x.
