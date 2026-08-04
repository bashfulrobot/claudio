# Favorites + Fuzzy Folder Picker Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the always-visible wall of folder chips on the "New session" card with a short, user-curated favorites row plus an fzf-style fuzzy-search dropdown on the existing Folder text input.

**Architecture:** Single-file change to `static/index.html` (no server/Go changes, no new endpoints). The existing `/api/folders` response is flattened client-side into `{root, dir, path}` entries once on load. Favorites are a `Set<string>` of full paths persisted to `localStorage`. A small inline fzf-style subsequence scorer ranks entries as the user types into `#cwd`; results render in an absolutely-positioned dropdown under the input, each row with a star toggle.

**Tech Stack:** Vanilla JS (ES2017+, no build step, no framework), plain CSS in a `<style>` block, `localStorage`. No new dependencies.

**Design doc:** `docs/superpowers/specs/2026-08-03-favorites-fzf-picker-design.md`

**Testing approach:** This repo has zero test infrastructure for the frontend (single inline `<script>`, no build/test tooling, no `node`/`npm` in the dev environment used for this plan). Consistent with the design doc's own Testing section, verification here is manual: each task ends with exact browser steps and the exact expected result. Task 8 is a full end-to-end pass across the edge cases called out in the design doc.

---

### Task 1: CSS — favorites row, picker wrapper, fuzzy dropdown

**Files:**
- Modify: `static/index.html` (`<style>` block)

- [ ] **Step 1: Replace the chip-group styles with favorites-row + dropdown styles**

Find this block (currently lines 120-131):

```css
  .chip-group { margin-bottom: 12px; }
  .chip-group-label { font-size: 12px; color: var(--muted); margin-bottom: 6px; }
  .chips { display: flex; flex-wrap: wrap; gap: 8px; }
  .chip {
    background: var(--bg);
    border: 1px solid var(--border);
    border-radius: 999px;
    padding: 8px 14px;
    font-size: 14px;
    color: var(--text);
  }
  .chip.selected { background: var(--accent); color: white; border-color: var(--accent); }
```

Replace it with:

```css
  .favorites-row { display: flex; flex-wrap: wrap; gap: 8px; margin-bottom: 12px; }
  .chip {
    background: var(--bg);
    border: 1px solid var(--border);
    border-radius: 999px;
    padding: 8px 14px;
    font-size: 14px;
    color: var(--text);
  }
  .chip.selected { background: var(--accent); color: white; border-color: var(--accent); }
  .picker { position: relative; }
  .fuzzy-dropdown {
    display: none;
    position: absolute;
    top: 100%;
    left: 0;
    right: 0;
    margin-top: 4px;
    background: var(--card);
    border: 1px solid var(--border);
    border-radius: 10px;
    max-height: 260px;
    overflow-y: auto;
    z-index: 10;
    box-shadow: 0 4px 16px rgba(0, 0, 0, 0.15);
  }
  .fuzzy-dropdown.show { display: block; }
  .fuzzy-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
    padding: 10px 12px;
  }
  .fuzzy-row:not(:last-child) { border-bottom: 1px solid var(--border); }
  .fuzzy-row.highlighted { background: var(--bg); }
  .fuzzy-name { font-size: 15px; }
  .fuzzy-root { color: var(--muted); font-size: 12px; margin-left: 6px; }
  .fuzzy-star {
    background: none;
    border: none;
    font-size: 18px;
    line-height: 1;
    color: var(--muted);
    padding: 4px;
  }
  .fuzzy-star.active { color: #ffb300; }
```

- [ ] **Step 2: Manually verify no CSS parse errors**

