// Terminal panel for the GoGen web UI: a docked strip at the bottom of
// the page (collapsed by default) with terminal tabs: a pinned
// interactive "User" shell (the default terminal) plus one read-only
// tab per agent command (execute_command, run_tests, run_lint)
// streaming live output. On mobile the dock is replaced by the
// "Terminal" header tab (full-screen).
//
// Wiring: app.js calls initTerminal({ getWs }) once at startup; the
// chat-socket handlers (term_opened/term_output/term_exit and the
// user_term_* messages) call the exported terminal* functions.

let deps = null;

let terminalPanel = null;
let terminalBody = null;
let terminalTabsEl = null;
let terminalChevron = null;
let terminalResizeHandle = null;
let terminalTabBtn = null;
let terminalTabBadge = null;
let terminalHint = null;
let terminalMoreBtn = null;
let terminalMoreMenu = null;

const TERM_STORE_KEY = 'gogen.terminalPanel.v1';
const TERM_MAX_TABS = 8;
const TERM_MIN_HEIGHT = 120;
const TERM_MAX_HEIGHT_RATIO = 0.7;
const TERM_DEFAULT_HEIGHT = 280;
export const USER_TERM_ID = 'user';

const terminals = new Map(); // termId -> {id, tab, host, term, fit, statusEl, titleEl, restartEl, closable, user, done, success}
let terminalActiveId = null;
let userTermState = 'spawning'; // spawning | ready | dead
let userTermFocused = false;
let terminalExpanded = false;
let terminalHeight = TERM_DEFAULT_HEIGHT;
// True when xterm.js failed to load/init; the strip then shows the
// fallback hint instead of looking silently broken.
let terminalLoadFailed = false;
try {
    const stored = JSON.parse(localStorage.getItem(TERM_STORE_KEY) || '{}');
    terminalExpanded = !!stored.expanded;
    if (Number.isFinite(stored.height) && stored.height >= TERM_MIN_HEIGHT) {
        // The height is only capped at drag time; a stored height from a
        // taller window must not be reapplied unclamped on load, or the
        // panel overflows the body (which is fixed at 100vh).
        terminalHeight = Math.min(stored.height, terminalMaxHeight());
    }
} catch (_) { /* corrupt storage — keep defaults */ }

function terminalMaxHeight() {
    return Math.round(window.innerHeight * TERM_MAX_HEIGHT_RATIO);
}

function terminalSaveState() {
    try {
        localStorage.setItem(TERM_STORE_KEY, JSON.stringify({
            expanded: terminalExpanded,
            height: terminalHeight,
        }));
    } catch (_) {}
}

// The strip docks below the chat input bar, so the scroll-to-bottom
// button must clear it to hover over the messages area. Publish the
// strip's live height as --terminal-strip-h (read by #scroll-bottom-btn
// in styles.css). While the chat pane is hidden (editor pane active,
// or mobile) the height is 0 and the button returns to its base
// offset; a ResizeObserver keeps the variable fresh on pane switches.
function terminalRefreshStripVar() {
    document.documentElement.style.setProperty(
        '--terminal-strip-h', terminalPanel.offsetHeight + 'px'
    );
}

function terminalIsMobile() {
    return window.matchMedia('(max-width: 767px)').matches;
}

function terminalTheme() {
    const cs = getComputedStyle(document.documentElement);
    return {
        background: cs.getPropertyValue('--bg').trim() || '#1e1e1e',
        foreground: cs.getPropertyValue('--fg').trim() || '#d4d4d4',
        cursor: cs.getPropertyValue('--accent').trim() || '#569cd6',
    };
}

