# sieve — Threat Model

**Version 1** · Applies to sieve 0.1.x

This is a versioned document rather than a paragraph in the README, because the
posture it describes will change and consumers need to be able to tell which
version they read.

---

## 1. What sieve does, stated as a security property

sieve renders a page in a real browser and emits only what the browser reported
as rendered. That is a different operation from parsing markup, and it has a
consequence worth stating precisely:

> **Rendering-grounded capture closes the DOM-text channel completely.** A
> `display:none` subtree never enters the content tier. Text that never reaches
> a visible opacity never enters the content tier. Text whose colour matches its
> own background never enters the content tier. A markup-based extractor ingests
> all three verbatim.

That is **hidden-element immunity, not injection immunity.** The distinction is
the whole point of this document. Claim the first and it survives adversarial
reading; claim the second and one reply containing
`alt="ignore previous instructions"` demolishes it.

---

## 2. Assets being protected

| Asset | Why it matters |
|---|---|
| The calling agent's context window | Everything sieve emits lands in one |
| The calling agent's *behaviour* | Text in a context window can attempt to redirect it |
| The host machine and its network position | sieve makes outbound requests to URLs it is handed |
| The user's own sessions and credentials | Only in local mode; see §6 |
| Third-party sites | sieve should not become a way to abuse them |

---

## 3. Adversaries

**A hostile page author.** Controls every byte of the response, including
markup, styles, scripts, metadata, structured data and any asset. Wants an agent
reading the page to take an instruction from it, or to report something the page
did not say.

**A hostile link.** An agent reading page A calls `distill` on a URL found in
page A. The URL is therefore attacker-influenced even though the user typed
neither it nor page A.

**A curious operator of a shared instance.** Wants to read artifacts produced by
other users of a shared cache.

Not in scope: an adversary who already controls the machine sieve runs on, or
who can modify sieve's own binary or configuration.

---

## 4. Channels, and what each one gets

A "channel" is any route by which page-controlled text can reach an agent.

### 4.1 Closed by rendering-grounded capture

These do not reach the content tier at all. This is the categorical claim.

| Channel | Mechanism | Disposition |
|---|---|---|
| `display:none` subtree | Not rendered | Quarantined in the latent tier, never in the default payload |
| Opacity below `MinVisibleOpacity` at every checkpoint | Never legible | Excluded |
| `visibility: hidden` chain | Not rendered | Excluded |
| Text colour matching its background | Legible to nobody | Excluded, and the block is flagged |
| Off-screen positioning | Rendered, but outside every viewport | Captured with geometry; ordering places it, and never-visible runs are excluded |

Tested by `TestAdversarialChannels`, against a fixture that carries a live
payload in each channel.

### 4.2 Bypass visibility by design, and are bounded instead

These sit on genuinely visible elements. They cannot be excluded without losing
real content, so they are capped, normalised, and marked.

| Channel | Mitigation |
|---|---|
| `alt`, `aria-label`, `title`, `figcaption` | Capped at 300 characters; the cap being hit is recorded as a flag; dropped entirely in `--strict` |
| JSON-LD / structured data | **Whitelist**, not blocklist. Only the fields in `jsonLDWhitelist` are read; values capped at 400 characters; nested objects are not flattened. Raw JSON-LD is never emitted |
| Form field labels and options | Capped and normalised |
| Link text and hrefs | Emitted as data; never followed automatically |

The whitelist means giving up fields nobody thought to include. That is the
right trade: a missing founding date is an inconvenience, and an unbounded
attacker-controlled string in an artifact whose premise is token reduction is
not.

### 4.3 Unicode

Normalised once, at the graph boundary, so every output format inherits it.

| Technique | Handling |
|---|---|
| Bidirectional overrides (`U+202A`–`U+202E`, `U+2066`–`U+2069`) | Removed; block flagged `bidi-control-removed` |
| Zero-width characters (`U+200B`–`U+200D`, `U+2060`, `U+FEFF`) | Removed; block flagged |
| Tag characters (`U+E0000`–`U+E007F`) | Removed |
| Soft hyphens, interlinear annotation, variation selectors | Removed |
| Other `Cf` category | Removed |

The normalizer carries a **version number that is an input to every content
hash.** Changing what it does invalidates every cached artifact everywhere, so
that has to be a decision rather than an accident.

### 4.4 Output-format structure injection

A page containing a line beginning `# ` would forge a heading inside the
artifact, changing its apparent structure. Markdown emission escapes characters
that are structural at line start, plus inline emphasis and link syntax. The
text still appears — escaped, not censored, because removing it would be lying
about what the page said.

