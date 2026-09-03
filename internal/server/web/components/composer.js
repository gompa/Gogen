// Composer input helpers for the GoGen web UI: the slash-command
// suggest box and the image-attachment flow (file picker, paste,
// preview chips).
//
// The send path itself (sendMessage) stays in app.js — it is glue over
// the socket, panes and stream state. app.js reads the pending
// attachments through getPendingAttachments / clearAttachments and
// routes the composer's keydown through slashKeydown (true = consumed).
//
// Wiring: app.js calls initComposer(deps) once at startup.
//   deps.showToast(message, kind) — the toast stack
import { icon } from '/components/icons.js';

const inputArea = document.getElementById('message-input');
const slashSuggest = document.getElementById('slash-suggest');
const attachBtn = document.getElementById('attach-btn');
const imageUpload = document.getElementById('image-upload');
const attachmentPreview = document.getElementById('attachment-preview');

let deps = null;

export function initComposer(d) {
    deps = d;
}

// ── Slash-command suggest ──
// Keep in sync with agent.SlashCommands (Web: true).
const SLASH_COMMANDS = [
    { name: '/help', description: 'Show available commands' },
    { name: '/plan', description: 'Switch to plan (read-only) mode' },
    { name: '/act', description: 'Switch to act mode' },
    { name: '/mode', description: 'Show current mode' },
    { name: '/think', description: 'Set thinking/reasoning level (off/low/medium/high)' },
    { name: '/models', description: 'List or switch models' },
    { name: '/context', description: 'Context usage details' },
    { name: '/new', description: 'Start a new session' },
    { name: '/resume', description: 'List, restore, or delete sessions' },
];
let slashMatches = [];
let slashIndex = 0;

export function getSlashCommands() {
    return SLASH_COMMANDS;
}

export function hideSlashSuggest() {
    slashSuggest.classList.remove('open');
    // hideSlashSuggest runs on every keystroke via
    // updateSlashSuggest; skip the innerHTML write when the box is
    // already empty so the common (non-slash) path is a true no-op.
    if (slashSuggest.childElementCount > 0) slashSuggest.innerHTML = '';
    slashMatches = [];
    slashIndex = 0;
}

function matchSlashCommands(value) {
    // Don't suggest once the user has typed args. Without whitespace
    // the whole value IS the command token, so the split form would
    // just be value.toLowerCase() — drop it.
    if (!value.startsWith('/') || /\s/.test(value)) return [];
    return SLASH_COMMANDS.filter((c) => c.name.startsWith(value.toLowerCase()));
}

function renderSlashSuggest() {
    if (slashMatches.length === 0) {
        hideSlashSuggest();
        return;
    }
    if (slashIndex >= slashMatches.length) slashIndex = 0;
    if (slashIndex < 0) slashIndex = slashMatches.length - 1;
    slashSuggest.innerHTML = '';
    slashMatches.forEach((cmd, i) => {
        const el = document.createElement('div');
        el.className = 'slash-item' + (i === slashIndex ? ' active' : '');
        el.setAttribute('role', 'option');
        el.setAttribute('aria-selected', i === slashIndex ? 'true' : 'false');
        el.innerHTML = '<span class="slash-name"></span><span class="slash-desc"></span>';
        el.querySelector('.slash-name').textContent = cmd.name;
        el.querySelector('.slash-desc').textContent = cmd.description;
        el.onmousedown = (e) => {
            e.preventDefault();
            applySlashCompletion(cmd.name);
        };
        slashSuggest.appendChild(el);
    });
    slashSuggest.classList.add('open');
    const active = slashSuggest.querySelector('.slash-item.active');
    if (active) active.scrollIntoView({ block: 'nearest' });
}

export function updateSlashSuggest() {
    slashMatches = matchSlashCommands(inputArea.value);
    slashIndex = 0;
    renderSlashSuggest();
}

function applySlashCompletion(name) {
    inputArea.value = name + ' ';
    hideSlashSuggest();
    inputArea.focus();
}

export function slashSuggestOpen() {
    return slashSuggest.classList.contains('open') && slashMatches.length > 0;
}