function terminalFit() {
    // Only fit when the terminal body is actually laid out; hidden or
    // zero-sized containers make xterm report bogus dimensions.
    if (!terminalPanel.classList.contains('expanded')) return;
    // The strip now lives inside the chat pane; when another pane is
    // active the panel is display:none and xterm would measure 0.
    if (!terminalPanel.getClientRects().length) return;
    const t = terminals.get(terminalActiveId);
    if (!t || !t.fit) return;
    try {
        let dims = t.fit.proposeDimensions();
        if (!dims && t.term) {
            // The terminal may have been opened while its container was
            // hidden (collapsed panel), leaving xterm's char-size
            // measurement at 0. Fit then refuses to do anything and the
            // screen keeps its default 80x24 size, spilling past the
            // panel. Resize with the current size to force a re-measure
            // now that the panel is laid out, then fit for real.
            t.term.resize(t.term.cols, t.term.rows);
            dims = t.fit.proposeDimensions();
        }
        if (dims) t.fit.fit();
        if (t.user) terminalSendResize(t);
    } catch (_) {}
}

let terminalFitRaf = 0;
export function terminalFitSoon() {
    if (terminalFitRaf) return;
    terminalFitRaf = requestAnimationFrame(() => {
        terminalFitRaf = 0;
        terminalFit();
    });
}

function terminalSetExpanded(expanded) {
    terminalExpanded = expanded;
    terminalPanel.classList.toggle('expanded', expanded);
    if (expanded) {
        terminalHeight = Math.min(terminalHeight, terminalMaxHeight());
        // The mobile overlay is positioned with top/bottom, so a pixel
        // height would override bottom and shrink it to a small strip.
        if (!terminalPanel.classList.contains('mobile-full')) {
            terminalPanel.style.height = terminalHeight + 'px';
        }
    } else {
        terminalPanel.style.height = '';
    }
    terminalChevron.textContent = expanded ? '▾' : '▴';
    terminalSaveState();
    terminalRefreshStripVar();
    if (expanded) {
        terminalFitSoon();
    } else if (terminalIsMobile()) {
        terminalCloseMobile();
    }
}

function terminalUpdateHint() {
    // Only the genuine failure case gets the fallback message: an
    // empty strip (no tabs yet, or all agent tabs closed) is normal
    // and shows nothing.
    terminalHint.style.display = (terminalLoadFailed && terminals.size === 0) ? '' : 'none';
}

