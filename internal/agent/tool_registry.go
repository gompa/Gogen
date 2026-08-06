package agent

import (
	"context"
	"errors"
	"fmt"

	"gogen/internal/llm"
)

// MCPToolRegistry exposes MCP tools to the agent.
type MCPToolRegistry interface {
	Definitions() []llm.Tool
	ToolNames() map[string]struct{}
	CallTool(ctx context.Context, name string, args map[string]interface{}) (string, error)
}

// ToolHandler executes a builtin tool given parsed arguments.
type ToolHandler func(ctx context.Context, a *Agent, args map[string]interface{}) (string, error)

// BuiltinToolHandlers returns the registry of builtin tool implementations.
// It is derived from builtinToolDefs, so every tool's schema and handler are
// declared in exactly one place and can never drift apart.
func BuiltinToolHandlers() map[string]ToolHandler {
	handlers := make(map[string]ToolHandler, len(builtinToolDefs))
	for _, d := range builtinToolDefs {
		handlers[d.Definition.Name] = d.Handler
	}
	return handlers
}

// SetToolHandlers replaces the builtin tool registry. The web server uses
// this to wrap FS-mutating tools with a workspace-level filesystem lock so
// concurrent sessions serialize actual file mutations without serializing
// whole turns (multi-session plan, Phase 2). Calling with nil restores the
// builtin handlers on the next executeTool.
func (a *Agent) SetToolHandlers(handlers map[string]ToolHandler) {
	a.toolMu.Lock()
	a.toolHandlers = handlers
	a.toolMu.Unlock()
}

func (a *Agent) executeTool(ctx context.Context, tc llm.ToolCall) (string, error) {
	if tc.ArgsError != "" {
		return "", fmt.Errorf("invalid tool arguments: %s", tc.ArgsError)
	}
	if err := a.checkPlanMode(tc.Name); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	ctx = a.toolContext(ctx)
	if a.MCPRegistry != nil {
		if names := a.MCPRegistry.ToolNames(); names != nil {
			if _, ok := names[tc.Name]; ok {
				return a.MCPRegistry.CallTool(ctx, tc.Name, tc.Args)
			}
		}
	}
	a.toolMu.RLock()
	handlers := a.toolHandlers
	a.toolMu.RUnlock()
	if handlers == nil {
		handlers = BuiltinToolHandlers()
	}
	h, ok := handlers[tc.Name]
	if !ok {
		return "", fmt.Errorf("unknown tool: %s", tc.Name)
	}
	res, err := h(ctx, a, tc.Args)
	if tc.Name == "patch_file" {
		// Only stale-diff failures (ErrPatchMismatch) count toward the
		// streak: permission, I/O, and path-safety errors mean the diff may
		// be fine and the environment is the problem, so the "regenerate the
		// diff" hint would be misleading. Success and non-mismatch errors
		// both reset.
		if err != nil && errors.Is(err, ErrPatchMismatch) {
			streak := a.patchFailStreak.Add(1)
			if streak > 3 {
				streak = 3 // keep the message from escalating forever
			}
			if streak >= 2 {
				err = fmt.Errorf("patch_file failed %d times in a row: %w. Do not retry the same diff — it will keep failing. Re-read the target file(s) with read_file (or search_code) and regenerate the diff from their current content. If a small file keeps failing, use replace_in_file or write_file instead",
					streak, err)
			}
		} else {
			a.patchFailStreak.Store(0)
		}
	}
	return res, err
}

func handleListFiles(_ context.Context, a *Agent, args map[string]interface{}) (string, error) {
	path, err := stringArg(args, "path")
	if err != nil {
		return "", err
	}
	recursive, _ := boolArg(args, "recursive", false)
	tracked, _ := boolArg(args, "tracked_only", false)
	return a.Executor.ListFiles(path, recursive, tracked)
}

func handleGlobFiles(_ context.Context, a *Agent, args map[string]interface{}) (string, error) {
	pattern, err := stringArg(args, "pattern")
	if err != nil {
		return "", err
	}
	subpath, _ := stringArgOptional(args, "path")
	tracked, _ := boolArg(args, "tracked_only", false)
	return a.Executor.GlobFiles(pattern, subpath, tracked)
}

func handleRepoOverview(_ context.Context, a *Agent, _ map[string]interface{}) (string, error) {
	return a.Executor.RepoOverview()
}

