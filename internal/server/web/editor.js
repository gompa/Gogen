// Monaco editor workspace for GoGen web UI.

let monaco = null;

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
let monacoInitPromise = null;
let wsRef = null;
let reqCounter = 0;
const pendingReqs = new Map();
const chatEditors = new Set(); // disposable Monaco editors in chat tool cards
let toastFn = null;
let searchDebounceTimer = null;
let searchGen = 0;

function $(id) {
  return document.getElementById(id);
}

export function setToastHandler(fn) {
  toastFn = typeof fn === 'function' ? fn : null;
}

function toast(message, kind = 'info') {
  if (toastFn) toastFn(message, kind);
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
  if (monacoInitPromise) return monacoInitPromise;

  // Dynamic import: avoids blocking the WebSocket connection on a 3.8 MB download.
  monacoInitPromise = (async () => {
    // Load the editor stylesheet alongside the JS bundle so the Chat-only
    // first paint never pays for 128KB of Monaco CSS. Once-only guard.
    // Must wait for the CSS to finish loading before creating any editor,
    // otherwise the diff viewer renders with wrong sizing and scrollbars.
    let cssPromise = Promise.resolve();
    if (!document.getElementById('monaco-editor-css')) {
      const link = document.createElement('link');
      link.id = 'monaco-editor-css';
      link.rel = 'stylesheet';
      link.href = '/monaco/editor.main.css';
      cssPromise = new Promise((resolve) => {
        link.onload = () => resolve();
        link.onerror = () => resolve(); // proceed anyway on error
        // Safety timeout: never block more than 5s waiting for CSS.
        setTimeout(() => resolve(), 5000);
      });
      document.head.appendChild(link);
    }
    // Start the JS download in parallel but don't proceed until CSS is ready.
    const jsPromise = import('/monaco/editor.bundle.js');
    await cssPromise;
    const mod = await jsPromise;
    monaco = mod.default;
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
  })();

  try {
    return await monacoInitPromise;
  } catch (err) {
    monacoInitPromise = null;
    throw err;
  }
}

// Common fence aliases → Monaco language ids.
const LANG_ALIASES = {
  js: 'javascript',
  jsx: 'javascript',
  mjs: 'javascript',
  cjs: 'javascript',
  ts: 'typescript',
  tsx: 'typescript',
  py: 'python',
  rb: 'ruby',
  sh: 'shell',
  bash: 'shell',
  zsh: 'shell',
  yml: 'yaml',
  md: 'markdown',
  golang: 'go',
  rs: 'rust',
  cs: 'csharp',
  csharp: 'csharp',
  kt: 'kotlin',
  plaintext: 'plaintext',
  text: 'plaintext',
  plain: 'plaintext',
  console: 'shell',
};

function resolveMonacoLanguage(langHint) {
  if (!monaco || !langHint) return null;
  let id = String(langHint).trim().toLowerCase();
  if (!id) return null;
  id = LANG_ALIASES[id] || id;
  const langs = monaco.languages.getLanguages();
  if (langs.some((l) => l.id === id)) return id;
  for (const l of langs) {
    if (l.aliases?.some((a) => String(a).toLowerCase() === id)) return l.id;
    if (l.extensions?.some((ext) => ext === `.${id}` || ext.slice(1).toLowerCase() === id)) {
      return l.id;
    }
  }
  return null;
}