// Creates a terminal tab and its xterm instance. opts:
//   title/tooltip — tab label + hover text
//   closable      — show a close button (agent tabs yes, user tab no)
//   user          — interactive terminal (stdin enabled) vs read-only
function terminalCreateTab(id, opts = {}) {
    const tab = document.createElement('div');
    tab.className = 'term-tab active' + (opts.user ? ' user-tab' : '');
    tab.title = opts.tooltip || opts.title || '';
    const titleEl = document.createElement('span');
    titleEl.className = 'term-tab-title';
    titleEl.textContent = opts.title || id;
    const statusEl = document.createElement('span');
    statusEl.className = 'term-tab-status';
    const restartEl = document.createElement('span');
    restartEl.className = 'term-tab-restart';
    restartEl.textContent = '↻';
    restartEl.title = 'Restart shell';
    restartEl.hidden = true;
    restartEl.addEventListener('click', (e) => {
        e.stopPropagation();
        terminalRestartUser();
    });
    const closeEl = document.createElement('span');
    closeEl.className = 'term-tab-close';
    closeEl.textContent = '✕';
    if (opts.closable === false) {
        closeEl.style.display = 'none';
    } else {
        closeEl.addEventListener('click', (e) => {
            e.stopPropagation();
            terminalClose(id);
        });
    }
    tab.appendChild(titleEl);
    tab.appendChild(statusEl);
    tab.appendChild(restartEl);
    tab.appendChild(closeEl);
    tab.addEventListener('click', () => terminalSelectAndFocus(id));
    terminalTabsEl.appendChild(tab);

    const host = document.createElement('div');
    host.className = 'term-host';
    const inst = document.createElement('div');
    inst.className = 'term-instance';
    host.appendChild(inst);
    terminalBody.appendChild(host);

    const term = new Terminal({
        disableStdin: !opts.user, // agent tabs are read-only mirrors
        cursorBlink: !!opts.user,
        convertEol: true,
        scrollback: 2000,
        fontSize: 12,
        fontFamily: getComputedStyle(document.documentElement)
            .getPropertyValue('--mono').trim() || 'monospace',
        theme: terminalTheme(),
    });
    let fit = null;
    try {
        const FA = (window.FitAddon && window.FitAddon.FitAddon) || window.FitAddon;
        if (FA) {
            fit = new FA();
            term.loadAddon(fit);
        }
    } catch (_) {}
    if (opts.user) {
        term.onData((d) => {
            const s = deps.getWs();
            if (userTermState === 'ready' && s && s.readyState === WebSocket.OPEN) {
                s.send(JSON.stringify({ type: 'user_term_input', content: d }));
            }
        });
    }
    term.open(inst);
    if (opts.user) {
        // xterm >= 6.0.0 removed onFocus/onBlur from the public
        // Terminal API; the textarea (created by open()) is public
        // and stable across versions, so track focus via its DOM
        // events (falling back to the typed events on older xterm).
        const markFocused = () => { userTermFocused = true; };
        const markBlurred = () => { userTermFocused = false; };
        if (typeof term.onFocus === 'function' && typeof term.onBlur === 'function') {
            term.onFocus(markFocused);
            term.onBlur(markBlurred);
        } else if (term.textarea) {
            term.textarea.addEventListener('focus', markFocused);
            term.textarea.addEventListener('blur', markBlurred);
        }
    }

    const entry = {
        id,
        tab,
        host,
        term,
        fit,
        statusEl,
        titleEl,
        restartEl,
        closable: opts.closable !== false,
        user: !!opts.user,
        done: false,
        success: false,
    };
    terminals.set(id, entry);
    return entry;
}

// Opens a read-only tab for one agent tool command.
export function terminalOpen(id, name, title) {
    if (terminals.has(id)) return;
    if (typeof window.Terminal !== 'function') {
        // xterm failed to load — degrade gracefully, the chat tool
        // cards still show the full result.
        console.error('xterm.js not loaded; terminal tabs disabled');
        return;
    }
    terminalPrune();
    const t = terminalCreateTab(id, { title: title || id, closable: true, user: false });
    // Echo the command like a shell prompt (title already includes "$ ").
    try { t.term.write('\x1b[90m' + (title || '') + '\x1b[0m\r\n'); } catch (_) {}
    // Select the new tab unless the user is actively typing in their
    // own terminal — don't yank focus away mid-command.
    if (!userTermFocused) terminalSelect(id);
    terminalUpdateHint();
    terminalFlashTab(id);
    terminalUpdateBadge();
    terminalMoreRender();
}

// The pinned default terminal: created on load, interactive, never
// pruned. It exists client-side regardless of the server shell state;
// user_term_opened/exit drive its ready/dead states.
export function terminalEnsureUserTab() {
    if (terminals.has(USER_TERM_ID)) return;
    if (typeof window.Terminal !== 'function') return;
    terminalCreateTab(USER_TERM_ID, {
        title: 'starting…',
        tooltip: 'User shell (interactive)',
        closable: false,
        user: true,
    });
    terminalSelect(USER_TERM_ID);
    terminalUpdateHint();
    terminalUpdateBadge();
    terminalMoreRender();
}

export function terminalUserOpened(title, wd) {
    terminalEnsureUserTab();
    const t = terminals.get(USER_TERM_ID);
    if (!t) return;
    userTermState = 'ready';
    t.titleEl.textContent = title || 'shell';
    t.tab.title = wd ? (title || 'shell') + ' — ' + wd : (title || 'shell');
    t.restartEl.hidden = true;
    t.done = false;
    t.statusEl.textContent = '';
    t.statusEl.className = 'term-tab-status';
    terminalSendResize(t);
    terminalUpdateBadge();
    terminalMoreRender();
}

