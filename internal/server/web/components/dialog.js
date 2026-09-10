// Shared decision-dialog plumbing for the GoGen web UI.
//
// editor.js owns the low-level modal show/hide (openModal / closeModal:
// aria-hidden, the Tab focus trap, focus restore to the trigger).
// openDialog layers the per-dialog wiring every confirm/cancel overlay
// used to hand-roll — button clicks, Escape, backdrop clicks, and the
// add/removeEventListener bookkeeping — so a dialog only describes its
// buttons and callbacks. Before this module, that wiring was copied into
// showReplacePreview / showCloseTabModal (editor.js), showSessionDeleteModal
// (sessions.js) and the delete-approval dialog (delete-approval.js).
//
// Focus behavior (a11y): openModal moves focus into the dialog, traps Tab
// inside it and remembers the trigger; closeModal restores focus to the
// trigger on close. Dialog markup orders buttons so the FIRST focusable is
// the safe action (Cancel / Keep / Deny — see index.html), which is what
// openModal focuses on open.
//
// Consumers: editor.js (showReplacePreview / showCloseTabModal),
// sessions.js (showSessionDeleteModal), delete-approval.js (the
// delete-approval queue).
//
// NOTE: dialog.js and editor.js import each other (the modal show/hide
// primitives live in editor.js and two of the dialogs live there too).
// Both sides only call each other's functions from inside function bodies,
// so the ESM cycle resolves safely. The web test harness (scripts/
// web-harness.js) evals this module directly — editor.js is stubbed
// there — which is why the shared wiring lives in its own module.

import { openModal, closeModal } from '/editor.js';

// `opts.confirm` / `opts.cancel` accept a button element or the button's
// document-wide id (the dialogs' buttons are static index.html markup).
// Missing ids throw before openModal runs, mirroring the loud failure the
// hand-rolled copies had when a getElementById came back null.
function resolveButton(el) {
    const found = typeof el === 'string' ? document.getElementById(el) : el;
    if (el && !found) throw new Error(`openDialog: button not found: ${el}`);
    return found;
}

/**
 * Open `overlay` as a decision dialog and wire the standard dismissal
 * plumbing. Escape and (unless opts.backdrop === false) backdrop clicks
 * cancel; the confirm and cancel buttons route to their callbacks. Every
 * dismissal path tears the wiring down exactly once — listeners removed,
 * overlay hidden, focus restored to the trigger — before the callback
 * runs, so a callback can immediately present the same overlay again
 * (the delete-approval queue does) without stale handlers.
 *
 * Returns a close() function for programmatic dismissal: it runs the same
 * teardown but invokes no callback.
 *
 * opts:
 *   onConfirm() — the confirm button (opts.confirm) was activated.
 *   onCancel()  — Escape, a backdrop click, or the cancel button
 *                 (opts.cancel). Callers map this to the dialog's safe
 *                 action (cancel / keep / deny).
 *   confirm     — confirm button element or document-wide id.
 *   cancel      — cancel button element or document-wide id.
 *   backdrop    — set false to ignore backdrop clicks (default: a click
 *                 landing on the overlay itself — never on the dialog or
 *                 its children — cancels).
 *   focusSelector / className — passed through to openModal.
 */
export function openDialog(overlay, opts = {}) {
    if (!overlay) return () => {};
    const confirmBtn = resolveButton(opts.confirm);
    const cancelBtn = resolveButton(opts.cancel);
    let settled = false;

    const onKey = (e) => {
        if (e.key !== 'Escape') return;
        e.stopPropagation(); // keep the document handler from cancelling the agent turn
        finish(opts.onCancel);
    };
    const onBackdrop = (e) => {
        if (e.target !== overlay) return; // clicks inside the dialog bubble through
        finish(opts.onCancel);
    };
    const onConfirmClick = () => finish(opts.onConfirm);
    const onCancelClick = () => finish(opts.onCancel);

    function finish(callback) {
        if (settled) return;
        settled = true;
        overlay.removeEventListener('keydown', onKey);
        overlay.removeEventListener('click', onBackdrop);
        if (confirmBtn) confirmBtn.removeEventListener('click', onConfirmClick);
        if (cancelBtn) cancelBtn.removeEventListener('click', onCancelClick);
        closeModal(overlay, opts);
        if (typeof callback === 'function') callback();
    }

    openModal(overlay, opts);
    overlay.addEventListener('keydown', onKey);
    if (opts.backdrop !== false) overlay.addEventListener('click', onBackdrop);
    if (confirmBtn) confirmBtn.addEventListener('click', onConfirmClick);
    if (cancelBtn) cancelBtn.addEventListener('click', onCancelClick);
    return () => finish(null);
}
