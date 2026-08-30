'use strict';
// Shared loader for the web UI regression tests (scripts/test_*.js).
//
// Evals the REAL app.js + web/components/*.js into a jsdom window with
// the ESM import/export syntax stripped: the component modules are eval'd
// first (dependency order), so their top-level declarations land in the
// global scope and app.js's bare references resolve — the same mechanism
// the editor.js / marked / DOMPurify window stubs use. app.js's own
// top-level function declarations become window properties, which is how
// the tests drive the UI (window.openBoardStartPopover, ...).
//
// Stubs (window.WebSocket, window.marked, editor functions, ...) must be
// installed BEFORE calling loadAppJs: app.js runs top-level code
// (marked.use, DOM wiring, connect()) during the eval.
const fs = require('fs');
const path = require('path');

// The editor.js functions app.js imports (the import is stripped above, so
// bare references resolve to the global scope and need a stub). Install
// these BEFORE loadAppJs. The conditional assignment is the contract: a
// test that pre-sets or later overrides a name (real-ish openModal/closeModal,
// a colorizeNode spy, ...) keeps its own implementation.
const EDITOR_STUBS = [
  'connectEditorSocket', 'setupEditorUI', 'refreshExplorer', 'disposeChatEditors',
  'mountDiffEditor', 'updateDiffEditor', 'updateDiffFallback', 'chatDiffWheelEdge',
  'extractDiffValue', 'initMonaco', 'colorizeNode', 'colorizeElement',
  'languageFromPath', 'setToastHandler', 'focusFindInFiles', 'editorUndo',
  'editorRedo', 'saveAll', 'saveActive', 'openFileAtLine', 'setMonacoTheme',
  'applyEditorPrefs', 'applyPatchFromDiff', 'openModal', 'closeModal',
];

function installEditorStubs(window, names = EDITOR_STUBS) {
  for (const name of names) {
    window[name] = window[name] || (() => Promise.resolve());
  }
}

const ROOT = path.join(__dirname, '..');

// Component modules app.js imports, in eval (dependency) order.
const COMPONENT_MODULES = [
  // Leaf module (no imports of its own); first because several components
  // and app.js call icon() — its top-level declaration must land in the
  // global scope before any of them eval.
  'internal/server/web/components/icons.js',
  'internal/server/web/components/popover.js',
  'internal/server/web/components/model-picker.js',
  'internal/server/web/components/board.js',
  'internal/server/web/components/toc.js',
  'internal/server/web/components/terminal.js',
  'internal/server/web/components/sessions.js',
  'internal/server/web/components/settings.js',
];

// Strips the module syntax the tests run as classic scripts:
//   - import { a, b } from '...'  (multi-line, as in app.js's editor.js import)
//   - import X from '...'
//   - export function/async function/const/let/var/class declarations
function stripModuleSyntax(src) {
  return src
    .replace(/import\s*\{[^}]*\}\s*from\s*['"][^'"]+['"];\s*/gs, '')
    .replace(/import\s+[A-Za-z_$][\w$]*\s+from\s*['"][^'"]+['"];\s*/g, '')
    .replace(/\bexport\s+(?=function|async\s+function|const|let|var|class)\s*/g, '');
}

// Evals the component modules (dependency order), then app.js, into the
// given jsdom window. APPJS env override (as before) for app.js only.
function loadAppJs(window) {
  for (const rel of COMPONENT_MODULES) {
    window.eval(stripModuleSyntax(fs.readFileSync(path.join(ROOT, rel), 'utf8')));
  }
  const appJs = process.env.APPJS || path.join(ROOT, 'internal/server/web/app.js');
  window.eval(stripModuleSyntax(fs.readFileSync(appJs, 'utf8')));
}

module.exports = { ROOT, COMPONENT_MODULES, stripModuleSyntax, loadAppJs, EDITOR_STUBS, installEditorStubs };
