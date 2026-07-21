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
		toolDef("list_files", "List files and directories. Set recursive=true to walk the tree (skips .git, node_modules, etc.). Directories are suffixed with /.",
			toolSchema(map[string]interface{}{
				"path":         toolProp("string", "Path to directory"),
				"recursive":    toolProp("boolean", "If true, list all entries recursively (max 500)"),
				"tracked_only": toolProp("boolean", "If true, only show git-tracked files"),
			}, "path")),
		toolDef("repo_overview", "Summarize repository layout: top-level directories with file counts and file counts and root files. Use first when exploring an unfamiliar codebase.",
			toolSchema(map[string]interface{}{})),
		toolDef("glob_files", "Find files by glob pattern under the working directory, e.g. *.go, internal/*.go, **/*.md",
			toolSchema(map[string]interface{}{
				"pattern":      toolProp("string", "Glob pattern"),
				"path":         toolProp("string", "Optional subdirectory to search under"),
				"tracked_only": toolProp("boolean", "If true, only show git-tracked files"),
			}, "pattern")),
		toolDef("read_file", "Read file content. With search, offset=context lines, limit=max returned lines.",
			toolSchema(map[string]interface{}{
				"file_path":   toolProp("string", "Path to file (required unless path is provided)"),
				"path":        toolProp("string", "Alternative path to file (fallback if file_path is empty)"),
				"offset":      toolProp("integer", "Without search: 1-based starting line (default 1). With search: lines of context before/after the match (default 10) — not a starting line."),
				"limit":       toolProp("integer", "Without search: max lines to read (default all, capped at 10000). With search: max lines returned around the match."),
				"search":      toolProp("string", "Optional regex; jump to first matching line. When set, offset means context lines (not start line) and limit caps the window size."),
				"line_numbers": toolProp("boolean", "If true, prefix each line with its line number (e.g. '42: content'). Numbers are right-aligned for readability."),
			})),
		toolDef("read_files", "Read multiple files in one call. Output uses === path === headers (max 20 files, 512KB total).",
			toolSchema(map[string]interface{}{
				"paths": toolPropArray("string", "File paths to read"),
			}, "paths")),
		toolDef("list_definitions", "List functions, methods, classes, and types in a source file with line numbers. Use before editing unfamiliar files; pair with search_code to locate files first.",
			toolSchema(map[string]interface{}{
				"path": toolProp("string", "Path to source file"),
			}, "path")),
		toolDef("write_file", "Write content to a file, creating parent directories as needed.",
			toolSchema(map[string]interface{}{
				"path":    toolProp("string", "Path to file"),
				"content": toolProp("string", "Content to write"),
			}, "path", "content")),
		toolDef("execute_command", "Execute a shell command in the working directory. Blocked patterns include sudo, rm -rf /, and curl|bash. Configure GOGEN_COMMAND_SAFETY=allowlist and GOGEN_COMMAND_ALLOWLIST for stricter control.",
			toolSchema(map[string]interface{}{
				"command": toolProp("string", "Shell command to run"),
			}, "command")),
		toolDef("run_tests", "Run the project test suite. Uses test_command from GOGEN.md when set, otherwise detects from ecosystem markers (go.mod, package.json, Cargo.toml, etc.).",
			toolSchema(map[string]interface{}{
				"target":     toolProp("string", "Optional package, path, or test pattern to scope the run"),
				"extra_args": toolProp("string", "Optional extra arguments appended to the test command"),
			})),
		toolDef("run_lint", "Run the project linter. Uses lint_command from GOGEN.md when set, otherwise detects from ecosystem markers (go vet, ruff, clippy, etc.).",
			toolSchema(map[string]interface{}{
				"extra_args": toolProp("string", "Optional extra arguments appended to the lint command"),
			})),
		toolDef("replace_in_file", "Replace a search string in a file. Default replaces the first occurrence; set replace_all=true for every occurrence. Prefer patch_file for multi-line or precise edits.",
			toolSchema(map[string]interface{}{
				"path":        toolProp("string", "Path to file"),
				"search":      toolProp("string", "Exact search string"),
				"replace":     toolProp("string", "Replacement string"),
				"replace_all": toolProp("boolean", "If true, replace all occurrences (default: first only)"),
			}, "path", "search", "replace")),
		{
			Name:        "delete_file",
			Description: "Delete a file. Requires explicit user approval before the delete runs.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path": map[string]interface{}{"type": "string", "description": "Path to file to delete"},
				},
				"required": []string{"path"},
			},
		},
		toolDef("move_file", "Rename or move a file within the working directory. Creates destination directories as needed. Does not move directories.",
			toolSchema(map[string]interface{}{
				"source":      toolProp("string", "Path to the file to move"),
				"destination": toolProp("string", "Destination path for the file"),
			}, "source", "destination")),
		toolDef("patch_file", "Apply unified diffs to files. Use dry_run=true to preview, fuzzy=true (default) for tolerance.",
			toolSchema(map[string]interface{}{
				"diff": toolProp("string", "Unified diff text. Format:\n"+
					"Each file section starts with '--- a/path' and '+++ b/path' (a/ and b/ prefixes are optional).\n"+
					"Then one or more hunks: '@@ -oldStart,oldCount +newStart,newCount @@' followed by context lines (space prefix), removed lines (minus prefix), and added lines (plus prefix).\n"+
					"Multi-file patches stack multiple ---/+++/@@ sections back-to-back (blank lines or 'diff --git' headers between files are OK).\n\n"+
					"Example (single file, single hunk):\n"+
					"--- a/main.go\n+++ b/main.go\n@@ -1,4 +1,5 @@\n package main\n \n+// new comment\n func main() {\n }\n"+
					"Example (two files):\n"+
					"--- a/foo.go\n+++ b/foo.go\n@@ -3,2 +3,3 @@\n x := 1\n+y := 2\n z := 3\n--- a/bar.go\n+++ b/bar.go\n@@ -10,1 +10,2 @@\n result := compute()\n+cache(result)\n"),
				"dry_run": toolProp("boolean", "If true, validate the patch and report what would change without writing. Set to false (or omit) to actually apply."),
				"fuzzy":   toolProp("boolean", "If false, require exact byte-for-byte context match. Omit or set true (the default) to tolerate: trailing whitespace on context lines, context lines shifted by a few lines (e.g. when a file header was added above the target region), and whitespace-only differences between context and the actual file. You almost always want the default (fuzzy=true)."),
			}, "diff")),
		toolDef("show_diff", "Show git diff for the working tree or a specific path. Requires a git repository.",
			toolSchema(map[string]interface{}{
				"path":   toolProp("string", "Optional file or directory path"),
				"staged": toolProp("boolean", "If true, show staged (cached) changes only"),
			})),
		toolDef("search_code", "Search for a regex or string in the codebase. Returns file:line:content matches. Use context_lines for surrounding lines (max 20). Pair with list_definitions to outline before reading.",
			toolSchema(map[string]interface{}{
				"pattern":       toolProp("string", "Regex or literal string to search for"),
				"path":          toolProp("string", "Optional subdirectory under the working directory to search (required for hidden dirs like .github)"),
				"glob":          toolProp("string", "Optional glob filter on match paths, e.g. *.go or internal/*.go"),
				"context_lines": toolProp("integer", "Optional lines of context before/after each match (max 20)"),
			}, "pattern")),
		toolDef("find_references", "Find all references to a symbol. Uses AST when supported; falls back to text search.",
			toolSchema(map[string]interface{}{
				"symbol": toolProp("string", "Symbol name to find references for"),
				"path":   toolProp("string", "Optional subdirectory to search under"),
				"glob":   toolProp("string", "Optional glob filter, e.g. *.go"),
			}, "symbol")),
		toolDef("git_log", "Show recent git commit history. Read-only; available in plan mode.",
			toolSchema(map[string]interface{}{
				"path":  toolProp("string", "Optional file or directory path to scope history"),
				"limit": toolProp("integer", "Max commits to show (default 20, max 100)"),
			})),
		toolDef("git_blame", "Show git blame for a file within a line range. Read-only; available in plan mode.",
			toolSchema(map[string]interface{}{
				"path":       toolProp("string", "Path to file"),
				"start_line": toolProp("integer", "Optional 1-based starting line (default 1)"),
				"limit":      toolProp("integer", "Optional number of lines to blame (default 50, max 200)"),
			}, "path")),
		toolDef("git_status", "Show git working tree status (short format). Read-only; available in plan mode.",
			toolSchema(map[string]interface{}{
				"path": toolProp("string", "Optional file or directory path to scope status"),
			})),
		toolDef("web_search", "Search the web for current information. Uses DuckDuckGo Lite by default (no API key needed). Set GOGEN_WEB_SEARCH_BACKEND=brave and GOGEN_WEB_SEARCH_API_KEY for better results.",
			toolSchema(map[string]interface{}{
				"query":       toolProp("string", "Search query"),
				"max_results": toolProp("integer", "Optional max results (default 10, max 20)"),
			}, "query")),
		toolDef("web_fetch", "Fetch and extract text content from a URL. Use for reading documentation, API references, or any web page. HTML is stripped to plain text. Set GOGEN_WEB_FETCH=off to disable.",
			toolSchema(map[string]interface{}{
				"url":       toolProp("string", "URL to fetch (https required by default)"),
				"max_bytes": toolProp("integer", "Optional max bytes to download (default 65536)"),
			}, "url")),
		toolDef("git_commit", "Create a git commit with the given message. Requires files to be staged first (use git_stage).",
			toolSchema(map[string]interface{}{
				"message": toolProp("string", "Commit message"),
			}, "message")),
		toolDef("git_stage", "Stage files for commit. When paths is empty or omitted, stages all changes (git add -A).",
			toolSchema(map[string]interface{}{
				"paths": toolPropArray("string", "Optional file paths to stage (empty = stage all)"),
			})),
		toolDef("git_branch", "List, create (create=true), or switch (create=false) git branches.",
			toolSchema(map[string]interface{}{
				"name":   toolProp("string", "Branch name (omit to list)"),
				"create": toolProp("boolean", "If true, create a new branch; if false and name given, switch to it"),
			})),
		toolDef("git_stash", "Push changes to the stash (or pop when pop=true).",
			toolSchema(map[string]interface{}{
				"message": toolProp("string", "Optional stash message"),
				"pop":     toolProp("boolean", "If true, pop the most recent stash instead of pushing"),
			})),
		toolDef("git_stash_list", "List all stash entries.",
			toolSchema(map[string]interface{}{})),
		toolDef("git_show", "Show a specific commit or range as a unified diff. When ref is empty, shows HEAD.",
			toolSchema(map[string]interface{}{
				"ref": toolProp("string", "Git ref (commit hash, tag, or range; empty = HEAD)"),
			})),
		toolDef("copy_file", "Copy a file within the working directory. Creates destination directories as needed.",
			toolSchema(map[string]interface{}{
				"source":      toolProp("string", "Source file path"),
				"destination": toolProp("string", "Destination file path"),
			}, "source", "destination")),
		toolDef("todo_add", "Add a new todo item to the task tracker.",
			toolSchema(map[string]interface{}{
				"text": toolProp("string", "Todo text"),
			}, "text")),
		toolDef("todo_list", "List all todo items with their status.",
			toolSchema(map[string]interface{}{})),
		toolDef("todo_done", "Mark a todo item as completed by its ID.",
			toolSchema(map[string]interface{}{
				"id": toolProp("integer", "Todo item ID"),
			}, "id")),
		toolDef("todo_remove", "Remove a todo item entirely by its ID.",
			toolSchema(map[string]interface{}{
				"id": toolProp("integer", "Todo item ID"),
			}, "id")),
		toolDef("todo_clear_done", "Clear all completed todo items.",
			toolSchema(map[string]interface{}{})),
		toolDef("find_file", "Find files by name (case-insensitive substring match). Faster than glob_files when you know part of the filename but not the path.",
			toolSchema(map[string]interface{}{
				"name":  toolProp("string", "Filename or substring to match"),
				"path":  toolProp("string", "Optional subdirectory to search under"),
				"limit": toolProp("integer", "Max results (default 50)"),
			}, "name")),
		toolDef("find_definition", "Find which file defines a symbol (cross-file go-to-definition). Scans supported languages via tree-sitter when available; falls back to text search.",
			toolSchema(map[string]interface{}{
				"symbol": toolProp("string", "Symbol name to find (function, type, variable, etc.)"),
				"path":   toolProp("string", "Optional subdirectory to search under"),
				"glob":   toolProp("string", "Optional glob filter, e.g. *.go"),
			}, "symbol")),
		toolDef("session_rename", "Rename or label the current session for easier identification when listing sessions.",
			toolSchema(map[string]interface{}{
				"label": toolProp("string", "New session label"),
			}, "label")),
		toolDef("session_usage", "Show accumulated token usage for the session (turns, tokens in/out).",
			toolSchema(map[string]interface{}{})),
		toolDef("context_pin_last", "Pin the most recent user message so it survives context compaction. Useful to preserve critical context like requirements or error messages.",
			toolSchema(map[string]interface{}{})),
		toolDef("context_pins", "List all pinned messages that will survive context compaction.",
			toolSchema(map[string]interface{}{})),
		toolDef("rename_symbol", "Rename a symbol across all files. Uses AST for supported languages, falls back to word-boundary text search.",
			toolSchema(map[string]interface{}{
				"old_name": toolProp("string", "Current symbol name"),
				"new_name": toolProp("string", "New symbol name"),
				"path":     toolProp("string", "Optional subdirectory to scope"),
				"glob":     toolProp("string", "Optional glob filter"),
				"dry_run":  toolProp("boolean", "Preview changes without applying"),
			}, "old_name", "new_name")),
		toolDef("multi_edit", "Apply the same transformation across multiple files. Language-agnostic text replacement.",
			toolSchema(map[string]interface{}{
				"pattern": toolProp("string", "Glob pattern to match files (e.g., *.go)"),
				"search":  toolProp("string", "String to search for"),
				"replace": toolProp("string", "Replacement string"),
				"dry_run": toolProp("boolean", "Preview changes without applying"),
			}, "pattern", "search", "replace")),
		toolDef("call_graph", "Analyze call relationships for a symbol (callers and callees).",
			toolSchema(map[string]interface{}{
				"symbol":    toolProp("string", "Function or method name"),
				"path":      toolProp("string", "Optional subdirectory to scope"),
				"glob":      toolProp("string", "Optional glob filter"),
				"direction": toolProp("string", "callers, callees, or both (default)"),
			}, "symbol")),
		toolDef("dependency_analysis", "Analyze impact of changing a symbol: dependents, risk score, and recommendations.",
			toolSchema(map[string]interface{}{
				"symbol": toolProp("string", "Symbol to analyze"),
				"path":   toolProp("string", "Optional subdirectory to scope"),
			}, "symbol")),
		toolDef("extract_function", "Extract a block of code into a new function. Analyzes inputs and outputs.",
			toolSchema(map[string]interface{}{
				"file":       toolProp("string", "Source file path"),
				"start_line": toolProp("integer", "Starting line number"),
				"end_line":   toolProp("integer", "Ending line number"),
				"func_name":  toolProp("string", "Name for the new function"),
			}, "file", "start_line", "end_line", "func_name")),
		toolDef("generate_test", "Generate test cases for a function. Supports table-driven and subtests styles.",
			toolSchema(map[string]interface{}{
				"func_name": toolProp("string", "Function to test"),
				"file":      toolProp("string", "Optional file path (auto-detect if empty)"),
				"style":     toolProp("string", "table-driven or subtests (default)"),
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
