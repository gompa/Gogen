package agent

import "gogen/internal/llm"

// BuiltinTools returns built-in tool definitions for the LLM.
func BuiltinTools() []llm.Tool {
	return []llm.Tool{
		{
			Name:        "list_files",
			Description: "List files and directories. Set recursive=true to walk the tree (skips .git, node_modules, etc.). Directories are suffixed with /.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path":         map[string]interface{}{"type": "string", "description": "Path to directory"},
					"recursive":    map[string]interface{}{"type": "boolean", "description": "If true, list all entries recursively (max 500)"},
					"tracked_only": map[string]interface{}{"type": "boolean", "description": "If true, only show git-tracked files"},
				},
				"required": []string{"path"},
			},
		},
		{
			Name:        "repo_overview",
			Description: "Summarize repository layout: top-level directories with file counts and root files. Use first when exploring an unfamiliar codebase.",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
		{
			Name:        "glob_files",
			Description: "Find files by glob pattern under the working directory, e.g. *.go, internal/*.go, **/*.md",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"pattern":      map[string]interface{}{"type": "string", "description": "Glob pattern"},
					"path":         map[string]interface{}{"type": "string", "description": "Optional subdirectory to search under"},
					"tracked_only": map[string]interface{}{"type": "boolean", "description": "If true, only show git-tracked files"},
				},
				"required": []string{"pattern"},
			},
		},
		{
			Name:        "read_file",
			Description: "Read content of a single file. Use offset/limit for large files (1-based line numbers). Use 'search' to jump to the first line matching a pattern.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"file_path": map[string]interface{}{"type": "string", "description": "Path to file"},
					"offset": map[string]interface{}{"type": "integer", "description": "Optional 1-based starting line (default: 1)"},
					"limit":  map[string]interface{}{"type": "integer", "description": "Optional max lines to read (default: all, capped at 10000)"},
					"search": map[string]interface{}{"type": "string", "description": "Optional regex pattern; jumps to the first matching line and reads N lines around it"},
				},
				"required": []string{"file_path"},
			},
		},
		{
			Name:        "read_files",
			Description: "Read multiple files in one call. Output uses === path === headers (max 20 files, 512KB total).",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"paths": map[string]interface{}{
						"type":        "array",
						"items":       map[string]interface{}{"type": "string"},
						"description": "File paths to read",
					},
				},
				"required": []string{"paths"},
			},
		},
		{
			Name:        "list_definitions",
			Description: "List functions, methods, classes, and types in a source file with line numbers. Use before editing unfamiliar files; pair with search_code to locate files first.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path": map[string]interface{}{"type": "string", "description": "Path to source file"},
				},
				"required": []string{"path"},
			},
		},
		{
			Name:        "write_file",
			Description: "Write content to a file",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path":    map[string]interface{}{"type": "string", "description": "Path to file"},
					"content": map[string]interface{}{"type": "string", "description": "Content to write"},
				},
				"required": []string{"path", "content"},
			},
		},
		{
			Name:        "execute_command",
			Description: "Execute a shell command in the working directory. Blocked patterns include sudo, rm -rf /, and curl|bash. Configure GOGEN_COMMAND_SAFETY=allowlist and GOGEN_COMMAND_ALLOWLIST for stricter control.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"command": map[string]interface{}{"type": "string", "description": "Shell command to run"},
				},
				"required": []string{"command"},
			},
		},
		{
			Name:        "run_tests",
			Description: "Run the project test suite. Uses test_command from GOGEN.md when set, otherwise detects from ecosystem markers (go.mod, package.json, Cargo.toml, etc.).",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"target":     map[string]interface{}{"type": "string", "description": "Optional package, path, or test pattern to scope the run"},
					"extra_args": map[string]interface{}{"type": "string", "description": "Optional extra arguments appended to the test command"},
				},
			},
		},
		{
			Name:        "run_lint",
			Description: "Run the project linter. Uses lint_command from GOGEN.md when set, otherwise detects from ecosystem markers (go vet, ruff, clippy, etc.).",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"extra_args": map[string]interface{}{"type": "string", "description": "Optional extra arguments appended to the lint command"},
				},
			},
		},
		{
			Name:        "replace_in_file",
			Description: "Replace a search string in a file. Default replaces the first occurrence; set replace_all=true for every occurrence. Prefer patch_file for multi-line or precise edits.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path":        map[string]interface{}{"type": "string", "description": "Path to file"},
					"search":      map[string]interface{}{"type": "string", "description": "Exact search string"},
					"replace":     map[string]interface{}{"type": "string", "description": "Replacement string"},
					"replace_all": map[string]interface{}{"type": "boolean", "description": "If true, replace all occurrences (default: first only)"},
				},
				"required": []string{"path", "search", "replace"},
			},
		},
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
		{
			Name:        "move_file",
			Description: "Rename or move a file within the working directory. Creates destination directories as needed. Does not move directories.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"source":      map[string]interface{}{"type": "string", "description": "Path to the file to move"},
					"destination": map[string]interface{}{"type": "string", "description": "Destination path for the file"},
				},
				"required": []string{"source", "destination"},
			},
		},
		{
			Name:        "patch_file",
			Description: "Apply a unified diff to one or more files in a single call. Include multiple ---/+++/@@ sections for coordinated multi-file edits. Set dry_run=true to validate all files before writing; fuzzy (default true) tolerates context drift and whitespace differences; set fuzzy=false to require exact context.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"diff":    map[string]interface{}{"type": "string", "description": "Unified diff text (---/+++/@@ hunks; multiple files allowed)"},
					"dry_run": map[string]interface{}{"type": "boolean", "description": "If true, validate the patch and report changes without applying"},
					"fuzzy":   map[string]interface{}{"type": "boolean", "description": "If false, require exact context match (default: true)"},
				},
				"required": []string{"diff"},
			},
		},
		{
			Name:        "show_diff",
			Description: "Show git diff for the working tree or a specific path. Requires a git repository.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path":   map[string]interface{}{"type": "string", "description": "Optional file or directory path"},
					"staged": map[string]interface{}{"type": "boolean", "description": "If true, show staged (cached) changes only"},
				},
			},
		},
		{
			Name:        "search_code",
			Description: "Search for a regex or string in the codebase. Returns file:line:content matches. Use context_lines for surrounding lines (max 20). Pair with list_definitions to outline before reading.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"pattern":       map[string]interface{}{"type": "string", "description": "Regex or literal string to search for"},
					"path":          map[string]interface{}{"type": "string", "description": "Optional subdirectory under the working directory to search (required for hidden dirs like .github)"},
					"glob":          map[string]interface{}{"type": "string", "description": "Optional glob filter on match paths, e.g. *.go or internal/*.go"},
					"context_lines": map[string]interface{}{"type": "integer", "description": "Optional lines of context before/after each match (max 20)"},
				},
				"required": []string{"pattern"},
			},
		},
		{
			Name:        "find_references",
			Description: "Find references to a symbol. Uses tree-sitter AST search when the file language is supported; falls back to word-boundary text search for all files.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"symbol": map[string]interface{}{"type": "string", "description": "Symbol name to find references for"},
					"path":   map[string]interface{}{"type": "string", "description": "Optional subdirectory to search under"},
					"glob":   map[string]interface{}{"type": "string", "description": "Optional glob filter, e.g. *.go"},
				},
				"required": []string{"symbol"},
			},
		},
		{
			Name:        "git_log",
			Description: "Show recent git commit history. Read-only; available in plan mode.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path":  map[string]interface{}{"type": "string", "description": "Optional file or directory path to scope history"},
					"limit": map[string]interface{}{"type": "integer", "description": "Max commits to show (default 20, max 100)"},
				},
			},
		},
		{
			Name:        "git_blame",
			Description: "Show git blame for a file within a line range. Read-only; available in plan mode.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path":       map[string]interface{}{"type": "string", "description": "Path to file"},
					"start_line": map[string]interface{}{"type": "integer", "description": "Optional 1-based starting line (default 1)"},
					"limit":      map[string]interface{}{"type": "integer", "description": "Optional number of lines to blame (default 50, max 200)"},
				},
				"required": []string{"path"},
			},
		},
		{
			Name:        "git_status",
			Description: "Show git working tree status (short format). Read-only; available in plan mode.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path": map[string]interface{}{"type": "string", "description": "Optional file or directory path to scope status"},
				},
			},
		},
		{
			Name:        "web_search",
			Description: "Search the web for current information. Uses DuckDuckGo Lite by default (no API key needed). Set GOGEN_WEB_SEARCH_BACKEND=brave and GOGEN_WEB_SEARCH_API_KEY for better results.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"query":       map[string]interface{}{"type": "string", "description": "Search query"},
					"max_results": map[string]interface{}{"type": "integer", "description": "Optional max results (default 10, max 20)"},
				},
				"required": []string{"query"},
			},
		},
		{
			Name:        "web_fetch",
			Description: "Fetch and extract text content from a URL. Use for reading documentation, API references, or any web page. HTML is stripped to plain text. Set GOGEN_WEB_FETCH=off to disable.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"url":       map[string]interface{}{"type": "string", "description": "URL to fetch (https required by default)"},
					"max_bytes": map[string]interface{}{"type": "integer", "description": "Optional max bytes to download (default 65536)"},
				},
				"required": []string{"url"},
			},
		},
		{
			Name:        "git_commit",
			Description: "Create a git commit with the given message. Requires files to be staged first (use git_stage).",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"message": map[string]interface{}{"type": "string", "description": "Commit message"},
				},
				"required": []string{"message"},
			},
		},
		{
			Name:        "git_stage",
			Description: "Stage files for commit. When paths is empty or omitted, stages all changes (git add -A).",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"paths": map[string]interface{}{
						"type":        "array",
						"items":       map[string]interface{}{"type": "string"},
						"description": "Optional file paths to stage (empty = stage all)",
					},
				},
			},
		},
		{
			Name:        "git_branch",
			Description: "List, create, or switch git branches. When name is provided and create=true, creates a new branch. When name is provided and create=false, switches to it. When name is empty, lists all branches.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name":   map[string]interface{}{"type": "string", "description": "Branch name (omit to list)"},
					"create": map[string]interface{}{"type": "boolean", "description": "If true, create a new branch; if false and name given, switch to it"},
				},
			},
		},
		{
			Name:        "git_stash",
			Description: "Push changes to the stash (or pop when pop=true).",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"message": map[string]interface{}{"type": "string", "description": "Optional stash message"},
					"pop":     map[string]interface{}{"type": "boolean", "description": "If true, pop the most recent stash instead of pushing"},
				},
			},
		},
		{
			Name:        "git_stash_list",
			Description: "List all stash entries.",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
		{
			Name:        "git_show",
			Description: "Show a specific commit or range as a unified diff. When ref is empty, shows HEAD.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"ref": map[string]interface{}{"type": "string", "description": "Git ref (commit hash, tag, or range; empty = HEAD)"},
				},
			},
		},
		{
			Name:        "copy_file",
			Description: "Copy a file within the working directory. Creates destination directories as needed.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"source":      map[string]interface{}{"type": "string", "description": "Source file path"},
					"destination": map[string]interface{}{"type": "string", "description": "Destination file path"},
				},
				"required": []string{"source", "destination"},
			},
		},
		{
			Name:        "todo_add",
			Description: "Add a new todo item to the task tracker.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"text": map[string]interface{}{"type": "string", "description": "Todo text"},
				},
				"required": []string{"text"},
			},
		},
		{
			Name:        "todo_list",
			Description: "List all todo items with their status.",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
		{
			Name:        "todo_done",
			Description: "Mark a todo item as completed by its ID.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"id": map[string]interface{}{"type": "integer", "description": "Todo item ID"},
				},
				"required": []string{"id"},
			},
		},
		{
			Name:        "todo_remove",
			Description: "Remove a todo item entirely by its ID.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"id": map[string]interface{}{"type": "integer", "description": "Todo item ID"},
				},
				"required": []string{"id"},
			},
		},
		{
			Name:        "todo_clear_done",
			Description: "Clear all completed todo items.",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
		{
			Name:        "find_file",
			Description: "Find files by name (case-insensitive substring match). Faster than glob_files when you know part of the filename but not the path.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name":  map[string]interface{}{"type": "string", "description": "Filename or substring to match"},
					"path":  map[string]interface{}{"type": "string", "description": "Optional subdirectory to search under"},
					"limit": map[string]interface{}{"type": "integer", "description": "Max results (default 50)"},
				},
				"required": []string{"name"},
			},
		},
		{
			Name:        "find_definition",
			Description: "Find which file defines a symbol (cross-file go-to-definition). Scans supported languages via tree-sitter when available; falls back to text search.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"symbol": map[string]interface{}{"type": "string", "description": "Symbol name to find (function, type, variable, etc.)"},
					"path":   map[string]interface{}{"type": "string", "description": "Optional subdirectory to search under"},
					"glob":   map[string]interface{}{"type": "string", "description": "Optional glob filter, e.g. *.go"},
				},
				"required": []string{"symbol"},
			},
		},
		{
			Name:        "session_rename",
			Description: "Rename or label the current session for easier identification when listing sessions.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"label": map[string]interface{}{"type": "string", "description": "New session label"},
				},
				"required": []string{"label"},
			},
		},
		{
			Name:        "session_usage",
			Description: "Show accumulated token usage for the session (turns, tokens in/out).",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
		{
			Name:        "context_pin_last",
			Description: "Pin the most recent user message so it survives context compaction. Useful to preserve critical context like requirements or error messages.",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
		{
			Name:        "context_pins",
			Description: "List all pinned messages that will survive context compaction.",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
	}
}
