// Settings modal + all persisted preferences for the GoGen web UI:
// the tabbed settings overlay (last-used tab persisted), the
// server-backed feature toggles (board / subagents), the runtime
// config options, the MCP test flow, the providers list, the theme
// (system-preference detection + override), file-click behavior,
// the chat diff viewer mode, the Monaco editor preferences,
// desktop notifications, the "show reply model" toggle and the
// accent color.
//
// Server-backed values round-trip over the chat WebSocket: app.js
// keeps the ws dispatch (config / test_mcp / provider_test) and
// forwards the payloads to applyFeatureSettings /
// applyRuntimeSettings / applyProviderSettings / applyMCPSettings
// below; the per-bubble reply-model render hook stays in app.js and
// reads the preference through getShowReplyModelPref.
//
// Wiring: app.js calls initSettings(deps) once at startup.
//   deps.getWs()                     — the chat WebSocket (or null)
//   deps.getPane()                   — the active pane
//   deps.getModels()                 — the available-models catalog
//   deps.requestModels()             — force a model-catalog refetch
//   deps.switchMainPane(pane)        — switch the main screen
//   deps.showToast(message, kind)    — the toast stack
//   deps.applyReplyModelChips()      — re-sync the per-bubble model chips
//   deps.setSubagentModel(v)         — the settings subagent picker state
//   deps.setSubagentThinkingLevel(v) — the settings subagent picker state
//   deps.renderSubagentPicker()      — re-render the settings subagent picker

import { openModal, closeModal, setMonacoTheme, applyEditorPrefs } from '/editor.js';
import { requestBoardState, setBoardStartPrompt } from '/components/board.js';

let deps = null;

export function initSettings(d) {
    deps = d;
}

// === Settings modal ===
const settingsBtn = document.getElementById('settings-btn');
const settingsOverlay = document.getElementById('settings-overlay');
const themeSelect = document.getElementById('theme-select');

// Settings sections are tabbed (sidebar): showSettingsTab reveals
// one .settings-group panel at a time. The last-used tab persists
// per browser (localStorage); opening the modal from a screen with
// a natural settings home (chat/editor/board) auto-selects that tab
// instead — the screen the user is on wins on open.
const SETTINGS_TAB_STORAGE = 'gogen-settings-tab';
const SCREEN_TO_SETTINGS_TAB = { chat: 'chat', editor: 'editor', board: 'agent' };
let settingsTab = localStorage.getItem(SETTINGS_TAB_STORAGE) || 'chat';

export function showSettingsTab(tab) {
    const panel = document.getElementById('settings-tab-' + tab);
    if (!panel) tab = 'chat'; // stale localStorage value — fall back
    settingsTab = tab;
    try { localStorage.setItem(SETTINGS_TAB_STORAGE, tab); } catch (e) { /* private mode */ }
    document.querySelectorAll('.settings-tab-btn').forEach((btn) => {
        const on = btn.dataset.tab === tab;
        btn.classList.toggle('active', on);
        btn.setAttribute('aria-selected', on ? 'true' : 'false');
    });
    document.querySelectorAll('.settings-group[role="tabpanel"]').forEach((p) => {
        p.hidden = p.id !== 'settings-tab-' + tab;
    });
}

export function openSettings() {
    const pane = document.querySelector('.main-tab.active')?.dataset.pane;
    showSettingsTab(SCREEN_TO_SETTINGS_TAB[pane] || settingsTab);
    openModal(settingsOverlay);
    // The subagent model picker (Agent tab) needs the model catalog;
    // fetch it on open — the reply refreshes the toolbar picker too
    // (shared list_models flow, server-cached).
    const ws = deps.getWs();
    if (document.getElementById('subagent-enabled-select')?.value === 'on'
        && ws && ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ type: 'list_models' }));
    }
}

export function closeSettings() {
    closeModal(settingsOverlay);
}

export function isSettingsOpen() {
    return settingsOverlay.classList.contains('active');
}

settingsBtn.addEventListener('click', openSettings);
document.querySelector('.settings-sidebar')?.addEventListener('click', (e) => {
    const btn = e.target.closest('.settings-tab-btn');
    if (btn && btn.dataset.tab) showSettingsTab(btn.dataset.tab);
});
document.getElementById('settings-close-btn')?.addEventListener('click', () => {
    closeSettings();
});
settingsOverlay.addEventListener('click', (e) => {
    if (e.target === settingsOverlay) closeSettings();
});

