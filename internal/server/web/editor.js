// Monaco editor workspace for GoGen web UI.

import monaco from '/monaco/editor.bundle.js';

// Must be set before any editor/worker is created.
self.MonacoEnvironment = {
  getWorker(_workerId, label) {
    const map = {
      json: '/monaco/json.worker.js',
      css: '/monaco/css.worker.js',
      scss: '/monaco/css.worker.js',
      less: '/monaco/css.worker.js',
      html: '/monaco/html.worker.js',
      handlebars: '/monaco/html.worker.js',
      razor: '/monaco/html.worker.js',
      typescript: '/monaco/ts.worker.js',
      javascript: '/monaco/ts.worker.js',
    };
    const url = map[label] || '/monaco/editor.worker.js';
    return new Worker(url, { type: 'module' });
  },
};

export const GOGEN_UI = {
  // Flip to false for inline (unified-style) DiffEditor rendering.
  diffRenderSideBySide: true,
  maxOpenTabs: 20,
};

const buffers = new Map(); // path -> { model, viewState, savedVersionId, lastUsed }
let openOrder = []; // paths in tab order
let activePath = null;
let mode = 'edit'; // 'edit' | 'diff'
let editor = null;
let diffEditor = null;
let monacoReady = false;
let wsRef = null;
let reqCounter = 0;
const pendingReqs = new Map();
const chatEditors = new Set(); // disposable Monaco editors in chat tool cards

function $(id) {
  return document.getElementById(id);
}

export function setWebSocket(ws) {
  wsRef = ws;
}

export function handleServerMessage(data) {
  if (!data || !data.requestId || !pendingReqs.has(data.requestId)) return false;
  const p = pendingReqs.get(data.requestId);
  pendingReqs.delete(data.requestId);
  if (data.error) p.reject(new Error(data.error));
  else p.resolve(data);
  return true;
}

function wsRequest(type, payload = {}) {
  return new Promise((resolve, reject) => {
    if (!wsRef || wsRef.readyState !== WebSocket.OPEN) {
      reject(new Error('not connected'));
      return;
    }
    const requestId = `ed-${++reqCounter}`;
    pendingReqs.set(requestId, { resolve, reject });
    wsRef.send(JSON.stringify({ type, requestId, ...payload }));
  });
}

export async function initMonaco() {
  if (monacoReady) return monaco;

  // Monaco 0.52+ no longer ships a built-in unified-diff highlighter.
  if (!monaco.languages.getLanguages().some((l) => l.id === 'diff')) {
    monaco.languages.register({ id: 'diff' });
  }
  monaco.languages.setMonarchTokensProvider('diff', {
    tokenizer: {
      root: [
        [/^\+\+\+.*$/, 'meta.diff.header'],
        [/^---.*$/, 'meta.diff.header'],
        [/^diff .*$/, 'meta.diff'],
        [/^index .*$/, 'comment'],
        [/^@@.*@@.*$/, 'meta.diff.hunk'],
        [/^\+.*$/, 'markup.inserted'],
        [/^-.*$/, 'markup.deleted'],
      ],
    },
  });

  // Strong diff colors for unified patches (language tokens + decorations).
  monaco.editor.defineTheme('gogen-dark', {
    base: 'vs-dark',
    inherit: true,
    rules: [
      { token: 'comment', foreground: '6A9955' },
      { token: 'meta.diff', foreground: '569CD6' },
      { token: 'meta.diff.header', foreground: '569CD6', fontStyle: 'bold' },
      { token: 'meta.diff.hunk', foreground: 'C586C0' },
      { token: 'markup.inserted', foreground: '4EC9B0' },
      { token: 'markup.deleted', foreground: 'F14C4C' },
    ],
    colors: {
      'diffEditor.insertedTextBackground': '#2ea04340',
      'diffEditor.removedTextBackground': '#f8514940',
      'diffEditor.insertedLineBackground': '#2ea04326',
      'diffEditor.removedLineBackground': '#f8514926',
      'editorGutter.addedBackground': '#2ea043',
      'editorGutter.deletedBackground': '#f85149',
      'editorGutter.modifiedBackground': '#d29922',
    },
  });
  monaco.editor.setTheme('gogen-dark');
  monacoReady = true;
  return monaco;
}

