# Security Policy

## Reporting a vulnerability

Please report security issues privately rather than in a public issue.

**GitHub Security Advisories** — <https://github.com/qcoderx/sieve/security/advisories/new>

Please include, where you can:

- What sieve did that it should not have.
- A URL or a fixture page that reproduces it.
- A snapshot, if you can produce one: `sieve distill <url> --snapshot ./bug.sieve`.
  A snapshot lets the whole graph stage be replayed offline, which usually turns
  a report into a fix in one round trip rather than several.

**Do not attach a snapshot recorded with `--private`.** Those come from
authenticated sessions and sieve refuses to write them without an explicit
override for exactly this reason.

You should get an acknowledgement within a few days. This is a small project;
please allow reasonable time before disclosing publicly.

---

## What counts as a vulnerability

The threat model is [THREAT_MODEL.md](THREAT_MODEL.md), and it is the reference
for what sieve claims. In particular:

### In scope

- **Hidden content reaching a default payload.** Anything that gets
  `display:none` text, zero-opacity text, or background-coloured text into
  `blocks`, `index.md`, `index.html`, or an MCP content response. This is the
  project's central claim and a break here is the most serious class of report.
- **Latent tier leakage.** Any path by which the quarantine merges into the
  content tier.
- **SSRF.** Any way to make sieve connect to a private, loopback, link-local or
  cloud-metadata address — including via redirect, DNS, or a URL form the guard
  does not recognise.
- **Private artifact disclosure.** Anything that serves or caches an artifact
  marked private, or writes an unredacted snapshot of an authenticated session.
- **Metadata channel escapes.** Unbounded page-controlled text reaching an
  artifact through `alt`, `aria-label`, JSON-LD, or any other channel that
  should be capped or whitelisted.
- **Speculative content presented as verified.** Anything that marks an
  uncorroborated pixel recovery as `confirmed`.
- **Resource exhaustion** from a hostile response: unbounded reads, decompression
  bombs, or a page that can make sieve consume memory without limit.

### Out of scope

- **Prompt injection in general.** sieve closes the hidden-element channel and
  marks everything else as untrusted data. It cannot stop a page saying
  something, and it cannot make the consuming agent treat text as data. That the
  words `ignore previous instructions` appear in an artifact, correctly labelled
  and correctly attributed to a visible element on the page, is sieve working as
  designed. See THREAT_MODEL.md §4.2 and §8.
- **DNS rebinding.** Known and disclosed in THREAT_MODEL.md §5. A report that
  demonstrates it is welcome as a normal issue; it is not a new finding.
- **Extraction inaccuracy.** A wrong heading level or a garbled reading order is
  a bug, and an important one, but it is not a security issue. Open it publicly
  with a snapshot attached.
- **Anything requiring control of the machine sieve runs on.**

---

## Supported versions

sieve is pre-1.0. Only the latest release receives security fixes.

| Version | Supported |
|---|---|
| 0.1.x | Yes |
| < 0.1 | No |

---

## Our own commitments

- sieve obeys `robots.txt` and `crawl-delay`, and sends an identifying user agent
  with a contact URL.
- Per-host concurrency and interval floors **cannot be configured to zero**.
  Publishing an identity is permanent, and one badly behaved release would make
  the project blockable forever for everyone.
- sieve does not fingerprint-spoof, solve challenges, or authenticate.
- Vision is off by default, so an artifact cannot contain model-invented text
  unless someone explicitly turned it on.

If you believe sieve is behaving impolitely towards a site you operate, that is
a security report as far as we are concerned. Please tell us.