export function terminalUserExited(code) {
    const t = terminals.get(USER_TERM_ID);
    userTermState = 'dead';
    if (!t) return;
    t.done = true;
    t.success = code === 0;
    t.tab.classList.add('done');
    t.statusEl.textContent = t.success ? '✓' : '✗';
    t.statusEl.classList.add(t.success ? 'ok' : 'err');
    t.restartEl.hidden = false;
    try {
        t.term.write('\r\n\x1b[90m[' + (t.success ? 'exited' : 'exited (' + code + ')') + ' — click ↻ to restart]\x1b[0m');
    } catch (_) {}
    terminalUpdateBadge();
    terminalMoreRender();
}

function terminalRestartUser() {
    if (userTermState !== 'dead') return;
    userTermState = 'spawning';
    const t = terminals.get(USER_TERM_ID);
    if (t) {
        t.done = false;
        t.tab.classList.remove('done');
        t.statusEl.textContent = '…';
        t.statusEl.className = 'term-tab-status';
        t.restartEl.hidden = true;
    }
    const s = deps.getWs();
    if (s && s.readyState === WebSocket.OPEN) {
        s.send(JSON.stringify({ type: 'user_term_request' }));
    }
}

// Sends the fitted terminal size to the server for the user shell.
function terminalSendResize(t) {
    if (!t || !t.fit || !t.term || userTermState !== 'ready') return;
    // While the panel is collapsed the terminal has no layout; a resize
    // here would report a bogus 1-row x 2-col size to the shell.
    if (!terminalPanel.classList.contains('expanded')) return;
    // Same for a panel hidden behind an inactive pane (editor view).
    if (!terminalPanel.getClientRects().length) return;
    try {
        const dims = t.fit.proposeDimensions();
        const s = deps.getWs();
        if (dims && dims.cols > 0 && dims.rows > 0 && s && s.readyState === WebSocket.OPEN) {
            s.send(JSON.stringify({
                type: 'user_term_resize',
                cols: Math.max(2, Math.round(dims.cols)),
                rows: Math.max(2, Math.round(dims.rows)),
            }));
        }
    } catch (_) {}
}

export function terminalWrite(id, chunk) {
    const t = terminals.get(id);
    if (!t || !chunk || t.done) return;
    try { t.term.write(chunk); } catch (_) {}
}

export function terminalExit(id, success) {
    const t = terminals.get(id);
    if (!t || t.done) return;
    t.done = true;
    t.success = success;
    t.tab.classList.add('done');
    t.statusEl.textContent = success ? '✓' : '✗';
    t.statusEl.classList.add(success ? 'ok' : 'err');
    try {
        t.term.write('\r\n\x1b[90m[' + (success ? 'exit 0' : 'failed') + ']\x1b[0m');
    } catch (_) {}
    terminalUpdateBadge();
    terminalMoreRender();
}

function terminalSelect(id) {
    terminalActiveId = id;
    for (const [tid, t] of terminals) {
        t.tab.classList.toggle('active', tid === id);
        t.host.style.display = tid === id ? '' : 'none';
        if (tid !== id && t.user && t.term) {
            // Blur the interactive terminal so keystrokes don't keep
            // flowing into a hidden tab.
            try { t.term.blur(); } catch (_) {}
        }
    }
    const t = terminals.get(id);
    if (t && t.user && userTermState === 'ready') terminalSendResize(t);
    terminalMoreRender();
    terminalFitSoon();
}

function terminalSelectAndFocus(id) {
    terminalSelect(id);
    const t = terminals.get(id);
    if (t && t.user && userTermState === 'ready' && t.term) {
        try { t.term.focus(); } catch (_) {}
    }
}

