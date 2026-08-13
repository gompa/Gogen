// Standalone verification of the multi-pane routing state machine copied
// verbatim from app.js: paneForMessage + handleBackgroundMessage.
// Run: node scripts/test_panerouting.js
'use strict';

// ── copied verbatim from app.js ──
function makePanesEnv() {
  const panes = new Map();
  let nextPaneKey = 0;
  let activePaneKey = 0;
  let pendingSessionResponse = false; // module var = active pane's mirror
  let notifications = [];

  function makePane() {
    const pane = {
      key: nextPaneKey++,
      id: null,
      label: '',
      turnActive: false,
      suppressTurnEnds: 0,
      pendingSessionResponse: false,
      pendingResendContent: null,
      resendAwaitingHistory: false,
      compacting: false,
      lastContextData: null,
      contextBaseUsed: 0,
      contextLimit: 0,
      contextEstAdded: 0,
      mode: 'act',
      thinkingLevel: 'off',
      model: '',
      _notifiedBusy: false,
    };
    panes.set(pane.key, pane);
    return pane;
  }

  function activePane() {
    return panes.get(activePaneKey);
  }

  function findPaneBySession(id) {
    if (!id) return null;
    for (const pane of panes.values()) {
      if (pane.id === id) return pane;
    }
    return null;
  }

  function paneForMessage(data) {
    const sid = data.sessionId;
    if (sid) {
      const p = findPaneBySession(sid);
      if (p) return p;
      const act = activePane();
      // A session command's reply (response with a sessionAction, e.g.
      // clear_chat for /new, /resume, /fork) carries the NEW session id
      // before the pane has adopted it — the response handler re-keys the
      // pane so the follow-up clear_chat/history/config route correctly.
      if (act && data.type === 'response' && data.sessionAction) return act;
      if (act && (act.id === null || act.id === sid || pendingSessionResponse)) return act;
      return null;
    }
    return activePane();
  }

  function handleBackgroundMessage(pane, data) {
    switch (data.type) {
      case 'thinking':
      case 'waiting':
      case 'thinking_token':
      case 'stream':
      case 'stream_end':
      case 'tool_call_start':
      case 'tool_call_delta':
      case 'tool_call':
      case 'tool_execute':
      case 'tool_result':
      case 'term_opened':
      case 'term_output':
      case 'term_exit':
        pane.turnActive = true;
        break;
      case 'turn_end':
      case 'cancelled':
        pane.turnActive = false;
        if (pane.suppressTurnEnds > 0) pane.suppressTurnEnds--;
        break;
      case 'session_state':
        pane.turnActive = !!data.turnActive;
        break;
      case 'context':
        pane.lastContextData = data || pane.lastContextData;
        if (data.usedSource !== 'estimated') {
          pane.contextBaseUsed = data.usedTokens || 0;
          pane.contextLimit = data.contextLimit || 0;
          pane.contextEstAdded = 0;
        }
        break;
      case 'config':
        pane.mode = data.mode || pane.mode;
        pane.thinkingLevel = data.thinkingLevel || pane.thinkingLevel;
        pane.model = data.model || pane.model;
        if (data.sessionLabel) pane.label = data.sessionLabel;
        break;
      case 'clear_chat':
      case 'response':
        pane.pendingSessionResponse = false;
        break;
      case 'history':
        break;
      case 'delete_approval':
        break;
      default:
        break;
    }
    if (pane.turnActive && !pane._notifiedBusy) {
      pane._notifiedBusy = true;
      notifications.push(`busy:${pane.label || pane.id || 'session'}`);
    } else if (!pane.turnActive) {
      pane._notifiedBusy = false;
    }
  }
  // ── end copy ──

  return {
    panes, makePane, activePane, findPaneBySession, paneForMessage,
    handleBackgroundMessage, notifications,
    get activePaneKey() { return activePaneKey; },
    set activePaneKey(v) { activePaneKey = v; },
    get pendingSessionResponse() { return pendingSessionResponse; },
    set pendingSessionResponse(v) { pendingSessionResponse = v; },
  };
}

let failures = 0;
function assert(cond, msg) {
  if (!cond) { failures++; console.error('FAIL: ' + msg); }
  else { console.log('ok: ' + msg); }
}

// 1. No sessionId → active pane.
{
  const env = makePanesEnv();
  const a = env.makePane(); a.id = 'A';
  env.activePaneKey = a.key;
  assert(env.paneForMessage({ type: 'stream' }) === a, 'no sessionId routes to active pane');
}

