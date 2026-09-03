// Chat auto-scroll / follow system for the GoGen web UI: the sticky
// stick-to-bottom state, the scroll-to-bottom jump button, the
// wheel/touch/keyboard unpin gestures, the re-pin machinery (grace
// window, near-bottom recovery, rAF-coalesced re-pin scheduling) and
// smartScroll (the per-token follow used while streaming).
//
// Owns the `stickToBottom` / `ignoreScrollEvent` state that used to live
// at app.js module scope; app.js reaches it through the exported
// isPinned / enableFollow / smartScroll / pinToBottom / unpinFromBottom /
// distanceFromBottom / updateScrollBottomBtn / scheduleRepinIfPinned.
//
// Wiring: app.js calls initScroll(deps) once at startup.
//   deps.isReplaying() — true while replayHistory() rebuilds the pane
//                        (suppresses smartScroll / re-pin)
//
// The sidebar-drag flag (sidebarDragActive) also lives here: the scroll
// ResizeObserver and the rAF re-pin probe skip the expensive TOC work
// mid-drag, and initSidebarResize (app.js) toggles it via
// sidebarDragStart / sidebarDragEnd.
import { chatDiffWheelEdge } from '/editor.js';
import { updateTocActive, syncTocRailBox, hideTocTooltip } from '/components/toc.js';

const messagesDiv = document.getElementById('messages');
const inputArea = document.getElementById('message-input');

let deps = null;

export function initScroll(d) {
    deps = d;
}

// Sticky auto-scroll. Cleared immediately on user scroll-up; re-enabled
// only when the user returns near the bottom (or clicks the jump button).
let stickToBottom = true;
let ignoreScrollEvent = false;
// Start with anchoring disabled while following the stream.
messagesDiv.classList.add('no-anchor');
// Re-enabling follow must always re-disable browser scroll anchoring:
// otherwise overflow-anchor fights smartScroll's scroll-to-bottom and
// the view stops following (drifts up) as content grows below — this
// surfaced when re-enabling follow after reading a diff/patch.
export function enableFollow() {
    stickToBottom = true;
    messagesDiv.classList.add('no-anchor');
}

export function isPinned() {
    return stickToBottom;
}

// === Scroll-to-bottom button ===
const scrollBottomBtn = document.getElementById('scroll-bottom-btn');
const NEAR_BOTTOM_PX = 32;
// Shared with app.js's initToc deps as a function (the web test harness
// evals each module in its own scope, so only function declarations are
// visible cross-module — the components/ sharing convention).
export function nearBottomPx() {
    return NEAR_BOTTOM_PX;
}
// Re-pin is suppressed for this long after an unpin so a single small
// wheel/touch nudge doesn't unpin and then immediately re-pin (which
// made streaming smartScroll yank the view back down mid-gesture).
const UNPIN_REPIN_GRACE_MS = 400;
// Touch drags shorter than this (in CSS px) are taps / accidental
// finger drift / momentum, not scroll intent. The old 2px threshold
// un-pinned on tap drift, pinch-zoom and overscroll.
const TOUCH_UNPIN_PX = 10;
// Vertically scrollable children inside #messages (Monaco diff
// viewers, tool-result bodies, diff fallbacks) consume wheel/touch
// input of their own. Events bubbling up from them must not un-pin the
// chat: the user is scrolling the diff, not leaving the bottom. Real
// chat scrolls still un-pin via the scroll handler below (native
// scroll chaining fires a #messages scroll event).
const NESTED_SCROLLER_SELECTOR = [
    '.monaco-scrollable-element', // Monaco editor internals
    '.monaco-tool-host',          // diff viewer host
    '.diff-fallback',             // pre-Monaco diff fallback
    '.tool-result-content',       // scrollable tool output bodies
    '.tool-live-output',          // live tool-output region (re-pins itself per chunk)
].join(', ');
// Timestamp (performance.now) of the last unpin; gates the d<=8
// re-pin branch in the scroll handler (see UNPIN_REPIN_GRACE_MS).
let lastUnpinAt = 0;
let unpinRecoverTimer = null;

export function distanceFromBottom() {
    return messagesDiv.scrollHeight - messagesDiv.scrollTop - messagesDiv.clientHeight;
}

// d: optional pre-computed distance from bottom (callers that already
// measured it in this frame pass it in to avoid a second forced
// layout on #messages).
export function updateScrollBottomBtn(d) {
    if (d === undefined) d = distanceFromBottom();
    scrollBottomBtn.classList.toggle('visible', !stickToBottom || d > NEAR_BOTTOM_PX);
}

