'use strict';
// Regression checks for the settings-modal sidebar rework and the subagent
// model picker. Loads the REAL index.html + app.js into jsdom (imports
// stripped, editor/marked/dompurify stubbed, no server needed) and asserts:
//   - the settings modal shows ONE section at a time (sidebar tabs),
//   - opening settings auto-selects the tab matching the active screen
//     (chat -> Chat, editor -> Editor, board -> Agent) and falls back to
//     the last-used tab otherwise (terminal/unmatched),
//   - manual tab switches persist to localStorage,
//   - the subagent model picker (Agent tab) renders the toolbar-style
//     grouped model list with an "Inherit" row, follows the server-pushed
//     subagentModel value (including the empty "inherit" state), sends
//     configFields:["subagentModel"] on selection, and hides when the
//     subagent feature is off.
// Run: node scripts/test_settings_tabs.js
const fs = require('fs');
const path = require('path');
const { JSDOM } = require('/tmp/gogen-jsdom/node_modules/jsdom');

const ROOT = path.join(__dirname, '..');

async function main() {
  const html = fs.readFileSync(path.join(ROOT, 'internal/server/web/index.html'), 'utf8');
  const dom = new JSDOM(html, { url: 'http://localhost/', runScripts: 'dangerously', pretendToBeVisual: true });
  const { window } = dom;
  const { document } = window;

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
  // A WebSocket stub that records every sent message (OPEN so the app's
  // ws.readyState guards pass).
  const sent = [];
  window.WebSocket = class {
    constructor() { this.readyState = 1; }
    static OPEN = 1;
    send(data) { sent.push(data); }
    close() {}
  };
  window.HTMLElement.prototype.scrollIntoView = function () {};

  const editorStubs = [
    'connectEditorSocket', 'setupEditorUI', 'refreshExplorer', 'disposeChatEditors',
    'mountDiffEditor', 'updateDiffEditor', 'updateDiffFallback', 'chatDiffWheelEdge',
    'extractDiffValue', 'initMonaco', 'colorizeCodeBlocks', 'colorizeElement',
    'languageFromPath', 'setToastHandler', 'focusFindInFiles', 'editorUndo',
    'editorRedo', 'saveAll', 'saveActive', 'openFileAtLine', 'setMonacoTheme',
    'applyEditorPrefs', 'applyPatchFromDiff',
  ];
  for (const name of editorStubs) {
    window[name] = window[name] || (() => Promise.resolve());
  }
  // Real-ish open/close so the overlay's .active class (which gates the
  // picker's re-render and the fetch-on-open) actually toggles.
  window.openModal = (overlay) => overlay.classList.add('active');
  window.closeModal = (overlay) => overlay.classList.remove('active');
  window.marked = { use() {}, parse(text) { return String(text == null ? '' : text); } };
  window.DOMPurify = { sanitize: (raw) => raw };

  const appJs = fs.readFileSync(path.join(ROOT, 'internal/server/web/app.js'), 'utf8');
  const stripped = appJs
    .replace(/import\s*\{[^}]*\}\s*from\s*['"][^'"]+['"];\s*/gs, '')
    .replace(/import\s+[A-Za-z_$][\w$]*\s+from\s*['"][^'"]+['"];\s*/g, '');
  window.eval(stripped);

  const settingsBtn = document.getElementById('settings-btn');
  const settingsOverlay = document.getElementById('settings-overlay');
  const sidebar = document.querySelector('.settings-sidebar');
  const mainTabs = () => Array.from(document.querySelectorAll('.main-tab'));
  const tabBtns = () => Array.from(document.querySelectorAll('.settings-tab-btn'));
  const panels = () => Array.from(document.querySelectorAll('.settings-group[role="tabpanel"]'));
  const visiblePanel = () => panels().find((p) => !p.hidden);
  const activeTabBtn = () => tabBtns().find((b) => b.classList.contains('active'));
  const picker = document.getElementById('subagent-model-picker');
  const pickerList = document.getElementById('subagent-model-list');
  const subagentSelect = document.getElementById('subagent-enabled-select');

  let failures = 0;
  const check = (desc, ok) => {
    console.log(`${ok ? 'PASS' : 'FAIL'}: ${desc}`);
    if (!ok) failures++;
  };
  const openFromPane = (pane) => {
    mainTabs().forEach((t) => t.classList.toggle('active', t.dataset.pane === pane));
    settingsBtn.click();
  };

  // ── Sidebar tabs: one visible panel at a time ──
  openFromPane('chat');
  check('opening settings from the chat screen selects the Chat tab', activeTabBtn() && activeTabBtn().dataset.tab === 'chat');
  check('exactly one settings panel is visible', panels().filter((p) => !p.hidden).length === 1);
  check('the visible panel is the Chat section', visiblePanel() && visiblePanel().id === 'settings-tab-chat');
  check('all other panels are hidden', panels().filter((p) => p.hidden).length === panels().length - 1);
  check('selected tab button carries aria-selected', activeTabBtn().getAttribute('aria-selected') === 'true');

  // ── Auto-open follows the active screen ──
  window.closeModal(settingsOverlay);
  openFromPane('editor');
  check('editor screen auto-opens the Editor tab', activeTabBtn() && activeTabBtn().dataset.tab === 'editor');
  check('Editor panel visible', visiblePanel() && visiblePanel().id === 'settings-tab-editor');

  window.closeModal(settingsOverlay);
  openFromPane('board');
  check('board screen auto-opens the Agent tab (board settings live there)', activeTabBtn() && activeTabBtn().dataset.tab === 'agent');
  check('Agent panel visible', visiblePanel() && visiblePanel().id === 'settings-tab-agent');

  // ── Manual tab switch persists to localStorage ──
  const providersBtn = tabBtns().find((b) => b.dataset.tab === 'providers');
  providersBtn.click();
  check('manual switch opens the Providers tab', activeTabBtn() && activeTabBtn().dataset.tab === 'providers');
  check('Providers panel visible', visiblePanel() && visiblePanel().id === 'settings-tab-providers');
  check('tab choice persisted', window.localStorage.getItem('gogen-settings-tab') === 'providers');

  // ── Unmatched screen falls back to last-used ──
  window.closeModal(settingsOverlay);
  openFromPane('terminal');
  check('terminal screen falls back to the last-used tab (providers)', activeTabBtn() && activeTabBtn().dataset.tab === 'providers');

  // ── MCP gets its own tab (moved out of Server) ──
  const mcpBtn = tabBtns().find((b) => b.dataset.tab === 'mcp');
  check('MCP tab button exists', !!mcpBtn);
  mcpBtn.click();
  check('MCP tab opens its own panel', visiblePanel() && visiblePanel().id === 'settings-tab-mcp');
  check('MCP select lives in the MCP panel', !!document.querySelector('#settings-tab-mcp #mcp-select'));
  check('MCP test form lives in the MCP panel', !!document.querySelector('#settings-tab-mcp #mcp-test-btn'));
  check('MCP server list container lives in the MCP panel', !!document.querySelector('#settings-tab-mcp #mcp-server-list'));
  check('Server panel no longer holds the MCP select', !document.querySelector('#settings-tab-server #mcp-select'));
  window.applyServerConfig({
    mcpServers: [{ name: 'fetch', command: 'npx', args: ['-y', '@modelcontextprotocol/server-fetch'], envSet: true }],
    mcp: 'on',
  });
  check('MCP server list renders inside the MCP panel', !!document.querySelector('#settings-tab-mcp .mcp-server-list .provider-row'));

  // ── Subagent model picker ──
  window.closeModal(settingsOverlay);
  openFromPane('board'); // Agent tab
  // Server push: subagents on, no default model yet.
  window.applyServerConfig({ board: 'off', subagent: 'on', subagentMaxDepth: 1, subagentModel: '' });
  check('picker is visible while subagents are enabled', picker && !picker.hidden);
  // Catalog reply (the toolbar shares this flow).
  window.handleModels({
    models: [
      { id: 'gpt-4o', provider: 'default', contextLimit: 128000 },
      { id: 'llama3.1', provider: 'local', contextLimit: 8192 },
    ],
    model: '',
  });
  const rows = () => Array.from(pickerList.querySelectorAll('.tb-model-row'));
  const inheritRow = () => rows().find((r) => r.textContent.includes('Inherit'));
  check('inherit row rendered', !!inheritRow());
  check('inherit row active when no default is configured', inheritRow().classList.contains('active'));
  check('model rows rendered (toolbar-style)', rows().length === 3); // inherit + 2 models
  check('provider group headers rendered', pickerList.querySelectorAll('.tb-model-group').length === 2);
  const gpt4oRow = rows().find((r) => r.textContent.startsWith('gpt-4o'));
  gpt4oRow.click();
  const pickMsg = sent.find((s) => s.includes('configFields') && s.includes('subagentModel'));
  check('selection sends configFields:["subagentModel"] with the model id',
    !!pickMsg && pickMsg.includes('"subagentModel":"gpt-4o"'));
  check('picked row highlights immediately', rows().find((r) => r.textContent.startsWith('gpt-4o')).classList.contains('active'));
  check('inherit row un-highlighted after pick', !inheritRow().classList.contains('active'));

  // Server push confirms the selection (echo from another tab).
  window.applyServerConfig({ board: 'off', subagent: 'on', subagentMaxDepth: 1, subagentModel: 'gpt-4o' });
  check('server push keeps the picked row active', rows().find((r) => r.textContent.startsWith('gpt-4o')).classList.contains('active'));

  // Clearing back to inherit (empty push) re-highlights the inherit row.
  window.applyServerConfig({ board: 'off', subagent: 'on', subagentMaxDepth: 1, subagentModel: '' });
  check('empty push (clear) re-highlights inherit', inheritRow().classList.contains('active'));

  // Feature off hides the picker.
  window.applyServerConfig({ board: 'off', subagent: 'off', subagentMaxDepth: 1, subagentModel: '' });
  check('picker hidden while subagents are disabled', picker.hidden);

  if (failures > 0) {
    console.error(`test_settings_tabs: ${failures} FAILURE(S)`);
    process.exit(1);
  }
  console.log('test_settings_tabs: all checks passed');
  process.exit(0);
}

main().catch((err) => {
  console.error('test_settings_tabs:', err);
  process.exit(1);
});
