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

**It has a price.** pear.no ships its FAQ twice, once as JSON-LD and once in a
`div` at zero opacity that no visitor sees. A markup reader takes the invisible
copy; sieve refuses it and reaches the same content by the JSON-LD instead. On
that page the price turns out to be small -- 43 of 45 against 44 -- but it is
the same refusal, and on a page that hides content with no second copy it would
cost more.

---

## The ordinary pages

Three runs per cell, median with the range beside it. Single runs are what
produced every wrong number in the earlier version of this document.

**organimo.com**, 59 facts. A commerce page that splits words across elements
and folds most of its substance into seven collapsed panels.

| tool | facts | tokens |
|---|---|---|
| **sieve** | **59/59** | 1,671 |
| jina-reader | 58/59 | 1,735 |
| webfetch-approx | 57/59 | 780 |
| firecrawl | 43/59 | 991 |

**pear.no**, 45 facts. Prose in the HTML, headings animated one letter per
element.

| tool | facts | tokens |
|---|---|---|
| webfetch-approx | 44/45 [44-44] | 1,045 |
| jina-reader | 44/45 [33-44] | 1,212 |
| sieve | 43/45 [43-43] | 1,902 |
| firecrawl | failed on all three runs | |

sieve costs more here and recovers about the same. The cheap readers get this
page because its prose is in the document; what they miss is `Pear makes you
appear`, the animated headline, which sieve reassembles.

### On variance

sieve was believed to be noisy on pear.no: two runs scored 42 and 35 of 45. It
is not. Three consecutive runs scored 43, 43 and 43, and igloo.inc scored 40, 40
and 40. The earlier swing was machine contention during a 200-site sweep, not
the tool, which is worth knowing because it is the same contention that produced
eighteen empty artifacts in that sweep.

Jina is the one that varies: 33 to 44 on the same page.

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

## The tier that cannot rot

Everything above points at a live site, which makes it a measurement of the
internet on a particular Tuesday. Sites get redesigned, put behind a challenge
page, and taken down. Nobody reading this in a year can re-run the igloo.inc row
and get the igloo.inc number, and neither can I.

So there is a second tier underneath it, and it runs offline:

```sh
go test ./internal/bench -run TestOfflineCorpus -v
```

Three pages ship in this repository under this project's own licence, each with
its ground truth beside it in `testdata/questions/`:

| fixture | what it is | coverage | tier reached |
|---|---|---|---|
| `immersive` | words drawn as glyph geometry in a three.js scene, empty `<body>` | 0.867 | render |
| `adversarial` | real prose threaded through eight injection channels | 1.000 | render |
| `disclosure` | everything worth knowing behind a control you must press | 1.000 | sweep |

No network, no third party's content, no link rot, and reproducible by anyone
forever. The floors are recorded in the test, set just under the measured score,
so a change that quietly costs coverage fails the build rather than being
noticed a month later while writing a table like this one.

What that tier deliberately does not measure is as important as what it does.
Two of those pages carry material sieve must **refuse**: eight injected
instructions on one, four controls that speak on a visitor's behalf on the
other. A coverage score has no way to express "and none of that appeared", so
those are asserted string by string in the render and distill tests instead.
Reading the table above as the whole fixture result would be reading half of it.

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