// === Feature settings: server-backed toggles (config WS message) ===
// Unlike the localStorage preferences above, these round-trip to the
// server: the toggle applies live to every session and is persisted
// to .gogen/gogen.conf. The select state follows the config push
// (applyFeatureSettings), so multiple tabs stay in sync.
const boardEnabledSelect = document.getElementById('board-enabled-select');
const subagentEnabledSelect = document.getElementById('subagent-enabled-select');
const subagentDepthInput = document.getElementById('subagent-depth-input');
const subagentLimitInput = document.getElementById('subagent-limit-input');
if (boardEnabledSelect) {
    boardEnabledSelect.addEventListener('change', () => {
        sendFeatureConfig({ board: boardEnabledSelect.value });
    });
}
if (subagentEnabledSelect) {
    subagentEnabledSelect.addEventListener('change', () => {
        sendFeatureConfig({ subagent: subagentEnabledSelect.value });
    });
}
if (subagentDepthInput) {
    subagentDepthInput.addEventListener('change', () => {
        const n = parseInt(subagentDepthInput.value, 10);
        if (!Number.isFinite(n) || n < 1) {
            subagentDepthInput.value = '1';
            return;
        }
        const clamped = Math.min(n, 10);
        subagentDepthInput.value = String(clamped);
        sendFeatureConfig({ subagentMaxDepth: clamped });
    });
}
if (subagentLimitInput) {
    subagentLimitInput.addEventListener('change', () => {
        const n = parseInt(subagentLimitInput.value, 10);
        if (!Number.isFinite(n) || n < 1) {
            subagentLimitInput.value = '1';
            return;
        }
        const clamped = Math.min(n, 32);
        subagentLimitInput.value = String(clamped);
        sendFeatureConfig({ subagentMaxConcurrent: clamped });
    });
}

// ── Feature settings (board / subagents) ──
// Server-backed: the settings-modal controls and the board tab
// visibility follow the config push (data.board / data.subagent /
// data.subagentMaxDepth), so every tab stays in sync without
// localStorage. Toggling board off while the board pane is the
// active view falls back to chat; the board data is untouched.
export function applyFeatureSettings(data) {
    const boardOn = data.board === 'on';
    const subagentOn = data.subagent === 'on';
    const boardTab = document.getElementById('board-tab');
    const wasVisible = boardTab && !boardTab.hidden;
    if (boardTab && boardTab.hidden === boardOn) boardTab.hidden = !boardOn;
    const boardSel = document.getElementById('board-enabled-select');
    if (boardSel && boardSel.value !== (boardOn ? 'on' : 'off')) boardSel.value = boardOn ? 'on' : 'off';
    // The board agent prompt is meaningful only while the board
    // feature is on (like the subagent model picker).
    const boardPromptRow = document.getElementById('board-prompt-row');
    if (boardPromptRow) boardPromptRow.hidden = !boardOn;
    const subSel = document.getElementById('subagent-enabled-select');
    if (subSel && subSel.value !== (subagentOn ? 'on' : 'off')) subSel.value = subagentOn ? 'on' : 'off';
    const depthInput = document.getElementById('subagent-depth-input');
    if (depthInput && data.subagentMaxDepth > 0 && String(depthInput.value) !== String(data.subagentMaxDepth)) {
        depthInput.value = String(data.subagentMaxDepth);
    }
    const limitInput = document.getElementById('subagent-limit-input');
    if (limitInput && data.subagentMaxConcurrent > 0 && String(limitInput.value) !== String(data.subagentMaxConcurrent)) {
        limitInput.value = String(data.subagentMaxConcurrent);
    }
    if (boardOn && !wasVisible) requestBoardState();
    if (!boardOn) {
        const boardPane = document.getElementById('board-pane');
        if (boardPane && boardPane.classList.contains('active')) deps.switchMainPane('chat');
    }
    // The subagent model + effort pickers are meaningful only while
    // subagents are enabled.
    const subModelPicker = document.getElementById('subagent-model-picker');
    if (subModelPicker) subModelPicker.hidden = !subagentOn;
    const subThinkingPicker = document.getElementById('subagent-thinking-picker');
    if (subThinkingPicker) subThinkingPicker.hidden = !subagentOn;
    const ws = deps.getWs();
    const models = deps.getModels();
    if (subagentOn && !subModelPicker.hidden && isSettingsOpen()
        && (!models || models.length === 0)
        && ws && ws.readyState === WebSocket.OPEN) {
        // Just enabled while the modal is open: fetch the catalog so
        // the picker is not stuck on "No models loaded".
        ws.send(JSON.stringify({ type: 'list_models' }));
    }
}

