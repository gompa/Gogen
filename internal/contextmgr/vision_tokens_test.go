package contextmgr

import (
	"testing"

	"gogen/internal/llm"
)

// TestComputeMessageTokensCountsImages verifies image inputs add a flat token
// estimate on top of the text tokens, so compaction accounts for vision
// input instead of silently underestimating the window.
func TestComputeMessageTokensCountsImages(t *testing.T) {
	base := ComputeMessageTokens(llm.Message{Role: "user", Content: "hi"})
	one := ComputeMessageTokens(llm.Message{
		Role:    "user",
		Content: "hi",
		Images:  []llm.ImageInput{{DataURL: "data:image/png;base64,AAAA"}},
	})
	two := ComputeMessageTokens(llm.Message{
		Role:    "user",
		Content: "hi",
		Images: []llm.ImageInput{
			{DataURL: "data:image/png;base64,AAAA"},
			{DataURL: "data:image/jpeg;base64,BBBB"},
		},
	})
	if one-base != 1024 {
		t.Fatalf("one image delta = %d, want 1024", one-base)
	}
	if two-base != 2048 {
		t.Fatalf("two image delta = %d, want 2048", two-base)
	}
}
