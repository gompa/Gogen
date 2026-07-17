package agent

import (
	"context"
	"fmt"

	"gogen/internal/llm"
)

// ToolHandler executes a builtin tool given parsed arguments.
type ToolHandler func(ctx context.Context, a *Agent, args map[string]interface{}) (string, error)

// BuiltinToolHandlers returns the registry of builtin tool implementations.
func BuiltinToolHandlers() map[string]ToolHandler {
	return map[string]ToolHandler{
		"list_files":       handleListFiles,
		"glob_files":       handleGlobFiles,
		"repo_overview":    handleRepoOverview,
		"read_file":        handleReadFile,
		"read_files":       handleReadFiles,
		"write_file":       handleWriteFile,
		"execute_command":  handleExecuteCommand,
		"replace_in_file":  handleReplaceInFile,
		"patch_file":       handlePatchFile,
		"run_tests":        handleRunTests,
		"run_lint":         handleRunLint,
		"delete_file":      handleDeleteFile,
		"move_file":        handleMoveFile,
		"show_diff":        handleShowDiff,
		"search_code":      handleSearchCode,
		"find_references":  handleFindReferences,
		"git_log":          handleGitLog,
		"git_blame":        handleGitBlame,
		"git_status":       handleGitStatus,
		"git_commit":       handleGitCommit,
		"git_stage":        handleGitStage,
		"git_branch":       handleGitBranch,
		"git_stash":        handleGitStash,
		"git_stash_list":   handleGitStashList,
		"git_show":         handleGitShow,
		"copy_file":        handleCopyFile,
		"todo_add":         handleTodoAdd,
		"todo_list":        handleTodoList,
		"todo_done":        handleTodoDone,
		"todo_remove":      handleTodoRemove,
		"todo_clear_done":  handleTodoClearDone,
		"list_definitions": handleListDefinitions,
		"web_search":       handleWebSearch,
		"web_fetch":        handleWebFetch,
		"find_file":        handleFindFile,
		"find_definition":  handleFindDefinition,
		"session_rename":   handleSessionRename,
		"session_usage":    handleSessionUsage,
		"context_pin_last": handleContextPinLast,
		"context_pins":     handleContextPins,
	}
}

func (a *Agent) executeTool(ctx context.Context, tc llm.ToolCall) (string, error) {
	if tc.ArgsError != "" {
		return "", fmt.Errorf("invalid tool arguments: %s", tc.ArgsError)
	}
	if err := a.checkPlanMode(tc.Name); err != nil {
		return "", err
	}
	if a.MCPRegistry != nil {
		if names := a.MCPRegistry.ToolNames(); names != nil {
			if _, ok := names[tc.Name]; ok {
				return a.MCPRegistry.CallTool(ctx, tc.Name, tc.Args)
			}
		}
	}
	ctx = a.toolContext(ctx)
	handlers := a.toolHandlers
	if handlers == nil {
		handlers = BuiltinToolHandlers()
	}
	h, ok := handlers[tc.Name]
	if !ok {
		return "", fmt.Errorf("unknown tool: %s", tc.Name)
	}
	return h(ctx, a, tc.Args)
}

func handleListFiles(_ context.Context, a *Agent, args map[string]interface{}) (string, error) {
	path, err := stringArg(args, "path")
	if err != nil {
		return "", err
	}
	recursive, _ := boolArgOptional(args, "recursive")
	tracked, _ := boolArgOptional(args, "tracked_only")
	return a.Executor.ListFiles(path, recursive, tracked)
}

func handleGlobFiles(_ context.Context, a *Agent, args map[string]interface{}) (string, error) {
	pattern, err := stringArg(args, "pattern")
	if err != nil {
		return "", err
	}
	subpath, _ := stringArgOptional(args, "path")
	tracked, _ := boolArgOptional(args, "tracked_only")
	return a.Executor.GlobFiles(pattern, subpath, tracked)
}

func handleRepoOverview(_ context.Context, a *Agent, _ map[string]interface{}) (string, error) {
	return a.Executor.RepoOverview()
}