function terminalClose(id) {
    const t = terminals.get(id);
    if (!t) return;
    try { t.term.dispose(); } catch (_) {}
    t.tab.remove();
    t.host.remove();
    terminals.delete(id);
    if (terminalActiveId === id) {
        terminalActiveId = null;
        const remaining = [...terminals.keys()];
        if (remaining.length > 0) terminalSelect(remaining[remaining.length - 1]);
    }
    terminalUpdateHint();
    terminalUpdateBadge();
    terminalMoreRender();
}

// Keep the tab strip tidy: when over the cap, auto-close the oldest
// finished tabs (their full output stays in the chat tool card). The
// pinned user tab is never pruned.
function terminalPrune() {
    if (terminals.size < TERM_MAX_TABS) return;
    for (const [id, t] of terminals) {
        if (terminals.size < TERM_MAX_TABS) break;
        if (t.done && id !== USER_TERM_ID) terminalClose(id);
    }
}

function terminalFlashTab(id) {
    const t = terminals.get(id);
    if (!t) return;
    t.tab.classList.remove('flash');
    void t.tab.offsetWidth; // restart the animation
    t.tab.classList.add('flash');
    setTimeout(() => t.tab.classList.remove('flash'), 900);
}

// Badge on the mobile "Terminal" header tab: number of running agent
// commands, shown only while the terminal panel itself is hidden.
function terminalUpdateBadge() {
    if (!terminalTabBadge) return;
    // Count running AGENT commands only — the user shell is always
    // "not done" but shouldn't keep the badge lit.
    const running = [...terminals.values()].filter((t) => !t.done && t.id !== USER_TERM_ID).length;
    const visible = terminalIsMobile()
        ? terminalPanel.classList.contains('mobile-full')
        : true;
    terminalTabBadge.hidden = !(running > 0 && !visible);
    terminalTabBadge.textContent = running > 0 ? String(running) : '';
    terminalTabBadge.classList.toggle('pulse', running > 0 && !visible);
}

// WS closed mid-command: the server will never finish these tabs, so
// mark every still-running one as interrupted so the UI isn't stuck.
// The user shell is also killed server-side on disconnect — show it
// as dead with a restart affordance (revived on reconnect's opened).
export function terminalInterruptAll() {
    for (const id of [...terminals.keys()]) {
        if (id === USER_TERM_ID) terminalUserExited(-1);
        else terminalExit(id, false);
    }
}

// ===== Overflow: '»' dropdown + wheel scrolling =====
// The tab strip clips when many agent terminals accumulate; the '»'
// menu keeps every terminal selectable (and closable) regardless of
// how many there are. Shown once more than the user tab exists.
function terminalMoreRender() {
    if (!terminalMoreBtn || !terminalMoreMenu) return;
    terminalMoreBtn.hidden = terminals.size < 2;
    if (terminals.size < 2) {
        terminalMoreMenu.hidden = true;
        return;
    }
    terminalMoreMenu.innerHTML = '';
    for (const [id, t] of terminals) {
        const row = document.createElement('div');
        row.className = 'term-more-row' + (id === terminalActiveId ? ' active' : '');
        const status = document.createElement('span');
        status.className = 'term-more-status'
            + (t.done ? (t.success ? ' ok' : ' err') : ' running');
        status.textContent = t.done ? (t.success ? '✓' : '✗') : '●';
        const title = document.createElement('span');
        title.className = 'term-more-title';
        title.textContent = t.titleEl.textContent;
        title.title = t.tab.title || t.titleEl.textContent;
        row.appendChild(status);
        row.appendChild(title);
        if (t.closable) {
            const close = document.createElement('span');
            close.className = 'term-more-close';
            close.textContent = '✕';
            close.title = 'Close terminal';
            close.addEventListener('click', (e) => {
                e.stopPropagation();
                terminalClose(id);
            });
            row.appendChild(close);
        }
        row.addEventListener('click', () => {
            terminalSelectAndFocus(id);
            terminalMoreMenu.hidden = true;
        });
        terminalMoreMenu.appendChild(row);
    }
}

