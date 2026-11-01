'use strict';
// Regression test: the early WebSocket opened by ws-bootstrap.js is ADOPTED by
// app.js's connect() instead of a second socket being created.
//
// index.html loads /ws-bootstrap.js before /app.js. Module scripts run in
// document order, so the tiny bootstrap module opens the chat socket while the
// browser is still fetching app.js's ~940 KB static import graph — the
// handshake no longer starts at app.js's last line. connect() then adopts that
// socket:
//   - no second WebSocket is constructed,
//   - an already-open socket runs the open path immediately (list_sessions is
//     sent after adoption),
//   - frames received before adoption (the server's unsolicited session_state /
//     history / config) are replayed in arrival order,
//   - a socket that closed before adoption is discarded and a fresh one opens.
//
// The bootstrap module itself is not executed by jsdom (it does not run
// `type="module"` scripts); this test hands connect() the same global the
// bootstrap would have set, which is the contract between the two files.
//
// Run: node scripts/test_early_ws.js
const fs = require('fs');
const path = require('path');
const { JSDOM } = require(require('node:path').join(__dirname, '..', 'tmp', 'gogen-jsdom', 'node_modules', 'jsdom'));
const { loadAppJs, installEditorStubs } = require('./web-harness');

const ROOT = path.join(__dirname, '..');
const html = fs.readFileSync(path.join(ROOT, 'internal/server/web/index.html'), 'utf8');

let failures = 0;
const check = (desc, ok) => {
  console.log(`${ok ? 'PASS' : 'FAIL'}: ${desc}`);
  if (!ok) failures++;
};

// ── 0. Ordering contract: index.html must load the bootstrap module BEFORE
// app.js (module scripts run in document order). ──
{
  const bootAt = html.indexOf('src="/ws-bootstrap.js"');
  const appAt = html.indexOf('src="/app.js"');
  check('index.html loads ws-bootstrap.js before app.js', bootAt !== -1 && appAt !== -1 && bootAt < appAt);
}

// newWindow builds a jsdom window with the env stubs app.js's top-level code
// needs, plus a WebSocket class that records every construction (connect()'s
// fallback path). It does NOT load app.js yet: callers prime
// window.__gogenEarlyWS and only then call loadAppJs (connect() runs there).
function newWindow(sockets, constructed) {
  const dom = new JSDOM(html, { url: 'http://localhost/', runScripts: 'dangerously', pretendToBeVisual: true });
  const { window } = dom;
  window.matchMedia = window.matchMedia || (() => ({ matches: false, addEventListener() {}, removeEventListener() {} }));
  window.Notification = { permission: 'denied', requestPermission: () => Promise.resolve('denied') };
  window.navigator.clipboard = { writeText: async () => {} };
  window.Terminal = class { open() {} write() {} fit() {} onData() {} onResize() { return { dispose() {} }; } dispose() {} reset() {} focus() {} };
  window.FitAddon = class { fit() {} };
  window.WebSocket = class {
    constructor(url) {
      this.url = url;
      this.readyState = 0; // CONNECTING
      this.sent = [];
      constructed.push(this);
    }
    static OPEN = 1;
    static CONNECTING = 0;
    send(msg) { this.sent.push(JSON.parse(msg)); }
    close() { this.readyState = 3; }
  };
  installEditorStubs(window);
  window.marked = { use() {}, parse(text) { return String(text == null ? '' : text); } };
  window.DOMPurify = { sanitize: (raw) => raw };
  sockets.push(...[]);
  return { dom, window };
}

// earlySocket is a WebSocket-shaped object as ws-bootstrap.js leaves it once
// the handshake completed before app.js ran.
function earlySocket(buffered, readyState) {
  const sock = {
    readyState: readyState === undefined ? 1 : readyState, // WebSocket.OPEN
    sent: [],
    send(msg) { sock.sent.push(JSON.parse(msg)); },
    close() { sock.readyState = 3; },
    onopen: null,
    onmessage: null,
    onclose: null,
  };
  return { sock, closed: false, adopted: false, buffered };
}

function toastMessages(window) {
  return [...window.document.querySelectorAll('#toast-host .toast')].map((el) => el.dataset.message);
}

