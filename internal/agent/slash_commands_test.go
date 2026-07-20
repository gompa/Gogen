package agent

import (
	"strings"
	"testing"
)

func TestHandleHelpCommand(t *testing.T) {
	out, ok := HandleHelpCommand("/help", false, true)
	if !ok {
		t.Fatal("expected handled")
	}
	if !strings.Contains(out, "/resume") || !strings.Contains(out, "/models") {
		t.Fatalf("help missing commands: %q", out)
	}
	if !strings.Contains(out, "/compact") {
		t.Fatalf("TUI help should include /compact: %q", out)
	}
	webOut, ok := HandleHelpCommand("/help", true, false)
	if !ok {
		t.Fatal("expected handled")
	}
	if strings.Contains(webOut, "/compact") {
		t.Fatalf("web help should omit TUI-only /compact: %q", webOut)
	}
	if _, ok := HandleHelpCommand("hello", true, true); ok {
		t.Fatal("expected not handled")
	}
}

func TestMatchSlashCommands(t *testing.T) {
	matches := MatchSlashCommands("/res", true, true)
	if len(matches) != 1 || matches[0].Name != "/resume" {
		t.Fatalf("got %#v", matches)
	}
	matches = MatchSlashCommands("/", true, false)
	for _, m := range matches {
		if !m.Web {
			t.Fatalf("web-only filter leaked %s", m.Name)
		}
	}
	if len(matches) == 0 {
		t.Fatal("expected web commands")
	}
}

func TestSlashCommandCompletions(t *testing.T) {
	got := SlashCommandCompletions("/res", false, true)
	if len(got) != 1 || got[0] != "/resume" {
		t.Fatalf("got %v", got)
	}
	if SlashCommandCompletions("/resume foo", false, true) != nil {
		t.Fatal("should not complete when args present")
	}
	if SlashCommandCompletions("resume", false, true) != nil {
		t.Fatal("should require leading slash")
	}
}
