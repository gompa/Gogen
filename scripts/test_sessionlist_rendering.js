// Standalone verification of the sidebar session-list rendering (copied
// verbatim from app.js renderSessionList): the sidebar has ONE session list
// (no separate "Open panes" section). Sessions open as panes and saved
// sessions share it; the focused pane's session is marked "current" (the
// active indicator) and every pane state shows a uniform colored-dot
// indicator in the meta (active=green, responding=amber, creating=gray,
// open=blue, resume-to-continue=violet).
// Run: node scripts/test_sessionlist_rendering.js
'use strict';

// ── copied verbatim from app.js (renderSessionList) ──
function sessionRowClassAndMeta(s, panes, activePaneKey, explicitPane) {
  const findPaneBySession = (id) => {
    if (!id) return null;
    for (const pane of panes.values()) if (pane.id === id) return pane;
    return null;
  };
  const act = panes.get(activePaneKey);
  const pane = explicitPane !== undefined ? explicitPane : findPaneBySession(s.id);
  const isActivePane = !!pane && pane === act;
  const rowClass = 'session-row'
    + (isActivePane ? ' current' : '')
    + (pane && pane.turnActive ? ' busy' : '');
  // Uniform colored-dot indicator per state (app.js builds a
  // .session-state-dot span whose class carries the state color, next to a
  // muted label). The model records the dot class + the label text.
  const parts = [];
  let stateDot = '';
  if (s.messageCount != null) parts.push(`${s.messageCount} msgs`);
  if (pane && pane.turnActive) {
    parts.push('responding'); stateDot = 'amber';
  } else if (pane && !pane.id) {
    parts.push('creating…'); stateDot = 'gray';
  } else if (isActivePane) {
    parts.push('active'); stateDot = 'green';
  } else if (pane) {
    // Background pane: uniform indicator (dot + "open" label).
    parts.push('open'); stateDot = 'blue';
  } else if (s.active) {
    parts.push('resume to continue');
  }
  return { rowClass, meta: parts.join(' · '), stateDot };
}

