package llm_test

import (
	"context"
	"testing"

	"gogen/internal/llm"
)

func TestMockProviderBasic(t *testing.T) {
	m := llm.NewMockProvider()
	m.Responses = []llm.Response{
		{Content: "first"},
		{Content: "second"},
	}
	r1, err := m.GenerateResponse(context.Background(), nil, nil, nil)
	if err != nil || r1.Content != "first" {
		t.Fatalf("r1=%+v err=%v", r1, err)
	}
	r2, err := m.GenerateResponse(context.Background(), nil, nil, nil)
	if err != nil || r2.Content != "second" {
		t.Fatalf("r2=%+v err=%v", r2, err)
	}
	stream, err := m.GenerateResponseStream(context.Background(), nil, nil, nil, nil)
	if err != nil || stream.Content != "second" {
		t.Fatalf("stream=%+v err=%v", stream, err)
	}
	if m.ModelName() != "mock-model" {
		t.Fatalf("model=%q", m.ModelName())
	}
}
