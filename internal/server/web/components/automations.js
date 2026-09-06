// Automations tab for the GoGen web UI: renders the file-based cron
// scheduler's saved jobs (prompt + working dir + schedule), their run
// history, and the create form.
//
// Server-backed like the board: the tab renders from automations_state
// broadcasts (server→client, pushed after every mutation) and sends
// automations_op messages (client→server) for list/runs/create/enable/
// disable/delete. Runs are requested lazily per job (expand a row) and
// arrive as automations_runs. The tab is visible only while the
// `automations` feature flag is on (applyFeatureSettings), exactly like
// the board tab's gating.
//
// Firing itself is unchanged: the host-side scheduler sweeps the store and
// starts headless `-p` runs (internal/automation). This tab only manages
// records and shows status/history.
//
// Wiring: app.js calls initAutomations(deps) once at startup.
//   deps.getWs() — the chat WebSocket (or null)

import { icon } from '/components/icons.js';

let deps = null;

export function initAutomations(d) {
    deps = d;
    initAutomationsTab();
}

// ── State ──

let lastAutomationsState = null;
// Runs are lazy: one payload per automation id (the expanded row's body).
const runsById = new Map();
// The run rows currently expanded (persisted across re-renders).
const expanded = new Set();
// The id being edited in the form (null = the form creates a new job).
let editingId = null;

// ── Tab visibility + pane helpers ──

export function automationsTabVisible() {
    const t = document.getElementById('automations-tab');
    return !!t && !t.hidden;
}

export function automationsPaneVisible() {
    const p = document.getElementById('automations-pane');
    return !!p && p.classList.contains('active');
}

export function requestAutomationsState() {
    sendAutomationsOp({ action: 'list' });
}

export function sendAutomationsOp(op) {
    const s = deps ? deps.getWs() : null;
    if (!s || s.readyState !== WebSocket.OPEN) return;
    s.send(JSON.stringify({ type: 'automations_op', automationsOp: op }));
}

// ── Server pushes ──

export function handleAutomationsState(data) {
    const snap = data.automationsState;
    if (!snap) return;
    lastAutomationsState = snap;
    // Drop history of deleted jobs.
    const known = new Set((snap.automations || []).map((a) => a.id));
    for (const id of [...runsById.keys()]) {
        if (!known.has(id)) {
            runsById.delete(id);
            expanded.delete(id);
        }
    }
    if (automationsPaneVisible()) renderAutomations();
}

export function handleAutomationsRuns(data) {
    const payload = data.automationsRuns;
    if (!payload || !payload.automationId) return;
    runsById.set(payload.automationId, payload.runs || []);
    if (expanded.has(payload.automationId) && automationsPaneVisible()) {
        renderRunRows(payload.automationId);
    }
}

// ── Rendering ──

export function renderAutomations() {
    const list = document.getElementById('automations-list');
    if (!list) return;
    const snap = lastAutomationsState;
    if (!snap) {
        requestAutomationsState();
        return;
    }
    list.replaceChildren();

    if (!snap.storeOk) {
        const err = document.createElement('div');
        err.className = 'automations-note error';
        err.textContent = 'Automation store unavailable: ' + (snap.storeError || 'unknown error');
        list.appendChild(err);
        return;
    }
    const autos = snap.automations || [];
    if (autos.length === 0) {
        const empty = document.createElement('div');
        empty.className = 'automations-note';
        empty.textContent = 'No automations yet. Create one below, or with `gogen automation create`.';
        list.appendChild(empty);
        return;
    }
    for (const a of autos) {
        list.appendChild(buildAutomationRow(a));
        if (expanded.has(a.id)) {
            list.appendChild(buildRunsRow(a.id));
        }
    }
}

