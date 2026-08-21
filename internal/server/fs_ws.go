package server

import (
	"context"
	"strings"
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
		status, err := s.gitStatusEntries(ctx)
		resp := WSMessage{Type: "git_status_result", RequestID: reqID}
		if err != nil {
			resp.Error = err.Error()
		} else {
			resp.Success = true
			resp.GitStatus = &status
			// Legacy flat list (Unstaged+Untracked) kept for one release
			// cycle so a stale browser tab (pre-v2 panel) keeps working.
			legacy := make([]GitStatusEntry, 0, len(status.Unstaged)+len(status.Untracked))
			legacy = append(legacy, status.Unstaged...)
			legacy = append(legacy, status.Untracked...)
			resp.GitEntries = legacy
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
	case "git_commit_message":
		// One-shot LLM call over the staged diff — no chat session is
		// created or touched (see generateCommitMessage). Run it OFF the
		// read loop: the call can take up to gitCommitMessageTimeout (60s),
		// and the read loop serializes every message on the connection
		// (cancel, FS reads/writes, editor saves). The reply is safe to
		// write from a goroutine (writeJSON only enqueues onto the conn's
		// send queue, drained by a single writer) and the client correlates
		// by RequestID, so out-of-order delivery is fine.
		go func() {
			resp := WSMessage{Type: "git_commit_message_result", RequestID: reqID}
			if out, err := s.generateCommitMessage(ctx); err != nil {
				resp.Error = err.Error()
			} else {
				resp.Success = true
				resp.Content = out
			}
			_ = ws.writeJSON(resp)
		}()
	}
}

func (s *Server) handleFSWriteMessage(ws *wsConn, ctx context.Context, msg WSMessage) {
	reqID := msg.RequestID
	path := msg.Path
	// Editor writes serialize on the workspace filesystem lock (not the
	// session turn lock): they wait only for the actual mutation
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
		replaced, fileCount, files, err := s.fsReplace(ctx, msg.Pattern, msg.Replacement, msg.Path, msg.Glob)
		resp := WSMessage{Type: "fs_replace_result", Path: path, RequestID: reqID, Pattern: msg.Pattern}
		if err != nil {
			resp.Error = err.Error()
		} else {
			resp.Success = true
			resp.Replaced = replaced
			resp.FileCount = fileCount
			resp.Files = files
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
	case "git_commit":
		// Commit composer (ticket #53). The message is passed as a single
		// argv element via GitCommit — never through a shell.
		resp := WSMessage{Type: "git_commit_result", RequestID: reqID}
		if strings.TrimSpace(msg.Content) == "" {
			resp.Error = "commit message is required"
		} else if out, err := s.ws.Exec.GitCommit(ctx, msg.Content); err != nil {
			resp.Error = err.Error()
		} else {
			resp.Success = true
			resp.Result = out
		}
		_ = ws.writeJSON(resp)
	case "git_stage":
		// Stage the given paths (empty = all changes, i.e. `git add -A`).
		// GitStage SecurePath-validates every path and builds argv-only.
		resp := WSMessage{Type: "git_stage_result", RequestID: reqID}
		if out, err := s.ws.Exec.GitStage(ctx, msg.Paths); err != nil {
			resp.Error = err.Error()
		} else {
			resp.Success = true
			resp.Result = out
		}
		_ = ws.writeJSON(resp)
	case "git_unstage":
		// Unstage the given paths (empty = all staged changes) via
		// `git restore --staged -- <paths>` — SecurePath-validated,
		// argv-only.
		resp := WSMessage{Type: "git_unstage_result", RequestID: reqID}
		if out, err := s.ws.Exec.GitUnstage(ctx, msg.Paths); err != nil {
			resp.Error = err.Error()
		} else {
			resp.Success = true
			resp.Result = out
		}
		_ = ws.writeJSON(resp)
	case "git_push":
		// Push to origin with a fixed argv; the only user-controlled input
		// is the branch ref, validated inside GitPush (validateGitRef).
		resp := WSMessage{Type: "git_push_result", RequestID: reqID}
		if out, err := s.ws.Exec.GitPush(ctx, msg.Branch); err != nil {
			resp.Error = err.Error()
		} else {
			resp.Success = true
			resp.Result = out
		}
		_ = ws.writeJSON(resp)
	}
}