/** Map a file path to a Monaco language id (mirrors server languageFromPath). */
export function languageFromPath(path) {
  if (!path) return 'plaintext';
  const base = String(path).split(/[/\\]/).pop() || '';
  const dot = base.lastIndexOf('.');
  const ext = dot >= 0 ? base.slice(dot).toLowerCase() : '';
  switch (ext) {
    case '.go':
    case '.mod':
      return 'go';
    case '.js':
    case '.mjs':
    case '.cjs':
    case '.jsx':
      return 'javascript';
    case '.ts':
    case '.tsx':
      return 'typescript';
    case '.json':
      return 'json';
    case '.md':
    case '.markdown':
      return 'markdown';
    case '.html':
    case '.htm':
      return 'html';
    case '.css':
      return 'css';
    case '.scss':
      return 'scss';
    case '.less':
      return 'less';
    case '.yaml':
    case '.yml':
      return 'yaml';
    case '.toml':
      return 'ini';
    case '.xml':
      return 'xml';
    case '.sh':
    case '.bash':
    case '.zsh':
      return 'shell';
    case '.py':
      return 'python';
    case '.rs':
      return 'rust';
    case '.java':
      return 'java';
    case '.c':
    case '.h':
      return 'c';
    case '.cpp':
    case '.cc':
    case '.cxx':
    case '.hpp':
      return 'cpp';
    case '.cs':
      return 'csharp';
    case '.sql':
      return 'sql';
    case '.rb':
      return 'ruby';
    case '.php':
      return 'php';
    case '.swift':
      return 'swift';
    case '.kt':
      return 'kotlin';
    case '.lua':
      return 'lua';
    case '.r':
      return 'r';
    case '.diff':
    case '.patch':
      return 'diff';
    default:
      return 'plaintext';
  }
}

/**
 * Colorize a single element's textContent with Monaco.
 * langHint may be a language id or a file path.
 */
export async function colorizeElement(el, langHint) {
  if (!el) return;
  const gen = (el._gogenHlGen = (el._gogenHlGen || 0) + 1);
  let m;
  try {
    m = await initMonaco();
  } catch (err) {
    console.warn('monaco colorize init failed', err);
    return;
  }
  if (el._gogenHlGen !== gen || !el.isConnected) return;

  let lang = resolveMonacoLanguage(langHint);
  if (!lang && langHint && String(langHint).includes('.')) {
    lang = resolveMonacoLanguage(languageFromPath(langHint));
  }
  if (!lang || lang === 'plaintext') return;

  const source = el.textContent || '';
  if (!source.trim()) return;

  try {
    const html = await m.editor.colorize(source, lang, {});
    if (el._gogenHlGen !== gen || !el.isConnected) return;
    el.innerHTML = html;
    el.dataset.monacoColorized = '1';
    el.classList.add('monaco-colorized');
    // Notify scroll system that DOM height may have changed.
    window.dispatchEvent(new CustomEvent('gogen-colorized', { bubbles: false }));
  } catch (_) {
    // Unknown / unloaded language — leave plain text.
  }
}

/**
 * Syntax-highlight fenced code blocks under root using Monaco's colorize API.
 * Safe to call repeatedly; skips already-highlighted nodes. Stale runs are dropped
 * when root is re-rendered (generation counter).
 */