// The composer keydown handling for the suggest box. Returns true when
// the event was consumed (app.js's keydown handler stops there); false
// falls through to send / turn-cancel.
export function slashKeydown(e) {
    if (!slashSuggestOpen()) return false;
    if (e.key === 'ArrowDown') {
        e.preventDefault();
        slashIndex = (slashIndex + 1) % slashMatches.length;
        renderSlashSuggest();
        return true;
    }
    if (e.key === 'ArrowUp') {
        e.preventDefault();
        slashIndex = (slashIndex - 1 + slashMatches.length) % slashMatches.length;
        renderSlashSuggest();
        return true;
    }
    if (e.key === 'Tab') {
        e.preventDefault();
        applySlashCompletion(slashMatches[slashIndex].name);
        return true;
    }
    if (e.key === 'Enter' && !e.shiftKey) {
        const selected = slashMatches[slashIndex].name;
        const token = inputArea.value.split(/\s/, 1)[0];
        if (token.toLowerCase() !== selected.toLowerCase()) {
            e.preventDefault();
            applySlashCompletion(selected);
            return true;
        }
        hideSlashSuggest();
        // fall through to send
        return false;
    }
    if (e.key === 'Escape') {
        e.preventDefault();
        hideSlashSuggest();
        return true;
    }
    return false;
}

// ── Image attachments (vision input) ──
// Mirrors the server's limits (internal/server/server.go
// validateImageInputs).
const MAX_ATTACHMENTS = 4;
const MAX_ATTACHMENT_BYTES = 5 * 1024 * 1024;
let pendingAttachments = []; // [{dataUrl, name}]

export function getPendingAttachments() {
    return pendingAttachments;
}

function addImageAttachment(file) {
    if (!file) return;
    if (!file.type || !file.type.startsWith('image/')) return;
    if (file.size > MAX_ATTACHMENT_BYTES) {
        deps.showToast(`Image "${file.name}" is larger than 5 MB`, 'error');
        return;
    }
    if (pendingAttachments.length >= MAX_ATTACHMENTS) {
        deps.showToast(`Max ${MAX_ATTACHMENTS} images per message`, 'error');
        return;
    }
    const reader = new FileReader();
    reader.onload = () => {
        const dataUrl = String(reader.result || '');
        if (!dataUrl.startsWith('data:image/')) {
            deps.showToast(`"${file.name}" is not a supported image`, 'error');
            return;
        }
        pendingAttachments.push({ dataUrl, name: file.name || 'image' });
        renderAttachmentPreview();
    };
    reader.readAsDataURL(file);
}

function removeAttachment(index) {
    pendingAttachments.splice(index, 1);
    renderAttachmentPreview();
}

export function clearAttachments() {
    pendingAttachments = [];
    renderAttachmentPreview();
}

function renderAttachmentPreview() {
    attachmentPreview.replaceChildren();
    if (pendingAttachments.length === 0) {
        attachmentPreview.hidden = true;
        return;
    }
    attachmentPreview.hidden = false;
    for (let i = 0; i < pendingAttachments.length; i++) {
        const att = pendingAttachments[i];
        const chip = document.createElement('span');
        chip.className = 'attachment-chip';
        chip.title = att.name;
        const img = document.createElement('img');
        img.src = att.dataUrl;
        img.alt = att.name;
        const remove = document.createElement('button');
        remove.type = 'button';
        remove.className = 'attachment-remove';
        remove.innerHTML = icon('x');
        remove.title = 'Remove image';
        remove.setAttribute('aria-label', 'Remove image');
        remove.addEventListener('click', () => removeAttachment(i));
        chip.appendChild(img);
        chip.appendChild(remove);
        attachmentPreview.appendChild(chip);
    }
}

attachBtn.addEventListener('click', () => {
    imageUpload.click();
});
imageUpload.addEventListener('change', () => {
    for (const file of imageUpload.files || []) {
        addImageAttachment(file);
    }
    imageUpload.value = '';
});
// Paste an image (or a file with an image MIME type) straight into
// the composer; text pastes behave exactly as before.
inputArea.addEventListener('paste', (e) => {
    const items = (e.clipboardData && e.clipboardData.items) || [];
    let handled = false;
    for (const item of items) {
        if (item.kind === 'file' && item.type && item.type.startsWith('image/')) {
            const f = item.getAsFile();
            if (f) {
                addImageAttachment(f);
                handled = true;
            }
        }
    }
    if (handled) e.preventDefault();
});