/** Colorize unified-diff lines via decorations (works even if language tokens are missing). */
export function applyUnifiedDiffDecorations(ed) {
  if (!ed) return;
  const model = ed.getModel();
  if (!model) return;
  const lineCount = model.getLineCount();
  const decorations = [];
  for (let i = 1; i <= lineCount; i++) {
    const text = model.getLineContent(i);
    let cls = null;
    if (text.startsWith('+++') || text.startsWith('---') || text.startsWith('diff ') || text.startsWith('index ')) {
      cls = 'gogen-diff-meta';
    } else if (text.startsWith('@@')) {
      cls = 'gogen-diff-hunk';
    } else if (text.startsWith('+')) {
      cls = 'gogen-diff-add';
    } else if (text.startsWith('-')) {
      cls = 'gogen-diff-del';
    }
    if (!cls) continue;
    decorations.push({
      range: new monaco.Range(i, 1, i, model.getLineMaxColumn(i)),
      options: {
        isWholeLine: true,
        className: cls,
        marginClassName: cls + '-margin',
      },
    });
  }
  const prev = ed.__gogenDiffDecorations || [];
  ed.__gogenDiffDecorations = ed.deltaDecorations(prev, decorations);
}

function ensureEditors() {
  const host = $('monaco-host');
  if (!host) return;
  if (!editor) {
    editor = monaco.editor.create(host, {
      automaticLayout: true,
      theme: 'gogen-dark',
      minimap: { enabled: false },
      fontSize: 13,
      wordWrap: 'on',
      scrollBeyondLastLine: false,
    });
    editor.addCommand(monaco.KeyMod.CtrlCmd | monaco.KeyCode.KeyS, () => {
      saveActive();
    });
    editor.onDidChangeModelContent(() => {
      updateDirtyIndicators();
    });
  }
}

function showEditPane() {
  mode = 'edit';
  const host = $('monaco-host');
  const diffHost = $('monaco-diff-host');
  if (host) host.style.display = 'block';
  if (diffHost) diffHost.style.display = 'none';
  if (diffEditor) {
    // Keep models; just hide
  }
}

function showDiffPane() {
  mode = 'diff';
  const host = $('monaco-host');
  const diffHost = $('monaco-diff-host');
  if (host) host.style.display = 'none';
  if (diffHost) diffHost.style.display = 'block';
  if (!diffEditor) {
    diffEditor = monaco.editor.createDiffEditor(diffHost, {
      automaticLayout: true,
      readOnly: true,
      theme: 'gogen-dark',
      renderSideBySide: GOGEN_UI.diffRenderSideBySide,
      minimap: { enabled: false },
      fontSize: 13,
      renderIndicators: true,
      originalEditable: false,
    });
  } else {
    diffEditor.updateOptions({ renderSideBySide: GOGEN_UI.diffRenderSideBySide });
  }
}

function basename(path) {
  const parts = path.split('/');
  return parts[parts.length - 1] || path;
}

function touchBuffer(path) {
  const b = buffers.get(path);
  if (b) b.lastUsed = Date.now();
}

function isDirty(path) {
  const b = buffers.get(path);
  if (!b || !b.model) return false;
  return b.model.getAlternativeVersionId() !== b.savedVersionId;
}

function updateDirtyIndicators() {
  renderTabs();
  const label = $('editor-path-label');
  if (label) {
    if (mode === 'diff' && activePath) {
      label.textContent = `${activePath} (unstaged diff)`;
    } else if (activePath) {
      label.textContent = isDirty(activePath) ? `${activePath} *` : activePath;
    } else {
      label.textContent = 'No file open';
    }
  }
}