// Sends a feature-setting change over the existing config WS message
// (the same channel the server uses to push config to us).
export function sendFeatureConfig(fields) {
    const ws = deps.getWs();
    if (!ws || ws.readyState !== WebSocket.OPEN) return;
    const pane = deps.getPane();
    ws.send(JSON.stringify(Object.assign({ type: 'config', sessionId: pane ? pane.id || '' : '' }, fields)));
}

// === Runtime config options: server-backed (config WS message) ===
// The settings-modal options below round-trip through the config
// message's ConfigFields mechanism: a change sends { type: 'config',
// configFields: [name], <prop>: value }, the server applies it (live
// or restart-staged) and pushes the current values back, which keeps
// every tab in sync. prop overrides the WSMessage property when it
// differs from the ConfigFields name (contextLimitConfig).
const RUNTIME_CONTROLS = [
    // Security
    { id: 'command-safety-select', field: 'commandSafety' },
    { id: 'command-allowlist-input', field: 'commandAllowlist' },
    { id: 'delete-approval-select', field: 'deleteApproval' },
    { id: 'command-sandbox-select', field: 'commandSandbox' },
    { id: 'command-timeout-input', field: 'commandTimeoutSecs' },
    // Context
    { id: 'context-limit-input', field: 'contextLimit', prop: 'contextLimitConfig' },
    { id: 'compact-threshold-input', field: 'compactThreshold' },
    { id: 'compact-keep-input', field: 'compactKeepRecentMessages' },
    { id: 'max-tool-bytes-input', field: 'maxToolResultBytes' },
    { id: 'compact-reserve-input', field: 'compactReserveTokens' },
    { id: 'compact-last-resort-select', field: 'compactLastResort' },
    { id: 'preserve-reasoning-select', field: 'preserveReasoning' },
    // Tools
    { id: 'web-fetch-select', field: 'webFetch' },
    { id: 'web-search-select', field: 'webSearch' },
    { id: 'web-search-backend-select', field: 'webSearchBackend' },
    { id: 'web-search-api-key-input', field: 'webSearchApiKey' },
    { id: 'web-allowed-domains-input', field: 'webAllowedDomains' },
    { id: 'web-fetch-mode-select', field: 'webFetchMode' },
    { id: 'treesitter-select', field: 'treesitter' },
    { id: 'treesitter-langs-input', field: 'treesitterLangs' },
    // Sessions
    { id: 'session-max-count-input', field: 'sessionMaxCount' },
    { id: 'session-max-age-input', field: 'sessionMaxAgeDays' },
    { id: 'approval-hold-input', field: 'webApprovalHoldSecs' },
    // Server (restart-staged)
    { id: 'web-bind-input', field: 'webBind' },
    { id: 'web-allowed-origins-input', field: 'webAllowedOrigins' },
    { id: 'web-auth-token-input', field: 'webAuthToken' },
    { id: 'web-tls-cert-input', field: 'webTLSCertFile' },
    { id: 'web-tls-key-input', field: 'webTLSKeyFile' },
    { id: 'web-max-active-input', field: 'webMaxActiveSessions' },
    { id: 'mcp-select', field: 'mcp' },
    // Prompts (configurable templates; settings Agent group)
    { id: 'board-start-prompt-input', field: 'boardStartPrompt' },
    { id: 'system-prompt-input', field: 'systemPrompt' },
    { id: 'subagent-prompt-input', field: 'subagentPrompt' },
];

// Sends one or more runtime-config changes: { field: {prop, value} }.
export function sendRuntimeConfig(changes) {
    const ws = deps.getWs();
    if (!ws || ws.readyState !== WebSocket.OPEN) return;
    const payload = { type: 'config', configFields: [] };
    for (const field of Object.keys(changes)) {
        payload.configFields.push(field);
        const { prop, value } = changes[field];
        payload[prop] = value;
    }
    ws.send(JSON.stringify(payload));
}

function wireRuntimeControl(ctrl) {
    const el = document.getElementById(ctrl.id);
    if (!el) return;
    el.addEventListener('change', () => {
        let value = el.value;
        if (el.type === 'number') {
            // Fractional fields (e.g. #compact-threshold-input, step="0.05")
            // must be parsed as floats; integer-step fields stay integers.
            const step = parseFloat(el.step || '1');
            const n = Number.isFinite(step) && step % 1 === 0
                ? parseInt(el.value, 10)
                : parseFloat(el.value);
            if (!Number.isFinite(n)) return;
            value = n;
        }
        sendRuntimeConfig({ [ctrl.field]: { prop: ctrl.prop || ctrl.field, value } });
    });
}
RUNTIME_CONTROLS.forEach(wireRuntimeControl);

