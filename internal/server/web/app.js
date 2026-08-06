        import {
            connectEditorSocket,
            setupEditorUI,
            refreshExplorer,
            disposeChatEditors,
            mountDiffEditor,
            updateDiffEditor,
            updateDiffFallback,
            chatDiffWheelEdge,
            extractDiffValue,
            initMonaco,
            colorizeCodeBlocks,
            colorizeElement,
            languageFromPath,
            setToastHandler,
            focusFindInFiles,
            editorUndo,
            editorRedo,
            saveAll,
            saveActive,
            openFileAtLine,
            setMonacoTheme,
            applyEditorPrefs,
            openModal,
            closeModal,
        } from '/editor.js';
        import { marked } from '/vendor/marked.esm.js';
        import DOMPurify from '/vendor/dompurify.esm.js';

        marked.use({ gfm: true, breaks: true });

        const messagesDiv = document.getElementById('messages');
        const inputArea = document.getElementById('message-input');
        const sendBtn = document.getElementById('send-btn');
        const cancelBtn = document.getElementById('cancel-btn');
        const slashSuggest = document.getElementById('slash-suggest');
        const attachBtn = document.getElementById('attach-btn');
        const imageUpload = document.getElementById('image-upload');
        const attachmentPreview = document.getElementById('attachment-preview');
        const dirInput = document.getElementById('working-dir-input');
        const setDirBtn = document.getElementById('set-dir-btn');
        const workingDirDisplay = document.getElementById('working-dir-display');
        const workingDirPath = document.getElementById('working-dir-path');
        const workingDirConfig = document.getElementById('working-dir-config');
        const connIndicator = document.getElementById('conn-indicator');
        const sessionInfoDiv = document.getElementById('session-info');
        const sessionListDiv = document.getElementById('session-list');
        let currentMode = 'act';
        let isGlobalMode = false;
        const inputProgress = document.getElementById('input-progress');
        const globalModeBadge = document.getElementById('global-mode-badge');
        const deleteOverlay = document.getElementById('delete-approval-overlay');
        const deleteReason = document.getElementById('delete-approval-reason');
        const deletePaths = document.getElementById('delete-approval-paths');
        const deleteAllowBtn = document.getElementById('delete-allow-btn');
        const deleteDenyBtn = document.getElementById('delete-deny-btn');
        const paletteOverlay = document.getElementById('command-palette-overlay');
        const paletteInput = document.getElementById('command-palette-input');
        const paletteList = document.getElementById('command-palette-list');
        const toastHost = document.getElementById('toast-host');
        // Toolbar elements
        const tbModelBtn = document.getElementById('tb-model-btn');
        const tbModelPopover = document.getElementById('tb-model-popover');
        const tbModelList = document.getElementById('tb-model-list');
        const tbModelFilter = document.getElementById('tb-model-filter');
        const tbThinkingGrid = document.getElementById('tb-thinking-grid');
        const nearCompactBanner = document.getElementById('near-compact-banner');
        const ncbCompactBtn = document.getElementById('ncb-compact-btn');
        const ncbDismissBtn = document.getElementById('ncb-dismiss-btn');
        const tbModeBtn = document.getElementById('tb-mode-btn');
        const tbContextBadge = document.getElementById('tb-context-badge');

        // Pending delete approvals, queued so a second session's approval
        // request (e.g. a background pane) cannot orphan the first: the modal
        // is single-slot, and overwriting the pending id would leave the
        // first session's turn waiting forever on a channel that is never
        // resolved. Each entry carries its sessionId so the response routes
        // to the right session's runtime.
        let pendingDeleteApprovals = []; // {approvalId, sessionId, reason, paths}
        let messageRawStore = new WeakMap();
        // Last sessions payload from the server. The sidebar's single
        // session list is re-rendered from this cache whenever pane state
        // (active focus, busy/responding, label) changes, avoiding a
        // round-trip per state flip.
        let lastSessions = null;

        // Bounded toast stack: a burst of events (copy feedback, connection
        // flips) must not pile an unbounded column over the composer. When
        // the cap is hit the oldest toast is removed first.
        const MAX_TOASTS = 8;

        function showToast(message, kind = 'info') {
            if (!toastHost || !message) return;
            while (toastHost.childElementCount >= MAX_TOASTS) {
                toastHost.firstElementChild?.remove();
            }
            const el = document.createElement('div');
            el.className = `toast ${kind}`;
            el.textContent = message;
            el.addEventListener('click', () => el.remove());
            toastHost.appendChild(el);
            setTimeout(() => {
                el.remove();
            }, 3000);
        }
        setToastHandler(showToast);

        function setConnState(state) {
            if (!connIndicator) return;
            connIndicator.classList.remove('connected', 'disconnected', 'reconnecting');
            connIndicator.classList.add(state);
            const label = state === 'connected' ? 'Connected'
                : state === 'reconnecting' ? 'Reconnecting…'
                : 'Disconnected';
            connIndicator.title = label;
            connIndicator.setAttribute('aria-label', label);
        }

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

        function hideSlashSuggest() {
            slashSuggest.classList.remove('open');
            slashSuggest.innerHTML = '';
            slashMatches = [];
            slashIndex = 0;
        }

        function matchSlashCommands(value) {
            if (!value.startsWith('/')) return [];
            const token = value.split(/\s/, 1)[0].toLowerCase();
            // Don't suggest once the user has typed args.
            if (/\s/.test(value)) return [];
            return SLASH_COMMANDS.filter((c) => c.name.startsWith(token));
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

        function updateSlashSuggest() {
            slashMatches = matchSlashCommands(inputArea.value);
            slashIndex = 0;
            renderSlashSuggest();
        }

        function applySlashCompletion(name) {
            inputArea.value = name + ' ';
            hideSlashSuggest();
            inputArea.focus();
        }

        function slashSuggestOpen() {
            return slashSuggest.classList.contains('open') && slashMatches.length > 0;
        }

        // Main Chat | Editor tabs
        document.querySelectorAll('.main-tab').forEach((btn) => {
            btn.addEventListener('click', async () => {
                if (btn.dataset.pane === 'terminal') {
                    terminalToggleMobile();
                    return;
                }
                // Switching to a regular pane on mobile dismisses the
                // full-screen terminal overlay so it can't cover the chat.
                if (terminalIsMobile() && terminalPanel.classList.contains('mobile-full')) {
                    terminalCloseMobile();
                }
                document.querySelectorAll('.main-tab').forEach((b) => b.classList.remove('active'));
                document.querySelectorAll('.pane').forEach((p) => p.classList.remove('active'));
                btn.classList.add('active');
                const pane = document.getElementById(btn.dataset.pane + '-pane');
                if (pane) pane.classList.add('active');
                if (btn.dataset.pane === 'editor') {
                    await initMonaco();
                    await refreshExplorer();
                }
            });
        });

        setupEditorUI();

        function respondDeleteApproval(approved) {
            deleteOverlay.removeEventListener('keydown', deleteApprovalEsc);
            if (!pendingDeleteApprovals.length || !ws || ws.readyState !== WebSocket.OPEN) {
                pendingDeleteApprovals = [];
                inputArea.disabled = false;
                sendBtn.disabled = false;
                closeModal(deleteOverlay);
                return;
            }
            const current = pendingDeleteApprovals.shift();
            ws.send(JSON.stringify({
                type: 'delete_approval_response',
                approvalId: current.approvalId,
                approved: approved,
                sessionId: current.sessionId || undefined
            }));
            if (pendingDeleteApprovals.length) {
                // More approvals queued — show the next one.
                renderDeleteApproval(pendingDeleteApprovals[0]);
            } else {
                inputArea.disabled = false;
                sendBtn.disabled = false;
                closeModal(deleteOverlay);
            }
        }

        function deleteApprovalEsc(e) {
            if (e.key === 'Escape') {
                e.stopPropagation(); // keep the document handler from cancelling the agent turn
                respondDeleteApproval(false);
            }
        }

        deleteAllowBtn.onclick = () => respondDeleteApproval(true);
        deleteDenyBtn.onclick = () => respondDeleteApproval(false);

        function renderDeleteApproval(data) {
            deleteReason.textContent = data.reason ? `Requested by: ${data.reason}` : 'The agent wants to delete files.';
            deletePaths.textContent = (data.paths || []).map(p => `- ${p}`).join('\n');
        }

        function showDeleteApproval(data) {
            const first = pendingDeleteApprovals.length === 0;
            pendingDeleteApprovals.push({
                approvalId: data.approvalId,
                sessionId: data.sessionId || null,
                reason: data.reason,
                paths: data.paths || [],
            });
            if (!first) {
                // The modal already shows an earlier approval; this one is
                // queued and renders when the current one resolves.
                return;
            }
            renderDeleteApproval(pendingDeleteApprovals[0]);
            inputArea.disabled = true;
            sendBtn.disabled = true;
            openModal(deleteOverlay);
            deleteOverlay.addEventListener('keydown', deleteApprovalEsc);
        }

        // ── Toolbar: model picker popover ──
        let tbModelPopoverOpen = false;
        let modelFilterQuery = '';

        function toggleModelPopover() {
            tbModelPopoverOpen = !tbModelPopoverOpen;
            if (tbModelPopoverOpen) {
                // Start from a clean slate each time the popover opens.
                if (tbModelFilter) tbModelFilter.value = '';
                modelFilterQuery = '';
                renderToolbarModelList(availableModels, availableModels.find((m) => m.current)?.id || '');
            }
            tbModelPopover.classList.toggle('open', tbModelPopoverOpen);
        }

        function closeModelPopover() {
            tbModelPopoverOpen = false;
            tbModelPopover.classList.remove('open');
        }

        // Click outside popover to close
        document.addEventListener('click', (e) => {
            if (tbModelPopoverOpen && !tbModelBtn.contains(e.target) && !tbModelPopover.contains(e.target)) {
                closeModelPopover();
            }
        });

        tbModelFilter?.addEventListener('input', () => {
            modelFilterQuery = tbModelFilter.value.trim().toLowerCase();
            renderToolbarModelList(availableModels, availableModels.find((m) => m.current)?.id || '');
        });

        tbModelBtn.addEventListener('click', (e) => {
            e.stopPropagation();
            if (availableModels.length <= 1) {
                // Fetch models if we don't have a list yet
                modelsRequested = false;
                ensureModelsLoaded();
            }
            toggleModelPopover();
        });

        function renderToolbarModelList(models, current) {
            if (!tbModelList) return;
            tbModelList.innerHTML = '';
            if (!models || models.length === 0) {
                const empty = document.createElement('div');
                empty.className = 'tb-model-row';
                empty.textContent = 'No models loaded';
                empty.style.color = 'var(--fg-muted)';
                empty.style.fontSize = '0.82em';
                empty.style.cursor = 'default';
                tbModelList.appendChild(empty);
                return;
            }
            const q = modelFilterQuery;
            const filtered = q ? models.filter((m) => m.id.toLowerCase().includes(q)) : models;
            if (filtered.length === 0) {
                const empty = document.createElement('div');
                empty.className = 'tb-model-row';
                empty.textContent = `No models match "${tbModelFilter ? tbModelFilter.value : q}"`;
                empty.style.color = 'var(--fg-muted)';
                empty.style.fontSize = '0.82em';
                empty.style.cursor = 'default';
                tbModelList.appendChild(empty);
                return;
            }
            for (const m of filtered) {
                const active = m.id === current || m.current;
                const row = document.createElement('button');
                row.className = 'tb-model-row' + (active ? ' active' : '');
                row.type = 'button';
                const label = document.createElement('span');
                label.textContent = m.id;
                row.appendChild(label);
                if (m.contextLimit) {
                    const limit = document.createElement('span');
                    limit.className = 'tb-model-limit';
                    limit.textContent = formatTokenCount(m.contextLimit);
                    row.appendChild(limit);
                }
                if (active) {
                    const check = document.createElement('span');
                    check.className = 'tb-check';
                    check.textContent = '✓';
                    row.appendChild(check);
                }
                row.addEventListener('click', (e) => {
                    e.stopPropagation();
                    if (m.id === current) { closeModelPopover(); return; }
                    if (!ws || ws.readyState !== WebSocket.OPEN) return;
                    // set_model is per-session: the sessionId scopes it
                    // to the requesting pane's provider only.
                    ws.send(JSON.stringify({ type: 'set_model', model: m.id, sessionId: activePane().id }));
                    closeModelPopover();
                });
                tbModelList.appendChild(row);
            }
        }

        // ── Toolbar: thinking level chips ──
        const THINKING_LABELS = { off: 'Off', low: 'L', medium: 'M', high: 'H' };

        function renderThinkingChips(level) {
            if (!tbThinkingGrid) return;
            tbThinkingGrid.innerHTML = '';
            for (const [key, label] of Object.entries(THINKING_LABELS)) {
                const chip = document.createElement('button');
                chip.className = 'tb-thinking-chip' + (key === level ? ' active' : '');
                chip.type = 'button';
                chip.textContent = label;
                chip.title = key;
                chip.addEventListener('click', (e) => {
                    e.stopPropagation();
                    if (key === level) return;
                    if (!ws || ws.readyState !== WebSocket.OPEN) return;
                    ws.send(JSON.stringify({ type: 'set_thinking_level', thinkingLevel: key, sessionId: activePane().id }));
                });
                tbThinkingGrid.appendChild(chip);
            }
        }

        // ── Toolbar: mode selector popover ──
        const tbModePopover = document.getElementById('tb-mode-popover');
        const tbModePicker = document.getElementById('tb-mode-picker');
        let tbModePopoverOpen = false;

        function toggleModePopover() {
            tbModePopoverOpen = !tbModePopoverOpen;
            tbModePopover.classList.toggle('open', tbModePopoverOpen);
        }

        function closeModePopover() {
            tbModePopoverOpen = false;
            tbModePopover.classList.remove('open');
        }

        document.addEventListener('click', (e) => {
            if (tbModePopoverOpen && !tbModePicker.contains(e.target)) {
                closeModePopover();
            }
        });

        tbModeBtn.addEventListener('click', (e) => {
            e.stopPropagation();
            toggleModePopover();
        });

        document.querySelectorAll('#tb-mode-list .tb-model-row').forEach((row) => {
            row.addEventListener('click', () => {
                const mode = row.dataset.mode;
                if (mode === currentMode) { closeModePopover(); return; }
                if (!ws || ws.readyState !== WebSocket.OPEN) return;
                ws.send(JSON.stringify({ type: 'set_mode', mode, sessionId: activePane().id }));
                closeModePopover();
            });
        });

        function updateToolbarMode(mode) {
            if (!tbModeBtn) return;
            const label = mode === 'plan' ? 'Plan' : 'Act';
            tbModeBtn.innerHTML = `${label} <span class="tb-arrow">▾</span>`;
            // Highlight active option in popover
            document.querySelectorAll('#tb-mode-list .tb-model-row').forEach((r) => {
                r.classList.toggle('active', r.dataset.mode === mode);
            });
        }

        // ── Toolbar: context badge ──
        // The badge DOM is built once and mutated in place. Per-token context
        // estimate updates (up to ~60/s while streaming) must not recreate
        // nodes each time.
        let tbBadgeRing = null;
        let tbBadgeLabel = null;
        let tbBadgeTitle = null;
        let tbBadgeTone = null;
        function ensureToolbarBadgeNodes() {
            if (tbBadgeRing) return true;
            if (!tbContextBadge) return false;
            tbContextBadge.innerHTML = '';
            tbBadgeRing = document.createElement('span');
            tbBadgeRing.className = 'ctx-ring';
            tbBadgeLabel = document.createTextNode('');
            tbContextBadge.appendChild(tbBadgeRing);
            tbContextBadge.appendChild(tbBadgeLabel);
            return true;
        }
        function updateToolbarContext(data) {
            if (!tbContextBadge) return;
            if (!ensureToolbarBadgeNodes()) return;
            const used = (data && data.usedTokens) || 0;
            const limit = (data && data.contextLimit) || 0;
            if (used <= 0 || limit <= 0) {
                tbBadgeRing.style.setProperty('--ctx-pct', '0%');
                if (tbBadgeLabel.nodeValue !== ' —') tbBadgeLabel.nodeValue = ' —';
                const title = 'Context usage unknown';
                if (tbBadgeTitle !== title) { tbContextBadge.title = title; tbBadgeTitle = title; }
                const tone = '';
                if (tbBadgeTone !== tone) {
                    tbContextBadge.removeAttribute('data-tone');
                    tbBadgeTone = tone;
                }
                return;
            }
            const pct = Math.min(100, Math.round((used / limit) * 100));
            tbBadgeRing.style.setProperty('--ctx-pct', pct + '%');
            const label = ` ${pct}% ${formatTokenCount(used)}/${formatTokenCount(limit)}`;
            if (tbBadgeLabel.nodeValue !== label) tbBadgeLabel.nodeValue = label;
            const title = `Context: ${used.toLocaleString()} / ${limit.toLocaleString()} tokens`;
            if (tbBadgeTitle !== title) { tbContextBadge.title = title; tbBadgeTitle = title; }
            const tone = pct >= 90 ? 'danger' : pct >= 75 ? 'warning' : '';
            if (tbBadgeTone !== tone) {
                if (tone) tbContextBadge.setAttribute('data-tone', tone);
                else tbContextBadge.removeAttribute('data-tone');
                tbBadgeTone = tone;
            }
        }

        // ── Context tooltip on toolbar badge (reuses sidebar tooltip element) ──
        tbContextBadge.addEventListener('mouseenter', () => {
            const d = lastContextData || {};
            if (!d.contextLimit && !d.usedTokens) return;
            const lines = [];
            lines.push('── Context ──');
            if (d.usedTokens) lines.push(`Used: ${formatTokenCount(d.usedTokens)}`);
            if (d.contextLimit) lines.push(`Limit: ${formatTokenCount(d.contextLimit)}`);
            if (d.promptTokens) lines.push(`Prompt: ${formatTokenCount(d.promptTokens)}`);
            if (d.completionTokens) lines.push(`Completion: ${formatTokenCount(d.completionTokens)}`);
            if (d.cachedTokens) lines.push(`Cached: ${formatTokenCount(d.cachedTokens)}`);
            if (d.cachedTokens && d.promptTokens > 0) lines.push(`Cache hit: ${Math.round((d.cachedTokens / d.promptTokens) * 100)}% of prompt`);
            if (d.compactAt) lines.push(`Compact at: ${formatTokenCount(d.compactAt)}`);
            if (d.messageCount > 0) lines.push(`Messages: ${d.messageCount}`);
            if (d.usedSource) lines.push(`Source: ${d.usedSource}`);
            const prompt = d.totalPromptTokens || 0;
            const completion = d.totalCompletionTokens || 0;
            const cached = d.totalCachedTokens || 0;
            const turns = d.totalTurns || 0;
            if (turns > 0 || prompt > 0 || currentModelPricing) {
                lines.push('── Session ──');
                if (turns > 0 || prompt > 0) {
                    lines.push(`Total: ${fmtTokK(prompt + completion)} · ${turns} turns`);
                    lines.push(`Prompt: ${fmtTokK(prompt)}`);
                    lines.push(`Completion: ${fmtTokK(completion)}`);
                    lines.push(`Cached: ${fmtTokK(cached)}`);
                    if (cached > 0 && prompt > 0) lines.push(`Cache hit: ${Math.round((cached / prompt) * 100)}% of session prompt`);
                }
                if (currentModelPricing) {
                    const billablePrompt = Math.max(0, prompt - cached);
                    const cost = (billablePrompt / 1e6) * currentModelPricing.input
                               + (cached / 1e6) * currentModelPricing.cached
                               + (completion / 1e6) * currentModelPricing.output;
                    lines.push(`Pricing: in $${currentModelPricing.input}/1M · out $${currentModelPricing.output}/1M`);
                    if (turns > 0 || prompt > 0) {
                        lines.push(`Est. cost: ${fmtCost(cost)}`);
                    }
                }
            }
            contextTooltip.textContent = lines.join('\n');
            contextTooltip.style.display = 'block';
        });
        tbContextBadge.addEventListener('mouseleave', () => {
            contextTooltip.style.display = 'none';
        });

        // ── Command mode indicator ──
        const inputAreaWrap = document.getElementById('input-area');
        inputArea.addEventListener('input', () => {
            const val = inputArea.value;
            inputAreaWrap.classList.toggle('command-mode', val.startsWith('/') && val.length > 0);
        });

        function updateModeInfo(mode) {
            currentMode = (mode || 'act').toLowerCase();
            updateToolbarMode(currentMode);
        }

        function updateGlobalMode(isGlobal) {
            // The server omits globalMode when false (JSON omitempty), so
            // absence always means project mode.
            isGlobalMode = !!isGlobal;
            if (globalModeBadge) globalModeBadge.classList.toggle('visible', isGlobalMode);
            // The working directory can only be changed in global mode:
            // project mode shows a read-only path, global mode shows the
            // editable control (the server also rejects the change).
            if (workingDirDisplay) workingDirDisplay.style.display = isGlobalMode ? 'none' : '';
            if (workingDirConfig) workingDirConfig.style.display = isGlobalMode ? '' : 'none';
        }

        function formatTokenCount(n) {
            if (!n || n <= 0) return '—';
            if (n < 1000) return String(n);
            const whole = Math.floor(n / 1000);
            const frac = Math.floor((n % 1000) / 100);
            return frac === 0 ? `${whole}k` : `${whole}.${frac}k`;
        }

        let availableModels = [];
        let currentModelPricing = null; // { input, output, cached } or null

        function updateModelInfo(model) {
            // Update toolbar model button
            if (tbModelBtn) {
                const name = model || (availableModels.length > 0 ? availableModels.find(m => m.current)?.id : null) || '—';
                tbModelBtn.innerHTML = name + ' <span class="tb-arrow">▾</span>';
            }
        }

        function updateThinkingInfo(level) {
            // Update toolbar thinking chips
            renderThinkingChips(level);
        }

        function updateModelSelect(models, current) {
            availableModels = Array.isArray(models) ? models : [];
            const modelId = current || availableModels.find((m) => m.current)?.id || '';
            updateModelInfo(modelId);
            // Populate toolbar model list
            renderToolbarModelList(availableModels, modelId);
            // Extract pricing for the active model
            const active = modelId
                ? availableModels.find((m) => m.id === modelId)
                : availableModels.find((m) => m.current);
            if (active && active.inputPricePer1M) {
                currentModelPricing = {
                    input: active.inputPricePer1M,
                    output: active.outputPricePer1M || 0,
                    cached: active.cachedPricePer1M || 0,
                };
            }
            if (!active || !active.inputPricePer1M) { currentModelPricing = null; }
        }

        // Fetch the model catalog lazily so it never blocks initial connect.
        let modelsRequested = false;
        function ensureModelsLoaded() {
            if (modelsRequested) return;
            if (!ws || ws.readyState !== WebSocket.OPEN) return;
            modelsRequested = true;
            ws.send(JSON.stringify({ type: 'list_models' }));
        }

        // ── Near-compact banner ──
        // The server flags warnNearCompact once usage reaches 75% of the
        // window — before the auto-compaction trigger (nearCompact) — so the
        // user gets lead time to compact manually. Dismissed state resets on
        // session change.
        let nearCompactDismissed = false;

        function updateNearCompactBanner(nearCompact) {
            if (!nearCompactBanner) return;
            nearCompactBanner.hidden = nearCompactDismissed || !nearCompact;
        }

        // True while a compact command is in flight (server-side summarization
        // can take a while); drives the persistent "Compacting context…"
        // indicator and gates duplicate clicks. Cleared when the compact
        // response arrives or on disconnect.
        let compacting = false;

        function startCompact() {
            if (!ws || ws.readyState !== WebSocket.OPEN) {
                showToast('Not connected', 'error');
                return;
            }
            if (compacting) return;
            compacting = true;
            ws.send(JSON.stringify({ type: 'compact', sessionId: activePane().id }));
            // Persistent progress, not a 3s toast: it stays until the server
            // reports the compact result (see the 'response' handler).
            setInputProgress('compacting', 'Compacting context…');
        }

        ncbCompactBtn?.addEventListener('click', () => {
            startCompact();
        });

        ncbDismissBtn?.addEventListener('click', () => {
            nearCompactDismissed = true;
            nearCompactBanner.hidden = true;
        });

        function updateContextInfo(data) {
            lastContextData = data || lastContextData;
            // warnNearCompact drives the banner (early warning); the server
            // always sends it explicitly, false included, so a post-compaction
            // message reliably hides a banner that was previously shown.
            if (typeof data.warnNearCompact === 'boolean') updateNearCompactBanner(data.warnNearCompact);
            const used = data.usedTokens || 0;
            const limit = data.contextLimit || 0;
            if (data.usedSource !== 'estimated') {
                contextBaseUsed = used;
                contextLimit = limit;
                contextEstAdded = 0;
            }

            if (used <= 0 && limit <= 0) {
                updateToolbarContext(null);
                return;
            }
            // Update toolbar context badge
            updateToolbarContext(data);
        }

        function applyServerConfig(data) {
            if (data.workingDir) {
                dirInput.value = data.workingDir;
                if (workingDirPath) workingDirPath.textContent = data.workingDir;
            }
            updateModelInfo(data.model);
            // Keep the popover model list in sync with the active model after a switch
            if (data.model && availableModels.length > 0) {
                for (const m of availableModels) {
                    m.current = (m.id === data.model);
                }
                renderToolbarModelList(availableModels, data.model);
            }
            updateThinkingInfo(data.thinkingLevel);
            updateModeInfo(data.mode);
            updateGlobalMode(data.globalMode);
            // Sync pricing from server-provided fields (always present on config from cached lookups)
            if (data.inputPricePer1M) {
                currentModelPricing = {
                    input: data.inputPricePer1M,
                    output: data.outputPricePer1M || 0,
                    cached: data.cachedPricePer1M || 0,
                };
            } else if (data.model && availableModels.length > 0) {
                // Fallback: look up from locally cached model list
                const active = availableModels.find((m) => m.id === data.model);
                if (active && active.inputPricePer1M) {
                    currentModelPricing = {
                        input: active.inputPricePer1M,
                        output: active.outputPricePer1M || 0,
                        cached: active.cachedPricePer1M || 0,
                    };
                }
            }
            updateContextInfo(data);

            if (data.sessionId) {
                sessionInfoDiv.textContent = data.sessionId;
                updateCurrentSessionLabel(data.sessionLabel);
            }
            // Refresh explorer when working dir changes
            if (document.getElementById('editor-pane').classList.contains('active')) {
                refreshExplorer().catch(() => {});
            }
        }

        function clearChat() {
            disposeChatEditors();
            messagesDiv.innerHTML = '';
            enableFollow();
            msgIdxCounter = 0;

            streamRafPending = false;
            streamLastRender = 0;
            endStream();
            currentThinkingDiv = null;
            currentThinkingSpan = null;
            currentThinkingRaw = '';
            thinkingRafPending = false;
            streamingToolCards = {};
            pendingToolCards = {};
            historyToolCallArgs = {};
            toolsStartedThisTurn = false;
            // noMirror: clearChat is also used mid-pane-switch (clear → load),
            // where the new pane's turnActive must survive the reset.
            setTurnActive(false, { silent: true, noMirror: true });
            setInputProgress(null);
            scrollBottomBtn.classList.remove('visible');
            maybeShowEmptyState();
        }

        // ── Empty chat state (fresh session) ──
        // Shown only while the transcript is truly empty; any message render
        // (appendMessageAtTime / startStream / showThinking) removes it.
        const EMPTY_STATE_STARTERS = [
            'Explain this codebase',
            'Run the tests and fix any failures',
            'Plan a new feature',
        ];

        function sendStarterPrompt(text) {
            if (!ws || ws.readyState !== WebSocket.OPEN) {
                showToast('Not connected', 'error');
                return;
            }
            inputArea.value = text;
            sendMessage();
        }

        function buildEmptyState() {
            const wrap = document.createElement('div');
            wrap.className = 'empty-state';
            const emoji = document.createElement('div');
            emoji.className = 'empty-state-emoji';
            emoji.textContent = '🤖';
            const title = document.createElement('div');
            title.className = 'empty-state-title';
            title.textContent = 'GoGen is ready';
            const sub = document.createElement('div');
            sub.className = 'empty-state-sub';
            sub.textContent = 'Ask me to explore, fix, or build something in this repository.';
            const prompts = document.createElement('div');
            prompts.className = 'empty-state-prompts';
            for (const p of EMPTY_STATE_STARTERS) {
                const btn = document.createElement('button');
                btn.type = 'button';
                btn.className = 'empty-state-btn';
                btn.textContent = p;
                btn.addEventListener('click', () => sendStarterPrompt(p));
                prompts.appendChild(btn);
            }
            const hint = document.createElement('div');
            hint.className = 'empty-state-hint';
            hint.textContent = 'Type / for commands · Ctrl+K for the command palette';
            wrap.append(emoji, title, sub, prompts, hint);
            return wrap;
        }

        function maybeShowEmptyState() {
            if (!messagesDiv) return;
            if (messagesDiv.querySelector('.message, .thought-card, .tool-card')) return;
            if (messagesDiv.querySelector('.empty-state')) return;
            messagesDiv.appendChild(buildEmptyState());
        }

        function removeEmptyState() {
            const el = messagesDiv.querySelector('.empty-state');
            if (el) el.remove();
        }

        let pendingSessionResponse = false;
        // After session_new / session_fork for edit-resend, send this as a
        // normal message once history has been replayed.
        let pendingResendContent = null;
        // True only while a beginResend is waiting for its history snapshot.
        // Prevents unrelated history (reconnect/resume) from flushing a stale send.
        let resendAwaitingHistory = false;
        // Count of cancel→turn_end pairs to ignore so a cancelled turn's turn_end
        // cannot clobber an in-flight resend/send (pendingSessionResponse / turnActive).
        let suppressTurnEnds = 0;
        let ws;
        let reconnectTimer;
        let wasDisconnected = false;
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
        function enableFollow() {
            stickToBottom = true;
            messagesDiv.classList.add('no-anchor');
        }
        let currentStreamDiv = null;
        let currentStreamRaw = '';
        const STREAM_RENDER_INTERVAL = 32; // ms between renders (~2 frames at 60fps)
        let streamRafPending = false;
        let streamLastRender = 0;
        let currentThinkingDiv = null;
        let currentThinkingSpan = null;
        let currentThinkingRaw = '';
        let thinkingRafPending = false;
        let thinkingLastRender = 0;
        let pendingToolCards = {};
        let streamingToolCards = {};
        let toolCallCounter = 0;
        let turnActive = false;
        let toolsStartedThisTurn = false;
        let contextBaseUsed = 0;
        let contextLimit = 0;
        let contextEstAdded = 0;
        let lastContextData = null;
        // Coalesces per-token context badge updates into one rAF pass.
        let ctxEstimateRafPending = false;
        // Map toolCallId → args for history replay of patch_file diffs
        let historyToolCallArgs = {};
        // True while replayHistory() rebuilds the pane; suppresses per-message
        // smartScroll so the rebuild doesn't force O(n) layout passes.
        let replayInProgress = false;
        // Tool-result colorize requests made while a history replay is in
        // flight are deferred until the replay finishes and first paint lands,
        // so Monaco tokenization doesn't fight the transcript rebuild for
        // main-thread time. Same colorize output, just later.
        let deferredResultColorize = [];
        // Monotonic counter for message positions (used for forking)
        let msgIdxCounter = 0;

        let _prevTurnActive = false;
        function setTurnActive(active, opts) {
            const silent = !!(opts && opts.silent);
            const noMirror = !!(opts && opts.noMirror);
            const wasActive = _prevTurnActive;
            turnActive = !!active;
            if (cancelBtn) cancelBtn.disabled = !turnActive;
            if (sendBtn) sendBtn.disabled = false; // send cancels+restarts; keep enabled
            // Toggle class so Send/Cancel swap places
            document.getElementById('input-area').classList.toggle('turn-active', turnActive);
            if (!active) {
                toolsStartedThisTurn = false;
            }
            // Keep the active pane object's mirror current (badges,
            // afterHistory's "resuming…" restore). clearChat passes noMirror
            // so its reset does not clobber the pane's state mid-switch.
            if (!noMirror) {
                const p = activePane();
                if (p) p.turnActive = turnActive;
            }
            // Live-refresh the sidebar session row when the active pane's
            // turn state flips, so the amber responding indicator appears the
            // moment a turn starts (previously the row only re-rendered on a
            // pane switch, a turn end, or a background-pane message — the
            // active pane's row went stale until the user switched sessions).
            if (!noMirror && wasActive !== turnActive) {
                refreshSidebarSessions();
            }
            // Turn-end notification on any active→inactive transition, except
            // user-initiated stops that immediately start a new turn
            // (send-while-busy / resend are tracked via suppressTurnEnds) and
            // silent state restores (pane focus, session_state replay).
            _prevTurnActive = turnActive;
            if (wasActive && !turnActive && suppressTurnEnds === 0 && !silent) {
                sendNotification('GoGen', 'Agent finished responding.', 'gogen-turn-end');
            }
        }

        function estimateTokenCount(text) {
            if (!text) return 0;
            // Rough char/4 estimate matching TUI streaming estimate spirit.
            return Math.max(1, Math.ceil(String(text).length / 4));
        }

        function bumpContextEstimate(delta) {
            if (!delta || (contextBaseUsed <= 0 && contextLimit <= 0)) return;
            contextEstAdded += estimateTokenCount(delta);
            // Batch the badge update to one rAF pass instead of a DOM write
            // per token. The server sends an authoritative context message at
            // turn end, which supersedes any pending estimate.
            if (ctxEstimateRafPending) return;
            ctxEstimateRafPending = true;
            requestAnimationFrame(() => {
                ctxEstimateRafPending = false;
                if (contextEstAdded === 0) return;
                const used = contextBaseUsed + contextEstAdded;
                const data = Object.assign({}, lastContextData || {}, {
                    usedTokens: used,
                    contextLimit: contextLimit || (lastContextData && lastContextData.contextLimit) || 0,
                    usedSource: 'estimated',
                    usedPercent: contextLimit > 0 ? used / contextLimit : 0,
                    toolTruncated: false,
                });
                updateContextInfo(data);
            });
        }

        // ── Multi-pane sessions ──
        // The UI renders one transcript at a time — the ACTIVE pane. Every
        // open pane keeps its control state (turnActive, pending flags,
        // context data) in `panes`; the module-level state variables above
        // are mirrors of the active pane. Switching panes saves the mirrors,
        // swaps which pane is active, then re-attaches so the server resends
        // session_state/history/config/context for the focused session — the
        // transcript is re-derived from the server (the accepted §4 caveat:
        // a mid-turn pane shows the completed history, then live events).
        const panes = new Map(); // paneKey -> pane
        let nextPaneKey = 0;
        let activePaneKey = 0;

        function makePane() {
            const pane = {
                key: nextPaneKey++,
                id: null, // session id (null until known)
                label: '',
                turnActive: false,
                // Latched when this pane is attached to a session whose turn
                // is RUNNING (mid-turn focus / reconnect): the attach history
                // snapshot lacks the in-flight reply, so on turn_end we
                // re-attach once to converge the transcript.
                needsFreshHistory: false,
                // Session id whose turn_end must be ignored: set when the
                // user resumes another session while this pane's session has
                // a running turn. The old session keeps running headless
                // (resume does not cancel it), so its turn_end must not clear
                // the pending flag or block the resumed session's
                // convergence. Cleared when the reply re-keys the pane.
                ignoreTurnEndsFor: null,
                suppressTurnEnds: 0,
                pendingSessionResponse: false,
                pendingResendContent: null,
                resendAwaitingHistory: false,
                compacting: false,
                lastContextData: null,
                contextBaseUsed: 0,
                contextLimit: 0,
                contextEstAdded: 0,
                mode: 'act',
                thinkingLevel: 'off',
                model: '',
                _notifiedBusy: false,
            };
            panes.set(pane.key, pane);
            // The new pane becomes the ACTIVE pane at every call site; put
            // it at the front of the open-pane list so the sidebar shows
            // the newest session at the top.
            movePaneToFront(pane);
            refreshSidebarSessions();
            return pane;
        }

        // Move a pane to the FRONT of the panes map so the sidebar renders
        // open panes most-recently-used first. JS Maps re-insert a deleted
        // key at the END, so a front-move rebuilds the map with the target
        // pane first and the rest in existing order.
        function movePaneToFront(pane) {
            if (!pane || panes.keys().next().value === pane.key) return;
            const reordered = new Map();
            reordered.set(pane.key, pane);
            for (const [k, p] of panes) {
                if (k !== pane.key) reordered.set(k, p);
            }
            panes.clear();
            for (const [k, p] of reordered) panes.set(k, p);
        }

        function activePane() {
            return panes.get(activePaneKey);
        }

        function findPaneBySession(id) {
            if (!id) return null;
            for (const pane of panes.values()) {
                if (pane.id === id) return pane;
            }
            return null;
        }

        // Route a session-scoped server message to the pane it belongs to.
        // Returns null when the message is for a session the client does not
        // have open (the connection is only attached to open panes, so this
        // is rare — e.g. the server's default-session handshake after a
        // reconnect where the default session is not an open pane).
        function paneForMessage(data) {
            const sid = data.sessionId;
            if (sid) {
                const act = activePane();
                const activeInFlight = !!(act && pendingSessionResponse);
                // A session command's reply (response with a sessionAction,
                // e.g. clear_chat for /new, /resume, /fork) carries the NEW
                // session id before the pane has adopted it — the response
                // handler re-keys the pane so the follow-up clear_chat/
                // history/config route correctly.
                // Route it to the pane that initiated the change: normally
                // the active pane (its mirror lives in the module-level
                // pendingSessionResponse), but the user may have switched
                // panes while the reply was in flight — the initiator's flag
                // lives on the pane object, which the module mirror no longer
                // reflects after a switch. (Two simultaneous in-flight
                // changes across panes can still interleave ambiguously —
                // each change is one round-trip, so this is a sub-second
                // race.)
                // The initiator is checked BEFORE a pane that already holds
                // the reply's session id: the latter is a different pane
                // (e.g. "resume latest" resolving to a session open
                // elsewhere) and must not steal the reply from the pane that
                // initiated the change, or that pane's in-flight flag would
                // stay stuck forever.
                if (data.type === 'response' && data.sessionAction) {
                    if (activeInFlight) return act;
                    for (const pane of panes.values()) {
                        if (pane !== act && pane.pendingSessionResponse) return pane;
                    }
                    return act;
                }
                const p = findPaneBySession(sid);
                if (p) return p;
                // The server sends history before config in a session change,
                // so the pane's id is stale until config adopts the new one.
                // While a session change is in flight, unknown ids belong to
                // the pane with the pending change (the follow-ups also
                // include the session_state resume now sends before its
                // reply).
                if (activeInFlight) return act;
                for (const pane of panes.values()) {
                    if (pane !== act && pane.pendingSessionResponse) return pane;
                }
                if (act && act.id === null) return act;
                return null;
            }
            return activePane();
        }

        function saveActivePaneState() {
            const pane = activePane();
            if (!pane) return;
            pane.turnActive = turnActive;
            pane.suppressTurnEnds = suppressTurnEnds;
            pane.pendingSessionResponse = pendingSessionResponse;
            pane.pendingResendContent = pendingResendContent;
            pane.resendAwaitingHistory = resendAwaitingHistory;
            pane.compacting = compacting;
            pane.lastContextData = lastContextData;
            pane.contextBaseUsed = contextBaseUsed;
            pane.contextLimit = contextLimit;
            pane.contextEstAdded = contextEstAdded;
        }

        function loadActivePaneState() {
            const pane = activePane();
            if (!pane) return;
            turnActive = !!pane.turnActive;
            suppressTurnEnds = pane.suppressTurnEnds;
            pendingSessionResponse = pane.pendingSessionResponse;
            pendingResendContent = pane.pendingResendContent;
            resendAwaitingHistory = pane.resendAwaitingHistory;
            compacting = pane.compacting;
            lastContextData = pane.lastContextData;
            contextBaseUsed = pane.contextBaseUsed;
            contextLimit = pane.contextLimit;
            contextEstAdded = pane.contextEstAdded;
            _prevTurnActive = turnActive;
            if (cancelBtn) cancelBtn.disabled = !turnActive;
            document.getElementById('input-area').classList.toggle('turn-active', turnActive);
            if (!turnActive) toolsStartedThisTurn = false;
        }

        // Apply a session-scoped message to a background pane without touching
        // the DOM: turn/badge state, context data, and config mirrors. The
        // transcript is re-derived from the server when the pane is focused.
        function handleBackgroundMessage(pane, data) {
            const beforeTurn = pane.turnActive;
            const beforeLabel = pane.label;
            const beforeId = pane.id;
            switch (data.type) {
                case 'thinking':
                case 'waiting':
                case 'thinking_token':
                case 'stream':
                case 'stream_end':
                case 'tool_call_start':
                case 'tool_call_delta':
                case 'tool_call':
                case 'tool_execute':
                case 'tool_result':
                case 'term_opened':
                case 'term_output':
                case 'term_exit':
                    pane.turnActive = true;
                    break;
                case 'cancelled':
                    pane.turnActive = false;
                    // No decrement here: the active pane's cancelled handler
                    // also ignores the message (it returns early), and the
                    // paired turn_end below is the single decrement point for
                    // a cancel cycle. Decrementing on BOTH left the pane's
                    // counter at -1 when the pane went background between the
                    // cancel and the terminal messages, disabling suppression
                    // for the pane's next cancel (send-while-busy / resend).
                    break;
                case 'turn_end':
                    pane.turnActive = false;
                    if (pane.suppressTurnEnds > 0) pane.suppressTurnEnds--;
                    break;
                case 'session_state':
                    pane.turnActive = !!data.turnActive;
                    // Keep the convergence latch in sync with the active
                    // pane's handler: an idle attach means the snapshot is
                    // complete, a busy attach needs the turn-end refetch.
                    pane.needsFreshHistory = !!pane.turnActive;
                    break;
                case 'context':
                    pane.lastContextData = data || pane.lastContextData;
                    if (data.sessionLabel) pane.label = data.sessionLabel;
                    if (data.usedSource !== 'estimated') {
                        pane.contextBaseUsed = data.usedTokens || 0;
                        pane.contextLimit = data.contextLimit || 0;
                        pane.contextEstAdded = 0;
                    }
                    break;
                case 'config':
                    pane.mode = data.mode || pane.mode;
                    pane.thinkingLevel = data.thinkingLevel || pane.thinkingLevel;
                    pane.model = data.model || pane.model;
                    if (data.sessionLabel) pane.label = data.sessionLabel;
                    // A resend whose reply sequence (response → clear_chat →
                    // history → config) completed while this pane was in the
                    // background: the active pane's config handler never ran,
                    // so the pending message was never flushed. The response
                    // case above already adopted the new session id, so send
                    // the message on the wire now — the transcript re-derives
                    // on focus and the server's history snapshot then contains
                    // both the message and its reply. No DOM work here: a
                    // background pane's transcript is rebuilt on focus.
                    if (pane.resendAwaitingHistory) {
                        const text = pane.pendingResendContent;
                        pane.resendAwaitingHistory = false;
                        pane.pendingResendContent = null;
                        if (text && ws && ws.readyState === WebSocket.OPEN) {
                            ws.send(JSON.stringify({ type: 'message', content: text, sessionId: pane.id }));
                        }
                    }
                    break;
                case 'clear_chat':
                    // Background panes have no DOM; the re-attach on focus
                    // rebuilds the transcript. Just clear stale flags.
                    pane.pendingSessionResponse = false;
                    break;
                case 'response':
                    pane.pendingSessionResponse = false;
                    pane.ignoreTurnEndsFor = null;
                    // An error reply to the pane's own session change (e.g. a
                    // fork/new from a resend that failed) must cancel a
                    // pending resend like the active pane's Error branch does;
                    // otherwise the config flush below would send the pending
                    // message to the old session anyway.
                    if (String(data.content || '').startsWith('Error:')) {
                        pane.pendingResendContent = null;
                        pane.resendAwaitingHistory = false;
                    }
                    // A session command's reply (typed /new, /resume, /fork)
                    // with a sessionAction carries the NEW session id. When
                    // this pane initiated the change but is now a background
                    // pane (the user switched away while the reply was in
                    // flight), still adopt the id and release the old
                    // session's attachment — the transcript re-derives on
                    // focus. Refuse to adopt an id another pane already
                    // holds, and never detach a session another pane still
                    // has open (that would orphan the other pane's event
                    // stream).
                    if (data.sessionAction && data.sessionId && pane.id !== data.sessionId) {
                        const other = findPaneBySession(data.sessionId);
                        if (!other || other === pane) {
                            const oldId = pane.id;
                            pane.id = data.sessionId;
                            if (oldId && !findPaneBySession(oldId) && ws && ws.readyState === WebSocket.OPEN) {
                                ws.send(JSON.stringify({ type: 'session_detach', sessionId: oldId }));
                            }
                        }
                    }
                    break;
                case 'history':
                    // Ignored — the transcript re-derives on focus.
                    break;
                case 'delete_approval':
                    // A background pane's turn needs an approval: surface the
                    // modal (it pauses input anyway) with the session tagged
                    // so the response routes correctly.
                    showDeleteApproval(data);
                    break;
                default:
                    break;
            }
            if (pane.turnActive && !pane._notifiedBusy) {
                pane._notifiedBusy = true;
                sendNotification('GoGen', `Pane ${pane.label || pane.id || 'session'} is responding.`, 'gogen-pane-busy');
            } else if (!pane.turnActive) {
                pane._notifiedBusy = false;
            }
            // Re-render the sidebar session rows only when something the
            // list shows actually changed (label / id / busy state) — stream
            // chunks for a background pane would otherwise rebuild the list
            // on every token.
            if (pane.turnActive !== beforeTurn || pane.label !== beforeLabel || pane.id !== beforeId) {
                refreshSidebarSessions();
            }
        }

        // Make a pane the active/visible one. The transcript is cleared and
        // re-attached so the server resends the pane's state.
        function focusPane(key) {
            if (key === activePaneKey) return;
            const pane = panes.get(key);
            if (!pane) return;
            // Focusing counts as activity: the sidebar shows the focused
            // pane at the top (most recently used first).
            movePaneToFront(pane);
            saveActivePaneState();
            activePaneKey = key;
            // clearChat resets module-level DOM state; load the pane's state
            // AFTER it so the restored flags survive.
            clearChat();
            loadActivePaneState();
            // Mode/thinking/model are per-session; restore the toolbar to
            // this pane's last-known values.
            if (pane.mode) updateModeInfo(pane.mode);
            if (pane.thinkingLevel) renderThinkingChips(pane.thinkingLevel);
            if (pane.model) updateModelInfo(pane.model);
            if (sessionInfoDiv) sessionInfoDiv.textContent = pane.id || '';
            refreshSidebarSessions();
            if (pane.id && ws && ws.readyState === WebSocket.OPEN) {
                ws.send(JSON.stringify({ type: 'session_attach', sessionId: pane.id }));
            }
        }

        // Close a pane: the session is explicitly closed — the server
        // cancels any in-flight turn and unregisters the runtime (the
        // session stays saved server-side) — then remove it from the UI.
        // Reopening it later reloads it from the saved list.
        function closePane(key) {
            const pane = panes.get(key);
            if (!pane) return;
            if (pane.id && ws && ws.readyState === WebSocket.OPEN) {
                ws.send(JSON.stringify({ type: 'session_close', sessionId: pane.id }));
            }
            panes.delete(key);
            if (activePaneKey === key) {
                if (panes.size === 0) {
                    const p = makePane();
                    activePaneKey = p.key;
                    clearChat();
                    loadActivePaneState();
                    if (ws && ws.readyState === WebSocket.OPEN) {
                        pendingSessionResponse = true;
                        ws.send(JSON.stringify({ type: 'session_new' }));
                    }
                } else {
                    focusPane(panes.keys().next().value);
                }
            } else {
                refreshSidebarSessions();
            }
            // The closed session is saved, not deleted: refresh the list so
            // the closed session's row reappears (and, if the active pane
            // was closed, the new active pane is marked).
            requestSessionList();
        }

        // Open (or focus) a session as the active pane — used by the sidebar
        // saved-sessions list. The session is attached (loaded from the store
        // when not currently active) and the transcript re-derives from the
        // server's attach response.
        function openSessionPane(id) {
            if (!ws || ws.readyState !== WebSocket.OPEN) {
                showToast('Not connected', 'error');
                return;
            }
            if (!id) return;
            if (resendAwaitingHistory) {
                showToast('Resend already in progress', 'info');
                return;
            }
            const existing = findPaneBySession(id);
            if (existing) {
                focusPane(existing.key);
                return;
            }
            saveActivePaneState();
            const pane = makePane();
            pane.id = id;
            activePaneKey = pane.key;
            // makePane refreshed the sidebar while the id was still unknown.
            // Refresh again so the new active row is marked "current": the
            // attach reply's config echo carries the SAME id, so the config
            // handler would not re-render (id unchanged) and the row would
            // stay stale until the next pane switch.
            refreshSidebarSessions();
            clearChat();
            loadActivePaneState();
            if (sessionInfoDiv) sessionInfoDiv.textContent = id;
            pendingSessionResponse = false;
            ws.send(JSON.stringify({ type: 'session_attach', sessionId: id }));
            requestSessionList();
        }

        // Collapse only when output is genuinely large (DOM / scroll cost).
        const BIG_RESULT_CHARS = 32000;
        const BIG_RESULT_LINES = 400;
        const BIG_ARG_CHARS = 4000;

        function isResultReallyBig(text) {
            const s = text || '';
            if (!s) return false;
            if (s.length > BIG_RESULT_CHARS) return true;
            // Avoid split on huge strings when length already qualifies.
            let lines = 1;
            for (let i = 0; i < s.length; i++) {
                if (s.charCodeAt(i) === 10) {
                    lines++;
                    if (lines > BIG_RESULT_LINES) return true;
                }
            }
            return false;
        }

        function summarizeResult(result, success) {
            const trimmed = (result || '').trim();
            if (!trimmed) return success ? '(empty)' : '(no output)';
            // Show full output unless it's really big.
            if (!isResultReallyBig(trimmed)) return trimmed;
            const lines = trimmed.split('\n').length;
            const chars = trimmed.length;
            if (!success) {
                let first = trimmed.split('\n')[0];
                if (first.length > 200) first = first.slice(0, 197) + '...';
                return `${first}\n\n… (${lines} lines, ${chars} chars)`;
            }
            return `(large output: ${lines} lines, ${chars} chars)`;
        }

        // Tool-result syntax highlighting (Monaco tokenization) is deferred
        // while a history replay is rebuilding the pane: restored sessions
        // paint the transcript first, then colorize in idle-time slices.
        // The colorize itself is unchanged, so restored cards end up looking
        // identical to live ones.
        function scheduleResultColorize(el, language) {
            if (replayInProgress) {
                deferredResultColorize.push(() => colorizeElement(el, language));
                return;
            }
            colorizeElement(el, language);
        }

        // Run deferred tool-result colorizes after the replay finishes, in
        // ~12ms idle slices so a session with many large results doesn't jank
        // the main thread in one block. Each job is awaited so the slice
        // deadline actually bounds the synchronous tokenization work.
        function flushDeferredResultColorize() {
            if (!deferredResultColorize.length) return;
            const jobs = deferredResultColorize;
            deferredResultColorize = [];
            let i = 0;
            const runSlice = async () => {
                const deadline = performance.now() + 12;
                while (i < jobs.length && performance.now() < deadline) {
                    try { await jobs[i](); } catch (_) { /* keep going */ }
                    i++;
                }
                if (i < jobs.length) {
                    if (typeof requestIdleCallback === 'function') {
                        requestIdleCallback(runSlice, { timeout: 100 });
                    } else {
                        setTimeout(runSlice, 0);
                    }
                }
            };
            if (typeof requestIdleCallback === 'function') {
                requestIdleCallback(runSlice, { timeout: 400 });
            } else {
                setTimeout(runSlice, 50);
            }
        }

        function appendExpandableResult(body, result, success, truncated, options = {}) {
            const { language = '' } = options;
            const content = document.createElement('div');
            content.className = `tool-result-content ${success ? '' : 'error-content'}`;
            const full = result || '';
            const summary = summarizeResult(full, success);
            content.textContent = summary;
            body.appendChild(content);

            // Copy button on tool results
            if (full.trim()) {
                const copyBtn = document.createElement('button');
                copyBtn.className = 'tool-result-copy';
                copyBtn.textContent = 'Copy';
                copyBtn.onclick = () => { navigator.clipboard.writeText(full).then(() => showToast('Copied', 'success')).catch(() => showToast('Copy failed', 'error')); };
                body.appendChild(copyBtn);
            }

            const canHighlight = success && language && language !== 'plaintext';
            const showingFull = summary === full.trim();
            if (canHighlight && showingFull) {
                scheduleResultColorize(content, language);
            }

            if (summary !== full.trim() && (full.trim() || truncated)) {
                const expandBtn = document.createElement('button');
                expandBtn.textContent = truncated ? 'Show received output' : 'Show full output';
                expandBtn.className = 'btn-link';
                expandBtn.onclick = () => {
                    content.textContent = full;
                    content.classList.remove('monaco-colorized');
                    delete content.dataset.monacoColorized;
                    if (canHighlight) colorizeElement(content, language);
                    expandBtn.remove();
                };
                body.appendChild(expandBtn);
            }
        }

        function readFilePathFromArgs(args) {
            if (!args || typeof args !== 'object') return '';
            const p = args.file_path || args.path || '';
            return typeof p === 'string' ? p : '';
        }

        function parseInlineJSONArgs(raw) {
            const s = (raw || '').trim();
            if (!s.startsWith('{')) return null;
            try {
                return JSON.parse(s);
            } catch (_) {
                return null;
            }
        }

        function formatArgsCompact(rawJSON, maxLen = BIG_ARG_CHARS) {
            const args = parseInlineJSONArgs(rawJSON);
            if (!args || typeof args !== 'object') return '';
            const parts = [];
            for (const [k, v] of Object.entries(args)) {
                if (k === 'diff') continue;
                let val = typeof v === 'string' ? v : String(v);
                if (val.length > maxLen) val = val.slice(0, maxLen - 3) + '...';
                parts.push(`${k}=${JSON.stringify(val)}`);
            }
            return parts.length ? `(${parts.join(', ')})` : '';
        }

        function abortInFlightUI(message) {
            finalizeThinking();
            endStream();
            setInputProgress(null);
            // Remove waiting spinners on pending cards
            for (const id of Object.keys(pendingToolCards)) {
                const cardInfo = pendingToolCards[id];
                if (cardInfo && cardInfo.waiting) {
                    cardInfo.waiting.remove();
                    cardInfo.waiting = null;
                }
            }
            for (const idx of Object.keys(streamingToolCards)) {
                const cardInfo = streamingToolCards[idx];
                if (cardInfo && cardInfo.argsStream) {
                    cardInfo.argsStream.classList.remove('cursor');
                }
            }
            streamingToolCards = {};
            // Keep completed pending cards; clear only in-flight stream map
            if (message) appendMessage('system', message);
        }

        function renderMarkdownHTML(text) {
            const raw = marked.parse(text || '', { async: false });
            return DOMPurify.sanitize(raw, { USE_PROFILES: { html: true } });
        }

        /** Escape HTML entities so tags are displayed as plain text. */
        function escapeHtml(str) {
            const map = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' };
            return (str || '').replace(/[&<>"']/g, (ch) => map[ch]);
        }

        function enhanceCodeBlocksWithCopy(root) {
            if (!root || !root.querySelectorAll) return;
            root.querySelectorAll('pre').forEach((pre) => {
                if (pre.closest('.code-block-wrap')) return;
                const wrap = document.createElement('div');
                wrap.className = 'code-block-wrap';
                pre.parentNode.insertBefore(wrap, pre);
                wrap.appendChild(pre);
                const btn = document.createElement('button');
                btn.type = 'button';
                btn.className = 'code-copy-btn';
                btn.textContent = 'Copy';
                btn.title = 'Copy code';
                btn.addEventListener('click', async (e) => {
                    e.preventDefault();
                    e.stopPropagation();
                    const code = pre.querySelector('code') || pre;
                    const text = code.textContent || '';
                    try {
                        await navigator.clipboard.writeText(text);
                        btn.textContent = 'Copied';
                        setTimeout(() => { btn.textContent = 'Copy'; }, 1500);
                    } catch (err) {
                        showToast('Copy failed', 'error');
                    }
                });
                wrap.appendChild(btn);
            });
        }

        // Colorize runs on the same cadence as markdown; stale async runs are dropped via _gogenHlGen.
        function setMessageMarkdown(el, text) {
            el.classList.add('md');
            el._gogenHlGen = (el._gogenHlGen || 0) + 1;
            // Wrap rendered content in a child element so edit-resend can
            // hide/show it without touching appended buttons.
            let textWrap = el.querySelector('.msg-text');
            if (!textWrap) {
                textWrap = document.createElement('div');
                textWrap.className = 'msg-text';
                el.appendChild(textWrap);
            }
            textWrap.innerHTML = renderMarkdownHTML(text);
            messageRawStore.set(el, text);
            enhanceCodeBlocksWithCopy(textWrap);
            linkifyMessageRefs(textWrap);
            colorizeCodeBlocks(textWrap);
        }

        /**
         * Make `@path:line` (and `@path:start-end`) references in rendered
         * assistant messages clickable. Code blocks, links and already-wrapped
         * nodes are skipped. Clicking opens the file in the editor, reveals the
         * line and highlights the range (matches the "Add Reference to Chat"
         * context-menu format).
         */
        function linkifyMessageRefs(root) {
            if (!root || !root.querySelectorAll) return;
            const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
            const candidates = [];
            while (walker.nextNode()) {
                const n = walker.currentNode;
                if (!n.nodeValue || !n.nodeValue.includes('@')) continue;
                const pe = n.parentElement;
                if (pe && pe.closest('pre, code, a')) continue;
                candidates.push(n);
            }
            const re = /(^|\s)@([\w./\\~-]+):(\d+)(?:-(\d+))?/g;
            for (const node of candidates) {
                const text = node.nodeValue;
                const frag = document.createDocumentFragment();
                let last = 0;
                let changed = false;
                let m;
                re.lastIndex = 0;
                while ((m = re.exec(text))) {
                    changed = true;
                    if (m.index > last) frag.appendChild(document.createTextNode(text.slice(last, m.index)));
                    const path = m[2];
                    const start = parseInt(m[3], 10);
                    const end = m[4] ? parseInt(m[4], 10) : start;
                    const span = document.createElement('span');
                    span.className = 'file-ref';
                    span.textContent = m[1] + '@' + path + ':' + m[3] + (m[4] ? '-' + m[4] : '');
                    span.title = `Open ${path}:${start} in editor`;
                    span.addEventListener('click', (e) => {
                        e.stopPropagation();
                        openFileAtLine(path, start, end).catch(() => {});
                    });
                    frag.appendChild(span);
                    last = m.index + m[0].length;
                }
                if (changed) {
                    if (last < text.length) frag.appendChild(document.createTextNode(text.slice(last)));
                    node.parentNode.replaceChild(frag, node);
                }
            }
        }

        // ===== Incremental streaming render =====
        // Streaming messages are re-rendered on a ~32ms cadence. Re-parsing,
        // re-sanitizing and re-swapping the whole accumulated markdown on every
        // flush is O(n²) in message length and forces a full reflow per flush.
        // Instead we render stable markdown blocks once and only re-render the
        // in-flight tail block. Block boundaries are conservative (blank lines
        // outside fenced code blocks), so no paragraph is ever rendered
        // partially. Any residual block-split artifacts (e.g. a list split at
        // a blank line) are corrected by the final full render when the stream
        // ends (see endStream/finalizeThinking).
        function splitStreamBlocks(text) {
            const blocks = [];
            let cur = '';
            let fence = null; // { mark: '`'|'~', len } while inside a fenced code block
            for (const line of String(text).split('\n')) {
                if (fence) {
                    cur += line + '\n';
                    // CommonMark closing rule: same char, length >= opener,
                    // nothing but trailing whitespace after it. This keeps a
                    // 3-tick line from closing a 4-tick outer fence (nested
                    // code blocks) and "```js" from closing a fence.
                    const c = /^\s*(`+|~+)\s*$/.exec(line);
                    if (c && c[1][0] === fence.mark && c[1].length >= fence.len) fence = null;
                    continue;
                }
                const m = /^\s*(```+|~~~+)/.exec(line);
                if (m) {
                    cur += line + '\n';
                    fence = { mark: m[1][0], len: m[1].length };
                    continue;
                }
                if (line.trim() === '') {
                    if (cur !== '') {
                        blocks.push(cur);
                        cur = '';
                    }
                    continue;
                }
                cur += line + '\n';
            }
            if (cur !== '') blocks.push(cur);
            return blocks;
        }

        // Incremental renderer for the live streaming paths (assistant stream
        // and thinking). Per-element state lives on el._gogenBlocks; the DOM
        // shape mirrors setMessageMarkdown (.msg-text wrapper) so edit/resend
        // and history replay behave identically.
        function renderStreamMarkdown(el, text) {
            el.classList.add('md');
            el._gogenHlGen = (el._gogenHlGen || 0) + 1;
            let textWrap = el.querySelector('.msg-text');
            if (!textWrap) {
                textWrap = document.createElement('div');
                textWrap.className = 'msg-text';
                el.appendChild(textWrap);
            }
            const st = el._gogenBlocks || (el._gogenBlocks = { done: [], tailNode: null });
            const blocks = splitStreamBlocks(text);
            // The last block is the in-flight tail; everything before it is complete.
            const doneCount = blocks.length > 0 ? blocks.length - 1 : 0;

            // Completed blocks: keep stable nodes for unchanged text, render
            // new ones. New nodes are inserted before the tail node so DOM
            // order always matches document order.
            for (let i = 0; i < doneCount; i++) {
                const prev = st.done[i];
                if (prev && prev.text === blocks[i]) continue;
                const node = document.createElement('div');
                node.className = 'md-block';
                node.innerHTML = renderMarkdownHTML(blocks[i]);
                if (prev) {
                    prev.node.replaceWith(node);
                    st.done[i] = { text: blocks[i], node };
                } else if (st.tailNode) {
                    textWrap.insertBefore(node, st.tailNode);
                    st.done.push({ text: blocks[i], node });
                } else {
                    textWrap.appendChild(node);
                    st.done.push({ text: blocks[i], node });
                }
            }
            // Drop completed blocks that no longer exist (text rewound — this is
            // defensive; streaming only ever grows).
            while (st.done.length > doneCount) {
                st.done.pop().node.remove();
            }

            // Tail: one stable node, re-rendered every flush.
            const tailText = doneCount < blocks.length ? blocks[doneCount] : '';
            if (!st.tailNode) {
                st.tailNode = document.createElement('div');
                st.tailNode.className = 'md-block md-tail';
                textWrap.appendChild(st.tailNode);
            }
            st.tailNode.innerHTML = tailText ? renderMarkdownHTML(tailText) : '';

            messageRawStore.set(el, text);
            enhanceCodeBlocksWithCopy(textWrap);
            colorizeCodeBlocks(textWrap);
        }

        // ===== Message timestamps =====
        // Single relative-time helper for messages and session rows. `now` is
        // passed in by the periodic refresh so all timestamps tick together
        // without re-reading the clock per element.
        function formatRelativeTime(date, now = Date.now()) {
            const diff = Math.max(0, Math.floor((now - date.getTime()) / 1000));
            if (diff < 5) return 'now';
            if (diff < 60) return `${diff}s ago`;
            if (diff < 3600) return `${Math.floor(diff / 60)}m ago`;
            if (diff < 86400) return `${Math.floor(diff / 3600)}h ago`;
            if (diff < 604800) return `${Math.floor(diff / 86400)}d ago`;
            return date.toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
        }

        function relativeTime(value, now = Date.now()) {
            if (!value) return '';
            const date = value instanceof Date ? value : new Date(value);
            if (Number.isNaN(date.getTime())) return '';
            return formatRelativeTime(date, now);
        }

        function formatExactTime(date) {
            return date.toLocaleString(undefined, {
                year: 'numeric', month: 'short', day: 'numeric',
                hour: '2-digit', minute: '2-digit'
            });
        }

        // Relative labels ("now", "3m ago") go stale; re-derive them on a
        // light cadence (and when the tab becomes visible again) so the
        // transcript stays truthful without re-rendering anything.
        function refreshMessageTimestamps() {
            if (document.hidden) return;
            const now = Date.now();
            for (const el of messagesDiv.querySelectorAll('.message-time')) {
                const msg = el.closest('.message');
                if (!msg || !msg.dataset.createdAt) continue;
                const t = Date.parse(msg.dataset.createdAt);
                if (Number.isNaN(t)) continue;
                const text = formatRelativeTime(new Date(t), now);
                if (el.textContent !== text) el.textContent = text;
            }
        }
        // Same staleness class for the sidebar session rows: their relative
        // times are computed at render time, so re-derive them in place on
        // the same cadence without re-rendering the list.
        function refreshSessionRowTimes() {
            if (document.hidden || !sessionListDiv) return;
            const now = Date.now();
            for (const el of sessionListDiv.querySelectorAll('.session-row-time')) {
                const upd = el.dataset.updated;
                if (!upd) continue;
                const text = relativeTime(upd, now);
                if (el.textContent !== text) el.textContent = text;
            }
        }
        setInterval(() => { refreshMessageTimestamps(); refreshSessionRowTimes(); }, 30000);
        document.addEventListener('visibilitychange', () => { refreshMessageTimestamps(); refreshSessionRowTimes(); });

        function addTimestampToMsg(msgDiv, date) {
            if (msgDiv.querySelector('.message-time')) return; // already added
            const timeEl = document.createElement('span');
            timeEl.className = 'message-time';
            timeEl.textContent = formatRelativeTime(date);
            timeEl.title = formatExactTime(date);
            msgDiv.appendChild(timeEl);
        }
        // ==============================

        /** Send a fork request.
         *  For history-replayed messages the server-side message index is used.
         *  For streaming messages (no histIdx) we pass -1 meaning "last assistant".
         *  The fork opens a NEW pane with the fork's history; the
         *  source session is left untouched. */
        function forkSession(msgIdx) {
            if (!ws || ws.readyState !== WebSocket.OPEN) return;
            if (resendAwaitingHistory) {
                showToast('Resend already in progress', 'info');
                return;
            }
            if (pendingSessionResponse) {
                showToast('Session change already in progress', 'info');
                return;
            }
            const srcId = activePane().id;
            // Open a new pane; the source pane stays open in the background.
            saveActivePaneState();
            const pane = makePane();
            activePaneKey = pane.key;
            clearChat();
            loadActivePaneState();
            pendingSessionResponse = true;
            ws.send(JSON.stringify({ type: 'session_fork', messageIndex: msgIdx, sessionId: srcId }));
        }

        function cancelActiveTurn() {
            if (!turnActive || !ws || ws.readyState !== WebSocket.OPEN) return;
            if (!activePane() || !activePane().id) return;
            suppressTurnEnds++;
            ws.send(JSON.stringify({ type: 'cancel', sessionId: activePane().id }));
            setTurnActive(false);
            abortInFlightUI(null);
        }

        function clearPendingResend() {
            pendingResendContent = null;
            resendAwaitingHistory = false;
        }

        /** Fork/new to before a user message, then send content as a normal turn. */
        function beginResend(histIdx, content) {
            if (!ws || ws.readyState !== WebSocket.OPEN) return;
            if (histIdx === undefined || histIdx < 0 || !content) return;
            if (resendAwaitingHistory || pendingResendContent !== null) {
                showToast('Resend already in progress', 'info');
                return;
            }
            cancelActiveTurn();
            pendingResendContent = content;
            resendAwaitingHistory = true;
            pendingSessionResponse = true;
            enableFollow();
            // D3: typed /new and the resend flow REPLACE the pane's session
            // (the config reply re-keys this pane to the new session id).
            const sid = activePane().id;
            if (histIdx === 0) {
                ws.send(JSON.stringify({ type: 'session_new', sessionId: sid }));
            } else {
                ws.send(JSON.stringify({ type: 'session_fork', messageIndex: histIdx - 1, sessionId: sid }));
            }
        }

        function flushPendingResend() {
            if (!resendAwaitingHistory) return;
            const text = pendingResendContent;
            clearPendingResend();
            // The resent turn streams live in this pane — no convergence refetch needed.
            if (activePane()) activePane().needsFreshHistory = false;
            if (!text || !ws || ws.readyState !== WebSocket.OPEN) return;
            const el = appendMessage('user', text);
            if (el) el.dataset.pendingAck = '1';
            streamingToolCards = {};
            pendingToolCards = {};
            toolsStartedThisTurn = false;
            contextEstAdded = 0;
            ws.send(JSON.stringify({ type: 'message', content: text, sessionId: activePane().id }));
            setTurnActive(true);
            enableFollow();
        }

        /** Attach resend/edit controls once a user bubble has a server histIdx. */
        function ensureUserResendActions(msgDiv) {
            if (!msgDiv || msgDiv.querySelector('.resend-btn')) return;
            if (msgDiv.dataset.histIdx === undefined) return;
            const histIdx = parseInt(msgDiv.dataset.histIdx, 10);
            if (Number.isNaN(histIdx) || histIdx < 0) return;

            const resendBtn = document.createElement('button');
            resendBtn.className = 'resend-btn';
            resendBtn.type = 'button';
            resendBtn.innerHTML = '↻';
            resendBtn.title = 'Resend this message';
            resendBtn.addEventListener('click', (e) => {
                e.stopPropagation();
                const idx = parseInt(msgDiv.dataset.histIdx, 10);
                const content = msgDiv.dataset.rawContent || '';
                if (!content || Number.isNaN(idx) || idx < 0) return;
                beginResend(idx, content);
            });
            msgDiv.appendChild(resendBtn);

            const editBtn = document.createElement('button');
            editBtn.className = 'edit-btn';
            editBtn.type = 'button';
            editBtn.innerHTML = '✎';
            editBtn.title = 'Edit and resend';
            editBtn.addEventListener('click', (e) => {
                e.stopPropagation();
                if (turnActive) return;
                if (msgDiv.querySelector('.inline-edit-bar')) return;

                const tw = msgDiv.querySelector('.msg-text');
                if (!tw) return;
                const raw = msgDiv.dataset.rawContent || '';

                msgDiv.querySelectorAll('.resend-btn, .edit-btn').forEach(b => { b.style.display = 'none'; });

                // Plain text — markdown DOM is not a reliable contenteditable surface.
                tw.textContent = raw;
                tw.contentEditable = 'true';
                tw.classList.add('editing');
                tw.focus();
                const range = document.createRange();
                range.selectNodeContents(tw);
                range.collapse(false);
                const sel = window.getSelection();
                sel.removeAllRanges();
                sel.addRange(range);

                const bar = document.createElement('div');
                bar.className = 'inline-edit-bar';
                const sendBtn2 = document.createElement('button');
                sendBtn2.className = 'inline-edit-send';
                sendBtn2.type = 'button';
                sendBtn2.innerHTML = '▶';
                sendBtn2.title = 'Send (Enter)';
                const cancelBtn2 = document.createElement('button');
                cancelBtn2.className = 'inline-edit-cancel';
                cancelBtn2.type = 'button';
                cancelBtn2.innerHTML = '×';
                cancelBtn2.title = 'Cancel (Esc)';
                bar.appendChild(sendBtn2);
                bar.appendChild(cancelBtn2);
                msgDiv.appendChild(bar);

                function onEditKeydown(ev) {
                    if (ev.key === 'Escape') {
                        ev.preventDefault();
                        finishEdit(true);
                    } else if (ev.key === 'Enter' && !ev.shiftKey) {
                        ev.preventDefault();
                        sendBtn2.click();
                    }
                }

                function finishEdit(restore) {
                    tw.removeEventListener('keydown', onEditKeydown);
                    tw.contentEditable = 'false';
                    tw.classList.remove('editing');
                    if (restore) {
                        setMessageMarkdown(msgDiv, escapeHtml(msgDiv.dataset.rawContent || ''));
                    }
                    bar.remove();
                    msgDiv.querySelectorAll('.resend-btn, .edit-btn').forEach(b => { b.style.display = ''; });
                }

                cancelBtn2.addEventListener('click', (ev) => {
                    ev.stopPropagation();
                    finishEdit(true);
                });

                tw.addEventListener('keydown', onEditKeydown);

                sendBtn2.addEventListener('click', (ev) => {
                    ev.stopPropagation();
                    const newContent = tw.innerText.replace(/\u00a0/g, ' ').trim();
                    if (!newContent) return;
                    finishEdit(false);
                    const idx = parseInt(msgDiv.dataset.histIdx, 10);
                    if (Number.isNaN(idx) || idx < 0) return;
                    beginResend(idx, newContent);
                });
            });
            msgDiv.appendChild(editBtn);
        }

        function appendMessage(role, text, date, histIdx, images) {
            return appendMessageAtTime(role, text, date || new Date(), histIdx, images);
        }

        function appendMessageAtTime(role, text, date, histIdx, images) {
            removeEmptyState();
            const msgDiv = document.createElement('div');
            msgDiv.className = `message ${role}`;
            const idx = msgIdxCounter++;
            msgDiv.dataset.msgIdx = idx;
            if (histIdx !== undefined && histIdx >= 0) {
                msgDiv.dataset.histIdx = histIdx;
            }
            if (role === 'user' && images && images.length) {
                // Render user-attached images (vision input) above the text.
                const imgRow = document.createElement('div');
                imgRow.className = 'chat-image-row';
                for (const img of images) {
                    if (!img || !img.dataUrl) continue;
                    const imgEl = document.createElement('img');
                    imgEl.className = 'chat-image';
                    imgEl.src = img.dataUrl;
                    imgEl.alt = 'attached image';
                    imgEl.loading = 'lazy';
                    imgRow.appendChild(imgEl);
                }
                if (imgRow.childNodes.length) msgDiv.insertBefore(imgRow, msgDiv.firstChild);
            }
            if (role === 'assistant' || role === 'user') {
                // User messages go through markdown too, but HTML tags are escaped first
                // to prevent pasted HTML from influencing the page layout.
                const safe = role === 'user' ? escapeHtml(text) : text;
                setMessageMarkdown(msgDiv, safe);
            } else {
                const span = document.createElement('span');
                span.textContent = text;
                msgDiv.appendChild(span);
                messageRawStore.set(msgDiv, text);
            }
            msgDiv.dataset.createdAt = date.toISOString();
            addTimestampToMsg(msgDiv, date);
            if (role === 'assistant') {
                const forkBtn = document.createElement('button');
                forkBtn.className = 'fork-btn';
                forkBtn.textContent = '⑂';
                forkBtn.title = 'Fork session from this message';
                forkBtn.addEventListener('click', (e) => {
                    e.stopPropagation();
                    // Use the server-side history index when available (history replay),
                    // otherwise fall back to the display index (streaming messages).
                    const targetIdx = msgDiv.dataset.histIdx !== undefined
                        ? parseInt(msgDiv.dataset.histIdx)
                        : parseInt(msgDiv.dataset.msgIdx);
                    forkSession(targetIdx);
                });
                msgDiv.appendChild(forkBtn);
            }
            if (role === 'user') {
                msgDiv.dataset.rawContent = text;
                ensureUserResendActions(msgDiv);
            }
            messagesDiv.appendChild(msgDiv);
            smartScroll();
            return msgDiv;
        }

        function flushStreamRender() {
            streamRafPending = false;
            streamLastRender = performance.now();
            if (!currentStreamDiv) return;
            const keepCursor = currentStreamDiv.classList.contains('cursor');
            renderStreamMarkdown(currentStreamDiv, currentStreamRaw);
            if (keepCursor) currentStreamDiv.classList.add('cursor');
            // Scroll after DOM height actually changes (tokens arrive before paint).
            smartScroll();
        }

        function scheduleStreamRender() {
            // Coalesce multiple token arrivals into a single paint-aligned render
            if (streamRafPending) return;
            const elapsed = performance.now() - streamLastRender;
            if (elapsed >= STREAM_RENDER_INTERVAL) {
                streamRafPending = true;
                requestAnimationFrame(() => {
                    if (!streamRafPending) return;
                    flushStreamRender();
                });
            }
        }

        function startStream() {
            removeEmptyState();
            streamRafPending = false;
            streamLastRender = 0;
            const msgDiv = document.createElement('div');
            msgDiv.className = 'message assistant cursor md';
            messagesDiv.appendChild(msgDiv);
            smartScroll();
            currentStreamDiv = msgDiv;
            currentStreamRaw = '';
        }

        function appendStreamToken(token) {
            if (!currentStreamDiv) return;
            currentStreamRaw += token;
            bumpContextEstimate(token);
            scheduleStreamRender();
        }

        function showThinking() {
            removeEmptyState();
            finalizeThinking();
            const div = document.createElement('div');
            div.className = 'thought-card';

            const header = document.createElement('div');
            header.style.cssText = 'display:flex;align-items:center;gap:6px;cursor:pointer;user-select:none;font-size:0.85em;color:var(--fg-muted);padding:2px 0;';
            const label = document.createElement('span');
            label.style.fontStyle = 'italic';
            label.textContent = 'Thinking';
            const toggle = document.createElement('span');
            toggle.textContent = '▾';
            toggle.style.fontSize = '0.8em';
            header.appendChild(label);
            header.appendChild(toggle);

            // Markdown is rendered live into this body (like the assistant
            // stream); base size stays compact, typography comes from CSS.
            const body = document.createElement('div');
            body.style.cssText = 'font-size:0.85em;padding:4px 0 0 0;';

            div.appendChild(header);
            div.appendChild(body);
            messagesDiv.appendChild(div);

            // Collapse toggle active from the start
            header.addEventListener('click', () => {
                const collapsed = body.style.display === 'none';
                body.style.display = collapsed ? '' : 'none';
                toggle.textContent = collapsed ? '▾' : '▸';
            });

            smartScroll();
            currentThinkingRaw = '';
            currentThinkingDiv = div;
            currentThinkingSpan = body;
        }

        function appendThinkingToken(token) {
            // Match TUI: ignore thinking after tool calls start in this round.
            // Cleared again on the next round's "thinking" (OnRoundStart).
            if (toolsStartedThisTurn) return;
            if (!currentThinkingSpan) {
                showThinking();
            }
            currentThinkingRaw += token;
            bumpContextEstimate(token);
            scheduleThinkingRender();
        }

        function flushThinkingRender() {
            thinkingRafPending = false;
            thinkingLastRender = performance.now();
            if (!currentThinkingSpan) return;
            // Live markdown render of the reasoning content, exactly like the
            // assistant stream (same cadence, same incremental renderer).
            renderStreamMarkdown(currentThinkingSpan, currentThinkingRaw);
            smartScroll();
        }

        function scheduleThinkingRender() {
            // Coalesce thinking-token arrivals into a single paint-aligned
            // render, exactly like the assistant stream (STREAM_RENDER_INTERVAL).
            if (thinkingRafPending) return;
            const elapsed = performance.now() - thinkingLastRender;
            if (elapsed >= STREAM_RENDER_INTERVAL) {
                thinkingRafPending = true;
                requestAnimationFrame(() => {
                    if (!thinkingRafPending) return;
                    flushThinkingRender();
                });
            }
        }

        // --- Input-area progress indicator (replaces textarea during streaming) ---
        let currentProgressPhase = null;

        /**
         * Show/update the progress indicator in the input area.
         * Phase: 'thinking' | 'streaming' | 'tool' | null
         * When phase is null, the indicator is hidden and the textarea is restored.
         */
        function setInputProgress(phase, label) {
            if (!inputProgress) return;
            if (phase == null) {
                // Hide progress, restore textarea
                inputProgress.classList.remove('active');
                inputArea.style.display = '';
                inputArea.focus();
                currentProgressPhase = null;
                return;
            }
            currentProgressPhase = phase;
            inputArea.style.display = 'none';
            inputProgress.classList.add('active');
            const spinner = inputProgress.querySelector('.progress-spinner');
            const labelEl = inputProgress.querySelector('.progress-label');
            if (spinner) {
                spinner.className = 'progress-spinner ' + phase;
            }
            if (labelEl) {
                labelEl.textContent = label || phase;
            }
        }

        function finalizeThinking() {
            if (!currentThinkingDiv) return;
            const div = currentThinkingDiv;
            const content = (currentThinkingRaw || '').trim();
            if (content) {
                // Final full render: corrects any block-split artifacts from
                // the incremental live stream (e.g. a list split at a blank
                // line) so the collapsed card matches a single-shot render.
                setMessageMarkdown(currentThinkingSpan, currentThinkingRaw);
            }
            currentThinkingDiv = null;
            currentThinkingSpan = null;
            currentThinkingRaw = '';
            thinkingRafPending = false;
            if (!content) {
                div.remove();
                return;
            }
            // Card is already fully structured from showThinking() —
            // the collapsible toggle is already active, no rebuild needed.
        }

        function endStream() {
            if (currentStreamDiv) {
                // Final full render: the incremental live renderer keeps
                // stable blocks plus an in-flight tail, so a full pass here
                // guarantees the final DOM exactly matches a single-shot
                // render (fixes any block-split artifacts from streaming).
                setMessageMarkdown(currentStreamDiv, currentStreamRaw || '');
                currentStreamDiv.classList.remove('cursor');
                addTimestampToMsg(currentStreamDiv, new Date());
                // Assign a message index and fork button.
                // Streaming messages use the server-side "last" target because
                // the client's clock may not match the server's CreatedAt (which
                // would cause the wrong message to be matched, or no match at all).
                const idx = msgIdxCounter++;
                currentStreamDiv.dataset.msgIdx = idx;
                const forkBtn = document.createElement('button');
                forkBtn.className = 'fork-btn';
                forkBtn.textContent = '⑂';
                forkBtn.title = 'Fork session from this message';
                forkBtn.addEventListener('click', (e) => {
                    e.stopPropagation();
                    // Streaming messages don't have a known server-side index,
                    // so send -1 which the server treats as "last assistant".
                    forkSession(-1);
                });
                currentStreamDiv.appendChild(forkBtn);
            }
            currentStreamDiv = null;
            currentStreamRaw = '';
        }

        // Also flush when the tab becomes visible again after being hidden
        // (rAF is paused in hidden tabs, so pending renders may be stale).
        document.addEventListener('visibilitychange', () => {
            if (!document.hidden && currentStreamDiv && streamRafPending) {
                streamRafPending = false;
                scheduleStreamRender();
            }
            if (!document.hidden && currentThinkingSpan && thinkingRafPending) {
                thinkingRafPending = false;
                scheduleThinkingRender();
            }
        });

        // Refresh relative timestamps every 30 seconds
        setInterval(() => {
            document.querySelectorAll('.message[data-created-at]').forEach(el => {
                const t = el.querySelector('.message-time');
                if (t) {
                    const date = new Date(el.dataset.createdAt);
                    if (isNaN(date.getTime())) return;
                    t.textContent = formatRelativeTime(date);
                }
            });
        }, 30000);

        function formatToolArgs(args) {
            if (!args || typeof args !== 'object') return '';
            const parts = [];
            for (const [key, value] of Object.entries(args)) {
                // Diffs are shown in Monaco; keep the header compact.
                if (key === 'diff' && typeof value === 'string') {
                    parts.push(`diff=<${value.length} chars>`);
                    continue;
                }
                let displayValue;
                if (typeof value === 'string' && value.length > BIG_ARG_CHARS) {
                    displayValue = `"${value.substring(0, BIG_ARG_CHARS - 3)}..."`;
                } else if (typeof value === 'string') {
                    displayValue = `"${value}"`;
                } else {
                    displayValue = String(value);
                }
                parts.push(`${key}=${displayValue}`);
            }
            return parts.length > 0 ? `(${parts.join(', ')})` : '';
        }

        /** Create a document fragment for tool args with clickable file paths. */
        function formatToolArgsFragment(args) {
            if (!args || typeof args !== 'object') return document.createTextNode('');
            const frag = document.createDocumentFragment();
            const FILE_KEYS = ['file_path', 'path', 'source', 'destination'];
            const entries = Object.entries(args);
            for (let i = 0; i < entries.length; i++) {
                const [key, value] = entries[i];
                if (i > 0) frag.appendChild(document.createTextNode(', '));
                frag.appendChild(document.createTextNode(`${key}=`));
                if (FILE_KEYS.includes(key) && typeof value === 'string' && value) {
                    const link = document.createElement('span');
                    link.className = 'file-link';
                    link.textContent = value;
                    link.title = 'Click to open in editor';
                    link.onclick = (e) => { e.stopPropagation(); openFileAtLine(value).catch(() => {}); };
                    frag.appendChild(link);
                } else {
                    let displayValue;
                    if (typeof value === 'string' && value.length > BIG_ARG_CHARS) {
                        displayValue = `"${value.substring(0, BIG_ARG_CHARS - 3)}..."`;
                    } else if (typeof value === 'string') {
                        displayValue = `"${value}"`;
                    } else {
                        displayValue = String(value);
                    }
                    frag.appendChild(document.createTextNode(displayValue));
                }
            }
            return frag;
        }

        function createToolCard(name, args, options = {}) {
            toolCallCounter++;
            const cardId = `tool-card-${toolCallCounter}`;
            const { streaming = false, streamIndex = null } = options;

            const card = document.createElement('div');
            card.className = 'tool-card';
            card.id = cardId;
            if (streamIndex !== null) {
                card.dataset.streamIndex = String(streamIndex);
            }

            const header = document.createElement('div');
            header.className = 'tool-call-header';

            const icon = document.createElement('span');
            icon.className = 'tool-icon';

            const toolName = document.createElement('span');
            toolName.className = 'tool-name';
            toolName.textContent = name;

            const toolArgs = document.createElement('span');
            toolArgs.className = 'tool-args';
            if (streaming) toolArgs.textContent = '';
            else toolArgs.appendChild(formatToolArgsFragment(args));

            const toggle = document.createElement('span');
            toggle.className = 'tool-toggle';
            toggle.textContent = '▾';

            header.appendChild(icon);
            header.appendChild(toolName);
            header.appendChild(toolArgs);
            header.appendChild(toggle);

            let waiting = null;
            let argsStream = null;
            let monacoHost = null;

            card.appendChild(header);

            // Always prepare a diff host for patch_file; other streaming tools
            // keep the raw args stream until finalized.
            if (streaming && name === 'patch_file') {
                monacoHost = document.createElement('div');
                monacoHost.className = 'monaco-tool-host';
                card.appendChild(monacoHost);
            } else if (streaming) {
                argsStream = document.createElement('div');
                argsStream.className = 'tool-args-streaming cursor';
                argsStream.textContent = '(';
                card.appendChild(argsStream);
            } else if (name === 'patch_file' && args && typeof args.diff === 'string') {
                monacoHost = document.createElement('div');
                monacoHost.className = 'monaco-tool-host';
                card.appendChild(monacoHost);
                waiting = document.createElement('div');
                waiting.className = 'tool-waiting';
                waiting.textContent = 'executing...';
                card.appendChild(waiting);
            } else {
                waiting = document.createElement('div');
                waiting.className = 'tool-waiting';
                waiting.textContent = 'executing...';
                card.appendChild(waiting);
            }

            messagesDiv.appendChild(card);
            smartScroll();

            header.addEventListener('click', () => {
                const collapsed = toggle.classList.toggle('collapsed');
                const body = card.querySelector('.tool-result-body');
                if (body) body.classList.toggle('collapsed', collapsed);
                if (argsStream) argsStream.classList.toggle('collapsed', collapsed);
                if (monacoHost) monacoHost.style.display = collapsed ? 'none' : 'block';
                const live = card.querySelector('.tool-live-output');
                if (live) live.classList.toggle('collapsed', collapsed);
                // Toggling changes content height below the fold; keep the
                // scroll system in sync (re-pin if following, refresh the
                // jump-button if detached).
                scheduleRepinIfPinned();
                if (!stickToBottom) updateScrollBottomBtn();
            });

            const cardInfo = {
                cardId,
                card,
                waiting,
                toggle,
                toolArgs,
                argsStream,
                monacoHost,
                monacoEditor: null,
                monacoMounting: null,
                toolName: name,
                args: args || {},
                rawArgs: '',
                pendingDiff: '',
                diffComplete: false,
                finalized: false,
                rafScheduled: false,
                lastFallbackText: '',
                scheduleFrameUpdate() {
                    // Coalesce per-delta tool-arg updates into one pass per
                    // frame: diff extraction, fallback/Monaco update, compact
                    // header, and scroll all run at most once per frame.
                    if (this.rafScheduled) return;
                    this.rafScheduled = true;
                    requestAnimationFrame(() => {
                        this.rafScheduled = false;
                        if (this.finalized || !this.card.isConnected) return;
                        if (this.monacoHost) {
                            let text;
                            if (this.diffComplete) {
                                // Diff value is final once its closing quote
                                // was seen; don't re-extract it every frame.
                                text = this.pendingDiff || '';
                            } else {
                                const extracted = extractDiffValue(this.rawArgs);
                                if (extracted.complete) {
                                    this.diffComplete = true;
                                    this.pendingDiff = extracted.value;
                                }
                                text = extracted.ok ? extracted.value : (this.pendingDiff || '');
                            }
                            if (!this.monacoEditor) {
                                // Fallback <pre> is visible only until Monaco
                                // paints; create it on first sight and then
                                // only touch it when the text actually changes.
                                const pre = this.monacoHost.querySelector('.diff-fallback');
                                if (!pre || this.lastFallbackText !== text) {
                                    this.lastFallbackText = text;
                                    updateDiffFallback(this.monacoHost, text);
                                }
                            } else {
                                updateDiffEditor(this.monacoEditor, text);
                            }
                        } else if (this.argsStream) {
                            this.argsStream.textContent = this.rawArgs;
                        }
                        // Compact the header once the JSON is parseable enough.
                        const compact = formatArgsCompact(this.rawArgs);
                        if (compact && this.toolArgs && this.toolArgs.textContent !== compact) {
                            this.toolArgs.textContent = compact;
                            if (this.argsStream) {
                                this.argsStream.remove();
                                this.argsStream = null;
                            }
                        }
                        smartScroll();
                    });
                },
                setCollapsed: (c) => { toggle.classList.toggle('collapsed', c); },
            };
            return cardInfo;
        }

        // Cap for the live-output region on tool cards: mirrors the server's
        // tool-result cap (128 KB) so an unbounded `yes`-style command cannot
        // balloon the transcript DOM. The terminal dock still shows the full
        // log (its own scrollback cap).
        const TOOL_CARD_LIVE_OUTPUT_LIMIT = 128 * 1024;

        // Attaches the live-output region to a tool card (once). Shell tools
        // (execute_command, run_tests, run_lint) stream their stdout/stderr
        // here as the command runs — the same chunks that feed the terminal
        // dock — so the chat card shows the output live instead of only the
        // final tool_result at the end.
        function ensureToolCardLiveOutput(cardInfo) {
            if (!cardInfo) return null;
            if (cardInfo.liveOutput) return cardInfo.liveOutput;
            const live = document.createElement('div');
            live.className = 'tool-live-output';
            // Insert above the "executing..." row so the log reads top-down
            // with the status line beneath it.
            const waiting = cardInfo.card.querySelector('.tool-waiting');
            if (waiting) cardInfo.card.insertBefore(live, waiting);
            else cardInfo.card.appendChild(live);
            cardInfo.liveOutput = live;
            cardInfo.liveOutputText = '';
            cardInfo.liveOutputTruncated = false;
            return live;
        }

        // Appends a chunk of streamed tool output to the card's live region,
        // capping the accumulated text so a huge command cannot balloon the
        // transcript (the terminal dock keeps the full log).
        function appendToolCardLiveOutput(cardInfo, chunk) {
            if (!cardInfo || !chunk) return;
            const live = ensureToolCardLiveOutput(cardInfo);
            if (cardInfo.liveOutputTruncated) return;
            if (cardInfo.liveOutputText.length + chunk.length > TOOL_CARD_LIVE_OUTPUT_LIMIT) {
                cardInfo.liveOutputText += '\n… output truncated in card (see terminal tab for the full log)';
                cardInfo.liveOutputTruncated = true;
            } else {
                cardInfo.liveOutputText += chunk;
            }
            live.textContent = cardInfo.liveOutputText;
            // Keep the newest output in view, like a terminal.
            live.scrollTop = live.scrollHeight;
            scheduleRepinIfPinned();
        }

        // The chat diff viewer is either a static line-numbered <pre>
        // (default) or a full Monaco editor — a user setting
        // (gogen_chat_diff_viewer). Reads localStorage directly so it works
        // regardless of when the settings UI initializes.
        function diffViewerMode() {
            return localStorage.getItem('gogen_chat_diff_viewer') === 'monaco' ? 'monaco' : 'tokenizer';
        }

        function createDiffHost(cardInfo) {
            const host = document.createElement('div');
            host.className = 'monaco-tool-host' + (diffViewerMode() === 'tokenizer' ? ' diff-static' : '');
            cardInfo.monacoHost = host;
            return host;
        }

        async function ensurePatchViewer(cardInfo, diffText) {
            if (!cardInfo) return;
            if (diffText != null && diffText !== '') {
                cardInfo.pendingDiff = diffText;
            }
            if (!cardInfo.monacoHost) {
                // Upgrade a non-diff streaming card into a patch viewer.
                if (cardInfo.argsStream) {
                    cardInfo.argsStream.remove();
                    cardInfo.argsStream = null;
                }
                const host = createDiffHost(cardInfo);
                // Insert before waiting/status if present
                const waiting = cardInfo.card.querySelector('.tool-waiting');
                if (waiting) cardInfo.card.insertBefore(host, waiting);
                else cardInfo.card.appendChild(host);
            }
            updateDiffFallback(cardInfo.monacoHost, cardInfo.pendingDiff || '');

            if (diffViewerMode() === 'tokenizer') {
                // Static path: the fallback <pre> IS the viewer (line-numbered
                // rows, colored by diff line type). Streaming updates flow
                // through updateDiffFallback; no Monaco editor is created.
                return;
            }
            if (cardInfo.monacoEditor) {
                if (cardInfo.pendingDiff) updateDiffEditor(cardInfo.monacoEditor, cardInfo.pendingDiff);
                return;
            }
            if (cardInfo.monacoMounting) {
                await cardInfo.monacoMounting;
                if (cardInfo.monacoEditor && cardInfo.pendingDiff) {
                    updateDiffEditor(cardInfo.monacoEditor, cardInfo.pendingDiff);
                }
                return;
            }
            cardInfo.monacoMounting = mountDiffEditor(cardInfo.monacoHost, cardInfo.pendingDiff || '')
                .then((ed) => {
                    cardInfo.monacoEditor = ed;
                    if (ed && cardInfo.pendingDiff) updateDiffEditor(ed, cardInfo.pendingDiff);
                    return ed;
                })
                .catch((err) => {
                    console.warn('patch viewer monaco failed', err);
                    updateDiffFallback(cardInfo.monacoHost, cardInfo.pendingDiff || '');
                    return null;
                })
                .finally(() => {
                    cardInfo.monacoMounting = null;
                });
            await cardInfo.monacoMounting;
        }

        function startStreamingToolCard(index, name) {
            finalizeThinking();
            endStream();
            toolsStartedThisTurn = true;
            const cardInfo = createToolCard(name, {}, { streaming: true, streamIndex: index });
            streamingToolCards[index] = cardInfo;
            if (name === 'patch_file') {
                ensurePatchViewer(cardInfo, '').catch(() => {});
            }
            return cardInfo;
        }

        function appendStreamingToolArgs(index, argsDelta) {
            const cardInfo = streamingToolCards[index];
            if (!cardInfo) return;
            cardInfo.rawArgs = (cardInfo.rawArgs || '') + (argsDelta || '');
            bumpContextEstimate(argsDelta);

            const tool = cardInfo.toolName;
            if (cardInfo.monacoHost) {
                // Patch viewer already exists (patch_file cards get one at
                // start): all per-delta work is coalesced into one rAF pass.
                cardInfo.toolName = 'patch_file';
            } else {
                // Not a patch viewer yet. Pre-upgrade args are short, so a
                // precise "diff" key check per delta is cheap.
                const extracted = extractDiffValue(cardInfo.rawArgs);
                if (tool === 'patch_file' || extracted.ok) {
                    cardInfo.toolName = 'patch_file';
                    ensurePatchViewer(cardInfo, extracted.ok ? extracted.value : (cardInfo.pendingDiff || '')).catch(() => {});
                }
            }
            cardInfo.scheduleFrameUpdate();
        }

        function finalizeStreamingToolCard(index, id, name, args) {
            let cardInfo = streamingToolCards[index];
            if (!cardInfo) {
                cardInfo = createToolCard(name, args || {});
            } else {
                delete streamingToolCards[index];
                if (cardInfo.argsStream) {
                    cardInfo.argsStream.classList.remove('cursor');
                    cardInfo.argsStream.remove();
                    cardInfo.argsStream = null;
                }
                cardInfo.toolArgs.textContent = formatToolArgs(args);
                cardInfo.args = args || {};

                if (!cardInfo.waiting) {
                    const waiting = document.createElement('div');
                    waiting.className = 'tool-waiting';
                    waiting.textContent = 'executing...';
                    cardInfo.card.appendChild(waiting);
                    cardInfo.waiting = waiting;
                }
            }

            cardInfo.finalized = true;
            cardInfo.toolName = name || cardInfo.toolName;
            cardInfo.args = args || cardInfo.args || {};
            if (id) {
                pendingToolCards[id] = cardInfo;
            }

            // patch_file: the diff lives in args, not the later tool_result summary.
            if ((name === 'patch_file' || cardInfo.toolName === 'patch_file') && args && typeof args.diff === 'string') {
                ensurePatchViewer(cardInfo, args.diff).catch(() => {});
            }
            return cardInfo;
        }

        async function updateToolCardWithResult(cardInfo, result, success, truncated, toolName) {
            const { card, waiting } = cardInfo;
            cardInfo.finalized = true;
            if (waiting) waiting.remove();
            cardInfo.waiting = null;

            const body = document.createElement('div');
            body.className = 'tool-result-body';

            const statusBar = document.createElement('div');
            statusBar.className = `tool-status-bar ${success ? 'success' : 'error'}`;
            statusBar.textContent = success ? 'OK' : 'FAILED';
            body.appendChild(statusBar);

            const name = toolName || cardInfo.toolName || '';
            const argsDiff = cardInfo.args && typeof cardInfo.args.diff === 'string' ? cardInfo.args.diff : '';
            const pending = cardInfo.pendingDiff || argsDiff;

            try {
                if (name === 'patch_file') {
                    // Ensure the patch (from args) is visible; result is usually a short summary.
                    if (pending) {
                        ensurePatchViewer(cardInfo, pending).catch(() => {});
                    }
                    if (result) {
                        appendExpandableResult(body, result, success, truncated);
                    }
                    card.appendChild(body);
                } else if (name === 'show_diff') {
                    // Same viewer machinery as patch_file (mount guard, fallback
                    // sync, resize → gogen-colorized → scroll re-pin), so the
                    // card behaves identically to a patch card.
                    if (!cardInfo.monacoHost) {
                        cardInfo.monacoHost = createDiffHost(cardInfo);
                        body.appendChild(cardInfo.monacoHost);
                    }
                    card.appendChild(body);
                    ensurePatchViewer(cardInfo, result || '').catch(() => {});
                } else if (name === 'read_file') {
                    const path = readFilePathFromArgs(cardInfo.args);
                    appendExpandableResult(body, result, success, truncated, {
                        language: languageFromPath(path),
                    });
                    card.appendChild(body);
                } else if (cardInfo.liveOutput && cardInfo.liveOutputText) {
                    // The output already streamed into the card while the
                    // command ran; appending the final result again would
                    // duplicate it. Keep the streamed log as the result body
                    // (plus a copy affordance / truncation note).
                    appendStreamedResultBody(body, cardInfo);
                    card.appendChild(body);
                } else {
                    appendExpandableResult(body, result, success, truncated);
                    card.appendChild(body);
                }
            } finally {
                // Always re-sync the scroll position after the card update,
                // even if a viewer mount failed or rejected.
                smartScroll();
            }
        }

        // Builds the result body for a shell tool card whose output already
        // streamed in live (tool-live-output): the status bar was appended by
        // the caller, and here we add a copy affordance for the streamed text
        // plus a truncation note when the card cap was hit (the terminal dock
        // still holds the full log).
        function appendStreamedResultBody(body, cardInfo) {
            const liveText = cardInfo.liveOutputText;
            const copyBtn = document.createElement('button');
            copyBtn.className = 'tool-result-copy';
            copyBtn.textContent = 'Copy';
            copyBtn.onclick = () => {
                navigator.clipboard.writeText(liveText).then(() => showToast('Copied', 'success')).catch(() => showToast('Copy failed', 'error'));
            };
            body.appendChild(copyBtn);
            if (cardInfo.liveOutputTruncated) {
                const note = document.createElement('div');
                note.className = 'tool-live-truncated';
                note.textContent = 'Output truncated in card; open the terminal tab for the full log.';
                body.appendChild(note);
            }
        }

        // A history replay rebuilds the whole pane with awaits (Monaco mounts,
        // colorize, image loads). If one of those awaits never settles,
        // replayInProgress stays true forever and every smartScroll() becomes a
        // no-op: the view silently stops following new messages. The watchdog
        // clears the flag and pins so the UI recovers even if a restore hangs.
        const REPLAY_WATCHDOG_MS = 20000;
        let replayWatchdog = null;
        function armReplayWatchdog() {
            clearTimeout(replayWatchdog);
            replayWatchdog = setTimeout(() => {
                replayWatchdog = null;
                if (!replayInProgress) return;
                console.warn('history replay watchdog fired; re-enabling scroll follow');
                replayInProgress = false;
                flushDeferredResultColorize();
                pinToBottom();
            }, REPLAY_WATCHDOG_MS);
        }
        function disarmReplayWatchdog() {
            clearTimeout(replayWatchdog);
            replayWatchdog = null;
        }

        async function replayHistory(history) {
            // Session/history loads should always land at the bottom.
            enableFollow();
            historyToolCallArgs = {};
            // Self-contained: a history snapshot replaces the whole pane, so
            // reset leftover tool-card state rather than relying on the caller
            // having run clearChat() first.
            streamingToolCards = {};
            pendingToolCards = {};
            toolsStartedThisTurn = false;
            // Suppress per-message smartScroll while rebuilding; the final
            // pinToBottom() below does a single scroll pass instead of O(n)
            // forced layouts. The history handler's error fallback also resets
            // this so a failed replay still scrolls while re-appending.
            replayInProgress = true;
            armReplayWatchdog();
            function msgDate(createdAt) {
                return createdAt ? new Date(createdAt) : new Date();
            }
            for (const h of history) {
                if (h.role === 'user') {
                    appendMessageAtTime('user', h.content || '', msgDate(h.createdAt), h.index, h.images);
                } else if (h.role === 'assistant') {
                    // Skip when reasoning was promoted into content (same text).
                    if (h.reasoning && h.reasoning !== h.content) {
                        const div = document.createElement('div');
                        div.className = 'thought-card';
                        const header = document.createElement('div');
                        header.style.cssText = 'display:flex;align-items:center;gap:6px;cursor:pointer;user-select:none;font-size:0.85em;color:var(--fg-muted);padding:2px 0;';
                        const label = document.createElement('span');
                        label.style.fontStyle = 'italic';
                        label.textContent = 'Thinking';
                        const toggle = document.createElement('span');
                        toggle.textContent = '▾';
                        toggle.style.fontSize = '0.8em';
                        header.appendChild(label);
                        header.appendChild(toggle);
                        const body = document.createElement('div');
                        body.style.cssText = 'font-size:0.85em;padding:4px 0 0 0;';
                        // Same live-style markdown render as streaming thinking.
                        setMessageMarkdown(body, h.reasoning || '');
                        div.appendChild(header);
                        div.appendChild(body);
                        header.addEventListener('click', () => {
                            const collapsed = body.style.display === 'none';
                            body.style.display = collapsed ? '' : 'none';
                            toggle.textContent = collapsed ? '▾' : '▸';
                        });
                        if (h.createdAt) {
                            div.title = formatExactTime(msgDate(h.createdAt));
                            div.dataset.createdAt = h.createdAt;
                        }
                        messagesDiv.appendChild(div);
                        smartScroll();
                    }
                    // Render refusal text through the normal assistant bubble
                    // when there is no content (OpenAI-style refusals).
                    const assistantText = h.content || h.refusal;
                    if (assistantText) appendMessageAtTime('assistant', assistantText, msgDate(h.createdAt), h.index);
                    if (Array.isArray(h.toolCalls)) {
                        for (const tc of h.toolCalls) {
                            historyToolCallArgs[tc.id] = tc.args || {};
                            const cardInfo = createToolCard(tc.name, tc.args || {});
                            cardInfo.toolName = tc.name;
                            cardInfo.args = tc.args || {};
                            if (tc.name === 'patch_file' && tc.args && typeof tc.args.diff === 'string') {
                                if (cardInfo.waiting) {
                                    cardInfo.waiting.remove();
                                    cardInfo.waiting = null;
                                }
                                // NOT awaited: the replay loop must stay
                                // synchronous. ensurePatchViewer renders the
                                // static fallback immediately and mounts the
                                // Monaco diff editor in the background; an
                                // await here would yield to the event loop
                                // mid-rebuild, letting live stream events and
                                // other panes' messages interleave with the
                                // transcript (garbled or "never loads").
                                ensurePatchViewer(cardInfo, tc.args.diff).catch(() => {});
                            }
                            if (tc.id) pendingToolCards[tc.id] = cardInfo;
                        }
                    }
                } else if (h.role === 'tool') {
                    const id = h.toolCallId;
                    let cardInfo = id ? pendingToolCards[id] : null;
                    const args = (id && historyToolCallArgs[id]) || {};
                    const name = cardInfo ? cardInfo.toolName : '';
                    if (!cardInfo) {
                        // Orphan tool result — create a minimal card
                        cardInfo = createToolCard(name || 'tool', args);
                        cardInfo.toolName = name || 'tool';
                        cardInfo.args = args;
                    }
                    if (id) delete pendingToolCards[id];
                    const success = !String(h.content || '').trim().startsWith('Error:');
                    updateToolCardWithResult(cardInfo, h.content || '', success, false, cardInfo.toolName);
                }
            }
            replayInProgress = false;
            disarmReplayWatchdog();
            flushDeferredResultColorize();
            // Pin twice: once immediately, then again after a frame to catch
            // late DOM mutations (Monaco colorization, image loads, etc.).
            pinToBottom();
            requestAnimationFrame(() => {
                pinToBottom();
                // Dispatch event so the gogen-colorized handler re-scrolls if
                // Monaco finishes coloring after this rAF.
                window.dispatchEvent(
                    new CustomEvent('gogen-colorized', { bubbles: false })
                );
            });
        }

        // ===== Terminal panel =====
        // A docked strip at the bottom of the page (collapsed by default) with
        // terminal tabs: a pinned interactive "User" shell (the default
        // terminal) plus one read-only tab per agent command (execute_command,
        // run_tests, run_lint) streaming live output. On mobile the dock is
        // replaced by the "Terminal" header tab (full-screen).
        const terminalPanel = document.getElementById('terminal-panel');
        const terminalBody = document.getElementById('terminal-body');
        const terminalTabsEl = document.getElementById('terminal-tabs');
        const terminalChevron = document.getElementById('terminal-chevron');
        const terminalResizeHandle = document.getElementById('terminal-resize-handle');
        const terminalTabBtn = document.getElementById('terminal-tab');
        const terminalTabBadge = document.getElementById('terminal-tab-badge');
        const terminalHint = document.getElementById('terminal-hint');
        const terminalMoreBtn = document.getElementById('terminal-more');
        const terminalMoreMenu = document.getElementById('terminal-more-menu');

        const TERM_STORE_KEY = 'gogen.terminalPanel.v1';
        const TERM_MAX_TABS = 8;
        const TERM_MIN_HEIGHT = 120;
        const TERM_MAX_HEIGHT_RATIO = 0.7;
        const TERM_DEFAULT_HEIGHT = 280;
        const USER_TERM_ID = 'user';

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
        function terminalFitSoon() {
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
                    if (userTermState === 'ready' && ws && ws.readyState === WebSocket.OPEN) {
                        ws.send(JSON.stringify({ type: 'user_term_input', content: d }));
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
        function terminalOpen(id, name, title) {
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
        function terminalEnsureUserTab() {
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

        function terminalUserOpened(title, wd) {
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

        function terminalUserExited(code) {
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
            if (ws && ws.readyState === WebSocket.OPEN) {
                ws.send(JSON.stringify({ type: 'user_term_request' }));
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
                if (dims && dims.cols > 0 && dims.rows > 0 && ws && ws.readyState === WebSocket.OPEN) {
                    ws.send(JSON.stringify({
                        type: 'user_term_resize',
                        cols: Math.max(2, Math.round(dims.cols)),
                        rows: Math.max(2, Math.round(dims.rows)),
                    }));
                }
            } catch (_) {}
        }

        function terminalWrite(id, chunk) {
            const t = terminals.get(id);
            if (!t || !chunk || t.done) return;
            try { t.term.write(chunk); } catch (_) {}
        }

        function terminalExit(id, success) {
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
        function terminalInterruptAll() {
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

        // ===== Terminal panel: expand/collapse + resize =====
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

        function terminalToggleMobile() {
            if (terminalPanel.classList.contains('mobile-full')) {
                terminalCloseMobile();
                if (terminalExpanded) terminalSetExpanded(false);
            } else {
                terminalOpenMobile();
            }
        }

        function initTerminal() {
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
        }
        try {
            initTerminal();
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

        function connect() {
            const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
            ws = new WebSocket(`${protocol}//${window.location.host}/ws`);

            ws.onopen = () => {
                clearTimeout(reconnectTimer);
                reconnectTimer = null;
                setConnState('connected');
                // Replace (don't append) on every connect: the server may omit
                // history when the session is empty, and reconnect must not
                // keep a stale transcript. Connection status lives in the
                // indicator, not as chat system messages.
                clearChat();
                // Reset the lazy model-catalog guard so a fresh connection can fetch again.
                modelsRequested = false;
                // Only request the cheap, local session list eagerly. The
                // model catalog is a remote /v1/models call that can dominate
                // startup latency, so fetch it after first paint (idle).
                ws.send(JSON.stringify({ type: 'list_sessions' }));
                const scheduleModels = window.requestIdleCallback
                    || ((fn) => setTimeout(fn, 200));
                scheduleModels(() => ensureModelsLoaded());

                // Re-attach every open pane: the server resends
                // session_state + history + config + context per pane. The
                // active pane's transcript rebuilds from its attach response;
                // background panes just re-register (their transcript
                // re-derives when focused). Panes with no session yet (first
                // load) get their state from the connect handshake.
                //
                // The server re-points its per-connection "current pane" at
                // EVERY attach, so the ACTIVE pane is attached LAST:
                // otherwise the pointer would land on the last-inserted pane
                // and messages typed in the active pane would route to the
                // wrong session until the next attach.
                const activeP = activePane();
                for (const pane of panes.values()) {
                    if (pane.id && pane !== activeP) {
                        ws.send(JSON.stringify({ type: 'session_attach', sessionId: pane.id }));
                    }
                }
                if (activeP && activeP.id) {
                    ws.send(JSON.stringify({ type: 'session_attach', sessionId: activeP.id }));
                }
                // Panes whose session creation never completed before the
                // disconnect have no id yet: re-issue session_new so they
                // get a real session instead of sitting at "creating…"
                // forever (or being adopted onto the handshake's default
                // session). Only on a RECONNECT — on first load the initial
                // pane (also id-less) must adopt the server's default session
                // from the connect handshake, not create a fresh one.
                if (wasDisconnected) {
                    for (const pane of panes.values()) {
                        if (!pane.id) {
                            pane.pendingSessionResponse = true;
                            if (pane === activeP) pendingSessionResponse = true;
                            ws.send(JSON.stringify({ type: 'session_new' }));
                        }
                    }
                }

                // Notify on reconnection after a disconnect
                if (wasDisconnected) {
                    wasDisconnected = false;
                    sendNotification('GoGen', 'Reconnected to server.', 'gogen-reconnect');
                }
            };

            ws.onmessage = (event) => {
                let data;
                try {
                    data = JSON.parse(event.data);
                } catch (err) {
                    console.error('Failed to parse server message:', err, 'raw:', event.data.slice(0, 200));
                    return;
                }
                // Editor responses (requestId-keyed) arrive on the separate
                // /ws/editor socket handled by editor.js; the chat socket
                // only ever carries chat/session/terminal messages.
                // Multi-pane routing: connection-scoped messages (no
                // sessionId) and messages for the active pane run the normal
                // handler below; background-pane messages update that pane's
                // state only (the transcript re-derives when focused).
                const msgPane = paneForMessage(data);
                if (!msgPane) return; // stale message for a closed session
                if (msgPane !== activePane()) {
                    handleBackgroundMessage(msgPane, data);
                    return;
                }
                if (data.type === 'compacting') {
                    // Auto-compaction inside a normal turn (or the manual
                    // /compact command): keep a live indicator until the next
                    // progress event (thinking/stream/tool) or the compact
                    // response replaces it.
                    setInputProgress('compacting', 'Compacting context\u2026');
                } else if (data.type === 'thinking') {
                    setTurnActive(true);
                    updateTitle('thinking');
                    // New model round (OnStart / OnRoundStart): clear per-round
                    // tool state so reasoning shows again, matching TUI.
                    toolsStartedThisTurn = false;
                    streamingToolCards = {};
                    finalizeThinking();
                    setInputProgress('thinking', 'Thinking\u2026');
                } else if (data.type === 'waiting') {
                    setTurnActive(true);
                    setInputProgress('thinking', 'Waiting for model\u2026');
                } else if (data.type === 'thinking_token') {
                    setTurnActive(true);
                    appendThinkingToken(data.content || '');
                } else if (data.type === 'stream') {
                    setTurnActive(true);
                    updateTitle('streaming');
                    setInputProgress('streaming', 'Streaming\u2026');
                    finalizeThinking();
                    if (!currentStreamDiv) startStream();
                    appendStreamToken(data.content);
                } else if (data.type === 'stream_end') {
                    finalizeThinking();
                    endStream();
                } else if (data.type === 'tool_call_start') {
                    setTurnActive(true);
                    setInputProgress('tool', 'Running ' + (data.tool || 'tool') + '\u2026');
                    startStreamingToolCard(data.index, data.tool);
                } else if (data.type === 'tool_call_delta') {
                    finalizeThinking();
                    if (!streamingToolCards[data.index]) {
                        startStreamingToolCard(data.index, data.tool || 'tool');
                    } else if (data.tool) {
                        streamingToolCards[data.index].toolName = data.tool;
                    }
                    appendStreamingToolArgs(data.index, data.argsDelta || '');
                } else if (data.type === 'tool_call') {
                    finalizeThinking();
                    endStream();
                    toolsStartedThisTurn = true;
                    finalizeStreamingToolCard(data.index, data.toolCallId, data.tool, data.args || {});
                } else if (data.type === 'tool_execute') {
                    // Update waiting text on the matching pending card if present
                    const name = data.tool || '';
                    setInputProgress('tool', name ? ('Running ' + name + '\u2026') : 'Running tool\u2026');
                    for (const id of Object.keys(pendingToolCards)) {
                        const cardInfo = pendingToolCards[id];
                        if (cardInfo && cardInfo.waiting && (!name || cardInfo.toolName === name)) {
                            cardInfo.waiting.textContent = name ? `running ${name}...` : 'executing...';
                        }
                    }
                } else if (data.type === 'tool_result') {
                    finalizeThinking();
                    // Account for tool result tokens in the live context estimate.
                    bumpContextEstimate(data.result);
                    // Stay busy until turn_end — more rounds may follow.
                    const cardInfo = pendingToolCards[data.toolCallId];
                    if (cardInfo) {
                        updateToolCardWithResult(
                            cardInfo,
                            data.result,
                            // Server serializes Success with omitempty, so a
                            // failed tool omits the field entirely (undefined).
                            // Only an explicit true means success.
                            data.success === true,
                            data.resultTruncated,
                            data.tool || cardInfo.toolName
                        );
                        delete pendingToolCards[data.toolCallId];
                    } else {
                        appendMessage('system', `[${data.tool}] ${data.result}`);
                    }
                } else if (data.type === 'term_opened') {
                    terminalOpen(data.termId, data.tool || '', data.content || '');
                    // Mirror the command echo into the pending tool card so
                    // shell output streams into the chat, not just the
                    // (collapsed-by-default) terminal dock.
                    const cardInfo = pendingToolCards[data.termId];
                    if (cardInfo && data.content) {
                        // Echo the command like the terminal tab does, on its
                        // own line ("$ ..." then the streaming output).
                        appendToolCardLiveOutput(cardInfo, data.content + '\n');
                    }
                } else if (data.type === 'term_output') {
                    terminalWrite(data.termId, data.content || '');
                    const cardInfo = pendingToolCards[data.termId];
                    if (cardInfo) {
                        appendToolCardLiveOutput(cardInfo, data.content || '');
                    }
                } else if (data.type === 'term_exit') {
                    // Success is omitted when false, so only an explicit true
                    // marks the tab as ok; anything else is a failure.
                    terminalExit(data.termId, data.success === true);
                    terminalFitSoon();
                    const cardInfo = pendingToolCards[data.termId];
                    if (cardInfo && cardInfo.liveOutput) {
                        cardInfo.liveOutput.classList.add('done');
                        cardInfo.liveOutput.classList.add(data.success === true ? 'success' : 'error');
                    }
                } else if (data.type === 'user_term_opened') {
                    terminalUserOpened(data.content || 'shell', data.workingDir || '');
                } else if (data.type === 'user_term_output') {
                    terminalEnsureUserTab();
                    terminalWrite(USER_TERM_ID, data.content || '');
                } else if (data.type === 'user_term_exit') {
                    terminalUserExited(data.code || 0);
                } else if (data.type === 'cancelled') {
                    // Stale cancel from a turn we replaced with resend/send — ignore.
                    if (suppressTurnEnds > 0) return;
                    abortInFlightUI(data.content || 'Cancelled.');
                    setTurnActive(false);
                    setInputProgress(null);
                    refreshSidebarSessions();
                } else if (data.type === 'turn_end') {
                    // Ignore turn_end from a turn we cancelled to start a resend/send.
                    // Otherwise it clears pendingSessionResponse / turnActive mid-flow.
                    if (suppressTurnEnds > 0) {
                        suppressTurnEnds--;
                        return;
                    }
                    // A turn we deliberately left behind when resuming another
                    // session: the old session keeps running headless (resume
                    // does not cancel it), so its turn_end must not clear the
                    // pending flag or block the resumed session's convergence.
                    // (Before the reply re-keys the pane, the old session is
                    // still this pane's id; after the re-key, paneForMessage
                    // drops the old id entirely.)
                    const pane = activePane();
                    if (pane && pane.ignoreTurnEndsFor && data.sessionId === pane.ignoreTurnEndsFor) {
                        pane.ignoreTurnEndsFor = null;
                        return;
                    }
                    setTurnActive(false);
                    updateTitle('idle');
                    setInputProgress(null);
                    pendingSessionResponse = false;
                    // A pane attached mid-turn never received the in-flight
                    // reply (assistant messages are appended only
                    // when a round completes). Now that the turn is done the
                    // full history is available — re-attach once so the
                    // transcript converges.
                    const tp = pane;
                    if (tp && tp.needsFreshHistory) {
                        tp.needsFreshHistory = false;
                        if (tp.id && ws && ws.readyState === WebSocket.OPEN) {
                            ws.send(JSON.stringify({ type: 'session_attach', sessionId: tp.id }));
                        }
                    }
                    refreshSidebarSessions();
                    // The turn just finished: fetch a fresh sessions payload
                    // instead of re-rendering the cached one, so the sidebar's
                    // saved-section order and per-row relative times reflect
                    // the completed turn's new Updated timestamp (the cache is
                    // only refreshed on explicit list_sessions round-trips).
                    requestSessionList();
                } else if (data.type === 'clear_chat') {
                    clearChat();
                    // Do NOT re-arm pendingSessionResponse here: the response
                    // handler already re-keyed the pane to the new session id
                    // and cleared the flag, so the follow-up history/config
                    // route by session id without the fallback. Re-arming left
                    // the flag stuck true until the next turn_end / reconnect,
                    // which permanently blocked resumeSession()/deleteSession()
                    // ("Session change already in progress") and made
                    // paneForMessage route stale messages to the active pane
                    // (which then wrongly re-keyed it).
                } else if (data.type === 'user_acked') {
                    // Server assigned the Messages index for the user turn just started.
                    // Index is in content so 0 is not dropped by JSON omitempty.
                    const idx = parseInt(data.content, 10);
                    const el = messagesDiv.querySelector('.message.user[data-pending-ack="1"]')
                        || [...messagesDiv.querySelectorAll('.message.user:not([data-hist-idx])')].at(-1);
                    if (el && Number.isFinite(idx) && idx >= 0) {
                        delete el.dataset.pendingAck;
                        el.dataset.histIdx = String(idx);
                        ensureUserResendActions(el);
                    }
                } else if (data.type === 'sessions') {
                    // Structured list only — never dump slash help text into chat.
                    // Deliberately do NOT finalize/endStream here: the sessions
                    // payload is sidebar metadata that can arrive while the
                    // active pane's turn is still streaming (e.g. closing a
                    // background pane's ✕ calls requestSessionList mid-turn).
                    // Finalizing would split the in-flight reply into a second
                    // assistant bubble and stamp a fork button on a partial one.
                    renderSessionList(data.sessions || []);
                    if (data.contextLimit || data.usedTokens) { updateContextInfo(data); }
                } else if (data.type === 'session_state') {
                    // Attach reply: tells us whether a headless turn is
                    // running so we can render "resuming…". It also carries
                    // the session id: adopt it when the pane does not have
                    // one yet, so the pane is usable immediately (routing,
                    // send guard, sidebar "open" row) even while a running
                    // turn keeps the config echo — which normally adopts the
                    // id — pending until the turn ends.
                    const pane = activePane();
                    if (data.sessionId && pane.id === null) {
                        pane.id = data.sessionId;
                    }
                    pane.turnActive = !!data.turnActive;
                    setTurnActive(pane.turnActive, { silent: true });
                    if (pane.turnActive) {
                        setInputProgress('thinking', 'Resuming\u2026');
                        // Attached to a running turn: the history snapshot
                        // cannot contain the in-flight reply, so latch the
                        // turn-end refetch that converges the transcript.
                        pane.needsFreshHistory = true;
                    } else {
                        // Idle attach: the history snapshot is complete — no
                        // convergence refetch needed. Also clears a latch
                        // left over from an earlier busy attach whose turn_end
                        // was already consumed (e.g. focus after the turn
                        // ended).
                        pane.needsFreshHistory = false;
                    }
                    // Reflect the busy/resuming state in the sidebar session
                    // row (same staleness class: the row shows turn state).
                    refreshSidebarSessions();
                } else if (data.type === 'session_removed') {
                    // The session no longer exists (deleted elsewhere, or an
                    // attach failed after a restart with pruning). Close its
                    // pane. For the ACTIVE pane: when THIS connection
                    // initiated the delete, the server re-keys the pane via
                    // the follow-up config (pendingSessionResponse is set);
                    // when ANOTHER connection deleted it, no follow-up comes
                    // — start a fresh session so the pane is not orphaned on
                    // a deleted runtime (messages would route to a stale
                    // agent and silently re-persist the deleted file).
                    const pane = findPaneBySession(data.sessionId);
                    if (pane === activePane() && !pendingSessionResponse) {
                        // Drop the stale pane FIRST (mirrors session_detached):
                        // leaving it in the map rendered a ghost sidebar row for
                        // the deleted session, and clicking it re-attached the
                        // deleted id — the server replied session_removed again,
                        // spawning another fresh session (and another ghost) per
                        // click. Its state is discarded with the session, so no
                        // saveActivePaneState is needed.
                        panes.delete(pane.key);
                        const p = makePane();
                        activePaneKey = p.key;
                        clearChat();
                        loadActivePaneState();
                        if (ws && ws.readyState === WebSocket.OPEN) {
                            pendingSessionResponse = true;
                            ws.send(JSON.stringify({ type: 'session_new' }));
                        }
                    } else if (pane && pane !== activePane()) {
                        panes.delete(pane.key);
                        refreshSidebarSessions();
                    }
                    requestSessionList();
                } else if (data.type === 'session_detached') {
                    // The runtime is gone: the registry evicted this session
                    // (web_max_active_sessions cap), or it was
                    // explicitly closed (session_close) while this tab raced
                    // to attach. It is saved, not deleted: close the pane
                    // like a removal — the session stays in the saved-
                    // sessions list and can be reopened (re-attach reloads
                    // it from the store). Closing avoids sending messages to
                    // a runtime that is no longer registered.
                    const pane = findPaneBySession(data.sessionId);
                    if (pane) {
                        panes.delete(pane.key);
                        if (activePaneKey === pane.key) {
                            if (panes.size === 0) {
                                const p = makePane();
                                activePaneKey = p.key;
                                clearChat();
                                loadActivePaneState();
                                if (ws && ws.readyState === WebSocket.OPEN) {
                                    pendingSessionResponse = true;
                                    ws.send(JSON.stringify({ type: 'session_new' }));
                                }
                            } else {
                                focusPane(panes.keys().next().value);
                            }
                        } else {
                            refreshSidebarSessions();
                        }
                    }
                    requestSessionList();
                } else if (data.type === 'response') {
                    finalizeThinking();
                    endStream();
                    // A session command (typed /new, /resume, /fork, resend)
                    // replies with the NEW session id on the response message
                    // — the first message of the change, before clear_chat/
                    // history/config arrive. Adopt it here so those follow-up
                    // messages route to this pane (its id is otherwise still
                    // the old session, and paneForMessage would drop them).
                    // Sidebar-new and resend already set pendingSessionResponse
                    // and go through this same path.
                    const rp = activePane();
                    let rekeySkipped = false;
                    if (rp && data.sessionId && rp.id !== data.sessionId) {
                        // A session change (typed /new, /resume, /fork,
                        // resend, delete-of-current) is the only legitimate
                        // reason to adopt a different id here — every such
                        // reply carries a sessionAction. A plain response
                        // with a different id (e.g. a delete whose reply was
                        // tagged with a stale server-side pane) must not
                        // hijack this pane onto another session. Also refuse
                        // to adopt an id another pane already holds (e.g.
                        // "resume latest" resolving to a session open
                        // elsewhere): that would create a duplicate pane for
                        // one session and desync one of the two.
                        const other = findPaneBySession(data.sessionId);
                        if ((rp.id === null || data.sessionAction) && (!other || other === rp)) {
                            const oldId = rp.id;
                            rp.id = data.sessionId;
                            refreshSidebarSessions();
                            // Release the old session's attachment only when
                            // no other pane still has it open: detaching a
                            // session a background pane still shows would
                            // orphan that pane's event stream.
                            if (oldId && !findPaneBySession(oldId) && ws && ws.readyState === WebSocket.OPEN) {
                                ws.send(JSON.stringify({ type: 'session_detach', sessionId: oldId }));
                            }
                        } else {
                            rekeySkipped = true;
                        }
                    }
                    // Whether the change succeeded or failed, the pane is (or
                    // stays) on a known session: from here its own turn_end
                    // must be processed normally. (On success the old
                    // session's id no longer routes here; on failure it is
                    // still this pane's session and its turn_end matters.)
                    // needsFreshHistory is deliberately NOT reset here — the
                    // session_state sent before the reply owns the latch, and
                    // a resumed session may be mid-turn and needs the
                    // turn-end convergence refetch.
                    rp.ignoreTurnEndsFor = null;
                    if (compacting) {
                        // Terminal event for the manual /compact command:
                        // clear the persistent indicator and surface the
                        // outcome (the context message that follows will
                        // refresh the badge/banner).
                        compacting = false;
                        setInputProgress(null);
                        const text = data.content || '';
                        if (String(text).startsWith('Error:')) {
                            showToast(text, 'error');
                        } else {
                            showToast(text, 'success');
                        }
                        appendMessage('system', text);
                    } else if (pendingSessionResponse) {
                        pendingSessionResponse = false;
                        nearCompactDismissed = false; // new session: allow the banner again
                        // Only track the session id when the pane actually
                        // adopted it (a skipped re-key keeps the old id).
                        if (data.sessionId && rp.id === data.sessionId) sessionInfoDiv.textContent = data.sessionId;
                        updateContextInfo(data);
                        const msg = (data.content || '').split('\n')[0] || 'Session updated';
                        if (rekeySkipped) {
                            // The reply named a session another pane already
                            // owns: this pane stays on its current session.
                            showToast('Session already open in another pane', 'info');
                            requestSessionList();
                        } else if (String(data.content || '').startsWith('Error:')) {
                            clearPendingResend();
                            showToast(data.content, 'error');
                            appendMessage('system', data.content);
                        } else if (!resendAwaitingHistory) {
                            // Normal session switch — toast. Resend follow-up
                            // suppresses the toast; flush happens after history.
                            showToast(msg, 'success');
                            requestSessionList();
                        } else {
                            requestSessionList();
                        }
                    } else if (data.content && String(data.content).startsWith('Error:')) {
                        showToast(data.content, 'error');
                        appendMessage('system', data.content);
                    } else {
                        appendMessage('assistant', data.content);
                    }
                } else if (data.type === 'models') {
                    if (data.content) {
                        finalizeThinking();
                        endStream();
                        appendMessage('assistant', data.content);
                    }
                    if (data.models) {
                        updateModelSelect(data.models, data.model);
                    } else if (data.model) {
                        updateModelInfo(data.model);
                    }
                    // list_models replies omit context stats — don't wipe the indicator
                    if (data.contextLimit || data.usedTokens || data.usedSource) {
                        updateContextInfo(data);
                    }
                } else if (data.type === 'history') {
                    // Full snapshot — replace the pane so reconnect / session
                    // restore never stacks duplicate transcripts. One guard:
                    // the turn_end convergence refetch can race a new turn
                    // the user started at the boundary — its snapshot
                    // is OLDER than the live transcript, so skip the rebuild
                    // (a fresh attach latches needsFreshHistory via its
                    // session_state first; a stale refetch does not).
                    const histPane = activePane();
                    if (histPane.needsFreshHistory || !turnActive) {
                        // Mid-turn attach: the snapshot is INCOMPLETE — it
                        // lacks the in-flight reply — and live events for
                        // that reply may already be rendering. The attach's
                        // history is sent from an async goroutine after a
                        // deep-cloned snapshot (SnapshotMessages under
                        // statsMu); for a large session it can arrive AFTER
                        // the first stream batches, so the DOM already shows
                        // the reply's beginning. Rebuilding from the
                        // snapshot would WIPE that visible reply — the turn
                        // seems to stop until the turn_end refetch. Keep the
                        // live content instead; the turn_end convergence
                        // refetch paints the full transcript.
                        if (histPane.needsFreshHistory
                            && (currentStreamDiv
                                || currentThinkingDiv
                                || Object.keys(streamingToolCards).length > 0)) {
                            return;
                        }
                        clearChat();
                        const afterHistory = () => {
                            // A headless turn may be running (session_state said
                            // turnActive): restore the busy indicator now that the
                            // transcript is rebuilt (clearChat reset it).
                            const pane = activePane();
                            if (pane && pane.turnActive) {
                                setTurnActive(true, { silent: true });
                                setInputProgress('thinking', 'Resuming\u2026');
                            }
                            // NOTE: the resend message is flushed from the config
                            // handler, which adopts the new session id first (the
                            // server sends history before config).
                        };
                        if (data.history && data.history.length) {
                            replayHistory(data.history).then(afterHistory).catch((err) => {
                                console.warn('history replay failed', err);
                                // Ensure the replay scroll-suppression flag is off
                                // so the fallback re-append below scrolls normally.
                                replayInProgress = false;
                                disarmReplayWatchdog();
                                flushDeferredResultColorize();
                                for (const h of data.history) {
                                    if (h.role === 'user' || h.role === 'assistant') {
                                        if (h.content) {
                                            appendMessageAtTime(
                                                h.role,
                                                h.content,
                                                h.createdAt ? new Date(h.createdAt) : new Date(),
                                                h.index,
                                                h.images
                                            );
                                        }
                                    }
                                }
                                afterHistory();
                            });
                        } else {
                            afterHistory();
                        }
                    }
                } else if (data.type === 'config') {
                    // The pane's session identity may have changed (new /
                    // resume / fork / attach): adopt the new id and keep the
                    // per-session toolbar mirrors current.
                    const pane = activePane();
                    const oldId = pane.id;
                    if (data.sessionId && pane.id !== data.sessionId) {
                        // Re-key ONLY when the pane has no id yet (startup /
                        // fresh pane) or a session change is genuinely in
                        // flight (typed /new, /resume, /fork, resend). A
                        // stale full config from a pane we left/closed (its
                        // ContextStats tokenization lags a running turn) must
                        // not flip the active pane's id — paneForMessage
                        // would then drop every message for the session the
                        // user switched to ("can't switch to the responding
                        // session").
                        if (pane.id === null || pendingSessionResponse) {
                            pane.id = data.sessionId;
                            pane.needsFreshHistory = false; // new session: no mid-turn gap from the old one
                            refreshSidebarSessions();
                            // The pane's session changed (typed /new, /resume,
                            // fork, resend): release the old session's
                            // attachment — the server keeps panes attached
                            // until explicitly detached, and the old session
                            // is no longer open. Only when no other pane
                            // still has the old session open (detaching it
                            // would orphan that pane's event stream).
                            if (oldId && !findPaneBySession(oldId) && ws && ws.readyState === WebSocket.OPEN) {
                                ws.send(JSON.stringify({ type: 'session_detach', sessionId: oldId }));
                            }
                        }
                    }
                    pane.mode = data.mode || pane.mode;
                    pane.thinkingLevel = data.thinkingLevel || pane.thinkingLevel;
                    pane.model = data.model || pane.model;
                    if (data.sessionLabel) pane.label = data.sessionLabel;
                    applyServerConfig(data);
                    // The resend message must carry the (possibly new) session
                    // id, which config just adopted — flushing from history
                    // would use the stale pre-fork id.
                    if (resendAwaitingHistory) flushPendingResend();
                } else if (data.type === 'context') {
                    updateContextInfo(data);
                    updateCurrentSessionLabel(data.sessionLabel);
                } else if (data.type === 'delete_approval') {
                    showDeleteApproval(data);
                }
            };

            ws.onclose = () => {
                setConnState('disconnected');
                endStream();
                streamingToolCards = {};
                pendingToolCards = {};
                clearPendingResend();
                suppressTurnEnds = 0;
                pendingSessionResponse = false;
                compacting = false;
                setTurnActive(false);
                // Reset per-pane in-flight state; the re-attach on reconnect
                // re-syncs each pane from the server (session_state).
                for (const pane of panes.values()) {
                    pane.turnActive = false;
                    pane.needsFreshHistory = false;
                    // A resume that never completed (connection dropped
                    // before the reply) must not leave a stale ignore flag:
                    // after reconnect this pane's own turn_end must count.
                    pane.ignoreTurnEndsFor = null;
                    pane.suppressTurnEnds = 0;
                    pane.pendingSessionResponse = false;
                    pane.pendingResendContent = null;
                    pane.resendAwaitingHistory = false;
                    pane.compacting = false;
                    pane._notifiedBusy = false;
                }
                terminalInterruptAll();
                setInputProgress(null);
                clearTimeout(reconnectTimer);
                setConnState('reconnecting');
                wasDisconnected = true;
                reconnectTimer = setTimeout(connect, 3000);
            };
        }

        // ── Image attachments (vision input) ──
        // Mirrors the server's limits (internal/server/server.go
        // validateImageInputs).
        const MAX_ATTACHMENTS = 4;
        const MAX_ATTACHMENT_BYTES = 5 * 1024 * 1024;
        let pendingAttachments = []; // [{dataUrl, name}]

        function addImageAttachment(file) {
            if (!file) return;
            if (!file.type || !file.type.startsWith('image/')) return;
            if (file.size > MAX_ATTACHMENT_BYTES) {
                showToast(`Image "${file.name}" is larger than 5 MB`, 'error');
                return;
            }
            if (pendingAttachments.length >= MAX_ATTACHMENTS) {
                showToast(`Max ${MAX_ATTACHMENTS} images per message`, 'error');
                return;
            }
            const reader = new FileReader();
            reader.onload = () => {
                const dataUrl = String(reader.result || '');
                if (!dataUrl.startsWith('data:image/')) {
                    showToast(`"${file.name}" is not a supported image`, 'error');
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

        function clearAttachments() {
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
                remove.textContent = '✕';
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

        function sendMessage() {
            const text = inputArea.value.trim();
            if (!text && pendingAttachments.length === 0) return;
            if (!ws || ws.readyState !== WebSocket.OPEN) {
                appendMessage('system', 'Not connected — wait for reconnection.');
                return;
            }
            if (!activePane() || !activePane().id) {
                showToast('Session not ready yet — try again in a moment', 'info');
                return;
            }
            if (resendAwaitingHistory) {
                showToast('Resend already in progress', 'info');
                return;
            }
            // A typed /resume of a session already open in another pane
            // would create a duplicate pane for one session (the re-key
            // reply and the follow-up detach would desync one of the two
            // panes). Block it like the sidebar does (openSessionPane
            // focuses the existing pane). "resume del" targets a session
            // for deletion and "resume latest" resolves server-side, so
            // both pass through.
            if (/^\/resume(?:\s|$)/.test(text.trim())) {
                const target = text.trim().replace(/^\/resume\s+/, '').trim();
                if (target && target !== 'del' && !target.startsWith('del ')) {
                    const other = findPaneBySession(target);
                    if (other && other !== activePane()) {
                        inputArea.value = '';
                        showToast('Session already open in another pane — switched to it', 'info');
                        focusPane(other.key);
                        return;
                    }
                }
            }
            // Sending while busy cancels the current turn (same as TUI interrupt + new prompt).
            cancelActiveTurn();
            // A turn started from the focused pane is fully live-rendered —
            // no turn_end convergence refetch needed.
            if (activePane()) activePane().needsFreshHistory = false;
            hideSlashSuggest();
            enableFollow();
            // Typed session commands (/new, /resume, /fork) re-key the pane:
            // mark the change in flight so the response handler treats the
            // reply as a session change (toast + list refresh) instead of an
            // assistant message, matching the sidebar flow.
            if (/^\/(new|resume|fork)(\s|$)/.test(text.trim())) {
                pendingSessionResponse = true;
                // These commands re-key this pane away from its current
                // session; the old session's turn (if running) keeps going
                // headless, so ignore its turn_end — it must not clear the
                // pending flag mid-change (mirrors resumeSession).
                const p = activePane();
                if (p && p.id) p.ignoreTurnEndsFor = p.id;
            }
            const images = pendingAttachments.map((a) => ({ dataUrl: a.dataUrl }));
            const el = appendMessage('user', text, undefined, undefined, images);
            if (el) el.dataset.pendingAck = '1';
            streamingToolCards = {};
            pendingToolCards = {};
            toolsStartedThisTurn = false;
            contextEstAdded = 0;
            const payload = { type: 'message', content: text, sessionId: activePane().id };
            if (images.length > 0) payload.images = images;
            ws.send(JSON.stringify(payload));
            inputArea.value = '';
            clearAttachments();
        }
        sendBtn.onclick = sendMessage;

        cancelBtn.onclick = () => {
            if (!ws || ws.readyState !== WebSocket.OPEN) return;
            if (!turnActive) return;
            if (!activePane() || !activePane().id) return;
            ws.send(JSON.stringify({ type: 'cancel', sessionId: activePane().id }));
            // Tear down immediately; server also emits cancelled + turn_end.
            abortInFlightUI(null);
            setTurnActive(false);
        };

        inputArea.addEventListener('input', updateSlashSuggest);

        /* Textarea auto-resize is handled natively by CSS
           `field-sizing: content` (see styles.css) — no JS size tracking.
           The browser grows/shrinks the box with its content and clamps it
           via min-height/max-height. Firefox 152+ / Chrome 123+ / Safari
           26.2+; older engines keep the box at min-height (static). */

        inputArea.addEventListener('blur', () => {
            // Delay so mousedown on a suggestion can fire first.
            setTimeout(hideSlashSuggest, 150);
        });

        inputArea.onkeydown = (e) => {
            if (slashSuggestOpen()) {
                if (e.key === 'ArrowDown') {
                    e.preventDefault();
                    slashIndex = (slashIndex + 1) % slashMatches.length;
                    renderSlashSuggest();
                    return;
                }
                if (e.key === 'ArrowUp') {
                    e.preventDefault();
                    slashIndex = (slashIndex - 1 + slashMatches.length) % slashMatches.length;
                    renderSlashSuggest();
                    return;
                }
                if (e.key === 'Tab') {
                    e.preventDefault();
                    applySlashCompletion(slashMatches[slashIndex].name);
                    return;
                }
                if (e.key === 'Enter' && !e.shiftKey) {
                    const selected = slashMatches[slashIndex].name;
                    const token = inputArea.value.split(/\s/, 1)[0];
                    if (token.toLowerCase() !== selected.toLowerCase()) {
                        e.preventDefault();
                        applySlashCompletion(selected);
                        return;
                    }
                    hideSlashSuggest();
                    // fall through to send
                }
                if (e.key === 'Escape') {
                    e.preventDefault();
                    hideSlashSuggest();
                    return;
                }
            }
            if (e.key === 'Enter' && !e.shiftKey) {
                e.preventDefault();
                sendMessage();
            }
            if (e.key === 'Escape' && turnActive) {
                e.preventDefault();
                cancelBtn.onclick();
            }
        };

        setDirBtn.onclick = () => {
            if (!isGlobalMode) {
                showToast('Working directory can only be changed in global mode', 'error');
                return;
            }
            const dir = dirInput.value;
            if (!dir) return;
            if (!ws || ws.readyState !== WebSocket.OPEN) {
                showToast('Not connected', 'error');
                return;
            }
            // The working dir is workspace-global; the sessionId scopes the
            // cancel-then-lock to the requesting pane.
            ws.send(JSON.stringify({ type: 'config', workingDir: dir, sessionId: activePane().id }));
            showToast('Working directory updated', 'success');
            setTimeout(() => requestSessionList(), 100);
        };

        function requestSessionList() {
            if (!ws || ws.readyState !== WebSocket.OPEN) return;
            ws.send(JSON.stringify({ type: 'list_sessions' }));
        }

        function renderSessionList(sessions) {
            if (!sessionListDiv) return;
            // Cache the payload so pane-state changes (focus, busy, label)
            // can re-render the list without a server round-trip.
            lastSessions = sessions || [];
            sessionListDiv.innerHTML = '';
            const list = lastSessions;
            // One unified list: open panes (client state) plus the server's
            // saved-session payload. Open panes are merged in FIRST so an
            // open session's row ALWAYS exists — even before the server's
            // store lists it (fresh session, list lag) — and shows the LIVE
            // pane label (kept current by config/context echoes). Open
            // panes render most-recently-used first (movePaneToFront keeps
            // the map in that order), then saved sessions in server order.
            const act = activePane();
            const rows = [];
            const openIds = new Set();
            for (const pane of panes.values()) {
                const entry = pane.id ? (list.find((s) => s.id === pane.id) || null) : null;
                if (pane.id) openIds.add(pane.id);
                rows.push({ pane, entry });
            }
            for (const s of list) {
                if (!openIds.has(s.id)) rows.push({ pane: null, entry: s });
            }
            if (!rows.length) {
                const empty = document.createElement('div');
                empty.className = 'session-list-empty';
                empty.textContent = 'No sessions';
                sessionListDiv.appendChild(empty);
                return;
            }
            for (const r of rows) {
                const pane = r.pane;
                const entry = r.entry;
                const s = entry || {
                    id: pane ? pane.id : '',
                    label: pane ? pane.label : '',
                    messageCount: null,
                    active: true,
                    updatedAt: '',
                };
                const isActivePane = !!pane && pane === act;
                // A fresh session has an id but no label yet — show the
                // "New session" placeholder as its title until the first
                // turn derives a real label. The raw id stays available in
                // the row tooltip and in dataset.sessionId (delete/attach).
                const paneTitle = pane ? (pane.label || (entry && entry.label) || '') : '';
                const label = pane
                    ? (paneTitle || 'New session…')
                    : (s.label || s.id || '(unknown)');
                const row = document.createElement('div');
                row.className = 'session-row' + (isActivePane ? ' current' : '') + (pane && pane.turnActive ? ' busy' : '');
                row.dataset.sessionId = pane ? pane.id : (entry ? entry.id : '');
                row.title = !paneTitle && pane && pane.id ? `${label} (${pane.id})` : label;
                const content = document.createElement('div');
                content.className = 'session-row-content';
                const title = document.createElement('div');
                title.className = 'session-row-title';
                title.textContent = label;
                const meta = document.createElement('div');
                meta.className = 'session-row-meta';
                const frags = [];
                // Every pane state gets a uniform colored-dot indicator: the
                // dot carries the state color and the label stays muted. The
                // status indicator is pushed first so it always renders ahead
                // of the message count and relative time in the meta row.
                //   active              → green   (this pane is focused)
                //   responding          → amber   (a turn is running)
                //   creating…           → gray    (session id not assigned yet)
                //   open                → blue    (background pane in this tab)
                //   resume to continue  → violet  (live elsewhere / headless)
                let stateLabel = '';
                let stateClass = '';
                if (pane && pane.turnActive) {
                    stateLabel = 'responding';
                    stateClass = 'amber';
                } else if (pane && !pane.id) {
                    stateLabel = 'creating…';
                    stateClass = 'gray';
                } else if (isActivePane) {
                    stateLabel = 'active';
                    stateClass = 'green';
                } else if (pane && pane.id) {
                    // Open as a background pane in this tab — clicking the
                    // row focuses it.
                    stateLabel = 'open';
                    stateClass = 'blue';
                } else if (s.active) {
                    // Live in-memory runtime elsewhere (another tab, or a
                    // headless turn running): reopening attaches to it.
                    stateLabel = 'resume to continue';
                    stateClass = 'violet';
                }
                if (stateLabel) {
                    const group = document.createElement('span');
                    group.className = 'session-state';
                    const dot = document.createElement('span');
                    dot.className = 'session-state-dot ' + stateClass;
                    dot.title = stateLabel;
                    dot.setAttribute('aria-label', stateLabel);
                    const label = document.createElement('span');
                    label.className = 'session-state-label';
                    label.textContent = stateLabel;
                    group.appendChild(dot);
                    group.appendChild(label);
                    frags.push(group);
                }
                if (entry && entry.messageCount != null) frags.push(`${entry.messageCount} msgs`);
                const rel = relativeTime(s.updatedAt);
                if (rel) {
                    // Tag the relative time so the 30s tick can refresh it in
                    // place (session rows are otherwise re-rendered only on
                    // pane-state changes or new sessions payloads, leaving the
                    // "3m ago" labels to go stale).
                    const t = document.createElement('span');
                    t.className = 'session-row-time';
                    t.dataset.updated = s.updatedAt || '';
                    t.textContent = rel;
                    frags.push(t);
                }
                for (let i = 0; i < frags.length; i++) {
                    if (i > 0) meta.appendChild(document.createTextNode(' · '));
                    if (typeof frags[i] === 'string') {
                        meta.appendChild(document.createTextNode(frags[i]));
                    } else {
                        meta.appendChild(frags[i]);
                    }
                }
                content.appendChild(title);
                content.appendChild(meta);
                content.onclick = () => {
                    if (pane) {
                        // Open pane rows focus the pane (no-op when already
                        // active).
                        focusPane(pane.key);
                    } else {
                        // Saved rows attach the session as a new pane.
                        openSessionPane(s.id);
                    }
                };
                row.appendChild(content);
                // Close/delete button (hidden by default, shown on row
                // hover): an OPEN session's ✕ closes the pane — the session
                // is detached (it stays saved and its row reappears as a
                // saved session); a saved session's ✕ deletes it.
                const closeBtn = document.createElement('button');
                closeBtn.className = 'session-row-del';
                closeBtn.textContent = '✕';
                if (pane) {
                    closeBtn.title = 'Close session (stays saved)';
                    closeBtn.onclick = (e) => {
                        e.stopPropagation();
                        closePane(pane.key);
                    };
                } else {
                    closeBtn.title = 'Delete this session';
                    closeBtn.onclick = (e) => {
                        e.stopPropagation();
                        deleteSession(s.id, s.label);
                    };
                }
                row.appendChild(closeBtn);
                sessionListDiv.appendChild(row);
            }
        }

        /**
         * Update the current session's row title in the sidebar without
         * re-rendering the entire list. The current session lives in the
         * unified session list (the active pane's row).
         */
        function updateCurrentSessionLabel(label) {
            if (!label || !sessionListDiv) return;
            const pane = activePane();
            if (pane) pane.label = label;
            if (!pane || !pane.id) return;
            // Keep the cached server entry in sync so a later re-render
            // keeps the new label even before the server echoes it.
            if (lastSessions) {
                const entry = lastSessions.find((s) => s.id === pane.id);
                if (entry) entry.label = label;
            }
            const row = findSessionRow(pane.id);
            if (!row) return;
            const titleEl = row.querySelector('.session-row-title');
            if (titleEl.textContent === label) return; // already up-to-date
            titleEl.textContent = label;
            row.title = label;
        }

        function findSessionRow(id) {
            if (!sessionListDiv || !id) return null;
            for (const row of sessionListDiv.querySelectorAll('.session-row')) {
                if (row.dataset.sessionId === id) return row;
            }
            return null;
        }

        // Re-render the sidebar session list from the cached payload. Used
        // when pane state the list shows (focus, busy/responding, label)
        // changes without a server round-trip; falls back to asking the
        // server when no payload has arrived yet.
        function refreshSidebarSessions() {
            if (lastSessions) {
                renderSessionList(lastSessions);
            } else {
                requestSessionList();
            }
        }

        function newSession() {
            if (!ws || ws.readyState !== WebSocket.OPEN) {
                showToast('Not connected', 'error');
                return;
            }
            if (resendAwaitingHistory) {
                showToast('Resend already in progress', 'info');
                return;
            }
            if (pendingSessionResponse) {
                // Two session changes on one connection would interleave:
                // both replies land in this pane and their clear_chat/
                // history/config follow-ups can repaint the wrong
                // transcript. Match deleteSession/resumeSession and wait.
                showToast('Session change already in progress', 'info');
                return;
            }
            // The sidebar "New" button opens a NEW pane; the previous
            // pane stays open in the background. The new pane's session id
            // arrives in the config reply (re-key via the config handler).
            saveActivePaneState();
            const pane = makePane();
            activePaneKey = pane.key;
            clearChat();
            loadActivePaneState();
            pendingSessionResponse = true;
            ws.send(JSON.stringify({ type: 'session_new' }));
        }

        async function deleteSession(id, label) {
            if (!ws || ws.readyState !== WebSocket.OPEN) {
                showToast('Not connected', 'error');
                return;
            }
            if (!id) return;
            if (resendAwaitingHistory) {
                showToast('Resend already in progress', 'info');
                return;
            }
            if (pendingSessionResponse) {
                showToast('Session change already in progress', 'info');
                return;
            }
            const displayName = label || id;
            const confirmed = await showSessionDeleteModal(displayName);
            if (!confirmed) return;
            // Deleting the active pane's session interrupts its turn; a
            // background session's delete leaves the active turn running.
            if (activePane() && activePane().id === id) {
                cancelActiveTurn();
            }
            pendingSessionResponse = true;
            ws.send(JSON.stringify({ type: 'session_delete', sessionId: id }));
        }

        // Custom confirm modal for session deletion (matches the other dialogs).
        // Esc cancels; the safe default (Cancel) is focused on open.
        function showSessionDeleteModal(displayName) {
            return new Promise((resolve) => {
                const overlay = document.getElementById('session-delete-overlay');
                const filenameEl = document.getElementById('session-delete-filename');
                const cancelBtn = document.getElementById('session-delete-cancel-btn');
                const confirmBtn = document.getElementById('session-delete-confirm-btn');
                if (!overlay) { resolve(window.confirm(`Delete session "${displayName}"? This cannot be undone.`)); return; }
                filenameEl.textContent = `Session "${displayName}" and its message history will be permanently deleted.`;
                openModal(overlay);
                const cleanup = (result) => {
                    closeModal(overlay);
                    cancelBtn.removeEventListener('click', onCancel);
                    confirmBtn.removeEventListener('click', onConfirm);
                    overlay.removeEventListener('keydown', onKey);
                    resolve(result);
                };
                const onCancel = () => cleanup(false);
                const onConfirm = () => cleanup(true);
                const onKey = (e) => {
                    if (e.key === 'Escape') {
                        e.stopPropagation(); // keep the document handler from cancelling the agent turn
                        onCancel();
                    }
                };
                cancelBtn.addEventListener('click', onCancel);
                confirmBtn.addEventListener('click', onConfirm);
                overlay.addEventListener('keydown', onKey);
            });
        }

        function resumeSession(id) {
            if (!ws || ws.readyState !== WebSocket.OPEN) {
                showToast('Not connected', 'error');
                return;
            }
            if (!id) return;
            if (resendAwaitingHistory) {
                showToast('Resend already in progress', 'info');
                return;
            }
            if (pendingSessionResponse) {
                showToast('Session change already in progress', 'info');
                return;
            }
            // The old session's turn keeps running headless (continuation
            // design — /new behaves the same): resume only re-keys this
            // pane, it does NOT cancel the turn. Record the old session id
            // so its turn_end (which may arrive before the reply re-keys the
            // pane) cannot clear our pending flag mid-resume or block the
            // resumed session's convergence; once the reply re-keys the
            // pane, the old session's events are dropped by paneForMessage.
            const p = activePane();
            if (p && p.id) p.ignoreTurnEndsFor = p.id;
            pendingSessionResponse = true;
            ws.send(JSON.stringify({ type: 'session_resume', sessionId: id }));
        }

        function switchMainPane(pane) {
            if (pane !== 'terminal' && terminalIsMobile() && terminalPanel.classList.contains('mobile-full')) {
                terminalCloseMobile();
            }
            document.querySelectorAll('.main-tab').forEach((t) => {
                t.classList.toggle('active', t.dataset.pane === pane);
            });
            document.querySelectorAll('.pane').forEach((p) => {
                p.classList.toggle('active', p.id === `${pane}-pane`);
            });
            if (pane === 'editor') {
                initMonaco().then(() => refreshExplorer()).catch(() => {});
            }
        }

        function exportChat() {
            const lines = [];
            const pane = activePane();
            const sessionId = (sessionInfoDiv.textContent || 'session').replace(/[^\w.-]+/g, '_');
            const label = (pane && pane.label) || '';
            const date = new Date().toISOString().slice(0, 10);
            lines.push(`# GoGen chat${label ? ` — ${label}` : ''} (${sessionId})`);
            lines.push(`_Exported ${new Date().toLocaleString()}_`);
            lines.push('');
            for (const el of messagesDiv.querySelectorAll('.message, .tool-card')) {
                if (el.classList.contains('system') || el.classList.contains('thinking') || el.classList.contains('thinking-block')) {
                    continue;
                }
                if (el.classList.contains('thought-card')) continue;
                if (el.classList.contains('tool-card')) {
                    // Tool call card: name, args, result, and (for patch_file /
                    // show_diff) the raw diff text from the fallback <pre>.
                    const nameEl = el.querySelector('.tool-name');
                    const name = nameEl ? nameEl.textContent.trim() : 'tool';
                    const statusEl = el.querySelector('.tool-status-bar');
                    const status = statusEl ? statusEl.textContent.trim() : '';
                    const argsText = (el.querySelector('.tool-args')?.textContent || '').trim();
                    let resultText = (el.querySelector('.tool-result-body')?.textContent || '').trim();
                    // The status bar is a child of the result body; drop its
                    // label from the text so it is not duplicated.
                    if (status && resultText.startsWith(status)) {
                        resultText = resultText.slice(status.length).trim();
                    }
                    const copyLabel = el.querySelector('.tool-result-copy')?.textContent || '';
                    if (copyLabel && resultText.startsWith(copyLabel)) {
                        resultText = resultText.slice(copyLabel.length).trim();
                    }
                    const diffText = (el.querySelector('.monaco-tool-host .diff-fallback')?.textContent || '').trim();
                    if (!argsText && !resultText && !diffText) continue;
                    lines.push(`### tool: ${name}${status ? ` — ${status}` : ''}`);
                    if (argsText) {
                        lines.push('');
                        lines.push('```');
                        lines.push(argsText);
                        lines.push('```');
                    }
                    if (diffText) {
                        lines.push('');
                        lines.push('```diff');
                        lines.push(diffText);
                        lines.push('```');
                    }
                    if (resultText) {
                        lines.push('');
                        lines.push(resultText);
                    }
                    lines.push('');
                    continue;
                }
                const role = el.classList.contains('user') ? 'user'
                    : el.classList.contains('assistant') ? 'assistant' : null;
                if (!role) continue;
                const raw = messageRawStore.get(el);
                const body = (raw != null ? raw : el.textContent || '').trim();
                if (!body) continue;
                const ts = el.dataset.createdAt ? new Date(el.dataset.createdAt) : null;
                const stamp = (ts && !Number.isNaN(ts.getTime())) ? ` — ${ts.toLocaleString()}` : '';
                lines.push(`## ${role}${stamp}`);
                lines.push('');
                lines.push(body);
                lines.push('');
            }
            const labelSlug = label.toLowerCase().replace(/[^\w.-]+/g, '_').replace(/^_+|_+$/g, '');
            const base = labelSlug ? `${labelSlug}-${sessionId}` : sessionId;
            const blob = new Blob([lines.join('\n')], { type: 'text/markdown;charset=utf-8' });
            const a = document.createElement('a');
            a.href = URL.createObjectURL(blob);
            a.download = `gogen-chat-${base}-${date}.md`;
            a.click();
            URL.revokeObjectURL(a.href);
            showToast('Chat exported', 'success');
        }

        const PALETTE_COMMANDS = [
            { id: 'new-session', label: 'New session', hint: '', run: () => newSession() },
            { id: 'resume-latest', label: 'Resume latest session', hint: '', run: () => resumeSession('latest') },
            { id: 'refresh-sessions', label: 'Refresh sessions', hint: '', run: () => requestSessionList() },
            { id: 'toggle-mode', label: 'Toggle Act / Plan mode', hint: '', run: () => {
                ws?.send(JSON.stringify({ type: 'set_mode', mode: currentMode === 'plan' ? 'act' : 'plan', sessionId: activePane().id }));
            }},
            { id: 'pane-chat', label: 'Switch to Chat', hint: '', run: () => switchMainPane('chat') },
            { id: 'pane-editor', label: 'Switch to Editor', hint: '', run: () => switchMainPane('editor') },
            { id: 'find-files', label: 'Find in files', hint: 'Ctrl+Shift+F', run: () => focusFindInFiles() },
            { id: 'export', label: 'Export chat', hint: 'Ctrl+Shift+E', run: () => exportChat() },
            { id: 'undo', label: 'Undo', hint: 'Ctrl+Z', run: () => editorUndo() },
            { id: 'redo', label: 'Redo', hint: 'Ctrl+Shift+Z', run: () => editorRedo() },
            { id: 'refresh-explorer', label: 'Refresh file explorer', hint: '', run: () => refreshExplorer().catch((e) => showToast(e.message, 'error')) },
            { id: 'save', label: 'Save file', hint: 'Ctrl+S', run: () => saveActive() },
            { id: 'save-all', label: 'Save all', hint: '', run: () => saveAll() },
            { id: 'compact', label: 'Compact context', hint: '', run: () => {
                startCompact();
            }},
            { id: 'context-detail', label: 'Show context usage', hint: '', run: () => {
                if (ws?.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type: 'message', content: '/context', sessionId: activePane().id }));
            }},
            { id: 'list-models', label: 'List / switch models', hint: '', run: () => {
                if (ws?.readyState === WebSocket.OPEN) {
                    modelsRequested = false; // palette is an explicit request; allow a refetch
                    ensureModelsLoaded();
                }
            }},
            { id: 'open-settings', label: 'Open settings', hint: '', run: () => settingsBtn.click() },
            { id: 'toggle-notifications', label: 'Toggle notifications', hint: '', run: () => {
                const cur = localStorage.getItem('gogen_notifications') || 'off';
                const next = cur === 'off' ? 'background' : 'off';
                localStorage.setItem('gogen_notifications', next);
                notificationsSelect.value = next;
                if (next !== 'off') requestNotificationPermission();
                showToast(`Notifications: ${next === 'off' ? 'off' : 'on (' + next + ')'}`, 'info');
            }},
        ];
        let paletteMatches = [];
        let paletteIndex = 0;

        function closePalette() {
            closeModal(paletteOverlay, { className: 'open' });
            paletteInput.value = '';
            paletteList.innerHTML = '';
        }

        function openPalette() {
            openModal(paletteOverlay, { className: 'open', focusSelector: '#command-palette-input' });
            paletteInput.value = '';
            paletteIndex = 0;
            renderPalette();
        }

        function renderPalette() {
            const q = paletteInput.value.trim().toLowerCase();
            paletteMatches = PALETTE_COMMANDS.filter((c) => !q || c.label.toLowerCase().includes(q) || c.id.includes(q));
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

        document.getElementById('session-new-btn')?.addEventListener('click', () => newSession());
        // Auto-refreshed on session mutations — no manual refresh button needed
        document.getElementById('export-chat-btn')?.addEventListener('click', () => exportChat());

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

        document.addEventListener('keydown', (e) => {
            const mod = e.ctrlKey || e.metaKey;
            const tag = (e.target && e.target.tagName) || '';
            const typing = tag === 'INPUT' || tag === 'TEXTAREA' || (e.target && e.target.isContentEditable);

            if (mod && e.key.toLowerCase() === 'k') {
                e.preventDefault();
                if (paletteOverlay.classList.contains('open')) closePalette();
                else openPalette();
                return;
            }
            if (mod && e.shiftKey && e.key.toLowerCase() === 'f') {
                e.preventDefault();
                focusFindInFiles();
                return;
            }
            if (mod && e.shiftKey && e.key.toLowerCase() === 'e') {
                e.preventDefault();
                exportChat();
                return;
            }
            if (mod && e.key === '`') {
                // Toggle the terminal panel (Ctrl+` like VS Code). On mobile
                // this opens the full-screen terminal; on desktop it switches
                // between the strip and the expanded panel.
                e.preventDefault();
                if (terminalIsMobile()) terminalOpenMobile();
                else terminalSetExpanded(!terminalExpanded);
                return;
            }
            if (e.key === 'Escape') {
                if (terminalMoreMenu && !terminalMoreMenu.hidden) {
                    e.preventDefault();
                    terminalMoreMenu.hidden = true;
                    return;
                }
                if (settingsOverlay.classList.contains('active')) {
                    e.preventDefault();
                    closeModal(settingsOverlay);
                    return;
                }
                const kbOverlay = document.getElementById('keybindings-overlay');
                if (kbOverlay && kbOverlay.classList.contains('active')) {
                    e.preventDefault();
                    closeModal(kbOverlay);
                    return;
                }
                if (paletteOverlay.classList.contains('open')) {
                    e.preventDefault();
                    closePalette();
                    return;
                }
                // Decision dialogs handle Esc themselves when focus is inside;
                // this guard covers the backdrop/focus-outside case so Esc
                // can't leak through to the turn-cancel below.
                const decisionModal = document.querySelector(
                    '#delete-approval-overlay.active, #session-delete-overlay.active, ' +
                    '#close-tab-overlay.active, #replace-preview-overlay.active'
                );
                if (decisionModal) {
                    e.preventDefault();
                    return;
                }
                if (!typing && turnActive) {
                    e.preventDefault();
                    cancelBtn.onclick();
                }
            }
        });

        // === Scroll-to-bottom button ===
        const scrollBottomBtn = document.getElementById('scroll-bottom-btn');
        const NEAR_BOTTOM_PX = 32;
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
        ].join(', ');
        // Timestamp (performance.now) of the last unpin; gates the d<=8
        // re-pin branch in the scroll handler (see UNPIN_REPIN_GRACE_MS).
        let lastUnpinAt = 0;
        let unpinRecoverTimer = null;

        function distanceFromBottom() {
            return messagesDiv.scrollHeight - messagesDiv.scrollTop - messagesDiv.clientHeight;
        }

        function updateScrollBottomBtn() {
            scrollBottomBtn.classList.toggle('visible', !stickToBottom || distanceFromBottom() > NEAR_BOTTOM_PX);
        }

        function unpinFromBottom() {
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

        function pinToBottom() {
            stickToBottom = true;
            clearTimeout(unpinRecoverTimer);
            unpinRecoverTimer = null;
            // Disable browser scroll anchoring while we're auto-following
            // (prevents anchoring from fighting smartScroll's scroll-to-bottom).
            messagesDiv.classList.add('no-anchor');
            ignoreScrollEvent = true;
            messagesDiv.scrollTop = messagesDiv.scrollHeight;
            requestAnimationFrame(() => { ignoreScrollEvent = false; });
            scrollBottomBtn.classList.remove('visible');
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
            // follow.
            if (e.deltaY < 0) unpinFromBottom();
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
        messagesDiv.addEventListener('scroll', () => {
            if (_scrollPending) return;
            _scrollPending = true;
            requestAnimationFrame(() => {
                _scrollPending = false;
                // Programmatic smartScroll()/pinToBottom() set ignoreScrollEvent
                // and defer the reset by one frame, so their own scroll events
                // are skipped here. User-initiated scrolls (wheel, trackpad,
                // scrollbar drag) reach this point with the flag clear.
                if (ignoreScrollEvent) return;
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
                updateScrollBottomBtn();
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
        function maybeRepinNearBottom() {
            if (stickToBottom || replayInProgress) return;
            // Don't yank the chat while the user is composing: typing resizes
            // the input row, which shrinks the messages viewport and moves the
            // bottom — that's not scroll intent and shouldn't trigger re-pin.
            if (document.activeElement === inputArea) return;
            if (performance.now() - lastUnpinAt <= UNPIN_REPIN_GRACE_MS) return;
            if (distanceFromBottom() <= NEAR_BOTTOM_PX) pinToBottom();
        }
        function scheduleRepinIfPinned() {
            if (_repinRafPending) return;
            _repinRafPending = true;
            requestAnimationFrame(() => {
                _repinRafPending = false;
                if (stickToBottom) smartScroll();
                else maybeRepinNearBottom();
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
        if (typeof ResizeObserver !== 'undefined') {
            // Fires when #messages' visible box changes (window/sidebar/composer
            // resize). scrollHeight growth is content, not the box, so the
            // streaming / colorize / image / font paths above cover that case.
            new ResizeObserver(() => {
                scheduleRepinIfPinned();
                if (!stickToBottom) updateScrollBottomBtn();
            }).observe(messagesDiv);
        }

        // Auto-scroll only while stickToBottom is set (user hasn't scrolled away).
        // The immediate scrollTop gives low-latency follow during streaming
        // (each token triggers smartScroll).  The gogen-colorized event
        // re-invokes smartScroll after Monaco finishes coloring.
        let _smartScrollRafPending = false;
        function smartScroll() {
            if (replayInProgress) return;
            if (!stickToBottom) {
                maybeRepinNearBottom();
                return;
            }
            // Suppress the scroll event generated by this programmatic scroll.
            // The flag must survive until that event fires, so reset it on the
            // next frame (scroll events are dispatched as tasks before the next
            // rendering update's rAF callbacks run).
            ignoreScrollEvent = true;
            messagesDiv.scrollTop = messagesDiv.scrollHeight;
            requestAnimationFrame(() => { ignoreScrollEvent = false; });
            // Late layout (async Monaco mounting, syntax highlighting, image
            // or font loads, composer height changes) can grow the DOM after
            // the synchronous scroll above. Re-check on the next frame so the
            // bottom stays pinned even if no other event arrives afterwards.
            if (_smartScrollRafPending) return;
            _smartScrollRafPending = true;
            requestAnimationFrame(() => {
                _smartScrollRafPending = false;
                if (!stickToBottom || replayInProgress) return;
                ignoreScrollEvent = true;
                messagesDiv.scrollTop = messagesDiv.scrollHeight;
                requestAnimationFrame(() => { ignoreScrollEvent = false; });
            });
        }

        // === Collapsible sidebar toggle ===
        const sidebarToggle = document.getElementById('sidebar-toggle');
        const sidebar = document.getElementById('sidebar');
        sidebarToggle.addEventListener('click', () => {
            sidebar.classList.toggle('open');
        });
        // Close sidebar when clicking outside on mobile
        document.addEventListener('click', (e) => {
            if (window.innerWidth <= 768 && sidebar.classList.contains('open')
                && !sidebar.contains(e.target) && e.target !== sidebarToggle) {
                sidebar.classList.remove('open');
            }
        });

        // === Resizable sidebar via drag handle ===
        function initSidebarResize() {
            const handles = document.querySelectorAll('.sidebar-resize-handle');

            handles.forEach(handle => {
                const targetId = handle.dataset.target;
                const sidebar = document.getElementById(targetId);
                if (!sidebar) return;

                // Restore saved width
                const saved = localStorage.getItem(targetId + '-width');
                if (saved) {
                    sidebar.style.width = saved;
                    sidebar.style.flexGrow = '0';
                    sidebar.style.flexShrink = '0';
                }

                handle.addEventListener('mousedown', (e) => {
                    e.preventDefault();
                    const startX = e.clientX;
                    const startWidth = sidebar.getBoundingClientRect().width;

                    handle.classList.add('active');

                    function onMouseMove(ev) {
                        const delta = ev.clientX - startX;
                        let newWidth = startWidth + delta;
                        // Clamp percentage-of-viewport so it scales on 4K/8K screens
                        const minPct = 0.12;  // at least 12% of viewport
                        const maxPct = 0.50;  // at most 50% of viewport
                        const minW = Math.max(120, window.innerWidth * minPct);
                        const maxW = window.innerWidth * maxPct;
                        newWidth = Math.max(minW, Math.min(maxW, newWidth));
                        sidebar.style.width = newWidth + 'px';
                        sidebar.style.flexGrow = '0';
                        sidebar.style.flexShrink = '0';
                    }

                    function onMouseUp() {
                        handle.classList.remove('active');
                        document.removeEventListener('mousemove', onMouseMove);
                        document.removeEventListener('mouseup', onMouseUp);
                        // Persist the width
                        localStorage.setItem(targetId + '-width', sidebar.style.width);
                    }

                    document.addEventListener('mousemove', onMouseMove);
                    document.addEventListener('mouseup', onMouseUp);
                });

                // Touch support: forward touch events to mouse handlers
                handle.addEventListener('touchstart', (e) => {
                    const touch = e.touches[0];
                    const mouseEvent = new MouseEvent('mousedown', {
                        clientX: touch.clientX,
                        clientY: touch.clientY,
                    });
                    handle.dispatchEvent(mouseEvent);
                }, { passive: true });
            });
        }

        initSidebarResize();

        // === Favicon (handled via link tag) ===

        // === Page title updates ===
        const baseTitle = 'GoGen AI Agent';
        function updateTitle(status) {
            let title;
            if (status === 'thinking' || status === 'streaming') {
                title = `⏳ ${baseTitle}`;
            } else if (status === 'disconnected') {
                title = `🔴 ${baseTitle}`;
            } else {
                title = baseTitle;
            }
            // Avoid rewriting the title bar on every mutation burst.
            if (document.title !== title) document.title = title;
        }
        // We use MutationObserver on messages div to detect streaming.
        // Coalesce mutation bursts (innerHTML swaps, colorize attribute
        // writes) into one rAF pass and derive state from variables instead
        // of a subtree querySelector per mutation record.
        let titleUpdatePending = false;
        const titleObserver = new MutationObserver(() => {
            if (titleUpdatePending) return;
            titleUpdatePending = true;
            requestAnimationFrame(() => {
                titleUpdatePending = false;
                if (currentProgressPhase === 'thinking') updateTitle('thinking');
                else if (currentStreamDiv) updateTitle('streaming');
                else updateTitle('idle');
            });
        });
        titleObserver.observe(messagesDiv, { childList: true, subtree: true, attributes: true });

        // === Context tooltip ===
        const contextTooltip = document.getElementById('context-tooltip');
        const fmtTokK = (n) => {
            if (!n || n <= 0) return '0';
            if (n < 1000) return String(n);
            if (n < 10000) {
                const w = Math.floor(n / 1000);
                const f = Math.floor((n % 1000) / 100);
                return f === 0 ? `${w}k` : `${w}.${f}k`;
            }
            return `${Math.floor(n / 1000)}k`;
        };
        const fmtCost = (usd) => {
            if (usd < 0.01) return `$${usd.toFixed(4)}`;
            if (usd < 1) return `$${usd.toFixed(3)}`;
            return `$${usd.toFixed(2)}`;
        };

        // === Settings modal ===
        const settingsBtn = document.getElementById('settings-btn');
        const settingsOverlay = document.getElementById('settings-overlay');
        const themeSelect = document.getElementById('theme-select');

        settingsBtn.addEventListener('click', () => {
            openModal(settingsOverlay);
        });
        document.getElementById('settings-close-btn')?.addEventListener('click', () => {
            closeModal(settingsOverlay);
        });
        settingsOverlay.addEventListener('click', (e) => {
            if (e.target === settingsOverlay) closeModal(settingsOverlay);
        });

        // === Theme: detect system preference, allow override ===
        const prefersDark = window.matchMedia('(prefers-color-scheme: dark)');
        const savedTheme = localStorage.getItem('gogen-theme') || 'auto';

        function resolveTheme(preference) {
            if (preference === 'auto' || !preference) return prefersDark.matches ? 'dark' : 'light';
            return preference;
        }

        function applyTheme(preference) {
            const resolved = resolveTheme(preference);
            document.documentElement.classList.toggle('light', resolved === 'light');
            localStorage.setItem('gogen-theme', preference);
            // Keep select in sync
            if (themeSelect.value !== preference) themeSelect.value = preference;
            // Switch Monaco theme to match
            setMonacoTheme(resolved === 'light');
        }

        // Initialize
        themeSelect.value = savedTheme;
        applyTheme(savedTheme);

        // Listen for OS theme changes (only affects "auto" mode)
        prefersDark.addEventListener('change', () => {
            const pref = localStorage.getItem('gogen-theme') || 'auto';
            if (pref === 'auto') applyTheme('auto');
        });

        themeSelect.addEventListener('change', () => {
            applyTheme(themeSelect.value);
        });

        // === File-click behavior: persist and apply ===
        const fileClickSelect = document.getElementById('file-click-behavior');
        const savedFileClick = localStorage.getItem('gogen_file_click_behavior') || 'open';
        fileClickSelect.value = savedFileClick;
        fileClickSelect.addEventListener('change', () => {
            localStorage.setItem('gogen_file_click_behavior', fileClickSelect.value);
        });
        // Listen for storage changes from other tabs
        window.addEventListener('storage', (e) => {
            if (e.key === 'gogen_file_click_behavior') {
                const val = e.newValue || 'open';
                if (fileClickSelect.value !== val) fileClickSelect.value = val;
            }
        });

        // === Chat diff viewer: static tokenizer pre (default) or full Monaco editor ===
        const diffViewerSelect = document.getElementById('chat-diff-viewer');
        const savedDiffViewer = localStorage.getItem('gogen_chat_diff_viewer') || 'tokenizer';
        diffViewerSelect.value = savedDiffViewer;
        diffViewerSelect.addEventListener('change', () => {
            localStorage.setItem('gogen_chat_diff_viewer', diffViewerSelect.value);
        });
        window.addEventListener('storage', (e) => {
            if (e.key === 'gogen_chat_diff_viewer') {
                const val = e.newValue || 'tokenizer';
                if (diffViewerSelect.value !== val) diffViewerSelect.value = val;
            }
        });

        // === Editor preferences: persist and apply to the live Monaco editor ===
        const editorMinimapSelect = document.getElementById('editor-minimap');
        const editorWordwrapSelect = document.getElementById('editor-wordwrap');
        const editorStickySelect = document.getElementById('editor-sticky');
        const editorFontsizeInput = document.getElementById('editor-fontsize');

        function clampFontSize(v) {
            // Mirror getEditorPrefs() clamping so the control can never show a
            // value the editor won't actually apply.
            const min = parseInt(editorFontsizeInput?.min || '8', 10);
            const max = parseInt(editorFontsizeInput?.max || '32', 10);
            let n = parseInt(v, 10);
            if (!Number.isFinite(n)) n = 13;
            return Math.max(min, Math.min(max, n));
        }

        function initEditorPrefControl(control, key, dflt) {
            if (!control) return;
            const saved = localStorage.getItem(key) || dflt;
            control.value = saved;
            control.addEventListener('change', () => {
                if (control.type === 'number') {
                    // Keep the visible value in sync with what the editor will
                    // actually apply (getEditorPrefs clamps to [min, max]).
                    control.value = String(clampFontSize(control.value));
                }
                localStorage.setItem(key, control.value);
                applyEditorPrefs();
            });
        }
        initEditorPrefControl(editorMinimapSelect, 'gogen_editor_minimap', 'off');
        initEditorPrefControl(editorWordwrapSelect, 'gogen_editor_wordwrap', 'on');
        initEditorPrefControl(editorStickySelect, 'gogen_editor_sticky', 'on');
        initEditorPrefControl(editorFontsizeInput, 'gogen_editor_fontsize', '13');
        // Keep editor prefs in sync across tabs (mirrors the other settings).
        window.addEventListener('storage', (e) => {
            let changed = false;
            if (e.key === 'gogen_editor_minimap' && editorMinimapSelect) {
                editorMinimapSelect.value = e.newValue || 'off';
                changed = true;
            } else if (e.key === 'gogen_editor_wordwrap' && editorWordwrapSelect) {
                editorWordwrapSelect.value = e.newValue || 'on';
                changed = true;
            } else if (e.key === 'gogen_editor_sticky' && editorStickySelect) {
                editorStickySelect.value = e.newValue || 'on';
                changed = true;
            } else if (e.key === 'gogen_editor_fontsize' && editorFontsizeInput) {
                editorFontsizeInput.value = String(clampFontSize(e.newValue || '13'));
                changed = true;
            }
            if (changed) applyEditorPrefs();
        });

        // === Desktop notifications ===
        const notificationsSelect = document.getElementById('notifications-select');
        const savedNotifications = localStorage.getItem('gogen_notifications') || 'off';
        notificationsSelect.value = savedNotifications;
        notificationsSelect.addEventListener('change', () => {
            localStorage.setItem('gogen_notifications', notificationsSelect.value);
            if (notificationsSelect.value !== 'off') {
                requestNotificationPermission();
            }
        });
        window.addEventListener('storage', (e) => {
            if (e.key === 'gogen_notifications') {
                const val = e.newValue || 'off';
                if (notificationsSelect.value !== val) notificationsSelect.value = val;
            }
        });

        // === Accent color: persist and apply ===
        const accentInput = document.getElementById('accent-color-input');

        // The soft accent variant is derived in CSS via color-mix, so JS only
        // needs to set the base --user-accent (see styles.css).
        function applyAccentColor(hex) {
            if (!hex) {
                document.documentElement.style.removeProperty('--user-accent');
                return;
            }
            document.documentElement.style.setProperty('--user-accent', hex);
        }

        const savedAccent = localStorage.getItem('gogen-accent-color') || '';
        if (savedAccent) {
            accentInput.value = savedAccent;
            applyAccentColor(savedAccent);
        }

        accentInput.addEventListener('input', function () {
            const hex = accentInput.value;
            applyAccentColor(hex);
            localStorage.setItem('gogen-accent-color', hex);
        });

        window.addEventListener('storage', function (e) {
            if (e.key === 'gogen-accent-color') {
                const val = e.newValue || '';
                if (accentInput.value !== val) accentInput.value = val;
                applyAccentColor(val);
            }
        });

        // === Accent color: reset to theme default ===
        document.getElementById('accent-reset-btn').addEventListener('click', function () {
            localStorage.removeItem('gogen-accent-color');
            document.documentElement.style.removeProperty('--user-accent');
            // Reset picker to the current theme's default accent
            const isLight = document.documentElement.classList.contains('light');
            accentInput.value = isLight ? '#0066cc' : '#569cd6';
            accentInput.blur();
        });

        function getNotificationPref() {
            return localStorage.getItem('gogen_notifications') || 'off';
        }

        function requestNotificationPermission() {
            if (!('Notification' in window)) return;
            if (Notification.permission === 'granted') return;
            if (Notification.permission === 'denied') return;
            Notification.requestPermission();
        }

        /**
         * Send a desktop notification if the user has enabled them.
         * @param {string} title
         * @param {string} [body]
         * @param {string} [tag] - dedup tag (prevents stacking)
         */
        function sendNotification(title, body, tag) {
            const pref = getNotificationPref();
            if (pref === 'off') return;
            if (!('Notification' in window)) return;
            if (Notification.permission !== 'granted') return;

            // "background" mode: only notify when the tab is not focused
            if (pref === 'background' && document.hasFocus()) return;

            try {
                const opts = { tag: tag || 'gogen-notification' };
                if (body) opts.body = body;
                new Notification(title, opts);
            } catch (_) {
                // Service worker or permission issue — silently ignore.
            }
        }

        // Restore notification permission on load if preference is non-off
        if (getNotificationPref() !== 'off') {
            requestNotificationPermission();
        }

        // Delete approval: always notify since it requires user action
        const _origShowDeleteApproval = showDeleteApproval;
        showDeleteApproval = function _showDeleteApprovalNotify(data) {
            _origShowDeleteApproval.call(null, data);
            const paths = (data.paths || []).join(', ');
            sendNotification('GoGen — Approval needed', `File delete requested: ${paths}`, 'gogen-delete-approval');
        };

        // Create the initial pane (the server's default session). Its session
        // id arrives in the connect handshake's config and is adopted by the
        // config handler.
        const initialPane = makePane();
        activePaneKey = initialPane.key;
        setTurnActive(false);
        setConnState('disconnected');
        connect();
        // The editor runs on its own /ws/editor socket; open it once here.
        // It manages its own reconnection and explorer refresh.
        connectEditorSocket();
