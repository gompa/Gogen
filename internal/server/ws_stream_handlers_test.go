package server

import "testing"

// TestWSStreamSinkOnStreamStatsFrame pins the wire contract of the token
// rate frame: the client's progress label reads data.tokensPerSec from a
// stream_stats frame (the shared SpeedMeter's smoothed rate, identical to
// the TUI's figure).
func TestWSStreamSinkOnStreamStatsFrame(t *testing.T) {
	var got []WSMessage
	sk := &wsStreamSink{write: func(m WSMessage) { got = append(got, m) }}
	sk.OnStreamStats(42.4)
	if len(got) != 1 {
		t.Fatalf("frames = %d, want 1", len(got))
	}
	if got[0].Type != "stream_stats" || got[0].TokensPerSec != 42.4 {
		t.Fatalf("frame = %+v, want stream_stats with tokensPerSec 42.4", got[0])
	}
}