// "Reset to default" for the prompt templates: send the explicit
// empty value; the server resolves it back to the built-in default
// and the next push re-populates the textarea with it.
const PROMPT_RESET_BUTTONS = [
    { id: 'board-prompt-reset-btn', field: 'boardStartPrompt' },
    { id: 'system-prompt-reset-btn', field: 'systemPrompt' },
    { id: 'subagent-prompt-reset-btn', field: 'subagentPrompt' },
];
for (const b of PROMPT_RESET_BUTTONS) {
    const el = document.getElementById(b.id);
    if (!el) continue;
    el.addEventListener('click', () => sendRuntimeConfig({ [b.field]: { prop: b.field, value: '' } }));
}

// Applies the runtime-config values from the server push to the
// controls, updates the secret placeholders, and renders the
// restart banner. Values not present in the push are left as-is.
export function applyRuntimeSettings(data) {
    for (const ctrl of RUNTIME_CONTROLS) {
        const el = document.getElementById(ctrl.id);
        if (!el) continue;
        const v = data[ctrl.prop || ctrl.field];
        if (v === undefined) continue;
        if (el.type === 'number') {
            if (String(el.value) !== String(v)) el.value = String(v);
        } else if (el.value !== String(v)) {
            el.value = String(v);
        }
    }
    const searchKeyEl = document.getElementById('web-search-api-key-input');
    if (searchKeyEl) searchKeyEl.placeholder = data.webSearchApiKeySet ? 'API key (blank keeps stored)' : 'API key';
    const authTokenEl = document.getElementById('web-auth-token-input');
    if (authTokenEl) authTokenEl.placeholder = data.webAuthTokenSet ? 'Token set (blank keeps stored)' : 'Auth token';
    const banner = document.getElementById('restart-banner');
    if (banner) {
        if (Array.isArray(data.restartRequired) && data.restartRequired.length > 0) {
            banner.textContent = '⚠ Restart gogen for these to take effect: ' + data.restartRequired.join(', ');
            banner.hidden = false;
        } else {
            banner.hidden = true;
        }
    }
    // Subagent default model: the server pushes the current value
    // ("" = inherit, included even when empty so a clear by another
    // tab syncs here too).
    if (data.subagentModel !== undefined) {
        deps.setSubagentModel(data.subagentModel || '');
        if (isSettingsOpen()) {
            // Model-aware effort options follow the pushed model.
            deps.renderSubagentPicker();
        }
    }
    // Subagent reasoning effort: the server pushes the current
    // value ("" = inherit, pointer-carried so a clear by another
    // tab syncs here too), like the model above.
    if (data.subagentThinkingLevel !== undefined) {
        deps.setSubagentThinkingLevel(data.subagentThinkingLevel || '');
        if (isSettingsOpen()) deps.renderSubagentPicker();
    }
    // Board start prompt template (resolved by the server: empty
    // config → built-in default): the "Start agent" popover's pen
    // editor pre-fills from it.
    if (data.boardStartPrompt !== undefined) {
        setBoardStartPrompt(data.boardStartPrompt || '');
    }
}

// === MCP test: server-backed (test_mcp WS message) ===
// The configured server list follows the config push (applyMCPSettings);
// row Test buttons probe a saved server by name (stored command/args/
// env resolved server-side), the form probes a typed command/args/env
// before adding it to the config file by hand. Never registers anything.
const mcpServerListEl = document.getElementById('mcp-server-list');
const mcpTestNameInput = document.getElementById('mcp-test-name');
const mcpTestCommandInput = document.getElementById('mcp-test-command');
const mcpTestArgsInput = document.getElementById('mcp-test-args');
const mcpTestEnvInput = document.getElementById('mcp-test-env');
const mcpTestBtn = document.getElementById('mcp-test-btn');
const mcpTestResult = document.getElementById('mcp-test-result');