export function unpinFromBottom() {
    if (!stickToBottom) return;
    stickToBottom = false;
    lastUnpinAt = performance.now();
    updateScrollBottomBtn();
    // Re-enable browser scroll anchoring now that the user is reading
    // (prevents content from jumping when the streaming div changes height).
    messagesDiv.classList.remove('no-anchor');
    // A tiny nudge (or a false-positive unpin) can leave the view just
    // above the bottom with no further scroll events to re-pin it.
    // Once the grace period passes, if the user is still within the
    // "truly at the bottom" threshold, resume following instead of
    // leaving the view stranded a few pixels up in history.
    clearTimeout(unpinRecoverTimer);
    unpinRecoverTimer = setTimeout(() => {
        unpinRecoverTimer = null;
        if (stickToBottom) return;
        if (distanceFromBottom() <= 8) pinToBottom();
    }, UNPIN_REPIN_GRACE_MS + 50);
}

export function pinToBottom() {
    stickToBottom = true;
    clearTimeout(unpinRecoverTimer);
    unpinRecoverTimer = null;
    // Disable browser scroll anchoring while we're auto-following
    // (prevents anchoring from fighting smartScroll's scroll-to-bottom).
    messagesDiv.classList.add('no-anchor');
    ignoreScrollEvent = true;
    // scrollTo with a top far beyond the max offset clamps to the
    // bottom without a prior scrollHeight read (which would force a
    // layout flush on every streaming frame).
    messagesDiv.scrollTo({ top: 1 << 30 });
    requestAnimationFrame(() => { ignoreScrollEvent = false; });
    scrollBottomBtn.classList.remove('visible');
    updateTocActive();
}

// Unpin as soon as the user scrolls up — don't wait for >120px distance,
// or streaming smartScroll will yank them back before they escape.
// Only unpin when there is actually content to scroll: a non-scrollable
// pane can't fire a scroll event to re-pin, leaving the button stuck.
// Wheel/touch/keyboard events bubbling up from nested scrollable
// children (Monaco diff viewers, tool results) are ignored: the user
// is interacting with that content, not leaving the bottom.
const messagesScrollable = () => messagesDiv.scrollHeight > messagesDiv.clientHeight;
messagesDiv.addEventListener('wheel', (e) => {
    if (!messagesScrollable()) return;
    // Any wheel-up leaves the bottom; the recovery machinery (scroll
    // back down, maybeRepinNearBottom, the jump button) re-engages
    // follow. Wheels over nested scrollable children (Monaco diff
    // viewers, tool results, diff fallbacks) scroll that content,
    // not the chat — skip them so reading a diff card can't silently
    // un-pin the chat (a diff-edge wheel that chains to the chat
    // still un-pins via the scroll event it generates below).
    if (e.deltaY < 0) {
        const t = e.target;
        const inNested = t && typeof t.closest === 'function'
            && t.closest(NESTED_SCROLLER_SELECTOR);
        if (!inNested) unpinFromBottom();
    }
}, { passive: true });
// Capture-phase wheel takeover for Monaco diff viewers: Firefox's
// wheel chaining over Monaco can swallow edge wheels, trapping the
// cursor inside the diff. Capture runs before Monaco's handlers;
// preventDefault also makes Monaco back off (it checks
// defaultPrevented), so no double scroll. Boundary detection uses
// Monaco's editor API (DOM scroll metrics on the scrollable element
// are unreliable). Mid-diff wheels are left to Monaco.
messagesDiv.addEventListener('wheel', (e) => {
    if (!messagesScrollable()) return;
    const edge = chatDiffWheelEdge(e);
    if (!edge.over) return;
    if (!((e.deltaY < 0 && edge.atTop) || (e.deltaY > 0 && edge.atBottom))) return;
    e.preventDefault();
    const px = e.deltaMode === 1 ? e.deltaY * 16
        : e.deltaMode === 2 ? e.deltaY * messagesDiv.clientHeight
        : e.deltaY;
    messagesDiv.scrollTop += px;
}, { capture: true });
messagesDiv.addEventListener('touchstart', () => {
    // Touch scroll direction is known on touchmove; mark intent on any
    // touch interaction away from bottom after move.
    messagesDiv._touchY = null;
}, { passive: true });
messagesDiv.addEventListener('touchmove', (e) => {
    const y = e.touches[0]?.clientY;
    if (y == null) return;
    if (messagesDiv._touchY != null && y > messagesDiv._touchY + TOUCH_UNPIN_PX) {
        // Finger moved down → content scrolls up. Touches inside a
        // nested scrollable (diff viewer, tool result) scroll that
        // content natively, not the chat — skip those.
        const t = e.target;
        const inNested = t && typeof t.closest === 'function'
            && t.closest(NESTED_SCROLLER_SELECTOR);
        if (messagesScrollable() && !inNested) unpinFromBottom();
    }
    messagesDiv._touchY = y;
}, { passive: true });
messagesDiv.addEventListener('keydown', (e) => {
    // ArrowUp/PageUp/Home pressed inside a Monaco editor or another
    // editable element move that editor's cursor, not the chat.
    const t = e.target;
    const inEditable = t && typeof t.closest === 'function' &&
        t.closest('textarea, input, [contenteditable="true"], .monaco-editor');
    if (inEditable) return;
    // Arrow keys on a focused button (fork/resend/expand/copy) don't
    // navigate anything — not chat-scroll intent.
    if (t && typeof t.closest === 'function' && t.closest('button')) return;
    if ((e.key === 'ArrowUp' || e.key === 'PageUp' || e.key === 'Home') && messagesScrollable()) unpinFromBottom();
});

