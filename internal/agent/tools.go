package agent

import (
	"gogen/internal/llm"
)

// ToolDef is the single registration record for a builtin tool: its LLM-facing
// schema and the handler that executes it. builtinToolDefs is the one source of
// truth — BuiltinTools and BuiltinToolHandlers are derived views of it, so a
// tool's schema and handler can never drift apart.
type ToolDef struct {
	Definition llm.Tool
	Handler    ToolHandler

	// ReadOnly marks handlers safe to execute concurrently within a single
	// turn: no workspace mutation, no session-state mutation, and no shell
	// execution, whose shared state is either internally synchronized or
	// only read. ReadOnly implies PlanAllowed.
	ReadOnly bool

	// PlanAllowed marks the tool as available in plan mode. Read-only tools
	// set both flags; the few session-local mutations allowed in plan mode
	// (todo_add, session_rename, context_pin_last) set PlanAllowed without
	// ReadOnly.
	PlanAllowed bool

	// MutatesFS marks tools that write to the working tree. The server wraps
	// these handlers with the workspace fsMu so editor saves and agent file
	// mutations stay serialized. MutatesFS tools are never ReadOnly and
	// never PlanAllowed.
	MutatesFS bool
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

// builtinToolDefs is the single registration table for builtin tools. Order is
// significant: it is the order tools are sent to the LLM provider, and provider
// prompt caches are keyed on the serialized tool list. Keep new tools appended
// at the end; do not reorder existing entries.
var builtinToolDefs = []ToolDef{
	{
		Definition: toolDef("list_files", "List directory contents as workspace-relative paths. Recursive=true walks tree (max 500 entries). Directories suffixed with /.",
			toolSchema(map[string]interface{}{
				"path":         toolProp("string", "Directory path"),
				"recursive":    toolProp("boolean", "Walk tree recursively (max 500)"),
				"tracked_only": toolProp("boolean", "Only git-tracked files"),
			}, "path")),
		Handler:     handleListFiles,
		ReadOnly:    true,
		PlanAllowed: true,
	},
	{
		Definition: toolDef("repo_overview", "Summarize repo layout: top-level directories, file counts, root files. Use first when exploring.",
			toolSchema(map[string]interface{}{})),
		Handler:     handleRepoOverview,
		ReadOnly:    true,
		PlanAllowed: true,
	},
	{
		Definition: toolDef("glob_files", "Find files by glob pattern (e.g. *.go, **/*.md). Matches dotfiles too (e.g. *.env); hidden directories are pruned — pass one as the path argument to search inside it.",
			toolSchema(map[string]interface{}{
				"pattern":      toolProp("string", "Glob pattern"),
				"path":         toolProp("string", "Optional subdirectory"),
				"tracked_only": toolProp("boolean", "Only git-tracked files"),
			}, "pattern")),
		Handler:     handleGlobFiles,
		ReadOnly:    true,
		PlanAllowed: true,
	},
	{
		Definition: toolDef("read_file", "Read file content. Use offset/limit for ranges. Search=regex jump.",
			toolSchema(map[string]interface{}{
				"path":         toolProp("string", "File path"),
				"file_path":    toolProp("string", "Deprecated alias for path (prefer path)"),
				"offset":       toolProp("integer", "Start line (no search) or context lines (with search, default 10)"),
				"limit":        toolProp("integer", "Max lines (no search, default all/max 10000) or window size (with search)"),
				"search":       toolProp("string", "Regex to jump to; offset/limit become context/window"),
				"line_numbers": toolProp("boolean", "Prefix lines with numbers"),
			})),
		Handler:     handleReadFile,
		ReadOnly:    true,
		PlanAllowed: true,
	},
	{
		Definition: toolDef("read_files", "Read multiple files (max 20, 512KB). Output: === path === headers.",
			toolSchema(map[string]interface{}{
				"paths": toolPropArray("string", "File paths"),
			}, "paths")),
		Handler:     handleReadFiles,
		ReadOnly:    true,
		PlanAllowed: true,
	},
	{
		Definition: toolDef("list_definitions", "List functions/types in a file with line numbers (requires tree-sitter; set GOGEN_TREESITTER=on). Use before editing.",
			toolSchema(map[string]interface{}{
				"path": toolProp("string", "Source file path"),
			}, "path")),
		Handler:     handleListDefinitions,
		ReadOnly:    true,
		PlanAllowed: true,
	},
	{
		Definition: toolDef("write_file", "Create a NEW file only (parent dirs ok). Refuses existing paths — use patch_file/replace_in_file to edit; never delete+recreate.",
			toolSchema(map[string]interface{}{
				"path":    toolProp("string", "File path"),
				"content": toolProp("string", "Content"),
			}, "path", "content")),
		Handler:   handleWriteFile,
		MutatesFS: true,
	},
	{
		Definition: toolDef("execute_command", "Run a shell command (destructive patterns blocked).",
			toolSchema(map[string]interface{}{
				"command":    toolProp("string", "Command"),
				"background": toolProp("boolean", "Run in the background and return immediately with a job id; poll with background_job_status and cancel with background_job_cancel (default false)"),
			}, "command")),
		Handler: handleExecuteCommand,
	},
	{
		Definition: toolDef("run_tests", "Run project tests (auto-detects test command from project markers).",
			toolSchema(map[string]interface{}{
				"target":     toolProp("string", "Scope to package/path/pattern"),
				"extra_args": toolProp("string", "Extra args appended"),
			})),
		Handler: handleRunTests,
	},
	{
		Definition: toolDef("run_lint", "Run project linter (auto-detects from project markers).",
			toolSchema(map[string]interface{}{
				"extra_args": toolProp("string", "Extra args appended"),
			})),
		Handler: handleRunLint,
	},
	{
		Definition: toolDef("replace_in_file", "Replace string in file. replace_all=true for all occurrences. Prefer patch_file for multi-line.",
			toolSchema(map[string]interface{}{
				"path":        toolProp("string", "File path"),
				"search":      toolProp("string", "Exact string to find"),
				"replace":     toolProp("string", "Replacement"),
				"replace_all": toolProp("boolean", "Replace all occurrences (default: first)"),
			}, "path", "search", "replace")),
		Handler:   handleReplaceInFile,
		MutatesFS: true,
	},
	{
		Definition: toolDef("delete_file", "Delete a file (requires approval). Do not use to replace content — edit in place with patch_file/replace_in_file.",
			toolSchema(map[string]interface{}{
				"path": toolProp("string", "File path"),
			}, "path")),
		Handler:   handleDeleteFile,
		MutatesFS: true,
	},
	{
		Definition: toolDef("move_file", "Rename/move a file (creates parent dirs).",
			toolSchema(map[string]interface{}{
				"source":      toolProp("string", "Source path"),
				"destination": toolProp("string", "Destination path"),
			}, "source", "destination")),
		Handler:   handleMoveFile,
		MutatesFS: true,
	},
	{
		Definition: toolDef("patch_file", "Apply surgical unified diff(s). Prefer over rewrites. dry_run=preview, fuzzy=default (tolerant). Do not remove+re-add whole files.",
			toolSchema(map[string]interface{}{
				"diff": toolProp("string", "Unified diff: '--- a/path'/'+++ b/path' headers (a/b optional), '@@ -oldStart,oldCount +newStart,newCount @@' hunks.\n"+
					"Context=space, removed=minus, added=plus. Multi-file: stack sections (diff --git OK).\n\n"+
					"Example:\n--- a/main.go\n+++ b/main.go\n@@ -1,4 +1,5 @@\n package main\n \n+// comment\n func main() {\n }\n"),
				"dry_run": toolProp("boolean", "Preview without writing"),
				"fuzzy":   toolProp("boolean", "Tolerate whitespace/shift drift (default true; leave on unless exact)"),
			}, "diff")),
		Handler:   handlePatchFile,
		MutatesFS: true,
	},
	{
		Definition: toolDef("show_diff", "Show git diff (working tree or path).",
			toolSchema(map[string]interface{}{
				"path":   toolProp("string", "Optional file/dir path"),
				"staged": toolProp("boolean", "Show staged changes only"),
			})),
		Handler:     handleShowDiff,
		ReadOnly:    true,
		PlanAllowed: true,
	},
	{
		Definition: toolDef("search_code", "Search codebase for regex/string. Returns file:line:content. Pair with list_definitions.",
			toolSchema(map[string]interface{}{
				"pattern":       toolProp("string", "Regex or literal"),
				"path":          toolProp("string", "Subdirectory (required for hidden dirs)"),
				"glob":          toolProp("string", "Glob filter (e.g. *.go)"),
				"context_lines": toolProp("integer", "Context lines (max 20)"),
			}, "pattern")),
		Handler:     handleSearchCode,
		ReadOnly:    true,
		PlanAllowed: true,
	},
	{
		Definition: toolDef("find_references", "Find symbol references (AST when supported, text fallback).",
			toolSchema(map[string]interface{}{
				"symbol": toolProp("string", "Symbol name"),
				"path":   toolProp("string", "Optional subdirectory"),
				"glob":   toolProp("string", "Optional glob filter"),
			}, "symbol")),
		Handler:     handleFindReferences,
		ReadOnly:    true,
		PlanAllowed: true,
	},
	{
		Definition: toolDef("git_log", "Recent git commits (read-only).",
			toolSchema(map[string]interface{}{
				"path":  toolProp("string", "Optional path to scope"),
				"limit": toolProp("integer", "Max commits (default 20, max 100)"),
			})),
		Handler:     handleGitLog,
		ReadOnly:    true,
		PlanAllowed: true,
	},
	{
		Definition: toolDef("git_blame", "Git blame for a file range (read-only).",
			toolSchema(map[string]interface{}{
				"path":       toolProp("string", "File path"),
				"start_line": toolProp("integer", "Start line (default 1)"),
				"limit":      toolProp("integer", "Lines to blame (default 50, max 200)"),
			}, "path")),
		Handler:     handleGitBlame,
		ReadOnly:    true,
		PlanAllowed: true,
	},
	{
		Definition: toolDef("git_status", "Git status (short, read-only).",
			toolSchema(map[string]interface{}{
				"path": toolProp("string", "Optional path to scope"),
			})),
		Handler:     handleGitStatus,
		ReadOnly:    true,
		PlanAllowed: true,
	},
	{
		Definition: toolDef("web_search", "Web search (DuckDuckGo Lite; no API key needed).",
			toolSchema(map[string]interface{}{
				"query":       toolProp("string", "Query"),
				"max_results": toolProp("integer", "Max results (default 10, max 20)"),
			}, "query")),
		Handler:     handleWebSearch,
		ReadOnly:    true,
		PlanAllowed: true,
	},
	{
		Definition: toolDef("web_fetch", "Fetch a web page: main article as Markdown, or the full page when no article exists; source/data files returned raw. selector: extract matching CSS elements. query: search within the result.",
			toolSchema(map[string]interface{}{
				"url":       toolProp("string", "URL (https)"),
				"max_bytes": toolProp("integer", "Max bytes (default 262144)"),
				"selector":  toolProp("string", "Optional CSS selector: extract only matching elements (e.g. article, .markdown-body, table, pre). Raises max_bytes on big pages"),
				"query":     toolProp("string", "Optional case-insensitive text search over the extracted content; returns matches with context lines"),
				"context":   toolProp("integer", "Context lines per query match (default 3, max 10)"),
			}, "url")),
		Handler:     handleWebFetch,
		ReadOnly:    true,
		PlanAllowed: true,
	},
	{
		Definition: toolDef("download_file", "Download a file from a URL into the workspace (raw bytes, no HTML stripping; binary-safe). Use for large source files or binaries that would exceed web_fetch's text caps — then read/search the saved file with read_file offset/limit, search_code, list_definitions, or patch_file. HTTPS-only; private/internal hosts blocked.",
			toolSchema(map[string]interface{}{
				"url":       toolProp("string", "URL (https)"),
				"path":      toolProp("string", "Destination path under the working directory (must not exist unless overwrite=true)"),
				"max_bytes": toolProp("integer", "Max download size in bytes (default 52428800, max 209715200)"),
				"overwrite": toolProp("boolean", "Overwrite an existing file (default false)"),
			}, "url", "path")),
		Handler:   handleDownloadFile,
		MutatesFS: true,
	},
	{
		Definition: toolDef("git_commit", "Commit (requires staged files).",
			toolSchema(map[string]interface{}{
				"message": toolProp("string", "Message"),
			}, "message")),
		Handler: handleGitCommit,
	},
	{
		Definition: toolDef("git_stage", "Stage files (empty = stage all).",
			toolSchema(map[string]interface{}{
				"paths": toolPropArray("string", "File paths (empty = all)"),
			})),
		Handler: handleGitStage,
	},
	{
		// git_branch: omitted from plan mode on purpose — create/switch
		// mutate the repo; listing happens via execute_command outside plan
		// mode. No PlanAllowed flag for that reason.
		Definition: toolDef("git_branch", "List/create/switch branches.",
			toolSchema(map[string]interface{}{
				"name":   toolProp("string", "Name (omit to list)"),
				"create": toolProp("boolean", "Create (true) or switch (false)"),
			})),
		Handler: handleGitBranch,
	},
	{
		Definition: toolDef("git_stash", "Stash changes (pop=true to pop).",
			toolSchema(map[string]interface{}{
				"message": toolProp("string", "Optional message"),
				"pop":     toolProp("boolean", "Pop instead of push"),
			})),
		Handler: handleGitStash,
	},
	{
		Definition: toolDef("git_stash_list", "List stash entries.",
			toolSchema(map[string]interface{}{})),
		Handler:     handleGitStashList,
		ReadOnly:    true,
		PlanAllowed: true,
	},
	{
		Definition: toolDef("git_show", "Show commit/range as diff (empty=HEAD).",
			toolSchema(map[string]interface{}{
				"ref": toolProp("string", "Ref (hash, tag, range; empty=HEAD)"),
			})),
		Handler:     handleGitShow,
		ReadOnly:    true,
		PlanAllowed: true,
	},
	{
		Definition: toolDef("copy_file", "Copy file (creates parent dirs).",
			toolSchema(map[string]interface{}{
				"source":      toolProp("string", "Source"),
				"destination": toolProp("string", "Destination"),
			}, "source", "destination")),
		Handler:   handleCopyFile,
		MutatesFS: true,
	},
	{
		Definition: toolDef("todo_add", "Add todo item.",
			toolSchema(map[string]interface{}{
				"text": toolProp("string", "Text"),
			}, "text")),
		Handler:     handleTodoAdd,
		PlanAllowed: true,
	},
	{
		Definition: toolDef("todo_list", "List todos with status.",
			toolSchema(map[string]interface{}{})),
		Handler:     handleTodoList,
		ReadOnly:    true,
		PlanAllowed: true,
	},
	{
		Definition: toolDef("todo_done", "Mark todo done by ID.",
			toolSchema(map[string]interface{}{
				"id": toolProp("integer", "ID"),
			}, "id")),
		Handler: handleTodoDone,
	},
	{
		Definition: toolDef("todo_remove", "Remove todo by ID.",
			toolSchema(map[string]interface{}{
				"id": toolProp("integer", "ID"),
			}, "id")),
		Handler: handleTodoRemove,
	},
	{
		Definition: toolDef("todo_clear_done", "Clear completed todos.",
			toolSchema(map[string]interface{}{})),
		Handler: handleTodoClearDone,
	},
	{
		Definition: toolDef("find_file", "Find files by name (case-insensitive substring). Faster than glob for known filenames.",
			toolSchema(map[string]interface{}{
				"name":  toolProp("string", "Filename substring"),
				"path":  toolProp("string", "Optional subdirectory"),
				"limit": toolProp("integer", "Max results (default 50)"),
			}, "name")),
		Handler:     handleFindFile,
		ReadOnly:    true,
		PlanAllowed: true,
	},
	{
		Definition: toolDef("find_definition", "Cross-file go-to-definition (tree-sitter or text fallback).",
			toolSchema(map[string]interface{}{
				"symbol": toolProp("string", "Symbol name"),
				"path":   toolProp("string", "Optional subdirectory"),
				"glob":   toolProp("string", "Optional glob filter"),
			}, "symbol")),
		Handler:     handleFindDefinition,
		ReadOnly:    true,
		PlanAllowed: true,
	},
	{
		Definition: toolDef("session_rename", "Rename current session.",
			toolSchema(map[string]interface{}{
				"label": toolProp("string", "Label"),
			}, "label")),
		Handler:     handleSessionRename,
		PlanAllowed: true,
	},
	{
		Definition: toolDef("session_usage", "Show session token usage.",
			toolSchema(map[string]interface{}{})),
		Handler:     handleSessionUsage,
		ReadOnly:    true,
		PlanAllowed: true,
	},
	{
		Definition: toolDef("context_pin_last", "Pin last user message to survive compaction.",
			toolSchema(map[string]interface{}{})),
		Handler:     handleContextPinLast,
		PlanAllowed: true,
	},
	{
		Definition: toolDef("context_pins", "List pinned messages.",
			toolSchema(map[string]interface{}{})),
		Handler:     handleContextPins,
		ReadOnly:    true,
		PlanAllowed: true,
	},
	{
		Definition: toolDef("rename_symbol", "Rename symbol across files (AST or text fallback).",
			toolSchema(map[string]interface{}{
				"old_name": toolProp("string", "Current name"),
				"new_name": toolProp("string", "New name"),
				"path":     toolProp("string", "Optional subdirectory"),
				"glob":     toolProp("string", "Optional glob filter"),
				"dry_run":  toolProp("boolean", "Preview only"),
			}, "old_name", "new_name")),
		Handler:   handleRenameSymbol,
		MutatesFS: true,
	},
	{
		Definition: toolDef("multi_edit", "Same literal text replacement across multiple files (not regex).",
			toolSchema(map[string]interface{}{
				"pattern": toolProp("string", "Glob pattern (e.g. *.go)"),
				"search":  toolProp("string", "Literal string to find"),
				"replace": toolProp("string", "Replacement text"),
				"dry_run": toolProp("boolean", "Preview only"),
			}, "pattern", "search", "replace")),
		Handler:   handleMultiEdit,
		MutatesFS: true,
	},
	{
		Definition: toolDef("call_graph", "Call relationships for a symbol.",
			toolSchema(map[string]interface{}{
				"symbol":    toolProp("string", "Symbol"),
				"path":      toolProp("string", "Optional subdirectory"),
				"glob":      toolProp("string", "Optional glob filter"),
				"direction": toolProp("string", "callers, callees, or both"),
			}, "symbol")),
		Handler:     handleCallGraph,
		ReadOnly:    true,
		PlanAllowed: true,
	},
	{
		Definition: toolDef("dependency_analysis", "Impact analysis for a symbol (dependents, risk).",
			toolSchema(map[string]interface{}{
				"symbol": toolProp("string", "Symbol"),
				"path":   toolProp("string", "Optional subdirectory"),
			}, "symbol")),
		Handler:     handleDependencyAnalysis,
		ReadOnly:    true,
		PlanAllowed: true,
	},
	{
		Definition: toolDef("background_job_status", "Check a background job started with execute_command background=true. Reports running/finished state, exit code, and the tail of the job's output.",
			toolSchema(map[string]interface{}{
				"job_id": toolProp("string", "Job id returned by execute_command background=true"),
			}, "job_id")),
		Handler: handleBackgroundJobStatus,
	},
	{
		Definition: toolDef("background_job_cancel", "Cancel a running background job (kills its process group). Jobs also die when their session closes.",
			toolSchema(map[string]interface{}{
				"job_id": toolProp("string", "Job id returned by execute_command background=true"),
			}, "job_id")),
		Handler: handleBackgroundJobCancel,
	},
}

// BuiltinTools returns built-in tool definitions for the LLM, in registration
// order.
func BuiltinTools() []llm.Tool {
	tools := make([]llm.Tool, 0, len(builtinToolDefs))
	for _, d := range builtinToolDefs {
		tools = append(tools, d.Definition)
	}
	return tools
}

// parallelSafeTools is derived from the ReadOnly flag on builtinToolDefs, so
// a tool is parallel-safe by declaration in its ToolDef instead of by
// remembering to edit a second map. MCP tools are never parallel-safe (their
// side effects are unknown), and tool names shadowed by an MCP server are
// excluded at call time (toolCallsParallelEligible).
var parallelSafeTools = deriveParallelSafeTools()

func deriveParallelSafeTools() map[string]bool {
	out := make(map[string]bool, len(builtinToolDefs))
	for _, d := range builtinToolDefs {
		if d.ReadOnly {
			out[d.Definition.Name] = true
		}
	}
	return out
}

// FSMutatingToolNames returns the names of builtin tools flagged MutatesFS —
// the handlers the server wraps with the workspace fsMu. Exported so the
// server derives its lock set from the registry instead of a second map.
func FSMutatingToolNames() []string {
	names := make([]string, 0, 8)
	for _, d := range builtinToolDefs {
		if d.MutatesFS {
			names = append(names, d.Definition.Name)
		}
	}
	return names
}

// maxParallelTools bounds how many tool calls run concurrently within one
// turn (bounded by the tool batch size).
const maxParallelTools = 4
