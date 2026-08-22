package agent

import (
	"context"
	"fmt"
	"strings"

	"gogen/internal/llm"
)

// skillsToolDef is the LLM-facing schema for the skill tool. Like the board
// tool it lives OUTSIDE builtinToolDefs (MCP-style gating): exposed only
// when the skills feature is enabled AND a manager is installed, so when
// skills: off the word "skill" appears nowhere in the tool registry.
//
// Plan-mode note: skill list/read are read-only, so the tool is plan-mode
// allowed (the board-style coordination exception in allowsTool).
func skillsToolDef() llm.Tool {
	return toolDef("skill",
		"Skill tool: list or read skills — structured, discoverable instructions from "+
			"<project>/.gogen/skills or ~/.config/gogen/skills (bundle dirs with SKILL.md or flat <name>.md files). "+
			"action=list returns available skills with descriptions; action=read <name> loads the full skill body "+
			"into context. Use when a task matches a listed skill; the body contains project-specific procedures, "+
			"conventions, or checklists.",
		toolSchema(
			map[string]any{
				"action": toolPropEnum("string", []string{"list", "read"}, "Skill action"),
				"name":   toolProp("string", "Skill name (action=read)"),
			},
			[]string{"action"}...,
		),
	)
}

// handleSkill executes a skill tool call. Reachable only when the skills
// feature is enabled (all tool surfaces gate on SkillsEnabled), so the
// manager nil-guard is the only check needed here.
func handleSkill(_ context.Context, a *Agent, args map[string]any) (string, error) {
	m := a.SkillsManager()
	if m == nil {
		return "", fmt.Errorf("skills manager is not initialized")
	}
	action, err := stringArg(args, "action")
	if err != nil {
		return "", err
	}
	switch action {
	case "list":
		skills := m.List()
		if len(skills) == 0 {
			return "No skills found.", nil
		}
		var b strings.Builder
		for _, s := range skills {
			if s.Description != "" {
				fmt.Fprintf(&b, "%s — %s\n", s.Name, s.Description)
			} else {
				b.WriteString(s.Name + "\n")
			}
		}
		return strings.TrimRight(b.String(), "\n"), nil
	case "read":
		name, err := stringArg(args, "name")
		if err != nil {
			return "", err
		}
		s, err := m.Read(name)
		if err != nil {
			return "", err
		}
		header := "# " + s.Name
		if s.Description != "" {
			header += "\n\n" + s.Description
		}
		return header + "\n\n" + s.Body, nil
	default:
		return "", fmt.Errorf("unknown skill action %q", action)
	}
}
