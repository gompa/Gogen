package agent

import (
	"context"
	"strings"
	"testing"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

func hasContent(msgs []llm.Message, content string) bool {
	for _, m := range msgs {
		if m.Content == content {
			return true
		}
	}
	return false
}

// seedDominantRequest builds a deep-dive history around a request that is
// most of the window: head, session summary, the request, a progress
// summary, and work after it that the layout around the request can
// summarize, so that layout succeeds and still leaves the request verbatim.
func seedDominantRequest(a *Agent, request string, after ...string) {
	a.SetDeepDive(true)
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	a.appendMessage(llm.Message{Role: "assistant", Content: contextmgr.SummaryPrefix + "earlier"})
	a.appendMessage(llm.Message{Role: "user", Content: request})
	a.appendMessage(llm.Message{Role: "assistant", Content: contextmgr.ProgressSummaryPrefix + "so far"})
	for _, content := range after {
		a.appendMessage(llm.Message{Role: "assistant", Content: content})
	}
	_ = a.ContextStats(context.Background())
}

// seedEmptyProgressRequest builds a deep-dive history whose progress region
// is empty. The work before the request is large enough to summarize, so
// the layout shrinks and still leaves the request verbatim.
func seedEmptyProgressRequest(a *Agent, request string) {
	a.SetDeepDive(true)
	a.appendMessage(llm.Message{Role: "user", Content: "hello"})
	a.appendMessage(llm.Message{Role: "assistant", Content: strings.Repeat("y", 8000)})
	a.appendMessage(llm.Message{Role: "assistant", Content: strings.Repeat("z", 8000)})
	a.appendMessage(llm.Message{Role: "user", Content: request})
	_ = a.ContextStats(context.Background())
}

func assertCompactionRecovered(t *testing.T, a *Agent) {
	t.Helper()
	a.statsMu.RLock()
	emergency := a.compactionGuards.emergency
	lastResort := a.compactionGuards.lastResort
	until := a.compactBackoffUntil
	a.statsMu.RUnlock()
	if emergency != 0 || lastResort != 0 || !until.IsZero() {
		t.Fatalf("after condensation emergency=%d lastResort=%d backoff=%s", emergency, lastResort, until)
	}
}

func pinnedCondensed(pm *PinManager, msgs []llm.Message) bool {
	for i, m := range msgs {
		if strings.HasPrefix(m.Content, contextmgr.CondensedMessagePrefix) && pm.IsPinned(i) {
			return true
		}
	}
	return false
}

func toolWasCondensed(msgs []llm.Message) bool {
	for _, m := range msgs {
		if m.Role == "tool" && strings.HasPrefix(m.Content, contextmgr.CondensedMessagePrefix) {
			return true
		}
	}
	return false
}

// errorModeOverWindowAgent is a deep-dive conversation whose view is over
// the window and whose head would be condensed. Error mode must not take
// that skip.
func errorModeOverWindowAgent(t *testing.T, mode string) (*Agent, *contextmgr.Manager, *countingProvider, string, string) {
	t.Helper()
	a, ctxMgr, provider := newEmergencyTestAgent(t, false)
	s := ctxMgr.SettingsSnapshot()
	s.CompactLastResort = mode
	ctxMgr.UpdateSettings(s)
	a.SetDeepDive(true)
	head := "HEAD " + strings.Repeat("x", 80000)
	middle := "MIDDLE " + strings.Repeat("x", 80000)
	a.appendMessage(llm.Message{Role: "user", Content: head})
	a.appendMessage(llm.Message{Role: "assistant", Content: middle})
	a.appendMessage(llm.Message{Role: "assistant", Content: "note"})
	a.appendMessage(llm.Message{Role: "user", Content: "keep-this-request"})
	_ = a.ContextStats(context.Background())
	if a.outgoingViewEstimate(a.rebuildView()) < ctxMgr.ContextLimit() {
		t.Fatal("test setup: estimate under the window")
	}
	return a, ctxMgr, provider, head, middle
}
