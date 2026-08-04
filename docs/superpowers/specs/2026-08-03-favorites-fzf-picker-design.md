# Favorites + fuzzy-search folder picker

Date: 2026-08-03
Status: Approved, ready for implementation plan

## Problem

The "New session" screen renders every subdirectory of every configured root
(`~/git`, `~/dev`) as a flat wall of chip buttons (`static/index.html:291-320`,
backed by `folders.go`'s `listFolders`). With ~40 repos across two roots, this
is the dominant, most cluttered part of the phone UI (see screenshot in prior
conversation). The free-text `#cwd` input already exists as a fallback but has
no autocomplete.

## Goal

Replace the always-visible chip wall with:

1. A short **favorites row** of repos the user has explicitly starred.
2. A **fuzzy-search dropdown** on the existing Folder text input, covering
   every repo from every root, for anything not favorited.

No server-side changes. Everything is client-side JS/CSS in `static/index.html`,
reusing the existing `/api/folders` response.

## Non-goals

- No frecency/auto-favoriting — favorites are explicit, user-curated (star
  toggle), per the user's explicit choice over auto/frecency and static-config
  alternatives.
- No multi-level directory browsing — the data model stays one level deep
  (root → immediate subdirectory), matching `listFolders`.
- No backend/API changes — `/api/folders` response shape is unchanged.

## Design

### Data flow

`loadFolders()` already fetches `[{root, dirs[]}]` from `/api/folders` on page
load. This becomes the single source of truth for both the favorites row and
the fuzzy dropdown; no new endpoint is needed. Flatten it client-side into a
list of `{root, dir, path}` entries once per load.

### Favorites storage

Pinned entries are stored client-side in `localStorage` under a single JSON
key (e.g. `claudio.favorites`), as an array of full paths
(`root.replace(/\/$/, '') + '/' + dir`). Plain array of strings — no need for
a richer structure since path is already the unique key used elsewhere in the
existing code (`cwdInput.value = path`).

Helper functions: `loadFavorites()`, `saveFavorites(paths)`,
`toggleFavorite(path)`. Read/write directly around a `Set<string>` kept in
memory, persisted to `localStorage` on every toggle.

### Favorites row (replaces the current `#folder-groups` chip wall)

- Rendered above the Folder input, same visual style as today's chips
  (`.chip` class), but only for paths currently in the favorites set.
- Empty state: the row renders nothing (no placeholder text, no empty
  container height) when there are zero favorites yet.
- Tap a favorite chip: same behavior as today's chip click — fills `#cwd` and
  `#name` (if `#name` is empty), highlights the selected chip.
- No unpin control directly on the favorites row itself; unpinning happens
  from the dropdown (see below) to avoid a second interaction pattern for the
  same star toggle.

### Fuzzy dropdown on the Folder input

- Triggered on `focus` (shows a ranked list even with an empty query, capped
  at ~10 rows: favorites first, then the rest alphabetically) and on every
  `input` event (re-filters/re-ranks against the flattened repo list).
- Match/score: classic fzf-style subsequence scoring — query characters must
  appear in order in the candidate string (matched against `dir`, the repo
  basename, not the full path), score favors contiguous runs and matches
  closer to the start of the string. Implemented inline, no dependency.
- Each result row shows the repo name (and root, when ambiguous or always, for
  clarity — e.g. "claudio — ~/git") plus a ★/☆ toggle button that calls
  `toggleFavorite(path)` without closing the dropdown or filling the input.
- Tapping the row itself (outside the star control) fills `#cwd`/`#name`
  exactly like a chip click today, and closes the dropdown.
- Dropdown closes on blur (with a short delay so a tap on a row/star isn't
  lost to the blur firing first — standard pattern: use `mousedown`/`touchstart`
  on rows instead of relying on `click`, or delay blur-close via `setTimeout`).
- Keyboard support (desktop use, since this is a phone-first tool but is still
  reachable from a browser): Up/Down to move a highlighted row, Enter to
  select, Escape to close.

### Removed

- The current unconditional rendering of every root's full `dirs[]` as chips
  (`static/index.html` `chip-group` / `chip-group-label` / `chips` divs and
  their render loop in `loadFolders()`).

### CSS

New/changed rules for: `.favorites-row` (reuses `.chip`/`.chips` styling),
`.fuzzy-dropdown` (positioned under `#cwd`, scrollable list, max-height so it
doesn't push the Spawn button off-screen on small phones), `.fuzzy-row`,
`.fuzzy-star`.

## Error handling / edge cases

- `/api/folders` fails or returns empty: dropdown and favorites row simply
  don't populate (matches existing silent-failure comment in `loadFolders()`
  at `static/index.html:317-319`); the plain `#cwd` text input keeps working
  as pure free text, same as today.
- A favorited path whose repo has since been deleted/renamed on disk: it can
  still be starred/shown (no validation against current `/api/folders` data
  is required at render time beyond checking whether it's still present — if
  absent, silently drop it from the favorites row render since it no longer
  matches anything in the flattened list). It stays in `localStorage` either
  way; no explicit cleanup needed for this small-scale tool.
- Duplicate repo names across the two roots (e.g. a `foo` under both `~/git`
  and `~/dev`): full path is the identity used for favorites and for filling
  `#cwd`, so this is unambiguous; the root label shown alongside the name in
  the dropdown disambiguates visually.

## Testing

This is a static-file, no-build-step frontend (plain `<script>` in
`index.html`, no test harness currently in the repo). Verification is manual:
run `claudio`, confirm favorites row/dropdown behavior against the cases
above (empty favorites, star/unstar, fuzzy match ranking, duplicate names
across roots, `/api/folders` failure). No new automated tests are proposed,
consistent with the rest of this codebase.
