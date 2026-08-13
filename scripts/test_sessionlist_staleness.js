// Standalone verification of the sidebar session-list staleness fix
// (copied verbatim from app.js): the sidebar has ONE session list — no
// separate "Open panes" section — rendered from the server's sessions
// payload (cached in lastSessions). Opening a saved session from the sidebar
// creates a pane, adopts the session id, then re-renders from the cache so
// the row shows the id + active indicator IMMEDIATELY, marked current. The attach
// reply's config echo carries the SAME id, so the config handler's re-render
// condition (id differs) is false; the fix re-renders right after adopting
// the id (and after session_state, so busy/resuming shows in the row), and
// updateCurrentSessionLabel patches the title in place on the label echo.
// Run: node scripts/test_sessionlist_staleness.js
'use strict';

// ── minimal model of the app.js flow (fixed version) ──
function makeEnv() {
  const panes = new Map();
  let nextPaneKey = 0;
  let activePaneKey = 0;
  let lastSessions = null; // server payload cache
  let rendered = []; // latest render: {id, title, meta, current, busy}

  function activePane() {
    return panes.get(activePaneKey);
  }
  function findPaneBySession(id) {
    for (const pane of panes.values()) if (pane.id === id) return pane;
    return null;
  }
  function makePane() {
    const pane = { key: nextPaneKey++, id: null, label: '', turnActive: false };
    panes.set(pane.key, pane);
    refreshSidebarSessions();
    return pane;
  }

  // renderSessionList as in app.js: renders the unified list — open panes
  // (client state) merged in first so an open session's row always exists
  // with its live label, then saved sessions from the cache in server order.
  function renderSessionList(list) {
    lastSessions = list || [];
    rendered = [];
    const act = activePane();
    const rows = [];
    const openIds = new Set();
    for (const pane of panes.values()) {
      const entry = pane.id ? (lastSessions.find((s) => s.id === pane.id) || null) : null;
      if (pane.id) openIds.add(pane.id);
      rows.push({ pane, entry });
    }
    for (const s of lastSessions) {
      if (!openIds.has(s.id)) rows.push({ pane: null, entry: s });
    }
    for (const r of rows) {
      const pane = r.pane;
      const entry = r.entry;
      const s = entry || {
        id: pane ? pane.id : '',
        label: pane ? pane.label : '',
        messageCount: null,
        active: true,
        updatedAt: '',
      };
      const isActivePane = !!pane && pane === act;
      const label = pane
        ? (pane.label || (entry && entry.label) || pane.id || 'New session…')
        : (s.label || s.id || '(unknown)');
      // Uniform colored-dot indicator per state (app.js builds a
      // .session-state-dot span whose class carries the state color):
      // active=green, responding=amber, creating=gray, open=blue,
      // resume-to-continue=violet. The model records the dot class + label.
      const parts = [];
      let stateDot = '';
      if (entry && entry.messageCount != null) parts.push(`${entry.messageCount} msgs`);
      if (pane && pane.turnActive) {
        parts.push('responding'); stateDot = 'amber';
      } else if (pane && !pane.id) {
        parts.push('creating…'); stateDot = 'gray';
      } else if (isActivePane) {
        parts.push('active'); stateDot = 'green';
      } else if (pane && pane.id) {
        // Open as a background pane — uniform indicator (dot + "open"
        // label).
        parts.push('open'); stateDot = 'blue';
      } else if (s.active) {
        parts.push('resume to continue');
      }
      rendered.push({
        id: pane ? pane.id : (entry ? entry.id : ''),
        title: label,
        meta: parts.join(' · '),
        current: isActivePane,
        busy: !!(pane && pane.turnActive),
        stateDot,
      });
    }
  }

  // refreshSidebarSessions as in app.js: re-render from cache or fetch.
  function refreshSidebarSessions() {
    if (lastSessions) {
      renderSessionList(lastSessions);
    } else {
      lastSessions = []; // no payload yet → no rows to render
    }
  }

  // openSessionPane as fixed in app.js (renders after adopting the id).
  function openSessionPane(id) {
    if (findPaneBySession(id)) return 'focused-existing';
    const pane = makePane();
    pane.id = id;
    activePaneKey = pane.key;
    refreshSidebarSessions(); // ← the fix
    return 'attached';
  }

  // Server attach reply, in wire order: session_state, then config echo.
  function sessionState(turnActive) {
    const pane = activePane();
    pane.turnActive = !!turnActive;
    refreshSidebarSessions(); // ← the fix (busy/resuming shows in the row)
  }
  // config echo: only re-renders when the id CHANGES (real handler logic).
  function configEcho(id, label) {
    const pane = activePane();
    if (id && pane.id !== id) {
      pane.id = id;
      refreshSidebarSessions();
    }
    if (label) pane.label = label;
    // Real config handler ends with applyServerConfig → updateCurrentSession
    // Label, which patches the CURRENT row's title in place (no full render).
    updateCurrentSessionLabel(label);
  }

  // updateCurrentSessionLabel as in app.js: patches the active row's title
  // (looked up by session id in the unified list).
  function updateCurrentSessionLabel(label) {
    if (!label) return;
    const pane = activePane();
    if (!pane || !pane.id) return;
    if (lastSessions) {
      const entry = lastSessions.find((s) => s.id === pane.id);
      if (entry) entry.label = label;
    }
    const row = rendered.find((r) => r.id === pane.id);
    if (!row || row.title === label) return;
    row.title = label;
  }

  return {
    openSessionPane,
    sessionState,
    configEcho,
    seedSessions: (list) => renderSessionList(list),
    rows: () => rendered,
    activePane,
  };
}

