package server

import (
	"context"
	"fmt"
	"strings"

	"gogen/internal/agent"
	"gogen/internal/llm"
)

type ModelEntry struct {
	ID               string  `json:"id"`
	ContextLimit     int     `json:"contextLimit,omitempty"`
	Current          bool    `json:"current,omitempty"`
	InputPricePer1M  float64 `json:"inputPricePer1M,omitempty"`
	OutputPricePer1M float64 `json:"outputPricePer1M,omitempty"`
	CachedPricePer1M float64 `json:"cachedPricePer1M,omitempty"`
	// ReasoningEfforts are the reasoning_effort values this model accepts
	// (models.dev); empty for unknown or toggle/budget-only models.
	ReasoningEfforts []string `json:"reasoningEfforts,omitempty"`
	// Description is the models.dev model description; empty for unknown
	// models. Shown as a hover tooltip in the client.
	Description string `json:"description,omitempty"`
}

type SessionEntry struct {
	ID           string `json:"id"`
	UpdatedAt    string `json:"updatedAt,omitempty"`
	MessageCount int    `json:"messageCount,omitempty"`
	Label        string `json:"label,omitempty"`
	Oneshot      bool   `json:"oneshot,omitempty"`
	// Active marks sessions with a live in-memory runtime: the client
	// can render "resume to continue" for them.
	// Idle runtimes whose last client detached are orphan-evicted (they
	// return to the saved list), so active here means genuinely live —
	// open in another tab, or a headless turn still running.
	Active bool `json:"active,omitempty"`
	// Label is now the full first user message — CSS text-overflow: ellipsis
	// handles dynamic truncation on the client side.
}

type HistoryToolCall struct {
	ID   string                 `json:"id"`
	Name string                 `json:"name"`
	Args map[string]interface{} `json:"args,omitempty"`
}

type HistoryEntry struct {
	Role      string           `json:"role"`
	Content   string           `json:"content,omitempty"`
	Images    []llm.ImageInput `json:"images,omitempty"`
	Reasoning string           `json:"reasoning,omitempty"`
	Refusal   string           `json:"refusal,omitempty"`
	// Model is the model ID that produced the reply, as reported by the
	// provider (may differ from the requested alias on router endpoints
	// such as OpenCode Zen). Empty when not reported.
	Model      string            `json:"model,omitempty"`
	ToolCalls  []HistoryToolCall `json:"toolCalls,omitempty"`
	ToolCallID string            `json:"toolCallId,omitempty"`
	Index      int               `json:"index"`               // index in agent.Messages (0 is valid; do not omitempty)
	CreatedAt  string            `json:"createdAt,omitempty"` // RFC3339Nano UTC when the message was created
}

