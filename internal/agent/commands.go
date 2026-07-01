package agent

import (
	"fmt"
	"strings"
)

// HandleModeCommand processes /plan, /act, and /mode.
func (a *Agent) HandleModeCommand(input string) (string, bool) {
	trimmed := strings.TrimSpace(input)
	switch trimmed {
	case "/plan", "plan":
		a.SetMode(ModePlan)
		return "Plan mode on (read-only). Use /act to implement.", true
	case "/act", "act":
		a.SetMode(ModeAct)
		return "Act mode on.", true
	case "/mode", "mode":
		return fmt.Sprintf("Mode: %s", a.Mode.String()), true
	default:
		return "", false
	}
}
