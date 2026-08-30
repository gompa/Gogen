// Inline SVG icon system.
//
// The sprite (<symbol> definitions) lives once in index.html; icon(name)
// returns an <svg><use href="#i-name"/></svg> string that references it.
// Symbols are stroke-based with no paint attributes of their own, so the
// .icon class supplies stroke/fill via currentColor and every icon follows
// the active theme and accent automatically. No font loading, no external
// requests — works offline like the rest of the vendored assets.

/**
 * Return the markup for a named icon from the global sprite.
 *
 * @param {string} name symbol name without the "i-" prefix (e.g. 'x', 'menu')
 * @param {string} [cls] extra class for the <svg> wrapper ("icon" by default)
 * @returns {string} HTML string for the icon
 */
export function icon(name, cls = 'icon') {
    return `<svg class="${cls}" aria-hidden="true"><use href="#i-${name}"></use></svg>`;
}
