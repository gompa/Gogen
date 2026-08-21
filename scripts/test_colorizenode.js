// Standalone verification of the block-level colorize machinery
// (colorizeNode / enqueueColorize / cachedColorize, copied verbatim from
// editor.js) that the markdown block renderer drives. Guards the
// language-resolution path: a fence whose language is a Monaco registry
// ALIAS but not in the static LANG_ALIASES map (e.g. "xhtml" -> html) must
// resolve through the registry before tokenizing — colorize with an
// unregistered id yields no tokens and the result is cached as "done",
// leaving the block permanently uncolored (the reasoning-context regression
// this test pins).
//
// Run: node scripts/test_colorizenode.js
'use strict';

// applyColorized dispatches on window — provide minimal globals (no jsdom).
global.window = { dispatchEvent() {} };
global.CustomEvent = class { constructor(type) { this.type = type; } };

// ── Verbatim copies from editor.js (keep in sync) ──
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

function normalizeColorizeHTML(html) {
  return html.replace(/<\s*br\s*\/?>\s*$/i, '');
}
const COLORIZE_CACHE_MAX = 300;
const colorizeCache = new Map();
const COLORIZE_SANITIZE_PROFILE = { USE_PROFILES: { html: true } };
// Test stub: mirrors the vendored DOMPurify used by editor.js (identity
// sanitize keeps the fake monaco output unchanged for the checks below).
const DOMPurify = { sanitize: (html) => html };

function cachedColorize(m, source, lang) {
  const key = lang + '\u0000' + source;
  const hit = colorizeCacheGet(key);
  if (hit !== undefined) return Promise.resolve(hit);
  return m.editor.colorize(source, lang, {}).then((raw) => {
    const html = DOMPurify.sanitize(normalizeColorizeHTML(raw), COLORIZE_SANITIZE_PROFILE);
    colorizeCachePut(key, html);
    return html;
  });
}

// LRU accessors (verbatim from editor.js): a hit re-inserts the key so
// re-used entries never age out behind one-shot ones.
function colorizeCacheGet(key) {
  const hit = colorizeCache.get(key);
  if (hit === undefined) return undefined;
  colorizeCache.delete(key);
  colorizeCache.set(key, hit);
  return hit;
}

function colorizeCachePut(key, html) {
  if (colorizeCache.size >= COLORIZE_CACHE_MAX) {
    const oldest = colorizeCache.keys().next().value;
    if (oldest !== undefined) colorizeCache.delete(oldest);
  }
  colorizeCache.set(key, html);
}

// Stand-in for the module-level `monaco` binding in editor.js (assigned by
// initMonaco); resolveMonacoLanguage reads it.
let monaco = null;

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

function langIdFromClass(code) {
  const classMatch = /(?:^|\s)language-(\S+)/.exec(code.className || '');
  if (!classMatch) return null;
  const id = String(classMatch[1]).trim().toLowerCase();
  if (!id) return null;
  return LANG_ALIASES[id] || id;
}

function applyColorized(code, html) {
  code.innerHTML = html;
  code.dataset.monacoColorized = '1';
  code.classList.add('monaco-colorized');
  // Notify scroll system that DOM height may have changed.
  window.dispatchEvent(new CustomEvent('gogen-colorized', { bubbles: false }));
}

function enqueueColorize(code, source, lang, opts) {
  const cache = !opts || opts.cache !== false;
  const requireTextMatch = !!(opts && opts.requireTextMatch);
  (async () => {
    try {
      const m = await initMonaco();
      if (!code.isConnected) return;
      const resolved = resolveMonacoLanguage(lang);
      if (!resolved || resolved === 'plaintext') return;
      const raw = await m.editor.colorize(source, resolved, {});
      if (!code.isConnected) return;
      // Single-threaded: nothing can mutate the element between this check
      // and applyColorized below, so one text comparison after the last
      // await is enough.
      if (requireTextMatch && code.textContent !== source) return;
      const html = DOMPurify.sanitize(normalizeColorizeHTML(raw), COLORIZE_SANITIZE_PROFILE);
      if (cache) colorizeCachePut(resolved + '\u0000' + source, html);
      applyColorized(code, html);
    } catch (_) {
      // Unknown / unloaded language or init failure — leave plain text.
    }
  })();
}

