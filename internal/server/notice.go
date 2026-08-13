package server

// writeNotice sends a non-chat UI-feedback message ("notice") to a client.
// The client toasts it and never renders it into the chat transcript, so
// UI-channel handlers (board ops, settings toggles, working-dir input, model
// picker, sidebar refreshes) report errors here — see the message-type
// contract in ws_types.go. kind scopes the notice for client-side follow-ups
// ("board", "settings", "workspace", "model", "sessions", "models", …).
func writeNotice(ws *wsConn, kind string, success bool, content string) {
	if ws == nil {
		return
	}
	_ = ws.writeJSON(WSMessage{Type: "notice", Kind: kind, Success: success, Content: content})
}

// writeNoticeError is writeNotice for the common error case.
func writeNoticeError(ws *wsConn, kind, errMsg string) {
	writeNotice(ws, kind, false, errMsg)
}