function renderTabs() {
  const strip = $('editor-tabs');
  if (!strip) return;
  strip.innerHTML = '';
  for (const path of openOrder) {
    const tab = document.createElement('div');
    tab.className = 'file-tab' + (path === activePath && mode === 'edit' ? ' active' : '');
    tab.title = path;
    const name = document.createElement('span');
    name.className = 'file-tab-name';
    name.textContent = (isDirty(path) ? '* ' : '') + basename(path);
    const close = document.createElement('button');
    close.className = 'file-tab-close';
    close.type = 'button';
    close.textContent = '×';
    close.title = 'Close';
    close.onclick = (e) => {
      e.stopPropagation();
      closeTab(path);
    };
    tab.appendChild(name);
    tab.appendChild(close);
    tab.onclick = () => activatePath(path);
    strip.appendChild(tab);
  }
}

async function enforceTabCap() {
  while (openOrder.length > GOGEN_UI.maxOpenTabs) {
    let victim = null;
    let oldest = Infinity;
    for (const p of openOrder) {
      if (p === activePath) continue;
      if (isDirty(p)) continue;
      const b = buffers.get(p);
      const t = b ? b.lastUsed : 0;
      if (t < oldest) {
        oldest = t;
        victim = p;
      }
    }
    if (!victim) break;
    disposeBuffer(victim);
  }
}

function disposeBuffer(path) {
  const b = buffers.get(path);
  if (b && b.model) b.model.dispose();
  buffers.delete(path);
  openOrder = openOrder.filter((p) => p !== path);
  if (activePath === path) {
    activePath = openOrder.length ? openOrder[openOrder.length - 1] : null;
    if (activePath) {
      showEditPane();
      ensureEditors();
      const nb = buffers.get(activePath);
      editor.setModel(nb.model);
      if (nb.viewState) editor.restoreViewState(nb.viewState);
    } else if (editor) {
      editor.setModel(null);
    }
  }
  updateDirtyIndicators();
}

async function closeTab(path) {
  if (isDirty(path)) {
    if (!window.confirm(`Close ${basename(path)} and discard unsaved changes?`)) return;
  }
  disposeBuffer(path);
}

function activatePath(path) {
  if (!buffers.has(path)) return;
  if (activePath && editor && mode === 'edit') {
    const cur = buffers.get(activePath);
    if (cur) cur.viewState = editor.saveViewState();
  }
  activePath = path;
  touchBuffer(path);
  showEditPane();
  ensureEditors();
  const b = buffers.get(path);
  editor.setModel(b.model);
  if (b.viewState) editor.restoreViewState(b.viewState);
  editor.focus();
  updateDirtyIndicators();
}

async function openFile(path) {
  await initMonaco();
  ensureEditors();
  if (buffers.has(path)) {
    activatePath(path);
    return;
  }
  const data = await wsRequest('fs_read', { path });
  const model = monaco.editor.createModel(data.content || '', data.language || 'plaintext');
  buffers.set(path, {
    model,
    viewState: null,
    savedVersionId: model.getAlternativeVersionId(),
    lastUsed: Date.now(),
  });
  openOrder.push(path);
  await enforceTabCap();
  activatePath(path);
}

async function savePath(path) {
  const b = buffers.get(path);
  if (!b) return false;
  try {
    await wsRequest('fs_write', { path, content: b.model.getValue() });
    b.savedVersionId = b.model.getAlternativeVersionId();
    updateDirtyIndicators();
    await refreshGitStatus();
    return true;
  } catch (err) {
    window.alert(`Save failed: ${err.message}`);
    return false;
  }
}

async function saveActive() {
  if (!activePath || mode !== 'edit') return;
  await savePath(activePath);
}

async function saveAll() {
  for (const path of [...openOrder]) {
    if (isDirty(path)) {
      const ok = await savePath(path);
      if (!ok) return;
    }
  }
}

async function openUnstagedDiff(path) {
  await initMonaco();
  if (activePath && editor && mode === 'edit') {
    const cur = buffers.get(activePath);
    if (cur) cur.viewState = editor.saveViewState();
  }
  activePath = path;
  showDiffPane();
  try {
    const data = await wsRequest('git_file_diff', { path });
    const lang = data.language || 'plaintext';
    const original = monaco.editor.createModel(data.original || '', lang);
    const modified = monaco.editor.createModel(data.modified || '', lang);
    const prev = diffEditor.getModel();
    diffEditor.setModel({ original, modified });
    if (prev) {
      if (prev.original) prev.original.dispose();
      if (prev.modified) prev.modified.dispose();
    }
  } catch (err) {
    window.alert(`Diff failed: ${err.message}`);
    showEditPane();
  }
  updateDirtyIndicators();
}

