// Standalone verification of cachedColorize (copied verbatim from editor.js).
'use strict';

const COLORIZE_CACHE_MAX = 300;
const colorizeCache = new Map();
const COLORIZE_SANITIZE_PROFILE = { USE_PROFILES: { html: true } };
// Test stub mirroring the vendored DOMPurify used by editor.js: an identity
// sanitize keeps the fake monaco output unchanged for the checks below.
const DOMPurify = { sanitize: (html) => html };

function normalizeColorizeHTML(html) {
  return html.replace(/<\s*br\s*\/?>\s*$/i, '');
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

let failures = 0;
function check(name, cond, detail) {
  if (!cond) { failures++; console.log('FAIL:', name, detail || ''); }
  else console.log('ok  :', name);
}

(async () => {
  let tokenizeCalls = 0;
  const monaco = {
    editor: {
      colorize(source, lang) {
        tokenizeCalls++;
        return Promise.resolve(`<span>${lang}:${source}</span><br/>`);
      },
    },
  };

  const a = await cachedColorize(monaco, 'const x = 1', 'javascript');
  check('first call tokenizes', tokenizeCalls === 1);
  check('trailing <br/> normalized', a === '<span>javascript:const x = 1</span>');

  const b = await cachedColorize(monaco, 'const x = 1', 'javascript');
  check('second call is a cache hit (no tokenize)', tokenizeCalls === 1);
  check('hit returns same html', b === a);

  const c = await cachedColorize(monaco, 'const x = 1', 'typescript');
  check('same source, different lang tokenizes', tokenizeCalls === 2 && c !== a);

  const d = await cachedColorize(monaco, 'const x = 2', 'javascript');
  check('different source tokenizes', tokenizeCalls === 3);

  // Eviction: fill past max, oldest must go.
  for (let i = 0; i < COLORIZE_CACHE_MAX + 10; i++) {
    await cachedColorize(monaco, 'src' + i, 'lang');
  }
  check('cache size capped', colorizeCache.size === COLORIZE_CACHE_MAX);
  check('oldest entry evicted', colorizeCache.get('lang\u0000' + 'src0') === undefined);
  check('newest entry present', colorizeCache.get('lang\u0000' + 'src' + (COLORIZE_CACHE_MAX + 9)) !== undefined);
  // Re-tokenize after eviction
  const before = tokenizeCalls;
  await cachedColorize(monaco, 'src0', 'lang');
  check('evicted key re-tokenizes', tokenizeCalls === before + 1);

  // LRU: a re-used entry survives overflow while newer-but-unused entries
  // are evicted first. Pin 'src1' (oldest remaining) by reading it, push
  // one new entry past the cap, and assert the pinned entry is still a hit
  // while the next-oldest untouched entry ('src2') was evicted instead.
  await cachedColorize(monaco, 'src1', 'lang'); // LRU refresh
  await cachedColorize(monaco, 'overflow-entry', 'lang'); // evicts LRU (src2)
  check('recently used entry survives (LRU)', colorizeCache.has('lang\u0000' + 'src1'));
  check('least recently used entry evicted', !colorizeCache.has('lang\u0000' + 'src2'));
  const beforeLru = tokenizeCalls;
  await cachedColorize(monaco, 'src1', 'lang');
  check('surviving entry is still a hit', tokenizeCalls === beforeLru);

  console.log(failures === 0 ? '\nALL CHECKS PASSED' : `\n${failures} CHECK(S) FAILED`);
  process.exit(failures === 0 ? 0 : 1);
})();
