# Vault export (Obsidian / Logseq)

## Why

mnemo's knowledge graph is *wide but shallow*: it captures everything
automatically but understands nothing. A human knowledge graph is the
reverse — *deep but narrow*: well-understood, but only what you chose
to write down.

The vault bridges them. When `vault_path` is set, mnemo continuously
materialises its SQLite graph as Markdown files. You annotate, connect,
and extend the notes in Obsidian or Logseq. Those additions are indexed
by mnemo's FTS5 engine within ~2 seconds of saving — so every
`mnemo_search` query searches both automated and human content together.

Concrete gains:

- **Context that survives sessions** — decisions, tradeoffs, and
  lessons learned accumulate in one place, readable without querying
  mnemo
- **Serendipitous cross-linking** — annotate a session note in one
  repo; a future search in a *different* repo surfaces it
- **Correcting AI blind spots** — mnemo's automated extraction misses
  nuance. Add context below the fence; it becomes searchable immediately
- **Human insight amplifies automated signal** — the generated graph
  grows by itself with every session; you only annotate the parts that
  matter, with zero maintenance burden on the rest

## What gets exported

```
<vault_path>/
├── index.md               — root index (repos + total sessions)
├── repos/<repo>.md        — per-repo index: recent sessions + decisions
├── sessions/<repo>/       — one note per session (full conversation)
├── decisions/<repo>/      — detected proposal + outcome pairs
├── memories/              — project memory files (globally unique names)
├── skills/                — skill procedures from ~/.claude/skills/
├── configs/               — CLAUDE.md project instruction notes
├── plans/<repo>/          — implementation plans
├── targets/<repo>/        — convergence targets
├── ci/<repo>/             — CI run summaries
└── prs/<repo>/            — pull requests and issues
```

Each note includes YAML frontmatter (tags, aliases, wikilinks) and a
`<!-- mnemo:generated -->` fence. Everything above the fence is
rewritten on each sync; everything below is **yours and is never
touched**.

## How to enable

Add `vault_path` to `~/.mnemo/config.json`:

```json
{
  "vault_path": "~/Documents/mnemo-vault"
}
```

`~` expands to your home directory. The directory is created on first
startup — you don't need to create it manually.

Restart the daemon after changing config:

```bash
brew services restart mnemo
```

**Verify it's working** — in any Claude Code session, ask Claude to
call `mnemo_vault_status`. It will report the vault path and the number
of Markdown files written. A freshly populated vault of 200 sessions
takes under 10 seconds; 5,000 sessions under 2 minutes. Subsequent
daemon restarts are near-instant because mnemo embeds the entity
timestamp in each note and skips files that are already up to date.

Then open the directory in Obsidian (`Open folder as vault`) or Logseq
(`Add a local graph`). No plugins required — the vault uses only core
features: YAML frontmatter, `[[wikilinks]]`, and standard Markdown.
Subsequent syncs run every 5 minutes in the background.

## Adding human knowledge

Two ways to flow content into mnemo's index:

**Annotate generated notes** — edit below the fence:

```markdown
# My session

*Generated content above — do not edit*

<!-- mnemo:generated -->

## My notes

Worth revisiting — approach has known edge cases with concurrent writes.
```

**Drop standalone `.md` files anywhere in the vault** — files without
a `<!-- mnemo:generated -->` fence (your own Obsidian/Logseq notes,
reading lists, design sketches, …) are indexed in full. mnemo never
overwrites them; only the directories it creates carry generated content.

Either way, mnemo picks up changes within ~2 seconds (OS-native file
watcher) and the new content becomes searchable via `mnemo_search`
results tagged `[vault]`. Deleting a file (or moving it out of the
vault) removes its index entry on the next sync.

## Manual sync

In any Claude Code session, ask Claude to call the vault tools:

- `mnemo_vault_sync` — triggers an immediate full sync (writes all
  changed notes; skips unchanged files)
- `mnemo_vault_status` — shows vault path and current file count

## Performance

When `vault_path` is **not set**, vault code is entirely inactive — no
goroutines, no file I/O, zero overhead.

When enabled, the cost is:

- **Initial sync**: scans all indexed entities on startup; an entity
  timestamp embedded in each note (just above the fence) makes
  restarts near-instant by skipping unchanged files
- **Periodic sync**: one `Sync()` call every 5 minutes, writing only
  changed files
- **File watcher**: OS-native kqueue/inotify — CPU overhead is ~zero
  when the vault is idle; fires only on file changes