async function loadTree(path, container) {
  container.innerHTML = '';
  let entries;
  try {
    const data = await wsRequest('fs_list', { path: path || '.' });
    entries = data.entries || [];
  } catch (err) {
    container.textContent = err.message;
    return;
  }
  entries.sort((a, b) => {
    if (a.isDir !== b.isDir) return a.isDir ? -1 : 1;
    return a.name.localeCompare(b.name);
  });
  for (const ent of entries) {
    const row = document.createElement('div');
    row.className = 'tree-item' + (ent.isDir ? ' dir' : ' file');
    row.textContent = (ent.isDir ? '📁 ' : '📄 ') + ent.name;
    row.title = ent.path;
    if (ent.isDir) {
      const child = document.createElement('div');
      child.className = 'tree-children';
      child.style.display = 'none';
      row.onclick = async () => {
        const open = child.style.display !== 'none';
        if (open) {
          child.style.display = 'none';
          return;
        }
        child.style.display = 'block';
        if (!child.dataset.loaded) {
          child.dataset.loaded = '1';
          await loadTree(ent.path, child);
        }
      };
      container.appendChild(row);
      container.appendChild(child);
    } else {
      row.onclick = () => openFile(ent.path).catch((e) => window.alert(e.message));
      container.appendChild(row);
    }
  }
}

async function refreshGitStatus() {
  const list = $('unstaged-list');
  if (!list) return;
  list.innerHTML = '';
  try {
    const data = await wsRequest('git_status', {});
    const entries = data.gitEntries || [];
    if (!entries.length) {
      list.textContent = 'Working tree clean';
      return;
    }
    for (const ent of entries) {
      const row = document.createElement('div');
      row.className = 'unstaged-item';
      row.textContent = `${ent.status}  ${ent.path}`;
      row.title = ent.path;
      row.onclick = () => openUnstagedDiff(ent.path).catch((e) => window.alert(e.message));
      list.appendChild(row);
    }
  } catch (err) {
    list.textContent = err.message;
  }
}

export async function refreshExplorer() {
  const tree = $('file-tree');
  if (tree) await loadTree('.', tree);
  await refreshGitStatus();
}

export function setupEditorUI() {
  $('btn-refresh-explorer')?.addEventListener('click', () => {
    refreshExplorer().catch((e) => window.alert(e.message));
  });
  $('btn-save-file')?.addEventListener('click', () => saveActive());
  $('btn-save-all')?.addEventListener('click', () => saveAll());
  $('btn-diff-layout')?.addEventListener('click', () => {
    GOGEN_UI.diffRenderSideBySide = !GOGEN_UI.diffRenderSideBySide;
    if (diffEditor) {
      diffEditor.updateOptions({ renderSideBySide: GOGEN_UI.diffRenderSideBySide });
    }
    const btn = $('btn-diff-layout');
    if (btn) btn.textContent = GOGEN_UI.diffRenderSideBySide ? 'Side-by-side' : 'Inline';
  });
}

// --- Chat tool-card Monaco helpers ---

export function extractDiffValue(rawJSON) {
  const idx = rawJSON.indexOf('"diff"');
  if (idx < 0) return { ok: false, value: '' };
  let rest = rawJSON.slice(idx + 6).replace(/^[ \t]+/, '');
  if (!rest.startsWith(':')) return { ok: false, value: '' };
  rest = rest.slice(1).replace(/^[ \t]+/, '');
  if (!rest.startsWith('"')) return { ok: false, value: '' };
  rest = rest.slice(1);
  let out = '';
  for (let i = 0; i < rest.length; i++) {
    const ch = rest[i];
    if (ch === '\\' && i + 1 < rest.length) {
      const n = rest[i + 1];
      if (n === 'n') out += '\n';
      else if (n === 't') out += '\t';
      else if (n === '"') out += '"';
      else if (n === '\\') out += '\\';
      else if (n === 'r') { /* skip */ }
      else {
        out += ch;
        out += n;
      }
      i++;
    } else if (ch === '"') {
      return { ok: true, value: out };
    } else {
      out += ch;
    }
  }
  return { ok: out.length > 0, value: out };
}