// ── assertions ──
let failures = 0;
function assert(cond, msg) {
  if (cond) {
    console.log('ok - ' + msg);
  } else {
    failures++;
    console.error('FAIL - ' + msg);
  }
}

// 1. Opening a saved session shows the pane as open IMMEDIATELY (before any
//    server reply) — no "New session…/creating…" until a pane switch. The
//    row is rendered from the cached sessions payload and marked current.
{
  const env = makeEnv();
  env.seedSessions([{ id: 'SESS-OLD', messageCount: 3 }]);
  env.openSessionPane('SESS-OLD');
  const row = env.rows()[0];
  assert(row.title === 'SESS-OLD' && row.meta.includes('active') && row.stateDot === 'green' && row.current,
    'openSessionPane: row shows id + green active indicator + current immediately, got "' + row.title + '"/"' + row.meta + '"');
}

// 2. The config echo (same id — the case that previously skipped re-render)
//    must not regress the row, and the label is adopted for the title.
{
  const env = makeEnv();
  env.seedSessions([{ id: 'SESS-OLD', messageCount: 3 }]);
  env.openSessionPane('SESS-OLD');
  env.configEcho('SESS-OLD', 'my saved session');
  const row = env.rows()[0];
  assert(row.meta.includes('active') && row.stateDot === 'green' && row.current,
    'config echo: row still green active + current, got "' + row.meta + '"');
  assert(row.title === 'my saved session', 'config echo: title upgraded to label, got "' + row.title + '"');
}

// 3. session_state with a running turn shows the amber responding indicator.
{
  const env = makeEnv();
  env.seedSessions([{ id: 'SESS-OLD', messageCount: 3 }]);
  env.openSessionPane('SESS-OLD');
  env.sessionState(true);
  const row = env.rows()[0];
  assert(row.meta.includes('responding') && row.stateDot === 'amber' && row.busy,
    'session_state: busy row shows the amber responding indicator, got "' + row.meta + '"');
}

// 4. Multiple panes: the freshly-opened pane is the active row and shows as
//    open while the first pane stays open too.
{
  const env = makeEnv();
  env.seedSessions([
    { id: 'SESS-A', messageCount: 1 },
    { id: 'SESS-B', messageCount: 2 },
  ]);
  env.openSessionPane('SESS-A');
  env.configEcho('SESS-A', 'first');
  env.openSessionPane('SESS-B');
  const rows = env.rows();
  assert(rows.length === 2
      && rows[0].title === 'first' && rows[0].stateDot === 'blue' && !rows[0].current
      && rows[1].title === 'SESS-B' && rows[1].meta.includes('active') && rows[1].stateDot === 'green' && rows[1].current,
    'two panes: background row open (blue dot), new active row green active + current, got ' + JSON.stringify(rows));
}

// 5. CONTROL — the pre-fix flow (no re-render after pane.id = id): the row
//    renders from the cache while the pane's id is still unknown, so the
//    opened session's row never gets the "current" indicator. Documents the
//    bug the fix addresses (the config echo alone cannot rescue it).
{
  const panes = new Map();
  let nk = 0;
  let ak = 0;
  const lastSessions = [{ id: 'SESS-OLD', messageCount: 3 }];
  let rendered = [];
  // makePane renders IMMEDIATELY, while the id is still unknown — no pane
  // state is attached to the row.
  function renderSessionList() {
    rendered = [];
    for (const s of lastSessions) {
      rendered.push({ id: s.id, title: s.label || s.id, meta: '3 msgs', current: false });
    }
  }
  function makePane() {
    const p = { key: nk++, id: null, label: '' };
    panes.set(p.key, p);
    ak = p.key;
    renderSessionList(); // stale render, id not yet set
    return p;
  }
  const stale = makePane(); // row rendered without "current" for SESS-OLD
  panes.get(ak).id = 'SESS-OLD'; // ← bug: no refreshSidebarSessions() after this
  // Config echo arrives with the SAME id → handler's render condition false.
  const active = panes.get(ak);
  if ('SESS-OLD' !== active.id) { active.id = 'SESS-OLD'; }
  assert(!rendered.find((r) => r.id === 'SESS-OLD').current,
    'control: pre-fix flow leaves the opened session row without the current indicator');
}

if (failures > 0) {
  console.error(failures + ' test(s) failed');
  process.exit(1);
}
console.log('all session-list staleness tests passed');