Open `static/index.html` in a browser directly (`file://` path is fine for this check) or run `claudio` and load the page (see Task 8 for the run command). Open DevTools console: confirm there are no CSS-related errors and the page still renders the header and "Running" section normally (the New Session card will look broken until Task 2 lands — that's expected).

- [ ] **Step 3: Commit**

```bash
git add static/index.html
git commit -m "style(claudio): add favorites row and fuzzy dropdown styles"
```

---

### Task 2: HTML — swap folder-groups markup for favorites row + picker

**Files:**
- Modify: `static/index.html` (New session card markup)

- [ ] **Step 1: Replace the folder-groups div with the new structure**

Find (currently lines 188-198):

```html
  <section>
    <h2>New session</h2>
    <div class="card">
      <div id="folder-groups"></div>
      <label for="cwd">Folder</label>
      <input id="cwd" type="text" placeholder="/home/you/git/some-project" autocapitalize="off" autocorrect="off" spellcheck="false">
      <label for="name">Name</label>
      <input id="name" type="text" placeholder="session name" autocapitalize="off" autocorrect="off" spellcheck="false">
      <button id="spawn-btn" class="spawn-btn">Spawn Session</button>
    </div>
  </section>
```

Replace with:

```html
  <section>
    <h2>New session</h2>
    <div class="card">
      <div id="favorites-row" class="favorites-row"></div>
      <label for="cwd">Folder</label>
      <div class="picker">
        <input id="cwd" type="text" placeholder="/home/you/git/some-project" autocapitalize="off" autocorrect="off" spellcheck="false" autocomplete="off">
        <div id="fuzzy-dropdown" class="fuzzy-dropdown"></div>
      </div>
      <label for="name">Name</label>
      <input id="name" type="text" placeholder="session name" autocapitalize="off" autocorrect="off" spellcheck="false">
      <button id="spawn-btn" class="spawn-btn">Spawn Session</button>
    </div>
  </section>
```

(`autocomplete="off"` is added to `#cwd` so the browser's own native autofill suggestions don't visually collide with the new fuzzy dropdown.)

- [ ] **Step 2: Manually verify markup**

Reload the page. DevTools console will show a JS error referencing `folder-groups` (the script still queries the old id) — expected until Task 3. Confirm in the Elements panel that `#favorites-row`, `.picker`, `#cwd`, and `#fuzzy-dropdown` exist and are nested as written above.

- [ ] **Step 3: Commit**

```bash
git add static/index.html
git commit -m "feat(claudio): add favorites row and fuzzy dropdown markup"
```

---

### Task 3: JS — favorites storage + flatten + fuzzy scoring (pure functions)

**Files:**
- Modify: `static/index.html` (`<script>` block)

- [ ] **Step 1: Update element references**

Find (currently lines 203-208):

```js
const sessionsEl = document.getElementById('sessions');
const foldersEl = document.getElementById('folder-groups');
const cwdInput = document.getElementById('cwd');
const nameInput = document.getElementById('name');
const spawnBtn = document.getElementById('spawn-btn');
const toastEl = document.getElementById('toast');
```

Replace with:

```js
const sessionsEl = document.getElementById('sessions');
const favoritesRowEl = document.getElementById('favorites-row');
const fuzzyDropdownEl = document.getElementById('fuzzy-dropdown');
const cwdInput = document.getElementById('cwd');
const nameInput = document.getElementById('name');
const spawnBtn = document.getElementById('spawn-btn');
const toastEl = document.getElementById('toast');

const FAVORITES_KEY = 'claudio.favorites';
let allEntries = [];       // flattened {root, dir, path} across all configured roots
let favorites = new Set(); // full paths the user has starred, populated at init (see bottom of script)
let currentResults = [];   // entries currently rendered in the fuzzy dropdown
let highlightedIndex = -1; // keyboard-selected row within currentResults, -1 = none
```

- [ ] **Step 2: Add favorites storage helpers**

Add this immediately after the `basename()` function (currently lines 231-234):

```js
function loadFavorites() {
  try {
    const raw = localStorage.getItem(FAVORITES_KEY);
    const arr = raw ? JSON.parse(raw) : [];
    return new Set(Array.isArray(arr) ? arr : []);
  } catch (e) {
    return new Set();
  }
}

function saveFavorites() {
  localStorage.setItem(FAVORITES_KEY, JSON.stringify([...favorites]));
}

function toggleFavorite(path) {
  if (favorites.has(path)) {
    favorites.delete(path);
  } else {
    favorites.add(path);
  }
  saveFavorites();
  renderFavoritesRow();
}
```

- [ ] **Step 3: Add `flattenGroups()` and `fuzzyScore()`**

Add directly after the helpers from Step 2:

```js
function flattenGroups(groups) {
  const entries = [];
  for (const g of (groups || [])) {
    if (!g.dirs || !g.dirs.length) continue;
    const root = g.root.replace(/\/$/, '');
    for (const d of g.dirs) {
      entries.push({ root, dir: d, path: root + '/' + d });
    }
  }
  return entries;
}

// Classic fzf-style subsequence match: every character of `query` must
// appear in `target`, in order, but not necessarily contiguous. Returns a
// score (higher = better match) or null if `target` doesn't match at all.
// Contiguous runs and matches near the start of the string score higher.
function fuzzyScore(query, target) {
  const q = query.toLowerCase();
  const t = target.toLowerCase();
  if (!q) return 0;
  let qi = 0;
  let score = 0;
  let streak = 0;
  for (let ti = 0; ti < t.length && qi < q.length; ti++) {
    if (t[ti] === q[qi]) {
      streak += 1;
      score += 1 + streak;
      if (ti === 0) score += 3;
      qi += 1;
    } else {
      streak = 0;
    }
  }
  if (qi < q.length) return null; // not every query character matched
  score -= t.length * 0.01;       // mild preference for shorter names
  return score;
}
```

- [ ] **Step 4: Manually verify the pure functions in the browser console**

Load the page (errors about `renderFavoritesRow`/old `loadFolders` code are still expected — later tasks fix those). In DevTools console, run:

```js
flattenGroups([{root: '/home/dustin/git/', dirs: ['claudio', 'hyprflake']}])
```
Expected: `[{root: '/home/dustin/git', dir: 'claudio', path: '/home/dustin/git/claudio'}, {root: '/home/dustin/git', dir: 'hyprflake', path: '/home/dustin/git/hyprflake'}]`

```js
fuzzyScore('cld', 'claudio')
```
Expected: a positive number (not `null`) — `c`, `l`, `d` all appear in order in "claudio".

```js
fuzzyScore('xyz', 'claudio')
```
Expected: `null` — no match.

```js
fuzzyScore('claudio', 'claudio') > fuzzyScore('cld', 'claudio')
```
Expected: `true` — an exact/full match scores higher than a sparse subsequence match.

- [ ] **Step 5: Commit**

```bash
git add static/index.html
git commit -m "feat(claudio): add favorites storage and fuzzy scoring helpers"
```

---

### Task 4: JS — render favorites row, wire selection, update `loadFolders()`

**Files:**
- Modify: `static/index.html` (`<script>` block)

- [ ] **Step 1: Add `renderFavoritesRow()` and `selectEntry()`**

Add after the `fuzzyScore()` function from Task 3:

```js
function renderFavoritesRow() {
  favoritesRowEl.textContent = '';
  const favEntries = allEntries.filter(e => favorites.has(e.path));
  for (const entry of favEntries) {
    const chip = el('button', 'chip', entry.dir);
    chip.type = 'button';
    chip.addEventListener('click', () => selectEntry(entry, chip));
    favoritesRowEl.appendChild(chip);
  }
}

function selectEntry(entry, chipEl) {
  favoritesRowEl.querySelectorAll('.chip.selected').forEach(c => c.classList.remove('selected'));
  if (chipEl) chipEl.classList.add('selected');
  cwdInput.value = entry.path;
  if (!nameInput.value) nameInput.value = entry.dir;
  closeDropdown();
}
```

(`closeDropdown()` doesn't exist yet — it's added in Task 5. This will produce a `ReferenceError` if `selectEntry` is actually invoked before Task 5 lands, but nothing calls it yet, so the page still loads fine.)

- [ ] **Step 2: Replace `loadFolders()`**

Find the existing function (currently lines 297-326):

```js
async function loadFolders() {
  try {
    const res = await fetch(`${API_BASE}/api/folders`);
    if (!res.ok) return;
    const groups = await res.json();
    foldersEl.textContent = '';
    for (const g of (groups || [])) {
      if (!g.dirs || !g.dirs.length) continue;
      const wrap = el('div', 'chip-group');
      wrap.appendChild(el('div', 'chip-group-label', g.root));
      const chips = el('div', 'chips');
      for (const d of g.dirs) {
        const chip = el('button', 'chip', d);
        chip.type = 'button';
        chip.addEventListener('click', () => {
          foldersEl.querySelectorAll('.chip.selected').forEach(c => c.classList.remove('selected'));
          chip.classList.add('selected');
          const path = g.root.replace(/\/$/, '') + '/' + d;
          cwdInput.value = path;
          if (!nameInput.value) nameInput.value = d;
        });
        chips.appendChild(chip);
      }
      wrap.appendChild(chips);
      foldersEl.appendChild(wrap);
    }
  } catch (e) {
    // folder picker is a convenience; failing silently is fine, free-text still works
  }
}
```

Replace with:

```js
async function loadFolders() {
  try {
    const res = await fetch(`${API_BASE}/api/folders`);
    if (!res.ok) return;
    const groups = await res.json();
    allEntries = flattenGroups(groups);
    renderFavoritesRow();
  } catch (e) {
    // folder picker is a convenience; failing silently is fine, free-text still works
  }
}
```

- [ ] **Step 3: Manually verify favorites render (with a temporary seeded favorite)**

Reload the page. In DevTools console, seed a favorite and re-render without waiting for the star UI (that lands in Task 5):

```js
favorites.add(allEntries[0].path);
renderFavoritesRow();
```

Expected: one chip appears in the favorites row (above the "Folder" label) showing `allEntries[0].dir`. Click it — expected: `#cwd` fills with the full path and `#name` fills with the dir name (both visible in the form fields). Note: clicking will throw in the console because `closeDropdown` isn't defined until Task 5 — confirm the *fields still filled correctly* despite the console error (the assignments happen before the throwing line), then run `favorites.delete(allEntries[0].path); renderFavoritesRow();` to reset state before moving on.

- [ ] **Step 4: Commit**

```bash
git add static/index.html
git commit -m "feat(claudio): render favorites row from flattened folder data"
```

---

### Task 5: JS — fuzzy dropdown rendering, star toggle, open/close

**Files:**
- Modify: `static/index.html` (`<script>` block)

- [ ] **Step 1: Add `computeResults()`, `renderDropdown()`, `openDropdown()`, `closeDropdown()`**

Add after `selectEntry()` from Task 4:

```js
function computeResults(query) {
  if (!query) {
    const favs = allEntries.filter(e => favorites.has(e.path));
    const rest = allEntries
      .filter(e => !favorites.has(e.path))
      .slice()
      .sort((a, b) => a.dir.localeCompare(b.dir));
    return favs.concat(rest).slice(0, 10);
  }
  const scored = [];
  for (const entry of allEntries) {
    const score = fuzzyScore(query, entry.dir);
    if (score !== null) scored.push({ entry, score });
  }
  scored.sort((a, b) => b.score - a.score);
  return scored.slice(0, 10).map(s => s.entry);
}

function renderDropdown(query) {
  currentResults = computeResults(query);
  highlightedIndex = -1;
  fuzzyDropdownEl.textContent = '';
  for (const entry of currentResults) {
    const row = el('div', 'fuzzy-row');

    const name = el('span', 'fuzzy-name', entry.dir);
    name.appendChild(el('span', 'fuzzy-root', entry.root));

    const isFav = favorites.has(entry.path);
    const star = el('button', 'fuzzy-star' + (isFav ? ' active' : ''), isFav ? '★' : '☆');
    star.type = 'button';
    star.addEventListener('mousedown', (e) => {
      e.preventDefault(); // don't let the input blur before the click registers
      toggleFavorite(entry.path);
      renderDropdown(cwdInput.value.trim());
    });

    row.addEventListener('mousedown', (e) => {
      if (e.target === star) return;
      e.preventDefault();
      selectEntry(entry, null);
    });

    row.appendChild(name);
    row.appendChild(star);
    fuzzyDropdownEl.appendChild(row);
  }
  fuzzyDropdownEl.classList.toggle('show', currentResults.length > 0);
}

function openDropdown() {
  renderDropdown(cwdInput.value.trim());
}

function closeDropdown() {
  fuzzyDropdownEl.classList.remove('show');
  fuzzyDropdownEl.textContent = '';
  currentResults = [];
  highlightedIndex = -1;
}
```

Note: `★` / `☆` above are literal UTF-8 characters (filled/outline star) — `static/index.html` already declares `<meta charset="utf-8">`, so paste them as-is; no escape sequence needed.

- [ ] **Step 2: Manually verify the dropdown renders and stars toggle**

Reload the page. In DevTools console:

```js
openDropdown();
```

Expected: the dropdown appears below the Folder input showing up to 10 rows (favorites first if any, then the rest alphabetically), each with a name, root label, and an outlined star (☆) unless already favorited.

Click a star on any row. Expected: the star switches to filled (★) and turns gold/amber, and `[...favorites]` in the console now includes that entry's `path`. Click it again: star reverts to outline and the path is removed from `favorites`.

Click anywhere on a row other than the star. Expected: `#cwd` and `#name` fill in with that entry's data, and the dropdown closes (`fuzzy-dropdown` loses its `show` class).

- [ ] **Step 3: Commit**

```bash
git add static/index.html
git commit -m "feat(claudio): render fuzzy dropdown with favorite star toggle"
```

---

### Task 6: JS — wire input/focus/blur/keyboard on `#cwd`

**Files:**
- Modify: `static/index.html` (`<script>` block)

- [ ] **Step 1: Add keyboard navigation**

Add after `closeDropdown()` from Task 5:

```js
function updateHighlight() {
  const rows = fuzzyDropdownEl.querySelectorAll('.fuzzy-row');
  rows.forEach((r, i) => r.classList.toggle('highlighted', i === highlightedIndex));
}

cwdInput.addEventListener('keydown', (e) => {
  if (!fuzzyDropdownEl.classList.contains('show')) return;
  if (e.key === 'ArrowDown') {
    e.preventDefault();
    highlightedIndex = Math.min(highlightedIndex + 1, currentResults.length - 1);
    updateHighlight();
  } else if (e.key === 'ArrowUp') {
    e.preventDefault();
    highlightedIndex = Math.max(highlightedIndex - 1, 0);
    updateHighlight();
  } else if (e.key === 'Enter') {
    if (highlightedIndex >= 0 && currentResults[highlightedIndex]) {
      e.preventDefault();
      selectEntry(currentResults[highlightedIndex], null);
    }
  } else if (e.key === 'Escape') {
    closeDropdown();
  }
});
```

- [ ] **Step 2: Replace the old `cwdInput` input listener and add focus/blur**

Find (currently lines 328-332):

```js
cwdInput.addEventListener('input', () => {
  if (!nameInput.value && cwdInput.value) {
    nameInput.value = basename(cwdInput.value);
  }
});
```

Replace with:

```js
cwdInput.addEventListener('input', () => {
  if (!nameInput.value && cwdInput.value) {
    nameInput.value = basename(cwdInput.value);
  }
  renderDropdown(cwdInput.value.trim());
});

cwdInput.addEventListener('focus', openDropdown);

cwdInput.addEventListener('blur', () => {
  // Delay so a row/star mousedown's own handler runs before we tear the
  // dropdown down (mousedown fires before blur's default focus-loss).
  setTimeout(closeDropdown, 150);
});
```

- [ ] **Step 3: Manually verify typing, keyboard nav, and blur behavior**

Reload the page (all functions now defined, no more console errors expected on interaction). Tap/click into the Folder field:

Expected: dropdown opens immediately showing favorites-first-then-alphabetical, even with an empty field.

Type a few characters of a known repo name (e.g. `cla` if `claudio` is one of your repos):

Expected: dropdown re-filters to fuzzy matches for `cla`, ranked with better matches first.

Press ArrowDown twice, then Enter:

Expected: the third row's entry (index 2) fills `#cwd`/`#name` and the dropdown closes.

Click into the field again, then click elsewhere on the page (not a dropdown row):

Expected: dropdown closes (after the ~150ms delay).

Click into the field, then click a star on one of the rows:

Expected: dropdown stays open (does not close from the implicit blur), star toggles, row list re-renders.

- [ ] **Step 4: Commit**

```bash
git add static/index.html
git commit -m "feat(claudio): wire fuzzy dropdown open/close and keyboard nav"
```

---

### Task 7: JS — fix `spawnBtn` reset to clear favorites-row selection

**Files:**
- Modify: `static/index.html` (`<script>` block)

- [ ] **Step 1: Update the post-spawn reset**

Find, inside the `spawnBtn` click handler (currently around line 351):

```js
    foldersEl.querySelectorAll('.chip.selected').forEach(c => c.classList.remove('selected'));
```

Replace with:

```js
    favoritesRowEl.querySelectorAll('.chip.selected').forEach(c => c.classList.remove('selected'));
```

- [ ] **Step 2: Manually verify**

Search the whole `<script>` block for the string `foldersEl` — expected: zero remaining occurrences (the old element and all its references are fully removed). Reload the page, select a favorite chip, spawn a session (or trigger the failure path by stopping the `claudio` server first and confirming the toast error still shows), and confirm no console errors reference `foldersEl`.

- [ ] **Step 3: Commit**

```bash
git add static/index.html
git commit -m "fix(claudio): clear favorites-row selection after spawn"
```

---

### Task 8: End-to-end manual verification against the design doc's edge cases

**Files:** none (verification only)

- [ ] **Step 1: Build and run claudio**

```bash
go build -o /tmp/claudio . && /tmp/claudio
```

Expected: log line `claudio listening on :PORT (roots: [/home/dustin/... /home/dustin/...])` (or whatever `-addr`/`-roots` defaults resolve to on this machine). Open the printed address in a browser.

- [ ] **Step 2: Empty favorites state**

With `localStorage` cleared for the page (DevTools → Application → Local Storage → clear, or `localStorage.clear()` in console + reload): confirm the favorites row renders nothing and takes up no visible space above the "Folder" label — no placeholder text, no empty gap.

- [ ] **Step 3: Star/unstar round-trips through a reload**

Star two repos from the dropdown. Reload the page. Expected: both still appear in the favorites row (persisted via `localStorage`) and still show filled stars (★) when they appear in dropdown results.

- [ ] **Step 4: Fuzzy ranking sanity check**

Type a query that's a subsequence of one repo name but a prefix of another (e.g. if you have both `claudio` and `claude-demo`, type `clad`). Expected: both appear (subsequence matches both), ordered by score — closer/more-contiguous matches rank first per `fuzzyScore`'s scoring.

- [ ] **Step 5: Duplicate directory names across roots**

If `~/git` and `~/dev` happen to share no duplicate names, temporarily create one: `mkdir -p ~/dev/claudio-dup-test && mkdir -p ~/git/claudio-dup-test` (or reuse any existing same-named pair). Confirm the dropdown shows both as separate rows, each with a distinct `.fuzzy-root` label so they're visually disambiguated, and that starring one does not star the other (favorites key on full `path`, not `dir`). Clean up the test directories afterward: `rmdir ~/dev/claudio-dup-test ~/git/claudio-dup-test`.

- [ ] **Step 6: `/api/folders` failure path**

Stop the `claudio` process (or block the request via DevTools → Network → offline, then reload). Expected: favorites row stays empty and the dropdown never populates, but `#cwd` still works as a plain free-text field, and manually typing a path and clicking "Spawn Session" still attempts the spawn (fails gracefully with a toast if the server is actually down, per existing `spawnBtn` error handling — unchanged by this plan).

- [ ] **Step 7: Update the design doc status line**

Edit `docs/superpowers/specs/2026-08-03-favorites-fzf-picker-design.md`, change:

```
Status: Approved, ready for implementation plan
```

to:

```
Status: Implemented
```

- [ ] **Step 8: Final commit**

```bash
git add docs/superpowers/specs/2026-08-03-favorites-fzf-picker-design.md
git commit -m "docs(claudio): mark favorites/fuzzy picker spec as implemented"
```

---

## Self-Review Notes

- **Spec coverage:** favorites row (Task 4), fuzzy dropdown + scoring (Tasks 3/5), star toggle (Task 5), keyboard nav (Task 6), removed chip wall (Tasks 1-2), edge cases — empty favorites, stale-on-disk favorite, duplicate names, `/api/folders` failure (Task 8) — all covered. No server/API changes, matching the spec's non-goals.
- **Type/name consistency:** `entry = {root, dir, path}` is the single shape used everywhere (`flattenGroups`, `renderFavoritesRow`, `computeResults`, `renderDropdown`, `selectEntry`) — no divergent field names across tasks.
- **Placeholder scan:** no TBD/TODO; every step shows exact code or exact verification commands and expected output.
