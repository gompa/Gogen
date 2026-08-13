// Standalone verification of splitStreamBlocks + the incremental render state
// machine used by renderStreamMarkdown (copied verbatim from app.js).
'use strict';

function splitStreamBlocks(text) {
  const blocks = [];
  let cur = '';
  let fence = null; // { mark: '`'|'~', len } while inside a fenced code block
  for (const line of String(text).split('\n')) {
    if (fence) {
      cur += line + '\n';
      // CommonMark closing rule: same char, length >= opener,
      // nothing but trailing whitespace after it.
      const c = /^\s*(`+|~+)\s*$/.exec(line);
      if (c && c[1][0] === fence.mark && c[1].length >= fence.len) fence = null;
      continue;
    }
    const m = /^\s*(```+|~~~+)/.exec(line);
    if (m) {
      cur += line + '\n';
      fence = { mark: m[1][0], len: m[1].length };
      continue;
    }
    if (line.trim() === '') {
      if (cur !== '') {
        blocks.push(cur);
        cur = '';
      }
      continue;
    }
    cur += line + '\n';
  }
  if (cur !== '') blocks.push(cur);
  return blocks;
}

// Simulate the per-element state from renderStreamMarkdown.
function makeRenderer() {
  const state = { done: [], tailNode: null };
  const log = [];
  function render(text) {
    const blocks = splitStreamBlocks(text);
    const doneCount = blocks.length > 0 ? blocks.length - 1 : 0;
    for (let i = 0; i < doneCount; i++) {
      const prev = state.done[i];
      if (prev && prev.text === blocks[i]) continue;
      const node = { html: 'R[' + blocks[i].trim() + ']' };
      if (prev) {
        state.done[i] = { text: blocks[i], node };
        log.push('REPLACE#' + i);
      } else if (state.tailNode) {
        state.done.push({ text: blocks[i], node });
        log.push('INSERT#' + i + ' before tail');
      } else {
        state.done.push({ text: blocks[i], node });
        log.push('APPEND#' + i);
      }
    }
    while (state.done.length > doneCount) {
      state.done.pop();
      log.push('DROP');
    }
    const tailText = doneCount < blocks.length ? blocks[doneCount] : '';
    if (!state.tailNode) state.tailNode = { html: '' };
    state.tailNode.html = tailText ? 'T[' + tailText.trim() + ']' : '';
    // Print DOM order
    const order = state.done.map((d, i) => 'done' + i + '=' + d.node.html).concat(['tail=' + state.tailNode.html]);
    log.push('ORDER: ' + order.join(' | '));
  }
  return { render, log };
}

let failures = 0;
function check(name, cond, detail) {
  if (!cond) { failures++; console.log('FAIL:', name, detail || ''); }
  else console.log('ok  :', name);
}

// --- Basic split ---
check('empty -> []', JSON.stringify(splitStreamBlocks('')) === '[]');
check('one para', JSON.stringify(splitStreamBlocks('hello')) === '["hello\\n"]');
check('two paras', JSON.stringify(splitStreamBlocks('a\n\nb')) === '["a\\n","b\\n"]');
check('fence kept whole', JSON.stringify(splitStreamBlocks('a\n\n```js\nx\n\nstill in fence\n```\n\nb')) ===
  '["a\\n","```js\\nx\\n\\nstill in fence\\n```\\n","b\\n"]');
check('unclosed fence is tail', JSON.stringify(splitStreamBlocks('a\n\n```js\nx')) ===
  '["a\\n","```js\\nx\\n"]');
check('~~~ fence', JSON.stringify(splitStreamBlocks('~~~\na\n~~~')) === '["~~~\\na\\n~~~\\n"]');
check('list no blank', JSON.stringify(splitStreamBlocks('- a\n- b')) === '["- a\\n- b\\n"]');
check('blank-only', JSON.stringify(splitStreamBlocks('\n\n')) === '[]');

// --- Nested / tricky fence cases (CommonMark closing rules) ---
// 4-tick outer fence containing a 3-tick inner fence: the 3-tick line must NOT
// close the outer block; the 4-tick line closes it.
check('4-tick outer holds 3-tick inner',
  JSON.stringify(splitStreamBlocks('````\n```js\nx\n```\n````')) ===
  '["````\\n```js\\nx\\n```\\n````\\n"]');
// Closing line with an info string ("```js") does NOT close per CommonMark.
check('info-string line does not close fence',
  JSON.stringify(splitStreamBlocks('```\na\n```js\nb')) ===
  '["```\\na\\n```js\\nb\\n"]');
// Longer closing fence (4 ticks) closes a 3-tick opener.
check('longer closing fence closes opener',
  JSON.stringify(splitStreamBlocks('```\na\n````')) ===
  '["```\\na\\n````\\n"]');
// Tilde fence is not closed by a backtick fence.
check('tilde fence not closed by backticks',
  JSON.stringify(splitStreamBlocks('~~~\na\n```\n')) ===
  '["~~~\\na\\n```\\n\\n"]');

// --- Incremental sequence simulation ---
const r = makeRenderer();
r.render('Hello, I will help');
r.render('Hello, I will help\n\nHere is a list:\n- a\n- b');
r.render('Hello, I will help\n\nHere is a list:\n- a\n- b\n\n```js\nconst x = 1;');
r.render('Hello, I will help\n\nHere is a list:\n- a\n- b\n\n```js\nconst x = 1;\n```\n\nDone!');
r.render('Hello, I will help\n\nHere is a list:\n- a\n- b\n\n```js\nconst x = 1;\n```\n\nDone!\n\nFinal line');

const last = r.log[r.log.length - 1];
check('final DOM order has 5 blocks in order', last ===
  'ORDER: done0=R[Hello, I will help] | done1=R[Here is a list:\n- a\n- b] | done2=R[```js\nconst x = 1;\n```] | done3=R[Done!] | tail=T[Final line]', last);

// No spurious REPLACE/DROP during monotonic growth
check('no replaces/drops during growth', !r.log.some(l => l.startsWith('REPLACE') || l === 'DROP'));

// Stable done blocks: re-render same text -> no DOM churn
const r2 = makeRenderer();
r2.render('a\n\nb\n\nc');
const before = r2.log[r2.log.length - 1];
r2.render('a\n\nb\n\nc\nd');
r2.render('a\n\nb\n\nc\nd\n\ne');
const after = r2.log[r2.log.length - 1];
check('steady-state keeps done blocks stable', after ===
  'ORDER: done0=R[a] | done1=R[b] | done2=R[c\nd] | tail=T[e]', after);

console.log(failures === 0 ? '\nALL CHECKS PASSED' : `\n${failures} CHECK(S) FAILED`);
process.exit(failures === 0 ? 0 : 1);