function buildAutomationRow(a) {
    const row = document.createElement('div');
    row.className = 'automations-row' + (a.enabled ? '' : ' paused');
    row.dataset.id = a.id;

    const main = document.createElement('div');
    main.className = 'automations-row-main';

    const title = document.createElement('div');
    title.className = 'automations-row-title';
    title.textContent = a.title;
    main.appendChild(title);

    const meta = document.createElement('div');
    meta.className = 'automations-row-meta';
    meta.textContent = describeSchedule(a.schedule) + '  ·  ' + (a.workingDir || '');
    main.appendChild(meta);

    const status = document.createElement('div');
    status.className = 'automations-row-status';
    status.textContent = describeLastRun(a);
    main.appendChild(status);
    row.appendChild(main);

    const next = document.createElement('div');
    next.className = 'automations-row-next';
    next.textContent = a.enabled ? 'next: ' + formatTime(a.nextRunAt) : 'paused';
    row.appendChild(next);

    const actions = document.createElement('div');
    actions.className = 'automations-row-actions';

    const runsBtn = document.createElement('button');
    runsBtn.type = 'button';
    runsBtn.title = 'Show run history';
    runsBtn.innerHTML = icon(expanded.has(a.id) ? 'chevron-up' : 'chevron-down') + '<span>Runs</span>';
    runsBtn.addEventListener('click', () => toggleRuns(a.id));
    actions.appendChild(runsBtn);

    const editBtn = document.createElement('button');
    editBtn.type = 'button';
    editBtn.title = 'Edit this automation';
    editBtn.innerHTML = icon('pen') + '<span>Edit</span>';
    editBtn.addEventListener('click', () => openEditForm(a));
    actions.appendChild(editBtn);

    const toggleBtn = document.createElement('button');
    toggleBtn.type = 'button';
    toggleBtn.title = a.enabled ? 'Pause (disable)' : 'Enable';
    toggleBtn.innerHTML = icon(a.enabled ? 'x' : 'check') + '<span>' + (a.enabled ? 'Pause' : 'Enable') + '</span>';
    toggleBtn.addEventListener('click', () => sendAutomationsOp({ action: a.enabled ? 'disable' : 'enable', id: a.id }));
    actions.appendChild(toggleBtn);

    const delBtn = document.createElement('button');
    delBtn.type = 'button';
    delBtn.title = 'Delete (with its run history)';
    delBtn.innerHTML = icon('trash') + '<span>Delete</span>';
    delBtn.addEventListener('click', () => {
        if (!confirm('Delete automation "' + a.title + '" and its run history?')) return;
        sendAutomationsOp({ action: 'delete', id: a.id });
    });
    actions.appendChild(delBtn);

    row.appendChild(actions);
    return row;
}

function buildRunsRow(id) {
    const wrap = document.createElement('div');
    wrap.className = 'automations-runs';
    wrap.dataset.for = id;
    const runs = runsById.get(id);
    if (!runs) {
        wrap.textContent = 'Loading run history…';
        return wrap;
    }
    if (runs.length === 0) {
        wrap.textContent = 'No runs recorded yet.';
        return wrap;
    }
    const table = document.createElement('table');
    table.className = 'automations-runs-table';
    const head = table.createTHead().insertRow();
    for (const label of ['Planned', 'Status', 'Session', 'Detail']) {
        const th = document.createElement('th');
        th.textContent = label;
        head.appendChild(th);
    }
    const body = table.createTBody();
    for (const r of runs) {
        const tr = body.insertRow();
        tr.insertCell().textContent = formatTime(r.plannedAt);
        const statusCell = tr.insertCell();
        statusCell.textContent = r.status;
        statusCell.className = 'run-status run-' + String(r.status || '').replace(/[^a-z]/g, '');
        tr.insertCell().textContent = r.sessionId || '—';
        const detail = tr.insertCell();
        detail.className = 'run-detail';
        detail.textContent = r.detail || '';
    }
    wrap.appendChild(table);
    return wrap;
}

