package llm

import (
	"context"
	"time"
)

// Tool represents a tool the AI agent can call.
type Tool struct {
	Type        string
	Name        string
	Description string
	Parameters  map[string]interface{}
}

// Usage reports token counts from the provider when available.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	// CachedTokens is the subset of PromptTokens served from the provider prompt cache.
	CachedTokens int
}

// Response represents the AI agent's response.
type Response struct {
	Content   string
	Reasoning string
	Refusal   string // model refusal text when content is empty (kept separate for cache stability)
	ToolCalls []ToolCall
	Usage     *Usage
	// Model is the model ID reported by the provider for this response (may
	// differ from the requested alias on router/proxy endpoints such as
	// OpenCode Zen). Empty when the provider did not report one.
	Model string
}

// ToolCall represents a request from the AI to execute a tool.
type ToolCall struct {
	Index int // stream index from the provider, used to correlate streaming UI
	ID    string
	Name  string
	Args  map[string]interface{}
	// ArgsStr is the exact JSON arguments string from the provider.
	// Prefer this when re-sending history so prompt-cache prefixes stay byte-stable.
	ArgsStr string
	// ArgsError is set when streamed tool arguments could not be parsed.
	// Callers must not execute the tool; return this error as the tool result.
	ArgsError string
	// ArgsJSONValid is true when ArgsStr is non-empty, already trimmed, and
	// valid JSON — i.e. the exact bytes the wire serializer will send.
	// Set by StabilizeToolCallArgs (stabilization, stream finalization) and
	// by session restore, always before the message is published; the wire
	// serializer only READS it to skip the per-request json.Valid scan of
	// every historical tool call. Never set by the serializer itself (it
	// runs outside the stats lock on shared stabilized ToolCalls). Not
	// serialized — restored messages recompute it once at load.
	ArgsJSONValid bool `json:"-"`
}

// StreamCallback receives partial tokens as they arrive from a streamed response.
type StreamCallback func(token string)

// StreamHandlers provides optional callbacks for streaming and progress events.
type StreamHandlers struct {
	OnStart          func()         // called once when processing begins
	OnRoundStart     func()         // called at the start of each LLM round after the first
	OnStreamOpened   func()         // called when the SSE connection is established
	OnCompacting     func()         // called when context compaction starts (before the summarization call)
	OnStreamStall    func()         // called when no SSE chunk arrives for several seconds
	OnStreamActivity func()         // called on the first visible content/refusal token
	OnThinkingToken  StreamCallback // called for each reasoning/thinking token (display separately)
	OnToken          StreamCallback // called for each content token
	OnStreamEnd      func()         // called when a streamed LLM turn completes with pending tool calls
	// OnReplyModel is called with the provider-reported model ID for every
	// completed round, right before OnStreamEnd finalizes it. Receives ""
	// when the provider did not report a model.
	OnReplyModel           func(model string)
	OnToolCallStart        func(index int, id, name string)            // called when a tool call name first appears in the stream
	OnToolCallArgsDelta    func(index int, id, name, argsDelta string) // called for each streamed args fragment
	OnToolCall             func(tc ToolCall)                           // called before a tool executes (args fully parsed)
	OnRecoverPartialStream func()                                      // reset UI after stream error mid-tool-call
	OnToolExecute          func(name string)                           // called immediately before a tool runs (may block)
	OnToolOutput           func(id, name, command, chunk string)       // live combined stdout/stderr chunks from shell tools while they run
	OnToolResult           func(id, name, result string, success bool) // called after a tool executes
}

// StreamResult holds the final accumulated response from a streamed call.
type StreamResult struct {
	Content       string
	Reasoning     string // reasoning/thinking content accumulated during streaming
	Refusal       string // refusal text accumulated during streaming (kept separate from Content)
	ToolCalls     []ToolCall
	Usage         *Usage
	PartialStream bool // true when streaming failed after partial output before fallback
	// Model is the model ID reported by the provider for this stream (may
	// differ from the requested alias on router/proxy endpoints such as
	// OpenCode Zen). Empty when the provider did not report one.
	Model string
}

// ModelInfo describes a model available from the provider endpoint.
type ModelInfo struct {
	ID           string
	ContextLimit int
	Current      bool
	// Pricing from models.dev (USD per 1M tokens). Zero means no pricing data.
	InputPricePer1M  float64
	OutputPricePer1M float64
	CachedPricePer1M float64
}

// LLMProvider defines the interface for different LLM providers.
type LLMProvider interface {
	GenerateResponse(ctx context.Context, messages []Message, allowedTools map[string]struct{}, extraTools []Tool) (Response, error)
	GenerateResponseStream(ctx context.Context, messages []Message, allowedTools map[string]struct{}, extraTools []Tool, h *StreamHandlers) (*StreamResult, error)
	ModelContextLimit(ctx context.Context) (int, error)
	ListModels(ctx context.Context) ([]ModelInfo, error)
	SetModel(id string) error
	ModelName() string
	// SetThinkingLevel controls reasoning/thinking effort. Pass empty string to
	// omit the parameter (equivalent to "off").
	SetThinkingLevel(level string)
}

// Message represents a chat message.
type Message struct {
	Role       string
	Content    string
	Reasoning  string     // reasoning/thinking content from the model (sent as reasoning_content)
	Refusal    string     // refusal text from the model (sent as refusal; not folded into Content)
	ToolCalls  []ToolCall // set on assistant messages that invoke tools
	ToolCallID string     // set on tool result messages
	// Images holds user-supplied images (data URLs) attached to a user
	// message, sent to vision-capable models as image_url content parts.
	// Only user messages carry images.
	Images    []ImageInput `json:"images,omitempty"`
	CreatedAt time.Time    `json:"createdAt,omitempty"` // when the message was created (UTC), zero when not set
	// Model is the model ID that produced this message, as reported by the
	// provider (may differ from the requested alias on router/proxy
	// endpoints such as OpenCode Zen). Empty when not reported.
	Model string `json:"model,omitempty"`

	// ArgsStabilized is true when ToolCalls[j].Args has been marshalled
	// into ArgsStr. Used to avoid re-stabilizing every message on every
	// turn. Not serialized — restored messages start as stabilized.
	ArgsStabilized bool `json:"-"`
}

// ImageInput is one user-supplied image attached to a user message,
// transported as a data URL (e.g. "data:image/png;base64,..."). Detail
// controls the vision detail level the provider receives: "auto", "low", or
// "high"; empty means "auto".
type ImageInput struct {
	DataURL string `json:"dataUrl"`
	Detail  string `json:"detail,omitempty"`
}

// HasImages reports whether the message carries one or more images.
func (m Message) HasImages() bool {
	return len(m.Images) > 0
}
