// Composer input helpers for the GoGen web UI: the slash-command
// suggest box and the attachment flow (file picker, clipboard paste,
// drag-and-drop onto the input area, preview chips).
//
// The send path itself (sendMessage) stays in app.js — it is glue over
// the socket, panes and stream state. app.js reads the pending
// attachments through getPendingAttachments / clearAttachments, asks
// composeMessageContent to inline text-file attachments into the
// outgoing content, and routes the composer's keydown through
// slashKeydown (true = consumed).
//
// Wiring: app.js calls initComposer(deps) once at startup.
//   deps.showToast(message, kind) — the toast stack
import { icon } from '/components/icons.js';

const inputArea = document.getElementById('message-input');
const slashSuggest = document.getElementById('slash-suggest');
const attachBtn = document.getElementById('attach-btn');
const imageUpload = document.getElementById('image-upload');
const attachmentPreview = document.getElementById('attachment-preview');
// The whole composer row (textarea + buttons) is the drop target —
// dropping on the buttons or the attachment strip must work too.
const composerEl = document.getElementById('input-area');
const dropOverlay = document.getElementById('composer-drop-overlay');

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

// ── Attachments (vision input + dropped text context) ──
// Image limits mirror the server's (internal/server/server.go
// validateImageInputs). The attachment-count cap is shared across both
// kinds; images additionally ride the payload's images array (max 4
// server-side), so the client cap can never exceed it.
const MAX_ATTACHMENTS = 4;
const MAX_ATTACHMENT_BYTES = 5 * 1024 * 1024;
// Text files are capped tighter than images: their content is inlined
// verbatim into the message text (and so into the LLM context and the
// persisted transcript), where even a few hundred KB is a lot of tokens.
const MAX_TEXT_ATTACHMENT_BYTES = 256 * 1024;
let pendingAttachments = []; // images: [{dataUrl, name}] · text files: [{text, name}]

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
        deps.showToast(`Max ${MAX_ATTACHMENTS} attachments per message`, 'error');
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

// ── Text/code files (dropped only — the picker stays image-only) ──
// Attached as context: read as UTF-8 and inlined into the outgoing
// message content by composeMessageContent. The server's wire format
// only carries image attachments separately, so text rides inside the
// message body — which also means edit/resend and the persisted
// transcript reproduce exactly what the model saw.
//
// Files dropped from the OS expose no filesystem path (browsers hide
// it), and the editor pane opens files by workspace-relative path over
// its socket, so "open in the editor" is not offered for drops —
// context attachment is the one behavior that always works.

// Common text/code MIME prefixes (checked before the extension: an
// explicit text/* from the OS beats any guessing).
const TEXT_MIME_PREFIXES = [
    'text/',
    'application/json', 'application/xml', 'application/javascript',
    'application/x-yaml', 'application/yaml', 'application/toml',
    'application/x-sh', 'application/graphql',
];
// Extensions accepted when the OS reports no MIME type. Includes
// extension-less dotfiles and bare names ("Makefile", ".gitignore").
const TEXT_EXTENSIONS = new Set([
    'go', 'js', 'jsx', 'ts', 'tsx', 'mjs', 'cjs', 'py', 'rb', 'rs', 'java',
    'kt', 'kts', 'swift', 'c', 'h', 'cpp', 'hpp', 'cc', 'cxx', 'cs', 'php',
    'sh', 'bash', 'zsh', 'fish', 'ps1', 'sql', 'json', 'jsonc', 'yaml',
    'yml', 'toml', 'xml', 'html', 'htm', 'css', 'scss', 'sass', 'less',
    'md', 'markdown', 'txt', 'text', 'csv', 'tsv', 'ini', 'cfg', 'conf',
    'env', 'properties', 'proto', 'graphql', 'gql', 'vue', 'svelte', 'astro',
    'dart', 'scala', 'pl', 'pm', 'lua', 'r', 'jl', 'ex', 'exs', 'erl', 'hrl',
    'clj', 'cljs', 'lisp', 'el', 'vim', 'dockerfile', 'makefile', 'cmake',
    'gradle', 'tf', 'tfvars', 'hcl', 'gitignore', 'gitattributes', 'editorconfig',
    'npmrc', 'babelrc', 'eslintrc', 'prettierrc', 'patch', 'diff', 'lock',
    'log', 'rst', 'adoc',
]);

// Extension of `name` — for extension-less files the whole lowercase
// basename, so "Makefile" → "makefile" and ".gitignore" → "gitignore".
function fileNameExtension(name) {
    const base = String(name || '').toLowerCase();
    const dot = base.lastIndexOf('.');
    return dot === -1 ? base : base.slice(dot + 1);
}

function isTextFile(file) {
    const type = String(file.type || '');
    if (type) {
        for (const prefix of TEXT_MIME_PREFIXES) {
            if (type.startsWith(prefix)) return true;
        }
        return false;
    }
    return TEXT_EXTENSIONS.has(fileNameExtension(file.name));
}

function addTextAttachment(file) {
    if (!file) return;
    // Only the text cap applies here: it is far tighter than
    // MAX_ATTACHMENT_BYTES (the image limit), so a 5 MB check would be dead
    // code with a misleading message.
    if (file.size > MAX_TEXT_ATTACHMENT_BYTES) {
        deps.showToast(
            `"${file.name}" is larger than 256 KB — text files are inlined into the message`,
            'error');
        return;
    }
    if (pendingAttachments.length >= MAX_ATTACHMENTS) {
        deps.showToast(`Max ${MAX_ATTACHMENTS} attachments per message`, 'error');
        return;
    }
    const reader = new FileReader();
    reader.onload = () => {
        pendingAttachments.push({ name: file.name || 'file', text: String(reader.result || '') });
        renderAttachmentPreview();
    };
    reader.onerror = () => deps.showToast(`Could not read "${file.name}"`, 'error');
    reader.readAsText(file);
}