### 4.5 The latent tier

Hidden content is **kept**, because a collapsed accordion is content a reader
can reach with one click, and discarding it is the largest source of missing
content in a scroll-only extractor.

Keeping it is safe only while the quarantine holds, so four rules do:

1. Latent content lives under its own top-level key and is never merged into
   `blocks`.
2. It is retrieved by a separate MCP tool, never by a flag on the normal content
   call. A flag is one typo away from being set by default.
3. Every latent block carries a fixed trust marker into every output format.
4. It is excluded from the headline token count.

`TestLatentNeverLeaksIntoDefaultOutput` asserts rule 1 across every format. If
that test fails, the claim in §1 is gone rather than weakened — fix the
renderer, not the test.

### 4.6 Recovered pixels

Text read out of a canvas by OCR or a vision model is a guess. It is
cross-checked against the text the site actually shipped:

- **Found** → marked `confirmed`, included.
- **Not found** → marked `speculative`, **excluded from every default payload**.

Vision is **off by default**. With it disabled the artifact structurally cannot
contain invented text.

---

## 5. Server-side request forgery

An agent calling `distill` means the URL is attacker-influenced in the general
case. The fetch layer therefore:

- Resolves DNS and refuses private, loopback, link-local, carrier-grade NAT,
  documentation, benchmarking and reserved ranges.
- Refuses cloud instance metadata endpoints by address, including the IPv6 one.
- Checks **every address** a host resolves to, not the first.
- Judges literal addresses directly rather than looking them up, so
  `http://127.0.0.1/` cannot be smuggled past a lookup-based filter.
- Re-checks on **every redirect hop**, at the browser's request-interception
  layer as well as the HTTP client's. Checking only the first request is the
  most common way an SSRF filter is defeated.
- Enforces scheme, size, time and redirect-count ceilings.

**Known residual risk: DNS rebinding.** sieve validates the addresses a host
resolves to, then the browser resolves the host again for the actual connection.
A resolver that answers differently between those two moments could direct the
connection somewhere the guard rejected. Pinning would require a browser process
per job. This is disclosed rather than fixed in 0.1.x.

---

## 6. Local mode and private artifacts

Local mode attaches to a user's own browser profile so that sites blocking
headless browsers can still be read. That profile has cookies and active
sessions, and the failure mode is obvious and serious: a page from behind a
login ending up in a shared cache, or in a trace file attached to a public issue.

- Anything produced with `--private` is marked private in its provenance.
- A private artifact is **never** served by `sieve serve` and is never eligible
  for a shared cache.
- `snapshot.Write` **refuses** to record a private session unless explicitly
  overridden, and redacts the served HTML when it is.
- The MCP server never enables local mode.

---

## 7. Politeness, and why it is a security property

sieve sends an identifying user agent with a contact URL, obeys `robots.txt` and
`crawl-delay`, caps concurrency per host, and enforces a minimum interval that
**cannot be configured to zero**.

Publishing an identity is permanent. Once there is a documented user agent and a
contact address, one badly behaved release makes the project recognisable and
blockable forever — for every user, not just the one who misconfigured it. The
floors are therefore not user-tunable downward.

sieve does not fingerprint-spoof, does not solve challenges, and does not
authenticate. A site that blocks it stays unread, and the artifact says so.

---

## 8. What sieve does not defend against

Stated plainly, because a threat model that claims completeness is not one.

1. **A determined page can still get text into a context window.** Anything a
   visitor can read, an agent can read. The defence is that it arrives labelled
   as third-party data, in a field, under a notice — not that it does not arrive.
2. **Prompt injection is not solved by any of this.** The consuming agent must
   still treat artifact text as data. sieve marks it; it cannot enforce it.
3. **New channels will appear.** This is a posture, not a fix.
4. **The confidence numbers are estimates.** They are reported as coarse buckets
   rather than decimals precisely because a wrong number is worse than no
   number, and they are uncalibrated until the benchmark corpus is large enough.
5. **Graph retention is not coverage.** It measures what survived the graph
   stage against what the capture observed. If the sweep missed half the page,
   it will report high retention on the half it saw. True coverage is a
   benchmark number, measured against hand-written ground truth.

---

## 9. Reporting

Security issues: see [SECURITY.md](SECURITY.md). Please do not open a public
issue for anything in §4 or §5.

---

## Changelog

- **v1** (0.1.0) — initial version.
