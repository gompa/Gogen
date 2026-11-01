'use strict';
// Regression test: the pinned user terminal tab must never stay on
// "starting…" — that placeholder is the strip's only progress indicator, so a
// shell that fails to start (or whose request is never answered) looked like a
// startup that never finishes, with the ✗ chip as the only hint and the
// server's explanation thrown away.
//
// Covered here (each frame sequence is what the server really sends):
//   1. failed spawn — user_term_exit BEFORE any user_term_opened. The tab
//      leaves "starting…", shows the server's own reason ("failed to start
//      shell: …"), and offers the ↻ restart, which re-asks for a shell.
//   2. silent server — no answer at all. The spawn watchdog converts the
//      pending request into the same dead/restartable state.
//
// Run: node scripts/test_term_stuck.js
const fs = require('fs');
const path = require('path');
const { JSDOM } = require(require('node:path').join(__dirname, '..', 'tmp', 'gogen-jsdom', 'node_modules', 'jsdom'));
const { loadAppJs, installEditorStubs } = require('./web-harness');

const ROOT = path.join(__dirname, '..');
const html = fs.readFileSync(path.join(ROOT, 'internal/server/web/index.html'), 'utf8');

let failures = 0;
const check = (desc, ok, extra) => {
  console.log(`${ok ? 'PASS' : 'FAIL'}: ${desc}${ok || !extra ? '' : ' — ' + extra}`);
  if (!ok) failures++;
};

// newWindow builds a jsdom window with the real index.html + app.js and a
// WebSocket stub that records what the client sends.
function newWindow(terms) {
  const dom = new JSDOM(html, { url: 'http://localhost/', runScripts: 'dangerously', pretendToBeVisual: true });
  const { window } = dom;
  window.matchMedia = window.matchMedia || (() => ({ matches: false, addEventListener() {}, removeEventListener() {} }));
  window.Notification = { permission: 'denied', requestPermission: () => Promise.resolve('denied') };
  window.navigator.clipboard = { writeText: async () => {} };
  // FakeTerminal records writes so the test can read what the tab printed.
  class FakeTerminal {
    constructor() {
      this.written = '';
      if (terms) terms.push(this);
    }
    open() {}
    write(s) { this.written += String(s); }
    fit() {}
    onData() {}
    onResize() { return { dispose() {} }; }
    dispose() {}
    reset() {}
    focus() {}
    blur() {}
  }
  window.Terminal = FakeTerminal;
  window.FitAddon = class { fit() {} proposeDimensions() { return { cols: 80, rows: 24 }; } };
  const sockets = [];
  window.WebSocket = class {
    constructor() { this.readyState = 0; this.sent = []; sockets.push(this); }
    static OPEN = 1;
    static CONNECTING = 0;
    send(m) { this.sent.push(JSON.parse(m)); }
    close() { this.readyState = 3; }
  };
  installEditorStubs(window);
  window.marked = { use() {}, parse: (t) => String(t == null ? '' : t) };
  window.DOMPurify = { sanitize: (r) => r };
  loadAppJs(window);
  const ws = sockets[0];
  if (!ws) throw new Error('app.js did not construct a WebSocket');
  ws.readyState = window.WebSocket.OPEN;
  ws.onopen();
  return { window, ws, terms };
}

const userTab = (window) => window.document.querySelector('.term-tab.user-tab');
const userTitle = (window) => {
  const el = window.document.querySelector('.term-tab.user-tab .term-tab-title');
  return el ? el.textContent : null;
};
const userStatus = (window) => {
  const el = window.document.querySelector('.term-tab.user-tab .term-tab-status');
  return el ? el.textContent : null;
};
const restartHidden = (window) => {
  const el = window.document.querySelector('.term-tab.user-tab .term-tab-restart');
  return el ? el.hidden : null;
};
// termWrites is everything the user shell's xterm received. The user tab is the
// only terminal a fresh window creates, so it is the first instance.
const termWrites = (terms) => (terms && terms.length ? terms[0].written : '');
const userTermRequests = (ws) => ws.sent.filter((m) => m.type === 'user_term_request').length;

