'use strict';
// Regression test for components/storage.js — the safe localStorage wrappers.
//
// These exist so a storage-blocked browser (third-party-cookie blocking,
// hardened privacy modes, some Android in-app WebViews) cannot throw during
// module evaluation: several preferences are read at module top level
// (settings.js, editor.js), and an unguarded access there aborts the whole
// ESM import graph, leaving a blank page. The wrappers must instead fall
// back to a default (reads) or no-op (writes/removes).
//
// The module is loaded with the real source (imports/exports stripped) via a
// Function whose `localStorage` parameter shadows the global, so both the
// working and the throwing-storage paths are exercised deterministically
// without a DOM.
// Run: node scripts/test_storage.js
const fs = require('node:fs');
const path = require('node:path');
const { stripModuleSyntax } = require('./web-harness');

const ROOT = path.join(__dirname, '..');
const SOURCE = fs.readFileSync(
  path.join(ROOT, 'internal/server/web/components/storage.js'), 'utf8');

// Load the module against a supplied localStorage implementation.
function loadWith(localStorageImpl) {
  const factory = new Function(
    'localStorage',
    `${stripModuleSyntax(SOURCE)}\nreturn { storageGet, storageSet, storageRemove };`);
  return factory(localStorageImpl);
}

// A minimal Map-backed localStorage.
function fakeStorage() {
  const m = new Map();
  return {
    getItem: (k) => (m.has(k) ? m.get(k) : null),
    setItem: (k, v) => { m.set(k, v); },
    removeItem: (k) => { m.delete(k); },
  };
}

const blockedStorage = () => {
  const boom = () => { throw new Error('SecurityError: storage blocked'); };
  return { getItem: boom, setItem: boom, removeItem: boom };
};

let failures = 0;
const check = (desc, ok) => {
  console.log(`${ok ? 'PASS' : 'FAIL'}: ${desc}`);
  if (!ok) failures++;
};

// ── Normal round-trip ──
{
  const s = loadWith(fakeStorage());
  check('missing key returns the given default', s.storageGet('nope', 'dflt') === 'dflt');
  check('missing key returns null without a default', s.storageGet('nope') === null);

  s.storageSet('k', 'v');
  check('set then get round-trips', s.storageGet('k') === 'v');
  s.storageSet('n', 42);
  check('set coerces the value to a string', s.storageGet('n') === '42');

  s.storageRemove('k');
  check('remove clears the value', s.storageGet('k', 'gone') === 'gone');
}

// ── Storage blocked: every wrapper degrades instead of throwing ──
{
  const s = loadWith(blockedStorage());
  let threw = null;
  let fallback;
  let nullable;
  try {
    fallback = s.storageGet('k', 'fallback');
    nullable = s.storageGet('x');
    s.storageSet('k', 'v');
    s.storageRemove('k');
  } catch (e) {
    threw = e;
  }
  check('no wrapper throws when localStorage access raises', threw === null);
  check('storageGet returns the given default when blocked', fallback === 'fallback');
  check('storageGet returns null by default when blocked', nullable === null);
}

console.log(failures === 0 ? '\nALL CHECKS PASSED' : `\n${failures} CHECK(S) FAILED`);
process.exit(failures === 0 ? 0 : 1);
