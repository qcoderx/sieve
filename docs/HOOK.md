# The Claude Code hook

Claude Code's `WebFetch` has no JavaScript engine. On a React or Next.js page it
retrieves the pre-render shell: a valid 200, valid HTML, no content. The agent
gets no signal that the fetch failed, so it either reports the page as empty or
invents something to fill the gap.

The detection half of this is already solved and published — community hooks
sniff for a `<noscript>` tag, an empty mount point or a challenge marker, and
then suggest reaching for a browser by hand. This is the other half. When
WebFetch comes back with a shell, sieve reads the page properly and hands the
content back in the same turn.

## Install

```json
{
  "hooks": {
    "PostToolUse": [{
      "matcher": "WebFetch",
      "hooks": [{
        "type": "command",
        "command": "sieve hook",
        "timeout": 60,
        "statusMessage": "WebFetch got a shell; reading the page with sieve"
      }]
    }]
  }
}
```

Put that in `~/.claude/settings.json` for every project, or `.claude/settings.json`
for one. `sieve` must be on `PATH`.

Check it is wired up without spending a browser:

```sh
echo '{"tool_name":"WebFetch","tool_input":{"url":"https://igloo.inc"},
       "tool_response":{"result":"You need to enable JavaScript to run this app."}}' \
  | sieve hook -dry-run
```

## What it does, and what it deliberately does not

**It fires only when the fetch failed.** A WebFetch that worked is left alone.
This matters more than anything else here: the hook sits on every fetch, and one
that re-reads pages the agent already has will be uninstalled within a day. The
decision costs a single string scan.

A response is treated as a shell when it says one of the things a page says when
its content has not arrived — "you need to enable JavaScript", "just a moment",
"checking your browser", "attention required", "access denied" — or when it
carries fewer than about forty words, which is a mount point rather than a
document.

**It never fails the turn.** Nothing on stdin, a payload from a future version of
Claude Code, a page sieve also cannot read: every one of those exits 0 and says
nothing. WebFetch already returned something, and sieve declining to improve on
it is not a reason to interrupt a session.

**It is bounded.** `-max-wait` defaults to 45 seconds and caps sieve's own
budgets, whatever the operator's usual settings are. A hook that stalls a turn is
worse than one that misses a page.

**It says why it fired.** The context begins with the reason WebFetch's response
was judged a shell, so a reader can disagree with it. A hook that silently
substitutes its own answer for a tool's is worse than one that explains itself.

**When sieve also fails, it says so rather than filling the silence.** If the
page turns out to be blocked, behind a login, or a challenge screen, the context
carries the outcome and its evidence, and explicitly tells the agent not to
describe the page as empty or guess at its contents. That case is the whole
reason the hook exists; getting it wrong there would be worse than not running.

## What it costs

On a page WebFetch read correctly: one string scan, no process, no browser.

On a shell: whatever the page costs sieve to read, bounded by `-max-wait`. For
igloo.inc — an empty `<body>` that draws every word in WebGL — that is about
1,280 tokens of content the agent would otherwise not have had at all.

## Flags

```
-dry-run     report the decision and do not read the page
-max-wait    give up rather than hold up the turn (default 45s)
```

Every flag `sieve distill` accepts also works here, so `-max-tier fetch` will
keep the hook out of the browser entirely, and `-v` will show what it decided.
