'use strict';
// Regression test for the modulepreload hints (board #100): index.html
// preloads the app's static ESM graph so the browser fetches it in parallel
// instead of discovering each module through the import waterfall. That list
// is hand-maintained and must track the real imports — this test fails if:
//   1. a modulepreload href points at a file that does not exist (typo/stale),
//   2. a statically imported module (from app.js / editor.js / components/*.js)
//      is missing from the hints (a newly added module would silently fall back
//      to the waterfall), or
//   3. the same href is listed twice.
// Monaco and xterm are dynamically imported on demand and are intentionally
// NOT part of the static graph, so they are not expected here.
//
// Zero-dependency (plain node + fs). Run: node scripts/test_modulepreload.js
const fs = require('node:fs');
const path = require('node:path');

const ROOT = path.join(__dirname, '..');
const WEB = path.join(ROOT, 'internal/server/web');

let failures = 0;
function check(cond, msg) {
  if (cond) {
    console.log('  ok   ' + msg);
  } else {
    failures++;
    console.error('  FAIL ' + msg);
  }
}

// ── 1. modulepreload hrefs from index.html ──
const indexHtml = fs.readFileSync(path.join(WEB, 'index.html'), 'utf8');
const preloadHrefs = [];
const linkRe = /<link\s+rel="modulepreload"\s+href="([^"]+)"/g;
let m;
while ((m = linkRe.exec(indexHtml)) !== null) preloadHrefs.push(m[1]);
const preloadSet = new Set(preloadHrefs);

console.log('index.html hints:');
check(preloadHrefs.length > 0, 'index.html declares at least one modulepreload hint');
check(preloadSet.size === preloadHrefs.length, 'no duplicate modulepreload hrefs (' + preloadHrefs.length + ' listed)');

// ── 2. Every hinted href resolves to an embedded file ──
console.log('hinted files exist:');
for (const href of preloadHrefs) {
  if (!href.startsWith('/')) { check(false, `href not absolute: ${href}`); continue; }
  const file = path.join(WEB, href.slice(1));
  check(fs.existsSync(file), `hint ${href} → ${path.relative(ROOT, file)} exists`);
}

// ── 3. The static graph is fully covered by the hints ──
// Union of every root-absolute import specifier across the graph's own
// source files (app.js is the entry; editor.js + components are its modules).
const sources = ['app.js', 'editor.js'];
for (const f of fs.readdirSync(path.join(WEB, 'components')).filter((x) => x.endsWith('.js')).sort()) {
  sources.push(path.join('components', f));
}
const graph = new Set();
for (const rel of sources) {
  const src = fs.readFileSync(path.join(WEB, rel), 'utf8');
  for (const re of [/\bfrom\s+['"](\/[^'"]+)['"]/g, /\bimport\s+['"](\/[^'"]+)['"]/g]) {
    let mm;
    while ((mm = re.exec(src)) !== null) graph.add(mm[1]);
  }
}

console.log('static graph coverage:');
check(graph.size > 0, 'found static imports to check (' + graph.size + ' modules)');
const missing = [...graph].filter((spec) => !preloadSet.has(spec)).sort();
check(missing.length === 0, 'every statically imported module is preloaded' + (missing.length ? ' — missing: ' + missing.join(', ') : ''));

// Report hints that are not part of the static graph (informational, not a
// failure: a hint for a dynamically loaded module would be intentional but
// is currently unexpected).
const extra = [...preloadSet].filter((spec) => !graph.has(spec)).sort();
if (extra.length) console.log('  note: hints not in the static graph: ' + extra.join(', '));

if (failures) {
  console.error('\n' + failures + ' check(s) FAILED');
  process.exit(1);
}
console.log('\nall modulepreload checks passed');
