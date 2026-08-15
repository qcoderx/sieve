# What sieve reuses, and how it knows the page has not changed

Two separate mechanisms, often confused. One decides whether to re-read a page
inside a session; the other decides whether an artifact you already have is
still the same artifact. They answer different questions and neither substitutes
for the other.

## Within a session: a time-boxed job cache

The MCP server keeps completed artifacts for **30 minutes** by default
(`-cache-ttl`). A second `distill` of the same URL inside that window returns
the stored manifest and says `"served from cache"` in the message.

Two things it will not serve:

- **A job that is not `ready`.** A render still in flight is polled, not reused.
- **An incomplete artifact.** If the sweep was cut short, the artifact is
  reusable for reading but never served as though it were final, because a
  caller cannot tell a partial read from a complete one by looking at it.

This is a convenience, not a correctness mechanism. It saves a browser launch
when an agent asks twice in a row.

## Across sessions: the content hash

Every artifact carries a `content_hash`: a SHA-256 over the **normalised
semantic graph** — block text, type, level and order — not over the bytes on
disk.

```
"content_hash": "sha256:7eb27ce43841af7fd7603167116d134aaaf1566f7d800de1ed7efff7fe14a4de"
```

What that buys is a comparison that means something. The hash does not change
when:

- the page is re-rendered at a different moment and asset URLs are rewritten
- whitespace, indentation or serialisation differs
- the timestamp, the Chromium build or the viewport differ
- the artifact is emitted by a different sieve version, as long as
  `normalizer_version` is unchanged

So **re-distilling an unchanged page is genuinely a no-op**: same hash, and you
can skip whatever you were going to do with it. Two artifacts of the same URL
with different hashes have different content, and that is the only thing a
differing hash means.

Deliberately **not** part of the hash:

- **The outcome.** A page read normally and the same page read through a rate
  limit are not the same content, but the hash answers *is this the same page*
  and the outcome answers *did reading it work*. Folding them together would
  make every transient refusal look like a changed page.
- **The audit.** Retention and confidence describe the read, not the document.

`normalizer_version` inside `provenance` is the escape hatch: it is part of the
hash by construction, so changing text normalisation invalidates every cached
artifact at once rather than silently comparing old normalisation against new.

## Compared with WebFetch

WebFetch caches by URL with a **15-minute TTL** and no content identity: after
15 minutes it re-fetches, and it cannot tell you whether anything changed.

sieve's job cache is twice as long, and the content hash answers a question
WebFetch has no way to answer — whether the page you have is the page that is
there now. A pipeline can store the hash, re-distill on a schedule, and do work
only when it differs.

## What is not cached

- **Nothing is written to a shared cache.** `--private` marks an artifact as
  never eligible for one and refuses to record a snapshot; there is no shared
  cache to write to today, and the flag exists so artifacts produced against an
  authenticated session are marked before there is.
- **robots.txt** is cached per host for the life of the process, with its own
  file beside the escalation memory.
- **Escalation memory** persists between runs: a domain that has ever needed a
  browser keeps needing one, so a page near the threshold is not judged
  differently on different days. Clear it by deleting the file named in
  `sieve distill -h` under `-memory`, or disable it with `-memory ""`.

## Checking it yourself

```sh
sieve distill https://example.com --out ./a --quiet
sieve distill https://example.com --out ./b --quiet
jq -r .content_hash ./a/example.com/manifest.json ./b/example.com/manifest.json
```

Identical hashes on an unchanged page. If they differ, the page changed between
the two reads, and the artifacts will show where.