// renderRunRows re-renders just the expanded runs block for one automation
// (a runs payload arrived).
function renderRunRows(id) {
    const wrap = document.querySelector('.automations-runs[data-for="' + id + '"]');
    if (!wrap) return;
    const fresh = buildRunsRow(id);
    wrap.replaceWith(fresh);
}

function toggleRuns(id) {
    if (expanded.has(id)) {
        expanded.delete(id);
    } else {
        expanded.add(id);
        if (!runsById.has(id)) {
            sendAutomationsOp({ action: 'runs', id });
        }
    }
    renderAutomations();
}

// ── Formatting helpers ──

function describeSchedule(s) {
    if (!s) return '?';
    const zone = s.timezone || 'local';
    switch (s.kind) {
        case 'once': return 'once ' + (s.at || '');
        case 'hourly': return 'hourly :' + String(s.minute).padStart(2, '0') + ' ' + zone;
        case 'daily': return 'daily ' + (s.time || '09:00') + ' ' + zone;
        case 'weekdays': return 'weekdays ' + (s.time || '09:00') + ' ' + zone;
        case 'weekly': return 'weekly ' + weekdayNames(s.weekdays) + ' ' + (s.time || '09:00') + ' ' + zone;
        default: return s.kind;
    }
}

function weekdayNames(days) {
    const names = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];
    return (days || []).map((d) => names[d] || '?').join(', ');
}

function describeLastRun(a) {
    if (!a.lastRunStatus) return 'never run';
    return 'last: ' + a.lastRunStatus + ' ' + formatTime(a.lastRunAt);
}

function formatTime(t) {
    if (!t) return '—';
    const d = new Date(t);
    if (Number.isNaN(d.getTime())) return t;
    const pad = (n) => String(n).padStart(2, '0');
    return d.getFullYear() + '-' + pad(d.getMonth() + 1) + '-' + pad(d.getDate()) + ' ' +
        pad(d.getHours()) + ':' + pad(d.getMinutes());
}

// ── Create / edit form ──
// One form serves both: null editingId creates; a set editingId edits that
// job (fields prefilled, submit sends the update op with the full schedule —
// the store keeps next_run_at when only non-schedule fields changed).

function initAutomationsTab() {
    const tab = document.getElementById('automations-tab');
    const pane = document.getElementById('automations-pane');
    if (!tab || !pane) return;

    document.getElementById('automations-new-btn').addEventListener('click', () => {
        const form = document.getElementById('automations-form');
        if (!form.hidden && editingId === null) {
            form.hidden = true; // toggle the create form closed
            return;
        }
        openCreateForm();
    });
    document.getElementById('automations-refresh-btn').addEventListener('click', requestAutomationsState);

    document.getElementById('automations-form-cancel').addEventListener('click', () => {
        document.getElementById('automations-form').hidden = true;
    });

    const kindSelect = document.getElementById('automations-form-kind');
    kindSelect.addEventListener('change', syncFormFields);
    syncFormFields();

    document.getElementById('automations-form').addEventListener('submit', (e) => {
        e.preventDefault();
        submitForm();
    });
}

function openCreateForm() {
    editingId = null;
    const form = document.getElementById('automations-form');
    form.hidden = false;
    setFormTitle('New automation');
    setSubmitLabel('Create automation');
    // Defaults for a fresh form (empty dir = the workspace working dir).
    setFormValues({ title: '', prompt: '', dir: '', kind: 'daily', time: '09:00', minute: '0', weekdays: '', at: '', timezone: '' });
    document.getElementById('automations-form-startdisabled').checked = false;
    syncFormFields();
    document.getElementById('automations-form-title').focus();
}

