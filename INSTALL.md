# Installing sieve

The binary does the work. Connect it to Codex with MCP, or install the Claude
Code plugin for MCP plus automatic WebFetch fallback.

## 1. The binary

**npm** is the shortest cross-platform path:

```sh
npm install -g @qcoderx/sieve
```

That downloads a prebuilt binary for your platform, checks it against the
published SHA-256, and runs `sieve version` to prove it works before finishing.
No Go toolchain, no compiler, no build step.

**Or download one directly** from
[the releases page](https://github.com/qcoderx/sieve/releases) — macOS, Linux and
Windows, x64 and arm64 — and put it on `PATH`.

**Or build it**, if you have Go and would rather:

```sh
go install github.com/qcoderx/sieve/cmd/sieve@latest
```

Check whichever you chose:

```sh
sieve version
```

Chromium is the one other dependency, and only the tiers above `fetch` need it.
sieve finds Chrome, Chromium or Edge on its own; `sieve doctor` says so if it
cannot.

## 2A. Connect Codex

Codex and Claude use the same token-cheap MCP server; there is no second
extraction path to drift or produce different answers. After installing the
binary, register it once:

```sh
codex mcp add sieve -- sieve mcp
codex mcp list
```

Or add it manually to `~/.codex/config.toml` (or a trusted project's
`.codex/config.toml`):

```toml
[mcp_servers.sieve]
command = "sieve"
args = ["mcp"]
startup_timeout_sec = 10
tool_timeout_sec = 360
```

Start a new Codex session after adding it. Ask Codex to read a URL; it can call
`distill`, inspect the manifest, search it, and fetch only the matching blocks.

## 2B. Connect Claude Code

```
/plugin marketplace add qcoderx/sieve
/plugin install sieve@sieve
```

One install, three surfaces:

| | what it does | what it costs |
|---|---|---|
| **skill** | teaches Claude when a page needs sieve, and to read `outcome` first | nothing until invoked |
| **hook** | when WebFetch returns a shell, reads the page properly in the same turn | one string scan per fetch |
| **MCP server** | `distill`, `get_content`, `search_content` and the rest | ~1,860 tokens per session |

The MCP server is the only standing charge of the three. If you would rather
drive sieve from the skill alone, disable that server in `/plugin` and keep the
other two.

## Pieces, without the plugin

**The skill in every project:**

```sh
mkdir -p ~/.claude/skills
cp -r plugin/skills/sieve ~/.claude/skills/
```

Anything in `~/.claude/skills/` loads everywhere. A skill in a project's
`.claude/skills/` loads only there.

**The hook**, merged into `~/.claude/settings.json` for every project or
`.claude/settings.json` for one. Keep whatever is already in the file:

```json
{
  "hooks": {
    "PostToolUse": [{
      "matcher": "WebFetch",
      "hooks": [{"type": "command", "command": "sieve hook", "timeout": 60}]
    }]
  }
}
```

Open `/hooks` once afterwards, or restart: the settings watcher only watches
directories that had a settings file when the session started.

**The MCP server:**

```sh
claude mcp add sieve -- sieve mcp
```

For Codex, the equivalent one-line registration is:

```sh
codex mcp add sieve -- sieve mcp
```

or in `~/.codex/config.toml`:

```toml
[mcp_servers.sieve]
command = "sieve"
args = ["mcp"]
```

## Check it works

```sh
sieve doctor

# the hook, without spending a browser
echo '{"tool_name":"WebFetch","tool_input":{"url":"https://igloo.inc"},
       "tool_response":{"result":"You need to enable JavaScript to run this app."}}' \
  | sieve hook -dry-run

# a page WebFetch cannot read
sieve distill https://igloo.inc --quiet
```

Then ask Claude Code to fetch `https://igloo.inc`. Without the hook it reports an
empty page; with it, the site.

## If something is wrong

**`sieve: command not found` after installing the plugin.** The plugin shells
out to `sieve` by name and cannot install it. Do step 1.

**The hook does not fire.** Open `/hooks` once — the settings watcher only picks
up files in directories that had settings when the session began.

**A flag beginning with a slash behaves strangely on Windows.** Git Bash rewrites
any argument that starts with `/` into a filesystem path, so `--include /docs`
arrives as `C:/Program Files/Git/docs`. sieve ignores leading slashes on
`--include` for this reason; if another flag ever misbehaves there, suspect this
first.
