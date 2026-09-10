// Paint-skip for off-screen transcript items that preserves the real scroll
// geometry. `content-visibility: auto` lets the browser skip layout/paint for
// items outside the viewport, which is the win for long transcripts (tool
// cards, Monaco diff viewers, xterm) where every streaming reflow would
// otherwise cover the whole #messages box. The catch: a skipped item is sized
// by its `contain-intrinsic-size` fallback, NOT its real height, so for
// anything never yet rendered — the bulk of a replayed or pane-switched
// transcript — #messages.scrollHeight came out a fraction of the truth. The
// scrollbar, the scroll-to-bottom button (distanceFromBottom) and every
// scroll-up then lurched as the placeholders resolved.
//
// This module keeps the paint skip but measures each item's REAL height once,
// in a rAF-batched read-then-write pass, records it in the item's `--ps-h`
// custom property, and only then lets the item be skipped (the .paint-skip
// class in styles.css). So a skipped item always reports its true height.
// The browser's own last-remembered size (`auto` in the CSS value) still takes
// over once an item has rendered, which keeps items that grow while on screen
// (streaming text, async Monaco mounts) correct.
//
// Contract: hand an item here only once its content is FINAL. In-flight
// content (the live streaming bubble, a tool card still receiving output)
// must NOT be skipped: its height is still changing, and if it happens to be
// off-screen the measured value would be stale. app.js calls this from the
// finalization points (appendMessageAtTime, finalizeThinking, endStream,
// updateToolCardWithResult).
//
// Wiring: app.js imports enablePaintSkip. No init/deps.
const pending = [];
let rafId = 0;

// Queue el to be measured and paint-skipped on the next frame. Idempotent:
// an already-skipped element is left alone (its height is tracked by the
// browser's remembered size from then on).
export function enablePaintSkip(el) {
    if (!el || !el.classList || el.classList.contains('paint-skip')) return;
    pending.push(el);
    if (!rafId) rafId = requestAnimationFrame(paintSkipFlush);
}

function paintSkipFlush() {
    rafId = 0;
    const batch = pending.splice(0, pending.length);
    if (!batch.length) return;
    // Read every measurement BEFORE writing any style: one forced layout for
    // the whole batch instead of a read/write interleave per item. The
    // elements are not yet .paint-skip, so they are laid out normally here
    // and the measurement is their real height even when off-screen.
    //
    // contain-intrinsic-size sizes the CONTENT box, so subtract the vertical
    // padding+border from the measured (border-box) rect — otherwise a card
    // with vertical padding (the thought card's 12px) came out taller when
    // skipped than when rendered, and scrollHeight drifted.
    const sizes = batch.map((el) => {
        if (!el.isConnected) return null;
        const rect = el.getBoundingClientRect().height;
        const cs = getComputedStyle(el);
        const num = (v) => parseFloat(v) || 0;
        const yBox = num(cs.paddingTop) + num(cs.paddingBottom)
            + num(cs.borderTopWidth) + num(cs.borderBottomWidth);
        return Math.max(0, rect - yBox);
    });
    for (let i = 0; i < batch.length; i++) {
        const el = batch[i];
        if (!el.isConnected) continue;
        // Record the content-box height (sub-pixel included —
        // contain-intrinsic-size accepts fractions) so a later skip reports
        // exactly the same box.
        const h = sizes[i] > 0 ? Math.round(sizes[i] * 100) / 100 : 0;
        el.style.setProperty('--ps-h', h + 'px');
        el.classList.add('paint-skip');
    }
}