function colorizeNode(node, opts) {
  if (!node || !node.querySelectorAll) return;
  const codes = node.querySelectorAll('pre code');
  if (!codes.length) return;
  const streamingTail = !!(opts && opts.streamingTail);

  for (const code of codes) {
    const lang = langIdFromClass(code);
    if (!lang || lang === 'plaintext') continue;

    const source = code.textContent || '';
    if (!source.trim()) continue;

    const key = lang + '\u0000' + source;
    const hit = colorizeCacheGet(key);
    if (hit !== undefined) {
      applyColorized(code, hit);
    } else {
      enqueueColorize(code, source, lang, streamingTail ? { cache: false, requireTextMatch: true } : undefined);
    }
  }
}
// ── End verbatim copies ──

// Test-controlled initMonaco: enqueueColorize (module scope) calls this
// name; main() re-points it at the fake monaco before running the checks.
let initMonaco = async () => { throw new Error('initMonaco not configured'); };

let failures = 0;
function check(name, cond, detail) {
  if (!cond) { failures++; console.log('FAIL:', name, detail || ''); }
  else console.log('ok  :', name);
}

// Minimal fake code element (colorizeNode touches className, textContent,
// innerHTML, dataset, classList, isConnected).
function fakeCode(className, text) {
  const el = {
    className,
    textContent: text,
    innerHTML: '',
    dataset: {},
    isConnected: true,
    _classes: new Set(),
    classList: {
      add: (c) => el._classes.add(c),
      contains: (c) => el._classes.has(c),
    },
  };
  return el;
}
const fakeNode = (codes) => ({ querySelectorAll: () => codes });
const flush = () => new Promise((r) => setTimeout(r, 5));