function terminalMoreToggle() {
    if (!terminalMoreMenu) return;
    if (terminalMoreMenu.hidden) {
        terminalMoreRender();
        terminalMoreMenu.hidden = false;
    } else {
        terminalMoreMenu.hidden = true;
    }
}

// Hides the '»' overflow menu if it is open; returns whether it hid
// one (the caller decides whether to swallow the key event).
export function terminalHideMoreMenu() {
    if (terminalMoreMenu && !terminalMoreMenu.hidden) {
        terminalMoreMenu.hidden = true;
        return true;
    }
    return false;
}

// ===== Mobile: "Terminal" header tab toggles a full-screen overlay =====
function terminalOpenMobile() {
    terminalPanel.classList.add('mobile-full');
    // The overlay is positioned with top/bottom; clear any docked
    // height (e.g. restored at init) so it fills the viewport instead
    // of shrinking to a strip.
    terminalPanel.style.height = '';
    // Align the overlay's top edge with the bottom of the header
    // (its height varies with wrapping on narrow screens).
    const tabs = document.getElementById('top-tabs');
    terminalPanel.style.top = (tabs ? tabs.getBoundingClientRect().height : 41) + 'px';
    terminalTabBtn.classList.add('active');
    terminalSetExpanded(true);
    terminalUpdateBadge();
    terminalFitSoon();
}

function terminalCloseMobile() {
    terminalPanel.classList.remove('mobile-full');
    terminalPanel.style.top = '';
    terminalTabBtn.classList.remove('active');
    terminalUpdateBadge();
}

export function terminalToggleMobile() {
    if (terminalPanel.classList.contains('mobile-full')) {
        terminalCloseMobile();
        if (terminalExpanded) terminalSetExpanded(false);
    } else {
        terminalOpenMobile();
    }
}

// The ⌘`-style shortcut: mobile opens the full-screen overlay,
// desktop toggles the docked strip.
export function terminalTogglePanel() {
    if (terminalIsMobile()) terminalOpenMobile();
    else terminalSetExpanded(!terminalExpanded);
}

// Closes the mobile overlay if it is open (pane switches and tab
// clicks must never leave it covering the chat).
export function terminalDismissMobile() {
    if (terminalIsMobile() && terminalPanel.classList.contains('mobile-full')) {
        terminalCloseMobile();
    }
}

