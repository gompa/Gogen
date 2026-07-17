package agent

import (
	"fmt"
	"strings"

	"gogen/internal/llm"
)

// PinnedMessage is a message that survives context compaction.
type PinnedMessage struct {
	Index int // index into canonical messages at pin time
	Msg   llm.Message
}

// PinManager tracks pinned message indices that must survive compaction.
type PinManager struct {
	pinned map[int]struct{} // indices into canonical history
}

// NewPinManager creates a pin manager.
func NewPinManager() *PinManager {
	return &PinManager{pinned: make(map[int]struct{})}
}

// PinLastUser pins the most recent user message so it survives compaction.
func (p *PinManager) PinLastUser(messages []llm.Message) {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			p.pinned[i] = struct{}{}
			return
		}
	}
}

// Unpin removes a pinned message by index.
func (p *PinManager) Unpin(index int) {
	delete(p.pinned, index)
}

// ClearPins removes all pins.
func (p *PinManager) ClearPins() {
	p.pinned = make(map[int]struct{})
}

// IsPinned reports whether the message at the given index is pinned.
func (p *PinManager) IsPinned(index int) bool {
	_, ok := p.pinned[index]
	return ok
}

// PinnedIndices returns all pinned indices.
func (p *PinManager) PinnedIndices() []int {
	indices := make([]int, 0, len(p.pinned))
	for idx := range p.pinned {
		indices = append(indices, idx)
	}
	return indices
}

// ListPins returns a formatted list of pinned messages.
func (p *PinManager) ListPins(messages []llm.Message) string {
	if len(p.pinned) == 0 {
		return "No pinned messages"
	}
	var b strings.Builder
	b.WriteString("Pinned messages (survive compaction):\n")
	for idx := range p.pinned {
		if idx >= 0 && idx < len(messages) {
			content := messages[idx].Content
			if len(content) > 80 {
				content = content[:80] + "…"
			}
			fmt.Fprintf(&b, "  #%d: %s\n", idx, content)
		}
	}
	return b.String()
}

// PinnedSet returns a copy of the pinned index set.
func (p *PinManager) PinnedSet() map[int]struct{} {
	if p == nil || len(p.pinned) == 0 {
		return nil
	}
	out := make(map[int]struct{}, len(p.pinned))
	for idx := range p.pinned {
		out[idx] = struct{}{}
	}
	return out
}

// ReplacePins replaces the pin set (used after compaction remaps indices).
func (p *PinManager) ReplacePins(pins map[int]struct{}) {
	if p == nil {
		return
	}
	if len(pins) == 0 {
		p.pinned = make(map[int]struct{})
		return
	}
	p.pinned = make(map[int]struct{}, len(pins))
	for idx := range pins {
		p.pinned[idx] = struct{}{}
	}
}

// MergePinsWithTail returns the set of indices that must be kept in the tail
// during compaction (merged with the normal keep-recent range).
func (p *PinManager) MergePinsWithTail(tailStart int, keepRecent int) int {
	if p == nil || len(p.pinned) == 0 {
		return tailStart
	}
	for idx := range p.pinned {
		if idx < tailStart && idx > 0 {
			tailStart = idx
		}
	}
	_ = keepRecent
	return tailStart
}