function renderMCPServerList(servers) {
    if (!mcpServerListEl || !Array.isArray(servers)) return;
    mcpServerListEl.innerHTML = '';
    if (servers.length === 0) {
        mcpServerListEl.textContent = 'No MCP servers configured (mcp_servers in the config file).';
        mcpServerListEl.className = 'settings-note';
        return;
    }
    mcpServerListEl.className = 'mcp-server-list';
    for (const s of servers) {
        const row = document.createElement('div');
        row.className = 'provider-row';
        const info = document.createElement('span');
        info.className = 'provider-row-info';
        info.textContent = s.name + '  ·  ' + s.command + (s.args && s.args.length ? '  ' + s.args.join(' ') : '') + (s.envSet ? '  ·  env •••' : '');
        row.appendChild(info);
        const actions = document.createElement('span');
        actions.className = 'provider-row-actions';
        const testBtn = document.createElement('button');
        testBtn.type = 'button';
        testBtn.textContent = 'Test';
        testBtn.title = 'Test connectivity and list tools';
        testBtn.addEventListener('click', () => {
            showMCPTest('Testing ' + s.name + '…', '');
            sendMCPTest({ name: s.name });
        });
        actions.appendChild(testBtn);
        row.appendChild(actions);
        mcpServerListEl.appendChild(row);
    }
}

export function showMCPTest(text, kind) {
    if (!mcpTestResult) return;
    mcpTestResult.textContent = text;
    mcpTestResult.className = 'settings-note' + (kind ? ' ' + kind : '');
}

function sendMCPTest(req) {
    const ws = deps.getWs();
    if (!ws || ws.readyState !== WebSocket.OPEN) return;
    ws.send(JSON.stringify({ type: 'test_mcp', mcpTest: req }));
}

// "KEY=VALUE KEY2=VALUE2" → { KEY: VALUE, KEY2: VALUE2 } (invalid
// parts silently dropped).
function parseEnvInput(raw) {
    const env = {};
    for (const part of String(raw || '').trim().split(/\s+/)) {
        if (!part) continue;
        const eq = part.indexOf('=');
        if (eq <= 0) continue;
        env[part.slice(0, eq)] = part.slice(eq + 1);
    }
    return env;
}

export function applyMCPSettings(data) {
    if (Array.isArray(data.mcpServers)) {
        renderMCPServerList(data.mcpServers);
    }
}

if (mcpTestBtn) {
    mcpTestBtn.addEventListener('click', () => {
        showMCPTest('Testing…', '');
        const rawArgs = mcpTestArgsInput.value.trim();
        sendMCPTest({
            name: mcpTestNameInput.value.trim(),
            command: mcpTestCommandInput.value.trim(),
            args: rawArgs ? rawArgs.split(/\s+/) : [],
            env: parseEnvInput(mcpTestEnvInput.value),
        });
    });
}

// === Providers: server-backed list (config WS message) ===
// The list follows the config push (applyProviderSettings); the
// add/edit/delete/test actions round-trip through provider_save /
// provider_delete / test_provider. Keys are never pushed — only the
// apiKeySet flag — and the storage warning renders the actual config
// file path from the server.
const providerListEl = document.getElementById('provider-list');
const providerWarningPath = document.getElementById('provider-config-path');
const providerNameInput = document.getElementById('provider-name');
const providerBaseURLInput = document.getElementById('provider-base-url');
const providerKeyInput = document.getElementById('provider-api-key');
const providerModelInput = document.getElementById('provider-model');
const providerAddBtn = document.getElementById('provider-add-btn');
const providerTestBtn = document.getElementById('provider-test-btn');
const providerTestResult = document.getElementById('provider-test-result');

function sendProviderOp(type, fields) {
    const ws = deps.getWs();
    if (!ws || ws.readyState !== WebSocket.OPEN) return;
    ws.send(JSON.stringify({ type, providerOp: fields }));
}

export function showProviderTest(text, kind) {
    if (!providerTestResult) return;
    providerTestResult.textContent = text;
    providerTestResult.className = 'settings-note' + (kind ? ' ' + kind : '');
}