(async () => {
  // Fake Monaco: registered languages with their aliases; colorize with an
  // UNREGISTERED id yields plain output with no token spans (mimics the real
  // standalone colorize's empty-tokenization fallback). Records the lang id
  // each colorize call receives.
  const registered = {
    html: { aliases: ['HTML', 'htm', 'html', 'xhtml'] },
    python: { aliases: ['py', 'gyp', 'ipython'] },
    javascript: { aliases: ['js', 'es', 'jsx', 'mjs', 'cjs'] },
    go: { aliases: ['golang'] },
  };
  const tokenizeLangs = [];
  monaco = {
    languages: {
      getLanguages: () => Object.keys(registered).map((id) => ({ id, aliases: registered[id].aliases })),
    },
    editor: {
      colorize(source, lang) {
        tokenizeLangs.push(lang);
        if (!(lang in registered)) {
          return Promise.resolve(`<div>${source}</div><br/>`); // unknown lang: no tokens
        }
        return Promise.resolve(`<span class="mtk1">${lang}:${source}</span><br/>`);
      },
    },
  };
  initMonaco = async () => monaco;

  // 1. Alias-gap fence ("xhtml" is a Monaco alias for html, not in the
  //    static map): must resolve to html and produce token spans. This is
  //    the regression: without the registry resolution, colorize receives
  //    the unregistered "xhtml" and the block stays plain forever.
  const code1 = fakeCode('language-xhtml', '<div>hi</div>');
  colorizeNode(fakeNode([code1]));
  await flush();
  check('alias-gap fence (xhtml) resolves to html for colorize', tokenizeLangs[tokenizeLangs.length - 1] === 'html',
    'got: ' + tokenizeLangs[tokenizeLangs.length - 1]);
  check('alias-gap fence block colored (mtk spans)', code1.innerHTML.includes('mtk1') && code1.classList.contains('monaco-colorized'),
    code1.innerHTML.slice(0, 60));
  check('cache stored under resolved id (html\\0source)', colorizeCache.has('html\u0000<div>hi</div>'));
  check('cache NOT stored under static id (xhtml\\0source)', !colorizeCache.has('xhtml\u0000<div>hi</div>'));

  // 2. Unknown fence language: skipped (no tokenize, no class, no cache).
  const tokenizeBefore = tokenizeLangs.length;
  const code2 = fakeCode('language-foo', 'bar');
  colorizeNode(fakeNode([code2]));
  await flush();
  check('unknown fence skipped (no tokenize)', tokenizeLangs.length === tokenizeBefore);
  check('unknown fence left plain (no class)', !code2.classList.contains('monaco-colorized'));
  check('unknown fence not cached', !colorizeCache.has('foo\u0000bar'));

  // 3. Static-map-covered alias ("py" -> python) still resolves.
  const code3 = fakeCode('language-py', 'print(1)');
  colorizeNode(fakeNode([code3]));
  await flush();
  check('covered alias (py) resolves to python', tokenizeLangs[tokenizeLangs.length - 1] === 'python');
  check('covered alias block colored', code3.innerHTML.includes('mtk1'));

  // 4. Sync inline hit: same (lang, source) on a fresh element applies
  //    synchronously from the cache — no extra tokenize call.
  const tokenizeBefore2 = tokenizeLangs.length;
  const code4 = fakeCode('language-py', 'print(1)');
  colorizeNode(fakeNode([code4]));
  check('sync cache hit inlines immediately (no await needed)', code4.classList.contains('monaco-colorized'));
  check('sync cache hit does not re-tokenize', tokenizeLangs.length === tokenizeBefore2);

  // 5. Plaintext fences are skipped by design.
  const tokenizeBefore3 = tokenizeLangs.length;
  const code5 = fakeCode('language-plaintext', 'whatever');
  colorizeNode(fakeNode([code5]));
  await flush();
  check('plaintext fence skipped', tokenizeLangs.length === tokenizeBefore3 && !code5.classList.contains('monaco-colorized'));

  // 6. streamingTail (streaming-tail renders): a cache MISS must still
  //    tokenize in the background — the per-flush streaming colorization
  //    the pre-optimization code provided — but must NOT write the shared
  //    cache (the tail source is still growing, so the key can never be hit
  //    again and would only evict a useful entry). A cache HIT still
  //    applies synchronously.
  const tokenizeBefore4 = tokenizeLangs.length;
  const code6 = fakeCode('language-go', 'func main() {}');
  colorizeNode(fakeNode([code6]), { streamingTail: true });
  await flush();
  check('streamingTail miss tokenizes', tokenizeLangs.length === tokenizeBefore4 + 1);
  check('streamingTail result applied', code6.classList.contains('monaco-colorized') && code6.innerHTML.includes('mtk1'));
  check('streamingTail miss not cached', !colorizeCache.has('go\u0000func main() {}'));

  // 6b. A flush that re-renders the tail detaches the captured element
  //     while the tokenize is in flight: the stale result must be dropped.
  const colorizeFn = monaco.editor.colorize;
  let finishColorize = null;
  monaco.editor.colorize = (source, lang) => new Promise((resolve) => {
    finishColorize = () => resolve(`<span class="mtk1">${lang}:${source}</span><br/>`);
  });
  const code6b = fakeCode('language-go', 'func main() {}');
  colorizeNode(fakeNode([code6b]), { streamingTail: true });
  await flush(); // macrotask tick: the tokenize reaches its in-flight await
  code6b.isConnected = false; // next flush re-rendered: old element detached
  finishColorize();
  await flush();
  check('stale tail result dropped (element detached)', !code6b.classList.contains('monaco-colorized'));
  monaco.editor.colorize = colorizeFn;

  const code7 = fakeCode('language-go', 'func main() {}');
  colorizeNode(fakeNode([code7]));
  await flush();
  check('normal render warms the cache', colorizeCache.has('go\u0000func main() {}'));

  const code8 = fakeCode('language-go', 'func main() {}');
  colorizeNode(fakeNode([code8]), { streamingTail: true });
  check('streamingTail cache hit still applies synchronously', code8.classList.contains('monaco-colorized'));

  console.log(failures === 0 ? '\nALL CHECKS PASSED' : `\n${failures} CHECK(S) FAILED`);
  process.exit(failures === 0 ? 0 : 1);
})().catch((err) => { console.error('TEST ERROR:', err); process.exit(2); });
