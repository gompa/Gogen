package debuglog

import (
	"os"
	"testing"
)

func TestDeriveResponseLogPath(t *testing.T) {
	// Unset GOGEN_RESPONSE_LOG so the derivation logic is tested directly.
	os.Unsetenv("GOGEN_RESPONSE_LOG")

	t.Parallel()
	cases := []struct {
		debugPath string
		sessionID string
		want      string
	}{
		{
			debugPath: "/tmp/gogen-logs/debug-4a318a.log",
			sessionID: "4a318a",
			want:      "/tmp/gogen-logs/llm-responses-4a318a.jsonl",
		},
		{
			debugPath: "/tmp/gogen-logs/debug-abc.log",
			sessionID: "",
			want:      "/tmp/gogen-logs/llm-responses-abc.jsonl",
		},
		{
			debugPath: "",
			sessionID: "",
			want:      "",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.want, func(t *testing.T) {
			t.Parallel()
			if got := deriveResponseLogPath(tc.debugPath, tc.sessionID); got != tc.want {
				t.Fatalf("deriveResponseLogPath() = %q, want %q", got, tc.want)
			}
		})
	}
}