export function initTerminal(d) {
    deps = d;
    terminalPanel = document.getElementById('terminal-panel');
    terminalBody = document.getElementById('terminal-body');
    terminalTabsEl = document.getElementById('terminal-tabs');
    terminalChevron = document.getElementById('terminal-chevron');
    terminalResizeHandle = document.getElementById('terminal-resize-handle');
    terminalTabBtn = document.getElementById('terminal-tab');
    terminalTabBadge = document.getElementById('terminal-tab-badge');
    terminalHint = document.getElementById('terminal-hint');
    terminalMoreBtn = document.getElementById('terminal-more');
    terminalMoreMenu = document.getElementById('terminal-more-menu');

    // '»' dropdown: open/close + outside-click dismissal.
    if (terminalMoreBtn) {
        terminalMoreBtn.addEventListener('click', (e) => {
            e.stopPropagation();
            terminalMoreToggle();
        });
    }
    document.addEventListener('click', (e) => {
        if (terminalMoreMenu && !terminalMoreMenu.hidden
            && !terminalMoreMenu.contains(e.target)
            && e.target !== terminalMoreBtn) {
            terminalMoreMenu.hidden = true;
        }
    });

    // Scroll the tab strip horizontally with the wheel (the scrollbar is
    // hidden to avoid a stray track under the tabs).
    terminalTabsEl.addEventListener('wheel', (e) => {
        if (terminalTabsEl.scrollWidth <= terminalTabsEl.clientWidth) return;
        e.preventDefault();
        terminalTabsEl.scrollLeft += e.deltaY || e.deltaX;
    }, { passive: false });

    // Expand/collapse + drag resize.
    terminalChevron.addEventListener('click', () => {
        terminalSetExpanded(!terminalExpanded);
    });
    let termDrag = null;
    terminalResizeHandle.addEventListener('mousedown', (e) => {
        // The mobile overlay is positioned with top/bottom; dragging its
        // handle must not set a pixel height (it would override bottom).
        if (!terminalExpanded || terminalPanel.classList.contains('mobile-full')) return;
        termDrag = { startY: e.clientY, startH: terminalPanel.offsetHeight };
        terminalResizeHandle.classList.add('active');
        e.preventDefault();
    });
    document.addEventListener('mousemove', (e) => {
        if (!termDrag) return;
        const h = Math.min(
            Math.max(termDrag.startH + (termDrag.startY - e.clientY), TERM_MIN_HEIGHT),
            terminalMaxHeight()
        );
        terminalHeight = h;
        terminalPanel.style.height = h + 'px';
        terminalRefreshStripVar();
    });
    document.addEventListener('mouseup', () => {
        if (!termDrag) return;
        termDrag = null;
        terminalResizeHandle.classList.remove('active');
        terminalSaveState();
        terminalFitSoon();
    });

    try {
        terminalPanel.classList.toggle('expanded', terminalExpanded);
        if (terminalExpanded) {
            terminalHeight = Math.min(terminalHeight, terminalMaxHeight());
            terminalPanel.style.height = terminalHeight + 'px';
        }
        terminalChevron.textContent = terminalExpanded ? '▾' : '▴';
        if (terminalIsMobile()) terminalCloseMobile();
        // The user shell is the default terminal: pinned tab, selected now.
        terminalEnsureUserTab();
        terminalUpdateHint();
        terminalUpdateBadge();
        terminalRefreshStripVar();
        if (typeof ResizeObserver !== 'undefined') {
            // Keep --terminal-strip-h in sync on any strip resize
            // (expand/collapse, drag, pane switches hiding/showing it).
            new ResizeObserver(() => terminalRefreshStripVar()).observe(terminalPanel);
        }
        const mq = window.matchMedia('(max-width: 767px)');
        const onMQChange = () => {
            if (mq.matches) {
                terminalCloseMobile();
                if (terminalExpanded) terminalSetExpanded(false);
            } else {
                // Clears mobile-full, the inline top offset and the tab
                // highlight.
                terminalCloseMobile();
                // Coming back to a desktop viewport: restore the docked
                // pixel height that mobile-full had cleared.
                if (terminalExpanded) {
                    terminalHeight = Math.min(terminalHeight, terminalMaxHeight());
                    terminalPanel.style.height = terminalHeight + 'px';
                }
            }
            terminalRefreshStripVar();
            terminalFitSoon();
        };
        if (typeof mq.addEventListener === 'function') mq.addEventListener('change', onMQChange);
        else if (typeof mq.addListener === 'function') mq.addListener(onMQChange);
        window.addEventListener('resize', () => {
            // The panel height is only capped while dragging; re-clamp it on
            // viewport changes too (e.g. opening devtools docked at the
            // bottom) so the panel can never exceed the body.
            if (terminalExpanded && !terminalPanel.classList.contains('mobile-full')) {
                terminalHeight = Math.min(terminalHeight, terminalMaxHeight());
                terminalPanel.style.height = terminalHeight + 'px';
            }
            terminalRefreshStripVar();
            terminalFitSoon();
        });
    } catch (err) {
        // A terminal-init failure must never block the chat connection:
        // tear down any half-created terminal UI and keep going so
        // connect() below still runs.
        console.error('terminal init failed:', err);
        terminalLoadFailed = true;
        document.querySelectorAll('#terminal-tabs .term-tab, #terminal-body .term-host')
            .forEach((el) => el.remove());
        terminals.clear();
        userTermState = 'dead';
        terminalUpdateHint();
        terminalUpdateBadge();
        terminalMoreRender();
    }
}