async function main() {
  // ── 0. The collapsed strip must not advertise a spawn that was never asked
  // for: the shell is spawned lazily on panel EXPAND, so a tab sitting in the
  // collapsed bar reads "shell" (neutral), not "starting…". Clicking it is a
  // request to see the terminal, so it expands the panel and asks. ──
  {
    const terms = [];
    const { window, ws } = newWindow(terms);
    check('0: the docked strip starts collapsed', !window.document.getElementById('terminal-panel').classList.contains('expanded'));
    check('0: a collapsed strip does not claim to be starting', userTitle(window) === 'shell', userTitle(window));
    check('0: no shell was requested before the panel is opened', userTermRequests(ws) === 0);
    userTab(window).click();
    check('0: clicking the tab expands the panel', window.document.getElementById('terminal-panel').classList.contains('expanded'));
    check('0: clicking the tab asks for the shell', userTermRequests(ws) === 1);
  }

  // ── 1. A shell that fails to start must leave "starting…" and say why. ──
  {
    const terms = [];
    const { window, ws } = newWindow(terms);
    window.terminalTogglePanel(); // expand → asks for the shell
    check('1: expanding the panel asks for the shell', userTermRequests(ws) === 1);
    check('1: tab starts on the "starting…" placeholder', userTitle(window) === 'starting…');

    // The server could not exec $SHELL: user_term_exit with its own message.
    const reason = 'failed to start shell: fork/exec /nonexistent-shell: no such file or directory';
    ws.onmessage({ data: JSON.stringify({ type: 'user_term_exit', content: reason, code: -1 }) });

    check('1: tab no longer says "starting…"', userTitle(window) !== 'starting…', userTitle(window));
    check('1: tab carries a name instead of the placeholder', userTitle(window) === 'shell', userTitle(window));
    check('1: status chip shows failure', userStatus(window) === '✗', userStatus(window));
    check('1: restart affordance is offered', restartHidden(window) === false);
    // The reason reaches the terminal (it is the only place it is reported).
    check('1: server reason is shown in the terminal',
      (() => { const t = termWrites(terms); return t.includes('failed to start shell') && t.includes('click ↻ to restart'); })(),
      JSON.stringify(termWrites(terms)));

    // ↻ re-asks; the answer restores a normal tab.
    userTab(window).querySelector('.term-tab-restart').click();
    check('1: restart sends a new user_term_request', userTermRequests(ws) === 2);
    check('1: restart clears the placeholder again', userStatus(window) === '…');
    ws.onmessage({ data: JSON.stringify({ type: 'user_term_opened', content: 'bash', workingDir: '/tmp' }) });
    check('1: opened names the shell', userTitle(window) === 'bash', userTitle(window));
    check('1: opened clears the status chip', userStatus(window) === '');
    check('1: opened hides the restart affordance', restartHidden(window) === true);
  }

  // ── 2. A request the server never answers must not hang forever. ──
  {
    const terms = [];
    const { window, ws } = newWindow(terms);
    // Shorten the spawn watchdog (a var so tests need not wait it out).
    window.eval('userTermSpawnTimeoutMs = 60');
    window.terminalTogglePanel();
    check('2: panel asked for the shell', userTermRequests(ws) === 1);
    check('2: tab waits on the placeholder', userTitle(window) === 'starting…');

    await new Promise((r) => setTimeout(r, 250));
    check('2: watchdog leaves "starting…"', userTitle(window) !== 'starting…', userTitle(window));
    check('2: watchdog marks the tab failed', userStatus(window) === '✗', userStatus(window));
    check('2: watchdog offers the restart affordance', restartHidden(window) === false);
    check('2: watchdog explains itself',
      (() => { const t = termWrites(terms); return t.includes('no response from the server') && t.includes('click ↻ to restart'); })(),
      JSON.stringify(termWrites(terms)));
    check('2: watchdog does not spam requests', userTermRequests(ws) === 1);

    // The restart path works from there.
    userTab(window).querySelector('.term-tab-restart').click();
    check('2: restart after a timeout re-asks', userTermRequests(ws) === 2);
  }

  // A successful open must disarm the watchdog: a later timer firing on a live
  // terminal would kill the tab's state.
  {
    const terms = [];
    const { window, ws } = newWindow(terms);
    window.eval('userTermSpawnTimeoutMs = 60');
    window.terminalTogglePanel();
    ws.onmessage({ data: JSON.stringify({ type: 'user_term_opened', content: 'bash', workingDir: '/tmp' }) });
    await new Promise((r) => setTimeout(r, 250));
    check('3: an opened shell is not killed by the spawn watchdog',
      userTitle(window) === 'bash' && userStatus(window) === '' && restartHidden(window) === true,
      `${userTitle(window)} / ${userStatus(window)}`);
  }

  if (failures) {
    console.error('\n' + failures + ' check(s) FAILED');
    process.exit(1);
  }
  console.log('\nall terminal-stuck checks passed');
  process.exit(0);
}

main();
