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
	// Editor writes serialize on the workspace filesystem lock (not the
	// session turn lock, Phase 2): they wait only for the actual mutation
	// window of a running tool, never for the whole streaming turn.
	s.ws.fsMu.Lock()
	defer s.ws.fsMu.Unlock()

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
	case "fs_apply_patch":
		// Applies a unified diff to files under the working directory using
		// the agent's patch engine (exact-context match, no fuzzy relocation).
		// Delete-only patches require approval and are rejected here; use the
		// agent for those.
		report, err := s.ws.Exec.PatchFile(ctx, msg.Diff, false, false)
		resp := WSMessage{Type: "fs_apply_patch_result", RequestID: reqID}
		if err != nil {
			resp.Error = err.Error()
			if report != "" {
				resp.Result = report
			}
		} else {
			resp.Success = true
			resp.Result = report
		}
		_ = ws.writeJSON(resp)
	}
}
