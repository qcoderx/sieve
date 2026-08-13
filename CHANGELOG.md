# Changelog

## 0.1.0

The first release with a frozen artifact format. Everything in
[docs/ARTIFACT.md](docs/ARTIFACT.md) is a contract from here: within schema 1.x,
fields are added and never removed or repurposed.

Validated against a 334-site corpus: 311 clean exits, median 4 seconds,
ninetieth percentile 19.

### What sieve does

Reads a web page the way a browser does and returns text an agent can use. It
escalates — a plain fetch answers most pages in under a second, and the browser,
the scrolling sweep and canvas recovery are used only when a cheaper read comes
back thin. Every artifact records which rung answered and why.

It reads pages that serve an empty body and build themselves in JavaScript, text
drawn as geometry inside a 3D scene, and text that sites split one letter per
element for animation. It presses through entry screens. It notices collapsed
panels and says what it could not open.

No model touches the extraction path. Token reduction comes from removing
markup, boilerplate and duplication — never from paraphrasing.

### Format

- **An outcome on every artifact.** `ok`, `blocked`, `challenge`,
  `auth_required`, `spa_shell`, `empty_after_render` or `partial`, each with the
  evidence that produced it, the HTTP status, and on an error the beginning of
  the response body. A bot challenge, a login wall and an unhydrated shell all
  arrive as a valid 200 carrying valid HTML and no content; without this field
  an agent reports the site as empty or invents something to fill the gap.
- **Section ids derived from heading text**, so they survive the page changing.
  They are handed to agents over MCP and used to fetch content by name; when they
  were positional, two distillations of one page minutes apart produced the same
  twenty-one sections with seventeen ids pointing at different content.
- Hidden text is quarantined in a latent tier that no default rendering
  includes, reachable only by a caller that asks for it by name.

### Measurements

Reproducible with `sieve bench <url> --tokens`, which needs no API key:

| page | served HTML | manifest | |
|---|---|---|---|
| stripe.com | 240,611 | 2,406 | 100× |
| en.wikipedia.org (Go) | 249,121 | 2,680 | 93× |
| organimo.com | 36,782 | 1,311 | 28× |
| news.ycombinator.com | 11,979 | 3,014 | 4× |

The margin tracks page size, because the manifest names sections and their costs
rather than carrying them. A link-dense front page barely benefits; a large
content page benefits enormously. On a page of a few hundred tokens the manifest
costs more than the page, and sieve says so.

MCP tool definitions cost ~1,737 tokens per session, paid once rather than per
page.

### Reliability

- **Bounded browser shutdown.** Cancelling the allocator waits for Chromium to
  exit with no bound, and a browser that has stopped answering never provides
  one. One corpus run reached that line with a complete artifact already on disk
  and did not return to its caller for another fourteen minutes. Shutdown now
  has an eight-second deadline, after which the process tree is killed.
- **A watchdog.** Past its budget plus a grace, sieve prints the stage it last
  reached, kills the browser tree, and exits 5 — distinct from a failed run (1),
  a usage error (2), a refusal by policy (3) and an unreachable host (4).
- Snapshots record the tier, so a replayed artifact reproduces the original
  byte for byte rather than losing the field a reader consults first.

### Known limits

- Text painted as a texture rather than drawn as geometry cannot be read.
  `hatom.com` is committed as a failing question set with the ruled-out causes
  written down.
- One page at a time. No multi-page or whole-site mode.
- Under heavy machine load, large pages can time out and produce an empty
  artifact. They are labelled `empty_after_render` or `spa_shell` rather than
  reported as success, but the content is missing all the same.