func handleReadFile(_ context.Context, a *Agent, args map[string]interface{}) (string, error) {
	path, err := stringArgOptional(args, "file_path")
	if err != nil {
		return "", err
	}
	if path == "" {
		path, err = stringArg(args, "path")
		if err != nil {
			return "", err
		}
	}
	offset, err := intArgOptional(args, "offset")
	if err != nil {
		return "", err
	}
	limit, err := intArgOptional(args, "limit")
	if err != nil {
		return "", err
	}
	search, _ := stringArgOptional(args, "search")
	return a.Executor.ReadFileRange(path, offset, limit, search)
}

func handleReadFiles(_ context.Context, a *Agent, args map[string]interface{}) (string, error) {
	paths, err := stringSliceArg(args, "paths")
	if err != nil {
		return "", err
	}
	return a.Executor.ReadFiles(paths)
}

func handleWriteFile(_ context.Context, a *Agent, args map[string]interface{}) (string, error) {
	path, err := stringArg(args, "path")
	if err != nil {
		return "", err
	}
	content, err := stringArg(args, "content")
	if err != nil {
		return "", err
	}
	if err := a.Executor.WriteFile(path, content); err != nil {
		return "", err
	}
	result := a.Executor.AppendSyntaxCheck("File written successfully", path)
	if diffOut, diffErr := runDiffQuick(a.WorkingDir, path); diffErr == nil && diffOut != "" {
		result += "\n\n" + diffOut
	}
	return result, nil
}

func handleExecuteCommand(ctx context.Context, a *Agent, args map[string]interface{}) (string, error) {
	command, err := stringArg(args, "command")
	if err != nil {
		return "", err
	}
	return a.Executor.ExecuteCommand(ctx, command)
}

func handleReplaceInFile(_ context.Context, a *Agent, args map[string]interface{}) (string, error) {
	path, err := stringArg(args, "path")
	if err != nil {
		return "", err
	}
	search, err := stringArg(args, "search")
	if err != nil {
		return "", err
	}
	replace, err := stringArg(args, "replace")
	if err != nil {
		return "", err
	}
	replaceAll, err := boolArgOptional(args, "replace_all")
	if err != nil {
		return "", err
	}
	return a.Executor.ReplaceInFile(path, search, replace, replaceAll)
}

func handlePatchFile(ctx context.Context, a *Agent, args map[string]interface{}) (string, error) {
	diff, err := stringArg(args, "diff")
	if err != nil {
		return "", err
	}
	dryRun, err := boolArgOptional(args, "dry_run")
	if err != nil {
		return "", err
	}
	fuzzy, err := boolArgDefault(args, "fuzzy", true)
	if err != nil {
		return "", err
	}
	return a.Executor.PatchFile(ctx, diff, dryRun, fuzzy)
}

func handleRunTests(ctx context.Context, a *Agent, args map[string]interface{}) (string, error) {
	target, _ := stringArgOptional(args, "target")
	extra, _ := stringArgOptional(args, "extra_args")
	return a.Executor.RunTests(ctx, target, extra, a.TestCommand)
}

func handleRunLint(ctx context.Context, a *Agent, args map[string]interface{}) (string, error) {
	extra, _ := stringArgOptional(args, "extra_args")
	return a.Executor.RunLint(ctx, extra, a.LintCommand)
}

func handleDeleteFile(ctx context.Context, a *Agent, args map[string]interface{}) (string, error) {
	path, err := stringArg(args, "path")
	if err != nil {
		return "", err
	}
	return a.Executor.DeleteFile(ctx, path)
}

func handleMoveFile(_ context.Context, a *Agent, args map[string]interface{}) (string, error) {
	src, err := stringArg(args, "source")
	if err != nil {
		return "", err
	}
	dst, err := stringArg(args, "destination")
	if err != nil {
		return "", err
	}
	return a.Executor.MoveFile(src, dst)
}