// Throttle scroll handler to rAF to avoid layout thrashing during streaming.
let _scrollPending = false;
messagesDiv.addEventListener('scroll', (e) => {
    // Scroll events bubble up from nested scrollers: the diff
    // viewer's fallback <pre> restores its scrollTop on every
    // streaming delta, and the live-output region re-pins itself per
    // chunk — both fire real scroll events whose target is the inner
    // scroller, not #messages. Reacting to those (e.g. evaluating
    // the chat's distance right after the diff host grew it past the
    // fold) flipped stickToBottom off and permanently killed follow
    // while the stream continued below. Only scrolls of #messages
    // itself are chat-scroll intent; chained native scrolls that
    // actually move the chat fire on #messages and still pass.
    const t = e.target;
    if (t && typeof t.closest === 'function' && t.closest(NESTED_SCROLLER_SELECTOR)) return;
    // Programmatic smartScroll()/pinToBottom() set ignoreScrollEvent and
    // defer the reset by one frame. The flag is only meaningful at the
    // moment the event is dispatched (scroll events run as tasks before
    // the next rendering update's rAF callbacks), so classify HERE,
    // synchronously. Checking it later inside the throttled rAF let the
    // flag's own clear-rAF run first — the chat then treated its own
    // programmatic scroll as a user scroll and could wrongly unpin
    // (stickToBottom = false) when a diff card grew asynchronously
    // after the scroll (Monaco mount/resize, colorize, image loads).
    // User-initiated scrolls (wheel, trackpad, scrollbar drag) reach
    // this point with the flag clear and are measured below as before.
    if (ignoreScrollEvent) return;
    if (_scrollPending) return;
    _scrollPending = true;
    requestAnimationFrame(() => {
        _scrollPending = false;
        // A user scroll moves the hovered dot away from the fixed
        // tooltip's anchor — dismiss the preview.
        hideTocTooltip();
        const d = distanceFromBottom();
        if (d > NEAR_BOTTOM_PX) {
            // Clearly away from bottom (scrollbar drag, etc.)
            stickToBottom = false;
            messagesDiv.classList.remove('no-anchor');
        } else if (d <= 8 && performance.now() - lastUnpinAt > UNPIN_REPIN_GRACE_MS) {
            // Truly back at the bottom — re-enable follow. The grace
            // window keeps a single small wheel/touch nudge from
            // unpinning and then immediately re-pinning (which yanked
            // the view back down mid-gesture while streaming).
            // lastUnpinAt is only set by the eager unpin gestures
            // (wheel/touch/keydown), NOT by the d>32 branch above:
            // otherwise a continuous scroll back down would keep
            // refreshing it and the re-pin would never fire.
            stickToBottom = true;
            messagesDiv.classList.add('no-anchor');
            clearTimeout(unpinRecoverTimer);
            unpinRecoverTimer = null;
        }
        // 8 < d <= NEAR_BOTTOM_PX: leave stickToBottom alone so a small
        // upward scroll after wheel-unpin is not immediately re-pinned.
        // Pass the already-measured distance: both helpers would
        // otherwise each force a second layout in this frame.
        updateScrollBottomBtn(d);
        updateTocActive(d);
    });
});
scrollBottomBtn.addEventListener('click', () => {
    pinToBottom();
});

