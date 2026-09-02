package server

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestRewindWireShape pins the JSON contract of the mid-turn attach rewind
// payload: the field names and values the client's rewind merge
// (appendStreamingToolArgs + trimToEnd) slices against. The payload is the
// shared streambuf.Snapshot (the TUI's mid-turn join renders the same
// struct), so this test is the tripwire that any change to that type's
// JSON tags keeps the wire format intact.
func TestRewindWireShape(t *testing.T) {
	rt := newTestRuntime(t)
	rt.live.AppendThinking("th")
	rt.live.AppendContent("hi")
	rt.live.ToolStart(0, "id1", "f")
	rt.live.AppendToolArgs(0, `{"a":1}`)

	data, err := json.Marshal(WSMessage{Type: "history", Rewind: rt.live.Snapshot()})
	if err != nil {
		t.Fatalf("marshal history: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal history: %v", err)
	}
	rw, ok := got["rewind"].(map[string]any)
	if !ok {
		t.Fatalf("rewind missing or not an object: %s", data)
	}
	want := map[string]any{
		"content":     "hi",
		"contentPos":  float64(2),
		"thinking":    "th",
		"thinkingPos": float64(2),
		"toolCalls": []any{
			map[string]any{
				"index":   float64(0),
				"id":      "id1",
				"name":    "f",
				"args":    `{"a":1}`,
				"argsPos": float64(7),
			},
		},
	}
	if !reflect.DeepEqual(rw, want) {
		t.Fatalf("rewind wire shape drifted:\n got  %v\n want %v", rw, want)
	}
}

// TestRewindWireOmittedWhenEmpty pins the between-rounds contract: an empty
// round snapshot must serialize as an OMITTED rewind field (nil + omitempty),
// so an idle attach never carries a rewind the client would render twice.
func TestRewindWireOmittedWhenEmpty(t *testing.T) {
	rt := newTestRuntime(t)
	data, err := json.Marshal(WSMessage{Type: "history", Rewind: rt.live.Snapshot()})
	if err != nil {
		t.Fatalf("marshal history: %v", err)
	}
	if strings.Contains(string(data), "rewind") {
		t.Fatalf("empty round must omit the rewind field: %s", data)
	}
}
