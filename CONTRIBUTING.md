# Contributing to sieve

Thank you for considering it. This document is short and specific, because the
parts of this project that are easy to get wrong are not the obvious ones.

---

## The fastest useful contribution

**A library detector.** `internal/render/fingerprints.json` is a data file, not
code. Adding a detector for an animation library, scroll hijacker, 3D renderer
or site builder takes a pull request against that file plus a fixture — no Go, no
release, no maintainer bottleneck.

```json
{
  "n": "my-scroll-library",
  "g": "MyScrollLib",
  "w": 0.9,
  "c": "scroll",
  "note": "Translates content with transforms; window.scrollY stays at zero, so document coordinates are meaningless and ordering falls back to checkpoint basis."
}
```

- `g` is a dotted path on `window`; `s` is a CSS selector. Either is enough.
- `w` is how strongly this predicts that a cheap fetch will miss content. A
  scroll hijacker is `1.0`; a tooltip library is `0`. Weights at or above `0.85`
  set a floor of `sweep` on their own, so be deliberate.
- `note` is surfaced by `sieve doctor`. Write what breaks, not what the library
  is.

---

## Reporting an extraction bug

Attach a snapshot. It is the difference between a fix in one round trip and a
conversation across a week.

```sh
sieve distill https://the-site.example --snapshot ./bug.sieve
```

A snapshot records the capture, not the artifact, so the whole graph stage can be
re-run against new code — offline, deterministically, long after the site has
changed.

```sh
sieve replay ./bug.sieve -blocks
```

Do not attach a snapshot recorded with `--private`; those come from
authenticated sessions.

---

## Building and testing

```sh
go build ./...
go test ./...                            # needs Chromium for some packages
SIEVE_SKIP_BROWSER=1 go test ./...       # the rest
```

The browser-free packages — `escalate`, `corroborate`, `textnorm`, `safety`,
`tokens`, `emit` — carry the tests that must pass everywhere. The `graph` and
`render` packages need a browser and are skipped without one.

---

## Golden files

Extraction quality regressions are **silent**. Nothing errors, nothing crashes,
the artifact just quietly says less than it did last week. Golden files are the
only cheap way to notice.

```sh
go test ./internal/graph -run TestGolden           # check
go test ./internal/graph -run TestGolden -update   # accept a change
```

Blocks are keyed on their structural position plus a hash of their text, not on
sequential ids, so inserting one paragraph shows up as one added line rather
than two hundred renumbered ones. **Read the diff before you `-update`.** A
golden diff nobody reads catches nothing.

---

## Things that will get a pull request sent back

These are not style preferences. Each one has cost this project real time or
would break a claim it makes.

### 1. Anything that lets latent content into a default payload

The latent tier holds exactly the material the visibility filter exists to
exclude. It is safe only while it stays in its box. One convenience shortcut
that flattens the arrays, one emit function that concatenates, and the security
claim in the README is gone rather than weakened.

`TestLatentNeverLeaksIntoDefaultOutput` asserts this on every format. If it
fails, fix the renderer — never the test.

Specifically: no flag on `get_content` that includes hidden content, no
`append(g.Blocks, g.Latent...)`, no "convenience" accessor that merges them.

### 2. Using intercepted payloads as a source of content

The corroboration index answers exactly one question: *does this string appear in
what the site shipped?* It offers no way to enumerate its contents, and that is
deliberate.

Hydration blobs routinely carry draft copy, other locales, unpublished fields and
adjacent records. Emitting any of it stops being extraction and starts being
republishing someone's back office. Confirmation promotes a guess to verified;
absence leaves it speculative. Nothing in the index may become a block.

### 3. Deriving an escalation threshold from the page being judged

Thresholds are pinned constants in `escalate.Thresholds`. A threshold computed
from the page under judgement can move under it, and a tool whose judgement
depends on the weather has no claim to determinism.

If a decision needs new evidence, add a factor with a stated weight — or, when
the evidence is genuinely decisive on its own, a floor in `libraryFloor` with a
comment explaining why arithmetic was not enough.

### 4. Non-determinism in anything that reaches the artifact

Two runs of an unchanged page must produce the same content hash. Things that
have broken this before:

- Iterating a Go map without sorting the keys.
- Feeding a viewport-sampling artefact into a confidence score. `EverVisible`
  looked like a visibility signal and was really a record of where the sweep
  happened to stop; it made confidence flip between runs.
- A geometry-only settle signature. A pure opacity fade moves nothing, so the
  page looked settled while text was still at 0.4 opacity.

If you add a signal, ask what it depends on that is not the page.

### 5. Loosening the politeness floors

Per-host concurrency and interval floors clamp upward and cannot reach zero.
Publishing an identity is permanent: one badly behaved release makes the project
recognisable and blockable forever, for every user.

---

## Style

Follow the surrounding code. A few conventions that are load-bearing rather than
aesthetic:

- **Comments explain why, not what.** The code says what it does. A comment
  earns its place by recording a decision, a constraint, or a trap — especially
  one that cost time to find.
- **Name things narrowly enough to be true.** `GraphRetention` is not called
  coverage because it is not coverage. If a name would be read to mean more than
  the value delivers, pick a smaller name.
- **Errors carry instructions.** `ErrChromiumNotFound` tells the user what to
  install and which flag to pass. A stack trace is not an error message.
- **Every claim in a comment should be checkable.** If a comment says a test
  asserts something, name the test.

---

## Commit messages

One line saying what changed, then a paragraph saying why if the why is not
obvious. Reference an issue if there is one.

---

## Licence

Contributions are accepted under Apache 2.0, the same licence as the project.