// ── copied verbatim from app.js (renderSessionList's merge) ──
// Builds the row list for one render: open panes (client state) merged in
// FIRST so an open session's row always exists (even before the server's
// store lists it), then saved sessions in server order.
function mergedRows(panes, sessions, activePaneKey, nestedParentIds) {
  const list = sessions || [];
  const rows = [];
  const openIds = new Set();
  for (const pane of panes.values()) {
    const entry = pane.id ? (list.find((s) => s.id === pane.id) || null) : null;
    if (pane.id) openIds.add(pane.id);
    rows.push({ pane, entry });
  }
  for (const s of list) {
    // Nested (subagent) rows render under their parent (appendNestedRows
    // in app.js), never as flat rows.
    if (s.parentId) continue;
    if (!openIds.has(s.id)) rows.push({ pane: null, entry: s });
  }
  // A nested (subagent) child open as a pane renders under its parent, not
  // as a flat open-pane row; it falls back to a flat row only when the
  // parent's row is missing from this render. nestedParentIds models the
  // live subagent_started/finished records (childId -> parentId).
  const rowIds = new Set();
  for (const r of rows) rowIds.add(r.pane ? r.pane.id : (r.entry ? r.entry.id : ''));
  const flatRows = rows.filter((r) => {
    const id = r.pane ? r.pane.id : (r.entry ? r.entry.id : '');
    if (!r.pane || !id) return true;
    const entry = list.find((s) => s.id === id);
    const parentId = (nestedParentIds && nestedParentIds[id]) || (entry && entry.parentId) || '';
    return !parentId || !rowIds.has(parentId);
  });
  return flatRows.map(({ pane, entry }) => {
    const s = entry || {
      id: pane ? pane.id : '',
      label: pane ? pane.label : '',
      messageCount: null,
      active: true,
      updatedAt: '',
    };
    const label = pane
      ? (pane.label || (entry && entry.label) || pane.id || 'New session…')
      : (s.label || s.id || '(unknown)');
    const { rowClass, meta, stateDot } = sessionRowClassAndMeta(s, panes, activePaneKey, pane);
    return { id: pane ? pane.id : (entry ? entry.id : ''), title: label, rowClass, meta, stateDot };
  });
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

// Startup with a restored latest session: the session open as the ACTIVE
// pane appears in the list, marked "current" + the green active indicator.
{
  const panes = new Map();
  panes.set(0, { id: 'LATEST', label: 'My latest session', turnActive: false });
  const sessions = [
    { id: 'LATEST', messageCount: 12 },
    { id: 'OLD1', messageCount: 3 },
    { id: 'OLD2', messageCount: 7 },
  ];
  const r = sessionRowClassAndMeta(sessions.find((s) => s.id === 'LATEST'), panes, 0);
  assert(r.rowClass.includes('current') && !r.rowClass.includes('busy'),
    'startup: active open session row is marked current, got "' + r.rowClass + '"');
  assert(r.meta.includes('active') && r.stateDot === 'green',
    'startup: active session meta has the green active indicator, got "' + r.meta + '" dot=' + r.stateDot);
}

// The whole list is rendered (open panes are NOT excluded): all sessions
// appear, each with its own state.
{
  const panes = new Map();
  panes.set(0, { id: 'LATEST', label: '', turnActive: false }); // active pane
  const sessions = [
    { id: 'LATEST', messageCount: 12 },
    { id: 'OLD1', messageCount: 3 },
  ];
  const r1 = sessionRowClassAndMeta(sessions[0], panes, 0);
  const r2 = sessionRowClassAndMeta(sessions[1], panes, 0);
  assert(r1.rowClass.includes('current'), 'open pane row is marked current in the unified list');
  assert(r2.rowClass === 'session-row' && !r2.rowClass.includes('current'),
    'saved row (not open) is not marked current, got "' + r2.rowClass + '"');
  assert(r2.meta.includes('3 msgs'), 'saved row still shows its message count, got "' + r2.meta + '"');
}

// Multiple open panes: the focused one is current/active, a busy background
// pane shows the amber responding indicator, an idle background pane shows
// the blue open indicator, and a non-open live session (another tab) shows
// "resume to continue" with the violet indicator.
{
  const panes = new Map();
  panes.set(0, { id: 'A', turnActive: true }); // background, running turn
  panes.set(1, { id: 'B', turnActive: false }); // active, idle
  panes.set(2, { id: null }); // pane awaiting its session id
  const sessions = [
    { id: 'A', messageCount: 5 },
    { id: 'B', messageCount: 2 },
    { id: 'C', messageCount: 1, active: true }, // live elsewhere (another tab)
  ];
  const a = sessionRowClassAndMeta(sessions[0], panes, 1);
  const b = sessionRowClassAndMeta(sessions[1], panes, 1);
  const c = sessionRowClassAndMeta(sessions[2], panes, 1);
  assert(a.rowClass.includes('busy') && a.meta.includes('responding') && a.stateDot === 'amber',
    'busy background pane row: busy + amber responding indicator, got "' + a.rowClass + '" / "' + a.meta + '" dot=' + a.stateDot);
  assert(b.rowClass.includes('current') && b.meta.includes('active') && b.stateDot === 'green',
    'focused pane row: current + green active indicator, got "' + b.rowClass + '" / "' + b.meta + '" dot=' + b.stateDot);
  assert(!c.rowClass.includes('current') && c.meta.includes('resume to continue'),
    'non-open live session: resume to continue, got "' + c.rowClass + '" / "' + c.meta + '"');
}

// All sessions open as panes: every row still appears (no empty list), and
// an idle background pane shows a dot indicator (no "open" word).
{
  const panes = new Map();
  panes.set(0, { id: 'A', turnActive: false });
  panes.set(1, { id: 'B', turnActive: false });
  const a = sessionRowClassAndMeta({ id: 'A', messageCount: 4 }, panes, 0);
  const b = sessionRowClassAndMeta({ id: 'B', messageCount: 6 }, panes, 0);
  assert(a.meta.includes('active') && a.stateDot === 'green',
    'active pane A shows the green active indicator, got "' + a.meta + '" dot=' + a.stateDot);
  assert(b.stateDot === 'blue' && b.meta.includes('open'),
    'idle background pane B shows the blue open indicator, got "' + b.meta + '" dot=' + b.stateDot);
}

// Merge: an open pane's row ALWAYS exists — even when the server list is
// empty (fresh workspace / list lag) — with the LIVE pane label.
{
  const panes = new Map();
  panes.set(0, { id: 'FRESH', label: 'my first message', turnActive: false }); // active pane
  const rows = mergedRows(panes, [], 0);
  assert(rows.length === 1 && rows[0].title === 'my first message',
    'merge: open pane renders from client state with its live label, got ' + JSON.stringify(rows));
  assert(rows[0].rowClass.includes('current') && rows[0].meta.includes('active') && rows[0].stateDot === 'green',
    'merge: open-pane row is current + green active indicator, got "' + rows[0].rowClass + '" / "' + rows[0].meta + '" dot=' + rows[0].stateDot);
}

// Merge: an open pane not yet in the server list falls back to its id; a
// creating pane (no id yet) shows the "New session…/creating…" placeholder.
{
  const panes = new Map();
  panes.set(0, { id: 'A', label: '', turnActive: false }); // background, no label yet
  panes.set(1, { id: null, label: '', turnActive: false }); // creating (active)
  const rows = mergedRows(panes, [], 1);
  assert(rows.length === 2
      && rows[0].title === 'A'
      && rows[1].title === 'New session…' && rows[1].meta.includes('creating…') && rows[1].stateDot === 'gray',
    'merge: id fallback + creating placeholder with gray indicator, got ' + JSON.stringify(rows));
}

// Merge: open panes first (client state), then saved sessions in server
// order; the open pane's live label overrides the stale server label.
{
  const panes = new Map();
  panes.set(0, { id: 'A', label: 'live label', turnActive: false }); // active pane
  const sessions = [
    { id: 'A', label: 'stale label', messageCount: 3 },
    { id: 'B', label: 'saved B', messageCount: 7 },
  ];
  const rows = mergedRows(panes, sessions, 0);
  assert(rows.length === 2
      && rows[0].title === 'live label'
      && rows[1].title === 'saved B',
    'merge: live pane label wins over server label, saved rows follow, got ' + JSON.stringify(rows));
}

// Nested (subagent) children: an open child stays under its parent — its
// flat open-pane row is skipped (appendNestedRows renders it under the
// parent instead), so opening a subagent does not make its row jump.
{
  const panes = new Map();
  panes.set(0, { id: 'CHILD', label: 'subagent: fix bug', turnActive: true }); // active pane
  const sessions = [
    { id: 'PARENT', label: 'parent session', messageCount: 4 },
    { id: 'CHILD', label: 'subagent: fix bug', messageCount: 1, parentId: 'PARENT' },
  ];
  const rows = mergedRows(panes, sessions, 0, { CHILD: 'PARENT' });
  assert(rows.length === 1 && rows[0].id === 'PARENT',
    'open nested child is not a flat row (stays under parent), got ' + JSON.stringify(rows));
}

// Nested child known only from the live event records (not yet persisted):
// same result via the nestedParentIds map.
{
  const panes = new Map();
  panes.set(0, { id: 'CHILD', label: 'subagent: fix bug', turnActive: false });
  const sessions = [{ id: 'PARENT', messageCount: 4 }];
  const rows = mergedRows(panes, sessions, 0, { CHILD: 'PARENT' });
  assert(rows.length === 1 && rows[0].id === 'PARENT',
    'live-only nested child is not a flat row, got ' + JSON.stringify(rows));
}

// Fallback: the child's parent row is missing from this render (fresh
// parent unknown to the store / another tab) — the child keeps its flat
// open-pane row so it never disappears from the sidebar.
{
  const panes = new Map();
  panes.set(0, { id: 'CHILD', label: 'subagent: fix bug', turnActive: false });
  const rows = mergedRows(panes, [], 0, { CHILD: 'PARENT' });
  assert(rows.length === 1 && rows[0].id === 'CHILD' && rows[0].title === 'subagent: fix bug',
    'nested child with missing parent row falls back to its flat row, got ' + JSON.stringify(rows));
}

// Saved nested children (not open) are excluded from the flat list: they
// render under their parent via appendNestedRows.
{
  const panes = new Map();
  panes.set(0, { id: 'PARENT', label: 'parent', turnActive: false });
  const sessions = [
    { id: 'PARENT', messageCount: 4 },
    { id: 'CHILD', messageCount: 1, parentId: 'PARENT' },
  ];
  const rows = mergedRows(panes, sessions, 0);
  assert(rows.length === 1 && rows[0].id === 'PARENT',
    'saved nested child is not a flat row, got ' + JSON.stringify(rows));
}

if (failures > 0) {
  console.error(failures + ' test(s) failed');
  process.exit(1);
}
console.log('all session-list rendering tests passed');