type WSMessage struct {
	Type            string                 `json:"type"`
	Content         string                 `json:"content,omitempty"`
	Tool            string                 `json:"tool,omitempty"`
	TermID          string                 `json:"termId,omitempty"`
	Cols            int                    `json:"cols,omitempty"`
	Rows            int                    `json:"rows,omitempty"`
	Code            int                    `json:"code,omitempty"`
	ToolCallID      string                 `json:"toolCallId,omitempty"`
	Index           int                    `json:"index,omitempty"`
	ArgsDelta       string                 `json:"argsDelta,omitempty"`
	Args            map[string]interface{} `json:"args,omitempty"`
	Result          string                 `json:"result,omitempty"`
	Success         bool                   `json:"success,omitempty"`
	ResultTruncated bool                   `json:"resultTruncated,omitempty"`
	WorkingDir      string                 `json:"workingDir,omitempty"`
	Model           string                 `json:"model,omitempty"`
	// Pricing for the current model (USD per 1M tokens), populated from models.dev cache.
	InputPricePer1M  float64 `json:"inputPricePer1M,omitempty"`
	OutputPricePer1M float64 `json:"outputPricePer1M,omitempty"`
	CachedPricePer1M float64 `json:"cachedPricePer1M,omitempty"`
	// ReasoningEfforts are the reasoning_effort values the current model
	// accepts (models.dev); empty means unknown or no effort control, and
	// clients fall back to the default set for chips.
	ReasoningEfforts []string `json:"reasoningEfforts,omitempty"`
	// ModelDescription is the models.dev description of the current model;
	// empty means unknown. Shown as a hover tooltip in the client.
	ModelDescription string `json:"modelDescription,omitempty"`
	ContextLimit     int    `json:"contextLimit,omitempty"`
	UsedTokens       int    `json:"usedTokens,omitempty"`
	UsedSource       string `json:"usedSource,omitempty"`
	PromptTokens     int    `json:"promptTokens,omitempty"`
	CompletionTokens int    `json:"completionTokens,omitempty"`
	CachedTokens     int    `json:"cachedTokens,omitempty"`
	CompactAt        int    `json:"compactAt,omitempty"`
	MessageCount     int    `json:"messageCount,omitempty"`
	// Images carries user-attached images (data URLs) on inbound "message"
	// frames; the server validates and forwards them to the agent.
	Images []llm.ImageInput `json:"images,omitempty"`
	// NearCompact and WarnNearCompact intentionally have NO omitempty: a
	// false value must reach the client or a previously shown near-compact
	// banner never hides after a compaction.
	NearCompact     bool    `json:"nearCompact"`
	WarnNearCompact bool    `json:"warnNearCompact"`
	UsedPercent     float64 `json:"usedPercent,omitempty"`
	ToolTruncated   bool    `json:"toolTruncated,omitempty"`
	// Accumulated session usage
	TotalPromptTokens     int          `json:"totalPromptTokens,omitempty"`
	TotalCompletionTokens int          `json:"totalCompletionTokens,omitempty"`
	TotalCachedTokens     int          `json:"totalCachedTokens,omitempty"`
	TotalTurns            int          `json:"totalTurns,omitempty"`
	Models                []ModelEntry `json:"models,omitempty"`
	ApprovalID            string       `json:"approvalId,omitempty"`
	Approved              bool         `json:"approved,omitempty"`
	Paths                 []string     `json:"paths,omitempty"`
	Reason                string       `json:"reason,omitempty"`
	Mode                  string       `json:"mode,omitempty"`
	ThinkingLevel         string       `json:"thinkingLevel,omitempty"`
	GlobalMode            bool         `json:"globalMode,omitempty"`
	SessionID             string       `json:"sessionId,omitempty"`
	SessionAction         string       `json:"sessionAction,omitempty"`
	SessionLabel          string       `json:"sessionLabel,omitempty"`
	// TurnActive describes a session's in-flight turn; sent in session_state
	// replies so a reconnecting client can render "resuming…".
	TurnActive   bool           `json:"turnActive,omitempty"`
	MessageIndex int            `json:"messageIndex,omitempty"`
	Sessions     []SessionEntry `json:"sessions,omitempty"`
	History      []HistoryEntry `json:"history,omitempty"`
	// ContentPos / ThinkingPos / ArgsPos are cumulative character offsets
	// within the current round's content / thinking / tool-args streams,
	// stamped on stream / thinking_token / tool_call_delta frames. A client
	// uses them to merge an attach rewind with live content exactly — never
	// duplicating a chunk already inside the snapshot nor dropping one
	// beyond it.
	ContentPos  int `json:"contentPos,omitempty"`
	ThinkingPos int `json:"thinkingPos,omitempty"`
	ArgsPos     int `json:"argsPos,omitempty"`
	// HistoryEpoch is a per-session counter bumped whenever the conversation
	// is replaced wholesale (compaction/restore/rollback/fork), letting
	// clients distinguish a stale snapshot from a reshaped history.
	HistoryEpoch uint64 `json:"historyEpoch,omitempty"`
	// Rewind carries the in-flight turn's partial output on a mid-turn
	// attach/resume history payload (nil when nothing has streamed yet).
	Rewind *liveRewindState `json:"rewind,omitempty"`
	// Filesystem / git editor APIs
	Path        string              `json:"path,omitempty"`
	Pattern     string              `json:"pattern,omitempty"`
	Glob        string              `json:"glob,omitempty"`
	Language    string              `json:"language,omitempty"`
	Error       string              `json:"error,omitempty"`
	Entries     []FSEntry           `json:"entries,omitempty"`
	GitEntries  []GitStatusEntry    `json:"gitEntries,omitempty"`
	Matches     []agent.SearchMatch `json:"matches,omitempty"`
	Truncated   bool                `json:"truncated,omitempty"`
	Original    string              `json:"original,omitempty"`
	Modified    string              `json:"modified,omitempty"`
	Diff        string              `json:"diff,omitempty"`
	RequestID   string              `json:"requestId,omitempty"`
	Replacement string              `json:"replacement,omitempty"`
	Replaced    int                 `json:"replaced,omitempty"`
	FileCount   int                 `json:"fileCount,omitempty"`
}

