'use strict';
// Regression test for the queued (steering) message UI:
//   - a send that races the server's busy state (our turnActive mirror still
//     says idle) is adopted by the server's queue_update through the bubble's
//     correlation tag — never rendered twice;
//   - an item that leaves the queue because its turn STARTED (queueLeft)
//     clears the chip, while one removed before running does not linger in
//     the pane's mirror;
//   - a dom-cached pane refocused after a background queue change renders the
//     queued items from its mirror (background queue frames never touch the
//     DOM);
//   - a user_acked never stamps a still-queued bubble with its history index
//     (resend/fork would then target a message that never ran).
//
// Loads the real index.html + app.js into jsdom (imports stripped, editor/
// marked/dompurify stubbed) with a controllable WebSocket stub.
// Run: node scripts/test_steer_queue_ui.js
const fs = require('fs');
const path = require('path');
const { JSDOM } = require('/tmp/gogen-jsdom/node_modules/jsdom');
const { loadAppJs, installEditorStubs } = require('./web-harness');

const ROOT = path.join(__dirname, '..');
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function main() {
  const html = fs.readFileSync(path.join(ROOT, 'internal/server/web/index.html'), 'utf8');
  const dom = new JSDOM(html, { url: 'http://localhost/', runScripts: 'dangerously', pretendToBeVisual: true });
  const { window } = dom;

  window.matchMedia = window.matchMedia || (() => ({ matches: false, addEventListener() {}, removeEventListener() {} }));
  window.Notification = { permission: 'denied', requestPermission: () => Promise.resolve('denied') };
  window.navigator.clipboard = { writeText: async () => {} };
  class FakeTerminal {
    constructor() { this._cbs = {}; }
    open() {} write() {} fit() {}
    onData(cb) { this._cbs.data = cb; }
    onResize() { return { dispose() {} }; }
    dispose() {} reset() {} focus() {}
  }
  window.Terminal = FakeTerminal;
  window.FitAddon = class { fit() {} };

  const sockets = [];
  class FakeWS {
    constructor() { this.readyState = 0; this.sent = []; sockets.push(this); }
    static OPEN = 1;
    static CONNECTING = 0;
    send(msg) { this.sent.push(JSON.parse(msg)); }
    close() { this.readyState = 3; }
  }
  window.WebSocket = FakeWS;

  installEditorStubs(window);
  window.escapeHtml = window.escapeHtml || ((s) => String(s == null ? '' : s));
  window.marked = { use() {}, parse(text) { return String(text == null ? '' : text); } };
  window.DOMPurify = { sanitize: (raw) => raw };

  loadAppJs(window);

  const evalJs = (code) => window.eval(code);
  const ws = sockets[0];
  if (!ws) throw new Error('app.js did not construct a WebSocket');
  window.addEventListener('error', (e) => { console.error('WINDOW ERROR:', e.error && e.error.stack || e.message); });
  const recv = (obj) => {
    try {
      ws.onmessage({ data: JSON.stringify(obj) });
    } catch (err) {
      console.error('RECV ERROR:', err && err.stack || err);
      throw err;
    }
  };
  const doc = window.document;
  const messagesDiv = doc.getElementById('messages');
  const input = doc.getElementById('message-input');

  let failures = 0;
  const check = (desc, ok) => {
    console.log(`${ok ? 'PASS' : 'FAIL'}: ${desc}`);
    if (!ok) failures++;
  };
  const userBubbles = () => [...messagesDiv.querySelectorAll('.message.user')];
  const queuedBubbles = () => [...messagesDiv.querySelectorAll('.message.user.queued')];
  const lastSentMessage = () => ws.sent.filter((m) => m.type === 'message').pop();
  const send = (text) => { input.value = text; evalJs('sendMessage()'); };

  // 1. Adopt session A (idle: no turn_active/thinking frame).
  ws.readyState = FakeWS.OPEN;
  ws.onopen();
  recv({
    type: 'config', sessionId: 'A', sessionLabel: 'A-label',
    mode: 'act', thinkingLevel: 'off', reasoningEfforts: [], model: 'm1',
    modelDescription: '', workingDir: '/tmp', globalMode: false,
  });
  recv({
    type: 'history', sessionId: 'A',
    history: [
      { role: 'user', content: 'question one', index: 0, createdAt: '2024-01-01T00:00:00Z' },
      { role: 'assistant', content: 'answer one', index: 1, createdAt: '2024-01-01T00:00:01Z' },
    ],
  });
  await sleep(200);
  check('pane A adopted with its transcript', userBubbles().length === 1);

  // 2. Send while our mirror says idle, but the SERVER queues it (its own
  //    busy state differed): the queue_update must ADOPT the tagged bubble.
  send('steered msg');
  const sent = lastSentMessage();
  check('message frame carries a queueId', !!sent && typeof sent.queueId === 'string' && sent.queueId.length > 0);
  check('optimistic bubble is tagged with the queue id',
    userBubbles().length === 2 && userBubbles()[1].dataset.queueId === sent.queueId);
  check('bubble is not chip-marked before the server confirms', queuedBubbles().length === 0);

  recv({ type: 'queue_update', sessionId: 'A', queue: [{ id: sent.queueId, text: 'steered msg' }] });
  check('queue_update adopted the bubble instead of duplicating it', userBubbles().length === 2);
  check('adopted bubble shows the queued chip',
    queuedBubbles().length === 1 && !!queuedBubbles()[0].querySelector('.queued-chip'));
  check('pane mirror carries the queued item', evalJs('activePane().queueItems.length') === 1);

  // 3. A user_acked must not stamp a still-queued bubble: with the local
  //    pending entries gone (another tab's sends), the DOM fallback picks the
  //    newest UNQUEUED bubble.
  send('plain msg');
  const second = lastSentMessage();
  check('second send tagged', userBubbles().length === 3);
  recv({ type: 'queue_update', sessionId: 'A', queue: [{ id: sent.queueId, text: 'steered msg' }, { id: second.queueId, text: 'plain msg' }] });
  check('both sends queued', queuedBubbles().length === 2);
  // Drop every local pending-ack entry (the state another tab's send would
  // leave behind): the ack below must then resolve through the DOM fallback,
  // which is the path the fix protects.
  evalJs('document.querySelectorAll(".message.user[data-pending-ack]").forEach((el) => dropPendingAck(el));');
  check('local pending-ack entries cleared', evalJs('document.querySelectorAll(".message.user[data-pending-ack]").length') === 0);
  // The ack belongs to the oldest item ("steered msg" started running): its
  // chip was already cleared by the queueLeft frame below.
  recv({ type: 'queue_update', sessionId: 'A', queue: [{ id: second.queueId, text: 'plain msg' }], queueLeft: [sent.queueId] });
  recv({ type: 'user_acked', sessionId: 'A', content: '1' });
  const steered = userBubbles().find((el) => el.textContent.includes('steered msg'));
  const plain = userBubbles().find((el) => el.textContent.includes('plain msg'));
  check('drained item is no longer marked queued', !!steered && !steered.classList.contains('queued'));
  check('drained item got the ack history index', !!steered && steered.dataset.histIdx === '1');
  check('still-queued bubble was NOT stamped', !!plain && plain.dataset.histIdx === undefined);
  check('still-queued bubble keeps its chip', !!plain && plain.classList.contains('queued'));

  // 4. A background queue change (mirror-only) is rendered when the pane
  //    regains focus from its cached transcript. A pane is only cached while
  //    settled, so the pending-ack bookkeeping of the sends above is resolved
  //    first (the state a completed local send leaves behind).
  evalJs('document.querySelectorAll(".message.user[data-pending-ack]").forEach((el) => dropPendingAck(el));');
  window.openSessionPane('B');
  check('pane B active, A cached', evalJs('activePane().id') === 'B' && evalJs('findPaneBySession("A").domCache !== null'));
  // A message another tab queued on A while A was backgrounded.
  recv({ type: 'queue_update', sessionId: 'A', queue: [{ id: 'q-other-tab', text: 'queued by other tab' }] });
  check('background queue frame updated the mirror only', evalJs('findPaneBySession("A").queueItems.length') === 1);
  window.openSessionPane('A');
  await sleep(50);
  const refocused = userBubbles().filter((el) => el.textContent.includes('queued by other tab'));
  check('refocus rendered the queued item from the mirror', refocused.length === 1);
  check('refocus rendered exactly one queued bubble', queuedBubbles().length === 1);

  console.log(failures === 0 ? '\nALL CHECKS PASSED' : `\n${failures} CHECK(S) FAILED`);
  process.exit(failures === 0 ? 0 : 1);
}

main().catch((err) => { console.error(err); process.exit(1); });
