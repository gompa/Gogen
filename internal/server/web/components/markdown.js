// Markdown rendering pipeline for the GoGen web UI: the shared
// text→sanitized-HTML renderer (marked + DOMPurify), the code-block
// copy-button enhancement, the @path:line linkifier, the .message-body
// wrapper helper, the single-shot full render (setMessageMarkdown) and
// the incremental streaming renderer (renderStreamMarkdown).
//
// This is the "given an element + text, render markdown into it" layer.
// The streaming STATE (currentStreamDiv, appendStreamToken, the rAF
// flush cadence) stays in app.js and drives renderStreamMarkdown; the
// ws handlers and history replay call setMessageMarkdown.
//
// Wiring: app.js calls initMarkdown(deps) once at startup.
//   deps.showToast(message, kind)      — the toast stack
//   deps.copyTextToClipboard(text)     — Promise<boolean> clipboard write
//   deps.getMessageRawStore()          — the WeakMap<el, rawText> (shared
//                                        with the sessions export/resend)
import { marked } from '/vendor/marked.esm.js';
import DOMPurify from '/vendor/dompurify.esm.js';
import { colorizeNode, openFileAtLine } from '/editor.js';

let deps = null;

export function initMarkdown(d) {
    deps = d;
}

function renderMarkdownHTML(text) {
    const raw = marked.parse(text || '', { async: false });
    return DOMPurify.sanitize(raw, { USE_PROFILES: { html: true } });
}