// 2. Known background pane → routed there, active untouched.
{
  const env = makePanesEnv();
  const a = env.makePane(); a.id = 'A';
  const b = env.makePane(); b.id = 'B';
  env.activePaneKey = a.key;
  const p = env.paneForMessage({ type: 'turn_end', sessionId: 'B' });
  assert(p === b, 'known background session routes to its pane');
  env.handleBackgroundMessage(p, { type: 'stream' });
  assert(b.turnActive === true && a.turnActive === false, 'background stream marks only pane B busy');
  env.handleBackgroundMessage(p, { type: 'turn_end' });
  assert(b.turnActive === false, 'background turn_end clears pane B');
}

// 3. Unknown sessionId with pending session change → active pane (re-key
//    window: server sends history before config).
{
  const env = makePanesEnv();
  const a = env.makePane(); a.id = 'A';
  env.activePaneKey = a.key;
  env.pendingSessionResponse = true;
  const p = env.paneForMessage({ type: 'history', sessionId: 'NEW' });
  assert(p === a, 'unknown id during session change routes to active pane');
  env.pendingSessionResponse = false;
  assert(env.paneForMessage({ type: 'history', sessionId: 'NEW' }) === null,
    'unknown id with no pending change is dropped');
}

// 4. Active pane with null id adopts any session's messages (initial load /
//    sidebar-new before config arrives).
{
  const env = makePanesEnv();
  const a = env.makePane(); // id null
  env.activePaneKey = a.key;
  assert(env.paneForMessage({ type: 'config', sessionId: 'DEFAULT' }) === a,
    'null-id pane adopts the handshake session');
}

// 5. Background session_state drives the badge. A continuous busy period
//    notifies exactly once; a new turn after idle notifies again.
{
  const env = makePanesEnv();
  const a = env.makePane(); a.id = 'A';
  const b = env.makePane(); b.id = 'B';
  env.activePaneKey = a.key;
  env.handleBackgroundMessage(b, { type: 'session_state', turnActive: true });
  assert(b.turnActive === true && env.notifications.includes('busy:B'),
    'session_state(true) marks pane B busy and notifies');
  env.handleBackgroundMessage(b, { type: 'stream' });
  env.handleBackgroundMessage(b, { type: 'stream' });
  assert(env.notifications.filter((n) => n === 'busy:B').length === 1,
    'busy notification fires once per continuous busy period');
  env.handleBackgroundMessage(b, { type: 'turn_end' });
  assert(b.turnActive === false, 'turn_end clears pane B');
  env.handleBackgroundMessage(b, { type: 'session_state', turnActive: true });
  assert(env.notifications.filter((n) => n === 'busy:B').length === 2,
    'a new turn after idle notifies again');
}

// 6. Background config mirrors per-session toolbar state.
{
  const env = makePanesEnv();
  const a = env.makePane(); a.id = 'A';
  const b = env.makePane(); b.id = 'B';
  env.activePaneKey = a.key;
  env.handleBackgroundMessage(b, { type: 'config', mode: 'plan', thinkingLevel: 'high', sessionLabel: 'hi' });
  assert(b.mode === 'plan' && b.thinkingLevel === 'high' && b.label === 'hi',
    'background config stored in the pane object');
}

// 7. Typed /new (no pendingSessionResponse): the response arrives with the
//    NEW session id + sessionAction=clear_chat. It must route to the active
//    pane (whose id is still the old session) so the handler can re-key, and
//    the follow-up clear_chat/config for the new id must then route to it.
{
  const env = makePanesEnv();
  const a = env.makePane(); a.id = 'A';
  env.activePaneKey = a.key;
  env.pendingSessionResponse = false; // typed command — flag NOT set
  const resp = env.paneForMessage({ type: 'response', sessionId: 'NEW', sessionAction: 'clear_chat' });
  assert(resp === a, 'typed /new response routes to the active pane despite stale id');
  // The response handler adopts the new id:
  const oldId = a.id;
  a.id = 'NEW';
  env.pendingSessionResponse = false;
  assert(env.paneForMessage({ type: 'clear_chat', sessionId: 'NEW' }) === a,
    'follow-up clear_chat for the new id routes to the re-keyed pane');
  assert(oldId === 'A', 're-key kept the old id for detach');
}

if (failures === 0) {
  console.log('\nAll pane-routing checks passed.');
  process.exit(0);
} else {
  console.error(`\n${failures} check(s) failed.`);
  process.exit(1);
}