func handleReadFile(_ context.Context, a *Agent, args map[string]interface{}) (string, error) {
	// Prefer "path", fall back to deprecated "file_path".
	path, err := stringArgOptional(args, "path")
	if err != nil {
		return "", err
	}
	if path == "" {
		path, err = stringArgOptional(args, "file_path")
		if err != nil {
			return "", err
		}
	}
	if path == "" {
		return "", fmt.Errorf("missing required argument %q", "path")
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
	lineNumbers, _ := boolArg(args, "line_numbers", false)
	return a.Executor.ReadFileRange(path, offset, limit, search, lineNumbers)
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
	// No diff shown: write_file only creates new files. Use show_diff
	// after staging/committing or patch_file for modifications.
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
	replaceAll, err := boolArg(args, "replace_all", false)
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
	dryRun, err := boolArg(args, "dry_run", false)
	if err != nil {
		return "", err
	}
	fuzzy, err := boolArg(args, "fuzzy", true)
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
	staged, err := boolArg(args, "staged", false)
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
	create, _ := boolArg(args, "create", false)
	return a.Executor.GitBranch(ctx, name, create)
}

func handleGitStash(ctx context.Context, a *Agent, args map[string]interface{}) (string, error) {
	message, _ := stringArgOptional(args, "message")
	pop, _ := boolArg(args, "pop", false)
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
	tm, err := a.todo()
	if err != nil {
		return "", err
	}
	out, err := tm.AddTodo(text)
	if err != nil {
		return "", err
	}
	a.persistTodos()
	return out, nil
}

func handleTodoList(_ context.Context, a *Agent, _ map[string]interface{}) (string, error) {
	tm, err := a.todo()
	if err != nil {
		return "", err
	}
	return tm.ListTodos(), nil
}

func handleTodoDone(_ context.Context, a *Agent, args map[string]interface{}) (string, error) {
	id, err := intArgOptional(args, "id")
	if err != nil || id == 0 {
		return "", fmt.Errorf("missing required argument %q", "id")
	}
	tm, err := a.todo()
	if err != nil {
		return "", err
	}
	out, err := tm.DoneTodo(id)
	if err != nil {
		return "", err
	}
	a.persistTodos()
	return out, nil
}

func handleTodoRemove(_ context.Context, a *Agent, args map[string]interface{}) (string, error) {
	id, err := intArgOptional(args, "id")
	if err != nil || id == 0 {
		return "", fmt.Errorf("missing required argument %q", "id")
	}
	tm, err := a.todo()
	if err != nil {
		return "", err
	}
	out, err := tm.RemoveTodo(id)
	if err != nil {
		return "", err
	}
	a.persistTodos()
	return out, nil
}

func handleTodoClearDone(_ context.Context, a *Agent, _ map[string]interface{}) (string, error) {
	tm, err := a.todo()
	if err != nil {
		return "", err
	}
	out, err := tm.ClearDoneTodos()
	if err != nil {
		return "", err
	}
	a.persistTodos()
	return out, nil
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
	selector, err := stringArgOptional(args, "selector")
	if err != nil {
		return "", err
	}
	query, err := stringArgOptional(args, "query")
	if err != nil {
		return "", err
	}
	contextLines, err := intArgOptional(args, "context")
	if err != nil {
		return "", err
	}
	return a.Executor.WebFetch(ctx, rawURL, WebFetchOptions{
		MaxBytes: maxBytes,
		Selector: selector,
		Query:    query,
		Context:  contextLines,
	})
}

func handleDownloadFile(ctx context.Context, a *Agent, args map[string]interface{}) (string, error) {
	rawURL, err := stringArg(args, "url")
	if err != nil {
		return "", err
	}
	path, err := stringArg(args, "path")
	if err != nil {
		return "", err
	}
	maxBytes, err := intArgOptional(args, "max_bytes")
	if err != nil {
		return "", err
	}
	overwrite, err := boolArg(args, "overwrite", false)
	if err != nil {
		return "", err
	}
	return a.Executor.DownloadFile(ctx, rawURL, path, maxBytes, overwrite)
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
	u := a.SnapshotUsageAccum()
	return u.Format(), nil
}

func handleContextPinLast(_ context.Context, a *Agent, _ map[string]interface{}) (string, error) {
	if a.PinManager == nil {
		// PinManager is unconditionally initialised in main(); this branch
		// only fires in tests / custom embeds. Tell the LLM the tool is a
		// no-op rather than returning an error that halts the turn.
		return "Pin manager not configured; context pinning is a no-op.", nil
	}
	return "Pinned the last user message", a.pinLastUser()
}

func handleContextPins(_ context.Context, a *Agent, _ map[string]interface{}) (string, error) {
	return a.listPins(), nil
}

func handleRenameSymbol(ctx context.Context, a *Agent, args map[string]interface{}) (string, error) {
	oldName, err := stringArg(args, "old_name")
	if err != nil {
		return "", err
	}
	newName, err := stringArg(args, "new_name")
	if err != nil {
		return "", err
	}
	subpath, _ := stringArgOptional(args, "path")
	glob, _ := stringArgOptional(args, "glob")
	dryRun, _ := boolArg(args, "dry_run", false)
	return a.Executor.RenameSymbol(ctx, oldName, newName, subpath, glob, dryRun)
}

func handleMultiEdit(ctx context.Context, a *Agent, args map[string]interface{}) (string, error) {
	pattern, err := stringArg(args, "pattern")
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
	dryRun, _ := boolArg(args, "dry_run", false)
	return a.Executor.MultiEdit(ctx, pattern, search, replace, dryRun)
}

func handleCallGraph(ctx context.Context, a *Agent, args map[string]interface{}) (string, error) {
	symbol, err := stringArg(args, "symbol")
	if err != nil {
		return "", err
	}
	subpath, _ := stringArgOptional(args, "path")
	glob, _ := stringArgOptional(args, "glob")
	direction, _ := stringArgOptional(args, "direction")
	return a.Executor.CallGraph(ctx, symbol, subpath, glob, direction)
}

func handleDependencyAnalysis(ctx context.Context, a *Agent, args map[string]interface{}) (string, error) {
	symbol, err := stringArg(args, "symbol")
	if err != nil {
		return "", err
	}
	subpath, _ := stringArgOptional(args, "path")
	return a.Executor.DependencyAnalysis(ctx, symbol, subpath)
}
