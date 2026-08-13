// Standalone verification of the config re-key gate (copied verbatim from
// app.js): a config message for a DIFFERENT session arriving late — an
// interleaved attach reply from a previous pane switch (the full config lags
// a running turn or its tokenization) — must NOT flip the active pane's id.
// If it did, paneForMessage would drop the history/config/context that
// follow for the pane's real session and the transcript would appear never
// to load. Re-keying is only allowed when the pane has no id yet (startup) or
// a session change is in flight (typed /new, /resume, /fork, resend).
// Run: node scripts/test_config_rekey_gate.js
'use strict';

// ── minimal model of the app.js flow (fixed version) ──
function makeEnv() {
  const panes = new Map();
  let nextPaneKey = 0;
  let activePaneKey = 0;
  let pendingSessionResponse = false; // module var = active pane's mirror
  const detaches = [];

  function makePane(id) {
    const pane = { key: nextPaneKey++, id: id || null, label: '', turnActive: false, pendingSessionResponse: false };
    panes.set(pane.key, pane);
    return pane;
  }
  function activePane() { return panes.get(activePaneKey); }
  function findPaneBySession(id) {
    if (!id) return null;
    for (const pane of panes.values()) if (pane.id === id) return pane;
    return null;
  }

  // paneForMessage as in app.js.
  function paneForMessage(data) {
    const sid = data.sessionId;
    if (sid) {
      const p = findPaneBySession(sid);
      if (p) return p;
      const act = activePane();
      if (act && data.type === 'response' && data.sessionAction) return act;
      if (act && (act.id === null || act.id === sid || pendingSessionResponse)) return act;
      return null;
    }
    return activePane();
  }

  // config handler as fixed in app.js (re-key gate).
  function configHandler(data) {
    const pane = activePane();
    const oldId = pane.id;
    if (data.sessionId && pane.id !== data.sessionId) {
      if (pane.id === null || pendingSessionResponse) {
        pane.id = data.sessionId;
        if (oldId) detaches.push(oldId);
      }
    }
    if (data.sessionLabel) pane.label = data.sessionLabel;
  }

  function switchTo(key) { activePaneKey = key; }

  return { makePane, activePane, paneForMessage, configHandler, switchTo, detaches, get pending() { return pendingSessionResponse; }, set pending(v) { pendingSessionResponse = v; } };
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

// 1. The reported bug: switch B→A, then a STALE full config for B (the pane
//    we just left; its full config lagged) arrives late. It must not flip the
//    active pane A back to B — otherwise history(A) that follows is dropped.
{
  const env = makeEnv();
  const paneB = env.makePane('SESS-B');
  env.makePane('SESS-A');
  env.switchTo(0); // pane B active
  env.switchTo(1); // switch to A (activePaneKey=1)
  env.configHandler({ type: 'config', sessionId: 'SESS-B' }); // stale config for B
  assert(env.activePane().id === 'SESS-A',
    'stale config for a previously-switched-away session does not re-key the active pane (got ' + env.activePane().id + ')');
  assert(env.detaches.length === 0, 'no session_detach sent for the stale config');

  // The pane's own history now routes to it and is NOT dropped.
  const routed = env.paneForMessage({ type: 'history', sessionId: 'SESS-A' });
  assert(routed === env.activePane(), 'history for the real session still routes to the active pane');
}

// 2. Startup: pane with no id yet adopts the id from the connect config.
{
  const env = makeEnv();
  const p = env.makePane(null);
  env.switchTo(p.key);
  env.configHandler({ type: 'config', sessionId: 'SESS-X' });
  assert(p.id === 'SESS-X', 'startup config adopts the id on a null-id pane (got ' + p.id + ')');
}

// 3. Legit re-key: typed /new with a session change in flight re-keys the
//    pane and detaches the old session.
{
  const env = makeEnv();
  const p = env.makePane('SESS-OLD');
  env.switchTo(p.key);
  env.pending = true; // typed /new marks the change in flight
  env.configHandler({ type: 'config', sessionId: 'SESS-NEW' });
  assert(p.id === 'SESS-NEW', 'in-flight session change re-keys the pane (got ' + p.id + ')');
  assert(env.detaches.includes('SESS-OLD'), 'old session detached after re-key');
}

// 4. response handler already re-keyed (normal path): the follow-up config
//    matches and is a no-op.
{
  const env = makeEnv();
  const p = env.makePane('SESS-NEW'); // response handler already adopted it
  env.switchTo(p.key);
  env.configHandler({ type: 'config', sessionId: 'SESS-NEW', sessionLabel: 'labeled' });
  assert(p.id === 'SESS-NEW' && p.label === 'labeled', 'matching config is a no-op + label adopted');
}

if (failures > 0) {
  console.error(failures + ' test(s) failed');
  process.exit(1);
}
console.log('all config re-key gate tests passed');
