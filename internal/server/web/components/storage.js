// Safe localStorage access for the GoGen web UI.
//
// Reading `window.localStorage` itself can throw a SecurityError when the
// browser blocks storage (third-party-cookie blocking, "block all cookies",
// hardened/privacy modes, some Android in-app WebViews), and setItem can
// throw QuotaExceededError in private modes. The UI is meant to be opened
// from a phone's QR scan — exactly the context where storage may be
// unavailable — and several preferences are read at MODULE TOP LEVEL
// (settings.js, editor.js). An unguarded access there throws during ESM
// evaluation, which aborts the whole import graph and leaves a blank page.
//
// These wrappers never throw: reads fall back to the caller's default and
// writes/removes become no-ops, so a storage-blocked browser still gets a
// working UI (the preference is simply session-only).
//
// Function declarations (not const) so the jsdom web-harness
// (scripts/web-harness.js), which evals each module as a classic script
// with imports stripped, sees them as globals — the components/ sharing
// convention (see tool-result.js).

/**
 * Read a persisted string preference.
 *
 * @param {string} key localStorage key
 * @param {string|null} [dflt] value returned when the key is absent or
 *   storage is unavailable
 * @returns {string|null} the stored string, or `dflt`
 */
export function storageGet(key, dflt = null) {
    try {
        const v = localStorage.getItem(key);
        return v === null ? dflt : v;
    } catch (_) {
        return dflt;
    }
}

/**
 * Persist a string preference. Coerces `value` to a string; a no-op when
 * storage is unavailable or full.
 *
 * @param {string} key localStorage key
 * @param {string|number} value value to store
 */
export function storageSet(key, value) {
    try {
        localStorage.setItem(key, String(value));
    } catch (_) {
        /* storage blocked or over quota — the preference stays session-only */
    }
}

/**
 * Remove a persisted preference. A no-op when storage is unavailable.
 *
 * @param {string} key localStorage key
 */
export function storageRemove(key) {
    try {
        localStorage.removeItem(key);
    } catch (_) {
        /* storage blocked */
    }
}