function renderProviderList(providers) {
    if (!providerListEl || !Array.isArray(providers)) return;
    providerListEl.innerHTML = '';
    for (const p of providers) {
        const row = document.createElement('div');
        row.className = 'provider-row';
        const info = document.createElement('span');
        info.className = 'provider-row-info';
        info.textContent = p.name + (p.baseUrl ? '  ·  ' + p.baseUrl : '') + (p.apiKeySet ? '  ·  key •••' : '');
        row.appendChild(info);
        const actions = document.createElement('span');
        actions.className = 'provider-row-actions';
        const testBtn = document.createElement('button');
        testBtn.type = 'button';
        testBtn.textContent = 'Test';
        testBtn.title = 'Test connectivity and list models';
        testBtn.addEventListener('click', () => {
            showProviderTest('Testing ' + p.name + '…', '');
            sendProviderOp('test_provider', { name: p.name, baseUrl: p.baseUrl, model: p.model });
        });
        actions.appendChild(testBtn);
        const editBtn = document.createElement('button');
        editBtn.type = 'button';
        editBtn.textContent = 'Edit';
        editBtn.addEventListener('click', () => {
            providerNameInput.value = p.name;
            providerBaseURLInput.value = p.baseUrl || '';
            providerKeyInput.value = '';
            providerModelInput.value = p.model || '';
            providerKeyInput.placeholder = p.apiKeySet ? 'API key (blank keeps stored)' : 'API key';
            providerNameInput.focus();
        });
        actions.appendChild(editBtn);
        const delBtn = document.createElement('button');
        delBtn.type = 'button';
        delBtn.textContent = 'Delete';
        delBtn.disabled = !p.deletable;
        if (p.deletable) {
            delBtn.title = 'Remove this provider';
            delBtn.addEventListener('click', () => {
                if (!confirm('Delete provider "' + p.name + '"? Its models will disappear from the picker.')) return;
                showProviderTest('', '');
                sendProviderOp('provider_delete', { name: p.name });
            });
        } else {
            delBtn.title = 'The default profile is built from the legacy config fields';
        }
        actions.appendChild(delBtn);
        row.appendChild(actions);
        providerListEl.appendChild(row);
    }
}

// Last-seen provider list (JSON): a change means a provider was
// added/edited/deleted, so the model catalog must be re-fetched —
// the picker's availableModels is otherwise a stale snapshot.
let lastProvidersJson = null;

export function applyProviderSettings(data) {
    if (Array.isArray(data.providers)) {
        const json = JSON.stringify(data.providers);
        if (json !== lastProvidersJson) {
            lastProvidersJson = json;
            // Re-request the aggregated catalog so the new provider's
            // models appear in the picker (and deleted ones drop).
            deps.requestModels();
        }
        renderProviderList(data.providers);
    }
    if (data.configFilePath && providerWarningPath) {
        providerWarningPath.textContent = data.configFilePath;
    }
}

function testProviderForm() {
    const name = providerNameInput.value.trim();
    const baseUrl = providerBaseURLInput.value.trim();
    const model = providerModelInput.value.trim();
    showProviderTest('Testing…', '');
    // Testing a saved provider by name uses its stored key; the
    // add-form test carries the typed endpoint (key included).
    sendProviderOp('test_provider', { name, baseUrl, apiKey: providerKeyInput.value, model });
}

if (providerAddBtn) {
    providerAddBtn.addEventListener('click', () => {
        const name = providerNameInput.value.trim();
        if (!name) {
            deps.showToast('Provider name is required', 'error');
            return;
        }
        const baseUrl = providerBaseURLInput.value.trim();
        const apiKey = providerKeyInput.value;
        const model = providerModelInput.value.trim();
        sendProviderOp('provider_save', { name, baseUrl, apiKey, model });
        providerKeyInput.value = '';
        providerKeyInput.placeholder = 'API key (blank keeps stored)';
        showProviderTest('', '');
    });
}
if (providerTestBtn) {
    providerTestBtn.addEventListener('click', testProviderForm);
}

// === Theme: detect system preference, allow override ===
const prefersDark = window.matchMedia('(prefers-color-scheme: dark)');
const savedTheme = localStorage.getItem('gogen-theme') || 'auto';

function resolveTheme(preference) {
    if (preference === 'auto' || !preference) return prefersDark.matches ? 'dark' : 'light';
    return preference;
}

function applyTheme(preference) {
    const resolved = resolveTheme(preference);
    document.documentElement.classList.toggle('light', resolved === 'light');
    localStorage.setItem('gogen-theme', preference);
    // Keep select in sync
    if (themeSelect.value !== preference) themeSelect.value = preference;
    // Address-bar color follows the theme.
    const themeMeta = document.querySelector('meta[name="theme-color"]');
    if (themeMeta) themeMeta.setAttribute('content', resolved === 'light' ? '#ffffff' : '#1e1e1e');
    // Switch Monaco theme to match
    setMonacoTheme(resolved === 'light');
}

// Initialize
themeSelect.value = savedTheme;
applyTheme(savedTheme);

// Listen for OS theme changes (only affects "auto" mode)
prefersDark.addEventListener('change', () => {
    const pref = localStorage.getItem('gogen-theme') || 'auto';
    if (pref === 'auto') applyTheme('auto');
});