export async function mountDiffEditor(container, value, opts = {}) {
  await initMonaco();
  // Keep a fallback <pre> so diffs remain visible if Monaco layout/workers fail.
  container.innerHTML = '';
  container.classList.add('monaco-tool-host');

  const fallback = document.createElement('pre');
  fallback.className = 'diff-fallback';
  container.appendChild(fallback);
  updateDiffFallback(container, value || '');

  try {
    const host = document.createElement('div');
    host.className = 'monaco-tool-editor';
    host.style.visibility = 'hidden';
    container.appendChild(host);

    const ed = monaco.editor.create(host, {
      value: value || '',
      language: 'diff',
      readOnly: true,
      theme: 'gogen-dark',
      // Fixed host size — avoid ResizeObserver fighting flex layout.
      automaticLayout: false,
      minimap: { enabled: false },
      fontSize: 12,
      wordWrap: 'on',
      scrollBeyondLastLine: false,
      lineNumbers: 'on',
      folding: false,
      renderLineHighlight: 'none',
      ...opts,
    });
    chatEditors.add(ed);
    applyUnifiedDiffDecorations(ed);

    const layoutFixed = () => {
      const w = container.clientWidth || host.clientWidth;
      const h = container.clientHeight || 280;
      if (w > 0 && h > 0) {
        ed.layout({ width: w, height: h });
      }
    };

    // Layout after the tool card has a real width in the flex column.
    requestAnimationFrame(() => {
      try {
        layoutFixed();
        // Prefer Monaco once it has painted; hide plain fallback.
        if (host.clientWidth > 0 && host.clientHeight > 0) {
          fallback.style.display = 'none';
          host.style.visibility = 'visible';
          layoutFixed();
        }
      } catch (_) { /* keep fallback visible */ }
    });
    return ed;
  } catch (err) {
    console.warn('monaco diff mount failed, using text fallback', err);
    return null;
  }
}

export function updateDiffEditor(ed, value) {
  if (!ed) return;
  const model = ed.getModel();
  if (!model) return;
  if (model.getValue() === value) return;
  model.setValue(value);
  applyUnifiedDiffDecorations(ed);
  const line = model.getLineCount();
  ed.revealLine(line);
  requestAnimationFrame(() => {
    try {
      const dom = ed.getDomNode();
      const parent = dom && dom.parentElement;
      const host = parent && parent.parentElement;
      const w = (host && host.clientWidth) || (dom && dom.clientWidth) || 0;
      const h = (host && host.clientHeight) || 280;
      if (w > 0) ed.layout({ width: w, height: h });
    } catch (_) { /* ignore */ }
  });
}

/** Update fallback <pre> inside a monaco-tool-host (always kept in sync). */
export function updateDiffFallback(container, value) {
  if (!container) return;
  const pre = container.querySelector('.diff-fallback');
  if (!pre) return;
  // Colorize plain-text fallback so diffs stay readable if Monaco fails.
  const text = value || '';
  pre.textContent = '';
  for (const line of text.split('\n')) {
    const span = document.createElement('span');
    span.textContent = line + '\n';
    if (line.startsWith('+++') || line.startsWith('---') || line.startsWith('diff ') || line.startsWith('index ')) {
      span.className = 'gogen-diff-meta';
    } else if (line.startsWith('@@')) {
      span.className = 'gogen-diff-hunk';
    } else if (line.startsWith('+')) {
      span.className = 'gogen-diff-add';
    } else if (line.startsWith('-')) {
      span.className = 'gogen-diff-del';
    }
    pre.appendChild(span);
  }
}

export function disposeChatEditors() {
  for (const ed of chatEditors) {
    try {
      ed.dispose();
    } catch (_) { /* ignore */ }
  }
  chatEditors.clear();
}

export { monaco };