// ── 1. An OPEN early socket is adopted: no new socket, open path runs, the
// buffered frames replay in order. ──
{
  const sockets = [];
  const constructed = [];
  const { window } = newWindow(sockets, constructed);
  const early = earlySocket([
    JSON.stringify({ type: 'notice', content: 'EARLY-ONE' }),
    JSON.stringify({ type: 'notice', content: 'EARLY-TWO' }),
  ]);
  window.__gogenEarlyWS = early;
  loadAppJs(window);

  check('adopts the early socket (no second WebSocket constructed)', constructed.length === 0);
  check('open path ran on adoption (list_sessions sent)', early.sock.sent.some((m) => m.type === 'list_sessions'));
  check('buffered frames replayed in arrival order', toastMessages(window).join(',') === 'EARLY-ONE,EARLY-TWO');
  check('early socket is handed over (global cleared, adopted flagged)', window.__gogenEarlyWS === null && early.adopted === true);
}

// ── 2. A still-CONNECTING early socket is adopted and its open event drives
// the open path later (no duplicate socket; nothing sent before open). ──
{
  const sockets = [];
  const constructed = [];
  const { window } = newWindow(sockets, constructed);
  const early = earlySocket([], 0); // CONNECTING
  window.__gogenEarlyWS = early;
  loadAppJs(window);

  check('connecting early socket adopted without a second WebSocket', constructed.length === 0);
  check('nothing sent before the open event', early.sock.sent.length === 0);
  early.sock.readyState = 1;
  early.sock.onopen();
  check('open path runs on the adopted socket', early.sock.sent.some((m) => m.type === 'list_sessions'));
}

// ── 3. A socket that closed before adoption is discarded: connect() falls back
// to a fresh WebSocket (pre-bootstrap behavior), and the stale buffer is not
// replayed. ──
{
  const sockets = [];
  const constructed = [];
  const { window } = newWindow(sockets, constructed);
  const early = earlySocket([JSON.stringify({ type: 'notice', content: 'STALE' })]);
  early.closed = true;
  window.__gogenEarlyWS = early;
  loadAppJs(window);

  check('closed early socket discarded (fresh WebSocket constructed)', constructed.length === 1);
  check('stale buffered frames not replayed', !toastMessages(window).includes('STALE'));
}

// ── 4. No bootstrap (hosts without the module, tests): connect() is unchanged. ──
{
  const sockets = [];
  const constructed = [];
  const { window } = newWindow(sockets, constructed);
  window.__gogenEarlyWS = undefined;
  loadAppJs(window);

  check('without a bootstrap a WebSocket is constructed as before', constructed.length === 1);
}

// ── 5. ws-bootstrap.js itself (jsdom does not execute module scripts, so its
// source is eval'd here): it opens exactly one socket, buffers until adoption,
// and never opens a second one once app.js owns the connection. ──
const bootstrapSrc = fs.readFileSync(path.join(ROOT, 'internal/server/web/ws-bootstrap.js'), 'utf8');
{
  const constructed = [];
  const { window } = newWindow([], constructed);
  window.eval(bootstrapSrc);
  const early = window.__gogenEarlyWS;
  check('bootstrap opens one early socket', constructed.length === 1 && !!early && !!early.sock);
  early.sock.onmessage({ data: '{"type":"config"}' });
  check('bootstrap buffers a frame received before adoption', early.buffered.length === 1);
  early.adopted = true;
  early.sock.onmessage({ data: '{"type":"ignored"}' });
  check('bootstrap stops buffering after adoption', early.buffered.length === 1);
  early.sock.onclose();
  check('bootstrap marks a socket that closed before adoption', early.closed === true);
}
{
  const constructed = [];
  const { window } = newWindow([], constructed);
  window.__gogenWSOwned = true; // app.js's connect() already ran
  window.eval(bootstrapSrc);
  check('bootstrap opens nothing after app.js owns the socket',
    constructed.length === 0 && window.__gogenEarlyWS === undefined);
}

// app.js starts periodic timers (session refresh, reconnect) that keep node
// alive; the suite is done here, so exit explicitly (the pattern the other
// DOM tests in this directory use).
if (failures) {
  console.error('\n' + failures + ' check(s) FAILED');
  process.exit(1);
}
console.log('\nall early-socket checks passed');
process.exit(0);