func handleShowDiff(ctx context.Context, a *Agent, args map[string]interface{}) (string, error) {
	subpath, _ := stringArgOptional(args, "path")
	staged, err := boolArgOptional(args, "staged")
	if err != nil {
		return "", err
	}
	return a.Executor.ShowDiff(ctx, subpath, staged)
}

func handleSearchCode(ctx context.Context, a *Agent, args map[string]interface{}) (string, error) {
	pattern, err := stringArg(args, "pattern")
	if err != nil {
		return "", err
	}
	subpath, _ := stringArgOptional(args, "path")
	glob, _ := stringArgOptional(args, "glob")
	contextLines, err := intArgOptional(args, "context_lines")
	if err != nil {
		return "", err
	}
	return a.Executor.SearchCode(ctx, pattern, subpath, glob, contextLines)
}

func handleFindReferences(ctx context.Context, a *Agent, args map[string]interface{}) (string, error) {
	symbol, err := stringArg(args, "symbol")
	if err != nil {
		return "", err
	}
	subpath, _ := stringArgOptional(args, "path")
	glob, _ := stringArgOptional(args, "glob")
	return a.Executor.FindReferences(ctx, symbol, subpath, glob)
}

func handleGitLog(ctx context.Context, a *Agent, args map[string]interface{}) (string, error) {
	subpath, _ := stringArgOptional(args, "path")
	limit, err := intArgOptional(args, "limit")
	if err != nil {
		return "", err
	}
	return a.Executor.GitLog(ctx, subpath, limit)
}

func handleGitBlame(ctx context.Context, a *Agent, args map[string]interface{}) (string, error) {
	path, err := stringArg(args, "path")
	if err != nil {
		return "", err
	}
	startLine, err := intArgOptional(args, "start_line")
	if err != nil {
		return "", err
	}
	limit, err := intArgOptional(args, "limit")
	if err != nil {
		return "", err
	}
	return a.Executor.GitBlame(ctx, path, startLine, limit)
}

func handleGitStatus(ctx context.Context, a *Agent, args map[string]interface{}) (string, error) {
	subpath, _ := stringArgOptional(args, "path")
	return a.Executor.GitStatus(ctx, subpath)
}

func handleGitCommit(ctx context.Context, a *Agent, args map[string]interface{}) (string, error) {
	message, err := stringArg(args, "message")
	if err != nil {
		return "", err
	}
	return a.Executor.GitCommit(ctx, message)
}

func handleGitStage(ctx context.Context, a *Agent, args map[string]interface{}) (string, error) {
	paths, err := stringSliceArgOptional(args, "paths")
	if err != nil {
		return "", err
	}
	return a.Executor.GitStage(ctx, paths)
}

func handleGitBranch(ctx context.Context, a *Agent, args map[string]interface{}) (string, error) {
	name, _ := stringArgOptional(args, "name")
	create, _ := boolArgOptional(args, "create")
	return a.Executor.GitBranch(ctx, name, create)
}

func handleGitStash(ctx context.Context, a *Agent, args map[string]interface{}) (string, error) {
	message, _ := stringArgOptional(args, "message")
	pop, _ := boolArgOptional(args, "pop")
	return a.Executor.GitStash(ctx, message, pop)
}

func handleGitStashList(ctx context.Context, a *Agent, _ map[string]interface{}) (string, error) {
	return a.Executor.GitStashList(ctx)
}

func handleGitShow(ctx context.Context, a *Agent, args map[string]interface{}) (string, error) {
	ref, _ := stringArgOptional(args, "ref")
	return a.Executor.GitDiffShow(ctx, ref)
}

func handleCopyFile(_ context.Context, a *Agent, args map[string]interface{}) (string, error) {
	src, err := stringArg(args, "source")
	if err != nil {
		return "", err
	}
	dst, err := stringArg(args, "destination")
	if err != nil {
		return "", err
	}
	return a.Executor.CopyFile(src, dst)
}