themeSelect.addEventListener('change', () => {
    applyTheme(themeSelect.value);
});

// === File-click behavior: persist and apply ===
const fileClickSelect = document.getElementById('file-click-behavior');
const savedFileClick = localStorage.getItem('gogen_file_click_behavior') || 'open';
fileClickSelect.value = savedFileClick;
fileClickSelect.addEventListener('change', () => {
    localStorage.setItem('gogen_file_click_behavior', fileClickSelect.value);
});
// Listen for storage changes from other tabs
window.addEventListener('storage', (e) => {
    if (e.key === 'gogen_file_click_behavior') {
        const val = e.newValue || 'open';
        if (fileClickSelect.value !== val) fileClickSelect.value = val;
    }
});

// === Chat diff viewer: static tokenizer pre (default) or full Monaco editor ===
const diffViewerSelect = document.getElementById('chat-diff-viewer');
const savedDiffViewer = localStorage.getItem('gogen_chat_diff_viewer') || 'tokenizer';
diffViewerSelect.value = savedDiffViewer;
diffViewerSelect.addEventListener('change', () => {
    localStorage.setItem('gogen_chat_diff_viewer', diffViewerSelect.value);
});
window.addEventListener('storage', (e) => {
    if (e.key === 'gogen_chat_diff_viewer') {
        const val = e.newValue || 'tokenizer';
        if (diffViewerSelect.value !== val) diffViewerSelect.value = val;
    }
});

// The chat diff viewer is either a static line-numbered <pre>
// (default) or a full Monaco editor — a user setting
// (gogen_chat_diff_viewer). Reads localStorage directly so it works
// regardless of when the settings UI initializes.
export function diffViewerMode() {
    return localStorage.getItem('gogen_chat_diff_viewer') === 'monaco' ? 'monaco' : 'tokenizer';
}

// === Editor preferences: persist and apply to the live Monaco editor ===
const editorMinimapSelect = document.getElementById('editor-minimap');
const editorWordwrapSelect = document.getElementById('editor-wordwrap');
const editorStickySelect = document.getElementById('editor-sticky');
const editorFontsizeInput = document.getElementById('editor-fontsize');

function clampFontSize(v) {
    // Mirror getEditorPrefs() clamping so the control can never show a
    // value the editor won't actually apply.
    const min = parseInt(editorFontsizeInput?.min || '8', 10);
    const max = parseInt(editorFontsizeInput?.max || '32', 10);
    let n = parseInt(v, 10);
    if (!Number.isFinite(n)) n = 13;
    return Math.max(min, Math.min(max, n));
}

function initEditorPrefControl(control, key, dflt) {
    if (!control) return;
    const saved = localStorage.getItem(key) || dflt;
    control.value = saved;
    control.addEventListener('change', () => {
        if (control.type === 'number') {
            // Keep the visible value in sync with what the editor will
            // actually apply (getEditorPrefs clamps to [min, max]).
            control.value = String(clampFontSize(control.value));
        }
        localStorage.setItem(key, control.value);
        applyEditorPrefs();
    });
}
initEditorPrefControl(editorMinimapSelect, 'gogen_editor_minimap', 'off');
initEditorPrefControl(editorWordwrapSelect, 'gogen_editor_wordwrap', 'on');
initEditorPrefControl(editorStickySelect, 'gogen_editor_sticky', 'on');
initEditorPrefControl(editorFontsizeInput, 'gogen_editor_fontsize', '13');
// Keep editor prefs in sync across tabs (mirrors the other settings).
window.addEventListener('storage', (e) => {
    let changed = false;
    if (e.key === 'gogen_editor_minimap' && editorMinimapSelect) {
        editorMinimapSelect.value = e.newValue || 'off';
        changed = true;
    } else if (e.key === 'gogen_editor_wordwrap' && editorWordwrapSelect) {
        editorWordwrapSelect.value = e.newValue || 'on';
        changed = true;
    } else if (e.key === 'gogen_editor_sticky' && editorStickySelect) {
        editorStickySelect.value = e.newValue || 'on';
        changed = true;
    } else if (e.key === 'gogen_editor_fontsize' && editorFontsizeInput) {
        editorFontsizeInput.value = String(clampFontSize(e.newValue || '13'));
        changed = true;
    }
    if (changed) applyEditorPrefs();
});

