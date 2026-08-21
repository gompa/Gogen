'use strict';
// Regression test for the live-only subagent failure notice: when a
// background subagent FAILS, the parent's chat shows an ephemeral system
// line with the failure reason (from the subagent_finished event's
// subagentSummary). The line is NOT part of the transcript: it carries no
// hist-idx, so history replay (reload / pane switch / turn convergence)
// removes it. A successful finish appends nothing, and a failure whose
// parent session has no open pane updates the sidebar only.
//
// Loads the real index.html + app.js into jsdom (imports stripped, editor/
// marked/dompurify stubbed) with a controllable WebSocket stub, then:
//   1. adopts a parent session A as the active pane
//   2. subagent_finished (failed) for child C of A → system line with the
//      label + reason, and NO data-hist-idx
//   3. subagent_finished (success) for child S of A → no new system line
//   4. subagent_finished (failed) for child X of parent B (no pane) → no
//      new system line
//
// Run: node scripts/test_subagent_failure_notice.js
// APPJS env var selects the app.js copy to test (defaults to the real one).
const fs = require('fs');
const path = require('path');
const { JSDOM } = require('/tmp/gogen-jsdom/node_modules/jsdom');
const { loadAppJs, installEditorStubs } = require('./web-harness');

const ROOT = path.join(__dirname, '..');
const APPJS = process.env.APPJS || path.join(ROOT, 'internal/server/web/app.js');

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
    constructor() {
      this.readyState = 0;
      this.sent = [];
      sockets.push(this);
    }
    static OPEN = 1;
    static CONNECTING = 0;
    send(msg) { this.sent.push(JSON.parse(msg)); }
    close() { this.readyState = 3; }
  }
  window.WebSocket = FakeWS;

  installEditorStubs(window);
  window.marked = { use() {}, parse(text) { return String(text == null ? '' : text); } };
  window.DOMPurify = { sanitize: (raw) => raw };

  loadAppJs(window);

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

  let failures = 0;
  const check = (desc, ok) => {
    console.log(`${ok ? 'PASS' : 'FAIL'}: ${desc}`);
    if (!ok) failures++;
  };
  const systemLines = () => [...window.document.querySelectorAll('#messages .message.system')];

  // 1. Connect and adopt the parent session A as the active pane.
  ws.readyState = FakeWS.OPEN;
  ws.onopen();
  recv({
    type: 'config', sessionId: 'A', sessionLabel: 'parent-session',
    mode: 'act', thinkingLevel: 'off', reasoningEfforts: [], model: 'm1',
    modelDescription: '', workingDir: '/tmp', globalMode: false,
  });
  if (window.eval('activePane().id') !== 'A') throw new Error('parent pane A not adopted');

  // 2. A background child of A FAILS: the event carries the reason in
  //    subagentSummary. The active parent pane must show an ephemeral
  //    system line with the label and the reason.
  recv({
    type: 'subagent_started', subagentId: 'C', subagentParent: 'A',
    subagentLabel: 'subagent: fix parser', subagentJob: 'fix parser',
  });
  recv({
    type: 'subagent_finished', subagentId: 'C', subagentParent: 'A',
    subagentLabel: 'subagent: fix parser', subagentSuccess: false,
    subagentSummary: 'provider exploded',
  });
  let lines = systemLines();
  check('failed child appends a system line to the parent chat', lines.length === 1);
  const text = lines[0] ? lines[0].textContent : '';
  check('system line names the child and the failure reason',
    text.includes('subagent: fix parser') && text.includes('failed') && text.includes('provider exploded'));
  check('system line is ephemeral (no data-hist-idx)',
    !!lines[0] && lines[0].dataset.histIdx === undefined);

  // 3. A child of A that SUCCEEDS appends nothing to the chat.
  recv({
    type: 'subagent_started', subagentId: 'S', subagentParent: 'A',
    subagentLabel: 'subagent: good job', subagentJob: 'good job',
  });
  recv({
    type: 'subagent_finished', subagentId: 'S', subagentParent: 'A',
    subagentLabel: 'subagent: good job', subagentSuccess: true,
    subagentSummary: 'all done',
  });
  lines = systemLines();
  check('successful child appends no system line', lines.length === 1);

  // 4. A failure whose parent session has NO open pane updates the sidebar
  //    only — no chat line (the event is sidebar-global).
  recv({
    type: 'subagent_started', subagentId: 'X', subagentParent: 'B',
    subagentLabel: 'subagent: other parent', subagentJob: 'other job',
  });
  recv({
    type: 'subagent_finished', subagentId: 'X', subagentParent: 'B',
    subagentLabel: 'subagent: other parent', subagentSuccess: false,
    subagentSummary: 'boom',
  });
  lines = systemLines();
  check('failure of a pane-less parent appends no system line', lines.length === 1);

  console.log(failures === 0 ? '\nALL CHECKS PASSED' : `\n${failures} CHECK(S) FAILED`);
  process.exit(failures === 0 ? 0 : 1);
}

main().catch((err) => { console.error(err); process.exit(1); });