func handleTodoAdd(_ context.Context, a *Agent, args map[string]interface{}) (string, error) {
	text, err := stringArg(args, "text")
	if err != nil {
		return "", err
	}
	if a.TodoManager == nil {
		return "", fmt.Errorf("todo manager is not initialized")
	}
	return a.TodoManager.AddTodo(text)
}

func handleTodoList(_ context.Context, a *Agent, _ map[string]interface{}) (string, error) {
	if a.TodoManager == nil {
		return "", fmt.Errorf("todo manager is not initialized")
	}
	return a.TodoManager.ListTodos(), nil
}

func handleTodoDone(_ context.Context, a *Agent, args map[string]interface{}) (string, error) {
	id, err := intArgOptional(args, "id")
	if err != nil || id == 0 {
		return "", fmt.Errorf("missing required argument %q", "id")
	}
	if a.TodoManager == nil {
		return "", fmt.Errorf("todo manager is not initialized")
	}
	return a.TodoManager.DoneTodo(id)
}

func handleTodoRemove(_ context.Context, a *Agent, args map[string]interface{}) (string, error) {
	id, err := intArgOptional(args, "id")
	if err != nil || id == 0 {
		return "", fmt.Errorf("missing required argument %q", "id")
	}
	if a.TodoManager == nil {
		return "", fmt.Errorf("todo manager is not initialized")
	}
	return a.TodoManager.RemoveTodo(id)
}

func handleTodoClearDone(_ context.Context, a *Agent, _ map[string]interface{}) (string, error) {
	if a.TodoManager == nil {
		return "", fmt.Errorf("todo manager is not initialized")
	}
	return a.TodoManager.ClearDoneTodos()
}

func handleListDefinitions(_ context.Context, a *Agent, args map[string]interface{}) (string, error) {
	path, err := stringArg(args, "path")
	if err != nil {
		return "", err
	}
	return a.Executor.ListDefinitions(path)
}

func handleWebSearch(ctx context.Context, a *Agent, args map[string]interface{}) (string, error) {
	query, err := stringArg(args, "query")
	if err != nil {
		return "", err
	}
	maxResults, err := intArgOptional(args, "max_results")
	if err != nil {
		return "", err
	}
	return a.Executor.WebSearch(ctx, query, maxResults)
}

func handleWebFetch(ctx context.Context, a *Agent, args map[string]interface{}) (string, error) {
	rawURL, err := stringArg(args, "url")
	if err != nil {
		return "", err
	}
	maxBytes, err := intArgOptional(args, "max_bytes")
	if err != nil {
		return "", err
	}
	return a.Executor.WebFetch(ctx, rawURL, maxBytes)
}

func handleFindFile(_ context.Context, a *Agent, args map[string]interface{}) (string, error) {
	name, err := stringArg(args, "name")
	if err != nil {
		return "", err
	}
	subpath, _ := stringArgOptional(args, "path")
	limit, _ := intArgOptional(args, "limit")
	return a.Executor.FindFile(name, subpath, limit)
}

func handleFindDefinition(ctx context.Context, a *Agent, args map[string]interface{}) (string, error) {
	symbol, err := stringArg(args, "symbol")
	if err != nil {
		return "", err
	}
	subpath, _ := stringArgOptional(args, "path")
	glob, _ := stringArgOptional(args, "glob")
	return a.Executor.FindDefinition(ctx, symbol, subpath, glob)
}

func handleSessionRename(_ context.Context, a *Agent, args map[string]interface{}) (string, error) {
	label, err := stringArg(args, "label")
	if err != nil {
		return "", err
	}
	return a.RenameSession(label)
}

func handleSessionUsage(_ context.Context, a *Agent, _ map[string]interface{}) (string, error) {
	return a.UsageAccum.Format(), nil
}

func handleContextPinLast(_ context.Context, a *Agent, _ map[string]interface{}) (string, error) {
	return "Pinned the last user message", a.pinLastUser()
}

func handleContextPins(_ context.Context, a *Agent, _ map[string]interface{}) (string, error) {
	return a.listPins(), nil
}
