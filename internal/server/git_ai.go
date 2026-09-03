package server

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

// gitCommitMessageDiffMaxBytes caps the staged-diff size embedded in the
// commit-message prompt (~30 KB) so a huge staged change cannot blow the
// context window or the request budget.
const gitCommitMessageDiffMaxBytes = 30 * 1024

// gitCommitMessageTimeout bounds the one-shot LLM call so a hung endpoint
// cannot wedge the editor socket's read loop forever.
const gitCommitMessageTimeout = 60 * time.Second

// gitCommitMessageInstruction is appended to the staged diff in the one-shot
// prompt. The model must answer with the message only, so the reply can be
// dropped straight into the composer's textarea.
const gitCommitMessageInstruction = "Write a concise git commit message. Output only the message, no preamble."

// gitStagedDiff returns the staged (index vs HEAD) diff, truncated to
// gitCommitMessageDiffMaxBytes. It returns an empty string when nothing is
// staged. Shared by the commit-message generator; the git_staged_diff WS
// endpoint (ticket #51) can reuse it for the composer preview.
func (s *Server) gitStagedDiff(ctx context.Context) (string, error) {
	diff, err := s.ws.Exec.GitStagedDiff(ctx)
	if err != nil {
		return "", err
	}
	// Truncate makes a rune-safe cut: a raw byte slice here could land
	// mid-rune on a multi-byte diff and emit invalid UTF-8 into the prompt.
	return contextmgr.Truncate(diff, gitCommitMessageDiffMaxBytes,
		contextmgr.TruncateOptions{Marker: "\n… (diff truncated)"}), nil
}

// openAIProviderConfigured reports whether the workspace has a usable LLM
// endpoint (a default key/URL or any registered provider profile). Only the
// real OpenAIProvider needs this preflight check — the test/embed path
// (mock/stub providers) is always usable.
func (s *Server) openAIProviderConfigured() bool {
	r := s.ws.GetRuntimeConfig()
	if strings.TrimSpace(r.OpenAIKey) != "" || strings.TrimSpace(r.OpenAIURL) != "" {
		return true
	}
	for _, p := range s.ws.GetOpenAIProviders() {
		if strings.TrimSpace(p.APIKey) != "" || strings.TrimSpace(p.BaseURL) != "" {
			return true
		}
	}
	return false
}

// generateCommitMessage builds a throwaway provider via the workspace's
// ProviderFactory and makes ONE one-shot GenerateResponse call with a single
// user message (staged diff + instruction). It never touches any chat
// session: no session is created, no history is appended, no turn runs —
// the reply is returned directly to the caller.
func (s *Server) generateCommitMessage(ctx context.Context) (string, error) {
	if s.ws == nil {
		return "", fmt.Errorf("workspace unavailable")
	}
	diff, err := s.gitStagedDiff(ctx)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(diff) == "" {
		return "", fmt.Errorf("nothing staged to commit; stage changes first")
	}
	if s.ws.ProviderFactory == nil {
		return "", fmt.Errorf("no LLM provider configured; add one in Settings")
	}
	provider := s.ws.ProviderFactory()
	if _, ok := provider.(*llm.OpenAIProvider); ok && !s.openAIProviderConfigured() {
		return "", fmt.Errorf("no LLM provider configured; add one in Settings")
	}
	prompt := diff + "\n\n" + gitCommitMessageInstruction
	cctx, cancel := context.WithTimeout(ctx, gitCommitMessageTimeout)
	defer cancel()
	resp, err := provider.GenerateResponse(cctx, []llm.Message{{Role: "user", Content: prompt}}, nil, nil)
	if err != nil {
		return "", fmt.Errorf("commit message generation failed: %w", err)
	}
	// Content is the expected channel; fall back to refusal/reasoning only
	// when the model answered in a non-content field, so the composer still
	// gets usable text instead of a silent empty fill.
	msg := strings.TrimSpace(resp.Content)
	if msg == "" {
		msg = strings.TrimSpace(resp.Refusal)
	}
	if msg == "" {
		msg = strings.TrimSpace(resp.Reasoning)
	}
	if msg == "" {
		return "", fmt.Errorf("model returned an empty commit message")
	}
	return msg, nil
}