export async function colorizeCodeBlocks(root) {
  if (!root || !root.querySelectorAll) return;
  const codes = root.querySelectorAll('pre code');
  if (!codes.length) return;

  const gen = (root._gogenHlGen = (root._gogenHlGen || 0) + 1);
  let m;
  try {
    m = await initMonaco();
  } catch (err) {
    console.warn('monaco colorize init failed', err);
    return;
  }
  if (root._gogenHlGen !== gen || !root.isConnected) return;

  for (const code of codes) {
    if (root._gogenHlGen !== gen || !code.isConnected) return;
    if (code.dataset.monacoColorized === '1') continue;

    const classMatch = /(?:^|\s)language-(\S+)/.exec(code.className || '');
    const lang = resolveMonacoLanguage(classMatch ? classMatch[1] : '');
    if (!lang || lang === 'plaintext') continue;

    const source = code.textContent || '';
    if (!source.trim()) continue;

    try {
      const html = await m.editor.colorize(source, lang, {});
      if (root._gogenHlGen !== gen || !code.isConnected) return;
      code.innerHTML = html;
      code.dataset.monacoColorized = '1';
      code.classList.add('monaco-colorized');
    } catch (_) {
      // Unknown / unloaded language — leave plain text.
    }
  }
  // Notify scroll system that DOM height may have changed.
  window.dispatchEvent(new CustomEvent('gogen-colorized', { bubbles: false }));
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
      updateUndoRedoButtons();
    });

    // --- Context menu: add selection reference to chat input ---
    editor.addAction({
      id: 'gogen-add-reference-to-chat',
      label: 'Add Reference to Chat',
      contextMenuGroupId: 'navigation',
      contextMenuOrder: 1.5,
      run(ed) {
        const selection = ed.getSelection();
        if (!selection || !activePath) return;
        const startLine = selection.startLineNumber;
        const endLine = selection.endLineNumber;
        const hasSelection = !selection.isEmpty();

        // Build the reference string
        let ref;
        if (hasSelection && startLine !== endLine) {
          ref = `@${activePath}:${startLine}-${endLine}`;
        } else {
          ref = `@${activePath}:${startLine}`;
        }

        const inputEl = document.getElementById('message-input');
        if (!inputEl) return;

        // Insert reference at cursor or append
        const start = inputEl.selectionStart;
        const end = inputEl.selectionEnd;
        const before = inputEl.value.slice(0, start);
        const after = inputEl.value.slice(end);
        const spacer = before && !before.endsWith(' ') && !before.endsWith('\n') ? ' ' : '';
        inputEl.value = before + spacer + ref + after;
        // Move cursor after the inserted reference
        const cursorPos = start + spacer.length + ref.length;
        inputEl.selectionStart = cursorPos;
        inputEl.selectionEnd = cursorPos;
        inputEl.focus();
        inputEl.dispatchEvent(new Event('input', { bubbles: true }));
      },
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
  updateUndoRedoButtons();
}

function updateUndoRedoButtons() {
  const undoBtn = $('btn-undo');
  const redoBtn = $('btn-redo');
  const canEdit = mode === 'edit' && editor && editor.getModel();
  let canUndo = false;
  let canRedo = false;
  if (canEdit) {
    // Monaco does not expose stack depth; enable whenever an editable model is active.
    canUndo = true;
    canRedo = true;
  }
  if (undoBtn) undoBtn.disabled = !canUndo;
  if (redoBtn) redoBtn.disabled = !canRedo;
}

export function editorUndo() {
  if (mode !== 'edit' || !editor) return;
  editor.focus();
  editor.trigger('toolbar', 'undo', null);
}

export function editorRedo() {
  if (mode !== 'edit' || !editor) return;
  editor.focus();
  editor.trigger('toolbar', 'redo', null);
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
    const confirmed = await showCloseTabModal(basename(path));
    if (!confirmed) return;
  }
  disposeBuffer(path);
}

