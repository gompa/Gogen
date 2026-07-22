package server

import (
	"context"
)

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
	case "fs_search":
		matches, truncated, err := s.fsSearch(ctx, msg.Pattern, path, msg.Glob)
		resp := WSMessage{Type: "fs_search_result", Path: path, RequestID: reqID, Pattern: msg.Pattern}
		if err != nil {
			resp.Error = err.Error()
		} else {
			resp.Success = true
			resp.Matches = matches
			resp.Truncated = truncated
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

func (s *Server) handleFSWriteMessage(ws *wsConn, ctx context.Context, msg WSMessage) {
	reqID := msg.RequestID
	path := msg.Path
	if !s.tryAcquireTurn(wsTurnAcquireWait) {
		resp := WSMessage{Type: msg.Type + "_result", Path: path, RequestID: reqID, Pattern: msg.Pattern}
		if msg.Type == "fs_write" {
			resp.Type = "fs_write_result"
		} else if msg.Type == "fs_replace" {
			resp.Type = "fs_replace_result"
		}
		resp.Error = "agent is busy with another client"
		_ = ws.writeJSON(resp)
		return
	}
	defer s.turnMu.Unlock()

	switch msg.Type {
	case "fs_write":
		resp := WSMessage{Type: "fs_write_result", Path: path, RequestID: reqID}
		if err := s.fsWrite(path, msg.Content); err != nil {
			resp.Error = err.Error()
		} else {
			resp.Success = true
		}
		_ = ws.writeJSON(resp)
	case "fs_replace":
		replaced, fileCount, err := s.fsReplace(ctx, msg.Pattern, msg.Replacement, msg.Path, msg.Glob)
		resp := WSMessage{Type: "fs_replace_result", Path: path, RequestID: reqID, Pattern: msg.Pattern}
		if err != nil {
			resp.Error = err.Error()
		} else {
			resp.Success = true
			resp.Replaced = replaced
			resp.FileCount = fileCount
		}
		_ = ws.writeJSON(resp)
	}
}