function removeAttachment(index) {
    pendingAttachments.splice(index, 1);
    renderAttachmentPreview();
}

export function clearAttachments() {
    pendingAttachments = [];
    renderAttachmentPreview();
}

// Outgoing message content: the typed text with every text-file
// attachment inlined as a fenced code block (images ride separately in
// the payload's images array — see sendMessage in app.js). Called once
// per send; empty when there is nothing to inline.
export function composeMessageContent(text) {
    let content = text;
    for (const att of pendingAttachments) {
        if (att.text === undefined) continue;
        if (content) content += '\n\n';
        content += fencedFileBlock(att);
    }
    return content;
}

// One attachment as "[Attached file: name]" + a fenced block. The fence
// grows past the longest backtick run inside the file so the block can
// never terminate early (markdown's own rule for embedding fences).
function fencedFileBlock(att) {
    const body = String(att.text || '');
    let run = 0;
    let longest = 0;
    for (const ch of body) {
        if (ch === '`') {
            run++;
            if (run > longest) longest = run;
        } else {
            run = 0;
        }
    }
    const fence = '`'.repeat(Math.max(3, longest + 1));
    const dot = String(att.name || '').lastIndexOf('.');
    const ext = dot === -1 ? '' : att.name.slice(dot + 1).toLowerCase();
    const lang = /^[a-z0-9]+$/.test(ext) ? ext : '';
    return '[Attached file: ' + att.name + ']\n' + fence + lang + '\n' + body + '\n' + fence;
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
        if (att.text !== undefined) {
            // Text/code file: icon + name instead of the image thumbnail.
            chip.classList.add('text');
            const fileIcon = document.createElement('span');
            fileIcon.className = 'attachment-file-icon';
            fileIcon.innerHTML = icon('code');
            const name = document.createElement('span');
            name.className = 'attachment-file-name';
            name.textContent = att.name;
            chip.appendChild(fileIcon);
            chip.appendChild(name);
        } else {
            const img = document.createElement('img');
            img.src = att.dataUrl;
            img.alt = att.name;
            chip.appendChild(img);
        }
        const remove = document.createElement('button');
        remove.type = 'button';
        remove.className = 'attachment-remove';
        remove.innerHTML = icon('x');
        remove.title = att.text !== undefined ? 'Remove file' : 'Remove image';
        remove.setAttribute('aria-label', remove.title);
        remove.addEventListener('click', () => removeAttachment(i));
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

// ── Drag-and-drop attachments ──
// Dropping files anywhere on the composer row (#input-area) attaches
// them: images take the exact picker/paste path (same limits), text and
// code files attach as context (see addTextAttachment). Only drags that
// actually carry files are claimed — dragging a text selection onto the
// textarea keeps the browser's native insert-on-drop.

// `types` includes the literal "Files" token whenever the drag carries
// file system items (OS file manager, attachments from other apps).
function dragHasFiles(e) {
    const types = e.dataTransfer && e.dataTransfer.types;
    if (!types) return false;
    for (const type of types) {
        if (type === 'Files') return true;
    }
    return false;
}

function addDroppedFiles(files) {
    for (const file of files) {
        if (!file) continue;
        if (file.type && file.type.startsWith('image/')) {
            addImageAttachment(file);
        } else if (isTextFile(file)) {
            addTextAttachment(file);
        } else {
            deps.showToast(
                `"${file.name}" was not attached — only images and text/code files are supported`,
                'error');
        }
    }
}

// Overlay visibility via an enter/leave depth counter: the events bubble
// from every child of #input-area, and dragenter on the new target fires
// before dragleave on the old one, so the depth never dips to 0 while
// the pointer moves between children.
let dragDepth = 0;

function resetDragState() {
    dragDepth = 0;
    dropOverlay.hidden = true;
}

composerEl.addEventListener('dragenter', (e) => {
    if (!dragHasFiles(e)) return;
    e.preventDefault();
    dragDepth++;
    dropOverlay.hidden = false;
});
composerEl.addEventListener('dragleave', () => {
    if (dragDepth > 0) dragDepth--;
    if (dragDepth === 0) dropOverlay.hidden = true;
});
// Accepting the drop (preventDefault on dragover) is also what makes the
// drop event fire at all; copy fits "attach a duplicate of this file".
composerEl.addEventListener('dragover', (e) => {
    if (!dragHasFiles(e)) return;
    e.preventDefault();
    e.dataTransfer.dropEffect = 'copy';
});
composerEl.addEventListener('drop', (e) => {
    if (!dragHasFiles(e)) return; // text drops fall through to the textarea
    e.preventDefault();
    resetDragState();
    addDroppedFiles(e.dataTransfer.files || []);
});

// Safety net: a file drop that misses the composer must not trigger the
// browser default — navigating this tab away to the raw file. The
// composer's and board's own handlers run first (target phase) and are
// unaffected; this only cancels truly unhandled file drops. Board card
// drags set only text/plain, so they never match dragHasFiles.
window.addEventListener('dragover', (e) => {
    if (dragHasFiles(e)) e.preventDefault();
});
window.addEventListener('drop', (e) => {
    if (dragHasFiles(e)) e.preventDefault();
});
