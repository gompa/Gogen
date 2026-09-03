// Command palette (Ctrl+K) for the GoGen web UI: the fuzzy-filtered
// command list, keyboard navigation and the overlay open/close state.
//
// app.js owns the command catalog (PALETTE_COMMANDS plus the
// slash-command prefill entries) and the global Ctrl+K / Escape
// shortcuts; it hands the catalog to initPalette and drives open/close
// through the exports. Keeping the catalog in app.js keeps the palette
// free of any session-state coupling (the entries' run() closures are
// app.js's).
import { openModal, closeModal } from '/editor.js';

const paletteOverlay = document.getElementById('command-palette-overlay');
const paletteInput = document.getElementById('command-palette-input');
const paletteList = document.getElementById('command-palette-list');

// The command catalog (array reference; app.js keeps filling it, e.g.
// with the slash-command prefill entries, before the palette is used).
let commands = [];
let paletteMatches = [];
let paletteIndex = 0;

export function initPalette(d) {
    commands = (d && d.commands) || [];
}

export function isPaletteOpen() {
    return paletteOverlay.classList.contains('open');
}

export function closePalette() {
    closeModal(paletteOverlay, { className: 'open' });
    paletteInput.value = '';
    paletteList.innerHTML = '';
}

export function openPalette() {
    openModal(paletteOverlay, { className: 'open', focusSelector: '#command-palette-input' });
    paletteInput.value = '';
    paletteIndex = 0;
    renderPalette();
}

function renderPalette() {
    const q = paletteInput.value.trim().toLowerCase();
    paletteMatches = commands.filter((c) => !q || c.label.toLowerCase().includes(q) || c.id.includes(q));
    if (q) {
        // Prefix matches rank above substring matches (stable sort,
        // so equal-rank entries keep array order).
        paletteMatches.sort((a, b) => {
            const aPre = a.label.toLowerCase().startsWith(q);
            const bPre = b.label.toLowerCase().startsWith(q);
            if (aPre !== bPre) return aPre ? -1 : 1;
            return 0;
        });
    }
    if (paletteIndex >= paletteMatches.length) paletteIndex = 0;
    if (paletteIndex < 0) paletteIndex = Math.max(0, paletteMatches.length - 1);
    paletteList.innerHTML = '';
    paletteMatches.forEach((cmd, i) => {
        const el = document.createElement('div');
        el.className = 'palette-item' + (i === paletteIndex ? ' active' : '');
        el.setAttribute('role', 'option');
        el.innerHTML = '<span class="palette-item-label"></span><span class="palette-item-hint"></span>';
        el.querySelector('.palette-item-label').textContent = cmd.label;
        el.querySelector('.palette-item-hint').textContent = cmd.hint || '';
        el.onmousedown = (e) => {
            e.preventDefault();
            closePalette();
            cmd.run();
        };
        paletteList.appendChild(el);
    });
}

paletteInput.addEventListener('input', () => {
    paletteIndex = 0;
    renderPalette();
});
paletteInput.addEventListener('keydown', (e) => {
    if (e.key === 'ArrowDown') {
        e.preventDefault();
        paletteIndex = (paletteIndex + 1) % Math.max(1, paletteMatches.length);
        renderPalette();
    } else if (e.key === 'ArrowUp') {
        e.preventDefault();
        paletteIndex = (paletteIndex - 1 + Math.max(1, paletteMatches.length)) % Math.max(1, paletteMatches.length);
        renderPalette();
    } else if (e.key === 'Enter') {
        e.preventDefault();
        const cmd = paletteMatches[paletteIndex];
        if (cmd) {
            closePalette();
            cmd.run();
        }
    } else if (e.key === 'Escape') {
        e.preventDefault();
        closePalette();
    }
});
paletteOverlay.addEventListener('mousedown', (e) => {
    if (e.target === paletteOverlay) closePalette();
});
