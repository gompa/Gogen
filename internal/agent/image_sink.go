package agent

import (
	"context"
	"sync"
	"time"

	"gogen/internal/llm"
)

// imageEntry is one image produced by a read_image tool call: the image
// itself and the caption attached to the synthetic user message that carries
// it into context.
type imageEntry struct {
	img     llm.ImageInput
	caption string
}

// imageSink accumulates images during tool execution so a handler never
// touches the transcript itself. executeTool runs before appendToolResult;
// a handler that appended a user message directly would place the image
// between the assistant's tool_call and its tool result — a wire-protocol
// violation. Instead the handler only collects the image into the sink, and
// the tool-round coordinator appends the synthetic user message immediately
// after the tool result, in both the sequential and the parallel paths.
// Handlers run concurrently in parallel read-only batches, so every
// mutation is mutex-guarded.
type imageSink struct {
	mu    sync.Mutex
	items []imageEntry
}

// Add records one image (thread-safe).
func (s *imageSink) Add(img llm.ImageInput, caption string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = append(s.items, imageEntry{img: img, caption: caption})
}

// drain returns the collected entries and clears the sink. Called by the
// tool-round coordinator on the turn goroutine after the tool result was
// appended; a never-populated sink drains to nil.
func (s *imageSink) drain() []imageEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := s.items
	s.items = nil
	return items
}

type imageSinkKey struct{}

// ContextWithImageSink returns a copy of ctx carrying sink. read_image
// handlers read it via ImageSinkFromContext; the tool-round coordinator
// attaches a fresh sink per tool call (mirroring ContextWithToolOutput).
func ContextWithImageSink(ctx context.Context, sink *imageSink) context.Context {
	return context.WithValue(ctx, imageSinkKey{}, sink)
}

// ImageSinkFromContext returns the image sink attached to ctx, or nil when
// none was set.
func ImageSinkFromContext(ctx context.Context) *imageSink {
	if ctx == nil {
		return nil
	}
	sink, _ := ctx.Value(imageSinkKey{}).(*imageSink)
	return sink
}

// appendImageMessages appends every image collected by one read_image tool
// call as a synthetic user message, preserving collection order. Must be
// called by the tool-round coordinator immediately after the matching tool
// result is appended, so the transcript reads assistant(tool_call) → tool
// (result) → user(image+caption) — the only wire-legal placement for image
// content (the provider accepts image parts on user messages only). The
// message append goes through appendMessage, so token-count caching and the
// statsMu discipline apply exactly as they do for user input; the turn
// loop's persistSession (and the cancel paths' FlushSession) then persist it
// like any other message.
func (a *Agent) appendImageMessages(sink *imageSink) {
	if sink == nil {
		return
	}
	for _, e := range sink.drain() {
		a.appendMessage(llm.Message{
			Role:      "user",
			Content:   e.caption,
			Images:    []llm.ImageInput{e.img},
			CreatedAt: time.Now().Truncate(time.Millisecond),
		})
	}
}
