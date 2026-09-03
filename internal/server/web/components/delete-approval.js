// Delete-approval queue + confirm modal for the GoGen web UI.
//
// The server asks for approval before the agent deletes files. The
// modal is single-slot, so requests are QUEUED: a second session's
// approval request (e.g. a background pane) cannot orphan the first —
// overwriting the pending id would leave the first session's turn
// waiting forever on a channel that is never resolved. Each entry
// carries its sessionId so the response routes to the right session's
// runtime.
//
// While an approval is pending the composer is disabled (inputArea /
// send-btn) so the user cannot race the decision with a new turn.
//
// Wiring: app.js calls initDeleteApproval(deps) once at startup and
// forwards the ws 'delete_approval' payload to showDeleteApproval.
//   deps.getWs() — the chat WebSocket (or null)
import { openModal, closeModal } from '/editor.js';
import { sendNotification } from '/components/settings.js';

const inputArea = document.getElementById('message-input');
const sendBtn = document.getElementById('send-btn');
const deleteOverlay = document.getElementById('delete-approval-overlay');
const deleteReason = document.getElementById('delete-approval-reason');
const deletePaths = document.getElementById('delete-approval-paths');
const deleteAllowBtn = document.getElementById('delete-allow-btn');
const deleteDenyBtn = document.getElementById('delete-deny-btn');

let deps = null;

export function initDeleteApproval(d) {
    deps = d;
}

let pendingDeleteApprovals = []; // {approvalId, sessionId, reason, paths}

function renderDeleteApproval(data) {
    deleteReason.textContent = data.reason ? `Requested by: ${data.reason}` : 'The agent wants to delete files.';
    deletePaths.textContent = (data.paths || []).map(p => `- ${p}`).join('\n');
}

function respondDeleteApproval(approved) {
    const ws = deps.getWs();
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
        // The keydown listener was removed above; re-arm it so Esc
        // keeps resolving queued approvals too.
        deleteOverlay.addEventListener('keydown', deleteApprovalEsc);
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

export function showDeleteApproval(data) {
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
    // Always notify: delete approval requires user action even when
    // the tab is backgrounded.
    const paths = (data.paths || []).join(', ');
    sendNotification('GoGen — Approval needed', `File delete requested: ${paths}`, 'gogen-delete-approval');
}
