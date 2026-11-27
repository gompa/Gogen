'use strict';
// Regression test for the cancel button's live UI cleanup:
//   1. cancelling while the "Thinking…" waiting placeholder is visible must
//      remove that placeholder (not just the composer's progress strip).
//   2. a thinking frame already queued on the socket when the user hits
//      cancel must not resurrect the spinner after the local stop.
//   3. the next real turn still shows its own indicator normally.
//
// Run: node scripts/test_cancel_thinking_indicator.js
const fs = require('fs');
const path = require('path');
const { JSDOM } = require(require('node:path').join(__dirname, '..', 'tmp', 'gogen-jsdom', 'node_modules', 'jsdom'));
const { loadAppJs, installEditorStubs } = require('./web-harness');

const ROOT = path.join(__dirname, '..');
let failures = 0;
const check = (desc, ok) => {
  console.log(`${ok ? 'PASS' : 'FAIL'}: ${desc}`);
  if (!ok) failures++;
};

function boot() {
  const html = fs.readFileSync(path.join(ROOT, 'internal/server/web/index.html'), 'utf8');
  const dom = new JSDOM(html, { url: 'http://localhost/', runScripts: 'dangerously', pretendToBeVisual: true });
  const { window } = dom;
  window.matchMedia = window.matchMedia || (() => ({ matches: false, addEventListener() {}, removeEventListener() {} }));
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
  window.marked = { use() {}, parse(text) { return String(text == null ? '' : text); } };
  window.DOMPurify = { sanitize: (raw) => raw };
  window.localStorage.setItem('gogen_notifications', 'off');
  window.addEventListener('error', (e) => {
    console.error('WINDOW ERROR:', (e.error && e.error.stack) || e.message);
    failures++;
  });
  loadAppJs(window);

  const ws = sockets[0];
  if (!ws) throw new Error('app.js did not construct a WebSocket');
  ws.readyState = FakeWS.OPEN;
  ws.onopen();
  return {
    window, ws,
    recv: (obj) => ws.onmessage({ data: JSON.stringify(obj) }),
    sentOf: (type) => ws.sent.filter((m) => m.type === type),
    $: (id) => window.document.getElementById(id),
    placeholders: () => [...window.document.querySelectorAll('.waiting-placeholder')],
    progressActive: () => window.document.getElementById('input-progress').classList.contains('active'),
    turnActive: () => window.document.getElementById('input-area').classList.contains('turn-active'),
  };
}

function main() {
  const env = boot();
  env.recv({
    type: 'config', sessionId: 'A', sessionLabel: 'A',
    mode: 'act', thinkingLevel: 'off', reasoningEfforts: [],
    model: 'm1', modelDescription: '', workingDir: '/tmp', globalMode: false,
  });

  // 1. Visible thinking indicator, then a local cancel.
  env.recv({ type: 'thinking', sessionId: 'A' });
  check('thinking shows the chat placeholder', env.placeholders().length === 1);
  check('thinking shows the composer progress', env.progressActive());
  check('turn is active before cancel', env.turnActive());

  env.$('cancel-btn').click();
  check('cancel frame sent', env.sentOf('cancel').length === 1);
  check('cancel clears busy chrome', !env.turnActive());
  check('cancel clears composer progress', !env.progressActive());
  check('cancel clears chat placeholder', env.placeholders().length === 0);

  env.recv({ type: 'cancelled', sessionId: 'A', content: 'Cancelled.' });
  env.recv({ type: 'turn_end', sessionId: 'A' });
  check('terminal frames keep the UI idle', env.placeholders().length === 0 && !env.progressActive());

  // 2. A thinking frame emitted before the cancel arrives afterwards.
  env.recv({ type: 'thinking', sessionId: 'A' });
  check('turn restarted before the second cancel', env.turnActive());
  env.$('cancel-btn').click();
  // A frame the server wrote before the cancel is delivered afterwards.
  env.recv({ type: 'thinking', sessionId: 'A' });
  check('stale thinking does not resurrect the chat placeholder', env.placeholders().length === 0);
  check('stale thinking does not resurrect composer progress', !env.progressActive());
  env.recv({ type: 'cancelled', sessionId: 'A', content: 'Cancelled.' });
  env.recv({ type: 'turn_end', sessionId: 'A' });

  // 3. A genuinely new turn still renders normally.
  env.recv({ type: 'thinking', sessionId: 'A' });
  check('new turn shows a placeholder again', env.placeholders().length === 1);
  check('new turn shows progress again', env.progressActive());
  check('new turn is active', env.turnActive());

  if (failures) {
    console.error(`test_cancel_thinking_indicator: ${failures} FAILURE(S)`);
    process.exit(1);
  }
  console.log('test_cancel_thinking_indicator: all checks passed');
  process.exit(0);
}

try { main(); } catch (err) {
  console.error('test_cancel_thinking_indicator:', err);
  process.exit(1);
}
