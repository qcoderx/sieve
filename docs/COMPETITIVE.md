# Where sieve wins, where it does not, and what was measured

Every figure here comes from a live call made on one machine on one day. Where
a claim has not been measured it says so. Reproduce any row with the commands
at the bottom.

The comparison set is page readers, the tools that take a URL and hand text to a
model: **Firecrawl**, **Jina Reader**, and a stand-in for a plain server-side
fetch with no JavaScript engine (**webfetch-approx**). Playwright MCP and Chrome
DevTools MCP appear only where noted; they are browser automation and never
claimed to be efficient at reading, so beating them proves little.

---

## The categorical win: pages the others return empty

igloo.inc serves an empty `<body>` and draws every word as glyph geometry inside
a three.js scene. Graded against 40 hand-written facts taken from the site's own
JavaScript bundle:

| tool | tokens | facts | coverage |
|---|---|---|---|
| **sieve** | 1,687 | **40/40** | **1.000** |
| jina-reader | 17 | 2/40 | 0.050 |
| webfetch-approx | 2 | 1/40 | 0.025 |
| firecrawl | 23 | 0/40 | 0.000 |

Firecrawl renders and still returns nothing, because the words are not in the
document at any point. This is not a multiple. It is an answer against no
answer, and it is the one thing nothing else in the category does.

A cost-per-fact column is meaningless here and is left out on purpose: two
tokens for one accidental fact is not efficiency.

---

## Hidden text cannot reach the model

The adversarial fixture plants the same instruction through five channels. A
markup-based extractor ingests what it finds; sieve emits what rendered.

| channel | markup extractor | sieve |
|---|---|---|
| hidden tab panel | ingested | excluded |
| colour on colour | ingested | excluded |
| zero opacity | ingested | excluded |
| positioned off-screen | ingested | excluded |
| unwhitelisted JSON-LD | not reached | excluded |

Four of five injected instructions reach the model through the markup route.
None reach it through sieve.

**Scope this precisely.** It is hidden-element immunity, not injection immunity.
A page can still put an instruction in visible prose, or in alt text, and sieve
will carry it as data with an untrusted-content notice attached. The claim is
that one whole channel is closed, which markup-based extraction cannot say.

**It has a price, and the price is real.** pear.no ships its FAQ twice, once as
JSON-LD and once in a `div` at zero opacity that no visitor sees. Firecrawl
takes the invisible copy and scores 45 of 45. sieve refuses it and scores 37.
The same refusal that loses that row is the one that closes the channel above.

---

## Where sieve loses

On an ordinary page whose text is in the HTML, the cheap readers are cheaper and
more complete. pear.no, 45 facts:

| tool | tokens | facts | coverage |
|---|---|---|---|
| firecrawl | **1,032** | **45/45** | 1.000 |
| webfetch-approx | 1,045 | 44/45 | 0.978 |
| jina-reader | 1,213 | 44/45 | 0.978 |
| sieve | 1,897 | 37/45 | 0.822 |

This is worth publishing rather than hiding. It says what sieve is for: not
every page, but the pages the others cannot read, and the cases where knowing
the read failed matters more than the tokens.

All three miss one thing on that page: `Pear makes you appear`, the headline
split one letter per element for animation. sieve reassembles it.

---

## Reading failure is reported rather than implied

A bot challenge, a login wall and an unhydrated shell all arrive as a valid 200
with valid HTML and no content. Every other tool in the table returns that
silently. Chrome DevTools MCP returned 7 of 45 facts on pear.no and said nothing
was wrong; Firecrawl returned 0 of 40 on igloo.inc and reported success.

sieve returns `ok`, `blocked`, `challenge`, `auth_required`, `spa_shell`,
`empty_after_render` or `partial`, each with the evidence, the HTTP status, and
on an error the beginning of the response body. Across a 534-site corpus, 77%
came back `ok` and every one of the rest said which of the others it was.

This is the easiest thing here for a competitor to copy. Being first is the
whole advantage.

---

## Manifest first, sections on request

Firecrawl and Jina hand back the whole page. sieve hands back a description and
lets the caller fetch what it needs. Measured, whole-artifact reads:

| page | sieve | playwright-mcp | chrome-devtools-mcp |
|---|---|---|---|
| news.ycombinator.com | **2,226** | 11,956 | 9,520 |
| kubernetes.io | **2,346** | 3,268 | 2,820 |
| stripe.com | 9,196 | 16,095 | **8,965** |

Below about 1,500 tokens the content travels with the manifest, because
describing a small page and then fetching it costs more than sending it.
`index_only` turns that off for a caller surveying several pages to choose
between them.

---

## Claims not measured here

Stated so they are not mistaken for findings:

- **No model in the extraction path.** Architectural rather than measured:
  nothing in the pipeline calls one, so reduction cannot paraphrase. Vision is
  off by default and its output is confirm-only.
- **Provenance and confidence on every block.** sieve ships `source`, retention,
  ordering and heading confidence. Whether competitors ship an equivalent was
  not surveyed.
- **Escalation.** sieve records which tier answered and why. Firecrawl always
  renders and Jina never does, so both overpay or underread on the wrong half
  of the web, but their cost curves were not measured.
- **Local, no key.** sieve runs on your machine over stdio. Firecrawl and Jina
  are hosted, so the URL and its contents reach a third party. Relevant for
  internal or sensitive URLs; a property rather than a measurement.
- **Crawl4AI** was not tested at all.

---

## Reproducing this

```sh
sieve bench <url> --tokens                       # what sieve costs, no API key
sieve bench ./artifacts/<host> --questions testdata/questions/<name>.yaml \
      -coverage-only                             # facts recovered, no API key
```

The competitor figures were taken by driving each tool over its own interface
and counting what came back: Firecrawl through `api.firecrawl.dev/v1/scrape`,
Jina through `r.jina.ai` with caching off, the fetch stand-in through a plain
GET with tags stripped, and the two MCP servers over a real stdio session.
Coverage uses the same matching rule sieve's own check uses, so the comparison
is not graded on a scale sieve invented for itself.