// === Desktop notifications ===
const notificationsSelect = document.getElementById('notifications-select');
const savedNotifications = localStorage.getItem('gogen_notifications') || 'off';
notificationsSelect.value = savedNotifications;
notificationsSelect.addEventListener('change', () => {
    setNotificationPref(notificationsSelect.value);
});
window.addEventListener('storage', (e) => {
    if (e.key === 'gogen_notifications') {
        const val = e.newValue || 'off';
        if (notificationsSelect.value !== val) notificationsSelect.value = val;
    }
});

// === Show reply model (Settings → Chat): per-bubble model chip ===
const showReplyModelSelect = document.getElementById('show-reply-model');
const savedShowReplyModel = localStorage.getItem('gogen_show_reply_model') || 'off';
if (showReplyModelSelect) {
    showReplyModelSelect.value = savedShowReplyModel;
    showReplyModelSelect.addEventListener('change', () => {
        localStorage.setItem('gogen_show_reply_model', showReplyModelSelect.value);
        deps.applyReplyModelChips();
    });
}
window.addEventListener('storage', (e) => {
    if (e.key === 'gogen_show_reply_model') {
        const val = e.newValue || 'off';
        if (showReplyModelSelect && showReplyModelSelect.value !== val) {
            showReplyModelSelect.value = val;
        }
        deps.applyReplyModelChips();
    }
});

// "Show reply model" (Settings → Chat): true when the per-bubble
// model chip preference is on. app.js's per-bubble render hook
// reads the preference through this (it stays in app.js).
export function getShowReplyModelPref() {
    return (localStorage.getItem('gogen_show_reply_model') || 'off') === 'on';
}

// === Accent color: persist and apply ===
const accentInput = document.getElementById('accent-color-input');

// The soft accent variant is derived in CSS via color-mix, so JS only
// needs to set the base --user-accent (see styles.css).
function applyAccentColor(hex) {
    if (!hex) {
        document.documentElement.style.removeProperty('--user-accent');
        return;
    }
    document.documentElement.style.setProperty('--user-accent', hex);
}

const savedAccent = localStorage.getItem('gogen-accent-color') || '';
if (savedAccent) {
    accentInput.value = savedAccent;
    applyAccentColor(savedAccent);
}

accentInput.addEventListener('input', function () {
    const hex = accentInput.value;
    applyAccentColor(hex);
    localStorage.setItem('gogen-accent-color', hex);
});

window.addEventListener('storage', function (e) {
    if (e.key === 'gogen-accent-color') {
        const val = e.newValue || '';
        if (accentInput.value !== val) accentInput.value = val;
        applyAccentColor(val);
    }
});

// === Accent color: reset to theme default ===
document.getElementById('accent-reset-btn').addEventListener('click', function () {
    localStorage.removeItem('gogen-accent-color');
    document.documentElement.style.removeProperty('--user-accent');
    // Reset picker to the current theme's default accent
    const isLight = document.documentElement.classList.contains('light');
    accentInput.value = isLight ? '#0066cc' : '#569cd6';
    accentInput.blur();
});

export function getNotificationPref() {
    return localStorage.getItem('gogen_notifications') || 'off';
}

// Persists the notification preference, keeps the settings select in
// sync and requests permission when turning on (the command palette's
// toggle command goes through this too).
export function setNotificationPref(value) {
    localStorage.setItem('gogen_notifications', value);
    if (notificationsSelect.value !== value) notificationsSelect.value = value;
    if (value !== 'off') {
        requestNotificationPermission();
    }
}

function requestNotificationPermission() {
    if (!('Notification' in window)) return;
    if (Notification.permission === 'granted') return;
    if (Notification.permission === 'denied') return;
    Notification.requestPermission();
}

/**
 * Send a desktop notification if the user has enabled them.
 * @param {string} title
 * @param {string} [body]
 * @param {string} [tag] - dedup tag (prevents stacking)
 */
export function sendNotification(title, body, tag) {
    const pref = getNotificationPref();
    if (pref === 'off') return;
    if (!('Notification' in window)) return;
    if (Notification.permission !== 'granted') return;

    // "background" mode: only notify when the tab is not focused
    if (pref === 'background' && document.hasFocus()) return;

    try {
        const opts = { tag: tag || 'gogen-notification' };
        if (body) opts.body = body;
        new Notification(title, opts);
    } catch (_) {
        // Service worker or permission issue — silently ignore.
    }
}

// Restore notification permission on load if preference is non-off
if (getNotificationPref() !== 'off') {
    requestNotificationPermission();
}
