package tui

import (
	"strings"
	"testing"

	"charm.land/bubbletea/v2"
	"gogen/internal/agent"
)

func TestLiveSessionPendingBuffer(t *testing.T) {
	s := &liveSession{id: "s9"}

	t.Run("fifo order", func(t *testing.T) {
		s.enqueue(streamTokenMsg{token: "a"})
		s.enqueue(streamTokenMsg{token: "b"})
		out := s.popAll()
		if len(out) != 2 || out[0].(streamTokenMsg).token != "a" {
			t.Fatalf("popAll = %v, want [a b]", out)
		}
		if left := s.popAll(); len(left) != 0 {
			t.Fatalf("second popAll = %v, want empty", left)
		}
	})

	t.Run("cap drops oldest", func(t *testing.T) {
		s2 := &liveSession{id: "s10"}
		for i := 0; i < maxPendingStreamEvents+8; i++ {
			s2.enqueue(streamTokenMsg{token: "x"})
		}
		if got := len(s2.pending); got != maxPendingStreamEvents {
			t.Fatalf("len = %d, want cap %d", got, maxPendingStreamEvents)
		}
	})
}

// Focusing a streaming session replays buffered events into the transcript
// without touching the agent (Messages stay owned by the stream goroutine).
func TestJoinStreamingSessionReplays(t *testing.T) {
	vp := NewViewport(80, 20)
	vp.Style = ViewportStyle
	target := &liveSession{id: "s2", label: "bg", agent: &agent.Agent{}, streaming: true}
	target.enqueue(streamStartMsg{})
	target.enqueue(streamThinkingMsg{token: "hmm"})
	target.enqueue(streamRoundEndMsg{})
	target.enqueue(streamTokenMsg{token: "hello"})

	m := &Model{lives: newLiveSessions(&agent.Agent{}), viewport: vp}
	m.lives.Add(target.agent, "bg")
	m.joinStreamingSession(target, target.popAll())

	if len(target.pending) != 0 {
		t.Fatal("pending buffer not drained")
	}
	joined := strings.Join(m.chatLines, "\n")
	if !strings.Contains(joined, "mid-turn") {
		t.Fatal("join notice missing")
	}
	if !strings.Contains(joined, "hello") {
		t.Fatalf("replayed token missing from transcript:\n%s", joined)
	}
	if !strings.Contains(joined, "<thinking>") {
		t.Fatal("replayed thinking block missing")
	}
}

// switchToLive drains the replay buffer on both sides of the focus flip, so
// an event emitted between the two drains (gate still closed) is caught by
// the second drain instead of being stranded in the buffer — and everything
// replayed stays older than what then flows live.
func TestSwitchToLiveDrainsAroundFlip(t *testing.T) {
	m := dragModel(t)
	a2 := newSwitchTestAgent(t)
	s2 := m.lives.Add(a2, "second")
	s2.streaming = true
	s2.enqueue(streamStartMsg{sid: s2.id})
	s2.enqueue(streamTokenMsg{token: "early", sid: s2.id})

	m.switchToLive(1)

	if len(s2.pending) != 0 {
		t.Fatalf("pending not fully drained around the flip: %d left", len(s2.pending))
	}
	joined := strings.Join(m.chatLines, "\n")
	if !strings.Contains(joined, "early") {
		t.Fatalf("buffered token missing from transcript:\n%s", joined)
	}
}

type fakeProgram struct{ sent *[]tea.Msg }

func (f *fakeProgram) Send(m tea.Msg) { *f.sent = append(*f.sent, m) }

func TestAdapterBuffersWhenBackground(t *testing.T) {
	var sent []tea.Msg
	prog := &fakeProgram{sent: &sent}
	sess := &liveSession{id: "s3"}
	sess.focused.Store(false)

	ad := NewStreamAdapter(sess.id, prog, sess)
	ad.send(streamTokenMsg{token: "z"})
	if len(sent) != 0 || len(sess.pending) != 1 {
		t.Fatal("background event must buffer, not send")
	}

	sess.focused.Store(true)
	ad.send(streamTokenMsg{token: "y"})
	if len(sent) != 1 || len(sess.pending) != 1 {
		t.Fatal("focused event must send directly")
	}
}
