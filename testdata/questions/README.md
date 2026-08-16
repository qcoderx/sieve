# The question corpus

Thirty sets, fifteen to twenty questions each, in three bands.

The bands exist because a benchmark that only measures the cases you win is
marketing. The hard band is where sieve is sometimes the only reader that
returns anything; the easy band is where it must simply not lose, and where the
honest result is a tie and the cheap readers are cheaper. Publishing the easy
band is what makes the hard band worth believing.

| band | what it is | why it is here |
|---|---|---|
| **hard** | canvas, WebGL, scroll-driven, shattered text | where sieve wins categorically |
| **medium** | heavy JS, SPA shells, content behind disclosures | where sieve wins on margin |
| **easy** | manual pages, specifications, plain documentation | where sieve must **not** lose |

## The rule about ground truth

Every fact is read from the page's own source, never from a sieve artifact.
Usually that is `curl | sed -e 's/<[^>]*>//g'`; for `igloo.inc`, where the words
are glyph geometry and appear in no document at any point, it is the site's own
JavaScript bundle.

Grading against your own output is a closed loop that can never fail. Each set
records in its header exactly where its truth came from, so that claim can be
checked rather than taken.

## Sets

### Hard

| set | page | note |
|---|---|---|
| `igloo` | igloo.inc | empty `<body>`; every word is MSDF glyph geometry |
| `lusion` | lusion.co | WebGL, scroll-driven |
| `brunosimon` | bruno-simon.com | a 3D scene as the whole interface |
| `cuberto` | cuberto.com | animation library hides most served text |
| `organimo` | organimo.com | canvas-driven |
| `pear` | pear.no | permanently animating; the noisiest row in the corpus |
| `locomotive` | locomotive.ca | scroll hijacked by the hijacker's own authors |
| `basement` | basement.studio | 200 KB of markup around 3 KB of prose |
| `dogstudio` | dogstudio.co | every sentence split across animated lines |
| `aurelia` | fixture | three.js text, offline, ships in this repository |

### Medium

| set | page | note |
|---|---|---|
| `hatom` | hatom.com | **deliberately failing**: kept because a corpus with no failures is not a corpus |
| `httpcaching` | MDN, HTTP caching | long article inside an application shell |
| `redisstrings` | Redis strings | examples behind language tabs |
| `caddyfile` | Caddyfile concepts | reference inside a docs SPA |
| `vitewhy` | Vite, Why Vite | carries a line addressed only to LLMs, which sieve refuses to read |
| `astroislands` | Astro, islands | article inside a forty-link sidebar |
| `supabasesessions` | Supabase sessions | second half is an FAQ in accordions |
| `stripewebhooks` | Stripe webhooks | prose rules among code samples in eight languages |
| `kilnworks` | fixture | everything worth knowing behind a control |
| `northwind` | fixture | prose threaded through eight injection channels |

### Easy

| set | page | note |
|---|---|---|
| `sqlite` | SQLite, appropriate uses | plain HTML, dense figures |
| `sqlitetxn` | SQLite, BEGIN TRANSACTION | rules with exact conditions attached |
| `nginx` | nginx beginner's guide | plain reference |
| `curlcookies` | curl, HTTP cookies | reference wrapped in a thirty-link sidebar |
| `pipe7` | pipe(7) | a manual page: about as plain as the web gets |
| `jsonapi` | JSON:API 1.1 | a specification document |
| `godev` | go.dev | a project home page |
| `kubernetes` | kubernetes.io | the whole-site row |
| `prometheus` | Prometheus overview | plain documentation |
| `optref` | fixture | API reference shape, offline |

## Running one

```sh
sieve distill <url> -out ./artifacts
sieve bench ./artifacts/<dir> --questions testdata/questions/<set>.yaml -coverage-only
```

`-coverage-only` needs no API key and calls no model: coverage is string matching
against the artifact, so writing a set costs nothing to check.

The four fixture sets run offline with no network at all, in CI:

```sh
go test ./internal/bench -run TestOfflineCorpus -v
```

## When a fact is missing

The bench output says it, and it is worth reading before assuming a bug: a fact
the page never states, or states in other words, is a fault in the question set
rather than in the extraction. Both kinds have been found this way, and the sets
record which was which where it was interesting — `vitewhy` q19 documents a
question that was wrong, and `dogstudio` documents two ways that row can fail
without sieve having changed.

Sets are not trimmed to score well. `locomotive` sits at 0.912 and `stripewebhooks`
at 0.943 because those pages really say things those artifacts really do not
carry. A corpus where every row scores 1.000 has been fitted to the tool rather
than measured with it.
