package agent

import (
	"fmt"

	"gogen/internal/llm"
)

// ToolDef pairs a tool definition with its handler. See ValidateToolSync.
type ToolDef struct {
	Definition llm.Tool
	Handler    ToolHandler
}

// toolProp creates a property definition for a tool parameter.
func toolProp(typ, desc string) map[string]interface{} {
	return map[string]interface{}{
		"type":        typ,
		"description": desc,
	}
}

// toolPropArray creates an array property definition for a tool parameter.
func toolPropArray(itemType, desc string) map[string]interface{} {
	return map[string]interface{}{
		"type": "array",
		"items": map[string]interface{}{
			"type": itemType,
		},
		"description": desc,
	}
}

// toolSchema creates a tool parameter schema with optional required fields.
func toolSchema(props map[string]interface{}, required ...string) map[string]interface{} {
	schema := map[string]interface{}{
		"type":       "object",
		"properties": props,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

// toolDef creates a complete tool definition.
func toolDef(name, desc string, params map[string]interface{}) llm.Tool {
	return llm.Tool{
		Name:        name,
		Description: desc,
		Parameters:  params,
	}
}

// BuiltinTools returns built-in tool definitions for the LLM.
func BuiltinTools() []llm.Tool {
	return []llm.Tool{
		toolDef("list_files", "List directory contents as workspace-relative paths. Recursive=true walks tree. Directories suffixed with /.",
			toolSchema(map[string]interface{}{
				"path":         toolProp("string", "Directory path"),
				"recursive":    toolProp("boolean", "Walk tree recursively (max 500)"),
				"tracked_only": toolProp("boolean", "Only git-tracked files"),
			}, "path")),
		toolDef("repo_overview", "Summarize repo layout: top-level directories, file counts, root files. Use first when exploring.",
			toolSchema(map[string]interface{}{})),
		toolDef("glob_files", "Find files by glob pattern (e.g. *.go, **/*.md).",
			toolSchema(map[string]interface{}{
				"pattern":      toolProp("string", "Glob pattern"),
				"path":         toolProp("string", "Optional subdirectory"),
				"tracked_only": toolProp("boolean", "Only git-tracked files"),
			}, "pattern")),
		toolDef("read_file", "Read file content. Use offset/limit for ranges. Search=regex jump.",
			toolSchema(map[string]interface{}{
				"file_path":    toolProp("string", "File path"),
				"path":         toolProp("string", "Alternative path (fallback)"),
				"offset":       toolProp("integer", "Start line (no search) or context lines (with search, default 10)"),
				"limit":        toolProp("integer", "Max lines (no search, default all/max 10000) or window size (with search)"),
				"search":       toolProp("string", "Regex to jump to; offset/limit become context/window"),
				"line_numbers": toolProp("boolean", "Prefix lines with numbers"),
			})),
		toolDef("read_files", "Read multiple files (max 20, 512KB). Output: === path === headers.",
			toolSchema(map[string]interface{}{
				"paths": toolPropArray("string", "File paths"),
			}, "paths")),
		toolDef("list_definitions", "List functions/types in a file with line numbers. Use before editing.",
			toolSchema(map[string]interface{}{
				"path": toolProp("string", "Source file path"),
			}, "path")),
		toolDef("write_file", "Write content to a file (creates parent dirs).",
			toolSchema(map[string]interface{}{
				"path":    toolProp("string", "File path"),
				"content": toolProp("string", "Content"),
			}, "path", "content")),
		toolDef("execute_command", "Run a shell command (destructive patterns blocked).",
			toolSchema(map[string]interface{}{
				"command": toolProp("string", "Command"),
			}, "command")),
		toolDef("run_tests", "Run project tests (auto-detects test command from project markers).",
			toolSchema(map[string]interface{}{
				"target":     toolProp("string", "Scope to package/path/pattern"),
				"extra_args": toolProp("string", "Extra args appended"),
			})),
		toolDef("run_lint", "Run project linter (auto-detects from project markers).",
			toolSchema(map[string]interface{}{
				"extra_args": toolProp("string", "Extra args appended"),
			})),
		toolDef("replace_in_file", "Replace string in file. replace_all=true for all occurrences. Prefer patch_file for multi-line.",
			toolSchema(map[string]interface{}{
				"path":        toolProp("string", "File path"),
				"search":      toolProp("string", "Exact string to find"),
				"replace":     toolProp("string", "Replacement"),
				"replace_all": toolProp("boolean", "Replace all occurrences (default: first)"),
			}, "path", "search", "replace")),
		{
			Name:        "delete_file",
			Description: "Delete a file (requires approval).",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path": map[string]interface{}{"type": "string", "description": "File path"},
				},
				"required": []string{"path"},
			},
		},
		toolDef("move_file", "Rename/move a file (creates parent dirs).",
			toolSchema(map[string]interface{}{
				"source":      toolProp("string", "Source path"),
				"destination": toolProp("string", "Destination path"),
			}, "source", "destination")),
		toolDef("patch_file", "Apply unified diff(s). dry_run=preview, fuzzy=default (tolerant).",
			toolSchema(map[string]interface{}{
				"diff": toolProp("string", "Unified diff: '--- a/path'/'+++ b/path' headers (a/b optional), '@@ -oldStart,oldCount +newStart,newCount @@' hunks.\n"+
					"Context=space, removed=minus, added=plus. Multi-file: stack sections (diff --git OK).\n\n"+
					"Example:\n--- a/main.go\n+++ b/main.go\n@@ -1,4 +1,5 @@\n package main\n \n+// comment\n func main() {\n }\n"),
				"dry_run": toolProp("boolean", "Preview without writing"),
				"fuzzy":   toolProp("boolean", "Tolerate whitespace/shift drift (default true; leave on unless exact)"),
			}, "diff")),
		toolDef("show_diff", "Show git diff (working tree or path).",
			toolSchema(map[string]interface{}{
				"path":   toolProp("string", "Optional file/dir path"),
				"staged": toolProp("boolean", "Show staged changes only"),
			})),
		toolDef("search_code", "Search codebase for regex/string. Returns file:line:content. Pair with list_definitions.",
			toolSchema(map[string]interface{}{
				"pattern":       toolProp("string", "Regex or literal"),
				"path":          toolProp("string", "Subdirectory (required for hidden dirs)"),
				"glob":          toolProp("string", "Glob filter (e.g. *.go)"),
				"context_lines": toolProp("integer", "Context lines (max 20)"),
			}, "pattern")),
		toolDef("find_references", "Find symbol references (AST when supported, text fallback).",
			toolSchema(map[string]interface{}{
				"symbol": toolProp("string", "Symbol name"),
				"path":   toolProp("string", "Optional subdirectory"),
				"glob":   toolProp("string", "Optional glob filter"),
			}, "symbol")),
		toolDef("git_log", "Recent git commits (read-only).",
			toolSchema(map[string]interface{}{
				"path":  toolProp("string", "Optional path to scope"),
				"limit": toolProp("integer", "Max commits (default 20, max 100)"),
			})),
		toolDef("git_blame", "Git blame for a file range (read-only).",
			toolSchema(map[string]interface{}{
				"path":       toolProp("string", "File path"),
				"start_line": toolProp("integer", "Start line (default 1)"),
				"limit":      toolProp("integer", "Lines to blame (default 50, max 200)"),
			}, "path")),
		toolDef("git_status", "Git status (short, read-only).",
			toolSchema(map[string]interface{}{
				"path": toolProp("string", "Optional path to scope"),
			})),
		toolDef("web_search", "Web search (DuckDuckGo Lite; no API key needed).",
			toolSchema(map[string]interface{}{
				"query":       toolProp("string", "Query"),
				"max_results": toolProp("integer", "Max results (default 10, max 20)"),
			}, "query")),
		toolDef("web_fetch", "Fetch web page, extract text (HTML stripped).",
			toolSchema(map[string]interface{}{
				"url":       toolProp("string", "URL (https)"),
				"max_bytes": toolProp("integer", "Max bytes (default 65536)"),
			}, "url")),
		toolDef("git_commit", "Commit (requires staged files).",
			toolSchema(map[string]interface{}{
				"message": toolProp("string", "Message"),
			}, "message")),
		toolDef("git_stage", "Stage files (empty = stage all).",
			toolSchema(map[string]interface{}{
				"paths": toolPropArray("string", "File paths (empty = all)"),
			})),
		toolDef("git_branch", "List/create/switch branches.",
			toolSchema(map[string]interface{}{
				"name":   toolProp("string", "Name (omit to list)"),
				"create": toolProp("boolean", "Create (true) or switch (false)"),
			})),
		toolDef("git_stash", "Stash changes (pop=true to pop).",
			toolSchema(map[string]interface{}{
				"message": toolProp("string", "Optional message"),
				"pop":     toolProp("boolean", "Pop instead of push"),
			})),
		toolDef("git_stash_list", "List stash entries.",
			toolSchema(map[string]interface{}{})),
		toolDef("git_show", "Show commit/range as diff (empty=HEAD).",
			toolSchema(map[string]interface{}{
				"ref": toolProp("string", "Ref (hash, tag, range; empty=HEAD)"),
			})),
		toolDef("copy_file", "Copy file (creates parent dirs).",
			toolSchema(map[string]interface{}{
				"source":      toolProp("string", "Source"),
				"destination": toolProp("string", "Destination"),
			}, "source", "destination")),
		toolDef("todo_add", "Add todo item.",
			toolSchema(map[string]interface{}{
				"text": toolProp("string", "Text"),
			}, "text")),
		toolDef("todo_list", "List todos with status.",
			toolSchema(map[string]interface{}{})),
		toolDef("todo_done", "Mark todo done by ID.",
			toolSchema(map[string]interface{}{
				"id": toolProp("integer", "ID"),
			}, "id")),
		toolDef("todo_remove", "Remove todo by ID.",
			toolSchema(map[string]interface{}{
				"id": toolProp("integer", "ID"),
			}, "id")),
		toolDef("todo_clear_done", "Clear completed todos.",
			toolSchema(map[string]interface{}{})),
		toolDef("find_file", "Find files by name (case-insensitive substring). Faster than glob for known filenames.",
			toolSchema(map[string]interface{}{
				"name":  toolProp("string", "Filename substring"),
				"path":  toolProp("string", "Optional subdirectory"),
				"limit": toolProp("integer", "Max results (default 50)"),
			}, "name")),
		toolDef("find_definition", "Cross-file go-to-definition (tree-sitter or text fallback).",
			toolSchema(map[string]interface{}{
				"symbol": toolProp("string", "Symbol name"),
				"path":   toolProp("string", "Optional subdirectory"),
				"glob":   toolProp("string", "Optional glob filter"),
			}, "symbol")),
		toolDef("session_rename", "Rename current session.",
			toolSchema(map[string]interface{}{
				"label": toolProp("string", "Label"),
			}, "label")),
		toolDef("session_usage", "Show session token usage.",
			toolSchema(map[string]interface{}{})),
		toolDef("context_pin_last", "Pin last user message to survive compaction.",
			toolSchema(map[string]interface{}{})),
		toolDef("context_pins", "List pinned messages.",
			toolSchema(map[string]interface{}{})),
		toolDef("rename_symbol", "Rename symbol across files (AST or text fallback).",
			toolSchema(map[string]interface{}{
				"old_name": toolProp("string", "Current name"),
				"new_name": toolProp("string", "New name"),
				"path":     toolProp("string", "Optional subdirectory"),
				"glob":     toolProp("string", "Optional glob filter"),
				"dry_run":  toolProp("boolean", "Preview only"),
			}, "old_name", "new_name")),
		toolDef("multi_edit", "Same text replacement across multiple files.",
			toolSchema(map[string]interface{}{
				"pattern": toolProp("string", "Glob pattern (e.g. *.go)"),
				"search":  toolProp("string", "String to find"),
				"replace": toolProp("string", "Replacement"),
				"dry_run": toolProp("boolean", "Preview only"),
			}, "pattern", "search", "replace")),
		toolDef("call_graph", "Call relationships for a symbol.",
			toolSchema(map[string]interface{}{
				"symbol":    toolProp("string", "Symbol"),
				"path":      toolProp("string", "Optional subdirectory"),
				"glob":      toolProp("string", "Optional glob filter"),
				"direction": toolProp("string", "callers, callees, or both"),
			}, "symbol")),
		toolDef("dependency_analysis", "Impact analysis for a symbol (dependents, risk).",
			toolSchema(map[string]interface{}{
				"symbol": toolProp("string", "Symbol"),
				"path":   toolProp("string", "Optional subdirectory"),
			}, "symbol")),
		toolDef("extract_function", "Extract code block into a new function.",
			toolSchema(map[string]interface{}{
				"file":       toolProp("string", "File"),
				"start_line": toolProp("integer", "Start line"),
				"end_line":   toolProp("integer", "End line"),
				"func_name":  toolProp("string", "Function name"),
			}, "file", "start_line", "end_line", "func_name")),
		toolDef("generate_test", "Generate tests (table-driven or subtests).",
			toolSchema(map[string]interface{}{
				"func_name": toolProp("string", "Function"),
				"file":      toolProp("string", "Optional file (auto-detect)"),
				"style":     toolProp("string", "table-driven or subtests"),
			}, "func_name")),
	}
}

// ValidateToolSync checks that BuiltinTools and BuiltinToolHandlers agree
// on every tool name. Call this from unit tests to catch drift.
func ValidateToolSync() error {
	defs := BuiltinTools()
	handlers := BuiltinToolHandlers()
	for _, d := range defs {
		if _, ok := handlers[d.Name]; !ok {
			return fmt.Errorf("tool %q has definition but no handler", d.Name)
		}
	}
	for name := range handlers {
		found := false
		for _, d := range defs {
			if d.Name == name {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("tool %q has handler but no definition", name)
		}
	}
	return nil
}
