// Standalone verification of cachedColorize (copied verbatim from editor.js).
'use strict';

const COLORIZE_CACHE_MAX = 300;
const colorizeCache = new Map();

function normalizeColorizeHTML(html) {
  return html.replace(/<\s*br\s*\/?>\s*$/i, '');
}

function cachedColorize(m, source, lang) {
  const key = lang + '\u0000' + source;
  const hit = colorizeCache.get(key);
  if (hit !== undefined) return Promise.resolve(hit);
  return m.editor.colorize(source, lang, {}).then((raw) => {
    const html = normalizeColorizeHTML(raw);
    if (colorizeCache.size >= COLORIZE_CACHE_MAX) {
      const oldest = colorizeCache.keys().next().value;
      if (oldest !== undefined) colorizeCache.delete(oldest);
    }
    colorizeCache.set(key, html);
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

  console.log(failures === 0 ? '\nALL CHECKS PASSED' : `\n${failures} CHECK(S) FAILED`);
  process.exit(failures === 0 ? 0 : 1);
})();
