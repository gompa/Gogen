package llm

import (
	"context"
	"sync"
)

// MockProvider is a configurable LLMProvider for tests.
type MockProvider struct {
	mu sync.Mutex

	Model         string
	ContextLimit  int
	Models        []ModelInfo
	Responses     []Response // consumed in order by GenerateResponse
	StreamResults []*StreamResult
	GenerateErr   error
	StreamErr     error
	SetModelErr   error
	CallCount     int
	LastMessages  []Message
	LastAllowed   map[string]struct{}
	OnGenerate    func(ctx context.Context, messages []Message) (Response, error)
	OnStream      func(ctx context.Context, messages []Message, h *StreamHandlers) (*StreamResult, error)
}

// NewMockProvider returns a mock with sensible defaults.
func NewMockProvider() *MockProvider {
	return &MockProvider{
		Model:        "mock-model",
		ContextLimit: 128000,
		Models:       []ModelInfo{{ID: "mock-model", ContextLimit: 128000, Current: true}},
		Responses:    []Response{{Content: "ok"}},
	}
}

func (m *MockProvider) GenerateResponse(ctx context.Context, messages []Message, allowedTools map[string]struct{}, _ []Tool) (Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.CallCount++
	m.LastMessages = append([]Message(nil), messages...)
	m.LastAllowed = allowedTools
	if m.OnGenerate != nil {
		return m.OnGenerate(ctx, messages)
	}
	if m.GenerateErr != nil {
		return Response{}, m.GenerateErr
	}
	if len(m.Responses) == 0 {
		return Response{Content: "ok"}, nil
	}
	resp := m.Responses[0]
	if len(m.Responses) > 1 {
		m.Responses = m.Responses[1:]
	}
	return resp, nil
}

func (m *MockProvider) GenerateResponseStream(ctx context.Context, messages []Message, allowedTools map[string]struct{}, extraTools []Tool, h *StreamHandlers) (*StreamResult, error) {
	m.mu.Lock()
	onStream := m.OnStream
	streamErr := m.StreamErr
	var result *StreamResult
	if len(m.StreamResults) > 0 {
		result = m.StreamResults[0]
		if len(m.StreamResults) > 1 {
			m.StreamResults = m.StreamResults[1:]
		}
	}
	m.mu.Unlock()

	if onStream != nil {
		return onStream(ctx, messages, h)
	}
	if streamErr != nil {
		return nil, streamErr
	}

	resp, err := m.GenerateResponse(ctx, messages, allowedTools, extraTools)
	if err != nil {
		return nil, err
	}
	if result != nil {
		if h != nil {
			emitStreamResult(h, result)
		}
		return result, nil
	}
	out := &StreamResult{Content: resp.Content, Reasoning: resp.Reasoning, Refusal: resp.Refusal, ToolCalls: resp.ToolCalls, Usage: resp.Usage}
	if h != nil {
		emitStreamResult(h, out)
	}
	return out, nil
}

func emitStreamResult(h *StreamHandlers, result *StreamResult) {
	if h.OnStart != nil {
		h.OnStart()
	}
	if h.OnStreamOpened != nil {
		h.OnStreamOpened()
	}
	if result.Reasoning != "" && h.OnThinkingToken != nil {
		h.OnThinkingToken(result.Reasoning)
	}
	if result.Content != "" && h.OnToken != nil {
		h.OnToken(result.Content)
	} else if result.Refusal != "" && h.OnToken != nil {
		h.OnToken(result.Refusal)
	}
	for _, tc := range result.ToolCalls {
		if h.OnToolCallStart != nil {
			h.OnToolCallStart(tc.Index, tc.ID, tc.Name)
		}
		if h.OnToolCall != nil {
			h.OnToolCall(tc)
		}
	}
	if h.OnStreamEnd != nil && len(result.ToolCalls) > 0 {
		h.OnStreamEnd()
	}
}

func (m *MockProvider) ModelContextLimit(context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ContextLimit > 0 {
		return m.ContextLimit, nil
	}
	return 128000, nil
}

func (m *MockProvider) ListModels(context.Context) ([]ModelInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Models != nil {
		return append([]ModelInfo(nil), m.Models...), nil
	}
	return []ModelInfo{{ID: m.ModelName(), Current: true}}, nil
}

func (m *MockProvider) SetModel(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.SetModelErr != nil {
		return m.SetModelErr
	}
	m.Model = id
	return nil
}

func (m *MockProvider) ModelName() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Model == "" {
		return "mock-model"
	}
	return m.Model
}

// Ensure MockProvider implements LLMProvider.
var _ LLMProvider = (*MockProvider)(nil)
