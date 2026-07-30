        import {
            setWebSocket,
            handleServerMessage,
            setupEditorUI,
            refreshExplorer,
            disposeChatEditors,
            mountDiffEditor,
            updateDiffEditor,
            updateDiffFallback,
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
        } from '/editor.js';
        import { marked } from '/vendor/marked.esm.js';
        import DOMPurify from '/vendor/dompurify.esm.js';

        marked.use({ gfm: true, breaks: true });

        const messagesDiv = document.getElementById('messages');
        const inputArea = document.getElementById('message-input');
        const sendBtn = document.getElementById('send-btn');
        const cancelBtn = document.getElementById('cancel-btn');
        const slashSuggest = document.getElementById('slash-suggest');
        const dirInput = document.getElementById('working-dir-input');
        const setDirBtn = document.getElementById('set-dir-btn');
        const connIndicator = document.getElementById('conn-indicator');
        const modelInfoDiv = document.getElementById('model-info');
        const modelSelect = document.getElementById('model-select');
        const contextInfoDiv = document.getElementById('context-info');
        const contextText = document.getElementById('context-text');
        const contextBar = document.getElementById('context-bar');
        const sessionInfoDiv = document.getElementById('session-info');
        const sessionListDiv = document.getElementById('session-list');
        const modeActBtn = document.getElementById('mode-act-btn');
        const modePlanBtn = document.getElementById('mode-plan-btn');
        let currentMode = 'act';
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
        const tbThinkingGrid = document.getElementById('tb-thinking-grid');
        const tbModeBtn = document.getElementById('tb-mode-btn');
        const tbContextBadge = document.getElementById('tb-context-badge');
        const composerToolbar = document.getElementById('composer-toolbar');

        let pendingDeleteApprovalId = null;
        let messageRawStore = new WeakMap();

        function showToast(message, kind = 'info') {
            if (!toastHost || !message) return;
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
            { name: '/think', description: 'Set thinking/reasoning level (off/low/medium/high/max)' },
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
            if (!pendingDeleteApprovalId || !ws || ws.readyState !== WebSocket.OPEN) {
                deleteOverlay.classList.remove('active');
                pendingDeleteApprovalId = null;
                return;
            }
            ws.send(JSON.stringify({
                type: 'delete_approval_response',
                approvalId: pendingDeleteApprovalId,
                approved: approved
            }));
            deleteOverlay.classList.remove('active');
            pendingDeleteApprovalId = null;
            inputArea.disabled = false;
            sendBtn.disabled = false;
        }

        deleteAllowBtn.onclick = () => respondDeleteApproval(true);
        deleteDenyBtn.onclick = () => respondDeleteApproval(false);

        function showDeleteApproval(data) {
            pendingDeleteApprovalId = data.approvalId;
            deleteReason.textContent = data.reason ? `Requested by: ${data.reason}` : 'The agent wants to delete files.';
            deletePaths.textContent = (data.paths || []).map(p => `- ${p}`).join('\n');
            deleteOverlay.classList.add('active');
            inputArea.disabled = true;
            sendBtn.disabled = true;
        }

        // ── Toolbar: model picker popover ──
        let tbModelPopoverOpen = false;

        function toggleModelPopover() {
            tbModelPopoverOpen = !tbModelPopoverOpen;
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

        tbModelBtn.addEventListener('click', (e) => {
            e.stopPropagation();
            if (availableModels.length <= 1) {
                // Fetch models if we don't have a list yet
                modelsRequested = false;
                ensureModelsLoaded({ showLoading: true });
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
            for (const m of models) {
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
                    ws.send(JSON.stringify({ type: 'set_model', model: m.id }));
                    closeModelPopover();
                });
                tbModelList.appendChild(row);
            }
        }

        // ── Toolbar: thinking level chips ──
        const THINKING_LABELS = { off: 'Off', minimal: 'Mi', low: 'L', medium: 'M', high: 'H', xhigh: 'XH', max: 'Max' };

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
                    ws.send(JSON.stringify({ type: 'set_thinking_level', thinkingLevel: key }));
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
                ws.send(JSON.stringify({ type: 'set_mode', mode }));
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
        function updateToolbarContext(data) {
            if (!tbContextBadge) return;
            const used = (data && data.usedTokens) || 0;
            const limit = (data && data.contextLimit) || 0;
            if (used <= 0 || limit <= 0) {
                tbContextBadge.innerHTML = '<span class="ctx-ring"></span> —';
                tbContextBadge.removeAttribute('data-tone');
                tbContextBadge.title = 'Context usage unknown';
                return;
            }
            const pct = Math.min(100, Math.round((used / limit) * 100));
            const ring = document.createElement('span');
            ring.className = 'ctx-ring';
            ring.style.setProperty('--ctx-pct', pct + '%');
            const label = `${pct}% ${formatTokenCount(used)}/${formatTokenCount(limit)}`;
            tbContextBadge.innerHTML = '';
            tbContextBadge.appendChild(ring);
            tbContextBadge.appendChild(document.createTextNode(' ' + label));
            tbContextBadge.title = `Context: ${used.toLocaleString()} / ${limit.toLocaleString()} tokens`;
            const tone = pct >= 90 ? 'danger' : pct >= 75 ? 'warning' : '';
            if (tone) tbContextBadge.setAttribute('data-tone', tone);
            else tbContextBadge.removeAttribute('data-tone');
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
            modeActBtn.classList.toggle('active', currentMode === 'act');
            modePlanBtn.classList.toggle('active', currentMode === 'plan');
            updateToolbarMode(currentMode);
        }

        function updateGlobalMode(isGlobal) {
            if (globalModeBadge) globalModeBadge.classList.toggle('visible', !!isGlobal);
        }

        modeActBtn.onclick = () => {
            if (!ws || ws.readyState !== WebSocket.OPEN) return;
            ws.send(JSON.stringify({ type: 'set_mode', mode: 'act' }));
        };
        modePlanBtn.onclick = () => {
            if (!ws || ws.readyState !== WebSocket.OPEN) return;
            ws.send(JSON.stringify({ type: 'set_mode', mode: 'plan' }));
        };

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
            if (availableModels.length > 1) {
                modelInfoDiv.hidden = true;
                modelInfoDiv.classList.remove('clickable');
                modelSelect.hidden = false;
                if (model && [...modelSelect.options].some((o) => o.value === model)) {
                    modelSelect.value = model;
                } else if (!model) {
                    modelSelect.value = '';
                }
            } else {
                // Catalog not loaded yet (or only one model): show the label.
                // Keep it clickable so users can trigger a catalog fetch — the
                // <select> stays hidden until we have options, so focus/pointer
                // listeners on modelSelect alone never fire.
                modelSelect.hidden = true;
                modelInfoDiv.hidden = false;
                modelInfoDiv.textContent = model || '—';
                modelInfoDiv.classList.add('clickable');
                modelInfoDiv.title = availableModels.length === 0
                    ? 'Click to load available models'
                    : 'Only one model available';
            }
            // Update toolbar model button
            if (tbModelBtn) {
                const name = model || (availableModels.length > 0 ? availableModels.find(m => m.current)?.id : null) || '—';
                tbModelBtn.innerHTML = name + ' <span class="tb-arrow">▾</span>';
            }
        }

        const thinkingInfoEl = document.getElementById('thinking-info');

        function updateThinkingInfo(level) {
            if (!thinkingInfoEl) return;
            const labels = { off: 'Off', minimal: 'Minimal', low: 'Low', medium: 'Medium', high: 'High', xhigh: 'Extra high', max: 'Maximum' };
            thinkingInfoEl.textContent = labels[level] || 'Off';
            // Update toolbar thinking chips
            renderThinkingChips(level);
        }

        function updateModelSelect(models, current) {
            availableModels = Array.isArray(models) ? models : [];
            modelSelect.innerHTML = '';
            if (availableModels.length > 1) {
                const placeholder = document.createElement('option');
                placeholder.value = '';
                placeholder.disabled = true;
                placeholder.textContent = 'Select a model…';
                modelSelect.appendChild(placeholder);
                for (const m of availableModels) {
                    const opt = document.createElement('option');
                    opt.value = m.id;
                    opt.textContent = m.contextLimit
                        ? `${m.id} (${formatTokenCount(m.contextLimit)})`
                        : m.id;
                    modelSelect.appendChild(opt);
                }
                const selected = current || availableModels.find((m) => m.current)?.id || '';
                if (selected && [...modelSelect.options].some((o) => o.value === selected)) {
                    modelSelect.value = selected;
                    placeholder.selected = false;
                } else {
                    placeholder.selected = true;
                }
            }
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

        modelSelect.onchange = () => {
            if (!ws || ws.readyState !== WebSocket.OPEN) return;
            const id = modelSelect.value;
            if (!id) return;
            ws.send(JSON.stringify({ type: 'set_model', model: id }));
        };

        // Fetch the model catalog lazily so it never blocks initial connect.
        // The select is hidden until the catalog arrives, so also allow a click
        // on the model label and a post-paint idle fetch.
        let modelsRequested = false;
        function ensureModelsLoaded(opts) {
            if (modelsRequested) return;
            if (!ws || ws.readyState !== WebSocket.OPEN) return;
            modelsRequested = true;
            if (opts && opts.showLoading && availableModels.length <= 1 && !modelInfoDiv.hidden) {
                modelInfoDiv.textContent = 'Loading models…';
            }
            ws.send(JSON.stringify({ type: 'list_models' }));
        }
        modelSelect.addEventListener('focus', () => ensureModelsLoaded(), { once: true });
        modelSelect.addEventListener('pointerdown', () => ensureModelsLoaded(), { once: true });
        modelInfoDiv.addEventListener('click', () => {
            if (availableModels.length > 1) return;
            modelsRequested = false;
            ensureModelsLoaded({ showLoading: true });
        });

        function updateContextInfo(data) {
            lastContextData = data || lastContextData;
            const used = data.usedTokens || 0;
            const limit = data.contextLimit || 0;
            if (data.usedSource !== 'estimated') {
                contextBaseUsed = used;
                contextLimit = limit;
                contextEstAdded = 0;
            }
            const source = data.usedSource === 'api' ? '' : (used > 0 ? 'estimated' : '');

            if (used <= 0 && limit <= 0) {
                contextText.textContent = '—';
                contextBar.style.width = '0%';
                contextBar.className = '';
                updateToolbarContext(null);
                return;
            }

            let text = `${formatTokenCount(used)} / ${formatTokenCount(limit)}`;
            if (limit > 0 && data.usedPercent > 0) {
                const pct = Math.min(100, Math.round(data.usedPercent * 100));
                text += ` (${pct}%)`;
            }
            if (source) text += ` · ${source}`;
            if (data.messageCount > 0) text += `\n${data.messageCount} msgs`;
            // near-compact & tool-truncated indicators removed to avoid context stats height oscillation
            // causing layout jumps (used to be: if (data.nearCompact) text += '\nnear auto-compact';)
            // if (data.toolTruncated) text += '\nTool results truncated'; — removed for same reason
            contextText.textContent = text;

            if (limit > 0 && used > 0) {
                const pct = Math.min(100, (used / limit) * 100);
                contextBar.style.width = `${pct}%`;
                contextBar.className = '';
                if (data.nearCompact || pct >= 75) contextBar.classList.add('warn');
                if (pct >= 90) contextBar.classList.add('danger');
            } else {
                contextBar.style.width = '0%';
                contextBar.className = '';
            }
            // Update toolbar context badge
            updateToolbarContext(data);
        }

        function applyServerConfig(data) {
            console.log('[pricing] applyServerConfig', { model: data.model, inputPrice: data.inputPricePer1M, totalTurns: data.totalTurns });
            if (data.workingDir) dirInput.value = data.workingDir;
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
                console.log('[pricing] SET from config message', data.inputPricePer1M);
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
            stickToBottom = true;
            msgIdxCounter = 0;

            streamRafPending = false;
            streamLastRender = 0;
            endStream();
            currentThinkingDiv = null;
            currentThinkingSpan = null;
            streamingToolCards = {};
            pendingToolCards = {};
            historyToolCallArgs = {};
            toolsStartedThisTurn = false;
            setTurnActive(false);
            setInputProgress(null);
            scrollBottomBtn.classList.remove('visible');
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
        let lastSmartScrollTime = 0;
        // Start with anchoring disabled while following the stream.
        messagesDiv.classList.add('no-anchor');
        let currentStreamDiv = null;
        let currentStreamRaw = '';
        const STREAM_RENDER_INTERVAL = 32; // ms between renders (~2 frames at 60fps)
        let streamRafPending = false;
        let streamLastRender = 0;
        let currentThinkingDiv = null;
        let currentThinkingSpan = null;
        let pendingToolCards = {};
        let streamingToolCards = {};
        let toolCallCounter = 0;
        let turnActive = false;
        let toolsStartedThisTurn = false;
        let contextBaseUsed = 0;
        let contextLimit = 0;
        let contextEstAdded = 0;
        let lastContextData = null;
        // Map toolCallId → args for history replay of patch_file diffs
        let historyToolCallArgs = {};
        // Monotonic counter for message positions (used for forking)
        let msgIdxCounter = 0;

        function setTurnActive(active) {
            turnActive = !!active;
            if (cancelBtn) cancelBtn.disabled = !turnActive;
            if (sendBtn) sendBtn.disabled = false; // send cancels+restarts; keep enabled
            // Toggle class so Send/Cancel swap places
            document.getElementById('input-area').classList.toggle('turn-active', turnActive);
            if (!active) {
                toolsStartedThisTurn = false;
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
            const used = contextBaseUsed + contextEstAdded;
            const data = Object.assign({}, lastContextData || {}, {
                usedTokens: used,
                contextLimit: contextLimit || (lastContextData && lastContextData.contextLimit) || 0,
                usedSource: 'estimated',
                usedPercent: contextLimit > 0 ? used / contextLimit : 0,
                toolTruncated: false,
            });
            // Mark estimated in the text via updateContextInfo source label
            updateContextInfo(data);
            // Append est marker
            if (contextText.textContent && !contextText.textContent.includes('(est.)')) {
                contextText.textContent += ' (est.)';
            }
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

        function appendExpandableResult(body, result, success, truncated, options = {}) {
            const { language = '' } = options;
            const content = document.createElement('div');
            content.className = `tool-result-content ${success ? '' : 'error-content'}`;
            const full = result || '';
            const summary = summarizeResult(full, success);
            content.textContent = summary;
            body.appendChild(content);

            // Task 16: Copy button on tool results
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
                colorizeElement(content, language);
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
            removeThinkingStatus();
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
            colorizeCodeBlocks(textWrap);
        }

        // ===== Message timestamps =====
        function formatRelativeTime(date) {
            const diff = Math.max(0, Math.floor((Date.now() - date.getTime()) / 1000));
            if (diff < 5) return 'now';
            if (diff < 60) return `${diff}s ago`;
            if (diff < 3600) return `${Math.floor(diff / 60)}m ago`;
            if (diff < 86400) return `${Math.floor(diff / 3600)}h ago`;
            if (diff < 604800) return `${Math.floor(diff / 86400)}d ago`;
            return date.toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
        }

        function formatExactTime(date) {
            return date.toLocaleString(undefined, {
                year: 'numeric', month: 'short', day: 'numeric',
                hour: '2-digit', minute: '2-digit'
            });
        }

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
         *  For streaming messages (no histIdx) we pass -1 meaning "last assistant". */
        function forkSession(msgIdx) {
            if (!ws || ws.readyState !== WebSocket.OPEN) return;
            if (resendAwaitingHistory) {
                showToast('Resend already in progress', 'info');
                return;
            }
            ws.send(JSON.stringify({ type: 'session_fork', messageIndex: msgIdx }));
            pendingSessionResponse = true;
        }

        function cancelActiveTurn() {
            if (!turnActive || !ws || ws.readyState !== WebSocket.OPEN) return;
            suppressTurnEnds++;
            ws.send(JSON.stringify({ type: 'cancel' }));
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
            stickToBottom = true;
            if (histIdx === 0) {
                ws.send(JSON.stringify({ type: 'session_new' }));
            } else {
                ws.send(JSON.stringify({ type: 'session_fork', messageIndex: histIdx - 1 }));
            }
        }

        function flushPendingResend() {
            if (!resendAwaitingHistory) return;
            const text = pendingResendContent;
            clearPendingResend();
            if (!text || !ws || ws.readyState !== WebSocket.OPEN) return;
            const el = appendMessage('user', text);
            if (el) el.dataset.pendingAck = '1';
            streamingToolCards = {};
            pendingToolCards = {};
            toolsStartedThisTurn = false;
            contextEstAdded = 0;
            ws.send(JSON.stringify({ type: 'message', content: text }));
            setTurnActive(true);
            stickToBottom = true;
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

        function appendMessage(role, text) {
            return appendMessageAtTime(role, text, new Date());
        }

        function appendMessageAtTime(role, text, date, histIdx) {
            const msgDiv = document.createElement('div');
            msgDiv.className = `message ${role}`;
            const idx = msgIdxCounter++;
            msgDiv.dataset.msgIdx = idx;
            if (histIdx !== undefined && histIdx >= 0) {
                msgDiv.dataset.histIdx = histIdx;
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
            setMessageMarkdown(currentStreamDiv, currentStreamRaw);
            if (keepCursor) currentStreamDiv.classList.add('cursor');
            // Scroll after DOM height actually changes (tokens arrive before paint).
            smartScroll();
            // Notify scroll system that DOM height may have changed
            window.dispatchEvent(new CustomEvent('gogen-colorized', { bubbles: false }));
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
            smartScroll();
        }

        function showThinking() {
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

            const body = document.createElement('div');
            body.style.cssText = 'font-size:0.85em;color:var(--fg-muted);font-style:italic;white-space:pre-wrap;padding:4px 0 0 0;';

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
            currentThinkingSpan.textContent += token;
            bumpContextEstimate(token);
            smartScroll();
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

        function removeThinkingStatus() {
            const el = document.getElementById('status-thinking');
            if (el) el.remove();
        }

        function finalizeThinking() {
            removeThinkingStatus();
            if (!currentThinkingDiv) return;
            const div = currentThinkingDiv;
            const body = currentThinkingSpan;
            const content = (body ? body.textContent : '').trim();
            currentThinkingDiv = null;
            currentThinkingSpan = null;
            if (!content) {
                div.remove();
                return;
            }
            // Card is already fully structured from showThinking() —
            // the collapsible toggle is already active, no rebuild needed.
        }

        function endStream() {
            if (currentStreamDiv) {
                flushStreamRender();
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
        });

        // Refresh relative timestamps every 30 seconds
        setInterval(() => {
            const now = Date.now();
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
            // Task 8: Make file paths clickable
            const FILE_KEYS = ['file_path', 'path', 'source', 'destination', 'glob'];
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
                rafScheduled: false,
                scheduleDiffUpdate(text) {
                    this.pendingDiff = text || '';
                    updateDiffFallback(this.monacoHost, this.pendingDiff);
                    if (this.rafScheduled) return;
                    this.rafScheduled = true;
                    requestAnimationFrame(() => {
                        this.rafScheduled = false;
                        if (this.monacoEditor) updateDiffEditor(this.monacoEditor, this.pendingDiff);
                    });
                },
                setCollapsed: (c) => { toggle.classList.toggle('collapsed', c); },
            };
            return cardInfo;
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
                cardInfo.monacoHost = document.createElement('div');
                cardInfo.monacoHost.className = 'monaco-tool-host';
                // Insert before waiting/status if present
                const waiting = cardInfo.card.querySelector('.tool-waiting');
                if (waiting) cardInfo.card.insertBefore(cardInfo.monacoHost, waiting);
                else cardInfo.card.appendChild(cardInfo.monacoHost);
            }
            updateDiffFallback(cardInfo.monacoHost, cardInfo.pendingDiff || '');

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
            const extracted = extractDiffValue(cardInfo.rawArgs);
            if (tool === 'patch_file' || extracted.ok) {
                cardInfo.toolName = 'patch_file';
                if (extracted.ok) {
                    ensurePatchViewer(cardInfo, extracted.value).catch(() => {});
                } else {
                    ensurePatchViewer(cardInfo, cardInfo.pendingDiff || '').catch(() => {});
                }
                // Compact non-diff keys in the header once JSON is parseable.
                const compact = formatArgsCompact(cardInfo.rawArgs);
                if (compact) cardInfo.toolArgs.textContent = compact;
                smartScroll();
                return;
            }

            // Non-patch tools: show compact args only when JSON is complete enough.
            const compact = formatArgsCompact(cardInfo.rawArgs);
            if (compact) {
                cardInfo.toolArgs.textContent = compact;
                if (cardInfo.argsStream) {
                    cardInfo.argsStream.remove();
                    cardInfo.argsStream = null;
                }
            } else if (cardInfo.argsStream) {
                // Show accumulating raw args so the user sees live progress
                cardInfo.argsStream.textContent = cardInfo.rawArgs;
            }
            smartScroll();
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

            if (name === 'patch_file') {
                // Ensure the patch (from args) is visible; result is usually a short summary.
                if (pending) {
                    await ensurePatchViewer(cardInfo, pending);
                }
                if (result) {
                    appendExpandableResult(body, result, success, truncated);
                }
                card.appendChild(body);
            } else if (name === 'show_diff') {
                const host = document.createElement('div');
                host.className = 'monaco-tool-host';
                body.appendChild(host);
                card.appendChild(body);
                await mountDiffEditor(host, result || '');
            } else if (name === 'read_file') {
                const path = readFilePathFromArgs(cardInfo.args);
                appendExpandableResult(body, result, success, truncated, {
                    language: languageFromPath(path),
                });
                card.appendChild(body);
            } else {
                appendExpandableResult(body, result, success, truncated);
                card.appendChild(body);
            }

            smartScroll();
        }

        async function replayHistory(history) {
            // Session/history loads should always land at the bottom.
            stickToBottom = true;
            historyToolCallArgs = {};
            function msgDate(createdAt) {
                return createdAt ? new Date(createdAt) : new Date();
            }
            for (const h of history) {
                if (h.role === 'user') {
                    appendMessageAtTime('user', h.content || '', msgDate(h.createdAt), h.index);
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
                        body.style.cssText = 'font-size:0.85em;color:var(--fg-muted);font-style:italic;white-space:pre-wrap;padding:4px 0 0 0;';
                        body.textContent = h.reasoning;
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
                    if (h.content) appendMessageAtTime('assistant', h.content, msgDate(h.createdAt), h.index);
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
                                await ensurePatchViewer(cardInfo, tc.args.diff);
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
                    await updateToolCardWithResult(cardInfo, h.content || '', success, false, cardInfo.toolName);
                }
            }
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

        function connect() {
            const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
            ws = new WebSocket(`${protocol}//${window.location.host}/ws`);
            setWebSocket(ws);

            ws.onopen = () => {
                clearTimeout(reconnectTimer);
                reconnectTimer = null;
                setConnState('connected');
                // Replace (don't append) on every connect: the server may omit
                // history when the session is empty, and reconnect must not
                // keep a stale transcript. Connection status lives in the
                // indicator, not as chat system messages.
                clearChat();
                // Reset the lazy model-catalog guard so a fresh connection can
                // fetch on next interaction; re-arm the one-shot listeners.
                modelsRequested = false;
                modelSelect.addEventListener('focus', () => ensureModelsLoaded(), { once: true });
                modelSelect.addEventListener('pointerdown', () => ensureModelsLoaded(), { once: true });
                // Only request the cheap, local session list eagerly. The
                // model catalog is a remote /v1/models call that can dominate
                // startup latency, so fetch it after first paint (idle) and
                // also on click of the model label / select.
                ws.send(JSON.stringify({ type: 'list_sessions' }));
                const scheduleModels = window.requestIdleCallback
                    || ((fn) => setTimeout(fn, 200));
                scheduleModels(() => ensureModelsLoaded());

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
                if (handleServerMessage(data)) return;

                if (data.type === 'thinking') {
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
                            data.success !== false,
                            data.resultTruncated,
                            data.tool || cardInfo.toolName
                        );
                        delete pendingToolCards[data.toolCallId];
                    } else {
                        appendMessage('system', `[${data.tool}] ${data.result}`);
                    }
                } else if (data.type === 'cancelled') {
                    // Stale cancel from a turn we replaced with resend/send — ignore.
                    if (suppressTurnEnds > 0) return;
                    abortInFlightUI(data.content || 'Cancelled.');
                    setTurnActive(false);
                    setInputProgress(null);
                } else if (data.type === 'turn_end') {
                    // Ignore turn_end from a turn we cancelled to start a resend/send.
                    // Otherwise it clears pendingSessionResponse / turnActive mid-flow.
                    if (suppressTurnEnds > 0) {
                        suppressTurnEnds--;
                        return;
                    }
                    setTurnActive(false);
                    updateTitle('idle');
                    setInputProgress(null);
                    pendingSessionResponse = false;
                } else if (data.type === 'clear_chat') {
                    clearChat();
                    pendingSessionResponse = true;
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
                    finalizeThinking();
                    endStream();
                    // Structured list only — never dump slash help text into chat.
                    renderSessionList(data.sessions || []);
                    if (data.sessionId) sessionInfoDiv.textContent = data.sessionId;
                    if (data.contextLimit || data.usedTokens) { updateContextInfo(data); }
                } else if (data.type === 'response') {
                    finalizeThinking();
                    endStream();
                    if (pendingSessionResponse) {
                        pendingSessionResponse = false;
                        if (data.sessionId) sessionInfoDiv.textContent = data.sessionId;
                        updateContextInfo(data);
                        const msg = (data.content || '').split('\n')[0] || 'Session updated';
                        if (String(data.content || '').startsWith('Error:')) {
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
                    // restore never stacks duplicate transcripts.
                    clearChat();
                    const afterHistory = () => {
                        if (resendAwaitingHistory) flushPendingResend();
                    };
                    if (data.history && data.history.length) {
                        replayHistory(data.history).then(afterHistory).catch((err) => {
                            console.warn('history replay failed', err);
                            for (const h of data.history) {
                                if (h.role === 'user' || h.role === 'assistant') {
                                    if (h.content) {
                                        appendMessageAtTime(
                                            h.role,
                                            h.content,
                                            h.createdAt ? new Date(h.createdAt) : new Date(),
                                            h.index
                                        );
                                    }
                                }
                            }
                            afterHistory();
                        });
                    } else {
                        afterHistory();
                    }
                } else if (data.type === 'config') {
                    applyServerConfig(data);
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
                setTurnActive(false);
                setInputProgress(null);
                clearTimeout(reconnectTimer);
                setConnState('reconnecting');
                wasDisconnected = true;
                reconnectTimer = setTimeout(connect, 3000);
            };
        }

        sendBtn.onclick = () => {
            const text = inputArea.value.trim();
            if (!text) return;
            if (!ws || ws.readyState !== WebSocket.OPEN) {
                appendMessage('system', 'Not connected — wait for reconnection.');
                return;
            }
            if (resendAwaitingHistory) {
                showToast('Resend already in progress', 'info');
                return;
            }
            // Sending while busy cancels the current turn (same as TUI interrupt + new prompt).
            cancelActiveTurn();
            hideSlashSuggest();
            stickToBottom = true;
            const el = appendMessage('user', text);
            if (el) el.dataset.pendingAck = '1';
            streamingToolCards = {};
            pendingToolCards = {};
            toolsStartedThisTurn = false;
            contextEstAdded = 0;
            ws.send(JSON.stringify({ type: 'message', content: text }));
            inputArea.value = '';
            inputArea.style.height = 'auto';
        };

        cancelBtn.onclick = () => {
            if (!ws || ws.readyState !== WebSocket.OPEN) return;
            if (!turnActive) return;
            ws.send(JSON.stringify({ type: 'cancel' }));
            // Tear down immediately; server also emits cancelled + turn_end.
            abortInFlightUI(null);
            setTurnActive(false);
        };

        inputArea.addEventListener('input', updateSlashSuggest);

        /* Task 1: Auto-resize textarea */
        inputArea.addEventListener('input', () => {
            inputArea.style.height = 'auto';
            inputArea.style.height = Math.min(inputArea.scrollHeight, 200) + 'px';
        });

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
                sendBtn.onclick();
            }
            if (e.key === 'Escape' && turnActive) {
                e.preventDefault();
                cancelBtn.onclick();
            }
        };

        setDirBtn.onclick = () => {
            const dir = dirInput.value;
            if (!dir) return;
            if (!ws || ws.readyState !== WebSocket.OPEN) {
                showToast('Not connected', 'error');
                return;
            }
            ws.send(JSON.stringify({ type: 'config', workingDir: dir }));
            showToast('Working directory updated', 'success');
            setTimeout(() => requestSessionList(), 100);
        };

        function requestSessionList() {
            if (!ws || ws.readyState !== WebSocket.OPEN) return;
            ws.send(JSON.stringify({ type: 'list_sessions' }));
        }

        function relativeTime(iso) {
            if (!iso) return '';
            const t = Date.parse(iso);
            if (Number.isNaN(t)) return '';
            const sec = Math.round((Date.now() - t) / 1000);
            if (sec < 60) return 'just now';
            if (sec < 3600) return `${Math.floor(sec / 60)}m ago`;
            if (sec < 86400) return `${Math.floor(sec / 3600)}h ago`;
            if (sec < 86400 * 7) return `${Math.floor(sec / 86400)}d ago`;
            return new Date(t).toLocaleDateString();
        }

        function renderSessionList(sessions) {
            if (!sessionListDiv) return;
            sessionListDiv.innerHTML = '';
            if (!sessions || !sessions.length) {
                const empty = document.createElement('div');
                empty.className = 'session-list-empty';
                empty.textContent = 'No saved sessions';
                sessionListDiv.appendChild(empty);
                return;
            }
            for (const s of sessions) {
                const row = document.createElement('div');
                row.className = 'session-row' + (s.current ? ' current' : '');
                row.title = s.label || s.id;
                const content = document.createElement('div');
                content.className = 'session-row-content';
                const title = document.createElement('div');
                title.className = 'session-row-title';
                title.textContent = s.label || s.id || '(unknown)';
                const meta = document.createElement('div');
                meta.className = 'session-row-meta';
                const parts = [];
                if (s.messageCount != null) parts.push(`${s.messageCount} msgs`);
                const rel = relativeTime(s.updatedAt);
                if (rel) parts.push(rel);
                if (s.current) parts.push('current');
                meta.textContent = parts.join(' · ');
                content.appendChild(title);
                content.appendChild(meta);
                content.onclick = () => {
                    if (s.current) {
                        showToast('Already on this session', 'info');
                        return;
                    }
                    resumeSession(s.id);
                };
                row.appendChild(content);
                // Delete button (hidden by default, shown on row hover)
                const delBtn = document.createElement('button');
                delBtn.className = 'session-row-del';
                delBtn.textContent = '✕';
                delBtn.title = 'Delete this session';
                delBtn.onclick = (e) => {
                    e.stopPropagation();
                    deleteSession(s.id, s.label);
                };
                row.appendChild(delBtn);
                sessionListDiv.appendChild(row);
            }
        }

        /**
         * Update the current session row's title in the sidebar without
         * re-rendering the entire list.
         */
        function updateCurrentSessionLabel(label) {
            if (!label || !sessionListDiv) return;
            const currentRow = sessionListDiv.querySelector('.session-row.current');
            if (!currentRow) return;
            const titleEl = currentRow.querySelector('.session-row-title');
            if (!titleEl) return;
            if (titleEl.textContent === label) return; // already up-to-date
            titleEl.textContent = label;
            // Also update the row's tooltip with the label.
            currentRow.title = label;
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
            pendingSessionResponse = true;
            ws.send(JSON.stringify({ type: 'session_new' }));
        }

        function deleteSession(id, label) {
            if (!ws || ws.readyState !== WebSocket.OPEN) {
                showToast('Not connected', 'error');
                return;
            }
            if (!id) return;
            if (resendAwaitingHistory) {
                showToast('Resend already in progress', 'info');
                return;
            }
            const displayName = label || id;
            if (!confirm(`Are you sure you want to delete session "${displayName}"?\n\nThis action cannot be undone.`)) {
                return;
            }
            pendingSessionResponse = true;
            ws.send(JSON.stringify({ type: 'session_delete', sessionId: id }));
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
            pendingSessionResponse = true;
            ws.send(JSON.stringify({ type: 'session_resume', sessionId: id }));
        }

        function switchMainPane(pane) {
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
            const sessionId = (sessionInfoDiv.textContent || 'session').replace(/[^\w.-]+/g, '_');
            const date = new Date().toISOString().slice(0, 10);
            lines.push(`# GoGen chat (${sessionId})`);
            lines.push('');
            for (const el of messagesDiv.querySelectorAll('.message')) {
                if (el.classList.contains('system') || el.classList.contains('thinking') || el.classList.contains('thinking-block')) {
                    continue;
                }
                if (el.classList.contains('tool-card') || el.classList.contains('thought-card')) continue;
                const role = el.classList.contains('user') ? 'user'
                    : el.classList.contains('assistant') ? 'assistant' : null;
                if (!role) continue;
                const raw = messageRawStore.get(el);
                const body = (raw != null ? raw : el.textContent || '').trim();
                if (!body) continue;
                lines.push(`## ${role}`);
                lines.push('');
                lines.push(body);
                lines.push('');
            }
            const blob = new Blob([lines.join('\n')], { type: 'text/markdown;charset=utf-8' });
            const a = document.createElement('a');
            a.href = URL.createObjectURL(blob);
            a.download = `gogen-chat-${sessionId}-${date}.md`;
            a.click();
            URL.revokeObjectURL(a.href);
            showToast('Chat exported', 'success');
        }

        const PALETTE_COMMANDS = [
            { id: 'new-session', label: 'New session', hint: '', run: () => newSession() },
            { id: 'resume-latest', label: 'Resume latest session', hint: '', run: () => resumeSession('latest') },
            { id: 'refresh-sessions', label: 'Refresh sessions', hint: '', run: () => requestSessionList() },
            { id: 'toggle-mode', label: 'Toggle Act / Plan mode', hint: '', run: () => {
                ws?.send(JSON.stringify({ type: 'set_mode', mode: currentMode === 'plan' ? 'act' : 'plan' }));
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
                if (ws?.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type: 'message', content: '/compact' }));
            }},
            { id: 'context-detail', label: 'Show context usage', hint: '', run: () => {
                if (ws?.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type: 'message', content: '/context' }));
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
            paletteOverlay.classList.remove('open');
            paletteOverlay.setAttribute('aria-hidden', 'true');
            paletteInput.value = '';
            paletteList.innerHTML = '';
        }

        function openPalette() {
            paletteOverlay.classList.add('open');
            paletteOverlay.setAttribute('aria-hidden', 'false');
            paletteInput.value = '';
            paletteIndex = 0;
            renderPalette();
            paletteInput.focus();
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
            if (e.key === 'Escape') {
                if (settingsOverlay.classList.contains('active')) {
                    e.preventDefault();
                    settingsOverlay.classList.remove('active');
                    return;
                }
                if (paletteOverlay.classList.contains('open')) {
                    e.preventDefault();
                    closePalette();
                    return;
                }
                if (!typing && turnActive) {
                    e.preventDefault();
                    cancelBtn.onclick();
                }
            }
        });

        // === Task 3: Scroll-to-bottom button ===
        const scrollBottomBtn = document.getElementById('scroll-bottom-btn');
        const NEAR_BOTTOM_PX = 32;

        function distanceFromBottom() {
            return messagesDiv.scrollHeight - messagesDiv.scrollTop - messagesDiv.clientHeight;
        }

        function updateScrollBottomBtn() {
            scrollBottomBtn.classList.toggle('visible', !stickToBottom || distanceFromBottom() > NEAR_BOTTOM_PX);
        }

        function unpinFromBottom() {
            if (!stickToBottom) return;
            stickToBottom = false;
            updateScrollBottomBtn();
            // Re-enable browser scroll anchoring now that the user is reading
            // (prevents content from jumping when the streaming div changes height).
            messagesDiv.classList.remove('no-anchor');
        }

        function pinToBottom() {
            stickToBottom = true;
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
        messagesDiv.addEventListener('wheel', (e) => {
            if (e.deltaY < 0) unpinFromBottom();
        }, { passive: true });
        messagesDiv.addEventListener('touchstart', () => {
            // Touch scroll direction is known on touchmove; mark intent on any
            // touch interaction away from bottom after move.
            messagesDiv._touchY = null;
        }, { passive: true });
        messagesDiv.addEventListener('touchmove', (e) => {
            const y = e.touches[0]?.clientY;
            if (y == null) return;
            if (messagesDiv._touchY != null && y > messagesDiv._touchY + 2) {
                // Finger moved down → content scrolls up
                unpinFromBottom();
            }
            messagesDiv._touchY = y;
        }, { passive: true });
        messagesDiv.addEventListener('keydown', (e) => {
            if (e.key === 'ArrowUp' || e.key === 'PageUp' || e.key === 'Home') unpinFromBottom();
        });

        // Throttle scroll handler to rAF to avoid layout thrashing during streaming.
        let _scrollPending = false;
        messagesDiv.addEventListener('scroll', () => {
            if (_scrollPending) return;
            _scrollPending = true;
            requestAnimationFrame(() => {
                _scrollPending = false;
                if (ignoreScrollEvent) return;
                // Skip if a programmatic smartScroll() happened recently —
                // the DOM may have grown since the scroll, making the
                // distance check unreliable (causes false unpin during streaming).
                if (performance.now() - lastSmartScrollTime < 100) return;
                const d = distanceFromBottom();
                if (d > NEAR_BOTTOM_PX) {
                    // Clearly away from bottom (scrollbar drag, etc.)
                    stickToBottom = false;
                    messagesDiv.classList.remove('no-anchor');
                } else if (d <= 8) {
                    // Truly back at the bottom — re-enable follow
                    stickToBottom = true;
                    messagesDiv.classList.add('no-anchor');
                }
                // 8 < d <= NEAR_BOTTOM_PX: leave stickToBottom alone so a small
                // upward scroll after wheel-unpin is not immediately re-pinned.
                scrollBottomBtn.classList.toggle('visible', !stickToBottom);
            });
        });
        scrollBottomBtn.addEventListener('click', () => {
            pinToBottom();
        });

        // Async code colorization changes DOM height after we've already scrolled.
        // Re-adjust if we're still pinned to the bottom.
        window.addEventListener('gogen-colorized', () => {
            if (stickToBottom) {
                smartScroll();
            }
        });

        // Auto-scroll only while stickToBottom is set (user hasn't scrolled away).
        // The immediate scrollTop gives low-latency follow during streaming
        // (each token triggers smartScroll).  The gogen-colorized event
        // re-invokes smartScroll after Monaco finishes coloring, so we do
        // NOT need a duplicate rAF double-check here.
        function smartScroll() {
            if (!stickToBottom) return;
            ignoreScrollEvent = true;
            messagesDiv.scrollTop = messagesDiv.scrollHeight;
            lastSmartScrollTime = performance.now();
            ignoreScrollEvent = false;
        }

        // === Task 4: Collapsible sidebar toggle ===
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

        // === Task 9: Favicon (handled via link tag) ===

        // === Task 10: Page title updates ===
        const baseTitle = 'GoGen AI Agent';
        function updateTitle(status) {
            if (status === 'thinking' || status === 'streaming') {
                document.title = `⏳ ${baseTitle}`;
            } else if (status === 'disconnected') {
                document.title = `🔴 ${baseTitle}`;
            } else {
                document.title = baseTitle;
            }
        }
        const origSetConnState = setConnState;
        // Patch setConnState to also update title
        const _origSetConnState = window.setConnState;
        // Override appendMessage to detect activity
        const origAppendMessage = appendMessage;
        // We use MutationObserver on messages div to detect streaming
        const titleObserver = new MutationObserver(() => {
            const streaming = messagesDiv.querySelector('.message.assistant.cursor');
            if (currentProgressPhase === 'thinking') updateTitle('thinking');
            else if (streaming) updateTitle('streaming');
            else updateTitle('idle');
        });
        titleObserver.observe(messagesDiv, { childList: true, subtree: true, attributes: true });

        // === Task 11: Context tooltip ===
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
        contextInfoDiv.addEventListener('mouseenter', () => {
            const d = lastContextData || {};
            if (!d.contextLimit && !d.usedTokens) return;
            const lines = [];
            lines.push(`── Context ──`);
            if (d.usedTokens) lines.push(`Used: ${formatTokenCount(d.usedTokens)}`);
            if (d.contextLimit) lines.push(`Limit: ${formatTokenCount(d.contextLimit)}`);
            if (d.promptTokens) lines.push(`Prompt: ${formatTokenCount(d.promptTokens)}`);
            if (d.completionTokens) lines.push(`Completion: ${formatTokenCount(d.completionTokens)}`);
            if (d.cachedTokens) lines.push(`Cached: ${formatTokenCount(d.cachedTokens)}`);
            if (d.compactAt) lines.push(`Compact at: ${formatTokenCount(d.compactAt)}`);
            if (d.usedSource) lines.push(`Source: ${d.usedSource}`);
            const prompt = d.totalPromptTokens || 0;
            const completion = d.totalCompletionTokens || 0;
            const cached = d.totalCachedTokens || 0;
            const turns = d.totalTurns || 0;
            if (turns > 0 || prompt > 0 || currentModelPricing) {
                lines.push(`── Session ──`);
                if (turns > 0 || prompt > 0) {
                    lines.push(`Total: ${fmtTokK(prompt + completion)} · ${turns} turns`);
                    lines.push(`Prompt: ${fmtTokK(prompt)}`);
                    lines.push(`Completion: ${fmtTokK(completion)}`);
                    lines.push(`Cached: ${fmtTokK(cached)}`);
                }
                if (currentModelPricing) {
                    console.log('[pricing] tooltip cost', { turns, prompt, completion, cached, pricing: currentModelPricing });
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
        contextInfoDiv.addEventListener('mouseleave', () => {
            contextTooltip.style.display = 'none';
        });

        // === Settings modal ===
        const settingsBtn = document.getElementById('settings-btn');
        const settingsOverlay = document.getElementById('settings-overlay');
        const themeSelect = document.getElementById('theme-select');

        settingsBtn.addEventListener('click', () => {
            settingsOverlay.classList.add('active');
        });
        settingsOverlay.addEventListener('click', (e) => {
            if (e.target === settingsOverlay) settingsOverlay.classList.remove('active');
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

        function hexToRgba(hex, alpha) {
            if (!/^#[0-9a-f]{6}$/i.test(hex)) return null;
            const r = parseInt(hex.slice(1, 3), 16);
            const g = parseInt(hex.slice(3, 5), 16);
            const b = parseInt(hex.slice(5, 7), 16);
            return 'rgba(' + r + ', ' + g + ', ' + b + ', ' + alpha + ')';
        }

        function applyAccentColor(hex) {
            if (!hex) {
                document.documentElement.style.removeProperty('--user-accent');
                document.documentElement.style.removeProperty('--user-accent-soft');
                return;
            }
            const soft = hexToRgba(hex, 0.18);
            if (!soft) return;
            document.documentElement.style.setProperty('--user-accent', hex);
            document.documentElement.style.setProperty('--user-accent-soft', soft);
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
            document.documentElement.style.removeProperty('--user-accent-soft');
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

        // Detect turn_end by observing the cancel button's disabled attribute,
        // which toggles whenever setTurnActive is called.
        let _prevTurnActive = false;
        const _turnObserver = new MutationObserver(() => {
            const isActive = !cancelBtn.disabled;
            if (_prevTurnActive && !isActive) {
                sendNotification('GoGen', 'Agent finished responding.', 'gogen-turn-end');
            }
            _prevTurnActive = isActive;
        });
        _turnObserver.observe(cancelBtn, { attributes: true, attributeFilter: ['disabled'] });

        // Delete approval: always notify since it requires user action
        const _origShowDeleteApproval = showDeleteApproval;
        showDeleteApproval = function _showDeleteApprovalNotify(data) {
            _origShowDeleteApproval.call(null, data);
            const paths = (data.paths || []).join(', ');
            sendNotification('GoGen — Approval needed', `File delete requested: ${paths}`, 'gogen-delete-approval');
        };

        setTurnActive(false);
        setConnState('disconnected');
        connect();