// Async code colorization, image/font loads, and layout resizes can grow
// the DOM after we've already scrolled. Re-adjust when pinned, coalesced
// to one rAF pass so bursts of events don't force repeated layouts.
let _repinRafPending = false;
// While detached, re-engage follow if the user is near the bottom.
// Needed because content growing below (streaming tokens, colorize,
// images, fonts, resizes) keeps moving the bottom away, so a plain
// scroll event can pass through the 8px threshold and never fire again.
// The threshold here is deliberately more forgiving (NEAR_BOTTOM_PX)
// than the scroll-event re-pin (8px): a tall last tool card (Monaco
// diff up to 400px, tool result up to 70vh) leaves its tail below the
// fold next to the composer toolbar, so a user at the *visual* bottom
// can still be >8px from the scroll bottom. If content grew while they
// are within ~2 lines of the bottom, they were effectively at the
// bottom — pull them to the new bottom.
// Returns the measured distance from bottom when it had to measure
// (undefined on early bail), so callers can hand it to
// updateTocActive and avoid a duplicate forced layout in the same
// frame.
export function maybeRepinNearBottom() {
    if (stickToBottom || deps.isReplaying()) return;
    // Don't yank the chat while the user is composing: typing resizes
    // the input row, which shrinks the messages viewport and moves the
    // bottom — that's not scroll intent and shouldn't trigger re-pin.
    if (document.activeElement === inputArea) return;
    if (performance.now() - lastUnpinAt <= UNPIN_REPIN_GRACE_MS) return;
    const d = distanceFromBottom();
    if (d <= NEAR_BOTTOM_PX) pinToBottom();
    return d;
}
export function scheduleRepinIfPinned() {
    if (_repinRafPending) return;
    _repinRafPending = true;
    requestAnimationFrame(() => {
        _repinRafPending = false;
        let d;
        if (stickToBottom) smartScroll();
        else d = maybeRepinNearBottom();
        // During a sidebar drag this per-frame probe is deferred to
        // the single sidebarDragEnd pass on release (see the
        // sidebarDragActive note above the ResizeObserver).
        // Pass the distance measured above (when any) so the TOC
        // probe doesn't force a second layout in this frame.
        if (!sidebarDragActive) updateTocActive(d);
    });
}
window.addEventListener('gogen-colorized', scheduleRepinIfPinned);

// <img> load doesn't bubble, so listen in the capture phase.
messagesDiv.addEventListener('load', (e) => {
    if (e.target && e.target.tagName === 'IMG') scheduleRepinIfPinned();
}, true);
if (document.fonts && document.fonts.ready) {
    document.fonts.ready.then(() => scheduleRepinIfPinned());
}
// True while a sidebar resize-handle drag is running (toggled by
// initSidebarResize in app.js via sidebarDragStart / sidebarDragEnd).
// Mid-drag we skip only the EXPENSIVE TOC work — updateTocActive's
// per-prompt getBoundingClientRect probe and syncTocRailBox's
// write/read interleave — while keeping scheduleRepinIfPinned alive,
// so a pinned chat still follows the bottom live while dragging.
// sidebarDragEnd runs one final sync on release (window blur covers a
// missed mouseup).
let sidebarDragActive = false;
export function sidebarDragStart() {
    sidebarDragActive = true;
}
export function sidebarDragEnd() {
    if (!sidebarDragActive) return;
    sidebarDragActive = false;
    updateTocActive();
    syncTocRailBox();
}

if (typeof ResizeObserver !== 'undefined') {
    // Fires when #messages' visible box changes (window/sidebar/composer
    // resize). scrollHeight growth is content, not the box, so the
    // streaming / colorize / image / font paths above cover that case.
    new ResizeObserver(() => {
        scheduleRepinIfPinned();
        if (!stickToBottom) updateScrollBottomBtn();
        // No direct updateTocActive() here: scheduleRepinIfPinned's
        // rAF already runs it exactly once per frame. Calling it in
        // the callback too probed every .message.user rect twice per
        // resize tick (sidebar drags made that per-pixel work).
        if (!sidebarDragActive) syncTocRailBox();
    }).observe(messagesDiv);
}

// Auto-scroll only while stickToBottom is set (user hasn't scrolled away).
// The immediate scrollTop gives low-latency follow during streaming
// (each token triggers smartScroll).  The gogen-colorized event
// re-invokes smartScroll after Monaco finishes coloring.
let _smartScrollRafPending = false;
export function smartScroll() {
    if (deps.isReplaying()) return;
    if (!stickToBottom) {
        maybeRepinNearBottom();
        return;
    }
    // Suppress the scroll event generated by this programmatic scroll.
    // The flag must survive until that event fires, so reset it on the
    // next frame (scroll events are dispatched as tasks before the next
    // rendering update's rAF callbacks run).
    ignoreScrollEvent = true;
    // scrollTo with a top far beyond the max offset clamps to the
    // bottom without a prior scrollHeight read (which would force a
    // layout flush on every streaming frame).
    messagesDiv.scrollTo({ top: 1 << 30 });
    requestAnimationFrame(() => { ignoreScrollEvent = false; });
    // Late layout (async Monaco mounting, syntax highlighting, image
    // or font loads, composer height changes) can grow the DOM after
    // the synchronous scroll above. Re-check on the next frame so the
    // bottom stays pinned even if no other event arrives afterwards.
    if (_smartScrollRafPending) return;
    _smartScrollRafPending = true;
    requestAnimationFrame(() => {
        _smartScrollRafPending = false;
        if (!stickToBottom || deps.isReplaying()) return;
        ignoreScrollEvent = true;
        messagesDiv.scrollTo({ top: 1 << 30 });
        requestAnimationFrame(() => { ignoreScrollEvent = false; });
    });
}