function showCloseTabModal(filename) {
  return new Promise((resolve) => {
    const overlay = document.getElementById('close-tab-overlay');
    const filenameEl = document.getElementById('close-tab-filename');
    const discardBtn = document.getElementById('close-tab-discard-btn');
    const keepBtn = document.getElementById('close-tab-keep-btn');
    if (!overlay) { resolve(window.confirm(`Close ${filename} and discard unsaved changes?`)); return; }
    filenameEl.textContent = `${filename} has unsaved changes that will be lost.`;
    overlay.classList.add('active');
    const cleanup = (result) => {
      overlay.classList.remove('active');
      discardBtn.removeEventListener('click', onDiscard);
      keepBtn.removeEventListener('click', onKeep);
      resolve(result);
    };
    const onDiscard = () => cleanup(true);
    const onKeep = () => cleanup(false);
    discardBtn.addEventListener('click', onDiscard);
    keepBtn.addEventListener('click', onKeep);
  });
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

async function openFile(path, line) {
  await initMonaco();
  ensureEditors();
  if (buffers.has(path)) {
    activatePath(path);
  } else {
    let data;
    try {
      data = await wsRequest('fs_read', { path });
    } catch (err) {
      toast(`Cannot open ${basename(path)}: ${err.message || 'read failed'}`, 'error');
      return;
    }
    if (data.error) {
      toast(`Cannot open ${basename(path)}: ${data.error}`, 'error');
      return;
    }
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
  if (line && line > 0 && editor) {
    editor.revealLineInCenter(line);
    editor.setPosition({ lineNumber: line, column: 1 });
    editor.focus();
  }
}

export async function openFileAtLine(path, line) {
  await openFile(path, line);
}

async function savePath(path) {
  const b = buffers.get(path);
  if (!b) return false;
  try {
    await wsRequest('fs_write', { path, content: b.model.getValue() });
    b.savedVersionId = b.model.getAlternativeVersionId();
    updateDirtyIndicators();
    await refreshGitStatus();
    toast(`Saved ${basename(path)}`, 'success');
    return true;
  } catch (err) {
    toast(`Save failed: ${err.message}`, 'error');
    return false;
  }
}

async function saveActive() {
  if (!activePath || mode !== 'edit') return;
  await savePath(activePath);
}

async function saveAll() {
  const loading = document.getElementById('save-all-loading');
  if (loading) loading.classList.add('active');
  let any = false;
  for (const path of [...openOrder]) {
    if (isDirty(path)) {
      any = true;
      const ok = await savePath(path);
      if (!ok) {
        if (loading) loading.classList.remove('active');
        return;
      }
    }
  }
  if (loading) loading.classList.remove('active');
  if (any) toast('All files saved', 'success');
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
    toast(`Diff failed: ${err.message}`, 'error');
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
      row.onclick = () => openFile(ent.path);
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
      row.onclick = () => openUnstagedDiff(ent.path);
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

export function focusFindInFiles() {
  switchToEditorPane();
  const input = $('find-in-files-input');
  if (input) {
    input.focus();
    input.select();
  }
}

function switchToEditorPane() {
  document.querySelectorAll('.main-tab').forEach((t) => {
    t.classList.toggle('active', t.dataset.pane === 'editor');
  });
  document.querySelectorAll('.pane').forEach((p) => {
    p.classList.toggle('active', p.id === 'editor-pane');
  });
  initMonaco().then(() => refreshExplorer()).catch(() => {});
}

async function runFindInFiles(pattern) {
  const results = $('find-in-files-results');
  if (!results) return;
  const q = (pattern || '').trim();
  if (!q) {
    results.textContent = '';
    return;
  }
  const gen = ++searchGen;
  results.textContent = 'Searching…';
  try {
    const data = await wsRequest('fs_search', { pattern: q });
    if (gen !== searchGen) return;
    const matches = data.matches || [];
    results.innerHTML = '';
    if (!matches.length) {
      results.textContent = 'No matches';
      return;
    }
    if (data.truncated) {
      const note = document.createElement('div');
      note.className = 'search-note';
      note.textContent = 'Results truncated';
      results.appendChild(note);
    }
    for (const m of matches) {
      const row = document.createElement('div');
      row.className = 'search-result';
      row.title = `${m.path}:${m.line}`;
      const loc = document.createElement('div');
      loc.className = 'search-result-loc';
      loc.textContent = `${m.path}:${m.line}`;
      const text = document.createElement('div');
      text.className = 'search-result-text';
      text.textContent = m.text || '';
      row.appendChild(loc);
      row.appendChild(text);
      row.onclick = () => {
        openFile(m.path, m.line).catch((e) => toast(e.message, 'error'));
      };
      results.appendChild(row);
    }
  } catch (err) {
    if (gen !== searchGen) return;
    results.textContent = err.message;
    toast(`Search failed: ${err.message}`, 'error');
  }
}

// --- Search & Replace ---

/**
 * Replace all occurrences of `search` with `replace` across matching files.
 * Uses the backend `fs_replace` message which performs a safe text replacement.
 * @param {string} [scopePath] - If provided, restrict replacement to this file.
 */
async function runReplaceAll(searchPattern, replacement, scopePath) {
  const results = $('find-in-files-results');
  if (!results) return;
  const q = (searchPattern || '').trim();
  const r = (replacement || '');
  if (!q) {
    toast('Search pattern is empty', 'error');
    return;
  }
  const gen = ++searchGen;
  results.textContent = 'Replacing…';
  try {
    const payload = { pattern: q, replacement: r };
    if (scopePath) payload.path = scopePath;
    const data = await wsRequest('fs_replace', payload);
    if (gen !== searchGen) return;
    if (data.replaced && data.replaced > 0) {
      toast(`Replaced ${data.replaced} occurrence(s) in ${data.fileCount || '?'} file(s)`, 'success');
      // Re-run the search to show updated results
      runFindInFiles(searchPattern);
    } else {
      results.textContent = 'No matches to replace';
    }
  } catch (err) {
    if (gen !== searchGen) return;
    results.textContent = err.message;
    toast(`Replace failed: ${err.message}`, 'error');
  }
}

export function setupEditorUI() {
  $('btn-refresh-explorer')?.addEventListener('click', () => {
    refreshExplorer().catch((e) => toast(e.message, 'error'));
  });
  $('btn-save-file')?.addEventListener('click', () => saveActive());
  $('btn-save-all')?.addEventListener('click', () => saveAll());
  $('btn-undo')?.addEventListener('click', () => editorUndo());
  $('btn-redo')?.addEventListener('click', () => editorRedo());
  $('btn-diff-layout')?.addEventListener('click', () => {
    GOGEN_UI.diffRenderSideBySide = !GOGEN_UI.diffRenderSideBySide;
    if (diffEditor) {
      diffEditor.updateOptions({ renderSideBySide: GOGEN_UI.diffRenderSideBySide });
    }
    const btn = $('btn-diff-layout');
    if (btn) btn.textContent = GOGEN_UI.diffRenderSideBySide ? 'Side-by-side' : 'Inline';
  });
  const searchInput = $('find-in-files-input');
  if (searchInput) {
    searchInput.addEventListener('input', () => {
      clearTimeout(searchDebounceTimer);
      searchDebounceTimer = setTimeout(() => {
        runFindInFiles(searchInput.value);
      }, 250);
    });
    searchInput.addEventListener('keydown', (e) => {
      if (e.key === 'Enter') {
        e.preventDefault();
        clearTimeout(searchDebounceTimer);
        runFindInFiles(searchInput.value);
      }
    });
  }
  // Keyboard shortcut: Ctrl+H opens the replace field
  document.addEventListener('keydown', (e) => {
    if ((e.ctrlKey || e.metaKey) && e.key === 'h') {
      e.preventDefault();
      if (e.shiftKey) {
        // Ctrl+Shift+H: toggle sidebar replace row
        const row = $('find-in-files-replace-row');
        if (row) {
          row.classList.toggle('open');
          if (row.classList.contains('open')) {
            $('find-in-files-replace-input')?.focus();
          } else {
            $('find-in-files-input')?.focus();
          }
        }
      } else {
        // Ctrl+H: open Monaco's built-in find/replace widget
        if (editor) {
          editor.getAction('editor.action.startFindReplaceAction').run();
        }
      }
    }
  });

  // --- Replace toggle & preview modal ---

  // Toggle replace row visibility
  $('btn-toggle-replace')?.addEventListener('click', () => {
    const row = $('find-in-files-replace-row');
    if (!row) return;
    row.classList.toggle('open');
    if (row.classList.contains('open')) {
      $('find-in-files-replace-input')?.focus();
    }
  });

  /**
   * Build the preview HTML for the replace modal.
   * Highlights the search term inside each matching line.
   */
  function buildPreviewHTML(matches, search, replacement) {
    // Group by file
    const byFile = new Map();
    for (const m of matches) {
      if (!byFile.has(m.path)) byFile.set(m.path, []);
      byFile.get(m.path).push(m);
    }

    let html = '';
    for (const [file, fileMatches] of byFile) {
      html += `<div class="rp-file-header">${escHTML(file)}</div>`;
      for (const m of fileMatches) {
        const line = m.text || '';
        const highlighted = highlightMatch(line, search);
        html += `<div class="rp-line">`
          + `<span class="rp-line-num">${m.line}</span>`
          + `<span class="rp-line-old">${highlighted}</span>`
          + `<span class="rp-arrow">→</span>`
          + `<span class="rp-line-new">${highlightMatch(line, search, replacement)}</span>`
          + `</div>`;
      }
    }
    return html;
  }

  /** Escape HTML entities. */
  function escHTML(s) {
    return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
  }

  /**
   * Highlight `search` inside `text` using the same regex semantics as backend search.
   * If `replacement` is provided, show the replaced line instead.
   */
  function highlightMatch(text, search, replacement) {
    let re;
    try {
      re = new RegExp(search, 'g');
    } catch {
      const lit = search.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
      re = new RegExp(lit, 'g');
    }
    if (replacement !== undefined) {
      return escHTML(text.replace(re, replacement));
    }
    let out = '';
    let last = 0;
    for (const m of text.matchAll(re)) {
      out += escHTML(text.slice(last, m.index));
      out += `<span class="rp-highlight">${escHTML(m[0])}</span>`;
      last = m.index + m[0].length;
    }
    out += escHTML(text.slice(last));
    return out;
  }

  /**
   * Show the replace preview modal.
   * Returns a Promise that resolves to true (confirm) or false (cancel).
   */
  function showReplacePreview(matches, search, replacement, scopeLabel) {
    return new Promise((resolve) => {
      const overlay = $('replace-preview-overlay');
      const summary = $('replace-preview-summary');
      const body = $('replace-preview-body');
      const confirmBtn = $('rp-confirm');
      const cancelBtn = $('rp-cancel');
      if (!overlay) { resolve(window.confirm(`Replace ${matches.length} occurrence(s)?`)); return; }

      // Set summary
      const fileCount = new Set(matches.map(m => m.path)).size;
      summary.textContent = `${matches.length} occurrence(s) in ${fileCount} file(s)${scopeLabel ? ' — ' + scopeLabel : ''}`;
      body.innerHTML = buildPreviewHTML(matches, search, replacement);
      overlay.classList.add('active');

      const cleanup = (result) => {
        overlay.classList.remove('active');
        confirmBtn.removeEventListener('click', onConfirm);
        cancelBtn.removeEventListener('click', onCancel);
        overlay.removeEventListener('click', onBackdrop);
        resolve(result);
      };
      const onConfirm = () => cleanup(true);
      const onCancel = () => cleanup(false);
      const onBackdrop = (e) => { if (e.target === overlay) cleanup(false); };
      confirmBtn.addEventListener('click', onConfirm);
      cancelBtn.addEventListener('click', onCancel);
      overlay.addEventListener('click', onBackdrop);
    });
  }

  // Shared handler: search first, then show preview modal, then apply on confirm
  async function handleReplace(scopePath, scopeLabel) {
    const search = $('find-in-files-input')?.value || '';
    const replacement = $('find-in-files-replace-input')?.value || '';
    if (!search.trim()) { toast('Search pattern is empty', 'error'); return; }
    try {
      const data = await wsRequest('fs_search', { pattern: search, ...(scopePath ? { path: scopePath } : {}) });
      const matches = data.matches || [];
      if (!matches.length) { toast('No matches found', 'info'); return; }
      const confirmed = await showReplacePreview(matches, search, replacement, scopeLabel);
      if (confirmed) {
        await runReplaceAll(search, replacement, scopePath);
      }
    } catch (err) {
      toast(`Search failed: ${err.message}`, 'error');
    }
  }

  $('btn-replace-one')?.addEventListener('click', () => {
    if (!activePath) { toast('No file open', 'error'); return; }
    handleReplace(activePath, `scope: ${activePath}`);
  });
  $('btn-replace-all')?.addEventListener('click', () => {
    handleReplace(null, 'all files');
  });

  updateUndoRedoButtons();
}

export { saveAll, saveActive };

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
  let pre = container.querySelector('.diff-fallback');
  if (!pre) {
    pre = document.createElement('pre');
    pre.className = 'diff-fallback';
    container.appendChild(pre);
  }
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
