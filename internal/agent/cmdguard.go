package agent

import (
	"fmt"
	"regexp"
	"strings"
)

// CommandGuard controls which shell commands the agent may run.
type CommandGuard struct {
	Mode      string   // blocklist, allowlist, off
	Allowlist []string // first token or full prefix match when Mode=allowlist
}

func NewCommandGuard(mode string, allowlist []string) *CommandGuard {
	mode = strings.ToLower(strings.TrimSpace(mode))
	switch mode {
	case "allowlist", "blocklist", "off":
	default:
		mode = "blocklist"
	}
	return &CommandGuard{Mode: mode, Allowlist: allowlist}
}

var blockedCommandPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bsudo\b`),
	regexp.MustCompile(`(?i)\bsu\s+-`),
	regexp.MustCompile(`(?i)\brm\s+(-[^\s]*\s+)*-[^\s]*r[^\s]*\s+(/|\./\.\./|/\*|~|\$HOME|\$\{HOME\}|\.)`),
	regexp.MustCompile(`(?i)\brm\s+(-[^\s]*\s+)*(/|\./\.\./|/\*|~|\$HOME|\$\{HOME\})`),
	regexp.MustCompile(`(?i)\bmkfs\b`),
	regexp.MustCompile(`(?i)\bdd\s+(if=|of=)`),
	regexp.MustCompile(`(?i)\bchmod\s+(-[^\s]*\s+)*777\b`),
	regexp.MustCompile(`(?i)\bchown\s+(-[^\s]*\s+)*(-R\s+)?/`),
	regexp.MustCompile(`(?i)(curl|wget)[^\n|]*\|\s*(ba)?sh\b`),
	regexp.MustCompile(`(?i)\|\s*(ba)?sh\s*(\s|$|<)`),
	regexp.MustCompile(`(?i)\|\s*(python3?|perl|ruby|node|zsh|php)\b`),
	regexp.MustCompile(`(?i)\b(python3?|perl|ruby|node)\s+-[ce]\b`),
	regexp.MustCompile(`(?i)\bfind\b[^\n]*\s-delete\b`),
	regexp.MustCompile(`(?i)\bshutdown\b`),
	regexp.MustCompile(`(?i)\breboot\b`),
	regexp.MustCompile(`(?i)\bpoweroff\b`),
	regexp.MustCompile(`(?i)\bkill\s+-9\s+1\b`),
	regexp.MustCompile(`(?i)\b:\(\)\s*\{\s*:\s*\|\s*:\s*&\s*\}\s*;\s*:`), // fork bomb
	regexp.MustCompile(`(?i)\bnc\s+(-[^\s]*\s+)*-e\b`),
	regexp.MustCompile(`(?i)\b(nmap|masscan)\b`),
}

func (g *CommandGuard) Check(command string) error {
	if g == nil || g.Mode == "off" {
		return nil
	}
	command = strings.TrimSpace(command)
	if command == "" {
		return fmt.Errorf("empty command")
	}

	switch g.Mode {
	case "allowlist":
		return g.checkAllowlist(command)
	default:
		return g.checkBlocklist(command)
	}
}

func (g *CommandGuard) checkAllowlist(command string) error {
	if len(g.Allowlist) == 0 {
		return fmt.Errorf("command blocked: allowlist mode is enabled but GOGEN_COMMAND_ALLOWLIST is empty")
	}
	if err := rejectShellMetacharacters(command); err != nil {
		return err
	}
	lower := strings.ToLower(command)
	for _, allowed := range g.Allowlist {
		allowed = strings.TrimSpace(strings.ToLower(allowed))
		if allowed == "" {
			continue
		}
		if lower == allowed || strings.HasPrefix(lower, allowed+" ") || strings.HasPrefix(lower, allowed+"\t") {
			return nil
		}
	}
	return fmt.Errorf("command blocked by allowlist: %q", command)
}

func rejectShellMetacharacters(command string) error {
	if strings.ContainsAny(command, ";|&`$()<>\n\r") {
		return fmt.Errorf("command blocked by allowlist: shell metacharacters are not permitted")
	}
	return nil
}

func (g *CommandGuard) checkBlocklist(command string) error {
	for _, re := range blockedCommandPatterns {
		if re.MatchString(command) {
			return fmt.Errorf("command blocked by safety policy (matched %q)", re.String())
		}
	}
	return nil
}

func ParseAllowlist(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