function openEditForm(a) {
    editingId = a.id;
    const form = document.getElementById('automations-form');
    form.hidden = false;
    setFormTitle('Edit: ' + a.title);
    setSubmitLabel('Save changes');
    setFormValues({
        title: a.title || '',
        prompt: a.prompt || '',
        dir: a.workingDir || '',
        kind: (a.schedule && a.schedule.kind) || 'daily',
        time: (a.schedule && a.schedule.time) || '',
        minute: a.schedule ? String(a.schedule.minute || 0) : '0',
        weekdays: a.schedule && a.schedule.weekdays ? (a.schedule.weekdays || []).join(',') : '',
        at: (a.schedule && a.schedule.at) || '',
        timezone: (a.schedule && a.schedule.timezone) || '',
    });
    document.getElementById('automations-form-startdisabled').checked = !a.enabled;
    syncFormFields();
    document.getElementById('automations-form-title').focus();
    // Bring the pane forward when the edit is triggered from elsewhere.
    if (typeof deps.switchMainPane === 'function') deps.switchMainPane('automations');
}

function setFormTitle(text) {
    const el = document.getElementById('automations-form-title-heading');
    if (el) el.textContent = text;
}

function setSubmitLabel(text) {
    const btn = document.querySelector('#automations-form button[type="submit"]');
    if (btn) btn.textContent = text;
}

function setFormValues(v) {
    document.getElementById('automations-form-title').value = v.title;
    document.getElementById('automations-form-prompt').value = v.prompt;
    document.getElementById('automations-form-dir').value = v.dir;
    document.getElementById('automations-form-kind').value = v.kind;
    document.getElementById('automations-form-time').value = v.time;
    document.getElementById('automations-form-minute').value = v.minute;
    document.getElementById('automations-form-weekdays').value = v.weekdays;
    document.getElementById('automations-form-at').value = v.at;
    document.getElementById('automations-form-timezone').value = v.timezone;
}

// syncFormFields shows only the schedule fields the selected kind uses
// (mirrors the CLI's exactly-one selector).
function syncFormFields() {
    const kind = document.getElementById('automations-form-kind').value;
    const show = (id, on) => { document.getElementById(id).hidden = !on; };
    show('automations-form-at-row', kind === 'once');
    show('automations-form-minute-row', kind === 'hourly');
    show('automations-form-time-row', kind === 'daily' || kind === 'weekdays' || kind === 'weekly');
    show('automations-form-weekdays-row', kind === 'weekly');
}

function submitForm() {
    const val = (id) => document.getElementById(id).value.trim();
    const kind = document.getElementById('automations-form-kind').value;
    const op = {
        title: val('automations-form-title'),
        prompt: val('automations-form-prompt'),
        workingDir: val('automations-form-dir'),
        kind,
        timezone: val('automations-form-timezone'),
        time: val('automations-form-time'),
        at: val('automations-form-at'),
        minute: parseInt(document.getElementById('automations-form-minute').value, 10) || 0,
        startDisabled: document.getElementById('automations-form-startdisabled').checked,
    };
    if (kind === 'weekly') {
        op.weekdays = parseWeekdaysInput(val('automations-form-weekdays'));
        if (!op.weekdays) {
            showToastAutomations('Weekdays must be 0–6 (e.g. 1,3,5)', 'error');
            return;
        }
    }
    if (editingId !== null) {
        op.action = 'update';
        op.id = editingId;
    } else {
        op.action = 'create';
    }
    sendAutomationsOp(op);
    document.getElementById('automations-form').hidden = true;
}

// parseWeekdaysInput parses "1,3,5" (0 = Sunday … 6 = Saturday); null on
// invalid input.
function parseWeekdaysInput(raw) {
    const seen = new Set();
    for (const part of String(raw || '').split(',')) {
        const t = part.trim();
        if (!t) continue;
        const d = parseInt(t, 10);
        if (!Number.isInteger(d) || d < 0 || d > 6) return null;
        seen.add(d);
    }
    if (seen.size === 0) return null;
    return [...seen].sort((a, b) => a - b);
}

function showToastAutomations(message, kind) {
    // The toast helper lives in app.js; keep the dependency surface small
    // by falling back to the global notice path (app.js injects showToast
    // through initAutomations deps when available).
    if (deps && deps.showToast) {
        deps.showToast(message, kind);
    }
}
