# Threads: a menu-bar navigator for Claude sessions

A feature proposal for mnemo. It lets a user keep one Claude Code session per
initiative — each pinned to its own working directory and terminal tab — and
navigate between them from a menu-bar popup with a live preview of each
initiative's context file.

Platform assumptions are concrete on purpose: **macOS**, **iTerm2** as the
terminal, **AppKit** for the UI. The threads root directory is configurable;
everything else (iTerm2, the tab badge emoji) is hardcoded.

> **Status.** Proposal. The original was a standalone macOS app (PoC); this
> document is the mnemo-integrated revision. The *feature behaviour* below is
> the PoC's hard-won knowledge and is unchanged. The *wiring* — process model,
> transport, config, search, persistence — has been reshaped to fit mnemo's
> actual architecture; see [§0 Integration with mnemo](#0-integration-with-mnemo)
> for the deltas and the rationale. Tracked by bullseye target T85 (and
> sub-targets T85.1–T85.5).

## 0. Integration with mnemo

mnemo is a single long-lived Go daemon that already exposes two HTTP surfaces on
`:19419` — the MCP endpoint at `/mcp` and a plain JSON API at `/api/*` (the
web dashboard at `/` is a client of the latter; `internal/api/api.go`). On
macOS it runs as a **LaunchAgent** via `brew services`, i.e. inside the user's
GUI session. Those two facts reshape the PoC's standalone assumptions.

**Deltas from the standalone PoC** (each is justified by a concrete mnemo
mechanism, not preference):

1. **The shim is a bundled `.app` that the daemon launches and supervises — not
   a fork-exec subprocess, and not a separate install step.** The PoC embedded
   all logic in the app; an earlier integration sketch made the Swift shim "a
   subprocess of mnemo". A fork-exec'd bare executable is the worst case for
   TCC: a global event monitor needs a *stable, signed bundle identity* to hold
   an Accessibility grant across upgrades, and an un-bundled child gets an
   unstable identity that re-prompts or fails silently. So the shim is its own
   signed `.app` bundle with its own TCC identity — but the **daemon launches
   it** (via `open -g`: background, no focus steal) on startup and relaunches it
   if it exits, so there is no second `brew services` unit and no manual launch.
   The shim talks to the daemon over `localhost` HTTP. The *intent* is
   preserved: all business logic lives in Go; the shim is a dumb view. This also
   serves the hard installation-friction requirement (§0.9).

2. **TCC is two identities, not one inherited grant.** TCC binds to a code
   signature and is never inherited from a parent process. iTerm2 is driven
   from Go via `osascript` (§4), so the **daemon** holds the **Automation →
   iTerm2** grant (this fits mnemo's existing subprocess-spawn pattern in
   `internal/compact/claudia.go`: neutral working dir, explicit PATH from the
   Homebrew formula). The **shim** holds only **Accessibility**, for its global
   ⌥-double-tap `NSEvent` monitor. The two grants are requested and surfaced
   independently in System Settings → Privacy.

3. **iTerm2 driving is centralized in the daemon; the CLI `go` delegates to it.**
   If `mnemo thread go <name>` (run from a terminal) drove iTerm2 directly, it
   would carry the terminal's TCC identity and trigger its own Automation
   prompt. Instead the daemon owns the single long-lived iTerm2 connection and
   the Automation grant; both the CLI verb and the shim are **clients** that
   POST `/api/thread/go`.

4. **Capabilities are a shared Go core behind two thin adapters.** Thread CRUD,
   `go`/find-or-spawn, sort, filter, and preview-render are implemented once in
   a Go package and exposed as **both** `mnemo_thread_*` MCP tools (for agents
   and the CLI) **and** `/api/thread/*` JSON endpoints (for the shim's
   interactive-rate hover/list path, which should not speak the MCP session
   protocol). The MCP tool-registration seam is `internal/tools/tools.go`
   (`Definitions()` + the `Call()` switch); the HTTP seam is
   `internal/api/api.go` (`RegisterRoutes`).

5. **The threads root is a mnemo config field, and the feature rides existing
   ingestion.** Add `threads_root` to the config struct (`internal/store/config.go`),
   with a `ResolvedThreadsRoot()` (`~` expansion) wired into `Registry.Reload()`
   for hot-reload, exactly like `synthesis_roots` — so it is editable live via
   `mnemo_config`, no restart, no private config file. Default `~/think/threads/`.
   Because `~/think` is mnemo's canonical **synthesis root**, adding the threads
   root to `synthesis_roots` means the proposal's **"deep search" (§6) is already
   satisfied**: thread `CLAUDE.md`s, TODO files, *and* transcripts are in the
   FTS index for free, and thread dirs that have sessions auto-register as
   trees-of-interest (`knownRepoRoots` derives roots from session cwds). No
   parallel thread indexer is required for search.

6. **Activity timestamp reuses the existing helper and keeps the filesystem
   walk.** §3's transcript half should call the existing `cwdToTranscripts()`
   (`internal/store/store.go`; already returns entries newest-mtime-first)
   rather than re-deriving the `~/.claude/projects/<encoded>/` encoding. Keep
   the *filesystem* walk rather than a pure DB query: the index stores
   `session_summary.last_msg` (the last **indexed** message timestamp), which
   lags un-ingested writes — a thread you are actively conversing in must rank
   live before ingest catches up. The DB value may be unioned in as a
   cross-check.

7. **No new SQLite tables.** Threads are a **filesystem projection** computed
   live (like configs, targets, and TODOs), respecting mnemo's strict
   append-only schema policy. The render cache stays shim-side, keyed on
   `(path, theme)` and invalidated by file mtime.

8. **Renderer: `goldmark`.** No markdown dependency exists in `go.mod` today and
   §5 leaves the choice open. `github.com/yuin/goldmark` with the GFM extension
   covers tables, strikethrough, autolinks, and task lists; it is pure Go (no
   new CGO, keeps the single-binary story); and it satisfies the proposal's
   "tag-filtering" requirement *for free* by leaving `WithUnsafe()` off, so raw
   HTML in a `CLAUDE.md` is never rendered into the preview. The GUI path needs
   **inlined** palette CSS because the HTML lands in `NSAttributedString(html:)`,
   not a web view; reuse the dashboard's existing light/dark token *values*
   (`ui/dashboard.html`) for visual parity.

9. **Installation friction is a hard requirement — the design minimizes it.**
   Concretely: (a) a single `brew install` ships the daemon binary *and* the
   bundled `.app`; `brew services start mnemo` starts the daemon, which
   auto-launches the menu-bar app (§0.1) — one unit, no separate GUI install,
   no second LaunchAgent. (b) The default activation is **clicking the status
   item, which needs no TCC grant at all**; the global ⌥-double-tap hotkey is
   **opt-in** — a toggle that requests Accessibility only when the user enables
   it — so a default install grants zero Accessibility. (c) Driving iTerm2 is
   centralized in the daemon (§0.3), so the user sees **one** Automation prompt,
   on first `go`, granted once. (d) The `.app` is notarized so Gatekeeper does
   not block first launch. Driving iTerm2 via AppleScript rather than its Python
   API (§4) means there is **no** "enable the Python API" step at all — the only
   prompt the feature ever raises is that one Automation grant.

The rest of this document describes the feature. Where a section's mechanism is
governed by a delta above, it is cross-referenced.

## 1. Concept

A **thread** is a directory representing one ongoing initiative. It contains a
context file (`CLAUDE.md`) that scopes a Claude Code session to that initiative,
plus whatever working files accumulate. Threads live flat under a configurable
root (default `~/think/threads/`; see Integration §0.5):

```
<threads-root>/
├── _template/          reserved: scaffold copied on `new`
│   └── CLAUDE.md       contains a {{NAME}} placeholder
├── _archived/          reserved: archive target (created lazily)
├── project-a/
├── project-b/
├── experiment-foo/
└── …
```

Rules:

- A directory is a thread iff its name does not start with `_` or `.`.
- `_template/` and `_archived/` are reserved and never listed as threads.
- Each thread's context file is `<thread>/CLAUDE.md`.

The context file follows a loose convention the tool reads in two places:

- `## Status` — the first non-blank, non-italic-placeholder line is the thread's
  status string; its **first word** (lowercased, markdown emphasis/punctuation
  stripped) is the compact state (`active`, `paused`, `blocked`, …).
- `## Current focus` — surfaced in verbose listing.

Section extraction stops at the next `## ` heading and skips italic-only
placeholder lines (`_like this_`) so an unfilled template section reads as empty
rather than echoing the placeholder.

### Relationship to mnemo

mnemo already ingests Claude Code transcripts and indexes them. This feature
reuses that in two places rather than re-implementing it:

- **Activity / MRU** depends on transcript-directory modification time (§3);
  mnemo already knows where each project's transcripts live (Integration §0.6).
- **Deep search** (§6) is a query against mnemo's existing FTS index; because
  the threads root is a synthesis root, thread content is already indexed
  (Integration §0.5).

The net-new surface is the thread data model, the iTerm2 tab plumbing, and the
menu-bar UI. Business logic lives in the mnemo daemon (Go); the AppKit shim is a
thin view that talks to the daemon over HTTP (Integration §0.1).

### Shim / service boundary

Draw the line so the Swift shim owns *only* what AppKit forces into its process:
window/popover/table/text-view construction, event handling, and forwarding user
intent. Everything else lives in mnemo (Go):

- **All capabilities are tools** (`mnemo_thread_*`) plus mirrored `/api/thread/*`
  endpoints (Integration §0.4). Thread CRUD, `go`/find-or-spawn, sort, and filter
  are server calls; the shim invokes them and does not reimplement their logic.
- **iTerm2 driving belongs in Go** (Integration §0.2–0.3, §4). Centralizing the
  `osascript` automation in the daemon keeps the Automation TCC grant singular;
  the shim never speaks to iTerm2 directly.
- **Markdown→HTML belongs in Go** (Integration §0.8). It feeds both the terminal
  and GUI paths and is pure logic. Only the irreducibly-AppKit step —
  HTML→attributed-string→text view — stays in the shim. See §5.

## 2. CLI / tool surface

One `mnemo` sub-command, `thread`, with sub-sub-commands; default is `list`.
The CLI attaches at the existing `os.Args[1]` switch in `main.go` (the same
dispatch that handles `diagnose`, `register-mcp`, …) as `case "thread"`. Tools
follow suit: `mnemo_thread_*`. MCP tools return structured JSON; the CLI may
offer an optional `--json`. Commands that touch iTerm2 (`go`, and `new` when it
opens a tab) delegate to the running daemon (Integration §0.3).

| Command | Behaviour |
|---|---|
| `list` (default) | Print active threads as a table. Columns: **name**, **state**, **activity**. `state` from `## Status` first word. `activity` = file count + relative time (`3 files, today`, `empty`). Header line `N threads in <root>`, trailing hint to run `show`. |
| `list -v` / `--verbose` | Four columns: name, truncated status (≤50), `focus: …` (≤80) from `## Current focus`, `(activity)`. No hint line. |
| `new <name>` | Validate kebab-case `^[a-z0-9][a-z0-9-]*$`; reject reserved names; reject if exists. Copy `_template/CLAUDE.md`, substituting `{{NAME}}`. Then open a tab for it (find-or-spawn, §4) unless `--no-tab`. |
| `show <name>` | Render the thread's `CLAUDE.md` to the terminal (§5 CLI path), then a `--- Files ---` listing of non-hidden, non-`CLAUDE.md` entries as `name size relative-time`, newest first (dirs get trailing `/`, size `-`). Unknown name → error + fallback listing on stderr, non-zero exit. |
| `archive <name>` | Move `<root>/<name>/` → `<root>/_archived/<name>/`. Refuse reserved names; error if destination exists. Create `_archived/` on demand. |
| `go <name-or-path>` | The only thread-action verb. Accept a bare kebab name (resolved under root) or an absolute/`~` path. Find the existing tagged tab and focus it; else spawn one (§4). `--no-resume`: always spawn a *fresh, untagged* tab — deliberately ephemeral, can't be re-matched by later `go`. |

Relative-time buckets (CLI): `today` / `yesterday` / `N days ago` (<7) /
`N weeks ago` (<30) / `N months ago`.

## 3. Thread data model and activity

A thread's **activity timestamp** is the max of two recursive walks:

1. The thread directory, taking the newest regular-file mtime, **excluding
   hidden files and `CLAUDE.md`**.
2. The thread's Claude Code transcript directory — located via mnemo's existing
   `cwdToTranscripts()` helper (Integration §0.6), which already canonicalises
   and returns entries newest-first.

The second walk is what makes a thread you are only *conversing* with (not
editing files in) still rank as recently active.

**File count** for the listing = top-level regular files, excluding hidden and
`CLAUDE.md`.

**Scaffolding** (`new`): validate name → read `_template/CLAUDE.md` → substitute
`{{NAME}}` → write the new `CLAUDE.md`. Nothing else is created; `HISTORY.md`,
`.claude/`, subdirs etc. accrue organically later.

**Archiving**: a directory move into `_archived/`, with reserved-name and
collision guards. No compression.

No SQLite tables are added for any of this — the model is a live filesystem
projection (Integration §0.7).

## 4. iTerm2 integration

> **Transport decision (delta from the PoC).** The original proposal drove
> iTerm2 through its **Python API** — protobuf messages over a WebSocket over
> iTerm2's Unix-domain socket, authenticated with a cookie+key. That is a
> large, fragile stack to rebuild, and it requires the user to *enable the
> Python API* in iTerm2's preferences. The implemented design drives iTerm2 via
> **AppleScript (`osascript`)** instead. iTerm2 3.3+ exposes exactly the
> operations `go` needs as scriptable commands: `variable named` (get) and
> `set variable named` (set) on a session, `create tab/window with profile`,
> `write`, and `select`. AppleScript is a fraction of the code, uses the same
> Automation TCC grant the daemon already needs, and — decisively for the
> minimal-installation-friction requirement (§0.9) — needs **no** Python-API
> enablement step. The daemon (Go) owns the single Automation identity; the
> Swift shim never touches iTerm2 (Integration §0.2–0.3).

The integration lives in `internal/iterm`. Each call builds a short AppleScript
and runs it via `osascript` (one `-e` argument per line, so quoting in a single
blob can't bite).

### Tab identity (tagging)

Each thread tab is tagged with the iTerm2 **session variable `user.thread`**,
set to **base64 of the thread's absolute path**. "The tab for thread X" is the
session whose `user.thread` equals `base64(X's path)`. base64 keeps the value a
single safe ASCII token (no spaces or quotes to escape through either the
AppleScript or shell layers), and comparing the *encoded* forms is equivalent
to — and simpler than — decoding both sides.

### Operations

- **Find + activate** — one AppleScript walks `sessions of tabs of windows`,
  reading each session's `user.thread` (defaulting to "" when unset); on a match
  it `select`s the session, its tab, and its window, then `activate`s iTerm2,
  and returns `found`. Otherwise `notfound`.
- **Spawn tagged tab** — `create tab with profile "Thread"` (falling back to the
  default profile when `Thread` is absent), or `create window …` when no window
  is open. Then `set variable named "user.thread"` to the tag and `write` the
  command: `cd <thread>` → set the badge via the `SetBadgeFormat` OSC escape
  (`🪡 <thread-name>`) → `exec /bin/zsh -ilc 'claude --continue 2>/dev/null ||
  claude'` (login shell so PATH comes from `.zprofile`; `--continue` resumes,
  falls back to fresh). With `--no-resume` the tab is **not** tagged and the
  command is plain `claude`, so a later `go` cannot re-match it.

## 5. Markdown preview rendering

Two independent paths. The markdown→HTML conversion is mnemo's (Go) in both
cases, using `goldmark` + the GFM extension (Integration §0.8). It must support
tables, strikethrough, autolinks, tag-filtering, and task lists.

**CLI (`thread show`)**: if stdout is a TTY and `glow` is on PATH, pipe through
`glow -w <cols> -` (width from `TIOCGWINSZ`, fallback `$COLUMNS`, then 100).
Otherwise print raw markdown. glow is terminal-only (no HTML mode), so it serves
the CLI path only.

**Menu-bar preview**: mnemo renders markdown → HTML, wraps it in a theme-aware
inline `<style>` block, and hands the HTML to the shim. The shim's only job is
the irreducibly-AppKit tail: `NSAttributedString(html:)` → display in an
`NSTextView`. (Rendering must not be a per-call subprocess on the GUI path —
≈320 ms startup each is too slow for hover; and a WebView content process won't
launch outside a `.app` bundle, which is why the HTML lands in a text view, not
a web view.)

Non-obvious constraints on the shim's HTML→text-view step, each of which will
bite if ignored:

- **`NSAttributedString(html:)` is main-thread only** — it uses WebKit's HTML
  parser internally. Never call it off the main thread.
- It **injects both literal bullet glyphs and `NSTextList` paragraph
  attributes**, producing doubled bullets and doubled indents. Strip the
  `NSTextList` attribute and fix head indents after constructing the string.
- The text view **must be TextKit 2** (`NSTextView(usingTextLayoutManager: true)`).
  TextKit 1 defers redraws of HTML-imported attributed strings until a scroll
  gesture ends, so scrolling looks frozen.
- The enclosing scroll view **must disable responsive scrolling**
  (`isCompatibleWithResponsiveScrolling = false` via an `NSScrollView` subclass).
  Even under TextKit 2, responsive scrolling defers draws inside layer-backed
  parents.
- For top-anchoring a short document, the clip view **must be flipped**
  (`isFlipped = true`); `NSClipView`'s default origin is bottom-left, so short
  content otherwise sticks to the bottom of the viewport.

The next three are HTML-generation concerns, so they sit on the mnemo side of
the boundary (the shim consumes finished HTML). The shim passes the current
appearance (light/dark) down so mnemo can pick the palette.

- **Preview-skip regions**: before rendering, strip any text between
  `<!-- preview-skip -->` and `<!-- /preview-skip -->`. Threads use this to keep
  boilerplate (a standard H1 + "this is a thread" preamble) in the file for
  Claude's context while omitting it from the hover preview.
- **Theming**: light/dark palettes (the shim reports `NSApp.effectiveAppearance`),
  injected as inline CSS. Palette values mirror the dashboard tokens
  (Integration §0.8).
- **Thread previews** get a synthetic H1 (the dir name) prepended at render time
  — not written to the file.

**Caching** is shim-side: cache the rendered attributed strings keyed on
`(path, theme)`, invalidated by file mtime.

**In-preview navigation**: internal markdown links (relative/absolute/`file://`
resolving to a local `.md`, or a directory containing `CLAUDE.md`) navigate
in-place with back/forward history (chevron overlay buttons). Everything else
(http(s)/mailto/…) opens externally and dismisses the popup.

## 6. The menu-bar popup

An accessory app (no Dock icon, `LSUIElement`) owning a status item, the popup,
and an *optional* global hotkey monitor. It is its own bundled, notarized
`.app` — launched and supervised by the daemon (Integration §0.1, §0.9) — and a
client of the mnemo daemon's HTTP API.

**Status item**: a fixed-square-length `list.bullet` SF Symbol template image.
Fixed square length matters — a variable-width glyph shifts the popover anchor.

**Activation**: click the status item to toggle — the default path, requiring
**no TCC grant**. Optionally, double-tap the Option (⌥) key anywhere to toggle
from anywhere; this global `NSEvent` monitor (a clean double-tap state machine:
each tap held <0.3 s, both within 0.3 s, any other key/modifier resets it;
left/right Option keycodes 58/61) requires Accessibility permission and is
therefore **opt-in** — enabling it in the shim's menu is what triggers the
Accessibility request (Integration §0.2, §0.9). A default install never asks.

### Component structure (the load-bearing part)

The popup is an **`NSPopover`** (transient → click-outside dismiss), anchored
below the status item. The choice of substrate is the single most important UI
decision:

- **Not `NSMenu`** — it runs a modal event-tracking loop that consumes all
  mouse/keyboard input including scroll-wheel events, so an embedded scrollable
  preview can never receive scroll.
- **Not a custom `.nonactivatingPanel`** — same scroll-routing problem from
  another angle; it can't become key, so scroll goes to whatever was previously
  key.
- `NSPopover` routes scroll/click/focus natively. The only thing you implement
  yourself is click-outside detection — but `.transient` gives that for free.

Layout inside the popover:

- **Left**: the markdown preview pane (~540 pt).
- **Right sidebar** (~261 pt), top to bottom: a search field, a scrollable
  thread list, a pinned footer of action rows.

**Thread list** is an **`NSTableView`** (two columns: name, compact age) in an
`NSScrollView`. Use a table view, not a stack view in a scroll view: the table
owns its own scroll layout, top-anchors naturally, reuses rows, and keeps scroll
position stable across content changes. Settings that matter: `style = .fullWidth`,
`automaticallyAdjustsContentInsets = false`, `selectionHighlightStyle = .none`
(selection is unused; clicks are dispatched via the table's `target`/`action`
using `clickedRow`).

**Marker column (🎯T85.6).** A leftmost column shows each thread's **marker** —
🪡 by default, ❗️ when the thread is marked *important*. Right-clicking a row
opens a context menu to toggle the marker; the menu is structured to grow into
more markers later (the marker is an open enum, `internal/threads.Marker`, not a
boolean). Importance persists as a hidden `.marker` file in the thread dir (a
filesystem projection — no schema). The column widens the whole popover by its
width. This generalizes — and replaces — the earlier special-cased `master`
thread: there is no `master` concept; any thread can be pinned.

Rows are sorted with **pinned (important) threads first — always at the top,
even when stale** — then by newest activity (threads with activity before those
without), ties by name. The age column uses compact buckets (`now` → minutes →
`30m` → 5-min increments → `3h`, hours → `2d` → days → `3w` → weeks).

Two `NSTableView` traps:

- `setRows` can run **before the popover is ever shown**, before `loadView` has
  created the table. `setRows` must force the view to load (touch `view` /
  `loadViewIfNeeded()`) or the table reference is nil and it crashes on first
  population.
- **Do not call `setFrame` on the popover's window after it shows** (e.g. in
  `popoverDidShow`). It re-triggers the popover's internal
  rounded-rect-with-arrow mask and crops ~13 pt off the top, hiding the first
  row. Set `contentSize` / `preferredContentSize` once at init and trust the
  popover's layout. The symptom is invisible in `frame`/`bounds` — only
  `tableView.visibleRect` reveals it.

**Hover**: exactly one row highlights at a time. Use a **single tracking area on
the table** (`.inVisibleRect`) + `mouseMoved` resolving `row(at:)` + one
`hoveredRow` index as the source of truth. Per-row `mouseEntered`/`mouseExited`
fire faster than they pair during fast scrolls and leave multiple rows stuck on.
Hovering a thread row renders its `CLAUDE.md` in the preview; hovering a footer
row clears it.

**Footer rows** (bottom-anchored, with a divider): "New thread…", "New session",
"Open", "Quit". "New thread…" shows a modal `NSAlert` for a kebab name then
invokes the `new` capability. "New … session" invokes `go` with `--no-resume`
semantics on the root's parent (a fresh untagged session there). "Open …"
reveals the root in Finder. "Quit" terminates the shim.

**Click dispatch**: clicking a thread row dismisses the popup and invokes the
`go` capability via the daemon. The daemon — which holds the iTerm2 Automation
grant — drives iTerm2; the shim never does (Integration §0.3).

**Prewarming**: on shim start and on each popup presentation, walk the thread
list and pre-fetch+cache each preview's attributed string one at a time so
hovering is instant.

### Deferred main-thread work

Under the shim's run loop, `Task { @MainActor … }` and
`DispatchQueue.main.async { … }` scheduled from event handlers **do not fire
reliably**. For deferred main-thread work use a zero-interval
`Timer(timeInterval: 0, repeats: false)` added to `RunLoop.main`. (Prewarming
chains these, guarded by a generation counter.)

### Copy / paste in a non-key popover

A status-item-anchored `.transient` popover does not become key window, so
`Cmd-C`/`V`/`X`/`A` have no first responder and beep. Route these manually to
the search field's editor or the preview pane. On show, activate the app and
make the popover window key so copy works without the user resorting to
right-click (which also half-breaks `.transient` dismissal).

### Search / filter

Typing filters the list on two channels:

- **Instant**: case-insensitive substring match on thread name, every keystroke.
- **Deep** (~200 ms debounce): a query against mnemo's FTS index for threads
  whose transcripts mention the term; results *broaden* the match set when they
  return. Empty/punctuation-only needles short-circuit to a no-op. Results are
  generation-counted so stale async replies are dropped. Because thread content
  is already indexed (Integration §0.5), this is a `/api/thread/search` call
  over existing FTS — no new index.

Keyboard nav: ↑/↓ move the hovered row (preview follows), Enter activates, Esc
clears the filter or — if already empty — dismisses the popup.

## 7. Dependencies and hardcoded values

**mnemo (Go) side**: `goldmark` for GFM markdown→HTML (Integration §0.8). No
protobuf or WebSocket dependency — iTerm2 is driven via `osascript` (§4). No new
SQLite schema (Integration §0.7).

**Swift shim side**: AppKit only — no markdown or protobuf dependency, since
that logic lives in mnemo.

**Shelled out to** (by the daemon): `osascript` (drives iTerm2, §4), `glow`
(optional, `thread show` only), `/bin/zsh`, `claude`.

**Configurable**: threads root directory (default `~/think/threads/`), as a
mnemo config field with hot-reload (Integration §0.5).

**Hardcoded**: iTerm2 as the terminal; iTerm2 profile name `Thread`; session
variable `user.thread`; marker file name `.marker` and the marker glyphs
(🪡 / ❗️); tab badge `🪡`; `~/.claude/projects/<encoded>/` transcript location.
