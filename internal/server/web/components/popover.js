// Generic anchored popover shell for the GoGen web UI.
//
// A popover is an element (CSS-hidden until it carries the `open` class)
// anchored to a trigger element. createPopover owns the dismissal and
// positioning behavior that every hand-rolled popover used to duplicate:
//   - outside-click close (clicks inside the popover or its anchor are
//     safe; triggers that stopPropagation never reach the handler),
//   - Escape close,
//   - for `fixed` popovers (position:fixed, JS-placed): placement below
//     the anchor, flipped above when there is no room below, clamped to
//     the viewport either way, re-anchored on scroll/resize and on the
//     popover's own size changes, and auto-close when the anchor scrolls
//     out of view.
// CSS-positioned popovers (position:absolute in the stylesheet) pass
// fixed: false and keep their stylesheet placement.
//
// cfg:
//   el        — the popover element (must toggle visibility on `.open`)
//   getAnchor — () => the anchor element (may differ between opens; null
//               while closed). Outside-click safe zone and, for fixed
//               popovers, the positioning reference.
//   fixed     — true: JS positioning (below the anchor, viewport-clamped)
//   onOpen    — optional callback after the popover opens
//   onClose   — optional callback after the popover closes
//
// Returns { el, open, close, toggle, isOpen }.
export function createPopover({ el, getAnchor, fixed = false, onOpen, onClose }) {
    let isOpen = false;

    function position() {
        const anchor = getAnchor();
        if (!anchor) return;
        const rect = anchor.getBoundingClientRect();
        // The anchor scrolled out of view: close instead of leaving an
        // orphan popover floating in place.
        if (rect.bottom < 0 || rect.top > window.innerHeight) {
            close();
            return;
        }
        // The size is CSS-driven, so measure the rendered box instead of
        // hardcoded constants that could drift from the stylesheet.
        const pw = el.offsetWidth;
        const ph = el.offsetHeight;
        let left = rect.left;
        if (left + pw > window.innerWidth - 8) left = Math.max(8, window.innerWidth - pw - 8);
        // Default: below the anchor. Flip above when there is no room
        // below (and the space above fits): a card near the viewport
        // bottom would otherwise push the popover off-screen.
        let top = rect.bottom + 4;
        if (top + ph > window.innerHeight - 8 && rect.top - 4 - ph >= 8) {
            top = rect.top - 4 - ph;
        }
        // Fallback for very small viewports (fits neither below nor
        // above): clamp into the viewport — the popover scrolls its own
        // content (CSS max-height) rather than overflowing.
        top = Math.min(top, Math.max(8, window.innerHeight - ph - 8));
        el.style.left = left + 'px';
        el.style.top = top + 'px';
    }

    function open() {
        if (isOpen) return;
        isOpen = true;
        // Show first, then position: fixed popovers size themselves via
        // CSS, so the clamp math measures the rendered width.
        el.classList.add('open');
        if (fixed) position();
        if (onOpen) onOpen();
    }

    function close() {
        if (!isOpen) return;
        isOpen = false;
        el.classList.remove('open');
        if (fixed) {
            el.style.left = '';
            el.style.top = '';
        }
        if (onClose) onClose();
    }

    // Click outside the popover (and its anchor) to close it. Toggle
    // buttons that stopPropagation never reach this handler.
    document.addEventListener('click', (e) => {
        if (!isOpen) return;
        if (el.contains(e.target)) return;
        const anchor = getAnchor();
        if (anchor && anchor.contains(e.target)) return;
        close();
    });

    document.addEventListener('keydown', (e) => {
        if (e.key === 'Escape' && isOpen) close();
    });

    if (fixed) {
        // Capture-phase scroll: inner containers scroll without bubbling
        // (the board columns), and window resize changes the CSS width —
        // both re-anchor the popover.
        document.addEventListener('scroll', () => {
            if (isOpen) position();
        }, true);
        window.addEventListener('resize', () => {
            if (isOpen) position();
        });
        // The popover's own height can change while open (e.g. the board
        // start popover's prompt editor expands); re-position so the
        // flip/clamp math stays correct.
        if (typeof ResizeObserver !== 'undefined') {
            new ResizeObserver(() => {
                if (isOpen) position();
            }).observe(el);
        }
    }

    return {
        el,
        open,
        close,
        toggle: () => (isOpen ? close() : open()),
        isOpen: () => isOpen,
    };
}
