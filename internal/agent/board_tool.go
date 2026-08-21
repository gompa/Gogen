package agent

import (
	"context"
	"fmt"
	"strconv"

	"gogen/internal/llm"
)

// boardToolDef is the LLM-facing schema for the board tool. It deliberately
// lives OUTSIDE builtinToolDefs (MCP-style gating): the tool is only exposed
// when the board feature is enabled, so when board: off the word "board"
// appears nowhere in the tool registry, the model-facing list, or the
// allowlist. The schema and handler stay co-located here so they cannot
// drift.
//
// D7: no plan-mode gating — the board is the coordination exception (like
// todo): an agent may update the board in plan mode so it can mark items for
// review. Documented behavior change from plan mode's otherwise read-only
// contract.
func boardToolDef() llm.Tool {
	return toolDef("board",
		"Board tool: project-wide kanban board of items available for agents to fix. "+
			"Actions: list (all cards), show <id> (card detail), add <title> [description] [priority], "+
			"claim <id> (assign to self and move to in_progress), move <id> <column>, "+
			"block <id> <reason>, comment <id> <text>, done <id>, remove <id> (delete a card entirely).",
		toolSchema(
			map[string]interface{}{
				"action":      toolPropEnum("string", []string{"list", "show", "add", "claim", "move", "block", "comment", "done", "remove"}, "Board action"),
				"id":          toolProp("integer", "Board item id (show, claim, move, block, comment, done, remove)"),
				"title":       toolProp("string", "Item title (add)"),
				"description": toolProp("string", "Acceptance criteria / details (add)"),
				"priority":    toolProp("string", "Priority: low, medium, high, urgent (add)"),
				"column":      toolPropEnum("string", BoardColumns, "Target column (move)"),
				"reason":      toolProp("string", "Block reason (block)"),
				"text":        toolProp("string", "Comment text (comment)"),
			},
			[]string{"action"}...,
		),
	)
}

// handleBoard executes a board tool call. The handler is only reachable when
// the board feature is enabled (llmTools/AllowedToolNames/executeTool all
// gate on a.BoardEnabled()), so no extra flag check is needed here beyond the
// manager nil-guard (mirrors the MCP registry contract).
func handleBoard(_ context.Context, a *Agent, args map[string]interface{}) (string, error) {
	m := a.BoardManager()
	if m == nil {
		return "", fmt.Errorf("board manager is not initialized")
	}
	action, err := stringArg(args, "action")
	if err != nil {
		return "", err
	}
	by := a.actorLabel()
	var out string
	var opErr error
	switch action {
	case "list":
		return m.List()
	case "show":
		id, err := intRequiredArg(args, "id")
		if err != nil {
			return "", err
		}
		return m.Show(strconv.Itoa(id))
	case "add":
		title, err := stringArg(args, "title")
		if err != nil {
			return "", err
		}
		desc, _ := stringArgOptional(args, "description")
		prio, _ := stringArgOptional(args, "priority")
		out, opErr = m.Add(title, desc, prio, by)
	case "claim":
		id, err := intRequiredArg(args, "id")
		if err != nil {
			return "", err
		}
		out, opErr = m.Claim(strconv.Itoa(id), by)
	case "move":
		id, err := intRequiredArg(args, "id")
		if err != nil {
			return "", err
		}
		column, err := stringArg(args, "column")
		if err != nil {
			return "", err
		}
		out, opErr = m.Move(strconv.Itoa(id), column, by)
	case "block":
		id, err := intRequiredArg(args, "id")
		if err != nil {
			return "", err
		}
		reason, err := stringArg(args, "reason")
		if err != nil {
			return "", err
		}
		out, opErr = m.Block(strconv.Itoa(id), reason, by)
	case "comment":
		id, err := intRequiredArg(args, "id")
		if err != nil {
			return "", err
		}
		text, err := stringArg(args, "text")
		if err != nil {
			return "", err
		}
		out, opErr = m.Comment(strconv.Itoa(id), text, by)
	case "done":
		id, err := intRequiredArg(args, "id")
		if err != nil {
			return "", err
		}
		out, opErr = m.Done(strconv.Itoa(id), by)
	case "remove":
		id, err := intRequiredArg(args, "id")
		if err != nil {
			return "", err
		}
		out, opErr = m.Delete(strconv.Itoa(id))
	default:
		return "", fmt.Errorf("unknown board action %q (want list, show, add, claim, move, block, comment, done, or remove)", action)
	}
	if opErr == nil {
		// Successful mutation: notify the web server so every open board
		// tab re-renders AND the user gets a toast (agent-triggered board
		// changes are visible even when not initiated by the user). The
		// message is the same output the model sees as the tool result.
		// No-op in TUI/CLI.
		a.notifyBoardChanged(out)
	}
	return out, opErr
}

// actorLabel identifies who performed a board mutation: the session label
// when the session has one, else the session id.
func (a *Agent) actorLabel() string {
	if label := a.SessionLabelSnapshot(); label != "" {
		return label
	}
	return a.SessionID
}
