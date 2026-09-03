// Standalone verification of splitStreamBlocks + the incremental render state
// machine used by renderStreamMarkdown (copied verbatim from app.js).
'use strict';

// Copied verbatim from app.js (returns { blocks, lastStart }).
function splitStreamBlocks(text) {
  const src = String(text);
  const blocks = [];
  let cur = '';
  let curStart = 0;   // offset where the current (in-flight) block began
  let lastStart = 0;  // offset where the most recently pushed block began
  let fence = null; // { mark: '`'|'~', len } while inside a fenced code block
  let offset = 0;
  for (const line of src.split('\n')) {
    const lineLen = line.length + 1; // +1 for the '\n'
    if (fence) {
      if (cur === '') curStart = offset;
      cur += line + '\n';
      // CommonMark closing rule: same char, length >= opener,
      // nothing but trailing whitespace after it.
      const c = /^\s*(`+|~+)\s*$/.exec(line);
      if (c && c[1][0] === fence.mark && c[1].length >= fence.len) fence = null;
      offset += lineLen;
      continue;
    }
    const m = /^\s*(```+|~~~+)/.exec(line);
    if (m) {
      if (cur === '') curStart = offset;
      cur += line + '\n';
      fence = { mark: m[1][0], len: m[1].length };
      offset += lineLen;
      continue;
    }
    if (line.trim() === '') {
      if (cur !== '') {
        blocks.push(cur);
        lastStart = curStart;
        cur = '';
      }
      offset += lineLen;
      continue;
    }
    if (cur === '') curStart = offset;
    cur += line + '\n';
    offset += lineLen;
  }
  if (cur !== '') {
    blocks.push(cur);
    lastStart = curStart;
  }
  return { blocks, lastStart: blocks.length ? lastStart : src.length };
}

const blocks = (text) => splitStreamBlocks(text).blocks;

// Simulate the per-element state from renderStreamMarkdown, including the
// incremental tail re-split (st.processedLen). Returns, for each flush, the
// list of stable done-block texts and the in-flight tail text — the same
// quantities the DOM shows. Also counts the split work (chars re-split) so
// the O(tail) vs O(n) behavior can be asserted.
function makeRenderer() {
  const state = { done: [], processedLen: 0, lastText: null };
  const frames = [];
  let totalSplitChars = 0;
  function render(text) {
    const stale = text.length < state.processedLen
      || (state.processedLen > 0 && text[state.processedLen - 1] !== '\n')
      || (state.lastText !== null && text.length === state.lastText.length && text !== state.lastText);
    if (stale) { state.done = []; state.processedLen = 0; }

    const tail = text.slice(state.processedLen);
    totalSplitChars += tail.length;
    const { blocks: tailBlocks, lastStart: tailLastStart } = splitStreamBlocks(tail);
    const inFlight = tailBlocks[tailBlocks.length - 1] || '';
    const newDone = tailBlocks.slice(0, tailBlocks.length - 1);
    for (const block of newDone) state.done.push({ text: block });
    state.processedLen += tailBlocks.length ? tailLastStart : tail.length;
    state.lastText = text;
    frames.push({ done: state.done.map((d) => d.text), tail: inFlight });
  }
  return { render, frames, work: () => totalSplitChars };
}

// The pre-optimization reference: full re-split every flush, last block is the
// tail, everything before it is a done block.
function fullSplitFrame(text) {
  const b = blocks(text);
  const doneCount = b.length > 0 ? b.length - 1 : 0;
  return { done: b.slice(0, doneCount), tail: doneCount < b.length ? b[doneCount] : '' };
}

let failures = 0;
function check(name, cond, detail) {
  if (!cond) { failures++; console.log('FAIL:', name, detail || ''); }
  else console.log('ok  :', name);
}

// --- Basic split (return type is now { blocks, lastStart }) ---
check('empty -> []', JSON.stringify(blocks('')) === '[]');
check('one para', JSON.stringify(blocks('hello')) === '["hello\\n"]');
check('two paras', JSON.stringify(blocks('a\n\nb')) === '["a\\n","b\\n"]');
check('fence kept whole', JSON.stringify(blocks('a\n\n```js\nx\n\nstill in fence\n```\n\nb')) ===
  '["a\\n","```js\\nx\\n\\nstill in fence\\n```\\n","b\\n"]');
check('unclosed fence is tail', JSON.stringify(blocks('a\n\n```js\nx')) ===
  '["a\\n","```js\\nx\\n"]');
check('~~~ fence', JSON.stringify(blocks('~~~\na\n~~~')) === '["~~~\\na\\n~~~\\n"]');
check('list no blank', JSON.stringify(blocks('- a\n- b')) === '["- a\\n- b\\n"]');
check('blank-only', JSON.stringify(blocks('\n\n')) === '[]');

// --- Nested / tricky fence cases (CommonMark closing rules) ---
check('4-tick outer holds 3-tick inner',
  JSON.stringify(blocks('````\n```js\nx\n```\n````')) ===
  '["````\\n```js\\nx\\n```\\n````\\n"]');
check('info-string line does not close fence',
  JSON.stringify(blocks('```\na\n```js\nb')) === '["```\\na\\n```js\\nb\\n"]');
check('longer closing fence closes opener',
  JSON.stringify(blocks('```\na\n````')) === '["```\\na\\n````\\n"]');
check('tilde fence not closed by backticks',
  JSON.stringify(blocks('~~~\na\n```\n')) === '["~~~\\na\\n```\\n\\n"]');

// --- lastStart points at the start of the last block ---
check('lastStart: two paras', splitStreamBlocks('a\n\nb').lastStart === 3,
  String(splitStreamBlocks('a\n\nb').lastStart));
check('lastStart: trailing blank (last block is completed)',
  splitStreamBlocks('a\n\nb\n\n').lastStart === 3,
  String(splitStreamBlocks('a\n\nb\n\n').lastStart));
check('lastStart: single in-flight fence', splitStreamBlocks('```\nx\n').lastStart === 0,
  String(splitStreamBlocks('```\nx\n').lastStart));
check('lastStart: empty -> src.length', splitStreamBlocks('\n\n').lastStart === 2,
  String(splitStreamBlocks('\n\n').lastStart));

// --- Incremental sequence matches the full-split reference exactly ---
// A realistic streaming transcript: paragraphs, lists, nested fences, and a
// fence that closes mid-sequence. Feed it one token-group at a time and assert
// the incremental frame (done blocks + tail) equals the full-split frame.
const transcript = [
  'Hello, I will help',
  'Hello, I will help\n\nHere is a list:\n- a\n- b',
  'Hello, I will help\n\nHere is a list:\n- a\n- b\n\n```js\nconst x = 1;',
  'Hello, I will help\n\nHere is a list:\n- a\n- b\n\n```js\nconst x = 1;\n```\n\nDone!',
  'Hello, I will help\n\nHere is a list:\n- a\n- b\n\n```js\nconst x = 1;\n```\n\nDone!\n\nFinal line',
];
const r = makeRenderer();
let seqOk = true;
for (const t of transcript) {
  r.render(t);
  const inc = r.frames[r.frames.length - 1];
  const ref = fullSplitFrame(t);
  if (JSON.stringify(inc) !== JSON.stringify(ref)) {
    seqOk = false;
    console.log('  mismatch at:', JSON.stringify(t), '\n   inc:', JSON.stringify(inc), '\n   ref:', JSON.stringify(ref));
  }
}
check('incremental frame == full-split frame over growth', seqOk);
const last = r.frames[r.frames.length - 1];
check('final DOM order has 5 blocks in order',
  JSON.stringify(last) === JSON.stringify({
    done: ['Hello, I will help\n', 'Here is a list:\n- a\n- b\n', '```js\nconst x = 1;\n```\n', 'Done!\n'],
    tail: 'Final line\n',
  }), JSON.stringify(last));

// --- Stress: 200 paragraphs + a long fence, one line per flush ---
// The key regression guard: the incremental renderer must produce the SAME
// done/tail as a full re-split at every step, while re-splitting only the
// tail (total split work far below the O(n^2) of re-splitting the whole
// message every flush).
const stress = makeRenderer();
let stressOk = true;
let fullWork = 0;
let text = '';
// Many small blank-line-separated paragraphs (the common case): each flush
// appends one paragraph, so the in-flight tail is small and the incremental
// renderer re-splits only it, while the pre-optimization code re-split the
// whole (growing) message.
const N = 500;
for (let i = 0; i < N; i++) {
  const para = 'paragraph number ' + i + ' with some words to make it a bit longer';
  text += (i ? '\n\n' : '') + para;
  fullWork += text.length; // what the pre-optimization code re-split this flush
  stress.render(text);
  const inc = stress.frames[stress.frames.length - 1];
  const ref = fullSplitFrame(text);
  if (JSON.stringify(inc) !== JSON.stringify(ref)) {
    stressOk = false;
    if (failures < 5) console.log('  stress mismatch at para', i);
  }
}
check('stress: incremental == full-split at every step', stressOk);
check('stress: final frame correct',
  JSON.stringify(stress.frames[stress.frames.length - 1]) === JSON.stringify(fullSplitFrame(text)));
// O(tail) evidence: total split work should be well under the O(n^2) full work.
const incWork = stress.work();
console.log('  split work: incremental=' + incWork + ' full=' + fullWork + ' ratio=' + (incWork / fullWork).toFixed(4));
check('stress: incremental split work << full re-split work', incWork < fullWork * 0.1,
  'inc=' + incWork + ' full=' + fullWork);

// --- A long in-flight fence: correctness holds (the fence is one block, so it
// is re-split whole while open — the inherent cost of detecting the close) ---
const fence = makeRenderer();
let fenceOk = true;
let ftext = 'intro\n\n```js\n';
fence.render(ftext);
for (let i = 0; i < 300; i++) {
  ftext += 'const line' + i + ' = ' + i + ';\n';
  fence.render(ftext);
  const inc = fence.frames[fence.frames.length - 1];
  const ref = fullSplitFrame(ftext);
  if (JSON.stringify(inc) !== JSON.stringify(ref)) fenceOk = false;
}
ftext += '```\n\noutro';
fence.render(ftext);
check('fence: incremental == full-split (open then closed)', fenceOk
  && JSON.stringify(fence.frames[fence.frames.length - 1]) === JSON.stringify(fullSplitFrame(ftext)));

// --- Staleness: a same-length rewrite of an early block triggers a reset ---
const rw = makeRenderer();
rw.render('alpha\n\nbeta');
rw.render('alpha\n\nbeta\n\ngamma');      // normal growth
const beforeLen = rw.frames[rw.frames.length - 1].done.length;
rw.render('ALPHA\n\nbeta\n\ngamma');      // same length, early block rewritten
const after = rw.frames[rw.frames.length - 1];
check('rewrite resets cache (done rebuilt from scratch)',
  after.done.length === beforeLen && after.done[0] === 'ALPHA\n',
  JSON.stringify(after));

// --- Staleness: a shrink (rewind) triggers a reset ---
const sh = makeRenderer();
sh.render('alpha\n\nbeta\n\ngamma');
sh.render('alpha');                        // shrank
check('shrink resets cache',
  JSON.stringify(sh.frames[sh.frames.length - 1]) === JSON.stringify(fullSplitFrame('alpha')),
  JSON.stringify(sh.frames[sh.frames.length - 1]));

console.log(failures === 0 ? '\nALL CHECKS PASSED' : `\n${failures} CHECK(S) FAILED`);
process.exit(failures === 0 ? 0 : 1);
