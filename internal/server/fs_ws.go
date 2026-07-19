package server

import "context"

func (s *Server) handleFSReadMessage(ws *wsConn, ctx context.Context, msg WSMessage) {
	reqID := msg.RequestID
	path := msg.Path
	switch msg.Type {
	case "fs_list":
		entries, err := s.fsList(path)
		resp := WSMessage{Type: "fs_list_result", Path: path, RequestID: reqID}
		if err != nil {
			resp.Error = err.Error()
		} else {
			resp.Success = true
			resp.Entries = entries
		}
		_ = ws.writeJSON(resp)
	case "fs_read":
		content, lang, err := s.fsRead(path)
		resp := WSMessage{Type: "fs_read_result", Path: path, RequestID: reqID, Language: lang}
		if err != nil {
			resp.Error = err.Error()
		} else {
			resp.Success = true
			resp.Content = content
		}
		_ = ws.writeJSON(resp)
	case "git_status":
		entries, err := s.gitStatusEntries(ctx)
		resp := WSMessage{Type: "git_status_result", RequestID: reqID}
		if err != nil {
			resp.Error = err.Error()
		} else {
			resp.Success = true
			resp.GitEntries = entries
		}
		_ = ws.writeJSON(resp)
	case "git_file_diff":
		original, modified, lang, err := s.gitFileDiff(ctx, path)
		resp := WSMessage{
			Type:      "git_file_diff_result",
			Path:      path,
			RequestID: reqID,
			Language:  lang,
		}
		if err != nil {
			resp.Error = err.Error()
		} else {
			resp.Success = true
			resp.Original = original
			resp.Modified = modified
		}
		_ = ws.writeJSON(resp)
	}
}

func (s *Server) handleFSWriteMessage(ws *wsConn, msg WSMessage) {
	reqID := msg.RequestID
	path := msg.Path
	resp := WSMessage{Type: "fs_write_result", Path: path, RequestID: reqID}
	if !s.tryAcquireTurn(wsTurnAcquireWait) {
		resp.Error = "agent is busy with another client"
		_ = ws.writeJSON(resp)
		return
	}
	err := s.fsWrite(path, msg.Content)
	s.turnMu.Unlock()
	if err != nil {
		resp.Error = err.Error()
	} else {
		resp.Success = true
	}
	_ = ws.writeJSON(resp)
}