func applyContextStats(msg *WSMessage, stats agent.TurnContext, accum *agent.UsageAccumulator) {
	snap := stats.Snapshot
	if snap.Limit > 0 {
		msg.ContextLimit = snap.Limit
	}
	if snap.Used > 0 {
		msg.UsedTokens = snap.Used
	}
	msg.PromptTokens = stats.PromptTokens
	msg.CompletionTokens = stats.CompletionTokens
	msg.CachedTokens = stats.CachedTokens
	msg.CompactAt = snap.CompactAt
	msg.MessageCount = snap.MessageCount
	msg.NearCompact = snap.NearCompact
	msg.WarnNearCompact = snap.WarnNearCompact
	msg.ToolTruncated = snap.ToolTruncated
	// When the API returned exact usage, the Snapshot.Used incorporates that
	// authoritative baseline (plus estimates for any messages added after the
	// last request). Otherwise it's purely a local estimate.
	msg.UsedSource = "estimated"
	if stats.LastUsage != nil && stats.LastUsage.PromptTokens > 0 {
		msg.UsedSource = "api"
	}
	if snap.Limit > 0 {
		msg.UsedPercent = snap.Percent
	}
	if accum != nil {
		msg.TotalPromptTokens = accum.TotalPromptTokens
		msg.TotalCompletionTokens = accum.TotalCompletionTokens
		msg.TotalCachedTokens = accum.TotalCachedTokens
		msg.TotalTurns = accum.TotalTurns
	}
}

// Limits for user-attached images (vision input). These cap WebSocket frame
// size and per-message model cost without being overly restrictive.
const (
	maxImagesPerMessage   = 4
	maxImageDataURLBytes  = 5 * 1024 * 1024
	imageDataURLPrefix    = "data:image/"
	imageDataURLBase64Sep = ";base64,"
)

// validateImageInputs checks user-attached images and returns a clean copy
// (nil when the message carried none). Empty inputs are rejected: a message
// must not claim an image that carries no bytes, and the data URL must look
// like a base64 image to keep the wire format honest.
func validateImageInputs(images []llm.ImageInput) ([]llm.ImageInput, error) {
	if len(images) == 0 {
		return nil, nil
	}
	if len(images) > maxImagesPerMessage {
		return nil, fmt.Errorf("too many images: %d (max %d per message)", len(images), maxImagesPerMessage)
	}
	out := make([]llm.ImageInput, 0, len(images))
	for i, img := range images {
		url := strings.TrimSpace(img.DataURL)
		if url == "" {
			return nil, fmt.Errorf("image %d has no data URL", i+1)
		}
		if len(url) > maxImageDataURLBytes {
			return nil, fmt.Errorf("image %d exceeds %d bytes (data URL too large)", i+1, maxImageDataURLBytes)
		}
		if !strings.HasPrefix(url, imageDataURLPrefix) || !strings.Contains(url, imageDataURLBase64Sep) {
			return nil, fmt.Errorf("image %d must be a base64 data URL (data:image/...;base64,...)", i+1)
		}
		detail := strings.ToLower(strings.TrimSpace(img.Detail))
		switch detail {
		case "", "auto", "low", "high":
		default:
			return nil, fmt.Errorf("image %d has invalid detail %q (use auto, low, or high)", i+1, img.Detail)
		}
		out = append(out, llm.ImageInput{DataURL: url, Detail: detail})
	}
	return out, nil
}

// contextMsg builds a context stats message. Safe to call while holding the
// session's turnMu (ContextStats snapshots under its own statsMu; the turn
// goroutine calls it during a turn), but it can be slow — ContextStats
// tokenizes the full history view — so callers on the WS read loop that are
// NOT holding the lock should keep it that way.
func contextMsg(ctx context.Context, a *agent.Agent) WSMessage {
	msg := WSMessage{Type: "context", SessionID: a.SessionID}
	accum := a.SnapshotUsageAccum()
	applyContextStats(&msg, a.ContextStats(ctx), &accum)
	msg.SessionLabel = a.SessionLabelSnapshot()
	return msg
}