function enhanceCodeBlocksWithCopy(root) {
    if (!root || !root.querySelectorAll) return;
    root.querySelectorAll('pre').forEach((pre) => {
        if (pre.closest('.code-block-wrap')) return;
        const wrap = document.createElement('div');
        wrap.className = 'code-block-wrap';
        pre.parentNode.insertBefore(wrap, pre);
        // Header bar: language chip (when marked tagged the fence)
        // on the left, copy button on the right.
        const head = document.createElement('div');
        head.className = 'code-block-head';
        const codeEl = pre.querySelector('code');
        const langMatch = codeEl && codeEl.className.match(/language-([\w+#.-]+)/);
        if (langMatch) {
            const lang = document.createElement('span');
            lang.className = 'code-lang';
            lang.textContent = langMatch[1];
            head.appendChild(lang);
        }
        wrap.appendChild(head);
        wrap.appendChild(pre);
        const btn = document.createElement('button');
        btn.type = 'button';
        btn.className = 'code-copy-btn';
        btn.textContent = 'Copy';
        btn.title = 'Copy code';
        btn.addEventListener('click', async (e) => {
            e.preventDefault();
            e.stopPropagation();
            const code = pre.querySelector('code') || pre;
            const text = code.textContent || '';
            const ok = await deps.copyTextToClipboard(text);
            if (ok) {
                btn.textContent = 'Copied';
                setTimeout(() => { btn.textContent = 'Copy'; }, 1500);
            } else {
                deps.showToast('Copy failed', 'error');
            }
        });
        head.appendChild(btn);
    });
}

// Shared post-render pipeline for a markdown node: the (sanitized)
// HTML, copy buttons, and Monaco colorize. Cached tokenized blocks
// inline synchronously (one DOM write, no async); uncached ones
// colorize in the background (colorizeNode). `linkify` is enabled
// only for full single-shot renders — the streaming path linkifies
// once at stream end via setMessageMarkdown. `streamingTail` marks
// the in-flight tail render (content still growing, node re-rendered
// every flush): uncached code still tokenizes in the background so
// the block stays colorized as it streams (the pre-optimization
// behavior), but in the no-cache mode — no LRU writes for a source
// that is still growing, stale results dropped by element identity
// and text match.
function renderBlockNode(node, blockText, opts) {
    node.innerHTML = renderMarkdownHTML(blockText);
    enhanceCodeBlocksWithCopy(node);
    colorizeNode(node, { streamingTail: !!(opts && opts.streamingTail) });
    if (opts && opts.linkify) linkifyMessageRefs(node);
}

// Returns the .message-body wrapper that holds a bubble's flow
// content (markdown, timestamp, model chip), creating it lazily.
// The hover buttons (fork/resend/edit) and the inline-edit bar are
// appended to .message itself, OUTSIDE this wrapper: .message-body
// carries content-visibility: auto, whose paint containment would
// otherwise clip the buttons' overhang past the bubble's edge.
// Non-.message elements (e.g. thought-card bodies) pass through.
export function msgBody(el) {
    if (!el || !el.classList || !el.classList.contains('message')) return el;
    let body = el.querySelector('.message-body');
    if (!body) {
        body = document.createElement('div');
        body.className = 'message-body';
        el.appendChild(body);
    }
    return body;
}

// Full single-shot render (stream end, history replay, edit/resend,
// thinking finalize). The WHOLE text goes through marked at once:
// per-block renders (renderStreamMarkdown) can split lists at blank
// lines, and this pass is the artifact corrector. The block cache
// from a prior streaming phase must be dropped — its nodes are
// detached by the innerHTML wipe below, and renderStreamMarkdown's
// index-based reconciliation would otherwise trust stale entries.
export function setMessageMarkdown(el, text) {
    el.classList.add('md');
    // Wrap rendered content in a child element so edit-resend can
    // hide/show it without touching appended buttons.
    let textWrap = el.querySelector('.msg-text');
    if (!textWrap) {
        textWrap = document.createElement('div');
        textWrap.className = 'msg-text';
        msgBody(el).appendChild(textWrap);
    }
    delete el._gogenBlocks;
    deps.getMessageRawStore().set(el, text);
    renderBlockNode(textWrap, text, { linkify: true });
}

/**
 * Make `@path:line` (and `@path:start-end`) references in rendered
 * assistant messages clickable. Code blocks, links and already-wrapped
 * nodes are skipped. Clicking opens the file in the editor, reveals the
 * line and highlights the range (matches the "Add Reference to Chat"
 * context-menu format).
 */
function linkifyMessageRefs(root) {
    if (!root || !root.querySelectorAll) return;
    const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
    const candidates = [];
    while (walker.nextNode()) {
        const n = walker.currentNode;
        if (!n.nodeValue || !n.nodeValue.includes('@')) continue;
        const pe = n.parentElement;
        if (pe && pe.closest('pre, code, a')) continue;
        candidates.push(n);
    }
    const re = /(^|\s)@([\w./\\~-]+):(\d+)(?:-(\d+))?/g;
    for (const node of candidates) {
        const text = node.nodeValue;
        const frag = document.createDocumentFragment();
        let last = 0;
        let changed = false;
        let m;
        re.lastIndex = 0;
        while ((m = re.exec(text))) {
            changed = true;
            if (m.index > last) frag.appendChild(document.createTextNode(text.slice(last, m.index)));
            const path = m[2];
            const start = parseInt(m[3], 10);
            const end = m[4] ? parseInt(m[4], 10) : start;
            const span = document.createElement('span');
            span.className = 'file-ref';
            span.textContent = m[1] + '@' + path + ':' + m[3] + (m[4] ? '-' + m[4] : '');
            span.title = `Open ${path}:${start} in editor`;
            span.addEventListener('click', (e) => {
                e.stopPropagation();
                openFileAtLine(path, start, end).catch(() => {});
            });
            frag.appendChild(span);
            last = m.index + m[0].length;
        }
        if (changed) {
            if (last < text.length) frag.appendChild(document.createTextNode(text.slice(last)));
            node.parentNode.replaceChild(frag, node);
        }
    }
}

// ===== Incremental streaming render =====
// Streaming messages are re-rendered on a ~32ms cadence. Re-parsing,
// re-sanitizing and re-swapping the whole accumulated markdown on every
// flush is O(n²) in message length and forces a full reflow per flush.
// Instead we render stable markdown blocks once and only re-render the
// in-flight tail block. Block boundaries are conservative (blank lines
// outside fenced code blocks), so no paragraph is ever rendered
// partially. Any residual block-split artifacts (e.g. a list split at
// a blank line) are corrected by the final full render when the stream
// ends (see endStream/finalizeThinking).
// Splits accumulated stream text into conservative markdown blocks
// (blank lines outside fenced code). Returns { blocks, lastStart }:
// blocks[i] is the i-th block and lastStart is the offset in `text`
// where the LAST block began (text.length when there are no blocks).
// The offset lets the incremental renderer remember the in-flight
// block boundary and re-split only the tail on the next flush.
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
            // nothing but trailing whitespace after it. This keeps a
            // 3-tick line from closing a 4-tick outer fence (nested
            // code blocks) and "```js" from closing a fence.
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

// Incremental renderer for the live streaming paths (assistant stream
// and thinking). Per-element state lives on el._gogenBlocks; the DOM
// shape mirrors setMessageMarkdown (.msg-text wrapper) so edit/resend
// and history replay behave identically.
export function renderStreamMarkdown(el, text) {
    el.classList.add('md');
    let textWrap = el.querySelector('.msg-text');
    if (!textWrap) {
        textWrap = document.createElement('div');
        textWrap.className = 'msg-text';
        msgBody(el).appendChild(textWrap);
    }
    const st = el._gogenBlocks || (el._gogenBlocks = { done: [], tailNode: null, processedLen: 0, lastText: null });

    // Incremental split: the streaming text for a given element is
    // append-only (appendStreamToken appends; rewinds start a fresh
    // element; setMessageMarkdown deletes this state), so
    // splitStreamBlocks is prefix-stable — completed blocks before
    // st.processedLen can never change. We therefore re-split only the
    // tail (from the last in-flight block boundary to the end) instead
    // of the whole message, keeping each flush O(tail) rather than
    // O(n). st.processedLen is the offset where the in-flight block
    // began; text[processedLen-1] is the blank line that ends the last
    // completed block, which doubles as a cheap staleness probe.
    //
    // The guards cover the ways the cache can go stale, all O(1) in the
    // common append path: a rewind (text shrank below processedLen), a
    // boundary rewrite (the blank line that ends the last completed
    // block is gone), a same-length content rewrite (text is the same
    // length but differs — the O(n) compare only runs when the length
    // is unchanged, i.e. no new token arrived, so it never costs on the
    // hot path), or a full re-render that detached the nodes while the
    // cache survived (setMessageMarkdown deletes it, but the isConnected
    // check keeps this safe). A longer rewrite (prefix changed but the
    // text grew) is impossible under the append-only invariant — a
    // rewind starts a fresh element — so it is deliberately not probed
    // (doing so would require an O(n) prefix compare every flush, which
    // is exactly the cost this optimization removes). On staleness we
    // drop the cached nodes and re-split from offset 0.
    const stale = text.length < st.processedLen
        || (st.processedLen > 0 && text[st.processedLen - 1] !== '\n')
        || (st.lastText !== null && text.length === st.lastText.length && text !== st.lastText)
        || (st.done.length && !st.done[0].node.isConnected);
    if (stale) {
        for (const entry of st.done) entry.node.remove();
        st.done = [];
        st.processedLen = 0;
    }

    const tail = text.slice(st.processedLen);
    const { blocks: tailBlocks, lastStart: tailLastStart } = splitStreamBlocks(tail);
    // The last block of the tail is the in-flight block; everything
    // before it is a newly-completed block (promoted from a prior
    // tail). The in-flight block is re-rendered every flush; completed
    // blocks are rendered once and cached.
    const inFlight = tailBlocks[tailBlocks.length - 1] || '';
    const newDone = tailBlocks.slice(0, tailBlocks.length - 1);

    // Completed blocks: render only the new ones, inserted before the
    // tail node so DOM order always matches document order.
    for (const block of newDone) {
        const node = document.createElement('div');
        node.className = 'md-block';
        renderBlockNode(node, block, { linkify: false });
        if (st.tailNode) {
            textWrap.insertBefore(node, st.tailNode);
        } else {
            textWrap.appendChild(node);
        }
        st.done.push({ text: block, node });
    }

    // Tail: one stable node, re-rendered every flush. Markdown and
    // copy buttons re-run here; colorize re-tokenizes the tail's
    // uncached code every flush (streamingTail mode: no LRU writes,
    // stale results dropped) so streaming code stays colorized as it
    // grows — the UX the pre-optimization code provided.
    if (!st.tailNode) {
        st.tailNode = document.createElement('div');
        st.tailNode.className = 'md-block md-tail';
        textWrap.appendChild(st.tailNode);
    }
    renderBlockNode(st.tailNode, inFlight, { linkify: false, streamingTail: true });

    // Advance the boundary to the start of the in-flight block; it is
    // the only part re-split next flush. (When the tail is all blank
    // lines there is no in-flight block, so consume the whole tail.)
    st.processedLen += tailBlocks.length ? tailLastStart : tail.length;
    st.lastText = text;

    deps.getMessageRawStore().set(el, text);
}

// ===== Message timestamps =====
// Single relative-time helper for messages and session rows. `now` is
