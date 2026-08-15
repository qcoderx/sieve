# Installing sieve

Two parts, and the order matters: the binary does the work, the plugin tells
Claude Code when to reach for it. The plugin is useless without the binary.

## 1. The binary

```sh
go install github.com/qcoderx/sieve/cmd/sieve@latest
sieve version
```

`sieve` must be on `PATH`. Everything below shells out to it by name.

Chromium is the one other dependency, and only the tiers above `fetch` need it.
sieve finds Chrome, Chromium or Edge on its own; `sieve doctor` will say if it
cannot.

## 2. The plugin, for every project

```
/plugin marketplace add qcoderx/sieve
/plugin install sieve@sieve
```

That brings three things at once:

| | what it does |
|---|---|
| **skill** | teaches Claude when a page needs sieve, and to read `outcome` first |
| **hook** | when WebFetch returns a shell, reads the page properly in the same turn |
| **MCP server** | `distill`, `get_content`, `search_content` and the rest |

The skill costs nothing until it is invoked. The hook costs one string scan on a
WebFetch that worked. The MCP tool definitions cost about 1,737 tokens per
session, which is the only standing cost of the three — disable that server in
`/plugin` if you would rather drive sieve from the skill alone.

## Just the skill, no plugin

If you want Claude to know about sieve in every project and nothing else:

```sh
mkdir -p ~/.claude/skills
cp -r plugin/skills/sieve ~/.claude/skills/
```

Personal skills in `~/.claude/skills/` load in every project. A skill in a
project's `.claude/skills/` loads only there.

## Just the hook, no plugin

Merge into `~/.claude/settings.json` for every project, or
`.claude/settings.json` for one. Keep whatever is already in the file.

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

## Just the MCP server, no plugin

```sh
claude mcp add sieve -- sieve mcp
```

For Codex, in `~/.codex/config.toml`:

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

## A note on Windows shells

Git Bash rewrites any argument that begins with a slash into a filesystem path,
so `--include /docs` arrives as `C:/Program Files/Git/docs`. sieve ignores
leading slashes on `--include` for exactly this reason, but if another flag ever
behaves strangely there, that is the first thing to suspect.
